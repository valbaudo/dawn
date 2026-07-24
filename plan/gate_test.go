package plan

import (
	"context"
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
	out := map[string]any{"text": in.Prompt, "note": "hello"}
	return aw.Result{
		Output:   out,
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
		Steps: []Step{{ID: "draft", Agent: "writer", Prompt: "write", Output: "text", Gate: g}},
	}
}

func TestGatedStepPasses(t *testing.T) {
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]aw.Backend{
		"gen": producer{}, "yes": voter{name: "a", approve: true},
		"yes2": voter{name: "b", approve: true}, "no": voter{name: "c", approve: false},
	})}
	p := gatedPlan(&Gate{Judges: []string{"a", "b", "c"}, Criteria: "be good"})
	done, err := r.Run(context.Background(), p, nil)
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
	_, err := r.Run(context.Background(), p, nil)
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
	done, err := r.Run(context.Background(), p, nil)
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
			{ID: "first", Agent: "w", Prompt: "make it", Output: "text"},
			{ID: "second", Agent: "w", Prompt: "use it", Needs: []string{"first"},
				Inputs: map[string]Source{
					"repo": {From: "steps.first.workspace"}, // a produced ref
					"note": {From: "steps.first.note"},      // a scalar
				}},
		},
	}
	if _, err := r.Run(context.Background(), p, nil); err != nil {
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
	if !strings.Contains(second.Prompt, "hello") {
		t.Fatal("a scalar input should still be rendered into the prompt")
	}
}

// Produced refs must survive a checkpoint: committing only the scalars would
// silently break a workspace chain on resume.
func TestProducedRefsSurviveResume(t *testing.T) {
	blobs := store.NewMem()
	r := &Runner{Blobs: blobs, Backend: byModel(map[string]aw.Backend{"gen": producer{}})}
	p := &Plan{
		Version: 1,
		Agents:  map[string]Agent{"w": {Backend: "x", Model: "gen"}},
		Steps:   []Step{{ID: "first", Agent: "w", Prompt: "make it", Output: "text"}},
	}
	done, err := r.Run(context.Background(), p, nil)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := Reload(blobs, Refs(done))
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := reloaded["first"].Produced["workspace"]
	if !ok {
		t.Fatal("resume dropped the step's produced refs")
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
