// Command aw-chain demonstrates workspace state flowing forward between agents:
// one agent fixes a bug in a repo, aw captures the resulting tree as a workspace
// ref, and a second agent materializes THAT tree and builds on it. The final
// repo@v3 accumulates both changes — proof that state moved between invocations
// with no shared mutable directory. Uses throwaway temp repos, deleted on exit.
//
//	go run ./cmd/aw-chain
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/backend/claude"
	"github.com/valbaudo/aw/store"
)

const buggy = `package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b // BUG: subtracts instead of adding
}
`

func main() {
	ctx := context.Background()
	blobs := store.NewMem()
	model := env("AW_GEN", "sonnet")

	// repo@v1: a throwaway repo with a planted bug.
	v1 := mkRepo()
	defer os.RemoveAll(v1)

	// [ws1] fix the bug on repo@v1 (explicit Dir).
	fmt.Println("[ws1] fix the bug on repo@v1")
	ctx1, cancel1 := context.WithTimeout(ctx, 5*time.Minute)
	r1, err := claude.Workspace{Dir: v1, Model: model, Store: blobs}.Invoke(ctx1,
		aw.Invocation{Prompt: "calc.go has a bug: Add subtracts instead of adding. Fix it so Add returns a + b. Change only calc.go."})
	cancel1()
	if err != nil {
		fatal(err)
	}
	v2 := r1.Produced["workspace"]
	fmt.Printf("%s\n  -> repo@v2 = %s\n\n", indent(field(r1, "diff")), shortRef(v2.URI))

	// [ws2] materialize repo@v2 (Dir empty) and build on the FIXED code.
	fmt.Println("[ws2] add a test on repo@v2 (materialized from the ref, not a shared dir)")
	ctx2, cancel2 := context.WithTimeout(ctx, 5*time.Minute)
	r2, err := claude.Workspace{Model: model, Store: blobs}.Invoke(ctx2,
		aw.Invocation{
			Prompt: "Add a file calc_test.go with a Go test verifying Add(2, 3) == 5. Read calc.go first; Add is already implemented.",
			Inputs: map[string]aw.Ref{"repo": v2},
		})
	cancel2()
	if err != nil {
		fatal(err)
	}
	v3 := r2.Produced["workspace"]
	fmt.Printf("%s\n  -> repo@v3 = %s\n\n", indent(field(r2, "diff")), shortRef(v3.URI))

	// [proof] materialize repo@v3 and show it carries BOTH changes.
	final, err := os.MkdirTemp("", "aw-v3-*")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(final)
	if err := claude.Materialize(blobs, v3, final); err != nil {
		fatal(err)
	}
	fmt.Println("[proof] repo@v3 accumulates ws1's fix AND ws2's test:")
	fmt.Printf("  calc.go contains the fix: %v\n", strings.Contains(readFile(final, "calc.go"), "a + b"))
	fmt.Printf("  files: %v\n", goFiles(final))
}

func mkRepo() string {
	dir, err := os.MkdirTemp("", "aw-v1-*")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte(buggy), 0o644); err != nil {
		fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "aw@example.com"},
		{"config", "user.name", "aw"}, {"add", "-A"}, {"commit", "-qm", "initial"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			fatal(fmt.Errorf("git %v: %v: %s", args, err, out))
		}
	}
	return dir
}

func field(r aw.Result, k string) string { s, _ := r.Output[k].(string); return s }

func readFile(dir, name string) string {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	return string(b)
}

func goFiles(dir string) []string {
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".go") {
			out = append(out, e.Name())
		}
	}
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "aw-chain:", err); os.Exit(1) }

func shortRef(r string) string {
	if len(r) > 18 {
		return r[:18]
	}
	return r
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "  (no change)"
	}
	return "  " + strings.ReplaceAll(s, "\n", "\n  ")
}
