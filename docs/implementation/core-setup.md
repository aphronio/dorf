# Core setup implementation

Operator installation and diagnosis live in [Getting started](../getting-started.md) and
[Support](../support.md). This page maps setup behavior to its code authorities.

- Host package and service convergence: [`internal/hostsetup`](../../internal/hostsetup)
- PostgreSQL and durable-runtime initialization: [`cmd/dorf/main.go`](../../cmd/dorf/main.go) and
  [`internal/postgres`](../../internal/postgres)
- Readiness observations and remediation: [`internal/doctor`](../../internal/doctor)
- Sandbox-profile storage and verification: [`internal/postgres`](../../internal/postgres),
  [`internal/profile`](../../internal/profile), and the composition root in [`cmd/dorf`](../../cmd/dorf)
- Provider installation and connection: [`internal/gateway`](../../internal/gateway) and the
  [Provider Gateway boundary](../project/provider-gateway.md)
- Sandbox artifact construction and qualification: [`internal/release`](../../internal/release), the
  [Incus image authority map](incus-image.md), and the [E2B template authority map](e2b-template.md)

Setup must remain convergent and observational: rerunning it rechecks current authorities and only
performs idempotent initialization. It must not commandeer operator-owned Incus configuration,
expose credentials, or infer external proof from unit tests. Explicit profile verification retains
its own typed functional-proof and cleanup receipt; ordinary doctor observations are not persisted.

PostgreSQL owns named profiles and the one verified default. Each profile binds an exact Incus image
fingerprint or E2B template build, Codex or Pi, and provider-specific settings. Only `E2B_API_KEY`
remains host-only environment configuration; profile creation records the exact deployment-owned
HTTPS `/v1` Gateway URL, timeout, and explicit internet-egress choice. Jobs durably retain the
selected name, while Workers resolve its adapter and Harness through the common runtime seam.
