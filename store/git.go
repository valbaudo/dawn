package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/valbaudo/dawn/proc"
)

// EmptyTree is git's well-known sha for the empty tree. Diff from it to see a
// captured tree as all-additions.
const EmptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Trees is a content-addressed store for directory TREES, backed by a bare git
// repository. A ref is a git tree sha, so identity is genuinely the content:
// the same bytes captured on a different day, by a different user, on a
// different machine produce the same ref. Timestamps, uid/gid and the capture
// order do not enter the hash.
//
// It replaces an earlier tar-based implementation that hashed a tarball rather
// than a tree, and so gave a different ref every time an mtime changed. Git also
// brings, for free, what that code got wrong or lacked: symlinks round-trip as
// real objects, the exec bit is normalized, identical blobs are stored once
// across versions, and any two captured refs can be diffed against each other
// rather than only against an immediately preceding baseline.
//
// git must be on PATH. The working directories it captures need no .git of their
// own; this store is the only repository involved.
type Trees struct{ gitDir string }

// NewTrees opens (creating if needed) a tree store at dir.
func NewTrees(dir string) (*Trees, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, "objects")); err != nil {
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", abs, err)
		}
		if out, err := git(context.Background(), "", nil, "init", "--bare", "-q", abs); err != nil {
			return nil, fmt.Errorf("store: init tree store: %w: %s", err, out)
		}
	}
	return &Trees{gitDir: abs}, nil
}

// Capture snapshots workDir and returns its tree ref. Files ignored by a
// .gitignore in the tree are excluded, so a capture and a diff agree on what
// counts as content. Capturing an unchanged tree is cheap and returns the same
// ref it did before.
//
// Each path in must is additionally forced into the tree and asserted to exist.
// Both properties matter and one call gives both: `git add -A` HONORS .gitignore,
// so a declared artifact under an ignored directory (dist/, build/, target/ — the
// normal case) would be silently absent from the captured tree; and `git add -f`
// fails loudly on a path that was never produced. Verified: with `dist/` ignored,
// plain `add -A` yields a tree where dist/dawn does not exist, while
// `add -f -- dist/aw` includes it and `add -f -- dist/nope` errors.
func (t *Trees) Capture(ctx context.Context, workDir string, must ...string) (string, error) {
	idx, done, err := t.tempIndex()
	if err != nil {
		return "", err
	}
	defer done()
	env := t.env(workDir, idx)
	if out, err := git(ctx, workDir, env, "add", "-A"); err != nil {
		return "", fmt.Errorf("store: capture %s: %w: %s", workDir, err, out)
	}
	if len(must) > 0 {
		args := append([]string{"add", "-f", "--"}, must...)
		if out, err := git(ctx, workDir, env, args...); err != nil {
			return "", fmt.Errorf("store: declared path missing from %s: %w: %s", workDir, err, strings.TrimSpace(out))
		}
	}
	out, err := git(ctx, workDir, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("store: write-tree %s: %w: %s", workDir, err, out)
	}
	return strings.TrimSpace(out), nil
}

// Materialize writes the tree ref into dir, creating it if needed. Existing
// files that the tree also contains are overwritten.
func (t *Trees) Materialize(ctx context.Context, tree, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	idx, done, err := t.tempIndex()
	if err != nil {
		return err
	}
	defer done()
	env := t.env(dir, idx)
	if out, err := git(ctx, dir, env, "read-tree", tree); err != nil {
		return fmt.Errorf("store: read-tree %s: %w: %s", tree, err, out)
	}
	if out, err := git(ctx, dir, env, "checkout-index", "-a", "-f"); err != nil {
		return fmt.Errorf("store: checkout %s: %w: %s", tree, err, out)
	}
	return nil
}

// Diff returns a unified diff between any two captured trees. Passing
// [EmptyTree] as from renders the whole of to as additions.
func (t *Trees) Diff(ctx context.Context, from, to string) (string, error) {
	out, err := git(ctx, "", t.env("", ""), "diff", from, to)
	if err != nil {
		return "", fmt.Errorf("store: diff %s..%s: %w: %s", from, to, err, out)
	}
	return out, nil
}

// tempIndex mints a scratch index file so a capture never inherits or mutates
// staging state from another call.
func (t *Trees) tempIndex() (path string, done func(), err error) {
	f, err := os.CreateTemp("", "aw-index-*")
	if err != nil {
		return "", nil, err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		return "", nil, err
	}
	// git wants to create the index itself; hand it a path, not an empty file.
	if err := os.Remove(name); err != nil {
		return "", nil, err
	}
	return name, func() { _ = os.Remove(name) }, nil
}

func (t *Trees) env(workTree, index string) []string {
	env := append(os.Environ(), "GIT_DIR="+t.gitDir)
	if workTree != "" {
		// ABSOLUTE. The command also runs with cmd.Dir = workTree, so a relative
		// GIT_WORK_TREE would be resolved a second time against it —
		// `--in examples/calc` looked for examples/calc/examples/calc.
		if abs, err := filepath.Abs(workTree); err == nil {
			workTree = abs
		}
		env = append(env, "GIT_WORK_TREE="+workTree)
	}
	if index != "" {
		env = append(env, "GIT_INDEX_FILE="+index)
	}
	return env
}

func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := proc.Command(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// Archive streams a captured tree as a tar to w. This is how a tree leaves aw:
// `dawn show plan.yaml fix.workspace | tar -x -C out/`. Piping rather than an
// --into flag keeps dawn out of the business of reimplementing tar's own
// --strip-components, --only and --list.
func (t *Trees) Archive(ctx context.Context, tree string, w io.Writer) error {
	cmd := proc.Command(ctx, "git", "archive", "--format=tar", tree)
	cmd.Env = t.env("", "")
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("store: archive %s: %w: %s", tree, err, strings.TrimSpace(errb.String()))
	}
	return nil
}
