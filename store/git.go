package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"

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

// MissingPathError reports a declared capture postcondition that the backend did
// not produce. Callers may treat this one failure as repairable while preserving
// every other capture failure as mechanical.
type MissingPathError struct{ Path string }

func (e *MissingPathError) Error() string {
	return fmt.Sprintf("declared path %q was not produced", e.Path)
}

// ValidateWorkspacePath accepts only normalized relative slash paths confined to
// a captured workspace. It is exported so plan validation and capture defense use
// one definition rather than drifting.
func ValidateWorkspacePath(p string) error {
	clean, err := NormalizeWorkspacePath(p)
	if err != nil {
		return err
	}
	if clean != p {
		return fmt.Errorf("path %q is not normalized", p)
	}
	return nil
}

// NormalizeWorkspacePath refuses a path that could escape or misname a captured
// workspace, and returns the cleaned form of one that cannot.
//
// The split from [ValidateWorkspacePath] is the difference between UNSAFE and
// merely UNTIDY. `./dist/out`, `dist//out` and `dist/out/` all name exactly
// `dist/out`; refusing them buys no safety and fails plans that ran yesterday,
// with no migration path and nothing for the author to do but retype a path that
// was already unambiguous. So an author's `expect:` is normalized here, and
// capture keeps the stricter check because by then the path has been through
// this and a non-canonical one means dawn itself lost the invariant.
//
// The escape test runs AFTER cleaning, which is what makes it real: `dist/../..`
// is only visibly an escape once it collapses to `..`.
func NormalizeWorkspacePath(p string) (string, error) {
	if err := unsafeWorkspacePath(p, p); err != nil {
		return "", err
	}
	clean := path.Clean(p)
	// EVERY guard again, on the cleaned result. path.Clean does not only delete
	// components, it can promote one to the front: `x/../C:/y` cleans to `C:/y`,
	// which is volume-qualified though the input was not. Checking the input alone
	// let normalization hand back a path its own validator would refuse — the
	// guards have to hold for the string that is actually used, not merely the one
	// that was typed.
	if err := unsafeWorkspacePath(clean, p); err != nil {
		return "", err
	}
	return clean, nil
}

// unsafeWorkspacePath reports why check cannot name a location inside a
// workspace. reported is the spelling to blame in the message, so a complaint
// about a cleaned path still quotes what the author wrote.
func unsafeWorkspacePath(check, reported string) error {
	switch {
	case check == "":
		return fmt.Errorf("path is empty")
	case strings.ContainsRune(check, '\\'):
		return fmt.Errorf("path %q must use forward slashes", reported)
	case path.IsAbs(check):
		return fmt.Errorf("path %q must be relative", reported)
	case hasVolumePrefix(check):
		return fmt.Errorf("path %q must not be volume-qualified", reported)
	case strings.IndexFunc(check, unicode.IsControl) >= 0:
		return fmt.Errorf("path %q contains control characters", reported)
	case check == "." || check == ".." || strings.HasPrefix(check, "../"):
		return fmt.Errorf("path %q must stay within the workspace", reported)
	}
	return nil
}

