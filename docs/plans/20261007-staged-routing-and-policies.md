# Staged routing and policy config

## Overview
- Routing asks Jev one Choice over every eligible (cli, model, effort) option, about 30 in the defaults. The probability mass spreads across them, so the top option usually gets 0.2-0.3, and a model whose mass is split across efforts loses to a model with its mass on one effort. Jev's Choice confidence is `(p_max - 1/n) / (1 - 1/n)` (docs.typesafe.ai/confidence), so a large `n` keeps it low.
- Route in up to three stages instead: **CLI → model → effort**. A stage is asked only when more than one candidate is left, and each stage narrows the options for the next. A value the caller passed fixes its stage. Jev calls cost nothing, so the price is latency: up to three sequential round trips.
- Shrink the `[agrouter]` question config to two keys, `routing_policy` and `complexity_policy`. They hold only the tunable preference. The state-field guides, the stage questions, and the relevance and evidence questions move into code next to what they describe. Adding stages adds no config.
- The removed keys `question`, `chunk_question`, `relevance`, `complexity_question` and `complexity_evidence` are config errors that name their replacement.
- Decision JSON and the exec log report one confidence per stage that was asked (`cli`, `model`, `effort`) beside `project_complexity`, replacing `model_selection`.

## Context (from discovery)
- Config: `pkg/config/config.go:25-35` (`Agrouter` fields), `pkg/config/load.go:220-250` (`decodeAgrouter`, unknown key at :342), `pkg/config/validate.go:48-62`, `pkg/config/defaults/config:18-75` (question texts and the comments describing state fields).
- Questions: `pkg/router/question.go` (`routeQuestion` :93, `fullRouteQuestion` :134, `relevanceQuestion`, `complexityQuestion`, `evidenceQuestion`, `budget` :214, its error wrap :225).
- Routing: `pkg/router/router.go` (`Route` :82, `decide`, `ask` :123, `chosen`, `cannotDecide` :153), `pkg/router/pool.go` (`single`, `pooled` :77 with `routeQuestion(... r.enc)` at :80, `askChunk`, `pool`, `pooledScores`, `ranked`), `pkg/router/fanout.go` (shared fan-out and 422 re-split), `pkg/router/complexity.go`, `pkg/router/eligibility.go` (`Eligibility`; unusable `--cli` warning :69-76; `filterEffort` :103-124).
- Catalog: `pkg/catalog/catalog.go` (`Option{ID, CLI, Section, Name, Effort}`, `ByCLI`, `ByModel`, `CLIs`).
- Budget: `pkg/prompt/budget.go` (`Questions`, `NewBudget` with old key names in its errors at :64/:67, `pairBudget`).
- Output: `cmd/agrouter/app.go` (decision JSON types :64-89, `run` exits before `debug.decision` at :113-118, `route` drops `d` on error at :222-225), `cmd/agrouter/confidence.go` (`model_selection`, verbose route/chunks/pooled scores), `cmd/agrouter/debug.go:80-100`.
- Test fakes that assume one joint question: `cmd/agrouter/app_test.go:59-137` (`fakeJev`), `pkg/router/evalcase_load_test.go:22-48` (`evalMock`, untagged, runs in `make test`).
- Fixtures setting the old keys: `pkg/catalog/catalog_test.go`, `pkg/args/build_test.go`, `pkg/config/config_test.go`, `pkg/config/validate_test.go`, `pkg/prompt/chunk_test.go`, `pkg/router/router_test.go` (struct fixture and `TestNewQuestionOverBudget`), `pkg/router/pool_test.go`, `pkg/router/question_test.go`, `pkg/router/evalcase_test.go`, `pkg/router/evalcase_load_test.go`.
- Eval: `pkg/router/eval_test.go` (the only `eval`-tagged file), `pkg/router/evalcase_test.go:200-240`, `testdata/routing/*.json`.
- Goldens: `pkg/router/testdata/request_*.json`, including `request_full_*` for the full encoding.
- Docs: `README.md` (Usage :35, Project complexity :68, Long inputs :80, Configuration :135, Exit codes :203), `CLAUDE.md`, `AGENTS.md`.

