---
worth: yes
where: cmd/agrouter/app.go:134
added: 2026-10-09
---
# Keep exec selection logs concise with --verbose

Deferred at the user's request after reverting the uncommitted implementation in
`README.md`, `cmd/agrouter/app.go`, `cmd/agrouter/app_test.go` and
`cmd/agrouter/flags.go`. Exec currently includes full routing details in its
selection line when `--verbose` is set, which makes Ralphex output noisy.

Always emit the concise selection form in exec mode, including with `--verbose`:
CLI, model, effort, completed project complexity and stage confidence summaries.
Keep stage records, chunk details, candidate probabilities and eligible options
in verbose decision output. Preserve the single `agrouter: selection: ` line on
stderr and exclude the prompt, documents, argv and command from it.

The reverted implementation passed `false` instead of `cmd.verbose` to
`selectionOutput` in the exec branch. Reapply that behavior with matching help,
README and behavioral test updates. Agrouter's `--verbose` must remain separate
from verbosity required by the child's configured output-format mapping.

Validate concise exec output both with and without `--verbose`, including chunked
routing, and retain full detail in verbose decision mode. This item should compose
with [fallback explanations](exec-fallback-target-and-reason.md): concise output
must still explain a fallback.
