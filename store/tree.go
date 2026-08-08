package store

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Trees stores directories as content-addressed trees, on top of [Blobs] and
// nothing else.
//
// This replaced a git-backed implementation, and the reason is the whole point
// of the file. git is an enormous configurable program that reads the machine it
// runs on, so "the same bytes give the same ref" had to be defended by listing
// what to neutralize: core.autocrlf, then core.excludesFile, then
// core.attributesFile, then GIT_TEMPLATE_DIR, then the object format, then the
// core.fileMode / ignorecase / precomposeunicode that `git init` bakes into the
// repository from whatever filesystem the state directory happens to sit on.
// Four audit rounds found four more entries for that list. The list has no end,
// because the set of things git reads is not dawn's to enumerate.
//
// A tree is therefore a MANIFEST: one line per path, sorted, naming a mode and
// the blob holding the content. The tree's ref is the manifest's own blob ref,
// so a tree is just a blob that happens to list other blobs — one store, no
// second repository, no refs to pin, and nothing for a `gc` to collect. Identity
// is a pure function of the bytes on disk and the rules in this file.
//
// Not git-compatible, deliberately. `dawn show REF` streams a tar; if you want a
// commit, pipe that into git yourself, outside the path that decides identity.
type Trees struct {
	blobs Blobs
	// exclude are absolute directories a capture must never descend into. Not
	// user configuration: dawn's state directory can sit INSIDE the tree being
	// captured (`--dir .dawn --in .`), and capturing it makes every run's tree
	// depend on the last run's journal — the ref changes when nothing did, and
	// every cache hit is lost. Held as an absolute path so it cannot be confused
	// with a relative rule from the ignore file.
	exclude []string
}

// NewTrees returns a tree store backed by blobs. exclude names absolute
// directories no capture may descend into; see [Trees.exclude].
func NewTrees(blobs Blobs, exclude ...string) *Trees {
	t := &Trees{blobs: blobs}
	for _, e := range exclude {
		if abs, err := filepath.Abs(e); err == nil {
			t.exclude = append(t.exclude, abs)
		}
	}
	return t
}

// excluded reports whether abs is one of the directories dawn owns.
func (t *Trees) excluded(abs string) bool {
	if len(t.exclude) == 0 {
		return false
	}
	resolved, err := filepath.Abs(abs)
	if err != nil {
		return false
	}
	return slices.Contains(t.exclude, resolved)
}

// EmptyTree is the ref of a tree with no entries — the baseline for "capture
// everything", and the `from` side of a first comparison.
var EmptyTree = Ref(nil)

// mode is the entry kind. Three values, because three is what survives a
// round trip through every filesystem dawn runs on: the exec bit is the only
// permission preserved, and everything else is noise that would put the host's
// umask into a content address.
type mode string

const (
	modeFile mode = "file"
	modeExec mode = "exec"
	modeLink mode = "link" // blob content is the link target
)

type entry struct {
	Path string
	Mode mode
	Ref  string
}

// IgnoreFile is read from the root of a captured directory. One path per line;
// blank lines and `#` comments are skipped.
//
// LITERAL paths and directory prefixes only — no globs, no patterns, no negation.
// `node_modules` ignores that directory; `dist/cache` ignores that subtree. A `*`
// is a file called `*`. This is the same rule as `expect:` for the same reason:
// the moment a declared string is interpreted as a pattern, it can match
// something other than itself, and `expect: [dist/*]` silently passing on a tree
// with no `dist/*` in it is exactly what that costs.
const IgnoreFile = ".dawnignore"

