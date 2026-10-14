# Exec native arguments and subscription readiness

## User revision implemented after external review (2026-10-14)

The original implementation below followed the initial contract. The user then requested simpler native passthrough. Ralphex stopped with `TASK_FAILED` because duplicate model flags are rejected by the child; emitting both selections cannot implement precedence. The revised contract supersedes the historical selection-conflict and context metadata requirements below.

- [x] Remove `selection_flags`, `config_flags`, `selection_config_keys`, `context_flags` and related `context_config_keys` fields, validation, embedded config and checks. Keep invocation templates; no replacement metadata catalog.
- [x] Remove native context detection and probe suppression. Readiness and default metadata use agrouter's configured context; native settings/profile/provider/config overrides apply only to the child.
- [x] Reserve agrouter's `--model`/`-m` and `--effort` spellings in native options and remove them, including attached forms. Model/effort are the narrow exception to unchanged forwarding. Native selections do not constrain Jev; on fallback they cannot replace the reported child defaults.
- [x] Add agrouter's `-m` alias. Emit native options, then generated model/effort, then the child's own `--` and positional tokens, then prompt_args. Preserve stdin and other native tokens. The requested native-only and caller-fixed model examples are covered by synthetic end-to-end tests.
- [x] Verify precedence without inference using the installed version's official source: [explicit model wins over config model](https://github.com/openai/codex/blob/rust-v0.162.1/codex-rs/core/src/config/mod.rs#L3747); [repeated config keys apply in argument order and the last value wins](https://github.com/openai/codex/blob/rust-v0.162.1/codex-rs/config/src/overrides.rs#L8). The [profile layer precedes CLI overrides](https://github.com/openai/codex/blob/rust-v0.162.1/codex-rs/config/src/loader/mod.rs#L275), so the generated effort also wins a profile default. The Opus partner challenged template-shape inference and we chose the smaller reserved-spelling filter with no config-key parsing.
- [x] Run all tests (97.2% overall coverage, every production package above 92%), build, host/Linux/macOS/Windows/eval lint and Opus high partner review. Final peer verdict: no actionable findings. No inference checks, commit, or Ralphex restart.

## Overview

Combine the three current backlog items into one change:

- Explicit exec CLI, native arguments, and separate verbosity (`docs/backlog/raw-cli-arguments-after-separator.md`, removed when this plan landed).
- CLI readiness before Jev (`docs/backlog/exec-cli-readiness-before-jev.md`, removed when this plan landed).
- Exec fallback targets and reasons (`docs/backlog/exec-fallback-target-and-reason.md`, removed when this plan landed).

The resulting invocation contract is:

```sh
agrouter [routing options] [prompt]
agrouter exec <cli> [routing options] [prompt] [-- native args...]
```

Decision mode routes across the configured enabled catalog, subject to caller constraints, and prints decision JSON without checking local CLI availability. Exec requires a positional CLI, checks that CLI's availability and subscription five-hour/weekly limits before any Jev request (including project complexity), routes within the remaining combinations, and starts the child with native arguments. If every exec candidate is blocked, it makes no Jev requests. Exec reports a quota block in Ralphex's existing limit-pattern style; Ralphex applies its own configured wait and retry policy.

Availability checks are exec-only. Decision mode performs no executable/auth/quota/capability probes and does not filter candidates using availability evidence. All provider-specific availability checks belong under that provider's `[cli.*]` configuration, so adding another CLI requires configuration rather than provider-specific Go changes.

Both modes include reasons in their output JSON when Jev fails or selection values are null. This user requirement expands the fallback backlog's exec-only diagnostics: decision JSON must also explain failure/nulls, without adding availability checks or changing the selection/fallback policy.

**Implementation scope: agrouter only.** All code, configuration, documentation, fixtures, and test changes stay in this repository. No task edits svtlv or Ralphex, migrates their configuration, or installs a wrapper there; the user does that later (see Post-completion). Required tests use agrouter-owned synthetic fixtures and do not require external checkouts.

## Decisions

- **Unknown availability defaults to `allow`** (user decision, 2026-10-13): exec runs combinations whose availability cannot be verified and prints one concise `agrouter: warning:` line. A CLI opts into `unknown = block` under `[cli.<name>.availability]`; a strict block exits 2 as "availability unverified", never as quota exhaustion. Unknown is never described as ready.
- **No readiness cache** (user decision, 2026-10-13): every exec invocation probes fresh, and whenever the CLI is available agrouter asks Jev every time. Nothing about availability is stored between invocations. This answers the readiness backlog's cache investigation.
- **No observation of real child output in this change.** The runner hands its own stdout/stderr files to the child; inserting pipes changes TTY detection, buffering, and `WaitDelay` truncation. Without a cache an observed failure has nowhere to persist, and the next invocation's fresh probe already sees a block the probe can report. Not planned; the only case it would help (the probe keeps passing or returning unknown while the child keeps failing) would need state saved between invocations.
- **Verified child defaults are in scope** (user decision, 2026-10-13): when exec delegates model or effort to the child, agrouter reports the child's actual default as `verified_child_default` when prompt-free evidence states it for the effective context, and `child_default_unknown` otherwise. A catalog default or an advertised recommendation is never presented as verified.
- **Three probe transports:** an SDK control-stream usage decoder, a JSON-RPC rate-limit decoder, and a `json-helper` transport whose stdout is agrouter's normalized evidence JSON. Each decoder is named after its wire protocol (for example `sdk-control-usage-v1`, `jsonrpc-ratelimits-v1`), never after a CLI. Config selects the transport and supplies command, argv, and timeout; there is no configurable field-path or unit-mapping language. Auth-only checks (`auth status`, `login status`) are not readiness and are not shipped as transports; the quota reads already surface auth failure.

The source backlogs' "investigate" items are answered here: the cache and child-failure recording by the decisions above, and child defaults by the implementation below. That is what lets the source items close.

## Contract changes

Every user-visible change, applied (with README and `CLAUDE.md` updates) in the task that introduces it:

