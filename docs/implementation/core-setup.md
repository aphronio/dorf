# Core setup implementation

Operator installation and diagnosis live in [Getting started](../getting-started.md) and
[Support](../support.md). This page maps setup behavior to its code authorities.

- Host package and service convergence: [`internal/hostsetup`](../../internal/hostsetup)
- PostgreSQL and durable-runtime initialization: [`cmd/dorf/main.go`](../../cmd/dorf/main.go) and
  [`internal/postgres`](../../internal/postgres)
- Readiness observations and remediation: [`internal/doctor`](../../internal/doctor)
- Provider installation and connection: [`internal/gateway`](../../internal/gateway) and the
  [Provider Gateway boundary](../project/provider-gateway.md)
- Image installation and verification: [`internal/release`](../../internal/release) and the
  [Incus image authority map](incus-image.md)

Setup must remain convergent and observational: rerunning it rechecks current authorities and only
performs idempotent initialization. It must not commandeer operator-owned Incus configuration,
persist derived readiness flags, expose credentials, or infer external proof from unit tests.
