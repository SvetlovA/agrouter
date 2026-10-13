---
worth: later
where: pkg/runner/runner.go:34
added: 2026-10-13
---
# Record auth/quota failures from real child execution

Execution can still fail after a preflight check passes. Recording recognized
machine-readable auth/rate-limit errors from the real child would let the next
invocation skip Jev sooner. Deferred from the
[exec readiness plan](../plans/20261012-exec-native-arguments-and-subscription-readiness.md).

The runner hands agrouter's own stdout/stderr files to the child. Observing output
needs pipes and copy goroutines, which change TTY detection and buffering and
interact with `WaitDelay`: a grandchild holding the pipes can truncate output after
the child exits. Observed failures only pay off with a persistent store, see
[readiness-cache-across-invocations](readiness-cache-across-invocations.md).

Settles the value question: that cache being built, and a transport (such as an
already non-TTY stream-json output) where observation does not change stream
semantics.

Findings to keep: match error records only in the configured output format; plain
generated text mentioning a limit must never create a block. Units differ by source:
the child's `rate_limit_event.rate_limit_info.unifiedWindows.*.utilization` is a
fraction (1 means exhausted) with Unix-second resets, while control-request
`get_usage` utilization is a percentage with ISO timestamps. Never reuse one decoder
for the other. Forward bytes unchanged and preserve exit and signal codes.
