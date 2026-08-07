# Revision-pinned repository Checks and Evidence

This is the executable terminal for issue #37. It extends the durable Go Job with one pinned
repository contract, deterministic setup, programmatic Git commits, Revision-scoped Checks, one
focused repair in the original Codex Session, and immutable Evidence in a deployment-owned local
content store.

The starting repository owns three direct commands in `.dorf.toml`: `prepare` is setup; `check`
and `smoke` are ordered Checks. Dorf pins their literal command strings from the exact starting
Revision before the implementation turn. They are shell commands because that is the repository's
human-readable development-tooling seam, not prompts, product hooks, a Dagger graph, or a generic
workflow. A changed `.dorf.toml` cannot weaken the pinned Checks for the active Job.

## Authorities and retention

- The Sandbox checkout and Git own branch, parent, tree, cleanliness, commit, and full Revision OID.
- PostgreSQL owns the current Revision, historical Revision line, pinned commands, Check outcomes,
  one-repair limit, and Evidence references. It stores no command output or transcript.
- Evidence bytes live under `DORF_EVIDENCE_ROOT` (default
  `$HOME/.local/state/dorf/evidence`) at `sha256/AA/REST`. Every reference records SHA-256, byte
  size, media type, producer, provenance, Action or Check, Revision, and bounded timing.
- Codex owns transcript prose. Inspection labels implementation and repair output as claims; only
  passing current-Revision Checks with independently rehashed observed Evidence make a Revision
  ready.

Setup and Check commands leave bounded completion receipts in the Sandbox before the worker records
PostgreSQL facts. The commit path writes a Git intent receipt containing exact parent, tree, branch,
and commit OID before `git update-ref`. Recovery reads those receipts and Git authority first. A
dirty, unborn, detached, diverged, changed-tree, or otherwise ambiguous checkout becomes durable
attention; Dorf does not guess or create another commit.

Command stdout and stderr are each capped at 512 KiB with explicit truncation flags. The worker
removes the scoped Provider Gateway route key before bytes enter Evidence and records that redaction;
repository commands receive no upstream credential or controller environment.

## Local deterministic verification

Use the pinned Absurd schema from [Go Job Spine](go-spine.md), then run:

```bash
UV_CACHE_DIR=.dorf/uv-cache uv sync --frozen --all-groups
go mod download
go test ./...
go vet ./...
go test -race ./...
export DORF_TEST_DATABASE_URL='postgresql:///dorf_test?host=/var/run/postgresql'
go test ./internal/postgres -count=1
UV_CACHE_DIR=.dorf/uv-cache uv run pytest
UV_CACHE_DIR=.dorf/uv-cache uv run ruff check .
UV_CACHE_DIR=.dorf/uv-cache uv lock --check
scripts/verify-python-package.sh
mkdir -p .dorf/bin
go build -o .dorf/bin/dorf ./cmd/dorf
go version -m .dorf/bin/dorf
```

The Python repository/Check/Evidence/repair implementation remains temporarily because deletion is
gated on the real Go terminal below. After that terminal passes, delete those Python components and
their coupled tests in the same issue branch; review, GitHub publication, and terminal-outcome code
remain owned by later slices.

## Exact Incus, repair, and SIGKILL terminal

Start from the issue implementation Revision, not an abbreviated hash. The bounded goal uses a real
test-first change whose first turn deliberately adds a failing regression test without its repair.
The workflow-owned repair message then supplies the failed Check, command, exit, exact Revision, and
Evidence digest to the same Session. The repair turn fixes product code and does not commit; Dorf
creates the second commit.

