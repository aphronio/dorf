# D068: Explicit Job retry rearms the current failed Absurd task once

- **Applicability:** current
- **Areas:** workflows, interaction
- **Read when:** Changing operator retry behavior or how a Job selects and rearms its failed Absurd task.
- **Decision history:** Accepted operator ergonomics, extended by cleanup dogfood — 2026-08-19
- **Decision:** `dorf job retry JOB_ID` is a thin operator command for the Job's latest attached Absurd
  task. Dorf records task handoffs as one append-only ordered attachment relation rather than named
  main/cleanup task columns; any future task handoff therefore becomes the retry target without new
  selection logic. Dorf calls Absurd's public in-place retry without overriding `max_attempts`, which
  atomically adds exactly one bounded attempt to the same task and retains its checkpoints. It never
  spawns a replacement task, mutates execution facts, resumes a Sandbox directly, or copies task
  attempts and checkpoint state into Dorf tables. Absurd atomically refuses a missing or non-failed
  task. Sandbox-profile admission remains the worker's responsibility when it claims the new run.
- **Truthful receipt:** The command returns only the Job, task, new run and attempt identities and says
  that retry was scheduled. Current work and continuation remain ordinary inspection projections;
  they become true only when a matching worker runs and new progress is observed.
- **Why:** A real E2B and Pi coding Job exhausted its automatic attempts during a host IPv4 outage
  after durable work had already reached isolated review. Calling Absurd's public retry on the same
  task scheduled attempt 6; the worker recovered the existing Job, Sandboxes, native harness state,
  and workflow facts, then completed review, publication, a terminal Outcome, and cleanup. The recovery
  proved the boundary, while requiring operators to construct a one-off Go client made the ordinary
  repair path unnecessarily obscure. Later investigation dogfood found that the main-task-only mapping
  left an exhausted cleanup task without a repair path even though the same Absurd primitive applied.
- **Refines:** D048's public-Absurd-API boundary and D054's earlier decision not to build a separate
  publication retry mechanism. Absurd still owns retry eligibility, attempts, runs, and checkpoints.
- **Reconsider when:** Different task classes need distinct operator policy, one-attempt rearming is
  insufficient in measured operations, or Absurd exposes a safer first-class repair receipt that
  should replace Dorf's thin projection.
