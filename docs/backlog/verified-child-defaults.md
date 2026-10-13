---
worth: later
added: 2026-10-13
---
# Resolve the child's effective default model/effort without a prompt

When routing falls back, exec delegates unset model/effort to the child and reports
`child_default_unknown` (see the
[exec readiness plan](../plans/20261012-exec-native-arguments-and-subscription-readiness.md)).
A verified `verified_child_default` source was dropped from that plan.

Settles the value question: a prompt-free source that accounts for the child's
effective provider, profile, settings files and invocation context. Today's options
don't: a catalog default or an advertised recommendation (for example an app-server
model list's default flag) is not the child's verified default, and project/user
settings can override it.

If such evidence appears, a delegated model could also be checked against
model-scoped quota blocks instead of being treated as unknown.
