# Binary and Documentation Truth Design

**Issue:** GitHub #1, “Make SPEC.md and README.md true of the binary”

**Goal:** Make all 21 acceptance criteria true through test-first runtime, CLI, storage, platform, documentation, and coverage changes.

## Decisions

- Complete all 21 criteria in one coordinated effort, delivered as independently green TDD slices.
- A missing `expect:` path under a gate is a rejection that consumes an attempt, supplies precise repair feedback, and invokes no judge for that attempt.
- Keep the non-Unix lock as a no-op and document that one-run-per-state-directory locking is Unix-only.
- Keep `dawn.Backend` minimal. Model backend-specific behavior through narrow optional capability interfaces and validate capabilities before any invocation.
- Add no production dependency unless implementation proves one is unavoidable.

## Architecture

### Backend capabilities and preflight

Add an optional capability interface for a backend that can materialize workspace inputs. Continue using `dawn.TreeCapturer` to identify a backend that produces `workspace` and `diff` and honors `Invocation.Expect`.

Create one runner preflight path shared by `Run` and `Status`. Before invoking or pricing any step, it resolves the plan’s generator and judge backends and validates:

1. every `in.workspace` reference has a root supplied by `--in`;
2. a step with `expect:` uses a tree-capturing backend;
3. references to an upstream `.workspace` or `.diff` originate from a tree-capturing backend;
4. a step receiving a workspace ref uses a backend that can materialize it; and
5. every configured generator and judge backend resolves.

The CLI validates requested `--redo` names after loading the plan and before constructing stores or a runner. Shared runner preflight repeats empty/unknown redo validation so direct callers receive the same `ValidationError` protection.

These are author/configuration errors. The CLI wraps them as usage errors so both `run` and `show` exit 2. Preflight must complete before any backend invocation.

### Gated expected-path repair

The store exposes a typed error for a required path absent during capture. The workspace backend preserves that error type while adding context.

Gate generation distinguishes three outcomes:

- candidate produced successfully: validate and judge it;
- candidate rejected before judging: consume the attempt, preserve generated result bookkeeping, produce synthetic feedback, and invoke zero judges;
- mechanical error: abort without turning the failure into a verdict.

Only the typed missing-required-path error becomes a pre-judge rejection. Its feedback names the missing path and tells the next generator attempt to produce it. Schema failures, git failures, backend failures, and other capture errors remain mechanical.

The accepted result remains selected by gate attempt index, including attempts that were rejected before judging.

### Judge evidence

A judge must receive:

- the generator’s original prompt;
- resolved scalar inputs as rendered into that prompt;
- the complete validated output object;
- the gate criteria; and
- no implicit access to the generated workspace.

The criteria remain the judge system instruction. The judge prompt is a deterministic evidence document containing the resolved generator prompt and JSON output. Every judge sees identical bytes, and judges remain independent.

Delete `gate.FromResult`; the runner deliberately judges the whole output object and no caller needs a field-selecting helper.

### CLI behavior and errors

Validate `--redo` names after loading the plan and before opening or running paid work. Empty or unknown names are usage errors that name the invalid step.

Both `show PLAN` and `show PLAN REF` run the same `Status`/runner preflight as `run`, including missing-`--in` and backend-capability checks. Either show form therefore requires `--in` when the plan references `in.workspace`; neither requires it for a plan without that reference. `show PLAN REF` reports an uncommitted result as a usage error. Both usage banner lines and the command package documentation list all four flags: `--dir`, `--in`, `--redo`, and `--jobs`.

Test `execute` directly for flag parsing, positional/flag splitting, and all five usage-error families without calling a real agent. Keep production refactoring limited to explicit dependency or writer seams needed by those tests.

`dawn show PLAN step.workspace [--in DIR]` continues streaming a tar archive; `--in` is required when the plan references `in.workspace`. Test the real `store.Trees.Archive` output by reading it with Go’s tar reader.

### Deterministic tree capture

Every git subprocess used by `store.Trees` runs under a controlled configuration environment. System and user configuration cannot change capture semantics. Explicit settings disable line-ending conversion and global excludes while preserving the captured directory’s own `.gitignore` behavior.

Regression tests set hostile ambient `core.autocrlf=true` and `core.excludesFile` values and prove the resulting tree ref and contents match a clean capture.

Exercise archive behavior, executable-bit normalization, malformed refs, `Mem.Get`, and defensive copies. Cover filesystem write/sync/rename failure handling through the smallest test seam that can induce each branch deterministically; do not rely on platform-specific permission behavior.

