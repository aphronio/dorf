# D151: Preserve native assistant phase at the client boundary

- **Applicability:** current
- **Areas:** harnesses, client-api, interaction
- **Read when:** Changing completed assistant items, native phase handling, or client presentation responsibility.
- **Decision history:** Accepted 2026-09-23; revises D150's phase-flattened reply projection.
- **Decision:** Project a completed native `agentMessage` as `assistant_message` with its original `commentary` or `final_answer` phase, text, native item identity, and completed-item index. Preserve an absent native phase as absent. Exclude empty messages and unknown phases from the completed-item projection.
- **Why:** The Harness supplies the phase. Relabeling both phases as an undifferentiated reply discards information clients need to interpret and present the message. Dorf translates native evidence without deciding whether a message is a progress update or closeout.
- **Boundary:** Clients decide publication, channel selection, presentation, and durable receipts. The existing completed-item observation path carries both phases; no separate commentary stream or Dorf delivery system is added.
- **Compatibility:** The public completed-item kind changes from `reply` to `assistant_message`. Coordinate client and Dorf upgrades; no duplicate kind is retained. Completed-item indexes do not change relative to D150.
- **Proof:** Codex active and cold reads and live completion tests preserve phase; API observation and client integration prove the same item reaches the application boundary.
