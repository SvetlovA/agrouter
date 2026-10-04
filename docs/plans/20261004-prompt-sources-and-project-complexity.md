# Prompt sources, unbounded chunking and project complexity

## Overview
- Remove the `[agrouter]` keys `max_chunks`, `chunk_parallel` and `relevance_floor`. Jev runs remotely, so every chunk request starts at once and the number of chunks is not limited; every chunk counts in pooling with its raw relevance as weight.
- Add `-p, --prompt TEXT` and `--prompt-file PATH`. The prompt may come from `-p`, the positional argument, `--prompt-file` or stdin, in any combination; none of them is a usage error (exit 2). `-p/--print` (a no-op today) is replaced.
- Add a repeatable `--doc PATH` (CLAUDE.md, AGENTS.md, ...) used only for routing. When given and Jev is asked, a first stage map-reduces the docs into a project complexity score from 0 to 10. A second stage, today's routing, then receives the score in its state and questions. A small script and an enterprise system can then get different models for the same task. Without `--doc` there is no complexity stage and no `project` field; floor removal and unlimited fan-out still change routing for every call.
- `docs/design.md` stays deleted. The rule that requires updating it, and the README links to it, are removed.

## Context (from discovery)
- Config: `pkg/config/config.go:30-32` (fields), `pkg/config/load.go:231-236` (keys), `pkg/config/validate.go:48-56`, `pkg/config/defaults/config:12-26`.
- Capture and limits: `pkg/prompt/capture.go` (`Capture`, `ErrCaptureLimit`), `pkg/prompt/mentions.go` (`ReadMentions(ctx, cwd, limit)`), `pkg/prompt/budget.go` (`CaptureLimit`, `chunkRoom` with `maxChunks` digit width), `pkg/prompt/chunk.go` (`Split(b, maxChunks)`, `ErrTooManyChunks`, `nonEmpty` checks at :106/:121).
- Routing: `pkg/router/router.go` (`Route`, one-option bypass :83-84, `cannotDecide`), `pkg/router/pool.go` (semaphore :127, `MaxChunks` recheck :103, `pool` floor :110/:237), `pkg/router/question.go` (questions, relevance criteria, `budget`).
- CLI: `cmd/agrouter/flags.go` (`Print` bool at :109, positional prompt at :89-95, help text), `cmd/agrouter/app.go` (`route` :158-189), `cmd/agrouter/debug.go`.
- Argv: `pkg/args/request.go:48` (`Prompt Optional`), `pkg/args/build.go:115-120` (`{prompt}`); Windows shims reject any CR/LF in an argument (`pkg/runner/batch.go:22,38-39`).
- Eval: `pkg/router/evalcase_test.go` (case format), `testdata/routing/*.json`.
- Docs: `README.md` (:10, :44-77, :109, :140), `CLAUDE.md:7`, `AGENTS.md:7`.

## Decisions (settled with the user and with the p2 partner review)
- **Prompt sources:** `-p` text, the positional prompt, `--prompt-file` text and stdin text, joined in that fixed order with `"\n\n"`. At least one must be non-empty, otherwise `ErrNoPrompt` (exit 2). `-p` and `--prompt-file` may each be given once; a second one is a usage error.
- **Child prompt:** `{prompt}` = `-p` + positional + prompt-file text, joined the same way. Stdin is never copied into argv; it is still replayed to the child byte for byte. Bytes stay unchanged (no trimming).
- **Prompt-file in exec mode on Windows npm `.cmd` shims:** any CR/LF in the argument fails to start the child (exit 127). This is accepted and documented, not worked around. Decision mode and native executables are unaffected.
- **Explicit files (`--prompt-file`, `--doc`):** paths are relative to the working directory, with no workdir-tree restriction. They must be regular text files: missing, unreadable, directory or binary = a usage error (exit 2) naming the flag and path. They don't inherit `ReadMentions`' silent skipping. `--prompt-file` text is part of the prompt, so the files it mentions are read as today. `--doc` text is not scanned for mentions.
- **`--doc` is routing-only:** it is never passed to the child, which loads its own CLAUDE.md/AGENTS.md. The docs are read and validated on every call; the complexity stage runs only when `--doc` is given and more than one option is eligible (after the one-option bypass).
- **Complexity stage (map):** docs are task-independent, so the prompt isn't included. If all docs fit one request, there is one request; otherwise one request per doc chunk, all at once. Each request asks:
  - `complexity`: a Choice over criteria `"0"`..`"10"` with short anchored descriptions. The chunk score is the expected value `Σ i·p(i)`, not the argmax.
  - `complexity_evidence`: a Noul, "does this text describe the project's size, architecture, dependencies or constraints".
