# agrouter

Go CLI that asks TypeSafe's Jev which `(cli, model, effort)` should run a prompt, then prints the decision or runs the chosen CLI. Module `github.com/SvetlovA/agrouter`, Go 1.27, dependencies vendored.

## Spec

`docs/design.md` is the specification. When an implementation change deviates from it, update the design in the same commit.

## Layout

```
cmd/agrouter/          # go-flags parsing, app pipeline (app.go), debug output, exit codes; end-to-end tests
pkg/config/            # INI loading, layering (embedded < global < local < env), validation
pkg/config/defaults/   # embedded config: the v1 catalog and argument mappings (the only place catalog names live in production)
pkg/catalog/           # options (model x effort), option ids, model/alias lookup
pkg/args/              # request model, argv building from templates, skipped arguments, redaction
pkg/prompt/            # prompt capture, stdin, binary detection, mentioned files, budget, chunking
pkg/jev/               # hand-written TypeSafe HTTP client: types, validation, retry, deadline
pkg/router/            # eligibility, Jev questions, chunk fan-out and pooling, cannot-decide policy
pkg/runner/            # child process, stdin replay, Unix signals / Windows Job Objects, .cmd quoting
```

Dependencies point one way: `config` <- `catalog` <- `router`; `prompt` depends on nothing internal; `cmd/agrouter` wires everything.

## Rules

- **CLI, model, and effort names come only from config.** Production Go code outside `pkg/config/defaults` must not hardcode catalog names, model aliases, or effort levels, including in help text. Per-CLI behavior and argument templates belong in `[cli.*]`/`[cli.*.args]`; models and efforts come from `[model.*]`/`[effort.*]`. `cmd/agrouter/guard_test.go` enforces this; tests, testdata, and mocks may use explicit synthetic names.
- **stdout carries only the decision JSON** (and `--help`/`--version`). Warnings, one-line `agrouter:` errors and `AGROUTER_DEBUG=1` output go to stderr, with the prompt and API key redacted.
- Unmapped arguments are skipped with a warning, never errors; values outside the catalog are passed through. Exit `2` only for the cases in the design's "Still errors" list; `127` when the child can't start.

## Code style

- Comments lowercase except godoc; errors wrapped with `%w` and context.
- Consumer-side interfaces (`JevClient` in `pkg/router`, `CommandRunner` in `cmd/agrouter`) with moq mocks in `mocks/`, regenerated with `make generate` (`go tool moq`, pinned in `go.mod`; no separate install).
- US spelling (`summarize`, `recognized`): the `misspell` linter is set to US.

## Testing

- Table-driven tests with testify; coverage 80%+ per package, mocks excluded.
- Test behavior, not the current configuration contents. Do not pin the shipped model list, model versions, option counts, default values, argument mappings, or equality with a documentation config block. Use explicit synthetic fixtures for catalog, filtering, lookup, and request-format tests; keep a smoke test that embedded config loads and validates.
- Child processes: the test binary doubles as the fake CLI when `GO_WANT_HELPER_PROCESS=1` (`cmd/agrouter/app_test.go`, `pkg/runner/runner_test.go`); it records argv, stdin and env. Jev is an `httptest` server or the moq mock.
- Golden requests in `pkg/router/testdata` use a synthetic catalog, independent of the embedded defaults: `go test ./pkg/router -update` rewrites them.
- Platform tests sit behind build tags (`runner_unix_test.go`, `runner_windows_test.go`) and only run on their own OS in CI.
- `.gitattributes` forces LF so fixtures and embedded defaults are byte-identical on Windows.
- `make eval-routing` runs `//go:build eval` tests against the real Jev API (needs `TYPESAFE_API_KEY`); never in CI.

## Commands

```sh
make test       # locally without -race (no cgo on the dev machine); CI runs make test RACE=-race
make lint       # golangci-lint v2.13.0 (CI pin); override with GOLANGCI_LINT=<path>
make build      # .bin/agrouter
make generate   # moq mocks
make fmt        # needs goimports: go install golang.org/x/tools/cmd/goimports@latest
```

Older golangci-lint builds (e.g. 2.12.2) cannot typecheck the Go 1.27 stdlib and silently skip packages; use v2.13.0 built with the local Go (`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.0`). Check lint for `GOOS=linux` and `GOOS=darwin` too, and with `--build-tags=eval`.
