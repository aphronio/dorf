# D005: The agent owns conversation history

- **Applicability:** current
- **Areas:** core, harnesses, persistence
- **Read when:** Changing transcript storage, replay, or ownership of queued conversation input.
- **Decision history:** Accepted — 2026-07-22; controller-owned input boundary clarified by D022 — 2026-07-26
- **Decision:** Codex remains authoritative for its transcript, turns, tool items, and context
  management. Dorf stores native IDs plus the lifecycle, run, workflow, and cleanup facts it
  owns; it does not duplicate the full transcript in SQLite or documents. Pinned goals and
  queued human/client messages are Dorf-owned control inputs required for durable delivery, not
  copies of agent-owned history.
- **Why:** Duplicating history creates synchronization and compatibility problems without serving the
  current coding workflow.
- **Reconsider when:** A real client needs lossless replay that agent-native history cannot supply, or
  history must remain available after environment destruction.
