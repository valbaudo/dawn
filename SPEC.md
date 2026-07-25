# The aw plan language

**Status: design. Not all of it is built** — see [Delta](#delta-spec-vs-code).

A plan is a static DAG of agent invocations. One mechanism — a **named typed input** —
carries every kind of state a step can need: a scalar, a JSON field, or a whole
directory tree. There is no session, no templating, no control flow.

**15 keys · 2 type forms · 2 reserved names · 2 commands.**

Two rules underpin everything and must never be conflated:

> **Work is input-addressed. Values are content-addressed.**
> A step's identity is a claim about the *question asked*. A ref is a claim about the
> *bytes produced*. A cache hit means "we already paid for an **accepted** answer to
> this exact question" — never "this is what the model produces."

---

## The whole language

```
version: 1                    strict parse; unknown keys are errors

agents:
  <name>:
    backend: claude | claude-ws | …     default: claude
    model:   <id>                       REQUIRED — no ambient defaults

steps:
  - id:      <name>                     unique
    agent:   <agent name>
    prompt:  <text>
    inputs:  {<name>: steps.<id>.<field>}   plain strings; one source only
    output:  {<field>: string | [a,b,c]}    default: {text: string}
    expect:  [<path>, …]                    workspace backends only
    gate:
      judges:   [<agent name>, …]
      criteria: <text>
      quorum:   <int>                   default: n/2+1 (ties reject)
```

Everything else is derived. `id` `agent` `prompt` `inputs` `output` `expect` `gate` ·
`backend` `model` · `judges` `criteria` `quorum` · `version` `agents` `steps`.

**Types.** `string`, or a YAML sequence which *is* an enum (`[low, medium, high]`).
No numbers, booleans, arrays or nested records: the reference grammar stops at
`steps.a.b`, so declaring depth buys nothing statically, and value constraints are
`criteria:`'s job.

**Reserved.** A workspace step auto-produces `workspace` (the tree) and `diff` (its
rendering); neither may be declared. The step id `in` is bound by `--in DIR`.

**CLI.**
```
aw run <plan.yaml> [--dir .aw] [--in DIR] [--redo ID]… [--dry-run]
aw show <step-id> [--into DIR]
```
Exit `0` accepted · `1` **gate refused** · `2` usage/parse/validate · `3` mechanical
or permanent · `130` interrupted. Unattended means something reads `$?`, and the
distinction nothing else gives you is *the panel refused* vs *the machine broke*.

---

## 1 · Declaration

An **agent** declares who runs. A **step** declares what to ask (`prompt`), what it
will produce (`output`, `expect`), and what it needs (`inputs`).

`output:` desugars to JSON Schema with `additionalProperties: false` and **every
declared field required** — always, implicitly. The author never writes those two
keywords, which makes the rule unviolatable rather than merely documented.

**There are no optional fields.** That is what makes the static guarantee a theorem:

```
load time  : the referenced field exists in the schema
runtime    : the output conforms
conformance: every declared field is present
⟹ a reference that passes load time can never resolve to a missing value
```

Optional fields would reintroduce GitHub Actions' silent-empty-string hole. Want
"maybe empty"? Declare the field; let the agent write `""`.

Runs are **sequential**. The jury is the only fan-out, and it already runs
concurrently inside the gate. If wall clock bites, `--jobs` is a *flag*, never a key.

---

## 2 · Context and caching

**Default and only behavior: a fresh context for every invocation.** No key changes
it. No inference from adjacency. **No session mechanism in the language.**

Two arguments, both structural:

1. **A session would kill the gate.** A judge inheriting the generator's transcript
   is not independent — and it fails *silently*: the judge still returns
   `approved: true`, so the run goes green while the differentiator is dead.
2. **A session makes a step's inputs undeclared.** Step 7 would depend on step 3
   with no edge in the DAG, which breaks rewind: replaying step 7 would mean
   reconstructing a conversation the log never described.

Because there is no session syntax anywhere, gate isolation is **structural, not
enforced** — zero lines of validation. Honest scope: aw cannot see inside a
black-box CLI. `claude -p` still loads `CLAUDE.md`, tools and cwd context every
call. The property is exactly *"the judge did not see the generator's transcript."*

### Caching is a prefix property

A provider cache is keyed on the **exact leading tokens**, scoped to (org, model).
Not on a conversation, not on a session id. Two unrelated invocations whose prompts
begin with identical bytes share a hit; two turns of one session that begin with
different bytes do not.

This is measurable, and measured ([docs/caching-measurements.md](docs/caching-measurements.md)) —
`claude -p`, `--model haiku`, a fixed ~8k-token block, `cc`/`cr` = cache created/read:

| shape | cc | cr |
|---|---|---|
| sessionless, **stable** prefix, 3 unrelated session ids | 33,288 → 7,458 → 8,184 | 0 → **25,830** → **25,830** |
| sessionless, Claude Code's **default** preset | ~20,501 **every call** | 20,215 flat |
| `--resume` (linear session) | 295 → 71 | **40,716** → **41,011** |
| `--fork-session` | ~20,033 | 20,215 flat |

Read those four rows together and the rule falls out. Caching works **without any
session** when the prefix is stable. It fails **with or without** a session when the
prefix drifts — Claude Code's default preset moves a few tokens per run (env, cwd,
git status), and since matching is exact, everything after the drift point
recomputes. `--resume` appears to fix caching only because a resumed turn is a
strict prefix extension of a prefix the provider already holds.

