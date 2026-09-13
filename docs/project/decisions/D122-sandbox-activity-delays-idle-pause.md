# D122: Sandbox activity delays idle pause

- **Applicability:** current
- **Areas:** core, sandboxes, client-api
- **Read when:** Changing idle pause timing, managed Sandbox access, or cancellation fences.
- **Decision history:** Accepted — 2026-09-13; revises D120
- **Decision:** A Job becomes eligible for automatic pause after one minute without native activity.
  The existing durable wait loop applies the policy. Pending, active, and uncertain AgentRuns and
  the admitted `keep_running` override still prevent pause.
- **Ownership:** Core retains one Job activity timestamp. File access, managed commands, and native
  Harness reads record completion under the existing Job effect fence. Passive status and empty
  work polls leave activity unchanged. Providers still own actual power state and pause support.
- **Why:** A completed Turn often needs an immediate file read to deliver its output. A short grace
  period avoids pausing and resuming the same Sandbox for that read. The existing wait loop is
  sufficient; the policy needs no timer service, activity journal, or process census.
- **Recovery:** Access clears the timestamp before an external operation and records the database
  clock after completion, including failure and cancellation. The fence survives request
  cancellation until the callback finishes. If the host dies or the completion write fails, the
  next eligible idle check starts a fresh grace period. Remote processes left behind by a crashed
  command have no independent durable activity custody.
- **Proof:** PostgreSQL tests exercise elapsed grace, unchanged idle polls, restart after a missing
  completion write, failed access, and cancellation racing with pause. Existing pending-work and
  admission race coverage remains in force. Provider pause capability remains separately verified.
