# Sandbox and VM provider watchlist snapshot

Snapshot date: 2026-08-14

This archived, non-normative snapshot records how Dorf ranked Sandbox and VM candidates on
2026-08-14. Its rankings, prices, capabilities, and proposed evaluation sequence are not current
guidance. Recheck current primary sources before making a decision.

## Criteria used for the snapshot

The 2026-08-14 ranking used the desired disposable-workstation experience in this order:

1. **VM fidelity:** a dedicated kernel or hardware microVM, root access, systemd, persistent disk,
   and normal Linux behavior.
2. **Workstation capability:** run Docker and Docker Compose, browsers, arbitrary processes, and
   network services with usable endpoints.
3. **Agent-friendly control:** stable identity, create/get/list/delete reconciliation, exec with
   stdin/stdout/stderr/exit status, files, process control, snapshots or stop/start, and a clean API.
4. **Controlled networking:** private connectivity, scoped ingress, enforceable egress, and a safe
   path to Dorf's Provider Gateway without placing upstream credentials in the Sandbox.
5. **Value and adoption friction:** starter credits or a useful free tier, transparent usage
   pricing, good price-to-capability ratio, and no mandatory cloud account for a local-only runtime.

The planned first portability proof also had to preserve Dorf's authority over Jobs, Messages,
AgentRuns, Evidence, Provider Routes, outcomes, and cleanup. The snapshot treated providers as
machine suppliers rather than replacements for Dorf's workflow or agent control plane.

## P0 in the 2026-08-14 snapshot

