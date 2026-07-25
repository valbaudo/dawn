// Package plan is aw's static-DAG runner: a strict, control-flow-free
// description of agent steps wired by typed references, executed in dependency
// order. There is deliberately no templating and no if/loop/map — a step names
// its inputs, the runner materializes them, and any unknown key is a parse
// error. Orchestration that needs branches, loops, or retries belongs in code
// around aw, not in this format.
package plan

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Plan is a parsed pipeline: named agents and a list of steps.
type Plan struct {
	Version int              `yaml:"version"`
	Agents  map[string]Agent `yaml:"agents"`
	Steps   []Step           `yaml:"steps"`
}

// Agent is a reusable backend spec a step refers to by name.
type Agent struct {
	Backend string `yaml:"backend"` // e.g. "claude"
	Model   string `yaml:"model"`
}

// Step is one invocation. Its dependencies come from Needs and from the steps
// its Inputs reference; both must be declared earlier in the plan.
type Step struct {
	ID     string            `yaml:"id"`
	Agent  string            `yaml:"agent"`
	Prompt string            `yaml:"prompt"`
	Needs  []string          `yaml:"needs,omitempty"`
	Inputs map[string]string `yaml:"inputs,omitempty"` // name -> "steps.<id>.<field>"
	Output map[string]Type   `yaml:"output,omitempty"` // field -> type; omitted => {text: string}
	Gate   *Gate             `yaml:"gate,omitempty"`   // optional acceptance check
}

// Type is a declared output field's type. There are exactly two forms:
//
//	summary: string              // the keyword
//	urgency: [low, medium, high] // a sequence IS an enum of its members
//
// No numbers, booleans, arrays or nested records. Two reasons: the reference
// grammar stops at steps.<id>.<field>, so declaring depth buys nothing that can
// be checked statically; and value constraints are the gate's job, since aw does
// no arithmetic and renders every value to text anyway. Keeping it to two forms
// also means no coercion question ever arises ("7" is not 7, 1 is not true).
type Type struct {
	Enum []string // nil => plain string
}

// UnmarshalYAML accepts the two forms and rejects everything else by name.
func (t *Type) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value != "string" {
			return fmt.Errorf("unknown type %q: use `string` or a sequence of allowed values", n.Value)
		}
		t.Enum = nil
		return nil
	case yaml.SequenceNode:
		vals := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("enum values must be scalars")
			}
			vals = append(vals, c.Value)
		}
		if len(vals) == 0 {
			return fmt.Errorf("an enum needs at least one value")
		}
		t.Enum = vals
		return nil
	default:
		return fmt.Errorf("a type is `string` or a sequence of allowed values")
	}
}

// Fields returns the step's declared output fields, defaulting to {text: string}
// when the author declared none. Resolving the default here (rather than at use)
// is what lets a reference to `steps.x.text` check out on an undeclared step.
func (s Step) Fields() map[string]Type {
	if len(s.Output) == 0 {
		return map[string]Type{"text": {}}
	}
	return s.Output
}

// reserved names a workspace step produces automatically. They may be
// REFERENCED but never DECLARED, so an author cannot shadow them.
var reserved = map[string]string{
	"workspace": "the captured tree",
	"diff":      "the rendering of what changed",
}

// Gate configures an independent acceptance check on a step. It is DATA, not
// control flow: the author writes no branch, no condition and no loop variable.
// The loop lives inside the gate library and this only configures it, the same
// way `retries: 3` configures a CI runner. That is why it does not violate the
// format's no-control-flow rule.
//
// A step whose gate does not reach quorum within Attempts FAILS. In an
// unattended run that is the point: nothing downstream should proceed on work
// the panel refused.
type Gate struct {
	Judges   []string `yaml:"judges"`             // agent names that vote
	Criteria string   `yaml:"criteria"`           // what the judges must check
	Quorum   int      `yaml:"quorum,omitempty"`   // approvals needed; default majority
	Attempts int      `yaml:"attempts,omitempty"` // bounded repair attempts; default 3
}

// Load reads a plan from a YAML (or JSON) file with strict decoding: unknown
// keys are errors, so a control-flow keyword like `loop:` or `if:` fails loudly
// instead of being silently ignored.
func Load(path string) (*Plan, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var p Plan
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("plan: parse %s: %w", path, err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("plan %s: %w", path, err)
	}
	return &p, nil
}

