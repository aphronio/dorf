# D051: One explicit coding coordinator uses stable Absurd Steps

- **Applicability:** current
- **Areas:** workflows, persistence
- **Read when:** Changing coding workflow coordination, stable Step identities, or recovery guards.
- **Decision history:** Accepted workflow boundary — 2026-08-10; refined by D061 and D087
- **Decision:** The coding path is ordered by one readable `coding.RunJob` coordinator. It invokes
  bounded operations in product order. `CurrentWork` selects the exact owning fact. Each external
  Action runs in its own `dorf/action/v1/<ActionID>` Step and returns
  `ActionStepResultV1{ActionID}`; AgentRun, Revision, Check, and policy operations retain their own
  stable versioned Step names and typed results. Absurd owns durable execution mechanics; PostgreSQL
  remains authoritative for Job facts and Action settlement.
- **Boundary:** Ordinary Incus, Git, Codex, and command work is not held under one long Job fence.
  Each code-owned external mutation reserves and reconciles its stable Action, performs a final
  claim check, records Action success, and then completes its Step. Each AgentRun instead reconciles
  its own Harness/Thread/Turn identity. D061 removes the transitional `workflow_phase`; the mixed
  service-layer coordinator that interpreted it was already deleted. The application constructs one
  compile-time `ServiceStore` and `ServiceExternals` boundary; runtime capability assertions do not
  select coding behavior.
- **Why:** The flow is understandable in one place, and interruption recovery comes from the chosen
  durable runtime rather than a second Dorf-owned program counter.
- **Reconsider when:** D061 absorbed the original trigger after review and publication became direct
  fact-ordered operations. Reconsider this remaining boundary only when real dogfood shows that a stable
  Step identity or local fact guard cannot represent recovery truthfully.