> **A session is a workaround for an unstable prefix, not the caching mechanism.**

Fan-out gets no help from sessions either: `--fork-session` inherits nothing
(reproduced twice, tightly timed), because a fork diverges exactly where the
breakpoint sits.

TTL is a **gap** budget, not a run budget: every hit slides it forward, and Claude
Code requests the 1-hour TTL on a subscription. It is unrelated to transcript
retention, which governs whether a prefix can be *reconstructed* rather than whether
its blocks are *resident*.

### What the runtime owes the cache

The money lever is a **runtime obligation, not a language key**. Three rules, none of
which an author writes:

1. **Emit a stable prefix.** Pass an explicit system prompt; never inherit a drifting
   preset. Leading bytes must be identical across invocations of the same model.
2. **Shared content first, and fold deterministically.** Bound inputs are appended in
   sorted order. A Go map range over three inputs measured **3 distinct hashes in 200
   runs** — enough to defeat every hit, silently.
3. **Append the repair critique, never prepend it.** Same model, near-identical
   leading bytes, re-sent within seconds.

### Per vendor, because the answer is not uniform

| CLI | cache keyed on | session needed? |
|---|---|---|
| claude, droid, goose, opencode, gemini-cli | prefix | no |
| **codex** | prefix **+ `prompt_cache_key`** | **yes, for routing affinity** |

`codex-rs/core/src/client.rs` sets `prompt_cache_key` from the session id with no
author-settable override, and `New`/`Cleared`/`Forked` each mint a fresh one — so
back-to-back `codex exec` runs lose affinity and `codex exec resume` is the only
handle. It degrades routing, it does not guarantee a miss. **This lives in the
adapter, behind the `aw.Backend` seam — it is not a key in the plan.**

**No `cache:` knob.** aw does not own the bytes that reach the provider. It ships
ordering discipline plus `cache_read`/`cache_create` per step in the journal.
**One measurement, no knob** — falsifiable, which a knob is not.

---

## 3 · Passing data

```yaml
inputs:
  draft: steps.triage.summary     # a plain string
```

**Two checks, both mandatory.** This is the line between systems that work (Bazel
analysis, Nix `.drv`, dbt `ref()`) and ones that bite at 3am (GH Actions: a
nonexistent property is the empty string; Airflow XCom: a runtime dict lookup).

**Load time** — zero processes spawned, before a token is spent:
1. the value is exactly `steps.<id>.<field>` — two dots, nothing else
2. `<id>` is a declared step (or `in`); the error lists known ids
3. `<field>` is in that step's declared output, or a reserved name; the error lists
   what the step actually declares
