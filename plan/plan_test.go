package plan

import (
	"strings"
	"testing"
)

// UNSAFE and UNTIDY are different. A path that cannot escape the workspace is
// normalized rather than refused: `./dist/out` names exactly `dist/out`, and
// failing a plan that ran yesterday over a leading `./` gives the author nothing
// to fix. The engine — key, invocation, capture assertion — sees the clean form,
// so the two spellings are one question asked once, not two cache misses.
func TestLoadNormalizesUntidyExpectPaths(t *testing.T) {
	for _, spelling := range []string{"dist/out", "./dist/out", "dist//out", "dist/./out", "dist/sub/../out"} {
		t.Run(spelling, func(t *testing.T) {
			y := head + "  build:\n    agent: x/tree\n    prompt: build\n    expect: [\"" + spelling + "\"]\n"
			p, err := loadPlan(t, y)
			if err != nil {
				t.Fatalf("%q names dist/out and must load: %v", spelling, err)
			}
			got := p.Steps["build"].canonicalExpect()
			if len(got) != 1 || got[0] != "dist/out" {
				t.Fatalf("canonicalExpect() = %v, want [dist/out]", got)
			}
		})
	}

	// And the key must not distinguish them, which is the point of normalizing.
	key := func(spelling string) string {
		t.Helper()
		y := head + "  build:\n    agent: x/tree\n    prompt: build\n    expect: [\"" + spelling + "\"]\n"
		p, err := loadPlan(t, y)
		if err != nil {
			t.Fatal(err)
		}
		s := p.Steps["build"]
		k, err := s.Key("build", Agent{Backend: "x", Model: "tree"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if a, b := key("dist/out"), key("./dist/out"); a != b {
		t.Fatalf("spelling changed the identity key: %s != %s", a, b)
	}
}

func TestLoadRejectsInvalidExpectPaths(t *testing.T) {
	for name, expect := range map[string]string{
		"empty":            "",
		"absolute":         "/tmp/output",
		"volume-qualified": "C:/output",
		"dot":              ".",
		"parent":           "../output",
		"backslash":        `dist\\output`,
		"newline":          "dist\\noutput",
		"control":          "dist\\u0001output",
	} {
		t.Run(name, func(t *testing.T) {
			y := head + "  build:\n    agent: x/tree\n    prompt: build\n    expect: [\"" + expect + "\"]\n"
			if _, err := loadPlan(t, y); err == nil || !strings.Contains(err.Error(), "expect") {
				t.Fatalf("invalid expect path %q must be rejected as authored plan data, got %v", expect, err)
			}
		})
	}
}

func TestLoadParsesValidPlan(t *testing.T) {
	p, err := loadPlan(t, head+
		"  draft:\n    agent: claude/sonnet\n    prompt: write\n    outputs: {text: string}\n"+
		"  edit:\n    agent: claude/opus\n    prompt: edit\n    inputs: {d: draft.text}\n")
	if err != nil {
		t.Fatal(err)
	}
	order, err := p.order()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "draft" || order[1] != "edit" {
		t.Fatalf("topological order wrong: %v", order)
	}
}

// Strict decode is the forward-compat mechanism: a control-flow keyword must fail
// loudly rather than be ignored. So must the keys the reduction cut.
func TestLoadRejectsRemovedAndForbiddenKeys(t *testing.T) {
	step := "  s:\n    agent: claude/m\n    prompt: hi\n"
	for name, y := range map[string]string{
		"loop":         head + step + "    loop: 3\n",
		"if":           head + step + "    if: something\n",
		"needs":        head + step + "    needs: [other]\n",
		"id":           head + step + "    id: s\n",
		"attempts":     head + step + "    gate: {judges: [claude/m], criteria: c, attempts: 5}\n",
		"version":      "version: 1\n" + head + step,
		"agents block": "agents:\n  w: {backend: claude, model: m}\n" + head + step,
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := loadPlan(t, y); err == nil {
				t.Fatal("expected a parse error")
			}
		})
	}
}

// The map form gives duplicate-id detection for free, with the line of the
// earlier definition.
func TestDuplicateStepIDIsAParseError(t *testing.T) {
	_, err := loadPlan(t, head+
		"  fix: {agent: claude/m, prompt: a}\n"+
		"  fix: {agent: claude/m, prompt: b}\n")
	if err == nil || !strings.Contains(err.Error(), "already defined") {
		t.Fatalf("expected a duplicate-key error, got: %v", err)
	}
}

func TestAgentMustBeBackendSlashModel(t *testing.T) {
	ok, err := ParseAgent("openrouter/anthropic/claude-opus")
	if err != nil {
		t.Fatal(err)
	}
	// splits on the FIRST slash, so slash-bearing model ids survive
	if ok.Backend != "openrouter" || ok.Model != "anthropic/claude-opus" {
		t.Fatalf("got %+v", ok)
	}
	for _, bad := range []string{"claude", "/model", "backend/", ""} {
		if _, err := ParseAgent(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	if _, err := loadPlan(t, head+"  s:\n    agent: claude\n    prompt: hi\n"); err == nil {
		t.Fatal("a bare backend must fail at load")
	}
}

func TestLoadDetectsCycle(t *testing.T) {
	_, err := loadPlan(t, head+
		"  x:\n    agent: claude/m\n    prompt: p\n    inputs: {a: y.text}\n"+
		"  y:\n    agent: claude/m\n    prompt: p\n    inputs: {a: x.text}\n")
	if err == nil {
		t.Fatal("a dependency cycle must be rejected")
	}
}

// THE load-time guarantee: a reference to an undeclared field fails before a
// token is spent, and the error names what the step actually declares.
func TestReferenceToUndeclaredFieldIsALoadError(t *testing.T) {
	_, err := loadPlan(t, head+
		"  a:\n    agent: claude/m\n    prompt: x\n    outputs: {summary: string}\n"+
		"  b:\n    agent: claude/m\n    prompt: y\n    inputs: {s: a.sevrty}\n")
	if err == nil {
		t.Fatal("a reference to an undeclared field must fail at load time")
	}
	for _, want := range []string{"sevrty", "summary"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestReferenceGrammarIsTwoSegments(t *testing.T) {
	for _, bad := range []string{"steps.a.text", "a", "a.b.c", ".text", "a."} {
		if _, _, err := ParseRef(bad); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
	id, field, err := ParseRef("audit.summary")
	if err != nil || id != "audit" || field != "summary" {
		t.Fatalf("got %q %q %v", id, field, err)
	}
}

// A step that declares no outputs implicitly declares {text: string}.
func TestDefaultOutputIsText(t *testing.T) {
	if _, err := loadPlan(t, head+
		"  a:\n    agent: claude/m\n    prompt: x\n"+
		"  b:\n    agent: claude/m\n    prompt: y\n    inputs: {s: a.text}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPlan(t, head+
		"  a:\n    agent: claude/m\n    prompt: x\n"+
		"  b:\n    agent: claude/m\n    prompt: y\n    inputs: {s: a.nope}\n"); err == nil {
		t.Fatal("only .text exists by default")
	}
}

func TestReservedNames(t *testing.T) {
	if _, err := loadPlan(t, head+
		"  a:\n    agent: claude-ws/m\n    prompt: x\n"+
		"  b:\n    agent: claude-ws/m\n    prompt: y\n    inputs: {r: a.workspace}\n"); err != nil {
		t.Fatalf("referencing a reserved name must be allowed: %v", err)
	}
	_, err := loadPlan(t, head+"  a:\n    agent: claude/m\n    prompt: x\n    outputs: {workspace: string}\n")
	if err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("declaring a reserved name must fail, got: %v", err)
	}
	// `in` is reserved for --in
	if _, err := loadPlan(t, head+"  in:\n    agent: claude/m\n    prompt: x\n"); err == nil {
		t.Fatal("declaring a step named `in` must fail")
	}
}

func TestTypeForms(t *testing.T) {
	p, err := loadPlan(t, head+"  a:\n    agent: claude/m\n    prompt: x\n    outputs:\n      s: string\n      u: [low, high]\n")
	if err != nil {
		t.Fatal(err)
	}
	f := p.Steps["a"].Fields()
	if f["s"].Enum != nil {
		t.Fatal("`string` must not carry an enum")
	}
	if got := f["u"].Enum; len(got) != 2 || got[0] != "low" {
		t.Fatalf("enum = %v", got)
	}
	for name, y := range map[string]string{
		"unknown keyword": "    outputs: {n: number}\n",
		"empty enum":      "    outputs: {n: []}\n",
		"nested record":   "    outputs: {n: {deep: string}}\n",
		"scalar sugar":    "    outputs: text\n",
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if _, err := loadPlan(t, head+"  a:\n    agent: claude/m\n    prompt: x\n"+y); err == nil {
				t.Fatal("expected a parse error")
			}
		})
	}
}

func TestSchemaAlwaysStrict(t *testing.T) {
	s := Step{Outputs: map[string]Type{"a": {}, "b": {Enum: []string{"x", "y"}}}}
	sch := s.Schema()
	if sch["additionalProperties"] != false {
		t.Fatal("additionalProperties must be false")
	}
	if req, _ := sch["required"].([]any); len(req) != 2 {
		t.Fatalf("every declared field must be required, got %v", req)
	}
	props, _ := sch["properties"].(map[string]any)
	if _, ok := props["b"].(map[string]any)["enum"]; !ok {
		t.Fatal("an enum field must compile to an enum")
	}
}

func TestValidateOutput(t *testing.T) {
	s := Step{Outputs: map[string]Type{"summary": {}, "urgency": {Enum: []string{"low", "high"}}}}
	if err := s.Validate(map[string]any{"summary": "ok", "urgency": "low"}); err != nil {
		t.Fatalf("conforming output rejected: %v", err)
	}
	for name, c := range map[string]struct {
		out  map[string]any
		want string
	}{
		"missing field":    {map[string]any{"summary": "ok"}, "urgency"},
		"undeclared field": {map[string]any{"summary": "ok", "urgency": "low", "extra": "x"}, "extra"},
		"enum not member":  {map[string]any{"summary": "ok", "urgency": "medium"}, "medium"},
		"wrong kind":       {map[string]any{"summary": 7, "urgency": "low"}, "summary"},
	} {
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

// A backend's auto-produced names are not "undeclared" — they are not model
// output. `workspace` is the only one left; `diff` went with git.
func TestValidateIgnoresReserved(t *testing.T) {
	s := Step{Outputs: map[string]Type{"text": {}}}
	if err := s.Validate(map[string]any{"text": "hi", "workspace": "tree-abc"}); err != nil {
		t.Fatalf("reserved backend fields must not trip validation: %v", err)
	}
	if err := s.Validate(map[string]any{"text": "hi", "diff": "no longer reserved"}); err == nil {
		t.Fatal("diff is not reserved any more and must fail as an undeclared field")
	}
}

func TestGateValidation(t *testing.T) {
	plan := func(g *Gate) *Plan {
		return &Plan{Steps: map[string]Step{"s": {Agent: "claude/m", Prompt: "p", Gate: g}}}
	}
	three := []string{"x/a", "x/b", "x/c"}
	for name, g := range map[string]*Gate{
		"no judges":       {Criteria: "x"},
		"no criteria":     {Judges: three},
		"bad judge spec":  {Judges: []string{"nope"}, Criteria: "x"},
		"quorum zero":     {Judges: three, Criteria: "x", Quorum: q(0)},
		"quorum negative": {Judges: three, Criteria: "x", Quorum: q(-1)},
		"quorum too high": {Judges: three, Criteria: "x", Quorum: q(4)},
	} {
		t.Run("rejects "+name, func(t *testing.T) {
			if err := plan(g).validate(); err == nil {
				t.Fatal("expected validation to reject this gate")
			}
		})
	}
	omitted := &Gate{Judges: three, Criteria: "c"}
	if err := plan(omitted).validate(); err != nil {
		t.Fatalf("an omitted quorum is legal: %v", err)
	}
	if got := omitted.Threshold(); got != 2 {
		t.Fatalf("omitted quorum over 3 judges = %d, want a majority of 2", got)
	}
	if (&Gate{Judges: three, Criteria: "c", Quorum: q(2)}).Threshold() != omitted.Threshold() {
		t.Fatal("writing the default explicitly must resolve identically")
	}
}