| Change | Task |
| --- | --- |
| `exec <cli>` positional operand required; `--cli` cannot supply it; conflicting explicit `--cli` is exit 2 | 1 |
| `AGROUTER_CLI` removed (user decision, 2026-10-13); if set, one `agrouter: warning:` line says it is ignored and points to `exec <cli>`/`--cli` | 1 |
| Child-option flags (`--permission-mode`, `--dangerously-*`, `--output-format`, `--sandbox`, `-c`) removed; native options go after `--` | 1 |
| `--print TEXT` becomes a long alias of `-p TEXT`/`--prompt TEXT` (user decision, 2026-10-13), replacing the boolean compatibility `--print`; `--print "text"` keeps working, a bare `--print` with no value before `--` is a usage error | 1 |
| Decision mode rejects `--` (even with an empty tail) with exit 2 | 1 |
| Exec selection line always concise, regardless of agrouter `--verbose`; no automatic child `--verbose` | 1 |
| `[cli.*.args]` removed with a migration error; `[cli.*]` gains fixed invocation descriptor keys; CLI names may not contain `.` | 2 |
| Decision JSON loses `skipped`; unmapped-argument warnings disappear | 2 |
| Native model/effort selection flags rejected with exit 2 | 2 |
| Prompt delivered in argv after `--` while the native tail contains `--` is exit 2 | 2 |
| Decision and exec JSON gain `reason` and `null_reasons`; `cli` becomes nullable | 3 |
| Decision mode, Jev cannot decide with more than one CLI: still exit 2 and the one-line `agrouter:` stderr error, now **plus** one decision JSON object on stdout with null values and reasons | 3 |
| Exec exit 127 broadened: missing executable, authentication failure, or exhausted subscription quota found during preflight | 8 |
| Exec exit 2 for a strict-policy unknown availability block | 8 |
| Exec quota block prints one configured native-compatible line, which may start with a configured severity prefix before `agrouter:` (narrow exception to the `agrouter:` prefix rule) | 8 |
| Exec JSON gains `fallback` and `effective` (including `verified_child_default`) and a fallback prints one `agrouter: warning:` line naming the fallen-back fields and their defaults; a fallback whose verified delegated default is quota-blocked exits 127 without starting the child | 9 |

## Context

- `cmd/agrouter/flags.go:76` splits at the first `--`; exec takes its CLI from `--cli` or the `AGROUTER_CLI` environment variable (used today to pin a Ralphex phase) and passes raw arguments through only when `Pinned`. `flags.go:155-188` translates child flags into `args.Arg` entries and `-c` keys through `config.ConfigKeyPrefix`.
- `pkg/args/request.go`, `build.go`, and `redact.go` own requests, mapped argv construction, and redaction. Embedded `prompt = ["--", "{prompt}"]` puts the prompt after `--` in argv.
- `pkg/config/load.go:147,183-197` treats every `cli.*` section not ending in `.args` as a CLI and decodes `.args` with its parent. Any new `[cli.<name>.<sub>]` section would load as a CLI named `<name>.<sub>`.
- `pkg/router/eligibility.go` applies constraints and mapped-argument preferences (`prefer`). `(*Router).cannotDecide` (`router.go:116-120`) indexes `clis[0]` without a guard and would panic on empty eligibility.
- `router.Decision.Undecided` records why routing failed; completed stages and complexity survive failure. Jev sentinel errors (`ErrNoKey`, `ErrUnauthorized`, `ErrOverloaded`, ...) support typed classification, but answer-validation errors in `pkg/jev/client.go:404-467` are plain `errors.New`.
- `cmd/agrouter/app.go:131` passes `cmd.verbose` into `selectionOutput` for exec, exposing verbose details. `selectionJSON.CLI` (`app.go:68`) is a non-pointer `string`.
- `pkg/runner/runner.go:63` does `exec.LookPath` after routing; `runner.go:32` hands `*os.File` streams to the child unchanged. On Unix the child stays in agrouter's process group (`runner_unix.go:14-19`); Windows uses Job Objects.
- `cmd/agrouter/guard_test.go` rejects CLI names (case-insensitive, including comments and identifiers) and string literals equal to any configured effort level outside embedded config. `pkg/readiness` must not contain a CLI name in a comment or a literal such as `"max"`.
- Default `[agrouter] timeout` is 10s.

## Readiness investigation

### Evidence collected on 2026-10-12

| CLI | Prompt-free mechanism | What it establishes | Limits |
| --- | --- | --- | --- |
| Claude Code | `claude auth status --json` | Current authentication method and logged-in status | Does not establish subscription headroom |
| Claude Code | SDK stream control request `get_usage`, with `skip_behaviors: true` | Subscription five-hour and weekly utilization, reset times, and scoped/extra-usage information when returned | Experimental protocol; missing fields and metadata failures are inconclusive |
| Codex | `codex login status` | Credentials are present | Does not validate quota or remote credential validity |
| Codex | App-server `account/read`, `account/rateLimits/read`, and `model/list` | Account/provider state, quota-window snapshots, advertised model/effort capabilities | Provider-dependent; advertised capabilities do not guarantee execution |

