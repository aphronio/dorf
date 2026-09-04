# D023: Job document plane; SQLite operational authority

- **Applicability:** historical
- **Areas:** persistence, core
- **Read when:** Reviewing the replaced split between Job documents and SQLite operational authority.
- **Decision history:** Superseded by D047, 2026-08-06
- **Decision:** Use the external Job directory for durable material that humans or agents should
  consume through ordinary file tools: the pinned goal and, after reviewed boundaries land,
  approved context, claims, and evidence. Use SQLite as the authority for transactional operational
  state including Worker, Room, Job, Assignment, conversation, FIFO admission, and delivery
  indexing. Do not maintain mutable copies of the same state in both surfaces. Native conversation
  history remains harness-owned and coding deliverables remain Git/GitHub-owned.
- **Why:** Documents benefit from `grep`, editors, shell tools, diffs, and bounded read-only exposure
  to a Room. Concurrent message admission and lifecycle transitions benefit from database
  transactions and programmatic queries; representing them as JSON merely because SQLite is also a
  file adds locking, crash-recovery, and synchronization code without helping an agent consume the
  data.
- **Room boundary:** The external Job document directory is never writable-mounted into the Room.
  Approved documents may be projected inward read-only. Worker-authored changes leave the
  Room only through the reviewed validation and ingestion boundary reserved for #126.
- **Compatibility:** Existing duplication between `job.json` and SQLite is refactorable evidence,
  not a schema promise. Remove duplication incrementally when a working slice owns the affected
  lifecycle behavior; do not expand this message slice into a broad Job-state rewrite.
- **Reconsider when:** Agents concretely need direct file-tool access to operational state, a
  multi-host control plane replaces SQLite, or a smaller single authority can preserve both
  transactional correctness and the document-tool experience.
