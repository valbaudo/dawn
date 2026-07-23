// Command aw is the demo and litmus for the aw runtime.
//
// With no argument it runs the full gate: one Claude model writes a release
// note, a jury of three independent models votes under a k-of-N quorum, and on
// a rejection the critique is fed back and the note regenerated, up to three
// attempts. The accepted decision is committed to a content-addressed store.
//
// With an argument it skips generation and runs a one-shot jury on the text you
// pass — handy for watching the panel reject a deliberately bad note.
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

const (
	reviewer = "You are a strict, independent reviewer. Approve the release note ONLY if it is " +
		"EXACTLY three sentences AND explicitly names the `aw run --json` feature. Be harsh."
	maxAttempts = 3
)

func main() {
	ctx := context.Background()
	blobs := store.NewMem()
	judges := backends(env("AW_JURY", "haiku,sonnet,opus"))
	genModel := env("AW_GEN", "sonnet")
	quorum := gate.Majority(len(judges))

	var candidate string
	var approved bool
	var votes []gate.Verdict

	if len(os.Args) > 1 && strings.TrimSpace(os.Args[1]) != "" {
		// One-shot jury on an injected candidate.
		candidate = os.Args[1]
		fmt.Printf("[jury] injected candidate, %d judges, quorum k=%d\n", len(judges), quorum)
		jctx, cancel := context.WithTimeout(ctx, 200*time.Second)
		approved, votes = gate.Jury(jctx, judges, reviewer, candidate, quorum)
		cancel()
	} else {
		// Full gate: generate -> jury -> repair.
		fmt.Printf("[gate] generate(%s) -> jury of %d, quorum k=%d, up to %d attempts\n",
			genModel, len(judges), quorum, maxAttempts)
		gctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		out, err := gate.Gate(gctx, generator(genModel), judges, reviewer, quorum, maxAttempts)
		cancel()
		if err != nil {
			fatal(err)
		}
		candidate, approved, votes = out.Candidate, out.Approved, out.Votes
		fmt.Printf("  settled on attempt %d of %d\n", out.Attempts, maxAttempts)
	}

	cref, err := blobs.Put([]byte(candidate))
	if err != nil {
		fatal(err)
	}
	fmt.Printf("  candidate %s:\n  %q\n", short(cref), candidate)
	for _, v := range votes {
		if v.Err != nil {
			fmt.Printf("  %-14s ERROR %s\n", v.Judge, oneLine(v.Err.Error()))
			continue
		}
		fmt.Printf("  %-14s approved=%-5v  %s\n", v.Judge, v.Approved, oneLine(v.Reason))
	}
	fmt.Printf("  => VERDICT: approved=%v\n\n", approved)

	// Commit the decision; read it back (all "resume" does).
	fmt.Println("[commit] content-addressed store")
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

// generator returns a Generate closure that writes a release note with the given
// model, folding any prior-attempt critique into the next prompt.
func generator(model string) gate.Generate {
	return func(ctx context.Context, feedback string) (string, error) {
		prompt := "Write a three-sentence release note for a new `aw run --json` flag that streams machine-readable events."
		if feedback != "" {
			prompt += "\n\n" + feedback
		}
		res, err := claude.Backend{Model: model}.Invoke(ctx, aw.Invocation{
			System: "You write concise release notes.",
			Prompt: prompt,
			Schema: object("release_note"),
		})
		if err != nil {
			return "", err
		}
		return str(res.Output, "release_note", "text"), nil
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

func object(field string) map[string]any {
	return map[string]any{
		"type": "object", "additionalProperties": false,
		"required":   []any{field},
		"properties": map[string]any{field: map[string]any{"type": "string"}},
	}
}

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
