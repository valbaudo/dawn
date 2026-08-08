// Package plan is dawn's static-DAG runner: a strict, control-flow-free description
// of agent steps wired by typed references, executed in dependency order. There is
// deliberately no templating and no if/loop/map — a step names its inputs, the
// runner materializes them, and any unknown key is a parse error. Orchestration
// that needs branches, loops or retries belongs in code around dawn, not here.
//
// See SPEC.md for the language and, more usefully, for what it refuses.
package plan

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/valbaudo/dawn/gate"
	"github.com/valbaudo/dawn/store"
	"gopkg.in/yaml.v3"
)

// Plan is a parsed pipeline. `steps` is the only top-level key: with `version:`
// cut, it is also the file's one self-identifying token, so a typo at the top
// level reports as an unknown key rather than as a malformed step.
type Plan struct {
	Steps map[string]Step `yaml:"steps"`
}

// Step is one invocation, keyed by its id in the Plan. Dependencies come from the
// references in Inputs — there is no `needs:`, because an edge carrying no data
// cannot enter the identity key, and an ordering constraint outside the key is one
// that silently stops applying the moment the step becomes a cache hit.
type Step struct {
	Agent   string            `yaml:"agent"`             // "<backend>/<model>"
	Prompt  string            `yaml:"prompt"`            //
	Inputs  map[string]string `yaml:"inputs,omitempty"`  // name -> "<step>.<field>"
	Outputs map[string]Type   `yaml:"outputs,omitempty"` // field -> type; omitted => {text: string}
	Expect  []string          `yaml:"expect,omitempty"`  // paths that must exist in the captured tree
	Gate    *Gate             `yaml:"gate,omitempty"`    // optional acceptance panel
}

// Agent is a backend and a model, written as ONE string (`claude/sonnet`) because
// a name pointing at a 2-tuple is indirection carrying no information. Both halves
// are mandatory: an omitted model would let the CLI resolve from ambient account
// state, which is a variable the identity key cannot see.
type Agent struct {
	Backend string
	Model   string
}

// ParseAgent splits on the FIRST slash, so a model id containing slashes
// (openrouter/anthropic/claude-opus) survives intact.
func ParseAgent(s string) (Agent, error) {
	b, m, ok := strings.Cut(s, "/")
	if !ok || b == "" || m == "" {
		return Agent{}, fmt.Errorf("agent %q must be <backend>/<model>", s)
	}
	return Agent{Backend: b, Model: m}, nil
}

func (a Agent) String() string { return a.Backend + "/" + a.Model }

// Type is a declared output field's type. Exactly two forms:
//
//	summary: string              // the keyword
//	urgency: [low, medium, high] // a sequence IS an enum of its members
//
// No numbers, booleans, arrays or nested records: the reference grammar stops at
// <step>.<field>, so declaring depth buys nothing checkable, and value constraints
// are the gate's job. Two forms also means the coercion question never arises.
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

// Gate is an independent acceptance panel. It is DATA, not control flow: the
// author writes no branch, no condition and no loop variable. The loop lives in
// the gate library and this only configures it, the way `retries: 3` configures CI.
//
// It is also what lets the language refuse if/loop/reduce entirely — a quorum needs
// counting, repair needs iteration, and halting needs a conditional. Packaging the
// one legitimate use of all three as data means none of them exists as syntax.
//
// A step whose panel never reaches quorum FAILS. In an unattended run that is the
// point: nothing downstream should proceed on work the panel refused.
type Gate struct {
	Judges   []string `yaml:"judges"`           // "<backend>/<model>" each
	Criteria string   `yaml:"criteria"`         // the standard, not a task
	Quorum   *int     `yaml:"quorum,omitempty"` // nil => majority; explicit must be 1..N
}

