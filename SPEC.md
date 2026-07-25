# The aw plan language

**Status: design.** The code implements an earlier, larger surface — see [Delta](#delta).

A plan is a static DAG of agent invocations. One mechanism — a **named input** —
carries every kind of state a step needs. No sessions, no templating, no control flow.

**10 keys · 2 type forms · 2 commands · 3 flags.**

> **Work is input-addressed. Values are content-addressed.**
> A step's identity is a claim about the *question asked*; a ref is a claim about the
> *bytes produced*. A cache hit means "we already paid for an **accepted** answer to
> this exact question" — never "this is what the model produces."

---

## The whole language

```yaml
steps:                        # the only top-level key
  <step-id>:
    agent:   <backend>/<model>          # both mandatory, no defaults
    prompt:  <text>
    inputs:  {<name>: <step>.<field>}   # a tree if the field is `workspace`, else a scalar
    outputs: {<field>: string | [a,b]}  # default: {text: string}
    expect:  [<path>, …]                # filesystem postconditions
    gate:
      judges:   [<backend>/<model>, …]
      criteria: <text>
      quorum:   <int>                   # optional; omitted = ⌊N/2⌋+1, ties reject
```

Ten keys: `steps` `agent` `prompt` `inputs` `outputs` `expect` `gate` `judges`
`criteria` `quorum`.

**Types — two forms, closed.** `string`, or a sequence which *is* an enum
(`[low, high]`). `outputs: {n: number}` is a load error. A sequence always means an
enum, with no exceptions, so a reader needs no table.

**One rule for scalar vs tree:**

> **`workspace` is the only field name that materializes. Everything else binds as a scalar.**

`inputs: {repo: build.workspace}` is a directory; `inputs: {bug: audit.summary}` is
text. At most one workspace input per step, and it becomes the step's **working
directory** — so a mount path is never written into a prompt. Scalars are appended by
the engine as labeled blocks. Nothing is substituted into prompt text; there is nowhere
a `{{ }}` could go.

**Reserved.** A tree-capturing step auto-produces `workspace` and `diff`: referenceable,
never declarable. The step id `in` is reserved and filled by `--in DIR`.

---

## Fixed by the engine, so they are not keys

- **References are two segments**, `<step>.<field>`, resolved at load against the
  upstream's declared `outputs:` plus the reserved names. Step ids are `[a-z0-9_-]+`, so
  a `.` can never make a reference ambiguous.
- **Repair is bounded at 3.** A bounded loop is the requirement; a tunable one is not.
  `attempts` is already excluded from the identity key — a result accepted under 3
  attempts is equally accepted under 5, which is policy — so fixing it changes no cache
  behavior and makes the cost preview exact.
- **A judge sees** `{prompt, inputs, captured outputs, criteria}` in a fresh context,
  against an engine-fixed verdict schema. No syntax could hand it the generator's
  transcript.
- **Crash ≠ verdict.** A mechanical judge failure propagates; it never consumes an
  attempt and never reads as approval.
- **Identity** = hash(step id, backend, model, prompt, resolved `outputs`, `expect`,
  resolved `gate`, resolved input values/refs). Rewind and redo need no key.
- **Unknown keys, unknown output fields and duplicate step ids are parse errors.**
  Duplicates come free from the map form — verified: yaml.v3 reports
  `line 4: mapping key "fix" already defined at line 2`.

---

## 1 · Declaration

An **agent** is a 2-tuple written as one string: `claude/claude-opus-4-6`. There is no
`agents:` block, because a name pointing at a 2-tuple is indirection carrying zero
information. Verified: slash-bearing ids (`openrouter/anthropic/claude-opus`) decode as
plain scalars in block and flow context alike.

The cost is real, and paid where it helps. Swapping a judge model everywhere becomes a
`sed` or a YAML anchor rather than a one-line edit. In exchange,
`judges: [claude/opus-4-6, claude/sonnet-4-6, claude/haiku-4-5]` shows the panel is
independent **at the point of use**, where `judges: [j1, j2, j3]` makes you scroll.

`outputs:` compiles to JSON Schema with `additionalProperties: false` and **every field
required**, always. The author never writes those two, which makes the rule unviolatable
rather than documented. **There are no optional fields**, and that is what makes the
load-time guarantee a theorem:

```
load time  : the referenced field is declared upstream
runtime    : the output conforms
conformance: every declared field is present
⟹ a reference that loads can never resolve to a missing value
```

Runs are sequential. The jury is the only fan-out, and it already runs concurrently.

---

## 2 · Context and caching

**Every invocation gets a fresh context.** No session key, no inference from adjacency.

Two structural reasons: a judge inheriting the generator's transcript is not
independent, **and it fails silently green**; and an implicit session makes a step's
inputs undeclared, which breaks rewind. Because no session syntax exists, gate isolation
is structural — zero lines of validation.

### Caching is a prefix property

A provider cache is keyed on the **exact leading tokens**, scoped to (org, model) — not
on a conversation. Measured
([docs/caching-measurements.md](docs/caching-measurements.md)):

| shape | cache_create | cache_read |
|---|---|---|
| sessionless, **stable** prefix, 3 unrelated session ids | 33,288 → 7,458 → 8,184 | 0 → **25,830** → **25,830** |
| sessionless, Claude Code's **default** preset | ~20,501 **every call** | 20,215 flat |
| `--resume` (linear session) | 295 → 71 | **40,716** |
| `--fork-session` | ~20,033 | 20,215 flat |

Caching works **without any session** when the prefix is stable, and fails **with or
without** one when it drifts — the default preset moves a few tokens per run (cwd, env,
git status) and matching is exact. `--resume` only appears to fix it because a resumed
turn is a strict prefix extension.

> **A session is a workaround for an unstable prefix, not the caching mechanism.**

Fan-out gets no help either: `--fork-session` inherits nothing.

**What the runtime owes the cache** — obligations, not keys: emit a stable system
prompt; put shared content first and fold inputs deterministically; append the repair
critique, never prepend. Measured after implementing these: two unrelated plans sharing
a ~6k-token prefix, `cache_read` 0 → **17,929**.

**Per vendor.** claude, droid, goose, opencode and gemini-cli key on the prefix alone.
**codex is the exception** — `prompt_cache_key` comes from the session id with no
override, so `codex exec resume` is the only handle. That lives in the adapter, not the
plan.

**No `cache:` knob.** aw owns ordering and reports `cache_read`/`cache_create` per step
in the journal. One measurement, no knob — falsifiable, which a knob is not.

---

## 3 · Passing data

```yaml
inputs:
  bug: audit.summary
```

**Two checks, both mandatory.** This is the line between systems that work (Bazel, Nix,
dbt) and ones that bite at 3am (GitHub Actions: a nonexistent property is the empty
string).

**Load time**, before a token is spent: the reference is exactly `<step>.<field>`; the
step exists; the field is declared upstream or reserved; the graph is acyclic; judges
are well-formed; an explicit quorum ∈ 1..N; `expect:` only on a tree-capturing backend.

**Runtime**, after the agent returns and *before* the step commits: strict-parse, every
declared field present, enum values members, **no undeclared fields**. Any failure
rejects the output whole — a stray key means the model improvised.

**Order of operations, non-negotiable:**
```
invoke → capture tree + assert expect → validate → jury → commit
```
A non-conforming candidate never reaches a judge. Neither does a step that failed to
produce a declared path.

A **schema violation** is a third thing: not a crash (the process exited 0), not a
verdict (no judge ran). The step fails; the retry is `aw run` again, which re-pays one
step rather than the run.

---

## 4 · Artifacts

The tree is the only artifact channel, and **producer path equals consumer path** —
`dist/aw` written by `build` is read at `dist/aw` by `smoke`. Every tree-capturing step
gets its own scratch dir: materialize inputs → run → capture → discard. The host
filesystem is never touched.

`expect:` is a postcondition, and two lines of git give both halves:

```
git add -A                  # everything; .gitignore honored
git add -f -- <expect…>     # forced past .gitignore, and errors if never produced
```

Verified: with `dist/` ignored, plain `add -A` yields a tree where `dist/aw`
**does not exist** — the flagship artifact silently absent.

**A missed `expect:` path is a rejection, not a crash.** Under a gate it feeds repair
with "you did not produce X" and consumes an attempt at **zero judge tokens**; ungated,
it fails the step. Every other capture error stays mechanical.

The host tree enters via `--in DIR`, never a key, so no machine-specific path lives in
the file whose bytes are the plan's identity.

> **The panel judges a rendering, not bytes.** `git diff` renders a changed 200KB binary
> as one sentence. To gate an artifact's *content*, declare a text rendering of it as an
> output field and write the criteria against that.

---

## 5 · Rewind and redo

**Resume is deleted as a concept.** `aw run` computes each key in topological order and
skips what the journal already holds. Re-running *is* the resume — one code path,
exercised every run rather than only after a crash.

**Not in the key:** wall clock, run id, hostname, cwd, PID, attempt number, tokens,
cost, the plan file as a whole, any other step, the agent CLI version.

| change | invalidates |
|---|---|
| prompt / model / backend / outputs / expect / gate | that step + descendants |
| writing a default explicitly, reordering judges | nothing |
| the `--in` tree | its consumers + descendants |
| upstream re-ran, same bytes | descendants skipped |
| gate rejected last run | that step re-runs |

**Only gate-accepted results serve a hit.** A rejection is recorded with no ref — the
model is nondeterministic, so the honest answer to "this was rejected last night" is to
run it again. A mechanical failure commits nothing.

**Rewind is a read**, not a state transition: every committed step is immutable, so "the
state as of X" is a lookup. Going forward differently is `--redo`.

The **journal** is one append-only JSONL. Two fields are load-bearing, `key` and `ref`;
the rest is provenance and is never hashed. Blob first, then the line — a crash between
them leaves an orphan blob, never a pointer to bytes that do not exist.

The right prior art for an impure step is Nix's **fixed-output derivation**: an impure
builder made safe by a declared predicate over its output. **The gate is that predicate,
with jury quorum replacing hash equality** — a hash accepts one output, a jury accepts an
equivalence class.

---

## 6 · Switching agents

There is no session in the language, so there is nothing to port and nothing to refuse.
Portable context is an ordinary step whose output is a handoff brief, consumed by a step
naming a different vendor. Unlike a transcript, a brief is content-addressed, diffable,
schema-validated, **gate-able** and replayable.

---

## 7 · Fan-out

**The money question needs no language support.** Because the cache is prefix-keyed, N
same-model invocations sharing an opening block all hit it — no session, no construct.

**The language question is refused**, and not because loops are scary:

> Fan-out without fan-in is useless, and fan-in is where the predecessor ballooned.

A `map:` key immediately owes answers to: how one instance is referenced, how *all* are,
what type that collection is under a two-form vocabulary, what happens when instance 7 of
20 has its gate refuse, and what the identity key of a reduce over a *set* is.

The line is **cardinality**: known at authoring time (write N steps) works today; known
at load time *could* be data; known only after a step runs is a program. The one fan-out
that already exists needs no key — **the jury**, which fans to N judges and reduces by
quorum. Its fan-in rule is fixed, which is exactly why it can be data.

---

## The CLI

```
aw run  PLAN       [--dir DIR] [--in DIR] [--redo NAME]…
aw show PLAN [REF] [--dir DIR] [--in DIR] [--redo NAME]…

REF ::= <step>[.<field>]     the plan's own grammar, same code path as a load-time check
```

Both commands take all three flags; there is no flag legal on one and not the other.

**`aw show PLAN` with no REF is the dry run**: per-step fresh/stale plus the worst-case
bill. `--dry-run` is deleted because "a mode of run that does not run" always grows a
second identity-resolution path inside `run`; this way `run` *is* `show` plus executing
the stale frontier. `--redo` works on `show`, which is what makes "what would forcing
this step cost?" expressible.

Two honest limits on the preview: the bill is exact in **calls** but a range in dollars
(nobody knows output tokens before running), and everything past the first stale step is
`unknown` rather than `stale`, because a step's key depends on its upstream's resolved
output.

**`aw show PLAN REF` writes to stdout** — `aw show p.yaml fix.workspace | tar -x -C out/`.
No `--into`, because `--into` is the first flag of a family ending in
`--strip-components`, `--only`, `--list`: tar, reimplemented badly.

Exit `0` accepted · `1` **gate refused** · `2` usage/parse/validate · `3` mechanical ·
`130` interrupted. Unattended means something reads `$?`, and the distinction nothing
else gives you is *the panel refused* vs *the machine broke*.

---

## Worked example

```yaml
steps:
  audit:
    agent: claude/claude-opus-4-6
    prompt: Find the single worst correctness bug in the CSV parser. Do not fix it.
    inputs: {repo: in.workspace}
    outputs:
      bug:      string
      severity: [low, high]

  fix:
    agent: claude-ws/claude-sonnet-4-6      # ws: the editing posture, a word you typed
    prompt: Fix the bug named in `bug`. Keep the public API. Add one regression test.
    inputs:
      repo:     in.workspace
      bug:      audit.bug
      severity: audit.severity
    outputs: {summary: string}
    expect:
      - csv/parser.go
      - csv/parser_test.go
    gate:
      judges: [claude/opus-4-6, claude/sonnet-4-6, claude/haiku-4-5]
      criteria: |
        Accept only if the diff fixes the described bug, exported identifiers are
        unchanged, and the new test fails against the old parser.
      quorum: 3

  note:
    agent: claude/claude-haiku-4-5
    prompt: Write one changelog line for the accepted fix. Imperative mood.
    inputs: {what: fix.summary}
    outputs: {line: string}
```

```sh
aw show plan.yaml --in ~/src/csvtool          # what is stale, and the call count
aw run  plan.yaml --in ~/src/csvtool          # run; re-run == resume
aw run  plan.yaml --in ~/src/csvtool --redo fix
aw show plan.yaml note.line                   # read a committed value
aw show plan.yaml fix.workspace | tar -x -C out/
```

---

## Refused

Each is a parse error naming the alternative.

| refused | why |
|---|---|
| `if:` `when:` `loop:` `until:` | changes *which* steps run; a gate could silently not run |
| `map:` `foreach:` | deferred — §7 |
| `needs:` | use a data edge; then every edge is in the identity key |
| `session:` `context:` | §2 |
| `dir:` | `--in DIR`; keeps host paths out of the plan's identity |
| `timeout:` `attempts:` | execution policy — hardcoded, flags if ever asked |
| `files:` | `expect:`; the tree is the channel |
| `continue_on_error:` | fail-closed **is** the product |
| `{{ }}` `${{ }}` | scalars are bound as inputs, never substituted |
| `inputs:` under `gate:` | this is the gate-isolation rule |
| `outputs: {n: number}` | two type forms: `string` \| `[enum]` |
| a path-typed output (`x: ./file`) | see below |

**Considered and reverted.** Making `outputs:` path-typed so it absorbs `expect:` was the
biggest available cut (10 → 9 keys) and failed on three independent counts. It breaks
`Validate`'s totality — a path field is backend-produced and never appears in the model's
reply, so every workspace step dies on attempt 1. The fix is a `./` prefix test
re-derived in three places that must agree forever. And `git add -f -- ./` force-adds
everything `.gitignore` excludes, pulling `node_modules` into the store. It also moves a
**privilege** boundary into a value prefix: adding `repo: ./` would silently promote a
prompt-to-JSON call into a file-editing agent. `claude-ws` is the visible word for that,
and a posture that dangerous should be a word an author typed.

---

## Open questions

1. **Memoized impurity over time.** The key is a claim about the question, not the
   answer, so an accepted result from March is served against a July model — and
   *because it is a hit, the gate never re-runs*. Stance: record version + timestamp,
   warn on divergence; if it bites, `--max-age` as an input to the *lookup rule*, never a
   key component.
2. **Which criteria are actually judgeable.** "The diff must not delete tests" is
   enforceable; "the tool is correct" is not; both compile to the same YAML. The fix is a
   manual of worked criteria, not a feature.
3. **Sequential execution.** The jury is the only concurrency. Right for a first version;
   revisit `--jobs` with a measured number when a real plan is slow.
4. **`--in` is required for `aw show PLAN`** (pricing needs the input digest) but optional
   for `aw show PLAN REF` (reading a committed artifact must not need live host state).
   Documented rather than papered over.

---

## Delta

The code implements the **earlier 15-key surface**: `version:`, an `agents:` block with
`backend:`/`model:`, `steps:` as a sequence with `id:`, `steps.`-prefixed references,
`output:` singular, and `attempts:`.

Built and unchanged by this reduction: typed outputs with both forms and the load-time
reference check · inputs resolved by kind · `expect:` · the identity key · the
append-only journal · `--redo` · fail-closed gates with quorum and bounded repair ·
committing the panel-approved attempt by index · per-step scratch dirs · tree
capture/materialize · process-group timeouts · deterministic sorted folding · stable
prefixes.

To reach this spec: `steps:` as a map keyed by id · `agent: <backend>/<model>` · drop
`version:` and the `steps.` prefix · `output:` → `outputs:` · hardcode `attempts:` · cut
`needs:` · `aw show` replacing `--dry-run` and `--into` · the exit-code table.
