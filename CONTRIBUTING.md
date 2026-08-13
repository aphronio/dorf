# Contributing

Use the repository-managed toolchain and run the repository contract:

```bash
scripts/dev/setup.sh
.dorf/bin/mise run check
.dorf/bin/mise exec -- go build -o .dorf/bin/dorf ./cmd/dorf
.dorf/bin/dorf version
.dorf/bin/mise exec -- go version -m .dorf/bin/dorf
```

`setup.sh` bootstraps a checkout-local Mise, installs the locked repository toolchain, and converges
the disposable PostgreSQL test database. `.dorf/bin/mise run check` rejects stale generated SQL and runs query
preparation, Go tests, and vet. Set `DORF_TEST_DATABASE_URL` to use an external test database.

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
