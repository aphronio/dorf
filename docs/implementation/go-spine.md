# Go durable Job messages and Session recovery

This is the executable terminal for issues #40 and #41. A complete Job and every later client
message are admitted to Dorf-owned PostgreSQL facts, scheduled or woken through Absurd, delivered by
a Go worker to one credential-free Incus Sandbox, and observed through one resumable Codex native
Session. Cleanup closes admission, cancels the delivery task, revokes the Sandbox's inference route,
and deletes the VM under the same Job mutation fence.

The demonstrated commands use the compiled Go binary only. PostgreSQL and Incus are native host
services; Dorf neither starts a container nor needs a cloud durability account or host Docker
socket. An already connected Provider Gateway is a setup prerequisite. Its persistent
`cli-proxy-api` broker owns upstream authentication; the Go path issues and revokes only a scoped
consumer route and never prints its key.

## Local prerequisites

Install Go 1.25, PostgreSQL 16 or newer, and Incus. Absurd currently marks its Go SDK experimental
and its v0.5.0 module declares Go 1.25, so this slice does not claim a stable SDK surface or an older
Go toolchain. On Ubuntu, PostgreSQL can be a normal local
cluster:

```bash
sudo apt-get install postgresql postgresql-client
sudo -u postgres createuser --createdb "$USER"
createdb dorf
export DORF_DATABASE_URL='postgresql:///dorf?host=/var/run/postgresql'
```

Use the existing [Incus image procedure](incus-image.md) to publish the `dorf-codex` alias. The
image must contain `git`, `curl`, and `codex app-server`, and must not contain
`/root/.codex/auth.json` or a provider route key. The Go worker checks that boundary before it
installs a route. The default Incus bridge is `incusbr0`; the Provider Gateway broker must be bound
to that exact bridge IPv4. A different RFC1918 address is rejected rather than treated as equivalent.

One-time upstream provider connection remains an owner setup operation. After it exists, record its
name and the gateway state location without exporting any upstream or downstream secret:

```bash
export DORF_PROVIDER_GATEWAY_STATE="$HOME/.local/state/dorf/provider-gateway"
export DORF_PROVIDER_CONNECTION='primary'
```

Initialize the exact Absurd schema through the Go binary. This avoids making Python part of either
database setup or the demonstrated Job. The Go SDK module is pinned to release `v0.5.0`; tag
`0.5.0` resolves to verified commit `550d3b9e6f9382d96178de6ab8c90c7f8edf2227`, and the schema is
downloaded from that immutable commit. Dorf verifies its SHA-256
`d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab` before executing it.

```bash
curl -fsSLo /tmp/absurd-0.5.0.sql \
  https://raw.githubusercontent.com/earendil-works/absurd/550d3b9e6f9382d96178de6ab8c90c7f8edf2227/sql/absurd.sql
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf migrate --absurd-schema /tmp/absurd-0.5.0.sql
./bin/dorf doctor --provider "$DORF_PROVIDER_CONNECTION"
```

Diagnostics distinguish PostgreSQL connectivity, Absurd version and queue, Incus command access,
private network, image availability, and provider-route authority. The worker separately refuses an
image containing an upstream credential or old route key. Each failed item includes a local repair.
No check probes Docker.

## Exactly-once two-message and SIGKILL terminal

Build and migrate exactly as above, then use one long enough real implementation input that the
native turn remains active while the second client call runs. The proof barriers are disabled by
default. Enabling one requires an issue-specific phrase, an exact FIFO sequence, and an explicit
directory. A barrier shortens only its current Absurd claim to ten seconds, writes one `.ready`
marker, and fails after eight seconds if the outer orchestrator did not SIGKILL it. These hooks are
deliberately unsuitable for production fault injection.

Absurd 0.5.0 `WorkBatch` does not claim an expired task in the same invocation that rescues its
expired claim. After each lease-expiry sleep below, the first unbarriered `worker --once` therefore
prints `Absurd delivery reconciled` and returns; the following invocation claims the next attempt.
Those rescue-only passes do not increment the task attempt count: the three killed barrier claims
are attempts 1 through 3, and the terminal drain is attempt 4.

