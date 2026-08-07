package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/gate"
	"github.com/valbaudo/dawn/store"
)

// StepResult is a committed step: its typed output, any state refs it produced,
// and the store ref that record was content-addressed to.
type StepResult struct {
	Output   map[string]any
	Produced map[string]dawn.Ref
	Tokens   dawn.Tokens
	Ref      string
}

// stepBlob is the committed record. Produced travels with Output so a later run
// can still hand a workspace ref to the next step; committing only the scalars
// would silently break the chain.
type stepBlob struct {
	Output   map[string]any      `json:"output"`
	Produced map[string]dawn.Ref `json:"produced,omitempty"`
}

// RejectedError is a panel refusing the work — a legitimate outcome, not a
// malfunction. It is distinguished from a mechanical failure so a caller can exit
// differently: in an unattended run, "the panel refused" and "the machine broke"
// need different responses.
type RejectedError struct {
	Step       string
	Attempts   int
	Objections string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("gate refused after %d attempt(s): %s", e.Attempts, e.Objections)
}

// ValidationError reports a plan author or backend configuration error found
// before status lookup or execution begins.
type ValidationError struct {
	Err error
}

func (e *ValidationError) Error() string { return e.Err.Error() }
func (e *ValidationError) Unwrap() error { return e.Err }

// Runner executes a Plan. Backend maps an agent spec to a concrete dawn.Backend, so
// the runner stays backend-agnostic.
type Runner struct {
	Blobs   store.Blobs
	Journal *Journal        // optional; nil means never reuse and never record
	Redo    map[string]bool // step ids to re-run even on a hit, this run only
	Root    *dawn.Ref       // optional: the tree bound by --in, referenced as in.workspace
	Jobs    int             // max steps in flight; 0 or 1 is sequential
	Backend func(Agent) (dawn.Backend, error)
	Log     func(format string, args ...any)

	logMu sync.Mutex // Log is called from workers once Jobs > 1
}

// Run executes the plan in dependency order, reusing any step whose identity key
// the journal already holds an accepted result for.
//
// There is no resume mode and no resume flag: re-running the same command IS the
// resume, so the recovery path is the normal path and gets exercised every run.
func (r *Runner) Run(ctx context.Context, p *Plan) (map[string]StepResult, error) {
	order, err := p.order()
	if err != nil {
		return nil, err
	}
	if err := r.preflight(p); err != nil {
		return nil, err
	}

	// EVERYTHING that mutates run state, and everything that WRITES it, happens in
	// this goroutine. Workers only make the expensive call and hand the result
	// back, so `Jobs` buys concurrency without touching the invariant that the
	// interpreter is the sole writer of durable state.
	done := map[string]StepResult{}
	if r.Root != nil {
		// The reserved root step: a value in the graph, not a special case in bind.
		done[RootStep] = StepResult{Produced: map[string]dawn.Ref{"workspace": *r.Root}}
	}

	sch := newSchedule(p, order)
	type finished struct {
		id  string
		res dawn.Result
		err error
	}
	fin := make(chan finished)
	keys := map[string]string{} // id -> identity key, for the commit
	who := map[string]Agent{}   // id -> agent, for the journal line
	errs := map[string]error{}  // id -> why it failed
	inflight, stopped := 0, false

	jobs := max(r.Jobs, 1)
	for {
		for !stopped && inflight < jobs && sch.pending() {
			id := sch.take()
			s := p.Steps[id]
			// Bound HERE, not in the worker. A step becomes ready only once every
			// upstream has committed, so `done` is stable for this read and needs no
			// lock — the frontier is the synchronization.
			bound, agent, key, err := r.prepare(id, s, done)
			if err != nil {
				errs[id], stopped = err, true
				break
			}
			rec, hit, err := r.reuse(id, key)
			if err != nil {
				errs[id], stopped = err, true
				break
			}
			if hit {
				r.logf("skip %s (%s)", id, short(rec.Ref))
				sch.complete(id)
				done[id] = rec
				continue
			}
			keys[id], who[id] = key, agent
			inflight++
			go func() {
				res, err := r.execute(ctx, id, s, bound)
				fin <- finished{id, res, err}
			}()
		}
		if inflight == 0 {
			break
		}

		f := <-fin
		inflight--
		if f.err != nil {
			var rej *RejectedError
			if r.Journal != nil && errors.As(f.err, &rej) {
				// Recorded for forensics, with no ref, so it can never serve a hit.
				_ = r.Journal.Append(Entry{Key: keys[f.id], Step: f.id, Rejected: rej.Objections})
			}
			errs[f.id], stopped = fmt.Errorf("step %q: %w", f.id, f.err), true
			// Not cancelled: the others are already paid for, and a step that
			// commits is a step the next run skips. Stop LAUNCHING, then drain.
			continue
		}
		res, err := r.commit(f.id, keys[f.id], who[f.id], f.res)
		if err != nil {
			errs[f.id], stopped = err, true
			continue
		}
		sch.complete(f.id)
		done[f.id] = res
		r.logf("done %s -> %s", f.id, short(res.Ref))
	}

	if len(errs) > 0 {
		// Report the failure earliest in topological order. Which worker lost first
		// is a race; which step is at fault is not, so the same broken plan reports
		// the same error whatever the scheduler did.
		first := ""
		for id := range errs {
			if first == "" || sch.rank[id] < sch.rank[first] {
				first = id
			}
		}
		return done, errs[first]
	}
	return done, nil
}

