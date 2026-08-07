# Binary and Documentation Truth Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make every runtime and documentation promise named by GitHub issue #1 true and guard all 21 acceptance criteria with tests or compile checks.

**Architecture:** Keep `dawn.Backend` minimal and describe tree behavior with optional capabilities. Run one capability-aware preflight from both execution and status paths, model missing expected paths as typed pre-judge rejections, isolate git from personal configuration, and expose only the narrow seams required for deterministic CLI/store tests.

**Tech Stack:** Go standard library, `gopkg.in/yaml.v3`, git CLI, GitHub Actions.

## Global Constraints

- Complete all 21 criteria in GitHub issue #1.
- Follow red-green-refactor: observe each new behavior test fail for the intended reason before changing production code.
- A gated missing `expect:` path consumes an attempt, provides repair feedback, and pays zero judges for that attempt.
- All author/configuration errors fail before any backend invocation and exit 2 through the CLI.
- Do not widen the required `dawn.Backend` interface.
- Add no production dependency.
- Keep the non-Unix lock a documented no-op and compile its build-tagged surface in CI.
- Preserve deterministic ordering and stable prompt prefixes.

---

### Task 1: Capability-aware plan preflight

**Files:**
- Modify: `dawn.go:53-61`
- Modify: `backend/claude/workspace.go:194-200`
- Modify: `plan/run.go:63-94,535-586`
- Test: `plan/run_test.go`

**Interfaces:**
- Produces: `type WorkspaceMaterializer interface { Backend; MaterializesWorkspace() }`
- Produces: `type ValidationError struct { Err error }` with `Error` and `Unwrap` methods.
- Produces: `func (r *Runner) preflight(p *Plan) error`
- Consumes: existing `TreeCapturer`, `Runner.Backend`, `Step.Inputs`, and reserved fields.

- [ ] **Step 1: Add failing producer and consumer capability tests**

Add a text-only counting fake and two marker fakes to `plan/run_test.go`:

```go
type treeProducer struct{ producer }
func (treeProducer) CapturesTree() {}

type workspaceConsumer struct{ producer }
func (workspaceConsumer) MaterializesWorkspace() {}
```

Add `TestPreflightRejectsReservedRefsFromNonTreeBackend` with an upstream `x/text`, downstream input `repo: upstream.workspace`, and a call counter. Assert the error contains `upstream`, `workspace`, and `captures no tree`, and that no backend was invoked.

Add the same assertion for `upstream.diff`.

Add `TestPreflightRejectsWorkspaceInputForNonMaterializingBackend` with a tree-producing upstream and text-only downstream. Assert the error names the downstream input and says the backend cannot materialize a workspace; assert zero invocations.

Add `TestPreflightAcceptsTreeProducerAndWorkspaceConsumer` using the two marker fakes and assert the two-step run succeeds.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test ./plan -run 'TestPreflight(RejectsReservedRefs|RejectsWorkspaceInput|AcceptsTree)' -count=1
```

Expected: invalid plans invoke at least one backend or fail later during binding; the valid materializer marker does not exist yet.

- [ ] **Step 3: Add the optional materialization capability and preflight**

In `dawn.go` add:

```go
type WorkspaceMaterializer interface {
	Backend
	MaterializesWorkspace()
}
```

In `backend/claude/workspace.go` add:

```go
func (Workspace) MaterializesWorkspace() {}
```

and its compile-time assertion.

In `plan/run.go`, extract the existing root/expect loop into `preflight`. Resolve each step backend once during preflight. For every input, parse its source and field. If the field is `workspace` or `diff` and the source is not `in`, resolve the source backend and require `dawn.TreeCapturer`. If the field is `workspace`, require the consuming backend to implement `dawn.WorkspaceMaterializer`. Keep `in.workspace` dependent on `Runner.Root`. Resolve every gate judge backend during this pass as well.

Wrap every preflight failure in `&ValidationError{Err: err}` so callers can distinguish author/configuration errors from journal, store, and invocation failures. Call `preflight` at the start of both `Run` and `Status`, after graph ordering but before journal lookup or invocation.

- [ ] **Step 4: Run focused and package tests for GREEN**

```bash
go test ./plan -run 'TestPreflight|TestExpectRequires|TestRootStep' -count=1
go test ./plan -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the capability slice**

