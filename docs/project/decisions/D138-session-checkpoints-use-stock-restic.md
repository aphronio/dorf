# D138: Session checkpoints use stock restic and scoped object storage

- **Applicability:** current
- **Areas:** sandboxes, harnesses
- **Read when:** Changing session checkpoint storage, capture, or recovery.
- **Decision history:** Adds provider-independent recovery for the first direct Codex profile, 2026-09-15.
- **Decision:** Run upstream restic in the sandbox and upload directly to R2 using short-lived
  credentials scoped to one logical session. The worker retains parent storage and encryption
  custody. PostgreSQL stores successful snapshot references alongside existing execution facts.
- **Scope:** Checkpoint after an idle interval and before requested cleanup. New input invalidates
  background capture without waiting for it. Restore an exact published checkpoint into a fresh
  VM under the existing delivery-hold and resource-ownership boundaries. Provider snapshots remain
  the package-upgrade rollback mechanism.
- **Why:** Reuse an existing backup format and object store. VM-loss recovery and isolation between
  unrelated sessions are required; preventing a compromised sandbox from destroying its own backup
  history is outside the prototype guarantee. No restic fork, bucket retention-rule manager, or
  append-only gateway is justified for this slice.
- **Execution:** Provider adapters own bounded command execution and cancellation. Checkpoint code
  does not implement a second remote process manager. Native consistency and publication checks
  remain necessary to reject captures changed by late writes or new work.
- **Verification:** The [active plan](../../implementation/session-checkpoints.md) records supported
  scope and evidence. Real source-VM loss, native continuation, cancellation, cleanup restore, and
  cross-session credential denial must pass; a host-only backup does not establish native recovery.