4. reserved names aren't declarable; `in` isn't a usable step id
5. a workspace input on a text agent is an error; so is `expect:` on one
6. the reference graph is acyclic
7. judges name declared agents; quorum ∈ 1..n; every agent declares a model

**Runtime** — after the agent returns, **before the step commits**: strict-parse,
verify every declared field present, kinds match, enums are members, **no undeclared
fields**. Any failure rejects the output whole. No coercion, no partial acceptance —
a stray key means the model improvised, and improvisation means the rest is
untrustworthy. ~10 lines of stdlib over a `map[string]any`; not a JSON Schema
dependency.

Schemas are pushed natively where supported (`claude --json-schema`) and appended to
the prompt otherwise. **Push-down is an optimization, never the authority** — the
local validator re-checks on every backend, always.

**A schema violation is a third thing.** Not a crash (the process exited 0), so
retrying spins forever. Not a verdict (no judge ran), so charging it to the gate
budget would burn a repair attempt on something nobody evaluated — the exact
accounting corruption *crash ≠ verdict* exists to prevent. **The step fails, exit 3.**
The retry is `aw run` again, and the journal means you re-pay one step, not the run.

**Order of operations, non-negotiable:**
```
invoke → [capture tree + assert expect] → schema-validate → jury → commit
```
A non-conforming candidate never reaches a judge. Neither does a step that failed to
produce a declared path. The judge's own verdict goes through the same validator
against `{approved: boolean, reason: string}` — that is the mechanism enforcing
*crash ≠ verdict*.

**No templating.** No `{{ }}`, no jq, no CEL. Scalars are **bound**, not substituted:
one labeled block appended per non-ref input. This single decision deletes the
interpolation rules, the undefined-variable check, and the whole rendering path.

**`needs:` is cut** — not taste, a defect. It carries no data, so it can't be in the
identity key; so adding `needs: [smoke]` to a committed `publish` leaves the key
unchanged, `publish` is a cache **hit**, and the ordering you just declared silently
never takes effect. Every ordering constraint is expressible as a data edge (every
step produces at least `text`), which lands it *in* the key and lets the downstream
agent actually see the upstream verdict.

---

## 4 · Binaries and pictures

**The tree is the artifact channel.** There is no second namespace. `dist/aw`
written by `build` is read at `dist/aw` by `smoke` — producer path equals consumer
path, so the prompt can name the path the agent already used.

**Every workspace step gets its own scratch dir**: mkdtemp → materialize declared
inputs → run → capture tree → remove. Never a shared mutable tree; the host
filesystem is never touched. That's Bazel's execroot, and it's already the shape of
the code.

**`expect:` is a mechanical postcondition**, and it's four load-bearing lines:

```
git add -A                  # everything; .gitignore honored
git add -f -- <expect…>     # declared paths, .gitignore overridden,
                            #   and errors if one was never produced
git write-tree
```

Verified, not hypothetical: with `dist/` in `.gitignore`, `git add -A` produces a
tree where `<tree>:dist/aw` is **absent** — the flagship artifact silently missing.
`add -f --` fixes it and fails loudly on a path that was never written. Because
capture runs inside the invocation, a missing artifact fails **before a judge is
paid**.

**Undeclared files are kept but not asserted** — a deliberate deviation from
Bazel/Nix, which discard them. Their contract is hermetic reproducibility; aw's tree
is both the evidence the gate judges and the thing rewind reads, and a black-box
agent writes `.claude/`, `node_modules` and logs by design.

**The host tree enters via `aw run --in DIR`, not a key.** Captured once at run
start, referenced as `steps.in.workspace`. Keeping the path out of the file matters:
the file's bytes are the plan's identity. And because the capture is
content-addressed, editing your source re-runs its consumers — Bazel/Nix input
hashing, for free.

