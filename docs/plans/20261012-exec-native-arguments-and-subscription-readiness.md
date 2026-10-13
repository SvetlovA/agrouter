# Exec native arguments and subscription readiness

## Overview

Combine the three current backlog items into one change:

- [Explicit exec CLI, native arguments, and separate verbosity](../backlog/raw-cli-arguments-after-separator.md).
- [CLI readiness before Jev](../backlog/exec-cli-readiness-before-jev.md).
- [Exec fallback targets and reasons](../backlog/exec-fallback-target-and-reason.md).

The resulting invocation contract is:

```sh
agrouter [routing options] [prompt]
agrouter exec <cli> [routing options] [prompt] [-- native args...]
```

Decision mode routes across the configured enabled catalog, subject to caller constraints, and prints decision JSON without checking local CLI availability. Exec requires a positional CLI, checks that CLI's availability and subscription five-hour/weekly limits before any Jev request (including project complexity), routes within the remaining combinations, and starts the child with native arguments. If every exec candidate is blocked, it makes no Jev requests. Exec reports the reason using Ralphex's existing limit-pattern style; Ralphex applies its own configured wait and retry policy.

Availability checks are exec-only, following the user's latest clarification and the source backlog. Decision mode performs no executable/auth/quota/capability probes, reads or writes no availability cache, and does not filter candidates using availability evidence. All provider-specific availability checks belong under that provider's `[cli.*]` configuration, so adding another CLI using a supported probe transport requires configuration rather than provider-specific Go changes.

Both modes include reasons in their output JSON when Jev fails or selection values are null. This user requirement expands the fallback backlog's exec-only diagnostics: decision JSON must also explain failure/nulls, without adding availability checks or changing the selection/fallback policy.

This file is a plan only. Keep the backlog items until their implementations land; creating a plan does not close them.

**Implementation scope: agrouter only.** All code, configuration, documentation, fixtures, and test changes stay in this repository. The svtlv and Ralphex findings inform agrouter's invocation and diagnostic contracts, but no task edits either consuming repository, migrates their configuration, or creates/installs a wrapper there. The user will make any consuming-project changes later. External setup and end-to-end consumer verification are informational, user-owned follow-up, not completion criteria for this plan. Required tests use agrouter-owned synthetic fixtures and do not require external checkouts.

## Context

- `cmd/agrouter/flags.go` already splits at the first `--`, but accepts mapped child flags and obtains exec's CLI from `--cli` or the environment.
- `pkg/args/request.go`, `build.go`, and `redact.go` own requests, mapped argv construction, and redaction.
- `pkg/config/config.go`, `load.go`, `validate.go`, and `defaults/config` define and validate `[cli.*.args]` mappings. Removing them requires updating synthetic fixtures throughout the repository.
- `pkg/router/eligibility.go` applies constraints and mapped-argument preferences. Its nonempty-list assumption and `router.cannotDecide` must be protected from empty readiness results.
- `cmd/agrouter/app.go` captures input, builds eligibility, and invokes `Router.Route`. Readiness belongs before that call, which can request project complexity before selection.
- `pkg/runner` currently performs executable lookup after routing and preserves child streams, stdin replay, process handling, and exit codes.
- `cmd/agrouter/confidence.go` already produces concise confidence summaries. Exec currently passes its verbosity flag into the shared selection formatter, exposing verbose details.
- `router.Decision.Undecided` already records why routing failed; incomplete stages are discarded for selection while completed complexity can remain.
- `cmd/agrouter/guard_test.go` prohibits CLI/model/effort names outside embedded config. Probe implementations must use generic protocol names and configured descriptors.

## Readiness investigation and chosen approach

### Evidence collected on 2026-10-12

| CLI | Prompt-free mechanism | What it establishes | Limits |
| --- | --- | --- | --- |
| Claude Code | `claude auth status --json` | Current authentication method and logged-in status | Does not establish subscription headroom |
| Claude Code | SDK stream control request `get_usage`, with `skip_behaviors: true` | Subscription five-hour and weekly utilization, reset times, and additional scoped/extra-usage information when returned | Experimental protocol; missing fields and metadata failures are inconclusive |
| Codex | `codex login status` | Credentials are present | Does not independently validate quota or remote credential validity |
| Codex | App-server `account/read`, `account/rateLimits/read`, and `model/list` | Account/provider state, quota-window snapshots, and advertised model/effort capabilities | Provider-dependent; advertised capabilities are not a guarantee that execution succeeds |

