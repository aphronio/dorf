# D125: Messages distinguish application observations

- **Applicability:** current
- **Areas:** client-api, harnesses, interaction
- **Read when:** Delivering asynchronous application updates without impersonating human input.
- **Decision history:** Accepted application observation input, 2026-09-14
- **Decision:** One immutable observation flag distinguishes application-generated text on existing
  Message admission. It requires explicit Follow, no attachments, and a supporting Harness.
  Scheduling, custody, replay, and user steering retain their existing semantics.
- **Adapter:** Codex uses `turn/start` with empty input and native `toolOutput`. An embedded
  AgentRun identity permits completed-input attribution on cold reads; Follow recovery continues
  to use its durable baseline. Public raw timelines omit observation payloads. Pi rejects the
  input before admission rather than assigning it human authority.
- **Why:** Task results, approval outcomes, and ingestion events can wake an idle Agent without
  pretending the human spoke. The caller owns their meaning and when to display results. No
  event bus, new delivery intent, transcript store, or notification policy belongs in Core.
- **Proof:** The pinned Codex 0.154.0 ran against a deterministic local Responses server. Idle
  output started a Turn, active output joined the existing Turn, model input retained the tool
  role, and both outputs survived process restart. Because active injection merges Turns and
  ignores `clientUserMessageId`, Dorf deliberately uses its established Follow boundary.
  PostgreSQL tests cover immutable replay and intent rejection; adapter tests cover cold input
  attribution, silent observations, and sanitized timelines. No paid model or personal connector
  was used in these proofs.
