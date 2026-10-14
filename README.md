# agrouter

agrouter picks the coding-agent CLI, model and reasoning effort for a prompt by asking TypeSafe's Jev, from a catalog of Claude Code and Codex options (and any other CLI described in config). Cheap models get trivial work, strong ones get hard work, per call, and the caller never needs to know each CLI's flags.

It has two modes:

- **Decision** (default): print the choice, project complexity and per-stage confidence as one JSON line. Add `--verbose` for the argv and other details when the caller launches the CLI itself.
- **Exec** (`agrouter exec <cli>`): route within the named CLI, add the chosen model and effort through config, append the CLI's own arguments given after `--`, and run it.

## Install

Requires Go 1.27 or later. Exec mode also needs the chosen CLI (`claude`, `codex`) on `PATH`; otherwise agrouter exits `127` before routing.

```sh
go install github.com/SvetlovA/agrouter/cmd/agrouter@latest
agrouter --version
```

To build from a checkout on Unix, run `make build`. On Windows, use GNU Make and PowerShell:

```powershell
make -f WMakefile build
```

Both makefiles name the executable `.bin/agrouter` on `master` and `.bin/agrouter.<branch>` on other branches, with `.exe` appended for Windows builds and filename-unsafe branch characters (including `/`) replaced by `-`. For example, branch `feature/example` produces `.bin/agrouter.feature-example.exe` on Windows; a detached HEAD uses `HEAD`. `WMakefile` also supports `test`, `lint`, `fmt`, `generate` and `eval-routing`.

On the first routing invocation, agrouter creates `~/.config/agrouter/config` (or the directory selected by `AGROUTER_CONFIG_DIR`) as a commented template. `go install` only installs the executable; `--help` and `--version` do not create config.

Before using Jev routing, edit the global config, uncomment `[agrouter]` and `api_key`, and set your TypeSafe API key. Creating the template leaves the key empty:

```ini
[agrouter]
api_key = your-typesafe-api-key
```

You can also supply the key through the environment:

```sh
export TYPESAFE_API_KEY=...
```

The key comes from `--jev-api-key`, then `TYPESAFE_API_KEY`, then `api_key` in config. Prefer the environment variable or the global config with user-only permissions: a command-line flag shows up in process listings, and a local `.agrouter/config` is easy to commit. The key is never printed and never reaches the child.

## Usage

```
agrouter           [options] [-p TEXT] [--prompt-file PATH] [--doc PATH]... [prompt]                       # decision mode
agrouter exec <cli> [options] [-p TEXT] [--prompt-file PATH] [--doc PATH]... [prompt] [-- native args...]  # exec mode
```

Exec needs the CLI to run right after `exec`; a missing operand, or a CLI that is not configured and enabled, is exit `2` before Jev is asked. `--cli` cannot supply it: `--cli` equal to the operand is accepted as redundant, a different one is exit `2`. Decision mode rejects `--`, even with nothing after it.

The prompt can come from four sources, in any combination: `-p TEXT`, the one positional argument, the text of `--prompt-file PATH` and stdin, all before `--`. agrouter joins them in that order with a blank line between them; at least one must be non-empty. Nothing after `--` is read as a prompt: text there goes to the child but is not routed on. `-p`, `--prompt` and `--print` are one option: it may be given once, and a value that looks like a flag (`-p --model`) is rejected, as is a bare `--print` with no value before `--`. `--prompt-file` may be given once. Arguments follow Claude Code's names:

| Option | Meaning |
|---|---|
| `--cli=NAME` | decision mode: route only within one CLI (`claude`, `codex`, ...); exec takes its CLI from the operand |
| `--model=M`, `--effort=E` | fix the model or effort; Jev chooses the rest. Values outside the catalog are passed through for the CLI to validate |
| `-p TEXT`, `--prompt=TEXT`, `--print=TEXT` | the prompt, before the positional prompt, `--prompt-file` and stdin |
| `--prompt-file=PATH` | read prompt text from a file |
| `--doc=PATH` | a project doc (`CLAUDE.md`, `AGENTS.md`, ...) for routing only; repeatable, never passed to the child |
| `--verbose` | full agrouter decision details; never affects routing. Before `--` it is agrouter's only; after `--` it is the child's own option |
| `--jev-api-key=KEY` | Jev API key; an empty value clears it |
| `-- native args...` | exec only: appended to the child's argv unchanged and in order, including empty tokens, repeats and later `--`. A native spelling that selects the model or effort is exit `2` (see [Invocation descriptors](#invocation-descriptors)) |
| `--help`, `--version` | print help or the version and exit |

Whenever stdin is not a terminal, agrouter reads it to EOF within `[agrouter] timeout` before routing. A script that passes the prompt only as an argument or a file should close stdin or redirect it from `/dev/null`; an inherited pipe that stays open uses up the timeout, and Jev cannot decide.

