# agrouter — design

agrouter is a drop-in proxy for coding-agent CLIs (Claude Code, Codex, and any other CLI described in config). It reads the prompt it was given, asks TypeSafe's **Jev** model to pick the best `(cli, model, effort)` combination from a configured catalog, and then runs that CLI with the caller's arguments forwarded.

```bash
# instead of
claude --dangerously-skip-permissions --output-format=stream-json --verbose --print < prompt.txt
# callers run
agrouter --cli=claude --dangerously-skip-permissions --output-format=stream-json --verbose --print < prompt.txt
```

Status: design, not implemented. Last verified against upstream docs and CLIs on 2026-09-27.

## Goals

- One binary that any tool (ralphex first) can call in place of `claude` or `codex`.
- Model and effort chosen per prompt by Jev, from a catalog with descriptions taken from the official vendor docs.
- A new CLI, model or effort is added by editing INI, with no code changes.
- Every argument is forwarded verbatim, with **one exception**: model and effort arguments written in the form of the CLI's configured template. Choosing those is agrouter's job, so the caller's values are replaced (see [Model and effort override](#model-and-effort-override)).

## Non-goals

- **No argument translation.** agrouter does not rewrite Claude flags into Codex flags or the other way around. Whether the forwarded arguments are valid for the selected CLI is the caller's responsibility.
- **No output translation.** The caller receives exactly what the selected CLI writes. A caller that parses one output format (ralphex parses Claude stream-json) must pin `--cli`.
- **No prompt rewriting.** The child process receives the prompt byte-for-byte.

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
- **Codex mode** (`--codex`) runs `<codex_command> exec ... -c model=... -c model_reasoning_effort=...` with the prompt on stdin.
  - `codex_command` is an executable only; there is no `codex_args`.
  - stdout and stderr are read through separate pipes (`codex.go:78-85`, `238-267`).
- The child environment is inherited, minus Claude session markers, so `AGROUTER_*` variables reach agrouter in both modes (`codex.go:46-68`).

As a result, agrouter prints nothing on stderr by default. In Claude mode it would corrupt error detection; in Codex mode it is merely noise.

## CLI

```
agrouter [--cli=NAME] [--jev-api-key=KEY] [--help] [--] <forwarded args...>
```

