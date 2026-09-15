# Plan: Package upgrades in persistent Sandboxes

Status: in progress, started on 2026-09-15 after the profile-revision slice in
[D132](../project/decisions/D132-profile-revisions-separate-promotion-from-job-custody.md).
This plan records implementation scope; it does not describe shipped behavior.

## Goal and scope

Upgrade runner packages such as Codex inside existing persistent Incus and E2B Sandboxes while
preserving the user's conversation. If installation or initial verification fails, restore the
pre-upgrade environment, including application data that the new version may have migrated.

Keep the first version small: one upgrade at a time per Sandbox, one checkpoint, a bounded
verification step, and rollback before reopening message delivery. Profile promotion selects
images for future Jobs; this follow-up updates packages inside existing VMs. Retain the original
profile revision and record the package upgrade separately so diagnostics explain both.

## High-level flow

```text
Upgrade requested
        |
Hold delivery; keep saving incoming messages
Finish current turn
        |
Checkpoint VM
        |
Install package upgrade
        |
Verify existing conversation resumes
        |
        +-- PASS ----------------------> Resume queued messages
        |
        +-- FAIL --> Restore checkpoint
                            |
                            +-- PASS --> Resume queued messages
                            |
                            +-- FAIL --> Keep queue; needs attention

Every transition/error --> Logfire, linked by upgrade_id
```

If checkpoint creation fails, do not install the upgrade. Preserve or restart the old runner and
report the failed upgrade attempt. A pause or empty final output is never proof of completion.

## Packages and writable state

- Prototype a minimal pinned Nix package generation on the existing Linux image. Decide the final
  installation mechanism from that proof; no NixOS conversion or custom package manager is needed.
- Nix generations select binaries and dependencies. They do not roll back mutable sessions,
  databases, or other runner data. The VM checkpoint supplies the matching state rollback.
- Stop the runner at a quiet turn boundary before capture. Confirm that the snapshot includes all
  actual writable runner state, rather than assuming one conventional `.codex` path contains it.
- Keep one pre-upgrade checkpoint through verification. Establish a simple retention/cleanup rule
  once the live proof establishes its cost.
- Automatic rollback ends when new real work is allowed. Restoring an older checkpoint afterwards
  could discard conversation or workspace changes; that is an explicit recovery operation.

## Provider behavior

| Provider | Checkpoint and recovery |
| --- | --- |
| Incus | Cleanly stop the VM, snapshot it, boot and upgrade. On failure, stop it, restore the snapshot, and restart the same instance. |
| E2B | Stop the runner while leaving the sandbox running, then create a snapshot. On failure, create a replacement sandbox from that snapshot and reconnect it to the same logical Dorf Sandbox and conversation. |

E2B's provider sandbox ID changes on replacement. Implement explicit ownership, provider binding,
route reconnection, and old-resource cleanup; a normal pause/resume does not implement this
rollback. Keep the old and replacement resources from both processing queued work. Reconcile a
lost provider acknowledgement before retrying creation or restore.

The live E2B adapter proof established that the provider refuses to delete a snapshot while a
running replacement uses it. After rollback, retain that checkpoint as a dependency of the new
resource until the resource is deleted. A successful upgrade without replacement can release its
checkpoint. Incus does not require retaining a restored snapshot as the running VM's image.

### Stable logical Sandbox, replaceable provider resource

Keep the Job and logical Sandbox IDs stable. Bind each logical Sandbox explicitly to its active
provider resource. Retain an append-only replacement history, accessible through Job inspection,
with the previous and replacement provider IDs, upgrade ID, checkpoint reference, reason,
verification timestamps, and cleanup outcome. Incus restore records the same resource ID on both
sides; E2B restore records a new one. Do not duplicate this history on the Job itself.

Persist replacement intent before creating a VM. Exact resource ownership must distinguish the
old VM from the replacement while both exist; broad owner discovery must not choose arbitrarily.
Hold delivery until verification succeeds and the active binding is committed. Preserve the old
resource's identity after cleanup so operators can correlate execution and Logfire records.

Check mounted storage coverage during the provider proof: Incus instance snapshots do not include
separately attached custom volumes. Local snapshots cannot undo external actions, so upgrade
verification must not send email or perform other real external mutations.

## Accepted messages and friendly status

Accept and durably save incoming messages while delivery is held. Preserve their IDs and ordering
and resume normal delivery automatically after successful upgrade or recovery. A queued message
must not be reported as delivered until the runner actually accepts it.

Expose explicit internal upgrade status and a queue wait reason such as `workspace_upgrade`.
Persist the recovery facts needed to derive that status and gate; avoid a second independent
workflow state machine. Refreshing the page or restarting the controller must not lose the hold.

Clients can render a nonblocking maintenance banner from the structured status.
Product-specific copy and UI behavior belong in the consuming application.

## Logfire observability

Assign a stable `upgrade_id` that survives retries and controller restarts. Correlate all upgrade
events and spans with Job ID, logical Sandbox ID, provider sandbox IDs, provider, old/new package
versions or generations, and checkpoint reference. Include durations and sanitized error details.

Record request acceptance, delivery hold, checkpoint creation, installation, verification,
rollback, provider replacement, checkpoint/resource cleanup, and delivery resumption. Retain a
clear terminal outcome: upgraded, rolled back, or needs attention; distinguish an attempt aborted
before installation. Report recovery errors as well as the original upgrade error.

Do not log credentials, private action tokens, or conversation/session contents. Durable control-plane
facts own recovery and UI status; Logfire owns the diagnostic trail. Telemetry availability must
not determine whether the system can restore a checkpoint.

## Implementation and verification order

