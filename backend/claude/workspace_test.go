package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/store"
)

// The loader refuses two workspace inputs before a token is spent, but the check
// is repeated at the backend because that is where every caller routes — a
// backend used directly has no plan in front of it. Ranging the map and taking
// the first match resolved the cwd at random: 173/27 over 200 calls.
func TestWorkspaceInputRefusesAmbiguity(t *testing.T) {
	two := map[string]dawn.Ref{
		"alpha": {Kind: dawn.KindWorkspace, URI: "tree-ALPHA"},
		"omega": {Kind: dawn.KindWorkspace, URI: "tree-OMEGA"},
	}
	// Deterministic: the same refusal every time, not one-in-eight.
	for i := 0; i < 200; i++ {
		if _, err := workspaceInput(two); err == nil {
			t.Fatal("two workspace refs must be an error, never a coin flip")
		}
	}
	_, err := workspaceInput(two)
	for _, want := range []string{"alpha", "omega", "working directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %q, got: %v", want, err)
		}
	}

	if _, err := workspaceInput(map[string]dawn.Ref{
		"repo": {Kind: dawn.KindWorkspace, URI: "tree-ONE"},
		"bug":  {Kind: dawn.KindValue, URI: "sha256:abc"},
	}); err != nil {
		t.Fatalf("one workspace alongside scalars is the normal case: %v", err)
	}
	if _, err := workspaceInput(nil); err == nil {
		t.Fatal("no workspace ref must be an error: there is nothing to materialize")
	}
}

// The end-to-end shape of the round-trip bug: step A builds an artifact under an
// ignored dist/ and declares it; step B edits source and has no reason to
// re-declare A's artifact. Before CaptureFrom, B's output tree silently lost it,
// so a later step or `dawn show B.workspace | tar -x` produced a workspace with
// no binary in it and nothing anywhere saying why.
func TestWorkspaceKeepsAnUpstreamArtifactItDidNotDeclare(t *testing.T) {
	trees, err := store.NewTrees(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	src := t.TempDir()
	for name, body := range map[string]string{
		".gitignore": "dist/\n",
		"main.go":    "package main\n",
		"dist/app":   "a binary\n",
	} {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// step A: declares the artifact, forcing it past .gitignore
	stepA, err := trees.Capture(ctx, src, "dist/app")
	if err != nil {
		t.Fatal(err)
	}

	// step B: edits source only, declares nothing
	bin := fakeCLI(t, `echo 'package main // fixed' > main.go
echo '{"type":"result","is_error":false,"result":"{\"summary\":\"fixed it\"}","usage":{}}'`)
	w := Workspace{Model: "haiku", Bin: bin, Trees: trees}
	res, err := w.Invoke(ctx, dawn.Invocation{
		Prompt: "fix main.go",
		Inputs: map[string]dawn.Ref{"repo": {Kind: dawn.KindWorkspace, URI: stepA}},
		Schema: map[string]any{
			"type": "object", "additionalProperties": false,
			"required":   []any{"summary"},
			"properties": map[string]any{"summary": map[string]any{"type": "string"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	if err := trees.Materialize(ctx, res.Produced["workspace"].URI, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "dist", "app")); err != nil {
		t.Fatal("step A's declared artifact vanished passing through step B")
	}
	got, err := os.ReadFile(filepath.Join(out, "main.go"))
	if err != nil || string(got) != "package main // fixed\n" {
		t.Fatalf("step B's edit must be captured, got %q (%v)", got, err)
	}
	// The diff is against the INPUT tree, so it shows the edit and not the
	// artifact appearing out of nowhere.
	diff, _ := res.Output["diff"].(string)
	if !strings.Contains(diff, "main.go") {
		t.Fatalf("diff should show the edit: %s", diff)
	}
	if strings.Contains(diff, "dist/app") {
		t.Fatalf("an untouched carried-through artifact must not appear in the diff: %s", diff)
	}
}
