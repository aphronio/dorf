# D135: Package recovery releases verified resource custody

- **Applicability:** current
- **Areas:** persistence, sandboxes, deployment
- **Read when:** Executing package upgrades, recovering replacement VMs, or investigating held messages and upgrade failures.
- **Decision history:** Composes D133 resource custody and D134 delivery holds into the retained direct task, 2026-09-15.
- **Decision:** Record immutable package intent, checkpoint and recovery receipts, and the exact
  reserved replacement before provider mutation. Derive the next operation from those facts in the
  existing direct task. Commit verified active-resource selection, exact hold release, and the
  execution wake in one transaction under the Job effect fence.
- **Why:** A separate upgrade task would introduce competing lifecycle ownership. A local program
  counter loses recovery authority on restart. Switching the VM separately from releasing delivery
  can dispatch accepted input to an unverified resource or lose the wake after a successful switch.
- **Authority:** Absurd owns claims and retries. PostgreSQL owns package intent, custody, and effect
  receipts. Provider adapters attest the VM and checkpoint. Codex resumes exact retained Threads
  and reads settled Turn identities without starting verification Turns. Empty agent prose is not
  upgrade success. Native verification precedes release; a failed recovery retains the hold.
- **Cleanup:** Delete the obsolete source before releasing a replacement. Retain an E2B backing
  checkpoint until its replacement VM is deleted. Job cleanup reconciles an unacknowledged
  checkpoint before removing its source and includes all reserved resources and checkpoints.
- **Scope:** Operator-requested Codex package changes on direct Jobs, using pre-staged immutable Nix
  closures. Staging must respect the admitted network policy; no automatic package distribution or
  network-policy expansion is introduced. Workflow Jobs and automatic fleet rollout remain outside
  this boundary.
- **Proof:** PostgreSQL fault tests cover immutable replay, stale release, lost replacement response,
  exact ownership, rollback, and failed-wake atomicity. The retained-worker recipe exercises actual
  Codex with a local Responses fixture on disposable Incus and E2B VMs, including worker restart,
  state mutation, recovery, FIFO continuation, and dependency-aware cleanup. See the active plan for
  concrete evidence and limitations.
