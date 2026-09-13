# Retained runtime tests in disposable Incus VMs

Observed on 2026-09-13. Non-normative experiments supporting the
[retained agent comparison](retained-agent-provider-comparison.md), not new supported Dorf profiles.

## Scope

Hypeman, CubeSandbox and smolvm ran inside separate disposable Debian 13 Incus VMs on a Linux AMD64
host with nested KVM enabled. Each runtime created its own nested guests. No production resources
or credentials were used. Software installation and storage preparation stayed inside the test VMs.

The tests target the accepted baseline: useful files survive idle periods and normal recovery,
with no mandatory short lifetime for retained environments. Memory preservation is optional.
Local snapshots and same-host recovery do not establish off-host backup recovery.

## Hypeman

Installed API v0.3.0, CLI v0.18.0, and Cloud Hypervisor v51.1 using the official installer.
The API binary SHA256 was `6f0dff19c0b7e784a6efab120f3524561c01e92e3daf2eac67044644bd8f3a61`.
The outer VM had 4 vCPU, 5 GiB RAM and a 40 GiB root disk. The main nested guest used Debian 13
slim, 1 vCPU, 512 MB RAM and a 2 GB writable overlay.

| Check | Observed result |
| --- | --- |
| Native create and exec | Passed shell and Python execution inside a Cloud Hypervisor guest. |
| Cold stop/start | Home, workspace and `/tmp` markers survived; boot identity changed. Provider identity and ownership tag survived; guest IP changed. |
| SQLite persistence | Committed row survived subsequent cold start; integrity check returned `ok`. Database was closed cleanly. |
| Disk snapshot recovery | Native stopped snapshot forked into a new running guest with the markers and SQLite row intact. |
| Memory standby/restore | Boot identity and a `/dev/shm` token survived. |
| Service restart | Inventory, ownership tag and configured snapshot schedule survived; an already running guest remained accessible with the same boot identity. |
| Snapshot schedule | A 24-hour interval with seven retained snapshots was configured and survived service restart. Scheduled execution was not observed. |
| Outbound network | Public TCP worked. Supported DNS configuration restored resolution and HTTPS, including after cold start. |

The installer reported success before the service was ready. Minimal Debian required two additional
packages: `iptables` for API startup and `erofs-utils` for OCI conversion. The default public DNS
resolver failed in this network. Setting the documented server `network.dns_server` to the outer
VM's working resolver fixed it persistently; editing guest `resolv.conf` alone did not survive boot.

A stopped-snapshot fork reached Running in 1.425 seconds; standby and restore took 0.606 and 0.500
seconds. Start returned before guest exec was ready, so an adapter must wait for readiness. These
are individual nested-development observations, not production benchmarks.

With all nested guests stopped, the outer VM reported 482 MiB used memory; API RSS was about
146 MiB. Runtime data occupied 1.3 GB including image cache, three stopped guests and a snapshot.
These measurements do not establish active-agent density.

## CubeSandbox

Installed the published v0.7.1 bundle with SDK 0.7.0. Bundle SHA256 was
`a516ed71e90e273d03f053a26bbbf3932e0d747387fbccd25ec76f89b20bc0a1`.
The outer VM had 4 vCPU, 8 GiB RAM and a 90 GiB root disk. A guest-only XFS reflink loop filesystem
provided `/data/cubelet`; bpffs was mounted inside the outer VM. Native nested KVM worked without
installing a PVM kernel. The official base-image template used 1 vCPU and 768 MB RAM.

| Check | Observed result |
| --- | --- |
| Native create and exec | Official base-image template became ready; SDK created a sandbox and executed commands. |
| Pause/resume | Home, workspace and `/tmp` marker hashes matched after a ten-second pause. |
| Retained lifecycle | Explicit `timeout=-1` produced no end time. |
| Expiring lifecycle | A separate sandbox with a ten-second kill timeout was absent after the observation wait; command and info calls could no longer find it. |
| Outer VM reboot | A paused sandbox resumed after the outer VM's boot identity changed; all three marker hashes still matched. No failed service units remained after reboot. |
| Temporary path | `/tmp` used the persistent overlay, not tmpfs, in this tested image. |

The installed stack included MySQL, Redis, MinIO, DNS, proxy, lifecycle, egress and web UI services
alongside CubeMaster, Cubelet, CubeAPI, CubeOps and CubeTemplateCenter.
Initial startup took roughly 3.5 minutes including fresh dependency downloads. A DNS dependency's
first pull exceeded a readiness check, leaving the host-DNS service inactive; explicitly starting
that service inside the test VM made the installer checks pass. Debian's packaged Compose name
also differed from the initial prerequisite command.

Single observations were 0.162 seconds for create, 0.123 seconds for pause and 0.130 seconds for
resume. After outer reboot, the API was ready in about 20 seconds and native sandbox resume took
0.361 seconds. These are nested-development diagnostics, not a comparison benchmark.