```bash
git add dawn.go backend/claude/workspace.go plan/run.go plan/run_test.go
git commit -m "fix(plan): reject impossible tree flows before invocation"
```

---

### Task 2: CLI usage validation and shared show behavior

**Files:**
- Modify: `cmd/dawn/main.go:1-16,101-172,223-250`
- Modify: `plan/run.go:68-94,535-603`
- Test: `cmd/dawn/main_test.go`
- Test: `plan/run_test.go`

**Interfaces:**
- Produces: `func validateRedo(p *plan.Plan, names map[string]bool) error`
- Produces: usage-wrapped preflight errors from `execute` for both commands.
- Consumes: `Runner.Status`, `Runner.Committed`, and `usageError`.

- [ ] **Step 1: Add failing CLI tests for flags, splitting, and five usage families**

Add table tests for `split` showing each value-bearing flag remains attached while positionals work before, between, and after flags:

```go
func TestSplitPositionalsAndFlags(t *testing.T) {
	pos, flags := split([]string{"--jobs", "2", "plan.yaml", "step.text", "--redo=draft"})
	if diff := cmpSlices(pos, []string{"plan.yaml", "step.text"}); diff != "" { t.Fatal(diff) }
	if diff := cmpSlices(flags, []string{"--jobs", "2", "--redo=draft"}); diff != "" { t.Fatal(diff) }
}
```

Use a local slice comparison helper rather than a new dependency.

Add direct `execute` cases asserting `errors.As(err, new(*usageError))` for:

1. missing plan;
2. unexpected positional argument;
3. malformed/unknown flag or `--jobs 0`;
4. unknown and empty `--redo` names; and
5. `show PLAN REF` where the step has no committed result.

Create plans in `t.TempDir()` with `os.WriteFile`; pass `--dir` pointing at another temp directory. Assert unknown redo names appear in the error.

Add a CLI-level `show` plan containing `in.workspace` with no `--in`. Assert usage classification and that the error names `--in`.

- [ ] **Step 2: Add a failing status preflight regression**

In `plan/run_test.go`, add `TestStatusRunsTheSamePreflightAsRun`: call `r.Status` on a plan requiring `in.workspace` and on a plan whose upstream text backend is referenced as `.workspace`. Assert both fail before any backend invocation.

- [ ] **Step 3: Run focused tests and verify RED**

```bash
go test ./cmd/dawn -run 'TestSplit|TestExecute|TestShow' -count=1
go test ./plan -run TestStatusRunsTheSamePreflightAsRun -count=1
```

Expected: unknown redo is silently accepted; uncommitted show ref is mechanical; missing `--in` status either reports unknown or is not usage-wrapped.

- [ ] **Step 4: Implement validation and usage classification**

Add:

```go
func validateRedo(p *plan.Plan, redo map[string]bool) error {
	for name := range redo {
		if name == "" { return usagef("--redo needs a step name") }
		if _, ok := p.Steps[name]; !ok { return usagef("--redo names unknown step %q", name) }
	}
	return nil
}
```

Call it immediately after `plan.Load`. Extend `exitCode` so `errors.As(err, new(*plan.ValidationError))` maps to `exitUsage`. Let `Run` and `Status` surface that typed error directly; do not wrap all `Status` errors, because journal/store corruption must remain mechanical. Avoid duplicate status work in `show` by keeping `showPlan` as its sole status call.

Change the uncommitted branch in `showRef` to `usagef`. Ensure capability and missing-root errors from both commands are usage errors without reclassifying store/backend invocation failures.

Add `[--jobs N]` to the `show` package-doc and usage-banner lines.

- [ ] **Step 5: Run focused and package tests for GREEN**

