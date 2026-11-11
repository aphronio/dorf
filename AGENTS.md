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

## Check and prove

Run these from the repository root. Times are from a Dorf Sandbox; a laptop is faster.

| Command | What it does | Time |
| --- | --- | --- |
| `.agents/setup` | Installs every package from its lockfile; safe to run again | 22 s |
| `npm run check` | Types of the agent, gate and page, and the page's lint | 17 s |
| `npm run proof` | The agent's proofs (history, models, lifecycle, make-room) with the faux model, against local stand-ins of the gate | 2 min |
| `deploy/proof.sh <name>` | One file of `apps/agent/test` against a cluster running Substrate; needs `kubectl` | 0.5 to 2 min |

`npm run proof` needs no cluster and no credentials, and CI runs it on every push. Its stand-ins
give each Sandbox a folder on one machine, so it proves the agent, not Substrate. Isolation between Sandboxes, files and
processes surviving a pause, the egress fence, token injection and upgrades are proved only by
`deploy/proof.sh substrate`, `repository` and `substrate-upgrade`. Say which you ran.

## In a Sandbox

A Thread working on Dorf runs in a gVisor Sandbox: Node, git and `gh`, no `kubectl`, no Docker and
no namespaces, and no way into the cluster. Run `.agents/setup` first, then `npm run check` and
`npm run proof` as you work. A change to the gate, the Sandbox image or `deploy/` needs a Substrate
proof, which the owner runs until staging exists.

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
