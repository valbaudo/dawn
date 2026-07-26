# Prompt-cache behavior of `claude -p`, measured

Measured 2026-07-24 on macOS, Claude Code 2.1.214, `--model haiku`, via
`--output-format json` and its `usage` object. The prompt prefix was a fixed
33,549-byte block (~5k tokens), byte-identical across every call.

`cc` = `cache_creation_input_tokens` (written to cache, billed ~1.25x)
`cr` = `cache_read_input_tokens` (read from cache, billed ~0.1x)

## 1. Sessionless calls do NOT share a cache, even with an identical prefix

| call | cc | cr |
|---|---|---|
| baseline, no prefix | 12,577 | 20,215 |
| fresh A, 5k prefix | 20,502 | 20,215 |
| fresh B, same prefix, +3s | 20,501 | 20,215 |

B re-paid cache **creation** on the whole prefix and read nothing extra. The
constant 20,215 is Claude Code's own system prompt and tools, cached
independently of anything the caller supplies.

## 2. A resumed session DOES cache the conversation

| call | cc | cr |
|---|---|---|
| turn C, `--session-id`, 5k prefix | 20,501 | 20,215 |
| turn D, `--resume` | 295 | **40,716** |
| turn E, `--resume` | 71 | **41,011** |

`cr` roughly doubles: the prefix written in turn C is read back in D and E.

## 3. `--fork-session` does NOT inherit the cache

Re-run tightly (warm + all forks inside ~60s, so no TTL confound):

| call | cc | cr |
|---|---|---|
| warm, `--session-id`, 5k prefix | 19,768 | 20,215 |
| linear, `--resume` | 208 | **39,983** |
| fork 1, `--resume --fork-session` | 20,033 | 20,215 |
| fork 2, `--resume --fork-session` | 20,032 | 20,215 |

Both forks re-paid the full write. Reproduced across two independent runs
(the first, looser run showed cc 20,924 / 20,924 / 20,929 with cr flat at 20,215).

## 4. With a STABLE prefix, sessionless calls DO share cache

Same 33,549-byte block, but passed as `--system-prompt` (which replaces Claude
Code's own drifting default preset) plus `--no-session-persistence`. Three runs,
three different session ids, no session anywhere:

| run | session id | cc | cr |
|---|---|---|---|
| 1 (cold) | f3b3dbdb | 33,288 | 0 |
| 2 | 1bb8a258 | 7,458 | **25,830** |
| 3 | e94808d1 | 8,184 | **25,830** |

**This is the decisive result: the cache is keyed on the token PREFIX, not on a
session.** Anthropic's docs say hits work "regardless of whether they're part of
the same conversation", and that is what runs 2-3 show across unrelated sessions.

Why §1 looked session-dependent: Claude Code's DEFAULT system prompt is not
byte-stable (env, cwd, git status), prefix matching is exact, and the 5k block in
§1 sat *after* that drift — so it could never hit, with or without a session.
Independent runs elsewhere measured the drift directly as cache_creation of
31,680 / 31,683 / 31,682 across three default-prompt calls.

**Not fully reproduced:** a separate investigation reported *zero* re-writes and
100% of the prefix served from cache under these flags. Here ~7-8k is still
written every run (~78% cached, not 100%). Unexplained; possibly breakpoint
placement or a residual unstable segment. Do not quote 100%.

## 5. After the fix: caller content caches across unrelated invocations

`claude.Backend` now passes an explicit `--system-prompt` (replacing the drifting
preset) and `claude.Workspace` passes
`--exclude-dynamic-system-prompt-sections`, which is Anthropic's own flag for
this: it *"moves per-machine sections (cwd, env info, memory paths, git status)
from the system prompt into the first user message"* and its documented purpose
is to *"improve cross-user prompt-cache reuse"*. It is ignored with
`--system-prompt`, which is why the two backends take different routes.

Measured through `dawn run` itself: two DIFFERENT plan files, in DIFFERENT state
directories, whose prompts share a ~6k-token leading block and differ only in the
final line.

| run | cc | cr |
|---|---|---|
| A (cold) | 30,345 | 0 |
| B | 12,416 | **17,929** |

B read 17,929 tokens that A created. Before the fix this number was structurally
zero for caller content: the flat 20,215 in §1 was Claude Code's own preset, and
the caller's block was re-created on every call.

**Partial, and expected to be.** ~59% of the cold cost came from cache; the rest
is the span after the last cache breakpoint before the two prompts diverge.
Breakpoint placement is the provider's, not dawn's — which is exactly why dawn ships
ordering discipline plus the measurement, and no knob.

## What this means

**Settled:**

- **A session is NOT required for a cache hit.** The key is the exact token
  prefix. §4 gets large reads across three unrelated session ids.
- **Prefix STABILITY is the actual variable.** With Claude Code's default preset
  the prefix drifts a few tokens per run, which invalidates everything after it —
  that is what made §1 look session-dependent. `--system-prompt` stabilizes it.
- **`--fork-session` gets no cache benefit** (§3, reproduced twice). Fan-out off a
  warmed session is not a money lever on this CLI. Consistent with a breakpoint at
  the end of the latest user turn: only a strict prefix extension hits.
- **A session is a workaround for an unstable prefix, not the caching mechanism.**

**NOT settled — the cost comparison.** On these numbers, in base-input-equivalents
(write 1.25x, read 0.1x), per steady-state call:

| shape | cost |
|---|---|
| §2 session `--resume` | 208x1.25 + 39,983x0.1 ≈ **4,258** |
| §4 fresh, stable prefix | 7,458x1.25 + 25,830x0.1 ≈ **11,906** |

which says session-resume is cheaper — the opposite of what an independent
analysis concluded from its own numbers (4,629 fresh vs 5,185 resume). The two
shapes are not apples-to-apples: §2 put the bulk in turn 1's user message and then
added a tiny turn, while §4 re-sends a different user message against a large
system prompt each run. **Do not cite a cost ratio from this document.** What is
safe to say: both shapes cache well, and the pathological case is neither of them
— it is a fresh call against a drifting default prefix, which re-pays everything.

A session also carries a real cost the table hides: it re-sends the entire
transcript, which grows monotonically, and a fresh context never sends it at all.

## Caveats

- One model (haiku), one machine, one CLI version. Claude Code's cache
  breakpoint placement is an implementation detail and can change.
- Not measured: extended (1h) TTL behavior, other models, concurrent resumes of
  one session, or whether a long-idle session still hits after the TTL expires
  (expected: no; the transcript survives, the cache does not).
- Vendors other than Anthropic are NOT covered here. OpenAI documents automatic
  prefix caching, which may behave differently; verify before generalizing.

## Reproduce

```sh
P=$(python3 -c "print('\n'.join('Reference document section %d: ...' % i for i in range(220)))")
u() { jq -c '{cc:.usage.cache_creation_input_tokens,cr:.usage.cache_read_input_tokens}'; }

claude -p "$P

say A" --model haiku --output-format json </dev/null | u
claude -p "$P

say B" --model haiku --output-format json </dev/null | u   # cr unchanged

S=$(uuidgen)
claude -p --session-id "$S" "$P

say C" --model haiku --output-format json </dev/null | u
claude -p --resume "$S" "say D" --model haiku --output-format json </dev/null | u   # cr ~2x
```