// Capture stores workDir as a tree and returns its ref.
//
// base is the tree the directory was materialized from, or "" for a fresh
// capture. It exists for one rule: IGNORES APPLY ONLY TO PATHS THAT ARE NEW.
// A path already in base stays, whatever the ignore file says. Without that,
// an artifact survives exactly one hop — `build` declares `dist/dawn` and forces
// it in, `test` receives the tree and has no reason to re-declare another step's
// artifact, and `test`'s capture drops it again. Declared `must` paths are kept
// for the same reason, and are asserted below.
func (t *Trees) Capture(ctx context.Context, workDir, base string, must ...string) (string, error) {
	baseline := map[string]entry{}
	if base != "" && base != EmptyTree {
		entries, err := t.manifest(base)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			baseline[e.Path] = e
		}
	}

	keep := make(map[string]bool, len(must))
	for _, m := range must {
		clean, err := NormalizeWorkspacePath(m)
		if err != nil {
			return "", fmt.Errorf("store: declared path %q: %w", m, err)
		}
		keep[clean] = true
	}

	rules, err := readIgnore(workDir)
	if err != nil {
		return "", err
	}

	var entries []entry
	err = filepath.WalkDir(workDir, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(workDir, abs)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		p := filepath.ToSlash(rel)
		ignored := rules.match(p)
		if d.IsDir() {
			if t.excluded(abs) {
				return filepath.SkipDir
			}
			// Prune, but only when nothing inside is protected. A subtree that holds
			// a declared path or a path from the baseline has to be walked.
			if ignored && !protects(keep, p) && !protects(baseline, p) {
				return filepath.SkipDir
			}
			return nil
		}
		if ignored && !keep[p] {
			if _, wasThere := baseline[p]; !wasThere {
				return nil
			}
		}
		e, ok, err := t.store(abs, d, p)
		if err != nil {
			return err
		}
		if ok {
			entries = append(entries, e)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("store: capture %s: %w", workDir, err)
	}

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.Path] = true
	}
	declared := maps.Keys(keep)
	for _, m := range slices.Sorted(declared) {
		if present[m] || protects(present, m) {
			continue
		}
		// A declared path that is not in the tree is a MISSING PATH, whatever the
		// reason: absent, a parent that turned out to be a file, a symlink cycle, a
		// FIFO the store cannot hold. They are one authoring mistake with different
		// causes, and the gate can repair all of them; splitting them by errno sent
		// the commonest down the mechanical path with no feedback.
		return "", &MissingPathError{Path: m}
	}
	return t.putManifest(entries)
}

// store hashes one filesystem entry. Anything that is not a regular file or a
// symlink — a FIFO, a socket, a device — is SKIPPED rather than captured: the
// store has no representation for it, and pretending otherwise is how a capture
// used to succeed with a declared path absent from the tree.
func (t *Trees) store(abs string, d fs.DirEntry, p string) (entry, bool, error) {
	info, err := d.Info()
	if err != nil {
		return entry{}, false, err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return entry{}, false, err
		}
		ref, err := t.blobs.Put([]byte(target))
		if err != nil {
			return entry{}, false, err
		}
		return entry{Path: p, Mode: modeLink, Ref: ref}, true, nil
	case info.Mode().IsRegular():
		content, err := os.ReadFile(abs)
		if err != nil {
			return entry{}, false, err
		}
		ref, err := t.blobs.Put(content)
		if err != nil {
			return entry{}, false, err
		}
		m := modeFile
		if info.Mode().Perm()&0o100 != 0 {
			m = modeExec
		}
		return entry{Path: p, Mode: m, Ref: ref}, true, nil
	default:
		return entry{}, false, nil
	}
}

// Materialize writes a tree into dir.
func (t *Trees) Materialize(ctx context.Context, tree, dir string) error {
	entries, err := t.manifest(tree)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Re-validated on the way OUT as well as in. A manifest is bytes from the
		// store, and a path that escapes `dir` would write wherever the uid can;
		// trusting it because dawn wrote it once is how an extraction bug becomes a
		// host compromise.
		clean, err := NormalizeWorkspacePath(e.Path)
		if err != nil || clean != e.Path {
			return fmt.Errorf("store: tree %s holds an unsafe path %q", tree, e.Path)
		}
		abs := filepath.Join(dir, filepath.FromSlash(e.Path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		content, err := t.blobs.Get(e.Ref)
		if err != nil {
			return err
		}
		if e.Mode == modeLink {
			if err := os.Symlink(string(content), abs); err != nil {
				return err
			}
			continue
		}
		perm := os.FileMode(0o644)
		if e.Mode == modeExec {
			perm = 0o755
		}
		if err := os.WriteFile(abs, content, perm); err != nil {
			return err
		}
	}
	return nil
}

// Archive streams a tree as a tar. This is how a tree leaves dawn.
func (t *Trees) Archive(ctx context.Context, tree string, w io.Writer) error {
	entries, err := t.manifest(tree)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		content, err := t.blobs.Get(e.Ref)
		if err != nil {
			return err
		}
		h := &tar.Header{Name: e.Path, Format: tar.FormatPAX}
		switch e.Mode {
		case modeLink:
			h.Typeflag, h.Linkname, h.Mode = tar.TypeSymlink, string(content), 0o777
		case modeExec:
			h.Typeflag, h.Size, h.Mode = tar.TypeReg, int64(len(content)), 0o755
		default:
			h.Typeflag, h.Size, h.Mode = tar.TypeReg, int64(len(content)), 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write(content); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// putManifest canonicalizes entries and stores them. Sorted by path and quoted,
// so one directory has exactly one serialization and the ref is the content.
func (t *Trees) putManifest(entries []entry) (string, error) {
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.Path, b.Path) })
	var b bytes.Buffer
	for i, e := range entries {
		if i > 0 && entries[i-1].Path == e.Path {
			return "", fmt.Errorf("store: duplicate path %q in tree", e.Path)
		}
		fmt.Fprintf(&b, "%s %s %s\n", e.Mode, e.Ref, strconv.Quote(e.Path))
	}
	return t.blobs.Put(b.Bytes())
}

