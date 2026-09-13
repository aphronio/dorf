# Retained agent infrastructure comparison

Research date: 2026-09-13. Non-normative evaluation, not a provider selection or support claim.
This supplements the dated [sandbox watchlist](sandbox-vm-watchlist.md).

## Evaluation scope

One retained assistant environment per product user, plus temporary delegated worker environments.
The harness stays inside the environment. Both use the same Dorf lifecycle; the client chooses
retention and when work is idle. See the [product boundary](../project/north-star.md#product-boundary).

The accepted persistence baseline is a filesystem that survives normal stop/start, with periodic
off-host backups, initially daily. Losing changes since the last successful backup after hardware
failure is acceptable. No snapshot after every turn is required. Long-running work must not face a
mandatory short session deadline. Full-memory resume is optional; native history and useful files
must survive a cold start. Temporary mounts and boot cleanup still need explicit coverage.

Shared infrastructure commitments are acceptable when distributed across users. A separate fixed
provider subscription for every product user is not the intended economic model.

## Comparison

Operational burden below is our assessment, not a measured provider property. Self-hosted runtime
software and managed hosting are different purchase units: software does not include servers,
storage, network, backup service, or an operations team.

| Candidate | Persistence approach | Cost model | Operational tradeoff / next proof |
| --- | --- | --- | --- |
| [Fly Machines](https://fly.io/docs/volumes/overview/) | Attached local volume; native daily volume snapshots; cold start must work | Running VM usage, retained volume, snapshot and stopped-rootfs storage | Managed hosts; arrange all necessary state on the volume and test restore. Suspend memory is disposable. |
| [ASCII Box](https://docs.ascii.dev/box/snapshots) | Provider captures selected filesystem changes on stop; latest saved state retained | Shared plan credits and running machine usage; start quotas matter | Managed hosts; verify exact capture paths, guest capabilities and account quotas. |
| [smol Cloud](https://www.smolmachines.com/docs/cloud/lifecycle-storage-networking) | Persistent local machine disk; checkpoint/export path for backups | Running-machine base plus consumed CPU/RAM and used disk | Managed hosts; establish scheduled off-host backup and cold-start behavior. Temporary mounts are excluded from persistence. |
| [smolvm self-hosted](https://github.com/smol-machines/smolvm) | Persistent machine disks, stopped `.smolmachine` exports and constrained portable live checkpoints | Self-hosted Apache-2.0 runtime; shared host capacity, storage and operations | Lightweight CLI/REST deployment; v1.15.0 passed basic lifecycle/ingress but DNS and native export failed in the hardened disposable test. Distinct from smol Cloud. |
| Existing Incus fleet | Persistent VM disks and operator-managed backup | Shared host capacity, storage, network and operations | Existing Dorf integration is the baseline to beat; backup permissions, restore procedure and host capacity remain operator work. |
| [Hypeman](https://github.com/kernel/hypeman) | Persistent writable overlays, attached volumes, disk or memory snapshots, snapshot schedules | Self-hosted MIT runtime; pay for host fleet and backups | Smaller deployment than CubeSandbox; own host placement, capacity and off-host recovery. Worth the first new self-hosted proof. |
| [CubeSandbox](https://github.com/TencentCloud/CubeSandbox) | Local copy-on-write disks/snapshots; optional S3 packages support cross-node restore | Self-hosted Apache-2.0 project with third-party component licenses; pay for cluster, storage and operations | Built-in cluster scheduling and E2B-compatible API. Thirty synthetic environments ran on one node; abrupt-stop recovery of paused entries failed. See the ten-user proof below. |

[Freestyle](https://www.freestyle.sh/docs/vms/lifecycle) remains a managed memory-pause alternative;
[exe.dev](https://exe.dev/pricing) remains a pooled-resource alternative needing fleet terms.
E2B is the previously tested recovery baseline, but its Hobby continuous-runtime cap is inconvenient
for long uninterrupted work. These remain candidates rather than selected Dorf profiles.

## Hypeman: smaller self-hosted runtime

Reviewed upstream commit `625f8204da1888fc89fdfab2d5b843ec11573994` through a librarian checkout.

Its authenticated remote API runs OCI images with Cloud Hypervisor, Firecracker or QEMU on Linux,
and Virtualization.framework on Apple silicon. Linux requires KVM. It exposes stop/start and
standby/restore, exec, ingress and volume operations. The instance manager keeps a writable overlay
and metadata on the host. Snapshot kinds distinguish disk-focused stopped snapshots from snapshots
containing memory/device state. These are useful primitives for keeping Dorf's harness inside a VM.
[README](https://github.com/kernel/hypeman/blob/625f8204da1888fc89fdfab2d5b843ec11573994/README.md),
[instance layout](https://github.com/kernel/hypeman/blob/625f8204da1888fc89fdfab2d5b843ec11573994/lib/instances/README.md),
[snapshot behavior](https://github.com/kernel/hypeman/blob/625f8204da1888fc89fdfab2d5b843ec11573994/lib/snapshot/README.md).

Expiration can be disabled by omitting expiration fields or setting a zero relative lifetime.
Per-instance snapshot schedules support intervals and retention, including daily captures. Scheduled
captures of running guests briefly enter standby and restore; they are not free of disruption.
The reviewed snapshot store is local. Off-host backup must preserve the necessary payloads and
dependencies; a local scheduled snapshot alone is not a backup against disk failure. Exporting an
OCI base image should not be mistaken for exporting a live instance's state.
[API](https://github.com/kernel/hypeman/blob/625f8204da1888fc89fdfab2d5b843ec11573994/openapi.yaml),
[schedule policy](https://github.com/kernel/hypeman/blob/625f8204da1888fc89fdfab2d5b843ec11573994/lib/scheduledsnapshots/README.md),
[snapshot store](https://github.com/kernel/hypeman/blob/625f8204da1888fc89fdfab2d5b843ec11573994/lib/snapshot/store.go).

Assessment: promising for one host or a modest fleet. It does not remove the need to operate hosts,
manage placement across them, and prove backup recovery. Do not replace the working Incus adapter
without a demonstrated benefit in image portability, lifecycle reliability, density or maintenance.

## CubeSandbox: a fuller self-hosted cluster

Reviewed upstream commit `6bf31c68a748f6ea05763df40eb7edaacba927a9` through a librarian checkout.

CubeSandbox has a cluster scheduler, node agents, a KVM runtime, E2B-compatible API, request routing,
and automatic pause/resume. Redis coordinates metadata and lifecycle events; the tested one-click deployment also runs MySQL.
Containerd, build tooling and networking services are additional dependencies. Its local snapshot engine uses XFS
reflink. These components explain both the stronger fleet feature set and the larger operations
burden. E2B compatibility does not prove compatibility with every operation Dorf currently uses.
[Architecture](https://cubesandbox.com/architecture/overview).

The lifecycle has no hosted E2B-style hard wall-clock ceiling. Idle expiration is configurable, but
manual pause alone does not protect a sandbox from a configured kill timeout. Retained assistants
need explicit non-destructive lifecycle configuration.
[Lifecycle](https://cubesandbox.com/guide/lifecycle).

Cross-node snapshotting is an optional S3 backend and is off by default. It packages rootfs, RAM and
metadata. The backend must be chosen when the template is built; local XFS snapshots cannot simply
be resumed on another host. Cross-node restore requires remote status `ready`, compatible CPU/kernel
identity, and suitable volume attachments. Raw host mounts pin the sandbox to its original node.
The release announcement labels the cross-node feature preview. This can supply a useful backup
mechanism, but does not eliminate deployment and recovery testing or establish R2 compatibility.
[Cross-node conditions](https://cubesandbox.com/guide/cross-node-snapshot),
[release description](https://github.com/TencentCloud/CubeSandbox/blob/6bf31c68a748f6ea05763df40eb7edaacba927a9/README.md).

A subsequent [ten-user live proof](cubesandbox-ten-user-proof.md) measured thirty running synthetic
environments at 15.4 GiB process memory, including a 2.1 GiB shared control stack. This demonstrates
why the fixed stack cost should be amortized rather than treated as a per-user burden. It also found
that five acknowledged paused entries could not resume after an abrupt host stop during concurrent
snapshot activity. The earlier clean single-sandbox reboot passed; crash recovery remains a blocker
for relying on this tested configuration for retained assistants.

Assessment: useful when cluster scheduling, dense placement, and cross-node recovery outweigh the
extra services. For an initial proof, use an isolated KVM host with suitable XFS storage; the
alternative PVM setup changes the host kernel and should not be introduced into the production
Dorf host as part of research.
[Deployment requirements](https://github.com/TencentCloud/CubeSandbox/blob/6bf31c68a748f6ea05763df40eb7edaacba927a9/docs/guide/quickstart.md).

## Economics and bounded next step

For self-hosting, compare total host, disk, network, backup and operations cost divided by the
supported user population at measured peak load. Retained users are not simultaneous running VMs.
Stopping a guest frees capacity for another guest but does not reduce a dedicated host's bill.
Published minimum VM overhead or startup benchmarks are not measured Codex/browser capacity.

Keep Incus as the existing self-hosted baseline. Evaluate Hypeman first if testing a new runtime;
retain CubeSandbox for the multi-host case. A disposable proof should run the actual Dorf image
capabilities, preserve the native conversation and files through stop/start, and restore a periodic
backup on replacement compute. Include home, workspace, scratch paths and installed-tool behavior.
Do not require per-turn backup, invent a transcript store, or build several provider adapters as
part of this comparison.

Hypeman, CubeSandbox and self-hosted smolvm were subsequently tested in disposable Incus VMs. See the
[versioned live proof](retained-runtime-incus-proof.md) for observed behavior, installation corrections
and remaining limits. Production reliability, fleet density and Dorf interoperability remain unmeasured.
