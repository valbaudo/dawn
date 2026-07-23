package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// FS is a filesystem-backed [Blobs]: each blob is a file named by its content
// hash, so the store is self-verifying and durable across processes. Writes are
// atomic (temp file + rename), so a crash mid-Put never leaves a partial blob
// under its final name. This is the whole "resume after a crash" story: a new
// process opening the same directory reads back every committed ref.
type FS struct{ root string }

// NewFS opens (creating if needed) a content-addressed store rooted at dir.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	return &FS{root: dir}, nil
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
		return ref, nil // already stored
	}
	tmp, err := os.CreateTemp(b.root, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("store: temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("store: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("store: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("store: commit: %w", err)
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
