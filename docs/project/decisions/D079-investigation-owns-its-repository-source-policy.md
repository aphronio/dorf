# D079: Investigation owns its repository source policy

- **Applicability:** partial
- **Areas:** workflows, core
- **Read when:** Changing investigation repository source policy or its boundary with Core execution.
- **Decision history:** Accepted Core/domain separation slice; source transport refined by D103 and Draft
  storage removed by D092 — 2026-08-27
- **Decision:** Keep the investigation `Source` in `internal/investigation`. Its typed runtime
  composes credential-free HTTPS cloning over shared Git workspace execution. The base runtime
  grants only execution. Coding and investigation each add their own Git-backed authority.
- **Why:** Keeping investigation types and repository rules in the shared Core package made the
  second workflow look like shared Core semantics. Core owns the Action, Sandbox, AgentRun, and
  cleanup mechanisms. The investigation workflow owns its repository and report-path policy.
- **Proof:** `internal/core` contains no investigation source or report-path policy. PostgreSQL, CLI,
  coordinator, and typed runtime consume the investigation-owned source contract directly.
- **Reconsider when:** Another workflow needs the same repository input and recovery semantics.
  Extract a neutral repository-input contract only after that second use.
