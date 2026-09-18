# D144: Retire built-in coding

- **Applicability:** partial
- **Areas:** workflows, client-api, persistence
- **Read when:** Changing application policy ownership or the direct-only public boundary.
- **Decision history:** Retires built-in coding and its review, publication, outcome, Evidence,
  and GitHub integration contracts, 2026-09-18. D147 removes the remaining application attribution
  and supersedes its preservation below.
- **Decision:** Make direct execution the only supported application path. Clients own repository
  setup, review, publication, credentials, evaluation, and business outcomes. Remove the application
  packages, CLI/API entry points, GitHub App setup page, strict-review adapters, and application
  storage. Core keeps fixed lifecycle effects, not arbitrary workflow Action callbacks.
- **Why:** These facilities are application policy. Their removal leaves fewer concepts and no
  replacement framework. Retained direct execution needs none of their dedicated tables.
- **Preservation:** Keep Message input and attachment blobs, native bindings, exact resource
  ownership, lifecycle receipts, and recovery records. Published migrations remain unchanged;
  the new migration drops only application tables and their review projection. Legacy attribution
  fields remain for the later Job/AgentRun ownership slices.
- **Retirement:** Deployments using old workflow Jobs complete cleanup and export needed
  application data with the previous release before upgrading. There is no workflow conversion
  or retained executor. External coding clients establish their own guarantees.
- **Verification:** Exercise direct admission, ordered input, native recovery, ownership fences,
  and cleanup. Remove dedicated workflow tests; do not add tests solely asserting absence.
  The repository code and documentation gates passed, including PostgreSQL migration/replay and
  retained direct behavior. Live provider proofs were not rerun; direct protocol behavior is retained.
- **Authority:** [North Star](../north-star.md#product-boundary),
  [Architecture](../architecture.md#application-composition), and
  [Control API](../../control-api.md#resources).
