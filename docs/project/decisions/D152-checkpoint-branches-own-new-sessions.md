# D152: Checkpoint branches own new Sessions

- **Applicability:** current
- **Areas:** core, sandboxes, client-api
- **Read when:** Changing checkpoint restore ownership, branch admission, preparation holds, or destination cleanup.
- **Decision history:** Accepted 2026-09-23; extends D138 checkpoint custody and D148's native Session boundary.
- **Decision:** An authorized immutable checkpoint may seed a new Session. The branch has its own Sandbox resource, repository namespace, route authority, native revision, and cleanup. The source Session and checkpoint remain untouched. A stable request identity returns the same destination or rejects conflicting input.
- **Preparation:** Restore runs in the destination with read-only authority to the source repository while native access is held. The client can replace restored file-based credentials and prepare its environment through bounded file operations. A release request installs destination-scoped model authority, verifies native Thread resume, and opens native access only after verification.
- **Recovery:** Use the existing resource and task custody machinery. Unknown provider outcomes reconcile against the reserved destination. Source replacement and branching share the same restore effect; branching does not relax replacement's native-safety guard.
- **Package boundary:** Reject checkpoints whose effective package generation is represented by a source-owned upgrade receipt until immutable destination-owned package provenance is supported. Treating that generation as the destination base would make later checkpoints incorrectly recoverable.
- **Why:** A new Session is the smallest existing ownership unit that lets a restored conversation continue independently without rewinding live work. A hold prevents restored credentials or a Harness process from running before the client establishes branch-specific authority.
- **Scope:** The initial command is an operator surface for the configured Codex/E2B base-package checkpoint profile. Process memory, external services, application snapshots, and controlled time remain client concerns. The [checkpoint contract](../../implementation/session-checkpoints.md#restore-into-a-new-session) owns the exact support and proof status.
