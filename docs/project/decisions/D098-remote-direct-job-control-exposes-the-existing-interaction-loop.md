# D098: Remote direct Job control exposes the existing interaction loop

- **Applicability:** current
- **Areas:** client-api, interaction
- **Read when:** Changing remote Job observation, messaging, retry, files, or Evidence access.
- **Decision history:** Accepted and dogfooded — 2026-08-26; common interaction contract reused by D099
- **Decision:** Extend D097's authenticated single-Deployment projection with conditional canonical
  Job observation and snapshot SSE, invariant follow and steer Message admission and inspection,
  caller-keyed retry of the existing eligible failed execution, exact Sandbox-level reads under the
  cleanup fence, and verified Evidence metadata. The transport translates existing
  Core/PostgreSQL/Absurd/Harness authorities; it adds no event store, workflow delivery policy,
  generic mutation ledger, Task/Run/Turn resources, transcript persistence, or filesystem API.
- **Boundary:** Files are exact reads only, and Evidence is verified metadata only. A direct Job may
  have no Evidence. Message delivery remains D096's universal FIFO Follow and exact-active-Turn
  Steer contract; the caller still decides result meaning and cleanup timing.
- **Refines:** D097's initial external projection, D095's direct client, D088's application boundary,
  D096's Message invariants, and D089's pre-cleanup file custody. The current contract lives in the
  [Remote Control API](../../control-api.md); the staged proof is
  [archived](../../history/control-api-slices.md).
- **Why:** A remote client is not useful if ordinary interaction falls back to SSH after the first
  Turn. Projecting the already-owned primitives completes that loop without promoting recovery facts
  or provider operations into public resources.
- **Reconsider when:** A real client earns durable event history or webhooks, streaming or writable
  files, Evidence-byte retrieval, or a broader recovery contract.
