# Session checkpoints

Status: prototype implemented and verified with stock restic directly to R2 for the exact
Codex 0.154.0 E2B profile. Disposable native replacement, cancellation and cleanup-restore proofs
pass. Activation is opt-in. The v0.17.0 shared image recipe includes stock restic;
deployment requires an image built from the clean release source, not a disposable proof image.

## Contract

An explicitly configured direct Codex profile can preserve native session files and its workspace
in an encrypted restic repository independently of its provider VM. PostgreSQL retains exact
successful snapshot references alongside the existing execution boundary and package-generation
reference. It does not duplicate the native transcript or accepted Message ledger.

After five seconds of continuous idle time, a backup may capture the latest state. New work takes
priority: it invalidates publication and requests remote cancellation without waiting for process
termination, storage, retry, or telemetry. Several turns can remain unprotected during continuous
activity. Only the latest successfully published checkpoint is the recovery point.

Requested cleanup uses the same checkpoint mechanism once execution has stopped. It may wait for
backup and durable publication before deleting compute. Failed backup leaves cleanup pending.
Cleanup does not create a separate checkpoint type or retention policy.

Cleanup during an unfinished package upgrade fails closed. Dorf retains the source and reports
cleanup attention rather than publishing a checkpoint whose package-generation reference may not
match an in-flight activation. Completing that cleanup requires explicit upgrade reconciliation;
this slice does not infer package state or release the upgrade hold.

For Jobs that normally pause after one minute, ordinary capture stops accepting work 15 seconds
before that pause deadline so remote cancellation has time to finish. A missed window retains the
previous checkpoint. Capture does not extend the idle pause policy. Cleanup and `keep_running`
Jobs use the configured backup timeout instead.

## Boundaries

- The existing worker process owns coordination and trusted credential custody. Backup tasks have
  independent Absurd capacity so they cannot occupy the last foreground task slot.
- Restic in the sandbox sends encrypted, deduplicated data directly to private object storage.
  Temporary credentials are restricted to the selected repository; parent storage credentials
  remain on the worker. Direct execution necessarily gives that sandbox its repository key.
- Credentials allow read/write inside one logical session's repository. A replacement VM uses
  that same repository; unrelated sessions cannot access it. Protecting a session's own history
  against malicious code inside its sandbox is outside this prototype's guarantee. Ordinary backup
  and restore never prune. No bucket retention rules, dependency patch, or storage gateway is needed.
- The provider command API owns command deadlines, cancellation and termination observation.
  Restic uses a no-fork kernel lock to prevent overlapping commands after uncertain termination;
  there is no restic-specific process manager or persisted restic command protocol.
- Native capture guards reject writes and uncertain filesystem observations during capture.
  An idle timer alone is not evidence that native state was saved consistently. The guard combines
  saved-turn verification, file-change observation, and before/after metadata fingerprints. Matching
  content hashes alone can miss temporary changes read by a backup before the source reverts.
  One native-file inventory owns selection and validation. Diagnostics retain bounded failure classes;
  completed watcher control files are removed after process termination is confirmed.
- Publication uses a short Job fence plus the admission transaction lock. There is no Job fence
  held across hashing, uploads, or remote cancellation. Accepted input, new activity, package
  maintenance, or resource replacement invalidate an older boundary.
- Recovery accepts an exact checkpoint and reserves its replacement with the delivery hold in one
  transaction. It restores, verifies native history, deletes the old resource, and atomically
  adopts the replacement, releases its hold and wakes queued input. The resource's provider binding
  records successful restore; resource deletion records supply cleanup state. Only native verification
  needs a separate receipt. Later ambiguous or accepted native work cannot be blindly replayed.
- The shared Codex quiescing API inspects and authenticates an existing server before reading settled
  history and stopping it. An absent server needs no startup or route key. Recovery does not inspect
  adapter-private PID files or maintain its own server-control protocol.
- Package upgrades continue using provider snapshots for rollback. Replacing that mechanism is
  a separate change requiring equivalent failed-upgrade recovery evidence.

## Operator configuration

This is opt-in for one exact direct Codex 0.154.0 E2B profile. Its pinned image must include upstream
restic 0.19.1 at `/nix/var/nix/profiles/dorf-tools/bin/restic`; the shared workstation recipe installs it.

Place the private configuration at
`${XDG_CONFIG_HOME:-$HOME/.config}/dorf/persistence.json` on the deployment host with mode `0600`.
The existing read-only configuration bind exposes that same file to the managed worker as
`/var/lib/dorf/.config/dorf/persistence.json`. File presence explicitly enables persistence; an
absent file leaves it disabled. Recreate the worker after adding or changing it. Standalone workers
may instead set `DORF_PERSISTENCE_CONFIG` to an absolute private configuration path. Keep the file
outside the repository and back it up independently of sandbox disks. It contains:

