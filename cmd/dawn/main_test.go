package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/plan"
	"github.com/valbaudo/dawn/store"
)

// The exit code is the entire interface an unattended caller has to the run.
//
// The load-bearing case is the last one: an interrupt cancels every in-flight
// Invoke, so the run surfaces as whatever the cancelled call returned. Classify
// on the error alone and Ctrl-C reports "the machine broke" — or worse, a
// cancelled gate reports "the panel refused".
func TestExitCode(t *testing.T) {
	rejected := fmt.Errorf("step %q: %w", "draft", &plan.RejectedError{Attempts: 3, Objections: "too vague"})
	validation := fmt.Errorf("plan cannot run: %w", &plan.ValidationError{Err: errors.New("missing --in")})

	for _, tc := range []struct {
		name        string
		interrupted bool
		err         error
		want        int
	}{
		{"the panel refused", false, rejected, exitRefused},
		{"the author typed it wrong", false, usagef("missing PLAN"), exitUsage},
		{"the plan cannot run", false, validation, exitUsage},
		{"the machine broke", false, errors.New("connection reset by peer"), exitMechanical},
		{"interrupted", true, errors.New("context canceled"), exitInterrupted},
		{"interrupt outranks a mechanical error", true, errors.New("connection reset by peer"), exitInterrupted},
		{"interrupt outranks a rejection", true, rejected, exitInterrupted},
	} {
		if got := exitCode(tc.interrupted, tc.err); got != tc.want {
			t.Errorf("%s: exitCode(%v, %v) = %d, want %d", tc.name, tc.interrupted, tc.err, got, tc.want)
		}
	}
}

func TestSplitPositionalsAndFlags(t *testing.T) {
	pos, flags := split([]string{"--jobs", "2", "plan.yaml", "step.text", "--redo=draft"})
	if diff := cmpSlices(pos, []string{"plan.yaml", "step.text"}); diff != "" {
		t.Fatal(diff)
	}
	if diff := cmpSlices(flags, []string{"--jobs", "2", "--redo=draft"}); diff != "" {
		t.Fatal(diff)
	}
}

