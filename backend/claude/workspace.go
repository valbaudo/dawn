package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/proc"
	"github.com/valbaudo/dawn/store"
)

// Workspace is an [dawn.Backend] that runs `claude -p` INSIDE a directory, letting
// the agent edit files, and captures the result as a content-addressed tree ref.
//
// It snapshots the tree ONCE, after the turn, and diffs that against the input
// ref it was already handed — so Result.Produced carries the new workspace ref
// and Result.Output carries the diff from the tree it started with. There is no
// before-snapshot: re-capturing the directory just materialized would pay a full
// extra capture per invocation to re-derive a ref already in hand, and a
// re-derivation that DIFFERS from the thing it re-derives is how a declared
// artifact went missing. Because both ends are refs in the same [store.Trees],
// any two captured versions can be diffed later, not just consecutive ones.
//
// The directory needs no .git of its own; the tree store is the only repository
// involved. The workspace ref in Invocation.Inputs is materialized into a fresh
// temp dir, which is how repo@vN reaches the next agent.
//
// ISOLATION: that fresh dir is the whole mechanism, and it is an allocator, not
// an enforcer. Every invocation — so every gate attempt — gets its own 0700
// MkdirTemp, which two steps cannot collide on because neither can name the
// other's. There is deliberately no Dir field: a caller-supplied directory is the
// only way two invocations could share one, so the guarantee is a property of the
// type rather than a convention. See SPEC §8.
//
// It is NOT containment. Editing files non-interactively requires
// --dangerously-skip-permissions, and an absolute path or a spawned subprocess
// writes wherever the uid can. Point Workspace only at a tree you are willing to
// let the agent change.
type Workspace struct {
	// Model is the default model; an invocation may override it.
	Model string
	// Bin is the CLI binary and defaults to "claude" on PATH.
	Bin string
	// Timeout bounds one invocation; zero selects DefaultTimeout.
	Timeout time.Duration
	// Trees is the required tree store used to capture and materialize workspaces.
	Trees *store.Trees
}

// Name reports the backend and its default model, e.g. "claude-ws:sonnet".
func (w Workspace) Name() string {
	if w.Model != "" {
		return "claude-ws:" + w.Model
	}
	return "claude-ws"
}

