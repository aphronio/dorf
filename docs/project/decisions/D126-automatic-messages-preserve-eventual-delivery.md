# D126: Automatic messages preserve eventual delivery

- **Applicability:** current
- **Areas:** core, persistence, client-api
- **Read when:** Changing automatic message resolution, steer reconciliation, or message replay.
- **Decision history:** Accepted after a retained-session terminal-target race — 2026-09-14.
- **Decision:** Treat automatic intent as eventual delivery of one immutable accepted Message. Core
  initially selects steer when exactly one active Turn exists and follow otherwise. If the selected
  Turn accepts the exact Message ID, the steer remains bound there. If reconciliation instead proves
  that Turn terminal without the exact acceptance, Core atomically changes the same Message's
  effective delivery to follow and returns it to FIFO selection. It never retargets another active
  Turn. Explicit steer remains exact and fails when its target terminates without acceptance.
- **Persistence and replay:** Retain requested intent as immutable admission truth and effective
  intent as current delivery truth. The transition preserves Message ID, sequence, content,
  attachments, execution envelope, and AgentRun identity; clears only the unaccepted steer target,
  baseline, and attention; and leaves cleanup closed. Replay validates the original request and
  returns the current effective intent without submitting another Message.
- **Why:** An automatic caller delegates the steer-or-follow choice so its accepted input is
  eventually delivered. Admission can observe an active Turn that terminates before the worker calls
  the Harness. Failing that Message would turn a transient timing snapshot into durable input loss.
  The exact accepted-Message history provides the proof boundary needed to retry safely without
  duplicating an accepted steer.
- **Proof:** PostgreSQL integration tests cover terminal-before-call and uncertain-acknowledgement
  races, same-Message replay, FIFO preservation, and the explicit-steer failure control. The pinned
  Codex adapter proof shows that native steer accepts the exact active Turn for both observation and
  ordinary turns. Client integration tests show that a steer receipt may become follow while keeping
  one accepted user input and one visible reply.
- **Supersedes:** D096 and D108 only for automatic-intent terminal-target behavior. Their explicit
  follow, explicit steer, interruption, cleanup, and session-continuity choices remain current.