| Option | Env | Meaning |
|---|---|---|
| `--cli=NAME` | `AGROUTER_CLI` | Restrict routing to one CLI (`claude`, `codex`, ...). Jev then picks only that CLI's model and effort. Empty: Jev picks across every configured CLI. |
| `--jev-api-key=KEY` | `TYPESAFE_API_KEY` | TypeSafe API key for Jev. See [API key](#api-key). |
| `--help` | | agrouter help. To see a CLI's own help, call that CLI directly. |

Parsing rules:

- agrouter options must come **first**. Parsing stops at the first token that is not an agrouter option, or at `--`, and everything from there on is forwarded in order.
- agrouter's own flags are consumed only in that leading prefix and are **never forwarded** to the child.
- The flag names `--cli` and `--jev-api-key` were chosen because they do not collide with any flag of the supported CLIs. `--agent` was rejected because Claude Code has `--agent` and `--agents`; `--api-key` was avoided as too generic.
- Parsing uses `jessevdk/go-flags`, following ralphex.
- `AGROUTER_CLI` exists because ralphex's `codex_command` cannot carry arguments.

Other runtime settings are environment variables only, which keeps the command-line surface to these two flags:

| Env | Meaning |
|---|---|
| `TYPESAFE_API_KEY` | Jev API key, used as-is when `--jev-api-key` is not given. Overrides `api_key` from config. |
| `AGROUTER_CONFIG_DIR` | Override the global config directory. |
| `AGROUTER_DEBUG=1` | Print the routing decision to stderr: number of options, choice, top probabilities, confidence, the Jev failure reason if any, replaced model/effort values, and the final command. Prompt text and unrelated argument values are never printed. The API key is redacted everywhere: in agrouter's own argv, in request headers, and in any HTTP error body that might echo it. |

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
  - Prefer the environment variable, or the global config with user-only file permissions.
  - A key in a local `.agrouter/config` is accepted but risky, because that file is easy to commit.

  agrouter does not print a runtime warning about any of this, because stderr must stay quiet. The cautions live in `--help` and in this document; `AGROUTER_DEBUG` notes the key's source, never its value.

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
# more chunks than this = cannot decide; also bounds how much stdin and prompt-file text is read. >= 1
max_chunks      = 64
# chunk requests in flight at once. >= 1
chunk_parallel  = 4
# lower bound for a chunk's relevance weight; a tunable coefficient, not a probability. 0 < x <= 1
relevance_floor = 0.05
# the routing question for a state sent whole; tune the cost/quality preference here
question  = Which coding-agent CLI, model and reasoning effort should run the task described in `state` (its `prompt`, `stdin`, `system_prompts`, `files` and `attachments`)? Judge each option as a whole: the model and effort together must be strong enough to complete the task well, at the lowest cost and time that achieves that. Look up each option's `model` in `models` and its `cli` and `effort` in `efforts`.
# the routing question for one chunk of a split state: same options, same preference
chunk_question = Which coding-agent CLI, model and reasoning effort should run the task described by `anchor` and `chunk.text` together, where `chunk.text` is one part of a longer input? Judge each option as a whole: the model and effort together must be strong enough to complete the task well, at the lowest cost and time that achieves that. Look up each option's `model` in `models` and its `cli` and `effort` in `efforts`.
# the relevance question asked beside chunk_question
relevance = Does the text in `chunk.text` state the task to perform, its requirements, or what makes it hard, beyond what `anchor` already says?

[cli.claude]
command      = claude
description  = Claude Code (Anthropic). Agentic coding CLI with file editing, shell, subagents (Task tool) and MCP.
# WHITELIST of what Jev may see; nothing else from the arguments leaves the machine
# positional prompt: the token right after one of these (claude -p "prompt")
prompt_after     = ["-p", "--print"]
# arguments whose value is prompt text; each entry is a template with {value}
prompt_args      = [["--system-prompt", "{value}"], ["--append-system-prompt", "{value}"]]
# arguments whose value is a FILE holding prompt text; its contents go to Jev, never the path
prompt_file_args = [["--system-prompt-file", "{value}"], ["--append-system-prompt-file", "{value}"]]
# arguments that switch stdin to a live JSONL stream (same matching as templates)
stream_input_args = ["--input-format", "stream-json"]
# arguments naming binary attachments; only their type and size go to Jev
attachment_args  = []
# how model and effort are passed; used to replace the caller's values and to inject the choice
model_args   = ["--model", "{model}"]
effort_args  = ["--effort", "{effort}"]

[cli.codex]
command      = codex
description  = Codex CLI (OpenAI). Agentic coding CLI with sandboxed shell, apply_patch and multi-agent spawn_agent.
# codex exec "prompt"
prompt_after     = ["exec"]
# instruction-bearing config keys (learn.chatgpt.com/docs/config-file/config-reference)
prompt_args      = [["-c", "developer_instructions={value}"], ["--config", "developer_instructions={value}"], ["-c", "compact_prompt={value}"], ["--config", "compact_prompt={value}"]]
prompt_file_args = [["-c", "model_instructions_file={value}"], ["--config", "model_instructions_file={value}"], ["-c", "experimental_instructions_file={value}"], ["--config", "experimental_instructions_file={value}"], ["-c", "experimental_compact_prompt_file={value}"], ["--config", "experimental_compact_prompt_file={value}"]]
stream_input_args = []
# -i/--image are variadic; only the first value after the flag is seen
attachment_args  = [["-i", "{value}"], ["--image", "{value}"]]
# -c form, the same one ralphex injects, so ralphex's own values are replaced
model_args   = ["-c", "model={model}"]
effort_args  = ["-c", "model_reasoning_effort={effort}"]

[model.claude-opus-5-5]
cli         = claude
# value substituted into {model}
name        = claude-opus-5-5
efforts     = low, medium, high, xhigh, max
description = For long-running agentic coding and knowledge work: multi-hour autonomous coding, large-scale refactoring, complex systems engineering. Moderate latency; $4 / $20 per MTok.
source      = https://platform.claude.com/docs/en/models/opus-5-5/overview

[effort.claude.high]
description = Spends as many tokens as the task needs for excellent results. Complex reasoning, difficult coding problems, agentic tasks.
source      = https://platform.claude.com/docs/en/build-with-claude/effort
```

Rules:

- **Templates are JSON string arrays.** `{model}` and `{effort}` are substituted inside each token, and the result goes straight to `os/exec` with no shell. This avoids all quoting problems and works the same on Windows.
- **agrouter knows only what it has to about each CLI:**
  - how model and effort are passed, so it can replace them;
  - where prompt text can be found, which is the whitelist of what Jev may see.
  - which arguments name binary attachments, whose type and size are described to Jev.

  Every other argument is proxied as-is, without being understood.
- **Model and effort placement.** Where the caller already passed that argument, it is replaced in place, so it keeps a position the CLI accepts. Otherwise the tokens are prepended before the caller's arguments; Claude and Codex both accept these flags before any subcommand. See [Model and effort override](#model-and-effort-override).
- **Efforts are per model.** A model with an empty `efforts` list produces one option with no effort flag (Claude Haiku 4.5 does not support effort). `effort_args` is then omitted.
- **Option ids.** An option's id is `<model section>@<effort>`, or `<model section>` for models without efforts. `@` is reserved: a model section name containing `@` is a config error. Section names are unique across CLIs, so the id also identifies the CLI.
- **No fallback.** The config has no default or fallback model. Jev always decides; when it cannot, see [When Jev cannot decide](#when-jev-cannot-decide).
- **Hard limits, checked at load time:**
  - at most **255** eligible options, the Jev limit. Options are never truncated silently; the catalog must be trimmed with `enabled = false`.
  - the serialized route and relevance questions must fit the budget on their own (see [Budget](#budget)).
- **Adding things needs no code.** A new CLI is a `[cli.*]` section, a new model is a `[model.*]` section, and a new effort description is an `[effort.<cli>.<level>]` section. Source URLs stay INI metadata and are not sent to Jev.

## Catalog shipped in v1

Descriptions below are condensed from the official pages linked in `source`. The embedded defaults carry the fuller text.

### Claude Code (`cli.claude`)

Source: [models overview](https://platform.claude.com/docs/en/about-claude/models/overview), [choosing a model](https://platform.claude.com/docs/en/about-claude/models/choosing-a-model), and the per-model pages `platform.claude.com/docs/en/models/<id>/overview`. The effort list matches `claude --help` in Claude Code 2.1.283 (`low, medium, high, xhigh, max`).

| Model id | Efforts (API default) | Official positioning | Price in/out per MTok | Latency |
|---|---|---|---|---|
| `claude-fable-5-1` | low, medium, high, xhigh, max (high) | Most capable model open to all customers. For demanding reasoning and long-horizon agentic work: multi-hour agent sessions, multistep deep research, analysis carried through to a finished document, spreadsheet or deck. Use when Opus 5.5 at higher effort still falls short. | $10 / $50 | slower |
| `claude-opus-5-5` | low, medium, high, xhigh, max (medium) | For long-running agentic coding and knowledge work: multi-hour autonomous coding, large-scale refactoring, complex systems engineering, vision-heavy workflows. Recommended starting point for most workloads. | $4 / $20 | moderate |
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
| `gpt-5.6-sol` | low, medium, high, xhigh, max, ultra (low) | Previous flagship for complex professional reasoning and coding. | $4 / $20 |
| `gpt-5.6-terra` | low, medium, high, xhigh, max, ultra (medium) | Previous balanced tier for straightforward, everyday work. | $2 / $12 |
| `gpt-5.6-luna` | low, medium, high, xhigh, max (medium) | Previous efficient tier for cost-sensitive, high-volume narrow work. | $0.20 / $1.20 |
| `gpt-5.5` | low, medium, high, xhigh (medium) | Legacy flagship for coding and professional work. **Leaves ChatGPT Codex on 2026-10-14**; API access is unaffected. | $5 / $30 |

Codex effort levels:

| Level | Meaning |
|---|---|
| `low` | Modest reasoning for scoped tool use, planning and coding; favours latency and cost. |
| `medium` | Balanced depth and reliability for multi-step agentic coding, research and everyday work. |
| `high` | Harder debugging and planning; favours quality over latency. |
| `xhigh` | Long-running deep research, security or code review, where the gain justifies the extra cost and time. |
| `max` | The most demanding problems: more exploration and verification, depth over speed. |
| `ultra` | CLI-level orchestration that spawns parallel subagents automatically. Only for complex work that divides into meaningful independent parts. |

This gives 38 options. Hidden models (`gpt-reserve`, `codex-auto-review`) are not public choices and are excluded.

With `--cli` empty the catalog is **54 options**, well under Jev's 255. API prices are shown for comparison only; subscription plans bill differently.

Keeping the catalog current is a documentation task, not a code task. Each release re-checks the vendor pages and the Codex model cache.

## Routing

```
caller argv + stdin
      │
      ▼
 parse agrouter options (--cli) ──► forwarded args
      │
      ▼
 build Jev state (stdin text + args + prompt files)
      │
      ▼
 build options = every enabled (model, effort) of eligible CLIs
      │
      ├─ exactly 1 option ──► use it (no Jev call)
      │
      ├─ state fits the budget ──► one Jev Choice request ──────────┐
      └─ state too large ────────► split into chunks; one request   │
                                   per chunk (Choice + relevance    │
                                   Noul), pool the answers ─────────┤
      ┌─────────────────────────────────────────────────────────────┘
      ▼
 Jev decision ──cannot decide──► --cli set:   run that CLI, args untouched
      │                          --cli empty: exit 2
      ▼
 chosen option
      │
      ▼
 replace/inject model and effort ──► exec
```

### Prompt capture

Jev gets a **whitelist**: only text taken from known prompt sources. Model flags, settings JSON, MCP config, file paths passed as arguments, session ids and every other argument are never sent. This limits *which arguments* are read, not what the prompt says: a prompt, stdin or prompt file that itself contains a path or a secret still reaches Jev, as it reaches the CLI. `prompt_after` is purely positional (see the `resume` example below). The whitelist is declared per CLI, so a new CLI needs no code:

| State field | Source | Examples |
|---|---|---|
| `stdin` | Stdin, when it is not a TTY (see *Reading stdin* below), replayed to the child byte-for-byte. Binary stdin is not sent; it becomes an `attachments` entry (see [Non-text input](#non-text-input)). If every non-empty line parses as JSON, only permitted content is taken: string values under `text` keys at any depth (content blocks included) go to `stdin`; image and document blocks become `attachments` entries; everything else in those lines, including base64 `data`, is dropped. JSON lines are never sent raw. | ralphex (both modes), `claude --print < prompt.txt`, `cat p.md \| codex exec -`, Claude `--input-format stream-json` |
| `prompt` | The token right after the first token listed in `prompt_after`, provided it does not start with `-` and is not `-`. | `claude -p "fix it"`, `codex exec "fix it"` |
| `system_prompts` | The values matched by `prompt_args` entries, which are templates with `{value}`, matched in split or `=` form like `model_args`. Key templates match the exact key only (`developer_instructions=` never matches `developer_instructions_x=`). Codex `-c` values, in `prompt_args` and `prompt_file_args` alike, are decoded as TOML, as Codex does: a TOML string is captured (quoted paths with spaces included), a non-string value such as a number or boolean is skipped, and invalid TOML falls back to the raw text. | Claude `--system-prompt "…"`, `--append-system-prompt "…"`; Codex `-c developer_instructions="…"`, `-c compact_prompt="…"` |
| `files` | For each match of a `prompt_file_args` template, the file's text, resolved against the caller's working directory. There is no per-file cap: a long file is split like any other text, and only the total text capture limit applies (see [Splitting large state](#splitting-large-state)). Missing or unreadable files are skipped; binary files become `attachments` entries. Only contents are sent, never paths. | Claude `--system-prompt-file`, `--append-system-prompt-file`; Codex `-c model_instructions_file=…`, its deprecated alias `experimental_instructions_file`, and `experimental_compact_prompt_file` |
| `attachments` | Metadata only, for binary stdin, binary prompt files, image/document blocks in JSON stdin, and each match of an `attachment_args` template: `{source, type, bytes}`, where `source` is `stdin`, `stream-json`, or the flag, and `type` is the detected media type or `unknown`. Never content, never paths or file names. | Codex `-i shot.png`, `claude --print < diagram.png`, image blocks in Claude stream-json |

The shipped whitelist covers the documented prompt, prompt-file and print-mode sources listed in the table above, not every conceivable instruction-bearing setting. Sources: `claude --help` (2.1.283), `codex --help` / `codex exec --help` (0.157.1), and the Codex config reference.

- **Deliberately excluded:**
  - Codex `instructions`, which is reserved for future use;
  - experimental realtime prompt keys, marked do-not-use;
  - nested subagent and auto-review policy instruction settings;
  - Claude `--agents` JSON: it contains agent prompts, but mixed with other configuration;
  - Claude `--file`, which names downloadable resources, not prompts.

  Any of these can be added in config.
- **Never whitelisted as a whole:** `-c`/`--config`. Only the exact keys listed are read.

Rules:

- **Only Jev's copy is built this way.** The child always gets the original stdin bytes and arguments, changed only by the model/effort override.
- **With `--cli` empty,** the whitelist rules of every eligible CLI are applied, because the CLI that will run is not known before Jev answers.
- **What the positional rule deliberately does not do.** It does not model each CLI's flags (a closed decision), so some positional forms are not seen:
  - a prompt that comes after other options, as in `claude -p --verbose "x"` or `codex exec --json "x"`;
  - a bare `claude "x"` with no `-p`.

  For `codex exec resume <id>` it sends the word `resume`; the session id is never sent. Nothing outside the whitelist is sent to compensate for these misses. ralphex is unaffected, because it passes the prompt on stdin.
- **When Jev is called.** Whenever at least one whitelisted field is non-empty, including a one-word prompt. When all are empty, see [When Jev cannot decide](#when-jev-cannot-decide).
- **Reading stdin.** How stdin is read is decided by the arguments, never guessed from its content:
  - **Normal (finite) input:** read to EOF. This is the ralphex case. Each line that parses as JSON goes through the `text` extraction above, so a JSON image or document line never contributes base64, even when mixed with plain text. Plain lines are kept as text. The **whole** captured text is then validated, not just the 8 KiB prefix: NUL bytes or invalid UTF-8 anywhere turn it into an `attachments` entry instead. Unknown binary that happens to look like valid text cannot be told apart and is sent as text.
  - **Live stream:** the arguments match the CLI's `stream_input_args` (Claude `--input-format stream-json`, in split or `=` form, with the same matching as templates). The caller keeps stdin open while it waits for output, so reading to EOF would hang. agrouter reads line by line until the **first user message with text or an attachment**; control and metadata lines before it are held. It routes on that message, replays every held line to the child byte-for-byte, and then relays the rest of stdin live. Later messages in the session run on the model chosen for the first. With `--cli` empty, the stream arguments of every eligible CLI are checked.
- **Non-text input** is described to Jev, never sent. See [Non-text input](#non-text-input).

#### Non-text input

Jev accepts text only, so images, PDFs and other binary data are never sent to it. The child always receives them unchanged.

- **Detection.** agrouter reads a bounded prefix (the first 8 KiB) and gets the size from `stat` or the byte count:
  1. **Known signatures first:** the file's magic bytes, as recognised by Go's `http.DetectContentType`, name the type (`image/png`, `image/jpeg`, `application/pdf`, `application/zip`, ...).
  2. **Otherwise a heuristic:** NUL bytes, or invalid UTF-8 that is not just a character cut at the 8 KiB boundary, mean binary of type `unknown`.

  A PDF whose opening bytes look like ASCII is still caught by its `%PDF-` signature. The heuristic can misjudge unusual text encodings; that affects routing only.
- **What Jev gets instead:** an `attachments` entry `{source, type, bytes}` and nothing else. No content, file name, path or base64 data. `attachment_args` names the flags whose value is an attachment path (Codex `-i`/`--image`). agrouter opens those files only to detect the type and size; a missing file is reported as `type: unknown`, `bytes: 0`.
- **Known limit:** Codex `-i a.png b.png` is variadic, but template matching sees only the first value. Jev learns that there is at least one image, not how many.
- **What metadata can and cannot do.** It gives modality hints only ("this task includes an image or a PDF"). The descriptions can steer such tasks towards vision-capable or stronger options; Anthropic, for example, positions Opus 5.5 for vision-heavy work. A media type and a size do not determine the right model, and whether this helps is measured with the routing evaluation set, not assumed.
- **Binary-only tasks** are still routed, since the state is non-empty, but on metadata alone. The choice then depends mostly on the descriptions and the `question` text.
- **No conversion.** PDF-to-text, OCR or image captioning would add heavy dependencies and latency for a routing hint, so they are out of scope.

### Jev request

There is **one Choice question over the joint (cli, model, effort) options**, not separate model and effort questions. Separate questions cannot weigh "cheap model at high effort" against "strong model at low effort". They could also pick an effort the chosen model does not support, and clamping it afterwards would execute a decision Jev never scored. A joint choice gives one probability and one confidence for the exact thing that runs.

To keep the question small, the catalog goes **once** into a structured `instructions` object. Each option's criterion is a small object that names its entries, following TypeSafe's documented structured instructions and criteria. Only the CLIs, models and efforts that survive the `--cli` filter are included, so pinning also shrinks the request.

```json
{
  "model": "jev-latest",
  "state": {
    "stdin": "<stdin text; a state over budget is split instead>",
    "prompt": "fix the flaky test in pkg/foo",
    "system_prompts": ["<--append-system-prompt text>"],
    "files": ["<--append-system-prompt-file contents>"],
    "attachments": [{ "source": "-i", "type": "image/png", "bytes": 48213 }]
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

- **Payload contents:** only the whitelisted state (stdin text, the positional prompt, system-prompt text, prompt-file contents, and attachment metadata: type and size only) and the catalog descriptions go to Jev. No other argument, file path, command, source URL or the API key ever does.
- **Needs validation:** this compact encoding is schema-valid, but its classification accuracy is not yet proven equivalent to a full description inside each criterion. The routing evaluation set (see [Testing](#testing)) decides. If the references prove unreliable, switch to full descriptions per criterion; 54 options × ~120 tokens still fits.

### Budget

Jev allows 32k tokens for the state plus the longest question, and 64k for the state plus all questions.

- **The state budget is an estimate.** agrouter estimates tokens as UTF-8 bytes ÷ 3 and sets the state budget to 30k minus the estimate of the longest question. That ratio is a heuristic, not a guarantee. When chunking, the route question, the relevance question and the chunk envelope (`index`, `of`, field name) are all counted, and the state plus both questions must also stay under 64k.
- **Oversized questions are caught at load.** If `question`, or `chunk_question` plus `relevance` and a maximal anchor, leave less than 2k tokens for the state, agrouter refuses to start, rather than computing a negative budget.
- **A state that fits is sent whole** in one request, as described above. This is the common case, and it costs one round trip.
- **A state that does not fit is split**, never truncated. See below.

### Splitting large state

Jev has no way to take one state in parts (see [Jev](#jev-typesafe)), so a state over budget is **split into chunks, every chunk is sent in its own request, and the answers are pooled in agrouter**. Every byte of whitelisted text reaches Jev, either in the anchor or in exactly one chunk; nothing is dropped. The anchor is repeated in every request on purpose.

**Anchor.** Each request carries a bounded `anchor`: the short sources that say what the task is, labelled by field, repeated in every chunk request.

- The anchor holds `prompt`, `system_prompts` and `attachments`, in that order, while they fit in **4k tokens**.
- **When there is no separate prompt** (ralphex passes its whole task on stdin), the anchor would be nearly empty. The room left in the 4k is then filled with the head and tail of the stdin text, labelled as such, so every chunk request still says what the task is. Stdin is still split in full into the chunks.
- A field that does not fit in the anchor contributes its head and tail to the anchor, and its **full** text is split into chunks like stdin. So the anchor's copy is a summary, and the chunks are the lossless partition.
- A field kept whole in the anchor is not repeated in the chunks. For an overflowing field, the head and tail in the anchor also appear in its chunks; that duplication is deliberate.

**Chunks.** The remaining text (stdin, prompt files, and anchor overflow) is cut into chunks that fit the budget beside the anchor:

- Cuts fall on line boundaries, and inside an overlong line on UTF-8 character boundaries. There is no overlap.
- Each chunk keeps its origin: `{"field": "stdin", "index": 3, "of": 9, "text": "..."}`. Fields are chunked in state order, so the chunk sequence reads like the original.

**Per-chunk request.** One request per chunk, carrying two questions over the same state (TypeSafe's [fan-out](https://docs.typesafe.ai/patterns/fan-out) of several questions in one request):

```json
{
  "state": {
    "anchor": { "prompt": "run the next task in the plan", "attachments": [] },
    "chunk":  { "field": "stdin", "index": 3, "of": 9, "text": "<what fits after anchor, questions and envelope>" }
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

- `route` is the joint Choice with the **same options and catalog** as the single request, so every chunk scores the same options. Only its question text differs: `chunk_question` names the paths this state actually has (`anchor`, `chunk`), because `question` names fields (`stdin`, `files`) that a chunk request does not carry.
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

**Bounds and failures.** Splitting never quietly routes on part of the input. Each case below either covers every chunk or makes Jev **unable to decide** (see [When Jev cannot decide](#when-jev-cannot-decide)), which already has a defined result: run the pinned CLI untouched, or exit `2`.

- **More chunks than `max_chunks`** (default `64`, about 1.9M tokens and $0.08) means Jev cannot decide. agrouter does not pick which chunks to skip. The same limit bounds capture of **text**, counted after binary detection and JSON extraction: a large PNG prompt file or a stream-json line full of base64 still becomes `attachments` metadata, not an oversize failure. A prompt file is sniffed first; only text files are read, up to the remaining capacity plus one byte to detect overflow. Once captured text passes the limit, Jev cannot decide and agrouter stops reading; partial text is never sent. The child still gets all of stdin: the bytes already buffered are replayed first, then the rest of stdin is relayed live, as in the stream path.
- **Any chunk request that still fails** after its retries means Jev cannot decide. Routing on the chunks that happened to succeed would drop the hard ones in a pattern nobody chose.
- **A `422` on one request** re-splits that chunk (or the whole state, in the single-request case) at half the budget, once. TypeSafe documents `422` as a general validation error; its body names the offending field, but there is no documented, stable way to tell an oversize request from other validation errors. So re-splitting is a guess that the token estimate was too low. A second `422`, or a `422` on a request whose chunk text is already under 2k tokens (splitting further cannot help; the cause is elsewhere), means Jev cannot decide. `max_chunks` is checked again after a re-split.
- **Parallelism and time.** At most `chunk_parallel` requests (default `4`, as in TypeSafe's cookbooks) run at once, all inside the one `timeout`, which covers capture, queueing, requests and retries. It does not scale with input size: 64 chunks at 4 in parallel is 16 rounds, which the 10s default fits only if Jev answers quickly. Users with very long inputs raise `timeout`. A timeout means Jev cannot decide.
- **Cost.** Price is per input token, so splitting costs what the text costs, plus the anchor and questions once per chunk: a 300k-token prompt is about 11 requests and under $0.02.

Splitting affects routing only. The child still receives stdin, files and arguments in full, changed only by the model/effort override.

### Decision

- **State sent whole:** Jev's `choice` is always executed. There is no confidence threshold; confidence and probabilities are reported under `AGROUTER_DEBUG` as data for tuning descriptions.
- **State split:** the pooled `argmax` is executed. It is agrouter's decision over Jev's per-chunk answers, not a Jev `choice`, and it has no confidence (see [Splitting large state](#splitting-large-state)).

With exactly one eligible option, it is used without a Jev call.

### When Jev cannot decide

Jev cannot decide in these cases:

- the state is empty: no stdin, no prompt files, and no arguments left after the removals (see [Prompt capture](#prompt-capture));
- no API key from any source (flag, env, config), or an explicit empty `--jev-api-key=`;
- a timeout or network error;
- an HTTP error on the request, or on any chunk request: `401`; `429` or `529` after backoff retries have used up `timeout`; `422` after the one re-split at half the budget;
- a state that needs more than `max_chunks` chunks (see [Splitting large state](#splitting-large-state));
- a malformed response, to the single request or to any chunk request: invalid JSON; a missing `route` answer, or a missing `relevance` answer in a chunk request; a `choice` that is not among the options sent; `confidence` missing, NaN or outside [0, 1]; `probabilities` whose keys are not exactly the options sent, with a value that is not finite or is outside [0, 1], or whose sum is not within 0.01 of 1; a `noul` that is not finite or is outside [0, 1].

There is no fallback model, so what happens depends only on `--cli`:

| `--cli` | Result |
|---|---|
| set | Run that CLI with the caller's arguments **untouched**: nothing is replaced or injected, so the CLI's own defaults, or whatever the caller passed, apply. The run is never blocked by routing. |
| empty | Exit `2` with one `agrouter:` line. Picking a CLI anyway, such as the first one in the config, would be a hidden fallback that depends on section order. |

Other errors that stop agrouter before any child starts:

- configuration errors: an unknown `--cli`, no enabled options, more than 255 options, a question over budget, `max_chunks` or `chunk_parallel` below 1, or `relevance_floor` outside (0, 1];
- a child that cannot be started.

## Model and effort override

Choosing model and effort is agrouter's job, so a model or effort the caller passes **in the form of the CLI's template** is replaced. This is the single exception to verbatim forwarding. Other spellings of the same setting are not recognised and are forwarded as the caller's responsibility, for example Codex `-m X`, `--model=X` or `--config model=X` when the template is `-c model={model}`. agrouter recognises these arguments from the same `model_args` and `effort_args` templates it uses to inject them, so there is nothing extra to configure.

Each template is turned into a pattern by treating the placeholder as "any value":

| Template | Matches in the caller's arguments |
|---|---|
| `["--model", "{model}"]` | `--model X` (two tokens) and `--model=X` |
| `["-c", "model={model}"]` | `-c model=X` (two tokens) and `-c=model=X`; other `-c` keys such as `-c sandbox_mode=...` do not match |
| `["--model={model}"]` | `--model=X` |

Rules:

- **Only tokens before `--` are inspected.**
- **Replace or prepend.** The first match is replaced in place with the chosen value, so it keeps a position the CLI accepts (for example after `exec`). Any further matches are removed. With no match, the template tokens are prepended.
- **Whose templates apply:** those of the **CLI that will run**. When `--cli` is empty, that is the CLI Jev selected.
- **A model with no efforts** (Haiku) gets its effort matches removed and no effort injected.
- **Pure token matching.** agrouter does not know which other flags take values, so a token equal to `--model` is treated as the model flag even if it was meant as another flag's value (`--system-prompt --model`). Arguments shaped like that are the caller's responsibility, like every other argument.
- **Debug output.** `AGROUTER_DEBUG=1` reports which model and effort values were replaced; no other argument values are printed.

Rejected alternative: treating a caller-supplied model or effort as a **constraint** (a pinned model makes Jev choose only among that model's efforts). That changes routing semantics and needs its own policy. A caller who wants a fixed model calls the CLI directly.

## Execution

- **Command line.** `command` + the caller's arguments, with model and effort replaced or prepended as above. When Jev could not decide under `--cli`, the caller's arguments are forwarded unchanged.
- **Process.** The child runs via `os/exec` with no shell, inheriting the environment minus `TYPESAFE_API_KEY`. Stdin is the replayed buffer followed by the rest of the original stdin (the rest is empty when stdin was read to EOF), or the original stdin when nothing was read. Stdout and stderr are the parent's own file descriptors, unbuffered and untranslated.
- **Startup failure.** If the command is missing from `PATH`, not executable, or fails to start, agrouter prints one `agrouter:` line and exits with `127`. It does **not** retry another CLI: the caller's arguments were written for the one it asked for.
- **Cancellation.** Platform-specific implementations, each tested on its own platform:
  - **Unix:** SIGINT and SIGTERM are forwarded to the child's process group. When agrouter is cancelled, the group is killed.
  - **Windows:** the child runs in a Job Object with kill-on-close, so closing or killing agrouter kills the whole tree. Console Ctrl events are delivered to the shared console group. Windows has no native SIGTERM equivalent; termination goes through the Job Object.
- **Exit code.** agrouter exits with the child's exit code. On Unix, a child killed by a signal exits as `128 + signal`. agrouter's own errors (configuration, or no decision without `--cli`) exit with `2`, before any child starts.
- **Diagnostics.** agrouter prints nothing on success, including when routing failed under `--cli`. Errors that stop agrouter get one `agrouter:` line each. Everything else goes to stderr only under `AGROUTER_DEBUG=1`.

## ralphex integration

Claude mode (recommended for the first rollout, since the output stays Claude stream-json):

```ini
# ~/.config/ralphex/config
claude_command = agrouter
claude_args    = --cli=claude --dangerously-skip-permissions --output-format=stream-json --verbose
```

Codex mode:

```ini
executor      = codex
codex_command = agrouter
```

Run it with `AGROUTER_CLI=codex ralphex ...`, because `codex_command` cannot carry arguments.

In both modes agrouter replaces any `--model`/`--effort` (Claude) or `-c model=`/`-c model_reasoning_effort=` (Codex) that ralphex injects from `task_model`, `review_model`, `plan_model`, `codex_model` or `codex_reasoning_effort` (`executor_factory.go:127-147`, `codex.go:195-199`). Leaving those settings empty is still recommended, so that the ralphex banner does not report a model that is not the one used.

Leaving `--cli` unset under ralphex is possible but unsupported: ralphex's prompts and output parsing are executor-specific ("Use the Task tool" versus `spawn_agent`, Claude stream-json versus the Codex rollout file). By the non-goals above, making that combination work is the caller's responsibility.

## Project layout

Follows ralphex conventions (`ralphex/CLAUDE.md` "Code Style").

```
cmd/agrouter/          # main: go-flags parsing, wiring, exit codes
pkg/config/            # INI loading, layering, validation; embedded defaults
pkg/config/defaults/   # embedded config with the v1 catalog
pkg/catalog/           # options (model × effort) derived from config; option ids
pkg/args/              # token matching: model/effort replace/inject, redaction, Jev copy of argv
pkg/prompt/            # Jev state: stdin buffering and JSONL text extraction, prompt files, budget, anchor and chunking
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
- **`pkg/config`:**
  - layering and per-key merges;
  - `enabled = false`;
  - malformed JSON templates;
  - `@` in a model section name;
  - more than 255 options;
  - a question over budget, including `chunk_question` plus `relevance` with a maximal anchor;
  - `max_chunks` and `chunk_parallel` below 1, and `relevance_floor` outside (0, 1];
  - a guard test that no embedded value carries a trailing inline comment;
  - API key precedence (flag > env > local > global > embedded placeholder), and an explicit empty flag clearing the key.
- **`pkg/args`:**
  - whitelist extraction: `prompt_after` (option or `-` after it is not taken; `exec resume` gives `resume`), `prompt_args` in split and `=` forms, `prompt_file_args` paths never sent, and a guard test that no other argument value (settings JSON, ids, model flags) ever appears in the Jev state; the child's argv unaffected;
  - template-derived matching: split and `=` forms, key templates (`-c model=` keeps other `-c` keys), tokens after `--` untouched;
  - replace-in-place of the first match, removal of later matches, prepend when absent, and effort removal for a model without efforts.
- **`pkg/prompt`:**
  - stdin capture, and byte-exact replay;
  - JSONL extraction: nested content blocks, image/document blocks turned into attachments, base64 `data` never present in the state, mixed JSON and non-JSON input treated as text;
  - non-text detection: PNG, JPEG and PDF signatures (including an ASCII-looking PDF), NUL bytes, a UTF-8 character cut at the 8 KiB boundary (still text), `attachment_args` with a missing file, and a binary-only task routed on metadata;
  - prompt files: relative paths, missing and binary files, a large binary file giving metadata rather than an oversize failure, the total text capture limit across files;
  - the empty-state rule: a one-word prompt and a flag-only argv are both routed, and only an empty state is not;
  - chunking: a state under budget is not split; anchor fields kept whole up to 4k, an oversized anchor field gives head and tail to the anchor and its full text to the chunks; the chunks plus the anchor-only fields reproduce every whitelisted byte (lossless coverage; only the head and tail of an overflowing field are duplicated, in the anchor); capture over the `max_chunks` limit, including across several prompt files, gives "cannot decide" with stdin still replayed in full; cuts on line and multibyte UTF-8 boundaries; route question, relevance question and envelope counted against 32k and 64k.
- **`pkg/router`:**
  - the golden JSON of the Jev request for a fixed catalog, with and without `--cli`;
  - every "cannot decide" case, via a mocked `JevClient`: untouched passthrough with `--cli`, exit `2` without it;
  - the single-option short-circuit;
  - pooling with a mocked `JevClient`: a short hard requirement plus a few long filler chunks (the relevant chunk outweighs them), and a `max_chunks` case where floored filler outweighs it (documents the known limit), weights floored at `relevance_floor`, all-low relevance giving equal weights, stable tie order by catalog, per-chunk confidence never averaged;
  - chunk failure policy: `max_chunks` exceeded, and one chunk failing after retries while the others succeed, both give "cannot decide"; a `422` on one chunk re-splits it once, a second `422` or one on a chunk under 2k tokens gives "cannot decide", and `max_chunks` is rechecked after the re-split; invalid probability maps (missing or extra keys, NaN, sum off by more than 0.01) and an invalid `noul` are malformed;
  - the per-chunk request golden JSON: same route question and catalog in every chunk, anchor repeated, relevance question present.
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
  - agrouter's own flags (`--cli`, `--jev-api-key`) never reach the child's arguments;
  - byte-exact stdin replay, including binary stdin, capture stopped at the text limit (buffered prefix replayed, then the rest relayed), and the stream path: held lines replayed, then live pass-through while stdin stays open; stream mode chosen by `stream_input_args`, not by content; an image-only first message ends the hold;
  - `TYPESAFE_API_KEY` absent from the child's environment;
  - exit code propagation;
  - exit `127` on a missing command;
  - cancellation, on Unix and Windows via build tags.

  The child is a helper process built from the test binary (`os.Args[0]` with `GO_WANT_HELPER_PROCESS`), so no real agent CLI is needed.
- **Routing evaluation set.** `testdata/routing/*.json` holds labelled prompts (real ralphex task, review and plan prompts; trivial edits; large refactors) with an acceptable-options list. A `make eval-routing` target, which needs `TYPESAFE_API_KEY` and does not run in CI, reports accuracy and the confidence distribution for both encodings. It is the basis for choosing the encoding and for tuning descriptions and the `question` text. It includes oversized prompts: a short hard requirement buried in long filler (up to `max_chunks` of it), and a requirement whose meaning depends on text in a distant chunk. Those cases tune `relevance`, `relevance_floor` and the anchor size, and measure what splitting loses against a prompt that fits. Split cases are scored on accuracy only, since a pooled decision has no confidence.
- **End-to-end:** a manual check with ralphex on a toy project in both modes, with `AGROUTER_DEBUG=1` to inspect decisions.

## Open questions

1. **Low-confidence choices.** Jev's choice is always executed. If `make eval-routing` shows low-confidence choices are often wrong, a confidence rule could return later. It would need a policy for what to run instead, since there is no fallback model.
2. **Routing context.** Should agrouter pass anything besides the prompt (repo size, which ralphex phase) in `state`? ralphex does not name the phase today, so this would need an upstream hint (for example an `AGROUTER_HINT` environment variable).
3. **Two-pass splitting.** If the eval shows pooled chunks route worse than a prompt that fits, an alternative is to use the relevance Nouls first and then ask one Choice over the anchor plus the most relevant chunks. That drops chunks, so it only becomes the default if the eval shows the loss is worth it.
4. **Decision log.** Is `AGROUTER_DEBUG` enough, or do users want a persistent decision log (JSONL) for tuning descriptions against outcomes?
5. **Catalog freshness.** Automate a `make catalog-check` that diffs `~/.codex/models_cache.json` and the Claude Models API against the embedded defaults, or keep it a manual release step?
