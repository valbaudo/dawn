package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/proc"
	"github.com/valbaudo/aw/store"
)

// Workspace is an [aw.Backend] that runs `claude -p` INSIDE a directory, letting
// the agent edit files, and captures the result as a content-addressed tree ref.
//
// It snapshots the tree before and after the turn, so Result.Produced carries the
// new workspace ref and Result.Output carries the diff between the two. Because
// both are refs in the same [store.Trees], any two captured versions can be
// diffed later, not just consecutive ones.
//
// The directory needs no .git of its own; the tree store is the only repository
// involved. Either set Dir, or pass a workspace ref in Invocation.Inputs and the
// backend materializes it into a fresh temp dir first, which is how repo@vN
// reaches the next agent.
//
// SECURITY: editing files non-interactively requires --dangerously-skip-
// permissions, which lets the agent modify anything under the working dir. Point
// Workspace only at a tree you are willing to let it change; the demos use a
// throwaway temp dir they create and delete.
type Workspace struct {
	Dir   string       // working dir; empty means materialize from Inputs
	Model string       // default model; an Invocation may override
	Bin   string       // defaults to "claude"
	Trees *store.Trees // required: where trees are captured and materialized
}

// Name reports the backend and its default model, e.g. "claude-ws:sonnet".
func (w Workspace) Name() string {
	if w.Model != "" {
		return "claude-ws:" + w.Model
	}
	return "claude-ws"
}

// Invoke runs one editing turn and captures the resulting tree.
func (w Workspace) Invoke(ctx context.Context, in aw.Invocation) (aw.Result, error) {
	if w.Trees == nil {
		return aw.Result{}, fmt.Errorf("workspace: Trees is required")
	}
	model := in.Model
	if model == "" {
		model = w.Model
	}
	prompt := in.Prompt
	if in.System != "" {
		prompt = in.System + "\n\n" + prompt
	}
	bin := w.Bin
	if bin == "" {
		bin = "claude"
	}

	dir := w.Dir
	if dir == "" {
		ref, ok := workspaceInput(in.Inputs)
		if !ok {
			return aw.Result{}, fmt.Errorf("workspace: set Dir, or pass a workspace ref in Inputs to materialize")
		}
		d, err := os.MkdirTemp("", "aw-ws-*")
		if err != nil {
			return aw.Result{}, err
		}
		defer os.RemoveAll(d) // the tree is captured below; the dir is scratch
		if err := w.Trees.Materialize(ctx, ref.URI, d); err != nil {
			return aw.Result{}, err
		}
		dir = d
	}

	base, err := w.Trees.Capture(ctx, dir)
	if err != nil {
		return aw.Result{}, err
	}

	cmd := proc.Command(ctx, bin, "-p", prompt, "--model", model,
		"--output-format", "json", "--dangerously-skip-permissions")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return aw.Result{}, fmt.Errorf("claude -p (workspace %s): %w: %s", model, err, strings.TrimSpace(stderr.String()))
	}
	var env claudeEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		return aw.Result{}, fmt.Errorf("claude: parse json: %w", err)
	}
	if env.IsError {
		return aw.Result{}, fmt.Errorf("claude: reported error: %s", env.Result)
	}

	tree, err := w.Trees.Capture(ctx, dir)
	if err != nil {
		return aw.Result{}, err
	}
	diff, err := w.Trees.Diff(ctx, base, tree)
	if err != nil {
		return aw.Result{}, err
	}
	return aw.Result{
		Output: map[string]any{"summary": env.Result, "diff": diff, "base": base, "tree": tree},
		Tokens: aw.Tokens{
			Input:       env.Usage.InputTokens,
			Output:      env.Usage.OutputTokens,
			CacheRead:   env.Usage.CacheReadTokens,
			CacheCreate: env.Usage.CacheCreationTokens,
		},
		Produced: map[string]aw.Ref{
			"workspace": {Kind: aw.KindWorkspace, URI: tree, Media: "application/vnd.git-tree"},
		},
	}, nil
}

// workspaceInput returns the first workspace-kind ref in inputs, if any.
func workspaceInput(inputs map[string]aw.Ref) (aw.Ref, bool) {
	for _, r := range inputs {
		if r.Kind == aw.KindWorkspace {
			return r, true
		}
	}
	return aw.Ref{}, false
}

var _ aw.Backend = Workspace{}
