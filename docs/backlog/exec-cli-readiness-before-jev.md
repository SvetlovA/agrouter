---
worth: yes
where: cmd/agrouter/app.go:240
added: 2026-10-09
---
# Check exec CLI readiness before spending on Jev routing

Before any Jev request in exec mode, check whether eligible CLI/model/effort
combinations can receive a prompt: the executable is available, authentication
is usable, the subscription is not currently rate limited, the combination is
supported, and no other detectable error prevents execution. Today executable
lookup happens in `pkg/runner/runner.go` after routing; authentication and quota
errors are left to the child.

Ralphex may retry every 15 minutes. Repeatedly paying Jev to select a CLI that is
still unauthenticated or rate limited wastes money. Prefer documented status,
authentication, quota or capability checks that send no inference prompt and
consume no model tokens. Do not add a recurring test prompt as a hidden probe.

First investigate the supported CLIs' current documented commands/APIs and
record exactly what each free check proves. Executable presence or a logged-in
status alone does not establish model access or remaining subscription quota.
Represent unsupported checks or inconclusive results as unknown, and define an
explicit policy for unknown readiness before implementation; do not claim a
guarantee that execution will succeed. Never expose credentials in diagnostics.

Requirements and design decisions:

- Run readiness checks before both project-complexity and selection requests;
  skip checks in decision mode. Check each relevant CLI once where possible and
  apply model/effort-specific evidence to its eligible combinations.
- Exclude combinations known to be unavailable before routing. Preserve fixed
  CLI/model/effort constraints rather than silently replacing them. When no
  executable candidate remains, report the reason and make zero Jev requests.
- Define zero-candidate and unknown-readiness behavior, bounded probe timeouts,
  and exit codes consistent with the existing documented CLI contract.
- Investigate a cache that survives separate agrouter invocations, including
  subscription reset/retry-after times, expiry and invalidation after login,
  account/configuration changes or CLI updates. Document how Ralphex's 15-minute
  retries avoid repeated paid routing while a known block persists and recover
  when availability returns. A cache must not keep a recovered CLI blocked
  indefinitely or treat stale success as guaranteed readiness.
- Execution can still fail after a check. Investigate recording recognized
  authentication/rate-limit failures from actual child execution for subsequent
  retries without swallowing output or changing child exit-code propagation.
- Keep per-CLI commands and mappings in configuration and names in the catalog;
  keep generic orchestration in code. Establish the trust boundary for any new
  configured probe commands. Do not read secrets into output or cache entries.

Validate with synthetic CLIs/status responses and mocked Jev: missing executable,
authentication failure, subscription limits, unsupported model/effort, unknown
status, timeout, alternative ready candidate, fixed constraints, all unavailable,
cache reuse across invocations, expiry/reset and recovery. Assert zero Jev calls
for a known all-unavailable state, unchanged stdin replay/child output, and no
inference prompts used by readiness probes.

Use [fallback explanations](exec-fallback-target-and-reason.md) to make any
readiness-driven exclusion or fallback understandable in exec diagnostics.
