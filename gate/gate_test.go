package gate

import (
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/dawn"
)

// fakeJudge is a Backend that votes a fixed way (or errors) without any network.
// It exists to prove the Backend seam is testable with a trivial in-memory impl.
type fakeJudge struct {
	name     string
	approved bool
	err      error
}

func (f fakeJudge) Name() string { return f.name }
func (f fakeJudge) Invoke(context.Context, dawn.Invocation) (dawn.Result, error) {
	if f.err != nil {
		return dawn.Result{}, f.err
	}
	return dawn.Result{Output: map[string]any{"approved": f.approved, "reason": "test"}}, nil
}

func TestJuryQuorum(t *testing.T) {
	cases := []struct {
		name   string
		votes  []bool
		quorum int
		want   bool
	}{
		{"unanimous pass", []bool{true, true, true}, Majority(3), true}, // 3>=2
		{"majority pass", []bool{true, true, false}, Majority(3), true}, // 2>=2
		{"minority fails", []bool{true, false, false}, Majority(3), false},
		{"tie fails closed", []bool{true, false}, Majority(2), false}, // 1<2
		{"all reject", []bool{false, false, false}, Majority(3), false},
		{"unanimous of four", []bool{true, true, true, true}, Majority(4), true}, // 4>=3
		{"even split of four fails", []bool{true, true, false, false}, Majority(4), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			judges := make([]dawn.Backend, len(c.votes))
			for i, v := range c.votes {
				judges[i] = fakeJudge{name: "j", approved: v}
			}
			got, votes := Jury(context.Background(), judges, "sys", "cand", c.quorum)
			if got != c.want {
				t.Fatalf("Jury(votes=%v, quorum=%d) = %v, want %v", c.votes, c.quorum, got, c.want)
			}
			if len(votes) != len(c.votes) {
				t.Fatalf("got %d votes, want %d", len(votes), len(c.votes))
			}
		})
	}
}

// An errored judge must not count as an approval — the crash≠verdict invariant
// in miniature: a judge that failed to run is not a "no", it is simply not a "yes".
func TestJuryErrorIsNotApproval(t *testing.T) {
	judges := []dawn.Backend{
		fakeJudge{name: "a", approved: true},
		fakeJudge{name: "b", err: errors.New("boom")},
		fakeJudge{name: "c", approved: true},
	}
	got, votes := Jury(context.Background(), judges, "s", "c", Majority(3))
	if !got {
		t.Fatalf("two real approvals should meet majority=2, got reject")
	}
	if votes[1].Err == nil {
		t.Fatalf("errored judge should carry Err")
	}
	if votes[1].Approved {
		t.Fatalf("errored judge must not be Approved")
	}
}

// malformedJudge returns a well-formed Result whose Output has no usable
// "approved" field — what a CLI adapter produces when the model replies with
// prose (a refusal, a rate-limit notice, a chatty preamble).
type malformedJudge struct {
	name   string
	output map[string]any
}

func (m malformedJudge) Name() string { return m.name }
func (m malformedJudge) Invoke(context.Context, dawn.Invocation) (dawn.Result, error) {
	return dawn.Result{Output: m.output}, nil
}

// A judge that returned no usable verdict has NOT voted no. Reading a missing or
// non-bool "approved" as false would silently convert a parse failure into a
// quality rejection.
func TestJudgeMalformedVerdictIsErrorNotRejection(t *testing.T) {
	cases := map[string]map[string]any{
		"missing approved":  {"reason": "I cannot help with that"},
		"non-bool approved": {"approved": "yes", "reason": "stringly typed"},
		"empty output":      {},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			v := Judge(context.Background(), malformedJudge{"m", out}, "sys", "candidate")
			if v.Err == nil {
				t.Fatal("a malformed verdict must set Err, not vote no")
			}
			if v.Approved {
				t.Fatal("a malformed verdict must never read as approval")
			}
		})
	}
}

// The same failure inside a Gate must propagate as mechanical and leave the
// repair budget untouched, rather than burning every attempt on a parse bug.
func TestGateMalformedVerdictDoesNotBurnAttempts(t *testing.T) {
	var generated []string
	judges := []dawn.Backend{
		fakeJudge{name: "ok", approved: true},
		malformedJudge{"chatty", map[string]any{"reason": "Sure! Here you go:"}},
		fakeJudge{name: "ok2", approved: true},
	}
	gen := func(_ context.Context, feedback string) (Candidate, error) {
		generated = append(generated, feedback)
		return Text("candidate"), nil
	}
	out, err := Gate(context.Background(), gen, judges, "sys", Majority(3), 3)
	if err == nil {
		t.Fatal("a malformed verdict must surface as a mechanical failure")
	}
	if out.Approved {
		t.Fatal("must not report approval")
	}
	if len(generated) != 1 {
		t.Fatalf("a parse failure must not consume repair attempts, generated %d times", len(generated))
	}
}
