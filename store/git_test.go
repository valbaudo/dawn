package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
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

func TestCaptureIgnoresPersonalAutocrlf(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	clean, hostile := t.TempDir(), t.TempDir()
	writeFile(t, clean, "a.txt", "line one\r\nline two\r\n")
	writeFile(t, hostile, "a.txt", "line one\r\nline two\r\n")

	cleanRef, err := tr.Capture(ctx, clean)
	if err != nil {
		t.Fatal(err)
	}
	global := writeFile(t, t.TempDir(), "gitconfig", "[core]\n\tautocrlf = true\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	hostileRef, err := tr.Capture(ctx, hostile)
	if err != nil {
		t.Fatal(err)
	}
	if hostileRef != cleanRef {
		t.Fatalf("personal core.autocrlf changed capture: %s != %s", hostileRef, cleanRef)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, hostileRef, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "line one\r\nline two\r\n"; string(got) != want {
		t.Fatalf("materialized bytes = %q, want %q", got, want)
	}
}

func TestCaptureIgnoresPersonalExcludesFile(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	clean, hostile := t.TempDir(), t.TempDir()
	writeFile(t, clean, "keep.txt", "keep\n")
	writeFile(t, hostile, "keep.txt", "keep\n")

	cleanRef, err := tr.Capture(ctx, clean)
	if err != nil {
		t.Fatal(err)
	}
	excludes := writeFile(t, t.TempDir(), "excludes", "keep.txt\n")
	global := writeFile(t, t.TempDir(), "gitconfig", "[core]\n\texcludesFile = "+excludes+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	hostileRef, err := tr.Capture(ctx, hostile)
	if err != nil {
		t.Fatal(err)
	}
	if hostileRef != cleanRef {
		t.Fatalf("personal core.excludesFile changed capture: %s != %s", hostileRef, cleanRef)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, hostileRef, dst); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "keep.txt")); err != nil || string(got) != "keep\n" {
		t.Fatalf("keep.txt was excluded: %q (%v)", got, err)
	}
}

func TestCaptureIgnoresCommandScopeGitConfig(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	clean, hostile := t.TempDir(), t.TempDir()
	for _, dir := range []string{clean, hostile} {
		writeFile(t, dir, "keep.txt", "keep\r\n")
	}
	cleanRef, err := tr.Capture(ctx, clean)
	if err != nil {
		t.Fatal(err)
	}

	excludes := writeFile(t, t.TempDir(), "excludes", "keep.txt\n")
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "core.autocrlf")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_CONFIG_KEY_1", "core.excludesFile")
	t.Setenv("GIT_CONFIG_VALUE_1", excludes)
	hostileRef, err := tr.Capture(ctx, hostile)
	if err != nil {
		t.Fatal(err)
	}
	if hostileRef != cleanRef {
		t.Fatalf("command-scope Git config changed capture: %s != %s", hostileRef, cleanRef)
	}
}

func TestGitEnvFiltersInheritedConfigCaseInsensitively(t *testing.T) {
	t.Setenv("gIt_CoNfIg_PaRaMeTeRs", "'core.autocrlf=true'")
	t.Setenv("gIt_CoNfIg_CoUnT", "99")
	t.Setenv("gIt_AtTr_NoSyStEm", "0")

	env := (&Trees{gitDir: t.TempDir()}).env("", "")
	counts := make(map[string]int)
	values := make(map[string]string)
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_CONFIG_") || upper == "GIT_ATTR_NOSYSTEM" {
			counts[upper]++
			values[upper] = value
		}
	}
	if counts["GIT_CONFIG_PARAMETERS"] != 0 {
		t.Fatal("mixed-case GIT_CONFIG_PARAMETERS survived filtering")
	}
	if counts["GIT_CONFIG_COUNT"] != 1 || values["GIT_CONFIG_COUNT"] != "2" {
		t.Fatalf("GIT_CONFIG_COUNT entries = %d, value = %q", counts["GIT_CONFIG_COUNT"], values["GIT_CONFIG_COUNT"])
	}
	if counts["GIT_ATTR_NOSYSTEM"] != 1 || values["GIT_ATTR_NOSYSTEM"] != "1" {
		t.Fatalf("GIT_ATTR_NOSYSTEM entries = %d, value = %q", counts["GIT_ATTR_NOSYSTEM"], values["GIT_ATTR_NOSYSTEM"])
	}
}

