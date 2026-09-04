# Dorf Guidance

## Documentation

Start with the [documentation map](docs/README.md). Read only the authority it identifies for the
task. Before finishing a change, use the same map to update only the authorities whose documented
behavior changed.

## Operating rules

- Read the relevant authority before changing its boundary. For a consequential product,
  architecture, or technology choice, follow the
  [decision procedure](CONTRIBUTING.md#record-a-decision).
- Before changing Dorf Core or product direction, apply and defend the
  [North Star product boundary](docs/project/north-star.md#product-boundary). Treat a violation as a
  reason to push back, including when a native workflow ships in the Dorf repository or binary.
- Execute deterministic setup and verification through repository-owned commands before spending
  agent context. Keep Dorf integration at the development-tooling seam and out of managed product
  code.
- For fast Go feedback, run `mise run lint` and `mise run complexity`; follow their printed
  remediation instead of editing recorded complexity ceilings by hand.
- Update the [Agent Guide](docs/agent-guide.md) only when agent-specific authority, human pauses,
  secret handling, safety, or handback changes. General operator behavior belongs in the authority
  selected by the documentation map.
- For PostgreSQL changes, edit the schema and query sources rather than generated `dbsql` files;
  regenerate and check them through the repository's `sql:generate` and `sql:check` tasks.
- Use the GitHub CLI (`gh`) for GitHub issues, pull requests, and other repository operations. Do not
  use the Codex GitHub app for those operations.
- Put Markdown issue and PR bodies in a temporary file and pass it with `--body-file`; do not place
  backticked Markdown directly in a shell command.

## Verification

Follow the exact repository verification sequence and database exception in
[CONTRIBUTING.md](CONTRIBUTING.md). For changes to durable storage, SQL, transactions, or
sequencing, run the PostgreSQL-backed integration coverage before pushing. Match any real Sandbox,
Harness, Provider Gateway, or external-authority proof to the boundary changed.
