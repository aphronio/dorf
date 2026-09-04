# D076: Core Jobs and workflow inputs have separate durable types

- **Applicability:** partial
- **Areas:** core, workflows, persistence
- **Read when:** Changing Core Job fields or durable workflow-specific input types.
- **Decision history:** Refined by D088, D089, and D103 — 2026-08-27
- **Decision:** Keep `dorf.jobs` limited to shared custody: identity, bounded goal, pinned workflow
  and Sandbox profile, execution attachment, attention, and cleanup lifecycle. Store coding
  repository, starting/current Revision, branch, GitHub authority, and selected setup in
  `coding_to_proposal_inputs`. Store the investigation repository and exact Revision in
  `codebase_investigation_sources`. Admission uses explicit typed inputs for each
  workflow; there is no generic input JSON, nullable workflow column set, or compatibility facade.
- **Execution:** Coding continues to own a mutable branch and Revision line. Investigation owns a
  clean detached checkout at one exact Revision and never fabricates a branch. Shared Sandbox,
  AgentRun, route, attention, task attachment, exact Sandbox file reads, and requested-cleanup
  custody remain shared Core mechanisms. Investigation report bytes remain workspace state until a
  caller reads them or requests cleanup.
- **Why:** The first coding workflow had placed its repository and GitHub assumptions on the shared
  Job row. The second workflow proved those were not Core facts and that retaining them would force
  false branches and coding authority onto unrelated work.
- **Proof:** PostgreSQL constraints and integration coverage reject cross-workflow facts, preserve
  complete-input idempotency, admit branchless remote investigations, and keep coding
  revision/setup/publication behavior typed. The full SQL generation/vet, Go unit/integration, and
  vet contract passes against a recreated prototype baseline.
- **Reconsider when:** Another workflow needs one of these facts with the same authority and recovery
  meaning. Repeated field names alone are insufficient reason to move it into Core.
