// Command dawn runs and inspects agent plans.
//
//	dawn run  PLAN       [--dir DIR] [--in DIR] [--redo NAME]… [--jobs N]
//	dawn show PLAN [REF] [--dir DIR] [--in DIR] [--redo NAME]… [--jobs N]
//
// `run` executes the plan's steps in dependency order, committing each step's
// typed output against its identity key. Re-running IS resuming: a step whose key
// the journal already holds is skipped.
//
// `show` with no REF is the dry run — what is fresh, what is stale, and the
// worst-case call count. There is no --dry-run flag, because "a mode of run that
// does not run" grows a second identity-resolution path inside run; this way run
// is show plus executing the stale frontier.
//
// `show PLAN <step>.<field>` prints a committed value, and a workspace field
// streams as a tar: `dawn show p.yaml fix.workspace | tar -x -C out/`.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/backend/claude"
	"github.com/valbaudo/dawn/plan"
	"github.com/valbaudo/dawn/store"
)

// Exit codes. Unattended means something reads $?, and the distinction nothing
// else in this ecosystem gives you is "the panel refused" versus "the machine
// broke".
const (
	exitRefused     = 1
	exitUsage       = 2
	exitMechanical  = 3
	exitInterrupted = 130
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run", "show":
		// A scheduler's SIGTERM and an operator's Ctrl-C both cancel ctx. On Unix,
		// proc.Command turns cancellation into a process-group kill, so the agent
		// CLI and its tool subprocesses die with the run. Other platforms kill the
		// direct child only. proc.WaitDelay bounds inherited-pipe waits everywhere.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := execute(ctx, cmd, args); err != nil {
			code := exitCode(ctx.Err() != nil, err)
			if code == exitInterrupted {
				// The reaped child reports "signal: killed", which reads like a crash
				// in a log nobody watched. Name what happened, keeping the step that
				// was in flight.
				err = fmt.Errorf("interrupted: %w", err)
			}
			die(code, err)
		}
	default:
		usage()
	}
}

// exitCode classifies a failed run for whatever reads $?.
//
// interrupted is checked FIRST and deliberately outranks the error's own type.
// A signal cancels every in-flight Invoke, so the run surfaces as whatever the
// cancelled call returned — for a gate, gate.Gate correctly reports the
// cancelled judges as mechanical rather than as a verdict. Classifying on the
// error alone would therefore report "the machine broke" for what was really an
// operator pressing Ctrl-C.
func exitCode(interrupted bool, err error) int {
	switch {
	case interrupted:
		return exitInterrupted
	case errors.As(err, new(*plan.RejectedError)):
		return exitRefused
	case errors.As(err, new(*usageError)), errors.As(err, new(*plan.ValidationError)):
		return exitUsage
	default:
		return exitMechanical
	}
}

type usageError struct{ error }

func usagef(format string, a ...any) error { return &usageError{fmt.Errorf(format, a...)} }

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n"+
		"  dawn run  PLAN       [--dir DIR] [--in DIR] [--redo NAME]... [--jobs N]\n"+
		"  dawn show PLAN [REF] [--dir DIR] [--in DIR] [--redo NAME]... [--jobs N]")
	os.Exit(exitUsage)
}

func die(code int, err error) {
	fmt.Fprintln(os.Stderr, "dawn:", err)
	os.Exit(code)
}

func execute(ctx context.Context, cmd string, args []string) error {
	positional, flags := split(args)
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dir := fs.String("dir", ".dawn", "state directory: blobs/, trees/ and journal.jsonl")
	in := fs.String("in", "", "host directory bound to the reserved step `in`")
	jobs := fs.Int("jobs", 1, "steps to run at once; independent steps only")
	var redo repeated
	fs.Var(&redo, "redo", "step to re-run even if committed (repeatable)")
	if err := fs.Parse(flags); err != nil {
		return &usageError{err}
	}
	// Refused rather than clamped. A silently-corrected 0 is how `quorum: 0` once
	// became a majority, and a knob that ignores what you typed is worse than one
	// that refuses it.
	if *jobs < 1 {
		return usagef("--jobs must be at least 1, got %d", *jobs)
	}
	switch {
	case len(positional) == 0:
		return usagef("missing PLAN")
	case cmd == "run" && len(positional) > 1, len(positional) > 2:
		return usagef("unexpected argument %q", positional[len(positional)-1])
	}

	p, err := plan.Load(positional[0])
	if err != nil {
		return &usageError{err} // parse/validate: nothing ran, nothing was paid for
	}
	redoNames := redo.set()
	if err := validateRedo(p, redoNames); err != nil {
		return err
	}

	blobs, err := store.NewFS(filepath.Join(*dir, "blobs"))
	if err != nil {
		return err
	}
	trees, err := store.NewTrees(filepath.Join(*dir, "trees"))
	if err != nil {
		return err
	}
	journal, err := plan.OpenJournal(*dir)
	if err != nil {
		return err
	}

	r := &plan.Runner{Blobs: blobs, Journal: journal, Redo: redoNames, Jobs: *jobs, Backend: backends(trees)}
	if *in != "" {
		tree, err := trees.Capture(ctx, *in)
		if err != nil {
			return fmt.Errorf("--in %s: %w", *in, err)
		}
		r.Root = &dawn.Ref{Kind: dawn.KindWorkspace, URI: tree, Media: "application/vnd.git-tree"}
	}

	if cmd == "show" {
		if len(positional) == 2 {
			return showRef(ctx, r, p, trees, positional[1], os.Stdout)
		}
		return showPlan(r, p)
	}
	// Taken for `run` only, and only once the plan has loaded: a bad plan should
	// report its own error, not "another run holds the lock".
	release, err := plan.LockRun(*dir)
	if err != nil {
		return err
	}
	defer release()

	r.Log = func(f string, a ...any) { fmt.Printf("  "+f+"\n", a...) }
	fmt.Printf("[run] %s (%d steps)\n", positional[0], len(p.Steps))
	done, err := r.Run(ctx, p)
	if err != nil {
		return err
	}
	fmt.Println("[done]")
	for _, id := range p.IDs() {
		res := done[id]
		fmt.Printf("  %-14s %s\n", id, short(res.Ref))
		for _, f := range slices.Sorted(fieldNames(p.Steps[id])) {
			v, _ := res.Output[f].(string)
			fmt.Printf("    %s: %s\n", f, oneLine(v))
		}
	}
	return nil
}

