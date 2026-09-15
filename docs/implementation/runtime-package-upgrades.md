# Plan: Package upgrades in persistent Sandboxes

Status: deferred follow-up, agreed on 2026-09-15. Start after the profile-revision slice in
[D132](../project/decisions/D132-profile-revisions-separate-promotion-from-job-custody.md) is finished.
This plan records future work; it does not describe shipped behavior.

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

1. Finish the profile-revision slice and its review before starting this work.
2. Prove the package install and checkpoint/restore sequence on disposable Incus and E2B Sandboxes.
   Measure interruption time and verify an existing runner conversation can resume after rollback.
3. Add the smallest durable upgrade operation and delivery hold needed for recovery. Exercise
   incoming messages and controller restart while held; prove no lost or duplicate delivery.
4. Expose structured maintenance status for consuming applications.
5. Verify the full flow with correlated Logfire events and update current architecture, support,
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
