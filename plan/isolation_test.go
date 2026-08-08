package plan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/dawn/store"
)

// SPEC has always said "at most one workspace input per step". Until this check
// nothing enforced it, and the runtime resolved the cwd by Go map order —
// measured 173/27 over 200 calls, a skew that passes every hand test and flips
// overnight. Two workspaces means two working directories, which is not a thing.
func TestTwoWorkspaceInputsIsALoadError(t *testing.T) {
	_, err := loadPlan(t, head+
		"  build:\n    agent: claude-ws/sonnet\n    prompt: build\n"+
		"  docs:\n    agent: claude-ws/sonnet\n    prompt: doc\n"+
		"  merge:\n    agent: claude-ws/opus\n    prompt: merge\n"+
		"    inputs: {a: build.workspace, b: docs.workspace}\n")
	if err == nil {
		t.Fatal("two workspace inputs must be refused at load time")
	}
	// Both names, so the author knows which two to reconcile.
	for _, want := range []string{"workspace inputs", "a", "b"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name %q, got: %v", want, err)
		}
	}
}

// One workspace plus any number of scalars is the normal case and must still load.
func TestOneWorkspaceInputWithScalarsLoads(t *testing.T) {
	if _, err := loadPlan(t, head+
		"  audit:\n    agent: claude/opus\n    prompt: find\n    outputs: {bug: string}\n"+
		"  build:\n    agent: claude-ws/sonnet\n    prompt: build\n"+
		"  fix:\n    agent: claude-ws/opus\n    prompt: fix\n"+
		"    inputs: {repo: build.workspace, bug: audit.bug}\n"); err != nil {
		t.Fatalf("one workspace and a scalar is the normal case: %v", err)
	}
}

// The state dir defaults to .dawn relative to the cwd, and `--in .` is the
// documented gesture — so the store lands INSIDE the tree being captured, and
// `git add -A` sweeps blobs, the journal and the object store into the agent's
// own workspace while git writes objects into that same tree mid-scan. The
// symptom is an unstable root sha: identical source, a different ref every run,
// so nothing ever hits the cache.
func TestStateDirIsNotCapturedIntoTheWorkspace(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := OpenJournal(filepath.Join(work, ".dawn"))
	if err != nil {
		t.Fatal(err)
	}
	// dawn owns the exclusion now, and states it: the state dir is passed to the
	// store rather than hidden behind an ignore file git happened to honour.
	trees := store.NewTrees(store.NewMem(), filepath.Join(work, ".dawn"))
	ctx := context.Background()

	first, err := trees.Capture(ctx, work, "")
	if err != nil {
		t.Fatal(err)
	}
	// Churn the state dir the way a run does, without touching a single source file.
	for i := 0; i < 3; i++ {
		if err := j.Append(Entry{Key: "k", Ref: "sha256:deadbeef", Step: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	second, err := trees.Capture(ctx, work, "")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("state churn changed the workspace ref: %s -> %s", first, second)
	}

	out := t.TempDir()
	if err := trees.Materialize(ctx, first, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, ".dawn")); !os.IsNotExist(err) {
		t.Fatal("the state dir must not appear in the agent's workspace")
	}
}