func validateRedo(p *plan.Plan, names map[string]bool) error {
	for name := range names {
		if name == "" {
			return usagef("--redo needs a step name")
		}
		if _, ok := p.Steps[name]; !ok {
			return usagef("--redo names unknown step %q", name)
		}
	}
	return nil
}

// showPlan is the dry run: the same identity walk `run` performs, minus execution.
func showPlan(r *plan.Runner, p *plan.Plan) error {
	status, err := r.Status(p)
	if err != nil {
		return err
	}
	stale, calls := 0, 0
	for _, st := range status {
		fmt.Printf("  %-8s %-14s %s\n", st.State, st.ID, short(st.Ref))
		if st.State != "fresh" {
			stale++
			calls += st.Calls
		}
	}
	fmt.Printf("\n  %d of %d steps to run.  worst case %d invocations.\n", stale, len(status), calls)
	if stale > 0 {
		// No "reason" column: a hash tells you MISS, not WHY, and printing "prompt
		// changed" would mean storing and diffing the whole previous definition.
		fmt.Println("  (exact in calls, a range in dollars; `unknown` means an upstream must run first)")
	}
	return nil
}

// showRef prints one committed value. A workspace field streams as a tar.
func showRef(ctx context.Context, r *plan.Runner, p *plan.Plan, trees *store.Trees, ref string, out io.Writer) error {
	id, field, err := plan.ParseRef(ref)
	if err != nil {
		return &usageError{err}
	}
	if _, ok := p.Steps[id]; !ok {
		return usagef("no step %q in the plan", id)
	}
	rec, ok, err := r.Committed(p, id)
	if err != nil {
		return err
	}
	if !ok {
		return usagef("step %q has no committed result for the current plan; run it first", id)
	}
	if produced, isRef := rec.Produced[field]; isRef {
		if produced.Kind != dawn.KindWorkspace {
			return fmt.Errorf("%s is a %s ref, which has no byte form to print", ref, produced.Kind)
		}
		return trees.Archive(ctx, produced.URI, out)
	}
	v, ok := rec.Output[field]
	if !ok {
		return usagef("step %q has no field %q", id, field)
	}
	_, err = fmt.Fprintln(out, v)
	return err
}

// backends maps an agent spec to a concrete backend. `claude` is a prompt-to-JSON
// call; `claude-ws` edits files, which is a privilege posture and therefore a word
// the author typed rather than something inferred from a value.
func backends(trees *store.Trees) func(plan.Agent) (dawn.Backend, error) {
	return func(a plan.Agent) (dawn.Backend, error) {
		switch a.Backend {
		case "claude":
			return claude.Backend{Model: a.Model}, nil
		case "claude-ws":
			return claude.Workspace{Model: a.Model, Trees: trees}, nil
		default:
			return nil, fmt.Errorf("unknown backend %q (have: claude, claude-ws)", a.Backend)
		}
	}
}

// split separates positionals from flags, value-aware so a flag's argument is not
// mistaken for one. Lets flags appear before or after the plan path.
func split(args []string) (positional, flags []string) {
	takesValue := map[string]bool{
		"-dir": true, "--dir": true, "-in": true, "--in": true, "-redo": true, "--redo": true,
		"-jobs": true, "--jobs": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if takesValue[a] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return positional, flags
}

// repeated collects a flag given more than once, e.g. --redo a --redo b.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }
func (r *repeated) set() map[string]bool {
	if len(*r) == 0 {
		return nil
	}
	m := make(map[string]bool, len(*r))
	for _, v := range *r {
		m[v] = true
	}
	return m
}

func fieldNames(s plan.Step) func(func(string) bool) {
	return func(yield func(string) bool) {
		for f := range s.Fields() {
			if !yield(f) {
				return
			}
		}
	}
}

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
