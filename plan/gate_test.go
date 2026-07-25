package plan

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/store"
)

// producer emits a text field plus a workspace ref, standing in for a workspace
// backend without running anything.
type producer struct{ seen *[]aw.Invocation }

func (p producer) Name() string { return "producer" }
func (p producer) Invoke(_ context.Context, in aw.Invocation) (aw.Result, error) {
	if p.seen != nil {
		*p.seen = append(*p.seen, in)
	}
	return aw.Result{
		Output:   conform(in, in.Prompt),
		Produced: map[string]aw.Ref{"workspace": {Kind: aw.KindWorkspace, URI: "tree-abc"}},
	}, nil
}

// voter approves or rejects, flipping to approve after flipAfter rejections so a
// repair loop can be exercised deterministically.
type voter struct {
	name      string
	approve   bool
	flipAfter int
	seen      *int
}

func (v voter) Name() string { return v.name }
func (v voter) Invoke(_ context.Context, _ aw.Invocation) (aw.Result, error) {
	approved := v.approve
	if v.seen != nil {
		*v.seen++
		if v.flipAfter > 0 && *v.seen > v.flipAfter {
			approved = true
		}
	}
	return aw.Result{Output: map[string]any{
		"approved": approved,
		"reason":   "because " + v.name,
	}}, nil
}

// byModel dispatches agents by their declared model name, so one plan can mix a
// generator and several judges.
func byModel(m map[string]aw.Backend) func(Agent) (aw.Backend, error) {
	return func(a Agent) (aw.Backend, error) {
		b, ok := m[a.Model]
		if !ok {
			return nil, &notFound{a.Model}
		}
		return b, nil
	}
}

type notFound struct{ model string }

func (e *notFound) Error() string { return "no backend for model " + e.model }

func gatedPlan(g *Gate) *Plan {
	return &Plan{
		Version: 1,
		Agents: map[string]Agent{
			"writer": {Backend: "x", Model: "gen"},
			"a":      {Backend: "x", Model: "yes"},
			"b":      {Backend: "x", Model: "yes2"},
			"c":      {Backend: "x", Model: "no"},
		},
		Steps: []Step{{ID: "draft", Agent: "writer", Prompt: "write", Output: map[string]Type{"text": {}}, Gate: g}},
	}
}

func TestGatedStepPasses(t *testing.T) {
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]aw.Backend{
		"gen": producer{}, "yes": voter{name: "a", approve: true},
		"yes2": voter{name: "b", approve: true}, "no": voter{name: "c", approve: false},
	})}
	p := gatedPlan(&Gate{Judges: []string{"a", "b", "c"}, Criteria: "be good"})
	done, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if done["draft"].Ref == "" {
		t.Fatal("an approved step must commit")
	}
}

// Fail closed: a panel that refuses stops the plan instead of passing the work
// downstream, and the error carries the objection.
func TestGatedStepFailsClosed(t *testing.T) {
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]aw.Backend{
		"gen": producer{}, "yes": voter{name: "a", approve: false},
		"yes2": voter{name: "b", approve: false}, "no": voter{name: "c", approve: false},
	})}
	p := gatedPlan(&Gate{Judges: []string{"a", "b", "c"}, Criteria: "be good", Attempts: 2})
	_, err := r.Run(context.Background(), p)
	if err == nil {
		t.Fatal("a rejected gate must fail the step, not pass it through")
	}
	if !strings.Contains(err.Error(), "because a") {
		t.Fatalf("error should carry the panel's objection, got: %v", err)
	}
}