The reboot test demonstrates recovery of a paused sandbox from retained host storage. It is not
a cold reboot of the nested guest, an unplanned host-crash test, or cross-host/S3 recovery.
SQLite was not tested in CubeSandbox because the base image lacked the required tooling. Native
Codex history was also not tested; this bounded experiment used newly generated file markers.

With no nested sandboxes running, the outer VM reported 2,787 MiB used memory. Its root filesystem
used 6.5 GiB including installers and dependencies; the XFS mount reported 1.7 GiB used. This was
the full one-click deployment, not a tuned minimal worker node or a cluster-density benchmark.

A subsequent [ten-user resource test](cubesandbox-ten-user-proof.md) ran thirty synthetic environments
successfully but found unusable paused entries after an abrupt host stop during concurrent pausing.
That later finding qualifies the earlier clean-reboot result above.

## smolvm self-hosted

Retested [v1.15.0](https://github.com/smol-machines/smolvm/releases/tag/v1.15.0), selected by the
live GitHub release API. The release archive matched the published checksum:
`65106527c0cf97e7924d86e968e402841a61aa5b867b6bc7e0fd30be1d0bd0b2`.
This supersedes the version-specific status, but does not rewrite the
[archived v1.8.0 proof](smol-local-capability-proof.md). The outer Debian VM had 4 vCPU, 5 GiB RAM
and a 50 GiB root disk. The nested bare agent guest had 1 vCPU and 1,024 MiB RAM. The REST service
kept its default seccomp and Landlock enforcement. Its test systemd unit used `KillMode=process`
so restarting the API process preserved runtime-owned guest scopes. This was not an OCI application
image test.

| Check | Observed result |
| --- | --- |
| Native REST lifecycle | Create, start, exec and stop worked. |
| Cold stop/start | Home, workspace and `/tmp` markers survived; boot identity changed. |
| Published ingress | An uploaded static HTTP helper was reachable through a published port. |
| Service restart | The service re-adopted the same running guest process and its HTTP endpoint remained usable. |
| Public networking | Direct public TCP succeeded, but default guest DNS timed out and HTTPS by hostname failed. A diagnostic guest resolver override also failed under retained hardening. |
| Portable live checkpoint | REST capture returned HTTP 500: libkrun could not create checkpoint staging state because permission was denied. No restore was possible. |
| Stopped machine export | `pack create --from-vm … --include-workspace` failed during overlay flattening; its helper VM exited before readiness. No restore was possible. |

A start call took about 1.94 seconds. With the nested guest stopped, the outer VM reported 380 MiB
used memory and the service RSS was about 22 MiB. Installed runtime files occupied about 150 MB,
with about 18 MB of machine data. This bare-guest observation is not directly comparable to the
OCI image caches and services included in the other runtime observations.

The failed checkpoint staging path was already under `/var/lib/smolvm`, rather than a root-home
cache. Export diagnostics showed a helper running under its own VM UID and a virtio-block
configuration failure. The experiment did not establish a supported configuration fix or exact
upstream defect, and did not disable isolation to force a pass.

Two source-level backup constraints also matter: stopped packs omit `/workspace` unless explicitly
included, and the portable live-checkpoint profile rejects custom DNS and several host-bound
attachments. Live restore checks runtime and CPU compatibility. These are documented/source
capabilities, not successful recovery results from this test.
[Pack options](https://github.com/smol-machines/smolvm/blob/a311ec2be7d2fc18ee1606476f675e0294f20975/src/cli/pack.rs),
[checkpoint constraints](https://github.com/smol-machines/smolvm/blob/a311ec2be7d2fc18ee1606476f675e0294f20975/src/portable_checkpoint.rs).

SQLite, native Codex history, OCI application execution, Docker and browser workloads were not
established in this trial. Self-hosted smolvm remains a lightweight candidate, but its DNS and native
export failures prevent promoting it over Hypeman for this tested deployment.

## Assessment and cleanup

All three runtimes passed the ordinary filesystem persistence operations exercised here. Hypeman was the smaller
stack to bring up and additionally demonstrated an independent disk-snapshot fork. CubeSandbox
recovered a paused sandbox after its host rebooted, with more supporting services to operate.
This supports keeping Hypeman first among new self-hosted runtime candidates; it does not yet
justify replacing the existing Incus integration. A provider choice still needs a representative
Dorf workload and off-host backup restoration.

The task-owned outer VMs were deleted after evidence collection. A final Incus inventory query
returned no matching test instances. Their nested workloads, generated credentials and guest-only
storage were removed with them. No production instance or host kernel/service was changed.

## Limits and evidence

No Dorf adapter, actual model turn, native Codex conversation recovery, browser/CDP, Docker-in-guest,
port routing, tenant isolation audit, overcommit benchmark, long-duration workload, or off-host
backup restoration was tested. Synthetic files and a committed SQLite row exercise persistence
without transferring a real assistant session. RAM-backed file recovery does not prove arbitrary
application connection recovery. A clean database check does not prove recovery of an interrupted
transaction.

Detailed local evidence is under `/tmp/dorf-oss-runtime-test-20260913/`; this document retains the
sanitized findings because temporary artifacts may disappear.