Progress as of 2026-09-15:

- Profile promotion and resource/delivery-hold foundations are implemented.
- Repeatable [package/provider recipe](../../scripts/runtime-upgrade/README.md) passes on Incus and
  E2B using Dorf's Go checkpoint adapters: package activation, nonempty native replies, original
  context, exact local-state restoration, retry identity, and resource/checkpoint cleanup.
- Resource foundation implemented: separate records, active binding, immutable attested locator,
  migration preserving ownership, Job inspection history, and retained deletion receipts.
  Checkpoint authority/fault tests, live proofs, and the full deterministic gate pass.
- Delivery-hold foundation implemented for direct Jobs: durable admission barrier, FIFO release
  with an atomic execution wake, stale-release protection, idle/access exclusion, and cleanup.
  `mise run integration:delivery-hold` verifies storage, public waiting status, and an actual
  Absurd worker restart with exactly one native submission per queued Message.

- Retained direct-task coordinator implemented for pre-staged Codex packages: operator admission,
  checkpoint/recovery receipts, exact native quiescence and retained-Thread verification, atomic
  replacement binding and release, replacement/backing-checkpoint cleanup, and correlated telemetry.
- Distribution remains operator-managed. Production delivery of closures to restricted-network
  guests and automatic fleet rollout remain unimplemented; no Job network policy is weakened.
  The local Responses fixture does not prove a real Provider Gateway reconnection.

### Verified provider evidence

The 2026-09-15 adapter proofs retained a native Codex conversation through version activation and
rollback, restored its exact pre-upgrade local-state digest, retried capture/replacement without
duplication, and completed owned resource/checkpoint cleanup:

| Provider | Proof ID | Provider resource before / after rollback |
| --- | --- | --- |
| Incus | `upgrade-proof-701729244a19` | Same instance, named after the proof ID |
| E2B | `upgrade-proof-f8124c50519f` | `iurz4xun9cg77bli8j8fh` / `iv18dmd4ezji4rygyt36x` |

Logfire ingestion is verified: 33 events per proof, including injected verification failure,
rollback, proof success, and completed cleanup, with the expected provider identities. Query
`dorf.upgrade_id` in the 2026-09-15 11:33–11:41 UTC window. An initial query-service outage returned
503; a later read confirmed the submitted records. No event re-export was needed.

The repeatable lost-checkpoint-response probes also passed their expected-failure assertions:
Incus `upgrade-proof-4b379a575785` and E2B `upgrade-proof-a34067e7ec65`. Both retained the source
after discarding the accepted checkpoint response, reported failure, and then completed the
standalone cleanup recovery recipe. Logfire contains 23 events per probe, including checkpoint
failure and completed cleanup recovery, in the 2026-09-15 12:12–12:18 UTC window. These prove
recipe recovery and provider reconciliation, not restart recovery of a durable upgrade executor.

### Retained-worker coordinator evidence

The final 2026-09-15 worker recipes passed activation, explicit post-migration verification failure,
rollback, original native context, substantive queued replies, exactly one model request per input,
and coordinated resource/checkpoint cleanup:

| Provider | Proof ID | Rollback provider resource before / after |
| --- | --- | --- |
| Incus | `worker-upgrade-incus-1789479802` | `dorf-182aa933cd365c023e38` on both sides |
| E2B | `worker-upgrade-e2b-1789479809` | `imq3ffo9j33xsgz3eh601` / `i09gfudis44vshfpweu0a` |

Logfire ingestion is confirmed in the 13:43–13:46 UTC window: 35 events for Incus and 37 for E2B.
Each proof has one `upgraded` and one `rolled_back` terminal receipt, and the injected verification
failure is present. Query `dorf.upgrade_id` using the proof ID plus `-0` or `-1` for each operation.
The original quiescence failures are also retained in Logfire under
`worker-upgrade-incus-1789478960-0` and `worker-upgrade-e2b-1789479008-0` (12 events each); those
older events indicate errors by severity rather than a `.failed` name. All failed-proof VMs were
subsequently removed after checking their retained checkpoint receipts.

The live loop corrected missing Python pidfd wrappers in the guest and Incus guest-agent readiness
after VM start. The fixture does not prove real Provider Gateway routing. Package staging used an
Internet-enabled disposable VM; activation and recovery do not change its network policy.

### Current operator boundary and remaining work

`dorf upgrade request` accepts an exact staged Nix closure and version for a direct Job. The same
retained task owns native work and upgrade reconciliation. A restarted executor reloads receipts;
stale claims or a changed source binding cannot select a replacement. The operator-facing receipt
and Job projection retain source/destination resources, checkpoint, package versions, verification,
terminal outcome, and failure codes. The profile revision remains unchanged.

Next, add pinned Nix and the initial Codex generation to the shared guest recipe used by both the
Incus image and E2B template builders, then build and verify both artifacts. The current shared
recipe installs Codex through npm; only the disposable proofs bootstrap Nix. Existing VMs require
a separate one-time bootstrap because profile promotion changes future VM creation only.

Still to prove or implement: restricted-network package distribution, real Provider Gateway routing
through replacement, automatic rollout policy, and a broader supported package catalog. The current
operator path deliberately requires a staged closure. Do not interpret the disposable fixture as
permission to upgrade a retained user VM or as a production deployment receipt.

## References

- [Nix profiles and generations](https://nix.dev/manual/nix/2.32/package-management/profiles)
- [Incus instance snapshots](https://linuxcontainers.org/incus/docs/main/howto/instances_backup/)
- [E2B snapshots and replacement sandboxes](https://docs.e2b.dev/sandbox/snapshots)
