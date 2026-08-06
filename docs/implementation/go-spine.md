# Go durable Job spine

This is the executable terminal for issue #40: a complete Job is admitted to Dorf-owned PostgreSQL
facts, scheduled in Absurd, delivered by a Go worker to one credential-free Incus Sandbox, and
observed through a real Codex app-server turn. Cleanup is a second durable task which revokes the
Sandbox's inference route before deleting the VM.

The demonstrated commands use the compiled Go binary only. PostgreSQL and Incus are native host
services; Dorf neither starts a container nor needs a cloud durability account or host Docker
socket. An already connected Provider Gateway is a setup prerequisite. Its persistent
`cli-proxy-api` broker owns upstream authentication; the Go path issues and revokes only a scoped
consumer route and never prints its key.

## Local prerequisites

Install Go 1.25, PostgreSQL 16 or newer, and Incus. On Ubuntu, PostgreSQL can be a normal local
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
to that private bridge address.

One-time upstream provider connection remains an owner setup operation. After it exists, record its
name and the gateway state location without exporting any upstream or downstream secret:

```bash
export DORF_PROVIDER_GATEWAY_STATE="$HOME/.local/state/dorf/provider-gateway"
export DORF_PROVIDER_CONNECTION='primary'
```

Initialize the exact Absurd schema through the Go binary. This avoids making Python part of either
database setup or the demonstrated Job. Dorf verifies the upstream file's pinned SHA-256
`d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab` before executing it.

```bash
curl -fsSLo /tmp/absurd-0.5.0.sql \
  https://raw.githubusercontent.com/earendil-works/absurd/0.5.0/sql/absurd.sql
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf migrate --absurd-schema /tmp/absurd-0.5.0.sql
./bin/dorf doctor --provider "$DORF_PROVIDER_CONNECTION"
```

Diagnostics distinguish PostgreSQL connectivity, Absurd version and queue, Incus command access,
private network, image availability, and provider-route authority. The worker separately refuses an
image containing an upstream credential or old route key. Each failed item includes a local repair.
No check probes Docker.

## Exact terminal and redelivery proof

Run these commands from the repository root. Replace the public repository, starting Revision, and
model only with values deliberately selected for the proof. Keep the admission key unchanged for
the repeated call.

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

The Job ID above is the deterministic SHA-256-derived identity for the literal admission key. If a
different key is used, take `job_id` from the first admission JSON rather than guessing it.

## Proof ledger

The parent #36 ledger should receive the captured output from the exact block above, including:

- starting and resulting Git Revision;
- wall-clock `time` output for admission, delivery, inspection, redelivery, and cleanup;
- every failed readiness or worker attempt and its repair;
- the one-row Dorf and Absurd counts, stable native identities, route revocation, and Incus deletion;
- `go version -m` output showing the Go executable and Absurd module with no Python process;
- deletion of `src/dorf/workflows/coding_admission.py` and
  `tests/test_coding_admission.py` in this slice.

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
