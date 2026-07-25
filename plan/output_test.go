package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadPlan(t *testing.T, yaml string) (*Plan, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

const head = "version: 1\nagents:\n  w: {backend: claude, model: sonnet}\nsteps:\n"

// THE load-time guarantee: a reference to a field the upstream step does not
// declare fails before a token is spent. Without typed output this was a runtime
// lookup, i.e. a 3am failure after paying for step 1.
func TestReferenceToUndeclaredFieldIsALoadError(t *testing.T) {
	_, err := loadPlan(t, head+
		"  - id: a\n    agent: w\n    prompt: x\n    output: {summary: string}\n"+
		"  - id: b\n    agent: w\n    prompt: y\n    inputs: {s: steps.a.sevrty}\n")
	if err == nil {
		t.Fatal("a reference to an undeclared field must fail at load time")
	}
	// the error has to be actionable: name the step and list what it does declare
	for _, want := range []string{"sevrty", "summary"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestReferenceToDeclaredFieldLoads(t *testing.T) {
	if _, err := loadPlan(t, head+
		"  - id: a\n    agent: w\n    prompt: x\n    output: {summary: string}\n"+
		"  - id: b\n    agent: w\n    prompt: y\n    inputs: {s: steps.a.summary}\n"); err != nil {
		t.Fatal(err)
	}
}

// A step that declares no output implicitly declares {text: string}, so a
// reference to .text resolves.
func TestDefaultOutputIsText(t *testing.T) {
	if _, err := loadPlan(t, head+
		"  - id: a\n    agent: w\n    prompt: x\n"+
		"  - id: b\n    agent: w\n    prompt: y\n    inputs: {s: steps.a.text}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlan(t, head+
		"  - id: a\n    agent: w\n    prompt: x\n"+
		"  - id: b\n    agent: w\n    prompt: y\n    inputs: {s: steps.a.nope}\n"); err == nil {
		t.Fatal("only .text exists by default")
	}
}

// A workspace step's auto-produced names may be referenced but never declared.
func TestReservedNames(t *testing.T) {
	if _, err := loadPlan(t, head+
		"  - id: a\n    agent: w\n    prompt: x\n"+
		"  - id: b\n    agent: w\n    prompt: y\n    inputs: {r: steps.a.workspace}\n"); err != nil {
		t.Fatalf("referencing a reserved name must be allowed: %v", err)
	}
	_, err := loadPlan(t, head+"  - id: a\n    agent: w\n    prompt: x\n    output: {workspace: string}\n")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("declaring a reserved name must fail, got: %v", err)
	}
}

func TestTypeForms(t *testing.T) {
	p, err := loadPlan(t, head+"  - id: a\n    agent: w\n    prompt: x\n    output:\n      s: string\n      u: [low, high]\n")
	if err != nil {
		t.Fatal(err)
	}
	f := p.Steps[0].Fields()
	if f["s"].Enum != nil {
		t.Fatal("`string` must not carry an enum")
	}
	if got := f["u"].Enum; len(got) != 2 || got[0] != "low" {
		t.Fatalf("enum = %v", got)
	}

	for name, y := range map[string]string{
		"unknown keyword": "  - id: a\n    agent: w\n    prompt: x\n    output: {n: number}\n",
		"empty enum":      "  - id: a\n    agent: w\n    prompt: x\n    output: {n: []}\n",
		"nested record":   "  - id: a\n    agent: w\n    prompt: x\n    output: {n: {deep: string}}\n",
		"scalar sugar":    "  - id: a\n    agent: w\n    prompt: x\n    output: text\n",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := loadPlan(t, head+y); err == nil {
				t.Fatal("expected a parse error")
			}
		})
	}
}

// Schema compiles with additionalProperties:false and ALL fields required — the
// author never writes those, which is what makes "no optional fields" hold.
func TestSchemaAlwaysStrict(t *testing.T) {
	s := Step{Output: map[string]Type{"a": {}, "b": {Enum: []string{"x", "y"}}}}
	sch := s.Schema()
	if sch["additionalProperties"] != false {
		t.Fatal("additionalProperties must be false")
	}
	req, _ := sch["required"].([]any)
	if len(req) != 2 {
		t.Fatalf("every declared field must be required, got %v", req)
	}
	props, _ := sch["properties"].(map[string]any)
	if _, ok := props["b"].(map[string]any)["enum"]; !ok {
		t.Fatal("an enum field must compile to an enum")
	}
}

func TestValidateOutput(t *testing.T) {
	s := Step{Output: map[string]Type{"summary": {}, "urgency": {Enum: []string{"low", "high"}}}}
	if err := s.Validate(map[string]any{"summary": "ok", "urgency": "low"}); err != nil {
		t.Fatalf("conforming output rejected: %v", err)
	}
	bad := map[string]struct {
		out  map[string]any
		want string
	}{
		"missing field":    {map[string]any{"summary": "ok"}, "urgency"},
		"undeclared field": {map[string]any{"summary": "ok", "urgency": "low", "extra": "x"}, "extra"},
		"enum not member":  {map[string]any{"summary": "ok", "urgency": "medium"}, "medium"},
		"wrong kind":       {map[string]any{"summary": 7, "urgency": "low"}, "summary"},
	}
	for name, c := range bad {
		t.Run(name, func(t *testing.T) {
			err := s.Validate(c.out)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error should mention %q, got: %v", c.want, err)
			}
		})
	}
}

// A backend's auto-produced names are not "undeclared" — they are not model output.
func TestValidateIgnoresReserved(t *testing.T) {
	s := Step{Output: map[string]Type{"text": {}}}
	if err := s.Validate(map[string]any{"text": "hi", "diff": "--- a\n+++ b"}); err != nil {
		t.Fatalf("reserved backend fields must not trip validation: %v", err)
	}
}
