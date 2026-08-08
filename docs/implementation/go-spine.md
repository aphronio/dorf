# Go/Absurd coding Job terminal

This is the supported Dorf product path. A complete Job and later client messages are admitted to
Dorf-owned PostgreSQL facts, scheduled and recovered through Absurd, executed in one
credential-free Incus Sandbox, and delivered to one resumable Codex Session. Repository setup,
commits, exact-Revision Checks and Evidence, deterministic review selection, material repair,
GitHub proposal publication, explicit outcome, and exact cleanup are phases of the same durable
Job.

No Python process, package, facade, SQLite store, hosted durability account, or host Docker socket
participates.

## Preparation

Follow [Getting started](../getting-started.md), then prove every local and external authority:

```bash
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf setup --provider "$DORF_PROVIDER_CONNECTION"
./bin/dorf doctor \
  --provider "$DORF_PROVIDER_CONNECTION" \
  --contract .dorf.toml \
  --repo https://github.com/aphronio/dorf.git \
  --github-repo aphronio/dorf \
  --github-installation "$GITHUB_INSTALLATION_ID" \
  --base greenfield
```

## Terminal

Admit one complete goal with a stable key, exact starting Revision, unique branch, named provider,
model selection, and exact GitHub App authority. Start `dorf worker`, then admit a second stable
message with `--intent steer` while the implementation turn is active.

Kill and restart the controller and task executor at the documented proof barriers. Absurd lease
recovery plus Dorf Action reconciliation must preserve exactly one Job, Sandbox, branch, Session,
native turn per accepted message, repository effect, and pull request. A timeout never authorizes a
blind repeat.

The worker then:

1. runs the repository's programmatic preparation contract;
2. reuses the original implementation Session through commit and any material repair;
3. records Checks and content-addressed Evidence against the exact Revision;
4. derives ChangeFacts and persists the deterministic selected ReviewPlan;
5. runs only selected review AgentRuns, treating findings as claims;
6. returns a material finding to the original Session and rechecks the new exact Revision;
7. reconciles the exact branch push and GitHub pull-request proposal; and
8. waits at GitHub's human authority boundary.

After observing GitHub, record `accepted`, `rejected`, or `abandoned` with `dorf outcome`. Cleanup
remains separate until every scoped provider route, review resource, app-server mutation, and Incus
Sandbox is reconciled. `dorf inspect` must show the outcome and `cleanup_state=complete`.

## Focused deterministic proof

```bash
go test ./...
go vet ./...
go build -o .dorf/bin/dorf ./cmd/dorf
.dorf/bin/dorf version
go version -m .dorf/bin/dorf
```

With `DORF_TEST_DATABASE_URL` configured, the integration tests exercise PostgreSQL admission,
FIFO/wake crash windows, exact Action identities, Revision/Evidence invalidation, review/repair,
publication ambiguity, outcome authority, task-executor loss, fencing, and cleanup. The final live
Incus, Codex, steering, GitHub, external SIGKILL, and human outcome proof remains an orchestrator
step because tests cannot manufacture those authorities.
