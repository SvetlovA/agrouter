# agrouter v1 first release

## Overview
- Implement agrouter v1 as specified in `docs/design.md` (commit `fa38f4b`, plus the two corrections committed with this plan): a Go CLI that asks TypeSafe's Jev which `(cli, model, effort)` should run a prompt. It then prints that decision (decision mode) or runs the chosen CLI with agrouter's arguments translated through config (exec mode).
- Solves: ralphex (and any caller) running one fixed model for every prompt. agrouter picks a cheaper model for trivial work and a stronger one for hard work, per call, without the caller knowing each CLI's flags.
- Integrates as a drop-in `claude_command` (ralphex Claude mode, `--cli=claude`) and `codex_command` (ralphex Codex mode and the external Codex review), and as a standalone router for scripts (decision mode JSON).
- Ships with GitHub Actions CI (tests on Linux, Windows and macOS, and lint) and a tag-driven release for `go install github.com/SvetlovA/agrouter/cmd/agrouter@<tag>`. No binary artifacts in this release.

## Context (from discovery)
- Repository today: `docs/design.md` (the full design), a stub `README.md`, `LICENSE`. No Go code yet. Module path: `github.com/SvetlovA/agrouter`.
- Design decisions settled in the design discussions (do not reopen during implementation):
  - Argument vocabulary follows Claude Code. Every mapped argument goes through the chosen CLI's `[cli.*.args]` templates (JSON string arrays, no shell).
  - `print` is a required mapping, always emitted first. `{prompt}` (positional prompt only) sits in `print` or the optional `prompt` key, and is dropped to zero tokens when there is none. Stdin passes to the child untouched. Only Jev's `prompt` joins positional and stdin with `\n\n`.
  - `--output-format text|json|stream-json`, `--permission-mode` (Claude names; `--dangerously-skip-permissions` and `--dangerously-bypass-approvals-and-sandbox` alias `bypassPermissions`), and Codex's `--skip-git-repo-check` living in the permission mappings. Approximate Codex modes are marked ≈.
  - ralphex Codex mode: `--sandbox` (value-keyed) and `-c key=value`. `-c` is keyed by config key (`config.<key>`) against a whitelist of ralphex's keys. `-c model=` and `-c model_reasoning_effort=` are constraints unless `--model`/`--effort` is also given.
  - An argument the chosen CLI does not map, or maps to `[]`, is **skipped with a stderr warning** (and a `skipped` array in decision JSON), never an error. Routing prefers the CLIs that skip the fewest arguments. Only the prompt is required.
  - A model or effort outside the catalog, and contradictory arguments, are **passed through** for the real CLI to validate.
  - Still exit `2`: an unknown flag, a second positional, no prompt, config errors, and Jev unable to decide while more than one CLI remains. Exit `127` when the child can't start; otherwise the child's exit code.
  - `exec` selects exec mode only as the first token; elsewhere it is an ordinary word.
  - Catalog: Claude 5 family plus Haiku 4.5 (16 options), and GPT-6 only for Codex (17 options), 33 in total.
  - No output translation. Per-call CLI choice under ralphex needs a ralphex change (design open question 8), which is out of scope.
- Reference patterns (ralphex, `~/Projects/worktrees/ralphex/windows-release`):
  - `.github/workflows/ci.yml`: `go test -race` with coverage, golangci-lint `v2.13.0` pinned, goveralls with `continue-on-error`.
  - `.golangci.yml`: the linter set to copy.
  - `Makefile`: `build`, `test`, `lint`, `fmt` targets.
  - `release.yml`: validates the tag format and branch ancestry before releasing. agrouter keeps the validation but drops goreleaser.
  - `pkg/execx/execx.go`: resolves Windows `.cmd`/`.bat` shims and quotes arguments for them.
  - `pkg/executor/procgroup_windows.go`: Job Object with kill-on-close.
- Dev machine facts:
  - Windows 11 with Go 1.27.1. `CGO_ENABLED=0` and no `gcc`, so `-race` is unavailable locally.
  - `moq` is not installed.
  - golangci-lint 2.12.2 locally vs `v2.13.0` in CI.
  - `claude.exe` and `codex.exe` are native executables here, but npm installs elsewhere use `.cmd` shims.
- Dependencies: Go 1.26 (`go.mod`, no `toolchain` line), `github.com/jessevdk/go-flags`, `gopkg.in/ini.v1`, `github.com/stretchr/testify`, `golang.org/x/sys` (Windows Job Object, Unix process groups), `github.com/matryer/moq` as a `go tool` (pinned in `go.mod`). Dependencies are vendored.

## Development Approach
- **testing approach**: Regular (code first, then tests in the same task)
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
- **CRITICAL: `docs/design.md` is the specification.** When implementation forces a deviation, update the design in the same task and note it here with ➕ or ⚠️.
- run `make test` and `make lint` after each change (locally without `-race`, since cgo is unavailable; CI runs `-race`)
- table-driven tests with testify; coverage target 80%+ per package (mocks excluded)

## Testing Strategy
- **unit tests**: required for every task (see Development Approach above), following the design's "Testing" section package by package.
- **child-process tests**: `pkg/runner` and the `cmd/agrouter` end-to-end tests use a helper process built from the test binary (`os.Args[0]` with `GO_WANT_HELPER_PROCESS`) as a fake `claude`/`codex`/made-up CLI, so no real agent CLI is needed. It records its argv, stdin and environment for assertions.
- **Jev tests**: `httptest` servers for `pkg/jev`; moq mocks of the consumer-side `JevClient` and `CommandRunner` interfaces.
- **platform tests**: process groups (Unix) and Job Objects (Windows) behind build tags, each tested on its own OS in the CI matrix.
- **line endings**: `.gitattributes` forces LF so golden files, byte-exact fixtures and the embedded defaults are identical on Windows.
- **routing evaluation**: `make eval-routing` runs a build-tagged test against the real Jev API, never in CI (needs `TYPESAFE_API_KEY`).
- no UI, so no e2e browser tests. The manual ralphex end-to-end check is in Post-Completion.

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview
- **Pipeline** (design "Routing"): parse arguments → load and validate config → start the routing deadline → capture the prompt (positional, stdin, mentioned files, attachments) → eligibility (`--cli`, `--model`, `--effort`, then the mapped-argument preference) → Jev:
  - one option left: no Jev call;
  - the state fits: one Choice request;
  - too large, or a `422` on the single request: chunked requests pooled by relevance.
  Then build argv from templates, collecting skipped arguments, and print the decision JSON or run the child.
