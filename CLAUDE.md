# agrouter

Go CLI that asks TypeSafe's Jev which `(cli, model, effort)` should run a prompt, then prints the decision or runs the chosen CLI. Module `github.com/SvetlovA/agrouter`, Go 1.27, dependencies vendored.

## Layout

```
cmd/agrouter/          # go-flags parsing, app pipeline (app.go), debug output, exit codes; end-to-end tests
pkg/config/            # INI loading, layering (embedded < global < local < env), validation
pkg/config/defaults/   # embedded config: the v1 catalog and invocation descriptors (the only place catalog names live in production)
pkg/catalog/           # options (model x effort), option ids, model/alias lookup
pkg/args/              # request model, argv building from invocation descriptors, reserved selection replacement, redaction
pkg/prompt/            # prompt sources (-p, positional, --prompt-file, stdin), --doc, strict file reads, mentioned files, request budget (policy: `request_tokens`, `bytes_per_token`), chunking, doc splitting
pkg/jev/               # hand-written TypeSafe HTTP client: types, validation, retry, deadline; the provider's limits (`Limits`), long-context header, overflow classification and reported input tokens
pkg/router/            # eligibility, complexity stage (--doc), staged routing (cli -> model -> effort), Jev questions, chunk fan-out and pooling, cannot-decide policy
pkg/runner/            # child process, stdin replay, Windows Job Objects, .cmd quoting; bounded prompt-free probes (own process group / Job Object, response limit)
pkg/readiness/         # exec-only availability: executable lookup, prompt-free probes (json-helper, SDK control usage, JSON-RPC rate limits), evidence evaluation against options
```

Dependencies point one way: `config` <- `catalog` <- `router`; `config`, `catalog`, `runner` <- `readiness` (which never imports `router` or `jev`); `prompt` depends on nothing internal; `cmd/agrouter` wires everything.

## Rules

- **CLI, model, and effort names come only from config.** Production Go code outside `pkg/config/defaults` must not hardcode catalog names, model aliases, or effort levels, including in help text. Per-CLI behavior and invocation descriptors (`exec_args`, `model_args`, `effort_args`, `prompt_args`) belong in `[cli.*]`; models and efforts come from `[model.*]`/`[effort.*]`. `cmd/agrouter/guard_test.go` enforces this; tests, testdata, and mocks may use explicit synthetic names.
- **stdout carries only the decision JSON** (and `--help`/`--version`) or the child's output in exec mode. The decision JSON is printed also when Jev cannot decide with more than one CLI left (exit 2 beside the stderr error line), with `null` values and reasons. Decision JSON and the exec selection line carry `reason` (typed-error code, stage, fixed message) on routing failure and `null_reasons` for every null field; codes come from typed errors (`jev` sentinels, `router.StageError`) in `cmd/agrouter/reason.go`, never from messages or bodies. Exec logs one `agrouter: selection: {json}` line with `cli`, `model` and `effort` to stderr before starting the child, without the prompt or argv; the prefix keeps callers that merge stderr into a stream-json stdout (ralphex) from dropping it as an unknown event. Warnings, one-line `agrouter:` errors and `AGROUTER_DEBUG=1` output also go to stderr, with the prompt and API key redacted; debug output never includes `--doc` text. Every line agrouter itself writes to stderr is also appended, stamped, to `<log_dir>/agrouter.log` (`pkg/config` `log_dir`, default `.agrouter/logs`, always on) by the tee in `cmd/agrouter/consolelog.go`; the child's output and stdout are never mirrored, and a log failure never changes console output or the exit code. For the default directory agrouter keeps the log gitignored through `.agrouter/.gitignore`; a custom `log_dir` is user-managed. A readiness block prints `agrouter: routing skipped: ...` (before routing) or `agrouter: run skipped: ...` (after it) before its own line. The one exception to the `agrouter:` prefix is exec's quota block line, which starts with the blocking check's configured `error_prefix` (for example a severity prefix) so a caller can match it as a limit message.
- **Invocation:** `agrouter [options] [prompt]` decides; `agrouter exec <cli> [options] [prompt] [-- native args...]` runs. The exec operand is required and must be an enabled CLI; `--cli` cannot supply it and a conflicting one is exit 2. Decision mode rejects `--`. Native tokens after `--` are never read as a prompt, routing constraint or readiness context. Agrouter removes its reserved `--model`/`-m` and `--effort` spellings before the child's own `--`; other tokens retain their order and contents. Generated model/effort arguments follow native options and precede the child's own `--`, so agrouter's selections win; `--verbose` before `--` is agrouter's only, and the exec selection line is always concise. `-p`/`--prompt`/`--print` are one option. `AGROUTER_CLI` is ignored with a warning; there are no child-option flags (`--permission-mode`, `--output-format`, `--sandbox`, `-c`, ...).
- `--doc` is routing-only: never in the child's argv. Routing has no capture, chunk-count or parallelism limits; only `[agrouter] timeout` bounds it.
- **Routing request size is configured, never recovered.** `[agrouter] request_tokens` (the whole request: state plus its questions) and `bytes_per_token` size every request through `prompt.Policy`/`prompt.Budget`; code owns no budget constants beyond `AnchorTokens`, `attachmentShare` and `MinStateTokens`. The provider's hard limits live only in the adapter (`jev.Limits(longContext)`); `router.New` checks `request_tokens` against them less a 2000-token reserve. There is no re-split, halving or resend: a 400 `max_tokens_exceeded` is `jev.ErrTooLong`, reason `request_too_long`, and the user tunes the config. User-facing names for new keys, reasons and messages stay provider-neutral; provider specifics stay in `pkg/jev`.
- **Config holds only policy; code owns question structure.** `[agrouter] routing_policy` and `complexity_policy` carry the cost/quality preference; stage questions, state guides, relevance/evidence questions and the complexity rubric are constants in `pkg/router/question.go`. Routing is staged (`cli` -> `model` -> `effort`, `pkg/router/stage.go`): a stage with one candidate, including one fixed by `--cli`/`--model` (`-m`)/`--effort`, is skipped; any stage failure fails the whole routing.
- There are no per-CLI argument mappings or selection/context metadata lists. Native reserved model/effort spellings never constrain Jev and are replaced, including when routing delegates a value to the child's default. Agrouter's own `--model`/`-m`/`--effort` spellings are reserved in native options; per-CLI invocation templates remain config-driven. Native config arguments pass through; placing the generated templates afterward gives them precedence under the child's override semantics. Readiness probes and default metadata describe agrouter's configured context, independently of native settings/profile/provider overrides.
- Exit `2` only for the cases in the README's exit code table (usage errors including exec operand errors, `--` in decision mode and a duplicate separator, bad `--prompt-file`/`--doc` files, config errors, no decision with more than one CLI left); `127` when the child can't start, or when exec's readiness preflight finds the executable missing, authentication failed or the subscription quota exhausted; a strict (`unknown = block`) unverified availability is exit 2. Readiness checks run only in exec, before any Jev request; decision mode never probes. Native options never disable readiness checks. There is no native option-arity table: a separate value literally spelled `-m`, `--model` or `--effort` is reserved too; use an attached value such as `--add-dir=-m` for that text. `-mVALUE` is the attached short-model form, so single-dash options beginning with `-m` are reserved too.

