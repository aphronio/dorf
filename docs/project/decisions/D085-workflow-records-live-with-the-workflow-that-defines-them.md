# D085: Workflow records live with the workflow that defines them

- **Applicability:** partial
- **Areas:** core, workflows, persistence
- **Read when:** Changing package ownership or persistence of workflow-defined records.
- **Decision history:** Refined by D088, D089, and D092 — 2026-08-22
- **Decision:** Keep shared custody types limited to Job, Message, AgentRun, Sandbox, Action,
  Evidence, Harness bindings, exact Sandbox file reads, and cleanup. `internal/coding` owns its typed Job input,
  Revision, ReviewPlan and reviewer projections, Proposal, Outcome, readiness policy, workflow
  identity, and review-scoped identities. `internal/investigation` owns its workflow identity,
  source records, and report-path prompt policy. `internal/gitworkspace` owns bounded Git
  observations.
- **Durability:** This is Go package ownership, not a storage or sequencing change. PostgreSQL keeps
  the same typed workflow tables and transactions; Absurd keeps the same task, step, retry, wait,
  and claim authority. Moving a record out of the shared package does not move it out of durable
  custody.
- **Why:** A shared package containing coding records made physical persistence look like Core
  semantics. Workflow-owned records can still be persisted transactionally through PostgreSQL
  without granting their meaning to Core or making the workflow less recoverable.
- **Proof:** The shared custody package declares no coding or investigation workflow identity,
  Revision, review, Proposal, or Outcome type. Existing SQL integration, readiness, publication,
  outcome, CLI, and workflow tests compile against the owning modules and pass unchanged behavior.
- **Reconsider when:** Two independent consumers prove identical semantics for one of these records;
  extract only that earned shared contract rather than moving a whole workflow model back into Core.