### Kind vocabulary and documentation

`KindWorkspace` is the only kind the current binary emits in `Result.Produced`. Keep `KindValue`, `KindArtifact`, and `KindSession` as reserved, currently unemitted constants for API compatibility; scalar values live in `Result.Output`. Documentation must not describe an independent artifact channel that the binary cannot produce: expected paths may be files or non-empty directories, and are forced into a captured workspace. Existing empty directories are rejected as missing because Git cannot capture them.

Split the fused Claude backend documentation so `DefaultTimeout`, `defaultSystem`, and each exported backend field have accurate adjacent comments. Add argument-capture tests proving the text backend retains `--system-prompt` and the workspace backend retains `--exclude-dynamic-system-prompt-sections`.

Update SPEC.md and README.md so they:

- no longer list step concurrency as unbuilt;
- describe four flags, including `--jobs` for both commands;
- state the Unix-only lock exception;
- describe gated missing-`expect:` repair;
- describe what judges receive;
- accurately describe workspace/artifact and `Kind` behavior; and
- contain no claim contradicted by the changed binary.

### Platform and CI

Add CI that runs `go test ./...` and `go vet ./...` on the supported native environment and cross-compiles the non-Unix build surface with `GOOS=windows go build ./...`; do not cross-run tests. The non-Unix check proves build-tagged files continue compiling even though locking and process-group behavior have documented platform exceptions.

## Test-Driven Delivery Slices

1. **Preflight and usage:** failing CLI tests for unknown/empty redo and uncommitted refs, plus runner/CLI tests for missing `--in`, invalid producer/consumer capabilities, and shared `show`/`run` preflight; then minimal preflight and error classification.
2. **Gated `expect:` repair:** failing tests showing first-attempt missing paths currently abort and pay no repair; then typed capture errors and pre-judge rejection behavior.
3. **Judge evidence:** failing tests capture judge invocations and assert prompt, resolved inputs, output, and criteria; then deterministic evidence construction.
4. **Capture determinism:** failing hostile-config tests; then controlled git configuration.
5. **CLI, archive, backend-prefix, and store coverage:** add failing or mutation-sensitive tests before the smallest seams/implementations required.
6. **Platform and documents:** add non-Unix compile CI and reconcile every identified claim after runtime behavior is settled.

Each slice follows red-green-refactor. A behavior test must be observed failing for the expected reason before production code changes. Run focused package tests after each green step and `go test ./...` after each slice.

## Acceptance Mapping

| Issue criterion | Design coverage |
| --- | --- |
| Gated missing `expect:` repairs without judges | Gated expected-path repair |
| Judge sees prompt, inputs, outputs, criteria | Judge evidence |
| Invalid `.workspace`/`.diff` producer fails before invocation | Backend capabilities and preflight |
| Unknown/empty `--redo` exits non-zero | CLI behavior and errors |
| `show` missing `--in` fails | Shared preflight |
| Capture ignores personal git config | Deterministic tree capture |
| Non-materializing backend rejects workspace input | Backend capabilities and preflight |
| Uncommitted `show REF` exits 2 | CLI behavior and errors |
| Capability and missing-input checks exit 2 in both commands | Preflight plus CLI wrapping |
| Non-Unix lock promise is honest | Platform and CI; documentation |
| SPEC concurrency delta corrected | Kind vocabulary and documentation |
| README concurrency and flag count corrected | Kind vocabulary and documentation |
| Usage/package docs list `--jobs` for show | CLI behavior and errors |
| Kind/artifact vocabulary is truthful | Kind vocabulary and documentation |
| Claude comments attach correctly | Kind vocabulary and documentation |
| Real CLI coverage | CLI behavior and errors |
| Stable-prefix flags mutation-sensitive | Backend-prefix tests |
| Tar export path tested | CLI archive test and `Trees.Archive` test |
| Store uncovered branches exercised or deleted | Deterministic tree capture and store coverage |
| `gate.FromResult` used or deleted | Delete it |
| Non-Unix build compiled in CI or Unix-only support stated | Both compile check and documented exception |

## Verification

Before completion:

- run focused tests for every changed package;
- run `go test ./...`;
- run `go test -race ./...` because runner and jury code are concurrent;
- run the configured non-Unix compile command locally where possible;
- run `go vet ./...`;
- inspect coverage for `cmd/dawn` and `store` to confirm the named paths execute;
- review the final diff against every issue checkbox; and
- verify README, SPEC, package docs, and CLI usage agree exactly.
