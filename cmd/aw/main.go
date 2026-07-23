// Command aw is the demo and litmus for the aw runtime: one Claude model writes
// a candidate, a jury of three independent models votes on it under a k-of-N
// quorum, and the decision is committed to a content-addressed store. Pass a
// candidate as the first argument to skip generation and watch the jury judge
// something you chose (e.g. a deliberately bad note).
//
//	go run ./cmd/aw
//	go run ./cmd/aw "The aw run --json flag is here. It streams events. You can pipe it. Enjoy."
//	AW_JURY=haiku,sonnet,opus AW_GEN=sonnet go run ./cmd/aw
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/backend/claude"
	"github.com/valbaudo/aw/gate"
	"github.com/valbaudo/aw/store"
)

const reviewer = "You are a strict, independent reviewer. Approve the release note ONLY if it is " +
	"EXACTLY three sentences AND explicitly names the `aw run --json` feature. Be harsh."

func main() {
	ctx := context.Background()
	blobs := store.NewMem()

	judges := backends(env("AW_JURY", "haiku,sonnet,opus"))
	genModel := env("AW_GEN", "sonnet")

	// [1] candidate: injected (first arg) or generated.
	var candidate string
	if len(os.Args) > 1 && strings.TrimSpace(os.Args[1]) != "" {
		candidate = os.Args[1]
		fmt.Println("[1] injected candidate")
	} else {
		fmt.Printf("[1] generate candidate (%s)\n", genModel)
		gctx, cancel := context.WithTimeout(ctx, 150*time.Second)
		res, err := claude.Backend{Model: genModel}.Invoke(gctx, aw.Invocation{
			System: "You write concise release notes.",
			Prompt: "Write a three-sentence release note for a new `aw run --json` flag that streams machine-readable events.",
			Schema: object("release_note"),
		})
		cancel()
		if err != nil {
			fatal(err)
		}
		candidate = str(res.Output, "release_note", "text")
	}
	cref, err := blobs.Put([]byte(candidate))
	if err != nil {
		fatal(err)
	}
	fmt.Printf("  committed %s:\n  %q\n\n", short(cref), candidate)

	// [2] jury: independent k-of-N vote.
	quorum := gate.Majority(len(judges))
	fmt.Printf("[2] jury of %d different models, quorum k=%d\n", len(judges), quorum)
	jctx, cancel := context.WithTimeout(ctx, 200*time.Second)
	approved, votes := gate.Jury(jctx, judges, reviewer, candidate, quorum)
	cancel()
	for _, v := range votes {
		if v.Err != nil {
			fmt.Printf("  %-14s ERROR %s\n", v.Judge, oneLine(v.Err.Error()))
			continue
		}
		fmt.Printf("  %-14s approved=%-5v  %s\n", v.Judge, v.Approved, oneLine(v.Reason))
	}
	fmt.Printf("  => VERDICT: approved=%v\n\n", approved)

	// [3] commit the decision; read it back (all "resume" does).
	fmt.Println("[3] commit decision to content-addressed store")
	decision, err := json.Marshal(map[string]any{"candidate": cref, "approved": approved, "votes": votes})
	if err != nil {
		fatal(err)
	}
	dref, err := blobs.Put(decision)
	if err != nil {
		fatal(err)
	}
	if _, err := blobs.Get(dref); err != nil {
		fatal(err)
	}
	fmt.Printf("  committed + read back: %s\n", short(dref))
}

func backends(csv string) []aw.Backend {
	models := strings.Split(csv, ",")
	out := make([]aw.Backend, 0, len(models))
	for _, m := range models {
		if m = strings.TrimSpace(m); m != "" {
			out = append(out, claude.Backend{Model: m})
		}
	}
	return out
}

// object builds a minimal single-string-field JSON Schema.
func object(field string) map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required":   []any{field},
		"properties": map[string]any{field: map[string]any{"type": "string"}},
	}
}

// str returns the first present string key from out.
func str(out map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := out[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "aw:", err); os.Exit(1) }

func short(ref string) string {
	if len(ref) > 18 {
		return ref[:18]
	}
	return ref
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 100 {
		return s[:97] + "..."
	}
	return s
}
