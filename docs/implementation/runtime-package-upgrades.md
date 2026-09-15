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
Finish current turn
Hold delivery; keep saving incoming messages
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

- Still pending: durable upgrade request/executor, native quiescence and recovery verification,
  atomic replacement binding and history, cleanup of replacement resources/backing checkpoints,
  upgrade-phase telemetry, and the combined control-plane/provider proof.
  Production package delivery to restricted-network E2B guests also remains unimplemented; the
  recipe stages downloads with Internet access and must not weaken a Job's network policy.
  These facts do not claim that running user VMs can be upgraded yet.

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

### Next implementation boundary

Ship the active-resource switch together with the durable delivery hold and cleanup coordination.
Persist replacement ownership before the provider call; use a compare-and-set on the expected
source binding after native recovery verification. Preserve source and destination records and
link the replacement to its upgrade/checkpoint. A restarted executor must reconcile the same
operation, and a stale executor must not reactivate a superseded resource. Until this boundary is
implemented, checkpoint adapters are exercised by the disposable recipe only.

New automatic messages during the hold become FIFO follows. Continue observing pre-hold active
turns and settling their steers; queued follows do not prevent reaching the quiet boundary. Release
must atomically publish the verified binding and wake normal delivery. Job cleanup must fence the
upgrade executor and account for every reserved resource and retained backing checkpoint.

## Remaining sequence

1. Compose the proven delivery hold with a durable upgrade executor, native quiescence checks,
   package activation, recovery verification, and atomic resource switching.
2. Extend the hold banner with verification and failed-recovery states derived from executor receipts.
3. Verify the full flow with correlated Logfire events and update current architecture, support,
   operator documentation, and the decision record when the implementation is established.

Use short fault-injection loops: successful upgrade; checkpoint failure before mutation; failed
installation; failed verification after modifying local state; successful rollback; failed rollback;
and interruption during E2B replacement. Prove package version, retained conversation, resource
identity, queue behavior, and terminal diagnostics at each relevant boundary. Local deployments are
test-only and may start fresh. Use synthetic conversations before any retained user VM upgrade.

## References

- [Nix profiles and generations](https://nix.dev/manual/nix/2.32/package-management/profiles)
- [Incus instance snapshots](https://linuxcontainers.org/incus/docs/main/howto/instances_backup/)
- [E2B snapshots and replacement sandboxes](https://docs.e2b.dev/sandbox/snapshots)
