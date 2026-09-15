# Dorf Guidance

## Authorities

Start with the [documentation map](docs/README.md). Read the owner of the boundary you are changing
and update it when its documented behavior changes. Follow
[CONTRIBUTING.md](CONTRIBUTING.md) for setup, verification, DCO sign-off, and consequential decisions.

Before changing Dorf Core or product direction, apply the
[North Star product boundary](docs/project/north-star.md#product-boundary). Push back on violations,
even when a native workflow ships in the Dorf repository or binary.

For delegated installation or operation, follow the [Agent Guide](docs/agent-guide.md).

## Development

- Use repository-managed commands. For fast Go feedback, run `mise run lint` and
  `mise run complexity`; follow their remediation rather than editing complexity ceilings by hand.
- For PostgreSQL changes, edit schema and query sources, then use `sql:generate` and `sql:check`.
  Do not hand-edit generated `dbsql` files.
- Durable storage, SQL, transaction, and sequencing changes require
  PostgreSQL-backed integration coverage before pushing. Match live Sandbox, Harness, Provider
  Gateway, or external-authority proof to the boundary changed.
- Use `gh` for GitHub repository operations, not the Codex GitHub app.
  Write Markdown issue and PR bodies to a temporary file and pass it with `--body-file`.

## Public repository confidentiality

This repository is public. Commit only reusable product code, documentation, and synthetic
verification data. Keep private application or customer names, repository and issue links,
conversation or task details, operational identifiers, deployment addresses, and cleanup records
out of tracked files and commit messages. Store deployment-specific evidence in the owning private
repository or untracked local receipts; describe the reusable behavior here in generic terms.

Before committing, review the staged diff for confidential information. Before pushing, review all
unpublished commits, including earlier versions of changed files. Removing private details from the
latest tree alone does not remove them from Git history. If unpublished history contains private
details, sanitize that history before publication. Never publish credentials or user content.