`--prompt-file` and `--doc` paths are relative to the working directory and may point anywhere. Each must be a regular text file: a missing, unreadable, directory or binary file is an error naming the flag and path (exit `2`), even when routing would not need it. Neither size nor the number of chunks is limited. If the timeout passes while a `--doc` is being read, Jev cannot decide (see below); while the `--prompt-file` is being read, it is an error (exit `2`), since the child needs that text.

The `-p`, positional and `--prompt-file` text reaches the child as one argument, so in exec mode it is bounded by the OS argument limits (128 KiB per argument on Linux, 32,767 characters for the whole command line on Windows); a longer prompt fails to start the child (exit `127`). Pipe a very long prompt on stdin instead: stdin has no such limit.

To route, agrouter also reads the text files the prompt names inside the working directory (quoted, backticked, Markdown-linked or plain paths) and sends their contents, never their paths, to TypeSafe. That includes files such as `.env` when the prompt mentions them. Paths outside the working directory are ignored and URLs are not fetched; binary files are described by type and size only. Nothing is read when only one option is left. The `--prompt-file` text counts as prompt, so the files it mentions are read too; `--doc` text is not scanned for mentions.

### Staged routing

agrouter routes in up to three stages, each narrowing the options for the next:

| Stage | Jev chooses among | Asked when |
|---|---|---|
| `cli` | the eligible CLIs | more than one CLI is left |
| `model` | the chosen CLI's eligible models | more than one model is left |
| `effort` | the chosen model's effort levels | more than one effort is left |

A stage with one candidate is skipped without asking Jev. Each `cli` candidate is described together with its models, their prices and effort levels, so Jev can weigh what lies below it; each `model` candidate with its efforts. A stage's confidence is therefore conditional on the stages before it.

A value the caller passes fixes its stage, because eligibility filters by it before routing:

| Passed | Stages asked |
|---|---|
| `--cli X` | `model` and `effort` within X (an unknown or disabled `--cli` is skipped with a warning and fixes nothing) |
| `--model M` from the catalog | `effort` only: M belongs to one CLI |
| `--model M` outside the catalog | `cli` only, when more than one CLI is left; M is passed through with the caller's `--effort`, if any |
| `--effort E` | `cli` and `model`; E is never re-decided |
| `--cli`, `--model` and `--effort` | none: one option is left and Jev is not asked |

If any stage fails, the whole routing fails and the cannot-decide policy below applies to the original eligibility; the stages that completed appear only in `--verbose` and `AGROUTER_DEBUG=1` output. Native arguments after `--` exist only in exec, whose CLI is always the operand.

### Project complexity

The same task can need a different model in a small script than in a large, constrained system. With `--doc`, agrouter first asks Jev to rate the project the docs describe, from 0 (a snippet) to 10 (a large enterprise system), without the prompt: one request when the docs fit, otherwise one per doc chunk, all at once, merged into a mean weighted by how much each chunk says about the project's size, architecture, dependencies or constraints. Every routing stage then gets `"project":{"complexity":6.9}` in its state, as context for judging the task rather than a multiplier: a typo fix stays trivial in any codebase. The doc text itself is not resent.

```sh
agrouter --doc CLAUDE.md --doc docs/architecture.md -p "refactor the retry logic"
```

This stage runs only when `--doc` is given and more than one option is left. If it fails (a Jev error, or the timeout), the cannot-decide policy below applies; docs are never silently dropped. Without `--doc` there is no complexity stage and routing is unchanged.

Complexity uses Jev's Score question with ten ordered descriptions. Jev returns a numeric score from 0 to 9, which agrouter scales to 0 to 10 before combining document chunks. Evidence remains a separate Noul weight; confidence is recorded only.

### Long inputs

A prompt with its files that is too large for one request is split into chunks, all sent at once. Each chunk also rates how relevant its text is to the task, and the chunks' answers are averaged with that relevance as weight, so filler with zero relevance has no effect. Low but non-zero relevance still dilutes the result when there is a lot of it. Every routing stage asks all chunks and pools their answers; every chunk then moves on to the same narrowed next stage.

`[agrouter] timeout` covers the whole routing: reading the inputs, the complexity stage and every routing stage. Routing makes up to three sequential Jev round trips (`cli`, `model`, `effort`), plus one for the complexity stage with `--doc`; chunks within a stage run in parallel, but a re-split after a too-large request adds another round trip. The timeout is cooperative: reading stdin and files and every Jev request stop at it, but splitting and encoding a very large input do not. Raise it when routing often runs out of time, typically with very long inputs, many `--doc` files or a slow network.

Decision mode:

```sh
agrouter -p "fix the flaky test in pkg/foo"
# {"cli":"codex","model":"gpt-6.1-sol","effort":"medium","confidence":{"cli":0.82,"model":0.64,"effort":0.41}}
```