```bash
go test ./cmd/dawn -count=1
go test ./plan -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the CLI usage slice**

```bash
git add cmd/dawn/main.go cmd/dawn/main_test.go plan/run.go plan/run_test.go
git commit -m "fix(cli): fail invalid plans and redo requests as usage errors"
```

---

### Task 3: Gated missing-path repair without judge payment

**Files:**
- Modify: `store/git.go:53-118`
- Modify: `gate/repair.go:11-76,89-100`
- Modify: `plan/run.go:394-464`
- Test: `store/git_test.go:229-243`
- Test: `gate/repair_test.go`
- Test: `plan/run_test.go`

**Interfaces:**
- Produces: `type MissingPathError struct { Path string }` in `store`.
- Produces: `Candidate.Rejection string`; non-empty means consume this attempt and skip `Jury`.
- Consumes: `errors.As` in the plan runner.

- [ ] **Step 1: Tighten the store test around a typed missing-path error**

Extend `TestCaptureFailsOnAMissingDeclaredPath`:

```go
var missing *MissingPathError
if !errors.As(err, &missing) { t.Fatalf("error type = %T, want *MissingPathError", err) }
if missing.Path != "dist/never-built" { t.Fatalf("Path = %q", missing.Path) }
```

- [ ] **Step 2: Add failing gate and runner repair tests**

In `gate/repair_test.go`, add `TestGateConsumesPreJudgeRejectionWithoutCallingJudges`. The generator returns `Candidate{Rejection: "you did not produce dist/dawn"}` first and `Text("good")` second. Use judges with an atomic call counter. Assert `Attempts == 2`, only one round of judges ran, and second-generation feedback contains the path.

In `plan/run_test.go`, add a generator fake that returns `&store.MissingPathError{Path: "dist/dawn"}` on attempt one and a valid result on attempt two. It implements `TreeCapturer`. Add counting judges. Assert the run succeeds, generator calls equal 2, judge calls equal one panel round, and the second prompt contains `dist/dawn`.

Add a neighboring test where the generator returns an ordinary capture error; assert it remains mechanical, consumes no repair attempt, and invokes no judge.

- [ ] **Step 3: Run tests and verify RED**

```bash
go test ./store -run TestCaptureFailsOnAMissingDeclaredPath -count=1
go test ./gate -run TestGateConsumesPreJudgeRejection -count=1
go test ./plan -run 'TestGatedMissingExpect|TestGatedCaptureError' -count=1
```

Expected: typed assertion fails; gate has no pre-judge rejection state; runner aborts on first miss.

- [ ] **Step 4: Implement typed store errors and pre-judge rejection**

Before forcing paths, check each required path with `os.Lstat(filepath.Join(workDir, filepath.FromSlash(path)))`. Return `&MissingPathError{Path: path}` for `os.IsNotExist`; preserve other stat errors. Define:

```go
type MissingPathError struct{ Path string }
func (e *MissingPathError) Error() string { return fmt.Sprintf("declared path %q was not produced", e.Path) }
```

Keep `git add -f --` after this check so ignored paths are still forced.

Add `Rejection string` to `gate.Candidate`. In `Gate`, after generation and before `Jury`, when `candidate.Rejection != ""`, set an unapproved outcome for this attempt, set feedback through a helper that starts with the standard rejection heading and includes the synthetic reason, then continue. Do not call judges.

In `runGated`, use `errors.As(err, &missing)` to append a zero-value placeholder to `generated` and return `gate.Candidate{Rejection: "you did not produce " + missing.Path}`. Append the real result on successful generations. The placeholder keeps `generated[out.Attempts-1]` aligned with gate attempt numbers but can never be selected because its attempt was rejected; retain the existing accepted-attempt bounds check.

- [ ] **Step 5: Run focused and full package tests for GREEN**

```bash
go test ./store ./gate ./plan -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the repair slice**

```bash
git add store/git.go store/git_test.go gate/repair.go gate/repair_test.go plan/run.go plan/run_test.go
git commit -m "fix(gate): repair missing expected paths before judging"
```

---

### Task 4: Complete deterministic judge evidence

**Files:**
- Modify: `gate/gate.go:46-96`
- Modify: `gate/repair.go:24-29`
- Modify: `plan/run.go:415-449`
- Test: `gate/gate_test.go`
- Test: `plan/run_test.go`

**Interfaces:**
- Produces: `func judgeEvidence(prompt string, output map[string]any) (string, error)` in `plan`.
- Removes: `gate.FromResult`.
- Consumes: existing `Judge(system, candidate)` contract; criteria remain `System`.

- [ ] **Step 1: Add a failing runner evidence test**

Create a recording judge fake that stores each invocation and approves. Build a two-step plan where the gated second step has prompt `Review the draft`, scalar input `draft: first.text`, output `summary: ok`, and criteria `Approve concise summaries`.

