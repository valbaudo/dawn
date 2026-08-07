package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemRejectsMalformedAndMissingRefs(t *testing.T) {
	b := NewMem()
	if _, err := b.Get("not-a-ref"); err == nil || !strings.Contains(err.Error(), "malformed ref") {
		t.Fatalf("Get malformed ref error = %v, want malformed ref", err)
	}
	if _, err := b.Get(Ref([]byte("never stored"))); err == nil || !strings.Contains(err.Error(), "ref not found") {
		t.Fatalf("Get missing ref error = %v, want ref not found", err)
	}
}

func TestMemDefensiveCopies(t *testing.T) {
	b := NewMem()
	input := []byte("hello")
	ref, err := b.Put(input)
	if err != nil {
		t.Fatal(err)
	}
	input[0] = 'j'

	got, err := b.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("stored bytes changed with input: got %q", got)
	}
	got[0] = 'j'
	gotAgain, err := b.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotAgain) != "hello" {
		t.Fatalf("stored bytes changed with Get result: got %q", gotAgain)
	}
}

func TestMemZeroValue(t *testing.T) {
	var b Mem
	ref, err := b.Put([]byte("zero value"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "zero value" {
		t.Fatalf("got %q, want zero value", got)
	}
}

func TestFSZeroOpsUseDefaults(t *testing.T) {
	b := &FS{root: t.TempDir()}
	ref, err := b.Put([]byte("default operations"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "default operations" {
		t.Fatalf("got %q, want default operations", got)
	}
}

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

type failingTempFile struct {
	*os.File
	fail string
}

func (f *failingTempFile) Write(p []byte) (int, error) {
	if f.fail == "write" {
		return 0, errors.New("injected write failure")
	}
	return f.File.Write(p)
}

func (f *failingTempFile) Sync() error {
	if f.fail == "sync" {
		return errors.New("injected sync failure")
	}
	return f.File.Sync()
}

func (f *failingTempFile) Close() error {
	err := f.File.Close()
	if f.fail == "close" {
		return errors.New("injected close failure")
	}
	return err
}

func TestFSPutFailureCleansUp(t *testing.T) {
	for _, tc := range []struct {
		name string
		want string
		ops  func(*testing.T) fsOps
	}{
		{
			name: "write",
			want: "store: write",
			ops: func(t *testing.T) fsOps {
				return fsOps{createTemp: failingCreateTemp(t, "write")}
			},
		},
		{
			name: "sync",
			want: "store: sync",
			ops: func(t *testing.T) fsOps {
				return fsOps{createTemp: failingCreateTemp(t, "sync")}
			},
		},
		{
			name: "close",
			want: "store: close",
			ops: func(t *testing.T) fsOps {
				return fsOps{createTemp: failingCreateTemp(t, "close")}
			},
		},
		{
			name: "rename",
			want: "store: commit",
			ops: func(*testing.T) fsOps {
				return fsOps{rename: func(string, string) error {
					return errors.New("injected rename failure")
				}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			b := newFS(dir, tc.ops(t))
			content := []byte("never committed")
			if _, err := b.Put(content); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Put error = %v, want %q", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(Ref(content), "sha256:"))); !os.IsNotExist(err) {
				t.Fatalf("final blob exists after failure: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("temporary files remain after failure: %v", entries)
			}
		})
	}
}

func failingCreateTemp(t *testing.T, fail string) func(string, string) (tempFile, error) {
	t.Helper()
	return func(dir, pattern string) (tempFile, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return &failingTempFile{File: f, fail: fail}, nil
	}
}
