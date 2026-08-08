# The dawn plan language

*dawn — Directed Agent Work Nodes.*

**Status: implemented.** See [Delta](#delta) for the few gaps that remain.

A plan is a static DAG of agent invocations. One mechanism — a **named input** —
carries every kind of state a step needs. No sessions, no templating, no control flow.

**10 keys · 2 type forms · 2 commands · 4 flags.**

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
- **Every invocation is bounded at 30m**, and the repair loop at 3 attempts. Both are
  the requirement (bounded), not a knob (tunable). Unattended plus no deadline means a
  hung tool call burns the night looking like slow work.
- **Repair is bounded at 3.** A bounded loop is the requirement; a tunable one is not.
  `attempts` is already excluded from the identity key — a result accepted under 3
  attempts is equally accepted under 5, which is policy — so fixing it changes no cache
  behavior and makes the cost preview exact.
- **A judge sees** the gate criteria as its system instruction and deterministic
  evidence containing the resolved generator prompt (rendered scalar inputs included)
  plus the complete validated output object. Workspace refs and the generator
  transcript are not included, and neither is the **repair critique** — that is the
  panel's own prior verdicts, attributed by judge name, so feeding it back would let
  round 2 read round 1 and make the panel dependent on itself in exactly the way that
  fails silently green. The generator's prompt grows across repair rounds; the judge's
  evidence does not, apart from the candidate. Every judge receives identical bytes in
  a fresh context against an engine-fixed verdict schema.

  **Both routes, not one.** Cleaning the prompt alone left the OUTPUT map carrying the
  same bytes: `diff` is reserved and computed by dawn from the whole tree delta, so an
  agent doing the most ordinary thing there is — writing down what it was asked to fix —
  put round 1's verdicts into round 2's evidence. dawn strips its own critique from the
  rendered evidence, verbatim, because those are strings dawn produced and can recognize.
  The committed result keeps the real diff: what the store records is state, what the
  panel reads is evidence. Scoped honestly — a generator that PARAPHRASES an objection is
  not a channel dawn opened, and no string match could close it. The guarantee is that
  dawn never carries a verdict back to the panel, not that no judge could ever infer that
  an earlier round happened.
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

**`--jobs N` runs independent steps at once, and needs no key.** The DAG already says
which steps are independent — `left` and `right` both reading `root.text` have no edge
between them — so concurrency is a property of the graph the author already wrote, and a
flag is the whole implementation. Nothing about a plan's meaning changes: `--jobs` moves
*when* work happens, never *what* is computed or reused.

Ordering is still the graph's. `--jobs 1` is the default and reproduces the sequential
order exactly, not merely some valid topological one, so two runs stay diffable.

Two honest edges. A failure stops the run from **launching** anything further but does not
cancel what is in flight — that work is already paid for, and a step that commits is a step
the next run skips. And the error reported is the one earliest in topological order, not
the first to arrive, so a broken plan blames the same step every time.

`--jobs` counts STEPS, and a gate multiplies it: `--jobs 4` over steps with three judges
each is up to sixteen agent processes at once. Provider rate limits are the real ceiling,
not cores.

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

**No `cache:` knob.** dawn owns ordering and reports `cache_read`/`cache_create` per step
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

**Load time:** the reference is exactly `<step>.<field>`; the step exists; the field is
declared upstream or reserved; the graph is acyclic; judges are well-formed; and an
explicit quorum ∈ 1..N.

**CLI validation**, after loading the plan but before constructing stores or a runner,
rejects empty or unknown `--redo` names.

**Runner preflight**, after concrete backends are available and before any invocation or
token spend: every generator and judge resolves; `expect:` and reserved
`workspace`/`diff` producers capture trees; workspace consumers can materialize them;
**a workspace consumer HAS a workspace input**; any referenced `in.workspace` has a root
supplied by `--in`; and every `Runner.Redo` key names a real, non-empty step id. The CLI
also validates `--redo` early, before constructing stores, while shared Runner preflight
protects direct callers.

That fourth check is the converse of the third, and it was the one missing. `expect:`
needs a tree capturer; a tree EDITOR needs a tree. The workspace input *is* the working
directory, so a `claude-ws` step that declares none cannot run — decidable from the plan
text alone, with no live agent required — yet it priced at exit 0 and then died at
runtime, after every upstream had been paid for. Implementing `WorkspaceMaterializer` is
therefore a **requirement**, not merely a capability, and that is what the interface now
says.

**Runtime**, after the agent returns and *before* the step commits: strict-parse, every
declared field present, enum values members, **no undeclared fields**. Any failure
rejects the output whole — a stray key means the model improvised.

**Order of operations, non-negotiable:**
```
invoke → capture tree + assert expect → validate → jury → commit
```
A non-conforming candidate never reaches a judge. A missing expected path under a gate
consumes an attempt, supplies path-specific repair feedback, and spends zero judge
tokens for that attempt; ungated it fails the step. Other capture errors are mechanical.

A **schema violation** is a third thing: not a crash (the process exited 0), not a
verdict (no judge ran). The step fails; the retry is `dawn run` again, which re-pays one
step rather than the run.

---

## 4 · Artifacts

The workspace tree is the only current file/artifact channel, and **producer path equals
consumer path** — `dist/dawn` written by `build` is read at `dist/dawn` by `smoke`.
Expected paths are files or non-empty directories forced into that captured workspace,
not independent `artifact` refs. Existing empty directories are missing postconditions
because Git cannot capture them. The public `KindValue`, `KindArtifact`, and `KindSession`
names remain reserved for future backends; the current binary emits none in
`Result.Produced`. Scalar values live in `Result.Output`. Every tree-capturing step gets
its own scratch dir: materialize inputs → run → capture → discard. The supplied host tree
is captured first; agents work only in the scratch copy.

`expect:` is a postcondition, and two lines of git give both halves:

```
git add -A                  # everything; .gitignore honored
git add -f -- <expect…>     # forced past .gitignore, and errors if never produced
```

Every git subprocess uses a controlled configuration environment. System and personal
configuration — `core.autocrlf`, `core.excludesFile`, `core.attributesFile`, ambient
`GIT_CONFIG_*` overrides, and the system config and attributes files — cannot alter the
captured bytes. The workspace's own `.gitignore` remains in force.

`core.attributesFile` is pinned by name rather than left to `GIT_ATTR_NOSYSTEM`, which
covers only the SYSTEM attributes file. Git resolves the personal one from
`$XDG_CONFIG_HOME/git/attributes` on a path of its own, so `GIT_CONFIG_GLOBAL=/dev/null`
does not suppress it — neutering global config makes git fall back to that default path
rather than to nothing. Measured, and not what it first looked like: `*.txt eol=crlf
text` in a personal attributes file leaves the captured **ref identical** and changes
what **materialize** writes back out. So the identity key was never at risk; what broke
is quieter — one ref hands two machines different bytes, and *materialize-then-capture is
the identity* stops holding, which means an agent edits a file dawn did not commit.

**An ambient `GIT_TEMPLATE_DIR` is the third channel, and the one that outlives its
environment.** `git init` copies a template's `info/exclude` and `info/attributes` into
the new repository, and `$GIT_DIR/info/exclude` is honored by every later `git add -A` —
so a variable set once, before the store existed, silently changed which files every
future capture contained. The store is created with `--template=` (empty); unsetting the
variable is not enough, because git falls back to a system template directory.

Verified: with `dist/` ignored, plain `add -A` yields a tree where `dist/dawn`
**does not exist** — the flagship artifact silently absent.

**A capture is taken against the input tree as its baseline, so the filter applies only
to files that APPEARED during the step.** Without that, `.gitignore` is re-applied from
scratch at every hop and an artifact survives exactly one: `build` declares `dist/dawn`
and forces it in, `smoke` receives the tree and has no reason to re-declare another
step's artifact, and `smoke`'s output tree silently loses it. Measured before the
baseline existed: capture → materialize → capture returned a *different* ref with the
binary gone. So **materialize-then-capture is the identity** — a tree that changed sha by
being written to disk and read back would not be content-addressed at all. What the
agent newly creates under an ignored path (a `node_modules`, a build cache) is still
untracked, still ignored, still out; and a file the agent deletes stays deleted, because
a baseline is a starting point, not a floor.

**A missed `expect:` path is a rejection, not a crash.** Under a gate it feeds repair
with "you did not produce X" and consumes an attempt at **zero judge tokens**; ungated,
it fails the step. Every other capture error stays mechanical.

**Missed means "not in the tree", and the index is what is asked.** `expect:` is a claim
about the artifact dawn commits, so it is settled by what git staged and never by
`os.Lstat`, which answers a different question and gets it wrong in both directions. It
says *present* for entries git will not store — a FIFO or a socket stats fine, stages
nothing, and the capture used to **succeed with the declared path absent from the tree**,
a false pass, which is worse than an abort. And it says nothing about paths that fail
inside `git add`: one behind a symlinked directory (`dist -> build`) resolves happily and
is refused as "beyond a symbolic link", as is one inside an embedded repo or one the
agent left unreadable. All of those escaped as mechanical aborts on attempt 1. Declared
paths are added one at a time so the error names the right one, and an empty directory is
still missing because git has no empty directories to stage.

**`expect:` paths are normalized, not policed for tidiness.** `./dist/out`, `dist//out`
and `dist/out/` all name `dist/out`, so they load and collapse to one identity key.
Refused instead are the paths that are not the same question: absolute, volume-qualified,
backslashed, control-charactered, and anything that still escapes the workspace after
cleaning — `dist/../..` is only visibly an escape once it collapses.

**An embedded git repository fails the capture.** A directory carrying its own `.git` — a
vendored dependency, a clone the agent made — is recorded by git as a *commit reference*,
not files, and that commit lives in the nested repo rather than dawn's store. Reproduced:
a tree holding `160000 commit 5f9bf40b… vendor/lib` materialized as `[main.go]`, with
`vendor/lib` not empty but **absent**, and no error anywhere. Refused rather than
repaired, because git will not descend into an embedded repo and capturing its `HEAD`
instead would silently substitute its last commit for its working state. A tree that
cannot round-trip is not a workspace. Ignore the path, or remove the nested `.git`; a
nested repo under an already-ignored path was never staged and is not affected.

**Every captured tree is pinned behind a ref.** `git write-tree` writes objects that
nothing points at, so a store with no refs is one where every committed workspace is
unreachable garbage by git's own definition — `git gc --prune=now` inside `.dawn/trees`
deleted the lot, reproduced as `fatal: failed to unpack tree object`. dawn never runs
gc, but an artifact that survives only until someone runs a standard maintenance command
in the state directory is not durable. A ref points straight at the tree; no wrapper
commit, which would carry a timestamp into something whose identity must stay its
content.

The host tree enters via `--in DIR`, never a key, so no machine-specific path lives in
the file whose bytes are the plan's identity.

> **The panel judges a rendering, not bytes.** `git diff` renders a changed 200KB binary
> as one sentence. To gate an artifact's *content*, declare a text rendering of it as an
> output field and write the criteria against that.

---

## 5 · Rewind and redo

**Resume is deleted as a concept.** `dawn run` computes each key in topological order and
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

## 8 · Isolation

Two agents editing at once must not touch each other's files. That is the whole
requirement: **mutual non-interference, not containment.** An agent that writes an
absolute path outside its directory is not prevented and is not meant to be. The threat
model is *two of my own steps trip over each other*, never *this step is hostile*.

**The mechanism is a distinct path, and dawn already has one.** Every invocation of a
tree-capturing step gets a fresh `os.MkdirTemp` — mode 0700, random suffix — materialized
from an immutable tree, used as the agent's working directory, and deleted on return.
Per invocation, so per gate attempt. Two steps cannot collide on a path neither can name.

> **Isolation is allocation, not enforcement.** Disjoint paths *are* non-interference. A
> kernel policy enforcing a property the allocator already guarantees buys nothing.

Nothing mutable is shared: inputs are content-addressed trees, the git index is a fresh
temp file per capture, blobs are fsync-then-rename, the journal is one `O_APPEND` line.
This is strictly stronger than the 2026 consensus primitive. A `git worktree` shares the
parent `.git`; git is a single writer, and concurrent agents lose work to `index.lock`.
dawn shares no index, no refs, no working tree — only an immutable object store.

**`cmd.Dir` is also the entire caller-side sandbox API.** Every agent CLI that confines
writes anchors that confinement on the process working directory — claude on cwd, codex
on `--cd`, droid on CWD, cursor on the workspace. There is no *declare my workspace* call
to make. Where an operator has enabled their CLI's own sandbox, dawn opts into it for
free; where they have not, dawn does not pretend otherwise.

**What an author writes: nothing.** There is no isolation key and no flag. The one
language consequence is a refusal that makes an existing promise true: **two `workspace`
inputs on one step is a load error.** The workspace input *is* the working directory, so
a second makes the cwd a coin flip — measured 173/27 over 200 resolutions, a skew that
passes every hand test and flips overnight.

**Platform.** Identical on macOS and Linux. No build tags, no syscalls, no fallback
branch, because there is nothing to fall back from. Scratch lands under `$TMPDIR`; where
`/tmp` is tmpfs, set `TMPDIR` to keep a large tree out of RAM. A run killed with
`SIGKILL` leaks its scratch dir, and `rm -rf "$TMPDIR"/dawn-ws-*` is the cleanup.

**Not guaranteed, plainly.** Escape is possible and needs no exploit. dawn passes
`--dangerously-skip-permissions`; an absolute path, a `cd ..`, or any spawned subprocess
writes wherever the uid can. No sandbox dawn could add would change that: an agent CLI's
own file-edit tools are documented as bypassing its own sandbox, and its model is
instructed to retry a blocked command with the sandbox disabled. Two further gaps are
named rather than papered over: an **absolute symlink** captured into a tree is
re-materialized into every downstream workspace and is writable through it; and ambient
state outside the workspace — `~/.claude`, `~/.npm`, `~/.cargo`, ports — is shared by
every step. Neither is a filesystem-isolation problem and neither has an
isolation-shaped fix.

### Refused

| refused | why |
|---|---|
| Seatbelt / bubblewrap / Landlock wrapper | containment, not the requirement. Measured: Seatbelt does not nest — a second profile is denied `sandbox_apply: Operation not permitted` even when strictly stricter, while a byte-identical one succeeds. Wrapping a CLI that sandboxes itself makes it *less* isolated |
| the writable-path allowlist such a sandbox needs | it must include `$TMPDIR` for the CLI to run at all, and every workspace is a direct child of `$TMPDIR` — so the profile permits exactly the sibling writes it exists to deny |
| `git worktree` | the consensus choice, and wrong here: it needs a shared mutable `.git`, puts a writable handle to the store inside the agent's cwd, and costs a full checkout. dawn's store is bare and its scratch is disposable |
| hardlink trees / reflink / clonefile / overlayfs | copy is ~0.1% of a step's 30m bound, so there is nothing to optimize; each is one platform only, or a second dependency. Hardlinks additionally corrupt the original through the link |
| containers | out of scope: single host, single static binary |
| per-step `$HOME` / `CLAUDE_CONFIG_DIR` | the one genuinely uncovered vector, and it is env vars rather than a sandbox. Deferred because relocating `HOME` logs the agent out, which is where its credentials live |
| `confine:` `sandbox:` `dir:` keys | a plan key whose meaning depends on the host OS and kernel version. `--in DIR` already keeps host paths out of the plan's identity |

---

## The CLI

```
dawn run  PLAN       [--dir DIR] [--in DIR] [--redo NAME]… [--jobs N]
dawn show PLAN [REF] [--dir DIR] [--in DIR] [--redo NAME]… [--jobs N]

REF ::= <step>[.<field>]     the plan's own grammar, same code path as a load-time check
```

Both commands take all four flags; there is no flag legal on one and not the other.
`--jobs` is inert on `show`, which executes nothing — accepted rather than special-cased,
because a flag that is legal here and illegal there is a rule to remember. It must be at
least 1: a silently-corrected `0` is how `quorum: 0` once became a majority.

**`dawn show PLAN` with no REF is the dry run**: per-step fresh/stale plus the worst-case
bill. `--dry-run` is deleted because "a mode of run that does not run" always grows a
second identity-resolution path inside `run`; this way `run` *is* `show` plus executing
the stale frontier. `--redo` works on `show`, which is what makes "what would forcing
this step cost?" expressible.

Two honest limits on the preview: the bill is exact in **calls** but a range in dollars
(nobody knows output tokens before running), and everything past the first stale step is
`unknown` rather than `stale`, because a step's key depends on its upstream's resolved
output.

**`dawn show PLAN REF` writes to stdout** —
`dawn show p.yaml fix.workspace --in src | tar -x -C out/`. It resolves whether the
requested step is committed through `Runner.Status`, so the shared runner preflight still
requires `--in` when any plan input references `in.workspace`. No `--into`, because
`--into` is the first flag of a family ending in `--strip-components`, `--only`, `--list`:
tar, reimplemented badly.

Exit `0` accepted · `1` **gate refused** · `2` usage/parse/validate · `3` mechanical ·
`130` interrupted. Unattended means something reads `$?`, and the distinction nothing
else gives you is *the panel refused* vs *the machine broke*.

**SIGINT and SIGTERM cancel the run.** On Unix each agent is placed in its own process
group and cancellation kills the whole group, including tool subprocesses. On non-Unix
platforms cancellation kills only the direct child. On every platform a 5s `WaitDelay`
bounds waiting for inherited pipes; process-group reaping is a Unix-only guarantee.

An interrupt is **not a verdict**. Cancelling mid-gate cancels every judge at once, which
is indistinguishable from a unanimous no by vote count alone; the judges' errors make the
round mechanical, so an interrupt never records a rejection and never spends a repair
attempt. It exits `130` even though the underlying error is mechanical, because *the
operator stopped it* and *the machine broke* are different facts.

**On Unix, one `run` per state directory is enforced by a non-blocking `flock`.** Two
runs against one `--dir` corrupt nothing — journal lines are atomic appends and blobs are
content-addressed — but they can both miss the same key, execute it, and pay. The second
Unix run is refused rather than queued, and the kernel releases the lock when its process
dies. `dawn show` never locks.

**"Unix" here means "has `syscall.Flock`", which is not the same set as Go's `unix` build
tag.** That tag includes **solaris** and **aix**, and neither provides `Flock` — so
`//go:build unix` did not mean "the lock works here", it meant the `plan` package did not
compile there at all, and a CI job that cross-built only Windows compiled one side of one
split and reported the tags covered. The lock is built for `unix && !solaris && !aix`;
everywhere else — solaris, aix, Windows, plan9, js — the implementation is a no-op, the
binary still builds, and concurrent runs against one state directory are unguarded: both
miss the same key, both execute it, both pay. CI cross-builds and vets every platform
whose build-tagged file differs, which is the only way this stays true.

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
dawn show plan.yaml --in ~/src/csvtool          # what is stale, and the call count
dawn run  plan.yaml --in ~/src/csvtool          # run; re-run == resume
dawn run  plan.yaml --in ~/src/csvtool --redo fix
dawn show plan.yaml note.line --in ~/src/csvtool  # read a committed value
dawn show plan.yaml fix.workspace --in ~/src/csvtool | tar -x -C out/
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
3. ~~**Sequential execution.**~~ Answered: `--jobs N`. It changed no keys, because the DAG
   already said which steps are independent — the flag only stopped ignoring it.
4. ~~**`--in` is optional for `dawn show PLAN REF`.**~~ Answered, and not the way the
   promise was written. A step's key includes its resolved inputs, so a step that reads
   `in.workspace` cannot be identified at all without the input digest — reading it back
   was never independent of live host state, in any version. What IS achievable is the
   narrower thing: a step that does not depend on the root reads back with no `--in`,
   even in a plan whose other branches do. `Runner.Committed` therefore skips the root
   requirement that `Run` and `dawn show PLAN` enforce, and a step that genuinely needs
   the root still names the missing flag — but ONLY that step. Asking merely "is there a
   root?" blamed any plan with a root-dependent branch anywhere, so reading an unrelated
   step reported a different step's missing `--in`, advice that does not help because
   supplying it then produces a third error. The question is whether THIS step depends on
   the root, transitively; if it does not, the truthful answer is that it has not run.
5. **The store only grows.** Pinning is per capture, including gate attempts nobody
   accepted, so `git gc` now reclaims nothing. That is the deliberate side of the trade —
   unbounded growth is a disk problem with an obvious fix, silent deletion of committed
   state is not — but it is only half an answer. The other half is a prune, and the pins
   are what make one expressible: a ref no journal line names is collectable, which is
   not a question you can even ask of a dangling object. Stance: wait for a real store to
   get big, then `dawn prune` reads the journal, deletes unnamed refs and runs gc.

---

## Delta

The code implements this spec. `dawn run` and `dawn show` work end to end: typed
outputs with load-time reference checking, inputs resolved by field name, `expect:`,
gates with quorum and bounded repair, the identity key, the append-only journal,
`--redo`, `--in`, `--jobs` step concurrency, per-step scratch dirs, deterministic tree
capture and materialize, stable prefixes, Unix process-group cancellation, and the
exit-code table. Run locking and process-group cancellation have the platform exceptions
documented in the CLI section.

Not yet: more backends than `claude` and `claude-ws` (codex, an HTTP LLM). They slot
behind existing seams.

Honest gaps in what is built, none of them language-level:
- `dawn show PLAN` prices a run in **calls**, not dollars, and everything past the
  first stale step reads `unknown` — a step's key depends on its upstream's
  resolved output, which is the price of early cutoff.
- The 128KB evidence cap and the diff rendering a binary as one sentence are
  described in §4 but not yet implemented; a panel currently sees the raw diff.
- Nothing yet warns when a hit's recorded agent version differs from the current
  one (open question 1).
