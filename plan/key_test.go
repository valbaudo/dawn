package plan

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/store"
)

func key(t *testing.T, s Step, a Agent, in map[string]string) string {
	t.Helper()
	k, err := s.Key(a, in)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var agentX = Agent{Backend: "claude", Model: "sonnet"}

func baseStep() Step {
	return Step{ID: "s", Agent: "w", Prompt: "do the thing", Output: map[string]Type{"text": {}}}
}

// The invalidation contract from SPEC §5, as a table. Each row is a real 3am
// question: "I changed X — what re-runs?"
func TestInvalidationContract(t *testing.T) {
	base := baseStep()
	baseKey := key(t, base, agentX, nil)

	same := map[string]func(*Step, *Agent){
		"recomputing changes nothing": func(*Step, *Agent) {},
		"writing the default output explicitly": func(s *Step, _ *Agent) {
			s.Output = map[string]Type{"text": {}} // identical to the default
		},
	}
	for name, mutate := range same {
		t.Run("same/"+name, func(t *testing.T) {
			s, a := baseStep(), agentX
			mutate(&s, &a)
			if got := key(t, s, a, nil); got != baseKey {
				t.Fatalf("key should not have changed:\n  base %s\n  got  %s", baseKey, got)
			}
		})
	}

	differ := map[string]func(*Step, *Agent){
		"edit the prompt":   func(s *Step, _ *Agent) { s.Prompt = "do the other thing" },
		"change the model":  func(_ *Step, a *Agent) { a.Model = "opus" },
		"change backend":    func(_ *Step, a *Agent) { a.Backend = "claude-ws" },
		"rename the step":   func(s *Step, _ *Agent) { s.ID = "renamed" },
		"add an output":     func(s *Step, _ *Agent) { s.Output["extra"] = Type{} },
		"constrain a field": func(s *Step, _ *Agent) { s.Output["text"] = Type{Enum: []string{"a", "b"}} },
	}
	for name, mutate := range differ {
		t.Run("differs/"+name, func(t *testing.T) {
			s, a := baseStep(), agentX
			mutate(&s, &a)
			if got := key(t, s, a, nil); got == baseKey {
				t.Fatal("key should have changed but did not")
			}
		})
	}
}

// A default written out explicitly must hash identically, or authors get punished
// for being explicit. The key hashes the RESOLVED definition.
func TestExplicitDefaultsAreFree(t *testing.T) {
	implicit := Step{ID: "s", Prompt: "p", Gate: &Gate{Judges: []string{"a", "b"}, Criteria: "c"}}
	explicit := Step{ID: "s", Prompt: "p",
		Output: map[string]Type{"text": {}},                                    // the default
		Gate:   &Gate{Judges: []string{"a", "b"}, Criteria: "c", Quorum: q(2)}} // Majority(2)
	if key(t, implicit, agentX, nil) != key(t, explicit, agentX, nil) {
		t.Fatal("writing a resolved default must hash identically")
	}
}

// Reordering a panel is cosmetic; quorum is over a set.
func TestJudgeOrderIsFree(t *testing.T) {
	a := Step{ID: "s", Prompt: "p", Gate: &Gate{Judges: []string{"x", "y", "z"}, Criteria: "c"}}
	b := Step{ID: "s", Prompt: "p", Gate: &Gate{Judges: []string{"z", "x", "y"}, Criteria: "c"}}
	if key(t, a, agentX, nil) != key(t, b, agentX, nil) {
		t.Fatal("judge order must not affect identity")
	}
}

// attempts is policy, not identity: a result accepted under 3 attempts is equally
// accepted under 5.
func TestAttemptsIsNotIdentity(t *testing.T) {
	a := Step{ID: "s", Prompt: "p", Gate: &Gate{Judges: []string{"x"}, Criteria: "c", Attempts: 3}}
	b := Step{ID: "s", Prompt: "p", Gate: &Gate{Judges: []string{"x"}, Criteria: "c", Attempts: 9}}
	if key(t, a, agentX, nil) != key(t, b, agentX, nil) {
		t.Fatal("attempts must not be part of the key")
	}
}

// Inputs enter as the upstream's RESOLVED value, so identical upstream bytes mean
// the downstream is correctly skipped (early cutoff), and different bytes re-run it.
func TestInputValuesDriveTheKey(t *testing.T) {
	s := Step{ID: "s", Prompt: "p", Inputs: map[string]string{"a": "steps.up.text"}}
	k1 := key(t, s, agentX, map[string]string{"a": "same"})
	k2 := key(t, s, agentX, map[string]string{"a": "same"})
	k3 := key(t, s, agentX, map[string]string{"a": "different"})
	if k1 != k2 {
		t.Fatal("identical inputs must give an identical key")
	}
	if k1 == k3 {
		t.Fatal("different upstream bytes must invalidate the downstream")
	}
}

// ---- journal ----

func TestJournalOnlyAcceptedResultsServeAHit(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Lookup("sha256:nope"); ok {
		t.Fatal("an empty journal must miss")
	}
	// a rejection is recorded but carries no ref
	if err := j.Append(Entry{Key: "k1", Step: "s", Rejected: "panel said no"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Lookup("k1"); ok {
		t.Fatal("a rejection must never serve a hit")
	}
	if err := j.Append(Entry{Key: "k1", Ref: "sha256:aaa", Step: "s"}); err != nil {
		t.Fatal(err)
	}
	ref, ok := j.Lookup("k1")
	if !ok || ref != "sha256:aaa" {
		t.Fatalf("expected the accepted ref, got %q %v", ref, ok)
	}
	// newest wins
	if err := j.Append(Entry{Key: "k1", Ref: "sha256:bbb", Step: "s"}); err != nil {
		t.Fatal(err)
	}
	if ref, _ := j.Lookup("k1"); ref != "sha256:bbb" {
		t.Fatalf("newest line with a ref must win, got %q", ref)
	}
	entries, err := j.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("the journal is append-only; expected 3 entries, got %d", len(entries))
	}
}

// ---- end to end: editing a prompt re-runs that step and nothing else ----

func runnerOn(t *testing.T, dir string, calls *int) *Runner {
	t.Helper()
	blobs, err := store.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Blobs: blobs, Journal: j,
		Backend: func(Agent) (aw.Backend, error) { return echoBackend{calls}, nil }}
}

func twoStepPlan(secondPrompt string) *Plan {
	return &Plan{
		Version: 1,
		Agents:  map[string]Agent{"a": {Backend: "echo", Model: "m"}},
		Steps: []Step{
			{ID: "first", Agent: "a", Prompt: "one", Output: map[string]Type{"text": {}}},
			{ID: "second", Agent: "a", Prompt: secondPrompt,
				Inputs: map[string]string{"p": "steps.first.text"}, Output: map[string]Type{"text": {}}},
		},
	}
}

func TestEditingAPromptRerunsOnlyThatStep(t *testing.T) {
	dir := t.TempDir()
	var calls int

	if _, err := runnerOn(t, dir, &calls).Run(context.Background(), twoStepPlan("two")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("cold run should execute both, got %d", calls)
	}

	// edit ONLY the second step's prompt
	calls = 0
	if _, err := runnerOn(t, dir, &calls).Run(context.Background(), twoStepPlan("two, revised")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("editing one prompt must re-run exactly that step, ran %d", calls)
	}
}

func TestRedoForcesAStepAndItsDescendants(t *testing.T) {
	dir := t.TempDir()
	var calls int
	p := twoStepPlan("two")
	if _, err := runnerOn(t, dir, &calls).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}

	calls = 0
	r := runnerOn(t, dir, &calls)
	r.Redo = map[string]bool{"first": true}
	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// `first` is forced. Its output is deterministic here, so its ref is unchanged
	// and `second` is correctly SKIPPED — downstream invalidation follows the bytes,
	// not the fact that an upstream was re-run.
	if calls != 1 {
		t.Fatalf("--redo should force exactly the named step when its bytes are stable, ran %d", calls)
	}
}