// manifest reads a tree back.
func (t *Trees) manifest(tree string) ([]entry, error) {
	content, err := t.blobs.Get(tree)
	if err != nil {
		return nil, fmt.Errorf("store: read tree %s: %w", tree, err)
	}
	var entries []entry
	sc := bufio.NewScanner(bytes.NewReader(content))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		m, rest, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("store: tree %s: malformed entry %q", tree, line)
		}
		ref, quoted, ok := strings.Cut(rest, " ")
		if !ok {
			return nil, fmt.Errorf("store: tree %s: malformed entry %q", tree, line)
		}
		p, err := strconv.Unquote(quoted)
		if err != nil {
			return nil, fmt.Errorf("store: tree %s: malformed path in %q", tree, line)
		}
		switch mode(m) {
		case modeFile, modeExec, modeLink:
		default:
			return nil, fmt.Errorf("store: tree %s: unknown mode %q", tree, m)
		}
		entries = append(entries, entry{Path: p, Mode: mode(m), Ref: ref})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("store: tree %s: %w", tree, err)
	}
	return entries, nil
}

// ignore is the parsed IgnoreFile: literal paths, matched exactly or as a
// directory prefix.
type ignore struct{ paths map[string]bool }

func readIgnore(dir string) (ignore, error) {
	ig := ignore{paths: map[string]bool{}}
	content, err := os.ReadFile(filepath.Join(dir, IgnoreFile))
	if os.IsNotExist(err) {
		return ig, nil
	}
	if err != nil {
		return ig, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		clean, err := NormalizeWorkspacePath(strings.TrimSuffix(line, "/"))
		if err != nil {
			return ig, fmt.Errorf("store: %s: %w", IgnoreFile, err)
		}
		ig.paths[clean] = true
	}
	return ig, nil
}

func (i ignore) match(p string) bool {
	if len(i.paths) == 0 {
		return false
	}
	for cur := p; ; {
		if i.paths[cur] {
			return true
		}
		parent := path.Dir(cur)
		if parent == cur || parent == "." {
			return false
		}
		cur = parent
	}
}

// protects reports whether any key in set lies under p, so an ignored directory
// holding a declared or inherited path is walked rather than pruned.
func protects[V any](set map[string]V, p string) bool {
	prefix := p + "/"
	for k := range set {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// MissingPathError reports a declared path absent from the captured tree. It is
// distinguished so a gate can feed it back as a rejection the agent can repair,
// rather than aborting the run as a machine failure.
type MissingPathError struct{ Path string }

func (e *MissingPathError) Error() string {
	return fmt.Sprintf("declared path %q was not produced", e.Path)
}

// ValidateWorkspacePath accepts only normalized relative slash paths confined to
// a captured workspace.
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
// UNSAFE and UNTIDY are different. `./dist/out`, `dist//out` and `dist/out/` all
// name exactly `dist/out`; refusing them buys no safety and fails plans that ran
// yesterday. Refused instead are the paths that are not the same question:
// absolute, volume-qualified, backslashed, control-charactered, and anything that
// still escapes after cleaning — `dist/../..` is only visibly an escape once it
// collapses.
//
// Every guard runs on the CLEANED result as well as the input, because path.Clean
// can promote an interior component to the front: `x/../C:/y` cleans to `C:/y`,
// which is volume-qualified though the input was not.
func NormalizeWorkspacePath(p string) (string, error) {
	if err := unsafeWorkspacePath(p, p); err != nil {
		return "", err
	}
	clean := path.Clean(p)
	if err := unsafeWorkspacePath(clean, p); err != nil {
		return "", err
	}
	return clean, nil
}

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

// hasVolumePrefix reports a Windows-style volume qualifier, which is absolute on
// one platform and a directory named "C:" on another.
func hasVolumePrefix(p string) bool {
	return len(p) >= 2 && p[1] == ':' &&
		((p[0] >= 'a' && p[0] <= 'z') || (p[0] >= 'A' && p[0] <= 'Z'))
}
