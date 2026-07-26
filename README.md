# dawn

**D**irected **A**gent **W**ork **N**odes.

Glue agent invocations together behind an independent acceptance gate. Lean by design.

The whole core is three ideas: an `Invocation`, a `Result`, and a `Backend` that turns
one into the other. Everything else is a thin, single-purpose package over those types.
Two seams vary — `Backend` (how one invocation runs) and `Blobs` (where state commits) —
and everything else is plain code whose dependency arrows all point at `dawn`.

## Layout

| Path | What | Depends on |
|------|------|------------|
| `dawn.go` | core types + the `Backend` seam | nothing |
| `proc/` | child processes whose whole group dies on cancel, so timeouts fire | nothing |
| `store/` | content-addressed state: `Blobs` for bytes (`Mem`, durable `FS`), `Trees` for directories (git-backed) | `proc` |
| `gate/` | judge / jury / k-of-N quorum / repair loop — a library, not an engine | `dawn` |
| `backend/claude/` | `Backend` over `claude -p`; `Workspace` edits a repo, captures + materializes a tree | `dawn`, `store` |
| `plan/` | strict static-DAG runner: typed output, no control flow, identity-keyed reuse | `dawn`, `store`, `gate`, `yaml.v3` |
| `cmd/dawn/` | `dawn run` and `dawn show` | all |

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
go run ./cmd/dawn show examples/pipeline.yaml      # what is stale, and the call count
go run ./cmd/dawn run  examples/pipeline.yaml      # run; commits to .dawn/
go run ./cmd/dawn run  examples/pipeline.yaml      # again: zero paid work
go run ./cmd/dawn run  examples/pipeline.yaml --redo draft
go run ./cmd/dawn show examples/pipeline.yaml tighten.sentence
go run ./cmd/dawn run  examples/gated.yaml         # a gated step: draft -> 3-model panel -> repair

# two agents editing a real repo, the tree flowing between them:
go run ./cmd/dawn run  examples/repo.yaml --in examples/calc
go run ./cmd/dawn show examples/repo.yaml test.workspace --in examples/calc | tar -x -C out/
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
  release_note:
    agent: claude/sonnet
    prompt: Write a three-sentence release note for the new --json flag.
    outputs: { text: string }
    gate:
      judges: [claude/haiku, claude/sonnet, claude/opus]  # independent votes
      quorum: 2                     # optional; omitted = majority, ties reject
      criteria: Approve ONLY if it is exactly three sentences.
```

`gate:` is also what lets the language refuse `if:`, `loop:` and reduce entirely —
a quorum needs counting, repair needs iteration, and halting needs a conditional.
Packaging the one legitimate use of all three as data means none of them exists as
syntax.

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
steps:
  draft:
    agent: claude/sonnet          # <backend>/<model>; no agents: block
    prompt: Write one sentence about content-addressed storage.
    outputs:
      sentence: string
      tone: [plain, vivid]        # a sequence IS an enum
  tighten:
    agent: claude/opus
    prompt: Tighten the provided sentence. Return only the rewrite.
    inputs:
      draft: draft.sentence       # checked at LOAD time against draft's outputs
    outputs: { sentence: string }
```

## State transfer

An invocation produces state refs (`Result.Produced`) and consumes them
(`Invocation.Inputs`). A `Workspace` invocation captures its tree as a
content-addressed `workspace` ref; a later invocation materializes that ref into
a fresh dir and builds on it — so `repo@v1 → agent → repo@v2 → agent → repo@v3`
flows with no shared mutable directory. `examples/repo.yaml` demonstrates it.

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

More backends (codex, an HTTP LLM), and concurrency beyond the jury. Each slots
behind an existing seam without touching the core.

The language is specified in [SPEC.md](SPEC.md) — 10 keys, 2 commands, 3 flags —
and the more useful half of that document is what it deliberately refuses, and why.
