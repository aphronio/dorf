# D108: Reuse direct Jobs for retained sessions and exact message control

- **Applicability:** current
- **Areas:** core, persistence, client-api
- **Read when:** Changing automatic message intent, interruption, or direct Job session continuity.
- **Decision history:** Accepted, 2026-09-09.
- **Decision:** Reuse direct Jobs and native Harness session storage. Remote messages default to
  steering active work or following when idle, resolved once at admission. Preserve the requested
  intent separately from the admitted delivery target. Add idempotent exact-message interruption
  for direct Codex Jobs, recorded on the original Turn-starting AgentRun and executed by the
  existing worker under its Job fence.
- **Why:** A conversational client needs to correct current work and stop it without creating a
  second runtime controller. Direct Jobs already retain Sandboxes, Threads, and ordered messages.
  Native tools execute beside Codex; an external tool relay or copied transcript is unnecessary.
- **Ownership:** The client owns channels, presentation, assistant instructions, and when to close
  the session. Codex owns reasoning, tools, conversation history, and compaction. Dorf owns execution,
  accepted commands, native bindings, and requested cleanup.
- **Failure boundary:** A stopped worker reconnects to the surviving runtime. Disk-retained native
  history does not prove the outcome of a command interrupted by process loss. No backup, destroyed
  Sandbox restoration, assistant memory service, or streamed assistant output is added.
- **Proof:** PostgreSQL tests cover automatic-intent replay across state changes, exact interruption
  through a steer, and isolation from successor turns. Native protocol tests reconcile an uncertain
  interrupt acknowledgement without stopping a successor. Live CLI evidence records the separately
  tested runtime failure cases.
