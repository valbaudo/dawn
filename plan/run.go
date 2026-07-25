package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/gate"
	"github.com/valbaudo/aw/store"
)

// StepResult is a committed step: its typed output, any state refs it produced,
// and the store ref that record was content-addressed to. Resume reloads these
// from the store — nothing else.
type StepResult struct {
	Output   map[string]any
	Produced map[string]aw.Ref
	Ref      string
}

// stepBlob is the committed record. Produced travels with Output so a resumed
// run can still hand a workspace ref to the next step; committing only the
// scalars would silently break the chain on restart.
type stepBlob struct {
	Output   map[string]any    `json:"output"`
	Produced map[string]aw.Ref `json:"produced,omitempty"`
}

// Runner executes a Plan. Backend maps an agent spec to a concrete aw.Backend,
// so the runner stays backend-agnostic — the caller decides that "claude" means
// claude.Backend, keeping this package free of any backend dependency.
type Runner struct {
	Blobs   store.Blobs
	Backend func(Agent) (aw.Backend, error)
	Log     func(format string, args ...any) // optional progress; nil is fine
}

// Run executes every step in dependency order, skipping any already present in
// done (resume), and returns the full result set. Each step's record is
// committed to Blobs before the next step runs, so a crash loses only the
// in-flight step.
func (r *Runner) Run(ctx context.Context, p *Plan, done map[string]StepResult) (map[string]StepResult, error) {
	order, err := p.order()
	if err != nil {
		return done, err
	}
	if done == nil {
		done = map[string]StepResult{}
	}
	idx := make(map[string]Step, len(p.Steps))
	for _, s := range p.Steps {
		idx[s.ID] = s
	}
	for _, id := range order {
		if res, ok := done[id]; ok {
			r.logf("skip %s (committed %s)", id, short(res.Ref))
			continue
		}
		res, err := r.runStep(ctx, p, idx[id], done)
		if err != nil {
			return done, fmt.Errorf("step %q: %w", id, err)
		}
		done[id] = res
		r.logf("done %s -> %s", id, short(res.Ref))
	}
	return done, nil
}

func (r *Runner) runStep(ctx context.Context, p *Plan, s Step, done map[string]StepResult) (StepResult, error) {
	backend, err := r.Backend(p.Agents[s.Agent])
	if err != nil {
		return StepResult{}, err
	}

	// Resolve declared inputs. A state ref (a workspace, an artifact) travels as
	// a REF, so the backend materializes it properly; only a scalar is rendered
	// into the prompt, because that is the only kind a model can read directly.
	//
	// SORTED, not a map range. A provider's prompt cache is keyed on the exact
	// leading tokens, so a randomized fold order changes the prompt bytes on every
	// run and silently defeats every cache hit. Measured before this was sorted:
	// three inputs produced three distinct prompts across repeated runs.
	prompt := s.Prompt
	inputs := map[string]aw.Ref{}
	for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
		src := s.Inputs[name]
		did, field, _ := parseFrom(src.From)
		up := done[did]
		if ref, ok := up.Produced[field]; ok {
			inputs[name] = ref
			continue
		}
		v, ok := up.Output[field]
		if !ok {
			return StepResult{}, fmt.Errorf("input %q: step %q produced no %q", name, did, field)
		}
		prompt += fmt.Sprintf("\n\n--- input: %s ---\n%v", name, v)
	}

	inv := aw.Invocation{Prompt: prompt, Inputs: inputs}
	if s.Output != "" {
		inv.Schema = map[string]any{
			"type": "object", "additionalProperties": false,
			"required":   []any{s.Output},
			"properties": map[string]any{s.Output: map[string]any{"type": "string"}},
		}
	}

	var res aw.Result
	if s.Gate == nil {
		r.logf("run  %s (%s)", s.ID, backend.Name())
		res, err = backend.Invoke(ctx, inv)
	} else {
		res, err = r.runGated(ctx, p, s, backend, inv)
	}
	if err != nil {
		return StepResult{}, err
	}

	blob, err := json.Marshal(stepBlob{Output: res.Output, Produced: res.Produced})
	if err != nil {
		return StepResult{}, err
	}
	ref, err := r.Blobs.Put(blob)
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{Output: res.Output, Produced: res.Produced, Ref: ref}, nil
}

// runGated generates, submits the result to an independent panel, and repairs
// from the critique until quorum or the attempt bound. A gate that never reaches
// quorum fails the step rather than passing the work through.
func (r *Runner) runGated(ctx context.Context, p *Plan, s Step, backend aw.Backend, inv aw.Invocation) (aw.Result, error) {
	g := s.Gate
	judges := make([]aw.Backend, 0, len(g.Judges))
	for _, name := range g.Judges {
		b, err := r.Backend(p.Agents[name])
		if err != nil {
			return aw.Result{}, err
		}
		judges = append(judges, b)
	}
	quorum := g.Quorum
	if quorum == 0 {
		quorum = gate.Majority(len(judges))
	}
	attempts := g.Attempts
	if attempts == 0 {
		attempts = 3
	}
	field := s.Output
	if field == "" {
		field = "text"
	}

	r.logf("run  %s (%s) + gate: %d judges, quorum %d, up to %d attempts",
		s.ID, backend.Name(), len(judges), quorum, attempts)

	var accepted aw.Result
	gen := func(ctx context.Context, feedback string) (gate.Candidate, error) {
		attempt := inv
		if feedback != "" {
			attempt.Prompt = inv.Prompt + "\n\n" + feedback
		}
		res, err := backend.Invoke(ctx, attempt)
		if err != nil {
			return gate.Candidate{}, err
		}
		accepted = res // Gate returns at the first pass, so this is the accepted one
		return gate.FromResult(res, field), nil
	}

	out, err := gate.Gate(ctx, gen, judges, g.Criteria, quorum, attempts)
	if err != nil {
		return aw.Result{}, err
	}
	if !out.Approved {
		return aw.Result{}, fmt.Errorf("gate rejected after %d attempt(s): %s", out.Attempts, objections(out.Votes))
	}
	r.logf("     gate passed on attempt %d", out.Attempts)
	return accepted, nil
}

// objections summarizes why a panel refused, for the step's error.
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

// Reload rebuilds committed results from an id->ref map by reading each ref back
// out of the store. Resume is literally re-reading content-addressed commits.
func Reload(b store.Blobs, refs map[string]string) (map[string]StepResult, error) {
	out := make(map[string]StepResult, len(refs))
	for id, ref := range refs {
		content, err := b.Get(ref)
		if err != nil {
			return nil, fmt.Errorf("reload %q: %w", id, err)
		}
		var blob stepBlob
		if err := json.Unmarshal(content, &blob); err != nil {
			return nil, fmt.Errorf("reload %q: %w", id, err)
		}
		out[id] = StepResult{Output: blob.Output, Produced: blob.Produced, Ref: ref}
	}
	return out, nil
}

// Refs extracts the id->ref map for persisting run state (checkpoint/resume).
func Refs(done map[string]StepResult) map[string]string {
	m := make(map[string]string, len(done))
	for id, r := range done {
		m[id] = r.Ref
	}
	return m
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