## Decisions (settled with the user and with the p2 partner review)
- **Rejected: pruning until confidence ≥ 0.8.** Confidence ≥ 0.8 needs `p_max ≥ 0.8 + 0.2/n` at any `n` (0.90 with 2 options), so the loop may never end. Renormalizing after a prune moves confidence up or down without new evidence. Complexity is an ordinal Score, and pruning levels would renumber them and break the 0-9 → 0-10 scaling.
- **Stages are group-by levels over the eligible options:**
  - `cli`: group key and criterion name are `o.CLI`.
  - `model`: group key and criterion name are `o.Section`.
  - `effort`: group key `o.ID`; criterion name is the effort label `o.Effort`, unique within one model.
  - A level with one group is skipped without asking Jev. After an asked level, the options are narrowed to the winning group.
  - Each stage request uses the level name (`cli`, `model`, `effort`) as its question id.
- **A value the caller passed fixes its stage (user's rule):** `Eligible` already filters by `--cli`, `--model` and `--effort` before routing, so every remaining option carries the passed value. That stage has one group and is never asked, and later stages don't re-decide it.
  - an accepted `--cli X`: the `cli` stage is skipped; `model` and `effort` are asked within X. An unusable `--cli` is still skipped with a warning (`eligibility.go:69-76`) and fixes nothing, so `cli` is asked as usual.
  - `--model M` from the catalog: M belongs to one CLI, so `cli` and `model` are both skipped and only `effort` is asked.
  - `--model M` outside the catalog: one option per remaining CLI, all carrying M. `cli` is asked when more than one CLI is left; `model` is skipped. M has no catalog efforts, so `effort` is skipped and the caller's `--effort`, if any, passes through.
  - `--effort E`: the options keep only effort E, so the `effort` stage is skipped. Under effort passthrough (no option has E), the options carry no effort and E is emitted as given.
  - With all three passed, one option is left and Jev is not asked (the existing one-option bypass).
- **The one-option bypass records its stages:** it fills the three `Stage` records as skipped, with their deterministic choices, without calling Jev or running the complexity stage. Verbose output then lists every stage in every case.
- **The stage instructions carry what lies below each criterion:**
  - The descriptions live once in the instructions object (compact style).
  - The `cli` stage's `clis` entry for each CLI has its description and its eligible models, each with description, price and effort levels. The `efforts` entry describes the effort levels per CLI.
  - Criteria values only identify their group: `{"cli"}`, `{"model", "cli"}` or `{"effort"}`.
  - Without the subtree, Jev can't apply the "cheapest option that can reliably do it" policy at a higher level.
- **Eligibility stays immutable:** routing narrows a private copy of the options. The caller and debug output keep the original filters.
- **Any stage failure fails the whole routing (user's choice):**
  - Partial decisions are discarded. The existing cannot-decide policy runs against the *original* eligibility: one CLI eligible → run it with only the caller's `--model`/`--effort`; more → exit 2.
  - A CLI chosen by Jev never sets `Pinned`; only an explicit `--cli` enables raw passthrough.
  - The completed stages stay in the decision for verbose and debug output, including on the exit-2 path.
  - When the routing is undecided, `confidence` carries no routing stage keys. Only `project_complexity` remains, as today.
- **Split prompts:** at each level, every chunk is asked that level's question plus relevance. The answers are pooled across all chunks with the existing relevance-weighted mean, and every chunk then moves on to the same narrowed next level. Hierarchies are never completed per chunk, because conditional distributions from different branches can't be pooled. Relevance is asked at every level (calls are free) rather than reused, because a 422 re-split changes chunk identity. The 422 behavior is unchanged per level: a whole state re-splits once at half the chunk budget, and a chunk re-splits once at half size.
- **Config owns only policy:**
  - `routing_policy` is the cost/quality preference (today's "lowest cost that can reliably satisfy… prefer cheaper… prefer less time…" text plus the `project.complexity` guidance). It is shared by every routing stage, whole or chunk.
  - `complexity_policy` is the "judge the codebase as a whole: size, architecture, dependencies and constraints, not any single task" preference.
  - Both must be non-empty.
- **Code owns structure,** as constants in `pkg/router/question.go` (router interprets; prompt only owns the data shape):
  - the state guide for a whole state, a chunk state, and docs/doc chunk;
  - each stage's question;
  - the lookup hints;
  - the relevance and evidence questions with their criteria;
  - the ten-level complexity rubric (already code).
- **Instructions are one structured object:** `{question, state, policy, clis|models|efforts}` for a routing stage, and `{question, state, policy}` for complexity. Relevance and evidence keep plain-text instructions from code.
- **Removed keys error with a hint:** `[agrouter] question (<layer path>): removed; set routing_policy instead`. Likewise `chunk_question` → `routing_policy` and `complexity_question` → `complexity_policy`. `relevance` and `complexity_evidence` say they are no longer configurable and point at the matching policy key. Old full questions mix structure and policy, so they can't be converted automatically. They are never silently ignored.
- **Budget:** a state's allowance is the limit minus the largest serialized question it can travel with, using the composed guides and policy.
  - `cli` stage: built over the whole catalog.
  - `model` stage: the largest over the catalog's CLIs, each built from that CLI's models.
  - `effort` stage: the largest over the catalog's models, each built from that model's efforts. Effort labels repeat across models, so one question over the whole catalog would have duplicate criteria.
  - The whole-state budget uses the max of the three. The chunk budget uses the max of the chunk-stage variants, paired with relevance. Doc budgets pair complexity with evidence.
  - Budget errors name `routing_policy` or `complexity_policy`.
- **`Stage.Choice` is the criterion name** (CLI, model section or effort label), so it matches the probability keys. `Decision.OptionID` carries the option ID. A skipped stage records the value its options share:
  - `cli`: the CLI;
  - `model`: the section, or the model name under model passthrough;
  - `effort`: the effort label, or the caller's passed effort or empty when the options carry none.
- **Confidence output:**
  - `confidence` holds `cli`, `model`, `effort` and `project_complexity`. A skipped stage is omitted, and all three are omitted when the routing is undecided. A pooled stage reports the mean of its chunk confidences, as `model_selection` does today.
  - `model_selection` is removed, which is a breaking change for its readers; this is documented.
  - Stage confidences are conditional (each given the stages before it), not the confidence of the full tuple.
  - `--verbose` lists every stage in order, skipped ones included, with their deterministic choice. Asked stages carry the full probability map, or per-chunk answers and pooled scores, so the decision can be reproduced by hand.
  - The verbose `options` list stays as the eligible options; its comment and README entry no longer say it keys the probabilities.
- **Full encoding removed:** `EncodingFull` compared two encodings of the single joint question, which no longer exists. The real-API `make eval-routing` stays, reporting accuracy, per-stage confidence and latency.
- **Timeout:** the default `timeout = 10s` stays. The README notes that routing makes up to three sequential round trips, more with a complexity stage, and says when to raise it.

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
- follow CLAUDE.md: no catalog names outside `pkg/config/defaults` (`guard_test.go`), synthetic fixtures (`requestFixture`, `syntheticCLIs`) rather than the embedded catalog for new and rewritten tests, test behavior rather than shipped config contents, coverage 80%+ per package, `make generate` after interface changes, golden requests via `go test ./pkg/router -update`

## Testing Strategy
- **unit tests**: required for every task, table-driven with testify.
- **Jev**: the `JevClient` moq mock for stage sequencing and failures; `httptest` where the wire format matters. Golden requests in `pkg/router/testdata` use the synthetic catalog.
- **test fakes**: `fakeJev` (`cmd/agrouter`) and `evalMock` (`pkg/router`) take a target option ID and answer each stage with that option's CLI, section or effort label.
- **end-to-end**: `cmd/agrouter` tests with the helper-process fake CLI for decision JSON, the exec log line and exit codes.
- **no UI e2e tests** in this project.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview
`Route` keeps its shape: one-option bypass, cannot-decide policy, then the optional complexity stage, then deciding. `decide` becomes a loop over the three levels. Each level groups the current options by its key. One group means the level is skipped. Several groups means Jev is asked once for the whole state, or once per chunk with the answers pooled. The options are then narrowed to the winning group. The last remaining option is the decision. `pool`, `ranked` and the fan-out work over criterion names rather than `catalog.Option`, so the same code pools CLIs, model sections and efforts.

Question text is composed per request from three sources:
- the stage's question (code);
- the state guide (code, chosen by the state's shape);
- the policy (config).

The config file shrinks to two short paragraphs. The comments that described state fields move into Go doc comments next to the constants.

## Technical Details
- **Config:** `Agrouter{..., RoutingPolicy, ComplexityPolicy string}`, replacing `Question`, `ChunkQuestion`, `Relevance`, `ComplexityQuestion` and `ComplexityEvidence`. `decodeAgrouter` maps each removed key to a `removedKey(k, hint)` error that carries the layer path, like `unknownKey`.
- **Levels:**
  ```go
  type level struct {
      name     string                      // "cli", "model", "effort": question id, confidence key, debug label
      key      func(catalog.Option) string // group key
      label    func(catalog.Option) string // criterion name
      question string                      // code-owned stage question
  }
  ```
  `groups(opts, lv)` returns the distinct criterion names in catalog order, with each group's options.
- **Effort labels:** the effort stage's criteria are effort labels, and its group keys are option IDs. Both the whole-state validation (`ask`) and the pooled path (`askChunk`, `pool`) check probabilities against the labels sent, then map the winning label to its options through the same `groups` table.
- **Decision:**
  ```go
  type Stage struct {
      Level   string
      Choice  string      // criterion name chosen, or the shared value when skipped
      Skipped bool        // one group: Jev not asked
      Answer  *jev.Answer // whole-state answer; nil when skipped or pooled
      Pooled  *Pooled     // split-state answers; nil otherwise
  }
  ```
  `Decision.Stages []Stage` replaces `Decision.Answer` and `Decision.Pooled`. `Decision.Complexity` is unchanged.
- **Pooling:** `Score{ID, Score}` already names by string. `pool`, `pooledScores` and `ranked` take the level's ordered criterion names instead of `[]catalog.Option`, and `askChunk` checks probabilities against those names.
- **Failure:**
  - `decide` returns the completed stages together with the error. `Route` attaches them to the cannot-decide `Decision` built from the original eligibility.
  - When cannot-decide ends in `ErrCannotDecide`, `Route` returns that partial `Decision` beside the error.
  - `app.route` passes it on, and `app.run` logs it with `debug` before `fail`. stdout stays empty.
- **Budget:** `prompt.Questions` gets:
  - `Route`: the max over the three stage questions;
  - `ChunkRoute`: the max over the chunk-stage questions;
  - `Relevance`, `Complexity` and `Evidence` from the composed questions.

  `NewBudget`'s errors name `routing_policy` or `complexity_policy`.
- **Confidence JSON:**
  ```json
  {"cli": 0.82, "model": 0.64, "effort": 0.41, "project_complexity": 0.9}
  ```
  `--verbose` adds `stages`: for each stage its `level`, `choice` and `skipped`, plus `confidence`, `probabilities` or `chunks` and `pooled_scores` when it was asked. This replaces the top-level `route`, `choice`, `probabilities`, `routing_chunks` and `pooled_scores`.

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, goldens, README, CLAUDE.md and AGENTS.md in this repo.
- **Post-Completion** (no checkboxes): real-API evaluation runs and updating consumers that read `confidence.model_selection`.

## Implementation Steps

### Task 1: Remove the full encoding

**Files:**
- Modify: `pkg/router/question.go`, `pkg/router/router.go`, `pkg/router/pool.go`
- Modify: `cmd/agrouter/app.go`
- Modify: `pkg/router/evalcase_test.go`, `pkg/router/eval_test.go`, `pkg/router/evalcase_load_test.go`
- Modify: `pkg/router/router_test.go`, `pkg/router/question_test.go`
- Delete: `pkg/router/testdata/request_full_*.json`

- [x] delete `Encoding`, `EncodingFull`, `fullRouteQuestion`, `Router.enc` and the `enc` parameter from `New`, `routeQuestion` (also the call in `pool.go:80`) and `budget`; update the call site in `cmd/agrouter`
- [x] remove the encoding loop from the eval test, keeping accuracy, confidence distribution and latency reporting
- [x] delete the full-encoding goldens and their test cases
- [x] update the remaining tests that passed an encoding
- [x] run `make test` and `go vet -tags eval ./pkg/router` - must pass before task 2

### Task 2: Replace the question keys with `routing_policy` and `complexity_policy`

**Files:**
- Modify: `pkg/config/config.go`, `pkg/config/load.go`, `pkg/config/validate.go`
- Modify: `pkg/config/defaults/config`
- Modify: `pkg/prompt/budget.go`
- Modify: `pkg/router/question.go`, `pkg/router/complexity.go`, `pkg/router/pool.go`, `pkg/router/router.go`
- Modify: `pkg/config/config_test.go`, `pkg/config/validate_test.go`, `pkg/catalog/catalog_test.go`, `pkg/args/build_test.go`, `pkg/prompt/chunk_test.go`
- Modify: `pkg/router/question_test.go`, `pkg/router/router_test.go`, `pkg/router/pool_test.go`, `pkg/router/evalcase_test.go`, `pkg/router/evalcase_load_test.go`, `pkg/router/testdata/*.json`

- [x] replace the five question fields with `RoutingPolicy` and `ComplexityPolicy`; decode the new keys; validate both as non-empty
- [x] make each removed key an error that names its replacement and the layer path (`removedKey`)
- [x] rewrite `pkg/config/defaults/config`: two policy paragraphs holding only the preference; move the state-field comments into Go doc comments
- [x] add code constants in `pkg/router/question.go`: whole/chunk/doc state guides, the joint route question (replaced in task 4), the relevance and evidence questions, the complexity question
- [x] compose the route, chunk route and complexity instructions as `{question, state, policy, …}`
- [x] rename the old keys in the `NewBudget` errors (`pkg/prompt/budget.go:64,67`) and the router wrap (`question.go:225`)
- [x] switch every synthetic fixture listed in Files to the two policy keys
- [x] write config tests: new keys load and layer; each removed key errors with its hint and path; an empty policy is rejected
- [x] rewrite `TestNewQuestionOverBudget` for `routing_policy` and `complexity_policy`; update the budget error assertions in `pkg/prompt/chunk_test.go`
- [x] write question tests for the composed instructions with a synthetic config; regenerate goldens with `go test ./pkg/router -update` and review the diff
- [x] run `make test` - must pass before task 3

### Task 3: Pool and rank by criterion name (refactor, no behavior change)

**Files:**
- Modify: `pkg/router/pool.go`, `pkg/router/router.go`
- Modify: `pkg/router/pool_test.go`

- [ ] make `pool`, `pooledScores`, `ranked` and `askChunk` take ordered criterion names instead of `[]catalog.Option`, mapping the winner back to its option at the call site
- [ ] keep the joint question and the outputs unchanged; the goldens must not change
- [ ] adapt the `pool_test.go` unit tests to the new signatures; add a case pooling names that aren't option IDs
- [ ] run `make test` - must pass before task 4

### Task 4: Staged routing for whole and split states

**Files:**
- Create: `pkg/router/stage.go`, `pkg/router/stage_test.go`
- Modify: `pkg/router/router.go`, `pkg/router/question.go`, `pkg/router/pool.go`
- Modify: `cmd/agrouter/app.go`, `cmd/agrouter/confidence.go`, `cmd/agrouter/debug.go` (mechanical only, to compile and keep today's output shape)
- Modify: `pkg/router/router_test.go`, `pkg/router/pool_test.go`, `pkg/router/project_test.go`, `pkg/router/evalcase_test.go`, `pkg/router/evalcase_load_test.go`
- Modify: `cmd/agrouter/app_test.go`, `cmd/agrouter/confidence_test.go`, `cmd/agrouter/debug_test.go`
- Modify: `pkg/router/testdata/*.json`

- [ ] add `level`, the three levels (`cli`, `model`, `effort`) and `groups(opts, level)` in `stage.go`
- [ ] build each stage's question (question id = level name) with the descendant subtree in the instructions, replacing the joint route question for whole and chunk states
- [ ] rewrite `decide`: loop over the levels on a private copy of the options. Skip levels with one group, recording the shared value. Otherwise ask, by `ask` for a whole state or by fan-out, relevance and pooling for a split state, then narrow every chunk identically. Record `Stage` entries; `Decision.Stages` replaces `Answer`/`Pooled`.
- [ ] map effort labels to options through `groups` in both `ask` and the pooled path
- [ ] on any stage error, return the completed stages with the error. `Route` applies `cannotDecide` against the original eligibility, attaches the stages, and returns the partial `Decision` beside `ErrCannotDecide`. A Jev-chosen CLI never sets `Pinned`.
- [ ] make the one-option bypass fill three skipped `Stage` records (no Jev call, no complexity stage)
- [ ] derive the budget per the Decisions rule: `cli` over the whole catalog, `model` max per CLI, `effort` max per model; the chunk variants paired with relevance
- [ ] mechanically adapt the consumers (`cmd/agrouter/confidence.go`, `debug.go`, `pkg/router/evalcase_test.go`) to read `Stages`. Output stays as close to today's as possible; the redesign is task 6 and the eval reporting is task 7.
- [ ] redesign `fakeJev` (`app_test.go:59-137`) and `evalMock` (`evalcase_load_test.go:22-48`) to take a target option ID and answer each stage with its CLI, section or effort label; update the single-request assertions to the per-stage sequence
- [ ] write tests for `groups` and level skipping: one CLI, one model, a no-effort model, model and effort passthrough, and the skipped-stage choice for each
- [ ] write tests for passed values fixing their stage:
  - `--cli`: only model and effort asked;
  - catalog `--model`: only effort asked;
  - passthrough `--model` with several CLIs: only cli asked, every criterion carrying the passed model;
  - `--effort`: effort never asked; criteria carry the passed effort;
  - all three passed: Jev not called, three skipped stages recorded;
  - an unusable `--cli`: warning kept, `cli` stage asked.
- [ ] write tests for stage sequencing with the moq mock, using synthetic fixtures: request order, question id and criteria per stage, narrowing, and a final option matching the last choice
- [ ] write failure tests:
  - multi-CLI → CLI succeeds → model fails: exit-2 error, stages kept;
  - model succeeds → effort fails;
  - one CLI eligible → runs with caller-fixed values only;
  - `Pinned` stays false.
- [ ] write split-state tests: per-level pooling with relevance weights and all-zero relevance; all chunks narrowed the same; a chunk failure in a later level fails the routing with earlier stages kept
- [ ] add per-stage goldens (`request_stage_cli.json`, `request_stage_model.json`, `request_stage_effort.json`, chunk and passthrough variants) and regenerate
- [ ] run `make test` - must pass before task 5

### Task 5: Staged routing edge cases: 422s, deadline, malformed answers, budget

**Files:**
- Modify: `pkg/router/stage.go`, `pkg/router/pool.go`, `pkg/router/question.go` (fixes only, if the tests find gaps)
- Modify: `pkg/router/stage_test.go`, `pkg/router/pool_test.go`, `pkg/router/router_test.go`

- [ ] write tests for a whole-state 422 at each level: re-split at half the chunk budget, pooled, routing continues to the next level
- [ ] write tests for a chunk 422 in a middle level: only that chunk re-splits, the pool still covers every chunk; a second 422 fails the routing with earlier stages kept
- [ ] write a deadline test: the context expires after the `cli` stage, the `model` stage fails, cannot-decide runs, and the completed stages are kept
- [ ] write malformed-answer tests for each level: a choice not among the criteria sent, a missing probability for a criterion (including an effort label), a missing answer id
- [ ] write budget tests with synthetic catalogs where the `model` or `effort` question is the largest, and an over-budget `routing_policy` and `complexity_policy`
- [ ] run `make test` - must pass before task 6

### Task 6: Per-stage confidence in the decision JSON, exec log and debug output

**Files:**
- Modify: `cmd/agrouter/confidence.go`, `cmd/agrouter/app.go`, `cmd/agrouter/debug.go`
- Modify: `cmd/agrouter/confidence_test.go`, `cmd/agrouter/app_test.go`, `cmd/agrouter/debug_test.go`

- [ ] replace `model_selection` with `cli`, `model` and `effort`, omitting skipped stages and all three when the routing is undecided; a pooled stage reports the mean of its chunk confidences
- [ ] replace the verbose top-level route, chunk and pooled fields with `stages`; skipped stages appear with `skipped: true` and their choice; update the `optionJSON` comment (`app.go:74`)
- [ ] debug output prints one line per stage: `stage <level>: skipped (<choice>)`, or the choice, confidence and top 3 (per-chunk lines for pooled stages)
- [ ] carry the partial `Decision` on the exit-2 path: `app.route` returns it beside the error, and `app.run` logs it with `debug` before `fail`. stdout stays empty.
- [ ] the exec selection line carries the same confidence object, still without the prompt or argv
- [ ] write tests:
  - three asked stages;
  - skipped stages omitted from `confidence` but present in verbose;
  - pooled stages;
  - an undecided run with one CLI eligible: no stage keys in `confidence`, completed stages in verbose;
  - redaction unchanged.
- [ ] write a `cmd/agrouter` integration test: two CLIs eligible, the CLI stage succeeds, the model stage fails → exit 2, empty stdout, and the completed CLI stage in `AGROUTER_DEBUG=1` output
- [ ] run `make test` - must pass before task 7

### Task 7: Adapt the routing evaluation

**Files:**
- Modify: `pkg/router/evalcase_test.go`, `pkg/router/eval_test.go`, `pkg/router/evalcase_load_test.go`

- [ ] read the decision from `Decision.Stages`: correctness on `OptionID`, split detection from any pooled stage, per-stage confidence
- [ ] report accuracy, latency per case and in total, and the confidence distribution per stage (labelled conditional)
- [ ] update the untagged eval-case and load tests that run in `make test`; `eval_test.go` is the only `eval`-tagged file
- [ ] run `make test` and `go vet -tags eval ./...` - must pass before task 8

### Task 8: Verify acceptance criteria
- [ ] verify every Overview item: three stages with skipping, passed values fixing their stage, two policy keys, removed-key errors, per-stage confidence
- [ ] verify edge cases: one option, `--cli`, `--model` and `--effort` passthrough, a complexity stage before routing, a 422 at every level, the deadline expiring mid-stage, an undecided run with completed stages
- [ ] run the full test suite: `make test` (and `make test RACE=-race` where cgo is available)
- [ ] run `make lint` and lint with `GOOS=linux`, `GOOS=darwin` and `--build-tags=eval`
- [ ] verify coverage is 80%+ per package (mocks excluded)

### Task 9: [Final] Update documentation
- [ ] README: Configuration (`routing_policy`, `complexity_policy`, the removed-key migration table), staged routing and how passed flags fix stages, the per-stage `confidence` shape, verbose `stages` and `options`, latency and when to raise `timeout`, the exit-code table unchanged
- [ ] CLAUDE.md and AGENTS.md: question structure lives in `pkg/router/question.go` and config holds only policy; routing is staged
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- run `make eval-routing` with `TYPESAFE_API_KEY` and compare accuracy, per-stage confidence and latency with a run of the joint question on `master`
- try a long prompt and `--doc` under the default 10s timeout; raise the default if staged routing regularly exceeds it

**External system updates:**
- consumers reading `confidence.model_selection` (ralphex step logs, scripts) switch to `confidence.cli` / `model` / `effort`
- users with `question`, `chunk_question`, `relevance`, `complexity_question` or `complexity_evidence` in a global or local config move their preference text into `routing_policy` / `complexity_policy`
