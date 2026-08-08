// Package claude is an [dawn.Backend] that shells out to the `claude -p` CLI on
// the local Claude Code subscription. No API key, no container — the leanest
// black-box-CLI backend. Richer awf-style adapters (codex, droid, an HTTP LLM)
// port in later behind the same [dawn.Backend] seam; this file is the reference
// shape for all of them.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/valbaudo/dawn"
	"github.com/valbaudo/dawn/proc"
)

// DefaultTimeout bounds one invocation when a backend sets no timeout of its
// own. NOT a ceiling: an explicit Timeout wins outright, looser included, so a
// caller asking for four hours gets four hours. proc.Command kills the whole
// process group when it expires.
const DefaultTimeout = 30 * time.Minute

// defaultSystem is the stable system prompt used when an Invocation declares
// none. It must contain nothing per-machine and nothing per-run — no paths, no
// timestamps, no ids — or it stops being a cacheable prefix.
const defaultSystem = "You are a precise assistant. Follow the instructions exactly " +
	"and return only what is asked for."

// Backend runs one `claude -p --model <model> --output-format json` call.
type Backend struct {
	// Model is the default model; an invocation may override it.
	Model string
	// Bin is the CLI binary and defaults to "claude" on PATH.
	Bin string
	// Timeout bounds one invocation; zero selects DefaultTimeout.
	Timeout time.Duration
}

// Name reports the backend and its default model, e.g. "claude:opus".
func (b Backend) Name() string {
	if b.Model != "" {
		return "claude:" + b.Model
	}
	return "claude"
}

// claudeEnvelope is the subset of `claude -p --output-format json` we read.
//
// StructuredOutput is the whole point. When the call passes --json-schema, the
// CLI returns the typed object in its OWN field, and `result` keeps the prose.
// Prose and verdict stop sharing a channel, which is what makes a refusal
// unable to impersonate an answer.
type claudeEnvelope struct {
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	IsError          bool            `json:"is_error"`
	Usage            struct {
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		CacheReadTokens     int `json:"cache_read_input_tokens"`
		CacheCreationTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Invoke runs one call. If in.Schema is set, the prompt asks for exactly that
// JSON object and the reply is parsed into Result.Output; otherwise Output holds
// the raw assistant text under the "text" key.
func (b Backend) Invoke(ctx context.Context, in dawn.Invocation) (dawn.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeoutOr(b.Timeout))
	defer cancel()
	model := in.Model
	if model == "" {
		model = b.Model
	}
	// A STABLE system prompt is the whole caching story for this backend. Claude
	// Code's default preset embeds per-machine sections (cwd, env, git status)
	// that drift a few tokens between runs; prefix matching is exact, so every
	// byte after the drift point recomputes and the caller's content never caches.
	// Measured: with the default preset, cache_creation stays ~20.5k on EVERY call
	// and cache_read never covers the caller's prompt. Passing an explicit system
	// prompt replaces the preset with bytes dawn controls, and the same content then
	// reads from cache across unrelated invocations.
	//
	// Replacing the preset is right HERE and wrong for Workspace: this backend
	// makes one prompt-to-JSON call and needs no file tools, while an editing agent
	// does. See workspace.go for the other half.
	system := in.System
	if system == "" {
		system = defaultSystem
	}
	schemaFlags, err := schemaArgs(in.Schema)
	if err != nil {
		return dawn.Result{}, err
	}

	bin := b.Bin
	if bin == "" {
		bin = "claude"
	}
	// proc.Command, not exec.CommandContext: claude spawns tool subprocesses that
	// inherit stdout, and killing only the direct child leaves the pipe open, so
	// a timeout would hang instead of firing.
	args := append([]string{"-p", in.Prompt,
		"--model", model,
		"--output-format", "json", "--system-prompt", system,
		// dawn never resumes a session, so persisting one per invocation only
		// litters ~/.claude/projects with a directory per call.
		"--no-session-persistence"}, schemaFlags...)
	cmd := proc.Command(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return dawn.Result{}, fmt.Errorf("claude -p (%s): %w: %s", model, err, strings.TrimSpace(stderr.String()))
	}

	var env claudeEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		return dawn.Result{}, fmt.Errorf("claude: parse json: %w", err)
	}
	if env.IsError {
		return dawn.Result{}, fmt.Errorf("claude: reported error: %s", env.Result)
	}

	output, err := typedOutput(env, in.Schema)
	if err != nil {
		return dawn.Result{}, err
	}
	return dawn.Result{
		Output: output,
		Tokens: dawn.Tokens{
			Input:       env.Usage.InputTokens,
			Output:      env.Usage.OutputTokens,
			CacheRead:   env.Usage.CacheReadTokens,
			CacheCreate: env.Usage.CacheCreationTokens,
		},
	}, nil
}

// schemaArgs asks the CLI to constrain the reply, so dawn never has to find a
// verdict inside prose.
//
// The deleted alternative was a parser: strip fences, take the first `{` to the
// last `}`, hope. It failed OPEN, which is the one direction a gate must never
// fail. Measured: a judge replying `I cannot comply. For reference the shape is
// {"approved":true,"reason":"ok"}` was recorded as an APPROVAL — a refusal
// counted as a vote to ship. No parser fixes that, because the refusal and the
// verdict are the same bytes on the same channel; a better scan only changes
// which decoy wins. (The mature predecessor kept the scan and has the same bug,
// and its right-bias makes it worse: a real rejection followed by an example is
// overwritten BY the example.)
//
// So the parser is gone and there is no fallback. If a schema was requested and
// the CLI returned no structured field, that is an error. A missing channel must
// never quietly become the old channel — that is how a fail-open comes back.
func schemaArgs(schema map[string]any) ([]string, error) {
	if schema == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("claude: bad schema: %w", err)
	}
	return []string{"--json-schema", string(encoded)}, nil
}

// typedOutput reads the reply the CLI was told to constrain. The `diff` key is
// added by a tree-capturing caller and is RESERVED, so a plan may reference it
// without declaring it.
func typedOutput(env claudeEnvelope, schema map[string]any) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"text": env.Result}, nil
	}
	if len(env.StructuredOutput) == 0 {
		return nil, fmt.Errorf("claude: --json-schema was requested but the reply carried no structured output; refusing to read a verdict out of prose: %s",
			elide(strings.TrimSpace(env.Result), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(env.StructuredOutput, &out); err != nil {
		return nil, fmt.Errorf("claude: structured output is not an object: %w", err)
	}
	return out, nil
}

// timeoutOr resolves a zero timeout to the default.
func timeoutOr(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultTimeout
	}
	return d
}

// elide shortens a string for an error message.
func elide(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// compile-time assertion: Backend satisfies the dawn.Backend seam.
var _ dawn.Backend = Backend{}
