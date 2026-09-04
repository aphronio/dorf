# D080: Workflows authorize Messages; PostgreSQL atomically records them

- **Applicability:** historical
- **Areas:** workflows, persistence
- **Read when:** Reviewing the former workflow-specific Message admission and atomic persistence design.
- **Decision history:** Superseded by D088 — 2026-08-21
- **Decision:** Remove the generic PostgreSQL `AdmitMessage` workflow switch. The workflow layer
  exposes `AdmitCodingMessage` and `AdmitInvestigationMessage`; the client adapter dispatches by the
  Job's immutable workflow identity. Each typed path authorizes its delivery intent, AgentRun role and
  capability, exact Revision, and Harness Thread reuse. One shared PostgreSQL transaction still
  locks the Job, verifies the expected workflow revision, preserves sender idempotency and FIFO,
  and inserts the authorized Message and AgentRun together.
- **Why:** Live investigation dogfood identified that generic storage was deciding when a workflow
  could accept input and what its AgentRun meant. Those are workflow policies. Transactional
  custody and concurrency fencing remain Core responsibilities; moving the decision does not weaken
  them.
- **Proof:** The generic transaction contains no workflow switch or role switch. PostgreSQL
  integration coverage retains concurrent FIFO, steer, outcome, cleanup-race, completed-run follow-up,
  same-Thread reuse, and replay behavior, and explicitly rejects using either typed admission path
  for the other workflow.
- **Reconsider when:** A third workflow demonstrates genuinely identical delivery authorization.
  Share only the repeated typed policy; do not restore a central workflow-name dispatcher in
  persistence.
