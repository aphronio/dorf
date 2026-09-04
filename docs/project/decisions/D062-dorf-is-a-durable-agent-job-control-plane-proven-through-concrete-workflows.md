# D062: Dorf is a durable agent-Job control plane proven through concrete workflows

- **Applicability:** historical
- **Areas:** product, core, workflows
- **Read when:** Reviewing the superseded research-first proof order and its original product rationale.
- **Decision history:** Superseded by D063 — 2026-08-13
- **Positioning:** Dorf is the open-source control plane for durable agent Jobs on infrastructure its
  owner controls. Workflows use deterministic code for knowable work and isolated agents for
  judgment, with recovery and Evidence built in. The supported claim remains transparent: Codex,
  Incus, and coding-to-PR are verified today; multiple Harnesses, Sandboxes, workflows, and trigger
  surfaces are direction until real implementations prove them.
- **Product boundary:** A trusted client owns personal context, priorities, and composition across
  Jobs. A workflow owns the semantics, policy, evaluation, and Outcome of one bounded Job. Dorf owns
  durable custody: admission, Messages, AgentRuns, Sandbox lifetime, stable external-effect
  reconciliation, observed Evidence, attention, recovery, and cleanup. A software factory, personal
  assistant, or agent organization may compose Dorf Jobs, but none becomes Dorf's core metaphor.
- **Next proof:** Coding-to-PR remains the only implementation requirements driver until a bounded
  research-to-report issue deliberately begins. Research is the candidate second workflow because it
  needs the same durable custody while owning no branch, Revision, Proposal, or GitHub Outcome. The
  first trusted client should invoke it through the existing same-host structured CLI boundary. Do
  not add HTTP or embed the Go runtime merely to integrate the first trusted client.
- **Extraction gate:** Add the research workflow's natural facts and explicit coordinator before
  changing the coding schema into generic payloads or nullable fields. After coding and research
  work, extract only duplicated behavior with the same authority and recovery meaning into an
  internal application API. Exercise that API through another small workflow or external author
  before declaring a public workflow-authoring compatibility promise.
- **Authoring and evaluation:** The intended shareable unit is ordinary versioned workflow source
  plus typed input and Outcomes, capability and connection requirements, deterministic operations,
  bounded AgentRun judgment, budgets, terminal conditions, and evaluation cases. Evals begin with
  the workflow rather than arriving after the SDK. Agents may author reviewable workflow code,
  manifests, tests, and evals, but may not activate new powers or production versions silently.
- **Clients and distribution:** CLI and trusted same-host clients come first. CI/GitHub is the likely
  first public trigger; HTTP/webhooks and MCP follow real remote-client needs; native Slack and
  scheduling remain later adapters. Share pinned Git-hosted workflow bundles before building a
  registry or marketplace. Triggers translate external events into idempotent admission and never
  become workflow authority.
- **Why:** The Go rebuild proved durable coding behavior and removed speculative Worker, Room, phase,
  and framework abstractions. It also left coding facts near records that look reusable. Returning to
  a broad framework now would repeat the same mistake. A materially different second workflow is the
  smallest visible result that can prove the building-block thesis while preserving Mitchell
  Hashimoto's posture: make simple dependable pieces, adopt them early, and let real composition earn
  public seams.
- **Refines:** D009's single-driver gate, D047's coding-shaped Go foundation, and D061's explicit
  fact-derived coordinator. It does not weaken their current coding invariants or authorize a generic
  DAG, plugin registry, provider matrix, or compatibility layer.
- **Reconsider when:** Research fails to reuse the proposed custody semantics, another second
  workflow offers a smaller proof, a remote client deployment justifies network transport, an
  external workflow author exposes a different API boundary, or repeated workflow evidence shows
  that the durable custody model does not reduce attention or improve trustworthy outcomes.