func hasVolumePrefix(p string) bool {
	return len(p) >= 2 && ((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z')) && p[1] == ':'
}

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
		// --template= (EMPTY) because a template is a third channel into the store,
		// independent of config and attributes files. `git init` copies the
		// template's info/exclude and info/attributes into the new repository, and
		// $GIT_DIR/info/exclude is honored by every later `git add -A` — so an
		// ambient GIT_TEMPLATE_DIR silently changed which files a capture contained,
		// permanently, from a variable set once before the store was created.
		// Passing it empty is the only way to opt out; unsetting the variable is not
		// enough, since git falls back to a system template directory.
		if out, err := git(context.Background(), "", controlledGitEnv(), "init", "--bare", "-q", "--template=", abs); err != nil {
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
// `add -f -- dist/dawn` includes it and `add -f -- dist/nope` errors.
func (t *Trees) Capture(ctx context.Context, workDir string, must ...string) (string, error) {
	return t.CaptureFrom(ctx, workDir, "", must...)
}

// CaptureFrom is Capture against a baseline tree: everything in base is already
// tracked, so .gitignore is applied only to files that APPEARED since.
//
// This is what makes a workspace survive a chain of steps. `git add -A` honors
// .gitignore for UNTRACKED files, and an index minted per call has nothing
// tracked in it — so a declared artifact that step A forced past .gitignore
// (`dist/app` under an ignored `dist/`) was materialized into step B's workspace,
// then silently dropped from B's capture, because B had no reason to re-declare
// A's artifact. Measured before this existed: capture → materialize → capture
// returned a DIFFERENT ref (6fd375c… → e1565f2…) with the artifact gone.
//
// Seeding the index from base fixes both halves at once: the round trip is
// identity again, and the filter keeps doing the job it exists for — a
// node_modules the agent created is still untracked, still ignored, still out.
//
// Determinism is unaffected: this is a pure function of (workDir bytes, base,
// must), and a step whose input tree differs already has a different identity key.
func (t *Trees) CaptureFrom(ctx context.Context, workDir, base string, must ...string) (string, error) {
	for _, declared := range must {
		if err := ValidateWorkspacePath(declared); err != nil {
			return "", fmt.Errorf("store: invalid declared path: %w", err)
		}
	}
	idx, done, err := t.tempIndex()
	if err != nil {
		return "", err
	}
	defer done()
	env := t.env(workDir, idx)
	if base != "" {
		if out, err := git(ctx, workDir, env, "read-tree", base); err != nil {
			return "", fmt.Errorf("store: seed index from %s: %w: %s", base, err, out)
		}
	}
	if out, err := git(ctx, workDir, env, "add", "-A"); err != nil {
		return "", fmt.Errorf("store: capture %s: %w: %s", workDir, err, out)
	}
	// `expect:` is a claim about the TREE, so it is asked of the index — the thing
	// that becomes the tree — and never of the filesystem.
	//
	// os.Lstat answers a different question and gets it wrong in both directions.
	// It says present for entries git will not store: a FIFO or a socket stats
	// fine, stages nothing, and the capture used to succeed with the declared path
	// silently absent from the tree — a false pass, which is worse than an abort.
	// And it says nothing about the paths that fail inside `git add`: one behind a
	// symlinked directory (`dist -> build`) resolves happily for Lstat and is
	// refused by git as "beyond a symbolic link", as is one inside an embedded
	// repo, or one the agent left unreadable. Those all used to escape as
	// mechanical errors, aborting the gate on attempt 1 with no feedback.
	//
	// Per path rather than one pathspec list, so the error names the path that is
	// actually missing. That is one extra git call per declared path against a
	// 30m invocation bound.
	for _, path := range must {
		if _, err := git(ctx, workDir, env, "add", "-f", "--", path); err != nil {
			return "", &MissingPathError{Path: path}
		}
		// add can succeed and stage nothing — an empty directory is the honest
		// case, a FIFO the dishonest one. Only the index settles it.
		staged, err := git(ctx, workDir, env, "ls-files", "-s", "--", path)
		if err != nil {
			return "", fmt.Errorf("store: inspect declared path %q in %s: %w", path, workDir, err)
		}
		if strings.TrimSpace(staged) == "" {
			return "", &MissingPathError{Path: path}
		}
	}
	if err := t.refuseGitlinks(ctx, workDir, env); err != nil {
		return "", err
	}
	out, err := git(ctx, workDir, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("store: write-tree %s: %w: %s", workDir, err, out)
	}
	tree := strings.TrimSpace(out)
	if err := t.pin(ctx, tree); err != nil {
		return "", err
	}
	return tree, nil
}

// refuseGitlinks fails a capture that staged an embedded git repository.
//
// A directory containing its own .git — a vendored dependency, a clone the agent
// made, a leftover checkout — is recorded by `git add -A` as mode 160000, a
// COMMIT reference rather than files. That commit lives in the nested repo's
// object store, not in dawn's, so the reference dangles the moment the scratch
// dir is deleted. Reproduced: a tree holding `160000 commit 5f9bf40b… vendor/lib`
// materialized as `[main.go]` — vendor/lib was not empty, it was absent, and
// every file under it was gone with no error anywhere.
//
// Refused rather than repaired, because git offers no way to descend into an
// embedded repo: `add -A` skips it, and naming a path inside it fails with
// "Pathspec is in submodule". Capturing the nested repo's HEAD tree instead would
// silently substitute its last commit for its working state. A tree that cannot
// round-trip is not a workspace, so this is a hard error at capture — before a
// judge is paid, and long before someone opens an empty directory.
//
// The check reads the index rather than walking for .git directories, which
// costs a listing proportional to the file count but is exact: a nested repo
// under an ignored path was never staged, and so is correctly not a problem.
func (t *Trees) refuseGitlinks(ctx context.Context, workDir string, env []string) error {
	out, err := git(ctx, workDir, env, "ls-files", "-s")
	if err != nil {
		return fmt.Errorf("store: read index for %s: %w: %s", workDir, err, out)
	}
	var found []string
	for _, line := range strings.Split(out, "\n") {
		// <mode> <sha> <stage>\t<path>, and 160000 is the gitlink mode.
		rest, ok := strings.CutPrefix(line, "160000 ")
		if !ok {
			continue
		}
		if _, path, ok := strings.Cut(rest, "\t"); ok {
			found = append(found, path)
		}
	}
	if len(found) == 0 {
		return nil
	}
	shown := found
	if len(shown) > 3 {
		shown = shown[:3]
	}
	more := ""
	if len(found) > len(shown) {
		more = fmt.Sprintf(" (and %d more)", len(found)-len(shown))
	}
	return fmt.Errorf("store: %s contains an embedded git repository: %s%s. "+
		"git records it as a commit reference, not files, and that commit is not in dawn's store, "+
		"so the directory would come back EMPTY. Remove the nested .git, or ignore the path",
		workDir, strings.Join(shown, ", "), more)
}

// pin makes a captured tree reachable, so git's own garbage collector cannot
// delete it.
//
// `write-tree` writes objects that NOTHING points at. A bare store with no refs
// is a store where every committed workspace is unreachable garbage by git's
// definition, and `git gc --prune=now` in .dawn/trees deletes the lot —
// reproduced: `fatal: failed to unpack tree object`. Nothing in dawn runs gc, but
// "the durable artifact survives only until someone runs a standard maintenance
// command inside the state directory" is not durable.
//
// A ref may point straight at a tree; no wrapper commit is needed, which is
// fortunate because a commit would carry a timestamp and a tree's identity must
// stay its content. Verified: `for-each-ref` reports `<sha> tree
// refs/dawn/t/<sha>`, and the tree survives `gc --prune=now`.
//
// Pinning is per capture, including gate attempts nobody accepted, so gc now
// reclaims nothing. That is the deliberate trade: unbounded growth is a disk
// problem with an obvious fix, and silent deletion of committed state is not. It
// is also what makes a real prune possible later — a ref not named by the journal
// is collectable, which is not a question you can even ask of a dangling object.
func (t *Trees) pin(ctx context.Context, tree string) error {
	if out, err := git(ctx, "", t.env("", ""), "update-ref", "refs/dawn/t/"+tree, tree); err != nil {
		return fmt.Errorf("store: pin %s: %w: %s", tree, err, strings.TrimSpace(out))
	}
	return nil
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
	f, err := os.CreateTemp("", "dawn-index-*")
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

func isRepositoryGitEnv(upper string) bool {
	switch upper {
	case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE",
		"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
		"GIT_PREFIX", "GIT_SUPER_PREFIX", "GIT_INTERNAL_SUPER_PREFIX",
		"GIT_GRAFT_FILE", "GIT_REPLACE_REF_BASE", "GIT_CONFIG",
		"GIT_IMPLICIT_WORK_TREE", "GIT_SHALLOW_FILE", "GIT_NO_REPLACE_OBJECTS":
		return true
	default:
		return false
	}
}

func controlledGitEnv() []string {
	ambient := os.Environ()
	env := make([]string, 0, len(ambient)+8)
	for _, entry := range ambient {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if strings.HasPrefix(upper, "GIT_CONFIG_") || upper == "GIT_ATTR_NOSYSTEM" || isRepositoryGitEnv(upper) {
			continue
		}
		env = append(env, entry)
	}
	return append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=core.autocrlf",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.excludesFile",
		"GIT_CONFIG_VALUE_1="+os.DevNull,
		// core.attributesFile, the excludesFile sibling that is easy to forget.
		// GIT_ATTR_NOSYSTEM covers only the SYSTEM attributes file; git resolves the
		// personal one from $XDG_CONFIG_HOME/git/attributes (else ~/.config/git/attributes)
		// on a path independent of the config files, so GIT_CONFIG_GLOBAL=/dev/null
		// does not suppress it — neutering global config makes git fall back to that
		// default path rather than to nothing.
		//
		// What it actually breaks, measured rather than assumed: `*.txt eol=crlf text`
		// in a personal attributes file leaves the captured REF unchanged and changes
		// what MATERIALIZE writes back out — CRLF where the capture held LF. So this
		// is not a key-stability bug, it is worse in a quieter way: the same ref hands
		// two machines different bytes, and SPEC §4's "materialize-then-capture is the
		// identity" stops holding. An agent then edits a file dawn did not commit.
		"GIT_CONFIG_KEY_2=core.attributesFile",
		"GIT_CONFIG_VALUE_2="+os.DevNull,
	)
}

func (t *Trees) env(workTree, index string) []string {
	env := append(controlledGitEnv(), "GIT_DIR="+t.gitDir)
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

// Archive streams a captured tree as a tar to w. The CLI pipes this output to
// standard output rather than adding an --into flag, keeping dawn out of the
// business of reimplementing tar's own --strip-components, --only and --list.
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
