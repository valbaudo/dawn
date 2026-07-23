package plan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/store"
)

// echoBackend records how many times it ran and echoes the prompt into every
// field the step asked for — enough to prove input wiring and resume with no
// network.
type echoBackend struct{ calls *int }

func (e echoBackend) Name() string { return "echo" }
func (e echoBackend) Invoke(_ context.Context, in aw.Invocation) (aw.Result, error) {
	*e.calls++
	out := map[string]any{"text": in.Prompt}
	if in.Schema != nil {
		if req, ok := in.Schema["required"].([]any); ok {
			for _, f := range req {
				out[f.(string)] = in.Prompt
			}
		}
	}
	return aw.Result{Output: out}, nil
}

func runner(calls *int) (*Runner, store.Blobs) {
	b := store.NewMem()
	return &Runner{Blobs: b, Backend: func(Agent) (aw.Backend, error) { return echoBackend{calls}, nil }}, b
}

var twoStep = &Plan{
	Version: 1,
	Agents:  map[string]Agent{"a": {Backend: "echo"}},
	Steps: []Step{
		{ID: "first", Agent: "a", Prompt: "hello", Output: "msg"},
		{ID: "second", Agent: "a", Prompt: "use the input", Needs: []string{"first"},
			Inputs: map[string]Source{"prior": {From: "steps.first.msg"}}, Output: "msg"},
	},
}

func TestRunWiresTypedInputs(t *testing.T) {
	var calls int
	r, _ := runner(&calls)
	done, err := r.Run(context.Background(), twoStep, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 invocations, got %d", calls)
	}
	// second's prompt must carry first's output (input resolution, not templating).
	secondPrompt, _ := done["second"].Output["text"].(string)
	if !strings.Contains(secondPrompt, "hello") {
		t.Fatalf("second step did not receive first's output:\n%s", secondPrompt)
	}
	if done["first"].Ref == "" || done["second"].Ref == "" {
		t.Fatal("every step must commit a ref")
	}
}

func TestResumeSkipsCommittedSteps(t *testing.T) {
	var calls int
	r, blobs := runner(&calls)
	done, err := r.Run(context.Background(), twoStep, nil)
	if err != nil {
		t.Fatal(err)
	}
	// simulate a fresh process: rebuild done from the committed refs, then re-run.
	reloaded, err := Reload(blobs, Refs(done))
	if err != nil {
		t.Fatal(err)
	}
	calls = 0
	r2, _ := runner(&calls)
	r2.Blobs = blobs
	if _, err := r2.Run(context.Background(), twoStep, reloaded); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("resume must skip committed steps, but ran %d", calls)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	// a control-flow keyword the format forbids must be a hard parse error.
	bad := "version: 1\nagents:\n  a: {backend: claude}\nsteps:\n  - id: s\n    agent: a\n    prompt: hi\n    loop: 3\n"
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load must reject an unknown key like `loop:`")
	}
}

func TestLoadParsesValidPlan(t *testing.T) {
	dir := t.TempDir()
	good := "version: 1\n" +
		"agents:\n  writer: {backend: claude, model: sonnet}\n" +
		"steps:\n" +
		"  - id: draft\n    agent: writer\n    prompt: write\n    output: text\n" +
		"  - id: edit\n    agent: writer\n    needs: [draft]\n    prompt: edit\n" +
		"    inputs:\n      d: {from: steps.draft.text}\n    output: text\n"
	path := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	order, err := p.order()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "draft" || order[1] != "edit" {
		t.Fatalf("topological order wrong: %v", order)
	}
}

func TestLoadDetectsCycle(t *testing.T) {
	p := &Plan{Version: 1, Agents: map[string]Agent{"a": {}},
		Steps: []Step{
			{ID: "x", Agent: "a", Prompt: "p", Needs: []string{"y"}},
			{ID: "y", Agent: "a", Prompt: "p", Needs: []string{"x"}},
		}}
	if err := p.validate(); err == nil {
		t.Fatal("a dependency cycle must be rejected")
	}
}
