# Routing input and output improvements

## Overview

Two backlog items, designed with the maintainer and partner review:

- **Input** (`docs/backlog/oversized-routing-input-needs-configurable-token-budgets.md`): routing request budgets and the bytes-to-token estimate are hardcoded in `pkg/prompt/budget.go`. A growing Ralphex progress log produced requests agrouter thought fit but the routing provider (Jev) rejected with HTTP 400 `max_tokens_exceeded`, which agrouter's 422-only size handling did not recognize. Make the budget configurable (svtlv's `MapBatchTokens`/`CharsPerToken` model), add the provider's long-context opt-in, classify the overflow precisely and report the provider's token counts for calibration. Sizing is the user's responsibility: overflow is never re-split or resent.
- **Output** (`docs/backlog/exec-routing-outcome-not-always-visible.md`): the `agrouter: selection:` line is lost when a caller hides agrouter's stderr (Ralphex's Codex executor shows only codex header lines, `ralphex/pkg/executor/codex.go:550`), and a readiness block leaves no routing line. Mirror every line agrouter itself writes to the console into an always-on log file (ralphex-style default, gitignored), and add explicit "routing skipped" / "run skipped" lines. Ralphex is not changed.

**Naming rule for everything new:** user-facing names stay provider-neutral (config keys, reason codes, messages, debug text, comments), so another classification provider can replace Jev later. Provider specifics (the long-context header, the provider's limits, its error shape) live only in the adapter package `pkg/jev`. Existing `jev_*` reason codes, `jev_model`, `--jev-api-key` and the "Jev ..." subjects in existing reason messages are not renamed in this plan.

**Known limitation, accepted:** with the shipped defaults (`request_tokens = 30000`, `bytes_per_token = 3`) the 2026-10-21 Ralphex input still overflows, now reported as `request_too_long` instead of `jev_api_error`. Users with log or code heavy prompts tune `bytes_per_token` (2.5 is the suggested starting point) or enable long context; the README says so.

## Context (from discovery)

- `pkg/prompt/budget.go`: `bytesPerToken = 3`, `stateLimit = 30_000`, `totalLimit = 64_000`, `AnchorTokens`, `attachmentShare`, `MinStateTokens`; `NewBudget`, `pairBudget`, `chunkRoom`, `docRoom`. Other conversions: `Result.StateTokens` (`chunk.go:94-96`), `anchor` (`chunk.go:163-175`), `Result.DocsTokens` (`doc.go:38-40`), `DocChunks(tokens)` (`doc.go:58`); router: `pkg/router/fanout.go:112`, `stage.go:170,174`, `complexity.go:46,50`. Budget tests live in `pkg/prompt/chunk_test.go:16-77`.
- `pkg/router/question.go:310-318` builds `prompt.Questions` and calls `prompt.NewBudget` from `router.New` (`router.go:40`). `ErrQuestionsOverBudget` messages name `routing_policy`/`complexity_policy` (`budget.go:59-65`, asserted in `router_test.go:174-191`).
- Re-split recovery: `stage.go:158-178`, `fanout.go:82,104-125` (`resplitRejected`, `errUnsplittable`), `pool.go:54-65` (`halve: prompt.Halve`), `complexity.go:43-50,72` (`halve: prompt.HalveDoc`), `prompt.Halve`/`HalveDoc`/`halveText` (`chunk.go:140-160`, `doc.go:72-80`).
- `pkg/jev/client.go`: `ErrUnprocessable` (422), `StatusError{Status, Body}` whose `Error()` prints the redacted body (`client.go:75-81`), `Ask(ctx, Request) (map[string]Answer, error)`, headers in `post` (`client.go:163-165`). The 200 response carries `usage.input_tokens`, not parsed today (`client_test.go:52` fixture). `cmd/agrouter` builds the client through `newJev func(key string) router.JevClient` (`app.go:38,55`; test override `app_test.go:333`).
- moq closures typed `func(...) (map[string]jev.Answer, error)`: `router_test.go:64,323,396`, `pool_test.go:59,385,558,599,628`, `stage_test.go:58,316,433,485,531,562,606,660`, `complexity_test.go:57,445,469`, `project_test.go:24`, `evalcase_load_test.go:26`; direct `Ask` use in `pkg/jev/score_test.go:25`.
- `cmd/agrouter/reason.go`: typed-error codes; `failureMessage` builds `subject + " failed: " + detail` (`reason.go:112-125`). `cmd/agrouter/debug.go:125-158` prints completed stages and chunks.
- Output: `app.go:142-176` (`a.route` error path prints `preflightError.line` at :148; post-routing block from `execSelection` (`fallback.go:71`, early return at `fallback.go:83`) printed at `app.go:169-172`; `logSelection` at `app.go:185`); `preflightBlock` lines in `readiness.go:95-103`, including the generic branch reached by the capability (unsupported model) verdict (`pkg/readiness/readiness.go:34`, `evaluate.go:169-181`); `newDebugLog(a.getenv(envDebug), a.stderr)` captures the writer at `app.go:125`; the child gets `a.stderr` at `app.go:405`. Config merge is per key, embedded first (`load.go:60-84`). Local config is `<workDir>/.agrouter/config` (`load.go:33-43`).
- Ralphex keeps logs in `.ralphex/progress/` and writes `.ralphex/.gitignore` = `.gitignore`, `progress/`, `worktrees/` (`ralphex/pkg/config/defaults.go:110`).
- Provider facts (TypeSafe Jev): [errors](https://docs.typesafe.ai/api/errors.md) — 400 with `max_tokens_exceeded` means the request is longer than the context length; 422 is validation. [Models](https://docs.typesafe.ai/models#long-context) — default 32k per question and 64k per request; header `TypeSafe-Danger-Zone-Long-Context: true` raises both to 262k, same price, slower and less accurate. The docs give decimal "k" values only.

## Development Approach

- **testing approach**: Regular (code first, then tests in the same task), table-driven testify tests
- complete each task fully before moving to the next; every task compiles and passes `make test` on its own
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change (`make test`), lint after each task (`make lint`)
- follow CLAUDE.md: no catalog names in production code; tests use synthetic catalogs and never pin shipped defaults; mocks regenerated with `make generate`

## Testing Strategy

- **unit tests**: required for every task (see Development Approach above)
- the provider is an `httptest` server or the moq mock; golden requests in `pkg/router/testdata` are rewritten with `go test ./pkg/router -update`
- child processes use the `GO_WANT_HELPER_PROCESS=1` fake CLI; log tests use a `t.TempDir()` work directory and a fixed `getenv`, never a real project or the developer's environment
- tests that run `git check-ignore` skip when `git` is not on `PATH`
- no e2e UI tests in this project

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

```
 config [agrouter]                          provider adapter (pkg/jev)
 request_tokens, bytes_per_token,           Limits(longContext): per question / per request
 long_context, log_dir                      default 32000/64000, long 262000/262000
        │                                   long-context header, overflow error shape
        ▼                                          │
 config.Validate: request_tokens > 0, ratio finite > 0, log_dir non-blank
        │                                          │
        ▼                                          ▼
 router.New: request_tokens ≤ min(limits) − 2000 reserve; room for state ≥ MinStateTokens
        │
        ▼
 prompt.Budget{Policy{BytesPerToken, RequestTokens}, State, Chunk, Doc}
        state = request_tokens − Σ question estimates (− anchor for chunks)
        │
        ▼
 provider request ── 200 ──► answers + reported input tokens ──► AGROUTER_DEBUG token lines
                  ── overflow (400 max_tokens_exceeded) ──► reason request_too_long (no resend)
                  ── other 400 / 422 ──────────────────► jev_api_error (unchanged)
```

**Note:** config holds only provider-neutral policy. The provider's hard limits come from its adapter and are checked when the router is built, so swapping providers means swapping the adapter, not the config.

Key decisions and why:

- **One allowance, `request_tokens`.** It is the entire serialized input of one routing request: state plus all its questions. agrouter sends at most two questions, so Jev's 64k request cap never binds under default limits, and under long context the request cap always binds. One knob is correct for both limit sets. It is not chunk text and not the child model's output.
- **Limits belong to the provider adapter.** `long_context` selects the provider's long-context mode (for Jev: the header and the raised limits); it does not raise `request_tokens`. The 2000-token reserve is a router constant, subtracted once in the `router.New` check, so the effective maxima are 30000 (default) and 260000 (long context).
- **No re-split.** Maintainer decision: the user sizes requests through config. All recovery code goes; the overflow is a precise, actionable error.
- **The estimator ratio travels inside `prompt.Budget`**, built by `NewBudget`, so `pkg/prompt` keeps depending on nothing internal and every bytes↔tokens conversion agrees. Any finite positive ratio is accepted; the arithmetic saturates instead of overflowing.
- **Defaults come from the embedded config only.** Load merges embedded defaults per key (`load.go:60-84`), so user configs that omit a key inherit it. Synthetic test fixtures that bypass the embedded layer set the keys explicitly; there is no second default source in code.
- **Reported tokens are permissive metadata.** Missing, null, malformed or negative `usage.input_tokens` is "unknown" and never fails a valid answer. It is returned per call (fan-out asks concurrently), never stored on the client.
- **The console mirror log** is always on. It copies agrouter's own stderr lines to `<log_dir>/agrouter.log`, unlimited and append-only. `log_dir` defaults to `.agrouter/logs` in the work directory, ralphex-style. For the default directory agrouter keeps the log gitignored; a custom `log_dir` is user-managed. The child's output and stdout are never mirrored (verbose decision JSON carries argv and the prompt).

## Technical Details

### Config keys (`[agrouter]`)

| Key | Type | Embedded default | Validation in config |
|-----|------|------------------|----------------------|
| `long_context` | bool (`strconv.ParseBool`) | `false` | — |
| `request_tokens` | int | `30000` | `> 0`; the provider-limit check is in `router.New` |
| `bytes_per_token` | float | `3` | finite and `> 0` |
| `log_dir` | path | `.agrouter/logs` | non-empty after trimming |

`log_dir` is resolved once in setup: a relative path against the work directory, an absolute path as is. There is no on/off switch and no environment override: the mirror log is always on. The `defaults/config` key reference header marks the four keys "default:".

`router.New` checks `request_tokens ≤ min(provider per-question limit, per-request limit) − 2000`, naming the value, the provider limit and the reserve. It keeps the room-for-state check, now worded around `request_tokens`: `request_tokens N less the routing_policy question leaves M tokens for the state, need 2000` (same for the chunk anchor and the complexity_policy questions). Both checks surface as config errors (exit 2).

Recommended long-context example, commented, in the embedded defaults and README:

```ini
# long context: the routing provider's experimental long-input mode (for TypeSafe Jev, the
# "danger zone" header TypeSafe-Danger-Zone-Long-Context: true). Jev's limits rise from 32k per
# question / 64k per request to 262k each, at the same price, but slower and less accurate.
# turning it on changes only the provider's limits: raise request_tokens yourself.
# recommended starting values, uncalibrated; compare AGROUTER_DEBUG=1 "tokens:" lines and tune:
# long_context    = true
# request_tokens  = 128000
# bytes_per_token = 2.5
# timeout         = 120s
```

128000 is a starting point well under the 260000 maximum, given the provider's accuracy warning. For the 2026-10-21 Ralphex input (78 KB plan + 102 KB progress log) it is an estimated ~72k tokens at 2.5, so it would likely be sent whole, though logs keep growing. 2.5 only raises estimates compared with 3. The rejection proved the real ratio was below 2.77 for that input, which does not make 2.5 safe even there, so calibrate it from the reported counts.

### Budget derivation (`pkg/prompt`)

```go
// Policy is how agrouter sizes routing requests: the estimator ratio and the allowance per request.
type Policy struct {
	BytesPerToken float64
	RequestTokens int
}

// Budget is how many tokens the state may take in each kind of request, under its policy.
type Budget struct {
	Policy
	State int
	Chunk int
	Doc   int
}
```

- `Policy.Tokens(n)` = `ceil(n / BytesPerToken)` and `Policy.Bytes(tokens)` = `floor(tokens × BytesPerToken)`, both saturating at `math.MaxInt` and never negative.
- `NewBudget(p Policy, q Questions)` fills `Policy` and the state budgets: `State = RequestTokens − Tokens(Route)`, `Chunk = RequestTokens − Tokens(ChunkRoute) − Tokens(Relevance)`, `Doc = RequestTokens − Tokens(Complexity) − Tokens(Evidence)`. Each subtraction is checked against what remains (a question estimate larger than the remainder is `ErrQuestionsOverBudget`, never a wrapped-around positive). `stateLimit`, `totalLimit` and `pairBudget` are removed. Chunk and doc states shrink by the companion question (about 1–1.5k) compared with today; whole-state requests are unchanged at 30000.
- `Fits`, `Split`, `DocsFit` and `SplitDocs` use `b.Policy`. `Result.StateTokens(p Policy)` and `Result.DocsTokens(p Policy)` take the policy. `anchor` takes the policy for its limit (`AnchorTokens` × ratio) and attachment share. `DocChunks(b Budget, tokens int)`.
- `AnchorTokens`, `attachmentShare` and `MinStateTokens` stay constants. `MinStateTokens` now only checks viability up front.
- Test `Budget{...}` literals set `Policy{BytesPerToken: 3, RequestTokens: ...}`. With ratio 3, `ceil(n/3.0)` and `floor(t×3.0)` equal today's integer math, so **no golden request changes**; any diff is a regression.

### Provider adapter (`pkg/jev`)

- `jev.Limits(longContext bool) (perQuestion, perRequest int)`: 32000/64000, or 262000/262000.
- `Client.LongContext bool`: when true, `post` sets `TypeSafe-Danger-Zone-Long-Context: true`. `cmd/agrouter`'s `newJev` becomes `func(key string, longContext bool) router.JevClient`.
- `StatusError` gains `ErrorType string`, taken from the raw body's `detail.error_type` before redaction and truncation (bounded read; parse failure leaves it empty). `ErrTooLong` matches only `Status == 400 && ErrorType == "max_tokens_exceeded"`. `ErrUnprocessable` is deleted; a 422 is a plain `StatusError`. The redacted body keeps appearing in `Error()` as today.
- `Ask` returns `Response{Answers map[string]Answer; InputTokens *int}`. `JevClient` in `pkg/router` and its moq mock change accordingly. Router tests wrap their existing closures with one helper, `answersOnly(func(...) (map[string]jev.Answer, error))`, instead of rewriting each.

### Overflow error and reason

- The failing request's error wraps `ErrTooLong` with the stage, chunk `i of n` when chunked, `estimated N tokens` for the full request, and `request_tokens`, without prompt text.
- `reason.go`: `request_too_long`, stage from `router.StageError`. The final message reads `<existing subject> failed: routing request over the provider's context length; lower request_tokens or bytes_per_token`. Other 400s and 422 stay `jev_api_error`; 429/529 retries are unchanged.
- Routing failure semantics are unchanged: caller-pinned values are kept, exec warns and runs, decision mode prints null JSON with reasons.

### Token diagnostics

- `router.Stage` (whole), `router.ChunkResult` and `router.DocScore` gain `Estimated int` (our estimate of the full request) and `Reported *int` (the provider's input-token count).
- Debug lines append `tokens: estimated N, provider reported M` or `tokens: estimated N, provider reported unknown`.

### Routing-skipped lines

- Pre-routing readiness block (`applyPreflight` → `preflightError`, printed at `app.go:148`): before the existing line, print `agrouter: routing skipped: <cli> <reason>; routing provider was not called; child CLI was not started`. The reason is fixed text per verdict: subscription quota exhausted, executable missing, authentication failed, availability unverified, model not supported.
- Post-routing block (`execSelection`, printed at `app.go:169-172`): first log the selection routing produced (`logSelection` with what is known: cli, model, effort, reasons), then print `agrouter: run skipped: <cli> <reason>; child CLI was not started`. `execSelection` returns before filling readiness/effective fields (`fallback.go:83`), so the line claims neither those fields nor a child run.
- The quota line keeps its configured `error_prefix`, message, reset information and exit code. stdout is unchanged.

### Console mirror log

```
 agrouter ─┬─ a.stderr (tee) ─┬─► console (unchanged)
           │                  └─► <log_dir>/agrouter.log   (default <workDir>/.agrouter/logs)
           │                       "2026-10-22T15:17:03Z pid=1234 agrouter: selection: {...}"
           └─ child stderr / stdout ─► console only (never mirrored)
```

**Note:** agrouter's own lines go to the console exactly as today and are copied to the log. The child's output never passes through the copy.

- The tee is built at the top of `run()`, before `newDebugLog` captures the writer (`app.go:125`), so debug lines are mirrored too. It starts in buffering mode; after config resolves `log_dir` it replays the buffered copy to the file only and then appends live. `run()` flushes it with a `defer`, and each `run()` builds a fresh tee, so repeated `app.run` calls never stack mirrors or warnings. The raw stderr is kept for `runner.Command.Stderr` (`app.go:405`).
- The tee writes the console first, unchanged in content, timing and order, then appends `<RFC3339 UTC> pid=<n> <line>` with one `O_APPEND` write per complete line. A trailing partial line is flushed on exit.
- Invocations that fail before config loads (usage errors, config errors) are not mirrored, since `log_dir` is not known yet.
- Default `log_dir` only, ralphex-style: before the first append, agrouter ensures `<workDir>/.agrouter/.gitignore` ends with the rules `.gitignore` and `logs/` (create the file if missing; add a newline if the last line lacks one; append both lines unless they are already the file's last two lines, so a later `!logs/` cannot undo them; never truncate). The local `.agrouter/config` stays trackable. If that cannot be done, mirroring is skipped for this invocation with the warning.
- A custom `log_dir` (anywhere, including another directory under `.agrouter`) is user-managed: agrouter creates the directory but never writes ignore files for it. The README says so.
- A mirror failure never changes console output or the exit code. It prints at most one console-only `agrouter: warning: log: ...` line per invocation and does not recurse.
- Debug lines are mirrored only when `AGROUTER_DEBUG=1` prints them. The prompt and API key are already redacted from every console line.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): code, tests and docs in this repository
- **Post-Completion** (no checkboxes): real-provider checks, calibration, installing the binary for Ralphex projects

## Implementation Steps

### Task 1: Add token budget, long-context and log_dir keys to config

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/load.go`
- Modify: `pkg/config/validate.go`
- Modify: `pkg/config/defaults/config`
- Modify: `pkg/config/config_test.go`, `pkg/config/validate_test.go` and other `pkg/config` fixtures
- Modify: synthetic fixtures that skip the embedded layer and now fail validation (`pkg/catalog/catalog_test.go:16-20`, `pkg/readiness/readiness_test.go:228`, `pkg/router/router_test.go:32` `requestFixture`)

- [x] add `LongContext bool`, `RequestTokens int`, `BytesPerToken float64`, `LogDir string` to `Agrouter`; parse `long_context` (`strconv.ParseBool`), `request_tokens`, `bytes_per_token`, `log_dir` in `load.go` with layering like the existing keys
- [x] validate provider-neutrally: ratio finite and `> 0`, `request_tokens > 0`, `log_dir` non-empty after trimming; no provider limits in `pkg/config`
- [x] add the keys, marked "default:" in the key reference header, with provider-neutral comments and the commented long-context example block, to `pkg/config/defaults/config`
- [x] set the new keys explicitly in synthetic fixtures that bypass the embedded layer
- [x] write tests for parsing and layering (embedded < global < local), a key omitted in global/local inheriting the embedded value, relative/absolute/blank `log_dir`
- [x] write tests for validation (zero, negative, NaN, ±Inf ratio; zero/negative `request_tokens`); keep the embedded-config smoke test; do not pin shipped defaults
- [x] run tests - must pass before next task

### Task 2: Provider adapter: limits, long-context header, overflow classification and reported tokens

**Files:**
- Modify: `pkg/jev/client.go`, `pkg/jev/types.go`
- Modify: `pkg/jev/client_test.go`, `pkg/jev/score_test.go`
- Modify: `pkg/router/router.go` (`JevClient`), `pkg/router/stage.go`, `pkg/router/pool.go`, `pkg/router/complexity.go` (call sites)
- Modify: `pkg/router/mocks/*` (regenerated)
- Create: `pkg/router/helpers_test.go` (`answersOnly`)
- Modify: `pkg/router/router_test.go`, `pool_test.go`, `stage_test.go`, `complexity_test.go`, `project_test.go`, `evalcase_load_test.go`
- Modify: `cmd/agrouter/app.go` (`newJev`), `cmd/agrouter/app_test.go`

- [x] add `jev.Limits(longContext)` and `Client.LongContext`; `post` sends `TypeSafe-Danger-Zone-Long-Context: true` only when set; `newJev(key, longContext)` passes `cfg.Agrouter.LongContext`
- [x] parse `detail.error_type` from the bounded raw error body into `StatusError.ErrorType` before redaction; add `ErrTooLong`, matched only by 400 + `max_tokens_exceeded`; keep `ErrUnprocessable` until task 3
- [x] parse `usage.input_tokens` permissively; `Ask` returns `Response{Answers, InputTokens}`; update `JevClient`, run `make generate`, update router call sites to use `Answers` (reported tokens are ignored until task 6); wrap test closures with `answersOnly`
- [x] write tests: `Limits` for both modes; header present only with `LongContext`; `ErrTooLong` for the structured 400, not for another 400, a 422, a 400 with a malformed or truncated body
- [x] write tests: usage present, absent, null, non-integer and negative (answers still valid, tokens unknown); existing redaction tests still pass
- [x] write an `app_test.go` case: the fake provider sees the long-context header with `long_context = true` and no header by default
- [x] run tests - must pass before next task

### Task 3: Remove re-split recovery

**Files:**
- Modify: `pkg/router/stage.go`, `pkg/router/fanout.go`, `pkg/router/pool.go`, `pkg/router/complexity.go`
- Modify: `pkg/prompt/chunk.go`, `pkg/prompt/doc.go`
- Modify: `pkg/jev/client.go`
- Modify: `pkg/router/stage_test.go`, `pkg/router/pool_test.go`, `pkg/router/complexity_test.go`
- Modify: `pkg/prompt/chunk_test.go`, `pkg/prompt/doc_test.go`, `pkg/jev/client_test.go`

- [x] delete the whole-state re-split and `routeState.resplit` (`stage.go:158-178`), the 422 branch, `resplitRejected` and its loop (`fanout.go:82,104-125`), the `resplit` flag and `halve`/`text`/`number` callbacks (`pool.go:54-65`, `complexity.go:43-50,72`), `errUnsplittable`, `prompt.Halve`/`HalveDoc`/`halveText`, and `jev.ErrUnprocessable`; keep `cut`, `headTail`, `DocChunks`
- [x] rewrite the 422 recovery tests as no-resubmit tests: a structured 400 at whole, chunk, doc and effort level is never sent a second time (assert per request, with a barrier where an exact count matters) and the stage fails with `ErrTooLong`; another 400 and a 422 fail as plain status errors
- [x] keep fan-out cancellation, ordering and deadline-success coverage; drop tests that only covered `Halve`/`HalveDoc`; redaction tests use a plain 422 `StatusError`
- [x] run tests - must pass before next task

### Task 4: Derive request budgets from the policy and check provider limits

**Files:**
- Modify: `pkg/prompt/budget.go`, `pkg/prompt/chunk.go`, `pkg/prompt/doc.go`
- Modify: `pkg/router/question.go`, `pkg/router/router.go`, `pkg/router/stage.go`, `pkg/router/pool.go`, `pkg/router/complexity.go`
- Modify: `pkg/prompt/chunk_test.go`, `pkg/prompt/doc_test.go`
- Modify: `pkg/router/router_test.go`, `stage_test.go`, `pool_test.go`, `project_test.go`, `complexity_test.go`, `question_test.go`

- [x] add `prompt.Policy` (saturating `Tokens`/`Bytes`) and embed it in `Budget`; replace `bytesPerToken`, `stateLimit`, `totalLimit`, `pairBudget` and package-level `Tokens`; `StateTokens`/`DocsTokens` take a `Policy`, `anchor` uses it, `DocChunks` takes a `Budget`
- [x] `NewBudget(p Policy, q Questions)`: state budgets are `RequestTokens` minus the request's question estimates, each subtraction checked against what remains; `ErrQuestionsOverBudget` messages name `request_tokens` and the policy question
- [x] `router.New` checks `request_tokens ≤ min(jev.Limits(long_context)) − 2000` and builds the policy from `cfg.Agrouter`
- [x] wrap the overflowing request's error with stage, chunk `i of n`, `estimated N tokens` and `request_tokens`
- [x] write tests for budget derivation with synthetic question sizes and ratios (fractional, very small and very large finite ratios: no overflow, no negative or wrapped budgets), and that chunk/doc room and anchor limits follow the ratio; update `Budget{...}` literals with an explicit policy
- [x] write tests for `router.New`: `request_tokens` at and over the provider limit under both limit sets; a too-small `request_tokens` alone naming `request_tokens` (update `router_test.go:174-191`)
- [x] write tests that the overflow error carries the stage, chunk position, estimate and `request_tokens`, and no prompt marker
- [x] run `go test ./pkg/router -update` and confirm there is no golden diff
- [x] run tests - must pass before next task

### Task 5: Add the request_too_long reason code

**Files:**
- Modify: `cmd/agrouter/reason.go`
- Modify: `cmd/agrouter/reason_test.go`, `cmd/agrouter/app_test.go`

- [x] map `jev.ErrTooLong` to `request_too_long` with the stage and the fixed provider-neutral detail; leave other status errors on `jev_api_error`
- [x] write tests for the code and the full message in decision JSON (`reason`, `null_reasons`) and in the exec fallback warning, with an `httptest` provider returning the structured 400
- [x] write tests that an unrelated 400 and a 422 still report `jev_api_error`
- [x] run tests - must pass before next task

### Task 6: Report estimated and provider-reported tokens in debug output

**Files:**
- Modify: `pkg/router/stage.go`, `pkg/router/pool.go`, `pkg/router/complexity.go`
- Modify: `cmd/agrouter/debug.go`
- Modify: `pkg/router/stage_test.go`, `pkg/router/pool_test.go`, `pkg/router/complexity_test.go`
- Modify: `cmd/agrouter/debug_test.go`

- [x] record `Estimated` (full request: state plus questions) and `Reported` (`InputTokens`) on `Stage`, `ChunkResult` and `DocScore`, per call
- [x] append `tokens: estimated N, provider reported M|unknown` to the stage, chunk and doc debug lines (`debug.go:125-158`)
- [x] write tests that estimates and reported counts land on the right stage/chunk under concurrent fan-out, and that unknown usage prints `unknown`
- [x] write tests that debug output still never includes `--doc` text, the prompt or the key
- [x] run tests - must pass before next task

### Task 7: Print routing-skipped and run-skipped lines on readiness blocks

**Files:**
- Modify: `cmd/agrouter/app.go`, `cmd/agrouter/readiness.go`, `cmd/agrouter/fallback.go`
- Modify: `cmd/agrouter/readiness_test.go`, `cmd/agrouter/app_test.go`

- [x] carry a fixed reason on `preflightError` (quota, executable missing, authentication, availability unverified, model not supported); on the pre-routing path print `agrouter: routing skipped: <cli> <reason>; routing provider was not called; child CLI was not started` before the existing line
- [x] on the post-routing `execSelection` block, log the selection routing produced, then print `agrouter: run skipped: <cli> <reason>; child CLI was not started`
- [x] extend `TestApp_ExecBlockedAcrossInvocations` and the readiness block cases: explanation first, zero provider requests, no child started, quota line, prefix, reset info and exit code unchanged across retries
- [x] write tests for each block kind, including the capability verdict, on both paths; stdout stays empty
- [x] run tests - must pass before next task

### Task 8: Add the line-buffering console tee

**Files:**
- Create: `cmd/agrouter/consolelog.go`
- Create: `cmd/agrouter/consolelog_test.go`

- [x] add a tee writer that writes the console first and unchanged, buffers lines until given a log file path, replays the buffer to the file only, then appends `<RFC3339 UTC> pid=<n> <line>` with one `O_APPEND` write per complete line; `Close` flushes a partial line
- [x] add the default-directory ignore setup: ensure `<workDir>/.agrouter/.gitignore` ends with `.gitignore` and `logs/` (create, fix a missing final newline, append unless they are already the last two lines, never truncate)
- [x] on any failure, emit at most one console-only warning per tee and stop mirroring; never alter console output
- [x] write tests: fragmented writes produce whole lines; buffered lines replayed to the file only; console bytes identical with and without a file; partial line flushed; one warning on an unwritable path
- [x] write tests for the ignore setup: new file, existing content kept, missing final newline, an existing `!logs/` followed by our rules, idempotent on repeat; in a temporary repository (skip without `git`) `git check-ignore` matches the log and not `.agrouter/config`
- [x] run tests - must pass before next task

### Task 9: Wire the mirror log into the app

**Files:**
- Modify: `cmd/agrouter/app.go`
- Modify: `cmd/agrouter/app_test.go`

- [x] build a fresh tee at the top of `run()`, before `newDebugLog` (`app.go:125`), and `defer` its close; keep the raw stderr for the child (`app.go:405`)
- [x] after config loads, resolve `log_dir` against the work directory; run the ignore setup only for the default directory; start file mirroring (skip with the warning if ignore setup fails)
- [x] write tests: selection, warnings, routing-skipped/run-skipped and debug lines mirrored; child stderr/stdout and decision JSON never mirrored (prompt and argv markers absent); config-error invocations write nothing; repeated `app.run` calls do not stack mirrors or warnings
- [x] write tests: a custom `log_dir` (relative, and absolute via `t.TempDir()`) gets the log and no ignore file is written; concurrent invocations keep lines intact; a read-only work directory warns once and the exit code is unchanged
- [x] run tests - must pass before next task
- ➕ `config.LocalDir(workDir)` added so the default log directory is `LocalDir/logs` without repeating `.agrouter`; mirroring starts once `setup` fully succeeds (config, catalog and `router.New`), so a `request_tokens` over the provider limit also writes nothing
- ⚠️ first runs racing on a fresh `.agrouter` may each append the ignore rules; git reads the repeats as the same rules, so no lock was added (documented on `ensureLogIgnored`, asserted in the concurrency test)

### Task 10: Verify acceptance criteria

- [x] verify the structured 400 yields `request_too_long` with no resend, and other 400/422 yield `jev_api_error`
- [x] verify `long_context` sends the header and changes only the provider limits; the `request_tokens` limit check under both limit sets
- [x] verify a Codex-style caller that drops stderr still leaves the selection in `.agrouter/logs/agrouter.log`, and that `.agrouter/config` stays trackable
- [x] verify no new user-facing name, message or comment mentions Jev outside `pkg/jev` and the long-context example comment
- [x] verify the README states the known default-size limitation and the tuning path
- [x] run full test suite: `make test`
- [x] run lint: `make lint`, plus `GOOS=linux`, `GOOS=darwin` and `--build-tags=eval`
- [x] verify coverage is 80%+ per package (mocks excluded)
- ➕ the README had no limitation or tuning text, so the `Request size` section (keys, `request_too_long`, the known limitation, the tuning path and the long-context example) and the removal of the re-split clause at `README.md:115` landed here; task 11 keeps the rest of the README work
- ➕ added the app-level case `a caller that drops stderr still has the selection in the log` (stderr is `io.Discard`; in a temporary git repository the log is ignored and `.agrouter/config` is not)

### Task 11: [Final] Update documentation

- [ ] README: the four `[agrouter]` keys, the long-context example, the known default-size limitation, the always-on log, `log_dir` (default ignored ralphex-style via `.agrouter/.gitignore`; a custom directory is user-managed), the routing-skipped/run-skipped lines, `request_too_long` in the reason table; the `Request size` section and the removal of the re-split promise at `README.md:115` already landed in task 10: check them and cover the rest
- [ ] CLAUDE.md: replace the hardcoded-budget assumption; provider limits live in the adapter; note the console mirror log beside the stdout/stderr rules; recovery is gone
- [ ] `git rm` both backlog items in the commit that lands them
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- `make eval-routing` (real provider, `TYPESAFE_API_KEY`) with default limits and with `long_context = true`, `request_tokens = 128000`: confirm the header is accepted and compare the selections with chunked runs of the same input, since the provider calls long context less accurate.
- Replay the 2026-10-21 reproduction (`AGROUTER_DEBUG=1 agrouter --cli claude -p '... progress-20261020-digest-staged-clustering.txt ...'`) and read the `tokens: estimated N, provider reported M` lines to calibrate `bytes_per_token` for log and code text.

**External system updates:**
- Install the new binary where Ralphex runs (`make build`, then update `~/go/bin`), and check that a Codex-executor run in Lampa leaves `agrouter: selection:` lines in `.agrouter/logs/agrouter.log`.
- Update `.agrouter/config` in svtlv and Lampa if they need long context or a different `bytes_per_token`.
