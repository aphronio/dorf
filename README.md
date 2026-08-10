# Dorf

Dorf is a local-first control plane that carries one coding goal durably through an isolated
Incus Sandbox to an exact-Revision GitHub pull-request proposal.

The supported product is one Go application. PostgreSQL stores Dorf facts, Absurd schedules
durable work, Codex app-server performs bounded agent work, and Git/GitHub remain authoritative for
the proposal. Dorf does not use Python, SQLite, a hosted durability account, or the host Docker
socket.

## Build

Dorf currently supports x86_64 Linux hosts with a local Incus daemon. macOS cannot host the local
Incus VM daemon and is not a supported Dorf host.

```bash
go build -o ./bin/dorf ./cmd/dorf
./bin/dorf version
```

Release archives contain the same static x86_64 Linux binary. See
[Getting started](docs/getting-started.md) for PostgreSQL, Incus, the credential-free Codex image,
Provider Gateway, GitHub App, and repository preparation.

## Product model

```text
A Job changes code in a Sandbox.
Actions and Checks do deterministic work.
AgentRuns do judgment.
Evidence proves claims about a Revision.
```

One admitted Job owns one isolated Sandbox and clone, branch, resumable implementation Session,
selected review plan, exact proposal, explicit outcome, and observable cleanup. Client and worker
processes may disappear; stable identities and external reconciliation prevent duplicate turns,
Sandboxes, pushes, and pull requests.

The main commands are:

```bash
dorf setup --provider personal-chatgpt
dorf doctor --provider personal-chatgpt
dorf admit ...
dorf worker
dorf message --job JOB --id STABLE_ID --input-file message.txt --intent steer
dorf inspect JOB
dorf abandon JOB
dorf cleanup JOB
```

`inspect` separates agent claims from observed Actions, Checks, Revision-pinned Evidence, Absurd
task state, GitHub proposal/outcome facts, and cleanup state.

## Development

The product remains one Go application; its repository contract also compiles dedicated PostgreSQL
query files:

```bash
scripts/dev/prepare.sh
scripts/dev/check.sh
scripts/build-release.sh dist/release
```

Preparation installs the pinned `sqlc` tool and converges the disposable PostgreSQL database.
The check command rejects stale generated query code, prepares every query against that live schema,
then runs the Go test and vet suites. Regenerate private query code with
`mise run sql:generate` after editing `internal/postgres/queries` or the baseline schema.
Developers who already use mise can install the versioned Go, PostgreSQL, sqlc, and `absurdctl`
toolchain with `mise install`; the preparation script remains the portable setup path.

Architecture and authority details are indexed in [docs/README.md](docs/README.md).
