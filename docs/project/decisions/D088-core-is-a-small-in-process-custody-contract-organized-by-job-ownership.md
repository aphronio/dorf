# D088: Core is a small in-process custody contract organized by Job ownership

- **Applicability:** partial
- **Areas:** core, workflows, client-api
- **Read when:** Changing the Core custody contract or its boundary with workflows and clients.
- **Decision history:** Accepted application-boundary correction; file custody refined by D089 and message
  semantics refined by D096 — 2026-08-25; external projection added by D097 and expanded through
  D099 — 2026-08-26
- **Decision:** The [North Star product boundary](../north-star.md#product-boundary) remains the sole
  authority for product ownership, and [Architecture](../architecture.md#execution-model) owns the
  current technical contract. Workflows, workflow modules, and trusted client adapters compose one
  small Core application contract in-process. Core owns generic durable custody and recovery; it
  owns no workflow, Git, coding, GitHub, publication, or human-in-the-loop policy.
- **Ownership-shaped contract:** Admitting complete Core intent returns a Job handle, and Job remains
  the durable aggregate owner. `EnsureSandbox` returns its exact Job-owned Sandbox handle, and that
  Sandbox exposes an Agent convenience handle bound to it. Core backs the convenience internally
  with the selected Harness plus durable Message and AgentRun facts. A caller-retained per-send
  idempotency key binds the complete admitted delivery request; follow is the default and steer is a
  distinct explicit mode. AgentRun remains an internal recovery fact rather than a resource
  consumers coordinate.
- **Artifacts and cleanup (superseded by D089):** Each AgentRun receives a dedicated run-owned
  artifact directory in the Sandbox working area, isolated from any workflow-managed source
  checkout. Core automatically retains files placed there as immutable Job-owned Artifacts with
  producing-run provenance. A durable collection obligation exists no later than terminal Harness
  observation becomes visible to cleanup. Core may neither revoke the route nor delete the Sandbox
  until every collection has a durable settled receipt. Only a workflow, composed module, or client
  may request cleanup; Core reconciles that request but never derives it from execution outcome or
  interaction state.
- **External projection (refined by D097 through D099):** This decision introduced no public transport,
  authentication contract, SDK, plugin system, workflow DSL, or embeddable-runtime promise.
  Provider and Harness interfaces remain internal adapter seams. D097 through D099 later add one
  deliberately narrower authenticated HTTPS projection for direct Jobs and two fixed typed
  workflows; it does not expose the Core contract itself.
- **Reconciliation:** D076's separate Core and workflow facts remain, but workflow identity and
  workflow input apply only to workflow-driven Jobs. D080's workflow-specific Message admission as
  the only route is superseded by the generic Agent handle. Consumers supply typed execution
  envelopes and infrastructure readiness, while Core retains intent authorization, atomic identity,
  FIFO and priority order, Thread semantics, and recovery. D081's transport framing is superseded;
  the stateful deployment and in-process Core boundary remain, while D097 through D099 own the later
  external projection. D082's consumer-facing capability-interface inventory is superseded by the
  Job/Sandbox/Agent handle shape; provider-neutral interfaces remain internal.
  D085 still assigns domain records to their defining workflow and now treats AgentRun as internal
  custody and Artifact as Job-owned. D086 still keeps the implementation in one Core package without
  making that package a public API. D087 still gives each native workflow its complete module, but
  task attachment, wake/wait details, Message creation, and AgentRun recovery remain behind Core's
  application contract rather than becoming workflow-facing orchestration primitives.
- **Why:** The prior boundaries exposed storage and recovery vocabulary as a composition surface and
  described a future transport before one existed. Following durable ownership produces a smaller
  contract for both direct clients and workflows, preserves recovery truth, and prevents shared Git
  or interaction needs from being mistaken for Core policy.
- **Reconsider when:** A real consumer proves the external projection must exceed the current closed
  direct-and-fixed-workflow surface, an independently distributed workflow proves loading and
  compatibility requirements, or exact file reads cannot preserve a real workflow's required
  deliverables and provenance.
