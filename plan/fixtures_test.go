package plan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/store"
)

// conform emits exactly the fields the pushed schema requires, echoing v into each
// free-form one and picking the first member of an enum. A fake that emitted
// anything else would fail Step.Validate, which is the point.
func conform(in aw.Invocation, v string) map[string]any {
	out := map[string]any{}
	req, _ := in.Schema["required"].([]any)
	props, _ := in.Schema["properties"].(map[string]any)
	for _, f := range req {
		name, _ := f.(string)
		if pd, ok := props[name].(map[string]any); ok {
			if en, ok := pd["enum"].([]any); ok && len(en) > 0 {
				out[name] = en[0]
				continue
			}
		}
		out[name] = v
	}
	return out
}

// echo records how many times it ran and echoes the prompt into every declared field.
type echo struct{ calls *int }

func (e echo) Name() string { return "echo" }
func (e echo) Invoke(_ context.Context, in aw.Invocation) (aw.Result, error) {
	*e.calls++
	return aw.Result{Output: conform(in, in.Prompt)}, nil
}

// producer also emits a workspace ref, standing in for a tree-capturing backend.
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

func (producer) CapturesTree() {}

// counting emits a distinguishable payload per call so a test can tell WHICH
// attempt's result was committed.
type counting struct{ n *int }

func (c counting) Name() string { return "counting" }
func (c counting) Invoke(_ context.Context, in aw.Invocation) (aw.Result, error) {
	*c.n++
	return aw.Result{Output: conform(in, fmt.Sprintf("attempt-%d", *c.n))}, nil
}

// voter approves or rejects, flipping to approve after flipAfter rejections.
type voter struct {
	name      string
	approve   bool
	flipAfter int
	seen      *int
}

func (v voter) Name() string { return v.name }
func (v voter) Invoke(context.Context, aw.Invocation) (aw.Result, error) {
	approved := v.approve
	if v.seen != nil {
		*v.seen++
		if v.flipAfter > 0 && *v.seen > v.flipAfter {
			approved = true
		}
	}
	return aw.Result{Output: map[string]any{"approved": approved, "reason": "because " + v.name}}, nil
}

// byModel dispatches on the model half of an agent spec, so one plan can mix a
// generator and several judges.
func byModel(m map[string]aw.Backend) func(Agent) (aw.Backend, error) {
	return func(a Agent) (aw.Backend, error) {
		b, ok := m[a.Model]
		if !ok {
			return nil, fmt.Errorf("no backend for model %q", a.Model)
		}
		return b, nil
	}
}

// q is a pointer helper: an omitted quorum and an explicit one are different
// things, which is the whole reason the field is a pointer.
func q(n int) *int { return &n }

func loadPlan(t *testing.T, yaml string) (*Plan, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

// durable builds a runner backed by a real store and journal in dir.
func durable(t *testing.T, dir string, b func(Agent) (aw.Backend, error)) *Runner {
	t.Helper()
	blobs, err := store.NewFS(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Blobs: blobs, Journal: j, Backend: b}
}

const head = "steps:\n"
