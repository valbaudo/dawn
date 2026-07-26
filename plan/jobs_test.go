package plan

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/store"
)

// tracker records concurrency: how many invocations overlapped at the peak, and
// the order steps started in.
type tracker struct {
	mu      sync.Mutex
	live    int
	peak    int
	started []string
}

func (t *tracker) enter(prompt string) {
	t.mu.Lock()
	t.live++
	if t.live > t.peak {
		t.peak = t.live
	}
	t.started = append(t.started, prompt)
	t.mu.Unlock()
}

func (t *tracker) leave() {
	t.mu.Lock()
	t.live--
	t.mu.Unlock()
}

// blocker rendezvouses the steps named in barrier: each waits until `need` of
// them have arrived. Steps outside the barrier pass straight through.
//
// A rendezvous rather than a sleep, so "these two overlapped" is proven by the
// run COMPLETING. A scheduler that serialized them would deadlock, where a
// sleep-and-measure test could pass by luck on a fast machine.
type blocker struct {
	t       *tracker
	barrier map[string]bool
	need    int
	arrived *atomic.Int64
	gate    chan struct{}
}

func (blocker) Name() string { return "blocker" }
func (b blocker) Invoke(ctx context.Context, in dawn.Invocation) (dawn.Result, error) {
	name := stepName(in.Prompt)
	b.t.enter(name)
	defer b.t.leave()
	if b.barrier[name] {
		if int(b.arrived.Add(1)) == b.need {
			close(b.gate)
		}
		select {
		case <-b.gate:
		case <-ctx.Done():
			return dawn.Result{}, ctx.Err()
		}
	}
	return dawn.Result{Output: fields(in.Schema, in.Prompt)}, nil
}

// fields answers with every field the step declared, so a fixture never fails
// validation for a reason the test is not about.
func fields(schema map[string]any, v string) map[string]any {
	out := map[string]any{}
	props, _ := schema["properties"].(map[string]any)
	for name := range props {
		out[name] = v
	}
	if len(out) == 0 {
		out["text"] = v
	}
	return out
}

// stepName recovers a step from its prompt: bind appends input blocks after a
// blank line, so the first line is what the author wrote.
func stepName(prompt string) string { return strings.SplitN(prompt, "\n", 2)[0] }

// free is a blocker that never blocks, for tests about ordering rather than overlap.
func free(t *tracker) dawn.Backend {
	var n atomic.Int64
	return blocker{t: t, arrived: &n, gate: make(chan struct{})}
}

// diamond: root -> {left, right} -> merge. left and right are independent.
func diamond() *Plan {
	return &Plan{Steps: map[string]Step{
		"root":  {Agent: "x/b", Prompt: "root", Outputs: map[string]Type{"text": {}}},
		"left":  {Agent: "x/b", Prompt: "left", Inputs: map[string]string{"x": "root.text"}, Outputs: map[string]Type{"text": {}}},
		"right": {Agent: "x/b", Prompt: "right", Inputs: map[string]string{"x": "root.text"}, Outputs: map[string]Type{"text": {}}},
		"merge": {Agent: "x/b", Prompt: "merge", Inputs: map[string]string{"a": "left.text", "b": "right.text"}, Outputs: map[string]Type{"text": {}}},
	}}
}

// The point of --jobs. left and right have no edge between them, so they must be
// in flight at the same time. Both backends block until BOTH have arrived, so a
// scheduler that serializes them deadlocks instead of merely being slow — this
// cannot pass by luck on a fast machine.
func TestJobsRunsIndependentStepsConcurrently(t *testing.T) {
	tr := &tracker{}
	var arrived atomic.Int64
	back := blocker{t: tr, barrier: map[string]bool{"left": true, "right": true}, need: 2,
		arrived: &arrived, gate: make(chan struct{})}
	r := &Runner{Blobs: store.NewMem(), Jobs: 2,
		Backend: byModel(map[string]dawn.Backend{"b": back})}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done, err := r.Run(ctx, diamond())
	if err != nil {
		t.Fatalf("left and right never overlapped, so the rendezvous deadlocked: %v", err)
	}
	if len(done) != 4 {
		t.Fatalf("expected 4 committed steps, got %d", len(done))
	}
	if tr.peak < 2 {
		t.Fatalf("peak concurrency %d: independent steps did not overlap", tr.peak)
	}
}

// --jobs 1 must NOT overlap them: the same plan that rendezvouses at 2 must time
// out at 1, which is what proves the flag is the thing doing the work.
func TestJobsOneDoesNotOverlap(t *testing.T) {
	tr := &tracker{}
	var arrived atomic.Int64
	back := blocker{t: tr, barrier: map[string]bool{"left": true, "right": true}, need: 2,
		arrived: &arrived, gate: make(chan struct{})}
	r := &Runner{Blobs: store.NewMem(), Jobs: 1,
		Backend: byModel(map[string]dawn.Backend{"b": back})}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := r.Run(ctx, diamond()); err == nil {
		t.Fatal("--jobs 1 must run left and right one at a time, so the rendezvous cannot complete")
	}
	if tr.peak != 1 {
		t.Fatalf("--jobs 1 ran %d steps at once", tr.peak)
	}
}

