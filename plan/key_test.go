package plan

import (
	"context"
	"testing"

	"github.com/valbaudo/aw"
)

var agentX = Agent{Backend: "claude", Model: "sonnet"}

func key(t *testing.T, id string, s Step, a Agent, in map[string]string) string {
	t.Helper()
	k, err := s.Key(id, a, in)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func baseStep() Step {
	return Step{Agent: "claude/sonnet", Prompt: "do the thing", Outputs: map[string]Type{"text": {}}}
}

// The invalidation contract from SPEC §5. Each row is a real 3am question:
// "I changed X — what re-runs?"
func TestInvalidationContract(t *testing.T) {
	baseKey := key(t, "s", baseStep(), agentX, nil)

	t.Run("same/recomputing", func(t *testing.T) {
		if got := key(t, "s", baseStep(), agentX, nil); got != baseKey {
			t.Fatal("recomputing must be stable")
		}
	})
	t.Run("same/explicit default output", func(t *testing.T) {
		s := baseStep()
		s.Outputs = map[string]Type{"text": {}} // identical to the default
		if got := key(t, "s", s, agentX, nil); got != baseKey {
			t.Fatal("writing a resolved default must be free")
		}
	})
	t.Run("same/expect order", func(t *testing.T) {
		a, b := baseStep(), baseStep()
		a.Expect = []string{"x", "y"}
		b.Expect = []string{"y", "x"}
		if key(t, "s", a, agentX, nil) != key(t, "s", b, agentX, nil) {
			t.Fatal("expect order is cosmetic")
		}
	})

	differ := map[string]func(*Step, *Agent, *string){
		"edit the prompt":  func(s *Step, _ *Agent, _ *string) { s.Prompt = "other" },
		"change the model": func(_ *Step, a *Agent, _ *string) { a.Model = "opus" },
		"change backend":   func(_ *Step, a *Agent, _ *string) { a.Backend = "claude-ws" },
		"rename the step":  func(_ *Step, _ *Agent, id *string) { *id = "renamed" },
		"add an output":    func(s *Step, _ *Agent, _ *string) { s.Outputs["extra"] = Type{} },
		"constrain a field": func(s *Step, _ *Agent, _ *string) {
			s.Outputs["text"] = Type{Enum: []string{"a", "b"}}
		},
		"declare an artifact": func(s *Step, _ *Agent, _ *string) { s.Expect = []string{"dist/aw"} },
	}
	for name, mutate := range differ {
		t.Run("differs/"+name, func(t *testing.T) {
			s, a, id := baseStep(), agentX, "s"
			mutate(&s, &a, &id)
			if got := key(t, id, s, a, nil); got == baseKey {
				t.Fatal("key should have changed but did not")
			}
		})
	}
}

// Reordering a panel is cosmetic; quorum is over a set. Writing the resolved
// default is free.
func TestGateKeyNormalization(t *testing.T) {
	mk := func(judges []string, quorum *int) Step {
		return Step{Agent: "claude/sonnet", Prompt: "p",
			Gate: &Gate{Judges: judges, Criteria: "c", Quorum: quorum}}
	}
	a := key(t, "s", mk([]string{"a/x", "a/y", "a/z"}, nil), agentX, nil)
	b := key(t, "s", mk([]string{"a/z", "a/x", "a/y"}, nil), agentX, nil)
	if a != b {
		t.Fatal("judge order must not affect identity")
	}
	if c := key(t, "s", mk([]string{"a/x", "a/y", "a/z"}, q(2)), agentX, nil); c != a {
		t.Fatal("writing the resolved majority explicitly must hash identically")
	}
	if d := key(t, "s", mk([]string{"a/x", "a/y", "a/z"}, q(3)), agentX, nil); d == a {
		t.Fatal("a different bar is a different question")
	}
}

// Inputs enter as the upstream's RESOLVED value, which buys early cutoff.
func TestInputValuesDriveTheKey(t *testing.T) {
	s := Step{Agent: "claude/sonnet", Prompt: "p", Inputs: map[string]string{"a": "up.text"}}
	k1 := key(t, "s", s, agentX, map[string]string{"a": "same"})
	k2 := key(t, "s", s, agentX, map[string]string{"a": "same"})
	k3 := key(t, "s", s, agentX, map[string]string{"a": "different"})
	if k1 != k2 {
		t.Fatal("identical inputs must give an identical key")
	}
	if k1 == k3 {
		t.Fatal("different upstream bytes must invalidate the downstream")
	}
}

func TestJournalOnlyAcceptedResultsServeAHit(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Lookup("sha256:nope"); ok {
		t.Fatal("an empty journal must miss")
	}
	if err := j.Append(Entry{Key: "k1", Step: "s", Rejected: "panel said no"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Lookup("k1"); ok {
		t.Fatal("a rejection must never serve a hit")
	}
	if err := j.Append(Entry{Key: "k1", Ref: "sha256:aaa", Step: "s"}); err != nil {
		t.Fatal(err)
	}
	if ref, ok := j.Lookup("k1"); !ok || ref != "sha256:aaa" {
		t.Fatalf("expected the accepted ref, got %q %v", ref, ok)
	}
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

func twoStepPlan(secondPrompt string) *Plan {
	return &Plan{Steps: map[string]Step{
		"first":  {Agent: "x/echo", Prompt: "one", Outputs: map[string]Type{"text": {}}},
		"second": {Agent: "x/echo", Prompt: secondPrompt, Inputs: map[string]string{"p": "first.text"}, Outputs: map[string]Type{"text": {}}},
	}}
}

// Re-running the same command IS the resume: no mode, no flag, one code path.
func TestReRunningSkipsCommittedSteps(t *testing.T) {
	dir := t.TempDir()
	var calls int
	mk := func() *Runner { return durable(t, dir, byModel(map[string]aw.Backend{"echo": echo{&calls}})) }

	if _, err := mk().Run(context.Background(), twoStepPlan("two")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("cold run should execute both, got %d", calls)
	}
	calls = 0
	if _, err := mk().Run(context.Background(), twoStepPlan("two")); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("a second run with no edits must do zero paid work, ran %d", calls)
	}
}

func TestEditingAPromptRerunsOnlyThatStep(t *testing.T) {
	dir := t.TempDir()
	var calls int
	mk := func() *Runner { return durable(t, dir, byModel(map[string]aw.Backend{"echo": echo{&calls}})) }

	if _, err := mk().Run(context.Background(), twoStepPlan("two")); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if _, err := mk().Run(context.Background(), twoStepPlan("two, revised")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("editing one prompt must re-run exactly that step, ran %d", calls)
	}
}

func TestRedoForcesAStep(t *testing.T) {
	dir := t.TempDir()
	var calls int
	p := twoStepPlan("two")
	if _, err := durable(t, dir, byModel(map[string]aw.Backend{"echo": echo{&calls}})).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	calls = 0
	r := durable(t, dir, byModel(map[string]aw.Backend{"echo": echo{&calls}}))
	r.Redo = map[string]bool{"first": true}
	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// `first` is forced; its bytes are stable, so `second` is correctly skipped —
	// downstream invalidation follows the bytes, not the fact of a re-run.
	if calls != 1 {
		t.Fatalf("--redo should force exactly the named step, ran %d", calls)
	}
}

// Produced refs must survive a commit, or a workspace chain breaks on the next run.
func TestProducedRefsSurviveACommit(t *testing.T) {
	dir := t.TempDir()
	p := &Plan{Steps: map[string]Step{"first": {Agent: "x/gen", Prompt: "make it", Outputs: map[string]Type{"text": {}}}}}
	mk := func() *Runner { return durable(t, dir, byModel(map[string]aw.Backend{"gen": producer{}})) }
	if _, err := mk().Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	done, err := mk().Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := done["first"].Produced["workspace"]; !ok || ref.URI != "tree-abc" {
		t.Fatalf("the committed record dropped the produced refs: %+v", done["first"].Produced)
	}
}

// Status is the dry run: the same identity walk, minus execution.
func TestStatusReportsFreshStaleUnknown(t *testing.T) {
	dir := t.TempDir()
	var calls int
	p := twoStepPlan("two")
	mk := func() *Runner { return durable(t, dir, byModel(map[string]aw.Backend{"echo": echo{&calls}})) }

	st, err := mk().Status(p)
	if err != nil {
		t.Fatal(err)
	}
	if st[0].State != "stale" || st[1].State != "unknown" {
		t.Fatalf("cold plan should be stale then unknown, got %+v", st)
	}
	if calls != 0 {
		t.Fatal("Status must not execute anything")
	}

	if _, err := mk().Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	st, err = mk().Status(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		if s.State != "fresh" {
			t.Fatalf("after a run everything is fresh, got %+v", st)
		}
	}
}

func TestWorstCaseCounts(t *testing.T) {
	plain := Step{}
	if got := worstCase(plain); got != 1 {
		t.Fatalf("an ungated step is one call, got %d", got)
	}
	g := Step{Gate: &Gate{Judges: []string{"a/x", "a/y", "a/z"}}}
	if got := worstCase(g); got != Attempts*4 {
		t.Fatalf("worst case is attempts x (1 gen + N judges), got %d", got)
	}
}