- **Package boundaries** follow the design's project layout: `cmd/agrouter` (flags, wiring, exit codes), `pkg/config` (+ `defaults/`), `pkg/catalog`, `pkg/args`, `pkg/prompt`, `pkg/jev`, `pkg/router`, `pkg/runner`.
  - Dependencies point one way: `config` ← `catalog` ← `router`; `prompt` depends on nothing internal; `cmd/agrouter` wires everything.
  - Checks that need later packages live where their inputs exist: option count in `catalog`, serialized-question budget in `router`.
- **CLI-agnostic core:** no production code outside `pkg/config/defaults` names a CLI. A repo-level guard test enforces it, with `--help` text excluded. A made-up CLI defined only in config must route, map and run end to end.
- **stdout discipline:** stdout carries only the decision JSON (and `--help`/`--version` output). stderr carries skipped-argument warnings, one-line `agrouter:` errors, and `AGROUTER_DEBUG=1` output with the prompt and API key redacted.
- **Release:** a `v0.x`/`v1.x` semver tag on `master` re-runs the tests and creates a GitHub Release with generated notes. `go install ...@<tag>` then resolves through the Go module proxy, and `agrouter --version` reports `debug.ReadBuildInfo().Main.Version`.

## Technical Details
- **Config layering:** embedded `pkg/config/defaults/config` < global `~/.config/agrouter/config` (`AGROUTER_CONFIG_DIR`) < local `.agrouter/config` < env, merged per section and per key; `ini.v1` with `IgnoreInlineComment: true`.
  - `[agrouter]` keys: `api_key`, `jev_model` (`jev-latest`), `timeout` (10s, the whole routing budget: capture, queueing, requests and retries), `max_chunks` (64), `chunk_parallel` (4), `relevance_floor` (0.05), `question`, `chunk_question`, `relevance`.
- **API key precedence:** `--jev-api-key` > `TYPESAFE_API_KEY` > local > global > embedded placeholder; an explicit empty flag (`--jev-api-key=`) clears it, so "unset" and "set to empty" must be distinguishable. It is removed from the child's environment and never printed.
- **Option ids:** `<model section>@<effort>`, or `<model section>` for models without efforts; `@` is reserved in section names.
- **Mapping keys:** a flag name without `--`; value-keyed as `<flag>.<value>` (`output-format.json`, `permission-mode.plan`, `sandbox.read-only`); `config.<key>` for `-c`; `model`, `effort`, `print` and `prompt` are special. Placeholders are `{model}`, `{effort}`, `{value}` (inside tokens) and `{prompt}` (whole token only).
- **argv order:** command, `print`, mapped args in the caller's order, model, effort, `prompt`, raw passthrough (only with `--cli`). Arguments that hit the same mapping key are emitted once. Order across different flags is kept by recording each flag as it is parsed (go-flags `func(string)` callbacks); slice fields alone lose it.
- **Decision JSON:** `{"cli","model","effort","argv","skipped"}` on one line; `effort` is `null` for models without efforts; when Jev cannot decide with the CLI known, the unchosen model/effort are `null`.
- **Prompt capture** (inside the routing deadline):
  - Read non-TTY stdin to EOF up to a text capture limit in bytes (derived from `max_chunks` and the chunk budget by the caller). The limit counts text only, after binary detection.
  - Binary stdin is detected from its prefix (`http.DetectContentType`, then NUL/invalid UTF-8 over the whole captured stdin). Its size comes from `stat` when stdin is a regular file, otherwise from counting while relaying; its content never counts against the text limit.
  - Mentioned files come from quoted, backtick and Markdown-link spans, then whitespace tokens, resolved inside the canonical working directory only.
- **Budget:** tokens ≈ UTF-8 bytes ÷ 3. The state budget is 30k minus the longest serialized question; 64k with all questions, counting the chunk envelope. The anchor is 4k with attachments capped at 1k (summarised by source+type). Chunks are cut on line and UTF-8 boundaries, and the anchor plus chunks cover every captured byte.
- **Pooling:** per chunk, a Choice over the same options plus a relevance Noul. The weight is max(noul, `relevance_floor`), the pooled score is the weighted average of probabilities, and the executed choice is argmax with catalog order breaking ties. There is no pooled confidence.
- **Jev failures → cannot decide:**
  - no key, timeout/network, `401`, `429`/`529` after retries within `timeout`;
  - `422` after one half-size re-split (single request or chunk), over `max_chunks`, attachments over their anchor share;
  - malformed answers (including a missing `route`/`relevance` answer).

  The CLI is known when set by `--cli`, implied by `--model`, or the only CLI left; then only the caller's fixed values are emitted. Otherwise exit `2`.
- **Execution:** `os/exec` with no shell; env minus `TYPESAFE_API_KEY`; stdin = the replayed buffer then a live relay of the rest; stdout/stderr are the parent's file descriptors.
  - `cmd.WaitDelay` set, so a child that exits while the caller keeps stdin open does not hang agrouter.
  - Windows `.cmd`/`.bat` targets are resolved and their arguments quoted as in ralphex's `execx`.
  - Unix forwards SIGINT/SIGTERM to the process group; Windows uses a Job Object with kill-on-close.
  - The exit code is the child's (`128+signal` on Unix).

## What Goes Where
- **Implementation Steps** (`[ ]` checkboxes): code, tests, config defaults, workflows, README/CLAUDE.md, design updates, all in this repository.
- **Post-Completion** (no checkboxes): pushing branches and tags, checking the workflows on GitHub, the manual ralphex end-to-end run, `make eval-routing` with a real key, enabling Coveralls, and the ralphex-side per-call CLI choice.

## Implementation Steps

### Task 1: Scaffold the Go module and tooling

