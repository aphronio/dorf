# Persistent Buzz Deployment

Buzz runs as the single developer's main, long-lived deployment in a clean Incus VM. It is used
directly throughout iterative development rather than promoted through separate staging and
production environments. It is not a Dorf Room: Dorf does not assign Jobs to it, clone
managed repositories into it, or give an agent control of its lifecycle.

The default capacity replaces the paused `hermes-vm` envelope:

- VM: `dorf-buzz`
- base: `images:ubuntu/24.04`
- limit: 4 vCPUs and 8 GiB memory
- root disk: 40 GiB
- autostart: enabled
- Buzz image: `ghcr.io/block/buzz:sha-2ce2d71`
- Buzz source: `block/buzz` commit `2ce2d71cc38a9657eaf344c10e07f155b8a18615`

Docker runs inside the VM. By default, the relay binds only to VM loopback. A narrowly scoped Incus
NAT proxy can bind the existing Omarchy Tailscale address and forward one port to the relay. While
that mapping is enabled, Docker publishes the relay on the VM NIC so Incus can reach it. The VM is
not a Tailscale node, and no host Docker socket or public ingress is exposed.

## Provision and validate the local relay

```bash
BUZZ_OWNER_PUBKEY=npub1... scripts/incus/provision-buzz.sh
```

For the first run, create and back up the human identity in Buzz Desktop before provisioning, then
copy the public `npub` shown by the client into `BUZZ_OWNER_PUBKEY`. A 64-character public hex key is
also accepted. The script validates and normalizes that public input before creating a new VM.

The script is retryable. After the first successful run, `BUZZ_OWNER_PUBKEY` may be omitted: the
script retains and validates the configured owner rather than replacing it. Supplying a different
owner on a rerun fails safely. The script creates or starts the VM, installs Docker, checks out the
pinned Buzz revision, generates the relay's stable deployment secrets on first run, starts the
upstream production Compose bundle, and checks `/_liveness`.

The human owner identity follows D033: create it in the first-party desktop client, retain its
private `nsec` only under human control, and supply only its public key to the relay as
`RELAY_OWNER_PUBKEY`. The relay's service-signing key and other generated deployment secrets remain
inside the VM. Back up the human key separately and take coherent backups of the Compose `.env`,
Postgres, MinIO, and git volumes before depending on the relay for durable history.

Provisioning never generates, requests, displays, writes, or transfers the human private key. A
missing or invalid public identity stops a fresh setup before it creates the VM. If a pre-existing
VM has not yet created its Compose `.env`, the same check stops before the guest provisioning script
is installed or run.

An intentional owner recovery is deliberately not an ordinary provisioning rerun. First create and
back up a replacement identity in Buzz Desktop and take a coherent Buzz backup. Open a break-glass
shell with `incus exec dorf-buzz -- bash`, back up
`/opt/dorf-buzz/source/deploy/compose/.env`, replace only `RELAY_OWNER_PUBKEY` with the
replacement identity's 64-character public hex value, and run `./run.sh restart` from that Compose
directory. Verify the replacement owner from Buzz Desktop before retiring the old identity. The
normalizer can produce the required public hex without private material:

```bash
scripts/incus/normalize-nostr-public-key.py npub1...
```

## Expose through the existing host tailnet

```bash
scripts/incus/expose-buzz.sh enable
```

This does not change the Tailscale configuration. It discovers the host's existing Tailscale IPv4
address, adds one persistent Incus NAT proxy device from that address on TCP port 3000 to the VM's
port 3000, pins the VM's current Incus address as its DHCP reservation, advertises
`http://omarchy:3000` and `ws://omarchy:3000` from Buzz, restarts the relay, and checks liveness
through the mapping.

Devices already on the same tailnet can then open:

```text
http://omarchy:3000
```

Use `ws://omarchy:3000` as the relay URL for clients that allow plaintext transport over the
encrypted tailnet. Use the hostname rather than the `100.x` address because Buzz binds community
state to the configured request host.

The pinned production mobile source explicitly requires WSS for every relay except debug-mode
localhost and rejects non-public IP literals. Therefore this mapping proves private network
reachability and supports relay/desktop/CLI validation, but it is not itself the phone endpoint.
Providing trusted WSS on the existing host is a separate client-compatibility slice; it is not
needed to make the underlying Tailscale transport private.

Inspect or roll back the complete exposure:

```bash
scripts/incus/expose-buzz.sh status
scripts/incus/expose-buzz.sh disable
```

`disable` removes only the `buzz-tailnet` Incus proxy device and its VM address reservation,
restores Buzz's loopback binding and advertised URLs, and restarts the relay.

## Current dogfood evidence

The first real client path was validated on 2026-07-28 with Buzz Desktop `v0.4.26` on a separate
MacBook already joined to the same tailnet. The client was subsequently upgraded to `v0.5.0`
without changing the relay path or identity:

```text
Buzz Desktop on Mac
  -> ws://omarchy:3000
  -> Tailscale to 100.126.177.27:3000
  -> Incus NAT proxy buzz-tailnet
  -> 10.125.18.211:3000 in dorf-buzz
  -> Buzz relay and PostgreSQL
```

The desktop generated the human identity locally. Its public identity
`npub1czt79ctlfgmrt6nav52wlcyfz5kh3ly6m4euqhnpqmtvptfrlekq6pr82j` is the sole relay
member with role `owner`; no copy of its private key is present in the VM. The temporary bootstrap
identity was removed only after this owner authenticated successfully, and its key files are absent.

The owner sent `hello from macbook` in the private Welcome channel. The relay accepted and persisted
the signed event with ID
`28cd01fd1df0bce8dacef64d717276d8682003ae3573032b2db8c115c307a083`, proving the
actual desktop-to-database path rather than only network liveness.

Buzz onboarding also created the built-in Fizz, Honey, and Bumble persona definitions. No AI
provider is connected and those agents are intentionally inactive. Dorf does not use that
ACP-managed path.

Normal Buzz channel messages, including access-controlled private-channel messages, are plaintext
to the relay database. Tailscale protects this deployment's network path, and Buzz membership
protects ordinary client reads, but an administrator of Omarchy, Incus, the VM, PostgreSQL, or its
backups can read stored conversation content. Treat the host and its backups as trusted.

## Integration boundary

The direct Buzz-to-Dorf coordinator was a disposable tracer bullet. It confirmed that channel
identity, relay messaging, replay handling, and assistant behavior belong outside Dorf's portable
runtime. Dorf exposes typed Worker and Job operations; a separate application may compose them at a
channel edge without giving individual Workers channel identities by default.

There is no supported Dorf `buzz` command or coordinator listener. Buzz changes independently and
is not a public Dorf integration contract. Inspect the current upstream project before implementing
or operating a new integration.

## Operate and recover

```bash
incus stop dorf-buzz
incus start dorf-buzz
incus exec dorf-buzz --cwd /opt/dorf-buzz/source/deploy/compose -- ./run.sh status
incus exec dorf-buzz --cwd /opt/dorf-buzz/source/deploy/compose -- ./run.sh logs relay
```

Container restart policies restore the Buzz stack when Docker starts, while Incus persists the
host port mapping. Re-run either repo script to reconcile its respective layer.

This is the main deployment, so upgrade it in place only through a small demonstrable step: take a
coherent backup, change the pinned revision, run the provisioning command, and validate relay
health plus the real desktop client before continuing. Add a separate environment only when a
concrete migration, additional user, or recovery risk makes it cheaper than restoring this one.

Do not delete `hermes-vm` as part of this setup. It remains stopped with `boot.autostart=false` and
retains its disk and snapshots for reversible recovery.
