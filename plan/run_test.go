package plan

import (
	"context"
	"errors"
	"strings"
	"sync"
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

type fixedGenerator struct {
	output   map[string]any
	produced map[string]dawn.Ref
}

func (g fixedGenerator) Name() string           { return "fixed-generator" }
func (g fixedGenerator) CapturesTree()          {}
func (g fixedGenerator) MaterializesWorkspace() {}
func (g fixedGenerator) Invoke(context.Context, dawn.Invocation) (dawn.Result, error) {
	return dawn.Result{Output: g.output, Produced: g.produced}, nil
}

type recordingJudge struct {
	mu          sync.Mutex
	invocations []dawn.Invocation
	approveAt   int
}

func (j *recordingJudge) Name() string { return "recording-judge" }
func (j *recordingJudge) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.invocations = append(j.invocations, in)
	approved := j.approveAt == 0 || len(j.invocations) >= j.approveAt
	return dawn.Result{Output: map[string]any{"approved": approved, "reason": "make it concise"}}, nil
}

func (j *recordingJudge) calls() []dawn.Invocation {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]dawn.Invocation(nil), j.invocations...)
}

func judgeEvidencePlan() *Plan {
	return &Plan{Steps: map[string]Step{
		"first": {
			Agent: "x/first", Prompt: "Write the draft", Outputs: map[string]Type{"text": {}},
		},
		"second": {
			Agent: "x/second", Prompt: "Review the draft",
			Inputs:  map[string]string{"draft": "first.text", "repo": "first.workspace"},
			Outputs: map[string]Type{"summary": {}, "alpha": {}},
			Gate:    &Gate{Judges: []string{"x/judge"}, Criteria: "Approve concise summaries"},
		},
	}}
}

func TestJudgeReceivesCompleteGeneratorEvidence(t *testing.T) {
	judge := &recordingJudge{}
	secretURI := "workspace://must-not-leak"
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"first": fixedGenerator{
			output:   map[string]any{"text": "draft body"},
			produced: map[string]dawn.Ref{"workspace": {Kind: dawn.KindWorkspace, URI: secretURI}},
		},
		"second": fixedGenerator{output: map[string]any{"summary": "ok", "alpha": "first"}},
		"judge":  judge,
	})}
	if _, err := r.Run(context.Background(), judgeEvidencePlan()); err != nil {
		t.Fatal(err)
	}
	calls := judge.calls()
	if len(calls) != 1 {
		t.Fatalf("judge calls = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.System != "Approve concise summaries" {
		t.Fatalf("judge system = %q, want criteria", got.System)
	}
	wantEvidence := "Generator request:\nReview the draft\n\n--- input: draft ---\ndraft body" +
		"\n\nCaptured output:\n{\n  \"alpha\": \"first\",\n  \"summary\": \"ok\"\n}"
	if got.Prompt != wantEvidence {
		t.Fatalf("judge evidence mismatch\ngot:\n%s\nwant:\n%s", got.Prompt, wantEvidence)
	}
	if strings.Contains(got.Prompt, secretURI) {
		t.Fatalf("workspace URI leaked into textual evidence:\n%s", got.Prompt)
	}
}

func TestJudgeEvidenceIsStable(t *testing.T) {
	var first string
	for i := 0; i < 50; i++ {
		judge := &recordingJudge{}
		output := map[string]any{"summary": "ok", "alpha": "first"}
		r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
			"first": fixedGenerator{output: map[string]any{"text": "draft body"}, produced: map[string]dawn.Ref{
				"workspace": {Kind: dawn.KindWorkspace, URI: "workspace://must-not-leak"},
			}},
			"second": fixedGenerator{output: output},
			"judge":  judge,
		})}
		if _, err := r.Run(context.Background(), judgeEvidencePlan()); err != nil {
			t.Fatal(err)
		}
		got := judge.calls()[0].Prompt
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("judge evidence on run %d differs from run 0\nfirst:\n%s\nrun %d:\n%s", i, first, i, got)
		}
	}
}

type sequencedGenerator struct {
	seen   *[]dawn.Invocation
	output map[string]any
}

func (g sequencedGenerator) Name() string { return "sequenced-generator" }
func (g sequencedGenerator) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	*g.seen = append(*g.seen, in)
	return dawn.Result{Output: g.output}, nil
}

