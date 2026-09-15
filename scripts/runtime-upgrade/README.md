# Disposable package upgrade proof

Run from the repository root:

```bash
mise run integration:delivery-hold # Uses the configured disposable PostgreSQL test database.
mise run integration:runtime-upgrade -- incus
mise run integration:runtime-upgrade -- e2b
# Exercise lost checkpoint acknowledgement (expected failure; retains owned recovery resources):
mise run integration:runtime-upgrade -- incus --lose-checkpoint-response
# Retry cleanup after a terminated proof process:
mise run integration:runtime-upgrade -- cleanup upgrade-proof-012345abcdef
```

The recipe exercises Dorf's Go provider adapters using only synthetic conversations and owned
disposable VMs. It stages two pinned
Codex packages in Nix, starts a native conversation with the old version, checkpoints the VM,
activates the new version, and resumes the same conversation. It then injects incompatible local
state, restores the checkpoint, compares the exact local-state digest, and verifies that the old
version resumes the original conversation with a nonempty reply. A localhost Responses fixture
supplies model responses; the proof does not need an AI account or perform user actions.

## Prerequisites

- Bun and the repository's pinned E2B tooling dependencies; the wrapper installs the locked set.
- Incus: a local daemon with `incusbr0`, the `default` storage pool, and the clean VM release
  manifest/archive currently selected in `runtime-upgrade-proof.ts`. The recipe owns a dedicated
  project, checks its owner label, and reuses its imported image.
- E2B: `E2B_API_KEY` and the built template manifest selected by the recipe. Bun may load the local
  repository `.env`; credentials are never included in evidence. The account must support snapshots.

No Nix installation on the host is needed. The shared
[`packages.json`](../sandbox/packages/packages.json) pins Nix, Nixpkgs, and the exact official
Codex archives. `guest.sh stage VERSION` builds an immutable closure and retains a GC root without
changing the active runner. `guest.sh activate VERSION` requires that staged closure and switches
the Nix profile. `guest.sh source-hash` is a maintenance helper for updating the Nixpkgs pin.

Staging downloads packages inside the guest using its existing Internet access. Offline delivery
is deferred until a concrete deployment needs it. A blocked download must not change network policy.

The shared package helper is installed as `dorf-packages` in new images. Build and verify exact
disposable candidates with `mise run integration:nix-image build incus` and the corresponding E2B
command. Each prints a repeatable verification command tied to its input hashes and artifact ID.
That verification requires Nix-managed Codex before any guest bootstrap and stages additional
versions using the installed helper. Candidate images remain available for inspection; test VMs
and checkpoints are removed by the retained-worker proof on success.

## Evidence and limits

Each run writes `.dorf/runtime-upgrade/<upgrade_id>/events.jsonl` and phase logs. Check
`proof.passed` **and** successful cleanup. Failed runs retain their diagnostics. Incus rollback
restores the original instance; E2B rollback records a different provider VM ID. Cleanup rediscovers
owned VMs to handle a lost creation response and never deletes a VM with a foreign owner label.

The private `custody.json` receipt retains exact ownership for cleanup recovery; do not publish it.
E2B requires deletion of a restored VM before deletion of its backing snapshot.
The lost-response option works on either provider. Cleanup retains the source when checkpoint
identity is unknown; the cleanup command reconciles that checkpoint before deleting owned resources.
`proof.finished` distinguishes the proof outcome from cleanup even when cleanup fails.

With the deployment's standard `OTEL_EXPORTER_OTLP_LOGS_*` variables configured, the built proof
helper can submit the public event trail through Dorf's normal publisher:

```bash
printf '{"operation":"publish","events_file":".dorf/runtime-upgrade/UPGRADE_ID/events.jsonl"}' \
  | .dorf/bin/runtime-upgrade-provider
```

Exporter success is not an ingestion assertion. Query Logfire for `dorf.upgrade_id` to confirm
the expected transitions, failure record, provider identities, and terminal cleanup.

Use a bounded time window in the deployment's Logfire query client and substitute the proof ID:

```sql
SELECT message, attributes->>'dorf.provider_sandbox_id' AS provider_resource_id, count(*) AS records
FROM records
WHERE attributes->>'dorf.upgrade_id' = 'UPGRADE_ID'
GROUP BY message, attributes->>'dorf.provider_sandbox_id'
ORDER BY message
LIMIT 100
```

The proof rejects attached custom volumes whose snapshot coverage has not been established.
Synthetic runner data lives under `/workspace/upgrade-proof`; production upgrades must establish
coverage for the actual runner state. This recipe proves package and provider behavior, not message
queue holds, controller recovery, live provider routing, or Logfire ingestion. Those belong to the
[active implementation plan](../../docs/implementation/runtime-package-upgrades.md).

## Retained-worker coordinator recipe

```bash
mise run integration:upgrade-worker incus
mise run integration:upgrade-worker e2b
# Diagnose the exact retained synthetic VM without admitting input:
mise run integration:upgrade-worker incus probe JOB
# Delete a failed proof resource only when checkpoint custody is fully settled:
mise run integration:upgrade-worker incus cleanup-unstarted JOB
```

The worker recipe uses the configured disposable PostgreSQL database, the provider artifacts and
Incus project above, and a local-only Responses fixture. It starts a real native conversation through
Core, stops the worker, admits an upgrade and queued message, and starts a new worker. It proves
activation, then injects incompatible runner-local state after switching a second pinned version to
force recovery. It checks the same native context, nonempty replies, exact request count, verified
resource selection, and all resource/checkpoint cleanup. No AI account, user input, or real Provider
Gateway is used. This is a package/custody proof, not a profile verification or Gateway routing proof.

The positive coordinator case activates the already staged 0.154.0 closure; the recovery case switches
to 0.147.0 and explicitly injects failure. The older adapter recipe separately proves version increase.
Both versions can resume the fixture; the fault is intentional, not a claim that 0.147.0 is broken.

Evidence is retained under `.dorf/runtime-upgrade/worker-upgrade-PROVIDER-TIMESTAMP/`. Events come
from the actual coordinator telemetry sink and can be exported with the publisher above. A failed
proof retains its VM for diagnosis; the probe restricts access to a synthetic proof Job and takes its
Job fence. The cleanup probe refuses unsettled checkpoint custody. Use normal coordinated Job
cleanup for an interrupted upgrade with retained recovery dependencies.

Live loops found two real guest boundaries: older guest Python lacks pidfd wrappers, so exact
process stopping uses Linux pidfd syscalls; and Incus start acknowledgement precedes guest-agent
readiness, so activation waits for bounded command readiness instead of immediately rolling back.