// Threshold resolves the panel's approval bar. Both the runner and the identity
// key call THIS — never their own copy — so the number hashed can never drift from
// the number enforced.
func (g Gate) Threshold() int {
	if g.Quorum != nil {
		return *g.Quorum
	}
	return gate.Majority(len(g.Judges))
}

// Attempts is the repair bound. Fixed, not a key: a bounded loop is the
// requirement, a tunable one is not, and a result accepted under 3 attempts is
// equally accepted under 5 — which is why it is excluded from the identity key.
const Attempts = 3

// RootStep is the reserved step id bound by `--in DIR`. It produces `workspace`
// and nothing else, which keeps a machine-specific path out of the file whose
// bytes are the plan's identity.
const RootStep = "in"

// reserved names a tree-capturing step produces automatically. Referenceable,
// never declarable, so an author cannot shadow them.
var reserved = map[string]string{
	"workspace": "the captured tree",
}

// stepID constrains ids so a reference can never be ambiguous: no dot means
// <step>.<field> always splits in exactly one place.
var stepID = regexp.MustCompile(`^[a-z0-9_-]+$`)

// Load reads a plan with strict decoding: unknown keys are errors, so a
// control-flow keyword like `loop:` fails loudly instead of being ignored.
// Duplicate step ids come free from the map form — yaml.v3 reports them with the
// line of the earlier definition.
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

// Fields returns the step's declared output fields, defaulting to {text: string}.
// Resolving the default HERE is what lets a reference to an undeclared step's
// `.text` still check out at load time.
func (s Step) Fields() map[string]Type {
	if len(s.Outputs) == 0 {
		return map[string]Type{"text": {}}
	}
	return s.Outputs
}

// canonicalExpect is what the engine — the identity key, the invocation, the
// capture assertion — actually sees. Cleaned as well as sorted, so `./dist/out`
// and `dist/out` are one question asked once rather than two cache misses.
func (s Step) canonicalExpect() []string {
	if len(s.Expect) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.Expect))
	for _, e := range s.Expect {
		clean, err := store.NormalizeWorkspacePath(e)
		if err != nil {
			clean = e // validate() already refused this; do not mask it here
		}
		out = append(out, clean)
	}
	slices.Sort(out)
	return out
}

// IDs returns the plan's step ids, sorted — the stable order everything else
// iterates in, so a map's randomization never reaches execution or a hash.
func (p *Plan) IDs() []string { return slices.Sorted(maps.Keys(p.Steps)) }

