package gate

import (
	"context"
	"errors"
	"testing"

	"github.com/valbaudo/aw"
)

// fakeJudge is a Backend that votes a fixed way (or errors) without any network.
// It exists to prove the Backend seam is testable with a trivial in-memory impl.
type fakeJudge struct {
	name     string
	approved bool
	err      error
}

func (f fakeJudge) Name() string { return f.name }
func (f fakeJudge) Invoke(context.Context, aw.Invocation) (aw.Result, error) {
	if f.err != nil {
		return aw.Result{}, f.err
	}
	return aw.Result{Output: map[string]any{"approved": f.approved, "reason": "test"}}, nil
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
			judges := make([]aw.Backend, len(c.votes))
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
	judges := []aw.Backend{
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
