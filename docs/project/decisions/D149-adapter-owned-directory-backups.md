# D149: Adapters own directory backup selection

- **Applicability:** current
- **Areas:** sandboxes, harnesses
- **Read when:** Changing checkpoint file selection, client configuration continuity, or backup privacy.
- **Decision history:** Extends D138 with the agreed directory selection and confidentiality boundary, 2026-09-19.
- **Decision:** Back up the Sandbox-provided workspace and the Harness-provided native home as
  directories. The Harness adapter owns narrowly justified operational exclusions; capture guards
  and backup use the same selection. The coordinator has no native file inventory. Other required
  directories remain explicit operator additions.
- **Confidentiality:** Include client settings and file-based credentials. Workspaces and native
  conversations already contain confidential data, so excluding recognized credential filenames
  cannot establish privacy. Retain stock restic encryption, scoped storage credentials, and the
  protected worker seed; no additional encryption layer or key service is needed.
- **Recovery:** Preserve client configuration and renew separately managed model authority. Keep
  native consistency, mutation-boundary and exact resource-ownership checks. Restored credentials
  retain their external validity limits; restoration does not rotate them.
- **Why:** Native directory ownership naturally includes new files and avoids a second configuration
  inventory in Dorf. Provider/Harness support still requires actual replacement and continuation
  proofs. A directory backup does not capture running processes or all workstation state.
- **Scope:** No new activation resource, profile redesign, automatic retention, or wider provider
  support claim. The [checkpoint contract](../../implementation/session-checkpoints.md) owns concrete
  adapter scope, operator custody and verification.