- **Complexity stage (reduce):** a weighted mean of the chunk scores with weight = evidence Noul. If all weights are 0, a plain mean. The result is rounded to one decimal.
- **Complexity stage failure:** a 422 re-splits that chunk once, at half size. A second 422, any other Jev error, or the deadline goes through the existing cannot-decide policy (one CLI left → run with only the caller's constraints; more → exit 2). Supplied docs are never silently dropped.
- **Second stage:** today's routing over the prompt and mentioned files only; doc text is not resent. The state gets `"project":{"complexity":6.9}`, top-level in a single state and inside the anchor of chunk states. `question` and `chunk_question` describe it as a contextual signal, judged against the specific task (an enterprise repo with a typo fix is still trivial). It isn't a multiplier of probabilities. Without `--doc` the field is omitted and routing is unchanged.
- **Pooling (second stage):** weight = raw relevance Noul, with no floor. If the total weight is 0, a plain mean. Zero-relevance filler has no effect at any length; low but non-zero relevance still dilutes when there is enough of it (a raw weighted mean has no cap). This limit is documented, not hidden. The relevance question text and criteria are unchanged (docs never reach stage 2).
- **No limits:** no capture limit, no chunk count limit, unbounded parallelism (429/529 are already retried within the deadline). The routing `timeout` covers both stages. It is cooperative: capture, file reading and network honor it, while splitting and marshaling don't. Documented as such.
- **Chunk envelope:** without `max_chunks`, `chunkRoom` reserves the decimal width of the largest `int` for each of `index` and `of` (2 x 19 digits on 64-bit, derived from `math.MaxInt`), on top of the exact JSON punctuation and field names and the serialized anchor. Every chunk, including ones renumbered after a 422 re-split, fits its budget without iterating. Doc requests have their own envelope (`{"doc":{"index":,"of":,"text":""}}`), not the routing `chunkRoom`.
- **Removed keys** are simply deleted (pre-release, no migration). An old config naming them gets the existing unknown-key error.

## Development Approach
- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
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
- run tests after each change (`make test`), lint with `make lint` (also `GOOS=linux`, `GOOS=darwin`, `--build-tags=eval`)
- follow CLAUDE.md: no catalog names outside `pkg/config/defaults` (`guard_test.go`), synthetic fixtures, coverage 80%+ per package, `make generate` after interface changes, golden requests via `go test ./pkg/router -update`

## Testing Strategy
- **unit tests**: required for every task, table-driven with testify; synthetic catalogs and docs.
- **end-to-end**: `cmd/agrouter` tests with the helper-process fake CLI and an `httptest` Jev, for the new flags, errors and argv contents.
- **golden requests**: new goldens for the complexity requests and for stage-2 states carrying `project.complexity`.
- **eval** (`make eval-routing`, real Jev, never in CI): new cases that contrast the same task with small-script and enterprise docs, plus controls (a trivial task in an enterprise repo, a style-only doc, task and doc fact both buried mid-text, unrelated modules).
- No UI, so no browser e2e tests.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

```
parse: -p, positional, --prompt-file, --doc..., stdin
  │
  ├─ read --prompt-file / --doc strictly (usage error on bad file)
  ├─ capture stdin (no limit, under the deadline) → Result{Prompt, Docs, ...}
  ├─ eligibility ── one option? → run it, no Jev
  ├─ read mentioned files (no limit)
  │
  ├─ Stage 1 (only with --doc):  docs → split → N requests in parallel
  │      each: complexity Choice "0".."10" + evidence Noul
  │      reduce: Σ evidence·E[score] / Σ evidence  → project.complexity
  │      failure → cannot-decide policy
  │
  └─ Stage 2: today's routing, state/anchor + project.complexity
         fits → one request;  else chunks, all in parallel
         pool: weight = raw relevance (all zero → plain mean)
```

## Technical Details
- `config.Agrouter`: drop `MaxChunks`, `ChunkParallel`, `RelevanceFloor`; add `ComplexityQuestion` and `ComplexityEvidence` (texts, required non-empty like the other questions).
- `prompt.Result`: add `Docs []string`. `prompt.State` and `prompt.Anchor` gain `Project *Project \`json:"project,omitempty"\`` with `Project{Complexity float64}`.
- `prompt.Budget`: add `Doc int`, the doc state budget derived from the complexity and evidence question sizes like `Chunk`. `NewBudget` returns `ErrQuestionsOverBudget` when that leaves less than `MinStateTokens`.
- Doc request state: `{"docs":["..."]}` when everything fits, else `{"doc":{"index":i,"of":n,"text":"..."}}`. It is cut with the same line/UTF-8 `cut`.
- `prompt.SplitDocs(b Budget) []DocChunk` and `prompt.HalveDoc`, or a shared generalization of `Halve`. The two stages share only the request lifecycle through a small helper: pending/done, fail-fast cancel, one 422 retry, halve and renumber. Scoring and result payloads stay stage-specific (the current `slot` in `pool.go:40-48` is route-specific; complexity must not be squeezed into `ChunkResult`/`probs`).
- A 422 on the initial whole-docs request turns the docs into doc chunks at half of `Budget.Doc` (not the routing `Budget.Chunk`), marked as already retried, mirroring the single-request handler in `pool.go:68-73`.
- `router.Route` runs stage 1 when `captured.Docs` has text and more than one option is eligible; any stage-1 error goes to `cannotDecide`. `Decision` gains `Complexity *ComplexityResult` (per-chunk score/weight, final score) for debug output only. The decision JSON is unchanged.
- Debug (`AGROUTER_DEBUG=1`): stage-1 per-chunk `score`/`evidence` and the final complexity, never doc text.
- `-p` value must not look like a flag: go-flags rejects an option-like value (`-p --model x`). This is covered by a test.

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, README/CLAUDE.md/AGENTS.md updates, eval cases.
- **Post-Completion** (no checkboxes): running `make eval-routing` against real Jev, checking Jev accepts an 11-criterion Choice, tuning the question texts.

## Implementation Steps

### Task 1: Remove max_chunks, chunk_parallel, relevance_floor and the limits they drive

One compiling change: every caller and test of a removed API is updated in this task.

**Files:**
- Modify: `pkg/config/config.go`, `pkg/config/load.go`, `pkg/config/validate.go`, `pkg/config/defaults/config`
- Modify: `pkg/prompt/capture.go`, `pkg/prompt/mentions.go`, `pkg/prompt/budget.go`, `pkg/prompt/chunk.go`
- Modify: `pkg/router/pool.go`, `pkg/router/router.go`, `cmd/agrouter/app.go`
- Modify: `pkg/config/config_test.go`, `pkg/config/validate_test.go`, `pkg/catalog/catalog_test.go`, `pkg/args/build_test.go`, `pkg/prompt/capture_test.go`, `pkg/prompt/mentions_test.go`, `pkg/prompt/chunk_test.go`, `pkg/router/router_test.go`, `pkg/router/pool_test.go`, `pkg/router/evalcase_test.go`, `cmd/agrouter/app_test.go`
- Modify: `testdata/routing/oversized-buried-requirement-max.json`

- [x] config: drop the three fields, their key parsing and validation; remove the keys from `defaults/config`; reword the `timeout` comment (covers both stages, cooperative)
- [x] `Capture` reads stdin to EOF under ctx with no byte limit; remove `ErrCaptureLimit` and the `limit` parameter (`Undecidable` keeps only the deadline case); `ReadMentions` drops `limit`
- [x] remove `CaptureLimit`; `chunkRoom` reserves the decimal width of the largest `int` for each of `index` and `of` (derived from `math.MaxInt`, 19 digits on 64-bit), in addition to the exact JSON envelope and the serialized anchor; `Split(b)` drops `maxChunks` and `ErrTooManyChunks`
- [x] `round` starts every pending chunk at once (no semaphore), keeping fail-fast cancel and deadline handling; drop the `MaxChunks` recheck after a re-split; `pool` weight = raw relevance, total 0 -> plain mean
- [x] update every caller: `app.route`, the eval harness (`evalcase_test.go`, check it compiles with `-tags eval`), synthetic configs in tests; update the oversized eval case description
- [x] write tests: removed keys are unknown-key errors; large stdin captured whole; deadline still undecidable; many chunks with no limit; every serialized chunk state within `Budget.Chunk`, also after `Halve` and renumbering to more digits; all chunks in flight at once (peak = chunk count)
- [x] write pooling tests: zero-relevance filler of any length has no effect; all-zero relevance -> plain mean; a finite low-positive-weight example (e.g. 0.95 relevant vs N x 0.01 filler) pinning where dilution flips the result, documenting the raw-mean limit (replaces the "floored filler outweighs it at max_chunks" test)
- [x] run `make test` and `go vet -tags eval ./...` - must pass before task 2

### Task 2: Strict reading of explicit files

**Files:**
- Create: `pkg/prompt/file.go`, `pkg/prompt/file_test.go`

- [x] `ReadTextFile(ctx, cwd, path string) (string, error)`: resolve relative to cwd, require a regular file, read under ctx, reject binary content (reuse `detect.go`), wrap errors with the path
- [x] sentinel errors for missing / not a regular file / binary, so the CLI can say which flag failed
- [x] write tests: text file read exactly (bytes unchanged, CRLF kept), relative and absolute paths, missing, directory, binary, unreadable (Unix-only case behind the build tag)
- [x] run tests - must pass before task 3

### Task 3: -p, --prompt-file and positional/stdin prompt sources

**Files:**
- Modify: `cmd/agrouter/flags.go`, `pkg/args/request.go`, `pkg/prompt/capture.go`, `cmd/agrouter/app.go`
- Modify: `cmd/agrouter/flags_test.go`, `pkg/prompt/capture_test.go`, `cmd/agrouter/app_test.go`

- [x] replace `-p/--print` with `-p, --prompt TEXT`; add `--prompt-file PATH`; a second `-p` or `--prompt-file` is a usage error; update usage line and help text
- [x] `args.Request` carries the `-p` text, positional text and prompt-file path separately; `app` reads the file (Task 2) before capture; usage error names `--prompt-file`
- [x] `Capture` takes the explicit texts (in order: `-p`, positional, file) and joins them with stdin text by `"\n\n"`; `ErrNoPrompt` when everything is empty; the error text names all four sources
- [x] `{prompt}` gets `-p` + positional + file text only; stdin is replayed, never in argv
- [x] write tests: each source alone, all four together (order pinned), empty `-p ""` with stdin, all empty → exit 2, repeated flags → exit 2, `-p --model x` rejected, bad `--prompt-file` → exit 2, argv contents for a native helper, a Windows `.cmd` shim with a prompt file containing a newline → exit 127 (Windows build tag)
- [x] run tests - must pass before task 4
- ➕ `--print` stays as a long-only no-op: ralphex's Claude mode passes it with the prompt on stdin, so only `-p` changes meaning (README update in Task 10)

### Task 4: --doc flag and docs in the captured result

**Files:**
- Modify: `cmd/agrouter/flags.go`, `pkg/args/request.go`, `pkg/prompt/capture.go`, `pkg/prompt/chunk.go`, `cmd/agrouter/app.go`
- Modify: `cmd/agrouter/flags_test.go`, `pkg/prompt/chunk_test.go`, `cmd/agrouter/app_test.go`

- [x] repeatable `--doc PATH`; every doc is read strictly on every call (bad path → usage error naming `--doc`, even with one eligible option)
- [x] `prompt.Result.Docs`; docs never reach the child argv or stdin, and are not scanned for mentions
- [x] `nonEmpty`-style checks in `Split` stay about prompt/files only (docs aren't stage-2 state); add a test that docs never make a short prompt leave the anchor
- [x] write tests: repeated `--doc` order kept, doc not in argv/stdin, bad doc → exit 2, docs ignored by mention scanning
- [x] run tests - must pass before task 5

### Task 5: Complexity questions, budget and doc splitting

**Files:**
- Modify: `pkg/config/config.go`, `pkg/config/load.go`, `pkg/config/validate.go`, `pkg/config/defaults/config`
- Modify: `pkg/router/question.go`, `pkg/prompt/budget.go`, `pkg/prompt/chunk.go`
- Modify: `pkg/config/validate_test.go`, `pkg/prompt/chunk_test.go`
- Create: `pkg/router/question_test.go` (if absent), golden `pkg/router/testdata/request_complexity*.json`

- [x] config keys `complexity_question` and `complexity_evidence` (non-empty), with default texts in `defaults/config`
- [x] `complexityQuestion`: Choice over `"0"`..`"10"` with anchored descriptions as constants in `question.go`; `evidenceQuestion`: Noul with true/false criteria
- [x] `Budget.Doc` from both question sizes; over-budget → config error
- [x] `SplitDocs(b)`: one state when all docs fit, else doc chunks cut on lines/UTF-8; `HalveDoc` for 422s (or a shared halving helper)
- [x] write tests: budget derivation and over-budget error, single vs split doc states, every doc state within `Budget.Doc`, golden requests for a single and a chunked doc request
- [x] run tests - must pass before task 6
- ➕ `SplitDocs` returns nil when the docs fit; `DocChunks(tokens)` cuts unconditionally, for the whole-docs 422 split at half of `Budget.Doc` in Task 6; `complexityQuestions(ag)` builds both doc questions; `DocChunk.Doc` (not serialized) keeps the doc index

### Task 6: Complexity stage: map, reduce and failure policy

**Files:**
- Create: `pkg/router/complexity.go`, `pkg/router/complexity_test.go`
- Modify: `pkg/router/pool.go` (extract the shared slot/re-split loop)

- [ ] fan out every doc request at once inside the routing deadline; fail-fast cancel on non-422 errors
- [ ] 422 on a doc chunk → halve that chunk once; second 422 or under `MinStateTokens` → error
- [ ] 422 on the initial whole-docs request → split at half of `Budget.Doc` into chunks marked as already retried; a 422 on one of those → error
- [ ] per-chunk score = `Σ i·p(i)`; validate that every criterion has a probability (else `jev.ErrMalformed`)
- [ ] reduce: evidence-weighted mean, all-zero → plain mean, round to one decimal; return per-chunk results for debug
- [ ] write tests with the moq mock: expected-value scoring, weighted reduce (a 9 with high evidence beats two 2s with low evidence), all-zero fallback, single-request path, malformed answer, deadline
- [ ] write re-split tests: whole-docs 422 -> half-budget chunks; completed chunks are not asked again; second 422 after the whole-doc retry fails; doc text is preserved across re-splits; every renumbered doc request fits `Budget.Doc`
- [ ] run tests - must pass before task 7

### Task 7: Feed project complexity into routing

**Files:**
- Modify: `pkg/prompt/chunk.go`, `pkg/router/router.go`, `pkg/router/pool.go`, `pkg/config/defaults/config`, `cmd/agrouter/debug.go`
- Modify: `pkg/router/router_test.go`, `pkg/router/pool_test.go`, `cmd/agrouter/debug_test.go`, `cmd/agrouter/app_test.go`, goldens in `pkg/router/testdata`

- [ ] `State.Project` / `Anchor.Project`; `Route` runs stage 1 after the one-option bypass when docs have text, then sets the project on the single state or the anchor; the anchor size includes it
- [ ] any stage-1 error → `cannotDecide`
- [ ] reword `question` and `chunk_question` defaults: `project.complexity` (0-10, when present) is context about the codebase, judged against the specific task
- [ ] debug output: stage-1 per-chunk score/evidence and the final complexity, no doc text
- [ ] write tests: no `--doc` → requests byte-identical to before (golden), with `--doc` → `project.complexity` in single and chunk states (goldens), one eligible option → no Jev calls at all, stage-1 failure with one CLI left vs several, debug lines, end-to-end decision with `--doc` via `httptest` Jev
- [ ] run tests - must pass before task 8

### Task 8: Eval cases for project complexity

**Files:**
- Modify: `pkg/router/evalcase_test.go`, `pkg/router/evalcase_load_test.go`
- Create: `testdata/routing/complexity-*.json`

- [ ] case format gains `docs` (inline text or files beside the case) and `prompt_file`
- [ ] cases: same refactor task with a small-script doc vs an enterprise doc (different acceptable sets); a trivial typo fix in an enterprise repo; a style-rules-only doc; task and the relevant doc fact both buried mid-text; a doc describing an unrelated module
- [ ] write tests for loading the new fields (untagged, against the mock)
- [ ] run tests - must pass before task 9

### Task 9: Verify acceptance criteria
- [ ] verify all requirements from Overview are implemented
- [ ] verify edge cases are handled (empty sources, bad files, one-option bypass, all-zero weights, 422 re-splits, deadline)
- [ ] run full test suite: `make test`
- [ ] run `make lint` with v2.13.0, also `GOOS=linux`, `GOOS=darwin` and `--build-tags=eval`
- [ ] verify test coverage is 80%+ per package (mocks excluded)

### Task 10: [Final] Update documentation
- [ ] README: prompt sources (`-p`, positional, `--prompt-file`, stdin), `--doc` and the complexity stage, removed keys, the cooperative timeout, the Windows `.cmd` + prompt-file limitation, exit codes (bad `--prompt-file`/`--doc`), the `argv` note (now holds `-p`/positional/file text); remove the `docs/design.md` links
- [ ] CLAUDE.md and AGENTS.md: remove the "docs/design.md is the specification" rule; update Layout/Rules where the new flags or stage change them
- [ ] remove the design references in `pkg/router/question.go:48` and `defaults/config` comments
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- run `make eval-routing` with `TYPESAFE_API_KEY`; check that Jev accepts an 11-criterion Choice and that the complexity cases separate small-script from enterprise context
- tune `complexity_question`, `complexity_evidence`, `question` and `chunk_question` texts from the eval results
- check latency of the two sequential stages against the 10s default `timeout` with a large CLAUDE.md; raise the default if needed
- watch for 429s from Jev with unbounded fan-out on very large inputs
