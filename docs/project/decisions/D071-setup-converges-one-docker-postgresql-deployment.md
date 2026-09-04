# D071: Setup converges one Docker PostgreSQL deployment

- **Applicability:** historical
- **Areas:** deployment, persistence
- **Read when:** Reviewing the former Docker PostgreSQL setup and deployment-custody contract.
- **Decision history:** Superseded by D101 — 2026-08-26
- **Decision:** Keep Dorf as a native Go binary with PostgreSQL as its external durability authority.
  The supported ordinary deployment has one database shape: Dorf-owned Docker PostgreSQL. There is
  no backend selection, native PostgreSQL installer, Compose wrapper, or separate host-install
  command. `dorf setup` is the one convergent entry point.
- **Docker custody:** Dorf owns one labeled `dorf-postgres` container and one labeled persistent
  `dorf-postgres-data` volume. It uses the reviewed PostgreSQL 17.10 Bookworm image tag, retains and
  re-attests the exact resolved image identity, binds the database only to loopback, stores its
  generated credential in the mode-0600 deployment record, starts a stopped owned container, and
  refuses same-named resources without exact Dorf ownership or configuration. The host Docker socket
  remains control-plane authority and is never exposed to a Sandbox or AgentRun.
- **Bootstrap:** Setup first observes the common Docker/PostgreSQL host requirements, reconciles
  PostgreSQL, and migrates Absurd and Dorf storage. Once that common foundation is ready, interactive
  setup asks which Sandbox providers this host should prepare and accepts zero or more; the repeatable
  `--sandbox-provider` flag supplies the same choice for automation. Selecting Incus adds Incus,
  QEMU, KVM, services, and group access to a second exact observed plan. Selecting only E2B adds no
  local virtualization requirement. Provider selection is setup input, not a durable enablement
  registry; named verified profiles remain runtime authority. If supported Ubuntu 24.04 changes are
  missing, a Huh prompt renders directly from the exact plan that will be applied; `--yes` approves
  that same plan for automation. A new login is reported only when newly granted group membership
  cannot affect the current process. Already-ready non-Ubuntu hosts are accepted, but Dorf does not
  mutate their packages. When at least one Sandbox provider is selected, setup continues through
  Harness choice, protected upstream authentication, provider credential and remote-Gateway input
  when required, exact profile creation, functional verification, and default selection. Success
  therefore means the selected profile can admit Agent work; selecting no provider deliberately
  stops after the common foundation. Explicit profile and provider commands remain composable
  operator surfaces, not required follow-up chores for the ordinary path.
  Rerunning setup freshly verifies every selected profile rather than treating a historical receipt
  as current provider availability.
  Guided E2B setup defaults to the exact public Dorf Standard template build compiled into that
  Dorf release; `--e2b-template` remains the explicit bring-your-own exact-build path.
- **Deliberate omission:** `DORF_DATABASE_URL` remains the existing advanced and test override, but
  there is no database-provider registry, native database path, Compose project,
  external-database command, database migration command, or automatic backend conversion. Those
  require measured deployment needs.
- **Why:** Clean Omarchy dogfood showed that an otherwise portable binary could not begin because
  PostgreSQL was absent, while adding distribution-specific package installation would expand host
  support without improving reproducibility. Docker supplies one consistent local PostgreSQL
  environment across already-Docker-capable hosts without containerizing Dorf or compromising its
  direct Incus and host authority boundaries.
- **Reconsider when:** Docker is absent on a supported non-Ubuntu host with real users, a remote
  deployment needs first-class database configuration, or container lifecycle or backup
  requirements exceed this bounded local custody.