**Files:**
- Create: `go.mod`, `go.sum`, `vendor/`
- Create: `cmd/agrouter/main.go`
- Create: `cmd/agrouter/main_test.go`
- Create: `Makefile`
- Create: `.golangci.yml`
- Create: `.gitignore`, `.gitattributes`
- Modify: `docs/design.md`

- [x] create `go.mod` (`module github.com/SvetlovA/agrouter`, `go 1.26`, and make sure local Go 1.27 adds no `toolchain` line) with go-flags, ini.v1, testify and `golang.org/x/sys`; add moq with `go get -tool github.com/matryer/moq@<pinned>`; `go mod vendor`
- [x] create `.gitattributes` (`* text=auto eol=lf`, `-text` for binary fixtures such as `*.png`, `*.pdf`, `*.bin`) before any fixture is committed
- [x] create `cmd/agrouter/main.go` with a testable `run(args []string, stdin io.Reader, stdout, stderr io.Writer) int` (`main` only calls `os.Exit(run(...))`), and `--version` printing `debug.ReadBuildInfo().Main.Version` (falling back to `unknown`)
- [x] create `Makefile` following ralphex: `build` (to `.bin/`), `test` (coverage excluding mocks; `RACE ?=` empty locally, CI passes `RACE=-race`), `lint`, `fmt`, `generate` (`go generate ./...` running `go tool moq`), and a placeholder `eval-routing`
- [x] copy ralphex's `.golangci.yml` (linter set and gosec excludes), dropping ralphex-only settings; add `.gitignore` for `.bin/` and coverage files
- [x] add `--version` to the design's "agrouter's own" argument table
- [x] write tests for `run` with `--version` (non-empty version line on stdout, exit 0) and `--help` (exit 0)
- [x] run `make test` and `make lint` - must pass before next task
- ⚠️ go-flags without the `forceposix` build tag uses Windows option style: `--help` lists `/version`, and any argument starting with `/` (for example a prompt `/review this`) is parsed as an option. `go install` can't pass build tags, so Task 6 must handle this (for example by parsing with a POSIX-style front end or re-routing `/`-prefixed tokens to the positional prompt) and test it on Windows
- ⚠️ local golangci-lint 2.12.2 (chocolatey) can't typecheck the Go 1.27 stdlib; `make lint` passes with v2.13.0 built locally (`GOBIN=/tmp/gl go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.0`, then `PATH=/tmp/gl:$PATH make lint`)

### Task 2: Config loading and layering (`pkg/config`)

**Files:**
- Create: `pkg/config/config.go`
- Create: `pkg/config/load.go`
- Create: `pkg/config/config_test.go`

- [x] define the config types: `Agrouter` settings (the `[agrouter]` section), `CLI` (command, description, enabled, args map of key → template), `Model` (cli, name, aliases, efforts, description, source, enabled), `Effort` descriptions keyed `<cli>.<level>`
- [x] implement loading with `ini.v1` (`IgnoreInlineComment: true`) and layering embedded < global (`AGROUTER_CONFIG_DIR` or `~/.config/agrouter/config`) < local `.agrouter/config`, merged per section and per key
- [x] parse `[cli.*.args]` values as JSON string arrays; `enabled = false` removes a section
- [x] implement API key resolution: flag > `TYPESAFE_API_KEY` > local > global > embedded placeholder, with an explicit empty flag clearing it (the caller passes "unset" vs "set to empty")
- [x] write tests for layering and per-key merges (one `[cli.*.args]` key overridden locally), `enabled = false`, `AGROUTER_CONFIG_DIR`, API key precedence and explicit-empty flag
- [x] write tests for error cases: malformed JSON template, unreadable file, bad duration/int values
- [x] run tests - must pass before next task
- ➕ disabling a `[cli.*]` section also drops its args, models and effort descriptions; an empty `TYPESAFE_API_KEY` counts as unset. Both recorded in `docs/design.md`
- ➕ unknown sections and unknown keys in `[agrouter]`, `[cli.*]`, `[model.*]` and `[effort.*]` are load errors (typos would otherwise be ignored silently); ini.v1's parent-section key inheritance is bypassed by merging layers by hand

### Task 3: Config validation rules (`pkg/config`)

**Files:**
- Create: `pkg/config/validate.go`
- Create: `pkg/config/validate_test.go`

- [x] validate CLI mappings:
  - `print` and `model` required; `effort` required when any of the CLI's models has efforts;
  - `{prompt}` exactly once across `print`/`prompt`, as a whole token only, nowhere else;
  - placeholders limited to `{model}`, `{effort}`, `{value}` (in `config.*` only), `{prompt}`.
- [x] validate models: `@` in section names rejected; duplicate `name`/alias across the catalog rejected; unknown `cli` rejected
- [x] validate `[agrouter]`: `max_chunks` and `chunk_parallel` ≥ 1, `relevance_floor` in (0, 1] (the option-count and question-budget checks live in Tasks 5 and 13, where their inputs exist)
- [x] write tests for each rule (valid config passes; each violation gives a clear error naming the section and key)
- [x] run tests - must pass before next task
- ➕ `Load` runs `Validate`, which joins every violation into one error; a model without `name` and an unknown `{placeholder}` are also config errors. Recorded in `docs/design.md`

### Task 4: Embedded v1 catalog and mappings (`pkg/config/defaults`)

**Files:**
- Create: `pkg/config/defaults/config`
- Create: `pkg/config/defaults/embed.go`
- Create: `pkg/config/defaults/defaults_test.go`

- [x] write the `[agrouter]` defaults, including the `question`, `chunk_question` and `relevance` texts from the design
- [x] write `[cli.claude]`/`[cli.claude.args]` and `[cli.codex]`/`[cli.codex.args]` exactly as in the design's config block:
  - `print`, `model`, `effort`;
  - all six `permission-mode.*` (Codex without `manual`/`dontAsk`);
  - `output-format.*` (Codex without `json`, and `text = []`);
  - `verbose`;
  - Codex `sandbox.*` and the `config.*` whitelist (`stream_idle_timeout_ms`, `project_doc`, `project_doc_fallback_filenames`, `features.multi_agent`, `agents.reviewer.description`, `model`, `model_reasoning_effort`).
