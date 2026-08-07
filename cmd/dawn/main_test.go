package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		{"unknown redo", "run", []string{planPath, "--dir", state, "--redo", "missing"}, "missing"},
		{"empty redo", "run", []string{planPath, "--dir", state, "--redo="}, "needs a step name"},
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

func TestShowMissingInIsUsage(t *testing.T) {
	planPath := writePlan(t, "steps:\n  edit:\n    agent: claude-ws/sonnet\n    prompt: edit\n    inputs: {repo: in.workspace}\n")
	err := execute(context.Background(), "show", []string{planPath, "--dir", t.TempDir()})
	if !errors.As(err, new(*plan.ValidationError)) {
		t.Fatalf("expected plan validation error, got %T: %v", err, err)
	}
	if exitCode(false, err) != exitUsage {
		t.Fatalf("missing --in classified as exit %d, want usage", exitCode(false, err))
	}
	if !strings.Contains(err.Error(), "--in") {
		t.Fatalf("error should name --in, got: %v", err)
	}
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

	step := plan.Step{Agent: "claude/sonnet", Prompt: "build dawn"}
	p := &plan.Plan{Steps: map[string]plan.Step{"build": step}}
	key, err := step.Key("build", plan.Agent{Backend: "claude", Model: "sonnet"}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(struct {
		Output   map[string]any      `json:"output"`
		Produced map[string]dawn.Ref `json:"produced"`
	}{
		Output: map[string]any{"text": "built"},
		Produced: map[string]dawn.Ref{"workspace": {
			Kind: dawn.KindWorkspace, URI: tree, Media: "application/vnd.git-tree",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	blobs := store.NewMem()
	blobRef, err := blobs.Put(blob)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := plan.OpenJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(plan.Entry{Key: key, Ref: blobRef, Step: "build"}); err != nil {
		t.Fatal(err)
	}
	r := &plan.Runner{Blobs: blobs, Journal: journal, Backend: backends(trees)}

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