Sources: [Claude CLI reference](https://code.claude.com/docs/en/cli-reference), [official Claude Agent SDK 0.3.296 package](https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.296.tgz), [Codex login reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli), and [Codex app-server reference](https://learn.chatgpt.com/docs/app-server).

Read-only local prototypes confirmed the mechanisms with Claude Code 2.1.296 and codex-cli 0.162.1:

- Claude accepted initialization followed by `get_usage` without any user message. Both quota windows were present; session cost was zero. Control JSON must go through stdin (for example a quoted heredoc), never as an argument after `--print`, which becomes an inference prompt. Successful metadata reads exit 0 even when weekly quota is exhausted, so evaluate the returned windows rather than process success.
- Codex accepted initialization followed by `account/rateLimits/read` without creating a thread or turn. Returned window durations were 300 and 10,080 minutes. Schema generation (`codex app-server generate-json-schema --out <dir>`) also exposes `ordinaryUsageAllowed`, `rateLimitReachedType`, and `spendControlReached`; an explicit denial must not be discarded because window percentages are below 100%, and null/missing permission is unknown.
- Claude's status-line quota fields appear only after an API response, so they cannot serve as preflight. [Status-line documentation](https://code.claude.com/docs/en/statusline).

Approaches considered: native control/status protocols (**chosen**: the child CLI handles its own credentials, agrouter reads structured metadata without a prompt); direct subscription HTTP endpoints (would need credential-store handling and an internal endpoint contract; not pursued); inference probes or auth-only preflight (rejected: neither satisfies the requirement).

## Development approach

- **Testing approach: Regular** (code first, then tests in the same task), table-driven testify tests, then checks before the next task.
- Synthetic catalogs, fake status processes, mocked Jev. Fake CLIs and probes reuse the `GO_WANT_HELPER_PROCESS` test-binary pattern so Windows CI exercises probe I/O and `.cmd` paths without real CLIs.
- Do not pin shipped names, model lists, counts, or configuration contents. Embedded availability descriptors get only the existing load-and-validate smoke test; descriptor behavior is tested with synthetic config.
- Keep production catalog names and per-CLI behavior in config. Use consumer-side interfaces and regenerate moq mocks with `make generate` when interfaces change.
- Run targeted tests after edits and `make test` before advancing. Maintain 80%+ coverage per package, excluding mocks.
- Every task leaves the repository buildable with all tests passing.

## Progress tracking

- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; document blockers with ⚠️ prefix.
- Update this plan when implementation deviates from scope.
- 2026-10-13: revised after plan review (task reorder, config section dispatch, cache/observation deferral, transport reduction, unknown policy decided).
- 2026-10-13: cache and child-output observation dropped entirely (no backlog items); verified child defaults restored to scope.

## Solution overview

### Invocation and native arguments

- Exec consumes its positional CLI immediately after `exec`. Missing, unknown, or disabled operands return exit 2 before probing or Jev. An explicit `--cli` equal to the operand is accepted as redundant, a conflicting one is exit 2.
- `AGROUTER_CLI` is removed: it only existed to pin a CLI for a Ralphex phase, which `exec <cli>` now does explicitly. It is no longer an environment config layer; when set, agrouter ignores it with one warning line so a stale export does not silently change meaning.
- Decision mode keeps the existing `--cli` constraint behavior, including the warning for an unusable constraint. Model/effort constraints and out-of-catalog values keep their behavior.
- The parser records whether a separator was supplied, independently of how many tokens follow. Decision mode rejects `--` with exit 2.
- **Prompt sources stay before `--` or on stdin.** agrouter reads its prompt from `-p`/`--prompt`/`--print` (one flag with three spellings), the positional prompt, `--prompt-file`, or stdin, all before the separator; nothing after `--` is ever read as a prompt. Ralphex (both executors) passes the prompt **through stdin** and appends only flags (`--print` for one, the native `exec` subcommand plus `-c`/`--sandbox` for the other) to argv. agrouter captures stdin for routing and replays the same bytes to the child's stdin, so the prompt is never placed in argv and `prompt_args` is not emitted. Appended native flags after `--` are forwarded unchanged, including a `--print` that duplicates `exec_args`. A caller who puts the prompt text after `--` gets it forwarded to the child but not routed on; with no other prompt source that is the existing "no prompt" exit 2.
- Native tokens are forwarded without rewriting, normalization, deduplication, catalog validation, or warnings, preserving empty tokens, spaces, repeated flags, and later `--` tokens.
- `--verbose` before the separator affects only agrouter; after it, only the child. Exec always logs the concise selection.

### Invocation descriptors (replacing mappings)

- Remove `[cli.*.args]`, `args.Arg`, `ConfigKeyPrefix`, `prefer`, `Skipped`, and the `skipped` output field. A `[cli.<name>.args]` section fails config loading with a migration error naming the replacement keys.
- `[cli.<name>]` gains fixed keys: `exec_args` (noninteractive invocation, for example `["-p"]` or `["exec"]`), `model_args`, `effort_args`, `prompt_args`. Only `{model}`, `{effort}`, `{prompt}` are valid placeholders, each only in its own key; there is no arbitrary mapping key or `{value}` expansion. `prompt_args` must place the prompt as its last element.
- `[cli.<name>]` also gains `selection_flags` (native spellings that select model/effort, for example `--model`, `-m`, `--effort`) and `selection_config_keys` (keys for a configured config flag such as `-c`, for example `model`, `model_reasoning_effort`), plus `config_flags` (the config flag spellings) and `context_flags` (spellings that change provider/profile/settings context).
- argv = command + `exec_args` + selected `model_args`/`effort_args` + native tokens + `prompt_args`. An unset model/effort omits its args rather than choosing a catalog default. Stdin replay and prompt-source ordering are unchanged.
- **Conflict scan:** before routing, scan the native tail up to the child's own `--` for a configured selection spelling (bare, `--flag=value`, short attached `-mvalue`) or a config flag whose key is a selection key (`-c key=...`, `-ckey=...`, TOML-quoted values). A hit is exit 2 naming the spelling but never the value. No option-arity table: a value that is literally a reserved spelling is a documented false positive. Native values never become routing constraints.
- **Duplicate separator:** when `prompt_args` puts the prompt after `--` and the native tail contains `--`, exit 2 with an actionable message (use stdin or drop the extra `--`).
- **Config section dispatch:** decode `cli.<name>` as a CLI, `cli.<name>.availability` and `cli.<name>.availability.<check>` as that CLI's availability, and `cli.<name>.args` as a migration error. CLI names containing `.` are invalid. Subsections of a disabled CLI are skipped; an orphan subsection without its `[cli.<name>]` is an error, as `.args` is today.

### Failure reasons in JSON

- Shared optional `reason` on decision JSON and exec selection JSON whenever routing fails: normalized `code`, failing `stage` when known, controlled `message`. Available without `--verbose`.
- Codes come from typed errors: `jev_no_key`, `jev_unauthorized`, `jev_timeout`, `jev_canceled`, `jev_api_error`, `jev_transport_error`, `jev_invalid_answer`, `jev_empty_answer`, `no_decision`. Answer validation in `pkg/jev` gets typed sentinel errors so classification never parses strings or raw bodies.
- `null_reasons` entries for every null selection field: `routing_failed`, `unresolved`, `delegated` (caller left it to the child), `no_applicable_effort`. A null/empty required answer from Jev is `jev_invalid_answer`/`jev_empty_answer`, not a successful omission. Successful fully populated selections omit both fields.
- `cli` becomes nullable. On decision-mode multi-CLI failure: exit 2, the existing one-line `agrouter:` stderr error, and one JSON object on stdout with null values and reasons; no argv, command, or fabricated confidence.

Illustrative decision JSON after a model-stage timeout with a fixed synthetic CLI:

```json
{
  "cli": "tool-a",
  "model": null,
  "effort": null,
  "reason": {"code": "jev_timeout", "stage": "model", "message": "Jev model selection timed out"},
  "null_reasons": {
    "model": {"code": "routing_failed", "message": "Model delegated to the child after routing failed"},
    "effort": {"code": "routing_failed", "message": "Effort delegated to the child after routing failed"}
  }
}
```

### Readiness boundary

- New `pkg/readiness` depends on `pkg/config` and generic probe support in `pkg/runner`; it does not import `pkg/router`. It returns evidence keyed by option id. `cmd/agrouter` applies it through a new `router.Eligibility.Exclude(ids)` method. Dependency line becomes `config` <- `catalog` <- `router`; `config`, `catalog`, `runner` <- `readiness` (`catalog` added in Task 7).
- `ReadinessChecker` is a consumer-side interface in `cmd/agrouter` with a moq mock. Clock is injectable.
- `[cli.<name>.availability]`: `enabled`, `checks`, `timeout` (default 5s), `unknown = allow | block`, `quota_error_prefix`, `quota_error_message`. `[cli.<name>.availability.<check>]`: `transport`, `command`, `args`, `min_version` (no transport-specific options were needed). Layered through embedded/global/local/environment like other config. Missing availability configuration yields unknown quota evidence, never implicit readiness. `enabled = false` is the escape hatch when a probe misbehaves.
- Configured probe commands share the existing configured child-command trust boundary and may come from any config layer, including project-local. Validation cannot prove an arbitrary helper read-only; that is part of the documented trust contract.
- Evidence dimensions: `executable`, `auth`, `quota`, `capability`, each `available | unavailable | unknown | not_applicable`, with reason code, scope (account, model), observation time, and reset time. Auth success never populates quota success.

### Probe execution

- Resolve the configured executable with the runner's semantics (Windows extensions, `.cmd` shims). Missing/nonexecutable is known unavailable.
- Check only the exec CLI, once per invocation, and filter only its combinations. Check all applicable windows even when every stage has one candidate and will skip Jev.
- Run the probe in the real working directory with the child's environment minus the Jev key, suppressing only hooks, MCP servers, and session persistence through configured flags. Do not use isolated temp directories or empty setting sources: project settings can switch the child to API-key or custom-provider auth, and a probe that ignores them could falsely block a runnable child. A false block is worse than unknown.
- If the native tail contains a configured `context_flags` spelling, quota evidence is unknown; native tokens are never parsed or propagated into probes.
- Probes get their own stdin containing only protocol messages, never prompt text, stdin replay, or documents. Response bytes are bounded independently of prompt capture.
- Probes start concurrently with input capture (prompt file, docs, stdin) and are bounded by the availability `timeout` and the remaining `[agrouter] timeout`. ⚠️ Measure cold-start latency during Tasks 5 and 6; if probes routinely consume a material share of the 10s budget, revisit before Task 8 (separate preflight bound or larger default) and record the choice in the README. ✅ Measured at about 0.5s to 2s cold; no separate bound (Task 8), recorded in the README.
- Unix: no shell; probes run with `Setpgid` and the group is killed on cancellation (the real child keeps its current process-group contract). Windows: probes run in their own Job Object, with existing batch quoting.
- Ship only read/status actions. Built-in transports reject known mutating actions (user messages, thread/turn start, login/logout). Never dump raw responses or error bodies.

**SDK control-stream decoder:** initialize a stream-json control connection, send `get_usage` with transcript scanning disabled, never send a `user` event. Do not use `--bare` (it skips OAuth/keychain authentication). Accept versions at or above `min_version` with response shape validation; an unrecognized shape is unknown. A non-subscription auth method is `not_applicable`. [Headless documentation](https://code.claude.com/docs/en/headless).

**JSON-RPC decoder:** one stdio app-server connection; read account state and rate limits; optionally the model list with pagination and hidden entries. Never create a thread or start a turn. Let the CLI refresh credentials where its read action does so. Request failure or omission is unknown unless an explicit authoritative denial establishes a block.

**json-helper transport:** run the configured helper with no stdin payload beyond an optional context JSON; its stdout must be the normalized evidence JSON (documented schema, versioned). This is how a third CLI is added through configuration only.

### Quota evaluation

- For subscription-backed execution inspect **both** the five-hour and weekly windows, using server-provided durations rather than assuming position. A valid applicable window at or above 100% (percentage units for control usage) makes subscription headroom unavailable.
- A five-hour or all-model weekly block affects the account; a model-scoped weekly block affects only models the evidence associates with that meter. A delegated (unset) model is unknown when any model-scoped block exists.
- With several exhausted windows, earliest recovery is the **latest** reset. Resetting the shorter window never clears an exhausted weekly window.
- Explicit backend denial (usage not allowed, limit reached, spend control reached) is unavailable regardless of percentages. Any enabled overage/credit path is unknown (no usable-credit computation). Null/missing permission is unknown. Never enable paid fallback.
- A metadata 429 means the probe was throttled: quota is unknown with reason `probe_throttled`, never a quota block.
- Unsupported versions, parse failures, timeouts, and incomplete metadata are unknown, never zero usage. Known blocks take precedence over unrelated unknowns. API-key/custom-provider contexts are `not_applicable`.
- Keep caller-fixed CLI/model/effort; never broaden a constraint to evade a block and never switch exec to another CLI. Capability evidence removes only explicitly unsupported combinations; a missing entry in an incomplete list is not a denial.

### Exec preflight outcomes

- Some combinations remain: route over them. Unknown-but-allowed combinations print one `agrouter: warning: <cli> availability unknown: <reason>` line.
- Known unavailable (missing executable, auth failure, quota exhausted): no Jev request, no selection line, no stdout bytes, exit 127.
  - Quota: exactly one line `<quota_error_prefix><quota_error_message>; <window> subscription quota exhausted[; resets at <RFC 3339>]`. The reset is the latest applicable verified reset and is omitted when unverified.
  - Auth/executable: `agrouter: <cli> unavailable: <reason>`. Never a quota phrase.
- Strict unknown block: `agrouter: <cli> availability unverified: <reason>`, exit 2.
- Prefix/message validation: preserve configured whitespace; reject newline and control characters. Provider wording lives only in config.
- On a Jev failure in exec, apply cannot-decide to readiness-filtered eligibility (exec always has one CLI, so it always falls back). Decision-mode fallback uses only configured/constraint-filtered candidates.

### Exec fallback metadata

- **A fallback must be unmistakable.** When Jev routing fails in exec, the output must say that something went wrong, which values fell back to the child's defaults, what those defaults are when known, and that they are not a Jev selection. Values Jev did select before the failure (for example the model when only the effort stage failed) and caller-fixed values keep their own source and are not reported as fallbacks.
- **Fallback warning line:** exactly one `agrouter: warning:` line on stderr before the selection line, readable without parsing JSON or `--verbose`. It names the failing stage and reason code, lists only the fallen-back fields, and gives each one's verified default or says it is unknown with a short reason:

  ```text
  agrouter: warning: Jev routing failed at the model stage (jev_timeout); model and effort fall back to tool-a defaults, not a Jev selection: model m-2 (verified child default), effort unknown (child did not report it)
  ```

- Exec-only `fallback` when `Undecided` is set: `action` (`child_defaults`), `fields` (the fallen-back fields), and a controlled `message` matching the warning, alongside the shared `reason`.
- Exec-only `effective` for model and effort: `value`, `source` (`router`, `caller`, `verified_child_default`, `child_default_unknown`), optional reason. Top-level nulls stay null when agrouter emitted no selection flag, even when `effective` shows a verified default.
- **Default evidence:** the same preflight probe connection reads the child's effective default model/effort when its protocol reports the value the child would use in this working directory with its settings, profile, and provider. Decoders and `json-helper` report `defaults` evidence; anything weaker (catalog default, a model list's recommended/default flag without effective-config backing) is unknown. A configured `context_flags` spelling in the native tail makes defaults unknown, as for quota. Decision mode never reads defaults.
- **Defaults and blocks:** a verified default model is checked against model-scoped quota blocks. If the delegated default is known blocked, the fallback is blocked: no child starts, the quota line is printed, and exec exits 127 (the same as a preflight block). An unknown delegated model stays unknown under the unknown policy.
- A compact readiness summary (excluded combination count by reason, unknown warning code) when exclusions affected the selection.
- Illustrative exec stderr for the warning above (the selection line remains the last agrouter line before the child starts):

  ```text
  agrouter: selection: {"cli":"tool-a","model":null,"effort":null,"reason":{"code":"jev_timeout","stage":"model","message":"Jev model selection timed out"},"null_reasons":{"model":{"code":"routing_failed","message":"Model delegated to the child after routing failed"},"effort":{"code":"routing_failed","message":"Effort delegated to the child after routing failed"}},"fallback":{"action":"child_defaults","fields":["model","effort"],"message":"Model and effort fall back to the child's defaults, not a Jev selection"},"effective":{"model":{"value":"m-2","source":"verified_child_default"},"effort":{"value":null,"source":"child_default_unknown","reason":{"code":"default_not_reported"}}}}
  ```

  Top-level `model`/`effort` stay null because agrouter passed no selection flag; `effective` and the warning carry the value the child will use.
- The selection line stays one prefixed line with `cli`, `model`, `effort`, completed project complexity, stage confidence summaries, and the new fields; no options, stage records, probabilities, chunks, prompt, docs, argv, or command.

## Technical details

Evidence record (in memory only):

```go
type Status string // available | unavailable | unknown | not_applicable

type Evidence struct {
    Dimension  string    // executable | auth | quota | capability
    Status     Status
    Reason     string    // normalized code, e.g. quota_exhausted, probe_throttled
    Scope      Scope     // account or model ids
    Window     string    // five_hour | weekly, when quota
    ObservedAt time.Time
    ResetAt    time.Time // zero when unverified
}
```

Normalized helper output (`json-helper`, schema `agrouter.readiness/v1`):

```json
{
  "schema": "agrouter.readiness/v1",
  "auth": {"status": "available"},
  "quota": [
    {"window": "five_hour", "status": "available"},
    {"window": "weekly", "status": "unavailable", "reason": "quota_exhausted", "models": [], "reset_at": "2026-10-13T15:00:00Z"}
  ],
  "capability": {"unsupported_models": []},
  "defaults": {"model": "m-2", "effort": null}
}
```

Synthetic config shape (tests use names like `tool-a`):

```ini
[cli.tool-a]
command               = tool-a
exec_args             = ["--print"]
model_args            = ["--model", "{model}"]
effort_args           = ["--effort", "{effort}"]
prompt_args           = ["--", "{prompt}"]
selection_flags       = --model, -m, --effort
config_flags          = -c, --config
selection_config_keys = model, effort
context_flags         = --settings, --profile

[cli.tool-a.availability]
checks              = usage
timeout             = 5s
unknown             = allow
quota_error_prefix  = "agrouter: "
quota_error_message = Limit reached

[cli.tool-a.availability.usage]
transport   = json-helper
command     = tool-a-status
args        = ["--json"]
```

Exec processing flow: parse → load config → validate operand/flags/conflicts → (concurrently) capture input and run probes → evaluate evidence → exclude/abort → `Router.Route` (complexity, stages) → build argv → selection line → child.

## What goes where

- **Implementation steps** (`[ ]`): code, tests, embedded config, README/`CLAUDE.md`/`AGENTS.md` in this repository.
- **Post-completion** (no checkboxes): consumer configuration and wrappers in svtlv/Ralphex, manual prompt-free smoke checks against real CLIs.

## Implementation steps

### Task 1: Positional exec CLI, separator, and verbosity boundaries

**Files:**
- Modify: `cmd/agrouter/flags.go`, `cmd/agrouter/flags_test.go`, `cmd/agrouter/app.go`, `cmd/agrouter/app_test.go`, `pkg/args/request.go`, `pkg/config/defaults/config` (comments only), `README.md`, `CLAUDE.md`.

- [x] Parse `exec <cli>`; validate missing/unknown/disabled operands and redundant/conflicting `--cli` before Jev.
- [x] Remove `AGROUTER_CLI` from `flags.go`, `pkg/args/request.go`, the embedded config comments, and README's environment layer; warn once when it is set.
- [x] Track separator presence; reject `--` in decision mode; forward the native tail as-is for exec (via the existing raw passthrough path).
- [x] Make `--print TEXT` a long alias of `-p`/`--prompt` (counted as the same flag for the "second `-p`" usage error) and update its help text.
- [x] Remove child-option flags (`--permission-mode`, `--dangerously-*`, `--output-format`, `--sandbox`, `-c`) and `-c` constraint extraction; mapping code goes dead but still compiles.
- [x] Always log the concise exec selection (`selectionOutput(d, false)`).
- [x] Write tests: operand errors, removed flags as usage errors, `--print TEXT` equal to `-p TEXT` in both modes, `-p a --print b` as a second-prompt error, bare `--print` before `--` as a usage error, `--print` after `--` forwarded to the child, `AGROUTER_CLI` ignored with a warning in both modes, stdin prompt replayed byte-for-byte with appended native flags after `--` (no prompt in argv), prompt text after `--` not used for routing, separator with empty tail, exact native forwarding (empty tokens, spaces, repeated flags, later `--`), and all four agrouter/child verbosity combinations.
- [x] Update end-to-end calls, README invocation/help sections, and `CLAUDE.md` for the new contract; run `make test`.

### Task 2: Replace mappings with invocation descriptors

**Files:**
- Modify: `pkg/config/config.go`, `pkg/config/load.go`, `pkg/config/validate.go`, `pkg/config/defaults/config`, `pkg/config/*_test.go`.
- Modify: `pkg/args/build.go`, `pkg/args/redact.go`, `pkg/args/request.go`, `pkg/args/*_test.go`, `pkg/router/eligibility.go`, `pkg/router/eligibility_test.go`.
- Modify: `cmd/agrouter/flags.go`, `cmd/agrouter/app.go`, `cmd/agrouter/*_test.go`, and synthetic fixtures in `pkg/catalog`, `pkg/router/testdata`.

- [x] Restructure section dispatch for `cli.<name>`, `cli.<name>.availability[.<check>]` (accepted and ignored until Task 4), and `cli.<name>.args` (migration error); forbid `.` in CLI names; skip subsections of disabled CLIs; error on orphans.
- [x] Add and validate `exec_args`, `model_args`, `effort_args`, `prompt_args`, `selection_flags`, `config_flags`, `selection_config_keys`, `context_flags`; migrate embedded config.
- [x] Remove `args.Arg`, `ConfigKeyPrefix`, `prefer`, `Skipped`, the `skipped` JSON field, and unmapped warnings; build argv from descriptors.
- [x] Implement the native conflict scan and the duplicate-separator check.
- [x] Write tests: dispatch (dotted names, orphans, disabled CLI subsections, `.args` migration error, layering), descriptor validation (placeholder placement, prompt last), argv with set/unset model/effort, conflict spellings (bare, `=`, attached short, `-c key=`, `-ckey=`, quoted, after child `--` ignored, value never echoed), duplicate separator, mapping-free eligibility.
- [x] Update `-update` goldens, README configuration section, and `CLAUDE.md` mapping rules; run `make test`.

### Task 3: Failure and null reasons in decision and exec JSON

**Files:**
- Modify: `pkg/jev/client.go`, `pkg/jev/*_test.go`, `pkg/router/router.go`, `pkg/router/stage.go`, `pkg/router/router_test.go`, `pkg/router/stage_test.go`.
- Modify: `cmd/agrouter/app.go`, `cmd/agrouter/confidence.go`, `cmd/agrouter/app_test.go`, `cmd/agrouter/confidence_test.go`, `README.md`, `CLAUDE.md`.

- [x] Add typed sentinel errors for Jev answer validation; carry failing stage and cause through router errors with `%w`, including the cannot-decide error.
- [x] Add `reason` and `null_reasons` to decision and exec JSON (concise and verbose); make `cli` nullable.
- [x] Emit decision JSON with nulls and reasons on multi-CLI failure, keeping exit 2 and the stderr error line.
- [x] Write tests in both modes and verbosity levels: success (fields omitted), missing key, timeout, cancellation, unauthorized, API/transport error, null/missing/empty answers, each failing stage, chunked routing, caller-fixed values preserved, `delegated` and `no_applicable_effort` omissions, and no secrets/prompt/doc text in messages.
- [x] Update the README output and exit-code sections and `CLAUDE.md` stdout rule; run `make test`.

### Task 4: Readiness model, availability config, and generic probe execution

**Files:**
- Create: `pkg/readiness/readiness.go`, `pkg/readiness/readiness_test.go`, `pkg/readiness/helper.go`, `pkg/readiness/helper_test.go`, `pkg/runner/probe.go`, `pkg/runner/probe_test.go`, `pkg/runner/probe_unix.go`, `pkg/runner/probe_windows.go`.
- Modify: `pkg/config/config.go`, `pkg/config/load.go`, `pkg/config/validate.go`, `pkg/config/*_test.go`, `pkg/runner/runner_unix_test.go`, `pkg/runner/runner_windows_test.go`.

- [x] Define evidence types, status/reason codes, and scopes; decode and validate `[cli.<name>.availability]` and check subsections (transport names, timeout, unknown policy, prefix/message without newline/control characters, `min_version`).
- [x] Add executable resolution and bounded probe I/O: own stdin, response byte limit, environment without the Jev key, real working directory, timeout.
- [x] Add `Setpgid` group kill on Unix and a probe Job Object on Windows, leaving the real child's process handling unchanged.
- [x] Implement the `json-helper` transport and normalized schema parsing.
- [x] Write tests: missing/nonexecutable command, timeout, response limit, key removal from env, stdin isolation, descendant cleanup (platform test files), malformed helper output as unknown, a third synthetic CLI configured entirely in a local layer.
- [x] Run `make test` (keep identifiers and comments free of CLI names and effort literals for the guard test).
- ➕ Notes: a check without `command` probes the CLI's own command; the SDK-control and JSON-RPC transport names are accepted by config now and report quota unknown (`transport_unsupported`) until Tasks 5-6; check sections not listed in `checks` are ignored so a layer can replace the list; `quota_error_prefix` defaults to `"agrouter: "`, `quota_error_message` to empty (Task 8 formats the line).

### Task 5: SDK control-stream usage decoder

**Files:**
- Create: `pkg/readiness/control.go`, `pkg/readiness/control_test.go`, `pkg/readiness/testdata/control/*.jsonl`.
- Modify: `pkg/config/defaults/config`.

- [x] Implement initialization, the usage control request, correlation, and unrelated-message handling; never emit a user event.
- [x] Normalize five-hour, weekly, model-scoped, and extra-usage evidence; map non-subscription auth to `not_applicable`; enforce `min_version` plus shape validation.
- [x] Configure the embedded Claude check (real working directory, hooks/MCP/persistence suppressed, no `--bare`) and its quota error prefix/message.
- [x] ⚠️ Investigate whether the control protocol's initialization or a read-only control request reports the effective default model/effort for the session; if it does, normalize it as `defaults` evidence, otherwise report defaults unknown and record the finding here.
- [x] Write tests with synthetic fixtures: quota available, weekly exhausted with five-hour available, both exhausted with different resets, missing/null windows, unsupported version, unknown shape, throttling, API-key context; assert every message the fake receives contains no prompt, user event, or inference action.
- [x] ⚠️ Measure cold-start latency of the real probe once (prompt-free) and record it here; run `make test`.
- ➕ Findings (Claude Code 2.1.296, prompt-free, 2026-10-13):
  - Requests sent, in order, each waiting for its correlated `control_response`: `initialize`, `get_binary_version` (only with `min_version`; its `{"version"}` avoids the init field whose name contains the CLI name, which the guard test rejects), then `get_usage` with `skip_behaviors: true` and `get_settings`. A fixed allow-list of these four reads is enforced in code; other stdout lines and other request ids are skipped.
  - Defaults: the read-only `get_settings` reports `applied.model` and `applied.effort`, the values the session would send after env overrides, settings and model-support downgrades. They are normalized as `defaults` evidence (a resolved model id, which Task 9 must match against catalog values/aliases); a failed or malformed settings read leaves defaults unknown without affecting quota.
  - Usage: `subscription_type` null with `rate_limits_available` false is `not_applicable` (`not_subscription`); `rate_limits` null otherwise is unknown. `five_hour`/`seven_day` are always reported (unknown when null). A non-null `locked_reason` is `quota_denied` whatever the utilization; a full window with `extra_usage.is_enabled` and no `spend_limit_reached` is unknown (`extra_usage_enabled`). `model_scoped[]` windows are weekly and scoped to the server's `display_name` label (Task 7 matches the label case-insensitively against catalog names/aliases); the legacy per-family keys are ignored so no model family is hardcoded. A successful usage read adds auth `available`.
  - Throttling is recognized only from a control error `error_code` (`rate_limit_error`, `rate_limited`, `throttled`); a usage fetch failure that the CLI answers with null `rate_limits` is unknown `not_reported`.
  - Not verified: whether a logged-out CLI answers with null `subscription_type` (it would then read as `not_applicable` and the child's own login error surfaces).
  - Cold start: about 1.6s for the bare CLI exchange and 1.8s through the checker (well inside the 5s check timeout; about 18% of the default 10s `[agrouter] timeout`, which Task 8 revisits).

### Task 6: JSON-RPC rate-limit decoder

**Files:**
- Create: `pkg/readiness/rpc.go`, `pkg/readiness/rpc_test.go`, `pkg/readiness/testdata/rpc/*.jsonl`.
- Modify: `pkg/config/defaults/config`.

- [x] Implement initialization, read requests, correlation, notifications, pagination, and bounded shutdown; reject thread/turn and other mutating methods.
- [x] Normalize multiple buckets, server-provided durations, resets, and explicit usage-permission/limit-reached/spend-control denials.
- [x] Configure the embedded Codex check and its quota error prefix/message.
- [x] ⚠️ Investigate the app-server read that returns the effective configuration (model and reasoning effort as resolved for this working directory and profile); normalize it as `defaults` evidence and never treat the model list's default flag alone as verified. Record the finding here.
- [x] Write tests: both windows, scoped buckets, denial despite low percentages, missing/null permission, credits as unknown, API-key/custom provider, hidden/paginated models, RPC errors, effective-config defaults present/absent, model-list default without effective config staying unknown; assert no thread/turn or mutating actions.
- [x] ⚠️ Measure cold-start latency once and record it; run `make test`.
- ➕ Findings (codex-cli 0.162.1, prompt-free, 2026-10-13):
  - Messages sent: `initialize` (its `userAgent` is `<client>/<version> ...`, the version source for `min_version`), the `initialized` notification, then `account/read`, `account/rateLimits/read` (with `excludeResetCreditDetails`) and `config/read` (with `cwd` = the real working directory) together, and `model/list` (`includeHidden`, paged by `nextCursor`, at most 20 pages) only when needed. A fixed allow-list of these reads is enforced in code; server notifications, server requests and other response ids are skipped. The server exits when its stdin closes; one that lingers is killed after 1s.
  - Account: `requiresOpenaiAuth` false (custom provider) or a non-subscription account type is quota `not_applicable`; a null account with auth required is auth `unavailable` (`auth_failed`); a subscription account adds auth `available`.
  - Rate limits: the top-level `rateLimits` bucket is account-wide and always reports both windows (unknown when missing); other `rateLimitsByLimitId` buckets are scoped to `normalModelSlug`, else `limitName`, else the limit id (Task 7 matches the label). Windows are named by `windowDurationMins` (300 five-hour, 10080 weekly, anything else unnamed), never by position. `ordinaryUsageAllowed` false is an account-wide `quota_denied`, null/missing is unknown `usage_permission_not_reported`; a bucket's `rateLimitReachedType` or `spendControlReached` is `quota_denied` for its scope whatever the percentages; credits (`hasCredits`/`unlimited`) turn a full window into unknown `extra_usage_enabled`. A read error with the `rateLimitExceeded` error info or an HTTP 429 status in its data is `probe_throttled`.
  - Defaults: `config/read` with `cwd` returns the effective `model` and `model_reasoning_effort` for that directory and the default profile; they are normalized as `defaults` evidence. A configured model without an effort takes that model's `defaultReasoningEffort` from the model list (hidden models included). With no configured model both stay unknown and the model list is not read, so its `isDefault` flag is never taken as the child's default. Native `-c`/`--profile` overrides are covered by the selection conflict check and `context_flags`.
  - Cold start: about 0.5s through the checker (well inside the 5s check timeout).

### Task 7: Evaluate evidence against eligible combinations

**Files:**
- Create: `pkg/readiness/evaluate.go`, `pkg/readiness/evaluate_test.go`.
- Modify: `pkg/router/eligibility.go`, `pkg/router/eligibility_test.go`, `pkg/router/router.go`, `pkg/router/router_test.go`.

- [x] Implement evaluation: per-option status, scope matching, delegated-model handling, latest-reset recovery, unknown policy.
- [x] Add `Eligibility.Exclude(ids)`; guard `Route` and `cannotDecide` against empty eligibility with a typed error.
- [x] Write tests: five-hour/weekly/both exhaustion, weekly survives five-hour reset, scoped alternatives within one CLI, fixed and out-of-catalog constraints, unknown allow/block, stale or malformed evidence, all candidates unavailable, empty-eligibility guard.
- [x] Run `make test`.
- ➕ Notes: `readiness.Evaluate(report, options, models, unknown, now)` imports `pkg/catalog` for the option type (dependency line for Task 11: `config`, `catalog`, `runner` <- `readiness`). Precedence per option: missing executable, failed auth, unsupported model, quota block, then unknowns; unknown executable/auth/capability never stop the child. Quota: an account-wide `not_applicable` needs no windows; otherwise both windows must be verified available (else unknown `window_not_reported`). A block whose verified reset is already past is unknown `quota_reset_passed`; a block's reset is the latest of its windows and is omitted when any is unverified. Model-scoped labels match the model name, section or an alias, ignoring case. `Evaluation.Model(name)` serves Task 9: an empty name (delegated) is unknown `delegated_model_unverified` under any model-scoped quota block, and is never matched against unsupported-model lists. When everything is excluded, `Blocked` is a known unavailable verdict when any exists (preferring the quota block with the earliest verified recovery), else the refused unknown. `Eligibility.Exclude` records a CLI it empties as a drop with reason `unavailable`; `Route` returns `router.ErrNoEligible` for empty eligibility without asking Jev, and `cannotDecide` wraps it with the cause.

### Task 8: Exec preflight before any Jev request

**Files:**
- Create: `cmd/agrouter/readiness.go`, `cmd/agrouter/readiness_test.go`, `cmd/agrouter/mocks/readiness_checker.go` (generated).
- Modify: `cmd/agrouter/app.go`, `cmd/agrouter/debug.go`, `cmd/agrouter/app_test.go`, `cmd/agrouter/debug_test.go`, `README.md`, `CLAUDE.md`.

- [x] Wire `ReadinessChecker` only in exec, after operand/config/conflict validation, concurrently with input capture, before `Router.Route`, under the total deadline.
- [x] Apply exclusions; emit the unknown warning, the quota line, the auth/executable line, or the strict-unknown line; return 127/2 as specified.
- [x] Add redacted readiness evidence to `AGROUTER_DEBUG=1` output.
- [x] Write end-to-end tests: total block with zero complexity and selection Jev calls, empty stdout and no selection line, quota line for both synthetic prefix styles (plain `agrouter:` and a severity prefix), latest reset, absent reset, auth/throttle/unknown never printing a quota phrase, fixed constraints, single-candidate bypass still probed, no cross-CLI switch, deadline behavior, unchanged stdin replay; decision mode never calls the checker, even with missing executables and `--doc`.
- [x] Update the README exit-code table and `CLAUDE.md` exit-code and prefix rules (narrow severity-prefix exception); run `make generate` and `make test`.
- ➕ Notes: probes start once the operand, native conflicts, `--prompt-file` and duplicate-separator checks pass, run while docs and stdin are captured, and share the routing context (`[agrouter] timeout`) besides the availability timeout; an early failure cancels them and waits. Budget (the Task 5 ⚠️): the measured 0.5-1.8s cold starts stay inside the default 10s routing budget, so there is no separate preflight bound and the README records this. A `context_flags` spelling in the native tail skips the probes (the CLI is checked with its availability cleared) and `Report.ContextChanged` keeps only the executable lookup plus unknown quota `context_changed`; native tokens never reach a probe. Blocks return `*preflightError` from `route`, printed verbatim; the quota line names the blocking windows (`five-hour and weekly`) and the UTC reset. Debug prints one `readiness <cli>:` line per evidence, the reported defaults, and the excluded count.

### Task 9: Exec fallback and effective metadata

**Files:**
- Modify: `cmd/agrouter/app.go`, `cmd/agrouter/confidence.go`, `cmd/agrouter/readiness.go`, `cmd/agrouter/app_test.go`, `cmd/agrouter/confidence_test.go`, `README.md`.

- [x] Add `fallback` when `Undecided` is set and `effective` model/effort with `router`/`caller`/`verified_child_default`/`child_default_unknown` sources, using preflight `defaults` evidence.
- [x] Check a verified delegated default model against model-scoped blocks; block the fallback (quota line, exit 127, no child) when it is known blocked.
- [x] Emit the fallback warning line (failing stage, reason code, fallen-back fields only, each verified default or "unknown" with its reason, "not a Jev selection") before the selection line.
- [x] Add the compact readiness summary to the selection line when exclusions or unknown warnings applied.
- [x] Write tests: success (no warning, no `fallback`), failure at each stage, effort-stage failure keeping Jev's model with `source: router` and warning only about effort, caller-fixed values never listed as fallbacks, warning text with verified and unknown defaults, exactly one warning and one selection line, verified and unknown delegated defaults, defaults unknown with a context flag, blocked verified default, readiness-filtered fallback, chunked routing, no fallback for intentional omissions, line stays single and concise.
- [x] Update README exec output section; run `make test`.
- ➕ Notes: the router now keeps a model Jev chose at an asked model stage when a later stage fails (`askedModel`), in both modes, so an effort-stage failure runs Jev's model; a skipped model stage still delegates. The fallback code lives in a new `cmd/agrouter/fallback.go`. `effective` appears only when a model or effort is left to the child (otherwise the top-level values already say what runs); a `NoEffort` model has no effort entry. A delegated model is judged with `Evaluation.Model` on success too: a verified default known blocked is the preflight block line (127), an unknown one under a model-scoped block warns once (`delegated_model_unverified`) or, under `unknown = block`, exits 2. The fake Jev ignores chunk uploads the client abandons once another chunk failed.

### Task 10: Verify acceptance criteria

**Files:**
- Modify: integration tests and synthetic fixtures under `cmd/agrouter`, `pkg/readiness`, `pkg/router`, `pkg/runner` as needed.

- [x] Verify every source backlog requirement and the decisions above; add missing cross-feature regressions (separate exec invocations never calling Jev while blocked; decision mode independent of availability).
- [x] Confirm every changed file belongs to agrouter.
- [x] Run `make test`, check 80%+ per-package coverage excluding mocks, run `make build`.
- [x] Run `make lint` (golangci-lint v2.14.0) for the host, `GOOS=linux`, `GOOS=darwin`, `GOOS=windows`, and `--build-tags=eval`; check guard tests and generated mocks.
- [x] Confirm no test invokes real inference or `make eval-routing`.
- ➕ Notes: added `TestApp_ExecBlockedAcrossInvocations` (three blocked exec runs each probe afresh, print the same quota line, exit 127 and make no Jev request; routing resumes once the probe reports headroom) and `TestApp_DecisionIndependentOfAvailability` (a blocking strict availability config leaves decision output and Jev requests byte-identical while the same config blocks exec). Per-package coverage 90.6-100% excluding mocks; lint clean with v2.14.0 on host, linux, darwin, windows and `eval`; `make generate` leaves mocks unchanged; every changed file is in this repository; the only real-Jev tests are the `eval`-tagged ones and no test starts a real CLI.

### Task 11: [Final] Documentation and backlog closure

**Files:**
- Modify: `README.md`, `AGENTS.md`, `CLAUDE.md`, embedded config comments.
- Delete: `docs/backlog/raw-cli-arguments-after-separator.md`, `docs/backlog/exec-cli-readiness-before-jev.md`, `docs/backlog/exec-fallback-target-and-reason.md`.
- Move: this plan into `docs/plans/completed/`.

- [x] Document the migration (removed flags, sections, and fields), native conflicts, per-CLI availability and unknown policy, a configuration-only third CLI with `json-helper`, probe/version limitations, a stdin-based control-probe example, and the Ralphex usage examples from Post-completion labeled as user-owned.
- [x] Update `CLAUDE.md` layout and dependency line for `pkg/readiness`, and fix the stale golangci-lint version (v2.13.0 → v2.14.0).
- [x] Verify help/examples against parser tests.
- [x] `git rm` the three source backlog items in the commit that lands this task, after Task 10's acceptance checks pass (their requirements span Tasks 1-9, so no earlier commit completes any of them). Move the plan to `docs/plans/completed/` in the same commit. Do not commit or push without user authorization.
- ➕ Notes: README gained "Availability checks" (keys, transports, unknown policy, json-helper schema with a configuration-only third CLI that was run end to end against a fake helper, limitations, a stdin control-probe example checked live), "Migrating from mapped arguments", and a user-owned Ralphex section with the adapter and quota-line overrides (the quoted `"ERROR: agrouter: "` prefix verified to keep its trailing space). `--help` lost a stale "skipped argument"; `TestParseArgs_DocumentedExamples` parses the documented shapes. `CLAUDE.md` and `AGENTS.md` are identical again.

## Post-completion

*Informational, user-owned, outside the implementation scope.*

**Ralphex contract this plan satisfies** (verified against local checkouts on 2026-10-12: svtlv `feture/digest-map-reduce` at `8df9ac9c`, Ralphex `windows` at `90f1b1b9`):

- Claude execution merges stderr into its parsed stream and treats non-JSON `agrouter:` lines as diagnostics (`pkg/executor/executor.go`); its patterns include `You've hit your limit`. Configure `quota_error_prefix = "agrouter: "` and `quota_error_message = You've hit your limit`.
- Codex execution accepts limit patterns only on `ERROR:`, `FATAL:`, or `PANIC:` lines (`pkg/executor/codex.go`). Configure `quota_error_prefix = "ERROR: agrouter: "` and `quota_error_message = Rate limit exceeded`. Its display filter can hide the selection line; the error matcher still sees the quota line.
- Example lines (only the exec CLI's line is printed):

  ```text
  agrouter: You've hit your limit; weekly subscription quota exhausted; resets at 2026-10-13T15:00:00Z
  ERROR: agrouter: Rate limit exceeded; weekly subscription quota exhausted; resets at 2026-10-13T15:00:00Z
  ```

- Ralphex alone waits and retries (`LimitPatternError`, `pkg/processor/execution_policy.go`). Each retry re-probes for free and exits with the same line while blocked; a weekly block survives a five-hour reset. A temporary harness confirmed both lines become `LimitPatternError`, a plain Codex `agrouter:` line fails the prefix gate, and auth failures are not limits.

**Consumer setup:**

- svtlv `.ralphex/config` already uses `claude_command = agrouter` and `claude_args = exec claude --doc=CLAUDE.md -- --dangerously-skip-permissions --output-format stream-json --verbose`; Ralphex appends `--print` and supplies the prompt through stdin, which reaches the child because `claude_args` ends after `--`. Keep `task_model = :`, `review_model = :`, `codex_model =`, `codex_reasoning_effort =`.
- Its `codex_command = agrouter exec codex --` is incompatible: Ralphex runs Command as one executable and prepends native `exec`. Install the adapter below and point `codex_command` at it.
- Migrate `.agrouter/config` from `[cli.claude.args]`/`[cli.codex.args]` to invocation and availability descriptors; never copy API keys.
- Enable waiting with `wait_on_limit = 15m` or `ralphex --wait=15m`; without it Ralphex stops on a recognized limit.
- Ralphex `--worktree` may keep the launch directory's `.ralphex/config` (its `docs/backlog/worktree-ignores-its-own-ralphex-config.md`); account for that when verifying.

Reference POSIX adapter (`scripts/agrouter-codex` in svtlv, executable):

```sh
#!/bin/sh
if [ "$1" != exec ]; then
    printf '%s\n' 'agrouter: expected native exec subcommand' >&2
    exit 2
fi
shift
exec agrouter exec codex --doc=CLAUDE.md -- "$@"
```

**Manual verification:** optional prompt-free smoke checks on supported CLI versions for quota windows and reset handling. Review the experimental control usage protocol on CLI upgrades; if it breaks, report unknown (or stop under strict policy), never substitute an inference probe or auth-only readiness.
