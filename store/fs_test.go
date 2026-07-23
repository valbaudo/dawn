package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFSRoundTrip(t *testing.T) {
	b, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := b.Put([]byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}

func TestFSIdempotent(t *testing.T) {
	b, _ := NewFS(t.TempDir())
	r1, _ := b.Put([]byte("x"))
	r2, _ := b.Put([]byte("x"))
	if r1 != r2 {
		t.Fatalf("same content, different refs: %s vs %s", r1, r2)
	}
}

// The point of the fs store: a fresh instance over the same directory (a new
// process, a resume) reads back what an earlier one committed.
func TestFSDurableAcrossInstances(t *testing.T) {
	dir := t.TempDir()
	b1, _ := NewFS(dir)
	ref, err := b1.Put([]byte("survives a restart"))
	if err != nil {
		t.Fatal(err)
	}
	b2, err := NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b2.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "survives a restart" {
		t.Fatalf("got %q across instances", got)
	}
}

func TestFSMissingRef(t *testing.T) {
	b, _ := NewFS(t.TempDir())
	if _, err := b.Get(Ref([]byte("never stored"))); err == nil {
		t.Fatal("Get of a missing ref must error")
	}
}

func TestFSDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	b, _ := NewFS(dir)
	ref, _ := b.Put([]byte("real content"))
	// tamper with the blob on disk
	if err := os.WriteFile(filepath.Join(dir, ref[len("sha256:"):]), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ref); err == nil {
		t.Fatal("Get of a corrupted blob must error, not return the wrong bytes")
	}
}
