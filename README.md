# agrouter

agrouter picks the coding-agent CLI, model and reasoning effort for a prompt by asking TypeSafe's Jev, from a catalog of Claude Code and Codex options (and any other CLI described in config). Cheap models get trivial work, strong ones get hard work, per call, and the caller never needs to know each CLI's flags.

It has two modes:

- **Decision** (default): print the choice and the argv that would run it as one JSON line; the caller launches the CLI itself.
- **Exec** (`agrouter exec`): translate agrouter's arguments into the chosen CLI's own arguments through config, and run it.

The full specification is [`docs/design.md`](docs/design.md).

## Install

Requires Go 1.26 or later. Exec mode also needs the chosen CLI (`claude`, `codex`) on `PATH`; otherwise agrouter exits `127`.

```sh
go install github.com/SvetlovA/agrouter/cmd/agrouter@latest
agrouter --version
```

To build from a checkout on Unix, run `make build`. On Windows, use GNU Make and PowerShell:

```powershell
make -f WMakefile build
```

Both makefiles name the executable `.bin/agroute.<branch>`, with `.exe` appended for Windows builds and filename-unsafe branch characters (including `/`) replaced by `-`. For example, branch `feature/example` produces `.bin/agroute.feature-example.exe` on Windows; a detached HEAD uses `HEAD`. `WMakefile` also supports `test`, `lint`, `fmt`, `generate` and `eval-routing`.

Routing needs a TypeSafe API key for Jev:

```sh
export TYPESAFE_API_KEY=...
```

The key comes from `--jev-api-key`, then `TYPESAFE_API_KEY`, then `api_key` in config. Prefer the environment variable or the global config with user-only permissions: a command-line flag shows up in process listings, and a local `.agrouter/config` is easy to commit. The key is never printed and never reaches the child.

## Usage

```
agrouter      [options] [prompt] [-- raw args...]    # decision mode
agrouter exec [options] [prompt] [-- raw args...]    # exec mode
```

The prompt is the one positional argument and/or stdin. Arguments follow Claude Code's names:

| Option | Meaning |
|---|---|
| `--cli=NAME` (`AGROUTER_CLI`) | route only within one CLI (`claude`, `codex`, ...) |
| `--model=M`, `--effort=E` | fix the model or effort; Jev chooses the rest. Values outside the catalog are passed through for the CLI to validate |
| `-p`, `--print` | accepted; non-interactive mode is always on |
| `--permission-mode=MODE`, `--dangerously-skip-permissions` | Claude's permission modes, mapped to the closest Codex setting; Codex's `--dangerously-bypass-approvals-and-sandbox` is an alias |
| `--output-format=text\|json\|stream-json`, `--verbose` | output options |
| `--sandbox=MODE`, `-c key=value` (`--config`) | Codex's spellings, so ralphex's Codex executor can call agrouter unchanged |
| `--jev-api-key=KEY` | Jev API key; an empty value clears it |
| `-- raw args...` | appended to the child's argv unchanged, only with `--cli` |
| `--help`, `--version` | print help or the version and exit |

Whenever stdin is not a terminal, agrouter reads it to EOF within `[agrouter] timeout` before routing. A script that passes only a positional prompt should close stdin or redirect it from `/dev/null`; an inherited pipe that stays open uses up the timeout, and Jev cannot decide.

To route, agrouter also reads the text files the prompt names inside the working directory (quoted, backticked, Markdown-linked or plain paths) and sends their contents, never their paths, to TypeSafe. That includes files such as `.env` when the prompt mentions them. Paths outside the working directory are ignored and URLs are not fetched; binary files are described by type and size only. Nothing is read when only one option is left.

Decision mode:

```sh
agrouter -p "fix the flaky test in pkg/foo" --dangerously-skip-permissions --output-format stream-json
# {"cli":"codex","model":"gpt-6-sol","effort":"medium","argv":["codex","exec","fix the flaky test in pkg/foo","--dangerously-bypass-approvals-and-sandbox","--skip-git-repo-check","--json","--model","gpt-6-sol","-c","model_reasoning_effort=\"medium\""],"skipped":[]}
```