// schedule is the DAG frontier: how many distinct upstreams each step is still
// waiting on, and whom to unlock when one lands.
//
// DISTINCT is load-bearing. Two inputs drawn from one upstream is one edge;
// counting it twice leaves the step waiting forever on a debt already paid.
type schedule struct {
	waiting map[string]int      // id -> upstreams not yet committed
	unlocks map[string][]string // id -> steps waiting on it
	rank    map[string]int      // id -> position in topological order
	ready   []string
}

func newSchedule(p *Plan, order []string) *schedule {
	s := &schedule{
		waiting: map[string]int{}, unlocks: map[string][]string{}, rank: map[string]int{},
	}
	for i, id := range order {
		s.rank[id] = i
	}
	for _, id := range order {
		ups := map[string]bool{}
		for _, ref := range p.Steps[id].Inputs {
			did, _, _ := ParseRef(ref)
			if did == RootStep {
				continue // always available; validate proved --in was given
			}
			if _, ok := p.Steps[did]; ok {
				ups[did] = true
			}
		}
		s.waiting[id] = len(ups)
		for up := range ups {
			s.unlocks[up] = append(s.unlocks[up], id)
		}
	}
	for _, id := range order {
		if s.waiting[id] == 0 {
			s.ready = append(s.ready, id)
		}
	}
	return s
}

func (s *schedule) pending() bool { return len(s.ready) > 0 }

// take pops the ready step that comes FIRST in topological order, so --jobs 1
// reproduces the old sequential order exactly rather than merely some valid one.
// A run whose log order shifts when nothing else changed is a run you cannot
// diff against yesterday's.
func (s *schedule) take() string {
	best := 0
	for i := 1; i < len(s.ready); i++ {
		if s.rank[s.ready[i]] < s.rank[s.ready[best]] {
			best = i
		}
	}
	id := s.ready[best]
	s.ready = append(s.ready[:best], s.ready[best+1:]...)
	return id
}

// complete releases the steps that were waiting on id.
func (s *schedule) complete(id string) {
	for _, d := range s.unlocks[id] {
		s.waiting[d]--
		if s.waiting[d] == 0 {
			s.ready = append(s.ready, d)
		}
	}
}

// prepare resolves a step's inputs and identity, all of which read `done` and so
// must happen on the scheduling goroutine.
func (r *Runner) prepare(id string, s Step, done map[string]StepResult) (bound, Agent, string, error) {
	b, err := r.bind(s, done)
	if err != nil {
		return bound{}, Agent{}, "", fmt.Errorf("step %q: %w", id, err)
	}
	agent, err := ParseAgent(s.Agent)
	if err != nil {
		return bound{}, Agent{}, "", fmt.Errorf("step %q: %w", id, err)
	}
	key, err := s.Key(id, agent, b.key)
	if err != nil {
		return bound{}, Agent{}, "", fmt.Errorf("step %q: %w", id, err)
	}
	return b, agent, key, nil
}

// reuse returns a committed result for key, if the journal holds one.
//
// A journal line pointing at bytes that are gone is a BROKEN STORE, not a miss.
// Degrading it to a miss would silently re-pay for work already bought and paper
// over the corruption, so it propagates.
func (r *Runner) reuse(id, key string) (StepResult, bool, error) {
	if r.Journal == nil || r.Redo[id] {
		return StepResult{}, false, nil
	}
	ref, ok := r.Journal.Lookup(key)
	if !ok {
		return StepResult{}, false, nil
	}
	rec, err := load(r.Blobs, ref)
	if err != nil {
		return StepResult{}, false, fmt.Errorf("step %q: %w", id, err)
	}
	rec.Ref = ref
	return rec, true, nil
}

