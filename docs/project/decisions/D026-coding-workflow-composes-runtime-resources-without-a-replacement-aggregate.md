# D026: Coding workflow composes runtime resources without a replacement aggregate

- **Applicability:** historical
- **Areas:** workflows, core, github
- **Read when:** Changing coding workflow ownership or considering a second orchestration aggregate beside core resources.
- **Decision history:** Superseded by D047 — 2026-08-06
- **Decision:** A coding slice creates deterministic Worker `coder-JOB` with explicit
  `coding-workflow` provenance and `dedicated` lifecycle policy, then creates one exact-goal Job and
  Assignment. Workflow-owned SQLite tables are keyed by Job and contain only repository, branch, PR,
  command, review, AFK, and terminal workflow facts. They do not duplicate current Worker, Room,
  Assignment, conversation, input, turn, or runtime lifecycle state into a renamed coding Session.
- **Workspace and turns:** Coding repositories are independent clones at `/workspace/jobs/JOB`, never
  worktrees or the Worker-general root. Repo setup and checks are workflow-observed commands;
  implementation, repair, and follow-up are messages on the Job FIFO and native conversation. Review
  agents remain workflow-owned one-shot commands rather than durable Workers.
- **Terminal policy:** PR creation, passing checks, successful turns, and Worker completion claims do
  not close runtime resources. Merge, explicit rejection, and abandonment are terminal coding
  workflow conditions; resource ending and Room cleanup remain the explicit lifecycle operation
  owned by #129. Revision preserves the Job, Assignment, Worker, Room, clone, branch, and PR.
- **Credentials:** Repository clone and Job-workspace fetch/push use short-lived GitHub App
  installation tokens passed through the current Assignment/Room execution boundary. Before a Job or
  Room exists, the controller may use a separately minted installation token to read/create the
  coding branch ref and fetch only the recorded remote base object into the orchestration checkout so
  it can pin the exact base; recovery uses the same scoped ref API. GitHub PR API calls likewise use
  controller-side installation tokens for the recorded repository. No path borrows ambient
  controller Git credentials or credential stores.
- **Compatibility:** The superseded Session runtime, tables, dispatchers, top-level L1 commands, and
  coding-session compatibility paths are deleted, not wrapped. Experimental development state is
  reset once out of band; no destructive migration is shipped.
- **Why:** Keeping a second aggregate would restore the ownership ambiguity D025 removed and permit
  runtime and workflow state to disagree. Explicit provenance prevents lifecycle policy from being
  inferred from the `coder-` name, while Job-keyed workflow facts preserve coding-specific recovery
  without leaking repository policy into the runtime.
- **Reconsider when:** A second workflow demonstrates a concrete shared orchestration record that
  cannot be represented by resource identity plus workflow-owned facts, or coding tasks need a
  deliberately reviewed Worker-sharing policy.
