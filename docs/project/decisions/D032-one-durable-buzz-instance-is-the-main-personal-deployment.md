# D032: One durable Buzz instance is the main personal deployment

- **Applicability:** current
- **Areas:** deployment, release
- **Read when:** Changing the durable Buzz deployment, upgrade process, backup posture, or environment strategy.
- **Decision history:** Accepted after provisioning the first owned relay; integration ownership revised by
  D034 — 2026-07-28
- **Decision:** Treat the persistent `dorf-buzz` Incus VM as the single developer's main Buzz
  deployment, not as a disposable fixture or a staging environment. Develop and validate client
  integrations against this instance, promote changes in place through small runnable demonstrations,
  and keep its identity, messages, and state across iterations. Do not introduce a staging/production
  split without an observed need.
- **Operating posture:** Pin source and container revisions, keep deployment secrets stable, make
  exposure and lifecycle changes programmatic and reversible, validate health and the real client
  path after changes, and take coherent backups before upgrades that put durable state at risk.
  Desktop is the first client for iterative integration work. Trusted WSS, a proper domain or other
  mobile-compatible exposure is a later deployment slice, after a real channel loop works and is
  useful.
- **Why:** There is one developer and operator. A second environment would add resource use,
  configuration drift, promotion ceremony, and false confidence without protecting other users.
  Using the actual long-lived instance makes early dogfood honest and follows the
  small-demonstration, early-adoption execution posture in the project principles.
- **Compatibility:** The VM remains shared infrastructure outside Dorf Room and Worker
  lifecycle. This decision does not make Buzz authoritative for Dorf state and does not weaken
  D034's ownership boundary.
- **Reconsider when:** Other users depend on the instance, public ingress or uptime expectations
  appear, an upgrade cannot be rolled back from a coherent backup, destructive migration testing
  becomes recurrent, or real usage demonstrates that pre-production validation is cheaper than
  recovery.
