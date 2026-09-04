# D069: Codebase investigation is the second explicit native workflow

- **Applicability:** partial
- **Areas:** workflows, core, client-api
- **Read when:** Changing the codebase-investigation workflow, its durable facts, or its client-facing execution boundary.
- **Decision history:** Accepted initial implementation slice; interaction boundary refined by D075 and report
  custody replaced by D092 — 2026-08-22; unused optional provider-capability declarations removed
  by D106 — 2026-09-05.
- **Decision:** Add `codebase-investigation` as a clean workflow identity, not a top-level
  `investigate` feature and not a generic researcher. One admitted Job pins the exact workflow
  revision, repository Revision, unstructured brief, execution profile, AI connection, model,
  and reasoning envelope. A workflow may declare optional broad provider primitives beyond Dorf's
  baseline Sandbox and Harness contracts; admission and Worker claim reject missing primitives
  before a Sandbox call. Repository dependencies remain the responsibility of its setup script or
  custom image. The current investigation needs no optional provider capability.
- **Workflow facts:** The workflow has one ordinary Go coordinator over its own dependency chain:
  main Sandbox create, exact repository checkout, scoped Provider Route, one `investigate` AgentRun
  per accepted initial or follow-up Message, unchanged-checkout verification, and draft recording.
  Each draft is an immutable typed workflow-owned Markdown result grounded in repository paths and
  lines. A draft may plainly state that no useful finding exists; there is
  no synthetic Outcome enum or machine-readable first-line marker. Agent prose remains a result, not
  proof of its claims.
- **Shared seam:** Jobs now durably pin workflow name and revision. Investigation reuses Job custody,
  exact Sandbox Actions, runner-neutral AgentRun reconciliation, content-addressed blob storage,
  profile fencing, and route-before-delete cleanup. Coding and investigation retain separate
  coordinators, snapshots, natural facts, and operation projections. There is no workflow registry, JSON result bag,
  DAG, provider matrix, or workflow DSL.
- **Client boundary:** `dorf workflow run codebase-investigation` is the first CLI projection of the
  workflow-as-durable-function boundary. The workflow itself remains independent of whether a future
  caller is a CLI, schedule, GitHub event, Slack tag, assistant, or another product.
- **Deliberate omissions:** This initial slice had no repository setup, coding Checks, branch mutation,
  review, GitHub authority, publication, external web sources, cron scheduling,
  automatic Job chaining, or persistent researcher identity. Approval and composition remain outside
  this workflow according to the North Star product boundary; later use must earn each additional
  capability.
- **Proof:** Unit coverage proves the independent operation order, flexible nonblank draft edge, and
  optional provider-capability diagnostics. PostgreSQL integration proves immutable workflow admission,
  cross-workflow idempotency conflict, the investigator Role/capability binding, immutable typed
  draft storage, one full fake-Harness draft loop, and explicit shared route-before-Sandbox cleanup.
  Live Codex dogfood produced two same-Thread Markdown drafts and exposed the misplaced
  decision-specific cleanup coupling. The corrected client-requested cleanup terminal remains a D075
  proof follow-up.
- **Why:** Dorf needs concrete workflows that improve its own development and demonstrate what Core
  enables. A repository-grounded investigation differs materially from coding-to-proposal because it
  owns no Revision mutation, Checks, review, Proposal, or GitHub Outcome. That difference is enough
  to expose workflow identity, optional provider-capability admission, typed drafts, and role-neutral AgentRun
  observation without generalizing the coding coordinator.
- **Reconsider when:** Live dogfood shows useful investigations require captured external sources or
  deterministic reference validation, another workflow repeats
  the same definition/admission/inspection code with identical authority, or a remote client proves a
  smaller public application boundary.
