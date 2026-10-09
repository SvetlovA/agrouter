---
worth: yes
where: cmd/agrouter/app.go:287
added: 2026-10-09
---
# Explain exec fallback targets and reasons

In exec mode, show which CLI, model and effort will actually receive the prompt
when routing falls back to defaults, and explain why. A `null` value alone is not
enough to understand a Ralphex step.

`router.Decision.Undecided` already carries the routing failure cause. The current
cannot-decide policy retains the known CLI and caller-fixed model/effort, leaving
unspecified model/effort to the child's defaults. Exec currently has a concrete
CLI; when several CLIs remain and Jev cannot decide, it exits instead of choosing
a default CLI. Preserve that policy and caller constraints.

Requirements:

- Add exec-only fallback information to the prefixed stderr selection output,
  available without `--verbose`, identifying the effective CLI/model/effort and
  the reason defaults were used (such as a missing Jev key, timeout or API error).
- Distinguish a routing failure from a model or effort intentionally omitted so
  that the child's normal default applies.
- Investigate how to resolve the child's effective model/effort without sending
  a prompt. Defaults can depend on child configuration and account state; never
  present a catalog entry as the child's verified default. If a default cannot
  be resolved, explicitly report that it is delegated to the child and unknown,
  with the reason, rather than silently showing only `null`.
- Keep decision-mode output behavior unchanged. Redact secrets and prompt/doc
  contents from reasons, and preserve stdout for the child's output.

Cover successful routing, failure before routing, failure at each stage,
preserved caller constraints, known and unresolved child defaults, and failure
with multiple CLIs. Coordinate with
[concise exec logs](concise-exec-selection-log.md) and
[CLI readiness checks](exec-cli-readiness-before-jev.md).