```bash
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf migrate
mkdir -p .proof/issue-41/barriers
printf '%s\n' 'Inspect this repository, implement the admitted issue completely, and run its focused checks. Keep working until the change is verified.' > .proof/issue-41/goal.txt
printf '%s\n' 'Before finishing, re-check the exact diff and run the focused Go tests again. Report any remaining risk.' > .proof/issue-41/steer.txt

./bin/dorf admit \
  --key issue-41-exactly-once-final-v1 \
  --goal-file .proof/issue-41/goal.txt \
  --repo https://github.com/aphronio/dorf.git \
  --revision "$(git rev-parse HEAD)" \
  --branch dorf/issue-41-exactly-once-final-v1 \
  --provider "$DORF_PROVIDER_CONNECTION" \
  --model gpt-5.6-sol \
  --reasoning high

# Copy job_id from the admission receipt once; every command below uses that exact identity.
export JOB_ID='job-REPLACE_FROM_RECEIPT'
export DORF_PROOF_FAULT_BARRIER_ENABLE='issue-41-external-sigkill-only'
export DORF_PROOF_FAULT_BARRIER_SEQUENCE='1'
export DORF_PROOF_FAULT_BARRIER_DIR="$PWD/.proof/issue-41/barriers"

# 1. The native baseline is committed, but turn/start has not been called.
DORF_PROOF_FAULT_BARRIER='before-submit' ./bin/dorf worker --once &
export WORKER_PID=$!
while kill -0 "$WORKER_PID" 2>/dev/null && ! test -f "$DORF_PROOF_FAULT_BARRIER_DIR/$JOB_ID-seq-1-before-submit.ready"; do sleep 0.1; done
test -f "$DORF_PROOF_FAULT_BARRIER_DIR/$JOB_ID-seq-1-before-submit.ready"
kill -KILL "$WORKER_PID"
wait "$WORKER_PID" || true
sleep 11

# Rescue expired attempt 1; this pass does not execute the Job task.
time ./bin/dorf worker --once

# 2. Codex accepted turn/start, but Dorf has bound neither the Session nor native turn ID.
DORF_PROOF_FAULT_BARRIER='after-submit-before-bind' ./bin/dorf worker --once &
export WORKER_PID=$!
while kill -0 "$WORKER_PID" 2>/dev/null && ! test -f "$DORF_PROOF_FAULT_BARRIER_DIR/$JOB_ID-seq-1-after-submit-before-bind.ready"; do sleep 0.1; done
test -f "$DORF_PROOF_FAULT_BARRIER_DIR/$JOB_ID-seq-1-after-submit-before-bind.ready"
kill -KILL "$WORKER_PID"
wait "$WORKER_PID" || true
sleep 11

# Rescue expired attempt 2; the next worker claims attempt 3.
time ./bin/dorf worker --once

# 3. Recovery matched the baseline suffix and durably bound the still-active native turn.
DORF_PROOF_FAULT_BARRIER='native-active' ./bin/dorf worker --once &
export WORKER_PID=$!
while kill -0 "$WORKER_PID" 2>/dev/null && ! test -f "$DORF_PROOF_FAULT_BARRIER_DIR/$JOB_ID-seq-1-native-active.ready"; do sleep 0.1; done
test -f "$DORF_PROOF_FAULT_BARRIER_DIR/$JOB_ID-seq-1-native-active.ready"

# Admission does not wait for the long Job mutation fence held by sequence 1.
time ./bin/dorf message --job "$JOB_ID" --id owner-steer-1 --input-file .proof/issue-41/steer.txt
time ./bin/dorf message --job "$JOB_ID" --id owner-steer-1 --input-file .proof/issue-41/steer.txt
kill -KILL "$WORKER_PID"
wait "$WORKER_PID" || true
sleep 11

# Rescue expired attempt 3 before the terminal drain claims attempt 4.
time ./bin/dorf worker --once

# Recovery reconnects to the exact live authenticated app-server when possible, reads and reconciles
# sequence 1 without loading the thread, resumes the exact bound thread, and then starts exactly one
# serialized native turn for sequence 2 in the same Session.
unset DORF_PROOF_FAULT_BARRIER
time ./bin/dorf worker --once
time ./bin/dorf inspect "$JOB_ID"

psql "$DORF_DATABASE_URL" -c "select caller_id,sequence from dorf.job_messages where job_id='$JOB_ID' order by sequence"
psql "$DORF_DATABASE_URL" -c "select m.sequence,r.message_id,r.state,r.baseline_native_turn_id,r.native_turn_id,r.native_outcome from dorf.agent_runs r join dorf.job_messages m on m.id=r.message_id where r.job_id='$JOB_ID' order by m.sequence"
psql "$DORF_DATABASE_URL" -c "select message_id,kind,state,attempts,external_id from dorf.actions where job_id='$JOB_ID' order by created_at"

time ./bin/dorf cleanup --now "$JOB_ID"
time ./bin/dorf worker --once
time ./bin/dorf inspect "$JOB_ID"
incus list --format csv -c n,s | rg '^dorf-' || true
```