// The repair loop runs inside the step: reject, feed the critique back, accept.
func TestGatedStepRepairs(t *testing.T) {
	var votes int
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]aw.Backend{
		"gen":  producer{},
		"yes":  voter{name: "a", flipAfter: 1, seen: &votes},
		"yes2": voter{name: "b", flipAfter: 1, seen: &votes},
		"no":   voter{name: "c", flipAfter: 1, seen: &votes},
	})}
	p := gatedPlan(&Gate{Judges: []string{"a", "b", "c"}, Criteria: "be good", Attempts: 3})
	done, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("the gate should have accepted the repaired attempt: %v", err)
	}
	if done["draft"].Ref == "" {
		t.Fatal("expected a committed result")
	}
}

// A state ref must travel as a REF into the next step's Invocation.Inputs, not
// be stringified into its prompt. This is what lets a workspace cross a step.
func TestRefInputsTravelAsRefs(t *testing.T) {
	var seen []aw.Invocation
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]aw.Backend{
		"gen": producer{seen: &seen},
	})}
	p := &Plan{
		Version: 1,
		Agents:  map[string]Agent{"w": {Backend: "x", Model: "gen"}},
		Steps: []Step{
			{ID: "first", Agent: "w", Prompt: "make it", Output: map[string]Type{"text": {}, "note": {}}},
			{ID: "second", Agent: "w", Prompt: "use it", Needs: []string{"first"},
				Inputs: map[string]string{
					"repo": "steps.first.workspace", // a produced ref
					"note": "steps.first.note",      // a scalar
				}},
		},
	}
	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 invocations, got %d", len(seen))
	}
	second := seen[1]
	ref, ok := second.Inputs["repo"]
	if !ok {
		t.Fatal("a produced ref must arrive in Invocation.Inputs")
	}
	if ref.URI != "tree-abc" || ref.Kind != aw.KindWorkspace {
		t.Fatalf("wrong ref: %+v", ref)
	}
	if strings.Contains(second.Prompt, "tree-abc") {
		t.Fatal("a ref must NOT be stringified into the prompt")
	}
	if !strings.Contains(second.Prompt, "make it") {
		t.Fatalf("a scalar input should still be rendered into the prompt:\n%s", second.Prompt)
	}
}

// Produced refs must survive a commit: recording only the scalars would silently
// break a workspace chain on the next run.
func TestProducedRefsSurviveACommit(t *testing.T) {
	dir := t.TempDir()
	blobs, err := store.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &Plan{
		Version: 1,
		Agents:  map[string]Agent{"w": {Backend: "x", Model: "gen"}},
		Steps:   []Step{{ID: "first", Agent: "w", Prompt: "make it", Output: map[string]Type{"text": {}}}},
	}
	mk := func() *Runner {
		return &Runner{Blobs: blobs, Journal: j, Backend: byModel(map[string]aw.Backend{"gen": producer{}})}
	}
	if _, err := mk().Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	// a second process reads it back purely from the journal + store
	done, err := mk().Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := done["first"].Produced["workspace"]
	if !ok {
		t.Fatal("the committed record dropped the step's produced refs")
	}
	if ref.URI != "tree-abc" {
		t.Fatalf("ref did not round-trip: %+v", ref)
	}
}

func TestGateValidation(t *testing.T) {
	bad := map[string]*Gate{
		"no judges":       {Criteria: "x"},
		"no criteria":     {Judges: []string{"a"}},
		"unknown judge":   {Judges: []string{"nope"}, Criteria: "x"},
		"quorum too high": {Judges: []string{"a"}, Criteria: "x", Quorum: 5},
	}
	for name, g := range bad {
		t.Run(name, func(t *testing.T) {
			if err := gatedPlan(g).validate(); err == nil {
				t.Fatal("expected validation to reject this gate")
			}
		})
	}
	ok := gatedPlan(&Gate{Judges: []string{"a", "b"}, Criteria: "x", Quorum: 2, Attempts: 1})
	if err := ok.validate(); err != nil {
		t.Fatalf("valid gate rejected: %v", err)
	}
}

