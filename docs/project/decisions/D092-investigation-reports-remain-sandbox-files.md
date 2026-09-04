# D092: Investigation reports remain Sandbox files

- **Applicability:** current
- **Areas:** workflows, sandboxes, persistence
- **Read when:** Changing how investigation reports are produced, accessed, or retained.
- **Decision history:** Accepted workflow simplification; follow admission refined by D096 — 2026-08-25
- **Decision:** `codebase-investigation` asks its agent, as workflow-owned prompt policy, to maintain
  workspace-root `REPORT.md`. A completed internal AgentRun is the durable completion fact. The
  workflow does not interpret Harness prose as the report, copy report bytes into PostgreSQL or the
  blob store, record a Draft or report receipt, or add a generic result abstraction.
- **Access and lifetime:** A caller retrieves the current exact bytes through
  `SandboxHandle.ReadFile` before requesting cleanup. Follow-up Messages reuse the authoritative
  Harness Thread and may update the same path. Missing files fail honestly at read time. Requested
  cleanup closes reads and Sandbox deletion makes the report unavailable; clients own any retention or
  publication they require.
- **Workflow boundary:** Remove the Draft domain, validation and unchanged-checkout checkpoint,
  PostgreSQL table and queries, history/display persistence, and follow-up Draft gate. Investigation
  supplies the typed execution envelope and report prompt, but does not authorize or defer Follow.
  Core injects no prompt, scans no output, and owns no Investigation result meaning.
- **Why:** Retaining Markdown duplicated mutable Sandbox state and forced workflow authors to know a
  persistence contract. The exact-read primitive already provides the smallest truthful boundary:
  workflow policy chooses a path, the stock agent writes ordinary files, and the client decides what
  to consume before cleanup.
- **Refines:** D069, D075, D076, D079, D089, and D090. D074 remains superseded history.
- **Proof:** Unit and PostgreSQL coverage preserve prompt ownership, completed-run follow admission,
  same-Thread distinct Turns, open-idle task recovery, explicit cleanup ordering, exact pre-cleanup
  file reads, and post-cleanup unavailability while deleting all Draft persistence.
