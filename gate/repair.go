package gate

import (
	"context"
	"fmt"
	"strings"

	"github.com/valbaudo/dawn"
)

// Candidate is one generated attempt: the rendering the jury reads, plus any
// state the attempt produced. Carrying the refs matters because a model can only
// judge a rendering (nobody feeds a directory tree to an LLM), but what the
// caller wants back is the artifact that rendering describes. Without them an
// accepted attempt's workspace would be unrecoverable from the outcome.
type Candidate struct {
	Text     string              // what the judges read
	Produced map[string]dawn.Ref // state refs this attempt created, if any
}

// Text is a Candidate with no artifacts, for generators that only produce prose.
func Text(s string) Candidate { return Candidate{Text: s} }

// FromResult builds a Candidate from an invocation, reading the jury's rendering
// from the named output field and carrying that invocation's produced refs.
func FromResult(res dawn.Result, field string) Candidate {
	s, _ := res.Output[field].(string)
	return Candidate{Text: s, Produced: res.Produced}
}

// Generate produces a candidate for evaluation. feedback is the aggregated
// critique from the previous rejected attempt ("" on the first attempt); the
// closure decides how to fold it into the next generation. A Generate that
// returns an error is a MECHANICAL failure, not a rejection: [Gate] propagates
// it and does not consume an attempt.
type Generate func(ctx context.Context, feedback string) (Candidate, error)

// Outcome is the result of a [Gate] run.
type Outcome struct {
	Approved  bool      // did a candidate reach quorum within maxAttempts?
	Candidate Candidate // the accepted attempt, or the last one tried
	Attempts  int       // number of evaluations performed
	Votes     []Verdict // the final round's votes
}

// Gate is the full acceptance primitive: generate -> jury -> repair. It runs
// gen, has the jury vote, and on a rejection folds the jury's critique back into
// the next gen call, up to maxAttempts. It returns at the first passing quorum,
// or after maxAttempts with Approved=false and a nil error (a bounded rejection
// is a legitimate outcome, not a crash).
//
// crash != verdict, engine-enforced here: a mechanical failure in generation OR
// evaluation (any judge's Invoke erroring) returns a non-nil error and does NOT
// count as a rejection or consume the attempt budget. Only a clean evaluation
// whose verdict is "reject" spends an attempt and triggers repair.
func Gate(ctx context.Context, gen Generate, judges []dawn.Backend, system string, quorum, maxAttempts int) (Outcome, error) {
	var out Outcome
	feedback := ""
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		candidate, err := gen(ctx, feedback)
		if err != nil {
			return out, fmt.Errorf("gate: generate attempt %d: %w", attempt, err)
		}
		approved, votes := Jury(ctx, judges, system, candidate.Text, quorum)
		if err := firstErr(votes); err != nil {
			// A judge could not deliver a verdict: inconclusive, not a rejection.
			return out, fmt.Errorf("gate: evaluate attempt %d (mechanical, not a verdict): %w", attempt, err)
		}
		out = Outcome{Approved: approved, Candidate: candidate, Attempts: attempt, Votes: votes}
		if approved {
			return out, nil
		}
		feedback = critique(votes)
	}
	return out, nil // attempts exhausted: bounded rejection
}

// firstErr returns the first judge error in a round, or nil if every judge
// delivered a real vote.
func firstErr(votes []Verdict) error {
	for _, v := range votes {
		if v.Err != nil {
			return v.Err
		}
	}
	return nil
}

// critique turns a rejected round into feedback for the next attempt: the
// objections of the judges that voted no. This is what makes repair converge.
func critique(votes []Verdict) string {
	var b strings.Builder
	b.WriteString("A prior version was REJECTED by the review panel. Address every objection:\n")
	for _, v := range votes {
		if !v.Approved && v.Reason != "" {
			fmt.Fprintf(&b, "- %s: %s\n", v.Judge, v.Reason)
		}
	}
	return b.String()
}
