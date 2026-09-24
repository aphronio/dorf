# Session checkpoints

Status: prototype implemented and verified with stock restic directly to R2 for the exact
Codex 0.154.0 E2B profile. Disposable native replacement, cancellation and cleanup-restore proofs
pass. Activation is opt-in. The v0.17.0 shared image recipe includes stock restic;
deployment requires an image built from the clean release source, not a disposable proof image.

The native Session contract uses a native mutation revision and workspace continuity manifest.
Live operator-requested E2B replacement, restored file/history checks and same-Thread continuation
passed after that change. The [native implementation record](native-session-contract.md#implementation-and-verification)
owns the current verification scope and distinguishes it from the earlier fault proofs below.

Independent checkpoint branches are implemented as an experimental operator command for the same
configured Codex/E2B base-package profile. A bounded live proof passed through the operator command,
worker, pinned Codex 0.154.0, two E2B VMs, and direct R2/restic checkpoints. It restored the same
native Thread into a held new Session, replaced synthetic credentials before release, continued a
native Turn, produced an independent checkpoint, and cleaned up only the destination. Model inference
used a deterministic local Responses fixture; a production model continuation is unproved. The
branch contract and current limits are below.

## Contract

An explicitly configured direct Codex profile can preserve native session files and its workspace
in an encrypted restic repository independently of its provider VM. PostgreSQL retains exact
successful snapshot references alongside the native mutation revision and package-generation
reference. It does not duplicate native input or transcripts. An unresolved native mutation blocks capture and recovery.

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

For Sessions that normally pause after one minute, ordinary capture stops accepting work 15 seconds
before that pause deadline so remote cancellation has time to finish. A missed window retains the
previous checkpoint. Capture does not extend the idle pause policy. Cleanup and `keep_running`
Sessions use the configured backup timeout instead.

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
  The Harness adapter owns native roots and exclusions; the same exclusions govern observation
  and backup. The Sandbox adapter supplies its workspace through `Workspace()`. Diagnostics retain bounded failure classes;
  completed watcher control files are removed after process termination is confirmed.
- A local operator may run `dorf checkpoint capture-pin SESSION --pin-command /absolute/executable -- [args...]`.
  The executable receives one JSON object on stdin with a fresh `attempt_id`, the exact
  `boundary`, and the uploaded `reference`. It must return a bounded JSON object describing
  a provisional application view within 30 seconds. Dorf keeps the native file guard armed
  through that command, then checks the guard and publishes the exact checkpoint. Only a
  successful invocation prints the checkpoint and pin result together. A failed or lost
  invocation leaves the application pin unready; its owner must expire or remove provisional
  artifacts. Every retry is a new capture attempt. This synchronous operation skips ineligible
  or busy boundaries and does not promise continuous save coverage. It does not itself gate
  application writers or turn a database pin into a complete application snapshot.
- Authenticated clients can use the same guard through the checkpoint capture API. Start returns
  an attempt; poll until `ready`, pin client state locally, confirm, then poll for `published`.
  Only that terminal result contains an authoritative checkpoint. Native activity still invalidates
  the save. Attempts last at most three minutes, with at most 30 seconds for client pinning; one
  pending attempt per Session and 32 retained attempts per worker bound resource use. Terminal
  replies expire five minutes after the deadline. Missing or restarted attempts require discarding
  unconfirmed client artifacts; a lost start response must not be blindly retried. Cancellation
  requests stop the guard; observe the final result because publication may have won the race.
  No application callback or executable is accepted by the API. The managed worker retains all
  provider/storage authority. Branch admission, status and release also have authenticated routes.
  [OpenAPI](../../internal/controlapi/openapi.json) owns the exact routes and schemas.
- Publication uses a short Session fence plus the admission transaction lock. There is no Session fence
  held across hashing, uploads, or remote cancellation. Accepted input, new activity, package
  maintenance, or resource replacement invalidate an older boundary.
- Recovery accepts an exact checkpoint and reserves its replacement with the delivery hold in one
  transaction. It restores, verifies native history, deletes the old resource, and atomically
  adopts the replacement and releases its hold. Clients retain unsent input during maintenance. The resource's provider binding
  records successful restore; resource deletion records supply cleanup state. Only native verification
  needs a separate receipt. Later ambiguous or accepted native work cannot be blindly replayed.
- The shared Codex quiescing API inspects and authenticates an existing server before reading settled
  history and stopping it. An absent server needs no startup or route key. Recovery does not inspect
  adapter-private PID files or maintain its own server-control protocol.
- Package upgrades continue using provider snapshots for rollback. Replacing that mechanism is
  a separate change requiring equivalent failed-upgrade recovery evidence.

## Backup scope and confidentiality

The Codex adapter selects its whole native home, currently `/root/.codex`, alongside the workspace
reported by the Sandbox adapter. The backup coordinator and restic transport do not enumerate
Harness-specific paths. New native files are included automatically; operators do not maintain a
second list of Codex files. A new adapter must supply and prove its own capture/restore contract;
this boundary does not automatically qualify an untested Harness or provider for recovery.

Codex history, SQLite databases and WAL files, instructions, skills, client `config.toml`, file-based
`auth.json` and MCP credentials are included. The adapter excludes only these top-level operational
entries: `log`, `logs_2.sqlite` and its WAL/SHM, `*.sqlite-shm`, and `shell_snapshots`. The exclusions
are anchored to Codex home, so a similarly named workspace file is still backed up. Unknown files
are included. Restic preserves symlinks without following their targets.

This selection follows the pinned `rust-v0.154.0` source: [configuration](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/core/src/config/mod.rs)
and [file-based auth](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/login/src/auth/storage.rs)
live in Codex home, log storage and [shell snapshots](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/core/src/shell_snapshot.rs)
are operational, and SQLite WAL belongs with its main database. Native app-server history/resume operations do not provide an atomic filesystem snapshot;
the existing idle, settled-history and file-change checks remain required. Custom SQLite homes,
keyring-backed credentials, other home directories (including `~/.agents/skills`), installed system
packages and running processes are not implicitly covered. Use explicit `additional_paths` for
other required directories or prepare them through the client. A backup is not a full machine image.

Treat **all** protected contents as confidential. Repositories and conversations can contain secrets
just as configuration files can; filename-based secret filtering cannot establish confidentiality.
Dorf's managed model route files live outside these roots and are recreated with fresh authority
on replacement. Client-owned credentials in the selected roots are restored as data; backup does
not rotate, revoke, or extend their external validity.

Encryption and custody use stock restic. The protected worker configuration holds the random
`password_key` seed and parent R2 credential. Dorf derives a stable repository password for each
logical Session/Sandbox; restic uses that password to unlock its random data-encryption keys. R2
stores encrypted data and encrypted key files, not the password or seed. Restic receives only the
selected repository password and temporary scoped R2 credentials inside the Sandbox. The worker
operator and that Sandbox remain trusted with plaintext. Keep the worker configuration backed up
separately and securely: losing the seed can make the stored backups unrecoverable. No extra key
service or encryption implementation is introduced. Retention and deletion policy are unchanged.

## Workspace inspection

Authenticated clients can read `GET /v1/sessions/{session}/workspace` for workspace-level
coverage without accessing storage credentials or native runtime directories. The existing
private worker reader consults current configuration and the latest published checkpoint.
The Sandbox adapter owns the workspace path. No provider operation, activity update,
checkpoint capture, or native thread read occurs, so an active turn can inspect coverage.

The response separates configured backup coverage from a successful checkpoint. A null
checkpoint time means none has been published; an old checkpoint can remain after coverage
is disabled. The timestamp describes the latest publication for the logical workspace,
including before resource replacement, and does not prove that current files were backed up.
Configuration and checkpoint reads are an observation, not a single transactional snapshot.
Failure to inspect configuration returns an error rather than disabled coverage. Workspace
coverage includes directory contents and Git history; symlinks do not include their targets.
Paths outside that workspace are outside this report, even if separately protected.

## Operator configuration

This is opt-in for one named direct Codex 0.154.0 E2B profile. Its pinned image must include upstream
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
| `profile_name` | Exact profile name |
| `profile_revision` | A 64-character revision digest, or `"*"` for all revisions of that named profile |
| `endpoint`, `account_id`, `bucket`, `prefix` | Private R2 destination and session namespace |
| `access_key_id`, `secret_access_key` | Parent credential scoped to the selected bucket |
| `password_key` | Base64 encoding of at least 32 random bytes; durable repository encryption custody |
| `restic_path` | Optional exact executable path; defaults to the pinned workstation profile |
| `additional_paths` | Optional disjoint absolute directories beyond the workspace and native home |
| `idle_delay_seconds` | Defaults to five; supported range 1–300 |
| `backup_timeout_seconds` | Defaults to 120; supported range 1–1800 |

The revision wildcard keeps checkpoints enabled across image updates without editing this file.
Each checkpoint still records its actual profile revision, and native compatibility checks still
apply. A wildcard does not upgrade existing VMs or make images without restic support backups.

Before enabling the worker, verify that delegated credentials cannot read, write or delete another
session's objects, and that stock backup, cancellation and exact restore work in the chosen prefix.
Do not apply blanket retention locks: restic must delete its own temporary repository locks.
Changing the logical identity, password key or path derivation can strand existing references.
Preserve the old configuration for recovery. The normal continuous worker discovers checkpoints;
`worker --once` does not run the background checkpoint queue.

`dorf checkpoint show SESSION` lists retained references and recovery receipts. To recover an open Session,
select the full snapshot ID from that output and run:

```sh
dorf checkpoint recover SESSION --id UNIQUE_REQUEST_ID --repository REPOSITORY_ID --snapshot FULL_SNAPSHOT_ID
```

The command admits recovery intent; the worker checks safety under a retained delivery hold before
restoring. Repeating the same request reconciles the same operation. A checkpoint that predates
accepted or ambiguous native execution cannot restore: the worker reports attention and retains
the hold and current resource. An operator must investigate that gap. Command acceptance alone
does not prove recovery is safe or complete. Cleanup closes admission, so a cleaned-up Session is not reopened
by this command. Investigation of retained cleanup state requires an isolated restore procedure;
the disposable cleanup proof verifies this procedure without reopening the Session.

### Restore into a new Session

An operator can select an exact published checkpoint and stable request identity through the local
`dorf` command. This slice does not add an authenticated client branch HTTP endpoint:

```bash
dorf checkpoint boundary SOURCE_SESSION
dorf checkpoint branch SOURCE_SESSION --id UNIQUE_BRANCH_ID --repository REPOSITORY_ID --snapshot FULL_SNAPSHOT_ID
dorf checkpoint branch-status UNIQUE_BRANCH_ID
```

`checkpoint boundary` is a read-only, single-Session observation under the Session fence. It
returns admission state, retained native Thread and mutation revision, pending native input/Turn
IDs, and the capture boundary/eligibility from that same fenced observation. It does not call the
Harness or read native history. Matching boundary observations alone cannot certify a composed
application/native save: unobserved file changes may occur between observations. Use `capture-pin`
to establish an application view while native file observation remains continuous. The application
owns consistency of that view and publishes its combined save only after the command succeeds.
Branch receipt JSON omits `restored_at`, `release_requested_at`, and `ready_at` until each milestone
is actually reached; clients must not treat a zero time as completion.

The request atomically admits a new Session with its own Sandbox resource, storage namespace, native
revision, and durable hold. Repeating the same request returns the same destination; a changed
reference under that ID is rejected. The source Session can continue after the chosen checkpoint
and is never held, replaced, or deleted by branching. Restic runs in the destination with temporary
read-only access to the source repository. Later backups use the destination repository.

The worker restores native home and workspace files before installing a model route or starting
the Harness. When `restored_at` appears, the client may use the ordinary bounded Sandbox file read
and write operations to replace application credentials and prepare its environment. Native input,
native history, and arbitrary Sandbox commands remain held. The checkpoint can include client
credentials and service configuration; the caller must replace or neutralize authority that should
not enter the branch before requesting release. File preparation does not run the Harness.

```bash
dorf checkpoint release-branch UNIQUE_BRANCH_ID
dorf checkpoint branch-status UNIQUE_BRANCH_ID
```

Release closes file preparation, installs a new destination-scoped model route, resumes and verifies
the restored native Thread, and only then removes the hold. `ready_at` is the native-access gate;
the release command's receipt is a request, not completion evidence. Unknown or interrupted provider
effects reconcile against the same destination reservation. Ordinary Session release cleans up only
the destination's route and resources; a branch closed before readiness needs no new checkpoint.

The current branch operation requires a retained native Thread and the exact configured profile
revision. A checkpoint with an activated package generation is rejected because that source-owned
upgrade receipt cannot describe future destination checkpoints: checkpoint provenance is bound to
an upgrade of the same Sandbox. Branching an upgraded Session therefore requires a later package
provenance design. Process memory, external service state, and arbitrary historical-turn rewind are
outside checkpoint scope. The isolated-home native proof does not exercise the configured provider,
object storage, or destination cleanup; the separate E2B/R2 lifecycle proof above does.

The opt-in `TestLiveCheckpointBranch` passed on 2026-09-23 in 123.57 seconds. It used a clean
Codex 0.154.0 E2B image with stock restic, two concurrent VMs, direct R2 storage, and a local
synthetic Responses server. The real worker crossed restore, held file preparation, restart/replay,
route issuance, native continuation, destination checkpoint, and Core cleanup. The source stayed
open and present after destination deletion; both owned VMs were confirmed absent at test cleanup.
Gateway management and model inference were controlled fixtures, so this does not establish real
model-provider behavior or application credential isolation beyond the exercised file replacement.

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

## Directory-scope verification

The expanded directory selection passed the disposable E2B/R2 native replacement proof on
2026-09-19. Exact hashes matched for client configuration, synthetic auth/MCP credential files
and an unlisted client file. Native `config/read` loaded the restored disabled MCP entry and its
synthetic environment value. Instructions, modified/untracked Git files, SQLite/WAL data, and the
original Thread survived replacement. Operational marker files were excluded. Production route
renewal and a lost-verification-acknowledgement retry preserved client files; same-Thread input
continued after adoption. Three captures were cancelled by new native input without publishing.
The proof confirmed deletion of both owned VMs. Native Codex, E2B, R2 and PostgreSQL were real;
model responses and Gateway management were deterministic fixtures.

The proof now records native acceptance latency instead of asserting the retired queued-admission
sub-second threshold. Observed native acceptance was about 1.6–2.4 seconds without active backup
and 1.5–2.0 seconds during cancellation. Three pairs are smoke evidence, not a performance guarantee.
The local code and documentation gates passed. The selection remains scoped to the verified profile.

## Prototype verification

The live native recovery and cleanup tests passed with upstream restic and actual E2B/R2.
The smaller recovery implementation was subsequently rechecked with the same native replacement proof:

- Automatic checkpoint discovery, publication and three cancellations on incoming work.
- Source deletion, fresh-VM restore, same native Thread continuation, and file/Git/SQLite WAL checks.
- Fresh route credentials and retry after a lost verification acknowledgement.
- Failed pre-cleanup backup retaining the source; successful retry publishing before deletion;
  isolated restore recovering final edits while the original Session remains closed.
- A separate R2 proof verified incremental reuse, old/latest exact restore, full repository check,
  a following backup after cancellation, and cross-session read/write/delete denial.

One small successful capture took 8.4 seconds including 5.5 seconds in restic. Three remote stop
confirmations took roughly 0.33–0.35 seconds. Paired input-admission smoke samples stayed around
3–7 ms. These samples establish the independent foreground path; they do not establish a statistical
zero-regression guarantee across workloads. The Responses and Gateway control plane used deterministic
fixtures; native Codex, E2B, R2 and PostgreSQL were real. Retention automation and production workload
cost limits remain outside this slice. Earlier patched-build experiments are superseded.
