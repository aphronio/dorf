# D096: Follow and steer are invariant custody; cleanup timing remains consumer policy

- **Applicability:** current
- **Areas:** interaction, core, workflows
- **Read when:** Changing follow, steer, or consumer-controlled cleanup semantics.
- **Decision history:** Accepted message-semantics convergence — 2026-08-25
- **Decision:** While Job admission is open, Core accepts follow as durable FIFO input. A follow may
  queue before the preceding Turn settles, reuses the Agent handle's authoritative retained Harness
  Thread, and receives a distinct Turn. Steer atomically captures the exact active Turn at admission,
  has priority over queued follows, and remains bound to that Turn. It never falls back to a new Turn;
  if the target becomes terminal before delivery settles, reconciliation reports that honest failure.
- **Consumer boundary:** A workflow or direct client supplies a typed execution envelope, including
  Role, capability, input Revision when applicable, and deterministic infrastructure readiness. It
  may own prompts, results, evaluation, and workflow-specific sequencing, but it does not authorize
  follow or steer, reorder accepted Messages, select their Thread behavior, or impose a completed-run
  gate on follow admission.
- **Lifecycle policy:** Cleanup timing is separate consumer policy. A workflow or direct client may
  conditionally call `RequestCleanup`; Core never derives that request from an Outcome, attention,
  AgentRun completion, idleness, or Harness state. Coding's explicit policy that observes a terminal
  GitHub Outcome and then requests cleanup remains valid because the workflow makes that conditional
  call; the Outcome itself does not trigger Core cleanup.
- **Refines:** D087's workflow composition, D088's Agent handle, D090's open-wait execution, D092's
  investigation follow-up gate, and D095's direct client. It supersedes the D048/D055 terminal-target
  steer fallback while retaining D048's priority intent; D089's pre-cleanup file boundary remains
  intact.
- **Why:** Follow and steer describe durable delivery and Harness identity, not the meaning a consumer
  assigns to work. Making those rules invariant removes divergent workflow gates without moving
  prompt, result, infrastructure, or cleanup policy into Core.