// commit content-addresses a result and then records the pointer. Blob FIRST: a
// crash between the two leaves an orphan blob, which is harmless garbage, where
// the other order leaves a journal pointer to bytes that do not exist.
func (r *Runner) commit(id, key string, agent Agent, res dawn.Result) (StepResult, error) {
	blob, err := json.Marshal(stepBlob{Output: res.Output, Produced: res.Produced})
	if err != nil {
		return StepResult{}, err
	}
	ref, err := r.Blobs.Put(blob)
	if err != nil {
		return StepResult{}, err
	}
	if r.Journal != nil {
		if err := r.Journal.Append(Entry{
			Key: key, Ref: ref, Step: id, Agent: agent.String(),
			Tokens: &Tokens{In: res.Tokens.Input, Out: res.Tokens.Output,
				CacheRead: res.Tokens.CacheRead, CacheCreate: res.Tokens.CacheCreate},
		}); err != nil {
			return StepResult{}, err
		}
	}
	return StepResult{Output: res.Output, Produced: res.Produced, Tokens: res.Tokens, Ref: ref}, nil
}

// bound is a step's resolved inputs: what the agent is asked, what refs it
// receives, and the canonical form those inputs take in the identity key.
type bound struct {
	prompt string
	refs   map[string]dawn.Ref
	key    map[string]string
}

// bind resolves declared inputs. A state ref (a workspace, an artifact) travels
// as a REF so the backend materializes it properly; only a scalar is rendered
// into the prompt, because that is the only kind a model can read directly.
//
// SORTED, not a map range. A provider's prompt cache is keyed on the exact
// leading tokens, so a randomized fold order changes the prompt bytes on every run
// and silently defeats every cache hit. Measured before this was sorted: three
// inputs produced three distinct prompts across repeated runs.
func (r *Runner) bind(s Step, done map[string]StepResult) (bound, error) {
	b := bound{prompt: s.Prompt, refs: map[string]dawn.Ref{}, key: map[string]string{}}
	for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
		did, field, _ := ParseRef(s.Inputs[name])
		up := done[did]
		if ref, ok := up.Produced[field]; ok {
			b.refs[name] = ref
			b.key[name] = ref.URI // the upstream's REF, not its key: early cutoff
			continue
		}
		v, ok := up.Output[field]
		if !ok {
			return bound{}, fmt.Errorf("input %q: step %q produced no %q", name, did, field)
		}
		b.prompt += fmt.Sprintf("\n\n--- input: %s ---\n%v", name, v)
		b.key[name] = fmt.Sprint(v)
	}
	return b, nil
}

// preflight resolves every configured backend and verifies that reserved tree
// flows match the optional capabilities at both ends before any external state is
// read or an invocation starts.
func (r *Runner) preflight(p *Plan) (err error) {
	defer func() {
		if err != nil {
			err = &ValidationError{Err: err}
		}
	}()

	backends := make(map[string]dawn.Backend, len(p.Steps))
	for _, id := range p.IDs() {
		s := p.Steps[id]
		b, err := r.backendFor(s)
		if err != nil {
			return fmt.Errorf("step %q: %w", id, err)
		}
		backends[id] = b

		if s.Gate != nil {
			for _, spec := range s.Gate.Judges {
				a, err := ParseAgent(spec)
				if err != nil {
					return fmt.Errorf("step %q gate judge %q: %w", id, spec, err)
				}
				if _, err := r.Backend(a); err != nil {
					return fmt.Errorf("step %q gate judge %q: %w", id, spec, err)
				}
			}
		}
	}

	for _, id := range p.IDs() {
		s := p.Steps[id]
		consumer := backends[id]
		if len(s.Expect) > 0 {
			if _, ok := consumer.(dawn.TreeCapturer); !ok {
				return fmt.Errorf("step %q declares expect: but agent %q captures no tree", id, s.Agent)
			}
		}
		for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
			did, field, err := ParseRef(s.Inputs[name])
			if err != nil {
				return fmt.Errorf("step %q input %q: %w", id, name, err)
			}
			if did == RootStep && r.Root == nil {
				return fmt.Errorf("step %q input %q references %s.workspace but no --in was given",
					id, name, RootStep)
			}
			if field != "workspace" && field != "diff" {
				continue
			}
			if did != RootStep {
				producer := backends[did]
				if _, ok := producer.(dawn.TreeCapturer); !ok {
					return fmt.Errorf("step %q input %q references %s.%s, but upstream agent %q captures no tree",
						id, name, did, field, p.Steps[did].Agent)
				}
			}
			if field == "workspace" {
				if _, ok := consumer.(dawn.WorkspaceMaterializer); !ok {
					return fmt.Errorf("step %q input %q references a workspace, but agent %q cannot materialize a workspace",
						id, name, s.Agent)
				}
			}
		}
	}
	return nil
}

