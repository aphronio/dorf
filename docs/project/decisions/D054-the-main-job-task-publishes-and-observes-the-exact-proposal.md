# D054: The main Job task publishes and observes the exact proposal

- **Applicability:** current
- **Areas:** workflows, github
- **Read when:** Changing proposal publication, pull-request observation, or acceptance handling in the main Job task.
- **Decision history:** Accepted workflow simplification — 2026-08-10
- **Decision:** The main Job task runs two direct, Revision-scoped Steps: push the exact Revision, then
  create or adopt its exact pull request. Stable Actions reconcile Git and GitHub before an uncertain
  effect repeats. There is no publication child task, attachment field, or mirrored task state.
  D068 later adds a thin Dorf command over Absurd's public retry without changing this ownership.
- **Acceptance UI:** The same task observes the exact pull request. A comment from the repository
  `OWNER` or `COLLABORATOR` becomes one idempotent human Message whose `FromID` is the GitHub comment
  identity. Dorf acknowledges it with an eyes reaction. Once the same implementation flow has handled
  the Message and republished, Dorf posts one completion comment naming the exact Revision. GitHub's
  idempotent reaction endpoint and a stable invisible completion marker make replay safe without a
  new core fact. Merge records acceptance, close without merge records rejection, and explicit Dorf
  abandonment remains available. Dorf stores Messages, Proposal, and Outcome, but does not mirror a
  comment cursor or mutable pull-request state. Every durable wake or timeout performs a fresh
  reconciliation pass; no in-memory poll counter or checkpointed GitHub observation is part of the
  workflow contract.
- **Why:** Push, propose, wait, and continue are one product story. Giving publication its own durable
  scheduler duplicated retry and attachment mechanics already owned by Absurd. Treating GitHub input
  like any other Message also lets the next implementation AgentRun decide whether to act without
  a new review-result or response type.
- **Reconsider when:** GitHub polling is measurably wasteful enough to justify a webhook wake-up. A
  webhook should wake the same observer; it must not create a second workflow authority.