func TestJudgeEvidenceIncludesRepairFeedback(t *testing.T) {
	judge := &recordingJudge{approveAt: 2}
	var generated []dawn.Invocation
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"gen":   sequencedGenerator{seen: &generated, output: map[string]any{"text": "fixed candidate"}},
		"judge": judge,
	})}
	p := gated(&Gate{Judges: []string{"x/judge"}, Criteria: "be concise"})
	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	calls := judge.calls()
	if len(calls) != 2 || len(generated) != 2 {
		t.Fatalf("judge calls = %d, generator calls = %d, want 2 each", len(calls), len(generated))
	}
	if got := generated[0].Prompt; got != "write" {
		t.Fatalf("first generator prompt = %q, want stable base request", got)
	}
	if got := generated[1].Prompt; !strings.HasPrefix(got, "write\n\n"+rejectionHeadingForTest) ||
		!strings.Contains(got, "make it concise") {
		t.Fatalf("second generator prompt lacks appended repair feedback: %q", got)
	}

	const delimiter = "\n\nCaptured output:\n"
	request, output, found := strings.Cut(calls[1].Prompt, delimiter)
	if !found {
		t.Fatalf("repaired evidence lacks captured-output delimiter:\n%s", calls[1].Prompt)
	}
	if !strings.HasPrefix(request, "Generator request:\nwrite") {
		t.Fatalf("repair changed stable evidence prefix:\n%s", request)
	}
	for _, want := range []string{rejectionHeadingForTest, "make it concise"} {
		if !strings.Contains(request, want) {
			t.Errorf("generator request section missing %q:\n%s", want, request)
		}
		if strings.Contains(output, want) {
			t.Errorf("repair feedback leaked into captured output section %q:\n%s", want, output)
		}
	}
	if output != "{\n  \"text\": \"fixed candidate\"\n}" {
		t.Fatalf("captured output must be fixed generator output, got:\n%s", output)
	}
}

const rejectionHeadingForTest = "A prior version was REJECTED by the review panel. Address every objection:"

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

func TestGatedMissingExpectRepairsBeforeJudging(t *testing.T) {
	var generatorCalls int
	var generatorPrompts []dawn.Invocation
	var judgeCalls atomic.Int64
	p := gated(&Gate{Judges: []string{"x/a", "x/b", "x/c"}, Criteria: "be good"})
	step := p.Steps["draft"]
	step.Expect = []string{"dist/dawn"}
	p.Steps["draft"] = step
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"gen": captureFailingProducer{
			calls: &generatorCalls, seen: &generatorPrompts,
			err: &store.MissingPathError{Path: "dist/dawn"}, once: true,
		},
		"a": voter{name: "a", approve: true, seen: &judgeCalls},
		"b": voter{name: "b", approve: true, seen: &judgeCalls},
		"c": voter{name: "c", approve: true, seen: &judgeCalls},
	})}

	done, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("missing expected path should be repaired: %v", err)
	}
	if generatorCalls != 2 {
		t.Fatalf("generator calls = %d, want 2", generatorCalls)
	}
	if got := judgeCalls.Load(); got != 3 {
		t.Fatalf("judge calls = %d, want one panel round (3)", got)
	}
	if !strings.Contains(generatorPrompts[1].Prompt, "dist/dawn") {
		t.Fatalf("second prompt must name missing path, got %q", generatorPrompts[1].Prompt)
	}
	if got := done["draft"].Produced["workspace"].URI; got != "tree-repaired" {
		t.Fatalf("committed workspace = %q, want accepted attempt", got)
	}
}

func TestGatedMultipleMissingExpectUsesCanonicalOrderAndCommitsAttemptThree(t *testing.T) {
	var generatorCalls int
	var generatorPrompts []dawn.Invocation
	var judgeCalls atomic.Int64
	p := gated(&Gate{Judges: []string{"x/a", "x/b", "x/c"}, Criteria: "be good"})
	step := p.Steps["draft"]
	step.Expect = []string{"z/second", "a/first"}
	p.Steps["draft"] = step
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"gen": orderedMissingProducer{
			calls: &generatorCalls, seen: &generatorPrompts,
			missing: []string{"a/first", "z/second"}, successURI: "tree-attempt-3",
		},
		"a": voter{name: "a", approve: true, seen: &judgeCalls},
		"b": voter{name: "b", approve: true, seen: &judgeCalls},
		"c": voter{name: "c", approve: true, seen: &judgeCalls},
	})}

	done, err := r.Run(context.Background(), p)
	if err != nil {
		t.Fatalf("multiple missing paths should repair in canonical order: %v", err)
	}
	if generatorCalls != 3 {
		t.Fatalf("generator calls = %d, want 3", generatorCalls)
	}
	for i, inv := range generatorPrompts {
		if got := strings.Join(inv.Expect, ","); got != "a/first,z/second" {
			t.Fatalf("attempt %d expect order = %q, want canonical order", i+1, got)
		}
	}
	if !strings.Contains(generatorPrompts[1].Prompt, "a/first") ||
		!strings.Contains(generatorPrompts[2].Prompt, "z/second") {
		t.Fatalf("repair feedback did not follow canonical missing order: %#v", generatorPrompts)
	}
	if got := judgeCalls.Load(); got != 3 {
		t.Fatalf("judge calls = %d, want one panel round (3)", got)
	}
	if got := done["draft"].Produced["workspace"].URI; got != "tree-attempt-3" {
		t.Fatalf("committed workspace = %q, want accepted attempt 3", got)
	}
}