After `Run`, assert the judge invocation:

```go
if got.System != "Approve concise summaries" { ... }
for _, want := range []string{
	"Review the draft",
	"--- input: draft ---",
	`"summary": "ok"`,
} {
	if !strings.Contains(got.Prompt, want) { ... }
}
```

Assert no workspace URI is rendered into judge evidence.

- [ ] **Step 2: Add stable-evidence ordering coverage**

Run the same gated setup repeatedly with multi-field output maps and assert the captured judge prompt bytes are identical. JSON indentation provides sorted map keys; this test guards that property.

- [ ] **Step 3: Run focused tests and verify RED**

```bash
go test ./plan -run 'TestJudgeReceives|TestJudgeEvidenceIsStable' -count=1
```

Expected: judge sees only the output object, not the generator prompt and resolved scalar inputs.

- [ ] **Step 4: Implement evidence construction and remove dead helper**

Add:

```go
func judgeEvidence(prompt string, output map[string]any) (string, error) {
	body, err := json.MarshalIndent(output, "", "  ")
	if err != nil { return "", err }
	return "Generator request:\n" + prompt + "\n\nCaptured output:\n" + string(body), nil
}
```

Use the attempt’s resolved prompt, including appended repair feedback where present, plus its validated output. Continue passing `g.Criteria` as `system` to `gate.Gate`. Delete `gate.FromResult` and its comment.

- [ ] **Step 5: Run gate and plan tests for GREEN**

```bash
go test ./gate ./plan -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the judge-evidence slice**

```bash
git add gate/gate.go gate/repair.go plan/run.go gate/gate_test.go plan/run_test.go
git commit -m "fix(gate): give judges the complete generation context"
```

---

### Task 5: Isolate tree capture from personal git configuration

**Files:**
- Modify: `store/git.go:250-275`
- Test: `store/git_test.go`

**Interfaces:**
- Produces: controlled git environment from `(*Trees).env`.
- Consumes: repository-local `.gitignore`; ignores system/global config and global excludes.

- [ ] **Step 1: Add hostile autocrlf and excludes tests**

Add `TestCaptureIgnoresPersonalAutocrlf`:

1. create `clean` and `hostile` directories with identical `a.txt` bytes containing `\r\n`;
2. capture `clean` under the ordinary environment;
3. write a temporary global config containing `[core]\n\tautocrlf = true\n`;
4. set `GIT_CONFIG_GLOBAL` to it for the hostile capture;
5. assert refs are identical and materialized bytes remain `\r\n`.

Add `TestCaptureIgnoresPersonalExcludesFile`:

1. write identical `keep.txt` in two directories;
2. create a global excludes file containing `keep.txt` and a global config pointing `core.excludesFile` to it;
3. capture under that hostile global config;
4. assert the ref matches clean capture and `keep.txt` materializes.

Keep `TestCaptureHonorsGitignore` unchanged to prove repository-local ignores still work.

- [ ] **Step 2: Run tests and verify RED**

```bash
go test ./store -run 'TestCaptureIgnoresPersonal|TestCaptureHonorsGitignore' -count=1
```

Expected: hostile config changes the ref or drops `keep.txt`.

- [ ] **Step 3: Implement a controlled git configuration environment**

In `Trees.env`, begin from `os.Environ()` filtered to remove every `GIT_CONFIG_...` entry plus `GIT_ATTR_NOSYSTEM`; this prevents duplicate environment keys and hostile command-scope config injection. Append:

```go
"GIT_CONFIG_NOSYSTEM=1",
"GIT_CONFIG_GLOBAL=" + os.DevNull,
"GIT_ATTR_NOSYSTEM=1",
"GIT_CONFIG_COUNT=2",
"GIT_CONFIG_KEY_0=core.autocrlf",
"GIT_CONFIG_VALUE_0=false",
"GIT_CONFIG_KEY_1=core.excludesFile",
"GIT_CONFIG_VALUE_1=" + os.DevNull,
```

Do not disable local `.gitignore` parsing.

- [ ] **Step 4: Run store tests for GREEN**

```bash
go test ./store -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit deterministic capture**

```bash
git add store/git.go store/git_test.go
git commit -m "fix(store): isolate captures from personal git config"
```