`argv` holds the positional prompt but never stdin: a caller that piped a prompt must send it to the child itself. `effort` is `null` for a model without efforts; `skipped` lists, as the caller spelled them, the arguments that got a skip warning: an unknown or disabled `--cli`, arguments the chosen CLI does not map or maps to nothing, and raw tokens after `--` without `--cli`.

Exec mode, with the prompt on stdin and the output format fixed by pinning the CLI:

```sh
agrouter exec --cli=claude --dangerously-skip-permissions --output-format stream-json --verbose < prompt.txt
```

On Windows with npm's `claude.cmd`/`codex.cmd` shims, pass a multi-line prompt on stdin: cmd.exe cannot carry a line break inside an argument, so such a positional prompt fails to start (exit `127`).

The child's stdout, stderr and exit code are agrouter's. An argument the chosen CLI does not map is skipped with one `agrouter: warning:` line on stderr, never an error. Set `AGROUTER_DEBUG=1` to see eligibility, Jev's probabilities and the final command on stderr (prompt text and key redacted).

When Jev cannot decide (no key, timeout, API errors) and the CLI is known (`--cli`, implied by `--model`, or the only one left), agrouter runs it with only the caller's fixed `--model`/`--effort`, so the CLI's defaults apply. With more than one CLI left, it exits `2`.

## Configuration

INI files, merged per section and per key, later layers winning:

1. embedded defaults ([`pkg/config/defaults/config`](pkg/config/defaults/config)): the catalog, questions and argument mappings;
2. global `~/.config/agrouter/config` (directory overridable with `AGROUTER_CONFIG_DIR`);
3. local `.agrouter/config` in the working directory;
4. environment (`TYPESAFE_API_KEY`, `AGROUTER_CLI`).

A local file can change one key without redefining the catalog; `enabled = false` removes a section (a disabled `[cli.*]` also drops its models). Comments must be on their own lines: inline `;` becomes part of the value. For example:

```ini
# ~/.config/agrouter/config
[agrouter]
api_key   = ...
# pin a Jev version to keep routing stable; the whole routing budget is timeout
jev_model = jev-1.13.0
timeout   = 20s

# never route to Codex
[cli.codex]
enabled = false
```

A new CLI, model, effort or argument mapping is added in config; no code names a CLI. See the design's [Configuration](docs/design.md#configuration) for every key.

## ralphex

Claude mode (`--cli=claude` keeps ralphex's stream-json parser working):

```ini
# ~/.config/ralphex/config
claude_command = agrouter
claude_args    = exec --cli=claude --dangerously-skip-permissions --output-format stream-json --verbose
```

Codex mode (`executor = codex`) and the external Codex review:

```ini
# ~/.config/ralphex/config
codex_command          = agrouter
# empty, so Jev chooses; ralphex's defaults would otherwise fix both
codex_model            =
codex_reasoning_effort =
```

A `--model`/`--effort` (or `-c model=`/`-c model_reasoning_effort=`) that ralphex adds from `task_model`, `review_model`, `plan_model`, `codex_model` or `codex_reasoning_effort` is a constraint; leave them empty to let Jev choose.

**`idle_timeout`:** agrouter prints nothing while it routes, up to `[agrouter] timeout` (10s by default). A ralphex `idle_timeout` must exceed that plus the child's time to its first output, or ralphex kills every run during routing.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | decision printed, or `--help`/`--version` |
| `2` | agrouter error before any child starts: unknown flag, second positional, no prompt, config error, or Jev cannot decide with more than one CLI left |
| `127` | exec mode: the chosen CLI could not be started |
| other | exec mode: the child's exit code (`128+signal` on Unix when it was killed) |

## Releasing

1. Merge the release changes into `master`.
2. Tag a commit on `master` with a `v0.x.y` or `v1.x.y` semver tag (optionally with a lowercase pre-release suffix, e.g. `v0.1.0-rc.1`) and push the tag:

   ```sh
   git tag v0.1.0
   git push origin v0.1.0
   ```

3. The `release` workflow validates the tag, re-runs the tests, creates a GitHub Release with generated notes (pre-release for suffixed tags) and warms the Go module proxy. The repository must be public for the proxy to fetch the module.
4. Install the release:

   ```sh
   go install github.com/SvetlovA/agrouter/cmd/agrouter@v0.1.0
   ```