Sources: [Claude CLI reference](https://code.claude.com/docs/en/cli-reference), [official Claude Agent SDK 0.3.296 package](https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.296.tgz), [Codex login reference](https://learn.chatgpt.com/docs/developer-commands?surface=cli), and [Codex app-server reference](https://learn.chatgpt.com/docs/app-server).

Read-only local prototypes confirmed the mechanisms with Claude Code 2.1.296 and codex-cli 0.162.1:

- Claude accepted initialization followed by `get_usage` without any user message. Both quota windows were present; session cost was zero.
- A copy-paste shell example must send control JSON through stdin (for example, a quoted heredoc), never as an argument after `--print`, which becomes a normal inference prompt. A verified two-control-message stdin probe on 2.1.296 returned only `control_response` records and cost zero. Successful metadata reads exit 0 even when weekly quota is exhausted, so evaluate the returned windows rather than treating process success as executable capacity.
- Codex accepted initialization followed by `account/rateLimits/read` without creating a thread or turn. Returned window durations were 300 and 10,080 minutes.
- The Claude SDK explicitly labels its usage method experimental. Record the verified protocol version and fixture shape; do not infer support for older releases.
- Schema generation from installed codex-cli 0.162.1 (`codex app-server generate-json-schema --out <temporary-directory>`) also confirms `ordinaryUsageAllowed`, `rateLimitReachedType`, and `spendControlReached` evidence. Backend permission/spend-control denials must not be discarded merely because window percentages are below 100%; null/missing permission is unknown and cannot establish recovery.
- Claude's documented status-line quota fields appear after an API response, so starting a session solely to obtain those fields does not satisfy preflight. [Status-line documentation](https://code.claude.com/docs/en/statusline).

Three approaches were considered:

1. **Native control/status protocols, chosen:** the child CLI handles its own credentials; agrouter reads structured quota metadata without sending a prompt.
2. **Direct subscription HTTP endpoints:** Claude's installed executable contains `/api/oauth/usage`, but integrating it would require credential-store handling and an internal endpoint contract. Keep this outside the initial implementation unless native control probing proves unusable.
3. **Inference probe or auth-only preflight:** neither satisfies the requirement. Do not send a hidden test prompt, and do not label a login check as subscription readiness.

## Development approach

- Confine every implementation task and required check to agrouter; document consumer setup as examples for the user to apply later.
- Regular development: implementation and behavioral tests in the same task, then checks before the next task.
- Use table-driven testify tests, synthetic catalogs, fake status processes, and mocked Jev. Do not pin shipped names, model lists, counts, or configuration contents.
- Complete each task fully; keep the plan synchronized with implementation discoveries and mark checkboxes immediately.
- Keep production catalog names and per-CLI behavior in config. Use consumer-side interfaces and regenerate moq mocks with `make generate` when interfaces change.
- Run targeted tests after edits and `make test` before advancing to the next code task. Maintain 80%+ coverage per package, excluding mocks.
- This is an intentional invocation/configuration migration. Document the changed contract and provide actionable errors for removed flags and sections.

## Solution overview

### Invocation, configuration, and native arguments

- Exec consumes its positional CLI immediately after `exec`. Missing, unknown, or disabled CLI operands return exit 2 before probing or Jev. `--cli` and `AGROUTER_CLI` cannot supply that operand.
- In exec, the positional CLI wins over `AGROUTER_CLI`; the environment remains a decision-mode constraint. An explicit `--cli` equal to the exec operand is accepted as redundant; a conflicting explicit value returns exit 2.
- In decision mode preserve the existing `--cli`/environment constraint behavior, including the warning for an unusable constraint. Model/effort constraints and values outside the catalog retain their existing behavior.
- Preserve whether a separator was supplied, independently of how many tokens follow it. Decision mode rejects `--`, including an empty passthrough tail, with exit 2.
- Remove translated child flags, `-c` routing extraction, `args.Arg`, mapping preferences, skipped-argument output, and all `[cli.*.args]` sections. Removed sections must identify the migration rather than silently changing execution.
- Retain a small, fixed invocation descriptor under `[cli.*]`: `exec_args`, `model_args`, `effort_args`, and `prompt_args`. These only construct agrouter-owned invocation pieces; they do not map caller options. Validate the three named value slots, require safe prompt placement, and do not retain arbitrary mapping keys or `{value}` expansion.
- Build argv as command + noninteractive args + selected model/effort args + native tokens + prompt delivery. Omit an unset model/effort rather than choosing a catalog default. Preserve current stdin replay and prompt-source ordering.
- Forward native tokens without rewriting, normalization, deduplication, catalog validation, or unmapped warnings. Preserve empty tokens, spaces, repeated flags, and subsequent `--` tokens exactly.
- Agrouter owns model/effort selection. Reject direct native selection overrides before routing, using configured reserved flag spellings and native config-key selectors, including aliases, `--flag=value`, short forms, and `-c` model/effort keys. Check only conflict syntax; do not turn native values into routing constraints or expose them in errors.
- Stop conflict scanning at the child's own option terminator and account for configured option-value arities so a flag-looking value is not misclassified. Do not claim that every opaque child settings file can be analyzed. If settings/profile/provider options change execution context, apply that same context to probes where supported; otherwise affected checks/default resolution become unknown.
- `--verbose` before the separator affects only agrouter; after it affects only the child. No automatic child verbosity injection for stream output.

### Probe boundary and protocols

- Add a `pkg/readiness` package depending on config and generic probe support in runner. It returns evidence; `cmd/agrouter` applies evidence to router eligibility.
- Put the complete availability descriptor in `[cli.<name>.availability]` and named check subsections `[cli.<name>.availability.<check>]`. The parent defines enabled checks, timeout, unknown policy, cache freshness/backoff, and `quota_error_prefix`/`quota_error_message` for native-compatible exec failures. Each check defines its generic transport, command/argv, initialization/read actions, response field paths/units, applicability, quota-window/model scope, supported versions, and explicit failure mappings. Resolve these through normal embedded/global/local/environment layering. Keep only the overall routing deadline under `[agrouter]`; do not add a separate provider registry or provider switches in Go.
- Support reusable exit-status, JSON command, SDK control stream, and JSON-RPC stream transports. An additional CLI with a JSON status command can be configured directly. A provider with a new wire protocol can use a configured prompt-free helper returning the normalized evidence JSON contract; adding an entirely new transport remains a generic code extension. Missing availability configuration yields unknown quota/auth evidence, never implicit readiness.
- Define `ReadinessChecker` on the consumer side in `cmd/agrouter`; tests replace it. Keep filesystem cache and clock injectable for deterministic tests.
- Resolve the configured executable with the same semantics as the runner, including Windows extensions and `.cmd` shims. Missing/nonexecutable commands are known unavailable.
- In exec check only the required positional CLI, once per relevant context where possible, and filter only its combinations. Decision/help/version remain probe-free, with no availability cache access or availability-based eligibility changes. In exec check all applicable quota windows even when a stage has only one candidate and will skip Jev.
- Probes receive their own stdin containing only protocol messages, never captured prompt text, stdin replay, or documents. Remove the Jev key from their environment, just as for the real child.
- Bound a probe to a configurable five-second default, further limited by the remaining `[agrouter] timeout`. Probe time belongs to the existing total pre-routing deadline. Bound probe response bytes independently of unrestricted prompt capture/chunking.
- Spawn no shell on Unix; retain the existing quoted Windows batch handling. Kill probe descendants on cancellation and release their pipes/process handles.
- Configured probe executables/helpers share the existing configured child-command trust boundary. Allow a new CLI and its checks in any already supported config layer, including project-local configuration; do not invent an embedded-only or global-only registration requirement. Validate descriptors and document that users must trust configured commands.
- Ship only read/status actions; never construct user messages, inference turns, login/logout, or credit-consuming actions. Reject known mutating actions in built-in protocol transports. An arbitrary configured executable/helper cannot be proven read-only by schema validation; its prompt-free behavior is part of the documented command trust contract. Credentials stay with the CLI; never dump raw responses or error bodies.

**Claude probe:** initialize a stream-json control connection, then send `get_usage` with transcript scanning disabled. Do not send a `user` event. The prototype used an isolated temporary working directory, `--safe-mode`, empty setting sources, disabled hooks, empty strict MCP configuration, and no session persistence. Do not use `--bare` for subscription checks: it skips OAuth/keychain authentication. Authenticate against the actual effective child context, and classify unrepresentable settings/provider changes as unknown. Recheck that startup customizations are suppressed on supported versions. [Headless documentation](https://code.claude.com/docs/en/headless).

**Codex probe:** initialize one stdio app-server connection; read account state and quota metadata, and optionally capability/default metadata. Never create a thread or start a turn. Follow pagination and include hidden entries when evaluating a complete advertised catalog. Request failure or omission is unknown unless an explicit authoritative denial establishes a block.

### Quota evaluation and unknown policy

- Represent executable, authentication, quota, and capability evidence separately as `available`, `unavailable`, `unknown`, or `not_applicable`, with normalized reason codes, scope, observation time, expiry, and reset time. An auth success must never populate quota success.
- For subscription-backed execution, inspect **both** the five-hour and weekly windows. Do not check only the first returned window. Use server-provided durations rather than assuming primary always means five hours.
- A valid applicable window at 100% or above makes subscription headroom unavailable. A five-hour block affects the account; a weekly all-model block does too. A model-scoped weekly block affects only models explicitly associated with that meter by configured/provider evidence.
- Configure utilization units per response format: Claude `get_usage.rate_limits.*.utilization` uses percentages (100 means exhausted), while actual child `rate_limit_event.rate_limit_info.unifiedWindows.*.utilization` uses fractions (1 means exhausted). The child event's reset timestamps are Unix seconds, while control usage resets use ISO timestamps. Never reuse one format's scale or timestamp decoder for the other.
- If several exhausted windows apply, the earliest possible recovery is the **latest** of their reset times. An unknown reset requires bounded reevaluation; resetting the shorter window must not clear an exhausted weekly window.
- Remaining quota is a snapshot, not a reservation or a prediction that the whole prompt fits. Validate percentages, timestamps, missing/null windows, and source freshness; never estimate remaining subscription allowance from local token counts.
- Respect explicit backend usage-permission, reached-limit, and spend-control evidence where the configured protocol supplies it. A denied included-usage path is not made available by a low percentage or elapsed reset; an already enabled verified credit path is evaluated separately. Preserve distinct reasons for subscription exhaustion, credit depletion, administrative spend controls, and unclassified permission denial. Missing/null fields are not affirmative permission.
- A metadata endpoint's HTTP 429 means the **probe was throttled**, not that inference quota is exhausted. Distinguish probe backoff from a subscription block. Similarly, expired credentials should be refreshed through the CLI where supported before treating them as unusable.
- Check extra-usage/credit state separately. Subscription exhaustion is reported even when paid overage is enabled; only classify the effective child as executable through overage when the child context already enables it and available evidence confirms usable credit. Never enable paid fallback automatically. Unknown overage capacity cannot cancel an established subscription block.
- API-key/custom-provider contexts have no established subscription windows: record `not_applicable` where known, or `unknown` where unverified, without requiring a subscription login.
- **Planning assumption for unknown evidence:** run every applicable free check; exclude known unavailable combinations and allow unknown combinations with a concise warning. Configure `unknown = allow | block` under each CLI's availability section; strict callers can block combinations whose quota cannot be verified. This choice was recommended during planning, but the user has not explicitly selected the default unknown policy. Unknown must never be described as ready.
- Unsupported protocol versions, parse failures, timeout, missing profile scope, or incomplete quota metadata produce unknown, not fabricated zero usage. Known blocks still take precedence over unrelated unknown checks.
- Keep caller-fixed CLI/model/effort values. Never broaden a fixed constraint to evade a block, and never switch exec to another CLI.
- Capability evidence can remove explicitly unsupported combinations. A missing model in a cached or incomplete picker list is not an authoritative access denial. Out-of-catalog caller values stay unknown unless a real denial proves them unusable.
- When no executable exec combination remains, stop before `Router.Route` and report normalized reasons/reset times. Exec returns 127 for a known unavailable target found during preflight. A strict-policy unknown block is reported as unverified availability, not subscription exhaustion, and returns exit 2. Retain existing decision-mode exit behavior and exit 2 for usage/configuration/input errors; local unavailability alone cannot make decision mode fail. Actual children retain their own exit codes.
- On a Jev failure in exec, apply cannot-decide policy to readiness-filtered eligibility. Preserve fixed values; check any independently resolved child defaults against known blocks. An unresolved child default remains explicitly unknown under the chosen policy. Decision-mode fallback uses its configured/constraint-filtered catalog without readiness evidence or runtime-default probes.

### Cache and recovery between Ralphex retries

- Store normalized quota evidence and recognized child-failure blocks in the OS user cache directory, outside the repository and independently of individual process lifetimes.
- Key records by executable identity/version, probe descriptor version, effective account/provider/configuration identity, and relevant context. Persist opaque fingerprints only; no account identifiers, prompt/doc/native arguments, credentials, or raw responses.
- Refresh lightweight auth/context identity for the selected CLI on each exec invocation. Invalidate affected records after account/login/config/provider/native-context or executable changes. If reliable identity/invalidation evidence is unavailable, use a short lifetime or disable cache reuse for that context. Decision mode does not consult or update this cache, including previously recorded quota blocks.
- Cache successful quota snapshots for at most one minute. A success never survives its freshness budget or acts as guaranteed capacity.
- For a known block with a reset, retain the block until reset, but refresh the free metadata on each Ralphex retry no more often than once per fifteen minutes. Fresh availability clears it early, including recovery through an already configured credit path. A failed metadata read cannot overwrite a still-valid block with success.
- Without a trustworthy reset, expire a negative record after thirty minutes; repeated newly observed matching child failures may extend backoff to at most sixty minutes. Never renew solely because a cached record was read. Cap unrefreshable long-reset records to sixty minutes so recovered accounts cannot stay blocked for a week on stale evidence.
- After reset/expiry, recheck free status; never turn the passage of time into proven readiness. Unknown policy applies when rechecking is inconclusive.
- Cache probe throttling separately using valid retry-after or a bounded backoff, without excluding model candidates merely because the usage endpoint is busy.
- Add exec-only `--refresh-readiness` to bypass cached evidence and retry metadata immediately. It cannot make unavailable evidence ready or override fixed constraints. Reject the flag in decision mode as a usage error rather than enabling checks there.
- Use user-only permissions, atomic replacement, and concurrency protection. Reject malformed, future-dated, or incompatible cache records. Cache I/O failures degrade to a fresh check with a redacted warning rather than a routing/config error.
- Example: weekly quota exhausted until tomorrow → Ralphex's fifteen-minute retries refresh free metadata and make zero Jev calls while the block is current → after reset or verified early recovery, routing resumes. Weekly exhaustion must remain active if the five-hour window resets first.

### Actual child failures and concise fallback diagnostics

- Observe recognized machine-readable auth/quota/capability errors from real child execution with a bounded streaming observer. Copy bytes to the original stdout/stderr unchanged; never buffer the entire child output or alter its exit status.
- Recognize error records only in the matching configured output format. For text output use narrowly defined supported terminal error signatures; ordinary generated text mentioning a limit cannot create a cached block. Record only normalized failure facts and timestamps; generic nonzero exits remain unclassified.
- Avoid changing direct terminal behavior merely to observe output. Enable observation only where transport permits it without changing stream semantics; otherwise rely on preflight evidence.
- Always construct exec logging with concise formatting, regardless of agrouter verbosity. Preserve exactly one `agrouter: selection: ` line immediately before starting a child.
- Keep `cli`, `model`, `effort`, completed project complexity, and available stage confidence summaries. No options, stage records, probabilities, chunks, prompt, docs, argv, or command in that line.
- Add shared optional `reason` metadata to decision JSON and exec selection JSON whenever routing fails: normalized `code`, failing `stage` when known, and a controlled `message`, available without `--verbose`. Cover missing Jev key, timeout/cancellation, authorization/API/transport errors, invalid or missing answers, null/empty choices, and no usable decision. Preserve failure provenance even if cannot-decide policy returns an error rather than a fallback decision. Classify from typed errors/evidence, not string parsing or raw response bodies.
- Add `null_reasons` entries for every null selection field, distinguishing routing failure, unresolved selection, intentionally delegated child defaults, and a model with no applicable effort. A successful intentional omission is not a Jev failure. A null/empty required answer from Jev is an invalid/no-decision response, not a successful intentional omission; do not fabricate a more specific server explanation than the evidence supports.
- Retain exec-only `fallback` metadata describing the allowed fallback action when `Undecided` is set, alongside the shared failure reason. Do not emit a routing-failure fallback for intentionally delegated defaults or infer a fallback merely because a value is null.
- Add exec-only `effective` entries for model/effort, each with `value`, `source` (`router`, `caller`, `verified_child_default`, or `child_default_unknown`), and an optional normalized reason. Preserve top-level selection nulls when agrouter emitted no selection flag.
- Resolve a child default only when prompt-free runtime evidence accounts for the effective provider, profile, settings, and invocation context. A catalog default or advertised recommendation alone is not the child's verified default. Otherwise explicitly label it delegated and unknown.
- Known readiness exclusions get short stderr diagnostics and, when useful, a compact reason summary in selection metadata. If preflight prevents execution, print an error instead of a fictitious selection line.
- Decision-mode selection values use configured/constraint-filtered candidates regardless of local availability, with the shared failure/null-reason fields added in both concise and verbose output. Verbose decision mode retains stages/chunks/probabilities/options and applicable argv details; remove the obsolete `skipped` field and explain that migration. Availability exclusions and availability reasons apply only to exec.
- If Jev fails with multiple CLIs still eligible, preserve exit 2 and do not invent a selection, but emit one decision-result JSON object with unresolved values represented as null and the failure/null reasons. Omit unbuilt argv/command details and fabricated confidence. This deliberately extends the old fatal-routing output behavior to satisfy the user's JSON-reason requirement. Usage/config/input failures and exec availability rejection retain their separate stderr contracts. Exec selection JSON remains on stderr before a child starts; never inject routing JSON into child stdout.
- Use fixed reason codes and controlled messages, not raw `%v` HTTP/probe errors that may contain secrets or prompt/doc text.

Illustrative decision JSON after a model-stage timeout with a fixed synthetic CLI (an allowed fallback, not a new selected model):

```json
{
  "cli": "tool-a",
  "model": null,
  "effort": null,
  "reason": {
    "code": "jev_timeout",
    "stage": "model",
    "message": "Jev model selection timed out"
  },
  "null_reasons": {
    "model": {"code": "routing_failed", "message": "Model delegated to the child after routing failed"},
    "effort": {"code": "routing_failed", "message": "Effort delegated to the child after routing failed"}
  }
}
```

The same reason fields appear in exec's prefixed selection JSON. Decision mode does not probe child defaults to populate these explanations; exec uses verified default evidence only where already permitted. Successful fully populated selections omit the reason fields; successful null fields retain their specific non-failure explanations.

### Ralphex integration and waiting contract

Checked the requested svtlv worktree at `/Users/svtlv/Projects/workspaces/svtlv/feture-digest-map-reduce`, whose actual branch name is `feture/digest-map-reduce` (HEAD `8df9ac9c310570e4989f74a06a5a2ef17d46d6e1`), and `/Users/svtlv/Projects/ralphex` on branch `windows` (HEAD `90f1b1b9f80c3a5cf243f1d2fbb8785cc4aa54c5`). These are local checkout observations, not claims about an upstream release.

- The svtlv `.ralphex/config` already uses `claude_command = agrouter` and `claude_args = exec claude --doc=CLAUDE.md -- --dangerously-skip-permissions --output-format stream-json --verbose`. Ralphex appends `--print` and supplies the prompt through stdin. Keep stdin intact and forward that native flag unchanged; account for it when validating the invocation descriptor rather than rewriting the passthrough tail. `task_model = :` and `review_model = :` suppress phase overrides; `codex_model =` and `codex_reasoning_effort =` explicitly clear inherited defaults, as confirmed by Ralphex's presence flags during config merge.
- Its `codex_command = agrouter exec codex --` is incompatible with Ralphex: `pkg/executor/codex.go` passes Command as one executable name through `pkg/execx/execx.go`; it does not split a shell command. Ralphex also prepends its own native `exec` argument. The user can later install a single executable adapter which consumes that first argument and invokes `agrouter exec codex --doc=CLAUDE.md --` with the remaining native arguments and inherited stdin. Agrouter supplies the native `exec` subcommand through its invocation descriptor. Agrouter must preserve sandbox, project-doc, timeout, and other non-selection `-c` arguments. Its documentation should explain keeping explicit Ralphex model/effort overrides empty so they cannot conflict with agrouter's selected values. Creating or installing this consumer wrapper is outside the implementation scope.
- The worktree's `.agrouter/config` still contains `[cli.claude.args]` and `[cli.codex.args]`. The user will later migrate these to invocation and availability descriptors, preserving routing policy and catalog customization. Agrouter supplies migration errors and documentation; this plan does not edit that external configuration. Never print or copy API keys into the plan or fixtures.
- Neither the inspected project nor global config enables `wait_on_limit`; the embedded default is also disabled. For later consumer setup, the user can set `wait_on_limit = 15m` or pass `ralphex --wait=15m` explicitly for automatic recovery. Without this opt-in Ralphex stops on a recognized limit. Changing either consumer's configuration is outside this plan.
- Claude execution merges stderr into its parsed stream. A non-JSON `agrouter:` line is surfaced and already treated as diagnostic text (`pkg/executor/executor.go`). Its configured patterns include `You've hit your limit` and `You've hit your weekly limit`. Configure `quota_error_prefix = agrouter: ` and `quota_error_message = You've hit your limit` for an exhausted subscription; append a short controlled explanation of the applicable window and any verified reset time.
- Codex execution scans stderr live and accepts limit patterns only on `ERROR:`, `FATAL:`, or `PANIC:` lines (`pkg/executor/codex.go`). Configure `quota_error_prefix = ERROR: agrouter: ` and `quota_error_message = Rate limit exceeded`. This satisfies its existing prefix gate and configured limit patterns without changing Ralphex. Validate and preserve configured prefix whitespace; reject newline/control-character injection. Codex's existing stderr display filter can hide agrouter's selection line, but its error matcher still detects the prefixed quota failure. Preserve the selection line and accept the consumer's existing display behavior.

On a known exec subscription block, print exactly one native-compatible stderr error and return 127 immediately, with no child launch, no selection line, no stdout bytes, and no Jev request. Illustrative failures:

```text
agrouter: You've hit your limit; weekly subscription quota exhausted; resets at 2026-10-13T15:00:00Z
ERROR: agrouter: Rate limit exceeded; weekly subscription quota exhausted; resets at 2026-10-13T15:00:00Z
```

The first is for the configured Claude invocation and the second for the configured Codex invocation; emit only the selected CLI's line. These are ordinary text diagnostics, not a new machine protocol or fabricated child JSON events. The reset time is informative only and is omitted when unverified. Keep normalized reason/scope/reset evidence internally for filtering and caching. Never emit a quota phrase for authentication failure, missing executables, unknown quota, metadata throttling, or routing failure; those stop or proceed under their own policies without requesting Ralphex's rate-limit wait.

The user's requirement to fit the existing consumer takes precedence over the current blanket `agrouter:` error-prefix rule. Plan a narrow documented exception for configured native-compatible exec quota failures: a leading native severity prefix may precede `agrouter:`. All other errors and the `agrouter: selection: ` line retain their existing prefixes. Prefixes/messages belong in CLI configuration, with no provider names or native wording hardcoded in Go.

Ralphex alone owns waiting and retries. Its existing `LimitPatternError` and `pkg/processor/execution_policy.go` recognize the text, sleep `--wait`/`wait_on_limit`, and retry the same prompt. Agrouter does not sleep until a reset or ask Ralphex to parse timestamps. Each retry checks fresh/cached availability for free and exits with the same recognized quota error while still blocked; once available, it routes and launches the child. A weekly block must survive a five-hour reset. No Ralphex source, parser, output-filter, or retry-policy changes are in scope.

A temporary harness run during this revision called the unchanged exported Claude/Codex executors with fake commands that wrote these lines to stderr and exited 127. Both quota messages became `LimitPatternError`; a plain Codex `agrouter:` quota line failed its prefix gate; authentication failures in both executors did not become limits. All five cases passed. This exercised actual executor classification with no inference, external source edits, or new dependency in agrouter. Ralphex's wait behavior was verified from its existing retry-policy code, not by sleeping through a real limit.

Ralphex's existing `--worktree` configuration-load ordering can retain the launch directory's `.ralphex/config` after changing directories (its `docs/backlog/worktree-ignores-its-own-ralphex-config.md`). The user should account for the launch directory when later verifying consumer setup; fixing that unrelated configuration behavior is outside this plan.

## Testing strategy

- Parsing: explicit/missing/disabled exec CLI, environment precedence, duplicate/conflicting `--cli`, removed flags, separator presence, and all native token edge cases.
- Routing: full multi-CLI decision stages without availability filtering, pinned exec CLI stage skipped, fixed model/effort, passthrough constraints, exec readiness filtering, one-option bypass, and zero-candidate guards. Assert decision mode never calls the checker or cache even with missing executables or previously cached auth/quota blocks.
- Free probes: fake SDK/JSON-RPC processes inspect every received message; assert no prompt, user event, thread/turn start, transcript scan, login, credit consumption, or Jev key.
- Quotas: five-hour-only exhaustion, weekly-only exhaustion, both exhausted with different reset times, multiple/scoped buckets, missing/null data, percentage units, extra credits, provider differences, stale snapshots, metadata 429, and protocol changes.
- Retry cache: separate invocations, changed account/config/executable, fifteen-minute retries, weekly-versus-five-hour reset ordering, early recovery, expiry, concurrent writers, corruption, and cache failures.
- Execution/output: exact argv/stdin, unchanged child streams and exit codes, Windows batch quoting, four verbosity combinations, concise chunked exec logs, stage failure fallbacks, known/unknown child defaults, and secret redaction.
- JSON reasons: both modes and verbosity levels, Jev transport/API failure, missing key, timeout/cancellation, null/missing/empty answers, each failed stage, preserved caller-fixed values, valid no-effort/default omissions, fatal multi-CLI failure JSON with exit 2, and no raw secrets/prompts/docs in explanations. Assert nulls always have specific explanations and successful omissions never masquerade as failures.
- Config extensibility: add a third synthetic CLI entirely through project-local `[cli.*.availability]`, using a JSON status helper; verify exec invokes its checks and filters its candidates without provider-specific dispatch, while decision mode retains it without invoking those checks.
- Ralphex contract: failed exec emits the configured native-compatible text prefix/phrase, optional informative reset time, empty stdout, no selection, and zero Jev calls. Use agrouter-owned synthetic fixtures reflecting the consumer patterns and severity-prefix gate verified above. Preserve the existing wait/retry contract without requiring Ralphex execution, external checkouts, or a Ralphex module dependency in required tests.
- No real inference or paid Jev calls in tests. Optional manual verification uses only the same prompt-free protocols demonstrated during planning.

## Implementation steps

### Task 1: Replace mappings with fixed invocation descriptors

**Files:**
- Modify: `pkg/config/config.go`, `pkg/config/load.go`, `pkg/config/validate.go`, `pkg/config/defaults/config`.
- Modify: `pkg/args/request.go`, `pkg/args/build.go`, `pkg/args/redact.go`, `pkg/router/eligibility.go`.
- Modify: mapping-dependent consumers and synthetic fixtures under `cmd/agrouter`, `pkg/config`, `pkg/catalog`, `pkg/args`, and `pkg/router`.

- [ ] Add and validate fixed invocation descriptors and native selection-conflict descriptors; migrate embedded config.
- [ ] Remove generic mappings, mapped eligibility preference, skipped reporting, translated request fields, and obsolete output fields; give removed config sections migration errors.
- [ ] Update argv construction/redaction and dependent fixtures atomically so the repository remains buildable.
- [ ] Add/update tests for descriptor validation, layering/migration errors, exact native forwarding, selected values, prompt safety, and mapping-free eligibility.
- [ ] Run targeted package tests and `make test`; pass before task 2.

### Task 2: Enforce the positional exec CLI and verbosity boundaries

**Files:**
- Modify: `cmd/agrouter/flags.go`, `cmd/agrouter/flags_test.go`, `cmd/agrouter/app.go`, `cmd/agrouter/app_test.go`, `pkg/args/request.go`.

- [ ] Parse `exec <cli>`, track separator presence, remove child-option flags and `-c` constraint extraction, and preserve agrouter's routing/input/output controls.
- [ ] Validate enabled exec CLI and explicit conflicts before probing/Jev; define environment and decision-mode behavior as above.
- [ ] Implement configured native selection-conflict checks and preserve remaining tokens exactly; reject decision-mode passthrough.
- [ ] Add tests for operand errors, removed flags, aliases/value syntax, flag-looking values, additional separators, native settings context, and all four verbosity combinations.
- [ ] Update existing end-to-end calls to the new contract and run `make test`; pass before task 3.

### Task 3: Add bounded generic probe execution

**Files:**
- Create: `pkg/runner/probe.go`, `pkg/runner/probe_test.go`.
- Modify: `pkg/runner/runner.go`, `pkg/runner/runner_unix.go`, `pkg/runner/runner_windows.go`, and platform tests as needed.
- Create: `pkg/readiness/readiness.go`, `pkg/readiness/readiness_test.go`, `cmd/agrouter/readiness.go`.
- Modify: config definitions/validation/tests and generated consumer mocks as needed.

- [ ] Define independent readiness dimensions, normalized evidence/scopes, the consumer interface, and layered per-CLI availability/check descriptors, including a JSON helper contract.
- [ ] Add executable resolution and isolated bounded probe I/O using existing environment filtering and platform quoting semantics.
- [ ] Implement cancellation/tree cleanup without changing the real child's process-group contract; validate allowed read-only actions.
- [ ] Add tests for missing/nonexecutable commands, timeout, response limits, secret filtering, known forbidden actions, descriptor layering, a third locally configured CLI, stdin isolation, and platform cleanup.
- [ ] Run `make generate` where needed and `make test`; pass before task 4.

### Task 4: Implement the SDK control-stream quota probe

**Files:**
- Create: `pkg/readiness/control.go`, `pkg/readiness/control_test.go`, synthetic response fixtures under `pkg/readiness/testdata`.
- Modify: `pkg/config/defaults/config` and relevant config tests.

- [ ] Implement configured initialization and quota-read control requests, correlation, unrelated-message handling, and unknown/error classification.
- [ ] Configure the verified Claude protocol, disable behavior scanning/startup customizations, preserve subscription identity, and enforce version compatibility.
- [ ] Normalize five-hour, weekly, model-scoped, and extra-usage evidence without persisting raw responses or secrets.
- [ ] Add tests for pre-prompt quota success, exhausted weekly quota with available five-hour quota, unsupported versions, missing fields, endpoint throttling, and provider/context mismatch.
- [ ] Assert no user messages or inference actions and run `make test`; pass before task 5.

### Task 5: Implement the JSON-RPC account and quota probe

**Files:**
- Create: `pkg/readiness/rpc.go`, `pkg/readiness/rpc_test.go`, synthetic fixtures under `pkg/readiness/testdata`.
- Modify: `pkg/config/defaults/config` and relevant config tests.

- [ ] Implement configured JSON-RPC initialization, read requests, correlation, notifications, pagination, and bounded shutdown.
- [ ] Configure Codex account/quota reads and capability/default reads where relevant; normalize multiple quota buckets, duration/reset information, and explicit backend usage-permission/reached-limit/spend-control evidence.
- [ ] Preserve non-subscription provider behavior and distinguish explicit denials from incomplete advertisements or metadata failures.
- [ ] Add tests for both windows, scoped buckets, credits, explicit denial despite low percentages, missing/null permission, auth refresh failure, API-key/custom provider, hidden/paginated models, RPC errors, and unknown defaults.
- [ ] Assert no thread/turn or mutating actions and run `make test`; pass before task 6.

### Task 6: Evaluate quota evidence against eligible combinations

**Files:**
- Create: `pkg/readiness/evaluate.go`, `pkg/readiness/evaluate_test.go`.
- Modify: `pkg/router/eligibility.go`, `pkg/router/eligibility_test.go`, `pkg/router/router.go`, `pkg/router/router_test.go`.

- [ ] Implement independent quota/capability evaluation, configured scope matching, unknown policy, and effective overage handling.
- [ ] Remove known blocked combinations without broadening constraints; compute recovery from all exhausted applicable windows.
- [ ] Guard router entry/cannot-decide against empty eligibility and retain filtered eligibility for routing failure policy.
- [ ] Add tests for five-hour/weekly/both exhaustion, scoped alternatives within one CLI, fixed/out-of-catalog constraints, unknown policy, stale or malformed data, and all candidates unavailable.
- [ ] Run `make test`; pass before task 7.

### Task 7: Persist evidence across retries and invalidate it safely

**Files:**
- Create: `pkg/readiness/cache.go`, `pkg/readiness/cache_test.go`.
- Modify: readiness orchestration, config definitions/validation/defaults/tests, and `cmd/agrouter/flags.go`/tests.

- [ ] Implement private atomic cache storage with injected clock and opaque context fingerprints; add exec-only `--refresh-readiness` and test decision-mode rejection.
- [ ] Apply success freshness, reset-aware negative expiry, free rechecks, bounded unknown-reset backoff, and independent probe-throttle backoff.
- [ ] Invalidate on effective identity/config/provider/native context or executable changes; degrade gracefully on storage failure.
- [ ] Add separate-invocation tests for fifteen-minute retries, weekly reset after five-hour reset, early recovery, stale evidence, bypass, corruption, permissions, and concurrent writers.
- [ ] Run `make test`; pass before task 8.

### Task 8: Filter availability before every exec Jev request

**Files:**
- Modify: `cmd/agrouter/app.go`, `cmd/agrouter/readiness.go`, `cmd/agrouter/debug.go`, `cmd/agrouter/app_test.go`, `cmd/agrouter/debug_test.go`, consumer mocks.

- [ ] Wire the checker only in exec, after input/config/constraint validation and before `Router.Route`, under the existing total routing deadline.
- [ ] Check only the required positional CLI in exec and use its filtered options for every stage; bypass all availability probes/cache/filtering in decision/help/version mode.
- [ ] Return 127 for known unavailable exec targets and exit 2 for strict unknown blocks or input/config errors; include redacted reasons/reset times and preserve existing decision-mode exit behavior.
- [ ] Add end-to-end tests for exec total blocks, fixed constraints, single-candidate bypass, zero complexity/selection Jev calls on total blocks, no cross-CLI switch, scoped alternatives, deadline behavior, and unchanged stdin replay. Assert decision mode retains configured candidates when one/all local CLIs are unavailable, performs no checker/cache calls, and ignores cached quota/auth blocks even with --doc.
- [ ] Regenerate mocks if needed and run `make test`; pass before task 9.

### Task 9: Remember recognized failures from real child execution

**Files:**
- Create: `pkg/readiness/observe.go`, `pkg/readiness/observe_test.go`.
- Modify: `cmd/agrouter/app.go`, `cmd/agrouter/app_test.go`, runner files/tests only if required for stream-preserving observation.
- Modify: failure descriptors in `pkg/config/defaults/config` and relevant config tests.

- [ ] Add bounded format-aware observation that forwards child bytes unchanged and records only recognized terminal failure facts.
- [ ] Update account/model-scoped blocks with trustworthy reset times or bounded retry expiry; leave generic failures unclassified.
- [ ] Preserve child exit/signal codes and terminal behavior; skip observation where it would change stream semantics.
- [ ] Add tests for actual quota/auth failures, fractional child-event versus percentage control-response utilization, both reset encodings, partial/interleaved records, large output, misleading generated text, cache reuse on the next invocation, and unchanged stdout/stderr/stdin/exit status.
- [ ] Run `make test`; pass before task 10.

### Task 10: Explain failed/null selections in JSON and emit compatible exec failures

**Files:**
- Modify: `cmd/agrouter/app.go`, `cmd/agrouter/confidence.go`, `cmd/agrouter/debug.go`, `cmd/agrouter/app_test.go`, `cmd/agrouter/confidence_test.go`, `cmd/agrouter/debug_test.go`.
- Modify: `pkg/router/router.go`, `pkg/router/stage.go`, their tests, and typed failure evidence as needed; preserve underlying error identity and stage/cause provenance.
- Modify: readiness default-resolution evidence/tests as necessary.
- Modify: per-CLI availability diagnostic descriptors/defaults/config tests; create synthetic diagnostic fixtures under `cmd/agrouter/testdata` as needed.

- [ ] Separate exec formatting from verbose decision formatting and retain one prefixed selection line with only concise summaries.
- [ ] Add shared `reason` and `null_reasons` fields to concise/verbose decision JSON and exec selection JSON; preserve structured cause/stage evidence, including fatal multi-CLI routing failure JSON with exit 2 and no fabricated selection.
- [ ] Add exec fallback-action metadata and effective model/effort provenance, including delegated unknown defaults and known exclusions; distinguish valid null omissions from routing failures.
- [ ] Use verified runtime defaults only when execution context is fully represented; preserve the existing cannot-decide selection policy.
- [ ] Emit the selected CLI's configured native-compatible quota prefix/message with optional verified reset details; keep non-limit reasons distinct, stderr one-line, stdout untouched, and preflight rejection immediate.
- [ ] Add tests in both modes and verbosity levels for success, missing Jev key, timeout/cancellation, API/transport failure, null/missing/empty answers, each failing stage, intentional omissions/no-effort models, preserved fixed values, known/unknown defaults, chunked routing, fatal multi-CLI JSON/exit behavior, and secret/prompt/doc redaction.
- [ ] Add failure-contract tests for both native pattern families and the severity-prefix gate using synthetic config, prefix whitespace/control-character validation, no selection on preflight blocks, latest applicable reset, absent reset, and authentication/probe-throttle/unknown errors that cannot request a limit wait.
- [ ] Run `make test`; pass before task 11.

### Task 11: Verify combined acceptance criteria

**Files:**
- Modify: integration tests and synthetic fixtures under `cmd/agrouter`, `pkg/readiness`, `pkg/router`, and `pkg/runner` as needed.

- [ ] Verify every source backlog requirement plus the user's required exec CLI/config-extensibility/Ralphex compatibility/shared JSON-reason requirements; add missing cross-feature regressions, including separate exec retries that never call Jev while blocked and decision-mode independence from availability.
- [ ] Confirm every changed file and required check belongs to agrouter; external wrapper installation, config migration, and consumer execution remain user-owned follow-up and are not acceptance gates.
- [ ] Run `make test`, check 80%+ per-package coverage excluding mocks, and run `make build`.
- [ ] Run `make lint` with golangci-lint v2.14.0; repeat for `GOOS=linux`, `GOOS=darwin`, and `--build-tags=eval`.
- [ ] Compile Windows paths locally and rely on Windows CI for OS-specific execution tests; check guard tests and generated mocks.
- [ ] Confirm no validation invokes real inference or `make eval-routing`, and all checks pass before task 12.

### Task 12: Document migration and close the implemented backlog

**Files:**
- Modify: `README.md`, `AGENTS.md`, `CLAUDE.md`, and embedded config comments.
- Delete upon implementation: the three source files in `docs/backlog/`.
- Move upon completion: this plan into `docs/plans/completed/`.

- [ ] Document required exec CLI syntax, decision constraints with no availability checks/cache/filtering, shared failure/null reason fields and fatal-routing JSON, removed mappings/flags/fields, native conflicts, separate verbosity, per-CLI exec availability/unknown policy, a configuration-only third CLI, probe/version limitations, a complete stdin-based control-probe example, cache recovery, and exit/output contracts.
- [ ] Update agrouter's documentation with Ralphex usage examples: explicit exec CLI/native flags, stdin handling, a reference single-executable Codex adapter, explicit child verbosity, `--wait=15m`, and native-compatible quota text. Label consumer configuration migration and wrapper installation as user-owned follow-up; do not create the wrapper or edit files in svtlv or Ralphex.
- [ ] Update repository guidance to match the new layout and remove obsolete mapping/skipped rules; preserve config-owned names/stdout/redaction rules and document the narrow configured native severity-prefix exception for exec quota errors.
- [ ] Verify help/examples against parser tests and rerun applicable checks if documentation exposes a behavior change.
- [ ] Remove each backlog item in the commit landing its complete implementation; move the completed plan only after all acceptance checks pass. Do not commit or push without user authorization.

## Post-completion

These notes are informational and outside the agrouter implementation scope. The user owns any later svtlv/Ralphex setup and consumer verification; none is required to complete this plan.

- For later svtlv `feture/digest-map-reduce` setup, the user can migrate `.agrouter/config`, retain the compatible Claude invocation and empty phase overrides, install the executable Codex adapter below, and point `codex_command` to its single executable path (for example, an absolute path to `scripts/agrouter-codex`). Keep `codex_model =` and `codex_reasoning_effort =` explicitly empty. The existing Ralphex CLI option `--wait=15m` or consuming-project `wait_on_limit = 15m` enables its wait policy. This plan performs none of those changes.
- The user can later verify compatibility using the unchanged Ralphex executors and fake commands. The expected flow is blocked preflight → recognized limit → configured wait → original-prompt retry → free availability check. Eventual recovery can be exercised with fake probe/Jev/child services without paid requests. Effective config depends on Ralphex's launch directory when using worktree mode.
- Optional prompt-free smoke checks on supported CLI versions should verify quota windows/reset handling and continued protocol compatibility. Record only normalized metadata.
- Review Claude's experimental usage protocol on upgrades. If it breaks, report unknown or stop under strict policy; never substitute a hidden inference probe or silently downgrade to auth-only readiness.

Reference POSIX adapter for agrouter's documentation. The user may later create it as `scripts/agrouter-codex` in svtlv, make it executable, and point `codex_command` to that single file. This is a documented template, not an implementation task in the consuming project; equivalent argument-preserving Windows handling would be consumer setup as needed:

```sh
#!/bin/sh
if [ "$1" != exec ]; then
    printf '%s\n' 'agrouter: expected native exec subcommand' >&2
    exit 2
fi
shift
exec agrouter exec codex --doc=CLAUDE.md -- "$@"
```