func TestGatedCaptureErrorRemainsMechanical(t *testing.T) {
	var generatorCalls int
	var generatorPrompts []dawn.Invocation
	var judgeCalls atomic.Int64
	captureErr := errors.New("capture exploded")
	p := gated(&Gate{Judges: []string{"x/a", "x/b", "x/c"}, Criteria: "be good"})
	step := p.Steps["draft"]
	step.Expect = []string{"dist/dawn"}
	p.Steps["draft"] = step
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"gen": captureFailingProducer{calls: &generatorCalls, seen: &generatorPrompts, err: captureErr},
		"a":   voter{name: "a", approve: true, seen: &judgeCalls},
		"b":   voter{name: "b", approve: true, seen: &judgeCalls},
		"c":   voter{name: "c", approve: true, seen: &judgeCalls},
	})}

	_, err := r.Run(context.Background(), p)
	if !errors.Is(err, captureErr) {
		t.Fatalf("ordinary capture error must remain mechanical, got %v", err)
	}
	if generatorCalls != 1 {
		t.Fatalf("mechanical error must consume no repair attempt, generator calls = %d", generatorCalls)
	}
	if got := judgeCalls.Load(); got != 0 {
		t.Fatalf("judge calls = %d, want 0", got)
	}
}

func TestGatedAllMissingPreservesFinalObjectionWithoutJudgesOrCommit(t *testing.T) {
	var generatorCalls int
	var generatorPrompts []dawn.Invocation
	var judgeCalls atomic.Int64
	journal, err := OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := gated(&Gate{Judges: []string{"x/a", "x/b", "x/c"}, Criteria: "be good"})
	step := p.Steps["draft"]
	step.Expect = []string{"a/first", "m/second", "z/final"}
	p.Steps["draft"] = step
	r := &Runner{Blobs: store.NewMem(), Journal: journal, Backend: byModel(map[string]dawn.Backend{
		"gen": orderedMissingProducer{
			calls: &generatorCalls, seen: &generatorPrompts,
			missing: []string{"a/first", "m/second", "z/final"},
		},
		"a": voter{name: "a", approve: true, seen: &judgeCalls},
		"b": voter{name: "b", approve: true, seen: &judgeCalls},
		"c": voter{name: "c", approve: true, seen: &judgeCalls},
	})}

	done, err := r.Run(context.Background(), p)
	var rejected *RejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("exhausted missing paths must be a rejection, got %v", err)
	}
	if rejected.Attempts != Attempts || generatorCalls != Attempts {
		t.Fatalf("attempts = %d, generator calls = %d, want %d", rejected.Attempts, generatorCalls, Attempts)
	}
	if got := judgeCalls.Load(); got != 0 {
		t.Fatalf("judge calls = %d, want 0", got)
	}
	if !strings.Contains(rejected.Objections, "z/final") {
		t.Fatalf("final objection lost synthetic reason: %q", rejected.Objections)
	}
	if _, committed := done["draft"]; committed {
		t.Fatal("exhausted rejected step must not commit")
	}
	entries, err := journal.Entries()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !strings.Contains(entries[0].Rejected, "z/final") {
		t.Fatalf("journal must preserve final missing path, got %+v", entries)
	}
}

