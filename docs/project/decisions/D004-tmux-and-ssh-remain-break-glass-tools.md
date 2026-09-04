# D004: Tmux and SSH remain break-glass tools

- **Applicability:** current
- **Areas:** harnesses, sandboxes, deployment
- **Read when:** Changing operational access, inspection, or takeover for a Sandbox.
- **Decision history:** Accepted — 2026-07-22; built-in tmux runner removed at resource cutover — 2026-07-27
- **Decision:** Keep SSH/direct Room access and manually started tmux available for operational
  takeover alongside the semantic harness driver. Do not make a resident tmux process part of
  durable identity or retain an unused built-in tmux runner.
- **Why:** Agent-native history does not replace the ability to inspect the VM, recover a stuck
  process, open a shell, or repair work when Dorf's control path fails.
- **Reconsider when:** Another proven operational mechanism offers equally simple and reliable local
  observation and takeover.
