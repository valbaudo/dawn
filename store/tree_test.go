package store

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func trees(t *testing.T) *Trees {
	t.Helper()
	return NewTrees(NewMem())
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

// THE regression the store exists for: identical content gives an identical ref,
// whatever the clock says.
func TestCaptureIsContentAddressedNotTimeAddressed(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	capture := func(mt time.Time) string {
		d := t.TempDir()
		p := writeFile(t, d, "a.txt", "identical bytes\n")
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		ref, err := tr.Capture(ctx, d, "")
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	if older, newer := capture(time.Unix(1e9, 0)), capture(time.Unix(17e8, 0)); older != newer {
		t.Fatalf("identical bytes gave different refs: %s vs %s", older, newer)
	}
}

// THE property the whole git removal was for: NOTHING outside the directory
// changes the ref. There is no configuration to leak because there is no program
// that reads configuration — this test is a tripwire against reintroducing one.
func TestCaptureIgnoresTheAmbientEnvironment(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	build := func() string {
		d := t.TempDir()
		writeFile(t, d, "crlf.txt", "line one\r\nline two\r\n")
		writeFile(t, d, "sub/keep.log", "kept\n")
		ref, err := tr.Capture(ctx, d, "")
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	clean := build()

	home := t.TempDir()
	writeFile(t, home, ".gitconfig", "[core]\n\tautocrlf = true\n\tfileMode = false\n\tignorecase = true\n")
	writeFile(t, home, ".config/git/attributes", "*.txt eol=crlf text\n")
	writeFile(t, home, ".config/git/ignore", "*.log\n")
	for k, v := range map[string]string{
		"HOME": home, "XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"GIT_CONFIG_GLOBAL": filepath.Join(home, ".gitconfig"),
		"GIT_TEMPLATE_DIR":  home, "GIT_DEFAULT_HASH": "sha256",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.autocrlf", "GIT_CONFIG_VALUE_0": "true",
		"GIT_ATTR_SOURCE": home, "GIT_DIR": home, "GIT_WORK_TREE": home,
	} {
		t.Setenv(k, v)
	}
	if hostile := build(); hostile != clean {
		t.Fatalf("the environment moved a captured ref: %s != %s", hostile, clean)
	}
}

func TestCaptureMaterializeRoundTrip(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	src := t.TempDir()
	writeFile(t, src, "a.txt", "alpha\n")
	writeFile(t, src, "sub/b.txt", "beta\n")
	tool := writeFile(t, src, "tool", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(tool, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	ref, err := tr.Capture(ctx, src, "")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"a.txt": "alpha\n", "sub/b.txt": "beta\n"} {
		got, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
	fi, err := os.Lstat(filepath.Join(dst, "link.txt"))
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link.txt did not come back a symlink: %v %v", fi, err)
	}
	info, err := os.Stat(filepath.Join(dst, "tool"))
	if err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec bit lost: %v %v", info, err)
	}

	// MATERIALIZE-THEN-CAPTURE IS THE IDENTITY. If a round trip moved the ref,
	// the store would not be content-addressed at all — an agent would edit bytes
	// dawn never committed.
	again, err := tr.Capture(ctx, dst, "")
	if err != nil {
		t.Fatal(err)
	}
	if again != ref {
		t.Fatalf("round trip changed the ref: %s -> %s", ref, again)
	}
}

// Only the exec bit survives; other permission noise must not enter a content
// address, or the host's umask does.
func TestCaptureNormalizesModes(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	capture := func(mode os.FileMode) string {
		d := t.TempDir()
		if err := os.Chmod(writeFile(t, d, "tool", "x\n"), mode); err != nil {
			t.Fatal(err)
		}
		ref, err := tr.Capture(ctx, d, "")
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	if a, b := capture(0o755), capture(0o775); a != b {
		t.Fatalf("an unsupported mode distinction changed the ref: %s != %s", a, b)
	}
	if x, plain := capture(0o755), capture(0o644); x == plain {
		t.Fatal("the exec bit MUST change the ref")
	}
}

// THE PROPERTY: `expect: [p]` succeeds if and only if the committed tree holds p.
// Both directions, because it has broken both ways: a FIFO used to stat fine and
// stage nothing (a silent false pass), and a path behind a symlinked directory
// used to abort the run instead of reaching the repair loop.
func TestExpectIsSatisfiedExactlyWhenTheTreeHasThePath(t *testing.T) {
	for _, tc := range []struct {
		name, declared string
		build          func(t *testing.T, dir string)
		inTree         bool
	}{
		{"plain file", "dist/out.txt", func(t *testing.T, d string) { writeFile(t, d, "dist/out.txt", "x") }, true},
		{"non-empty directory", "dist", func(t *testing.T, d string) { writeFile(t, d, "dist/a", "x") }, true},
		{"absent", "dist/out.txt", func(t *testing.T, d string) {}, false},
		{"parent is a file", "dist/out.txt", func(t *testing.T, d string) { writeFile(t, d, "dist", "x") }, false},
		{"empty directory", "dist", func(t *testing.T, d string) {
			if err := os.MkdirAll(filepath.Join(d, "dist"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"behind a symlinked directory", "dist/app", func(t *testing.T, d string) {
			writeFile(t, d, "build/app", "binary")
			if err := os.Symlink("build", filepath.Join(d, "dist")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, false},
		// A declared path is a PATH. These matched a different file when the
		// declaration reached a tool that reads patterns.
		{"glob star", "dist/*", func(t *testing.T, d string) { writeFile(t, d, "dist/out.txt", "x") }, false},
		{"character class", "dist/[o]ut.txt", func(t *testing.T, d string) { writeFile(t, d, "dist/out.txt", "x") }, false},
		{"pathspec magic", ":(glob)dist/**", func(t *testing.T, d string) { writeFile(t, d, "dist/out.txt", "x") }, false},
		{"negation", ":!nope", func(t *testing.T, d string) { writeFile(t, d, "dist/out.txt", "x") }, false},
		{"a star really is a filename", "dist/*", func(t *testing.T, d string) { writeFile(t, d, "dist/*", "star") }, true},
		{"fifo", "out.pipe", func(t *testing.T, d string) {
			bin, err := exec.LookPath("mkfifo")
			if err != nil {
				t.Skipf("mkfifo unavailable: %v", err)
			}
			if out, err := exec.Command(bin, filepath.Join(d, "out.pipe")).CombinedOutput(); err != nil {
				t.Skipf("mkfifo failed: %v: %s", err, out)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr, ctx := trees(t), context.Background()
			d := t.TempDir()
			writeFile(t, d, "main.go", "package main\n")
			tc.build(t, d)

			ref, err := tr.Capture(ctx, d, "", tc.declared)
			if !tc.inTree {
				var missing *MissingPathError
				if !errors.As(err, &missing) {
					t.Fatalf("error = %T %v, want *MissingPathError so the gate can repair it", err, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("capture: %v", err)
			}
			dst := t.TempDir()
			if err := tr.Materialize(ctx, ref, dst); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(tc.declared))); err != nil {
				t.Fatalf("capture succeeded but the tree lacks %q: %v", tc.declared, err)
			}
		})
	}
}

// The ignore file is LITERAL. It replaced .gitignore, and the reason it is not a
// pattern language is the reason `expect:` is not one.
func TestIgnoreFileIsLiteralPathsOnly(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	d := t.TempDir()
	writeFile(t, d, IgnoreFile, "# junk\nnode_modules\nbuild/cache\n*\n\n")
	writeFile(t, d, "main.go", "package main\n")
	writeFile(t, d, "node_modules/dep/index.js", "junk\n")
	writeFile(t, d, "build/cache/blob", "junk\n")
	writeFile(t, d, "build/app", "kept\n")
	writeFile(t, d, "*", "a file literally named star\n")
	writeFile(t, d, "keep.txt", "kept\n")

	ref, err := tr.Capture(ctx, d, "")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := tr.Materialize(ctx, ref, dst); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"node_modules/dep/index.js", "build/cache/blob", "*"} {
		if _, err := os.Stat(filepath.Join(dst, gone)); err == nil {
			t.Errorf("%q should have been ignored", gone)
		}
	}
	// `*` is a filename, not a wildcard: it is ignored, and nothing else is.
	for _, kept := range []string{"main.go", "build/app", "keep.txt"} {
		if _, err := os.Stat(filepath.Join(dst, kept)); err != nil {
			t.Errorf("%q must survive — the ignore file is not a pattern language: %v", kept, err)
		}
	}
}

// An ignore rule must not eat an artifact an earlier step declared. Without this
// an artifact survives exactly one hop: `build` declares `dist/app`, `test`
// receives the tree, has no reason to re-declare another step's artifact, and
// drops it again.
func TestIgnoreDoesNotDropDeclaredOrInheritedPaths(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	a := t.TempDir()
	writeFile(t, a, IgnoreFile, "dist\n")
	writeFile(t, a, "main.go", "package main\n")
	writeFile(t, a, "dist/app", "binary\n")

	// Step A declares it, so it is kept despite the rule.
	stepA, err := tr.Capture(ctx, a, "", "dist/app")
	if err != nil {
		t.Fatal(err)
	}

	// Step B materializes A's tree, edits a source file, declares nothing.
	b := t.TempDir()
	if err := tr.Materialize(ctx, stepA, b); err != nil {
		t.Fatal(err)
	}
	writeFile(t, b, "main.go", "package main // fixed\n")
	stepB, err := tr.Capture(ctx, b, stepA)
	if err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	if err := tr.Materialize(ctx, stepB, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "dist", "app")); err != nil {
		t.Fatal("a declared artifact vanished on the second hop")
	}
	// But something NEW under the ignored path still does not enter the tree.
	writeFile(t, b, "dist/scratch", "junk\n")
	stepC, err := tr.Capture(ctx, b, stepB)
	if err != nil {
		t.Fatal(err)
	}
	out2 := t.TempDir()
	if err := tr.Materialize(ctx, stepC, out2); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out2, "dist", "scratch")); err == nil {
		t.Fatal("a NEW file under an ignored path must not be captured")
	}
}

func TestArchiveRoundTripNamesAndBytes(t *testing.T) {
	tr, ctx := trees(t), context.Background()
	src := t.TempDir()
	want := map[string]string{"bin/dawn": "binary bytes\x00\xff", "README.md": "# dawn\n"}
	for name, content := range want {
		writeFile(t, src, name, content)
	}
	ref, err := tr.Capture(ctx, src, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tr.Archive(ctx, ref, &buf); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	tr2 := tar.NewReader(&buf)
	for {
		h, err := tr2.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr2)
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = string(body)
	}
	for name, content := range want {
		if got[name] != content {
			t.Errorf("archive %s = %q, want %q", name, got[name], content)
		}
	}
}

// A manifest is bytes from the store. Trusting a path in it because dawn wrote
// it once is how an extraction bug becomes a host compromise.
func TestMaterializeRefusesAnUnsafeManifest(t *testing.T) {
	blobs := NewMem()
	tr, ctx := NewTrees(blobs), context.Background()
	content, err := blobs.Put([]byte("owned"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../escape", "/etc/passwd", "a/../../b"} {
		hostile, err := blobs.Put([]byte("file " + content + " " + strconvQuote(bad) + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := tr.Materialize(ctx, hostile, t.TempDir()); err == nil {
			t.Fatalf("materialized an escaping path %q", bad)
		}
	}
}

func strconvQuote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// dawn must never capture its own state directory: the tree would change every
// run because the journal grew, and every cache hit would be lost.
func TestCaptureSkipsTheStateDirectory(t *testing.T) {
	work := t.TempDir()
	state := filepath.Join(work, ".dawn")
	tr, ctx := NewTrees(NewMem(), state), context.Background()
	writeFile(t, work, "main.go", "package main\n")
	writeFile(t, work, ".dawn/journal.jsonl", "{}\n")

	first, err := tr.Capture(ctx, work, "")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, work, ".dawn/journal.jsonl", "{}\n{}\n{}\n")
	second, err := tr.Capture(ctx, work, "")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("state churn changed the workspace ref: %s -> %s", first, second)
	}
}