- [x] write the `[model.*]` sections with full section names and descriptions/sources:
  - Claude: `claude-fable-5-1`, `claude-opus-5-5` (alias `opus`), `claude-sonnet-5`, `claude-haiku-4-5` (no efforts);
  - Codex: `gpt-6-astra`, `gpt-6-sol`, `gpt-6-luna`;
  - the `[effort.<cli>.<level>]` descriptions.

  The design's catalog text is the source. If fuller text is taken from the linked vendor pages, record the sources checked here.
- [x] write tests: the embedded config loads and validates, the Haiku section has no efforts, and a guard that no value carries a trailing inline comment
- [x] run tests - must pass before next task
- ➕ descriptions and efforts come from the design's catalog tables (price and latency appended, as in the design's opus example); no vendor pages were re-checked in this task. Per-model `source` URLs follow the design's `platform.claude.com/docs/en/models/<short id>/overview` and `developers.openai.com/api/docs/models/<id>` patterns; `effort.codex.ultra` cites learn.chatgpt.com, since `ultra` is CLI-only
- ➕ `TestMatchesDesign` compares the `[agrouter]` and `[cli.*]` sections key by key against the ini block in `docs/design.md`, so the spec and the defaults cannot drift apart

### Task 5: Options, ids and lookup (`pkg/catalog`)

**Files:**
- Create: `pkg/catalog/catalog.go`
- Create: `pkg/catalog/catalog_test.go`

- [x] derive options (model × effort, or the bare model when it has no efforts) from enabled config, in catalog order, with ids `<section>@<effort>` / `<section>`
- [x] return config errors from `catalog.Build`: no enabled options, more than 255 options
- [x] implement model lookup by `name` or alias (resolving to `name`), and "not in catalog" reporting for pass-through
- [x] implement option filtering helpers used by eligibility (by CLI, by model, by effort)
- [x] write tests: the embedded catalog gives 16 Claude + 17 Codex = 33 options; ids; alias resolution; disabled sections; stable ordering; no enabled options; 256 options
- [x] write tests for lookup misses and a model section without efforts
- [x] run tests - must pass before next task
- ➕ besides the filters, `catalog.CLIs` (distinct CLIs in first-seen order) and `Catalog.HasEffort` (empty effort never matches) serve eligibility; `ResolveModel` returns the value unchanged with `inCatalog=false` for pass-through

### Task 6: Argument model and parsing (`cmd/agrouter`)

**Files:**
- Create: `pkg/args/request.go`
- Create: `cmd/agrouter/flags.go`
- Create: `cmd/agrouter/flags_test.go`

- [x] define `args.Request` in `pkg/args` (mode, cli, api key with unset/empty distinction, model/effort constraints and their source, ordered mapped arguments with caller spelling and mapping key, positional prompt, raw tokens), so parsing stays in `cmd/agrouter` and building in `pkg/args`
- [x] implement go-flags parsing:
  - pre-split argv at the first `--` into flags and raw tokens;
  - check `args[0] == "exec"` for exec mode, with a later `exec` being an ordinary word;
  - flags: `--cli` (+ `AGROUTER_CLI`), `--jev-api-key`, `-p/--print`, `--help`, `--version`, `--model`, `--effort`, `--permission-mode`, the two bypass aliases, `--output-format`, `--verbose`, `--sandbox`, repeatable `-c/--config`;
  - `func(string)` callback options, to record caller order across different flags;
  - one positional anywhere.
- [x] implement `-c` handling: split at the first `=`; `model`/`model_reasoning_effort` become constraints (TOML quotes removed) unless `--model`/`--effort` is also given, in which case they stay ordinary `config.*` keys
- [x] exit `2` with one `agrouter:` line for an unknown flag and a second positional
- [x] write tests:
  - flags in any order around the positional, split and `=` forms;
  - decision vs exec mode, a later `exec` as the positional;
  - `-p` accepted, `AGROUTER_CLI`, `--jev-api-key=` vs absent;
  - a `-c` value containing `=`, and `-c model=` beside `--model` kept as a `config.model` key;
  - the ralphex Claude argv and the full ralphex Codex argv (`features.multi_agent`, `agents.reviewer.description` with spaces, bypass alias, `--sandbox danger-full-access`, `stream_idle_timeout_ms`, `project_doc_fallback_filenames`) parsed unchanged;
  - raw tokens kept apart from the positional.
- [x] write tests for error cases: unknown flag, second positional
- [x] run tests - must pass before next task
- ➕ `exec` is a first-token check, not a go-flags command, and tokens starting with `/` are escaped before go-flags parses them (go-flags on Windows treats `/x` as an option). Values are parsed with `unquote:"false"` so they stay byte-exact. `design.md` updated to match
- ➕ caller spellings are normalized to the split form (`--output-format json`, `-c key=value`); an explicit empty `--cli=` overrides `AGROUTER_CLI`; the last of several `-c model=` constraints wins

### Task 7: Argv building and skipped arguments (`pkg/args`)

**Files:**
- Create: `pkg/args/build.go`
- Create: `pkg/args/redact.go`
- Create: `pkg/args/build_test.go`

- [x] implement template substitution (`{model}`, `{effort}`, `{value}` inside tokens; `{prompt}` as a whole token, dropped when there is no positional prompt) with no shell quoting
- [x] implement argv building in the design order (command, `print`, mapped args in caller order, model, effort, `prompt`, raw passthrough only with `--cli`), emitting each mapping key once
- [x] collect skipped arguments as caller spellings plus warning lines in the design's format:
  - a missing key → "has no mapping";
  - `[]` → "maps to nothing";
  - raw tokens without `--cli`.
- [x] implement a redacted rendering for debug (`{prompt}` → `<prompt>`, raw tokens as a count, API key never present)
- [x] write tests:
  - v1 golden argv for Claude and Codex for every `--output-format` and `--permission-mode` value, with the `print` mapping emitted even without `-p`;
  - Codex without `--permission-mode` gets no `--skip-git-repo-check`; bypass aliases give one mapping;
  - contradictions translated in order (two `--sandbox`);
  - each `config.<key>` entry forwarded byte-exact, repeated `-c` in the caller's order, `-c approval_policy=never` skipped with a warning;
  - `{prompt}` with spaces/quotes/newlines as one token and dropped when absent; prompt-at-end via the `prompt` key;
  - a model without efforts getting no effort argument;
  - the skipped warning text; `--cli`/`--jev-api-key` never in argv.
