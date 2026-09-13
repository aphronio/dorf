# D123: Message timelines project completed native replies

- **Applicability:** current
- **Areas:** client-api, harnesses, interaction
- **Read when:** Changing live assistant reply delivery, retained item identity, or input attribution.
- **Decision history:** Accepted completed Message timeline projection, 2026-09-13
- **Decision:** A client addresses one admitted Message to read completed inputs and final assistant
  replies from its exact native Turn. Core resolves the stored AgentRun and owned Sandbox under the
  cleanup fence. The Harness adapter selects completed native conversation items and returns their
  original order. Dorf retains no additional transcript.
- **Identity:** Clients identify an entry by retained Harness Thread, Turn, and its index among
  completed inputs and final replies. Native item IDs remain diagnostic references. The verified
  Codex history modes preserve that completed prefix across active, terminal, and cold reads.
  Changing history mode or rewriting the retained session is outside that identity promise.
- **Input attribution:** A native input receives a Dorf Message ID only when its client identity
  matches a stored AgentRun in the same Job, Sandbox, Harness Thread, and Turn. This establishes
  input origin. It does not claim that a particular assistant reply answers one particular input.
- **Why:** An assistant item can finish while the native Turn remains active after steering.
  Waiting for terminal Message results delays those replies and combines distinct messages.
  Consumers should not parse raw Harness objects or mistake changing native IDs for durable keys.
- **Scope:** The Message endpoint reuses passive timeline reads and their existing bounds. It adds
  no event subscription, session storage, pagination cursor, or automatic notification policy.
  Existing raw Job timelines and terminal Message results keep their contracts. Clients own
  delivery deduplication and retention after Sandbox cleanup.
- **Extends:** D119 with a normalized completed-item projection and verified input origin.
- **Proof:** Captured Codex 0.154.0 legacy, paginated, and default history reads cover a completed
  final reply before Turn completion and the same completed prefix after reconnect. Protocol tests
  replay those snapshots. PostgreSQL integration verifies exact Message custody, passive reads, and
  serialization with cleanup.
