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

No Nix installation on the host is needed. `packages.json` pins Nix, Nixpkgs, and the exact official
Codex archives. `guest.sh stage VERSION` builds an immutable closure and retains a GC root without
changing the active runner. `guest.sh activate VERSION` requires that staged closure and switches
the Nix profile. `guest.sh source-hash` is a maintenance helper for updating the Nixpkgs pin.

Staging currently downloads packages inside the disposable guest. This proves Internet-enabled
Sandboxes only. Production package delivery to restricted-network E2B Sandboxes remains part of
the implementation plan; an upgrade must preserve the admitted network policy.

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
