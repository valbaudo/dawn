package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	cmd := exec.CommandContext(ctx, bin, "-p", prompt, "--model", model,
		"--output-format", "json", "--dangerously-skip-permissions")
	cmd.Dir = w.Dir
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

	diff, err := gitDiff(ctx, w.Dir)
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
	}
	if w.Store != nil && diff != "" {
		ref, err := w.Store.Put([]byte(diff))
		if err != nil {
			return aw.Result{}, fmt.Errorf("commit diff: %w", err)
		}
		res.Produced = map[string]aw.Ref{"diff": {Kind: aw.KindArtifact, URI: ref, Media: "text/x-diff"}}
	}
	return res, nil
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
