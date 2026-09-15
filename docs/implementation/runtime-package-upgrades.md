# Package upgrades in persistent Sandboxes

Status: the scoped Codex upgrade and shared workstation slices shipped in v0.16.0 on
2026-09-15, following the profile-revision slice in
[D132](../project/decisions/D132-profile-revisions-separate-promotion-from-job-custody.md).
The sections below preserve implementation decisions and intermediate verification evidence.
The release verification section records the final artifacts; earlier candidates are historical.

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
- Distribution remains operator-managed. The shared image recipe installs Nix-managed Codex and
  its package helper downloads pinned packages directly inside Internet-enabled guests, verifying
  them before activation. Offline delivery is deferred until
  a concrete deployment needs it; automatic fleet rollout remains unimplemented.
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

### Operator boundary and historical candidate verification

The Nix image slice passed both candidate builds and live verification on 2026-09-15. These are
disposable local/test artifacts from an explicitly recorded dirty tree, with exact input hashes;
they have not been published or promoted into a deployment profile.

| Provider | Candidate build proof | Retained-worker proof |
| --- | --- | --- |
| Incus | `upgrade-proof-2e72ef4e336f` | `worker-upgrade-incus-1789482252` |
| E2B | `upgrade-proof-88f1f93c4142` | `worker-upgrade-e2b-1789482129` |

Incus fingerprint: `cd569f45a5ff25eab6b447eae7be7b10209501eb9cca9a5e407e567185bfc468`.
E2B template: `dorf-nix-upgrade-proof-88f1f93c4142:0b55c486-ab2e-4fc1-9fce-345029ca86aa`.
Both guests had Nix-managed Codex before staging, and used the installed helper to download the
second pinned version. Activation, forced rollback, original native context, nonempty queued
replies, exactly one model request per input, and all test VM/checkpoint cleanup passed. Incus
restored `dorf-f4f76fd18578a28aad69` in place; E2B replaced `inqk3z9pterhnckohjd8t` with
`iiqv44xbbemtp1zsayk0g`. Candidate images/templates remain available for inspection.

The first E2B readiness check exceeded its combined 30-second startup bound while running Pi.
A separate disposable probe completed Pi startup in 9.1 seconds and a repeat in 1.1 seconds;
the combined cold-start readiness bound is now 90 seconds. The successful profile check verified
Codex, Pi, Nix, executable selection, package provenance, and absence of image credentials. Logfire
ingestion is confirmed in the 14:17–14:26 UTC window: 4 Incus image events, 6 E2B image events
(including the initial failure), and 35/37 coordinator events respectively. Each coordinator proof
has one `upgraded` and one `rolled_back` outcome. The full deterministic repository gate also passed.

`dorf upgrade request` accepts an exact staged Nix closure and version for a direct Job. The same
retained task owns native work and upgrade reconciliation. A restarted executor reloads receipts;
stale claims or a changed source binding cannot select a replacement. The operator-facing receipt
and Job projection retain source/destination resources, checkpoint, package versions, verification,
terminal outcome, and failure codes. The profile revision remains unchanged.

The shared guest recipe installs pinned Nix, the initial Codex generation, and the same workstation
package set for both Incus images and E2B templates. Pi, developer tools, and the browser environment
come from Nix; the agent owns browser processes. Codex retains its separate runner profile so its
upgrade cannot replace the workstation. One package directory supplies image construction, guest
staging, and upgrade proofs. Profile promotion changes future VM creation only. Bootstrapping older images without Nix is outside the scope of this slice.

Use the VM's existing Internet access to download pinned packages and verify their integrity before
holding delivery and activating an upgrade. No controller-side package relay or offline import path
is needed for this slice. If a deployment's network policy blocks package downloads, report the
staging failure without changing that policy. Defer offline delivery until a concrete need appears.

Real Provider Gateway routing through replacement is verified by the repeatable Gateway recipe.
Automatic rollout policy and a broader supported package catalog are deferred; they are not prerequisites for this Codex slice. The current
operator path deliberately requires a staged closure. Do not interpret the disposable fixture as
permission to upgrade a retained user VM or as a production deployment receipt.

### Initial shared workstation verification

Fresh guests on both providers reported the identical workstation
`/nix/store/4dhf7dsi78p7amdmwfxgz95aypzv2q12-dorf-workstation` and matching tool inventories.
The repeatable candidate recipe exercises compilation, Python environments, Playwright interaction,
and browser-use navigation against a local fixture. It verifies that no browser runs at startup,
then starts and closes its own browser. The resulting `workstation.json` records are retained with
each worker proof; exact package versions remain in those records and image metadata.

The first Incus candidate passed the live checks but exceeded the release archive size bound.
The builder now discards unused filesystem blocks before publication, after removing temporary
build inputs. The final archive is 1,486,102,348 bytes, within the unchanged 2,000,000,000-byte
release bound.

Final candidate and retained-worker checks passed on 2026-09-15:

| Provider | Candidate build proof | Retained-worker proof |
| --- | --- | --- |
| Incus | `upgrade-proof-57e33c7d9a58` | `worker-upgrade-incus-1789485493` |
| E2B | `upgrade-proof-7af8b4af375f` | `worker-upgrade-e2b-1789485194` |

