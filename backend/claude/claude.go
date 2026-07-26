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

// defaultSystem is the stable system prompt used when an Invocation declares
// none. It must contain nothing per-machine and nothing per-run — no paths, no
// timestamps, no ids — or it stops being a cacheable prefix.
// DefaultTimeout bounds ONE invocation. Unattended means nobody is watching, and a
// hung tool call with no deadline is indistinguishable from slow work — it just
// burns the night. proc.Command already kills the whole process group on cancel;
// this is what finally drives it. A field rather than a constant so a caller can
// tighten it, zero meaning the default.
const DefaultTimeout = 30 * time.Minute

const defaultSystem = "You are a precise assistant. Follow the instructions exactly " +
	"and return only what is asked for."

// Backend runs one `claude -p --model <model> --output-format json` call. Model
// is the default model ("haiku"|"sonnet"|"opus"|a full id); an Invocation may
// override it per call. Bin overrides the binary name for tests.
type Backend struct {
	Model   string
	Bin     string        // defaults to "claude" on PATH
	Timeout time.Duration // 0 => DefaultTimeout
}

// Name reports the backend and its default model, e.g. "claude:opus".
func (b Backend) Name() string {
	if b.Model != "" {
		return "claude:" + b.Model
	}
	return "claude"
}

// claudeEnvelope is the subset of `claude -p --output-format json` we read.
type claudeEnvelope struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	Usage   struct {
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
	prompt, err := withSchema(in.Prompt, in.Schema)
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
	cmd := proc.Command(ctx, bin, "-p", prompt,
		"--model", model,
		"--output-format", "json",
		"--system-prompt", system,
		// dawn never resumes a session, so persisting one per invocation only
		// litters ~/.claude/projects with a directory per call.
		"--no-session-persistence")
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

	output, err := parseReply(env.Result, in.Schema)
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

// extractJSON pulls the first {...} object out of a model reply, tolerating code
// fences and surrounding prose. A reply that holds no JSON object is an ERROR,
// never a value: returning a placeholder here (the old {"_unparsed": ...}) let a
// refusal or a rate-limit message flow downstream as if it were data, where a
// caller reading a missing field would score it as a legitimate answer.
func extractJSON(s string) (map[string]any, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '{'); i >= 0 {
		if j := strings.LastIndexByte(s, '}'); j > i {
			var m map[string]any
			if err := json.Unmarshal([]byte(s[i:j+1]), &m); err == nil {
				return m, nil
			}
		}
	}
	return nil, fmt.Errorf("claude: reply contained no JSON object: %s", elide(strings.TrimSpace(s), 200))
}

// withSchema appends the typed-output contract to a prompt. Shared by both
// backends: they diverged once — the workspace backend returned a fixed field set
// and ignored the schema entirely, so a step declaring its own outputs failed
// validation on the backend's own keys.
func withSchema(prompt string, schema map[string]any) (string, error) {
	if schema == nil {
		return prompt, nil
	}
	hint, err := json.Marshal(schema)
	if err != nil {
		return "", fmt.Errorf("claude: bad schema: %w", err)
	}
	return prompt + "\n\nRespond with ONLY one JSON object matching this schema — no prose, no code fences:\n" + string(hint), nil
}

// parseReply turns a CLI reply into typed output, honoring the declared schema.
// The `diff` key is added by a tree-capturing caller and is RESERVED, so a plan
// may reference it without declaring it.
func parseReply(reply string, schema map[string]any) (map[string]any, error) {
	if schema == nil {
		return map[string]any{"text": reply}, nil
	}
	return extractJSON(reply)
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
