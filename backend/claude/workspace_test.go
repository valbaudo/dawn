package claude

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