func (p *Plan) validate() error {
	if len(p.Steps) == 0 {
		return fmt.Errorf("no steps")
	}
	for _, id := range p.IDs() {
		s := p.Steps[id]
		switch {
		case id == RootStep:
			return fmt.Errorf("step id %q is reserved for --in", RootStep)
		case !stepID.MatchString(id):
			return fmt.Errorf("step %q: id must match %s", id, stepID)
		case s.Prompt == "":
			return fmt.Errorf("step %q: missing prompt", id)
		}
		if _, err := ParseAgent(s.Agent); err != nil {
			return fmt.Errorf("step %q: %w", id, err)
		}
		for _, expected := range s.Expect {
			if _, err := store.NormalizeWorkspacePath(expected); err != nil {
				return fmt.Errorf("step %q expect %q: %w", id, expected, err)
			}
		}
		for field := range s.Outputs {
			if why, bad := reserved[field]; bad {
				return fmt.Errorf("step %q: %q is reserved (%s) and cannot be declared", id, field, why)
			}
		}
		if g := s.Gate; g != nil {
			switch {
			case len(g.Judges) == 0:
				return fmt.Errorf("step %q: gate needs at least one judge", id)
			case g.Criteria == "":
				return fmt.Errorf("step %q: gate needs criteria", id)
			case g.Quorum != nil && (*g.Quorum < 1 || *g.Quorum > len(g.Judges)):
				return fmt.Errorf("step %q: gate quorum %d out of range; must be 1..%d, or omitted for a majority",
					id, *g.Quorum, len(g.Judges))
			}
			for _, j := range g.Judges {
				if _, err := ParseAgent(j); err != nil {
					return fmt.Errorf("step %q: gate judge: %w", id, err)
				}
			}
		}
	}

	for _, id := range p.IDs() {
		s := p.Steps[id]
		var trees []string // input names that bind a directory, not a scalar
		for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
			did, field, err := ParseRef(s.Inputs[name])
			if err != nil {
				return fmt.Errorf("step %q input %q: %w", id, name, err)
			}
			if field == "workspace" {
				trees = append(trees, name)
			}
			if did == RootStep {
				if field != "workspace" {
					return fmt.Errorf("step %q input %q: %s produces only `workspace`", id, name, RootStep)
				}
				continue
			}
			up, ok := p.Steps[did]
			if !ok {
				return fmt.Errorf("step %q input %q references unknown step %q; known: %s",
					id, name, did, strings.Join(p.IDs(), ", "))
			}
			// THE load-time guarantee: the referenced field must be declared
			// upstream. Because conformance is checked at runtime and there are no
			// optional fields, a reference that passes here can never resolve to a
			// missing value.
			if _, ok := up.Fields()[field]; ok {
				continue
			}
			if _, ok := reserved[field]; ok {
				continue // auto-produced by a tree-capturing step
			}
			return fmt.Errorf("step %q input %q: step %q has no output field %q; it declares: %s",
				id, name, did, field, strings.Join(fieldNames(up), ", "))
		}
		// The workspace input IS the working directory, so a second one asks for two
		// cwds and gets whichever the runtime happens to pick. SPEC has always said
		// "at most one"; until this check, nothing made it true.
		if len(trees) > 1 {
			return fmt.Errorf("step %q has %d workspace inputs (%s); at most one, because the workspace input is the step's working directory",
				id, len(trees), strings.Join(trees, ", "))
		}
	}
	_, err := p.order()
	return err
}

// ParseRef splits a reference into its two segments. Exactly two: step ids cannot
// contain a dot, so there is one parse rule and one error message — the same one
// the loader uses and the CLI uses for a REF argument.
func ParseRef(s string) (step, field string, err error) {
	a, b, ok := strings.Cut(s, ".")
	if !ok || a == "" || b == "" || strings.Contains(b, ".") {
		return "", "", fmt.Errorf("reference %q must be <step>.<field>", s)
	}
	return a, b, nil
}

// order returns step ids in dependency order. Ids are visited sorted so the
// topological order is stable across runs rather than a map's randomization.
func (p *Plan) order() ([]string, error) {
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
		s := p.Steps[id]
		for _, name := range slices.Sorted(maps.Keys(s.Inputs)) {
			did, _, err := ParseRef(s.Inputs[name])
			if err != nil {
				return err
			}
			if did == RootStep {
				continue
			}
			if _, ok := p.Steps[did]; !ok {
				continue // validate reports this with a better message
			}
			if err := visit(did); err != nil {
				return err
			}
		}
		state[id] = done
		out = append(out, id)
		return nil
	}
	for _, id := range p.IDs() {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fieldNames lists a step's declared output fields, sorted, for error messages
// that tell an author what they could have meant.
func fieldNames(s Step) []string { return slices.Sorted(maps.Keys(s.Fields())) }

// Schema compiles a step's declared output into JSON Schema. additionalProperties
// is always false and every declared field is always required — the author never
// writes those, which makes "no optional fields" unviolatable rather than
// documented, and that is what makes the load-time check a theorem.
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

// Validate checks an agent's actual output against the declaration: every declared
// field present, every value a string, enum values members, and NO undeclared
// fields. Any failure rejects the output whole — a stray key means the model
// improvised, and improvisation is the signal that the rest is untrustworthy.
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
		return fmt.Errorf("output has undeclared field %q; declared: %s", name, strings.Join(fieldNames(s), ", "))
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