> **The panel judges a rendering, not bytes.** `git diff` renders a changed 200KB
> binary as one sentence: *"Binary files a/dist_aw and b/dist_aw differ."* A panel
> gated on "the tool must be correct" votes on that sentence. Mitigations:
> `--stat --patch` so the panel always sees path/mode/size, and a 128KB body cap
> whose marker is **in the text the judges read** (silent truncation means approving
> work you didn't see). **To gate an artifact's content, declare a text rendering of
> it as an output field and write the criteria against that.** Zero keys.

---

## 5 · Rewind and redo

**Resume is deleted as a concept.** `aw run` computes each step's key in topological
order; if the journal holds an accepted entry for that key, reuse its ref and skip.
Re-running the same command *is* the resume — no mode, no flag, no second code path,
and the safest path is the one exercised every run.

```
key = sha256(canonical JSON of {
    v, id, backend, model, prompt,
    output (resolved), expect (sorted),
    inputs: {name → resolved ref URI, or resolved scalar},
    gate: {judges sorted, criteria, quorum resolved}
})
```

Bazel's action key with aw's nouns: the prompt is the command line, input refs are
the Merkle inputs, backend+model is the toolchain. The key hashes the **resolved**
definition, so writing a default explicitly (`quorum: 2` over two judges) is free.

**Not in the key:** wall clock, run id, hostname, cwd, PID, attempt number, prior
verdicts, tokens, cost, the plan file as a whole, any other step, the agent CLI
version. Anything per-run makes every run a miss; anything global makes every edit a
global miss.

**Only gate-accepted results serve a hit.** A rejection is recorded for forensics but
carries no ref — the model is nondeterministic, so the honest response to "this was
rejected last night" is to run it again. A mechanical failure commits nothing, or an
infrastructure blip becomes a permanent verdict.

| change | invalidates |
|---|---|
| prompt / model / backend / output / expect | that step + descendants |
| gate judges, criteria, or quorum | that step + descendants |
| writing a default explicitly | nothing |
| the `--in` tree | its consumers + descendants |
| reordering steps, judges, or expect paths | nothing |
| add a step | only it |
| delete a step | nothing (lines orphan) |
| upstream re-ran, same bytes | descendants skipped |
| gate rejected last run | that step re-runs |
| agent CLI upgraded | nothing; one-line warning |

**Rewind is a read, not a state transition.** Every committed step is immutable and
content-addressed, so "the state as of step X" is a lookup: `aw show X --into DIR`.
Terraform needs plan/apply/taint because it mutates a live world; aw's outputs are
immutable bytes. Going forward differently is `--redo ID`; downstream invalidation
isn't a feature, it falls out of keying on input refs. No `--from X` — "from"
presumes a total order and a DAG has only a topological one.

**Guaranteed:** a key never serves a result whose defining bytes differ; nothing the
gate didn't accept is reused; a crash loses at most the in-flight step; the journal
is append-only; a second `aw run` with no edits does zero paid work.
**Not guaranteed:** reproducibility (`--redo` yields different text — aw guarantees
*skip*, not *sameness*); detection of change outside the key (model drift, network
the agent read).

The right prior art for an impure step is Nix's **fixed-output derivation**: an
impure builder made safe not by determinism but by a declared predicate over its
output. **aw's gate is that predicate, with jury quorum replacing hash equality** —
a hash accepts one exact output, a jury accepts an equivalence class. Subject to the
rendering limit above.

**The journal** is one append-only `.aw/journal.jsonl`. Two load-bearing fields,
`key` and `ref`; everything else is provenance and is never hashed. Blob first
(fsync), then append (fsync) — a crash between them leaves an orphan blob (harmless
garbage), never a dangling pointer. The journal is also the event stream.

---

## 6 · Switching agents

With no session in the language, **this question has no mechanism to refuse** — which
is the point.
There is no native handle in the language, so there's nothing to mistranslate and no
cross-vendor rule to write.

**Portable context is an ordinary step:**

```yaml
- id: handoff
  agent: writer
  inputs: {summary: steps.triage.summary, diff: steps.build.diff}
  prompt: |
    Write a brief for an assistant that has not seen this work: decisions,
    constraints, rejected alternatives, open questions, current state.
  output: {brief: string}

- id: port
  agent: porter                  # different vendor
  inputs: {brief: steps.handoff.brief}
```

Nobody ports a native handle, and nobody should pretend to: a transcript's on-disk
format is private, version-unstable, keyed to a working directory, and carries a
model identity — vendor blocks get dropped even between models of the *same* vendor.
There's no fidelity claim aw could test, so there's no promise aw should make.

The payoff: a brief is content-addressed, diffable, schema-validated, **gate-able**
and replayable. A vendor transcript is none of those. **Q2 and Q6 have the same
answer, and it costs zero keys.**

---

## 7 · The rest, at zero keys

- **Timeouts** — hardcoded 30m per invocation. `proc/` already kills process groups
  and nothing drives it; unattended plus no timeout means one hung tool call burns
  the night. That gap is in the runtime, not the language. `--timeout` is a flag if
  someone asks.
- **Gate attempts** — hardcoded 3. A result accepted under 3 attempts is equally
  accepted under 5; that's the definition of policy, and policy is a flag.
- **Retries** — none. The retry is `aw run` again; the journal means you re-pay one
  step.
- **Secrets** — never a value in the file. The agent CLI reads its own credentials.
- **Signals** — SIGINT stops launching, cancels children as process groups, exit 130.
  Nothing to flush; commit already happened per step.
- **Agent version** — recorded in the journal, `DISABLE_AUTOUPDATER=1` for children,
  one-line warning when a hit's recorded version differs. **Not** in the key: hashing
  the toolchain turns every background auto-update into a full re-run.
- **`--dry-run`** — prints topological order, fresh/stale/unknown, worst-case
  invocation count. No "reason" column: a hash tells you MISS, not WHY.

---

## 8 · Fan-out, and where the line is

Two questions get conflated here. Separating them dissolves most of the problem.

**The money question needs no language support.** Because the cache is keyed on the
leading tokens (§2), N same-model invocations that begin with an identical block all
hit that block, with no session and no construct. Fan-out is cache-efficient when
the runtime puts shared content first and per-item content last — obligations 1 and
2, which apply to every step anyway. There is no session-shaped shortcut to reach
for either: `--fork-session` measurably inherits nothing.

**The language question is refused in v1**, and the reason is not "loops are scary":

> Fan-out without fan-in is useless, and fan-in is where the predecessor ballooned.

A `map:` key immediately owes answers to: how is one instance's output referenced;
how are *all* instances' outputs referenced; what type is that collection when the
type vocabulary is `string | [enum]`; what happens when instance 7 of 20 fails or
its gate refuses; does the reduce step see partial results; what is the identity key
of a reduce whose inputs are a set. In the predecessor those questions produced
`map`, `reduce`, `prune`, `quorum`, and a cross-cutting rule about what a value
*means* after it crosses `gate → map → reduce → resume`. That is the combinatorial
cost this rebuild exists to escape.

**Where the line sits, for when it is revisited.** Cardinality is the whole test:

| shape | cardinality known | verdict |
|---|---|---|
| N steps written out | at authoring time | works today |
| `over:` a declared literal list | at load time | *could* be data |
| `over:` a runtime-produced array | only after a step runs | a program |

Only the middle row is arguable, and even it must earn its keep against the reduce
problem above. The bottom row breaks three properties at once: `--dry-run` cannot
count the bill, the reference grammar has no way to name instance *k*, and the
identity key of the fan-in is not computable until the fan-out has run.

**What to do today:** write the N steps. It is more lines and it is honest lines —
each one is independently keyed, independently gated, independently resumable, and
visible in `--dry-run`. And note the one fan-out that already exists needs no key at
all: **the jury**, which fans a candidate to N judges concurrently and reduces by
quorum. That is a fan-out with a fixed fan-in rule, which is exactly why it can be
data instead of syntax.

---

## Refused

Each is a parse error with a one-sentence message naming the alternative.

| refused | why |
|---|---|
| `if:` `when:` `unless:` | changes *which* steps run; a gate could silently not run |
| `loop:` `while:` `until:` | data-dependent iteration plus a feedback edge |
| `map:` `foreach:` `each:` | deferred — see open questions |
| `on:` `schedule:` `cron:` | aw is invoked; cron invokes it |
| `needs:` | use a data edge; then every edge is in the key |
| `session:` `context:` | §2 |
| `dir:` | `--in DIR`; keeps host paths out of the plan's identity |
| `timeout:` `attempts:` | execution policy — hardcoded, flags if asked |
| `files: {name: path}` | `expect: [path]`; the tree is the channel |
| `continue_on_error:` | fail-closed **is** the product |
| `{{ }}` `${{ }}` | scalars are bound as inputs, never substituted |
| `inputs:` under `gate:` | this is the gate-isolation rule |
| `output: {n: number}` | two type forms: `string` \| `[enum]` |

---

## Open questions

1. **Memoized impurity over time.** The key is a claim about the question, not the
   answer, so an accepted result from March is served against a July model that
   drifted — and *because it's a hit, the gate never re-runs*. Every mitigation costs
   real: a TTL puts wall clock in the key (Make's disease); re-judging every hit pays
   the jury forever. **Stance:** record version + timestamp, warn on divergence; if
   it bites, `--max-age` as an input to the *lookup rule*, never a key component.
2. **Which criteria are actually judgeable.** "The diff must not delete tests" is
   enforceable; "the tool is correct" is not; both compile to the same YAML. No
   mechanical check, and I'm not inventing one — the fix is a manual section of
   worked criteria.
3. **The 128KB evidence cap is a guess.** Measure against a real repair loop. It's a
   constant, not a key, so changing it invalidates nothing.
4. **No re-ask is an unmeasured bet.** With native schema push-down, violations
   should be near zero. For a CLI without it the schema arrives as prompt text and
   the conformance rate is unknown. Measure before the second backend lands; the fix
   is a hardcoded 1, not a key.
5. ~~Fan-out~~ — **settled, see §8.** Refused in v1 because fan-out without fan-in is
   useless and fan-in is where the predecessor ballooned. The cache motivation for it
   dissolved: prefix ordering already makes same-model fan-out cache-efficient with
   no construct at all.
6. **The `--jobs` question.** Runs are sequential (§1) and the jury is the only
   concurrency. That is right for a first version — it deletes journal interleaving
   and rate-limit meltdowns — but an unattended overnight run of a 12-step plan pays
   for it in wall clock. Revisit as a flag, with a measured number, once a real plan
   is slow rather than hypothetically slow.

---

## Delta: spec vs code

Built today: `version` `agents` `steps` `id` `agent` `prompt` `gate`
(judges/criteria/quorum/attempts) · **`output:` as a typed field map** with both
type forms, defaulting to `{text: string}`, compiling to strict JSON Schema and
re-validated locally on every backend · **`inputs:` as plain
`steps.<id>.<field>` strings**, resolved by kind (ref → `Invocation.Inputs`,
scalar → prompt) · **load-time field checking** (a reference to an undeclared
field fails before a token is spent) · reserved-name rules · cycle checks ·
per-step scratch dirs · tree capture/materialize · fail-closed gates ·
process-group timeouts · **deterministic sorted input folding** (runtime
obligation 2 — was a real bug, see below) · **the identity key** (resolved
definition + resolved input refs; explicit defaults and judge order are free;
`attempts` is excluded) · **the append-only journal** (`--dir`, blob-then-line,
only an accepted result carries a ref, so a rejection is recorded and never
reused) · **`--redo`** · exit 1 for a refusing panel vs 3 for a mechanical
failure.

Not yet: `expect:` · `--in` · `--dry-run` · `aw show` · cutting `needs:` ·
hardcoding `attempts:` · committing the attempt the panel approved *by index*
rather than the last one generated · a stable emitted system prompt (runtime
obligation 1).

Fixed while writing this spec, both found by specifying rather than by a bug report:
the input fold used a Go map range, so any step with 2+ scalar inputs produced
different prompt bytes every run and could never hit a cache; and `git add -A`
silently drops a `.gitignore`'d declared artifact, which `expect:` closes with
`git add -f --`.
