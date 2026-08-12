# Contributing

Dorf's supported product is the Go application in `cmd/dorf` and `internal`. Use Go 1.25 or newer.

```bash
scripts/dev/prepare.sh
scripts/dev/check.sh
go build -o .dorf/bin/dorf ./cmd/dorf
.dorf/bin/dorf version
go version -m .dorf/bin/dorf
```

## DCO sign-off

Dorf uses the [Developer Certificate of Origin](https://developercertificate.org/)
(DCO) instead of a Contributor License Agreement. By adding a `Signed-off-by` line to
each commit, you certify that you wrote the contribution or otherwise have the right
to submit it under the project's Apache 2.0 license.

Sign off every commit with the `-s` flag:

```bash
git commit -s
```

The commit message must include a trailer matching your repository-local author identity:

```text
Signed-off-by: Jane Doe <jane@example.com>
```

Pull requests with an unsigned commit fail the DCO status check. Amend a single unsigned commit
with `git commit --amend --signoff --no-edit`; use an interactive rebase when several commits need
repair.

## Licensing of contributions

Contributions are submitted under the same Apache 2.0 license, per the DCO and section 5 of the
Apache License. You retain the copyright on your contributions.

The coding-to-PR workflow is the only requirements driver. Keep deterministic setup, Git,
verification, evidence, publication, retry, and cleanup in code; reserve AgentRuns for judgment.
Do not add a compatibility facade, a second workflow, a plugin system, a provider registry, a
generic durability interface, or host Docker access.

Changes touching architecture, durable authority, setup, image distribution, Provider Gateway, or
release behavior must read the corresponding documents linked from [AGENTS.md](AGENTS.md). Prefer a
small runnable vertical slice and delete superseded code and tests once its real terminal works.

Run one broad suite at a time on small development machines. Repository preparation installs the
pinned Go, Absurd, and `sqlc` tools plus PostgreSQL when the VM does not already have them, then
converges a disposable `dorf_test` database. `scripts/dev/check.sh` rejects stale generated SQL,
prepares every query against that live schema, and runs the Go test and vet suites. PostgreSQL
integration tests run only when `DORF_TEST_DATABASE_URL` names that disposable database.
