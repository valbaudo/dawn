// Command dawn runs and inspects agent plans.
//
//	dawn run  PLAN       [--dir DIR] [--in DIR] [--redo NAME]…
//	dawn show PLAN [REF] [--dir DIR] [--in DIR] [--redo NAME]…
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
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/backend/claude"
	"github.com/valbaudo/dawn/plan"
	"github.com/valbaudo/dawn/store"
)

// Exit codes. Unattended means something reads $?, and the distinction nothing
// else in this ecosystem gives you is "the panel refused" versus "the machine
// broke".
const (
	exitRefused    = 1
	exitUsage      = 2
	exitMechanical = 3
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "run", "show":
		if err := execute(cmd, args); err != nil {
			var rej *plan.RejectedError
			if errors.As(err, &rej) {
				die(exitRefused, err)
			}
			var use *usageError
			if errors.As(err, &use) {
				die(exitUsage, err)
			}
			die(exitMechanical, err)
		}
	default:
		usage()
	}
}

type usageError struct{ error }

func usagef(format string, a ...any) error { return &usageError{fmt.Errorf(format, a...)} }

func usage() {
	fmt.Fprintln(os.Stderr, "usage:\n"+
		"  dawn run  PLAN       [--dir DIR] [--in DIR] [--redo NAME]...\n"+
		"  dawn show PLAN [REF] [--dir DIR] [--in DIR] [--redo NAME]...")
	os.Exit(exitUsage)
}

func die(code int, err error) {
	fmt.Fprintln(os.Stderr, "aw:", err)
	os.Exit(code)
}

func execute(cmd string, args []string) error {
	positional, flags := split(args)
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	dir := fs.String("dir", ".dawn", "state directory: blobs/, trees/ and journal.jsonl")
	in := fs.String("in", "", "host directory bound to the reserved step `in`")
	var redo repeated
	fs.Var(&redo, "redo", "step to re-run even if committed (repeatable)")
	if err := fs.Parse(flags); err != nil {
		return &usageError{err}
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

	ctx := context.Background()
	r := &plan.Runner{Blobs: blobs, Journal: journal, Redo: redo.set(), Backend: backends(trees)}
	if *in != "" {
		tree, err := trees.Capture(ctx, *in)
		if err != nil {
			return fmt.Errorf("--in %s: %w", *in, err)
		}
		r.Root = &dawn.Ref{Kind: dawn.KindWorkspace, URI: tree, Media: "application/vnd.git-tree"}
	}

	if cmd == "show" {
		if len(positional) == 2 {
			return showRef(ctx, r, p, trees, positional[1])
		}
		return showPlan(r, p)
	}
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
func showRef(ctx context.Context, r *plan.Runner, p *plan.Plan, trees *store.Trees, ref string) error {
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
		return fmt.Errorf("step %q has no committed result for the current plan; run it first", id)
	}
	if produced, isRef := rec.Produced[field]; isRef {
		if produced.Kind != dawn.KindWorkspace {
			return fmt.Errorf("%s is a %s ref, which has no byte form to print", ref, produced.Kind)
		}
		return trees.Archive(ctx, produced.URI, os.Stdout)
	}
	v, ok := rec.Output[field]
	if !ok {
		return usagef("step %q has no field %q", id, field)
	}
	fmt.Println(v)
	return nil
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
