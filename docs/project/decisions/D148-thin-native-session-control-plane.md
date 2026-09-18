# D148: Thin native Session control plane

- **Applicability:** current
- **Areas:** core, harnesses, client-api
- **Read when:** Changing input custody, native observation, Session storage, or the Harness adapter.
- **Decision history:** Replaces durable Message custody as the product direction in D055, D096,
  and D126; retains the Session identity and Thread binding established by D145/D146, 2026-09-18.
- **Decision:** Dorf provides a stable API over supported Harness input, controls, events, and
  history. Dorf owns Session configuration, native binding, compute, authority, maintenance,
  supported recovery, and release. The Harness owns messages, Turns, input placement, and execution.
- **Input contract:** Ordinary send succeeds on confirmed native acceptance. Preparing or
  unavailable Sessions do not enqueue input for eventual delivery. Expose ambiguous outcomes;
  never treat native correlation as idempotency or native acknowledgement as persisted history.
- **Storage:** No independent durable Message inbox or Turn aggregate is required merely to expose
  native work. Retain lifecycle facts for actual ownership and recovery needs. Native history
  availability follows its verified storage/recovery contract, including after resource release.
- **Why:** The old Follow/Steer/Auto controller duplicates native input placement and makes clients
  reconcile Dorf delivery states around native execution. Codex already supplies start-or-steer,
  native history, and execution controls. The control plane should manage access to those capabilities.
- **Evidence:** Pinned Codex source and an isolated real-server probe confirm start-or-steer,
  completed history after restart, absence of duplicate suppression by client input ID, and an
  acknowledged-but-unpersisted steering window. The
  [capability review](../../implementation/native-session-contract.md) owns exact evidence and limits.
- **Transition:** The uncommitted shared-Turn implementation and migration are withdrawn. Current
  Message endpoints and queued execution remain implemented until their coordinated replacement
  slice. Do not describe the new send contract as shipped or delete maintenance/recovery guards
  whose current implementation depends on Messages. No deployment change is part of this decision.
- **Scope:** Codex is the implementation focus. Further Harnesses, native queues, new configuration
  resources, controller replacement, and a shared guest service require their own concrete evidence.
- **Authority:** [North Star](../north-star.md#product-boundary),
  [architecture](../architecture.md#accepted-native-boundary), and
  [slice tracker](../../implementation/session-product-proposals.md).
