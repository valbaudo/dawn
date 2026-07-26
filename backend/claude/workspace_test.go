package claude

import (
	"strings"
	"testing"

	"github.com/valbaudo/dawn"
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
