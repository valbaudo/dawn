# aw

Glue agent invocations together behind an independent acceptance gate. Lean by design.

The whole core is three ideas: an `Invocation`, a `Result`, and a `Backend` that turns
one into the other. Everything else is a thin, single-purpose package over those types.
Two seams vary — `Backend` (how one invocation runs) and `Blobs` (where state commits) —
and everything else is plain code whose dependency arrows all point at `aw`.

## Layout

| Path | What | Depends on |
|------|------|------------|
| `aw.go` | core types + the `Backend` seam | nothing |
| `proc/` | child processes whose whole group dies on cancel, so timeouts fire | nothing |
| `store/` | content-addressed state: `Blobs` for bytes (`Mem`, durable `FS`), `Trees` for directories (git-backed) | `proc` |
| `gate/` | judge / jury / k-of-N quorum / repair loop — a library, not an engine | `aw` |
| `backend/claude/` | `Backend` over `claude -p`; `Workspace` edits a repo, captures + materializes a tree | `aw`, `store` |
| `plan/` | strict static-DAG runner: typed input wiring, no control flow, resume | `aw`, `store`, `yaml.v3` |
| `cmd/aw/` | `aw run` (pipeline) and `aw demo` (gate) | all |
| `cmd/aw-fix/` | kind-2 demo: claude edits a throwaway repo, jury judges the diff | claude, gate, store |
| `cmd/aw-chain/` | workspace forward: fix a repo, feed the result to a second agent | claude, store |

Add a backend or a store behind its interface; the core never changes. Resume is not a
feature — it is re-reading committed refs from the store.

One dependency, `gopkg.in/yaml.v3`, used only by the plan loader. The core is dep-free.

## Try it

Uses your local Claude Code login (no API key):

```sh
go test ./...                                  # deterministic; no network
go run ./cmd/aw demo                            # generate -> 3-model jury -> repair
go run ./cmd/aw demo "One sentence. Two sentence. Three. Four."   # watch the jury reject
go run ./cmd/aw run examples/pipeline.yaml --store .aw --state run.json
go run ./cmd/aw run examples/pipeline.yaml --store .aw --state run.json   # re-run: skips committed steps
go run ./cmd/aw-fix                             # claude fixes a bug; jury judges the diff
go run ./cmd/aw-chain                           # fix a repo, feed repo@v2 to a second agent
```

## The plan format

Strict and control-flow-free: steps are wired by TYPED references, never string
templating, and any unknown key (`if:`, `loop:`, `map:`) is a parse error.

```yaml
version: 1
agents:
  writer: { backend: claude, model: sonnet }
steps:
  - id: draft
    agent: writer
    prompt: Write one sentence about content-addressed storage.
    output: sentence
  - id: tighten
    agent: writer
    needs: [draft]
    prompt: Tighten the provided sentence. Return only the rewrite.
    inputs:
      draft: { from: steps.draft.sentence }
    output: sentence
```

## State transfer

An invocation produces state refs (`Result.Produced`) and consumes them
(`Invocation.Inputs`). A `Workspace` invocation captures its tree as a
content-addressed `workspace` ref; a later invocation materializes that ref into
a fresh dir and builds on it — so `repo@v1 → agent → repo@v2 → agent → repo@v3`
flows with no shared mutable directory. `cmd/aw-chain` demonstrates it.

A tree ref is a git tree sha, so identity really is the content: the same bytes
captured on another day, by another user, on another machine give the same ref.
Symlinks round-trip, the exec bit is normalized, identical blobs are stored once
across versions, `.gitignore` is honored, and **any two captured refs diff
directly** — not just consecutive ones. The working directory needs no `.git`;
the store is the only repository involved.

## What it refuses to do

A judge that could not return a verdict has not voted. If an evaluator errors, or
replies with prose instead of the requested JSON, `gate` surfaces a mechanical
failure — it does not score it as a rejection, does not consume a repair attempt,
and never reads it as approval. The same rule holds at the adapter boundary: a
reply containing no JSON object is an error, never a placeholder value that flows
downstream as data. Most review-gate implementations in the wild fail *open* (a
broken reviewer lets the work through), which is a reasonable choice when a human
is watching and the wrong one when nobody is.

Timeouts have to actually fire, too. An agent CLI spawns tool subprocesses that
inherit its stdout, so killing the CLI alone leaves the pipe open and a cancelled
context turns into a hang that looks exactly like slow work. Every child runs in
its own process group and cancellation signals the group.

## Not here yet

`session` refs (claude `--resume`), more backends (codex, an HTTP LLM), workspace
inputs wired through the plan format, and a YAML frontend over a canonical model.
Each slots behind an existing seam without touching the core.
