# D030: Recovery does not replace a lost Room without restorable native history

- **Applicability:** historical
- **Areas:** core, sandboxes, harnesses
- **Read when:** Reviewing the former restriction against replacing a lost Room without restorable native history.
- **Decision history:** Superseded by D047, 2026-08-06
- **Decision:** `worker recover NAME` restores and reconciles only the exact recorded Room while its
  provider body and disk survive. Provider-confirmed absence marks that Room absent, clears the
  Worker's current Room, and leaves the Worker offline. Dorf retains Worker, Job, Assignment,
  goal, documents, queued input, and typed native bindings, but does not provision a replacement
  Room, roll Assignment generation, rebuild a coding clone, or start a fresh native thread and call
  it continuity. `job end --interrupt` may acknowledge proven Room loss without attempting native or
  Room-local cleanup; the Roomless Worker can then be ended.
- **Why:** Authenticated replacement dogfooding preserved control-plane IDs but Codex could not load
  history stored only in the deleted VM. Continuing would therefore require distributed transcript
  or harness-state storage, harness-supported history restoration, or an explicit new-conversation
  handoff protocol. Those mechanisms are disproportionate at the current local coding-to-PR stage,
  and replacement without them creates misleading continuity plus Assignment, workspace, reporting,
  credential, and retry complexity.
- **Compatibility:** This narrows D025's currently implemented replaceable-Room posture and supersedes
  D028's absent-provider replacement path. Same-Room restoration, process recovery, native-turn
  reconciliation, durable offline inspection, and queued admission remain supported.
- **Reconsider when:** Codex or another current harness can restore native history independently of
  Room disk, Dorf deliberately owns sufficient conversation state, or a concrete workflow needs
  and validates an explicit handoff into a new native conversation.
