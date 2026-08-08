package gate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

type recordingFakeJudge struct {
	name string
	mu   sync.Mutex
	seen []dawn.Invocation
}

func (f *recordingFakeJudge) Name() string { return f.name }
func (f *recordingFakeJudge) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, in)
	return dawn.Result{Output: map[string]any{"approved": true, "reason": "valid"}}, nil
}

type mutatingSchemaJudge struct {
	mutated chan struct{}
	checked chan struct{}
}

func (mutatingSchemaJudge) Name() string { return "mutator" }
func (j mutatingSchemaJudge) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	properties := in.Schema["properties"].(map[string]any)
	approved := properties["approved"].(map[string]any)
	reason := properties["reason"].(map[string]any)
	required := in.Schema["required"].([]any)
	in.Schema["type"] = "mutated"
	properties["leaked"] = true
	approved["type"] = "string"
	reason["type"] = "number"
	required[0] = "mutated"
	close(j.mutated)
	<-j.checked

	// Restore the received schema so this RED test does not poison unrelated tests
	// while the implementation still uses one package-global map.
	in.Schema["type"] = "object"
	delete(properties, "leaked")
	approved["type"] = "boolean"
	reason["type"] = "string"
	required[0] = "approved"
	return dawn.Result{Output: map[string]any{"approved": true, "reason": "valid"}}, nil
}

type checkingSchemaJudge struct {
	name    string
	wait    <-chan struct{}
	checked chan<- struct{}
	err     chan<- error
}

func (j checkingSchemaJudge) Name() string { return j.name }
func (j checkingSchemaJudge) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	if j.wait != nil {
		<-j.wait
	}
	properties, propertiesOK := in.Schema["properties"].(map[string]any)
	approved, approvedOK := map[string]any(nil), false
	reason, reasonOK := map[string]any(nil), false
	_, leaked := properties["leaked"]
	if propertiesOK {
		approved, approvedOK = properties["approved"].(map[string]any)
		reason, reasonOK = properties["reason"].(map[string]any)
	}
	required, requiredOK := in.Schema["required"].([]any)
	valid := in.Schema["type"] == "object" && propertiesOK && approvedOK && reasonOK && !leaked &&
		approved["type"] == "boolean" && reason["type"] == "string" &&
		requiredOK && len(required) == 2 && required[0] == "approved" && required[1] == "reason"
	if !valid {
		j.err <- fmt.Errorf("mutated verdict schema leaked into %s: %#v", j.name, in.Schema)
	} else {
		j.err <- nil
	}
	if j.checked != nil {
		close(j.checked)
	}
	return dawn.Result{Output: map[string]any{"approved": true, "reason": "valid"}}, nil
}

func TestJuryVerdictSchemasAreDeeplyIndependent(t *testing.T) {
	mutated := make(chan struct{})
	checked := make(chan struct{})
	errs := make(chan error, 2)
	judges := []dawn.Backend{
		mutatingSchemaJudge{mutated: mutated, checked: checked},
		checkingSchemaJudge{name: "concurrent judge", wait: mutated, checked: checked, err: errs},
	}
	approved, votes := Jury(context.Background(), judges, "criteria", "evidence", 2)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if !approved || len(votes) != 2 {
		t.Fatalf("jury result approved=%v votes=%d, want approval with two votes", approved, len(votes))
	}

	Judge(context.Background(), checkingSchemaJudge{name: "future judge", err: errs}, "criteria", "evidence")
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

func TestJuryGivesEachJudgeIndependentIdenticalEvidence(t *testing.T) {
	a := &recordingFakeJudge{name: "a"}
	b := &recordingFakeJudge{name: "b"}
	approved, _ := Jury(context.Background(), []dawn.Backend{a, b}, "criteria", "evidence", 2)
	if !approved {
		t.Fatal("two approvals should pass")
	}
	for _, judge := range []*recordingFakeJudge{a, b} {
		if len(judge.seen) != 1 {
			t.Fatalf("judge %s calls = %d, want 1", judge.name, len(judge.seen))
		}
		got := judge.seen[0]
		if got.System != "criteria" || got.Prompt != "evidence" {
			t.Fatalf("judge %s got system=%q prompt=%q", judge.name, got.System, got.Prompt)
		}
	}
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

// Tolerating a missing `reason` on the way IN must not silence it on the way
// OUT. A rejection is the only signal repair has, so a panel that refuses
// without reasons has to produce a critique that says so — otherwise the next
// attempt is told it was refused and nothing about what to change, and the loop
// spends its whole budget regenerating against no information.
func TestCritiqueNamesAJudgeThatRejectedWithoutAReason(t *testing.T) {
	got := critique([]Verdict{
		{Judge: "a", Approved: false},
		{Judge: "b", Approved: false, Reason: "too vague"},
		{Judge: "c", Approved: true},
	})
	if !strings.Contains(got, "- a:") {
		t.Fatalf("a reasonless rejection must still appear:\n%s", got)
	}
	if !strings.Contains(got, "- b: too vague") {
		t.Fatalf("a stated objection must survive:\n%s", got)
	}
	if strings.Contains(got, "- c:") {
		t.Fatalf("an approval is not an objection:\n%s", got)
	}
	if strings.TrimSpace(strings.TrimPrefix(got, rejectionHeading)) == "" {
		t.Fatal("critique is the heading alone, which tells the generator nothing")
	}
}

// The mirror image: `reason` decides nothing, so a judge that omits it or types
// it badly has still voted. Failing the run on a field nothing counts makes
// success depend on a model's output discipline, and turns a panel that reached
// quorum cleanly into a hard failure.
func TestJudgeVotesWithoutAUsableReason(t *testing.T) {
	cases := map[string]map[string]any{
		"missing reason":    {"approved": true},
		"non-string reason": {"approved": true, "reason": 42},
		"null reason":       {"approved": true, "reason": nil},
	}
	for name, out := range cases {
		t.Run(name, func(t *testing.T) {
			v := Judge(context.Background(), malformedJudge{"m", out}, "sys", "candidate")
			if v.Err != nil {
				t.Fatalf("reason is provenance, not a verdict: %v", v.Err)
			}
			if !v.Approved {
				t.Fatal("the approval was well-formed and must be counted")
			}
			if v.Reason != "" {
				t.Fatalf("Reason = %q, want empty", v.Reason)
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
