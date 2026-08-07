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
  passing current-Revision Checks with independently rehashed and decoded observed Evidence make a
  Revision ready. Each proving artifact repeats its stable Check identity and exact Revision; its
  command, exit code, bounded timing, producer, and provenance must match the PostgreSQL Check and
  Evidence rows before readiness is persisted or rendered.

Setup and Check commands leave bounded completion receipts in the Sandbox before the worker records
PostgreSQL facts. The commit path uses a deterministic author and committer timestamp exactly one
second after its parent, so a crash after `git commit-tree` but before the receipt recreates the same
OID. It then writes a Git intent receipt containing exact parent, tree, branch, and commit OID before
`git update-ref`. Recovery reads those receipts and Git authority first. A dirty, unborn, detached,
diverged, changed-tree, or otherwise ambiguous checkout becomes durable attention; Dorf does not
guess or create another commit.

After the FIFO appears drained, Dorf locks the Job row, rechecks for an unsettled admitted input,
and changes `implementing` or `repairing` to `committing` in the transaction that reserves the
Revision Action. An admission that commits first is delivered before the Revision; an admission
that loses that boundary is rejected before a native turn can start. Existing caller-ID retries
remain readable. `committing` is an additive recovery phase handled by the existing
`dorf-job-messages-v2` task, so already-admitted v2 Jobs are not reattached or stranded and no task
identity change is required.

A repository setup command that exits nonzero is terminal for that setup Action and its Evidence.
After an operator repairs the concrete environment, `dorf setup-retry` atomically selects one new
setup Action scoped by a stable retry identity and admits a byte-exact FIFO repair note as the
durable wake. An
exact client retry returns the same Action and message; a stale completion from the superseded
Action is rejected. The failed observation remains inspectable and is never deleted or rewritten.
Before sleeping, the task selects the reserved retry message or the oldest otherwise-deliverable
pending FIFO position before choosing a future sequence, so admission between an idle observation
and event wait cannot strand work. Failed earlier AgentRuns still block later messages rather than
turning an already-consumed event into a busy loop.

Command stdout and stderr are each capped at 512 KiB with explicit truncation flags. Before
truncation or Evidence retention, the worker removes both known Room-scoped bearer capabilities:
`/root/.config/dorf/provider-route.key` and
`/tmp/dorf/codex-app-server.control-token`. The bounded allowlist records which redactions occurred;
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
gated on the real Go terminal below. The pinned `check` command intentionally begins with
`scripts/verify-issue-37-legacy-deletion.sh`, which exits 37 while this superseded surface exists.
This is the expected first-Revision Check failure, not a pre-terminal verification command.

After the Go terminal has proved setup, commit, Checks, Evidence, and recovery, the single focused
repair removes exactly:

- `src/dorf/repo_contract.py`, `src/dorf/command_runner.py`, and
  `src/dorf/workflows/coding_commands.py`;
- `tests/test_repo_contract.py` and `tests/test_command_runner.py`;
- `_check_gate`, `_ready_gate`, `_run_verify_fix`, `verify_fix_prompt`, and
  `verify_job_readiness` from `src/dorf/workflows/coding.py`, with their coupled check/fix tests from
  `tests/test_coding_workflows.py`; and
- the `_VERIFICATION_COMMANDS` Check projection and its coupled Check-Evidence assertions from
  `src/dorf/workflows/coding_dossier.py` and `tests/test_coding_dossier.py`.

Remove obsolete imports and exports mechanically. Review is owned by the Go review-policy path; the
superseded Python reviewer has no retained process-execution seam. Preserve GitHub publication,
terminal outcome, host setup, the core image, Provider Gateway, generic claim documents, and release
code. The guard names the production paths and symbols above and passes only when this bounded
deletion is complete.

## Exact Incus, repair, and SIGKILL terminal

Start from the issue implementation Revision, not an abbreviated hash. The first bounded Go change
hardens `evidence.Store.ReadVerified` against invalid byte sizes and is complete with passing focused
regression tests in the initial implementation turn. It does not deliberately leave a failing Go
test: the intentional first-Revision failure is solely the legacy-deletion guard. The workflow-owned
repair message then supplies that failed Check, command, exit, exact Revision, and Evidence digest to
the same Session. The repair turn performs only the documented legacy deletion and does not commit;
Dorf creates the second commit.

```bash
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf migrate
mkdir -p .proof/issue-37/barriers
printf '%s\n' 'Make one small Go coding change with one focused regression test and the smallest implementation that satisfies it. Do not alter the repository contract or legacy-deletion guard, do not run repository-wide checks, and do not commit; Dorf owns Checks and Git commits.' > .proof/issue-37/goal.txt

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
and times out after eight seconds. On recovery, an exact marker for the same Job, stable identity,
point, and bounded payload means the boundary was already reached and execution continues; corrupt
or conflicting bytes stop. Absurd 0.5.0 needs the rescue-only worker pass after each expiry.

Success means the first full Revision fails only the legacy-deletion guard with observed Evidence;
the same-Session repair performs the exact deletion above; and inspection says the second full
Revision is ready. Generation 1 retains the failed Check and Evidence but cannot prove generation
2. A clean terminal has one successful setup Action; an explicitly repaired environment retains
both the terminal failed setup Action/Evidence and its one successful retry Action. The
workflow-owned Check repair has role `repair` and the same Session as the implementation; every
proving artifact independently rehashes and matches its Check row; and cleanup deletes the route
and Sandbox while retaining Revisions, Checks, and Evidence.

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

The consolidated pre-terminal repair added the atomic `committing` admission boundary,
payload-validated barrier resumption, deterministic pre-receipt Git commit identity, independent
artifact-to-Check readiness verification, both concrete Room capability redactions, and the
intentional deletion guard. Focused service recovery tests cover setup-complete, commit-created,
and Check-exited before their PostgreSQL records; PostgreSQL integration covers both sides of the
late-steering boundary and rejects row-only readiness. The retained Python suite is run directly
before the terminal; the pinned contract guard is expected to fail until the same-Session repair
performs the documented deletion.

The first outer setup observation on 2026-08-07 exited 127 because the published `dorf-codex`
image contained Git, Node, uv, and Codex but no Go executable. Dorf retained Evidence digest
`748a1205459e4b8f89553bcb0eb75c30c9e0651b7e222dbef7ac3d4912c9fca5` and blocked the Job. The
operator then installed the official Go 1.25.12 Linux amd64 archive in that exact Sandbox after
verifying SHA-256 `234828b7a89e0e303d2556310ee549fbcf253d28de937bac3da13d6294262ac1`.
Migration `006_setup_retry.sql` and `dorf setup-retry` preserve the failed generation while giving
the repaired environment a distinct setup Action and durable wake. The public image/toolchain
contract remains separate cutover resistance #55. Exact successful retry and later barrier
artifacts are retained in the Job inspection and epic ledger when the terminal runs.
