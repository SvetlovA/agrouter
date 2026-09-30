# agrouter — design

agrouter picks the best `(cli, model, effort)` for a prompt from a configured catalog of coding-agent CLIs (Claude Code, Codex, and any other CLI described in config), by asking TypeSafe's **Jev** model. It has its own small, Claude-style argument set and two modes:

- **Decision** (default): print the choice, and the command line that would run it, as one JSON line. The caller launches the CLI itself.
- **Exec** (`agrouter exec`): translate agrouter's arguments into the chosen CLI's own arguments through config, and run it.

```bash
# decision: which CLI, model and effort, and the argv to run them with
agrouter -p "fix the flaky test in pkg/foo" --dangerously-skip-permissions --output-format stream-json
# {"cli":"codex","model":"gpt-6-sol","effort":"medium","argv":["codex","exec","fix the flaky test in pkg/foo","--dangerously-bypass-approvals-and-sandbox","--skip-git-repo-check","--json","--model","gpt-6-sol","-c","model_reasoning_effort=\"medium\""]}

# exec: run it
agrouter exec --dangerously-skip-permissions --output-format stream-json --verbose --print < prompt.txt
```

Status: design, not implemented. Last verified against upstream docs and CLIs on 2026-09-27; the v1 argument mappings were checked against the installed `claude --help` and `codex exec --help` on 2026-09-29.

## Goals

- One binary that any tool (ralphex first) can call to choose, and optionally run, a coding agent per prompt.
- Model and effort chosen per prompt by Jev, from a catalog with descriptions taken from the official vendor docs.
- **One vocabulary for every CLI.** Callers write agrouter's arguments once; each CLI's `[cli.*.args]` config section says how every one of them is spelled for that CLI. Model and effort are injected through the same mapping.
- A new CLI, model, effort or argument mapping is added by editing INI, with no code changes.
- **One implementation for every CLI.** The code has no CLI names and no per-CLI branches. What differs between CLIs lives only in its `[cli.*]` and `[cli.*.args]` sections. Claude and Codex are simply the config that ships; Gemini or any other CLI is another pair of sections, handled by the same code.

## Non-goals