func (p *Plan) validate() error {
	if p.Version != 1 {
		return fmt.Errorf("version must be 1, got %d", p.Version)
	}
	seen := map[string]bool{}
	for i, s := range p.Steps {
		switch {
		case s.ID == "":
			return fmt.Errorf("step %d: missing id", i)
		case seen[s.ID]:
			return fmt.Errorf("duplicate step id %q", s.ID)
		case s.Prompt == "":
			return fmt.Errorf("step %q: missing prompt", s.ID)
		}
		if _, ok := p.Agents[s.Agent]; !ok {
			return fmt.Errorf("step %q: unknown agent %q", s.ID, s.Agent)
		}
		for field := range s.Output {
			if why, bad := reserved[field]; bad {
				return fmt.Errorf("step %q: %q is reserved (%s) and cannot be declared", s.ID, field, why)
			}
		}
		if g := s.Gate; g != nil {
			switch {
			case len(g.Judges) == 0:
				return fmt.Errorf("step %q: gate needs at least one judge", s.ID)
			case g.Criteria == "":
				return fmt.Errorf("step %q: gate needs criteria", s.ID)
			case g.Quorum < 0 || g.Quorum > len(g.Judges):
				return fmt.Errorf("step %q: gate quorum %d out of range for %d judges", s.ID, g.Quorum, len(g.Judges))
			case g.Attempts < 0:
				return fmt.Errorf("step %q: gate attempts must not be negative", s.ID)
			}
			for _, j := range g.Judges {
				if _, ok := p.Agents[j]; !ok {
					return fmt.Errorf("step %q: gate names unknown judge agent %q", s.ID, j)
				}
			}
		}
		seen[s.ID] = true
	}
	byID := make(map[string]Step, len(p.Steps))
	for _, s := range p.Steps {
		byID[s.ID] = s
	}
	for _, s := range p.Steps {
		for _, n := range s.Needs {
			if !seen[n] {
				return fmt.Errorf("step %q needs unknown step %q", s.ID, n)
			}
		}
		for name, src := range s.Inputs {
			id, field, err := parseFrom(src)
			if err != nil {
				return fmt.Errorf("step %q input %q: %w", s.ID, name, err)
			}
			up, ok := byID[id]
			if !ok {
				return fmt.Errorf("step %q input %q references unknown step %q; known steps: %s",
					s.ID, name, id, strings.Join(stepIDs(p.Steps), ", "))
			}
			// THE load-time guarantee: the referenced field must be declared
			// upstream. Because conformance is checked at runtime and there are no
			// optional fields, a reference that passes here can never resolve to a
			// missing value.
			if _, ok := up.Fields()[field]; ok {
				continue
			}
			if _, ok := reserved[field]; ok {
				continue // auto-produced by a workspace step
			}
			return fmt.Errorf("step %q input %q: step %q has no output field %q; it declares: %s",
				s.ID, name, id, field, strings.Join(fieldNames(up), ", "))
		}
	}
	if _, err := p.order(); err != nil {
		return err
	}
	return nil
}

// parseFrom splits "steps.<id>.<field>".
func parseFrom(from string) (id, field string, err error) {
	parts := strings.Split(from, ".")
	if len(parts) != 3 || parts[0] != "steps" {
		return "", "", fmt.Errorf("from %q must be steps.<id>.<field>", from)
	}
	return parts[1], parts[2], nil
}

// order returns step ids in dependency (topological) order, treating both Needs
// and Inputs as edges. It errors on a cycle.
func (p *Plan) order() ([]string, error) {
	idx := make(map[string]Step, len(p.Steps))
	for _, s := range p.Steps {
		idx[s.ID] = s
	}
	const (
		unseen = iota
		visiting
		done
	)
	state := map[string]int{}
	var out []string
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case visiting:
			return fmt.Errorf("dependency cycle at step %q", id)
		case done:
			return nil
		}
		state[id] = visiting
		s := idx[id]
		deps := append([]string(nil), s.Needs...)
		// sorted, so the topological order itself is stable across runs
		for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
			did, _, _ := parseFrom(s.Inputs[name])
			deps = append(deps, did)
		}
		for _, d := range deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		state[id] = done
		out = append(out, id)
		return nil
	}
	for _, s := range p.Steps {
		if err := visit(s.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// stepIDs lists declared step ids, for error messages that tell the author what
// they could have meant.
func stepIDs(steps []Step) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.ID)
	}
	return out
}

// fieldNames lists a step's declared output fields, sorted.
func fieldNames(s Step) []string {
	return slices.Sorted(maps.Keys(s.Fields()))
}

// Schema compiles a step's declared output into JSON Schema. additionalProperties
// is always false and every declared field is always required — the author never
// writes those, which is what makes "no optional fields" unviolatable rather than
// merely documented.
func (s Step) Schema() map[string]any {
	fields := s.Fields()
	props := make(map[string]any, len(fields))
	for name, t := range fields {
		if len(t.Enum) > 0 {
			props[name] = map[string]any{"enum": toAny(t.Enum)}
			continue
		}
		props[name] = map[string]any{"type": "string"}
	}
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required":   toAny(slices.Sorted(maps.Keys(fields))),
		"properties": props,
	}
}

// Validate checks an agent's actual output against the step's declaration: every
// declared field present, every value a string, enum values members, and NO
// undeclared fields. A stray key means the model improvised, and improvisation is
// the signal that the rest is untrustworthy — so any failure rejects the output
// whole. No coercion, no defaults, no partial acceptance.
func (s Step) Validate(got map[string]any) error {
	fields := s.Fields()
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		v, ok := got[name]
		if !ok {
			return fmt.Errorf("output is missing declared field %q", name)
		}
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("output field %q must be a string, got %T", name, v)
		}
		if e := fields[name].Enum; len(e) > 0 && !slices.Contains(e, str) {
			return fmt.Errorf("output field %q is %q, not one of: %s", name, str, strings.Join(e, ", "))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(got)) {
		if _, ok := fields[name]; ok {
			continue
		}
		if _, ok := reserved[name]; ok {
			continue // backend-produced, not model-produced
		}
		return fmt.Errorf("output has undeclared field %q; declared: %s",
			name, strings.Join(fieldNames(s), ", "))
	}
	return nil
}

func toAny[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}