| Field | Meaning |
| --- | --- |
| `id` | Stable logical repository identity retained in PostgreSQL |
| `profile_name`, `profile_revision` | Exact profile name and its 64-character revision digest |
| `endpoint`, `account_id`, `bucket`, `prefix` | Private R2 destination and session namespace |
| `access_key_id`, `secret_access_key` | Parent credential scoped to the selected bucket |
| `password_key` | Base64 encoding of at least 32 random bytes; durable repository encryption custody |
| `restic_path` | Optional exact executable path; defaults to the pinned workstation profile |
| `additional_paths` | Optional disjoint absolute directories beyond the supported native inventory |
| `idle_delay_seconds` | Defaults to five; supported range 1–300 |
| `backup_timeout_seconds` | Defaults to 120; supported range 1–1800 |

Before enabling the worker, verify that delegated credentials cannot read, write or delete another
session's objects, and that stock backup, cancellation and exact restore work in the chosen prefix.
Do not apply blanket retention locks: restic must delete its own temporary repository locks.
Changing the logical identity, password key or path derivation can strand existing references.
Preserve the old configuration for recovery. The normal continuous worker discovers checkpoints;
`worker --once` does not run the background checkpoint queue.

`dorf checkpoint show JOB` lists retained references and recovery receipts. To recover an open Job,
select the full snapshot ID from that output and run:

```sh
dorf checkpoint recover JOB --id UNIQUE_REQUEST_ID --repository REPOSITORY_ID --snapshot FULL_SNAPSHOT_ID
```

The worker performs recovery under a retained delivery hold. Repeating the same request reconciles
the same operation. A checkpoint that predates accepted or ambiguous native execution is rejected;
an operator must investigate that gap. Cleanup closes admission, so a cleaned-up Job is not reopened
by this command. Investigation of retained cleanup state requires an isolated restore procedure;
the disposable cleanup proof verifies this procedure without reopening the Job.

Only one unfinished maintenance operation may own a Sandbox. Recovery and package-upgrade requests
preserve exact same-request replay but reject a distinct request while delivery is held. A typed
operation releases its hold only at atomic completion or after admission closes for cleanup.

## Verification before support

1. PostgreSQL races: input and activity during upload, publication acknowledgement loss, stale
   resources and worker claims, retention of the previous reference, and cleanup retry.
2. Native capture: exact retained Thread/Turn history, SQLite main/WAL consistency, workspace and
   Git metadata, delayed writers, watcher overflow, and fail-closed capture rejection.
3. Storage: scoped credentials, cross-session read/write/delete denial, interrupted upload,
   actual remote process termination, fresh-cache restore, and trusted key custody.
4. Replacement: source VM absent, exact effective package generation, fresh route authority,
   atomic resource adoption, lost worker claims and verification acknowledgements, queued-input order,
   interrupted-recovery cleanup, and no blind repeat of external effects.
5. Interaction cost: real native response and next-turn latency with backups enabled/disabled,
   large backups, cancellation and unavailable storage. Report durability lag and managed-VM
   overhead independently of bucket deduplication. Successful backup telemetry separates restic's
   logical added bytes from its packed new-blob payload. The packed value excludes failed transfers,
   some repository metadata, and transport/request overhead, so it is neither guest network traffic
   nor an object-storage billing total.

Keep public test data synthetic. Deployment configuration, identifiers, credentials, application
content, and operational receipts belong outside this public repository.

## Prototype verification

The live native recovery and cleanup tests passed with upstream restic and actual E2B/R2.
The smaller recovery implementation was subsequently rechecked with the same native replacement proof:

- Automatic checkpoint discovery, publication and three cancellations on incoming work.
- Source deletion, fresh-VM restore, same native Thread continuation, and file/Git/SQLite WAL checks.
- Fresh route credentials and retry after a lost verification acknowledgement.
- Failed pre-cleanup backup retaining the source; successful retry publishing before deletion;
  isolated restore recovering final edits while the original Job remains closed.
- A separate R2 proof verified incremental reuse, old/latest exact restore, full repository check,
  a following backup after cancellation, and cross-session read/write/delete denial.

One small successful capture took 8.4 seconds including 5.5 seconds in restic. Three remote stop
confirmations took roughly 0.33–0.35 seconds. Paired input-admission smoke samples stayed around
3–7 ms. These samples establish the independent foreground path; they do not establish a statistical
zero-regression guarantee across workloads. The Responses and Gateway control plane used deterministic
fixtures; native Codex, E2B, R2 and PostgreSQL were real. Retention automation and production workload
cost limits remain outside this slice. Earlier patched-build experiments are superseded.
