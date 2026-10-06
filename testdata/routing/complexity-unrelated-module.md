# ledger-core

This document covers only `ledger-core`, the settlement engine of the monorepo.

- A double-entry ledger handling 40,000 transactions per second across 12 currencies.
- Event-sourced on a custom log with exactly-once processing; replays must be bit-for-bit deterministic.
- Multi-region active-active with conflict resolution; consistency proofs run nightly.
- Every change needs a design review, a formal model update (TLA+) and sign-off from two owners.
- 2,500 property-based tests; a full replay of production history runs before each release.

Other tools in the monorepo (scripts under `tools/`, the docs site) are small and owned by their
authors; they are not described here.
