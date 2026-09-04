# D107: Commit Job task scheduling and attachment together

- **Applicability:** current
- **Areas:** persistence, core
- **Read when:** Changing admission, task handoffs, cleanup scheduling, or recovery across Absurd and Dorf.
- **Decision history:** Accepted, 2026-09-05.
- **Decision:** Commit admission facts, the initial Absurd task, and its Dorf attachment in one
  PostgreSQL transaction. Ordinary task handoffs likewise commit task creation and attachment
  together. Requested cleanup takes the Job external-effect fence, closes admission, cancels the
  predecessor, and schedules and attaches cleanup in one transaction. Use Absurd's public SQL
  functions, as explicit retry already does; the pinned Go client cannot join a caller transaction.
- **Why:** Separate commits created runnable tasks without attachments and durable Jobs or cleanup
  requests without scheduled work. Reconciliation could repair these states, but sharing the
  existing database lets new writes avoid them entirely. Admission services no longer coordinate
  persistence with a separate scheduler.
- **Ownership:** Consumer adapters supply task names and idempotency keys. Core retains only
  execution custody and explicitly requested cleanup. No workflow acceptance, completion, or
  release policy moves into Core or its primitives.
- **Preserved behavior:** Keep the append-only task history, predecessor fencing, bounded automatic
  retries, and same-task operator retry. A task requesting its own cleanup is not cancelled, but
  loses execution authority when cleanup attaches. Keep replay and periodic recovery for incomplete
  writes from earlier releases and concurrent older writers during an upgrade. The Control API and
  Agent0's investigation, file-read, and cleanup contract remain unchanged.
- **Failure boundary:** A failed scheduling transaction leaves no newly admitted Job or unattached
  task. A failed cleanup scheduling transaction also rolls back admission closure and predecessor
  cancellation. Callers can retry the same request.
- **Proof:** PostgreSQL tests inject attachment failure after task creation, race admissions against
  a running worker, roll back cleanup cancellation, and execute a task that requests its own
  cleanup. Existing custody-fence, replay, task-history, and cleanup-recovery tests remain required.
