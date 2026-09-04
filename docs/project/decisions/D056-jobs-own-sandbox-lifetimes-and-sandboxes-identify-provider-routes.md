# D056: Jobs own Sandbox lifetimes and Sandboxes identify Provider Routes

- **Applicability:** current
- **Areas:** sandboxes, workflows, persistence
- **Read when:** Changing Sandbox ownership, Provider Route identity, or Job cleanup custody.
- **Decision history:** Accepted resource-lifecycle simplification — 2026-08-10
- **Decision:** The Job is the aggregate and lifetime owner of every Sandbox created for the coding
  task, including isolated review Sandboxes. Each Sandbox deterministically identifies its one scoped
  Provider Route, so Dorf does not store a second Route row. AgentRuns use a Sandbox and record that
  binding, but do not own infrastructure. Cleanup walks Job → Sandboxes and records each Route revoke
  before Sandbox deletion as an immutable Action success. There is no polymorphic owner kind/id.
- **Why:** Ownership follows the longest relevant lifetime, keeps database relationships concrete,
  permits AgentRun retries and follow-ups to reuse a Sandbox, and gives one cleanup inventory without
  copied reviewer-resource state or role-specific cleanup algorithms.
- **Reconsider when:** A concrete workflow requires resources to outlive their Job, or a Sandbox must
  be shared safely by multiple Jobs; either case would require an explicit new aggregate and custody
  rules rather than a polymorphic owner shortcut.
