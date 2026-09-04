# D086: One package owns the in-process Core boundary

- **Applicability:** partial
- **Areas:** core, workflows
- **Read when:** Changing the in-process Core package boundary or its workflow-facing capabilities.
- **Decision history:** Refined by D088 — 2026-08-21
- **Decision:** Merge the former shared-record `spine` package and application-facing `controlplane`
  package into `internal/core`. Core owns its provider-neutral records, narrow execution interfaces,
  recovery implementation, Absurd task attachment, and requested cleanup in one package. Workflows
  import this boundary and compose their own Git, coding, investigation, and publication modules over
  it.
- **Why:** Splitting Core's records and implementation behind two package names implied two product
  layers that did not exist. It also left a coding-specific `ObserveAgentRun` shortcut in the common
  contract. One package makes the actual boundary visible and lets workflows state their AgentRun
  role explicitly.
- **Durability:** This is an in-process package merge only. PostgreSQL remains authoritative for
  durable facts, Absurd remains authoritative for task execution and checkpoints, and stable IDs,
  task names, schema, retry behavior, provider behavior, and cleanup ordering are unchanged.
- **Proof:** `internal/spine` and `internal/controlplane` no longer exist; `internal/core` imports no
  workflow module or concrete Sandbox provider; coding and investigation compile against the same
  narrow Core interfaces; the SQL generator, full Go suite, PostgreSQL integrations, vet, build, and
  version checks pass.
- **Not included:** No public transport, SDK, plugin system, or new product vocabulary is introduced.
- **Reconsider when:** A real external transport earns a separate public resource contract or a Core
  capability proves independently useful enough to justify another package boundary.
