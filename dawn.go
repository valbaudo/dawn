// Package dawn is a lean runtime for gluing agent invocations together behind an
// independent acceptance gate. The whole core is three ideas: an [Invocation],
// a [Result], and a [Backend] that turns one into the other. Everything else in
// this module — the jury ([github.com/valbaudo/dawn/gate]), the store
// ([github.com/valbaudo/dawn/store]), a concrete backend
// ([github.com/valbaudo/dawn/backend/claude]) — is a thin layer over these types
// and depends only on this package. Nothing depends the other way.
//
// What is deliberately ABSENT from [Invocation] is the point: no node paths, no
// gate feedback, no attempt counters, no session/config-dir plumbing, no
// workflow directory. Those are orchestration concerns and live in whatever
// drives dawn (a for-loop, a plan runner, Temporal), never in the call itself.
package dawn

import "context"

// Kind classifies a piece of state carried between invocations. The filesystem
// is one way to materialize these, not the data model.
type Kind string

const (
	KindValue     Kind = "value"     // small JSON / scalar result
	KindArtifact  Kind = "artifact"  // a file or immutable blob
	KindWorkspace Kind = "workspace" // a directory / repository tree
	KindSession   Kind = "session"   // agent-native conversation/session state
)

// Ref is a content-addressed reference to a piece of state. The bytes live in a
// store ([github.com/valbaudo/dawn/store]); a Ref carries identity, not payload,
// so it is cheap to pass between invocations and durable across a crash.
type Ref struct {
	Kind  Kind   `json:"kind"`
	URI   string `json:"uri"`             // store address, e.g. "sha256:<hex>"
	Media string `json:"media,omitempty"` // optional media type
}

// Invocation is one call to an agent: WHAT to run, never how often or under what
// condition. A [Backend] interprets Model/System/Prompt/Schema; Inputs are state
// refs the backend materializes into the execution environment.
type Invocation struct {
	Model  string         // backend-specific model id or alias
	System string         // system / role prompt
	Prompt string         // the request
	Schema map[string]any // JSON Schema for typed output; nil => free text
	Inputs map[string]Ref // named state fed in (materialized by the backend)
	// Expect names paths that MUST exist in what this invocation produces. It is
	// a postcondition, not a request: a backend that captures a tree asserts it at
	// capture time, so a step that failed to produce a declared artifact fails
	// before anything downstream — including a judge — is paid for.
	Expect []string
}

// TreeCapturer is an optional [Backend] interface: a backend that runs its agent
// inside a directory and captures the resulting tree, and can therefore honor
// [Invocation.Expect]. A backend that does not implement it has no tree to assert
// against, which lets a caller reject `expect:` on a text-only agent up front
// rather than discovering it mid-run.
type TreeCapturer interface {
	Backend
	CapturesTree()
}

// WorkspaceMaterializer is an optional [Backend] interface: a backend that can
// turn a workspace ref in [Invocation.Inputs] into the directory it runs in.
type WorkspaceMaterializer interface {
	Backend
	MaterializesWorkspace()
}

// Result is what one invocation produced. Committing it to a store (and the
// [Ref]s that yields) is the caller's job — see the store package. That
// separation is why "durable resume" falls out of correct commits rather than
// being a runtime feature.
type Result struct {
	Output map[string]any // typed output (validated against Invocation.Schema by the caller)
	Tokens Tokens         // cost / cache signal
	// Produced holds new state refs this invocation created — a workspace diff,
	// a captured session, an artifact. Empty for a plain text call. This is the
	// other half of state transfer: what one invocation emits, the next consumes
	// via Invocation.Inputs.
	Produced map[string]Ref `json:"produced,omitempty"`
}

// Tokens is the usage signal every backend surfaces. CacheRead/CacheCreate make
// the caching money-lever measurable without any extra plumbing.
type Tokens struct {
	Input       int
	Output      int
	CacheRead   int
	CacheCreate int
}

// Backend is the one seam that genuinely varies: how a single Invocation
// executes. A local `claude -p` CLI, an HTTP LLM call, codex, or a fake for
// tests are all backends. Keep it this small; put execution-environment choices
// (container vs host) inside the backend, never in this interface.
type Backend interface {
	// Name identifies the backend for logs and jury verdicts (e.g. "claude:opus").
	Name() string
	// Invoke runs one invocation to completion and returns its typed result.
	Invoke(ctx context.Context, in Invocation) (Result, error)
}
