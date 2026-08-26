# Dorf Guidance

## Context Map

Read only the authority relevant to the task:

- [Principles](docs/project/principles.md): judgment, abstractions, and vertical-slice completion.
- [North Star](docs/project/north-star.md): product direction, vocabulary, and experience.
- [Visual Style](docs/project/style.md): brand character, palette, artwork, and interface presentation.
- [Architecture](docs/project/architecture.md): storage, sequencing, recovery, and composition.
- [Decision Log](docs/project/decisions.md): accepted consequential choices and reconsideration triggers.
- [Provider Gateway](docs/project/provider-gateway.md): provider authentication, routing, and broker ownership.
- [Getting Started](docs/getting-started.md): deployment-host and remote-client installation and setup.
- [Support](docs/support.md): supported platforms, diagnostics, and fault attribution.
- [Agent Guide](docs/agent-guide.md): delegated installation and CLI-operation runbook.
- [Buzz Deployment](docs/implementation/buzz.md): Buzz infrastructure and operations.
- [Remote Control API](docs/control-api.md): shipped HTTPS client contract and accepted Compose
  deployment boundary.
- [Release Process](docs/releasing.md): release operator entry point.
- [Sandbox and VM Watchlist](docs/research/sandbox-vm-watchlist.md): non-normative candidates and
  current evaluation priority; consult when discussing or selecting Sandbox or VM providers.

Material under `docs/research/` and `docs/history/` is archival and non-normative. Read it only when
the task explicitly needs historical evidence, archived product exploration, or an ecosystem
comparison such as Sandbox or VM provider selection; neither directory is a source of Dorf
requirements.

## Operating Rules

- Keep each fact, contract, and procedure in one authoritative place. Link to it elsewhere instead of
  restating product direction, architecture, versions, inventories, commands, or proof steps. When
  an authority changes, update its pointers and remove stale copies.
- Read the relevant authority before changing its boundary. Update the Decision Log when making,
  revising, or reversing a consequential product, architecture, or technology decision.
- Before changing Dorf Core or product direction, apply and defend the
  [North Star product boundary](docs/project/north-star.md#product-boundary). Treat a violation as a
  reason to push back, including when a native workflow ships in the Dorf repository or binary.
- Execute deterministic setup and verification through repository-owned commands before spending
  agent context. Keep Dorf integration at the development-tooling seam and out of managed product
  code.
- When installation, setup prompts, profile or AI connection readiness, Job operation,
  Messages, retry, file retrieval, or cleanup UX changes, update its existing operator authority and the
  [Agent Guide](docs/agent-guide.md) in the same slice. Keep the guide concise and link to authority
  instead of copying detailed contracts into it.
- For PostgreSQL changes, edit the schema and query sources rather than generated `dbsql` files;
  regenerate and check them through the repository's `sql:generate` and `sql:check` tasks.
- Use the GitHub CLI (`gh`) for GitHub issues, pull requests, and other repository operations. Do not
  use the Codex GitHub app for those operations.
- Put Markdown issue and PR bodies in a temporary file and pass it with `--body-file`; do not place
  backticked Markdown directly in a shell command.

## Verification

Keep every slice runnable with the repository contract:

```bash
scripts/dev/setup.sh
.dorf/bin/mise run check
.dorf/bin/mise exec -- go build -o .dorf/bin/dorf ./cmd/dorf
.dorf/bin/dorf version
```

Run the PostgreSQL-backed integration suite locally before pushing changes to durable storage, SQL,
transactions, or sequencing. CI repeats deterministic unit and PostgreSQL integration coverage;
real affected Sandbox, Harness, Provider Gateway, and external-authority proof should match the
boundary changed.