---

### Task 6: CLI archive path and interface coverage

**Files:**
- Modify: `cmd/dawn/main.go:200-250` only if writer injection is needed
- Modify: `cmd/dawn/main_test.go`
- Modify: `store/git_test.go`

**Interfaces:**
- Produces: testable writer form `showRef(..., out io.Writer)` if direct stdout prevents isolation.
- Consumes: `Trees.Archive(ctx, tree, writer)`.

- [ ] **Step 1: Add a failing archive round-trip test in store**

Capture a tree containing `bin/dawn` and `README.md`, call `Archive` into `bytes.Buffer`, read with `archive/tar.NewReader`, and assert both names and bytes are present. This test passes only if the real archive stream is valid tar.

- [ ] **Step 2: Add a CLI `show REF` tar test**

Create a plan, in-memory journal record, blob record, and tree store representing a committed workspace. Call a writer-injected `showRef` and parse its bytes as tar. Assert `bin/dawn` is present.

First write the test against this desired signature:

```go
err := showRef(context.Background(), r, p, trees, "build.workspace", &buf)
```

- [ ] **Step 3: Run tests and verify RED**

```bash
go test ./cmd/dawn ./store -run 'Test.*Archive|TestShowRefStreamsTar' -count=1
```

Expected: CLI test does not compile because `showRef` has no writer parameter; store archive test provides coverage for the previously untested method.

- [ ] **Step 4: Inject the output writer minimally**

Change `showRef` to accept `io.Writer`, pass `os.Stdout` from `execute`, pass the writer to `Trees.Archive`, and use `fmt.Fprintln(out, v)` for scalar output. Do not introduce a command framework.

- [ ] **Step 5: Run CLI and store packages for GREEN**

```bash
go test ./cmd/dawn ./store -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit archive coverage**

```bash
git add cmd/dawn/main.go cmd/dawn/main_test.go store/git_test.go
git commit -m "test(cli): cover workspace tar export"
```

---

### Task 7: Stable-prefix flag guards and Claude documentation

**Files:**
- Modify: `backend/claude/claude.go:20-40`
- Modify: `backend/claude/claude_test.go`
- Modify: `backend/claude/workspace_test.go`

**Interfaces:**
- Consumes: fake CLI scripts receive their argv through shell `$@`.
- Guards: text backend `--system-prompt`; workspace backend `--exclude-dynamic-system-prompt-sections`.

- [ ] **Step 1: Add mutation-sensitive argument tests**

For `Backend`, use a fake CLI script that writes `"$@"` to a path supplied through an environment variable and then emits a valid envelope. Invoke the backend and assert the recorded args contain `--system-prompt` followed by the stable default prompt.

For `Workspace`, capture a base tree, run a fake CLI that records args and emits valid JSON, then assert args contain `--exclude-dynamic-system-prompt-sections`.

These tests must fail if either production flag is removed.

- [ ] **Step 2: Run focused tests and confirm they pass against current behavior**

```bash
go test ./backend/claude -run 'Test.*StablePrefixFlag' -count=1
```

Expected: PASS. Because the flags already exist, prove mutation sensitivity manually by temporarily removing each flag, observing RED, and restoring it before continuing; do not commit the mutation.

- [ ] **Step 3: Split fused comments and document every exported field**

Move the `DefaultTimeout` comment directly above the constant. Keep `defaultSystem`’s comment directly above `defaultSystem`. Replace inline field comments with adjacent field comments:

```go
type Backend struct {
	// Model is the default model; an invocation may override it.
	Model string
	// Bin is the CLI binary and defaults to "claude" on PATH.
	Bin string
	// Timeout bounds one invocation; zero selects DefaultTimeout.
	Timeout time.Duration
}
```

Apply the same form to `Workspace` fields without changing behavior.

- [ ] **Step 4: Run docs-sensitive package tests**

```bash
go test ./backend/claude -count=1
go vet ./backend/claude
```

Expected: PASS.

- [ ] **Step 5: Commit backend guards and comments**

```bash
git add backend/claude/claude.go backend/claude/workspace.go backend/claude/claude_test.go backend/claude/workspace_test.go
git commit -m "test(claude): guard stable prompt prefixes"
```

---

### Task 8: Store defensive behavior and failure branches

**Files:**
- Modify: `store/store.go:36-72`
- Modify: `store/fs.go:15-72`
- Test: `store/fs_test.go`
- Create: no new production package; keep any operation seam private in `store/fs.go`.

**Interfaces:**
- Produces: private `fsOps` with `createTemp`, `rename`, and sync-capable file behavior only if required to induce failures.
- Consumes: public `NewFS`, `Blobs.Put`, and `Blobs.Get` unchanged.

- [ ] **Step 1: Add Mem malformed-ref and defensive-copy tests**

Add tests that:

- `Mem.Get("not-a-ref")` returns a malformed-ref error;
- mutating the input slice after `Put` does not alter stored bytes;
- mutating bytes returned by `Get` does not alter the next `Get`;
- a valid missing ref returns `ref not found`.

- [ ] **Step 2: Add executable-bit normalization test**

In `store/git_test.go`, capture identical executable files created under permissive modes such as `0755` and `0775`, assert equal refs, materialize, and assert owner-executable is retained while unsupported mode distinctions do not affect identity.

- [ ] **Step 3: Add deterministic FS write/sync/rename failure tests**

Introduce tests against a private constructor `newFS(root string, ops fsOps)` while leaving `NewFS` unchanged. Provide fake operations that fail separately at:

- temp-file write;
- file sync;
- close; and
- rename.

For each, assert `Put` returns an error containing `store: write`, `store: sync`, `store: close`, or `store: commit`, and assert no final hash-named file exists. Use a small private `tempFile` interface exposing `Write`, `Sync`, `Close`, and `Name`.

- [ ] **Step 4: Run tests and verify RED**

```bash
go test ./store -run 'TestMem|TestCaptureNormalizesExecBit|TestFSPutFailure' -count=1
```

Expected: defensive-copy tests may pass; malformed/exec coverage records existing behavior; injected FS failure tests do not compile until the private seam exists.

- [ ] **Step 5: Add the minimal private operation seam**

Define:

```go
type tempFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

