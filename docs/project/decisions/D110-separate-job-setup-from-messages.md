# D110: Separate Job setup from Message delivery

- **Applicability:** current
- **Areas:** core, persistence, client-api
- **Read when:** Changing Job creation, workspace instructions, or Message admission.
- **Decision history:** Accepted, 2026-09-10.
- **Decision:** Create direct and workflow Jobs with execution configuration and resource custody.
  Admit all agent input through the ordinary Message API. Remove the Job goal, initial Message
  identifier, and admission-time AgentRun. The CLI may compose creation and Message delivery.
- **Why:** A conversation's first message has the same meaning and recovery requirements as every
  later message. Duplicating it as Job state forces clients and workflows to maintain two paths.
- **Ownership:** Clients own assistant guidance. Direct admission can install that guidance as
  workspace `AGENTS.md` before the Harness starts, using the existing Sandbox preparation effect.
  Workflows retain repository and outcome policy while consuming the same Message machinery.
- **Records:** Preserve existing Messages and native sessions. Remove the duplicate goal only after
  verifying that each retained original input remains in its Message.
- **Proof:** PostgreSQL tests cover empty setup, first and later Message delivery, replay, FIFO,
  retained Threads, workflow idle state, and cleanup. Native proof uses the deployed Codex profile.
