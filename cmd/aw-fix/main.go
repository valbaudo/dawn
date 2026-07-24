// Command aw-fix is the workspace demo: claude -p edits a real directory, aw
// captures the change as a content-addressed tree ref plus a diff, and a jury
// judges the diff. It uses a throwaway temp dir with a planted bug, deleted on
// exit, so nothing you care about is touched.
//
//	go run ./cmd/aw-fix
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
	"github.com/valbaudo/aw/gate"
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
	dir, err := os.MkdirTemp("", "aw-fix-*")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte(buggy), 0o644); err != nil {
		fatal(err)
	}

	trees, err := store.NewTrees(filepath.Join(dir, ".aw-trees"))
	if err != nil {
		fatal(err)
	}
	ws := claude.Workspace{Dir: dir, Model: env("AW_GEN", "sonnet"), Trees: trees}

	// [1] the agent edits the dir; aw captures the tree and the diff.
	fmt.Printf("[edit] claude (%s) fixes a planted bug in a throwaway dir\n", ws.Model)
	fctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	res, err := ws.Invoke(fctx, aw.Invocation{
		Prompt: "calc.go has a bug: Add subtracts instead of adding. Fix it so Add returns a + b. Change only what is necessary.",
	})
	cancel()
	if err != nil {
		fatal(err)
	}
	diff, _ := res.Output["diff"].(string)
	if strings.TrimSpace(diff) == "" {
		fatal(fmt.Errorf("agent made no change"))
	}
	fmt.Printf("  %s -> %s (content-addressed tree refs)\n", short(res.Output["base"]), short(res.Output["tree"]))
	fmt.Println(indent(diff))

	// [2] the jury judges the diff.
	judges := backends(env("AW_JURY", "haiku,sonnet,opus"))
	quorum := gate.Majority(len(judges))
	fmt.Printf("[gate] jury of %d judges the diff, quorum k=%d\n", len(judges), quorum)
	jctx, jcancel := context.WithTimeout(ctx, 200*time.Second)
	approved, votes := gate.Jury(jctx, judges,
		"You are a strict code reviewer. Approve ONLY if this unified diff makes the Go function Add(a, b int) return a + b (the sum), with no unrelated changes.",
		diff, quorum)
	jcancel()
	for _, v := range votes {
		if v.Err != nil {
			fmt.Printf("  %-14s ERROR %s\n", v.Judge, oneLine(v.Err.Error()))
			continue
		}
		fmt.Printf("  %-14s approved=%-5v  %s\n", v.Judge, v.Approved, oneLine(v.Reason))
	}
	fmt.Printf("  => VERDICT: fix accepted=%v\n", approved)
}

func backends(csv string) []aw.Backend {
	var out []aw.Backend
	for _, m := range strings.Split(csv, ",") {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, claude.Backend{Model: m})
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

func fatal(err error) { fmt.Fprintln(os.Stderr, "aw-fix:", err); os.Exit(1) }

func short(v any) string {
	s, _ := v.(string)
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 100 {
		return s[:97] + "..."
	}
	return s
}
