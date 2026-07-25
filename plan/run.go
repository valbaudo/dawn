package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/gate"
	"github.com/valbaudo/aw/store"
)

// StepResult is a committed step: its typed output, any state refs it produced,
// and the store ref that record was content-addressed to.
type StepResult struct {
	Output   map[string]any
	Produced map[string]aw.Ref
	Tokens   aw.Tokens
	Ref      string
}

// stepBlob is the committed record. Produced travels with Output so a later run
// can still hand a workspace ref to the next step; committing only the scalars
// would silently break the chain.
type stepBlob struct {
	Output   map[string]any    `json:"output"`
	Produced map[string]aw.Ref `json:"produced,omitempty"`
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

// Runner executes a Plan. Backend maps an agent spec to a concrete aw.Backend, so
// the runner stays backend-agnostic.
type Runner struct {
	Blobs   store.Blobs
	Journal *Journal        // optional; nil means never reuse and never record
	Redo    map[string]bool // step ids to re-run even on a hit, this run only
	Root    *aw.Ref         // optional: the tree bound by --in, referenced as in.workspace
	Backend func(Agent) (aw.Backend, error)
	Log     func(format string, args ...any)
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
	// Preflight, over EVERY step before any of them runs, so an author mistake
	// costs nothing rather than surfacing halfway through a paid pipeline.
	for _, id := range p.IDs() {
		s := p.Steps[id]
		for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
			if did, _, _ := ParseRef(s.Inputs[name]); did == RootStep && r.Root == nil {
				return nil, fmt.Errorf("step %q input %q references %s.workspace but no --in was given",
					id, name, RootStep)
			}
		}
		if len(s.Expect) == 0 {
			continue
		}
		b, err := r.backendFor(s)
		if err != nil {
			return nil, fmt.Errorf("step %q: %w", id, err)
		}
		if _, ok := b.(aw.TreeCapturer); !ok {
			return nil, fmt.Errorf("step %q declares expect: but agent %q captures no tree", id, s.Agent)
		}
	}

	done := map[string]StepResult{}
	if r.Root != nil {
		// The reserved root step: a value in the graph, not a special case in bind.
		done[RootStep] = StepResult{Produced: map[string]aw.Ref{"workspace": *r.Root}}
	}

	for _, id := range order {
		s := p.Steps[id]
		bound, err := r.bind(s, done)
		if err != nil {
			return done, fmt.Errorf("step %q: %w", id, err)
		}
		agent, err := ParseAgent(s.Agent)
		if err != nil {
			return done, fmt.Errorf("step %q: %w", id, err)
		}
		key, err := s.Key(id, agent, bound.key)
		if err != nil {
			return done, fmt.Errorf("step %q: %w", id, err)
		}

		if r.Journal != nil && !r.Redo[id] {
			if ref, ok := r.Journal.Lookup(key); ok {
				rec, err := load(r.Blobs, ref)
				if err != nil {
					return done, fmt.Errorf("step %q: %w", id, err)
				}
				rec.Ref = ref
				done[id] = rec
				r.logf("skip %s (%s)", id, short(ref))
				continue
			}
		}

		res, err := r.execute(ctx, id, s, bound)
		if err != nil {
			var rej *RejectedError
			if r.Journal != nil && errors.As(err, &rej) {
				// Recorded for forensics, with no ref, so it can never serve a hit.
				_ = r.Journal.Append(Entry{Key: key, Step: id, Rejected: rej.Objections})
			}
			return done, fmt.Errorf("step %q: %w", id, err)
		}

		// Blob FIRST, then the journal line: a crash between them leaves an orphan
		// blob (harmless), never a journal pointer to bytes that do not exist.
		blob, err := json.Marshal(stepBlob{Output: res.Output, Produced: res.Produced})
		if err != nil {
			return done, err
		}
		ref, err := r.Blobs.Put(blob)
		if err != nil {
			return done, err
		}
		if r.Journal != nil {
			if err := r.Journal.Append(Entry{
				Key: key, Ref: ref, Step: id, Agent: agent.String(),
				Tokens: &Tokens{In: res.Tokens.Input, Out: res.Tokens.Output,
					CacheRead: res.Tokens.CacheRead, CacheCreate: res.Tokens.CacheCreate},
			}); err != nil {
				return done, err
			}
		}
		res.Ref = ref
		done[id] = res
		r.logf("done %s -> %s", id, short(ref))
	}
	return done, nil
}

// bound is a step's resolved inputs: what the agent is asked, what refs it
// receives, and the canonical form those inputs take in the identity key.
type bound struct {
	prompt string
	refs   map[string]aw.Ref
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
	b := bound{prompt: s.Prompt, refs: map[string]aw.Ref{}, key: map[string]string{}}
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

// backendFor constructs the backend named by a step's agent string.
func (r *Runner) backendFor(s Step) (aw.Backend, error) {
	a, err := ParseAgent(s.Agent)
	if err != nil {
		return nil, err
	}
	return r.Backend(a)
}

func (r *Runner) execute(ctx context.Context, id string, s Step, b bound) (StepResult, error) {
	backend, err := r.backendFor(s)
	if err != nil {
		return StepResult{}, err
	}
	// The schema is pushed to the agent as an OPTIMIZATION. It is never the
	// authority: Step.Validate re-checks locally on every backend, always.
	inv := aw.Invocation{Prompt: b.prompt, Inputs: b.refs, Schema: s.Schema(), Expect: s.Expect}

	var res aw.Result
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
		return StepResult{}, err
	}
	return StepResult{Output: res.Output, Produced: res.Produced, Tokens: res.Tokens}, nil
}

// runGated generates, submits the result to an independent panel, and repairs
// from the critique until quorum or the attempt bound. A gate that never reaches
// quorum fails the step rather than passing the work through.
func (r *Runner) runGated(ctx context.Context, id string, s Step, backend aw.Backend, inv aw.Invocation) (aw.Result, error) {
	g := s.Gate
	judges := make([]aw.Backend, 0, len(g.Judges))
	for _, spec := range g.Judges {
		a, err := ParseAgent(spec)
		if err != nil {
			return aw.Result{}, err
		}
		b, err := r.Backend(a)
		if err != nil {
			return aw.Result{}, err
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
	var generated []aw.Result
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
		return aw.Result{}, err
	}
	if !out.Approved {
		return aw.Result{}, &RejectedError{Step: id, Attempts: out.Attempts, Objections: objections(out.Votes)}
	}
	// Commit the attempt the panel APPROVED, addressed by index — never merely the
	// last one generated.
	if out.Attempts < 1 || out.Attempts > len(generated) {
		return aw.Result{}, fmt.Errorf("gate reported attempt %d of %d generated: refusing to commit an unidentified result",
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

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func short(ref string) string {
	if len(ref) > 18 {
		return ref[:18]
	}
	return ref
}

// StepStatus is one row of `aw show PLAN`: what a step would do on the next run.
type StepStatus struct {
	ID    string
	State string // fresh | stale | unknown
	Ref   string // set when fresh
	Calls int    // worst-case invocations if it runs
}

// Status resolves what the next run would do, WITHOUT executing anything. It is
// the same identity-resolution walk Run performs, which is the point: `aw run` is
// `aw show` plus executing the stale frontier, so there is one implementation of
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
	done := map[string]StepResult{}
	if r.Root != nil {
		done[RootStep] = StepResult{Produced: map[string]aw.Ref{"workspace": *r.Root}}
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
// re-execute it. Used by `aw show PLAN REF` so reading a result and previewing a
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