type fsOps struct {
	createTemp func(string, string) (tempFile, error)
	rename func(string, string) error
	remove func(string) error
}
```

Store `ops fsOps` in `FS`; have `NewFS` install wrappers around `os.CreateTemp`, `os.Rename`, and `os.Remove`. Use only this seam in `Put`; preserve cleanup and error text. Do not export it.

- [ ] **Step 6: Run store tests for GREEN**

```bash
go test ./store -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit store coverage**

```bash
git add store/store.go store/fs.go store/fs_test.go store/git_test.go
git commit -m "test(store): cover defensive copies and atomic write failures"
```

---

### Task 9: Kind vocabulary, platform contract, CI, and documentation truth

**Files:**
- Modify: `dawn.go:17-26,67-74`
- Modify: `README.md`
- Modify: `SPEC.md`
- Modify: `plan/lock_other.go:7-10`
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Keeps: all existing `Kind` constants for API compatibility, explicitly marked reserved where unreachable.
- Documents: Unix-only run lock and Unix-only process-group cancellation.

- [ ] **Step 1: Add non-Unix compile commands locally**

Run before adding CI:

```bash
GOOS=windows GOARCH=amd64 go build ./plan ./proc
GOOS=windows GOARCH=amd64 go build ./...
```

Expected: capture whether the current non-Unix build compiles. If it fails, fix only build-tag/platform compilation errors before proceeding and record the exact failure in the task notes.

- [ ] **Step 2: Add CI with native tests and Windows compile**

Create `.github/workflows/ci.yml`:

```yaml
name: CI
on:
  push:
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test ./...
      - run: go vet ./...
  windows-compile:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: GOOS=windows GOARCH=amd64 go build ./...
```

Pin actions to immutable commit SHAs before committing if repository policy requires supply-chain pinning; otherwise use these stable major tags consistently.

- [ ] **Step 3: Make Kind comments truthful**

Keep `KindValue` and `KindWorkspace` as produced kinds. Mark `KindArtifact` and `KindSession` as reserved for future backends, or remove them only if repository-wide search proves no public examples/tests rely on them. Update `Result.Produced` comments to stop claiming the current binary emits independent artifact/session refs.

- [ ] **Step 4: Reconcile README claims**

