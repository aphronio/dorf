# D074: Investigation drafts wait for exact human disposition

- **Applicability:** partial
- **Areas:** workflows, interaction, harnesses
- **Read when:** Changing investigation draft revision loops, Harness Thread reuse, or disposition policy.
- **Decision history:** Superseded before release by D075 — 2026-08-20
- **Retained finding:** Numbered typed Markdown drafts, follow-up AgentRuns, and the exact Harness Thread
  are useful workflow mechanisms. Immediate cleanup after the first draft destroys valuable revision
  context.
- **Superseded finding:** Persisting accept/reject decisions and letting them choose cleanup moved
  interaction policy into the wrong layer. D075 is the current authority; Git history retains the
  discarded design details.
