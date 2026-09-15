# D133: Logical Sandboxes retain provider resource identities

- **Applicability:** current
- **Areas:** sandboxes, persistence, deployment
- **Read when:** Replacing a provider VM, implementing package upgrade rollback, or investigating which infrastructure served a Job.
- **Decision history:** Separates logical workstation custody from physical VM identity, 2026-09-15.
- **Decision:** Keep Job and logical Sandbox identities stable. Give each provider resource a
  retained record with its exact ownership token and attested opaque locator. The logical Sandbox
  points to its active resource. Do not overwrite an old resource's locator to represent replacement.
- **Why:** Incus restores a checkpoint onto the same VM, while E2B creates a replacement VM.
  Treating the logical Sandbox as the physical VM makes recovery ambiguous and loses the old
  identity needed to investigate failures and reconcile cleanup.
- **Authority:** Provider adapters interpret locators and attest ownership. Core records the
  observation under the Job fence; repeated writes must confirm the same resource and locator.
  Existing records migrate without changing ownership tokens or inventing unknown provider IDs.
- **Scope:** The resource foundation records new creation and exposes its binding and retained
  resource receipts through Job inspection. Delivery holds are covered by D134.
  Replacement authorization, checkpoint recovery, and replacement
  history are subsequent parts of the [active upgrade plan](../../implementation/runtime-package-upgrades.md).
- **Proof:** PostgreSQL tests cover baseline migration with retained ownership, idempotent binding,
  rejection of locator redirection and foreign ownership, and stable logical Sandbox reads. The
  disposable package recipe separately proves native Codex conversation recovery on Incus restore
  and E2B replacement; it does not yet prove control-plane upgrade orchestration.
