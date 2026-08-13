# Dorf Guidance

Dorf Core is the open-source control plane for running supported agent Harnesses on infrastructure its
owner controls. Coding-to-PR with Codex on local Incus is the verified baseline. Prove a second
Harness on Incus, then Codex on a second Sandbox provider, before general workflow authoring.

## Context Map

Keep this file as the compact operating guide. Read deeper context only when the task touches it:

- [Principles](docs/project/principles.md): default product and architecture judgment; read before
  introducing a new abstraction, backend, workflow, or managed-repo integration, and before scoping
  or declaring complete any implementation slice. Its vertical-slice rule defines what counts as a
  terminal.
- [North Star](docs/project/north-star.md): accepted Core product vocabulary,
  deterministic/agentic boundary, workflow examples, current verified slice, and high-level
  experience. Read for product direction and DX taste; not an API spec or backlog.
- [Greenfield Architecture](docs/project/architecture.md): accepted Go, Absurd, and PostgreSQL boundaries,
  authority model, recovery rules, local/hosted shape, and Python cutover constraints. Read before changing
  runtime storage, durable sequencing, service composition, or the replacement strategy.
- [Showcase Ideals](docs/project/showcase-ideals.md): workflow-layer DX direction (contract-first
  acceptance, verification ladders, autonomy labels, calibration) that composes on the runtime and
  must not leak into it; read alongside the north star for showcase-facing changes.
- [Decision Log](docs/project/decisions.md): accepted choices, their rationale, and reconsideration triggers;
  consult it before changing an established choice, and update it when making, revising, or reversing
  a consequential product, architecture, or technology decision.
- [Provider Gateway](docs/project/provider-gateway.md): shared provider connections, scoped
  inference routes, local broker ownership, and the client/Sandbox composition boundary; read for
  model-provider authentication, routing, or broker work. D036 retains the durable project decision.
- [Orchestration](docs/project/orchestration.md): stable protocol for taking a bounded issue from
  observed state to a verified terminal and recovering after interruption; read it when acting as
  an epic orchestrator.
- [Core Setup](docs/implementation/core-setup.md): accepted core-only first-run and summon DX,
  official latest-validated Codex image, guided Incus installation, convergent setup, and
  agent-readable diagnostics; read before changing host setup, image distribution, default
  deployment configuration, or setup troubleshooting.
- [sqlc Working Guide](docs/project/sqlc.md): stable query, transaction, type-mapping, generation,
  and verification patterns; read before changing the PostgreSQL schema, named queries, generated
  query package, or `postgres.Store` mappings.
- [Incus Image](docs/implementation/incus-image.md): local VM provisioning and ChatGPT-subscription authentication;
  read for image, toolchain, credential, or Incus setup changes.
- [Buzz Deployment](docs/implementation/buzz.md): persistent pinned Buzz deployment and private
  Tailscale access; read for Buzz infrastructure, lifecycle, credentials, or dogfood setup.
- [Release Process](docs/releasing.md): Go artifact and credential-free Incus image verification and
  publication; read before release changes.

Material under `docs/research/` is archival and non-normative. Read it only when a task explicitly
requires ecosystem comparison; it is not a source of Dorf requirements.

## Working Rules

- Coding-to-PR is the only currently verified workflow. Core portability is the next proof: add a
  second supported Harness on Incus, then Codex on a second Sandbox provider, then cross the second
  Harness and provider. Common client and workflow code must not branch beyond profile selection and
  capability admission. General workflow authoring follows that proof and does not authorize a
  generic workflow API.
- Trusted clients such as the CLI or Agent0 compose the same application boundary. CI, HTTP,
  webhooks, MCP, schedules, Slack, and user interfaces are trigger or presentation adapters, not
  workflow authorities.
- The Provider Gateway is a sibling application subsystem, not a provider registry in the durable
  Job core. The ChatGPT-to-Codex route and scoped client routes are its current implementation
  drivers; validate each later provider and wire dialect before claiming support.
- One coding task slice maps to one goal-backed Job, isolated Sandbox and clone, branch, and PR proposal.
  Human-requested revision continues the Job and Session. Acceptance, rejection, or abandonment is a Job
  outcome; cleanup remains a separate observable lifecycle fact until reconciled.
- Execute deterministic setup and verification programmatically through repo-owned commands before
  spending agent context. Keep Dorf integration at the development-tooling seam and out of
  managed product code.
- Incus is the first Sandbox provider. Codex app-server is the first Agent runner;
  tmux and SSH remain break-glass observation and takeover tools. Describe multiple Harnesses and
  Sandboxes as product direction until a real second implementation proves each seam.
- The Go and Absurd replacement has no compatibility or old-data requirement. Do not add SQLite/PostgreSQL
  dual writes, migrations, Python facades, deprecated CLI paths, or tests that preserve superseded behavior.

## Command Expectations

Use the Go commands declared by the repository contract and keep every slice runnable.
Run PostgreSQL-backed integration tests locally before pushing changes to durable storage or
sequencing. CI repeats deterministic unit and PostgreSQL integration coverage as an independent
merge gate; real Incus, Codex, and GitHub proof remains proportional to the boundary changed.

Use the GitHub CLI (`gh`) for GitHub operations such as viewing, closing, or updating issues and pull requests. Do not use the Codex GitHub app for those operations in this repository.

When creating or editing GitHub issue or PR bodies that contain Markdown backticks, write the body to a temporary file and pass it with `gh --body-file` or `gh ... --body-file`. Do not put backticked Markdown directly inside a shell command string; the shell will treat backticks as command substitution.

```bash
scripts/dev/prepare.sh
scripts/dev/check.sh
go build -o .dorf/bin/dorf ./cmd/dorf
.dorf/bin/dorf version
```

Do not add broad abstractions without a concrete second implementation or an observed workflow need.
Workflow evaluations belong to each workflow contract from its first real slice; a public authoring
API, plugin system, or marketplace requires further external proof.
