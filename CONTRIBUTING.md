# Contributing

Use the repository-managed toolchain and run the repository contract:

```bash
mise trust --yes
mise install --locked go sqlc github:earendil-works/absurd
docker compose -f compose.dev.yaml up --detach --wait postgres
mise run db:init
mise run check
mise run build
.dorf/bin/dorf version
mise exec -- go version -m .dorf/bin/dorf
```

Contributors and coding agents provide Mise and Docker Compose. Mise installs the locked native
toolchain, while `compose.dev.yaml` supplies PostgreSQL 17.10 at
`127.0.0.1:55432/dorf_test`. Mise uses the matching connection URL unless
`DORF_TEST_DATABASE_URL` overrides it; skip the Compose start when using that external database.
`mise run db:init` idempotently initializes the Absurd and Dorf schemas. `mise run check` rejects
stale generated SQL and runs query preparation, Go tests, and vet without rebuilding an image.
Self-hosted deployments use the published image through `deploy/compose.yaml` and do not require
Mise.

Read [AGENTS.md](AGENTS.md) before changing a documented product, architecture, storage, provider,
setup, image, or release boundary.

## DCO sign-off

Dorf uses the [Developer Certificate of Origin](https://developercertificate.org/) (DCO) instead of
a Contributor License Agreement. By adding a `Signed-off-by` line to each commit, you certify that
you wrote the contribution or otherwise have the right to submit it under the project's Apache 2.0
license.

Sign off every commit with:

```bash
git commit -s
```

The trailer must match your repository-local author identity:

```text
Signed-off-by: Jane Doe <jane@example.com>
```

Pull requests with unsigned commits fail the DCO check. Amend one with
`git commit --amend --signoff --no-edit`; use an interactive rebase for several commits.

## Licensing

Contributions are submitted under the Apache 2.0 license. Contributors retain copyright in their
contributions.