Update README to state:

- both commands accept `--dir`, `--in`, `--redo`, and `--jobs`;
- independent step concurrency is implemented;
- declared artifact paths are files forced into the workspace tree, not a separate emitted artifact kind;
- personal git configuration cannot alter captures;
- one-run locking and process-group cancellation are Unix guarantees, with non-Unix builds compiling but the lock remaining a no-op;
- the language summary says four flags, not three.

Remove concurrency from “Not here yet”; retain only genuinely unbuilt backends/features.

- [ ] **Step 5: Reconcile SPEC claims**

Update SPEC sections for:

- load/preflight validation terminology: capability checks occur before invocation when concrete backends are available;
- judge evidence: criteria plus resolved generator prompt/input rendering and complete output object;
- gated missing-path repair and zero judge tokens;
- deterministic capture independent of system/global git config;
- Unix-only lock/process-group exception;
- Delta: remove step concurrency from “Not yet”.

Preserve the settled behavior selected in the design spec.

- [ ] **Step 6: Check documentation contradictions**

Search:

```bash
# Use the repository grep tool for these patterns:
# "3 flags", "three flags", "concurrency beyond", "Not yet", "lock", "judge", "artifact"
```

Read every match in README/SPEC and remove or qualify stale claims. Confirm command package docs and usage output show the same four flags.

- [ ] **Step 7: Run compile and full tests**

```bash
go test ./...
go vet ./...
GOOS=windows GOARCH=amd64 go build ./...
```

Expected: PASS.

- [ ] **Step 8: Commit platform and documentation truth**

```bash
git add .github/workflows/ci.yml dawn.go plan/lock_other.go README.md SPEC.md cmd/dawn/main.go
git commit -m "docs: align Dawn's promises with the binary"
```

---

### Task 10: Coverage audit, full verification, and issue checklist review

**Files:**
- Modify tests from earlier tasks only when a named path remains uncovered.
- Do not change production behavior without a new failing test.

**Interfaces:**
- Verifies all prior task outputs; produces no new public API.

- [ ] **Step 1: Run race-enabled full suite**

```bash
go test -race ./...
```

Expected: PASS with no races.

- [ ] **Step 2: Generate package coverage reports**

```bash
go test ./cmd/dawn -coverprofile=/tmp/dawn-cmd.cover
go tool cover -func=/tmp/dawn-cmd.cover
go test ./store -coverprofile=/tmp/dawn-store.cover
go tool cover -func=/tmp/dawn-store.cover
```

Confirm named paths execute: `split`, flag/usage validation, `showRef`, `Archive`, `Mem.Get`, `FS.Put` failure cleanup, malformed refs, and exec-bit normalization. If a named path remains uncovered, add one focused test, observe its intended failure or mutation sensitivity, and rerun the package.

- [ ] **Step 3: Run standard verification**

```bash
go test ./...
go vet ./...
gofmt -w dawn.go backend/claude/*.go cmd/dawn/*.go gate/*.go plan/*.go store/*.go
git diff --check
GOOS=windows GOARCH=amd64 go build ./...
```

Run `go test ./...` again after `gofmt`.

- [ ] **Step 4: Review all 21 issue criteria against evidence**

For each checkbox in issue #1, record the guarding test, compile job, or exact documentation section. Specifically verify:

- zero-judge missing-path repair;
- complete judge evidence;
- producer and consumer capability preflight;
- redo and missing-`--in` usage errors;
- hostile git config determinism;
- uncommitted-ref exit classification;
- Unix-only lock disclosure;
- concurrency/flag/docs corrections;
- Kind and Claude-comment truth;
- CLI, prefix, archive, store, and non-Unix coverage;
- `gate.FromResult` absence.

- [ ] **Step 5: Review the final diff**

```bash
git status --short
git diff origin/master...HEAD --stat
git diff origin/master...HEAD --check
git log --oneline origin/master..HEAD
```

Inspect for accidental API growth, stale comments, secrets, generated binaries, or unrelated edits.

- [ ] **Step 6: Commit any verification-only test corrections**

If Step 2 revealed missing named-path coverage, commit only those tests:

```bash
git add '*_test.go'
git commit -m "test: close issue one coverage gaps"
```

If no changes exist, do not create an empty commit.
