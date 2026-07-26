package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func trees(t *testing.T) *Trees {
	t.Helper()
	tr, err := NewTrees(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// THE regression this store exists for: identical content must give an identical
// ref regardless of mtime. The previous tar-based implementation failed this,
// which meant "content-addressed" was not true.
func TestCaptureIsContentAddressedNotTimeAddressed(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	capture := func(mt time.Time) string {
		d := t.TempDir()
		p := writeFile(t, d, "a.txt", "identical bytes\n")
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		ref, err := tr.Capture(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	older := capture(time.Unix(1000000000, 0))
	newer := capture(time.Unix(1700000000, 0))
	if older != newer {
		t.Fatalf("identical bytes gave different refs: %s vs %s", older, newer)
	}
}

func TestCaptureMaterializeRoundTrip(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	src := t.TempDir()
	writeFile(t, src, "a.txt", "alpha\n")
	writeFile(t, src, "sub/b.txt", "beta\n")

	ref, err := tr.Capture(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.txt": "alpha\n", "sub/b.txt": "beta\n"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The tar implementation hard-failed on any symlink ("archive: write too long")
// and dropped them on the way back out.
func TestSymlinksRoundTrip(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	src := t.TempDir()
	writeFile(t, src, "real.txt", "content\n")
	if err := os.Symlink("real.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	ref, err := tr.Capture(ctx, src)
	if err != nil {
		t.Fatalf("capture with a symlink failed: %v", err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link.txt came back as a regular file, not a symlink")
	}
}

// Two captures of the same tree dedup, and any two refs can be diffed against
// each other (not just against an immediately preceding baseline).
func TestDiffBetweenArbitraryRefs(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	d := t.TempDir()
	writeFile(t, d, "calc.go", "func Add(a, b int) int { return a - b }\n")
	v1, err := tr.Capture(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, d, "calc.go", "func Add(a, b int) int { return a + b }\n")
	v2, err := tr.Capture(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, d, "calc_test.go", "func TestAdd(t *testing.T) {}\n")
	v3, err := tr.Capture(ctx, d)
	if err != nil {
		t.Fatal(err)
	}

	// v1 -> v3 spans both changes, which a baseline-only design cannot express.
	diff, err := tr.Diff(ctx, v1, v3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "return a + b") || !strings.Contains(diff, "calc_test.go") {
		t.Fatalf("v1..v3 diff should span both changes:\n%s", diff)
	}
	if v1 == v2 || v2 == v3 {
		t.Fatal("distinct trees must have distinct refs")
	}
}

// A .gitignore is honored, so a capture and a diff agree on what counts as
// content (the tar version captured ignored junk the diff excluded).
func TestCaptureHonorsGitignore(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	d := t.TempDir()
	writeFile(t, d, ".gitignore", "junk/\n")
	writeFile(t, d, "keep.txt", "keep\n")
	writeFile(t, d, "junk/huge.bin", "ignore me\n")

	ref, err := tr.Capture(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "junk", "huge.bin")); !os.IsNotExist(err) {
		t.Fatal("ignored files must not enter the tree")
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Fatal("tracked files must survive")
	}
}

// A working dir that is itself a git repo must not leak its .git into the tree.
func TestCaptureIgnoresNestedGitDir(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	d := t.TempDir()
	writeFile(t, d, "a.txt", "alpha\n")
	writeFile(t, d, ".git/HEAD", "ref: refs/heads/main\n")

	ref, err := tr.Capture(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git", "HEAD")); !os.IsNotExist(err) {
		t.Fatal("a nested .git must not be captured as content")
	}
}

// `git add -A` HONORS .gitignore, so a declared artifact under an ignored
// directory (dist/, build/, target/ — the normal case) would be silently absent
// from the captured tree. Capture's `must` paths are forced past it.
func TestCaptureForcesDeclaredPathsPastGitignore(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	d := t.TempDir()
	writeFile(t, d, ".gitignore", "dist/\n")
	writeFile(t, d, "main.go", "package main\n")
	writeFile(t, d, "dist/dawn", "a binary\n")

	// without declaring it, the artifact is silently dropped
	plain, err := tr.Capture(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	bare := t.TempDir()
	if err := tr.Materialize(ctx, plain, bare); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bare, "dist", "dawn")); !os.IsNotExist(err) {
		t.Fatal("precondition: an ignored path should NOT be captured by default")
	}

	// declaring it forces it in
	forced, err := tr.Capture(ctx, d, "dist/dawn")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, forced, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "dist", "dawn"))
	if err != nil {
		t.Fatalf("declared artifact missing from the tree: %v", err)
	}
	if string(got) != "a binary\n" {
		t.Fatalf("declared artifact = %q", got)
	}
}

// A declared path the agent never produced must fail LOUDLY, at capture time —
// which is before the diff and before any judge is paid.
func TestCaptureFailsOnAMissingDeclaredPath(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	d := t.TempDir()
	writeFile(t, d, "main.go", "package main\n")

	_, err := tr.Capture(ctx, d, "dist/never-built")
	if err == nil {
		t.Fatal("a declared path that was never produced must fail the capture")
	}
	if !strings.Contains(err.Error(), "dist/never-built") {
		t.Fatalf("the error must name the missing path, got: %v", err)
	}
}

// A relative working directory must capture correctly. It did not: the command
// runs with cmd.Dir set to the same path, so a relative GIT_WORK_TREE was
// resolved twice — `--in examples/calc` went looking for
// examples/calc/examples/calc. Found by pointing --in at a real relative path.
func TestCaptureAcceptsARelativeWorkDir(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	base := t.TempDir()
	work := filepath.Join(base, "repo")
	writeFile(t, work, "a.txt", "alpha\n")

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	ref, err := tr.Capture(ctx, "repo") // relative, as a CLI user would type it
	if err != nil {
		t.Fatalf("a relative work dir must capture: %v", err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(got) != "alpha\n" {
		t.Fatalf("relative capture lost content: %q %v", got, err)
	}
}

// A workspace ref IS a git tree sha, so materializing a tree and capturing it
// back must be the identity. It was not: `git add -A` honors .gitignore for
// UNTRACKED files, and an index minted per call has nothing tracked in it, so a
// declared artifact forced past .gitignore by one step was silently dropped by
// the next — which had no reason to re-declare another step's artifact.
// Measured before the fix: 6fd375c… -> e1565f2…, with dist/app gone.
func TestCaptureFromRoundTripsAndKeepsDeclaredArtifacts(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	src := t.TempDir()
	writeFile(t, src, ".gitignore", "dist/\nnode_modules/\n")
	writeFile(t, src, "main.go", "package main\n")
	writeFile(t, src, "dist/app", "a binary\n")

	// step A declares its artifact, forcing it past .gitignore
	base, err := tr.Capture(ctx, src, "dist/app")
	if err != nil {
		t.Fatal(err)
	}

	// step B receives A's workspace and does not re-declare A's artifact
	work := t.TempDir()
	if err := tr.Materialize(ctx, base, work); err != nil {
		t.Fatal(err)
	}
	got, err := tr.CaptureFrom(ctx, work, base)
	if err != nil {
		t.Fatal(err)
	}
	if got != base {
		t.Fatalf("materialize then capture must be the identity: %s -> %s", base, got)
	}

	// the agent edits a tracked file and creates ignored junk of its own
	writeFile(t, work, "main.go", "package main // edited\n")
	writeFile(t, work, "node_modules/x/y.js", "junk\n")
	next, err := tr.CaptureFrom(ctx, work, base)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := tr.Materialize(ctx, next, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "dist", "app")); err != nil {
		t.Fatal("a declared artifact must survive a step that did not declare it")
	}
	// The filter still does the job it exists for: only NEW ignored files are cut.
	if _, err := os.Stat(filepath.Join(out, "node_modules", "x", "y.js")); !os.IsNotExist(err) {
		t.Fatal("newly created ignored files must still be filtered out")
	}
	edited, err := os.ReadFile(filepath.Join(out, "main.go"))
	if err != nil || string(edited) != "package main // edited\n" {
		t.Fatalf("an edit to a tracked file must be captured, got %q (%v)", edited, err)
	}

	// A baseline must not resurrect what the agent deleted.
	if err := os.Remove(filepath.Join(work, "dist", "app")); err != nil {
		t.Fatal(err)
	}
	after, err := tr.CaptureFrom(ctx, work, base)
	if err != nil {
		t.Fatal(err)
	}
	gone := t.TempDir()
	if err := tr.Materialize(ctx, after, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gone, "dist", "app")); !os.IsNotExist(err) {
		t.Fatal("a deleted file must not be resurrected by the baseline")
	}
}