// A provider's prompt cache is keyed on the exact leading tokens, so the input
// fold MUST be deterministic. This was a real bug: `for name := range s.Inputs`
// is a Go map range, randomized by construction, so any step with 2+ scalar
// inputs got different prompt bytes on every run and never hit the cache.
//
// The loop matters — a single recomputation passes by luck roughly 1/n! of the
// time it should fail.
func TestInputFoldIsDeterministic(t *testing.T) {
	p := &Plan{
		Version: 1,
		Agents:  map[string]Agent{"w": {Backend: "x", Model: "gen"}},
		Steps: []Step{
			{ID: "a", Agent: "w", Prompt: "A", Output: map[string]Type{"text": {}}},
			{ID: "b", Agent: "w", Prompt: "B", Output: map[string]Type{"text": {}}},
			{ID: "c", Agent: "w", Prompt: "C", Output: map[string]Type{"text": {}}},
			{ID: "d", Agent: "w", Prompt: "D", Output: map[string]Type{"text": {}}},
			{ID: "sink", Agent: "w", Prompt: "SINK", Inputs: map[string]string{
				"alpha":   "steps.a.text",
				"bravo":   "steps.b.text",
				"charlie": "steps.c.text",
				"delta":   "steps.d.text",
			}},
		},
	}
	var first string
	for i := 0; i < 100; i++ {
		var seen []aw.Invocation
		r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]aw.Backend{"gen": producer{seen: &seen}})}
		if _, err := r.Run(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		got := seen[len(seen)-1].Prompt // the sink step
		if i == 0 {
			first = got
			if !strings.Contains(got, "alpha") || !strings.Contains(got, "delta") {
				t.Fatalf("test is not exercising the fold; prompt was:\n%s", got)
			}
			continue
		}
		if got != first {
			t.Fatalf("run %d differs from run 0 — the fold is not deterministic\n--- 0 ---\n%s\n--- %d ---\n%s",
				i, first, i, got)
		}
	}
}

// counting emits a distinguishable payload per call, so a test can tell WHICH
// attempt's result was committed.
type counting struct{ n *int }

func (c counting) Name() string { return "counting" }
func (c counting) Invoke(_ context.Context, in aw.Invocation) (aw.Result, error) {
	*c.n++
	return aw.Result{Output: conform(in, fmt.Sprintf("attempt-%d", *c.n))}, nil
}

// approveOn votes yes only once the candidate carries the given marker.
type approveOn struct {
	name, marker string
}

func (a approveOn) Name() string { return a.name }
func (a approveOn) Invoke(_ context.Context, in aw.Invocation) (aw.Result, error) {
	return aw.Result{Output: map[string]any{
		"approved": strings.Contains(in.Prompt, a.marker),
		"reason":   "looking for " + a.marker,
	}}, nil
}

// The committed record must be the attempt the panel APPROVED, selected by the
// gate's own attempt index — not whatever the generator happened to produce last.
func TestCommitsTheApprovedAttemptNotTheLast(t *testing.T) {
	var n int
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]aw.Backend{
		"gen": counting{&n},
		"j1":  approveOn{"j1", "attempt-2"},
		"j2":  approveOn{"j2", "attempt-2"},
	})}
	p := &Plan{
		Version: 1,
		Agents: map[string]Agent{
			"w": {Backend: "x", Model: "gen"},
			"a": {Backend: "x", Model: "j1"},
			"b": {Backend: "x", Model: "j2"},
		},
		Steps: []Step{{ID: "draft", Agent: "w", Prompt: "write",
			Output: map[string]Type{"text": {}},
			Gate:   &Gate{Judges: []string{"a", "b"}, Criteria: "c", Attempts: 4}}},
	}
	done, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := done["draft"].Output["text"].(string)
	if !strings.Contains(got, "attempt-2") {
		t.Fatalf("committed the wrong attempt: %q (the panel approved attempt 2)", got)
	}
	if n != 2 {
		t.Fatalf("expected the gate to stop generating at the approved attempt, generated %d", n)
	}
}
