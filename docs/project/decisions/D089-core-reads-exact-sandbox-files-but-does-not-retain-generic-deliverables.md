# D089: Core reads exact Sandbox files but does not retain generic deliverables

- **Applicability:** current
- **Areas:** core, sandboxes, persistence
- **Read when:** Changing Sandbox file reads, deliverable retention, or cleanup-time file access.
- **Decision history:** Accepted boundary reversal — 2026-08-22
- **Decision:** Expose one exact `SandboxHandle.ReadFile` operation for a caller-named, clean
  workspace-relative regular file from the exact Job-owned Sandbox. The read preserves arbitrary
  bytes, runs under the Job cleanup fence, and rejects traversal, symlinks, resolved workspace
  escapes, directories, and non-regular entries. Multiple files are repeated reads. Discovery,
  listing, stat, globbing, archives, batch reads, and directory downloads remain compositions over
  the existing Sandbox command seam rather than new Core filesystem APIs.
- **Custody:** Core does not prescribe an output path, change stock agent behavior, scan files,
  interpret output, or retain generic deliverables. A caller or workflow must know or discover the
  path and read the file before requesting cleanup. Requested cleanup closes reads; Sandbox deletion
  makes the bytes unavailable. D092 applies that boundary to Investigation: the workflow chooses
  `REPORT.md`, but neither Core nor the workflow retains its bytes.
- **Storage:** Remove the generic Artifact domain, PostgreSQL table and queries, Core adapters, and
  `dorf artifact` CLI. The content-addressed blob store remains only for Evidence. No migration
  preserves the pre-release Artifact shape.
- **Reconciliation:** This supersedes D072, refines D069 and D075's investigation Draft
  representation, and replaces D088's automatic retention design. The distinct Evidence proof
  boundary remains unchanged. D070's mandatory profile contract advances
  to `base-2` so every admitted profile has functionally proved exact binary file reads. The North
  Star remains the authority for native workflows as non-privileged Core consumers.
- **Why:** Automatic retention invented output directories, naming and collision policy,
  post-processing, cleanup obligations, and agent-facing conventions before a consumer proved those
  abstractions. Exact caller-selected reads preserve the reusable authority and byte transport while
  letting workflows own discovery, meaning, and any durable typed result.
- **Reconsider when:** A real consumer cannot satisfy a proven need by reading exact files before
  cleanup or recording a workflow-owned typed result, and can specify the durable generic custody,
  lifetime, naming, size, and recovery contract without changing stock agent behavior.
