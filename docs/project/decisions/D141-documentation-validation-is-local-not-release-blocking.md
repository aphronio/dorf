# D141: Documentation validation is local, not release-blocking

- **Applicability:** current
- **Areas:** release
- **Read when:** Changing the CI validation gate or contributor verification commands.
- **Decision history:** Separates documentation validation from blocking CI, 2026-09-16.
- **Decision:** Local development and CI use `mise run check` for the existing SQL and Go checks.
  Documentation authors run `mise run docs:check` separately. No additional code-check task or
  scheduled documentation workflow is added.
- **Why:** The maintainer accepts documentation-link and generated-index drift as a local review
  responsibility rather than a reason to block release publication. The measured documentation
  check is inexpensive; this is a process choice, not a claimed material performance improvement.
- **Preserved behavior:** All SQL generation checks, PostgreSQL-backed tests, static analysis,
  artifact and installer proofs, clean-source provenance, and immutable publication checks remain
  blocking. Documentation validation still rejects invalid metadata, stale indexes, and broken
  local links when invoked.
- **Tradeoff:** CI can succeed while documentation validation would fail. Run `mise run docs:check`
  before contributing documentation changes.
- **Authority:** [Contributor verification](../../../CONTRIBUTING.md) owns the local commands;
  [release process](../../releasing.md) owns CI and publication gates.
