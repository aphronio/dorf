# Core setup implementation

Operator installation and diagnosis live in [Getting started](../getting-started.md) and
[Support](../support.md). This page maps setup behavior to its code authorities.

- Host package and service convergence: [`internal/hostsetup`](../../internal/hostsetup)
- PostgreSQL and durable-runtime initialization: [`cmd/dorf/main.go`](../../cmd/dorf/main.go) and
  [`internal/postgres`](../../internal/postgres)
- Readiness observations and remediation: [`internal/doctor`](../../internal/doctor)
- Sandbox-profile selection and validation: [`internal/config`](../../internal/config) and the
  composition root in [`cmd/dorf`](../../cmd/dorf)
- Provider installation and connection: [`internal/gateway`](../../internal/gateway) and the
  [Provider Gateway boundary](../project/provider-gateway.md)
- Image installation and verification: [`internal/release`](../../internal/release) and the
  [Incus image authority map](incus-image.md)

Setup must remain convergent and observational: rerunning it rechecks current authorities and only
performs idempotent initialization. It must not commandeer operator-owned Incus configuration,
persist derived readiness flags, expose credentials, or infer external proof from unit tests.

Incus remains the default profile. The incremental E2B proof profile is selected with
`DORF_SANDBOX_PROFILE=e2b` and currently admits Codex only. It requires the host-only `E2B_API_KEY`,
an exact `DORF_E2B_TEMPLATE`, and an exact deployment-owned HTTPS `/v1` URL in
`DORF_E2B_PROVIDER_GATEWAY_URL`. `DORF_E2B_ALLOW_INTERNET=true` is an explicit broader egress policy,
not an implicit adapter default. Jobs durably retain the selected profile so a differently
configured worker cannot recover or clean them through the wrong provider authority.
