# D081: Dorf is a deployed control plane; workflows are optional Core consumers

- **Applicability:** historical
- **Areas:** core, deployment, client-api
- **Read when:** Reviewing the former deployed control-plane and optional workflow-consumer boundary.
- **Decision history:** Superseded by D088 — 2026-08-21
- **Decision:** The [North Star product boundary](../north-star.md#product-boundary) is the authority.
  A self-hosted Dorf deployment exposes one application contract. Native workflows consume it
  in-process and add policy; external clients consume it through supported transports and may drive
  bounded execution directly without selecting a predefined workflow. A future language SDK is a
  thin client for a running deployment, not an embeddable Dorf runtime.
- **Why:** Dorf coordinates durable PostgreSQL and Absurd state, workers, Provider Gateway routes,
  Sandbox providers, Harness processes, recovery, and cleanup. Hiding those authorities behind an
  imported runtime library would create misleading lifecycle and deployment semantics. Requiring a
  predefined workflow for every caller would instead make optional policy the only door into Core.
- **Current boundary:** Native workflows already consume the in-process Core machinery. The CLI is
  the supported external client surface. A direct-execution resource contract, network transport,
  authentication model, and client SDK remain uncommitted until one external-client slice proves
  them together.
- **Refines:** D063's composition direction and supersedes any remaining implication that clients or
  products should embed the Go control plane. D034's Python SDK was already superseded by D047.
- **Reconsider when:** A genuinely process-local consumer can preserve the same durable ownership,
  recovery, and operational truth with materially less machinery than a deployed control plane, or
  direct client use proves a different resource boundary.
