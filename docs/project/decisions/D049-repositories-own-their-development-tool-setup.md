# D049: Repositories own their development-tool setup

- **Applicability:** partial
- **Areas:** workflows, sandboxes, deployment
- **Read when:** Changing how repositories prepare development tools and services inside Sandboxes.
- **Decision history:** Accepted setup boundary — 2026-08-09; fixed image-inventory sentence superseded by
  D064 and D066 while repository ownership remains accepted — 2026-08-13; PostgreSQL installation
  changed from a Mise source build to the native distribution package — 2026-08-14; development
  database moved from host-package bootstrap to Docker Compose — 2026-08-26
- **Decision:** Do not expand the shared Incus image for Dorf-specific Go, Absurd, PostgreSQL, or
  inspection needs. D064 and D066 own the current shared image profile. A Dorf development host or
  Sandbox supplies Mise and Docker Compose. Repository-owned deterministic commands install the
  trusted, locked native tools through `mise trust --yes` and `mise install --locked`, start the
  static `compose.dev.yaml` PostgreSQL service, and idempotently initialize the Absurd and Dorf
  schemas through `mise run db:init`. Dorf's declared `commands.prepare` selects those deterministic
  Actions; no AgentRun decides or performs setup conversationally. `mise run check` retains the
  native feedback loop. No repository script installs host packages or manages Docker or Compose.
  D101's self-hosted deployment remains image-only and does not require Mise.
- **Cost:** Every fresh Sandbox may pay tool and PostgreSQL image download and initialization time,
  and development environments must supply both prerequisites. That cost is accepted because the
  setup remains explicit, repository-versioned, and independent of a Dorf-wide development image.
- **Why:** Different repositories need different toolchains. Encoding all of them in one shared
  image couples repository evolution to image publication and grows a supposedly reusable base.
  A repository contract is the simplest correct ownership boundary. Mise owns pinned native tools,
  Compose owns the disposable database service, and native checks preserve the fast edit/verify loop
  without a privileged host-package bootstrap.
- **Reconsider when:** Repeated measurements show setup materially dominates Job latency or network
  reliability. First add a content-addressed package cache; if that is insufficient, snapshot a
  successfully prepared repository environment behind the same `commands.prepare` contract.