## Code style

- Comments lowercase except godoc; errors wrapped with `%w` and context.
- Consumer-side interfaces (`JevClient` in `pkg/router`, `CommandRunner` and `ReadinessChecker` in `cmd/agrouter`) with moq mocks in `mocks/`, regenerated with `make generate` (`go tool moq`, pinned in `go.mod`; no separate install).
- US spelling (`summarize`, `recognized`): the `misspell` linter is set to US.

## Testing

- Table-driven tests with testify; coverage 80%+ per package, mocks excluded.
- Test behavior, not the current configuration contents. Do not pin the shipped model list, model versions, option counts, default values, invocation descriptors, or equality with a documentation config block. Use explicit synthetic fixtures for catalog, filtering, lookup, and request-format tests; keep a smoke test that embedded config loads and validates.
- Readiness probes are tested with synthetic jsonl fixtures in `pkg/readiness/testdata/{control,rpc}` replayed by a fake peer; no test starts a real CLI.
- Child processes: the test binary doubles as the fake CLI when `GO_WANT_HELPER_PROCESS=1` (`cmd/agrouter/app_test.go`, `pkg/runner/runner_test.go`, `pkg/readiness/readiness_test.go`); it records argv, stdin and env. Jev is an `httptest` server or the moq mock.
- Golden requests in `pkg/router/testdata` use a synthetic catalog, independent of the embedded defaults: `go test ./pkg/router -update` rewrites them.
- Platform tests sit behind build tags (`runner_unix_test.go`, `runner_windows_test.go`) and only run on their own OS in CI.
- `.gitattributes` forces LF so fixtures and embedded defaults are byte-identical on Windows.
- `make eval-routing` runs `//go:build eval` tests against the real Jev API (needs `TYPESAFE_API_KEY`); never in CI.

## Commands

```sh
make test       # locally without -race (no cgo on the dev machine); CI runs make test RACE=-race
make lint       # golangci-lint v2.14.0 (CI pin); override with GOLANGCI_LINT=<path>
make build      # .bin/agrouter on master; .bin/agrouter.<branch> otherwise (with .exe for Windows builds)
make generate   # moq mocks
make fmt        # needs goimports: go install golang.org/x/tools/cmd/goimports@latest
```

Older golangci-lint builds, including v2.13.0, cannot decode the Go 1.27.2 stdlib export data (version 5) and fail typechecking; use v2.14.0 (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0`). Rebuilding v2.13.0 with the local Go does not fix its export-data reader. Check lint for `GOOS=linux` and `GOOS=darwin` too, and with `--build-tags=eval`.
