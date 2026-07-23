# aw

Glue agent invocations together behind an independent acceptance gate. Lean by design.

The whole core is three ideas: an `Invocation`, a `Result`, and a `Backend` that turns
one into the other. Everything else is a thin, single-purpose package over those types.

## Layout

| Path | What | Depends on |
|------|------|------------|
| `aw.go` | core types + the `Backend` seam | nothing |
| `store/` | content-addressed `Blobs` (seam) + in-memory impl | nothing |
| `gate/` | judge / jury / k-of-N quorum — a library, not an engine | `aw` |
| `backend/claude/` | a `Backend` over the `claude -p` CLI (no API key) | `aw` |
| `cmd/aw/` | demo: generate a note, a 3-model jury votes, commit | all |

Two seams vary, everything else is plain code: `Backend` (how one invocation runs) and
`Blobs` (where state commits). Add a backend or a store behind its interface; the core
never changes. Resume is not a feature — it is re-reading committed refs.

## Try it

Uses your local Claude Code login (no API key):

```sh
go test ./...
go run ./cmd/aw
go run ./cmd/aw "The aw run --json flag is here. It streams events. You can pipe it. We hope you enjoy it."
```

The first run generates a good note and the jury passes it; the second feeds a
four-sentence note and the jury rejects it.

## Not here yet

Bounded-YAML runner, `workspace`/`session` state refs, crash-resume, and more backends
(codex, an HTTP LLM). Each slots behind an existing seam without touching the core.