// backendFor constructs the backend named by a step's agent string.
func (r *Runner) backendFor(s Step) (dawn.Backend, error) {
	a, err := ParseAgent(s.Agent)
	if err != nil {
		return nil, err
	}
	return r.Backend(a)
}

func (r *Runner) execute(ctx context.Context, id string, s Step, b bound) (dawn.Result, error) {
	backend, err := r.backendFor(s)
	if err != nil {
		return dawn.Result{}, err
	}
	// The schema is pushed to the agent as an OPTIMIZATION. It is never the
	// authority: Step.Validate re-checks locally on every backend, always.
	inv := dawn.Invocation{Prompt: b.prompt, Inputs: b.refs, Schema: s.Schema(), Expect: s.Expect}

	var res dawn.Result
	if s.Gate == nil {
		r.logf("run  %s (%s)", id, backend.Name())
		res, err = backend.Invoke(ctx, inv)
		if err == nil {
			err = s.Validate(res.Output)
		}
	} else {
		res, err = r.runGated(ctx, id, s, backend, inv)
	}
	if err != nil {
		return dawn.Result{}, err
	}
	return res, nil
}

// runGated generates, submits the result to an independent panel, and repairs
// from the critique until quorum or the attempt bound. A gate that never reaches
// quorum fails the step rather than passing the work through.
func (r *Runner) runGated(ctx context.Context, id string, s Step, backend dawn.Backend, inv dawn.Invocation) (dawn.Result, error) {
	g := s.Gate
	judges := make([]dawn.Backend, 0, len(g.Judges))
	for _, spec := range g.Judges {
		a, err := ParseAgent(spec)
		if err != nil {
			return dawn.Result{}, err
		}
		b, err := r.Backend(a)
		if err != nil {
			return dawn.Result{}, err
		}
		judges = append(judges, b)
	}
	quorum := g.Threshold()
	r.logf("run  %s (%s) + gate: %d judges, quorum %d, up to %d attempts",
		id, backend.Name(), len(judges), quorum, Attempts)

	// One entry per generated attempt, so the committed record can be selected by
	// the gate's OWN attempt index. Keeping a single `accepted` variable that each
	// generation overwrites is correct only as long as Gate returns at the first
	// pass and the caller errors on non-approval — i.e. it is a landmine: the day
	// anything returns a last candidate on non-approval, an UNJUDGED result would
	// ship under an accepted key.
	var generated []dawn.Result
	gen := func(ctx context.Context, feedback string) (gate.Candidate, error) {
		attempt := inv
		if feedback != "" {
			// APPENDED, never prepended: the leading bytes stay identical, which is
			// the one cache win the repair loop can honestly claim.
			attempt.Prompt = inv.Prompt + "\n\n" + feedback
		}
		res, err := backend.Invoke(ctx, attempt)
		if err != nil {
			return gate.Candidate{}, err
		}
		// Validate BEFORE the panel sees it: a non-conforming candidate never
		// reaches a judge, and a schema violation is a mechanical failure rather
		// than a rejection that would burn a repair attempt nobody evaluated.
		if err := s.Validate(res.Output); err != nil {
			return gate.Candidate{}, err
		}
		generated = append(generated, res)
		// The jury reads the whole validated object, which deletes the question of
		// which field the judges are looking at.
		text, err := json.MarshalIndent(res.Output, "", "  ")
		if err != nil {
			return gate.Candidate{}, err
		}
		return gate.Candidate{Text: string(text), Produced: res.Produced}, nil
	}

	out, err := gate.Gate(ctx, gen, judges, g.Criteria, quorum, Attempts)
	if err != nil {
		return dawn.Result{}, err
	}
	if !out.Approved {
		return dawn.Result{}, &RejectedError{Step: id, Attempts: out.Attempts, Objections: objections(out.Votes)}
	}
	// Commit the attempt the panel APPROVED, addressed by index — never merely the
	// last one generated.
	if out.Attempts < 1 || out.Attempts > len(generated) {
		return dawn.Result{}, fmt.Errorf("gate reported attempt %d of %d generated: refusing to commit an unidentified result",
			out.Attempts, len(generated))
	}
	r.logf("     gate passed on attempt %d", out.Attempts)
	return generated[out.Attempts-1], nil
}