The terminal has exactly two immutable FIFO messages, two stable per-input Actions and AgentRuns,
one native Session, and at most one native turn per accepted message. Sequence 2 is admitted while
sequence 1 is active but starts only after sequence 1 completes. The repeated `owner-steer-1` call
returns `created=false` with the same message and sequence. Inspection names queued, active,
terminal, blocked, or genuinely uncertain delivery truth and identifies any blocking sequence and
reason; it never prints or stores transcript items. Cleanup closes admission before cancellation and
leaves the route revoked, the Sandbox deleted, and the cleanup task checkpointed.

The detached app-server keeps only replaceable operational control facts in the Sandbox. Its exact
PID is `/tmp/dorf/codex-app-server.pid`; the raw reconnect capability is retained separately at
`/tmp/dorf/codex-app-server.control-token` in a root-only directory and file. App-server argv contains
only `--ws-token-sha256 <digest>`, never the raw capability or the retained file path. Recovery reads
the raw capability only after matching the tracked PID to the expected app-server endpoint and auth
argv and matching its digest. A live process with a missing, mismatched, or rejected capability is
attention and is not killed; once no app-server is live, stale PID and capability files may be
atomically replaced. Neither file is native Session identity or PostgreSQL state, and proof output
must not print the capability.

Reconnect history inspection uses `thread/read`, which deliberately leaves a persisted thread
unloaded. Only after PostgreSQL/native-history reconciliation proves the next input is safe to
submit does Dorf call `thread/resume`; its response must return the exact bound native Session ID
before `turn/start` is allowed. A rejection records only the safe protocol method category, never
the app-server's arbitrary message or data.

Codex 0.146.0 does not create a rollout for an empty `thread/start`; closing that WebSocket can
discard the thread before another connection can read it. For FIFO sequence 1 only, Dorf therefore
keeps `thread/start` and the first `turn/start` on one protocol connection and does not complete the
Session Action until the native turn is accepted. If the connection dies before acceptance, the
empty thread has no durable identity and recovery starts the still-unsettled initial delivery again.
If acceptance may have happened, recovery lists the isolated Sandbox's Sessions and reads the one
persisted turn before deciding; it adopts that exact Session/turn and does not query or trust
`clientUserMessageId` deduplication. Multiple Sessions or turns persist attention instead of being
guessed. Once sequence 1 is bound, all later FIFO inputs use the read/reconcile/resume flow above.

Message admission and its Absurd wake hint do not commit atomically. The honest crash invariant is:
PostgreSQL commits the immutable message and FIFO sequence first, then Dorf emits the deterministic
event identity `dorf.job-message:<job>:<zero-padded-sequence>`. Absurd 0.5.0 events are immutable
first-write-wins, so every sequence has a distinct wake identity. A crash in between leaves accepted
input durable but may leave the task asleep; retrying the same caller ID and byte-identical input
returns the same message and sequence and re-emits that same event identity. Changed input conflicts,
and the worker treats every event only as a hint before rereading PostgreSQL delivery truth. The
PostgreSQL integration test `TestAbsurdDistinctMessageWakesResumeSeparateIdleCyclesInFIFO` exercises
that crash window and two later admissions across separate Absurd wait/wake cycles.