- [E2B](https://e2b.dev/docs) — Managed in-house Firecracker-like microVM runtime with an explicit
  Docker and Docker Compose template, browsers, public endpoints, pause/resume, snapshots, metadata,
  and list/adopt lifecycle APIs; its free Hobby tier includes $100 in one-time credits, making it the
  best no-fixed-cost managed spike. A [live Dorf capability proof](e2b-capability-proof.md) passed
  systemd, root, Docker Compose, Chrome, authenticated ingress, metadata adoption, full-memory
  pause/resume, egress denial, and cleanup. The remaining tradeoffs are no official Go SDK, a
  one-hour continuous Hobby runtime, remote Provider Gateway reachability, and a newly announced
  runtime whose detailed architecture is not yet published. See the
  [2026-08-13 announcement](https://x.com/mlejva/status/2087947332507807830).
- [smol machines](https://smolmachines.com/) — Open local and managed hardware microVMs with the
  same workload model, persistent disks, OCI images, nested Docker, headless Chromium, ports,
  deny-by-default networking, deterministic names, exec, files, stop/start, snapshots, and forks.
  Local is free; cloud has no minimum or card requirement and includes $10 monthly credit. This is
  the strongest local-to-managed candidate, but the [cloud proof](smol-cloud-capability-proof.md)
  found a documented-default network mismatch and the
  [local v1.8.0 proof](smol-local-capability-proof.md) found a hardened-service DNS blocker. Keep it
  behind the E2B slice until one prepared workstation passes both surfaces and the adapter choice is
  resolved; the current unified SDK is Node/Python.
- [Namespace Devboxes](https://namespace.so/docs/devbox) — Managed persistent development machines
  with Docker-oriented images, durable execs, endpoints, strong egress controls, Tailscale, a mature
  Go SDK, a 30-day trial, and per-minute pricing with no compute charge while paused; confirm exact
  isolation and create idempotency.
- [Daytona](https://www.daytona.io/docs/en/sandboxes/) — Managed Linux VM or container Sandboxes with
  Docker support, browser/computer use, ports, persistence, snapshots, files, and a Go 1.25 SDK;
  strong credits and pricing, but managed VM entitlement and private Gateway reachability need proof.
- [Runloop Devboxes](https://docs.runloop.ai/docs/devboxes/overview) — Managed microVM devboxes with
  Docker-in-Docker, custom images, browsers, tunnels, snapshots, metadata, and a complete REST API;
  good trial value, while private owner networking and some persistence features are paid upgrades.

## P1 in the 2026-08-14 snapshot

- [Freestyle VMs](https://www.freestyle.sh/products/vms) — Managed hardware microVMs with root,
  systemd, nested KVM, persistent disk, suspend, snapshots, forks, endpoints, and private VPC access;
  Free includes up to 10 concurrent VMs and daily CPU, memory, and storage allowances, but persistent
  VMs, persistent snapshots, and custom sizing require a $50/month minimum, making it a weaker fit
  before Dorf has paying users; the VM API is beta.
- [Blaxel Sandboxes](https://docs.blaxel.ai/Sandboxes/Overview) — Managed microVMs with OCI images,
  memory and disk standby, durable named processes, files, endpoints, deterministic identity, a Go
  SDK, and no base subscription; prove Docker Compose and hard egress enforcement.
- [Hypeman](https://github.com/kernel/hypeman) — Open-source self-hosted VM daemon for Linux and
  Apple silicon with OCI images, snapshots, forks, volumes, endpoints, tags, and an authenticated
  OpenAPI surface; promising but early, and Docker Compose plus file transfer need a real spike.
- [Docker Sandboxes](https://docs.docker.com/ai/sandboxes/) — Local Linux microVMs on macOS, Linux,
  and Windows with a private Docker Engine, Compose, browsers, services, persistent state, and safe
  host-Docker isolation; mandatory Docker account sign-in, proprietary distribution, CLI-only
  control, and 0.x maturity add avoidable friction to a local-only runtime.
- [CubeSandbox](https://cubesandbox.com/architecture/overview) — Self-hosted KVM microVMs with OCI
  templates, a Go SDK, files, PTY, pause/resume, snapshots, metadata, and network policy; powerful but
  young and operationally heavier because Dorf must run the cluster.
- [NVIDIA OpenShell](https://docs.nvidia.com/openshell/latest/) — Open Linux/macOS microVM or container
  runtime with strong filesystem, process, and deny-by-default network policy; alpha and overlaps
  Dorf's provider, inference-routing, and policy authority.
- [exe.dev](https://exe.dev/docs/) — Persistent KVM/Cloud Hypervisor VMs with OCI images, Docker,
  browsers, public services, SSH, Tailscale, and unusually good small-team pricing; its CLI-over-HTTP
  and SSH control surface lacks durable exec and robust create reconciliation.
- [Box](https://box.ascii.dev/) — Low-cost full Ubuntu VMs with Docker, Chrome, desktop streaming,
  dedicated IPs, public services, stop/resume, snapshots, files, and commands; $20 buys roughly 555
  default-VM hours after its trial. Hold for Dorf because create accepts no caller identity,
  metadata, or idempotency key and the public API does not document permanent deletion, custom
  images, or enforceable egress controls.
- [NVIDIA Brev](https://docs.nvidia.com/brev/latest/guides/ai-agents/agent-sandboxes) — Managed cloud
  VMs with Docker, custom environments, endpoints, SSH, exec, and persistent workspaces; useful for
  later GPU profiles, but CLI-only automation and weak private-network and reconciliation contracts
  reduce its priority.
- [Archil persistent Sandboxes](https://docs.archil.com/compute/persistent-sandboxes) — Managed Linux
  microVMs with persistent disks, pause/resume, forks, PTYs, and durable exec IDs; preview preemption,
  an eight-hour powered-session limit, and missing private networking currently block a real proof.

## P2 in the 2026-08-14 snapshot

- [Cloudflare Sandbox](https://developers.cloudflare.com/sandbox/) — GA isolated VMs with rich exec,
  files, browser workloads, endpoints, excellent egress controls, and credential injection; idle
  sleep loses local state, Docker Compose is not the target shape, and the Go-facing bridge cannot
  yet reconcile an uncertain create.
- [Modal Sandboxes](https://modal.com/docs/guide/sandboxes) — Managed gVisor Sandboxes with an official
  Go SDK, deterministic identity, OCI images, exec, files, endpoints, snapshots, and monthly credits;
  the standard runtime is not a full VM, lives at most 24 hours, and lacks native stop/resume.
- [SkyPilot Sandboxes](https://docs.skypilot.co/en/latest/sandboxes.html) — BYOC Kubernetes Sandboxes
  with OCI images, exec, names, volumes, warm pools, and scale; limited early access, shared-kernel by
  default, Python-first, and operationally much heavier than a direct VM provider.
- [machinen.dev](https://github.com/redwoodjs/machinen.dev) — Fast local microVMs for Linux and Apple
  silicon with snapshot, restore, and live fork; source and a durable remote provider API are not yet
  available, so Dorf would have to build the provider around it.
- [Cloudflare Computer](https://github.com/cloudflare/computer) — Experimental durable-computer layer
  over Cloudflare execution backends; interesting future work, but preview-only and too overlapping
  with Dorf's workspace and runtime composition.

## P3 in the 2026-08-14 snapshot

- [Miren Runtime](https://miren.md/) — Mature open container PaaS with Dockerfiles, disks, networking,
  and exec, but its apps, versions, pools, routing, and scaling form a second orchestrator rather than
  a thin Sandbox provider.
- [Google Cloud Run Sandboxes](https://docs.cloud.google.com/run/docs/code-execution) — Nested
  Sandboxes inside an existing Cloud Run instance, not durable external machines Dorf can reconcile
  after controller or parent-instance loss.
- [Runra](https://runra.dev/docs/) — New managed Sandbox service built on CubeSandbox with basic
  lifecycle, files, ports, and pause/resume; too new and less direct than evaluating Cube itself.

## Names needing disambiguation

- **Run** — confirm whether this means [Runloop](https://runloop.ai/),
  [Cloud Run Sandboxes](https://docs.cloud.google.com/run/docs/code-execution), or
  [Runra](https://runra.dev/) before evaluating it.
- **Box** — this list assumes [Box by ASCII](https://box.ascii.dev/).

## Evaluation sequence proposed on 2026-08-14

The **E2B**, **smol cloud**, and **smolvm local v1.8.0** capability spikes were complete. The working
preference was one narrow **E2B** second-provider slice first because its bounded proof passed the
most complete workstation terminal. This was not a support or adapter decision. The proposed order
then put smol local/cloud next after resolving its DNS, explicit-network, prepared-image, and remote
Provider Gateway gaps. **Namespace**, **Daytona VM**, and **Runloop** followed if needed.
**Freestyle** was deferred until a fixed $50/month persistence commitment could be justified.

The proposed acceptance bar for every candidate was: immutable image, Docker Compose, headless
browser, authenticated served endpoint, scoped Provider Gateway route, controller-loss
reconciliation, stop/start persistence, and confirmed deletion.

The adapter implementation was open. The proposal compared the provider's official SDK, a small
direct or generated Go client, and a narrowly isolated SDK sidecar only against the operations
earned by the E2B slice. It rejected building a provider registry or cross-language adapter
framework in advance. Dorf's
use of [Absurd's official Go SDK](https://github.com/earendil-works/absurd/tree/main/sdks), alongside
its Python and TypeScript SDKs, is a useful upstream-maintenance precedent—not a requirement that
every Sandbox provider expose all three languages.

For the next local profile, the snapshot proposed rerunning **smolvm** only after its DNS/control-plane
gap changed in a newer pinned release, with **Hypeman** next otherwise. It proposed comparing
**Docker Sandboxes** only if its built-in Docker Compose experience outweighed mandatory account
sign-in and proprietary distribution, testing Linux and Apple silicon where applicable, and
expressing the combined Harness image as a reproducible OCI build rather than preserving
Incus-specific packaging.
