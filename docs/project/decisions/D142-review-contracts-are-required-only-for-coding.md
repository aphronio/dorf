# D142: Review contracts are required only for coding

- **Applicability:** current
- **Areas:** harnesses, sandboxes, workflows
- **Read when:** Changing ordinary adapter contracts or composing coding review execution.
- **Decision history:** Separates mandatory review contracts from ordinary execution, 2026-09-18.
- **Decision:** Ordinary Harness and Sandbox interfaces do not require strict-review methods,
  review metadata attachment, or review attestation. Coding keeps its explicit review transport
  and Harness contracts. Runtime composition creates the review controller only for coding
  execution or a retained strict-review AgentRun, including cleanup and recovery.
- **Why:** A provider or Harness used for direct execution should not have to implement coding
  review. Consumer-specific composition preserves the existing review boundary without imposing
  it on every adapter.
- **Preserved behavior:** Strict review requires exact provider review attestation before native
  access and fresh attestation after Codex reconnect or process replacement. Missing review
  support fails with an explicit unsupported-capability error; it never falls back to ordinary
  execution. Job identity, storage, AgentRuns, input ordering, and Absurd orchestration are unchanged.
- **Verification:** Adapter tests cover missing attestation for review start, recovery, and reads.
  Runtime tests cover ordinary execution without review contracts and explicit coding composition.
  Existing strict-review protocol, provider, and PostgreSQL tests remain applicable.
- **Authority:** [Architecture](../architecture.md#harness-and-sandbox-adapters) owns the adapter
  boundary; the [proposal tracker](../../implementation/session-product-proposals.md) owns
  remaining tentative slices.
