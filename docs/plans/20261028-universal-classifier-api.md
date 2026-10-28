# Universal Classifier API

## Overview
- agrouter talks to one hard-wired routing provider today: TypeSafe's Jev. Its URL is a Go constant (`pkg/jev/client.go:20`), and its name runs through config (`jev_model`), the flag (`--jev-api-key`), the env var (`TYPESAFE_API_KEY`), reason codes (`jev_*`), the package (`pkg/jev`), help text and docs.
- Goal: make the routing API universal so other classifier sources (OpenAI Luna and others) can be used. The user configures the classifier in a new `[classifier]` section: `url`, `api_key`, `model`, plus the provider-dependent sizing keys `long_context`, `request_tokens` and `bytes_per_token`, which move there from `[agrouter]`. Switching providers then means overriding one section.
- The request and response structure stays exactly the one Jev uses today. A source that accepts that wire format works by changing `url` and `model`.
- Jev stays the default: the embedded config ships the Jev `url` and `model`.
- No "jev" naming remains in production Go code outside the embedded defaults. The default model id `jev-latest` is data, not naming.

```
before                                   after
------                                   -----
[agrouter] api_key, jev_model,           [classifier] url, api_key, model,
  long_context, request_tokens,            long_context, request_tokens,
  bytes_per_token                          bytes_per_token
--jev-api-key, TYPESAFE_API_KEY          --api-key, AGROUTER_CLASSIFIER_API_KEY
pkg/jev (DefaultURL const)       ──►     pkg/classifier (url from config)
router.JevClient                         router.Classifier
reason jev_*                             reason classifier_*
```
**Note:** every Jev-named piece on the left gets a neutral name on the right, and everything that depends on the provider moves into `[classifier]`. The JSON sent over the wire does not change. `[agrouter]` keeps `timeout`, `routing_policy`, `complexity_policy` and `log_dir`.

## Context (from discovery)
- files/components involved:
  - `pkg/config/{config,load,validate,install}.go`, `pkg/config/defaults/config`: `[agrouter] api_key`/`jev_model`, `EnvAPIKey = "TYPESAFE_API_KEY"`, `ResolveAPIKey`, generated global config
  - `pkg/jev/{client,types}.go` (+ tests): client, `DefaultURL`, sentinels (`ErrNoKey` = "no Jev API key"), `Limits`, long-context header
  - `pkg/router/{router,stage,pool,complexity,question,eligibility}.go`, `pkg/router/mocks/jev_client.go`, golden `pkg/router/testdata/request_*.json`
  - `cmd/agrouter/{app,flags,reason,fallback,debug,confidence,readiness,main}.go`: wiring, `--jev-api-key`, `jev_*` codes, "Jev routing failed" warning, help text
  - `pkg/runner/runner.go:118`: `childEnv` strips the API key env var from the child
  - comments only: `pkg/catalog/catalog.go`, `pkg/prompt/{capture,chunk,mentions}.go`, `pkg/args/request.go`
  - `Makefile`, `WMakefile` (`eval-routing` needs `TYPESAFE_API_KEY`), `README.md` (53 mentions), `CLAUDE.md`, `AGENTS.md`, `.agrouter/config`
- related patterns found:
  - unknown config keys are config errors that never echo the value (`pkg/config/load.go:475`, `:524`)
  - `cmd/agrouter/guard_test.go` scans production Go code for forbidden names
- dependencies identified: one-way `config` <- `catalog` <- `router`; `classifier` replaces `jev` with the same position (imported by `router` and `cmd/agrouter`, never by `readiness`)

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
- run tests after each change (`make test`, `make lint`)
- backward compatibility: **none, by user decision**. Previous versions' keys, flag and env var get no special handling, warnings or migration notes.

## Testing Strategy
- **unit tests**: required for every task (see Development Approach above)
- **e2e tests**: no UI. The end-to-end tests in `cmd/agrouter` (test binary as fake CLI, `httptest` classifier) cover flag, env and reason-code changes
- tests use synthetic names (for example model `test-classifier`, an `httptest` URL), never the shipped defaults

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