The long-lived message consumer uses versioned Absurd idempotency identity `run:v2:<job>`. When an
open schema-001/002 Job still points at `dorf-job-spine-v1`, repeating its complete admission may
replace `jobs.task_id` only after PostgreSQL verifies that the old task has the exact Job params,
legacy idempotency key, expected task name, and a terminal Absurd state. The new task is likewise
verified as the exact live `dorf-job-messages-v2` consumer before attachment. The v1 task row remains
historical Absurd evidence; an active predecessor, unrelated task, or key collision stops without
overwriting the current attachment. `TestMigration003PreservesCompletedGoJobFacts` runs that real
upgrade, reattachment, retry, later-message wake, and v1-evidence proof.

## Exact terminal and redelivery proof

The remainder of this section is the retained issue #40 one-turn proof record. Its task names and
row counts describe that historical Revision; use the issue #41 terminal above for current code.

Run these commands from the repository root. Replace the public repository, starting Revision, and
model only with values deliberately selected for the proof. Keep the admission key unchanged for
the repeated call. `--revision` accepts only a canonical lowercase full commit OID (40 hexadecimal
characters for SHA-1 or 64 for SHA-256); branch names, tags, and abbreviated hashes are rejected.
After checkout, the worker observes the guest's `HEAD` and requires it to equal that OID exactly.

```bash
git rev-parse HEAD
mkdir -p .proof
printf '%s\n' 'Inspect the cloned repository and report its current Git revision and top-level purpose. Do not modify files. Keep the response concise.' > .proof/goal.txt

time ./bin/dorf admit \
  --key issue-40-final-proof-v3 \
  --goal-file .proof/goal.txt \
  --repo https://github.com/aphronio/dorf.git \
  --revision "$(git rev-parse HEAD)" \
  --branch dorf/issue-40-final-proof-v3 \
  --provider "$DORF_PROVIDER_CONNECTION" \
  --model gpt-5.6-sol \
  --reasoning high

time ./bin/dorf worker --once
time ./bin/dorf inspect job-545a9cf0ec7e8930c45e

# Same complete input: created=false and the same Absurd task ID.
time ./bin/dorf admit \
  --key issue-40-final-proof-v3 \
  --goal-file .proof/goal.txt \
  --repo https://github.com/aphronio/dorf.git \
  --revision "$(git rev-parse HEAD)" \
  --branch dorf/issue-40-final-proof-v3 \
  --provider "$DORF_PROVIDER_CONNECTION" \
  --model gpt-5.6-sol \
  --reasoning high
time ./bin/dorf worker --once

psql "$DORF_DATABASE_URL" -c "select count(*) from dorf.jobs where admission_key='issue-40-final-proof-v3'"
psql "$DORF_DATABASE_URL" -c "select kind,state,attempts,external_id from dorf.actions where job_id='job-545a9cf0ec7e8930c45e' order by created_at"
psql "$DORF_DATABASE_URL" -c "select count(*) from dorf.sandboxes where job_id='job-545a9cf0ec7e8930c45e'"
psql "$DORF_DATABASE_URL" -c "select task_id,state,attempts from absurd.t_dorf_jobs order by enqueue_at"
incus list --format csv -c n,s | rg '^dorf-'

time ./bin/dorf cleanup job-545a9cf0ec7e8930c45e
time ./bin/dorf worker --once
time ./bin/dorf cleanup job-545a9cf0ec7e8930c45e
time ./bin/dorf worker --once
time ./bin/dorf inspect job-545a9cf0ec7e8930c45e
incus list --format csv -c n,s | rg '^dorf-' || true
go version -m ./bin/dorf
```

The expected terminal has one Job, one Sandbox record, one native Session, one AgentRun, one
`dorf-job-spine-v1` task and checkpoint, and seven stable Actions. Inspection renders the native
turn ID and terminal status but no transcript. After cleanup, the route and Sandbox observations
are `revoked` and `deleted`, the Incus list is empty for the Job, and repeating cleanup performs no
new external effect.

Absurd checkpoints are sequencing evidence, not exactly-once effect authority. Task claims may
briefly overlap after lease loss, so every clone, Sandbox, route, Session, turn, and cleanup effect
first receives a stable Dorf Action ID. A retry inspects the external authority and reconciles that
Action before deciding whether any effect is still required. A PostgreSQL transaction-scoped
advisory fence keyed by Job serializes all of those external effects across overlapping claims; a
later claimant waits, then observes the first claimant's durable Action receipt instead of executing
the pending clone, Session, or turn concurrently. Database or process loss releases the fence so
normal Action reconciliation can resume.

