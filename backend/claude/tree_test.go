package claude

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestTarUntarRoundTrip(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a.txt", "alpha\n")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, src, filepath.Join("sub", "b.txt"), "beta\n")
	// a .git entry that must NOT travel with the tree snapshot
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, src, filepath.Join(".git", "HEAD"), "ref: refs/heads/main\n")

	data, err := tarTree(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := untarTree(data, dst); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dst, "a.txt"); got != "alpha\n" {
		t.Fatalf("a.txt = %q", got)
	}
	if got := read(t, dst, filepath.Join("sub", "b.txt")); got != "beta\n" {
		t.Fatalf("sub/b.txt = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatal(".git must be excluded from the tree snapshot")
	}
}

func TestUntarRejectsTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("owned")
	if err := tw.WriteHeader(&tar.Header{Name: "../evil.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := untarTree(buf.Bytes(), t.TempDir()); err == nil {
		t.Fatal("untarTree must reject a ../ path escape")
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