```
config layers ──► [classifier] url, api_key, model ──► classifier.New(url, key)
                                                            │
flag --api-key ──────────────────┐                          ▼
env AGROUTER_CLASSIFIER_API_KEY ─┴─► ResolveAPIKey ──► POST url  {model, state, questions}
                                                            │
                                    router.Classifier ◄─────┘  same Request/Response types
```
**Note:** the URL now comes from config like everything else. The key precedence and the request body stay as they are.

Key decisions (debated with the partner agent, p2, then settled by the user):

| Decision | Choice | Why |
|---|---|---|
| Section shape | new `[classifier]` with `url`, `api_key`, `model`, `long_context`, `request_tokens`, `bytes_per_token` | all of these depend on the provider (Luna takes ~8x Jev's input; tokenizers differ), so one section switches providers. `[agrouter]` keeps routing behavior: `timeout`, the two policies, `log_dir` (user) |
| Previous versions | not supported: removed keys are simply unknown keys (`pkg/config/load.go:478`); no aliases, warnings or hints | user decision |
| Package | `pkg/jev` -> `pkg/classifier`; `JevClient` -> `Classifier` | neutral naming. The wire types stay unchanged |
| URL default | only in `pkg/config/defaults/config`; `DefaultURL` const removed | same rule as catalog names: provider data lives only in config |
| Flag / env | `--api-key`, `AGROUTER_CLASSIFIER_API_KEY` | a provider-specific env name is wrong for other sources |
| Reason codes | `jev_*` -> `classifier_*` | decision-JSON contract change, called out in README |
| `long_context`, `Limits`, startup limit check | **behavior kept as is** (user); the key moves to `[classifier]`. The header is TypeSafe's, and other providers ignore it. The startup check uses TypeSafe's limits for **every** endpoint (`pkg/router/router.go:61` has no endpoint condition), and this is documented | user decision. The plan does not try to make limits generic |
| `max_tokens_exceeded` -> `request_too_long`, 429/529 retry | kept | protocol behavior, invents no capacity |
| `url` validation | any value is allowed; only an empty url is a config error | the user owns the url (user) |

## Technical Details

Embedded defaults (`pkg/config/defaults/config`):

```ini
# [classifier]: the routing classifier agrouter asks which (cli, model, effort) runs a prompt, and
# how big one routing request may be. any source accepting the same request body works.
#   url              endpoint the routing requests are POSTed to. required
#   api_key          bearer key for the endpoint. default: empty (no key)
#   model            the classifier model answering the routing questions. required
#   long_context     use the provider's long-input mode. default: false
#   request_tokens   token allowance of one routing request: its state plus all its questions. default: 30000
#   bytes_per_token  estimated bytes per token, used to size routing requests. default: 3
# precedence for api_key: --api-key > non-empty AGROUTER_CLASSIFIER_API_KEY > local > global > embedded.
[classifier]
url     = https://api.typesafe.ai/v1/systemone
api_key =
# pin a versioned id (e.g. jev-1.13.0) to keep routing stable across releases
model   = jev-latest
# (the existing request_tokens / bytes_per_token explanation moves here unchanged)
request_tokens  = 30000
bytes_per_token = 3
# long_context works only with Jev (TypeSafe): it sends TypeSafe's long-context header; other
# providers ignore it. the startup check of request_tokens uses Jev's limits for every url:
# at most 30000 with long_context = false, 260000 with long_context = true.
long_context    = false

# example: OpenAI Luna (Decisions API), unverified: it works only if the endpoint accepts this
# request body. put it in the global config, not a committed local one. Luna takes up to 922K
# input tokens and bills inputs over 272K at 2x, so 250000 stays under that step with margin;
# long_context = true only lifts agrouter's startup check. larger requests take longer: raise
# [agrouter] timeout too (e.g. 120s).
# [classifier]
# url            = https://api.openai.com/v1/decisions
# api_key        = sk-...
# model          = gpt-6-luna
# long_context   = true
# request_tokens = 250000
```

Config struct:

```go
type Config struct {
	Agrouter   Agrouter
	Classifier Classifier
	// ... CLIs, Models, Efforts unchanged
}

// Classifier is the [classifier] section: the endpoint routing requests are sent to, and how big
// one request may be.
type Classifier struct {
	URL          string
	APIKey       string
	APIKeySource string // layer that set api_key, one of the Layer* names
	Model        string
	LongContext  bool    // moved from Agrouter, doc comment kept
	RequestTokens int    // moved from Agrouter, doc comment kept
	BytesPerToken float64 // moved from Agrouter, doc comment kept
}
```

`Agrouter` loses `APIKey`, `APIKeySource`, `JevModel`, `LongContext`, `RequestTokens`, `BytesPerToken`. `ResolveAPIKey` takes `Classifier`. `router.checkLimits` takes `config.Classifier`. Error texts that name the moved keys say `[classifier]` (`router.go:63`, `question.go:322`, `validate.go:52,55`).

Previous versions are not supported: an old key is an ordinary unknown key, the old flag an unknown flag, and the old env var is simply not read.

Reason codes: `classifier_no_key`, `classifier_unauthorized`, `classifier_timeout`, `classifier_canceled`, `classifier_api_error`, `classifier_transport_error`, `classifier_invalid_answer`, `classifier_empty_answer`. `request_too_long` stays as is.

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, docs in this repo
- **Post-Completion** (no checkboxes): a live check against OpenAI's endpoint

## Implementation Steps

### Task 1: Add the [classifier] config section

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/config/load.go`
- Modify: `pkg/config/validate.go`
- Modify: `pkg/config/defaults/config`
- Modify: `.agrouter/config` (tracked local copy: its active `jev_model` at line 35 would fail a branch build from this task on)
- Modify: `pkg/config/config_test.go`, `pkg/config/validate_test.go`, `pkg/config/install_test.go`, `pkg/config/defaults/defaults_test.go`
- Modify: `cmd/agrouter/app.go` (`:264` long context), `pkg/router/{stage,pool,complexity}.go` (`cfg.Agrouter.JevModel` -> `cfg.Classifier.Model`), `pkg/router/router.go` (`:48`, `checkLimits` `:60-64`), `pkg/router/question.go` (`:319`, `:322`); only the fields move, no renames yet
- Modify: every test fixture and assertion on the moved fields: `pkg/router/router_test.go:32,204-220`, `pkg/router/overflow_test.go:53-54,116`, `pkg/router/complexity_test.go:168`, `pkg/router/question_test.go:65`, `pkg/config/config_test.go:211-246`, `cmd/agrouter/app_test.go:595` (keep the test model *value* unchanged until Task 3, so golden files do not move yet)

- [ ] add `Classifier` struct and `Config.Classifier`; move `LongContext`, `RequestTokens`, `BytesPerToken` (with their doc comments) from `Agrouter` into it; remove `APIKey`, `APIKeySource`, `JevModel` from `Agrouter`
- [ ] decode `[classifier]` keys `url`, `api_key`, `model`, `long_context`, `request_tokens`, `bytes_per_token` in `load.go`, tracking `APIKeySource` per layer as today
- [ ] drop the `api_key`, `jev_model`, `long_context`, `request_tokens`, `bytes_per_token` cases from the `[agrouter]` decoder (`load.go:246-261`)
- [ ] validate `url` and `model` as required (non-empty), any url value accepted; move the `request_tokens` / `bytes_per_token` checks (`validate.go:51-55`) to `[classifier]` with the section name in their messages
- [ ] `checkLimits` takes `config.Classifier`; the `[agrouter]` prefix in its error (`router.go:63`) and in the budget error (`question.go:322`) becomes `[classifier]`
- [ ] `ResolveAPIKey` reads `Classifier`. `EnvAPIKey` keeps its current name until Task 4, so `pkg/runner`'s `TestChildEnv` (`runner_test.go:251-257`) stays green and the key is still stripped from children
- [ ] update `.agrouter/config` to the new `[classifier]` section
- [ ] write the `[classifier]` block with the moved sizing keys and their comments, the Jev-only `long_context` note and the commented OpenAI Luna example in `defaults/config`; drop the moved keys from the `[agrouter]` block and its key reference (`install.go` needs no change: it comments out the embedded config generically, `pkg/config/install.go:28-33`)
- [ ] move the field at the call sites in `cmd/agrouter/app.go` and `pkg/router`, and in all their tests, so the tree compiles and tests pass (no renames there yet)
- [ ] write tests: `[classifier]` loading and layering for all six keys, key precedence (flag > env > local > global > embedded)
- [ ] write tests: empty `url`, empty `model`, invalid `request_tokens` / `bytes_per_token` under `[classifier]`, the over-limit startup error naming `[classifier]`
- [ ] run tests - must pass before next task

### Task 2: Rename pkg/jev to pkg/classifier

**Files:**
- Create: `pkg/classifier/{client,types}.go`, `pkg/classifier/{client_test,score_test}.go` (moved with `git mv`)
- Delete: `pkg/jev/`
- Modify: every `pkg/jev` import: `cmd/agrouter/{app,reason}.go` (`reason.go:7`) and cmd tests, `pkg/router/*.go` and tests, `pkg/router/mocks/jev_client.go:8` (import path only; the mock is renamed in Task 3), `pkg/router/eval_test.go:26` (constructor)
- Modify: `pkg/catalog/catalog.go` (comment and error text only, `:13`, `:53`), `pkg/catalog/catalog_test.go` (`:144` asserts the old text)

- [ ] `git mv pkg/jev pkg/classifier`, package `classifier`; update every import listed above so the tree stays green
- [ ] remove `DefaultURL`; `New(url, key string)` sets `Client.URL`
- [ ] in `cmd/agrouter/app.go` rename `newJev`/`newJevClient` (`:42`, `:59`, `:65-66`) to `newClassifier`/`newClassifierClient` and thread the url through the injected factory: `func(url, key string, longContext bool)`; the call at `:264` passes `cfg.Classifier.URL`
- [ ] neutral sentinel texts (`ErrNoKey`: "no classifier API key") and godoc; comment the long-context header and `Limits` with TypeSafe wording (they are TypeSafe's), and state that the startup limit check applies to every endpoint
- [ ] neutralize the `jev:` error prefixes in the client (`client.go:101,160,170,438,444,448`) and their assertions (`client_test.go:533-534`)
- [ ] `catalog.MaxOptions` comment and error text say "the classifier's limit"; update `catalog_test.go:144`
- [ ] update tests: client uses the configured URL (`httptest` server), the factory receives the configured url, sentinel matching unchanged
- [ ] `go vet -tags eval ./pkg/router` passes (`eval_test.go` is not compiled by `make test`)
- [ ] run tests - must pass before next task

### Task 3: Rename the router's client interface

**Files:**
- Modify: `pkg/router/{router,stage,pool,complexity,question,eligibility}.go`
- Delete: `pkg/router/mocks/jev_client.go`
- Create: `pkg/router/mocks/classifier.go` (generated)
- Modify: `pkg/router/*_test.go`, `pkg/router/testdata/request_*.json`
- Modify: `cmd/agrouter/app.go` (`:42`, `:59` factory return type), `cmd/agrouter/app_test.go` (`:364`)

- [ ] `JevClient` -> `Classifier`, field `r.jev` -> `r.classifier`, `go:generate` directive, `make generate`
- [ ] update the `router.JevClient` references in `cmd/agrouter/app.go` and `app_test.go` in the same task, so `cmd` still compiles
- [ ] neutral comments and error texts in `pkg/router` (`ErrCannotDecide` "jev cannot decide" and "without Jev", `router.go:25,160`); update the cmd tests asserting that text in this same task: `cmd/agrouter/app_test.go:751`, `debug_test.go:118,208`, `reason_test.go:263` (only these router-error assertions; reason codes stay for Task 4)
- [ ] golden fixtures use a synthetic model (`test-classifier`); regenerate with `go test ./pkg/router -update` and check the diff only changes the model value
- [ ] update router tests for the renamed mock
- [ ] run tests - must pass before next task

### Task 4: Rename the flag, env var and reason codes in the CLI

**Files:**
- Modify: `cmd/agrouter/{flags,reason,fallback,debug,confidence,readiness,main,app}.go`
- Modify: `pkg/args/request.go`, `pkg/runner/runner.go`
- Modify: `cmd/agrouter/*_test.go`, `pkg/runner/runner_test.go`, `pkg/args/build_test.go`
- Modify: `pkg/config/load.go` (`EnvAPIKey`), `Makefile`, `WMakefile`

- [ ] rename `EnvAPIKey` to `AGROUTER_CLASSIFIER_API_KEY` and update the godoc precedence text, in the same task as `childEnv` below
- [ ] `eval-routing` in `Makefile` and `WMakefile` checks `AGROUTER_CLASSIFIER_API_KEY`
- [ ] `--jev-api-key` -> `--api-key` (`flags.go`, `args.Request.APIKey` comment); its description says it is the routing classifier's key (in exec, a child's own `--api-key` goes after `--`); help text says "a routing classifier" instead of "TypeSafe's Jev"
- [ ] reason codes `jev_*` -> `classifier_*` in `reason.go`; fallback warning "Jev routing failed" / "not a Jev selection" -> neutral wording; debug output labels
- [ ] `runner.childEnv` strips `AGROUTER_CLASSIFIER_API_KEY` (case-insensitive on Windows, as today)
- [ ] write tests: new flag and env precedence end to end, `classifier_*` codes in decision JSON, child env without the key
- [ ] run tests - must pass before next task

### Task 5: Guard against jev naming and sweep comments

**Files:**
- Modify: `cmd/agrouter/guard_test.go`
- Modify: `pkg/prompt/{capture,chunk,mentions}.go`, `pkg/config/config.go:80`, remaining comment hits the guard reports

- [ ] add `TestGuard_NoJevNamingInProduction`: case-insensitive `jev` must not appear in non-test Go files outside `pkg/config/defaults`, with no exceptions
- [ ] reword remaining comments ("Jev's prompt" -> "the classifier's prompt", etc.); provider-specific comments use "TypeSafe" wording, until the guard passes
- [ ] run tests - must pass before next task

### Task 6: Verify acceptance criteria
- [ ] `[classifier] url/api_key/model` drive the request and its sizing keys drive the budget; `[agrouter]` holds only `timeout`, the two policies and `log_dir`; embedded defaults are Jev
- [ ] request body unchanged: golden fixtures differ only in the synthetic model value
- [ ] `grep -ri jev --include='*.go' cmd pkg` hits only tests and `pkg/config/defaults`
- [ ] run full test suite: `make test`
- [ ] run lint: `make lint`, also `GOOS=linux`, `GOOS=darwin` and `--build-tags=eval`
- [ ] verify coverage stays 80%+ per package

### Task 7: [Final] Update documentation
- [ ] README: `[classifier]` section with the sizing keys, OpenAI Luna example, new flag/env, `classifier_*` reason codes (no migration notes)
- [ ] CLAUDE.md and AGENTS.md: `pkg/classifier` layout line, `router.Classifier`, `classifier` sentinels, env var, the no-jev guard rule, and the "routing request size is configured" rule naming `[classifier] request_tokens`/`bytes_per_token` instead of `[agrouter]`
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- run `make eval-routing` against Jev with `AGROUTER_CLASSIFIER_API_KEY` to confirm nothing changed on the wire
- try the OpenAI Luna example against the live endpoint. Public reports say OpenAI's Decisions API takes `input` and a `questions` *array*, while agrouter sends `state` and a `questions` *map*. If they differ, Luna needs a request adapter, which would be a separate plan