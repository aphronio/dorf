# D099: Fixed typed workflow admission reuses remote Job control

- **Applicability:** current
- **Areas:** client-api, workflows
- **Read when:** Changing remote admission or projection for a built-in typed workflow.
- **Decision history:** Accepted and dogfooded — 2026-08-26; source admission refined by D103 — 2026-08-27
- **Decision:** Add exactly two workflow admission resources to D097's authenticated Deployment:
  `POST /v1/workflows/coding/jobs` and
  `POST /v1/workflows/codebase-investigation/jobs`. Each accepts its complete typed input under the
  existing caller-owned idempotency key. Canonical inspection and watch return one closed, flat Job
  union discriminated as `direct`, `coding`, or `codebase-investigation`; there is no shared nullable
  workflow payload, extension map, registry, dynamic schema, or workflow DSL.
- **Common interaction:** Every Job kind inherits D098's canonical Message, watch, eligible retry,
  exact Sandbox file, verified Evidence, and cleanup resources and invariants. The projection does
  not expose workflow internals, credentials, provider or Harness operations, or an alternate
  lifecycle.
- **Workflow authority:** Coding retains its exact repository, Revision, branch, Proposal, GitHub
  Outcome, and policy that conditionally requests cleanup after a terminal Outcome. Investigation
  retains its exact source and `REPORT.md` policy and remains open and idle until its client requests
  cleanup. Investigation admission accepts only a credential-free reachable HTTPS repository at an
  exact full commit OID.
- **Why:** Real off-host clients need the existing built-in workflows without SSH, but a generic
  workflow surface would erase the typed policy and authority that make those workflows honest.
  Fixed typed routes reuse the proven Job interaction contract while keeping each compiled workflow
  as an ordinary Core consumer.
- **Refines:** D097's initially direct-only admission, D098's interaction projection, D088's external
  application boundary, D096's invariant Message semantics, and D069/D073/D092's investigation
  input and report custody. The route and CLI contract live in the
  [Remote Control API](../../control-api.md); the real HTTPS proof is
  [archived](../../history/control-api-slices.md).
- **Reconsider when:** Another proved workflow cannot be expressed as a fixed typed admission and
  closed Job member, or independently distributed workflows earn a loading and compatibility
  contract.
