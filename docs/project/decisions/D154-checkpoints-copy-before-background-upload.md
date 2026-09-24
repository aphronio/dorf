# D154: Checkpoints copy before background upload

- **Applicability:** current
- **Areas:** client-api, core, sandboxes
- **Read when:** Changing checkpoint capture, background upload, save scheduling or restored Session APIs.
- **Decision history:** Accepted 2026-09-24; replaces D153's client confirmation protocol and autonomous idle saves.
- **Decision:** Clients own save timing. Dorf copies settled native files under its existing consistency guard, validates the copy boundary, and uploads the private copy independently of subsequent source execution. There is no cross-application commit handshake. Cleanup retains backup-before-delete protection.
- **API:** One Checkpoint resource exposes copying/uploading/ready and durable opaque identity. Restore creates a held Session from a checkpoint; ordinary Session observation and activation own preparation and readiness. Internal custody receipts remain authoritative without a public branch lifecycle.
- **Application boundary:** Clients stabilize their own relevant state through native copying and publish their combined save only after all components succeed. No application state or logic enters Core.
- **Cost:** Copying takes time and temporary guest disk space; uploads need not hold the application pause. Copy-on-write optimization is deferred until observed copy cost justifies it. Worker loss abandons pending uploads; incomplete artifacts are never published. This is not a resumable storage workflow.
- **Preservation:** Existing native checkpoints and internal branch records remain; the identity migration adds an opaque ID without rewriting their immutable content. Existing restore and cleanup primitives are reused.
- **Evidence:** Local independent-copy, stock-restic restore-layout, source-advance-during-upload, failure/restart, and PostgreSQL historical-publication tests cover the changed boundaries. The implementation contract owns additional provider verification.
