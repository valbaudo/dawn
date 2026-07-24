// Command aw-chain demonstrates workspace state flowing forward between agents:
// one agent fixes a bug, aw captures the resulting tree as a content-addressed
// ref, and a second agent materializes THAT tree and builds on it. repo@v3
// accumulates both changes, proving state moved between invocations with no
// shared mutable directory. Uses throwaway temp dirs, deleted on exit.
//
//	go run ./cmd/aw-chain
package main

import (
	"context"
	"fmt"
	"os"
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
	root, err := os.MkdirTemp("", "aw-chain-*")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(root)

	trees, err := store.NewTrees(filepath.Join(root, "trees"))
	if err != nil {
		fatal(err)
	}
	model := env("AW_GEN", "sonnet")

	// repo@v1: a throwaway dir with a planted bug.
	v1dir := filepath.Join(root, "v1")
	if err := os.MkdirAll(v1dir, 0o755); err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Join(v1dir, "calc.go"), []byte(buggy), 0o644); err != nil {
		fatal(err)
	}

	// [ws1] fix the bug on repo@v1 (explicit Dir).
	fmt.Println("[ws1] fix the bug on repo@v1")
	ctx1, cancel1 := context.WithTimeout(ctx, 5*time.Minute)
	r1, err := claude.Workspace{Dir: v1dir, Model: model, Trees: trees}.Invoke(ctx1,
		aw.Invocation{Prompt: "calc.go has a bug: Add subtracts instead of adding. Fix it so Add returns a + b. Change only calc.go."})
	cancel1()
	if err != nil {
		fatal(err)
	}
	v2 := r1.Produced["workspace"]
	fmt.Printf("%s\n  -> repo@v2 = %s\n\n", indent(field(r1, "diff")), short(v2.URI))

	// [ws2] materialize repo@v2 (Dir empty) and build on the FIXED code.
	fmt.Println("[ws2] add a test on repo@v2 (materialized from the ref, not a shared dir)")
	ctx2, cancel2 := context.WithTimeout(ctx, 5*time.Minute)
	r2, err := claude.Workspace{Model: model, Trees: trees}.Invoke(ctx2,
		aw.Invocation{
			Prompt: "Add a file calc_test.go with a Go test verifying Add(2, 3) == 5. Read calc.go first; Add is already implemented.",
			Inputs: map[string]aw.Ref{"repo": v2},
		})
	cancel2()
	if err != nil {
		fatal(err)
	}
	v3 := r2.Produced["workspace"]
	fmt.Printf("%s\n  -> repo@v3 = %s\n\n", indent(field(r2, "diff")), short(v3.URI))

	// [proof] materialize repo@v3 and show it carries BOTH changes.
	final := filepath.Join(root, "v3")
	if err := trees.Materialize(ctx, v3.URI, final); err != nil {
		fatal(err)
	}
	fmt.Println("[proof] repo@v3 accumulates ws1's fix AND ws2's test:")
	fmt.Printf("  calc.go contains the fix: %v\n", strings.Contains(readFile(final, "calc.go"), "a + b"))
	fmt.Printf("  files: %v\n", goFiles(final))

	// Any two captured refs diff directly, not just consecutive ones.
	span, err := trees.Diff(ctx, r1.Output["base"].(string), v3.URI)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("  v1..v3 cumulative diff: %d lines\n", strings.Count(span, "\n"))
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

func short(r string) string {
	if len(r) > 12 {
		return r[:12]
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
