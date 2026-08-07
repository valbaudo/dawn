package gate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valbaudo/dawn"
)

// contentJudge approves iff the candidate (delivered as in.Prompt) contains the
// word "good" — a stand-in for a quality bar the repair loop must satisfy.
type contentJudge struct{ name string }

func (c contentJudge) Name() string { return c.name }
func (c contentJudge) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	return dawn.Result{Output: map[string]any{
		"approved": strings.Contains(in.Prompt, "good"),
		"reason":   "must contain the word good",
	}}, nil
}

func threeContentJudges() []dawn.Backend {
	return []dawn.Backend{contentJudge{"a"}, contentJudge{"b"}, contentJudge{"c"}}
}

type countingContentJudge struct {
	name  string
	calls *atomic.Int64
}

func (c countingContentJudge) Name() string { return c.name }
func (c countingContentJudge) Invoke(ctx context.Context, in dawn.Invocation) (dawn.Result, error) {
	c.calls.Add(1)
	return contentJudge{name: c.name}.Invoke(ctx, in)
}

// scripted returns a Generate that yields seq in order (repeating the last),
// recording the feedback it was handed on each call.
func scripted(seen *[]string, seq ...string) Generate {
	i := 0
	return func(_ context.Context, feedback string) (Candidate, error) {
		*seen = append(*seen, feedback)
		s := seq[i]
		if i < len(seq)-1 {
			i++
		}
		// Each attempt carries a ref named after its own text, so a test can
		// tell WHICH attempt's artifact came back in the outcome.
		return Candidate{
			Text:     s,
			Produced: map[string]dawn.Ref{"workspace": {Kind: dawn.KindWorkspace, URI: "tree-" + s}},
		}, nil
	}
}

func TestGatePassesFirstTry(t *testing.T) {
	var fb []string
	out, err := Gate(context.Background(), scripted(&fb, "a good draft"),
		threeContentJudges(), "sys", Majority(3), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Approved || out.Attempts != 1 {
		t.Fatalf("want approved on attempt 1, got %+v", out)
	}
}

func TestGateConsumesPreJudgeRejectionWithoutCallingJudges(t *testing.T) {
	var feedback []string
	var calls atomic.Int64
	attempt := 0
	gen := func(_ context.Context, seen string) (Candidate, error) {
		feedback = append(feedback, seen)
		attempt++
		if attempt == 1 {
			return Candidate{Rejection: "you did not produce dist/dawn"}, nil
		}
		return Text("good"), nil
	}
	judges := []dawn.Backend{
		countingContentJudge{name: "a", calls: &calls},
		countingContentJudge{name: "b", calls: &calls},
		countingContentJudge{name: "c", calls: &calls},
	}

	out, err := Gate(context.Background(), gen, judges, "sys", Majority(3), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Approved || out.Attempts != 2 {
		t.Fatalf("want approval on attempt 2, got %+v", out)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("judge calls = %d, want one panel round (3)", got)
	}
	if !strings.Contains(feedback[1], "dist/dawn") {
		t.Fatalf("repair feedback must name the missing path, got %q", feedback[1])
	}
}

func TestGateRepairsAfterFeedback(t *testing.T) {
	var fb []string
	out, err := Gate(context.Background(), scripted(&fb, "a bad draft", "a good draft"),
		threeContentJudges(), "sys", Majority(3), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Approved {
		t.Fatalf("expected approval after one repair, got %+v", out)
	}
	if out.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", out.Attempts)
	}
	if fb[0] != "" {
		t.Fatalf("attempt 1 feedback should be empty, got %q", fb[0])
	}
	if !strings.Contains(fb[1], "REJECTED") {
		t.Fatalf("attempt 2 should carry the rejection critique, got %q", fb[1])
	}
}

func TestGateExhaustsIsBoundedRejection(t *testing.T) {
	var fb []string
	out, err := Gate(context.Background(), scripted(&fb, "a bad draft"),
		threeContentJudges(), "sys", Majority(3), 2)
	if err != nil {
		t.Fatalf("exhausting attempts is not an error: %v", err)
	}
	if out.Approved {
		t.Fatalf("should not approve a persistently bad draft")
	}
	if out.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (maxAttempts)", out.Attempts)
	}
}

func TestGateGenerateErrorPropagates(t *testing.T) {
	gen := func(context.Context, string) (Candidate, error) { return Candidate{}, errors.New("gen boom") }
	_, err := Gate(context.Background(), gen, threeContentJudges(), "sys", Majority(3), 3)
	if err == nil {
		t.Fatal("a generator failure must propagate, not be swallowed")
	}
}

// A judge that could not deliver a verdict is a mechanical failure: it must
// propagate as an error, never be counted as a rejection that burns an attempt.
func TestGateJudgeErrorIsMechanicalNotRejection(t *testing.T) {
	var fb []string
	judges := []dawn.Backend{
		contentJudge{"a"},
		fakeJudge{name: "flaky", err: errors.New("timeout")},
		contentJudge{"c"},
	}
	out, err := Gate(context.Background(), scripted(&fb, "a good draft"),
		judges, "sys", Majority(3), 3)
	if err == nil {
		t.Fatal("a judge error must surface as a mechanical failure")
	}
	if out.Approved {
		t.Fatal("a mechanically failed evaluation must not report approval")
	}
	if len(fb) != 1 {
		t.Fatalf("mechanical failure must not consume repair attempts, generated %d times", len(fb))
	}
}

// The point of Candidate carrying refs: after a repair loop, the caller can
// recover the artifact the ACCEPTED attempt produced. Before this, Outcome held
// only a string and the accepted repo@vN was unrecoverable.
func TestGateReturnsAcceptedAttemptsArtifact(t *testing.T) {
	var fb []string
	out, err := Gate(context.Background(), scripted(&fb, "a bad draft", "a good draft"),
		threeContentJudges(), "sys", Majority(3), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Approved || out.Attempts != 2 {
		t.Fatalf("expected approval on attempt 2, got %+v", out)
	}
	ref, ok := out.Candidate.Produced["workspace"]
	if !ok {
		t.Fatal("outcome dropped the accepted attempt's state refs")
	}
	if ref.URI != "tree-a good draft" {
		t.Fatalf("got the wrong attempt's artifact: %q (rejected attempt 1 leaked through)", ref.URI)
	}
	if ref.Kind != dawn.KindWorkspace {
		t.Fatalf("ref kind = %q", ref.Kind)
	}
}