Incus fingerprint: `320d445597d874ae2a04c7226b7f1d4560f1ed07383aba7191358b23d07ec1db`.
E2B template: `dorf-nix-upgrade-proof-7af8b4af375f:1e56502d-9b9a-400b-978d-603591da7350`.
Both proofs passed activation, forced rollback, original conversation context, nonempty queued
replies, and complete owned VM/checkpoint cleanup. The standalone packaging lab was also removed.
The full repository gate and local binary build passed. These remain explicitly dirty-source test
candidates, with exact input hashes; no deployment profile or retained user VM was changed.

Logfire ingestion is confirmed for the final worker proofs in the 15:13–15:20 UTC window: 36
Incus events and 38 E2B events, including workstation verification and one `upgraded` and one
`rolled_back` receipt per provider. Query the worker proof ID for workstation identity and append
`-0` or `-1` for each coordinator operation. Image build/verification events use the candidate IDs.

### Browser dependency and skill correction

The shared recipe now uses browser-use directly with preinstalled Chromium. Upstream browser-use
and browser-harness control the browser through CDP and do not require Playwright for this flow.
The separately selected Playwright package, its two exclusive Python dependencies, and the
separate headless-shell/Playwright browser bundle are removed. Chromium itself supports headless mode.

The installer copies the upstream browser-use skill without inserting or modifying any text. Future
environment guidance must stay separate and follow observed agent friction. The fresh-image proof
checks that the installed skill matches the upstream CLI output, Playwright is absent, and
browser-use can navigate and click through an explicitly started Chromium process.

The corrected E2B candidate `upgrade-proof-16e39a42ff0d` passed the profile and retained-worker
checks with `worker-upgrade-e2b-1789486457`. Its template is
`dorf-nix-upgrade-proof-16e39a42ff0d:ce1d8f9d-734c-4190-a7ac-952d356c22dd`.
The rollback restore step failed once, then succeeded through the existing coordinator retry.
The proof verified rollback, original context, substantive replies, and all resource/checkpoint
cleanup. Logfire ingestion includes that restore failure and both restore attempts; the proof
retains 41 coordinator/workstation events in the 15:34–15:37 UTC window. The provider's underlying
error cause was not established by this proof.

The corrected Incus candidate `upgrade-proof-5707827c7d5f` and retained-worker proof
`worker-upgrade-incus-1789486623` also passed browser interaction, activation, rollback, native
context, substantive replies, and complete cleanup. Its fingerprint is
`59a6a6a426adf98c01d68e5941ad5f2848035e869a08b9d4380309ba9139b31e`; the archive is
1,367,301,222 bytes. Both corrected candidates report the same workstation
`/nix/store/5fzd1f3093nzvia98xmfdkhivpvxvjxf-dorf-workstation` and identical tool inventories.
These results supersede the initial workstation candidates above. The repository gate passed;
the candidates remain test-only pending clean release construction and profile promotion.

### Real Gateway verification

The repeatable `integration:upgrade-gateway` recipe passed on both providers on 2026-09-15.
It used an existing authenticated Gateway with temporary synthetic consumer routes. A native
conversation recalled its original marker after activation and after forced rollback; E2B changed
its provider resource while preserving that conversation. Both runs completed scoped route,
resource, and checkpoint cleanup. Incus completed in approximately 74 seconds and E2B in 121
seconds. Exact synthetic receipts remain local and in correlated telemetry. The deterministic
fixture remains the oracle for exact model-request counts; these live checks establish actual
Gateway connectivity across recovery.

The public repository contains generic recipes and synthetic verification behavior only.
Deployment-specific configuration and operational evidence stay outside tracked files.

### Release verification

The immutable [v0.16.0 release](https://github.com/aphronio/dorf/releases/tag/v0.16.0)
was built from clean source. Both final provider artifacts passed the real Gateway recipe:
package activation, forced rollback, original conversation context, substantive queued replies,
and owned resource/checkpoint cleanup. Correlated telemetry ingestion was verified, including
the injected failure and recovery. The full deterministic gate and exact-source CI passed.

Both images report the same workstation
`/nix/store/5fzd1f3093nzvia98xmfdkhivpvxvjxf-dorf-workstation` and preinstalled Nix-managed
Codex 0.154.0. The release Incus archive is 1,369,788,167 bytes, below the unchanged release
size limit. The E2B release template is public after explicit publication authorization and was
verified using credentials from a different deployment team.

Deployed profile verification and synthetic initial/follow-up checks passed on both providers.
Each follow-up recalled its original marker; image metadata matched the release, and test Jobs
completed cleanup. Deployment-specific identities and consuming-application checks remain in
private operational records. Automatic fleet rollout, offline delivery, old-image bootstrap,
and live updates of packages other than Codex remain deferred.

## References

- [Nix profiles and generations](https://nix.dev/manual/nix/2.32/package-management/profiles)
- [Incus instance snapshots](https://linuxcontainers.org/incus/docs/main/howto/instances_backup/)
- [E2B snapshots and replacement sandboxes](https://docs.e2b.dev/sandbox/snapshots)
