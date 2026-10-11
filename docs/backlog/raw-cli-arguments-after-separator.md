---
worth: yes
where: cmd/agrouter/flags.go:68
added: 2026-10-11
---
# Require an exec CLI, remove argument mappings and add OpenCode

Use two invocation modes:

```sh
agrouter [routing options] [prompt]                         # decide; print JSON
agrouter exec <cli> [routing options] [prompt] [-- args...] # route within this CLI, then run it
```

Exec requires an explicit positional CLI, for example `agrouter exec claude`,
`agrouter exec codex` or `agrouter exec opencode`. Native arguments cannot safely
be applied to an arbitrary CLI chosen by routing. This replaces the earlier
proposal to forward raw arguments even when the caller has not fixed the CLI.

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
  one candidate as usual. In exec mode, require a configured, enabled `<cli>`; a missing,
  unknown or disabled CLI is a usage error (exit 2) before any Jev request.
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
  For example, agrouter's `--verbose` controls routing details, while a child's
  `--verbose` must be supplied after the separator.
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

OpenCode configuration:

- Add an enabled, config-defined OpenCode CLI with all models currently offered
  through the Codex and Claude catalogs, not a separate reduced model list.
- Support shared model identities across CLIs without ambiguous lookup or alias
  collisions. Keep descriptions and policy data consistent across providers.
- Verify OpenCode's documented provider-qualified model IDs, invocation syntax
  and model-specific effort support during implementation; represent them in
  configuration without assuming another CLI's spelling works unchanged.

Validate with synthetic catalogs and fake child processes: decision routing
across the full CLI/model/effort set and explicit constraint filtering, missing
and invalid exec CLI operands, skipped CLI routing, model/effort constraints,
exact native-token forwarding, decision-mode passthrough rejection, conflicting
native selection flags, prompt/stdin delivery and redacted diagnostics. Cover
shared models across CLIs without pinning shipped model names or counts.
