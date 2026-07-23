package plan

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/store"
)

// StepResult is a committed step: its typed output and the store ref that output
// was content-addressed to. Resume reloads these from the store — nothing else.
type StepResult struct {
	Output map[string]any
	Ref    string
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
// done (resume), and returns the full result set. Each step's typed output is
// committed to Blobs before the next step runs, so a crash loses only the
// in-flight step; on the returned map even a partial run is resumable.
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
	// Materialize typed inputs as labeled blocks appended to the prompt. This is
	// reference resolution, not string interpolation: the author writes a plain
	// prompt and names inputs; the runner attaches the resolved values.
	prompt := s.Prompt
	for name, src := range s.Inputs {
		did, field, _ := parseFrom(src.From)
		v, ok := done[did].Output[field]
		if !ok {
			return StepResult{}, fmt.Errorf("input %q: step %q has no field %q", name, did, field)
		}
		prompt += fmt.Sprintf("\n\n--- input: %s ---\n%v", name, v)
	}

	inv := aw.Invocation{Prompt: prompt}
	if s.Output != "" {
		inv.Schema = map[string]any{
			"type": "object", "additionalProperties": false,
			"required":   []any{s.Output},
			"properties": map[string]any{s.Output: map[string]any{"type": "string"}},
		}
	}
	r.logf("run  %s (%s)", s.ID, backend.Name())
	res, err := backend.Invoke(ctx, inv)
	if err != nil {
		return StepResult{}, err
	}
	blob, err := json.Marshal(res.Output)
	if err != nil {
		return StepResult{}, err
	}
	ref, err := r.Blobs.Put(blob)
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{Output: res.Output, Ref: ref}, nil
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
		var o map[string]any
		if err := json.Unmarshal(content, &o); err != nil {
			return nil, fmt.Errorf("reload %q: %w", id, err)
		}
		out[id] = StepResult{Output: o, Ref: ref}
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
