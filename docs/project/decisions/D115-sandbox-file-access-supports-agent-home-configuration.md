# D115: Sandbox file access supports agent home configuration

- **Applicability:** current
- **Areas:** core, client-api
- **Read when:** Changing Sandbox file paths, helper installation, or client credential delivery.
- **Decision history:** Accepted, 2026-09-11. Extends the file access boundary in D111.
- **Decision:** The existing authenticated file API accepts absolute Sandbox paths, `~/` paths
  relative to the Sandbox execution user's home, and workspace-relative paths. Reads and writes
  target exact regular files. Writes atomically replace the destination or preserve it for
  create-only setup, creating private parent directories when needed.
- **Why:** Clients need to install skills, helper scripts, and scoped credentials outside a Git
  checkout. Restricting public writes to workspace-root files forces staging scripts and artificial
  repository layouts even though the owned Sandbox already supplies the filesystem boundary.
- **Boundary:** The path resolves only inside the attested Job-owned Sandbox. Keep the existing
  authentication and cleanup fence. Reject non-canonical paths, symlinks, and non-regular files.
  Files use mode 0600 and newly created directories use mode 0700. The API does not execute scripts
  or expand arbitrary shell expressions. Clients own credential policy, expiry, and renewal;
  ordinary agent tools execute installed helpers. Sandbox file access does not conceal those files
  from the agent or trusted deployment Clients.
- **Proof:** File transport tests write and read exact binary bytes at nested, absolute, and home
  paths, inspect private permissions, preserve create-only defaults, and reject symlink parents
  before creating directories. API and control-reader tests retain authentication, size limits,
  exact Job custody, and cleanup fencing.