An observed Job means only that Dorf recorded the harness-native outcome. `completed`, `failed`, and
`interrupted` remain distinct native outcomes; `state=observed` does not assert success and does not
blindly resubmit a failed or interrupted turn.

Cleanup may also be scheduled after the run task is terminal `failed` or `cancelled`. While holding
the same Job fence, Dorf records that terminal failure fact in the Job row before scheduling the
cleanup task, so a partial route or Sandbox is not stranded and cleanup cannot race an active effect.
The image proof normally uses durable cleanup. If its worker has returned with a pending retry, its
`finally` path uses the bounded Go-only fallback below: cancellation, route revocation, and deletion
all reconcile the same stable Job and Action identities.

```bash
./bin/dorf cleanup --now "$JOB_ID"
./bin/dorf worker --once
./bin/dorf inspect "$JOB_ID"
```

The Job ID above is the deterministic SHA-256-derived identity for the literal admission key. If a
different key is used, take `job_id` from the first admission JSON rather than guessing it.

## Proof ledger

The parent #36 ledger should receive the captured output from the exact block above, including:

- starting and resulting Git Revision;
- wall-clock `time` output for admission, delivery, inspection, redelivery, and cleanup;
- every failed readiness or worker attempt and its repair;
- the one-row Dorf and Absurd counts, stable native identities, route revocation, and Incus deletion;
- `go version -m` output showing the Go executable and Absurd module with no Python process;
- deletion of the Python admission, CLI/SDK launch and end composition, Codex/Incus environment,
  route installation, clone helpers, replaceable controllers, and their effect-coupled tests.

Repo-owned Incus image and host-provisioning assets remain as operational evidence. Python
Checks/review/PR policy also remains for later replacement slices, but the package no longer
publishes a Python `dorf` CLI or `Dorf` facade and the Go path does not preserve `.dorf.toml`,
Worker, Room, or Assignment compatibility.

Do not post `routes.json`, gateway authority, route keys, environment dumps, or Codex transcript
content to the ledger.

## Observed Assignment proof — 2026-08-06

The implementation workspace and cloned Job both started at Revision
`2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c`. The Assignment used native PostgreSQL 16.14, Absurd
0.5.0, Incus 6.0.0 with QEMU 8.2.2, and private bridge `10.31.162.1/24`. It imported the immutable
credential-free v0.1.1 image fingerprint
`0c269e0aa0c5a765e45bb50542b64d06e6c55930b920754459643991c7349775`, added only the missing Git
package, and locally published proof image fingerprint
`aa6a802dc730620c53250d405a65c5cf37161c876772c3460fdc4545be2ffa53`. The guest reported Codex
0.146.0, contained no `/root/.codex/auth.json`, and received only its revocable route config and key.

The host-owned Go broker was pinned CLIProxyAPI 7.2.104 with release SHA-256
`993babb37b6de831600f0eb31527ca0f938337e1d1f837d5cf846263affa9724`. For this isolated Assignment
it used the Assignment-scoped provider route as its protected upstream connection, bound only to
the Incus bridge, and derived a separate per-Sandbox route. Neither route secret was printed or
retained in this repository. No Python process participated in migrate, doctor, admission, worker,
inspection, redelivery, or cleanup.

The accepted clean proof used admission key `issue-40-final-proof-v3`, Job
`job-545a9cf0ec7e8930c45e`, run task `019fd911-b672-7b43-a8e4-2768316cbd03`, and cleanup task
`019fd913-5449-7587-9f15-e02ef445fc39`. Observed timings were:

| Operation | First | Repeat |
| --- | ---: | ---: |
| Admit complete input | 0.03 s (`created=true`) | 0.01 s (`created=false`) |
| Go worker delivery | 66.14 s | 0.01 s completed-task redelivery |
| Inspect | 0.02 s | 0.00 s after cleanup |
| Schedule cleanup | 0.01 s | 0.00 s (`scheduled=false`) |
| Cleanup worker | 0.62 s | 0.01 s completed-task redelivery |

