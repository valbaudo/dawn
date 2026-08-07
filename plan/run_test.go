package plan

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/store"
)

func gen(model string) map[string]Step {
	return map[string]Step{"draft": {Agent: "x/" + model, Prompt: "write", Outputs: map[string]Type{"text": {}}}}
}

func TestRunWiresTypedInputs(t *testing.T) {
	var calls int
	p := &Plan{Steps: map[string]Step{
		"first":  {Agent: "x/echo", Prompt: "hello", Outputs: map[string]Type{"msg": {}}},
		"second": {Agent: "x/echo", Prompt: "use it", Inputs: map[string]string{"prior": "first.msg"}, Outputs: map[string]Type{"msg": {}}},
	}}
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{"echo": echo{&calls}})}
	done, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 invocations, got %d", calls)
	}
	if got, _ := done["second"].Output["msg"].(string); !strings.Contains(got, "hello") {
		t.Fatalf("second step did not receive first's output:\n%s", got)
	}
}

// A state ref travels as a REF into Invocation.Inputs; a scalar is rendered into
// the prompt. That distinction is what lets a workspace cross a step.
func TestRefInputsTravelAsRefs(t *testing.T) {
	var seen []dawn.Invocation
	p := &Plan{Steps: map[string]Step{
		"first":  {Agent: "x/gen", Prompt: "make it", Outputs: map[string]Type{"text": {}, "note": {}}},
		"second": {Agent: "x/gen", Prompt: "use it", Inputs: map[string]string{"repo": "first.workspace", "note": "first.note"}},
	}}
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{"gen": workspaceConsumer{producer{seen: &seen}}})}
	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	second := seen[1]
	ref, ok := second.Inputs["repo"]
	if !ok || ref.URI != "tree-abc" || ref.Kind != dawn.KindWorkspace {
		t.Fatalf("a produced ref must arrive in Invocation.Inputs: %+v", second.Inputs)
	}
	if strings.Contains(second.Prompt, "tree-abc") {
		t.Fatal("a ref must NOT be stringified into the prompt")
	}
	if !strings.Contains(second.Prompt, "make it") {
		t.Fatalf("a scalar input should still be rendered:\n%s", second.Prompt)
	}
}

// A provider's cache is keyed on the exact leading tokens, so the input fold must
// be deterministic. A single recomputation passes by luck; the loop is the test.
func TestInputFoldIsDeterministic(t *testing.T) {
	p := &Plan{Steps: map[string]Step{
		"a":    {Agent: "x/echo", Prompt: "A", Outputs: map[string]Type{"text": {}}},
		"b":    {Agent: "x/echo", Prompt: "B", Outputs: map[string]Type{"text": {}}},
		"c":    {Agent: "x/echo", Prompt: "C", Outputs: map[string]Type{"text": {}}},
		"d":    {Agent: "x/echo", Prompt: "D", Outputs: map[string]Type{"text": {}}},
		"sink": {Agent: "x/echo", Prompt: "SINK", Inputs: map[string]string{"alpha": "a.text", "bravo": "b.text", "charlie": "c.text", "delta": "d.text"}},
	}}
	var first string
	for i := 0; i < 100; i++ {
		var seen []dawn.Invocation
		r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{"echo": producer{seen: &seen}})}
		if _, err := r.Run(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		got := seen[len(seen)-1].Prompt
		if i == 0 {
			first = got
			if !strings.Contains(got, "alpha") || !strings.Contains(got, "delta") {
				t.Fatalf("test is not exercising the fold:\n%s", got)
			}
			continue
		}
		if got != first {
			t.Fatalf("run %d differs from run 0 — the fold is not deterministic", i)
		}
	}
}

func gated(g *Gate) *Plan {
	s := gen("gen")["draft"]
	s.Gate = g
	return &Plan{Steps: map[string]Step{"draft": s}}
}

func panel(judges map[string]dawn.Backend) func(Agent) (dawn.Backend, error) {
	judges["gen"] = producer{}
	return byModel(judges)
}

