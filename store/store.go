// Package store is aw's commit layer: content-addressed bytes. Put returns a
// stable ref; Get round-trips it. Because a ref is the hash of its content,
// "resume" for the linear case is nothing more than re-reading committed refs —
// which is why aw has no separate resume engine.
//
// [Blobs] is the seam. [Mem] is the in-memory implementation used by tests and
// the demo; a filesystem or object-store implementation slots in behind the
// same two methods without touching any caller.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// Blobs is a content-addressed byte store. Implementations must be safe for
// concurrent use so a jury fan-out can commit in parallel.
type Blobs interface {
	// Put stores content and returns its ref ("sha256:<hex>"). Idempotent:
	// storing identical content twice yields the same ref.
	Put(content []byte) (ref string, err error)
	// Get returns the content for a ref, or an error if it is unknown.
	Get(ref string) ([]byte, error)
}

// Ref computes the content address for content without storing it. Exposed so
// callers can name a blob before (or without) committing it.
func Ref(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Mem is an in-memory Blobs, safe for concurrent Put/Get.
type Mem struct {
	mu sync.Mutex
	m  map[string][]byte
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem { return &Mem{m: map[string][]byte{}} }

// Put stores a defensive copy and returns the content-address ref.
func (b *Mem) Put(content []byte) (string, error) {
	ref := Ref(content)
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.m[ref]; !ok {
		cp := make([]byte, len(content))
		copy(cp, content)
		b.m[ref] = cp
	}
	return ref, nil
}

// Get returns a defensive copy of the stored content for ref.
func (b *Mem) Get(ref string) ([]byte, error) {
	if !strings.HasPrefix(ref, "sha256:") {
		return nil, fmt.Errorf("store: malformed ref %q", ref)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.m[ref]
	if !ok {
		return nil, fmt.Errorf("store: ref not found: %s", ref)
	}
	cp := make([]byte, len(c))
	copy(cp, c)
	return cp, nil
}