PostgreSQL contained one Job, one Sandbox record, one Session, one AgentRun, one run task, and one
cleanup task. All seven Actions had `attempts=1`. Native Session
`019fd912-81df-7b92-bb08-8dd07f8bc24a` and turn
`019fd912-82ab-7ec1-9952-699ef501082b` were observed with outcome `completed`; the guest clone was
on the admitted Revision and branch `dorf/issue-40-final-proof-v3`. Dorf inspection rendered those
bindings and the native outcome, while schema inspection found no transcript, message, item, or
context column. `go version -m` identified a Go 1.25 executable, Absurd 0.5.0, pgx, and the WebSocket
module with no Python dependency.

Cleanup revoked route `route-19a7fd1a72872c56`, deleted Sandbox
`dorf-d7fdfe8a04a35d2d78b0`, completed both Absurd checkpoints, and converged on
`cleanup_state=complete`. Repeated cleanup returned the same task and `scheduled=false`; route-state
inspection and `incus list` both found zero remaining slice resources. A separately retained
background proof Job was also reconciled, leaving no live `sandbox:job-*` route or `dorf-*` Incus
instance in the Assignment.

Failures were retained as evidence rather than hidden. The initial image launch needed QEMU; the
818 MiB Assignment host needed bounded swap to avoid an Incus OOM during image preparation. The
first real worker then exposed an orphan app-server race (`rejected its scoped control capability`,
37.21 s), and the next retry proved that an empty Codex Session is not durable across app-server
restart (`thread/read`, 1.83 s). The implementation now records both stable Actions before one
app-server lifecycle and tracks the native guest PID for exact teardown. Repeating cleanup exposed
a non-monotonic `complete` to `scheduled` projection; an integration regression and monotonic SQL
update fixed it. The earlier PostgreSQL-only delivery failure caused by absent Incus remains useful
diagnostic evidence, but the terminal above supersedes it as the merge proof.

## Post-review boundary proof — 2026-08-06

Critical-boundary repair Revision `257e90e375c12883af4f38e071acc5bf4fa3755f` was served read-only
over the private Incus bridge from a bare clone of this real repository because the Assignment's
short-lived GitHub push credential expired before the proof. A fresh PostgreSQL database applied
both Dorf migrations and the pinned Absurd 0.5.0 schema. Doctor reported every check ready. The
provider broker and Git daemon bound only to `10.31.162.1`, the exact configured `incusbr0` IPv4.

Admission key `issue-40-review-proof-v1` produced Job `job-c2331637a51588c8bb6f` and run task
`019fd968-b56f-740a-9d5d-ea2ef21a9e57`. Admission took 0.015 seconds and the real Go worker took
50.601 seconds. Sandbox `dorf-55b996f2fdbbe05f4158` cloned
`git://10.31.162.1:9418/dorf-review-proof.git`; both the admitted Revision and observed guest `HEAD`
were exactly `257e90e375c12883af4f38e071acc5bf4fa3755f`. Route
`route-5f78c959cad03b10` reached Codex through the exact bridge address. Native Session
`019fd969-7230-77a1-9569-1598b4a96943` and turn
`019fd969-7355-7962-a7ca-84464cfc6c11` completed. Inspection took 0.040 seconds and described
`observed` as a neutral native-outcome fact, not success.

Repeating the complete admission returned `created=false`, the same Job and run task, in 0.024
seconds. Worker redelivery took 0.010 seconds. PostgreSQL still contained exactly one Job, Sandbox
record, Session, and AgentRun. Durable cleanup scheduling took 0.041 seconds and its worker took
0.696 seconds. Cleanup task `019fd96a-437b-7e03-8c4c-dec58348d508` completed on attempt 1. Replaying
the validator's `cleanup --cancel-run --now` failure-safe path took 0.014 seconds and changed no
effect; cleanup-task redelivery took 0.011 seconds. All seven stable Actions remained succeeded at
one attempt, the route was observed `revoked`, and the Sandbox was observed `deleted`.

After proof, route state contained zero consumers, `incus list` contained zero `dorf-*` instances,
and no proof broker, Git daemon, listener, PostgreSQL database, gateway authority, bare repository,
or temporary binary remained. The deterministic suite for this Revision was Go unit, vet, race,
PostgreSQL advisory-fence and terminal-cancellation integration, Ruff, 180 retained Python tests,
lock validation, and diff checks; all passed.
