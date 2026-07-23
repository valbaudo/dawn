// Command aw-fix is the kind-2 (workspace) demo: claude -p edits a real repo,
// aw captures the change as a git diff and a content-addressed ref, and a jury
// judges the diff. It creates a THROWAWAY temp git repo with a planted bug and
// deletes it on exit, so nothing you care about is touched.
//
//	go run ./cmd/aw-fix
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
	setupRepo(dir)

	blobs := store.NewMem()
	ws := claude.Workspace{Dir: dir, Model: env("AW_GEN", "sonnet"), Store: blobs}

	// [1] the agent edits the repo; aw captures the diff.
	fmt.Printf("[edit] claude (%s) fixes a planted bug in a throwaway repo\n", ws.Model)
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
	fmt.Printf("  captured diff -> %s (Produced state ref)\n", res.Produced["diff"].URI[:18])
	fmt.Println(indent(diff))

	// [2] the gate judges the diff.
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

func setupRepo(dir string) {
	git(dir, "init", "-q")
	git(dir, "config", "user.email", "aw@example.com")
	git(dir, "config", "user.name", "aw")
	if err := os.WriteFile(filepath.Join(dir, "calc.go"), []byte(buggy), 0o644); err != nil {
		fatal(err)
	}
	git(dir, "add", "-A")
	git(dir, "commit", "-qm", "initial")
}

func git(dir string, args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		fatal(fmt.Errorf("git %v: %v: %s", args, err, out))
	}
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