// --jobs must never break the DAG. root precedes left and right; merge follows
// both. Concurrency is allowed to reorder siblings, never dependencies.
func TestJobsRespectsDependencies(t *testing.T) {
	for _, jobs := range []int{1, 2, 4, 8} {
		tr := &tracker{}
		r := &Runner{Blobs: store.NewMem(), Jobs: jobs,
			Backend: byModel(map[string]dawn.Backend{"b": free(tr)})}
		if _, err := r.Run(context.Background(), diamond()); err != nil {
			t.Fatalf("jobs=%d: %v", jobs, err)
		}
		at := map[string]int{}
		for i, name := range tr.started {
			at[name] = i
		}
		if at["root"] > at["left"] || at["root"] > at["right"] {
			t.Fatalf("jobs=%d: root must start before its dependents: %v", jobs, tr.started)
		}
		if at["merge"] < at["left"] || at["merge"] < at["right"] {
			t.Fatalf("jobs=%d: merge must start after both branches: %v", jobs, tr.started)
		}
	}
}

// --jobs 1 must reproduce the old sequential order exactly, not merely some
// valid topological order. A log that reshuffles when nothing changed is a log
// you cannot diff against yesterday's.
func TestJobsOneIsTheSequentialOrder(t *testing.T) {
	p := diamond()
	order, err := p.order()
	if err != nil {
		t.Fatal(err)
	}
	tr := &tracker{}
	r := &Runner{Blobs: store.NewMem(), Jobs: 1,
		Backend: byModel(map[string]dawn.Backend{"b": free(tr)})}
	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if strings.Join(tr.started, ",") != strings.Join(order, ",") {
		t.Fatalf("--jobs 1 must match the topological order\n  want %v\n  got  %v", order, tr.started)
	}
}

// failing errors on the named prompt and records what else ran.
type failing struct {
	bad  string
	ran  *atomic.Int64
	fail *atomic.Int64
}

func (failing) Name() string { return "failing" }
func (f failing) Invoke(_ context.Context, in dawn.Invocation) (dawn.Result, error) {
	f.ran.Add(1)
	if strings.HasPrefix(in.Prompt, f.bad) {
		f.fail.Add(1)
		return dawn.Result{}, fmt.Errorf("boom")
	}
	return dawn.Result{Output: map[string]any{"text": in.Prompt}}, nil
}

// A failure must stop the run whatever the scheduler did, and must report the
// SAME step every time — which worker lost the race is nondeterministic, which
// step is at fault is not.
func TestJobsReportsTheEarliestFailureDeterministically(t *testing.T) {
	for i := 0; i < 30; i++ {
		var ran, failed atomic.Int64
		r := &Runner{Blobs: store.NewMem(), Jobs: 4,
			Backend: byModel(map[string]dawn.Backend{"b": failing{bad: "left", ran: &ran, fail: &failed}})}
		_, err := r.Run(context.Background(), diamond())
		if err == nil {
			t.Fatal("a failing step must fail the run")
		}
		if !strings.Contains(err.Error(), `step "left"`) {
			t.Fatalf("run %d reported the wrong step: %v", i, err)
		}
		// merge depends on left, so it must never have been launched.
		if strings.Contains(err.Error(), "merge") {
			t.Fatalf("run %d: a step downstream of the failure ran: %v", i, err)
		}
	}
}

// A committed step must be skipped on the next run at any --jobs. Concurrency
// changes when work happens, never whether it is reused.
func TestJobsStillSkipsCommittedSteps(t *testing.T) {
	dir := t.TempDir()
	tr := &tracker{}
	back := byModel(map[string]dawn.Backend{"b": free(tr)})

	first := durable(t, dir, back)
	first.Jobs = 4
	if _, err := first.Run(context.Background(), diamond()); err != nil {
		t.Fatal(err)
	}
	ranFirst := len(tr.started)
	if ranFirst != 4 {
		t.Fatalf("expected 4 invocations on a cold run, got %d", ranFirst)
	}

	second := durable(t, dir, back)
	second.Jobs = 4
	if _, err := second.Run(context.Background(), diamond()); err != nil {
		t.Fatal(err)
	}
	if got := len(tr.started) - ranFirst; got != 0 {
		t.Fatalf("a second run at --jobs 4 re-executed %d steps that were already committed", got)
	}
}

// Two inputs drawn from ONE upstream is one edge. Counting it twice leaves the
// step waiting forever on a debt already paid — and the failure is silent: the
// scheduler simply runs out of ready work and returns, with the step missing
// from the results and no error anywhere.
func TestJobsCountsDistinctUpstreamsNotInputs(t *testing.T) {
	p := &Plan{Steps: map[string]Step{
		"root": {Agent: "x/b", Prompt: "root", Outputs: map[string]Type{"text": {}, "note": {}}},
		"sink": {Agent: "x/b", Prompt: "sink",
			Inputs:  map[string]string{"a": "root.text", "b": "root.note"},
			Outputs: map[string]Type{"text": {}}},
	}}
	for _, jobs := range []int{1, 4} {
		tr := &tracker{}
		r := &Runner{Blobs: store.NewMem(), Jobs: jobs,
			Backend: byModel(map[string]dawn.Backend{"b": free(tr)})}
		done, err := r.Run(context.Background(), p)
		if err != nil {
			t.Fatalf("jobs=%d: %v", jobs, err)
		}
		if _, ok := done["sink"]; !ok {
			t.Fatalf("jobs=%d: sink never ran — two inputs from one step counted as two edges", jobs)
		}
		if len(tr.started) != 2 {
			t.Fatalf("jobs=%d: expected 2 invocations, got %d (%v)", jobs, len(tr.started), tr.started)
		}
	}
}
