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
| `plan/` | strict static-DAG runner: typed output, no control flow, identity-keyed reuse | `aw`, `store`, `gate`, `yaml.v3` |
| `cmd/aw/` | `aw run` (pipeline) and `aw demo` (gate) | all |
| `cmd/aw-fix/` | kind-2 demo: claude edits a throwaway repo, jury judges the diff | claude, gate, store |
| `cmd/aw-chain/` | workspace forward: fix a repo, feed the result to a second agent | claude, store |

Add a backend or a store behind its interface; the core never changes. Resume is not a
feature and not a flag: a step's identity is a hash of the question it asks (its
resolved definition plus its resolved input refs), so **re-running the same command
IS the resume** — and the recovery path is the normal path, exercised every run.
Edit one prompt and exactly that step and its descendants re-run.

One dependency, `gopkg.in/yaml.v3`, used only by the plan loader. The core is dep-free.

## Try it

Uses your local Claude Code login (no API key):

```sh
go test ./...                                  # deterministic; no network
go run ./cmd/aw demo                            # generate -> 3-model jury -> repair
go run ./cmd/aw demo "One sentence. Two sentence. Three. Four."   # watch the jury reject
go run ./cmd/aw run examples/gated.yaml          # a gated step: draft -> 3-model panel -> repair
go run ./cmd/aw run examples/pipeline.yaml           # commits to .aw/
go run ./cmd/aw run examples/pipeline.yaml           # again: zero paid work
go run ./cmd/aw run examples/pipeline.yaml --redo draft   # force one step
go run ./cmd/aw-fix                             # claude fixes a bug; jury judges the diff
go run ./cmd/aw-chain                           # fix a repo, feed repo@v2 to a second agent
```

## The plan format

Strict and control-flow-free: steps are wired by TYPED references, never string
templating, and any unknown key (`if:`, `loop:`, `map:`) is a parse error.

A step can declare a `gate:` — an independent panel that has to agree before the
step commits. That is data, not control flow: no branch, no loop variable. The
loop lives in the runtime and this configures it, the way `retries: 3` configures
a CI job. A panel that never reaches quorum **fails the step**, so nothing
downstream runs on work it refused.

```yaml
steps:
  - id: release_note
    agent: writer
    prompt: Write a three-sentence release note for the new --json flag.
    output: { text: string }
    gate:
      judges: [haiku, sonnet, opus]   # different models, independent votes
      quorum: 2                        # default: majority, ties reject
      attempts: 3                      # bounded repair on rejection
      criteria: Approve ONLY if it is exactly three sentences.
```

`output:` is a field map with two type forms: `string`, or a sequence which IS an
enum. It compiles to strict JSON Schema — `additionalProperties: false` and every
field required, always, so there are no optional fields and **a reference that
loads can never resolve to a missing value**. A reference to a field the upstream
does not declare fails when the plan loads, before a token is spent.

Inputs resolve by kind: a state ref (a workspace, an artifact) travels as a ref
into the next invocation and is materialized by the backend, while a scalar is
rendered into the prompt. Both are committed, so a later run can still hand a
workspace to the next step.

```yaml
version: 1
agents:
  writer: { backend: claude, model: sonnet }
steps:
  - id: draft
    agent: writer
    prompt: Write one sentence about content-addressed storage.
    output:
      sentence: string
      tone: [plain, vivid]     # a sequence IS an enum
  - id: tighten
    agent: writer
    prompt: Tighten the provided sentence. Return only the rewrite.
    inputs:
      draft: steps.draft.sentence   # checked at LOAD time against draft's output
    output: { sentence: string }
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

`expect:` (declared artifact postconditions), `--in DIR` for a host tree, `--dry-run`,
`aw show`, and more backends (codex, an HTTP LLM). Each slots behind an existing
seam without touching the core. See [SPEC.md](SPEC.md) for the full language and
what is deliberately refused.