- **No arbitrary argument translation.** Only agrouter's own [vocabulary](#arguments) is translated, by config. Arbitrary CLI flags are not understood; they can be passed through raw only when the CLI is pinned (see [Raw passthrough](#raw-passthrough)).
- **No output translation.** The caller receives exactly what the selected CLI writes. `--output-format stream-json` gives Claude stream-json from Claude and Codex JSONL from Codex. A caller that parses one output format (ralphex parses Claude stream-json) must pin `--cli`.
- **No prompt rewriting.** The child receives the positional prompt as one argv token and stdin byte-for-byte; when both are given, the CLI combines them itself (see [Prompt](#prompt)).

## Background

### Jev (TypeSafe)

Jev is a "System One" classifier, not a text-generating LLM. It evaluates typed questions against a `state` and returns structured answers ([intro](https://docs.typesafe.ai/introduction), [API](https://docs.typesafe.ai/api)).

- Endpoint: `POST https://api.typesafe.ai/v1/systemone`, `Authorization: Bearer <key>`, body `{state, model, questions}`.
- A **Choice** question has `instructions` and `criteria`. `criteria` is always a map of option name to description, with up to 255 options. `instructions`, and each description value inside `criteria`, can be a string, an object or an array, with free-form object field names. The answer is `{choice, probabilities, confidence}` ([Choice](https://docs.typesafe.ai/primitives/choice)).
- `confidence` (0–1) comes from how spread out `probabilities` is. TypeSafe recommends confidence-gated routing: act on high confidence and fall back on low ([confidence routing](https://docs.typesafe.ai/patterns/confidence-routing)).
- Limits for `jev-1.13.0`: 64k tokens per request for `state` plus **all** questions; **32k tokens for `state` plus the longest question**. Input is text only ([models](https://docs.typesafe.ai/models)). Verified against the models page on 2026-09-28.
- **No multi-part input.** Jev has no session or streaming-state API: each request is independent and ingests its `state` once. A state over 32k can only be handled as several independent requests whose answers are combined in the caller's code (see [Splitting large state](#splitting-large-state)).
- Accuracy falls as the state fills with detail unrelated to the question ([Jev 1.13 jaggedness](https://docs.typesafe.ai/model-jaggedness/jev-1.13), item 5). TypeSafe's advice is to filter first or use a Noul to judge relevance, and to combine atomic answers in code ([composite scoring](https://docs.typesafe.ai/patterns/composite-scoring)).
- Price: $0.042 per million input tokens; output tokens are free. A routing call costs a fraction of a cent.
- Errors: `401`, `422` (validation, including an oversized request), `429` (rate limit), `529` (overloaded). Retry `429` and `529` with backoff.

### Caller contract (ralphex)

ralphex is the first caller and fixes the constraints agrouter must respect (`ralphex/docs/custom-providers.md`, `pkg/executor/executor.go`, `pkg/executor/codex.go`):

- **Claude mode** runs `<claude_command> <claude_args...> [--model M] [--effort E] --print` and sends the **prompt on stdin**. `-p <prompt>` is supported only for backward compatibility. It parses stdout as Claude stream-json and detects `<<<RALPHEX:...>>>` signals in the assistant text.
  - It **merges the child's stderr into that stream**, and non-JSON lines feed its error and rate-limit pattern detection (`executor.go:123-145`, `508-520`).
- **Codex mode** (`--codex`) runs `<codex_command> exec ... --sandbox S -c model=... -c model_reasoning_effort=... -c stream_idle_timeout_ms=... [-c project_doc=...]` with the prompt on stdin (`codex.go:180-210`).
  - `codex_command` is an executable only; there is no `codex_args`.
  - stdout and stderr are read through separate pipes (`codex.go:78-85`, `238-267`).
- The child environment is inherited, minus Claude session markers, so `AGROUTER_*` variables reach agrouter in both modes (`codex.go:46-68`).

As a result, agrouter's only stderr output on a run that starts a child is one warning line per [skipped argument](#skipped-arguments), and ralphex's own calls map fully, so they get none. In Claude mode a warning would not be neutral: ralphex keeps non-JSON lines of the merged stream as diagnostics and scans them against its error and limit patterns even on a clean run (`executor.go:443-447`, `508-519`). The default patterns (`You've hit your limit`, `API Error: 4xx`, ...; `config/defaults/config:252-265`) do not match `agrouter: warning:`, but a caller's own patterns might. In Codex mode it is merely noise.

## CLI

```
agrouter      [options] [prompt] [-- raw args...]    # decision mode
agrouter exec [options] [prompt] [-- raw args...]    # exec mode
```

### Arguments

agrouter's argument names and style follow Claude Code. They fall into three groups.

**agrouter's own**, consumed by agrouter and never mapped:

| Option | Env | Meaning |
|---|---|---|
| `--cli=NAME` | `AGROUTER_CLI` | Restrict routing to one CLI (`claude`, `codex`, ...). Jev then picks only that CLI's model and effort. Empty: Jev picks across every configured CLI. |
| `--jev-api-key=KEY` | `TYPESAFE_API_KEY` | TypeSafe API key for Jev. See [API key](#api-key). |
| `-p`, `--print` | | Boolean, as in Claude, accepted so that `agrouter -p "x"` and ralphex's appended `--print` work. agrouter is always non-interactive, so the chosen CLI's `print` mapping is emitted **whether or not `-p` is given** (see [Argument mapping](#argument-mapping)): Claude's `--output-format` works only with `--print`, and Codex without `exec` starts its TUI. |
| `--help` | | agrouter help. To see a CLI's own help, call that CLI directly. |
| `--version` | | Print agrouter's version (the module version from `go install ...@<tag>`, or `unknown`) and exit `0`. |

**Model and effort**, a constraint on the choice and then mapped (see [Model and effort](#model-and-effort)):

| Option | Meaning |
|---|---|
| `--model=M` | Use this model. Jev chooses only its effort. `M` is a `[model.*]` `name` or one of its `aliases`. |
| `--effort=E` | Use this effort. Jev chooses only among options with that effort. |

**Mapped**, translated into the chosen CLI's arguments by its `[cli.*.args]` section (see [Argument mapping](#argument-mapping)). The v1 vocabulary:

| Option | Claude | Codex |
|---|---|---|
| `-p`, `--print` *(always emitted, first)* | `-p {prompt}` | `exec {prompt}` |
| `--permission-mode=bypassPermissions`, or its alias `--dangerously-skip-permissions` | `--dangerously-skip-permissions` | `--dangerously-bypass-approvals-and-sandbox --skip-git-repo-check` |
| `--permission-mode=plan` | `--permission-mode plan` | `--sandbox read-only -c approval_policy="never" --skip-git-repo-check` ≈ |
| `--permission-mode=acceptEdits` | `--permission-mode acceptEdits` | `--sandbox workspace-write` ≈ |
| `--permission-mode=auto` | `--permission-mode auto` | `--approve-for-me` ≈ |
| `--permission-mode=manual` | `--permission-mode manual` | *(unsupported)* |
| `--permission-mode=dontAsk` | `--permission-mode dontAsk` | *(unsupported)* |
| `--output-format=text` | `--output-format text` | *(nothing: the default, final message on stdout)* |
| `--output-format=json` | `--output-format json` | *(unsupported)* |
| `--output-format=stream-json` | `--output-format stream-json` | `--json` |
| `--verbose` | `--verbose` | *(nothing)* |
| `--sandbox=read-only`, `workspace-write`, `danger-full-access` | *(unsupported)* | `--sandbox <value>` |
| `-c`, `--config key=value` (repeatable; keys in the Codex mapping) | *(unsupported)* | `-c key=value` |

`{prompt}` is the positional prompt (see [Prompt](#prompt)); it may instead sit after the other mapped arguments, before raw passthrough (see [Argument mapping](#argument-mapping)).

**Permission modes** use Claude's names. `--dangerously-skip-permissions` is agrouter's alias for `--permission-mode=bypassPermissions`, as it is in Claude, and so is Codex's spelling `--dangerously-bypass-approvals-and-sandbox`, so both hit the one mapping key and never emit duplicate tokens (Codex rejects a repeated `--skip-git-repo-check`); given with a different `--permission-mode`, both are translated and the CLI applies its own precedence or fails with its own error (see [Pass-through, not validation](#pass-through-not-validation)). Codex's `--skip-git-repo-check` lives in these mappings, not in `print`: relaxing the git-repository guard is a permission decision, so a Codex call without `--permission-mode` fails outside a git repository, as `codex exec` itself does. The v1 defaults relax it for `bypassPermissions` and for read-only `plan`; a config that wants it with other modes adds it to those mappings. **≈ marks the closest Codex setting, not an equivalent:** Claude's `acceptEdits` auto-approves file edits while Codex `workspace-write` sandboxes all commands to the workspace; Claude's `auto` classifies every tool call while Codex `--approve-for-me` reviews only requests beyond its workspace sandbox. For `plan`, `approval_policy="never"` stops Codex from asking to leave the read-only sandbox, so, like Claude's `plan`, it cannot edit. `manual` (Claude asks before edits and shell) and `dontAsk` (deny anything not pre-allowed) have no Codex counterpart, so routing prefers Claude for them, and on Codex they are skipped.

**Codex-spelled arguments** exist so that ralphex's Codex executor can call agrouter unchanged (see [ralphex integration](#ralphex-integration)):

- **`--sandbox`** is value-keyed like `--output-format`. Claude has no sandbox mapping, so routing prefers Codex. Given together with a `--permission-mode` whose Codex mapping also sets the sandbox, both are emitted and Codex rejects the repeated `--sandbox` with its own error; agrouter does not pick one, because dropping either could weaken what the caller asked for. With `bypassPermissions` there is no clash; Codex accepts `--sandbox` beside `--dangerously-bypass-approvals-and-sandbox`, which is what ralphex sends in Docker.
- **`-c key=value`** is Codex's config override, and agrouter spells it the same way; agrouter has no `--continue`, so Claude's `-c` does not clash. The token is split at the first `=`. Two keys are **constraints**, not passthrough: `-c model=<id>` is `--model <id>`, and `-c model_reasoning_effort=<level>` is `--effort <level>`; a TOML-quoted value (`"gpt-6-sol"`) is unquoted first. Given together with `--model` or `--effort`, the flag is the constraint and the `-c` form is an ordinary key, forwarded unchanged through `config.model` or `config.model_reasoning_effort` for Codex to resolve. Every other key must have a `config.<key>` mapping, keyed by the part before `=` like a value-keyed flag, and is emitted once per occurrence, in the caller's order, with the whole `key=value`, byte-exact, as `{value}`. The v1 Codex mapping lists exactly the keys ralphex sends (`codex.go:137-147,195-205`): `stream_idle_timeout_ms`, `project_doc`, `project_doc_fallback_filenames`, `features.multi_agent` and `agents.reviewer.description`. Any other key is unmapped and skipped, so `-c approval_policy=...` or `-c sandbox_mode=...` never overrides a `--permission-mode`. Claude has no `config.*` mapping, so a `-c` makes routing prefer Codex.

Every Claude output format is mapped. Codex has no single-result JSON on stdout (`-o` writes the last message to a file), so its mapping has no `output-format.json` key and `--output-format json` prefers Claude, and on Codex it is skipped. Which values are accepted is decided by the mappings, not code (see [Eligibility](#eligibility)). The vocabulary grows by adding a flag to agrouter and a key to each CLI's mapping; see [Open questions](#open-questions).

**Prompt**: at most one positional argument, and/or stdin. See [Prompt](#prompt).

Parsing rules:

- `exec` selects exec mode only as the first token; anywhere else it is an ordinary word (the positional prompt, if it is the only positional). Everything else may come in any order, as in Claude: agrouter knows all of its flags, so the positional prompt is found exactly, wherever it is.
- Flags take values in split (`--model M`) or `=` (`--model=M`) form.
- **An unknown flag is an error** (exit `2`, one `agrouter:` line). This is the one kind of argument that cannot be skipped: agrouter cannot know whether it takes a value, so it could not tell where the positional prompt is.
- A second positional argument is an error.
- Parsing uses `jessevdk/go-flags`, following ralphex; `exec` is a go-flags command.
- `AGROUTER_CLI` exists because ralphex's `codex_command` cannot carry arguments. ralphex's Codex calls usually do not need it: their `--sandbox` and `-c` already leave only Codex eligible.

### Raw passthrough

Tokens after `--` are appended to the child's argv unchanged, after the mapped arguments. This is the escape hatch for CLI-specific flags outside the vocabulary, and it takes effect **only with `--cli` set**: a raw flag is written for one CLI and cannot be valid for whichever one Jev picks. Without `--cli`, the raw tokens are skipped with one warning. Raw tokens are never read for routing and never sent to Jev.

### Skipped arguments

Every agrouter argument is translated through the chosen CLI's mapping. **An argument the chosen CLI does not map, or maps to `[]`, is skipped, never an error:** the child runs without it, and agrouter prints one line to stderr:

```
agrouter: warning: skipped --output-format json: codex has no mapping for it
agrouter: warning: skipped --verbose: maps to nothing for codex
```

Decision mode prints the same lines on stderr and also lists the skipped arguments, as the caller spelled them, in a `skipped` array in its JSON. The **prompt is the only required input**: with no positional prompt and no stdin, agrouter exits `2`.

What else is skipped with a warning rather than failing:

- an unknown or disabled `--cli`; routing then spans every CLI;
- raw tokens after `--` without `--cli`.

Still errors (exit `2`): an unknown flag, a second positional argument, no prompt at all, a configuration error, and Jev failing to decide while more than one CLI is still possible (see [When Jev cannot decide](#when-jev-cannot-decide)).

### Pass-through, not validation

agrouter translates; **the real CLI validates.** agrouter never rejects a value because it thinks the CLI would:

- **A `--model` outside the catalog** is passed through as given, unresolved, through the chosen CLI's `model` template. If the model does not exist, Claude or Codex fails with its own error. The CLI is `--cli`, or the only CLI the mapped-argument preference leaves (ralphex's Codex argv); otherwise Jev chooses the CLI, with one option per remaining CLI and that model fixed. Jev picks no effort for a model outside the catalog: the caller's `--effort` is passed through if given, and otherwise the CLI's default applies.
- **An `--effort` a model does not list** (`--model claude-haiku-4-5 --effort high`), or that no catalog option has, is passed through the same way through the `effort` template. With `--model` absent, Jev chooses among the models with the effort left out of the option ids, and the caller's value is emitted.
- **Contradictory arguments are both translated,** in the caller's order: `--dangerously-skip-permissions --permission-mode plan` gives Claude both flags, and `--sandbox read-only --permission-mode acceptEdits` gives Codex two `--sandbox` flags, which it rejects. Arguments that hit the same mapping key (the bypass aliases, a repeated flag) are still emitted once.
- Aliases in the catalog are still resolved to the model's `name`; only values the catalog does not know pass through unchanged.

### Decision mode

`agrouter [options] [prompt]` routes and prints **one JSON line on stdout**, then exits `0`:

```json
{"cli":"claude","model":"claude-opus-5-5","effort":"high","argv":["claude","-p","--dangerously-skip-permissions","--output-format","stream-json","--model","claude-opus-5-5","--effort","high"]}
```

- `argv` is exactly what exec mode would run: the CLI's `command`, then its `print` mapping, the mapped arguments, model and effort, the `prompt` mapping if configured, and raw passthrough (see [Execution](#execution)).
- `argv` holds the positional prompt, if one was given, as the `{prompt}` token; the example above had none. It never holds stdin: the caller sends that on the child's stdin, as exec mode does. If agrouter read the caller's stdin, it is consumed; the caller must still have its own copy to send.
- `effort` is `null` for a model without efforts (Claude Haiku 4.5).
- `skipped` lists the arguments left out of `argv` (see [Skipped arguments](#skipped-arguments)); it is `[]` when nothing was.
- When Jev cannot decide and the CLI is known (`--cli`, implied by `--model`, or the only CLI left), the line holds only what the caller fixed: `--model` and `--effort` if given, `null` otherwise, and an `argv` without the unchosen arguments, so the CLI's own defaults apply. With the CLI unknown, exit `2` (see [When Jev cannot decide](#when-jev-cannot-decide)).

### Exec mode

`agrouter exec [options] [prompt]` routes the same way and runs the chosen argv, with the positional prompt in it and the caller's stdin passed through. Its stdout and stderr are the child's; its exit code is the child's. See [Execution](#execution).

Other runtime settings are environment variables only, which keeps agrouter's own command-line surface to `--cli` and `--jev-api-key`:

| Env | Meaning |
|---|---|
| `TYPESAFE_API_KEY` | Jev API key, used as-is when `--jev-api-key` is not given. Overrides `api_key` from config. |
| `AGROUTER_CONFIG_DIR` | Override the global config directory. |
| `AGROUTER_DEBUG=1` | Print the routing decision to stderr: number of options, choice, top probabilities, confidence, the Jev failure reason if any, the eligible CLIs and why others were filtered out, and the final command with the `{prompt}` token shown as `<prompt>`. Prompt text is never printed, and raw passthrough tokens are shown only as a count. The API key is redacted everywhere: in agrouter's own argv, in request headers, and in any HTTP error body that might echo it. |

### API key

The Jev key can come from three places. The first one that is set wins:

1. `--jev-api-key=KEY`
2. the `TYPESAFE_API_KEY` environment variable
3. `api_key` in `[agrouter]`, following the usual config layering (local > global > embedded)

The embedded defaults ship `api_key =` as an empty placeholder, which means no key.

- **An explicit `--jev-api-key=` (empty) clears any key** from the environment or config. Jev then cannot decide, which is useful for forcing a passthrough under `--cli`.
- **The key is sent only** in the `Authorization: Bearer` header to the TypeSafe endpoint. It is never logged and never reaches the child: agrouter strips its own flag and removes `TYPESAFE_API_KEY` from the child's environment.
- **Safety of each source:**
  - A command-line flag is visible to other local users in process listings (`ps`, Task Manager).
  - Under ralphex, the flag would be stored in plain text in `claude_args`.
  - In decision mode the key is never part of the printed `argv`.
  - Prefer the environment variable, or the global config with user-only file permissions.
  - A key in a local `.agrouter/config` is accepted but risky, because that file is easy to commit.

  agrouter does not print a runtime warning about any of this: stderr carries only [skipped-argument](#skipped-arguments) warnings. The cautions live in `--help` and in this document; `AGROUTER_DEBUG` notes the key's source, never its value.

## Configuration

INI, loaded with `gopkg.in/ini.v1` using `IgnoreInlineComment: true`, as ralphex does. That means **comments must be on their own lines**: an inline `; ...` would become part of the value. Layering also follows ralphex:

- **Embedded defaults** (`pkg/config/defaults/config`) ship the full catalog below.
- **Global:** `~/.config/agrouter/config`, overridable with `AGROUTER_CONFIG_DIR`.
- **Local:** `.agrouter/config` in the working directory.
- **Precedence:** env > local > global > embedded, merged **per section and per key**. A local file can change one model's description without redefining the catalog.
- **Removal:** a section can be removed from the catalog with `enabled = false`.

### Sections

```ini
[agrouter]
# TypeSafe API key for Jev. Empty = no key. Prefer the env var or the global config;
# never put it in a local .agrouter/config that may be committed.
api_key   =
# pin a versioned id (e.g. jev-1.13.0) to keep routing stable across Jev releases
jev_model = jev-latest
# total budget for all Jev calls, chunk requests and retries included; raise it for very long inputs
timeout   = 10s
# a state over the Jev budget is split into chunks, one Jev request each (see "Splitting large state").
# more chunks than this = cannot decide; also bounds how much prompt (positional + stdin) and mentioned-file text is read. >= 1
max_chunks      = 64
# chunk requests in flight at once. >= 1
chunk_parallel  = 4
# lower bound for a chunk's relevance weight; a tunable coefficient, not a probability. 0 < x <= 1
relevance_floor = 0.05
# the routing question for a state sent whole; tune the cost/quality preference here
question  = Which coding-agent CLI, model and reasoning effort should run the task described in `state` (its `prompt`, the `files` the prompt mentions, and `attachments`, where `attachments` lists images, PDFs and other non-text inputs the task includes, by type and size only)? Judge each option as a whole: the model and effort together must be strong enough to complete the task well, at the lowest cost and time that achieves that. Look up each option's `model` in `models` and its `cli` and `effort` in `efforts`.
# the routing question for one chunk of a split state: same options, same preference
chunk_question = Which coding-agent CLI, model and reasoning effort should run the task described by `anchor` and `chunk.text` together (`anchor.attachments` lists images, PDFs and other non-text inputs the task includes, by type and size only), where `chunk.text` is one part of a longer input? Judge each option as a whole: the model and effort together must be strong enough to complete the task well, at the lowest cost and time that achieves that. Look up each option's `model` in `models` and its `cli` and `effort` in `efforts`.
# the relevance question asked beside chunk_question
relevance = Does the text in `chunk.text` state the task to perform, its requirements, or what makes it hard, beyond what `anchor` already says?

[cli.claude]
command      = claude
description  = Claude Code (Anthropic). Agentic coding CLI with file editing, shell, subagents (Task tool) and MCP.

# how each agrouter argument is spelled for this CLI. A missing key = not supported:
# a call using it never routes to this CLI. [] = supported, adds nothing.
[cli.claude.args]
# required, always emitted first: non-interactive mode. {prompt} = the positional prompt, dropped when none;
# claude -p reads stdin, and appends it after a positional prompt
print                        = ["-p", "{prompt}"]
model                        = ["--model", "{model}"]
effort                       = ["--effort", "{effort}"]
# --dangerously-skip-permissions is agrouter's alias for this key
permission-mode.bypassPermissions = ["--dangerously-skip-permissions"]
permission-mode.plan         = ["--permission-mode", "plan"]
permission-mode.acceptEdits  = ["--permission-mode", "acceptEdits"]
permission-mode.auto         = ["--permission-mode", "auto"]
permission-mode.manual       = ["--permission-mode", "manual"]
permission-mode.dontAsk      = ["--permission-mode", "dontAsk"]
output-format.text           = ["--output-format", "text"]
output-format.json           = ["--output-format", "json"]
output-format.stream-json    = ["--output-format", "stream-json"]
verbose                      = ["--verbose"]

[cli.codex]
command      = codex
description  = Codex CLI (OpenAI). Agentic coding CLI with sandboxed shell, apply_patch and multi-agent spawn_agent.

[cli.codex.args]
# codex exec reads stdin when no positional prompt is given, and appends it as a <stdin> block when one is.
# to put the prompt at the end instead: print = ["exec"] and prompt = ["{prompt}"]
print                        = ["exec", "{prompt}"]
model                        = ["--model", "{model}"]
# -c values are TOML; the quotes are part of the token (no shell)
effort                       = ["-c", "model_reasoning_effort=\"{effort}\""]
# --skip-git-repo-check (run outside a git repository) is a permission decision, so it lives here.
# approximate mappings: closest Codex setting, not equivalent. manual and dontAsk are absent: no counterpart
permission-mode.bypassPermissions = ["--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check"]
permission-mode.plan         = ["--sandbox", "read-only", "-c", "approval_policy=\"never\"", "--skip-git-repo-check"]
permission-mode.acceptEdits  = ["--sandbox", "workspace-write"]
permission-mode.auto         = ["--approve-for-me"]
sandbox.read-only            = ["--sandbox", "read-only"]
sandbox.workspace-write      = ["--sandbox", "workspace-write"]
sandbox.danger-full-access   = ["--sandbox", "danger-full-access"]
# -c keys accepted, keyed by the part before "="; {value} = the whole key=value, byte-exact.
# exactly what ralphex sends
config.stream_idle_timeout_ms         = ["-c", "{value}"]
config.project_doc                    = ["-c", "{value}"]
config.project_doc_fallback_filenames = ["-c", "{value}"]
config.features.multi_agent           = ["-c", "{value}"]
config.agents.reviewer.description    = ["-c", "{value}"]
# only when --model / --effort is also given; otherwise these two -c keys are constraints
config.model                          = ["-c", "{value}"]
config.model_reasoning_effort         = ["-c", "{value}"]
# default output: final message on stdout. No single-result JSON: output-format.json is absent
output-format.text           = []
output-format.stream-json    = ["--json"]
verbose                      = []

[model.claude-opus-5-5]
cli         = claude
# value substituted into {model}
name        = claude-opus-5-5
# other values --model accepts for this model; unique across the catalog
aliases     = opus
efforts     = low, medium, high, xhigh, max
description = For long-running agentic coding and knowledge work: multi-hour autonomous coding, large-scale refactoring, complex systems engineering. Moderate latency; $4 / $20 per MTok.
source      = https://platform.claude.com/docs/en/models/opus-5-5/overview

[effort.claude.high]
description = Spends as many tokens as the task needs for excellent results. Complex reasoning, difficult coding problems, agentic tasks.
source      = https://platform.claude.com/docs/en/build-with-claude/effort
```

Rules:

- **Every `[cli.*.args]` value is a template, as a JSON string array,** because agrouter writes them: `{model}`, `{effort}` and `{value}` are substituted inside each token, and the result goes straight to `os/exec` with no shell. This avoids all quoting problems and works the same on Windows. A template may hold any number of tokens, including none.
- **Mapping keys** are agrouter flag names without the leading `--`. A flag with a value is keyed per value, `<flag>.<value>` (`output-format.stream-json`), so each value maps to whatever that CLI needs, even a different flag (`--json`). `model` and `effort` are the exception: one template each, with a placeholder. `-c` is keyed by its config key, `config.<key>`, and its templates take the whole `key=value` as `{value}`, emitted once per occurrence.
- **`print` is required and always emitted, first,** so a subcommand such as Codex `exec` precedes everything else. Its spelling is the config's choice: `["-p", "{prompt}"]`, `["--print", "{prompt}"]`, `["exec", "{prompt}"]`.
- **`{prompt}` goes in `print`, or in the optional `prompt` key,** which is emitted at the end of the mapped arguments. So `print = ["exec", "{prompt}"]` puts the prompt right after `exec`, and `print = ["exec"]` with `prompt = ["{prompt}"]` puts it after every other mapped argument. It is a whole token, in exactly one of the two: it becomes the positional prompt, or **zero tokens** when there is none (never an empty string, which Codex would take as an empty prompt). A CLI without `print`, `{prompt}` in both or in neither, anywhere else, or inside a longer token, is a config error.
- **agrouter knows only what the config says about each CLI:** its command and how each agrouter argument is spelled, `print` included. It never parses a CLI's own flags.
- **Efforts are per model.** A model with an empty `efforts` list produces one option with no effort flag (Claude Haiku 4.5 does not support effort). The `effort` template is then omitted.
- **Aliases.** `aliases` lists extra values `--model` accepts for a model (`opus`). Only the model's `name` is ever passed to the CLI. A `name` or alias used by two models is a config error.
- **A CLI section without `[cli.*.args]` `model`** is a config error, since agrouter could not pass its choice. `effort` may be missing only if none of that CLI's models has efforts.
- **Option ids.** An option's id is `<model section>@<effort>`, or `<model section>` for models without efforts. `@` is reserved: a model section name containing `@` is a config error. Section names are unique across CLIs, so the id also identifies the CLI.
- **No fallback.** The config has no default or fallback model. Jev decides whenever more than one option is eligible; when it cannot, see [When Jev cannot decide](#when-jev-cannot-decide).
- **Hard limits, checked at load time:**
  - at most **255** eligible options, the Jev limit. Options are never truncated silently; the catalog must be trimmed with `enabled = false`.
  - the serialized route and relevance questions must fit the budget on their own (see [Budget](#budget)).
- **Adding things needs no code.** A new CLI is a `[cli.*]` section plus its `[cli.*.args]` mapping, a new model is a `[model.*]` section, and a new effort description is an `[effort.<cli>.<level>]` section. Source URLs stay INI metadata and are not sent to Jev.

## Catalog shipped in v1

Descriptions below are condensed from the official pages linked in `source`. The embedded defaults carry the fuller text.

### Claude Code (`cli.claude`)

Source: [models overview](https://platform.claude.com/docs/en/about-claude/models/overview), [choosing a model](https://platform.claude.com/docs/en/about-claude/models/choosing-a-model), and the per-model pages `platform.claude.com/docs/en/models/<id>/overview`. The effort list matches `claude --help` in Claude Code 2.1.283 (`low, medium, high, xhigh, max`).

| Model id | Efforts (API default) | Official positioning | Price in/out per MTok | Latency |
|---|---|---|---|---|
| `claude-fable-5-1` | low, medium, high, xhigh, max (high) | Most capable model open to all customers. For demanding reasoning and long-horizon agentic work: multi-hour agent sessions, multistep deep research, analysis carried through to a finished document, spreadsheet or deck. Use when Opus 5.5 at higher effort still falls short. | $10 / $50 | slower |
| `claude-opus-5-5` (alias `opus`) | low, medium, high, xhigh, max (medium) | For long-running agentic coding and knowledge work: multi-hour autonomous coding, large-scale refactoring, complex systems engineering, vision-heavy workflows. Recommended starting point for most workloads. | $4 / $20 | moderate |
| `claude-sonnet-5` | low, medium, high, xhigh, max (high) | Best combination of speed and intelligence. Everyday coding, code generation, data analysis, agentic tool use. | $2 / $10 | fast |
| `claude-haiku-4-5` | none (effort not supported) | Fastest model with near-frontier intelligence. Real-time and high-volume processing, cost-sensitive work, subagent tasks. 200K context. | $1 / $5 | fastest |

Claude effort levels ([effort docs](https://platform.claude.com/docs/en/build-with-claude/effort)):

| Level | Official meaning |
|---|---|
| `low` | Most efficient: significant token savings with some capability reduction. Simpler tasks that need speed and low cost, such as subagents. |
| `medium` | Balanced, with moderate token savings. Agentic tasks balancing speed, cost and performance. |
| `high` | Spends as many tokens as the task needs. Complex reasoning, difficult coding, agentic tasks. |
| `xhigh` | Extended capability for long-horizon work: long-running agentic and coding tasks (over 30 minutes). |
| `max` | Absolute maximum capability with no limit on token spending. The deepest reasoning; can overthink simple or structured tasks. |

This gives 16 options (3 models × 5 efforts, plus Haiku).

### Codex (`cli.codex`)

Sources: `developers.openai.com/api/docs/models/<id>`, the [reasoning guide](https://developers.openai.com/api/docs/guides/reasoning), and [learn.chatgpt.com/docs/models](https://learn.chatgpt.com/docs/models). Effort lists come from what the Codex CLI itself advertises (`~/.codex/models_cache.json`, client 0.158.0), not from the API pages. The API also has `none`, which the CLI does not offer, and the CLI's `ultra` is not an API value.

| Model id | Efforts (CLI default) | Official positioning | Price in/out per MTok |
|---|---|---|---|
| `gpt-6-astra` | low, medium, high, xhigh, max, ultra (low) | Frontier intelligence for the most demanding end-to-end reasoning, coding, computer use, research and documents, where quality and judgment dominate. | $10 / $50 |
| `gpt-6-sol` | low, medium, high, xhigh, max, ultra (low) | Workhorse for complex coding and agent workflows; better factual reliability than 5.6 Sol. The capable everyday choice. | $2 / $10 |
| `gpt-6-luna` | low, medium, high, xhigh, max (medium) | Fast and affordable: focused, high-volume extraction, classification, transformations, structured summaries and focused coding. | $0.10 / $0.50 |

Codex effort levels:

| Level | Meaning |
|---|---|
| `low` | Modest reasoning for scoped tool use, planning and coding; favours latency and cost. |
| `medium` | Balanced depth and reliability for multi-step agentic coding, research and everyday work. |
| `high` | Harder debugging and planning; favours quality over latency. |
| `xhigh` | Long-running deep research, security or code review, where the gain justifies the extra cost and time. |
| `max` | The most demanding problems: more exploration and verification, depth over speed. |
| `ultra` | CLI-level orchestration that spawns parallel subagents automatically. Only for complex work that divides into meaningful independent parts. |

This gives 17 options. Only the latest generation (GPT-6) ships: older models (`gpt-5.6-*`, `gpt-5.5`) would only add options Jev must weigh against a newer tier that is stronger and no more expensive (GPT-6 Sol is $2 / $10 against 5.6 Sol at $4 / $20), and a user who wants one adds a `[model.*]` section in their own config. Hidden models (`gpt-reserve`, `codex-auto-review`) are not public choices and are excluded.

With `--cli` empty the catalog is **33 options**, well under Jev's 255. API prices are shown for comparison only; subscription plans bill differently.

Keeping the catalog current is a documentation task, not a code task. Each release re-checks the vendor pages and the Codex model cache.

## Routing

```
caller argv + stdin
      │
      ▼
 parse agrouter arguments ──unknown flag, second positional, or no prompt──► exit 2
      │
      ▼
 eligible options = enabled (model, effort) of CLIs that match --cli and
 satisfy --model / --effort (a constraint nothing satisfies is skipped),
 preferring CLIs that map every argument used (never emptying the list)
      ▼
 build Jev state (prompt text + mentioned files + attachments)
      │
      ├─ exactly 1 option ──► use it (no Jev call)
      │
      ├─ state fits the budget ──► one Jev Choice request ──────────┐
      └─ state too large ────────► split into chunks; one request   │
                                   per chunk (Choice + relevance    │
                                   Noul), pool the answers ─────────┤
      ┌─────────────────────────────────────────────────────────────┘
      ▼
 Jev decision ──cannot decide──► CLI known:   that CLI, only caller's --model/--effort
      │                          CLI unknown: exit 2
      ▼
 argv = command + print + mapped args + model + effort + prompt + raw passthrough
        (the positional prompt is in print or in prompt)
      │
      ├─ decision mode ──► print one JSON line
      └─ exec mode ──────► run it, stdin passed through
```

### Eligibility

Every filter runs **before** Jev, so Jev only scores options that fit the arguments given. **No filter ever empties the list:** a filter that would is skipped with a warning (see [Skipped arguments](#skipped-arguments)). They run in this order:

- **`--cli`** keeps one CLI. An unknown or disabled name is skipped.
- **`--model`** keeps exactly one model, found by `name` or `aliases` among enabled models. A model outside the catalog is [passed through](#pass-through-not-validation): the options become one per remaining CLI with that model fixed.
- **`--effort`** keeps only options with that effort. If none remains (`--model claude-haiku-4-5 --effort high`), the value is [passed through](#pass-through-not-validation) and the options keep their models without the effort.
- **Mapped arguments** are a **preference**, not a filter: only the remaining CLIs that would **skip the fewest** of the caller's mapped arguments stay, counting both a missing key and a `[]` mapping as skipped; Jev chooses among those. So `--output-format json`, `--permission-mode manual` or `--verbose` routes to Claude (Codex would skip them), `--sandbox` or a `-c` routes to Codex, and `--cli=codex --output-format json` runs Codex with the format skipped. A tie keeps every tied CLI.

With `--model` and `--effort` both given, at most one option is left and Jev is not called.

### Prompt

The prompt comes from the positional argument, from stdin, or both.

| Given | Child | Jev's `prompt` |
|---|---|---|
| positional only | the `{prompt}` token; empty stdin | the positional text |
| stdin only | no prompt token; stdin replayed byte-for-byte | stdin text |
| both | the `{prompt}` token, and stdin byte-for-byte; the CLI combines them | the positional text, two LF bytes (`0x0A 0x0A`), then stdin text; for binary stdin, the positional text only |
| neither | not started: exit `2`, the prompt is required | — |

- **The positional prompt goes in argv, stdin stays on stdin.** The positional text becomes the `{prompt}` token of the `print` or `prompt` mapping, one argv token, unchanged. Both CLIs read stdin when no positional prompt is given. Large material stays on stdin, which avoids the Windows command-line length limit, the same reason ralphex uses stdin (`codex.go:207-210`); a positional prompt already fit in agrouter's own command line. It never starts with `-`, because agrouter's parser would have taken it as a flag, so the CLI cannot mistake it for an option either. In decision mode the printed `argv` carries it, and the caller sends stdin.
- **Both given** mirrors `claude -p "summarize" < file`: the instruction first, the material after. The child gets the two separately and its CLI combines them its own way: Claude appends stdin to the prompt; Codex appends it as a `<stdin>` block (`codex exec --help`). What the model reads therefore contains both sources but is not byte-identical to Jev's `prompt`. agrouter adds nothing to stdin and normalises nothing (no CRLF conversion, no trailing newline). Binary stdin stays out of Jev's `prompt` and is reported as an `attachments` entry with `source: stdin`, so the two sources stay distinguishable.
- **Reading stdin.** When stdin is not a TTY, it is read to EOF. v1 has no live stream input (no `--input-format`), and v1 requires the caller to close stdin before it waits for output: a caller that keeps stdin open while waiting would hang, because routing needs the whole prompt before the child starts. ralphex writes the prompt and closes stdin.
- **Binary stdin** is not sent to Jev; it becomes an `attachments` entry (see [Non-text input](#non-text-input)) and Jev's `prompt` holds only the positional text. The whole captured stdin is validated, not just a prefix: NUL bytes or invalid UTF-8 anywhere make it binary. Unknown binary that happens to look like valid text cannot be told apart and is sent as text.
- **What Jev sees** is the `prompt`, the files it mentions (see below), and attachment metadata. No other argument is ever sent: not flags, not raw passthrough, not model or effort.
- **When Jev is called.** Whenever more than one option is eligible, including for a one-word prompt. With no prompt at all agrouter exits `2` before routing (see [Skipped arguments](#skipped-arguments)).
- **Capture limit.** Text capture, stdin and mentioned files together, is bounded by `max_chunks` (see [Splitting large state](#splitting-large-state)). Past it, Jev cannot decide and agrouter stops reading; in exec mode the child still gets all of stdin, the buffered bytes first, then the rest relayed live.

| State field | Content |
|---|---|
| `prompt` | The prompt text, as in the table above. |
| `files` | Contents of text files the prompt mentions, in order: `["<contents>", ...]`. Never paths. |
| `attachments` | Metadata of binary stdin and binary mentioned files: `{source, type, bytes}`, where `source` is `stdin` or `mentioned` and `type` is the detected media type or `unknown`. In a split state a long list may be grouped into `{source, type, count, bytes}` entries, where `bytes` is the group total. Never content, never paths or file names. |

#### Files mentioned in text

A prompt often names the files the task is about (`fix src/auth/login.go`, `see [the plan](docs/plans/x.md)`). agrouter reads those too, so Jev sees what the task works on, not only its wording.

- **Where it looks.** Only in the text `prompt`. Text of a mentioned file is **not** scanned again: one level, no recursion.
- **Finding candidates.** Quoted and backtick spans and Markdown link destinations (`[x](path)`) are taken first, so paths with spaces are found; the rest of the text is then split on whitespace. Candidates are then processed in order of their position in the original text, so a quoted mention and a plain one keep their textual order. Each candidate is tried as written first, then with surrounding brackets and trailing punctuation removed, then without a numeric `:line` or `:line:column` suffix (`src/a.go:42`). A Windows drive colon (`C:\x`) is never treated as a suffix.
- **Scope: the working directory only.** A candidate is resolved against the caller's working directory and canonicalised (symlinks and Windows junctions followed, case-insensitive on Windows). It is read only if the result is a regular file **inside** the canonical working directory, checked by directory boundary, not by string prefix: `../cwd2/x` and a junction pointing out of the tree do not count. So `~/.ssh/id_rsa` or `/etc/passwd` in a prompt are never read.
- **What happens to it.** Text goes into `files`; binary becomes an `attachments` entry with `source: mentioned` (see [Non-text input](#non-text-input)). The path is never sent; the prompt text that contains it is sent as written. Missing files, directories and unreadable files are ignored.
- **Order and duplicates.** Files are read in the order they are mentioned; each canonical path is read once.
- **URLs are not fetched.** `http://` and `https://` links stay text. Fetching them would add network calls, latency and a way to make agrouter request arbitrary addresses, for a routing hint.
- **Limits.** Mentioned files count against the same `max_chunks` text capture limit and the same `timeout` as everything else. Over the limit, Jev cannot decide, as for any other oversized input.
- **Trade-off.** Contents of any file the prompt names inside the working directory go to TypeSafe, including a `.env` the prompt happens to mention. agrouter reads it up front for routing, whether or not the CLI later chooses to read it, so Jev is one more service that sees it.

#### Non-text input

Jev accepts text only, so images, PDFs and other binary data are never sent to it. The child's stdin is always the original stdin bytes, unchanged; a positional prompt goes in argv (see [Prompt](#prompt)).

- **Detection.** agrouter reads a bounded prefix (the first 8 KiB) and gets the size from `stat` or the byte count:
  1. **Known signatures first:** the file's magic bytes, as recognised by Go's `http.DetectContentType`, name the type (`image/png`, `image/jpeg`, `application/pdf`, `application/zip`, ...).
  2. **Otherwise a heuristic:** NUL bytes, or invalid UTF-8 that is not just a character cut at the 8 KiB boundary, mean binary of type `unknown`.

  A PDF whose opening bytes look like ASCII is still caught by its `%PDF-` signature. The heuristic can misjudge unusual text encodings; that affects routing only.
- **Always mentioned.** Binary stdin and each binary mentioned file get their own `attachments` entry, even when the type is `unknown`. Whenever a Jev request is made, every one goes with it, either as its own entry or inside a counted group, and none is silently dropped for budget: a single request carries them in `state.attachments`, and a split state carries them in every chunk's anchor (see [Splitting large state](#splitting-large-state)).
- **What metadata can and cannot do.** It gives modality hints only ("this task includes an image or a PDF"). The descriptions can steer such tasks towards vision-capable or stronger options; Anthropic, for example, positions Opus 5.5 for vision-heavy work. A media type and a size do not determine the right model, and whether this helps is measured with the routing evaluation set, not assumed.
- **Binary-only tasks** are still routed, since the state is non-empty, but on metadata alone. The choice then depends mostly on the descriptions and the `question` text.
- **No conversion.** PDF-to-text, OCR or image captioning would add heavy dependencies and latency for a routing hint, so they are out of scope.

### Jev request

There is **one Choice question over the joint (cli, model, effort) options**, not separate model and effort questions. Separate questions cannot weigh "cheap model at high effort" against "strong model at low effort". They could also pick an effort the chosen model does not support, and clamping it afterwards would execute a decision Jev never scored. A joint choice gives one probability and one confidence for the exact thing that runs.

To keep the question small, the catalog goes **once** into a structured `instructions` object. Each option's criterion is a small object that names its entries, following TypeSafe's documented structured instructions and criteria. Only the CLIs, models and efforts that survive [eligibility](#eligibility) are included, so pinning also shrinks the request.

```json
{
  "model": "jev-latest",
  "state": {
    "prompt": "fix the flaky test in pkg/foo/foo_test.go\n\n<stdin text; a state over budget is split instead>",
    "files": ["<contents of pkg/foo/foo_test.go>"],
    "attachments": [{ "source": "mentioned", "type": "image/png", "bytes": 48213 }]
  },
  "questions": {
    "route": {
      "type": "choice",
      "instructions": {
        "question": "<[agrouter] question>",
        "clis":    { "claude": "<cli.claude description>", "codex": "<cli.codex description>" },
        "models":  { "claude-opus-5-5": "<description>", "gpt-6-sol": "<description>" },
        "efforts": {
          "claude": { "low": "<...>", "high": "<...>" },
          "codex":  { "low": "<...>", "ultra": "<...>" }
        }
      },
      "criteria": {
        "claude-opus-5-5@high": { "cli": "claude", "model": "claude-opus-5-5", "effort": "high" },
        "claude-haiku-4-5":     { "cli": "claude", "model": "claude-haiku-4-5", "effort": "none (not supported)" },
        "gpt-6-sol@medium":     { "cli": "codex",  "model": "gpt-6-sol",        "effort": "medium" }
      }
    }
  }
}
```

- **Payload contents:** only the state (the prompt text, the contents of text files it mentions inside the working directory, and attachment metadata: type and size only) and the catalog descriptions go to Jev. No argument, command, source URL or the API key ever does. The path of a file agrouter reads is never sent (see [Prompt](#prompt)).
- **Needs validation:** this compact encoding is schema-valid, but its classification accuracy is not yet proven equivalent to a full description inside each criterion. The routing evaluation set (see [Testing](#testing)) decides. If the references prove unreliable, switch to full descriptions per criterion; 33 options × ~120 tokens still fits.

### Budget

Jev allows 32k tokens for the state plus the longest question, and 64k for the state plus all questions.

- **The state budget is an estimate.** agrouter estimates tokens as UTF-8 bytes ÷ 3 and sets the state budget to 30k minus the estimate of the longest question. That ratio is a heuristic, not a guarantee. When chunking, the route question, the relevance question and the chunk envelope (`index`, `of`, field name) are all counted, and the state plus both questions must also stay under 64k.
- **Oversized questions are caught at load.** If `question`, or `chunk_question` plus `relevance` and a maximal anchor, leave less than 2k tokens for the state, agrouter refuses to start, rather than computing a negative budget.
- **A state that fits is sent whole** in one request, as described above. This is the common case, and it costs one round trip.
- **A state that does not fit is split**, never truncated. See below.

### Splitting large state

Jev has no way to take one state in parts (see [Jev](#jev-typesafe)), so a state over budget is **split into chunks, every chunk is sent in its own request, and the answers are pooled in agrouter**. Every byte of captured text reaches Jev, either in the anchor or in exactly one chunk; nothing is dropped. The anchor is repeated in every request on purpose.

**Anchor.** Each request carries a bounded `anchor`: the short sources that say what the task is, labelled by field, repeated in every chunk request.

- The anchor always holds `attachments` first, never cut and never moved to chunks, so every chunk request knows about non-text input. Their serialized JSON gets at most **1k** of the anchor's 4k tokens, by the same token estimate as the rest of the budget (roughly 40 entries; the budget, not the count, is the limit). A longer list is **summarised**, not dropped: entries with the same `source` and `type` are merged into one `{source, type, count, bytes}` entry, where `bytes` is the total. That keeps every kind of input and its total size, and loses only the per-item sizes. If even the summary exceeds 1k (dozens of distinct sources and types), Jev cannot decide. agrouter stops building the anchor once that is clear, and the child still gets the same argv, positional prompt included, and the original stdin unchanged. Then comes `prompt`, whole if it fits in the rest of the **4k tokens**.
- **A prompt that does not fit** contributes its labelled head and tail to the anchor, and its **full** text is split into chunks. A long positional instruction joined to a long stdin (ralphex) therefore keeps its opening instruction in every chunk request. So the anchor's copy is a summary, and the chunks are the lossless partition.
- **Filling the rest.** Room left in the 4k after `attachments` and `prompt` is filled, while it lasts, with labelled head and tail of `files`, so material the task only refers to comes last. That text is still split in full into the chunks.
- A field kept whole in the anchor is not repeated in the chunks. The head and tail copied into the anchor also appear in the chunks; that duplication is deliberate.

**Chunks.** The remaining text (the prompt if it overflowed, and mentioned files) is cut into chunks that fit the budget beside the anchor:

- Cuts fall on line boundaries, and inside an overlong line on UTF-8 character boundaries. There is no overlap.
- Each chunk keeps its origin: `{"field": "prompt", "index": 3, "of": 9, "text": "..."}`. Fields are chunked in state order, so the chunk sequence reads like the original.

**Per-chunk request.** One request per chunk, carrying two questions over the same state (TypeSafe's [fan-out](https://docs.typesafe.ai/patterns/fan-out) of several questions in one request):

```json
{
  "state": {
    "anchor": { "attachments": [{ "source": "mentioned", "type": "image/png", "bytes": 48213 }], "prompt": { "head": "run the next task in the plan ...", "tail": "... report when done" } },
    "chunk":  { "field": "prompt", "index": 3, "of": 9, "text": "<what fits after anchor, questions and envelope>" }
  },
  "questions": {
    "route":     { "type": "choice", "instructions": { "question": "<[agrouter] chunk_question>", "...": "catalog as in the single request" }, "criteria": { "...": "same" } },
    "relevance": {
      "type": "noul",
      "instructions": "<[agrouter] relevance>",
      "criteria": {
        "true":  "`chunk.text` adds to what the task is, what it requires, or what makes it hard",
        "false": "`chunk.text` is only material the task works on, or repeats `anchor`"
      }
    }
  }
}
```

- `route` is the joint Choice with the **same options and catalog** as the single request, so every chunk scores the same options. Only its question text differs: `chunk_question` names the paths this state actually has (`anchor`, `chunk`), because `question` names fields (`prompt`, `files`) that a chunk request does not carry.
- `relevance` asks whether **`chunk.text`** states the task, its requirements or its difficulty **beyond what `anchor` already says**. It judges the chunk, not the whole state; otherwise the repeated anchor would make every chunk look relevant.

**Pooling.** The per-chunk option vectors are combined by a weighted average in code. The rule is agrouter's own, inspired by TypeSafe's [composite scoring](https://docs.typesafe.ai/patterns/composite-scoring) (separate answers combined with weights the caller controls). TypeSafe does not document it for chunks of one state.

```
w_i  = max(noul_i, relevance_floor)
P[o] = Σ_i w_i · p_i[o] / Σ_i w_i
choice = argmax_o P[o]      ties broken by catalog order
```

- **Why relevance weights, not size weights.** Weighting by chunk size would let a 200 KB log dump outvote a two-line hard requirement, which is the distractor failure TypeSafe warns about. The Noul lets the chunk that states the task count more than the chunks that only carry material. It does not guarantee that chunk wins: at the default floor, 63 filler chunks at `0.05` weigh `3.15` against one relevant chunk at `1.0`, so enough filler voting the same way can still outvote it. This is a known limit, measured by the eval set, not fixed by a different algorithm.
- **`relevance_floor`** (default `0.05`) keeps every chunk in the pool. It is a tunable weighting coefficient, not a calibrated probability. When every chunk's relevance is at or below the floor, every weight equals the floor and the result is equal-weight pooling over chunks; it is not a uniform score over options.
- **`P` is an aggregate routing score**, not a calibrated probability over the whole prompt, and there is **no pooled confidence**. Per-chunk confidences are not averaged and are not used as weights. Under `AGROUTER_DEBUG=1`, agrouter prints each chunk's field, index, relevance and top options, then the pooled top options.
- **Other pooling rules were rejected.** Multiplying the vectors (product of experts) assumes the chunks are independent evidence, which they are not. Majority vote or max-per-option throws away the probabilities.

**Limits of splitting.** No chunk sees the whole text, so a relationship between two distant chunks (a requirement in chunk 1 that only matters because of code in chunk 7) cannot be judged by any single request. Seeing every chunk is not the same as understanding the whole. The routing evaluation set measures how much this costs.

**Bounds and failures.** Splitting never quietly routes on part of the input. Each case below either covers every chunk or makes Jev **unable to decide** (see [When Jev cannot decide](#when-jev-cannot-decide)), which already has a defined result: run the known CLI with only what the caller fixed, or exit `2`.

- **More chunks than `max_chunks`** (default `64`, about 1.9M tokens and $0.08) means Jev cannot decide. agrouter does not pick which chunks to skip. The same limit bounds capture of **text**, counted after binary detection: a large PNG on stdin or mentioned in the prompt still becomes `attachments` metadata, not an oversize failure. A mentioned file is sniffed first; only text files are read, up to the remaining capacity plus one byte to detect overflow. Once captured text passes the limit, Jev cannot decide and agrouter stops reading; partial text is never sent. In exec mode the child still gets all of stdin: the bytes already buffered are replayed first, then the rest of stdin is relayed live.
- **Any chunk request that still fails** after its retries means Jev cannot decide. Routing on the chunks that happened to succeed would drop the hard ones in a pattern nobody chose.
- **A `422` on one request** re-splits that chunk (or the whole state, in the single-request case) at half the budget, once. TypeSafe documents `422` as a general validation error; its body names the offending field, but there is no documented, stable way to tell an oversize request from other validation errors. So re-splitting is a guess that the token estimate was too low. A second `422`, or a `422` on a request whose chunk text is already under 2k tokens (splitting further cannot help; the cause is elsewhere), means Jev cannot decide. `max_chunks` is checked again after a re-split.
- **Parallelism and time.** At most `chunk_parallel` requests (default `4`, as in TypeSafe's cookbooks) run at once, all inside the one `timeout`, which covers capture, queueing, requests and retries. It does not scale with input size: 64 chunks at 4 in parallel is 16 rounds, which the 10s default fits only if Jev answers quickly. Users with very long inputs raise `timeout`. A timeout means Jev cannot decide.
- **Cost.** Price is per input token, so splitting costs what the text costs, plus the anchor and questions once per chunk: a 300k-token prompt is about 11 requests and under $0.02.

Splitting affects routing only. The child still receives the positional prompt in argv and the whole of stdin.

### Decision

- **State sent whole:** Jev's `choice` is always executed. There is no confidence threshold; confidence and probabilities are reported under `AGROUTER_DEBUG` as data for tuning descriptions.
- **State split:** the pooled `argmax` is executed. It is agrouter's decision over Jev's per-chunk answers, not a Jev `choice`, and it has no confidence (see [Splitting large state](#splitting-large-state)).

With exactly one eligible option, it is used without a Jev call.

### When Jev cannot decide

Jev cannot decide in these cases:

- no API key from any source (flag, env, config), or an explicit empty `--jev-api-key=`;
- a timeout or network error;
- an HTTP error on the request, or on any chunk request: `401`; `429` or `529` after backoff retries have used up `timeout`; `422` after the one re-split at half the budget;
- a state that needs more than `max_chunks` chunks (see [Splitting large state](#splitting-large-state));
- a split state whose `attachments` exceed their 1k anchor share even after summarising (see [Splitting large state](#splitting-large-state));
- a malformed response, to the single request or to any chunk request: invalid JSON; a missing `route` answer, or a missing `relevance` answer in a chunk request; a `choice` that is not among the options sent; `confidence` missing, NaN or outside [0, 1]; `probabilities` whose keys are not exactly the options sent, with a value that is not finite or is outside [0, 1], or whose sum is not within 0.01 of 1; a `noul` that is not finite or is outside [0, 1].

There is no fallback model, so what happens depends only on whether the CLI is already known:

| CLI | Result |
|---|---|
| known: `--cli` set, implied by `--model`, or the only CLI left after [eligibility](#eligibility) | Use that CLI with only what the caller fixed: the caller's `--model` and `--effort` if given, and no model or effort argument otherwise, so the CLI's own defaults apply. A fixed value is a hard constraint and is never dropped: `agrouter exec --cli=claude --effort high` still runs `--effort high` when Jev is unavailable. Eligibility has already checked that the CLI supports it. Every other mapped argument is still translated. Exec mode runs it; decision mode prints it, with `null` for what was not chosen. Routing never blocks the run. |
| unknown: more than one CLI still eligible | Exit `2` with one `agrouter:` line. This is not a skipped argument: there is nothing to run without a choice. Picking a CLI anyway, such as the first one in the config, would be a hidden fallback that depends on section order. |

With `--model` and `--effort` both given, Jev is never called (at most one option is left), so this case does not arise.

Other errors that stop agrouter before any child starts (exit `2`):

- argument errors: an unknown flag, a second positional argument;
- no prompt: neither a positional prompt nor stdin (binary-only stdin counts as a prompt, routed on its attachment entry);
- configuration errors: no enabled options, more than 255 options, a question over budget, `max_chunks` or `chunk_parallel` below 1, `relevance_floor` outside (0, 1], a malformed template, a CLI without a `model` mapping, a duplicate model name or alias;
- in exec mode, a child that cannot be started (exit `127`, see [Execution](#execution)).

## Model and effort

`--model` and `--effort` are **constraints** on the choice, applied in [eligibility](#eligibility): `--model` fixes the model and leaves the effort to Jev, `--effort` fixes the effort and leaves the model to Jev, both together skip Jev. A value the catalog does not know is passed through to the CLI unchanged, and the CLI reports an invalid one (see [Pass-through, not validation](#pass-through-not-validation)); agrouter never swaps it for a different model.

The chosen model and effort are then passed through the CLI's `model` and `effort` templates in `[cli.*.args]`, the same mapping as every other argument:

| CLI | Template | Result for `gpt-6-sol@high` / `claude-opus-5-5@high` |
|---|---|---|
| Claude | `["--model", "{model}"]`, `["--effort", "{effort}"]` | `--model claude-opus-5-5 --effort high` |
| Codex | `["--model", "{model}"]`, `["-c", "model_reasoning_effort=\"{effort}\""]` | `--model gpt-6-sol -c model_reasoning_effort="high"` |

- `{model}` is always the model's `name`, even when the caller passed an alias.
- A model with no efforts (Haiku) gets no effort argument.
- A CLI-spelled model flag in [raw passthrough](#raw-passthrough) is not recognised or replaced; like any raw token, it is the caller's responsibility.

## Argument mapping

The chosen CLI's argv is built from config only:

```
command  print...  mapped args...  model  effort  raw passthrough...
claude   -p       --dangerously-skip-permissions --output-format stream-json --verbose  --model claude-opus-5-5  --effort high
codex    exec     --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check --json  --model gpt-6-sol  -c model_reasoning_effort="high"
```

The prompt came on stdin here, so there is no `{prompt}` token; with `agrouter -p "fix it" ...` it would follow `-p` and `exec`, or come after `-c model_reasoning_effort=...` with `prompt = ["{prompt}"]`.

- **Order.** `print` first, so a subcommand such as Codex `exec` comes before everything else. Then the mapped arguments **in the order the caller gave them**, then model, then effort, then `prompt`, then raw passthrough. `prompt` comes before raw passthrough because a raw flag that takes a value would otherwise swallow the prompt. Claude and Codex both accept these flags in any order after `print`.
- **A mapping of `[]`** (Codex `verbose`) adds nothing: the argument is accepted and has no effect on that CLI.
- **A repeated flag** is mapped once.
- **Only the positional prompt is ever in argv,** as the `{prompt}` token; stdin never is (see [Prompt](#prompt)).

## Execution

Exec mode only; decision mode prints the argv and exits.

- **Command line.** The argv built above. When Jev could not decide and the CLI is known, it carries the caller's `--model` and `--effort` if given, and no model or effort argument otherwise.
- **Process.** The child runs via `os/exec` with no shell, inheriting the environment minus `TYPESAFE_API_KEY`. Stdin is the caller's stdin as in [Prompt](#prompt): the replayed buffer, then the rest of the original stdin (nothing more when it was read to EOF), or empty when there was none. The positional prompt is in argv, not stdin. Stdout and stderr are the parent's own file descriptors, unbuffered and untranslated.
- **Startup failure.** If the command is missing from `PATH`, not executable, or fails to start, agrouter prints one `agrouter:` line and exits with `127`. It does **not** retry another CLI: the choice was made, and silently running a different one would hide a broken installation.
- **Cancellation.** Platform-specific implementations, each tested on its own platform:
  - **Unix:** SIGINT and SIGTERM are forwarded to the child's process group. When agrouter is cancelled, the group is killed.
  - **Windows:** the child runs in a Job Object with kill-on-close, so closing or killing agrouter kills the whole tree. Console Ctrl events are delivered to the shared console group. Windows has no native SIGTERM equivalent; termination goes through the Job Object.
- **Exit code.** agrouter exits with the child's exit code. On Unix, a child killed by a signal exits as `128 + signal`. agrouter's own errors (arguments, no prompt, configuration, or no decision with the CLI unknown) exit with `2`, before any child starts.
- **Diagnostics.** agrouter prints nothing on success, including when routing failed with the CLI known, except one warning per [skipped argument](#skipped-arguments). Errors that stop agrouter get one `agrouter:` line each. Everything else goes to stderr only under `AGROUTER_DEBUG=1`.

## ralphex integration

Claude mode, in exec mode. The output stays Claude stream-json because the CLI is pinned:

```ini
# ~/.config/ralphex/config
claude_command = agrouter
claude_args    = exec --cli=claude --dangerously-skip-permissions --output-format stream-json --verbose
```

ralphex runs `agrouter exec --cli=claude ... [--model M] [--effort E] --print` with the prompt on stdin (`executor.go:375-378`). Every token is in agrouter's vocabulary, and `exec` comes first because ralphex puts `claude_args` before its own flags. A `--model` or `--effort` that ralphex adds from `task_model`, `review_model` or `plan_model` is a **constraint**: set, it fixes that value; leave those settings empty to let Jev choose.

**`idle_timeout`.** ralphex starts its idle timer before launching agrouter and resets it only on output (`executor.go:393-407`), while agrouter is silent during routing: up to `[jev] timeout` (10s by default), the single budget for capture, every chunk request and retries. A ralphex `idle_timeout` must therefore exceed that budget plus the child's startup and time to its first output line, or ralphex kills every run while it routes. It is off by default.

**Codex mode** (`executor = codex`, and the external Codex review in Claude mode), also in exec mode:

```ini
# ~/.config/ralphex/config
codex_command          = agrouter
# empty, so Jev chooses; the embedded defaults (gpt-5.6-sol, high) would otherwise fix both
# (gpt-5.6-sol is not in agrouter's catalog, so it would be passed through to codex as is)
codex_model            =
codex_reasoning_effort =
```

ralphex runs `agrouter exec [-c model="M"] [-c model_reasoning_effort=E] -c stream_idle_timeout_ms=N [-c project_doc="P"] [--dangerously-bypass-approvals-and-sandbox] --sandbox S` with the prompt on stdin (`codex.go:180-210`). Its first token `exec` is agrouter's exec mode, and every other token is in the vocabulary. `--sandbox` and `-c stream_idle_timeout_ms` map only for Codex, so routing prefers Codex without `--cli`, and Codex counts as known when Jev cannot decide. `-c model=` and `-c model_reasoning_effort=`, which ralphex emits when `codex_model`, `codex_reasoning_effort`, `task_model` or `review_model` set them, are constraints like `--model` and `--effort`. A model missing from the catalog is passed through to Codex unchanged. Codex's stdout (the final answer) and stderr (the header ralphex reads the session id from) pass through untouched, so ralphex's rollout tail works. The `idle_timeout` note above applies too.

**Choosing the CLI per call is not possible under ralphex as it stands.** ralphex picks its executor once at startup (`processor/executor_factory.go:19`), and its prompts (`processor/prompts.go:134,216,248`: the Task tool versus `spawn_agent`) and output handling (Claude stream-json on merged stdout versus Codex's stderr header, rollout tail and final stdout, `executor/codex.go:156-260`) all follow that choice. If agrouter ran Codex under ralphex's Claude executor, ralphex would receive output it cannot parse, from a CLI following prompts written for the other one. Translating the output would be lossy and would still leave the prompts wrong. So `--cli=claude` stays in `claude_args`, and a Codex run is a Codex run. Per-call CLI choice needs a ralphex change (see [Open questions](#open-questions)).

## Project layout

Follows ralphex conventions (`ralphex/CLAUDE.md` "Code Style").

```
cmd/agrouter/          # main: go-flags parsing (decision mode and the exec command), wiring, exit codes
pkg/config/            # INI loading, layering, validation; embedded defaults
pkg/config/defaults/   # embedded config with the v1 catalog and argument mappings
pkg/catalog/           # options (model × effort) derived from config; option ids; model name and alias lookup
pkg/args/              # argument mapping: templates, argv building, redaction
pkg/prompt/            # Jev state: positional + stdin join, stdin buffering, mentioned files, attachments, budget, anchor and chunking
pkg/jev/               # TypeSafe HTTP client: request/response types, validation, retry, timeout
pkg/router/            # eligibility, Jev question construction, per-chunk fan-out and pooling, answer validation, no-decision policy
pkg/runner/            # child process, stdin replay, signals / Job Object, exit code
```

- **Libraries:** Go 1.26, `jessevdk/go-flags`, `gopkg.in/ini.v1`, `stretchr/testify`; vendored dependencies.
- **Tooling:** `Makefile` targets `build` (binary in `.bin/`), `test` (race and coverage), `lint` (golangci-lint v2) and `fmt`.
- **Code style:**
  - Comments lowercase except godoc.
  - Errors wrapped with `%w` and context.
  - Consumer-side interfaces (`JevClient`, `CommandRunner`) with `moq`-generated mocks.
- **Jev client:** hand-written. TypeSafe ships Python and JavaScript SDKs only, and the API is one endpoint.

## Testing

- **Table-driven tests with testify; coverage target 80%+.**
- **`cmd/agrouter` parsing:**
  - decision mode by default, exec mode only with `exec` as the first token;
  - flags in any order around the positional prompt, split and `=` forms;
  - `-p`/`--print` accepted; the `print` mapping emitted whether or not it is given;
  - ralphex's Codex argv parsed unchanged: `exec`, `-c key=value` split at the first `=` (a value containing `=` kept whole), `-c model="M"` and `-c model_reasoning_effort=E` becoming constraints with TOML quotes removed, other `-c` kept in order, `--sandbox`, `--dangerously-bypass-approvals-and-sandbox` as the bypass alias; each of the `config.<key>` entries forwarded byte-exact, an unknown key skipped with a warning, repeated `-c` kept in the caller's order; the full ralphex `--codex` argv (`features.multi_agent`, `agents.reviewer.description`, bypass alias, `--sandbox danger-full-access`, `stream_idle_timeout_ms`, and `project_doc_fallback_filenames` with `--pass-claude-md`) and the external-review argv (`--sandbox read-only`) giving the expected Codex argv;
  - skipped with one warning each, never exit `2`: a `-c` key the chosen CLI does not map (`-c approval_policy=never` with `--cli=codex`), `--` without `--cli`, an unknown `--cli`;
  - passed through unvalidated: a `--model` outside the catalog (with `--cli`, and without it as one option per CLI), an `--effort` the model does not list, contradictions translated in order (`--sandbox` beside `--permission-mode acceptEdits` giving two `--sandbox`), `-c model=` beside `--model` forwarded as a `config.model` key;
  - an unknown flag, a second positional, and no prompt at all are exit `2`;
  - raw tokens after `--` with `--cli` reach argv unchanged and never the Jev state.
- **`pkg/config`:**
  - layering and per-key merges, including one key of a `[cli.*.args]` section overridden locally;
  - `enabled = false`;
  - malformed JSON templates;
  - `@` in a model section name;
  - a duplicate model name or alias;
  - a CLI without a `print` or `model` mapping, and one without `effort` whose models have efforts;
  - `{prompt}` in both `print` and `prompt`, in neither, elsewhere, or inside a longer token;
  - more than 255 options;
  - a question over budget, including `chunk_question` plus `relevance` with a maximal anchor;
  - `max_chunks` and `chunk_parallel` below 1, and `relevance_floor` outside (0, 1];
  - a guard test that no embedded value carries a trailing inline comment;
  - API key precedence (flag > env > local > global > embedded placeholder), and an explicit empty flag clearing the key.
- **CLI-agnostic:**
  - a made-up CLI defined only in config (helper process, new command, its own `print` mapping with `{prompt}` and other mappings, including a value-keyed one to a different flag and an `[]` mapping) is routed, gets every argument mapped, and receives the positional prompt as the `{prompt}` token and stdin unchanged; renaming its config section changes nothing;
  - a guard that production source outside the embedded defaults contains no CLI names (`claude`, `codex`, ...); fixtures, vendor URLs and descriptions are data.
- **`pkg/args`:**
  - argv order: command, `print`, mapped args in the caller's order, model, effort, `prompt`, raw passthrough; the prompt right after `exec` and at the end of the mapped arguments;
  - `--dangerously-skip-permissions` and `--permission-mode=bypassPermissions` giving the same argv, once each even when both are given; with another `--permission-mode`, both translated; `--permission-mode manual` or `dontAsk` preferring Claude, and skipped when Codex is pinned; Codex without `--permission-mode` getting no `--skip-git-repo-check`;
  - `{model}`/`{effort}` substitution inside a token (`model_reasoning_effort="{effort}"`), no shell quoting;
  - `[]` mappings adding nothing, a repeated flag mapped once, a model without efforts getting no effort argument;
  - the v1 golden argv for Claude and Codex, matching the mappings table, for every `--output-format` value (`json` has no Codex argv);
  - `{prompt}` replaced by the positional prompt as one token, including one with spaces, quotes and newlines, and dropped to zero tokens without one; stdin never appears in argv, and the API key never in decision-mode output.
- **`pkg/prompt`:**
  - the positional/stdin table: each of the four cases, the `\n\n` join in Jev's `prompt` only, byte-exact replay of stdin;
  - non-text detection: PNG, JPEG and PDF signatures (including an ASCII-looking PDF), NUL bytes, a UTF-8 character cut at the 8 KiB boundary (still text), binary stdin with a positional prompt (prompt only in state, attachment for stdin), and a binary-only task routed on metadata;
  - mentioned files: quoted, backtick and Markdown-link paths with spaces, `path:line` and `path:line:col` suffixes, a Windows drive path, trailing punctuation; `../` traversal, an absolute path outside, a sibling `cwd2` directory, and a symlink or junction escaping the tree all not read; a binary mention giving an attachment; dedupe of the same file; mixed quoted and plain mentions kept in text order; no recursion into a mentioned file's text; URLs not fetched; mentioned text counted in the shared capture limit and timeout;
  - the empty-state rule: a one-word prompt is routed; binary-only stdin is routed on its attachment entry; neither positional prompt nor stdin is exit `2`;
  - chunking: a state under budget is not split; a long stdin with a short positional instruction keeps the instruction in every anchor; `attachments` present in every chunk's anchor, even when the prompt alone overflows the 4k anchor; a long attachment list summarised by `source` and `type` with counts and total bytes; a summary still over 1k gives "cannot decide" with stdin replayed unchanged; the chunks plus the anchor-only fields reproduce every captured byte (lossless coverage; the only duplicates are the head-and-tail copies in the anchor); capture over the `max_chunks` limit gives "cannot decide" with stdin still replayed in full; cuts on line and multibyte UTF-8 boundaries; route question, relevance question and envelope counted against 32k and 64k.
- **`pkg/router`:**
  - eligibility: `--cli`; the mapped-argument preference (`--output-format json` preferring Claude; with `--cli=codex`, Codex chosen and the format skipped); `[]` counting as skipped in the preference (`--verbose` preferring Claude), fewest-skips ties kept; `--model` by name and by alias; a `--model` outside the catalog passed through; `--effort` alone; `--model` with `--effort` skipping Jev; `--effort` with a model without efforts passed through; no filter ever emptying the list;
  - skip warnings: exact stderr lines, one per skipped argument, and the decision-mode `skipped` array; none when everything maps (ralphex's Claude and Codex argv);
  - the golden JSON of the Jev request for a fixed catalog, with and without `--cli`;
  - every "cannot decide" case, via a mocked `JevClient`: CLI known by `--cli`, by `--model`, or as the only CLI left (ralphex's Codex argv without `--cli`) gives an argv with the caller's `--model`/`--effort` kept and nothing else chosen (including `--cli=claude --effort high` keeping `--effort high`); CLI unknown gives exit `2`;
  - the single-option short-circuit;
  - pooling with a mocked `JevClient`: a short hard requirement plus a few long filler chunks (the relevant chunk outweighs them), and a `max_chunks` case where floored filler outweighs it (documents the known limit), weights floored at `relevance_floor`, all-low relevance giving equal weights, stable tie order by catalog, per-chunk confidence never averaged;
  - chunk failure policy: `max_chunks` exceeded, and one chunk failing after retries while the others succeed, both give "cannot decide"; a `422` on one chunk re-splits it once, a second `422` or one on a chunk under 2k tokens gives "cannot decide", and `max_chunks` is rechecked after the re-split; invalid probability maps (missing or extra keys, NaN, sum off by more than 0.01) and an invalid `noul` are malformed;
  - the per-chunk request golden JSON: same route question and catalog in every chunk, anchor repeated, relevance question present;
  - the decision-mode JSON line, including `null` model and effort.
- **`pkg/jev`:** `httptest` server covering:
  - 200;
  - 401;
  - 422 then a smaller retry;
  - 429 then 200;
  - 529;
  - a slow response against the timeout;
  - malformed JSON;
  - NaN or out-of-range confidence;
  - an unknown choice;
  - the key appears only in the `Authorization` header and is redacted from debug output, including echoed error bodies.
- **`pkg/runner`:**
  - agrouter's own flags (`--cli`, `--jev-api-key`) never reach the child's arguments, and `-p` reaches it only as the `print` mapping;
  - the child's stdin: byte-exact stdin with nothing prepended, including binary stdin, and capture stopped at the text limit (buffered prefix replayed, then the rest relayed);
  - `TYPESAFE_API_KEY` absent from the child's environment;
  - exit code propagation;
  - exit `127` on a missing command;
  - cancellation, on Unix and Windows via build tags.

  The child is a helper process built from the test binary (`os.Args[0]` with `GO_WANT_HELPER_PROCESS`), so no real agent CLI is needed.
- **Routing evaluation set.** `testdata/routing/*.json` holds labelled prompts (real ralphex task, review and plan prompts; trivial edits; large refactors) with an acceptable-options list. A `make eval-routing` target, which needs `TYPESAFE_API_KEY` and does not run in CI, reports accuracy and the confidence distribution for both encodings. It is the basis for choosing the encoding and for tuning descriptions and the `question` text. It includes oversized prompts: a short hard requirement buried in long filler (up to `max_chunks` of it), and a requirement whose meaning depends on text in a distant chunk. Those cases tune `relevance`, `relevance_floor` and the anchor size, and measure what splitting loses against a prompt that fits. Split cases are scored on accuracy only, since a pooled decision has no confidence.
- **End-to-end:** a manual check with ralphex on a toy project in Claude mode, and of decision mode with both CLIs running the printed argv, with `AGROUTER_DEBUG=1` to inspect decisions.

## Open questions

1. **Low-confidence choices.** Jev's choice is always executed. If `make eval-routing` shows low-confidence choices are often wrong, a confidence rule could return later. It would need a policy for what to run instead, since there is no fallback model.
2. **Routing context.** Should agrouter pass anything besides the prompt (repo size, which ralphex phase) in `state`? ralphex does not name the phase today, so this would need an upstream hint (for example an `AGROUTER_HINT` environment variable).
3. **Two-pass splitting.** If the eval shows pooled chunks route worse than a prompt that fits, an alternative is to use the relevance Nouls first and then ask one Choice over the anchor plus the most relevant chunks. That drops chunks, so it only becomes the default if the eval shows the loss is worth it.
4. **Decision log.** Is `AGROUTER_DEBUG` enough, or do users want a persistent decision log (JSONL) for tuning descriptions against outcomes?
5. **Catalog freshness.** Automate a `make catalog-check` that diffs `~/.codex/models_cache.json` and the Claude Models API against the embedded defaults, or keep it a manual release step?
6. **Growing the vocabulary.** Candidates after v1, each needing a mapping for every CLI or an explicit "unsupported": `--system-prompt` / `--append-system-prompt` (Codex `-c developer_instructions`), images (Codex `-i`), `--input-format stream-json` (which would bring back live stdin), resume. Resume differs in meaning across CLIs, so it needs its semantics written down, not just a spelling, as the permission modes have.
7. **Knowing which CLI ran in exec mode.** Output is untranslated, so a caller that does not pin `--cli` cannot tell Claude stream-json from Codex JSONL without inspecting it. Options: an `AGROUTER_DECISION_FILE` the caller names, or recommending decision mode for such callers. stdout and stderr cannot carry it (see [Caller contract](#caller-contract-ralphex)).
8. **Per-call CLI choice in ralphex.** A new ralphex executor mode would call agrouter's decision mode on each phase's prompt, read `cli`, `model` and `effort` from the JSON, build that executor's prompt variant, and run its own Claude or Codex executor with that model and effort. agrouter needs nothing new for it. Open points: routing on one CLI's prompt variant before the other is built, and one Jev call per phase.
