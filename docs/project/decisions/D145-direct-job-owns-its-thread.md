# D145: Direct Job owns its Thread

- **Applicability:** current
- **Areas:** harnesses, persistence
- **Read when:** Changing conversation continuity, native acceptance recovery, or Thread migration.
- **Decision history:** Moves authoritative direct conversation binding from prior AgentRuns to
  their Job, superseding that part of D055, 2026-09-18.
- **Decision:** Store the primary Harness/Thread pair on the direct Job. Bind it atomically with proven
  native Turn acceptance. Select later Follows and read the conversation timeline through that
  binding. AgentRuns retain their native attribution and submission recovery facts. Native
  subagent threads remain Harness-owned; the binding does not limit their creation.
- **Why:** Continuing a conversation should not reconstruct its owner by scanning earlier runs.
  One stable binding replaces backward lookup and repeated all-run consistency checks.
- **Recovery:** An empty Job binding is not permission to repeat an uncertain initial start.
  Reconcile the retained submission first. Conflicting native bindings fail without partially
  committing either the Job binding or the run receipt. Acquire the Job lock before the run lock.
- **Migration:** Backfill direct Jobs only when retained bindings agree. Conflicts stop migration;
  never select the latest row as a winner. Retired application history and queued input remain
  intact. Stop old workers before applying the migration and start the updated worker afterward;
  old writers do not maintain the new binding.
- **Scope:** No Session rename, public API change, Turn table, controller replacement, or native
  protocol change. The existing Job fence, queue claims, and delivery ordering remain in force.
- **Verification:** PostgreSQL migration/replay and conflict checks, initial-acceptance recovery
  with queued Follows, concurrent binding and receipt atomicity, and native timeline custody.
- **Authority:** [Architecture](../architecture.md#messages-and-agentruns).
