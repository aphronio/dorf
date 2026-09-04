# D002: Workflow, runtime, agent driver, and environment seams

- **Applicability:** historical
- **Areas:** core, workflows, sandboxes
- **Read when:** Reviewing the responsibility model that preceded the Go control plane and its current domain boundaries.
- **Decision history:** Superseded by D047, 2026-08-06
- **Decision:** Responsibility flows from the coding workflow through the durable Worker, Room, Job,
  and Assignment lifecycle to an agent driver operating through an Environment adapter. This is not
  a package-import graph; D019
  records that boundary.
- **Why:** The current runner mixes GitHub, repository, Incus, tmux, and Codex concerns. Separating
  them lets the coding workflow dogfood reusable primitives without leaking PR policy into agent or
  environment implementations.
- **Reconsider when:** A working vertical slice demonstrates a smaller interface with fewer concepts
  and no workflow leakage.
