# D057: Ordinary external Actions target one exact Sandbox

- **Applicability:** current
- **Areas:** sandboxes, workflows
- **Read when:** Changing the scope or reconciliation of ordinary Sandbox-related external Actions.
- **Decision history:** Accepted Action-scope simplification — 2026-08-10
- **Decision:** Sandbox creation, repository clone, provider-route creation and revocation, exact
  review checkout preparation, and Sandbox deletion use one Sandbox-scoped Action path. Setup keeps
  its generation-aware operation and publication keeps its exact-Revision operations; there is no
  generic Job-Action API or polymorphic target abstraction. The first reconciled Action success is
  immutable and an identical retry is a no-op. Exact external-result validation belongs to the
  adapter before that success is recorded.
- **Why:** These ordinary mutations all change or serve one exact Sandbox. A generic Job path hid a
  redirect to the main Sandbox and created a category with only repository clone as a real member.
  Explicit Sandbox scope makes Action identity, Absurd Step identity, reconciliation, and cleanup
  tell the same story while preserving the crash boundary between execution and external truth.
- **Reconsider when:** A second ordinary external mutation genuinely targets only the Job aggregate,
  or an external system returns a non-Sandbox identity that cannot live in its natural product fact.
