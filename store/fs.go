package store

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FS is a filesystem-backed [Blobs]: each blob is a file named by its content
// hash, so the store is self-verifying and durable across processes. Writes are
// atomic (temp file + rename), so a crash mid-Put never leaves a partial blob
// under its final name. This is the whole "resume after a crash" story: a new
// process opening the same directory reads back every committed ref.
type FS struct {
	root string
	ops  fsOps
}

type tempFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

type fsOps struct {
	createTemp func(string, string) (tempFile, error)
	rename     func(string, string) error
	remove     func(string) error
}

func (ops fsOps) withDefaults() fsOps {
	if ops.createTemp == nil {
		ops.createTemp = func(dir, pattern string) (tempFile, error) {
			return os.CreateTemp(dir, pattern)
		}
	}
	if ops.rename == nil {
		ops.rename = os.Rename
	}
	if ops.remove == nil {
		ops.remove = os.Remove
	}
	return ops
}

func newFS(root string, ops fsOps) *FS {
	return &FS{root: root, ops: ops.withDefaults()}
}

// NewFS opens (creating if needed) a content-addressed store rooted at dir.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	return newFS(dir, fsOps{}), nil
}

// ponytail: flat directory keyed by full hash. Shard by hash prefix only if a
// single store ever holds enough blobs that one directory becomes a problem.
func (b *FS) path(ref string) string {
	return filepath.Join(b.root, strings.TrimPrefix(ref, "sha256:"))
}

// Put writes content atomically and returns its ref. Idempotent: identical
// content yields the same ref and is not rewritten. Safe for concurrent use —
// a distinct temp file per writer plus an atomic rename means racing Puts of the
// same content simply converge on the same final file.
func (b *FS) Put(content []byte) (string, error) {
	ref := Ref(content)
	path := b.path(ref)
	if _, err := os.Stat(path); err == nil {
		if _, err := b.Get(ref); err != nil {
			return "", err
		}
		return ref, nil // already stored and verified
	}
	ops := b.ops.withDefaults()
	tmp, err := ops.createTemp(b.root, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("store: temp: %w", err)
	}
	tmpName := tmp.Name()
	n, err := tmp.Write(content)
	if err == nil && n != len(content) {
		err = io.ErrShortWrite
	}
	if err != nil {
		_ = tmp.Close()
		_ = ops.remove(tmpName)
		return "", fmt.Errorf("store: write: %w", err)
	}
	// Flush to disk BEFORE the rename. Without this the rename can land while
	// the content is still in the page cache, so a power loss leaves a
	// correctly-named file full of zeros — a committed ref pointing at nothing.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = ops.remove(tmpName)
		return "", fmt.Errorf("store: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = ops.remove(tmpName)
		return "", fmt.Errorf("store: close: %w", err)
	}
	if err := ops.rename(tmpName, path); err != nil {
		_ = ops.remove(tmpName)
		return "", fmt.Errorf("store: commit: %w", err)
	}
	// Best-effort directory sync improves rename persistence where the platform
	// supports it. File Sync plus atomic rename are the enforced durability boundary.
	if dir, err := os.Open(b.root); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return ref, nil
}

// Get reads the content for ref and verifies it still hashes to ref, so silent
// on-disk corruption surfaces as an error rather than a wrong answer.
func (b *FS) Get(ref string) ([]byte, error) {
	if !strings.HasPrefix(ref, "sha256:") {
		return nil, fmt.Errorf("store: malformed ref %q", ref)
	}
	content, err := os.ReadFile(b.path(ref))
	if err != nil {
		return nil, fmt.Errorf("store: get %s: %w", ref, err)
	}
	if got := Ref(content); got != ref {
		return nil, fmt.Errorf("store: corruption: %s hashes to %s", ref, got)
	}
	return content, nil
}

var _ Blobs = (*FS)(nil)