// objections summarizes why a panel refused, for the step's error and its journal
// line.
func objections(votes []gate.Verdict) string {
	var b strings.Builder
	for _, v := range votes {
		if v.Approved || v.Reason == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %s", v.Judge, v.Reason)
	}
	if b.Len() == 0 {
		return "no reasons given"
	}
	return b.String()
}

// load reads a committed record back out of the store. Reuse is literally
// re-reading a content-addressed commit.
func load(b store.Blobs, ref string) (StepResult, error) {
	content, err := b.Get(ref)
	if err != nil {
		return StepResult{}, fmt.Errorf("load %s: %w", ref, err)
	}
	var blob stepBlob
	if err := json.Unmarshal(content, &blob); err != nil {
		return StepResult{}, fmt.Errorf("load %s: %w", ref, err)
	}
	return StepResult{Output: blob.Output, Produced: blob.Produced}, nil
}

// logf serializes the log. Above --jobs 1 this is called from workers, and two
// half-written lines interleaved is the kind of output that makes an operator
// distrust the whole log at 3am.
func (r *Runner) logf(format string, args ...any) {
	if r.Log == nil {
		return
	}
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.Log(format, args...)
}

func short(ref string) string {
	if len(ref) > 18 {
		return ref[:18]
	}
	return ref
}

// StepStatus is one row of `dawn show PLAN`: what a step would do on the next run.
type StepStatus struct {
	ID    string
	State string // fresh | stale | unknown
	Ref   string // set when fresh
	Calls int    // worst-case invocations if it runs
}

// Status resolves what the next run would do, WITHOUT executing anything. It is
// the same identity-resolution walk Run performs, which is the point: `dawn run` is
// `dawn show` plus executing the stale frontier, so there is one implementation of
// "is this step fresh" rather than a second one inside a --dry-run branch.
//
// Honest limit: a step's key depends on its upstream's RESOLVED output, so
// everything past the first stale step is `unknown` rather than `stale`. That is
// the price of early cutoff (an upstream that re-runs to identical bytes correctly
// skips its descendants) and Nix has the same limitation with floating outputs.
func (r *Runner) Status(p *Plan) ([]StepStatus, error) {
	order, err := p.order()
	if err != nil {
		return nil, err
	}
	if err := r.preflight(p); err != nil {
		return nil, err
	}
	done := map[string]StepResult{}
	if r.Root != nil {
		done[RootStep] = StepResult{Produced: map[string]dawn.Ref{"workspace": *r.Root}}
	}
	out := make([]StepStatus, 0, len(order))
	for _, id := range order {
		s := p.Steps[id]
		st := StepStatus{ID: id, State: "unknown", Calls: worstCase(s)}

		bound, err := r.bind(s, done)
		if err != nil {
			out = append(out, st) // an upstream is unknown, so this one is too
			continue
		}
		agent, err := ParseAgent(s.Agent)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", id, err)
		}
		key, err := s.Key(id, agent, bound.key)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", id, err)
		}
		st.State = "stale"
		if r.Journal != nil && !r.Redo[id] {
			if ref, ok := r.Journal.Lookup(key); ok {
				rec, err := load(r.Blobs, ref)
				if err != nil {
					return nil, fmt.Errorf("step %q: %w", id, err)
				}
				rec.Ref = ref
				done[id] = rec
				st.State, st.Ref, st.Calls = "fresh", ref, 0
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// worstCase is the invocation count if a step runs and its panel refuses every
// time: one generation plus the whole panel, per attempt. Exact in CALLS; the
// dollar cost is a range, because nobody knows output tokens before running.
func worstCase(s Step) int {
	if s.Gate == nil {
		return 1
	}
	return Attempts * (1 + len(s.Gate.Judges))
}

// Committed returns a step's committed record, or false if the next run would
// re-execute it. Used by `dawn show PLAN REF` so reading a result and previewing a
// run share one notion of what "already done" means.
func (r *Runner) Committed(p *Plan, id string) (StepResult, bool, error) {
	status, err := r.Status(p)
	if err != nil {
		return StepResult{}, false, err
	}
	for _, st := range status {
		if st.ID != id {
			continue
		}
		if st.State != "fresh" {
			return StepResult{}, false, nil
		}
		rec, err := load(r.Blobs, st.Ref)
		if err != nil {
			return StepResult{}, false, err
		}
		rec.Ref = st.Ref
		return rec, true, nil
	}
	return StepResult{}, false, fmt.Errorf("no step %q", id)
}
