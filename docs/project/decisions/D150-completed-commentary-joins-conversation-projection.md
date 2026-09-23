# D150: Completed commentary joins the conversation projection

- **Applicability:** partial
- **Areas:** harnesses, client-api, interaction
- **Read when:** Changing native assistant message visibility or completed Turn observations.
- **Decision history:** Accepted 2026-09-23; extends D123's final-answer-only projection. D151 preserves the native assistant phase in the public item.
- **Decision:** Project completed Codex `commentary` and `final_answer` agent messages as ordered assistant replies in native Turn observations. Keep empty messages and unknown phases excluded. The raw native timeline retains each original phase.
- **Why:** Commentary is user-directed progress and can carry a time-sensitive message. Excluding it lets an application observe a successful Turn while publishing no user-visible message, even when the model spoke during the Turn.
- **Boundary:** Dorf reports completed items as they arrive. The client decides which channels receive them and owns durable publication and delivery receipts. Dorf does not infer that a projected message reached a person.
- **Compatibility:** Completed-item indexes include commentary after this change. Drain in-flight Turns before upgrading the projection; a client cursor saved under the earlier final-only projection cannot be interpreted against the new prefix.
- **Proof:** Protocol tests cover live completed-item order and active, terminal, and cold native history. Client integration must verify publication before Turn completion and channel enqueue.
