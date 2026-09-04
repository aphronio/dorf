# D007: Cleanup has an explicit, retryable outcome

- **Applicability:** current
- **Areas:** core, workflows, sandboxes
- **Read when:** Changing resource cleanup outcomes, retries, or failure visibility.
- **Decision history:** Accepted — 2026-07-22
- **Decision:** Workflow completion and environment cleanup are separate facts. Cleanup is
  idempotent, retryable, and visibly failed until resources are released.
- **Why:** A completed or discarded proposal does not prove that a local VM or future billable
  environment was deleted.
- **Reconsider when:** The representation may change, but cleanup failure must remain observable.
