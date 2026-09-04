# D061: One fact-derived coding flow replaces the durable program counter

- **Applicability:** current
- **Areas:** workflows, core, persistence
- **Read when:** Changing coding-flow sequencing, workflow projections, or durable progress representation.
- **Decision history:** Accepted workflow-authority and inspection decision — 2026-08-10
- **Decision:** Dorf does not persist `workflow_phase`, `next_work`, or another derived workflow
  status. It loads one concrete coding `Snapshot` and derives a disposable `Projection` containing
  readiness and the pure `CurrentWork` decision. Execution, human history, and structured inspection
  share that same Snapshot rather than loading or interpreting the facts independently. `RunJob`
  reloads after each recorded fact, and every mutation transactionally revalidates its exact owner
  before recording an effect. Absurd alone owns task eligibility, claims, named checkpoints, sleeps,
  waits, retries, and cancellation.
- **Missing recovery fact:** A completed implementation Turn does not prove that Dorf inspected its
  mutable checkout. Bind the implementation AgentRun to its input Revision, then retain the existing
  `git-revision` Evidence for both changed and unchanged clean observations with that AgentRun as its
  owner and observed `HEAD` as its Revision. A changed observation also creates the next immutable
  Revision; equality records an unchanged result. This reuses one natural proof fact rather than
  adding a Revision-observation table or stored changed/unchanged enum, and replaces the only real
  information previously hidden in implementation/review handoff phases.
- **Human view:** Inspection derives both the expected coding dependency chain and chronological
  Message, Action, AgentRun, Revision, `git-revision` Evidence, Check, ReviewPlan, Proposal, Outcome,
  attention, and cleanup facts from the same Snapshot, and marks the same `CurrentWork` used by
  execution. This Projection is disposable. Structured inspection may show attached task correlation
  and terminal result, but it does not copy Absurd attempts, checkpoints, waits, or leases;
  `absurdctl` and Habitat remain the operational execution-history tools.
- **Why:** One source of truth removes phase transitions and impossible phase/fact disagreements,
  makes recovery ask only which authoritative fact is missing, and puts the deterministic flow in one
  readable place. It also makes change local and composable: a new Check adds a Check fact, a selected
  reviewer adds a Message and AgentRun, and another feedback source adds a Message instead of changing
  a schema enum and transition matrix across admission, readiness, publication, inspection, and
  tests. Reordering or extending the coding flow changes explicit dependencies rather than synchronized
  status machinery.
- **Constraint:** This is not permission to build a general DAG engine, configurable workflow DSL,
  generic step registry, persisted status projection, copied event-sourcing layer, or giant SQL
  `next_work` query. Keep the concrete coding decision small and visible in Go. Add a new durable fact
  only for an observed product or recovery need, and keep product chronology derived from natural fact
  timestamps.
- **Reconsider when:** A second real workflow proves that a smaller shared decision primitive exists,
  or measured fact reconstruction becomes a material bottleneck that cannot be addressed by ordinary
  read projections without creating another authority.
