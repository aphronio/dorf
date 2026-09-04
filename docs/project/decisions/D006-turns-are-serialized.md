# D006: Turns are serialized

- **Applicability:** historical
- **Areas:** core, interaction, persistence
- **Read when:** Changing concurrent-turn rules or ordered delivery within one conversation.
- **Decision history:** Superseded by D022 — 2026-07-26
- **Decision:** One turn may actively mutate an agent session at a time. Later messages are delivered
  sequentially. The original decision deferred a durable FIFO until a concrete workflow needed to
  accept a message during active work; #125 supplied that requirement.
- **Why:** Concurrent turns against one context have ambiguous ordering and workspace effects.
- **Reconsider when:** A supported agent has defined concurrent-turn semantics and a real workflow
  benefits from them.
