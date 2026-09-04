# Contributing

Use the repository-managed toolchain and run the repository contract:

```bash
mise trust --yes
mise install --locked
docker compose -f compose.dev.yaml up --detach --wait postgres
mise run db:init
mise run check
mise run build
.dorf/bin/dorf version
mise exec -- go version -m .dorf/bin/dorf
```

Contributors and coding agents provide Mise and Docker Compose. Mise installs the locked native
toolchain, while [`compose.dev.yaml`](compose.dev.yaml) supplies disposable PostgreSQL. The
repository-default test database URL lives in [`.mise.toml`](.mise.toml);
`DORF_TEST_DATABASE_URL` may override it. Skip the Compose start when using an external database.
`mise run db:init` idempotently initializes the Absurd and Dorf schemas. `mise run check` rejects
stale generated SQL and runs query preparation, Go tests, and vet without rebuilding an image.
Self-hosted deployments use the published image through `deploy/compose.yaml` and do not require
Mise.

Use the [documentation map](docs/README.md) to find the authority for a product, architecture,
storage, provider, setup, image, or release boundary before changing it. Coding agents must also
follow [AGENTS.md](AGENTS.md).

Run `mise run docs:check` for focused documentation validation. It checks decision records and
generated indexes, then validates repository-local paths and heading anchors in Markdown. It makes
no network requests and does not decide whether prose still describes the product correctly.

## Record a decision

For a new consequential product, architecture, or technology choice:

1. Update the document that owns the current boundary.
2. Add the next contiguous `DNNN-stable-slug.md` record under `docs/project/decisions/`. Keep that
   filename for the life of the record.
3. Set `Applicability`, `Areas`, `Read when`, and `Decision history`. The generator validates these
   routing fields.
4. Run `mise run decisions:generate`.

If the choice revises or reverses an earlier decision, add a new record. Do not rewrite the earlier
decision or its rationale. Change its `Applicability` to `partial` or `historical`, and append the
new decision ID to its `Decision history`. Then regenerate both indexes.

Edit an existing record without adding a new one only to clarify its wording, correct an error, or
append evidence that does not change the choice.

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