// Invoke runs one editing turn and captures the resulting tree.
func (w Workspace) Invoke(ctx context.Context, in dawn.Invocation) (dawn.Result, error) {
	if w.Trees == nil {
		return dawn.Result{}, fmt.Errorf("workspace: Trees is required")
	}
	ctx, cancel := context.WithTimeout(ctx, timeoutOr(w.Timeout))
	defer cancel()
	model := in.Model
	if model == "" {
		model = w.Model
	}
	prompt, err := withSchema(in.Prompt, in.Schema)
	if err != nil {
		return dawn.Result{}, err
	}
	if in.System != "" {
		prompt = in.System + "\n\n" + prompt
	}
	bin := w.Bin
	if bin == "" {
		bin = "claude"
	}

	ref, err := workspaceInput(in.Inputs)
	if err != nil {
		return dawn.Result{}, err
	}
	dir, err := os.MkdirTemp("", "dawn-ws-*")
	if err != nil {
		return dawn.Result{}, err
	}
	// The tree is captured below; the directory is scratch. A failure here leaks it
	// rather than losing anything, but it is reported: silently leaking a
	// materialized repo per invocation is how a disk fills up overnight with no
	// record of why.
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "dawn: leaked scratch dir %s: %v\n", dir, err)
		}
	}()
	if err := w.Trees.Materialize(ctx, ref.URI, dir); err != nil {
		return dawn.Result{}, err
	}

	// The baseline IS the input ref. Re-capturing the directory we just
	// materialized to re-derive a tree we were handed was a full extra capture per
	// invocation, and worse than redundant: a re-derivation can DIFFER from the
	// thing it re-derives, which is how a declared artifact went missing.
	base := ref.URI

	// An editing agent NEEDS Claude Code's default system prompt (that is where its
	// file tools are described), so unlike the text backend this one cannot replace
	// it. --exclude-dynamic-system-prompt-sections is the surgical alternative:
	// Anthropic moves the per-machine sections (cwd, env, memory paths, git status)
	// out of the system prompt and into the first user message, which is precisely
	// the drift that defeats prefix caching. Its own documentation says it
	// "improves cross-user prompt-cache reuse"; it is ignored with --system-prompt,
	// which is why the two backends take different routes to the same property.
	cmd := proc.Command(ctx, bin, "-p", prompt, "--model", model,
		"--output-format", "json", "--dangerously-skip-permissions", "--exclude-dynamic-system-prompt-sections",
		"--no-session-persistence")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return dawn.Result{}, fmt.Errorf("claude -p (workspace %s): %w: %s", model, err, strings.TrimSpace(stderr.String()))
	}
	var env claudeEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		return dawn.Result{}, fmt.Errorf("claude: parse json: %w", err)
	}
	if env.IsError {
		return dawn.Result{}, fmt.Errorf("claude: reported error: %s", env.Result)
	}

	// Declared paths are forced into the tree AND asserted here — before the diff,
	// before any judge. A step that did not produce what it promised fails without
	// anyone paying to review it.
	tree, err := w.Trees.CaptureFrom(ctx, dir, base, in.Expect...)
	if err != nil {
		return dawn.Result{}, err
	}
	diff, err := w.Trees.Diff(ctx, base, tree)
	if err != nil {
		return dawn.Result{}, err
	}
	// The model's own reply is parsed against the step's declared outputs, exactly
	// as the text backend does. Only `diff` is added, and only because it is a
	// RESERVED name a plan may reference without declaring. `base` and the raw
	// tree stay internal: the tree is already the workspace ref below, and a
	// backend field that is neither declarable nor reserved would fail validation.
	output, err := parseReply(env.Result, in.Schema)
	if err != nil {
		return dawn.Result{}, err
	}
	output["diff"] = diff
	return dawn.Result{
		Output: output,
		Tokens: dawn.Tokens{
			Input:       env.Usage.InputTokens,
			Output:      env.Usage.OutputTokens,
			CacheRead:   env.Usage.CacheReadTokens,
			CacheCreate: env.Usage.CacheCreationTokens,
		},
		Produced: map[string]dawn.Ref{
			"workspace": {Kind: dawn.KindWorkspace, URI: tree, Media: "application/vnd.git-tree"},
		},
	}, nil
}

// workspaceInput returns the one workspace-kind ref in inputs.
//
// This used to range the map and take the first match, which with two workspace
// refs picked the step's working directory at random — measured 173/27 over 200
// resolutions, the kind of skew that passes every hand test and flips overnight.
// The loader refuses two workspace inputs before a token is spent; the check is
// repeated here because this is where every caller routes, including a backend
// used directly with no plan in front of it.
func workspaceInput(inputs map[string]dawn.Ref) (dawn.Ref, error) {
	var found []string
	for _, name := range slices.Sorted(maps.Keys(inputs)) {
		if inputs[name].Kind == dawn.KindWorkspace {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 0:
		return dawn.Ref{}, fmt.Errorf("workspace: no workspace ref in inputs to materialize")
	case 1:
		return inputs[found[0]], nil
	default:
		return dawn.Ref{}, fmt.Errorf("workspace: %d workspace inputs (%s); the workspace input is the working directory, so there can be at most one",
			len(found), strings.Join(found, ", "))
	}
}

// CapturesTree marks Workspace as able to honor Invocation.Expect.
func (Workspace) CapturesTree() {}

// MaterializesWorkspace marks Workspace as able to consume a workspace input.
func (Workspace) MaterializesWorkspace() {}

var (
	_ dawn.Backend               = Workspace{}
	_ dawn.TreeCapturer          = Workspace{}
	_ dawn.WorkspaceMaterializer = Workspace{}
)
