# D075: Core mechanisms do not own workflow or interaction policy

- **Applicability:** partial
- **Areas:** core, workflows, interaction
- **Read when:** Changing ownership between Core mechanisms, workflow policy, and client interaction policy.
- **Decision history:** Accepted and implemented; investigation report custody refined by D089 and D092 — 2026-08-22
- **Decision:** Make the [North Star product boundary](../north-star.md#product-boundary) the sole current
  authority for Core, workflow, and client ownership. This entry records the correction and its
  rationale rather than restating that contract.
- **Investigation correction:** `codebase-investigation` accepts follow-up Messages on the same
  Harness Thread and asks the agent to maintain workspace-root `REPORT.md`. It does not persist the
  report, accept/reject decisions, or cleanup timing. A personal assistant, n8n, a UI, another
  workflow, or a human-operated client may read the report before cleanup, create an Issue, chain
  work, request a revision, record its own disposition, and finally request Dorf cleanup.
- **Coding distinction:** `coding-to-proposal` may interpret GitHub Proposal observations and request
  cleanup because that is workflow policy. Those facts and choices remain outside Core; compiling
  the workflow into Dorf grants no privileged authority or hidden execution path.
- **Refines:** D063's Core authority, D069's investigation terminal, D072's former deliverable consumer,
  and D074's human disposition. It preserves D074's same-Thread revision loop while removing its
  decision authority.
- **Why:** Maintainer-radar dogfood initially encoded accept/reject as a typed investigation decision.
  That made a useful client policy look like a Core requirement and contradicted the intended role of
  workflows as ordinary Core consumers. The reusable primitives are exact Sandbox reads, Messages,
  AgentRuns, retained Harness context, and requested cleanup—not the meaning a caller assigns to a
  report.
- **Proof:** The decision table, domain type, CLI command, workflow wake wrapper, automatic cleanup
  branch, and decision rendering are deleted. Unit and PostgreSQL integration coverage prove
  same-Thread follow-up, explicit cleanup closing admission, and shared route-before-Sandbox
  cleanup. D089 and D092 replace retained Drafts with exact caller-selected reads of workspace-root
  `REPORT.md` before cleanup.
- **Reconsider when:** A reusable custody mechanism cannot support multiple real workflows without
  knowing their terminal policy. A single workflow needing a typed decision is not sufficient; that
  decision remains workflow-owned.
