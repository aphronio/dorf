# Dorf Guidance

## Authorities

The [North Star](docs/north-star.md) owns the product, its shape, vocabulary and
boundaries, and what was deleted with the conditions to add it back. Push back on changes outside
it. [Slices](docs/slices.md) owns the plan, each slice's proof, and carry-over facts.

## Development

- TypeScript on Node, with Bun for installs and scripts. Pin dependencies exactly and install with
  `bun install --frozen-lockfile --ignore-scripts`.
- Build on Pi Durable the way its README and `packages/durable/test/examples` do. Read the matching
  example before adding host-side state or a new mechanism. The package ships no examples; read them
  in `earendil-works/pi` at the tag of the pinned version, since `main` has features it lacks.
- Only the gate (`apps/gate`) and deployment files use Substrate terms (Actor, Worker,
  Atespace, ActorTemplate).
- One concept per file. Keep a module as a file until a second caller needs it; extract a package
  only when a second app imports it.
- Tests use Pi's faux model. Never put real model keys in tests, examples or CI.
- Prove a slice with the one command its row in [Slices](docs/slices.md) names.
- Use `gh` for GitHub repository operations. Write Markdown issue and PR bodies to a temporary
  file and pass it with `--body-file`.

## Check and prove

Run these from the repository root.

| Command | What it does | Time in a Sandbox |
| --- | --- | --- |
| `.agents/setup` | Installs every package from its lockfile; safe to run again | seconds |
| `bun run check` | Types of the agent, gate and page, and the page's lint | seconds |
| `bun run proof` | The proofs its script in `apps/agent/package.json` names, with the faux model, against local stand-ins of the gate | under a minute |
| `deploy/proof.sh <name>` | One file of `apps/agent/test` against a cluster running Substrate; needs `kubectl` | minutes |

`bun run proof` needs no cluster and no credentials, and CI runs it on every push. Its stand-ins
give each Sandbox a folder on one machine, so it proves the agent, not Substrate. What only
Substrate does is proved by the test files that call `needSubstrate()`. Say which you ran.

## In a Sandbox

A Thread working on Dorf runs in a gVisor Sandbox: Node, Bun, git, `gh`, curl, and a headless Chromium
that the `browser-harness` skill drives; no `kubectl`, no Docker and no namespaces, and no way into
the cluster. Run `.agents/setup` first, then `bun run check` and `bun run proof` as you work.

## Ship

- Commit with the Sandbox's git identity, in Conventional Commits whose subject states the impact.
- When the change touches only `apps/agent`, `apps/web` or `docs`, and check and proof pass, push
  to `main` and follow its CI run with `gh run watch`. If CI fails, fix it forward at once. Main
  deploys on its own a minute or two after CI passes; the newest of
  `gh api repos/{owner}/{repo}/deployments` and its statuses say whether your commit is live, held or
  rolled back.
- Push any other change to a branch and say which Substrate proof it needs: the gate, the Sandbox
  image, `deploy/`, CI and this file wait for the owner until staging exists.
- Reply with the `show-me` skill: the smallest picture that proves the change, sized to it, with as
  little text as it needs. Write commit messages with the `prose` skill.
- When something about Dorf got in your way, or you found a problem you did not fix, file it with
  `gh issue create --label friction`, describing the behavior in generic terms.

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
