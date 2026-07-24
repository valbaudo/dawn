// Package plan is aw's static-DAG runner: a strict, control-flow-free
// description of agent steps wired by typed references, executed in dependency
// order. There is deliberately no templating and no if/loop/map — a step names
// its inputs, the runner materializes them, and any unknown key is a parse
// error. Orchestration that needs branches, loops, or retries belongs in code
// around aw, not in this format.
package plan

import (
	"fmt"
	"os"
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
	Inputs map[string]Source `yaml:"inputs,omitempty"` // name -> source
	Output string            `yaml:"output,omitempty"` // typed field to extract; "" => raw text
	Gate   *Gate             `yaml:"gate,omitempty"`   // optional acceptance check
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

// Source is a typed reference to another step's output: "steps.<id>.<field>".
// It is a reference, not a template string — the runner materializes the value,
// it never substitutes text into a prompt.
type Source struct {
	From string `yaml:"from"`
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
	for _, s := range p.Steps {
		for _, n := range s.Needs {
			if !seen[n] {
				return fmt.Errorf("step %q needs unknown step %q", s.ID, n)
			}
		}
		for name, src := range s.Inputs {
			id, _, err := parseFrom(src.From)
			if err != nil {
				return fmt.Errorf("step %q input %q: %w", s.ID, name, err)
			}
			if !seen[id] {
				return fmt.Errorf("step %q input %q references unknown step %q", s.ID, name, id)
			}
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
		for _, src := range s.Inputs {
			did, _, _ := parseFrom(src.From)
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