func TestGatedStepPasses(t *testing.T) {
	r := &Runner{Blobs: store.NewMem(), Backend: panel(map[string]dawn.Backend{
		"a": voter{name: "a", approve: true}, "b": voter{name: "b", approve: true}, "c": voter{name: "c"},
	})}
	done, err := r.Run(context.Background(), gated(&Gate{Judges: []string{"x/a", "x/b", "x/c"}, Criteria: "be good"}))
	if err != nil {
		t.Fatal(err)
	}
	if done["draft"].Ref == "" {
		t.Fatal("an approved step must commit")
	}
}

// Fail closed: a panel that refuses stops the plan, and the error carries why.
func TestGatedStepFailsClosed(t *testing.T) {
	r := &Runner{Blobs: store.NewMem(), Backend: panel(map[string]dawn.Backend{
		"a": voter{name: "a"}, "b": voter{name: "b"}, "c": voter{name: "c"},
	})}
	_, err := r.Run(context.Background(), gated(&Gate{Judges: []string{"x/a", "x/b", "x/c"}, Criteria: "be good"}))
	if err == nil {
		t.Fatal("a rejected gate must fail the step, not pass it through")
	}
	if !strings.Contains(err.Error(), "because a") {
		t.Fatalf("error should carry the objection, got: %v", err)
	}
}

func TestGatedStepRepairs(t *testing.T) {
	var votes atomic.Int64
	r := &Runner{Blobs: store.NewMem(), Backend: panel(map[string]dawn.Backend{
		"a": voter{name: "a", flipAfter: 1, seen: &votes},
		"b": voter{name: "b", flipAfter: 1, seen: &votes},
		"c": voter{name: "c", flipAfter: 1, seen: &votes},
	})}
	done, err := r.Run(context.Background(), gated(&Gate{Judges: []string{"x/a", "x/b", "x/c"}, Criteria: "be good"}))
	if err != nil {
		t.Fatalf("the gate should have accepted the repaired attempt: %v", err)
	}
	if done["draft"].Ref == "" {
		t.Fatal("expected a committed result")
	}
}

// approveOn votes yes only once the candidate carries the marker.
type approveOn struct{ name, marker string }

func (a approveOn) Name() string { return a.name }
func (a approveOn) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	return dawn.Result{Output: map[string]any{
		"approved": strings.Contains(in.Prompt, a.marker), "reason": "looking for " + a.marker,
	}}, nil
}

// The committed record must be the attempt the panel APPROVED, by index — not
// whatever the generator happened to produce last.
func TestCommitsTheApprovedAttemptNotTheLast(t *testing.T) {
	var n int
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"gen": counting{&n}, "j1": approveOn{"j1", "attempt-2"}, "j2": approveOn{"j2", "attempt-2"},
	})}
	done, err := r.Run(context.Background(), gated(&Gate{Judges: []string{"x/j1", "x/j2"}, Criteria: "c", Quorum: q(2)}))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := done["draft"].Output["text"].(string); !strings.Contains(got, "attempt-2") {
		t.Fatalf("committed the wrong attempt: %q", got)
	}
	if n != 2 {
		t.Fatalf("expected generation to stop at the approved attempt, got %d", n)
	}
}

type textProducer struct{ calls *int }

func (p textProducer) Name() string { return "text-producer" }
func (p textProducer) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	*p.calls++
	return dawn.Result{Output: conform(in, in.Prompt)}, nil
}

type treeProducer struct{ producer }

func (treeProducer) CapturesTree() {}

type workspaceConsumer struct{ producer }

func (workspaceConsumer) MaterializesWorkspace() {}

func TestStatusRunsTheSamePreflightAsRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		plan *Plan
		want string
	}{
		{
			name: "missing root",
			plan: &Plan{Steps: map[string]Step{
				"edit": {Agent: "x/workspace", Prompt: "edit", Inputs: map[string]string{"repo": "in.workspace"}},
			}},
			want: "--in",
		},
		{
			name: "text backend referenced as workspace",
			plan: &Plan{Steps: map[string]Step{
				"draft": {Agent: "x/text", Prompt: "draft"},
				"edit":  {Agent: "x/workspace", Prompt: "edit", Inputs: map[string]string{"repo": "draft.workspace"}},
			}},
			want: "captures no tree",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls int
			r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
				"text": textProducer{calls: &calls}, "workspace": workspaceConsumer{},
			})}
			_, err := r.Status(tc.plan)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected preflight error containing %q, got: %v", tc.want, err)
			}
			var validation *ValidationError
			if !errors.As(err, &validation) {
				t.Fatalf("status preflight failure must be ValidationError, got %T: %v", err, err)
			}
			if calls != 0 {
				t.Fatalf("status must not invoke backends, but %d invocations ran", calls)
			}
		})
	}
}

func TestPreflightResolvesAllBackendsBeforeCapabilityValidation(t *testing.T) {
	var calls int
	var resolved []string
	p := &Plan{Steps: map[string]Step{
		"a-invalid": {Agent: "x/invalid", Prompt: "fail", Expect: []string{"artifact"}},
		"b-later": {
			Agent: "x/later", Prompt: "later",
			Gate: &Gate{Judges: []string{"x/judge-a", "x/judge-b"}, Criteria: "correct"},
		},
	}}
	r := &Runner{Backend: func(a Agent) (dawn.Backend, error) {
		resolved = append(resolved, a.Model)
		return textProducer{calls: &calls}, nil
	}}

	err := r.preflight(p)
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("preflight failure must be a ValidationError, got %T: %v", err, err)
	}
	if got, want := strings.Join(resolved, ","), "invalid,later,judge-a,judge-b"; got != want {
		t.Fatalf("all generators and judges must resolve before capability validation: got %q, want %q", got, want)
	}
	if calls != 0 {
		t.Fatalf("preflight must not invoke backends, but %d invocations ran", calls)
	}
}

func TestPreflightRejectsReservedRefsFromNonTreeBackend(t *testing.T) {
	for _, field := range []string{"workspace", "diff"} {
		t.Run(field, func(t *testing.T) {
			var calls int
			p := &Plan{Steps: map[string]Step{
				"upstream": {Agent: "x/text", Prompt: "produce", Outputs: map[string]Type{"text": {}}},
				"downstream": {
					Agent: "x/text", Prompt: "consume",
					Inputs: map[string]string{"repo": "upstream." + field},
				},
			}}
			r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
				"text": textProducer{calls: &calls},
			})}

			_, err := r.Run(context.Background(), p)
			if err == nil || !strings.Contains(err.Error(), "upstream") ||
				!strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "captures no tree") {
				t.Fatalf("expected reserved ref preflight rejection, got: %v", err)
			}
			if calls != 0 {
				t.Fatalf("preflight must precede execution, but %d invocations ran", calls)
			}
		})
	}
}

func TestPreflightRejectsWorkspaceInputForNonMaterializingBackend(t *testing.T) {
	var calls int
	p := &Plan{Steps: map[string]Step{
		"upstream": {Agent: "x/tree", Prompt: "produce"},
		"downstream": {
			Agent: "x/text", Prompt: "consume",
			Inputs: map[string]string{"repo": "upstream.workspace"},
		},
	}}
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"tree": treeProducer{}, "text": textProducer{calls: &calls},
	})}

	_, err := r.Run(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "downstream") ||
		!strings.Contains(err.Error(), "repo") || !strings.Contains(err.Error(), "cannot materialize a workspace") {
		t.Fatalf("expected workspace consumer preflight rejection, got: %v", err)
	}
	if calls != 0 {
		t.Fatalf("preflight must precede execution, but %d invocations ran", calls)
	}
}

