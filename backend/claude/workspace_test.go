package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/store"
)

// gitDiff is the non-trivial capture logic (stage everything, diff against HEAD).
// This exercises it against a real temp repo — no claude, no network.
func TestGitDiffCapturesModifiedAndNewFiles(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	git := func(args ...string) {
		if out, err := gitCmd(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	write(t, dir, "a.txt", "one\n")
	git("add", "-A")
	git("commit", "-qm", "init")

	// modify a tracked file and add an untracked one
	write(t, dir, "a.txt", "one\ntwo\n")
	write(t, dir, "b.txt", "brand new\n")

	diff, err := gitDiff(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "b.txt") {
		t.Fatalf("diff should include the new file b.txt:\n%s", diff)
	}
	if !strings.Contains(diff, "+two") {
		t.Fatalf("diff should include the added line 'two':\n%s", diff)
	}
}

func TestGitDiffEmptyWhenNoChange(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	git := func(args ...string) {
		if out, err := gitCmd(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	write(t, dir, "a.txt", "one\n")
	git("add", "-A")
	git("commit", "-qm", "init")

	diff, err := gitDiff(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(diff) != "" {
		t.Fatalf("clean tree should diff empty, got:\n%s", diff)
	}
}

// materialize is the forward-the-workspace path: a stored tree ref becomes a
// working dir with a git baseline, ready for the next agent — no claude needed.
func TestMaterializeRestoresTreeWithBaseline(t *testing.T) {
	src := t.TempDir()
	write(t, src, "calc.go", "package calc\n")
	data, err := tarTree(src)
	if err != nil {
		t.Fatal(err)
	}
	blobs := store.NewMem()
	ref, err := blobs.Put(data)
	if err != nil {
		t.Fatal(err)
	}

	w := Workspace{Store: blobs}
	dir, cleanup, err := w.materialize(context.Background(), aw.Ref{Kind: aw.KindWorkspace, URI: ref})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if got := read(t, dir, "calc.go"); got != "package calc\n" {
		t.Fatalf("materialized file = %q", got)
	}
	// a baseline commit must exist so a later gitDiff has a HEAD to diff against
	if out, err := gitCmd(context.Background(), dir, "rev-parse", "HEAD"); err != nil {
		t.Fatalf("materialized tree has no HEAD: %v: %s", err, out)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
