# D027: Worker-addressed attachment is synchronous presence with implicit detach

- **Applicability:** historical
- **Areas:** interaction, sandboxes, core
- **Read when:** Reviewing the former synchronous Worker attachment and presence model.
- **Decision history:** Superseded by D047, 2026-08-06
- **Decision:** `worker attach NAME` resolves the Worker's exact current Room at invocation, records
  one transactional human-presence claim fenced to that Room, and opens a direct interactive shell at
  `/workspace`. It does not select the assigned Job workspace, pause native turns, or change Worker,
  Room, Job, Assignment, conversation, workspace, or branch identity. Exiting the shell with
  `Ctrl-D`, `exit`, or ordinary client interruption clears presence. A process-held advisory lock
  distinguishes a live owner from a stale row after an ungraceful crash; inspection reports the
  latter detached and the next attachment reclaims it. A second concurrent attachment fails
  honestly. Do not add a separate `worker detach` command until a concrete remote
  forced-detachment need exists; direct provider access remains the break-glass fallback.
- **Why:** The current local single-user workflow needs a simple door into the Room, not a resident
  terminal service or remote session-management protocol. Starting consistently at `/workspace`
  keeps attachment independent of Assignment state, while shell lifetime already supplies an honest
  presence boundary. A separate detach verb would require durable remote process handles and orphan
  reconciliation without serving an observed workflow.
- **Reconsider when:** A remote client must evict an attachment it does not own, multi-human presence
  becomes real, a non-Incus Environment cannot expose a synchronous terminal, or shell interruption
  cannot provide reliable presence cleanup.
