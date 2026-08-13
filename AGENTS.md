# Dorf Guidance

## Context Map

Read only the authority relevant to the task:

- [Principles](docs/project/principles.md): judgment, abstractions, and vertical-slice completion.
- [North Star](docs/project/north-star.md): product direction, vocabulary, and experience.
- [Architecture](docs/project/architecture.md): storage, sequencing, recovery, and composition.
- [Showcase Ideals](docs/project/showcase-ideals.md): workflow-facing acceptance and verification DX.
- [Decision Log](docs/project/decisions.md): accepted consequential choices and reconsideration triggers.
- [Provider Gateway](docs/project/provider-gateway.md): provider authentication, routing, and broker ownership.
- [Orchestration](docs/project/orchestration.md): coordinating and recovering bounded issues or epics.
- [Core Setup](docs/implementation/core-setup.md): host setup, default deployment, and diagnostics.
- [sqlc Guide](docs/project/sqlc.md): schema, queries, generation, transactions, and type mapping.
- [Incus Image](docs/implementation/incus-image.md): image construction, release, and installation authorities.
- [Buzz Deployment](docs/implementation/buzz.md): Buzz infrastructure and dogfood operations.
- [Release Process](docs/releasing.md): release operator entry point.

Material under `docs/research/` and `docs/history/` is archival and non-normative. Read research only
for an explicit ecosystem comparison; neither directory is a source of Dorf requirements.

## Operating Rules

- Keep each fact, contract, and procedure in one authoritative place. Link to it elsewhere instead of
  restating product direction, architecture, versions, inventories, commands, or proof steps. When
  an authority changes, update its pointers and remove stale copies.
- Read the relevant authority before changing its boundary. Update the Decision Log when making,
  revising, or reversing a consequential product, architecture, or technology decision.
- Execute deterministic setup and verification through repository-owned commands before spending
  agent context. Keep Dorf integration at the development-tooling seam and out of managed product
  code.
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
real Incus, Codex, and GitHub proof should match the boundary changed.
