# D018: Stabilize only the dogfooded internal runtime boundary

- **Applicability:** historical
- **Areas:** core, workflows, harnesses
- **Read when:** Reviewing the former repository-internal runtime surface and its deliberately excluded extension points.
- **Decision history:** Superseded by D025 — 2026-07-27
- **Decision:** The then-supported repository-internal runtime surface was the logical-session lifecycle
  exercised by the coding workflow: create or retry environment provisioning, start and observe the
  initial native turn, reconnect and inspect, continue or recover one serialized turn, and end with
  observable retryable cleanup. Incus and Codex remain direct built-in adapters. Provider selection,
  generalized runner and agent registries, capability matrices, worker artifact paths, and
  app-server-specific public errors are outside that surface. GitHub, repository, check, review,
  repair, publication, and acceptance policy remain in the coding workflow.
- **Why:** The [#94 ledger](https://github.com/aphronio/dorf/issues/94) directly observed
  later-client reconnect without new runs, three
  sequential turns on one Codex thread, and successful retry after partial cleanup. Every slice used
  Incus and Codex; no second workflow, environment, or interactive agent supplied evidence for a
  generalized selection layer. The registry had no second implementation, and current setup
  recovery no longer needed the old run-kind reclassification shim. Process-liveness and
  app-server error names also leaked replaceable implementation details.
- **Compatibility:** This surface is internal and experimental. Python types, SQLite representation
  and migration policy, CLI rendering, and opaque Codex-native inspection payloads may change with
  further dogfood.
  Public packaging, licensing, releases, and external compatibility commitments are deferred.
- **Reconsider when:** A second real workflow needs the same lifecycle with different caller facts;
  a deliberately selected second environment or interactive agent proves a shared selection seam;
  agent-native history cannot satisfy a real inspection need; or the owner chooses to prepare a
  licensed public release with an explicit compatibility policy.