func TestTreeOperationsIgnoreMalformedConfigParametersSetBeforeNewTrees(t *testing.T) {
	t.Setenv("GIT_CONFIG_PARAMETERS", "'unterminated")
	tr, err := NewTrees(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatalf("NewTrees inherited hostile Git config: %v", err)
	}
	ctx := context.Background()
	src := t.TempDir()
	writeFile(t, src, "a.txt", "one\n")
	before, err := tr.Capture(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, src, "a.txt", "two\n")
	after, err := tr.Capture(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, after, dst); err != nil {
		t.Fatal(err)
	}
	if diff, err := tr.Diff(ctx, before, after); err != nil || !strings.Contains(diff, "+two") {
		t.Fatalf("Diff under hostile config = %q, %v", diff, err)
	}
	var archive bytes.Buffer
	if err := tr.Archive(ctx, after, &archive); err != nil || archive.Len() == 0 {
		t.Fatalf("Archive under hostile config produced %d bytes, %v", archive.Len(), err)
	}
	roundTrip, err := tr.Capture(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip != after {
		t.Fatalf("subsequent Capture = %s, want %s", roundTrip, after)
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
	var missing *MissingPathError
	if !errors.As(err, &missing) {
		t.Fatalf("error type = %T, want *MissingPathError", err)
	}
	if missing.Path != "dist/never-built" {
		t.Fatalf("Path = %q", missing.Path)
	}
}

func TestCaptureRejectsUnsafeDeclaredPathsMechanically(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	for name, declared := range map[string]string{
		"empty":            "",
		"absolute":         "/tmp/output",
		"volume-qualified": "C:/output",
		"dot":              ".",
		"parent":           "../output",
		"traversal":        "dist/../output",
		"not normalized":   "dist//output",
		"backslash":        `dist\output`,
		"newline":          "dist\noutput",
		"control":          "dist\x01output",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tr.Capture(ctx, t.TempDir(), declared)
			if err == nil {
				t.Fatalf("unsafe declared path %q must fail", declared)
			}
			var missing *MissingPathError
			if errors.As(err, &missing) {
				t.Fatalf("unsafe path is configuration, not repairable absence: %v", err)
			}
		})
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

// `write-tree` writes objects nothing points at, so a bare store with no refs is
// one where every committed workspace is unreachable garbage by git's own
// definition. Reproduced before the fix: `git gc --prune=now` inside
// .dawn/trees left `fatal: failed to unpack tree object`. dawn never runs gc,
// but a durable artifact that survives only until someone runs a standard
// maintenance command in the state directory is not durable.
func TestCapturedTreesSurviveGitGC(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	src := t.TempDir()
	writeFile(t, src, ".gitignore", "dist/\n")
	writeFile(t, src, "main.go", "package main\n")
	writeFile(t, src, "dist/app", "a binary\n")

	ref, err := tr.Capture(ctx, src, "dist/app")
	if err != nil {
		t.Fatal(err)
	}
	// A second, unrelated tree, so the test would notice a pin that only ever
	// covers the most recent capture.
	other := t.TempDir()
	writeFile(t, other, "notes.md", "hello\n")
	ref2, err := tr.Capture(ctx, other)
	if err != nil {
		t.Fatal(err)
	}

	gc := exec.CommandContext(ctx, "git", "gc", "--prune=now")
	gc.Env = append(os.Environ(), "GIT_DIR="+tr.gitDir)
	if out, err := gc.CombinedOutput(); err != nil {
		t.Fatalf("gc failed: %v: %s", err, out)
	}

	for _, want := range []string{ref, ref2} {
		dst := t.TempDir()
		if err := tr.Materialize(ctx, want, dst); err != nil {
			t.Fatalf("gc destroyed committed tree %s: %v", want, err)
		}
	}
	// The forced artifact must come back too, not just the tree object.
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "dist", "app")); err != nil {
		t.Fatalf("a declared artifact must survive gc: %v", err)
	}
}

// nestedRepo makes dir a git repository with one committed file.
func nestedRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "-A"},
		{"-c", "user.email=a@b.c", "-c", "user.name=x", "commit", "-qm", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("nested repo setup %v: %v: %s", args, err, out)
		}
	}
}

// A directory with its own .git is staged as mode 160000 — a COMMIT reference
// that lives in the nested repo, not in dawn's store. Reproduced before this
// check: a tree holding `160000 commit 5f9bf40b… vendor/lib` materialized as
// [main.go]. vendor/lib was not empty, it was absent, and nothing errored.
func TestCaptureRefusesAnEmbeddedGitRepo(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	src := t.TempDir()
	writeFile(t, src, "main.go", "package main\n")
	writeFile(t, src, "vendor/lib/lib.go", "package lib\n")
	nestedRepo(t, filepath.Join(src, "vendor", "lib"))

	_, err := tr.Capture(ctx, src)
	if err == nil {
		t.Fatal("an embedded git repository must fail the capture, not vanish from the tree")
	}
	for _, want := range []string{"embedded git repository", "vendor/lib", "EMPTY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must say what and what to do about it (%q): %v", want, err)
		}
	}
}

// The check must be exact, not a hunt for .git directories: a nested repo under
// an ignored path was never staged, so it is not a problem and must not become
// one. This is the node_modules-with-a-clone-in-it case.
func TestCaptureAllowsAnIgnoredEmbeddedRepo(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	src := t.TempDir()
	writeFile(t, src, ".gitignore", "node_modules/\n")
	writeFile(t, src, "main.go", "package main\n")
	writeFile(t, src, "node_modules/dep/index.js", "module.exports = 1\n")
	nestedRepo(t, filepath.Join(src, "node_modules", "dep"))

	ref, err := tr.Capture(ctx, src)
	if err != nil {
		t.Fatalf("an ignored nested repo was never staged and must not fail the capture: %v", err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "main.go")); err != nil {
		t.Fatal("the real content must still be captured")
	}
	if _, err := os.Stat(filepath.Join(dst, "node_modules")); !os.IsNotExist(err) {
		t.Fatal("the ignored directory must stay out of the tree")
	}
}