func TestUngatedMissingPathErrorRemainsMechanical(t *testing.T) {
	var calls int
	var seen []dawn.Invocation
	missing := &store.MissingPathError{Path: "dist/dawn"}
	p := &Plan{Steps: map[string]Step{
		"build": {Agent: "x/gen", Prompt: "build", Expect: []string{"dist/dawn"}},
	}}
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"gen": captureFailingProducer{calls: &calls, seen: &seen, err: missing},
	})}

	done, err := r.Run(context.Background(), p)
	var gotMissing *store.MissingPathError
	if !errors.As(err, &gotMissing) || gotMissing.Path != "dist/dawn" {
		t.Fatalf("ungated missing path must remain mechanical, got %v", err)
	}
	var rejected *RejectedError
	if errors.As(err, &rejected) {
		t.Fatalf("ungated missing path became rejection: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if _, committed := done["build"]; committed {
		t.Fatal("mechanically failed step must not commit")
	}
}

func TestMalformedExpectIsAuthorValidationNotRepair(t *testing.T) {
	var calls int
	var seen []dawn.Invocation
	p := &Plan{Steps: map[string]Step{
		"build": {Agent: "x/gen", Prompt: "build", Expect: []string{"../escape"}, Gate: &Gate{
			Judges: []string{"x/judge"}, Criteria: "safe",
		}},
	}}
	r := &Runner{Blobs: store.NewMem(), Backend: byModel(map[string]dawn.Backend{
		"gen":   captureFailingProducer{calls: &calls, seen: &seen, err: &store.MissingPathError{Path: "../escape"}},
		"judge": voter{name: "judge", approve: true},
	})}

	_, err := r.Run(context.Background(), p)
	var validation *ValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("malformed expect must be author validation, got %T: %v", err, err)
	}
	var rejected *RejectedError
	if errors.As(err, &rejected) {
		t.Fatalf("malformed expect entered repair: %v", err)
	}
	if calls != 0 {
		t.Fatalf("validation must precede generation, calls = %d", calls)
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

type orderedMissingProducer struct {
	calls      *int
	seen       *[]dawn.Invocation
	missing    []string
	successURI string
}

func (p orderedMissingProducer) Name() string  { return "ordered-missing-producer" }
func (p orderedMissingProducer) CapturesTree() {}
func (p orderedMissingProducer) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	*p.calls++
	*p.seen = append(*p.seen, in)
	if *p.calls <= len(p.missing) {
		return dawn.Result{}, &store.MissingPathError{Path: p.missing[*p.calls-1]}
	}
	return dawn.Result{
		Output:   conform(in, in.Prompt),
		Produced: map[string]dawn.Ref{"workspace": {Kind: dawn.KindWorkspace, URI: p.successURI}},
	}, nil
}

type captureFailingProducer struct {
	calls *int
	seen  *[]dawn.Invocation
	err   error
	once  bool
}

func (p captureFailingProducer) Name() string  { return "capture-failing-producer" }
func (p captureFailingProducer) CapturesTree() {}
func (p captureFailingProducer) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	*p.calls++
	*p.seen = append(*p.seen, in)
	if !p.once || *p.calls == 1 {
		return dawn.Result{}, p.err
	}
	return dawn.Result{
		Output:   conform(in, in.Prompt),
		Produced: map[string]dawn.Ref{"workspace": {Kind: dawn.KindWorkspace, URI: "tree-repaired"}},
	}, nil
}

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

func TestRunAndStatusRejectInvalidRedoBeforeBackendUse(t *testing.T) {
	p := &Plan{Steps: map[string]Step{"draft": {Agent: "x/model", Prompt: "write"}}}
	for _, tc := range []struct {
		name string
		redo map[string]bool
		want string
	}{
		{name: "empty", redo: map[string]bool{"": true}, want: "needs a step name"},
		{name: "unknown", redo: map[string]bool{"missing": true}, want: "unknown step"},
	} {
		for _, operation := range []string{"run", "status"} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				var resolved, invoked int
				r := &Runner{Blobs: store.NewMem(), Redo: tc.redo, Backend: func(Agent) (dawn.Backend, error) {
					resolved++
					return textProducer{calls: &invoked}, nil
				}}
				var err error
				if operation == "run" {
					_, err = r.Run(context.Background(), p)
				} else {
					_, err = r.Status(p)
				}
				var validation *ValidationError
				if !errors.As(err, &validation) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %T %v, want ValidationError containing %q", err, err, tc.want)
				}
				if resolved != 0 || invoked != 0 {
					t.Fatalf("invalid redo used backend: resolved=%d invoked=%d", resolved, invoked)
				}
			})
		}
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
