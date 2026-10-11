---
worth: yes
where: cmd/agrouter/flags.go:68
added: 2026-10-11
---
# Require an exec CLI, remove mappings and separate verbosity

Use two invocation modes:

```sh
agrouter [routing options] [prompt]                         # decide; print JSON
agrouter exec <cli> [routing options] [prompt] [-- args...] # route within this CLI, then run it
```

Exec requires an explicit positional CLI, for example `agrouter exec claude`
or `agrouter exec codex`. Native arguments cannot safely be applied to an
arbitrary CLI chosen by routing.

Invocation and passthrough requirements:

- Before `--`, accept only arguments needed by agrouter itself: mode and CLI
  selection, routing constraints, prompt sources, routing documents, Jev
  configuration and agrouter output controls. All child CLI options belong
  after `--`, even when agrouter previously recognized or translated them.
  Child verbosity, output format, permissions, sandbox settings and native
  configuration flags must not be consumed as agrouter options.
- Keep `agrouter` as decision mode, selecting across the full enabled
  CLI/model/effort catalog and printing the decision JSON. Decision mode does
  not require a CLI; explicit routing constraints may narrow the candidate set.
  Keep all three routing stages (CLI, model, effort), skipping only stages with
  one candidate as usual. In exec mode, require a configured, enabled `<cli>`;
  a missing, unknown or disabled CLI is a usage error (exit 2) before any Jev request.
  `--cli` or `AGROUTER_CLI` must not substitute for the required exec operand;
  document their decision-mode behavior and resolve conflicting constraints.
- Fix the exec CLI before routing, skip the CLI stage, and select model/effort
  only from that CLI's catalog. Never fall back to another CLI.
- Consume the first `--` separator and forward every following token unchanged
  to the named CLI. Preserve order, unknown flags, empty values, spaces and
  further `--` tokens. Do not parse these tokens as agrouter options, routing
  constraints or prompt text, validate them against the catalog, or skip them
  with an unmapped-argument warning.
- Native arguments require exec mode with a known CLI. Reject passthrough in
  decision mode rather than choosing a CLI based on native flags or dropping
  those flags.

Remove argument mappings:

- Remove all `[cli.*.args]` mappings and the generic mapping/template machinery,
  mapping-based eligibility filters, skipped-argument reporting and agrouter
  flags that translate child options. Callers provide native options after `--`.
- Retain agrouter's own model/effort constraints and output controls before `--`;
  distinguish them in help and examples from native child options after `--`.
  Agrouter's `--verbose` controls verbose decision output, while a child's
  `--verbose` must be supplied after the separator. Exec logging stays concise
  as specified below.
- Define how exec supplies its selected model/effort, non-interactive invocation
  and prompt after removing mappings. Execution must still apply the routing
  decision and preserve prompt sources and stdin replay. Keep CLI-specific
  invocation settings and all CLI/model/effort names in configuration; do not
  replace the mapping table with hardcoded CLI switches in Go.
- Specify precedence or conflict handling when native arguments also set a
  model/effort, so the logged decision agrees with what the child executes.
- Update configuration loading/validation, help, README, decision JSON and
  Ralphex integration examples for the new contract. Preserve redaction,
  routing-only `--doc`, child output and exit-code propagation.

Separate verbosity and keep exec selection logs concise:

- `--verbose` before `--` belongs only to agrouter. `--verbose` after `--`
  belongs only to the child. Either can be supplied independently, or both
  together; child verbosity must not enable verbose routing diagnostics.
- Callers supply any verbosity required by native output formats explicitly.
  For example, `agrouter exec claude -p "task" -- --output-format stream-json
  --verbose` requests Claude's verbose stream output without enabling agrouter
  verbosity. Remove the automatic child `--verbose` injection from the old
  output-format mapping, and update Ralphex examples accordingly.
- Always emit the concise selection form in exec mode, including when agrouter's
  own `--verbose` is set: CLI, model, effort, completed project complexity and
  stage confidence summaries. Preserve the single `agrouter: selection: ` line
  on stderr and exclude prompt, documents, argv and command from it.
- Keep stage records, chunk details, candidate probabilities and eligible
  options in verbose decision output. Update help and README to explain both
  verbosity scopes and the concise exec behavior.
- Compose with [fallback explanations](exec-fallback-target-and-reason.md):
  concise selection output must still explain a fallback.

Validate with synthetic catalogs and fake child processes: decision routing
across the full CLI/model/effort set and explicit constraint filtering, missing
and invalid exec CLI operands, skipped CLI routing, model/effort constraints,
exact native-token forwarding, decision-mode passthrough rejection, conflicting
native selection flags, prompt/stdin delivery and redacted diagnostics. Cover
all four combinations of agrouter and child verbosity, concise exec output
including chunked routing, unchanged child output, and full details in verbose
decision mode. Do not pin shipped model names or counts.
