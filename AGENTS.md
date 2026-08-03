# Dorf Guidance

Dorf is a local-first control plane for durable AI Workers in isolated Rooms.

## Context Map

Keep this file as the compact operating guide. Read deeper context only when the task touches it:

- [Principles](docs/project/principles.md): default product and architecture judgment; read before
  introducing a new abstraction, backend, workflow, or managed-repo integration, and before scoping
  or declaring complete any implementation slice. Its vertical-slice rule defines what counts as a
  terminal.
- [North Star](docs/project/north-star.md): aspirational goal for the agent runtime / control plane building
  block (workers, rooms, jobs); coding-to-PR and AFK capacity are showcases, not the core. Read for product
  direction and DX taste; not an API spec.
- [Showcase Ideals](docs/project/showcase-ideals.md): workflow-layer DX direction (contract-first
  acceptance, verification ladders, autonomy labels, calibration) that composes on the runtime and
  must not leak into it; read alongside the north star for showcase-facing changes.
- [Decision Log](docs/project/decisions.md): accepted choices, their rationale, and reconsideration triggers;
  consult it before changing an established choice, and update it when making, revising, or reversing
  a consequential product, architecture, or technology decision.
- [Runtime Surface](docs/project/runtime.md): portable lifecycle boundary, built-in adapter
  responsibilities, and compatibility posture; read for runtime integration or boundary changes.
- [Provider Gateway](docs/project/provider-gateway.md): shared provider connections, scoped
  inference routes, local broker ownership, and the client/Room composition boundary; read for
  model-provider authentication, routing, or broker work. D036 retains the durable project decision.
- [Orchestration](docs/project/orchestration.md): durable operating protocol for sequencing an epic,
  choosing agent settings, maintaining dogfood evidence, and recovering after interruption or
  context compaction; read it when acting as an epic orchestrator.
- [Core Setup](docs/implementation/core-setup.md): accepted core-only first-run and summon DX,
  official latest-validated Codex image, guided Incus installation, convergent setup, and
  agent-readable diagnostics; read before changing host setup, image distribution, default
  deployment configuration, or setup troubleshooting.
- [Incus Image](docs/implementation/incus-image.md): local VM provisioning and ChatGPT-subscription authentication;
  read for image, toolchain, credential, or Incus setup changes.
- [Buzz Deployment](docs/implementation/buzz.md): persistent pinned Buzz deployment and private
  Tailscale access; read for Buzz infrastructure, lifecycle, credentials, or dogfood setup.
- [Release Process](docs/releasing.md): Python distribution verification, Trusted Publisher setup,
  TestPyPI rehearsal, and production release procedure; read before package publication changes.

Material under `docs/research/` is archival and non-normative. Read it only when a task explicitly
requires ecosystem comparison; it is not a source of Dorf requirements.

## Working Rules

- The coding-to-PR workflow is the only current runtime-semantics requirements driver. SDK clients
  compose those primitives rather than creating a second Dorf workflow. Do not add
  support for hypothetical research, app-builder, deployment, swarm, provider, or other workflows.
- The Provider Gateway is a sibling application subsystem, not a provider registry in
  `dorf.runtime`. The ChatGPT-to-Codex route and scoped client routes are its current implementation
  drivers; validate each later provider and wire dialect before claiming support.
- One coding task slice maps to one goal-backed Job, Assignment, isolated clone, branch, and PR
  proposal. Human-requested revision continues those identities; merge, rejection, or abandonment
  is workflow-terminal, while explicit resource ending remains a separate lifecycle operation.
- Execute deterministic setup and verification programmatically through repo-owned commands before
  spending agent context. Keep Dorf integration at the development-tooling seam and out of
  managed product code.
- Incus VM is the only current environment adapter. Codex app-server is the first agent driver;
  tmux and SSH remain break-glass observation and takeover tools.

## Command Expectations

Use `uv` for Python project commands once dependencies are installed.

Use the GitHub CLI (`gh`) for GitHub operations such as viewing, closing, or updating issues and pull requests. Do not use the Codex GitHub app for those operations in this repository.

When creating or editing GitHub issue or PR bodies that contain Markdown backticks, write the body to a temporary file and pass it with `gh --body-file` or `gh ... --body-file`. Do not put backticked Markdown directly inside a shell command string; the shell will treat backticks as command substitution.

```bash
uv run dorf --help
uv run pytest
uv run ruff check .
```

Do not add broad abstractions without a concrete second implementation or an observed workflow need.
