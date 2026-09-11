# D111: Workspace files own live Codex instructions

- **Applicability:** partial
- **Areas:** core, client-api
- **Read when:** Changing workspace instruction editing or Codex context loading.
- **Decision history:** Accepted, 2026-09-10. D115 broadens the generic file API beyond workspace-root writes.
- **Decision:** Keep current instructions in the Job-owned workspace. Add one bounded root-file
  write to the existing authenticated Sandbox file API, including create-only initialization.
  Reuse the existing provider transport, ownership checks, and cleanup fence.
- **Codex:** Discover AGENTS.md natively for new threads. When its bytes change, append a native
  developer notice asking for a complete read. Inject complete SOUL.md developer context initially
  and on changes. Empty and missing files clear their prior instructions. Cache only hashes in the
  worker; rebuild from files after losing that cache. Use native thread injection on both
  the pinned CLI and the current verified CLI.
- **Why:** The client editor and the agent must read and write the same files. Database copies,
  accepted-message snapshots, and content-addressed instruction artifacts create competing owners.
  The next turn reads the current workspace; admission time does not freeze instruction bytes.
- **Boundary:** Core owns generic file access. The Codex adapter owns native context behavior.
  Clients own defaults, personality, user identity, and the editor. No shared per-user mount,
  SFTP service, new instruction resource, or instruction field on Messages is required.
- **Records:** Keep applied migrations in sequence. A forward migration removes experimental
  Message instruction storage after preserving its local synthetic proof records. Existing Jobs,
  Messages, and native histories remain intact.
- **Proof:** File transport tests exercise real atomic writes, create-only retries, empty files,
  and rejected escapes. API tests cover authentication, limits, exact custody, and cleanup. Native
  integration receipts belong to the client exercise; deployed provider verification remains a
  separate gate.
