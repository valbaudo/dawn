// Command aw runs aw pipelines and demos.
//
//	aw run <plan.yaml> [--store DIR] [--state FILE]   run a static-DAG pipeline
//	aw demo [candidate]                               gate/jury demo (release note)
//
// `run` executes a plan's steps in dependency order, committing each typed output
// to a content-addressed store; with --store DIR (durable) and --state FILE it
// checkpoints, so a re-run skips already-committed steps. `demo` shows the gate:
// one model drafts, a jury of three votes, repair on rejection.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/backend/claude"
	"github.com/valbaudo/aw/gate"
	"github.com/valbaudo/aw/plan"
	"github.com/valbaudo/aw/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "run":
		runPlan(os.Args[2:])
	case "demo":
		demo(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n  aw run <plan.yaml> [--store DIR] [--state FILE]\n  aw demo [candidate]")
	os.Exit(2)
}

// ---- aw run: the static-DAG pipeline runner ----

func runPlan(args []string) {
	// stdlib flag stops at the first positional, so lift the plan path out first
	// — this lets flags appear before OR after it (aw run p.yaml --store DIR).
	planPath, flags := splitPlanArg(args)
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	storeDir := fs.String("store", "", "content-addressed store directory (default: in-memory)")
	statePath := fs.String("state", "", "checkpoint/resume file (JSON id->ref)")
	_ = fs.Parse(flags)
	if planPath == "" {
		fatal(errors.New("usage: aw run <plan.yaml> [--store DIR] [--state FILE]"))
	}

	p, err := plan.Load(planPath)
	if err != nil {
		fatal(err)
	}

	var blobs store.Blobs = store.NewMem()
	if *storeDir != "" {
		b, err := store.NewFS(*storeDir)
		if err != nil {
			fatal(err)
		}
		blobs = b
	}

	done := map[string]plan.StepResult{}
	if *statePath != "" {
		if refs := loadState(*statePath); len(refs) > 0 {
			if done, err = plan.Reload(blobs, refs); err != nil {
				fatal(err)
			}
		}
	}

	r := &plan.Runner{
		Blobs:   blobs,
		Backend: claudeFactory,
		Log:     func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) },
	}
	fmt.Printf("[run] %s (%d steps)\n", planPath, len(p.Steps))
	done, runErr := r.Run(context.Background(), p, done)
	if *statePath != "" { // checkpoint even on a partial/failed run so resume works
		saveState(*statePath, plan.Refs(done))
	}
	if runErr != nil {
		fatal(runErr)
	}

	fmt.Println("[done]")
	for _, s := range p.Steps {
		res := done[s.ID]
		fmt.Printf("  %-12s %s\n  %s\n", s.ID, short(res.Ref), oneLine(fmt.Sprint(pick(res.Output, s.Output))))
	}
}

// splitPlanArg separates the single positional plan path from the flags,
// value-aware so a flag's value (e.g. the DIR in `--store DIR`) is not mistaken
// for the plan path. Supports flags before or after the path, and `--flag=val`.
func splitPlanArg(args []string) (planPath string, flags []string) {
	valueFlag := map[string]bool{"-store": true, "--store": true, "-state": true, "--state": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if valueFlag[a] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if planPath == "" {
			planPath = a
		} else {
			flags = append(flags, a) // extra positional -> let flag report it
		}
	}
	return planPath, flags
}

func claudeFactory(a plan.Agent) (aw.Backend, error) {
	switch a.Backend {
	case "claude", "":
		return claude.Backend{Model: a.Model}, nil
	default:
		return nil, fmt.Errorf("unknown backend %q (only \"claude\" is wired)", a.Backend)
	}
}

func loadState(path string) map[string]string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil // no state yet: a fresh run
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

func saveState(path string, refs map[string]string) {
	b, _ := json.MarshalIndent(refs, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "aw: warning: could not write state:", err)
	}
}

func pick(out map[string]any, field string) any {
	if field != "" {
		if v, ok := out[field]; ok {
			return v
		}
	}
	return out["text"]
}

// ---- aw demo: the gate (generate -> jury -> repair) ----

const (
	reviewer = "You are a strict, independent reviewer. Approve the release note ONLY if it is " +
		"EXACTLY three sentences AND explicitly names the `aw run --json` feature. Be harsh."
	maxAttempts = 3
)

func demo(args []string) {
	ctx := context.Background()
	blobs := store.NewMem()
	judges := backends(env("AW_JURY", "haiku,sonnet,opus"))
	genModel := env("AW_GEN", "sonnet")
	quorum := gate.Majority(len(judges))

	var candidate string
	var approved bool
	var votes []gate.Verdict

	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		candidate = args[0]
		fmt.Printf("[jury] injected candidate, %d judges, quorum k=%d\n", len(judges), quorum)
		jctx, cancel := context.WithTimeout(ctx, 200*time.Second)
		approved, votes = gate.Jury(jctx, judges, reviewer, candidate, quorum)
		cancel()
	} else {
		fmt.Printf("[gate] generate(%s) -> jury of %d, quorum k=%d, up to %d attempts\n",
			genModel, len(judges), quorum, maxAttempts)
		gctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		out, err := gate.Gate(gctx, generator(genModel), judges, reviewer, quorum, maxAttempts)
		cancel()
		if err != nil {
			fatal(err)
		}
		candidate, approved, votes = out.Candidate.Text, out.Approved, out.Votes
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
	fmt.Printf("  => VERDICT: approved=%v\n", approved)
}

func generator(model string) gate.Generate {
	return func(ctx context.Context, feedback string) (gate.Candidate, error) {
		prompt := "Write a three-sentence release note for a new `aw run --json` flag that streams machine-readable events."
		if feedback != "" {
			prompt += "\n\n" + feedback
		}
		res, err := claude.Backend{Model: model}.Invoke(ctx, aw.Invocation{
			System: "You write concise release notes.",
			Prompt: prompt,
			Schema: map[string]any{
				"type": "object", "additionalProperties": false,
				"required":   []any{"release_note"},
				"properties": map[string]any{"release_note": map[string]any{"type": "string"}},
			},
		})
		if err != nil {
			return gate.Candidate{}, err
		}
		if s, ok := res.Output["release_note"].(string); ok && s != "" {
			return gate.FromResult(res, "release_note"), nil
		}
		return gate.FromResult(res, "text"), nil
	}
}

// ---- shared helpers ----

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