```bash
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf migrate
mkdir -p .proof/issue-37/barriers
printf '%s\n' 'Make one small test-first coding change. In this first turn, add the focused regression test that specifies the requested behavior but deliberately leave its implementation unrepaired so the repository Check fails. Do not run repository-wide checks and do not commit; Dorf owns Checks and Git commits.' > .proof/issue-37/goal.txt

./bin/dorf admit \
  --key issue-37-revision-evidence-final-v1 \
  --goal-file .proof/issue-37/goal.txt \
  --repo https://github.com/aphronio/dorf.git \
  --revision "$(git rev-parse HEAD)" \
  --branch dorf/issue-37-revision-evidence-final-v1 \
  --provider "$DORF_PROVIDER_CONNECTION" \
  --model gpt-5.6-sol \
  --reasoning high

export JOB_ID='job-REPLACE_FROM_ADMISSION'
export DORF_PROOF_FAULT_BARRIER_ENABLE='issue-37-external-sigkill-only'
export DORF_PROOF_FAULT_BARRIER_JOB="$JOB_ID"
export DORF_PROOF_FAULT_BARRIER_DIR="$PWD/.proof/issue-37/barriers"

# 1. Setup succeeded and its Sandbox receipt exists; PostgreSQL is not recorded.
DORF_PROOF_FAULT_BARRIER='setup-complete-before-record' ./bin/dorf worker --once &
worker_pid=$!
while kill -0 "$worker_pid" 2>/dev/null && ! find "$DORF_PROOF_FAULT_BARRIER_DIR" -name "$JOB_ID-*-setup-complete-before-record.ready" -print -quit | grep -q .; do sleep 0.1; done
kill -KILL "$worker_pid"; wait "$worker_pid" || true; sleep 11
./bin/dorf worker --once # rescue expired claim only

# 2. The implementation commit and Git receipt exist; its Revision is not in PostgreSQL.
DORF_PROOF_FAULT_BARRIER='commit-created-before-record' ./bin/dorf worker --once &
worker_pid=$!
while kill -0 "$worker_pid" 2>/dev/null && ! find "$DORF_PROOF_FAULT_BARRIER_DIR" -name "$JOB_ID-*-commit-created-before-record.ready" -print -quit | grep -q .; do sleep 0.1; done
kill -KILL "$worker_pid"; wait "$worker_pid" || true; sleep 11
./bin/dorf worker --once # rescue expired claim only

# 3. The intentional Check exited and its Sandbox receipt exists; its row is absent.
DORF_PROOF_FAULT_BARRIER='check-exited-before-record' ./bin/dorf worker --once &
worker_pid=$!
while kill -0 "$worker_pid" 2>/dev/null && ! find "$DORF_PROOF_FAULT_BARRIER_DIR" -name "$JOB_ID-*-check-exited-before-record.ready" -print -quit | grep -q .; do sleep 0.1; done
kill -KILL "$worker_pid"; wait "$worker_pid" || true; sleep 11
./bin/dorf worker --once # rescue expired claim only

# Attempt 4 records failure, admits one repair, resumes the Session, commits Revision 2, and
# reruns Revision-dependent verification without repeating setup.
unset DORF_PROOF_FAULT_BARRIER
./bin/dorf worker --once
./bin/dorf inspect "$JOB_ID"
./bin/dorf evidence verify "$JOB_ID"

psql "$DORF_DATABASE_URL" -c "select oid,parent_oid,tree_oid,generation from dorf.revisions where job_id='$JOB_ID' order by generation"
psql "$DORF_DATABASE_URL" -c "select name,revision,state,exit_code,evidence_id from dorf.checks where job_id='$JOB_ID' order by finished_at,id"
psql "$DORF_DATABASE_URL" -c "select producer,provenance,kind,revision,digest,byte_size from dorf.evidence where job_id='$JOB_ID' order by created_at"
psql "$DORF_DATABASE_URL" -c "select m.sequence,r.role,r.session_id,r.state from dorf.job_messages m join dorf.agent_runs r on r.message_id=m.id where m.job_id='$JOB_ID' order by m.sequence"

./bin/dorf cleanup --now "$JOB_ID"
./bin/dorf worker --once
./bin/dorf inspect "$JOB_ID"
./bin/dorf evidence verify "$JOB_ID"
incus list --format csv -c n,s | rg '^dorf-' || true
```

The barriers are disabled unless the issue-specific phrase, exact Job, point, and directory are
supplied. Each shortens only its current Absurd claim to ten seconds, writes one `.ready` marker,
and times out after eight seconds. Absurd 0.5.0 needs the rescue-only worker pass after each expiry.

Success means inspection says the second full Revision is ready; generation 1 retains the failed
Check and Evidence but cannot prove generation 2; setup has one Action and was not rerun; FIFO
sequence 2 has role `repair` and the same Session as sequence 1; every byte independently rehashes;
and cleanup deletes the route and Sandbox while retaining Revisions, Checks, and Evidence.

## Current implementation ledger

The implementation started at `edacb4754e758d035a03fdce4b3c497161432dc7`. Local verification on
2026-08-07 used checksum-verified Go 1.25.12 and PostgreSQL 16.14. The pinned Absurd 0.5.0 schema
rehash was `d34309370c539f3a51f2b36b69b1f77551f8e4a14480a1c8def8bb8f40fd9aab`.
Go unit, vet, race, and PostgreSQL integration passed, including historical failed Evidence,
same-Session repair, second-Revision readiness, and cleanup retention. All 180 retained Python
tests, Ruff, lock validation, package builds, and the direct Go smoke build also passed. The
Incus/Codex terminal, its Revision OIDs and Evidence digests, external SIGKILL outcomes, and Python
path deletion remain explicitly pending the outer orchestrator; no value is inferred or fabricated
before that run.