func TestPreflightAcceptsTreeProducerAndWorkspaceConsumer(t *testing.T) {
	var seen []dawn.Invocation
	p := &Plan{Steps: map[string]Step{
		"upstream": {Agent: "x/tree", Prompt: "produce"},
		"downstream": {
			Agent: "x/workspace", Prompt: "consume",
			Inputs: map[string]string{"repo": "upstream.workspace"},
		},
	}}
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"tree": treeProducer{}, "workspace": workspaceConsumer{producer{seen: &seen}},
	})}

	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 {
		t.Fatalf("expected the workspace consumer to run once, got %d", len(seen))
	}
}

// `expect:` is a postcondition on a captured tree, so a text-only agent has
// nothing to assert against — and the check runs before ANYTHING executes.
func TestExpectRequiresATreeCapturingBackend(t *testing.T) {
	var calls int
	p := &Plan{Steps: map[string]Step{
		"build": {Agent: "x/echo", Prompt: "build it", Expect: []string{"dist/dawn"}},
	}}
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{"echo": echo{&calls}})}
	_, err := r.Run(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "captures no tree") {
		t.Fatalf("expected a preflight rejection, got: %v", err)
	}
	if calls != 0 {
		t.Fatalf("the check must precede execution, but %d invocations ran", calls)
	}
}

// Referencing the reserved root step without --in is caught before anything runs.
func TestRootStepRequiresIn(t *testing.T) {
	var calls int
	p := &Plan{Steps: map[string]Step{
		"a": {Agent: "x/echo", Prompt: "p", Inputs: map[string]string{"repo": "in.workspace"}},
	}}
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{"echo": echo{&calls}})}
	if _, err := r.Run(context.Background(), p); err == nil {
		t.Fatal("in.workspace without --in must fail")
	}
	if calls != 0 {
		t.Fatal("and must fail before anything runs")
	}
}

func TestRootStepSuppliesAWorkspace(t *testing.T) {
	var seen []dawn.Invocation
	p := &Plan{Steps: map[string]Step{
		"a": {Agent: "x/gen", Prompt: "p", Inputs: map[string]string{"repo": "in.workspace"}},
	}}
	root := dawn.Ref{Kind: dawn.KindWorkspace, URI: "tree-root"}
	r := &Runner{Blobs: store.NewMem(), Root: &root, Backend: byModel(map[string]dawn.Backend{"gen": workspaceConsumer{producer{seen: &seen}}})}
	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if got := seen[0].Inputs["repo"]; got.URI != "tree-root" {
		t.Fatalf("the root tree must arrive as a ref, got %+v", got)
	}
}

// interrupted is a judge that fails the way a real one does when a signal
// cancels the run out from under it.
type interrupted struct{ name string }

func (i interrupted) Name() string { return i.name }
func (i interrupted) Invoke(ctx context.Context, _ dawn.Invocation) (dawn.Result, error) {
	return dawn.Result{}, ctx.Err()
}

// An interrupt is not a verdict.
//
// SIGTERM cancels every in-flight judge at once, so a cancelled gate looks
// exactly like a unanimous no: zero approvals. Counting that as a rejection
// would write a permanent "the panel refused" into the journal for work no judge
// ever read, and burn the repair budget on it. Cancellation must stay mechanical.
func TestInterruptDuringGateIsNotARejection(t *testing.T) {
	j, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the signal landed before the panel could vote

	r := &Runner{Blobs: store.NewMem(), Journal: j, Backend: panel(map[string]dawn.Backend{
		"a": interrupted{"a"}, "b": interrupted{"b"}, "c": interrupted{"c"},
	})}
	_, err = r.Run(ctx, gated(&Gate{Judges: []string{"x/a", "x/b", "x/c"}, Criteria: "be good"}))
	if err == nil {
		t.Fatal("a cancelled run must fail")
	}
	var rej *RejectedError
	if errors.As(err, &rej) {
		t.Fatalf("cancellation reported as a rejection: %v", err)
	}

	entries, err := j.Entries()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Rejected != "" {
			t.Fatalf("cancellation wrote a rejection to the journal: %q", e.Rejected)
		}
	}
}
