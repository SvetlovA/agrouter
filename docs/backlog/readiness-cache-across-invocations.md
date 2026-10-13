---
worth: later
added: 2026-10-13
---
# Cache exec readiness evidence across invocations

The [exec readiness plan](../plans/20261012-exec-native-arguments-and-subscription-readiness.md)
runs prompt-free availability probes fresh on every exec invocation and keeps no
persistent cache. Ralphex retries every fifteen minutes and a cached success is only
trustworthy for about a minute, so a cache adds nothing for the main scenario; zero
Jev calls while blocked already holds with a fresh probe.

Settles the value question: measured probe latency (recorded in plan Tasks 5 and 6)
or metadata endpoint throttling (`probe_throttled` unknowns) becoming frequent enough
that repeated probing costs more than a cache's complexity.

If built, the plan's earlier design is the starting point: OS user cache directory,
opaque fingerprints of executable version, probe descriptor, account/provider/config
and native context; success at most one minute; a known block kept until its latest
applicable reset but refreshed on each retry; without a trustworthy reset expire
after thirty minutes, capped at sixty; separate throttle backoff honoring retry-after;
user-only permissions, atomic replacement, locking, rejection of malformed or
future-dated records, and degrading to a fresh check on I/O failure. An exec-only
`--refresh-readiness` bypass belongs with it.
