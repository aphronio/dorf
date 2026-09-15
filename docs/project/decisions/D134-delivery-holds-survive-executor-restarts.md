# D134: Delivery holds survive executor restarts

- **Applicability:** current
- **Areas:** persistence, sandboxes
- **Read when:** Coordinating a persistent Sandbox upgrade or explaining accepted input that is waiting during maintenance.
- **Decision history:** Adds a retained delivery barrier for direct Jobs, 2026-09-15.
- **Decision:** Persist one active hold per Sandbox. Serialize hold creation with Message admission
  and the Job effect fence. Queue new automatic input as follows while allowing existing native
  work to settle. Release only the exact hold ID and commit its execution wake atomically.
- **Why:** An executor-local pause disappears on restart. Closing admission loses the desired
  ability to save incoming messages. Releasing by Sandbox alone lets an old executor clear a
  newer operation's barrier.
- **Authority:** PostgreSQL owns the barrier and retained release receipt. Native Harness
  observations must still prove quiescence, and the coordinating operation owns upgrade and
  recovery verification. A hold by itself grants no VM mutation authority.
- **Scope:** Direct Jobs only. Existing workflows have additional workspace mutations that must
  honor a hold before support can expand. D135 composes the hold with package mutation and verified
  resource switching in the existing direct task.
- **Proof:** PostgreSQL and an actual Absurd worker verify retained input, active-turn/steer drain,
  restart, FIFO release, exactly one native submission per queued input, stale-release rejection,
  wake rollback, idle-pause exclusion, and cleanup while held. API checks preserve accepted/waiting
  status without inventing a result.

- **Continuation:** D135 composes these foundations with package recovery, verified resource switching, and cleanup.
