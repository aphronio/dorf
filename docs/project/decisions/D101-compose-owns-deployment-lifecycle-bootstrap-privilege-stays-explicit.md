# D101: Compose owns deployment lifecycle; bootstrap privilege stays explicit

- **Applicability:** current
- **Areas:** deployment, release, sandboxes
- **Read when:** Changing Compose topology, setup-applied lifecycle, bootstrap, or release images.
- **Decision history:** Accepted implementation direction — 2026-08-26; setup application refined by live
  `v0.5.4` dogfood — 2026-08-26; guided ingress topology refined by D102 — 2026-08-27; remote Incus
  and public Control API terminals passed — 2026-08-27; frozen clean-host reproduction deferred
  until its trigger — 2026-08-29; release image made reproducible without collapsing reusable
  layers — 2026-08-30
- **Decision:** Replace D071's standalone PostgreSQL container and D100's systemd units with one
  versioned static Dorf Docker Compose project. The
  [Remote Control API](../../control-api.md#deployment-services) owns its exact service and network
  topology. Each long-running responsibility has one foreground container process and Compose is
  its only supervisor.
- **Static delivery and operator lifecycle:** The application archive carries
  `dorf-compose.yaml` and `dorf-compose-incus.yaml`, and the installer places them beside the binary.
  Setup derives and atomically writes the protected `.env` in the generated project directory, then
  applies only those installed manifests through Compose and waits for factual readiness before
  continuing. It reapplies the same exact project whenever guided configuration changes its inputs.
  Calling `dorf setup` is sufficient deployment intent: there is no second permission prompt,
  manual Compose handoff, separate `dorf start`, or setup-start-setup cycle. Dorf Go code does not
  render Compose YAML, inspect arbitrary Docker resources, or expose a general Compose lifecycle
  wrapper. Humans and deployment agents use Compose directly only for advanced observation and
  process operations;
  [Getting started](../../getting-started.md#1-install-the-application-initialize-a-deployment-host)
  is the sole procedural authority. The installer still never starts services. Updating replaces
  the binary and static manifests; one subsequent setup run refreshes `.env`, applies the project,
  and continues through readiness.
- **Release image:** The release authority builds and pushes one exact Linux/amd64 semantic-version
  image at `ghcr.io/aphronio/dorf:MAJOR.MINOR.PATCH`. Official setup selects that reference with
  `pull_policy: always`. There is no production Docker image tar, local image cache, OCI parser,
  tag-to-image receipt, or Go-owned image loading and attestation path. The canonical recipe pins
  its package inputs and Dockerfile frontend, removes build-host and wall-clock package state, and
  exports layers at a fixed timestamp. Stable runtime dependencies and the linked Dorf binary stay
  in separate reusable layers. The exact source commit remains in Go VCS metadata rather than in
  layer timestamps. This makes same-input release images exact without forcing unchanged runtime
  layers to change on every commit.
- **Project topology:** The base project contains Compose-owned PostgreSQL, a one-shot migration,
  the durable worker, and the control API. Successful migration gates the worker and API.
  The Provider Gateway and guided Cloudflare Tunnel are optional profiled foreground services. The
  default project uses bridge networking, publishes PostgreSQL only on host loopback and the control
  API on host port `8745`, and never uses host networking or mounts the host Docker socket into a
  Dorf workload or Sandbox. When selected, the Tunnel reaches only the control API and Provider
  Gateway over their shared ingress bridge. The fresh one-shot migration reaches its
  checksum-pinned Absurd schema through the existing runtime-egress bridge before it exits.
- **Database boundary:** The Compose-managed deployment defined here always uses the PostgreSQL
  service and named volume in the project. Its fixed PostgreSQL image belongs in the static manifest,
  not mutable deployment state. `DORF_DATABASE_URL` remains a development, test, and explicitly
  manually supervised process override; it does not select another managed topology. There is no
  standalone PostgreSQL custody or released database handoff to preserve.
- **Capability custody:** The control API keeps database and HTTP admission authority but
  receives no Sandbox-provider, GitHub, Gateway, or Incus credential or socket. The already
  credentialed worker hosts one independently authenticated narrow reader exposing only the fixed
  read operations required by that API; it is not a standalone service. The exact capability and
  network separation lives in the
  [deployment-service authority](../../control-api.md#deployment-services).
- **Privilege and helpers:** Host prerequisites remain outside Dorf's runtime custody. The exact
  no-hidden-privilege and administrator-handoff contract lives in
  [Getting started](../../getting-started.md#1-install-the-application-initialize-a-deployment-host)
  and its equivalent manual authorities. The release ships the same small, inspectable helpers from
  `scripts/bootstrap/`; they remain explicit, idempotent recipes for their stated proven host, not a
  universal package manager or a second runtime reconciler. Setup materializes the same-host Docker
  and Incus helpers; the installer places the remote Incus helper beside the binary for a separate
  workstation. Setup invokes Compose under its same
  operator identity, root or non-root; Dorf never elevates, changes identity, runs helpers, installs
  Docker or Incus, or manages host privilege. Cloudflare remains in the existing
  guided browser/DNS/Tunnel flow; a shell wrapper would only duplicate that authority.
- **Sandbox boundary:** Incus remains a provider behind the existing Sandbox adapter rather than a
  universal deployment dependency. One Dorf Deployment configures at most one Incus endpoint and
  client identity; its profiles name their restricted project, storage pool, network, and exact
  guest-reachable Gateway URL. The same official Incus client can address a prepared local Unix
  socket or remote HTTPS daemon without a connection registry or selector layer. The
  `instance_port_forward` API extension introduced by Incus
  7.3 carries Dorf's private worker-to-guest app-server connection through that same control plane,
  so neither local nor remote workers require direct routes to guest addresses. This does not solve
  the separate guest-to-Provider-Gateway path. Guided local setup follows D036's one-time route
  rule; every other topology requires an explicit guest-reachable private/VPN address or public
  HTTPS ingress, and profile verification must prove it before admission. Dorf mounts neither a
  general operator CLI configuration nor an invented data-plane proxy. Guided remote HTTPS setup
  accepts only the fixed `dorf-remote` project, `dorfbr0` network, native Incus HTTPS on a Tailscale
  address, and a stable HTTPS Gateway URL. Its one-use offer yields a pinned server certificate and
  fresh client identity restricted to that project. No unreleased legacy Profile adoption or
  migration path remains.
- **Guided experience:** Missing infrastructure is not a documentation dead end. Interactive setup
  keeps its deliberate choices, exact plans, secret/browser pauses, profile creation, functional
  verification, default selection, and resumable progress. Once prerequisites exist, setup applies
  the base project, continues through Sandbox and Harness choice, AI connection and any guided
  Cloudflare flow, reapplies changed project inputs, verifies the selected Profile, and reaches ready
  as one continuous command. Humans receive concise explanation and a manual prerequisite path;
  agents receive an exact command and must still pause for consequential external authority.
- **Dogfood refinement:** A fresh-host `v0.5.4` terminal successfully installed and updated Dorf,
  then setup wrote its deployment configuration and stopped before the existing Sandbox, Harness,
  AI-connection, Cloudflare, Profile, and verification flow. Requiring the operator to locate the
  generated project, invoke Compose, and rerun setup preserved an internal implementation boundary
  at the cost of the accepted zero-friction setup experience. Setup therefore owns the narrow exact
  project application above; that does not transfer general Docker lifecycle or host custody to
  Dorf.
- **Compose dogfood correction:** Fresh `v0.5.5` setup proved two invalid manifest assumptions:
  migration could not download its pinned Absurd schema from an internal-only network, and Docker
  did not realize published ports for services attached only to internal networks. The corrected
  project reuses its ordinary runtime bridge for migration and host publication, maps the control
  API directly as `8745:8745`, and keeps PostgreSQL's host mapping on loopback. Setup also selects
  quiet Compose progress so transport events do not replace its own concise presentation.
- **Remote Incus proof:** A shared Linux controller without KVM reached native Incus HTTPS on an
  owner-controlled workstation through one Tailscale TCP 8443 grant. Enrollment retained an mTLS
  client restricted to `dorf-remote`. A verified Profile completed a real `gpt-5.6-sol` turn from
  an isolated VM through the public Provider Gateway, survived a worker restart, and returned an
  exact file. Cleanup removed the scoped Gateway route and VM. The workstation retained no active
  offers, and the controller retained no pending enrollment.
- **Why:** The API and worker need durable supervision, not a custom privileged service manager or
  a custom Compose manager. PostgreSQL is already containerized, while the systemd,
  standalone-container, image-acquisition, and Compose-manager implementations duplicate lifecycle,
  identity, readiness, update, and recovery concerns already owned by release artifacts and
  Compose. A bounded invocation of the installed project lets setup preserve its deliberately
  low-friction experience without making Dorf responsible for a host's package manager, init system,
  virtualization policy, general container lifecycle, or ingress product.
- **Supersedes:** D041's automatic host mutation, D071's deliberate Compose omission and standalone
  database reconciler, and D100's entire Dorf-owned service-lifecycle interface. D100's OpenAPI,
  authentication, Job-list, Problem, API/worker separation, and operator-owned custom control
  ingress decisions remain accepted; D102 owns the guided Cloudflare exception. Unreleased systemd,
  standalone-database, image-archive, Compose-manager, and legacy-Profile compatibility shapes are
  deleted rather than migrated.
- **Refines:** D012's local-Incus assumption, D036's Gateway and Cloudflare supervision, D067's
  provider-route wording, D070's Incus profile custody, and D097's API/worker deployment shape. It
  does not change their Job, Sandbox, Gateway, or authenticated-client authorities.
- **Early-release proof:** CI must pass on the exact release commit, the release authority must
  verify its immutable artifacts, and each changed external authority needs its relevant live
  terminal. The current deployment passed remote Incus, the public Control API, a real model Turn,
  process restart, file retrieval, route revocation, and VM cleanup. That evidence is enough for the
  current early release line. Run the repository-owned
  [frozen Compose VM harness](../../../scripts/integration/compose-vm.sh) before the first external
  operator relies on clean-host reproduction, or after a material change to installation,
  bootstrap helpers, Compose topology, or release packaging. The harness must then prove the same
  setup, restart, retrieval, and cleanup contract on a fresh host.
- **Reconsider when:** Compose cannot preserve deterministic migration and recovery under real
  process loss, a supported non-Docker deployment earns equal proof, rootless container operation
  materially changes the Docker authority boundary, or a second private Sandbox provider or
  repeated remote-host attachment makes an outbound connector concrete. The non-normative
  [private provider attachment playbook](../../research/private-provider-attachment.md) records the
  starting hypotheses for that investigation.
