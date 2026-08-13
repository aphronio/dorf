# smolvm Local Capability Proof

Observed on 2026-08-14 with
[smolvm v1.8.0](https://github.com/smol-machines/smolvm/releases/tag/v1.8.0) on Linux x86-64 with
host KVM access. The tested release archive was
`c578655a8e57ff142699db614fea2470296101d293a732654ad6ed27158f38fb`, matching the upstream
`smolvm-1.8.0-linux-x86_64.tar.gz` checksum. This is ecosystem research, not a supported Dorf
profile.

## Result

The DNS-independent local control-plane proof passed:

- a loopback REST service created and controlled bare hardware microVMs;
- a guest HTTP service was reachable through an explicit host-to-guest port mapping;
- stopping and restarting the REST service re-adopted the still-running exact machine and preserved
  its published endpoint;
- stopping and starting the VM preserved its uploaded executable;
- omitted networking defaulted to blocked;
- explicit `network: false` blocked outbound traffic;
- `network: true` with an empty allowed-CIDR list also blocked outbound traffic; and
- all four control VMs, their exact VMM PIDs, the public inventory, and the isolated state root were
  confirmed absent at cleanup.

A separate workstation attempt also established deterministic names, duplicate-name fencing,
exact REST adoption, root access, `init` as PID 1, REST file upload, binary-safe base64 exec output,
stdin, separate stdout and stderr, and exact exit status. v1.8.0 CLI ownership labels were visible in
JSON inventory. Labels were not present in the tested REST create schema.

Detailed machine-readable evidence remains only under the ignored local `.dorf/smol-local-spike/`
path. No credential, machine ID, nonce, PID, local address, username, or host topology is retained
here.

## Workstation blocker

The intended agent-friendly REST topology could not complete the Docker Compose and Chromium
workload on this host because guest DNS did not converge:

1. A REST-created persistent virtio-net machine timed out against smol's guest DNS proxy while
   resolving its OCI registry.
2. The v1.8.0 REST create schema exposed no DNS-resolver field even though the CLI exposed `--dns`.
3. A CLI-created persistent machine retained enough configuration to boot through REST, but a
   loopback resolver was interpreted inside the guest and package repository resolution failed.
4. Direct ephemeral CLI/TSI execution succeeded through an existing non-loopback host resolver.
   Starting an equivalent persistent machine through the hardened REST service denied access to
   that private resolver.

This is not cloud parity. The hosted service completed Docker Compose, native Chromium rendering,
and private ingress on one VM. Local v1.8.0 demonstrated the underlying VM, networking, endpoint,
ownership, and REST primitives, but the supported hardened service could not combine them into one
fresh general-purpose workstation on this host.

## Recommendation

Keep smolvm as a high-priority later local profile, not the first second-provider slice. Before a
Dorf terminal, ask upstream for one supported hardened-service recipe that provides:

- a persisted DNS resolver through REST;
- working outbound DNS alongside published virtio-net ports;
- an operator-approved resolver path that does not disable service hardening; and
- ownership labels on the same REST create/list/get surface used for reconciliation.

After that capability lands, rerun this exact versioned proof with a prepared combined Harness
artifact, Docker Compose, external browser navigation, controller loss, and confirmed deletion.
The recorded v1.8.0 version and checksum make a later comparison explicit rather than silently
benchmarking a different release.
