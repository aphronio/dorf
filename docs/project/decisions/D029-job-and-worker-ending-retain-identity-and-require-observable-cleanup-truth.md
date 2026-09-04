# D029: Job and Worker ending retain identity and require observable cleanup truth

- **Applicability:** historical
- **Areas:** core, workflows, sandboxes
- **Read when:** Reviewing the former Job and Worker ending, retention, and cleanup semantics.
- **Decision history:** Superseded by D047, 2026-08-06
- **Decision:** `job end NAME` closes new admission and requires settled prior input. It admits one
  stable cleanup input, requires its native turn to succeed, removes the exact Assignment workspace
  and Room-local reporting scope, ends the current Assignment, returns a caller-managed Worker idle,
  and retains the Job, goal, timeline, evidence, and native binding. `--interrupt` explicitly marks
  unsettled work interrupted and bypasses cooperative cleanup. `worker end NAME` requires no open Job,
  stops and destroys the exact current Room, clears its binding, and retains an ended Worker tombstone.
- **Retry and policy:** Ending and cleanup are separate durable facts. Workspace or Room cleanup
  failure remains `ending`/`cleanup-failed`; retry reconciles the same cleanup input and exact Room.
  Coding merge, explicit rejection, and abandonment force-end their Job. A dedicated coding Worker is
  then ended; a caller-managed Worker remains ready for another Job. Lifecycle policy is never
  inferred from its name.
- **Why:** Conversation success and PR state cannot prove process, workspace, credential, or VM
  cleanup. Retained identities preserve history and prevent accidental name reuse, while exact
  provider/workspace fencing prevents a retry from deleting another resource generation.
- **Reconsider when:** Keep/freeze Room behavior is implemented, permanent deletion/name reuse is
  required, or a harness exposes a stronger cooperative process-shutdown primitive.
