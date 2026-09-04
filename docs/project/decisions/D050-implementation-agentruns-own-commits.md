# D050: Implementation AgentRuns own commits

- **Applicability:** current
- **Areas:** workflows, harnesses
- **Read when:** Changing commit ownership or Revision handoff for implementation AgentRuns.
- **Decision history:** Accepted workflow correction — 2026-08-10; terminology clarified by D052 and D055
- **Decision:** An implementation AgentRun owns the code change and, when it changes
  code, creates one or many commits in the Job checkout. Its successful change contract requires a clean
  checkout and a final `HEAD` that is a proper descendant of the AgentRun's input Revision. Dorf
  validates those facts and records the observed `HEAD` as the next exact Revision; it does not
  manufacture, squash, or amend the agent's commits. A follow-up Message may instead be handled with
  a clean unchanged `HEAD`; that creates no new Revision.
- **Action boundary:** An Action is a code-owned external mutation such as creating a Sandbox or
  route, pushing Git, or publishing a pull request. An AgentRun owns submission and recovery of its
  harness Turn directly; it is not paired with an Action. Tool calls and commits made inside that
  AgentRun are also not separate Actions. Their transcript remains harness-owned, their commits
  remain Git-owned, and Dorf retains the observed Revision and Revision-pinned Evidence it needs.
- **Why:** Commit structure is part of implementation judgment and may naturally require more than
  one commit. Treating commit creation as a later deterministic Dorf step both erases that judgment
  and misstates the recovery boundary. Validating clean descendant Git state gives the workflow an
  exact handoff without making Dorf the author of the change.
- **Reconsider when:** A supported harness cannot reliably commit inside the Sandbox, or a concrete
  acceptance surface requires a separately reviewed normalization step that preserves authorship
  and exact source-Revision provenance.
