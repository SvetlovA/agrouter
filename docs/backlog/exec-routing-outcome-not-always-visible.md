---
worth: yes
where: cmd/agrouter/app.go:185
added: 2026-10-15
---
# Routing outcome is not visible in every mode

Whatever agrouter decides, the user should be able to see it, in every mode and also when agrouter runs inside another tool. That means the selection JSON it actually used, or a clear line saying routing was skipped. Today there are two gaps.

## 1. The selection line is lost when a caller wraps agrouter

`logSelection` writes `agrouter: selection: {...}` to stderr only. That works in a terminal, and it works for ralphex's Claude executor, which merges stderr into its output. It is lost wherever the caller drops or separates the child's stderr.

Observed on 2026-10-20 at 15:26 (Asia/Jerusalem), in the Lampa `feature/frontend-backend-sync` ralphex run (`executor = codex`, `codex_command = .ralphex/scripts/codex-agrouter.sh`, plan `20261020-user-data-sync`). agrouter started `codex exec ... --model gpt-6.1-sol -c model_reasoning_effort="high"`, and the installed binary contains `selectionPrefix`. Still, `.ralphex/progress/progress-20261020-user-data-sync.txt` has no `agrouter:` line. The only way to tell that agrouter, not ralphex, picked the model and effort was `ps` on the live process tree. The `model:` and `reasoning effort:` lines in the progress log come from codex's own header, and they would read the same if ralphex had passed the values itself.

agrouter cannot control what its parent does with stderr. So the decision JSON in use needs a durable channel the caller does not mediate, for example an append-only JSONL decision log. It would have one record per exec or decision call: timestamp, cwd, requested CLI, mode, the selection JSON, and the outcome (`ran` / `blocked` / `no_decision`). It would live at a default state path, with a config key or env var to change or disable it. Keep the stderr line as it is. The open questions are the path, rotation, and whether the log is on by default.

## 2. A CLI preflight block exits before Jev and leaves no routing line

When exec readiness finds no runnable options, agrouter exits before asking Jev. It reports only the CLI availability or quota error. From the output, users cannot tell that routing was skipped and the child CLI never started. Ralphex labels the quota message as a limit "in claude output", which blurs the difference further.

Observed in the Svetlov digest map-reduce Ralphex session on 2026-10-15. Claude hit its session limit at 11:09:39 (Asia/Jerusalem). At 11:24:41 and 11:39:43, agrouter's preflight printed `agrouter: You've hit your limit; five-hour subscription quota exhausted; resets at 2026-10-15T09:00:00Z`, and Ralphex waited another 15 minutes. Neither blocked retry called Jev or started Claude.

The early return in `app.run` (`cmd/agrouter/app.go:148`) prints only `preflightError.line` and its exit code. `applyPreflight` in `cmd/agrouter/readiness.go` returns that error before `rt.Route`, so the selection report is bypassed.

Add a short, non-debug stderr explanation. It should name the requested CLI and the blocking reason, and say that Jev was not called because readiness blocked execution. For example: `agrouter: routing skipped: claude subscription quota exhausted; Jev was not called; child CLI was not started`. Cover the other pre-routing readiness blocks too: missing executable, failed authentication, and strict unknown availability. Keep this distinct from a Jev routing failure or a fallback selection. Write the same outcome to the decision log from part 1.

## Constraints for both

- Keep the configured quota error prefix and message, the reset information, and the exit codes, so callers' limit matching and retry behavior keep working.
- Emit the explanation before any quota line that can make a caller stop reading output.
- Keep stdout unchanged. Keep the stderr line prefixed so stream-json callers do not parse it as an event.
- Never log prompts, argv, or credentials.
- Only claim Jev was not called on the pre-routing block path. A later default-readiness block can happen after routing.

Tests: extend `TestApp_ExecBlockedAcrossInvocations` and the readiness block cases in `cmd/agrouter/readiness_test.go`. They should check the explanation, zero Jev requests, no child execution, and that quota matching still works across retries. Add cases that check a decision-log record for ran, blocked, and no-decision exec calls and for decision mode, while stderr stays unchanged.
