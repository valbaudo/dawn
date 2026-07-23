package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/valbaudo/aw"
	"github.com/valbaudo/aw/store"
)

// Workspace is an [aw.Backend] that runs `claude -p` INSIDE a directory, letting
// the agent edit files, then captures what changed as a unified git diff. The
// diff is placed in Result.Output["diff"] so a gate can judge the change, and —
// if Store is set — committed as a Produced "diff" artifact ref, so the change
// is durable and content-addressed like any other state.
//
// Dir must be a git working tree with at least one commit (HEAD must exist).
//
// SECURITY: editing files non-interactively requires --dangerously-skip-
// permissions, which lets the agent modify anything under Dir. Point Workspace
// only at a tree you are willing to let it change; the aw-fix demo uses a
// throwaway temp repo it creates and deletes.
type Workspace struct {
	Dir   string      // git working tree the agent operates in
	Model string      // default model; an Invocation may override
	Bin   string      // defaults to "claude"
	Store store.Blobs // optional: where the captured diff is committed
}

// Name reports the backend and its default model, e.g. "claude-ws:sonnet".
func (w Workspace) Name() string {
	if w.Model != "" {
		return "claude-ws:" + w.Model
	}
	return "claude-ws"
}

// Invoke runs one editing turn and captures the resulting diff.
func (w Workspace) Invoke(ctx context.Context, in aw.Invocation) (aw.Result, error) {
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

	// Resolve the working tree: an explicit Dir, or materialize an input
	// workspace ref into a fresh temp dir — this is how repo@vN reaches the
	// next agent.
	dir := w.Dir
	if dir == "" {
		ref, ok := workspaceInput(in.Inputs)
		if !ok {
			return aw.Result{}, fmt.Errorf("workspace: set Dir, or pass a workspace ref in Inputs to materialize")
		}
		d, cleanup, err := w.materialize(ctx, ref)
		if err != nil {
			return aw.Result{}, err
		}
		defer cleanup()
		dir = d
	}

	cmd := exec.CommandContext(ctx, bin, "-p", prompt, "--model", model,
		"--output-format", "json", "--dangerously-skip-permissions")
	cmd.Dir = dir
	cmd.Stdin = nil
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

	diff, err := gitDiff(ctx, dir)
	if err != nil {
		return aw.Result{}, err
	}
	res := aw.Result{
		Output: map[string]any{"summary": env.Result, "diff": diff},
		Tokens: aw.Tokens{
			Input:       env.Usage.InputTokens,
			Output:      env.Usage.OutputTokens,
			CacheRead:   env.Usage.CacheReadTokens,
			CacheCreate: env.Usage.CacheCreationTokens,
		},
		Produced: map[string]aw.Ref{},
	}
	if w.Store != nil {
		if diff != "" {
			ref, err := w.Store.Put([]byte(diff))
			if err != nil {
				return aw.Result{}, fmt.Errorf("commit diff: %w", err)
			}
			res.Produced["diff"] = aw.Ref{Kind: aw.KindArtifact, URI: ref, Media: "text/x-diff"}
		}
		// Capture the resulting tree so it can be fed to the next invocation.
		tree, err := tarTree(dir)
		if err != nil {
			return aw.Result{}, fmt.Errorf("capture workspace: %w", err)
		}
		wref, err := w.Store.Put(tree)
		if err != nil {
			return aw.Result{}, fmt.Errorf("commit workspace: %w", err)
		}
		res.Produced["workspace"] = aw.Ref{Kind: aw.KindWorkspace, URI: wref, Media: "application/x-tar+gzip"}
	}
	return res, nil
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

// Materialize writes a stored workspace ref (a gzip'd tar captured when a
// Workspace invocation snapshotted its tree) into dir. Exposed so a caller can
// inspect or reuse a captured workspace outside a backend run.
func Materialize(b store.Blobs, ref aw.Ref, dir string) error {
	data, err := b.Get(ref.URI)
	if err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	return untarTree(data, dir)
}

// materialize writes a workspace ref into a fresh temp dir and gives it a
// baseline git commit, so the agent edits repo@vN and gitDiff has a HEAD to diff
// against. The caller must invoke cleanup when done.
func (w Workspace) materialize(ctx context.Context, ref aw.Ref) (dir string, cleanup func(), err error) {
	if w.Store == nil {
		return "", nil, fmt.Errorf("workspace: Store required to materialize an input")
	}
	dir, err = os.MkdirTemp("", "aw-ws-*")
	if err != nil {
		return "", nil, err
	}
	cleanup = func() { _ = os.RemoveAll(dir) }
	if err := Materialize(w.Store, ref, dir); err != nil {
		cleanup()
		return "", nil, err
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.email", "aw@example.com"},
		{"config", "user.name", "aw"}, {"add", "-A"}, {"commit", "-qm", "baseline"},
	} {
		if out, e := gitCmd(ctx, dir, args...); e != nil {
			cleanup()
			return "", nil, fmt.Errorf("materialize baseline git %v: %v: %s", args, e, out)
		}
	}
	return dir, cleanup, nil
}

// gitDiff stages every change (so new and deleted files also appear) and returns
// the unified diff against HEAD. An empty string means the agent changed nothing.
func gitDiff(ctx context.Context, dir string) (string, error) {
	if out, err := gitCmd(ctx, dir, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add: %w: %s", err, out)
	}
	out, err := gitCmd(ctx, dir, "diff", "--cached")
	if err != nil {
		return "", fmt.Errorf("git diff: %w: %s", err, out)
	}
	return out, nil
}

func gitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

var _ aw.Backend = Workspace{}