func TestSplitKeepsFlagValuesAttachedAroundPositionals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		pos   []string
		flags []string
	}{
		{"before", []string{"--dir", "state", "plan.yaml"}, []string{"plan.yaml"}, []string{"--dir", "state"}},
		{"between", []string{"plan.yaml", "--in", "work", "step.text"}, []string{"plan.yaml", "step.text"}, []string{"--in", "work"}},
		{"after", []string{"plan.yaml", "step.text", "--redo", "draft", "--jobs=2"}, []string{"plan.yaml", "step.text"}, []string{"--redo", "draft", "--jobs=2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pos, flags := split(tc.args)
			if diff := cmpSlices(pos, tc.pos); diff != "" {
				t.Fatal(diff)
			}
			if diff := cmpSlices(flags, tc.flags); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestExecuteClassifiesAuthorErrorsAsUsage(t *testing.T) {
	planPath := writePlan(t, "steps:\n  draft:\n    agent: claude/sonnet\n    prompt: write\n")
	state := t.TempDir()
	for _, tc := range []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{"missing plan", "run", nil, "missing PLAN"},
		{"unexpected positional", "run", []string{planPath, "extra"}, "unexpected argument"},
		{"unknown flag", "run", []string{planPath, "--wat"}, "flag provided but not defined"},
		{"malformed jobs", "run", []string{planPath, "--jobs", "many"}, "invalid value"},
		{"zero jobs", "run", []string{planPath, "--jobs", "0"}, "at least 1"},
		{"unknown redo run", "run", []string{planPath, "--dir", state, "--redo", "missing"}, "missing"},
		{"empty redo run", "run", []string{planPath, "--dir", state, "--redo="}, "needs a step name"},
		{"unknown redo show", "show", []string{planPath, "--dir", state, "--redo", "missing"}, "missing"},
		{"empty redo show", "show", []string{planPath, "--dir", state, "--redo="}, "needs a step name"},
		{"uncommitted show ref", "show", []string{planPath, "draft.text", "--dir", state}, "run it first"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := execute(context.Background(), tc.cmd, tc.args)
			var usage *usageError
			if !errors.As(err, &usage) {
				t.Fatalf("expected usage error, got %T: %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

type countingBackend struct {
	calls        *int
	captures     bool
	materializes bool
}

func (b countingBackend) Name() string { return "counting" }
func (b countingBackend) Invoke(context.Context, dawn.Invocation) (dawn.Result, error) {
	*b.calls++
	return dawn.Result{}, errors.New("unexpected invocation")
}

type countingTreeBackend struct{ countingBackend }

func (countingTreeBackend) CapturesTree()          {}
func (countingTreeBackend) MaterializesWorkspace() {}

func TestExecutePreflightFailuresAreUsageWithoutBackendInvocation(t *testing.T) {
	original := backendFactory
	t.Cleanup(func() { backendFactory = original })
	for _, failure := range []struct {
		name string
		plan string
		want string
	}{
		{
			name: "missing in",
			plan: "steps:\n  edit:\n    agent: fake/tree\n    prompt: edit\n    inputs: {repo: in.workspace}\n",
			want: "--in",
		},
		{
			name: "capability",
			plan: "steps:\n  draft:\n    agent: fake/text\n    prompt: write\n    expect: [artifact]\n",
			want: "captures no tree",
		},
	} {
		for _, command := range []struct {
			name string
			cmd  string
			ref  string
		}{
			{name: "run", cmd: "run"},
			{name: "show plan", cmd: "show"},
			{name: "show plan ref", cmd: "show", ref: "draft.text"},
		} {
			t.Run(failure.name+"/"+command.name, func(t *testing.T) {
				planPath := writePlan(t, failure.plan)
				var calls int
				backendFactory = func(*store.Trees) func(plan.Agent) (dawn.Backend, error) {
					return func(a plan.Agent) (dawn.Backend, error) {
						base := countingBackend{calls: &calls}
						if a.Model == "tree" {
							return countingTreeBackend{base}, nil
						}
						return base, nil
					}
				}
				args := []string{planPath}
				if command.ref != "" {
					id := "draft"
					if failure.name == "missing in" {
						id = "edit"
					}
					args = append(args, id+".text")
				}
				args = append(args, "--dir", t.TempDir())
				err := execute(context.Background(), command.cmd, args)
				var validation *plan.ValidationError
				if !errors.As(err, &validation) {
					t.Fatalf("error = %T %v, want ValidationError", err, err)
				}
				if exitCode(false, err) != exitUsage || !strings.Contains(err.Error(), failure.want) {
					t.Fatalf("error = %v, exit=%d; want usage containing %q", err, exitCode(false, err), failure.want)
				}
				if calls != 0 {
					t.Fatalf("preflight failure invoked backend %d times", calls)
				}
			})
		}
	}
}

type fixedTreeCapturer struct{ result dawn.Result }

func (f fixedTreeCapturer) Name() string { return "fixed-tree-capturer" }
func (f fixedTreeCapturer) Invoke(context.Context, dawn.Invocation) (dawn.Result, error) {
	return f.result, nil
}
func (fixedTreeCapturer) CapturesTree() {}

func runCommitted(t *testing.T, p *plan.Plan, backend dawn.Backend) *plan.Runner {
	t.Helper()
	journal, err := plan.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &plan.Runner{
		Blobs: store.NewMem(), Journal: journal,
		Backend: func(plan.Agent) (dawn.Backend, error) { return backend, nil },
	}
	if _, err := r.Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestShowRefStreamsTar calls showRef with its own buffer, which leaves the one
// token that matters untested: whether `execute` hands it os.Stdout. Swapping
// that for io.Discard makes `dawn show PLAN REF | tar -x` emit nothing, exit 0,
// and silently produce an empty directory — so drive the real command instead
// and read the bytes back off a replaced stdout.
func TestShowRefCommandWritesTarToStdout(t *testing.T) {
	original := backendFactory
	t.Cleanup(func() { backendFactory = original })
	var mu sync.Mutex
	calls := &[]dawn.Invocation{}
	backendFactory = func(trees *store.Trees) func(plan.Agent) (dawn.Backend, error) {
		return func(plan.Agent) (dawn.Backend, error) {
			return capturingBackend{
				recordingBackend: recordingBackend{mu: &mu, calls: calls},
				trees:            trees,
			}, nil
		}
	}
	planPath := writePlan(t, "steps:\n  build:\n    agent: fake/m\n    prompt: build\n")
	dir := t.TempDir()
	if err := execute(context.Background(), "run", []string{planPath, "--dir", dir}); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "stdout")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = f
	err = execute(context.Background(), "show", []string{planPath, "build.workspace", "--dir", dir})
	os.Stdout = saved
	f.Close()
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("the command wrote no bytes to stdout; `| tar -x` would silently yield an empty directory")
	}
	names := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stdout is not a readable tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		names[h.Name] = string(body)
	}
	if got := names["artifact.txt"]; got != "built bytes\n" {
		t.Fatalf("tar on stdout = %v, want artifact.txt with the captured bytes", names)
	}
}

// capturingBackend writes a file and captures the real tree, so the committed
// workspace ref points at bytes the store can archive.
type capturingBackend struct {
	recordingBackend
	trees *store.Trees
}

func (b capturingBackend) Invoke(ctx context.Context, in dawn.Invocation) (dawn.Result, error) {
	dir, err := os.MkdirTemp("", "dawn-test-ws-*")
	if err != nil {
		return dawn.Result{}, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "artifact.txt"), []byte("built bytes\n"), 0o644); err != nil {
		return dawn.Result{}, err
	}
	tree, err := b.trees.Capture(ctx, dir)
	if err != nil {
		return dawn.Result{}, err
	}
	res, err := b.recordingBackend.Invoke(ctx, in)
	if err != nil {
		return dawn.Result{}, err
	}
	res.Produced = map[string]dawn.Ref{"workspace": {
		Kind: dawn.KindWorkspace, URI: tree, Media: "application/vnd.git-tree",
	}}
	return res, nil
}

func TestShowRefStreamsTar(t *testing.T) {
	ctx := context.Background()
	trees, err := store.NewTrees(filepath.Join(t.TempDir(), "trees"))
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "bin", "dawn"), []byte("executable bytes\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tree, err := trees.Capture(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}

	p := &plan.Plan{Steps: map[string]plan.Step{
		"build": {Agent: "fake/model", Prompt: "build dawn"},
	}}
	r := runCommitted(t, p, fixedTreeCapturer{result: dawn.Result{
		Output: map[string]any{"text": "built"},
		Produced: map[string]dawn.Ref{"workspace": {
			Kind: dawn.KindWorkspace, URI: tree, Media: "application/vnd.git-tree",
		}},
	}})

	var buf bytes.Buffer
	if err := showRef(ctx, r, p, trees, "build.workspace", &buf); err != nil {
		t.Fatal(err)
	}
	tarReader := tar.NewReader(&buf)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("archive does not contain bin/dawn")
		}
		if err != nil {
			t.Fatalf("read archive: %v", err)
		}
		if header.Name == "bin/dawn" {
			content, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != "executable bytes\n" {
				t.Fatalf("bin/dawn = %q", content)
			}
			break
		}
	}
}

func TestShowRefWritesScalarToInjectedWriter(t *testing.T) {
	trees, err := store.NewTrees(filepath.Join(t.TempDir(), "trees"))
	if err != nil {
		t.Fatal(err)
	}
	p := &plan.Plan{Steps: map[string]plan.Step{
		"draft": {Agent: "fake/model", Prompt: "write"},
	}}
	r := runCommitted(t, p, fixedTreeCapturer{result: dawn.Result{
		Output: map[string]any{"text": "exact scalar"},
	}})

	var buf bytes.Buffer
	if err := showRef(context.Background(), r, p, trees, "draft.text", &buf); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), "exact scalar\n"; got != want {
		t.Fatalf("scalar bytes = %q, want %q", got, want)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestShowRefReturnsScalarWriterError(t *testing.T) {
	trees, err := store.NewTrees(filepath.Join(t.TempDir(), "trees"))
	if err != nil {
		t.Fatal(err)
	}
	p := &plan.Plan{Steps: map[string]plan.Step{
		"draft": {Agent: "fake/model", Prompt: "write"},
	}}
	r := runCommitted(t, p, fixedTreeCapturer{result: dawn.Result{
		Output: map[string]any{"text": "unwritable"},
	}})
	sentinel := errors.New("writer failed")

	err = showRef(context.Background(), r, p, trees, "draft.text", errorWriter{sentinel})
	if !errors.Is(err, sentinel) {
		t.Fatalf("showRef error = %v, want sentinel writer error", err)
	}
}

// recordingBackend answers conformingly and records what it was asked.
type recordingBackend struct {
	mu    *sync.Mutex
	calls *[]dawn.Invocation
	gate  chan struct{} // optional: closed once `want` calls are in flight at once
	want  int
}

func (b recordingBackend) Name() string           { return "recording" }
func (b recordingBackend) CapturesTree()          {}
func (b recordingBackend) MaterializesWorkspace() {}
func (b recordingBackend) Invoke(ctx context.Context, in dawn.Invocation) (dawn.Result, error) {
	b.mu.Lock()
	*b.calls = append(*b.calls, in)
	n := len(*b.calls)
	b.mu.Unlock()
	if b.gate != nil {
		if n >= b.want {
			select {
			case <-b.gate:
			default:
				close(b.gate)
			}
		}
		select {
		case <-b.gate:
		case <-ctx.Done():
			return dawn.Result{}, ctx.Err()
		}
	}
	out := map[string]any{}
	if req, ok := in.Schema["required"].([]any); ok {
		for _, f := range req {
			name, _ := f.(string)
			out[name] = "ok"
		}
	}
	return dawn.Result{Output: out, Produced: map[string]dawn.Ref{
		"workspace": {Kind: dawn.KindWorkspace, URI: "tree-out"},
	}}, nil
}

// A flag the parser ACCEPTS but whose value never reaches the Runner is worse
// than one it rejects: the run does something other than what was typed and says
// nothing. Every flag here is asserted by its effect, so deleting the wiring for
// any of them turns this red.
func TestAcceptedFlagValuesReachTheRunner(t *testing.T) {
	original := backendFactory
	t.Cleanup(func() { backendFactory = original })

	setup := func(t *testing.T, gate chan struct{}, want int) (*sync.Mutex, *[]dawn.Invocation) {
		t.Helper()
		var mu sync.Mutex
		calls := &[]dawn.Invocation{}
		backendFactory = func(*store.Trees) func(plan.Agent) (dawn.Backend, error) {
			return func(plan.Agent) (dawn.Backend, error) {
				return recordingBackend{mu: &mu, calls: calls, gate: gate, want: want}, nil
			}
		}
		return &mu, calls
	}

	t.Run("--dir places state where it was told", func(t *testing.T) {
		setup(t, nil, 0)
		planPath := writePlan(t, "steps:\n  draft:\n    agent: fake/m\n    prompt: write\n")
		dir := filepath.Join(t.TempDir(), "state")
		// A clean cwd, because the assertion below is "nothing landed in the
		// DEFAULT .dawn" and the package directory is not guaranteed to be empty.
		t.Chdir(t.TempDir())
		if err := execute(context.Background(), "run", []string{planPath, "--dir", dir}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "journal.jsonl")); err != nil {
			t.Fatalf("--dir was ignored; no journal under %s: %v", dir, err)
		}
		if _, err := os.Stat(".dawn"); err == nil {
			t.Fatal("state landed in the default .dawn despite --dir")
		}
	})

	t.Run("--in binds the reserved root step", func(t *testing.T) {
		_, calls := setup(t, nil, 0)
		planPath := writePlan(t, "steps:\n  edit:\n    agent: fake/m\n    prompt: edit\n    inputs: {repo: in.workspace}\n")
		host := t.TempDir()
		if err := os.WriteFile(filepath.Join(host, "a.txt"), []byte("hi\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := execute(context.Background(), "run",
			[]string{planPath, "--dir", t.TempDir(), "--in", host}); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(*calls))
		}
		ref, ok := (*calls)[0].Inputs["repo"]
		if !ok || ref.Kind != dawn.KindWorkspace || ref.URI == "" {
			t.Fatalf("--in did not reach the invocation as a workspace ref: %+v", (*calls)[0].Inputs)
		}
	})

	t.Run("--redo re-pays a committed step", func(t *testing.T) {
		_, calls := setup(t, nil, 0)
		planPath := writePlan(t, "steps:\n  draft:\n    agent: fake/m\n    prompt: write\n")
		dir := t.TempDir()
		for range 2 {
			if err := execute(context.Background(), "run", []string{planPath, "--dir", dir}); err != nil {
				t.Fatal(err)
			}
		}
		if len(*calls) != 1 {
			t.Fatalf("the second run must be a cache hit; calls = %d", len(*calls))
		}
		if err := execute(context.Background(), "run",
			[]string{planPath, "--dir", dir, "--redo", "draft"}); err != nil {
			t.Fatal(err)
		}
		if len(*calls) != 2 {
			t.Fatalf("--redo did not reach the Runner; calls = %d, want 2", len(*calls))
		}
	})

	t.Run("--jobs actually overlaps independent steps", func(t *testing.T) {
		// Both steps block until BOTH are in flight. With Jobs pinned to 1 this
		// deadlocks and the context deadline fires, so the assertion is real
		// concurrency rather than a value read back out of a struct.
		gate := make(chan struct{})
		setup(t, gate, 2)
		planPath := writePlan(t, "steps:\n"+
			"  left:\n    agent: fake/m\n    prompt: l\n"+
			"  right:\n    agent: fake/m\n    prompt: r\n")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := execute(ctx, "run", []string{planPath, "--dir", t.TempDir(), "--jobs", "2"}); err != nil {
			t.Fatalf("--jobs 2 must run the two independent steps at once: %v", err)
		}
	})
}

func writePlan(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cmpSlices(got, want []string) string {
	if len(got) != len(want) {
		return fmt.Sprintf("got %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("got %q, want %q", got, want)
		}
	}
	return ""
}