Add `--verbose` for indented JSON with every stage, chunk and option, plus a readable command. The default stays on one JSON line. For example, with project docs the concise result can be:

```json
{"cli":"codex","model":"gpt-6.1-sol","effort":"medium","project_complexity":"6.9/10","confidence":{"cli":0.82,"model":0.64,"effort":0.41,"project_complexity":0.9}}
```

`project_complexity` is the final project score formatted as `score/10` with one decimal, for example `5.1/10` and is omitted when the complexity stage did not complete. In verbose output, `argv` holds the `-p`, positional and `--prompt-file` text, joined like the routing prompt, as its last token, after `--` in the shipped `prompt_args`, so text that starts with `-` reaches the child as the prompt and never as one of its flags. It never holds stdin: a caller that piped a prompt must send it to the child itself. `--doc` is never in `argv`; the child loads its own `CLAUDE.md`/`AGENTS.md`. `effort` is `null` for a model without efforts, and `cli` is `null` only when routing failed with more than one CLI left (see [Failure and null reasons](#failure-and-null-reasons)).

Exec mode, with the prompt on stdin and the CLI's own options after `--`:

```sh
agrouter exec claude -- --dangerously-skip-permissions --output-format stream-json --verbose < prompt.txt
```

agrouter reads stdin for routing and replays the same bytes to the child, so the prompt never goes into argv. The `--verbose` after `--` is Claude's own (`claude -p` requires it for stream-json); agrouter adds none itself.

On Windows with npm's `claude.cmd`/`codex.cmd` shims, pass a multi-line prompt on stdin: cmd.exe cannot carry a line break inside an argument, so a multi-line `-p`, positional or `--prompt-file` prompt fails to start (exit `127`). This includes any prompt combined from two sources, since they are joined with a blank line. Decision mode and native executables are unaffected.

Before starting the child, exec mode logs one line to stderr: `agrouter: selection: ` followed by a JSON object with `cli`, `model`, `effort`, any completed `project_complexity` score and the same `confidence` object as decision mode, so tools such as Ralphex show the selection for each step. The prefix keeps the line from passing for one of the child's stream-json events when a caller merges stderr into stdout (Ralphex drops JSON lines that aren't events and prints other lines); strip it to parse the JSON. Model and effort are `null` when the CLI's defaults apply, with the same `reason` and `null_reasons` as decision mode, plus exec's own `fallback`, `effective` and `readiness` fields (see [Exec fallback](#exec-fallback)). The line stays concise with agrouter's `--verbose` and never includes the prompt or argv.

Decision JSON and the exec selection line include `confidence` when Jev answered:

| Key | Meaning |
|---|---|
| `cli`, `model`, `effort` | Jev's confidence in each routing stage it was asked; a skipped stage is omitted, and all three are omitted when Jev could not decide |
| `project_complexity` | confidence in the project complexity score; omitted without a completed complexity stage |

Each value is the arithmetic mean of that stage's final request confidences, including zeros, without relevance or evidence weights; a single request uses its own confidence. Stage confidences are conditional, each given the stages before it, and none of them is the probability that the whole result is correct. Confidence changes neither weighting nor the choice.

`cli`, `model` and `effort` replace the former `model_selection` key; readers of `confidence.model_selection` must switch to them.

Verbose output includes all available numeric details, without truncating chunks or options:

| Field | Meaning |
|---|---|
| `options` | every eligible option's ID, CLI, model and effort, in catalog order, before any stage narrowed them |
| `confidence.stages` | every routing stage in order (`cli`, `model`, `effort`) with its `level`, `choice` and `skipped`; a skipped stage's choice is the value its options share |
| `confidence.stages[].confidence`, `probabilities` | for a stage asked with the whole request: Jev's Choice confidence and the probability of every candidate: the CLI name, the model's `[model.<section>]` name (the passed model under passthrough), or the effort label |
| `confidence.stages[].chunks` | for a pooled stage: every chunk's field, index/count, choice, Choice confidence, relevance weight and full probability map |
| `confidence.stages[].pooled_scores` | for a pooled stage: every candidate's `name` and final `score` in catalog order, using relevance weights or an ordinary mean when all relevance is zero; catalog order breaks ties |
| `confidence.complexity_chunks` | every document request's index/count, formatted project complexity (`5.1/10`), raw numeric `score` for precise calculations, Score confidence and evidence weight; scores use evidence weights or an ordinary mean when all evidence is zero |
| `argv` | exact child arguments; decision mode only |
| `command` | readable command text for copying; adapt quoting to your terminal; decision mode only |
| `stdin_required` | whether the command needs the original stdin supplied again; stdin contents are not included in the command |

Stage probabilities and pooled scores are separate from Jev's Choice confidence. Verbose JSON retains zero values. The exec selection log remains one prefixed line on stderr and excludes argv and command; the child's output goes to stdout.


The child's stdout, stderr and exit code are agrouter's. Set `AGROUTER_DEBUG=1` to see eligibility, each `--doc` chunk's complexity score, evidence and confidence with the project complexity, one line per routing stage (`stage <level>: skipped (<choice>)`, or the choice, confidence and top candidates, per chunk when pooled), Jev's probabilities and the final command on stderr (prompt text and key redacted; doc text is never printed). In exec mode it also prints one `readiness <cli>:` line per piece of availability evidence (dimension, status, reason code, window, model scope, reset, check), the defaults the checks reported, and how many options they excluded; never a probe's raw response.

When Jev cannot decide (no key, timeout, API errors, at any stage) and the CLI is known (`--cli`, implied by `--model`, or the only one left), agrouter runs it with only the caller's fixed `--model`/`--effort` and a model Jev already chose before a later stage failed, so the CLI's defaults apply to the rest. With more than one CLI left, decision mode exits `2`: the one-line `agrouter:` error goes to stderr and one decision JSON object with `null` values and the reasons goes to stdout, without `argv`, `command` or stage confidence. `AGROUTER_DEBUG=1` shows the stages that completed.

### Failure and null reasons

Decision JSON and the exec selection line explain a failed routing and every `null` value, with or without `--verbose`. A successful, fully populated selection has neither field.

| Field | Meaning |
|---|---|
| `reason` | why routing failed: a normalized `code`, the failing `stage` (`complexity`, `cli`, `model` or `effort`) when known, and a fixed `message` |
| `null_reasons` | one entry per `null` among `cli`, `model` and `effort`, each with a `code` and a fixed `message` |

| `reason.code` | Cause |
|---|---|
| `jev_no_key` | no Jev API key is set |
| `jev_unauthorized` | Jev rejected the key (HTTP 401) |
| `jev_timeout` | `[agrouter] timeout` passed |
| `jev_canceled` | routing was canceled |
| `jev_api_error` | Jev answered with another error status, including rate limits that outlasted the retries |
| `jev_transport_error` | Jev could not be reached, or the connection failed |
| `jev_invalid_answer` | Jev answered with an answer that does not fit the question |
| `jev_empty_answer` | Jev left a question without an answer, or answered `null` or without the value asked for |
| `no_decision` | any other reason Jev could not decide |

| `null_reasons.*.code` | Meaning |
|---|---|
| `routing_failed` | routing failed with the CLI known; the child's default applies |
| `unresolved` | routing failed with more than one CLI left; nothing was chosen |
| `delegated` | routing succeeded and left the value to the child, for example the effort of a passed-through `--model` |
| `no_applicable_effort` | the chosen model has no effort levels |

Messages never carry the prompt, `--doc` text, the key or Jev's response body. A value the caller fixed (`--cli`, `--model`, `--effort`) stays set after a failure. For example, `--cli=alpha` with the model stage timing out prints:

```json
{"cli":"alpha","model":null,"effort":null,"reason":{"code":"jev_timeout","stage":"model","message":"Jev model selection timed out"},"null_reasons":{"model":{"code":"routing_failed","message":"Model delegated to the child after routing failed"},"effort":{"code":"routing_failed","message":"Effort delegated to the child after routing failed"}}}
```

### Exec readiness

Before any Jev request, including the `--doc` complexity stage, exec checks that its CLI can run now: the executable, then the prompt-free checks in `[cli.<name>.availability]` (authentication, the subscription five-hour and weekly quota windows, model support). The checks run once per invocation, while agrouter reads the prompt, docs and stdin, within both the availability `timeout` and `[agrouter] timeout`; nothing is cached between invocations. Decision mode never runs them.

Exec then routes only over the CLI's options that are left. It never switches to another CLI and never broadens a `--model`/`--effort` the caller fixed. Every outcome is one stderr line:

| Outcome | stderr | Exit |
|---|---|---|
| some options are unverified (a probe timed out, was throttled, or did not report a window) and `unknown = allow` | `agrouter: warning: <cli> availability unknown: <reason>`, then routing and the child as usual | the child's |
| nothing is left: missing executable or failed authentication | `agrouter: <cli> unavailable: <reason>` | `127` |
| nothing is left: subscription quota exhausted | `<quota_error_prefix><quota_error_message>; <windows> subscription quota exhausted[; resets at <RFC 3339>]` | `127` |
| nothing is left under `unknown = block` | `agrouter: <cli> availability unverified: <reason>` | `2` |

A preflight block prints no selection line, nothing on stdout, and makes no Jev request. (A block on a delegated model's default comes after routing; see [Exec fallback](#exec-fallback).) The reset is the latest reset of the exhausted windows, since a weekly window outlasts a five-hour one, and is left out when any of them is unverified. `quota_error_prefix` (default `agrouter: `) and `quota_error_message` come from config, so the line can match a caller's own limit pattern, for example a severity prefix such as `ERROR: agrouter: `. An unknown availability is never reported as a quota block. Native arguments after `--` that switch the child's context (`context_flags`, or a config flag with a `context_config_keys` key) make the probes moot: only the executable is checked, and the rest is unknown (`context_changed`).

The `<reason>` in these lines, and the reasons in the selection line's `readiness`, are fixed codes:

| Reason | Kind | Meaning |
|---|---|---|
| `executable_missing`, `auth_failed` | block (`127`) | the CLI is not on `PATH`, or its login failed |
| `quota_exhausted`, `quota_denied` | block (`127`) | a quota window is used up, or the backend refuses it whatever its usage |
| `unsupported_model` | excludes the model | the CLI reports it cannot run that model |
| `availability_not_configured`, `availability_disabled` | unknown | no availability section, or `enabled = false`: only the executable was checked |
| `context_changed` | unknown | native arguments switch the child's provider, profile or settings |
| `transport_unsupported`, `version_unsupported` | unknown | the check's transport is not known, or the CLI is older than `min_version` |
| `probe_missing`, `probe_timeout`, `probe_failed`, `probe_response_too_large`, `probe_malformed`, `probe_throttled` | unknown | the probe could not start, ran out of time, exited nonzero, answered too much, answered off-schema, or was rate-limited |
| `not_reported`, `window_not_reported`, `usage_permission_not_reported` | unknown | the probe did not report a value, a quota window, or whether usage is allowed |
| `extra_usage_enabled` | unknown | a full window that paid overage may still cover |
| `quota_reset_passed` | unknown | a block whose reset time has already passed |
| `delegated_model_unverified` | unknown | no model is set and some model has its own quota block |
| `evidence_invalid` | unknown | evidence with a status or window the evaluation does not know |

**Note:** a block stops exec; an unknown is one warning under `unknown = allow` and exit `2` under `unknown = block`. `not_subscription` (API-key or other-provider auth) needs no quota windows and is neither.

The shipped checks take about 0.5s to 2s on a cold start, which is part of the routing budget (`[agrouter] timeout`, 10s by default). A probe cut off by either timeout is unknown, not a block.

### Exec fallback

When Jev routing fails in exec, the child runs with its own defaults for whatever Jev did not choose. agrouter makes this unmistakable: one warning line comes before the selection line, and the selection line says which values fell back and what they are.

```text
agrouter: warning: Jev routing failed at the model stage (jev_timeout); model and effort fall back to tool-a defaults, not a Jev selection: model m-2 (verified child default), effort unknown (child did not report it)
agrouter: selection: {"cli":"tool-a","model":null,"effort":null,"reason":{...},"null_reasons":{...},"fallback":{"action":"child_defaults","fields":["model","effort"],"message":"Model and effort fall back to the child's defaults, not a Jev selection"},"effective":{"model":{"value":"m-2","source":"verified_child_default"},"effort":{"value":null,"source":"child_default_unknown","reason":{"code":"default_not_reported"}}}}
```

Note: the first line is readable without parsing JSON; the second carries the same facts for tools.

| Field | Meaning |
|---|---|
| `fallback` | only after a routing failure: `action` (`child_defaults`), the fallen-back `fields`, and a fixed `message` |
| `effective` | when a model or effort is left to the child: each value's `value` and `source`, with a `reason` when the default is unknown |
| `readiness` | when readiness excluded options or warned: `excluded` (option count by reason) and `unknown` (the warning's reason) |

| `effective.*.source` | Meaning |
|---|---|
| `router` | agrouter chose it: Jev, or the one option left |
| `caller` | `--model` or `--effort` fixed it |
| `verified_child_default` | the child's own default, as its prompt-free probe reported it for this working directory and settings |
| `child_default_unknown` | the child's default is not known: `default_not_reported`; `context_changed` when native arguments switch the child's context; or `default_for_other_model` when the probe reported the default effort of the child's default model, but agrouter or the caller set another model |

Values Jev chose before the failure (the model when only the effort stage failed) and values the caller fixed keep their source and are never listed as fallbacks. Top-level `model`/`effort` stay `null` whenever agrouter passes no selection flag, even when `effective` shows a verified default. A catalog default or a model list's recommendation is never reported as verified. An intentional omission (a model without effort levels, the effort of a passed-through `--model`) is no fallback and prints no warning.

A delegated model is checked against model-scoped quota blocks. When the verified default is blocked, exec prints the quota line and exits `127` without starting the child, as a preflight block does. When the default is unknown and some model has its own block, the model is unverified (`delegated_model_unverified`): a warning under `unknown = allow`, exit `2` under `unknown = block`.

## Configuration

INI files, merged per section and per key, later layers winning:

1. embedded defaults ([`pkg/config/defaults/config`](pkg/config/defaults/config)): the catalog, routing policies and invocation descriptors;
2. global `~/.config/agrouter/config` (directory overridable with `AGROUTER_CONFIG_DIR`);
3. local `.agrouter/config` in the working directory;
4. environment (`TYPESAFE_API_KEY`).

`AGROUTER_CLI` is no longer read. When it is set, agrouter prints one warning and ignores it: use `exec <cli>` to run a CLI, or `--cli=NAME` to restrict a decision.

The global config is installed with every setting and section commented out. Uncomment the section header and settings you want to customize. Missing, empty or comment-only files are refreshed from the current embedded defaults on each routing invocation; files with any active content are preserved. New directories use permissions `0700` and new files `0600`. Local `.agrouter/config` files are never created automatically.

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

CLI names, model IDs and aliases, and effort labels come from the merged config. Adding or renaming any of them, or changing how a CLI is invoked, requires no Go code changes. The embedded defaults document every key.

### Invocation descriptors

Each `[cli.<name>]` section says how agrouter builds that CLI's argv. CLI names cannot contain `.`.

```text
argv = command + exec_args + model_args + effort_args + native args after "--" + prompt_args
```

**Note:** agrouter emits each part left to right. `model_args` and `effort_args` appear only for a value routing selected or the caller fixed; `prompt_args` only when there is a `-p`, positional or `--prompt-file` prompt.

| Key | Holds |
|---|---|
| `exec_args` | the noninteractive invocation, for example `["-p"]` or `["exec"]`; required, may be `[]` |
| `model_args` | how to pass the model, with `{model}`; required |
| `effort_args` | how to pass the effort, with `{effort}`; required when a model of the CLI has efforts |
| `prompt_args` | how to pass the prompt; `{prompt}` must be its last element and a whole token; required |
| `selection_flags` | native spellings that select the model or effort, for example `--model, -m, --effort` |
| `config_flags`, `selection_config_keys` | a native config flag (`-c`) and the keys that select the model or effort through it; set together |
| `context_flags` | native spellings that switch the CLI's provider, profile or settings |
| `context_config_keys` | config keys that switch them through a `config_flags` flag, dotted subkeys included (for example `profile` also covers `-c profile.x=y`); needs `config_flags` |

Template keys are JSON arrays of strings; the flag and key lists are comma-separated. Only `{model}`, `{effort}` and `{prompt}` are placeholders, each only in its own key. An unset model or effort leaves its args out, so the CLI's own default applies.

agrouter has no other per-CLI argument mapping: pass every other native option after `--` with exec. Before routing, exec scans the native tokens up to the child's own `--`. A selection flag (`--model x`, `--model=x`, `-mx`) or a config flag with a selection key (`-c model=x`, `-cmodel=x`, `--config=model=x`) is exit `2`, naming the spelling but never its value. Use agrouter's `--model`/`--effort` instead, so routing knows the constraint. There is no option-arity table, so a native value that is literally a reserved spelling is rejected too.

When `prompt_args` puts the prompt after `--` and the native tokens already hold a `--`, a command-line prompt is exit `2`: the child would read the prompt's `--` as prompt text. Pass the prompt on stdin, or drop the extra `--`.

The former `[cli.<name>.args]` mapping sections are config errors that name the replacement keys. `[cli.<name>.availability]` sections configure the exec readiness checks.

### Availability checks

Each CLI's exec readiness checks live under its own `[cli.<name>]` section, so a new CLI needs only config. A CLI without an availability section, or with `enabled = false`, gets the executable check only. Its quota is then unknown: each exec prints one `availability unknown` warning (`availability_not_configured` or `availability_disabled`), or exits `2` under `unknown = block`.

```text
[cli.<name>.availability]            policy: which checks, timeout, unknown policy, quota line
  └── [cli.<name>.availability.<check>]   one probe: transport, command, args, min_version
```

**Note:** the policy section lists its checks by name; each named check has its own subsection. Subsections not listed in `checks` are ignored, so a local layer can replace the list.

| `[cli.<name>.availability]` key | Meaning |
|---|---|
| `enabled` | `false` skips every check except the executable |
| `checks` | comma-separated check names, run in order |
| `timeout` | bound for all of the CLI's checks together (default `5s`); `[agrouter] timeout` bounds them too |
| `unknown` | `allow` (default) runs with one warning when a check cannot verify; `block` exits `2` instead |
| `quota_error_prefix` | start of the quota line (default `agrouter: `) |
| `quota_error_message` | text after the prefix, for example a caller's limit pattern |

| `[cli.<name>.availability.<check>]` key | Meaning |
|---|---|
| `transport` | `json-helper`, `sdk-control-usage-v1` (stream-json control reads) or `jsonrpc-ratelimits-v1` (stdio JSON-RPC rate-limit reads); required |
| `command` | the probe executable; the CLI's own `command` when unset |
| `args` | the probe's arguments, a JSON array |
| `min_version` | for the two built-in transports: a CLI older than this, or one that reports no version, is unknown (`version_unsupported`) |

Every probe runs in the real working directory, with the user's real settings and login, its own stdin, a 1 MiB response limit, and without the Jev key in its environment. A probe never sends a prompt or a user message, so it starts no inference. When it exits, times out or overflows, agrouter kills its whole process tree (a process group on Unix, a Job Object on Windows).

**A third CLI with `json-helper`.** A helper is any executable that prints one JSON object on stdout and reads nothing from stdin. Every section is optional; a missing one is unknown:

```ini
# .agrouter/config
[cli.tool-x]
command     = tool-x
description = Example agent CLI.
exec_args   = ["run"]
model_args  = ["--model", "{model}"]
prompt_args = ["--", "{prompt}"]
selection_flags = --model

[cli.tool-x.availability]
checks = ready

[cli.tool-x.availability.ready]
transport = json-helper
command   = tool-x-ready

[model.tool-x-large]
cli         = tool-x
name        = tool-x-large
description = The example CLI's large model.
```

```json
{
  "schema": "agrouter.readiness/v1",
  "auth": {"status": "available"},
  "quota": [
    {"window": "five_hour", "status": "available"},
    {"window": "weekly", "status": "unavailable", "reset_at": "2026-10-20T09:00:00Z"},
    {"window": "weekly", "status": "unavailable", "models": ["tool-x-large"], "reset_at": "2026-10-18T09:00:00Z"}
  ],
  "capability": {"unsupported_models": []},
  "defaults": {"model": "tool-x-large", "effort": null}
}
```

**Note:** `status` is `available`, `unavailable`, `unknown` or `not_applicable` (for example quota under API-key auth). `window` is `five_hour` or `weekly`; `models` scopes a window to some models (matched against model names, sections and aliases, ignoring case); `reason` is an optional lowercase code. `defaults` are what the child would use without selection flags; leave a value `null` when unknown. Output that does not match this schema, extra fields included, is unknown (`probe_malformed`).

Limitations:

- The built-in transports read experimental, undocumented CLI protocols. `min_version` guards the versions they were checked against; when a CLI upgrade changes the protocol, the check reports unknown (or stops exec under `unknown = block`). agrouter never falls back to an inference probe or to an auth-only check.
- A `json-helper` has no `min_version` check: the helper decides what it supports.
- A probe cannot see native options after `--`. Options in `context_flags`, and config flags with a `context_config_keys` key, switch the child's provider, profile or settings, so with one of them exec checks only the executable.
- Evidence is not cached: every exec probes again.

To see what the stream-json control check reads, send the same prompt-free requests on stdin yourself. Nothing here is a user message, so the CLI answers from its login and settings without asking a model:

```sh
printf '%s\n' \
  '{"type":"control_request","request_id":"1","request":{"subtype":"initialize"}}' \
  '{"type":"control_request","request_id":"2","request":{"subtype":"get_usage","skip_behaviors":true}}' |
  claude -p --input-format stream-json --output-format stream-json --verbose \
    --no-session-persistence --strict-mcp-config --settings '{"disableAllHooks":true}'
```

Each request gets one `control_response` line with the same `request_id`; the second holds the subscription windows.

### Migrating from mapped arguments

agrouter no longer translates child options. Pass them after `--` with exec instead.

| Removed | Replacement |
|---|---|
| `agrouter exec --cli=NAME ...` | `agrouter exec NAME ...` |
| `AGROUTER_CLI` | `exec <cli>`, or `--cli=NAME` in decision mode; when set it is ignored with a warning |
| `--permission-mode`, `--dangerously-*`, `--output-format`, `--sandbox`, `-c`/`--config` | the CLI's own spelling after `--`: `agrouter exec <cli> -- --output-format stream-json` |
| native `--model`/`--effort` or `-c model=...` after `--` | agrouter's `--model`/`--effort` before `--` (a native selection is exit `2`) |
| `[cli.<name>.args]` sections | `exec_args`, `model_args`, `effort_args`, `prompt_args` and the native flag lists in `[cli.<name>]` |
| `skipped` in decision JSON and its skip warnings | none: nothing is mapped, so nothing is skipped |
| raw tokens after `--` in decision mode | none: decision mode rejects `--` |

The questions sent to Jev and the description of every state field are built into agrouter. Config holds only the preference they follow, in two `[agrouter]` keys that must be non-empty:

| Key | Preference |
|---|---|
| `routing_policy` | cost versus quality when choosing the CLI, model and effort; shared by every routing stage, for a whole prompt and for each chunk |
| `complexity_policy` | how the `--doc` stage judges the project the docs describe |

The old question keys are config errors that name their replacement, for example `[agrouter] question (global ~/.config/agrouter/config): removed; set routing_policy instead`. They mixed question structure with preference, so they are not converted: move only the preference text into the new key.

| Removed key | Replacement |
|---|---|
| `question`, `chunk_question` | `routing_policy` |
| `relevance` | no longer configurable; tune `routing_policy` |
| `complexity_question` | `complexity_policy` |
| `complexity_evidence` | no longer configurable; tune `complexity_policy` |

`max_chunks`, `chunk_parallel` and `relevance_floor` no longer exist: chunks are unlimited and run in parallel, and relevance has no floor. A config that still sets one of them fails with an unknown-key error.

## ralphex

*User-owned setup.* The config, wrapper and agrouter overrides below live in your Ralphex and agrouter configs, not in this repository; adapt them to your Ralphex version.

Ralphex passes the prompt on stdin and appends only native flags, so its arguments must land after agrouter's `--`. Claude mode: `claude_args` ends with `--`, and Ralphex's appended `--print` goes to `claude` unchanged.

Ralphex config:

```ini
# ~/.config/ralphex/config
# agrouter environment variables (set before starting ralphex, not as INI keys):
# TYPESAFE_API_KEY supplies Jev's key instead of [agrouter] api_key; --jev-api-key wins.
# AGROUTER_CONFIG_DIR selects the directory containing agrouter's global config file.
# AGROUTER_DEBUG=1 enables redacted stderr diagnostics; it does not enable --verbose.
# no environment overrides exist for model, effort, --doc, --verbose, timeout or jev_model.
claude_command = agrouter
claude_args    = exec claude --doc=CLAUDE.md -- --dangerously-skip-permissions --output-format stream-json --verbose
```

Codex mode (`executor = codex`) and the external Codex review. `codex_command` accepts only an executable, and Ralphex prepends Codex's own `exec`, so point it at a small wrapper that drops that `exec` (an executable `agrouter-codex` on `PATH`):

```sh
#!/bin/sh
if [ "$1" != exec ]; then
    printf '%s\n' 'agrouter: expected native exec subcommand' >&2
    exit 2
fi
shift
exec agrouter exec codex --doc=CLAUDE.md -- "$@"
```

```ini
# ~/.config/ralphex/config
codex_command          = agrouter-codex
# empty, so Jev chooses; ralphex's defaults would otherwise fix both
codex_model            =
codex_reasoning_effort =
# wait for a recognized limit to pass, then retry; without it ralphex stops
wait_on_limit          = 15m
```

When a subscription quota is exhausted, exec prints one quota line and exits `127`, and Ralphex's own limit patterns decide whether to wait and retry. Each retry probes again, without asking Jev, until the quota is back; a weekly block outlasts a five-hour reset. Ralphex's Claude executor matches limit patterns on any line, while its Codex executor matches them only on `ERROR:`, `FATAL:` or `PANIC:` lines, so the Codex line needs a severity prefix:

```ini
# ~/.config/agrouter/config
[cli.claude.availability]
quota_error_prefix  = "agrouter: "
quota_error_message = You've hit your limit

[cli.codex.availability]
quota_error_prefix  = "ERROR: agrouter: "
quota_error_message = Rate limit exceeded
```

```text
agrouter: You've hit your limit; weekly subscription quota exhausted; resets at 2026-10-13T15:00:00Z
ERROR: agrouter: Rate limit exceeded; weekly subscription quota exhausted; resets at 2026-10-13T15:00:00Z
```

**Note:** only the exec CLI's own line is printed. An authentication failure or an unknown availability never uses this wording, so Ralphex does not mistake it for a limit.

Leave `task_model`, `review_model`, `plan_model`, `codex_model` and `codex_reasoning_effort` empty to let Jev choose: a model or effort Ralphex appends after `--` goes to the child, not to routing.

**`idle_timeout`:** agrouter prints nothing while it routes, up to `[agrouter] timeout` (10s by default). A ralphex `idle_timeout` must exceed that plus the child's time to its first output, or ralphex kills every run during routing.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | decision printed, or `--help`/`--version` |
| `2` | agrouter error before any child starts: unknown flag (including the removed `--permission-mode`, `--dangerously-*`, `--output-format`, `--sandbox` and `-c`), second positional, second `-p`/`--prompt`/`--print` or `--prompt-file`, a missing, unknown or disabled exec CLI, a `--cli` conflicting with it, `--` in decision mode, a native model or effort selection after `--`, a command-line prompt after a second `--`, no prompt, a bad `--prompt-file` or `--doc` file, the timeout passing while the `--prompt-file` is read, config error, or Jev cannot decide with more than one CLI left (decision mode then also prints the decision JSON with `null` values and reasons) |
| `2` | exec mode: availability unverified under `unknown = block` (`agrouter: <cli> availability unverified: <reason>`), before routing or, for a delegated model's default, after it |
| `127` | exec mode: the chosen CLI could not be started; or before routing: its executable is missing, its authentication failed, or its subscription quota is exhausted (one quota line in the configured wording); or after routing: the child's verified default model is quota-blocked |
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
