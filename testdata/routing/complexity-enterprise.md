# Payments platform

## Architecture
- 42 Go services in one monorepo (about 1.8 million lines), deployed to 6 regions on Kubernetes.
- Services talk over gRPC and Kafka; 30+ outbound integrations (card networks, banks, fraud vendors),
  each with its own timeout, idempotency and retry contract written into partner agreements.
- Shared libraries in `platform/` are imported by every service; a change there ships everywhere.

## Constraints
- PCI DSS scope: every outbound call path is audited, and retry behavior is part of the audit.
- Retries must stay idempotent: a duplicate capture or refund is a financial incident.
- Public APIs and Kafka schemas are versioned; breaking changes need a deprecation window.
- 9,000+ tests, contract tests per partner, and a canary rollout per region; a red build blocks 300 engineers.

## Conventions
- Errors wrapped with context; structured logs; no global state.
