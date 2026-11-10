# Dorf Guidance

## Authorities

The [North Star](docs/north-star.md) owns the product, its shape, vocabulary and
boundaries, and what was deleted with the conditions to add it back. Push back on changes outside
it. [Slices](docs/slices.md) owns the plan, each slice's proof, and carry-over facts.

## Development

- TypeScript on Node. Pin dependencies exactly and install with `npm ci --ignore-scripts`.
- Build on Pi Durable the way its README and `packages/durable/test/examples` do. Read the matching
  example before adding host-side state or a new mechanism.
- Only the gate (`apps/gate`) and deployment files use Substrate terms (Actor, Worker,
  Atespace, ActorTemplate).
- One concept per file. Keep a module as a file until a second caller needs it; extract a package
  only when a second app imports it.
- Tests use Pi's faux model. Never put real model keys in tests, examples or CI.
- Prove a slice with the one command its row in [Slices](docs/slices.md) names.
- Use `gh` for GitHub repository operations. Write Markdown issue and PR bodies to a temporary
  file and pass it with `--body-file`.

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
