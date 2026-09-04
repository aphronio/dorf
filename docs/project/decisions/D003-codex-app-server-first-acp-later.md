# D003: Codex app-server first; ACP later

- **Applicability:** current
- **Areas:** harnesses, model-access
- **Read when:** Changing the Codex harness protocol or considering ACP support.
- **Decision history:** Accepted — 2026-07-22
- **Decision:** Codex is the first interactive agent. Use a thin driver over authenticated Codex
  app-server WebSocket; do not add ACP before a second interactive harness driver is required.
- **Why:** App-server exposes Codex-native threads, turns, approvals, history, interruption, and live
  updates directly. A thin driver keeps its experimental protocol replaceable without prematurely
  creating an agent-plugin framework.
- **Reconsider when:** App-server cannot satisfy reconnect or security needs, or a concrete second
  interactive agent such as Claude Code or Kimi CLI enters the supported coding workflow. One-shot
  reviewer commands do not meet this trigger.
