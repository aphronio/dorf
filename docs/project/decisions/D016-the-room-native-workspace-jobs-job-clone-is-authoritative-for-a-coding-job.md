# D016: The Room-native `/workspace/jobs/JOB` clone is authoritative for a coding Job

- **Applicability:** current
- **Areas:** workflows, sandboxes, persistence
- **Read when:** Changing where a coding Job's authoritative checkout lives.
- **Decision history:** Retained pre-consolidation decision.
- **Decision:** The Room-native `/workspace/jobs/JOB` clone is authoritative for a coding Job.
- **Why:** The host checkout is orchestration context only; each Job is an independent clone, not a worktree or the Worker-general `/workspace` directory.
- **Reconsider when:** An environment without a durable native filesystem proves another workspace model necessary.