- [x] write tests for a made-up CLI defined only in config (value-keyed mapping to a different flag, an `[]` mapping, custom `print`)
- [x] run tests - must pass before next task
- ➕ `args.Build(cli, req, Choice)` takes a `Choice` (model, effort, the caller's effort spelling, `Pinned` for a valid `--cli`); raw passthrough is appended only when `Pinned`, so an unknown `--cli` also skips raw tokens. Arguments are deduplicated by mapping key plus `-c` value, so `-c k=1 -c k=2` both reach Codex and an identical repeat is emitted once; a CLI without an `effort` mapping skips a caller's effort with a warning. Raw tokens without `--cli` give one warning: `skipped N raw argument(s) after --: passed through only with --cli`
- ⚠️ the local golangci-lint 2.12.2 (built with go1.26.2) prints typecheck errors on the Go 1.27.1 stdlib (`math/rand/v2`) for every package, including ones this task did not touch, and still reports 0 issues; CI's pinned v2.13.0 is unaffected

### Task 8: Prompt capture, stdin and non-text input (`pkg/prompt`)

**Files:**
- Create: `pkg/prompt/capture.go`
- Create: `pkg/prompt/detect.go`
- Create: `pkg/prompt/capture_test.go`

- [x] read non-TTY stdin to EOF under the routing context and a text capture limit in bytes (both passed in), keeping the buffered bytes for replay and exposing the unread remainder for live relay; a deadline hit during capture means cannot decide, with stdin still replayed in full
- [x] build Jev's `prompt` per the design's table: positional, stdin text, or `positional + "\n\n" + stdin`; binary stdin stays out and becomes an `attachments` entry (`source: stdin`)
- [x] implement binary detection: `http.DetectContentType` signatures first, then NUL/invalid-UTF-8 over the whole captured stdin (a character cut at the 8 KiB prefix boundary is still text for files)
- [x] large binary stdin is never an oversize failure: the type comes from the prefix, the size from `stat` for a regular file or from counting otherwise, and it never counts against the text limit (if a non-file pipe forces buffering all of it, record a ⚠️ and ask before deviating)
- [x] report "no prompt" (no positional, empty stdin) so the caller exits `2`; binary-only stdin counts as a prompt
- [x] write tests:
  - the four positional/stdin cases, the `\n\n` join in Jev's prompt only, byte-exact replay;
  - PNG/JPEG/PDF signatures (including an ASCII-looking PDF), NUL bytes, the UTF-8 boundary case;
  - binary stdin with a positional, and a large binary stdin becoming an attachment rather than oversize;
  - capture stopping at the limit with buffer + rest reproducing stdin exactly;
  - a slow stdin hitting the deadline (cannot decide, stdin still replayed).
- [x] run tests - must pass before next task
- ➕ `prompt.Capture(ctx, positional, stdin, limit)` returns a `Result` with Jev's `Prompt`, `Attachments`, `TextBytes` (positional + stdin text, for Task 9's shared limit), `Stdin{Buffered, Rest}` and `Undecidable` (`ErrCaptureLimit` or the context's error); `ErrNoPrompt` for no prompt. The positional prompt counts against the text limit; an empty positional counts as none. `StdinOf(os.Stdin)` returns nil for a terminal (any character device, so `< /dev/null` is "no stdin"). Stdin is read through a goroutine pump so a read blocked at the deadline loses no bytes; after capture the pump is `Rest`
- ⚠️ binary stdin from a non-file pipe is buffered in memory to EOF to count its size (routing needs the size before the child starts, and the bytes must be replayed); it still never counts against the text limit and is bounded by the routing deadline. Not confirmed with the user (non-interactive run): alternatives are spooling to a temp file or reporting only the prefix size

### Task 9: Files mentioned in the prompt (`pkg/prompt`)

**Files:**
- Create: `pkg/prompt/mentions.go`
- Create: `pkg/prompt/mentions_test.go`

- [x] extract candidates: quoted, backtick and Markdown-link spans first, then whitespace tokens, processed in textual order; try as written, then without surrounding brackets/trailing punctuation, then without `:line`/`:line:col` (never a Windows drive colon)
- [x] resolve against the canonical working directory (symlinks and junctions followed, case-insensitive on Windows) and read only regular files inside it by directory boundary; dedupe by canonical path; never fetch URLs; no recursion
- [x] text files go into `files`, binary into `attachments` (`source: mentioned`); mentioned text counts against the shared capture limit and the routing context
- [x] write tests:
  - paths with spaces in quotes/backticks/links, `path:line` and `path:line:col`;
  - a Windows drive path, trailing punctuation;
  - dedupe, mixed quoted and plain order, URLs not fetched, no recursion;
  - a binary mention giving an attachment;
  - mentioned text counted in the limit and the deadline.
- [x] write tests for escapes: `../` traversal, an absolute path outside, a sibling `cwd2` directory, a symlink (Unix) or junction (Windows) escaping the tree, missing files and directories ignored
- [x] run tests - must pass before next task
- ➕ `(*Result).ReadMentions(ctx, cwd, limit)` scans `Result.Prompt` (the text prompt only), appends to the new `Result.Files` and `Result.Attachments`, and adds mentioned text to `TextBytes`; over the limit or past the deadline it sets `Undecidable` and empties prompt, files and attachments. It errors only when `cwd` cannot be resolved. Single-quoted spans count only at word boundaries, so apostrophes stay words; `[x](<a b>)` destinations drop the angle brackets
- ➕ `canon_windows.go`/`canon_other.go`: on Windows the file is opened first and canonicalised from its handle (`GetFinalPathNameByHandle`), because `filepath.EvalSymlinks` stopped resolving junctions in Go 1.23; elsewhere `EvalSymlinks`. `golang.org/x/sys/windows` is now vendored (x/sys became a direct dependency)

### Task 10: Budget, anchor and chunking (`pkg/prompt`)

**Files:**
- Create: `pkg/prompt/budget.go`
- Create: `pkg/prompt/chunk.go`
- Create: `pkg/prompt/chunk_test.go`

- [x] implement the token estimate (UTF-8 bytes ÷ 3) and budget functions that take the serialized question sizes as parameters (so `prompt` has no dependency on `router`): state budget = 30k minus the longest question; total with all questions and the chunk envelope ≤ 64k
- [x] expose the text capture limit in bytes for a given `max_chunks` and budget, for Task 8's caller
- [x] build the 4k anchor:
  - `attachments` first, ≤1k; summarised by `source`+`type` into `{source,type,count,bytes}` when needed; over 1k even summarised → cannot decide;
  - then `prompt`, whole or head+tail;
  - then head+tail of `files`.
- [x] split everything not whole in the anchor into chunks cut on line and multibyte UTF-8 boundaries; more than `max_chunks` → cannot decide
- [x] write tests:
  - a state under budget is not split;
  - a short positional instruction plus a long stdin keeps the instruction in every anchor;
  - attachments in every anchor even when the prompt overflows; summarisation and its over-1k failure;
  - lossless coverage (anchor-only fields + chunks reproduce every captured byte, the only duplicates being the anchor's head/tail copies);
  - multibyte cuts, the 64k total check, the `max_chunks` limit.
- [x] run tests - must pass before next task
- ➕ `NewBudget(Questions{Route, ChunkRoute, Relevance})` takes serialized question sizes in bytes and returns `Budget{State, Chunk}` in tokens, with `ErrQuestionsOverBudget` when the single state, or a chunk state beside a maximal 4k anchor, gets under 2k (Task 13 reuses it for the load-time check). `CaptureLimit(b, maxChunks)` = maxChunks × the chunk text room beside a maximal anchor. `(*Result).Split(b, maxChunks)` returns nil when `Fits`, else `Split{Anchor, Chunks}`; `ChunkState{anchor, chunk}` is the per-chunk state and `(*Result).State()` the single one. Sizes are measured on the JSON encoding (escapes included, `encoding/json` with HTML escaping), not raw bytes
- ➕ `index`/`of` count the whole chunk sequence, and no chunk spans two files (`Chunk.File` keeps the file index, not sent). Summarised attachments carry `count` (new `Attachment.Count`, omitted otherwise). An overflowing prompt's head+tail take all the anchor room left, so files get anchor room only when the prompt is whole; files share it equally in order and stop below 64 bytes each. When only the attachments overflow, the prompt moves from the anchor into the chunks so there is always a chunk (design updated)

### Task 11: Jev client (`pkg/jev`)

**Files:**
- Create: `pkg/jev/client.go`
- Create: `pkg/jev/types.go`
- Create: `pkg/jev/client_test.go`

- [x] implement request/response types for Choice and Noul questions and the `POST /v1/systemone` call with `Authorization: Bearer`, honouring the routing context's deadline
- [x] retry `429`/`529` with backoff inside the deadline; surface `401`, `422` (as a distinct error so the router can re-split) and other statuses as typed errors
- [x] validate answers:
  - `choice` among the options sent; `confidence` finite in [0,1];
  - `probabilities` keys exactly the options, with finite values in [0,1] summing to 1±0.01;
  - `noul` finite in [0,1]; a missing `route` or `relevance` answer is malformed.
- [x] redact the key from every error, including echoed response bodies
- [x] write tests with `httptest`:
  - 200, 401, 422, 429 then 200, 429/529 exhausted within the deadline, 529;
  - a slow response against the deadline, malformed JSON;
  - NaN/out-of-range confidence, an unknown choice, bad probability maps, a missing answer;
  - the key appearing only in the header.
- [x] run tests - must pass before next task
- ➕ `jev.New(key).Ask(ctx, Request) (map[string]Answer, error)`: `Request{Model, State any, Questions}`, `Question{Type, Instructions any, Criteria}`; `Criteria` is an ordered `[]Criterion{Name, Value}` encoded as an object in slice (catalog) order. The response envelope is `{model, answers: {<question id>: answer}, usage}` (docs.typesafe.ai/api); every question sent must be answered, and an answer `type` that differs from the question's is malformed. Errors: `ErrNoKey` (no request made), `*StatusError{Status, Body}` matching `ErrUnauthorized`/`ErrUnprocessable`/`ErrOverloaded` via `errors.Is`, `ErrMalformed`, and context errors for the deadline. Backoff is exponential from 200ms to 2s, honouring a `Retry-After` in seconds, and stops without sleeping when the wait would pass the deadline. Error bodies are redacted, flattened to one line and cut at 512 bytes
- ⚠️ the local golangci-lint 2.12.2 (built with Go 1.26) cannot type-check Go 1.27's standard library behind `net/http`, so it silently skipped `pkg/prompt` and failed on `pkg/jev`. Linting now uses v2.13.0 (the CI pin) built with local Go (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.0`, ahead of the chocolatey one on PATH). It surfaced 30 issues in earlier packages, now fixed: `summarise`→`summarize` and `unrecognised`→`unrecognized` in code (misspell is US), `strings.FieldsSeq`, `nilerr` directives where `Undecidable` is an outcome rather than a failure, and `capture.go` split into `readText`/`finishBinary` (nestif). Lint is clean with GOOS=windows, linux and darwin

### Task 12: Eligibility and mapped-argument preference (`pkg/router`)

**Files:**
- Create: `pkg/router/eligibility.go`
- Create: `pkg/router/eligibility_test.go`

- [ ] implement eligibility in design order:
  - `--cli`: an unknown or disabled name is skipped with a warning;
  - `--model`: a catalog model narrows to it (by name or alias); a model outside the catalog gives one option per remaining CLI with the model passed through;
  - `--effort`: an effort nothing has is passed through, with efforts dropped from the ids;
  - the mapped-argument preference: keep the CLIs with the fewest skips, `[]` counting as skipped, never emptying the list.
- [ ] report which CLIs were dropped and why, for debug output
- [ ] write tests:
  - each filter and the preference: `--output-format json` → Claude, `--verbose` → Claude, `--permission-mode manual`/`dontAsk` → Claude (skipped when `--cli=codex`), `--sandbox`/`-c` → Codex, `--cli=codex --output-format json` → Codex with a skip;
  - fewest-skips ties kept;
  - `--model` by name and by alias, `--model` outside the catalog with and without `--cli`;
  - `--effort` alone, `--effort` with a model without efforts passed through;
  - no filter ever empties the list; ralphex's Claude and Codex argv produce no skips.
- [ ] run tests - must pass before next task

### Task 13: Jev question, decision and cannot-decide policy (`pkg/router`)

**Files:**
- Create: `pkg/router/router.go`
- Create: `pkg/router/question.go`
- Create: `pkg/router/router_test.go`
- Create: `pkg/router/mocks/jev_client.go` (moq, via `go:generate`)

- [ ] define the consumer-side `JevClient` interface and generate its mock
- [ ] short-circuit a single remaining option without calling Jev
- [ ] build the Choice request (compact encoding): structured `instructions` (question, clis, models, efforts for the remaining options only) and per-option criteria objects, as in the design's JSON; keep the encoding behind a small option so Task 17 can switch to full per-criterion descriptions
- [ ] implement the load-time budget check on the serialized questions (route `question`, and `chunk_question` + `relevance` + a maximal anchor, leave ≥2k tokens for the state) as a config error
- [ ] implement the decision and the cannot-decide policy: CLI known (`--cli`, implied by `--model`, or the only CLI left) → only the caller's fixed values; otherwise exit `2`
- [ ] write tests:
  - the single-option short-circuit;
  - the golden Jev request JSON with and without `--cli`;
  - a question over budget.
- [ ] write tests for every cannot-decide case via the mocked `JevClient`:
  - no key, timeout, 401, 429/529 exhausted, malformed;
  - CLI known by `--cli`, by `--model`, and as the only CLI left (ralphex's Codex argv without `--cli`);
  - `--cli=claude --effort high` keeping `--effort high`; CLI unknown giving exit `2`.
- [ ] run tests - must pass before next task

### Task 14: Chunked routing and pooling (`pkg/router`)

**Files:**
- Create: `pkg/router/pool.go`
- Create: `pkg/router/pool_test.go`

- [ ] fan out one request per chunk (same route question and catalog, the anchor repeated, plus the `relevance` Noul) with `chunk_parallel` in flight, inside the one routing deadline
- [ ] pool: weight = max(noul, `relevance_floor`); score = weighted average of probabilities; argmax with catalog order breaking ties; no pooled confidence
- [ ] failure policy:
  - a `422` on the unsplit single request splits the whole state at half the budget, once, and takes the pooled path;
  - a `422` on a chunk re-splits that chunk once at half size;
  - `max_chunks` is rechecked after any re-split;
  - a second `422`, one on a chunk under 2k tokens, or any chunk failing after retries → cannot decide.
- [ ] expose per-chunk results (field, index, relevance, top options) and the pooled top options for debug output
- [ ] write tests:
  - a short hard requirement outweighing long filler, and floored filler outweighing it at `max_chunks` (the documented limit);
  - all-low relevance giving equal weights, stable tie order, per-chunk confidence never averaged;
  - the per-chunk request golden JSON.
- [ ] write tests for the failure policy cases above, including the single-request 422 re-split
- [ ] run tests - must pass before next task

### Task 15: Child process execution (`pkg/runner`)

**Files:**
- Create: `pkg/runner/runner.go`
- Create: `pkg/runner/runner_unix.go`
- Create: `pkg/runner/runner_windows.go`
- Create: `pkg/runner/runner_test.go`
- Create: `pkg/runner/runner_unix_test.go`
- Create: `pkg/runner/runner_windows_test.go`

- [ ] define the consumer-side `CommandRunner` interface (used by `cmd/agrouter`) and implement it with `os/exec`:
  - no shell; environment inherited minus `TYPESAFE_API_KEY`;
  - stdin = the replayed buffer, then the live remainder (or empty);
  - stdout/stderr = the parent's file descriptors;
  - `cmd.WaitDelay` set.
- [ ] on Windows, resolve `.cmd`/`.bat` targets and quote arguments for them following ralphex's `pkg/execx` (a `{prompt}` token with quotes, `&`, `%` or newlines must arrive intact or fail safely); record a ⚠️ if some characters cannot be passed safely
- [ ] exit `127` with one `agrouter:` line when the command is missing or fails to start; never retry another CLI; propagate the child's exit code (`128+signal` on Unix)
- [ ] Unix: forward SIGINT/SIGTERM to the child's process group and kill the group on cancellation. Windows: assign the child to a Job Object with kill-on-close (ralphex `procgroup_windows.go`)
- [ ] write tests with the helper process:
  - exact argv received;
  - byte-exact stdin including binary and the buffer + relay case;
  - `TYPESAFE_API_KEY` absent;
  - exit code propagation, `127` on a missing command;
  - no hang when the child exits while stdin stays open.
- [ ] write the platform tests behind build tags (Unix signal forwarding; Windows tree killed when the Job is closed; a `.cmd` target receiving a prompt with special characters)
- [ ] run tests - must pass before next task

### Task 16: Wiring, decision and exec modes (`cmd/agrouter`)

**Files:**
- Modify: `cmd/agrouter/main.go`
- Create: `cmd/agrouter/app.go`
- Create: `cmd/agrouter/app_test.go`
- Create: `cmd/agrouter/guard_test.go`

- [ ] wire the pipeline:
  - parse, then config, then catalog;
  - start the routing deadline before prompt capture, then capture, route, and build argv;
  - decision mode prints one JSON line `{"cli","model","effort","argv","skipped"}` on stdout; exec mode runs the child through `CommandRunner`.
- [ ] print one stderr warning per skipped argument in both modes; errors as one `agrouter:` line each; `--help` includes the API key cautions from the design
- [ ] exit codes: `2` for argument/prompt/config errors and cannot-decide with the CLI unknown, `127` for startup failure, the child's code otherwise
- [ ] write end-to-end tests (fake CLIs via the helper process, Jev via `httptest`):
  - the ralphex Claude-mode argv, ralphex Codex-mode argv and external-review argv, with no skip warnings;
  - decision JSON including `null` model/effort and `skipped`;
  - a prompt-only invocation, the `--cli=codex --output-format json` skip warning, no-prompt exit `2`;
  - raw tokens and flags never in the Jev state;
  - a made-up CLI defined only in config, routed and run with the `{prompt}` token and stdin unchanged, where renaming its section changes nothing.
- [ ] write the repo-level guard test: production sources outside `pkg/config/defaults` contain no CLI names (excluding `--help` text and test files)
- [ ] run tests - must pass before next task

### Task 17: Debug output and routing evaluation

**Files:**
- Create: `cmd/agrouter/debug.go`
- Create: `cmd/agrouter/debug_test.go`
- Create: `pkg/router/eval_test.go` (build tag `eval`)
- Create: `testdata/routing/*.json`
- Modify: `pkg/router/question.go`
- Modify: `Makefile`

- [ ] implement `AGROUTER_DEBUG=1`:
  - option count, choice, top probabilities, confidence, the Jev failure reason;
  - eligible CLIs and why others were dropped;
  - per-chunk lines (field, index, relevance, top options) and the pooled top options;
  - the redacted final command, and the API key's source (never its value).
- [ ] add the full per-criterion encoding option to the Choice request builder
- [ ] define the eval case format (prompt or stdin file, optional mentioned files, acceptable option ids) and seed cases:
  - ralphex task, review and plan prompts; trivial edits; large refactors;
  - an oversized prompt with a buried requirement;
  - a requirement whose meaning depends on a distant chunk.
- [ ] implement the `eval`-tagged test: route every case against the real Jev for both encodings, and report accuracy and the confidence distribution (split cases on accuracy only)
- [ ] make `make eval-routing` run `go test -tags=eval ./pkg/router/...` (needs `TYPESAFE_API_KEY`; excluded from `make test` and CI)
- [ ] write tests for debug output redaction (prompt shown as `<prompt>`, raw tokens counted, key never present) and for eval case loading/scoring against the mocked `JevClient`
- [ ] run tests - must pass before next task

### Task 18: CI workflow

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] trigger on `push` and `pull_request`; `permissions: contents: read`; `defaults: run: shell: bash`; `actions/checkout@v7` with `persist-credentials: false`; `actions/setup-go@v7` with Go `1.26`
- [ ] matrix `ubuntu-latest`, `windows-latest`, `macos-latest` with `fail-fast: false`: `make test RACE=-race` (Windows runners have gcc for cgo; if `-race` fails there, drop it for Windows only and record a ⚠️ here)
- [ ] lint: `golangci/golangci-lint-action@v9` pinned to `v2.13.0` on ubuntu, plus a `GOOS=windows` lint run so `*_windows.go` is linted; align the local golangci-lint version
- [ ] ubuntu-only: filter mocks out of coverage and submit with goveralls (`continue-on-error: true`)
- [ ] validate the workflow with `actionlint` locally
- [ ] run `make test` and `make lint` - must pass before next task

### Task 19: Release workflow for `go install`

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] trigger on tags `v*`; `permissions: contents: write`; checkout with `fetch-depth: 0` and `git fetch origin master`
- [ ] validate the tag: `^v[01]\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9a-z-]+(\.[0-9a-z-]+)*)?$` (no `/v2` module path, so v2+ is rejected; lowercase pre-releases avoid proxy case-escaping); require the tagged commit to be an ancestor of `origin/master`
- [ ] re-run `go test ./...` on the tagged commit, then `gh release create "$GITHUB_REF_NAME" --verify-tag --generate-notes` (plus `--prerelease` for a suffix) with `env: GH_TOKEN: ${{ github.token }}`
- [ ] warm the module proxy with a retry loop around `GOPROXY=https://proxy.golang.org GOFLAGS=-mod=mod go list -m "github.com/SvetlovA/agrouter@$GITHUB_REF_NAME"` (needs the repository to be public; note it in the workflow)
- [ ] validate the workflow with `actionlint`; add a "Releasing" section to `README.md` (tag on master, push the tag, the install command)
- [ ] run `make test` - must pass before next task

### Task 20: Verify acceptance criteria
- [ ] verify every design section is implemented:
  - CLI and arguments, skipped arguments, pass-through, decision and exec modes, API key;
  - configuration and catalog;
  - routing and eligibility, prompt/mentions/non-text, Jev request, budget, chunking, cannot-decide;
  - argument mapping, execution;
  - ralphex Claude and Codex integration.
- [ ] verify every item in the design's Testing section is covered by a test
- [ ] run full test suite: `make test`
- [ ] run linter: `make lint`
- [ ] verify coverage ≥ 80% per package (mocks excluded)
- [ ] verify `go install ./cmd/agrouter` from a clean checkout and `agrouter --version`

### Task 21: [Final] Update documentation
- [ ] expand `README.md` around the Task 19 "Releasing" section:
  - what agrouter does, `go install`, decision/exec usage examples;
  - config layering and overrides;
  - ralphex Claude and Codex configuration, including `codex_model =`/`codex_reasoning_effort =` and the `idle_timeout` note;
  - exit codes.
- [ ] create `CLAUDE.md` with project conventions (layout, code style, test helpers, `go tool moq`, local tests without `-race`, "design.md is the spec")
- [ ] update `docs/design.md` for any remaining deviation made during implementation, and change its "Status" line from "design, not implemented"
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion
*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification:**
- push the branch and confirm the CI workflow passes on all three OSes before merging
- run ralphex on a toy project in Claude mode (`claude_command = agrouter`, `claude_args = exec --cli=claude ...`) and in `--codex` mode (`codex_command = agrouter`, empty `codex_model`/`codex_reasoning_effort`) with `AGROUTER_DEBUG=1`, and check the decisions, the absence of skip warnings, and the progress display
- run decision mode with both CLIs executing the printed argv
- run `make eval-routing` with a real `TYPESAFE_API_KEY`, then pick the question encoding and tune descriptions before tagging

**Release:**
- make sure the repository is public (the module proxy and `go install` need it), push the first tag on `master` (e.g. `v1.0.0`), and confirm the GitHub Release and `go install github.com/SvetlovA/agrouter/cmd/agrouter@v1.0.0`
- enable the repository on Coveralls if coverage reporting is wanted

**External system updates:**
- ralphex: a per-call CLI choice executor mode calling agrouter's decision mode (design open question 8) is a separate ralphex change
- binary releases (goreleaser archives for Windows/macOS/Linux) are a later release
