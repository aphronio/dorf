# D001: Durable logical agent session

- **Applicability:** historical
- **Areas:** core, harnesses, sandboxes
- **Read when:** Reviewing how durable agent identity was modeled before Jobs and Sandboxes replaced logical sessions.
- **Decision history:** Superseded by D025 — 2026-07-27
- **Decision:** The durable product identity is a logical agent session bound to an isolated
  environment and an agent-native conversation identity. OS processes and terminal panes are
  replaceable operational details.
- **Why:** Process identity cannot survive crashes, hibernation, or different environment providers,
  while the user needs stable conversational and workspace continuity.
- **Reconsider when:** A supported agent cannot resume native context and a resident process must
  become an explicit part of that driver's behavior.
