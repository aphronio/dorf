# smol Cloud Capability Proof

Observed on 2026-08-14 through the hosted smol cloud REST API. The hosted runtime did not expose a
version in the tested API, so this report records the observation date rather than inferring one.
This is ecosystem research, not a supported Dorf profile or the D063 second-provider terminal. The
ranked candidate list remains the [Sandbox and VM Provider Watchlist](sandbox-vm-watchlist.md).

## Result

One disposable 4-vCPU, 4-GB Debian 13 machine and bounded network-policy controls established:

- Linux x86-64, root access, a dedicated 20-GB workspace disk, and `tail` rather than systemd as
  PID 1;
- deterministic caller names, duplicate-name `409` fencing, exact list/get adoption, and terminal
  deletion;
- file upload plus exec with stdin, separate stdout and stderr, exact nonzero exit status, and
  truncation indicators;
- a guest-local Docker daemon and successful Docker Compose workload without the host Docker
  socket;
- native headless Chromium rendering a nonce-bearing local page;
- a guest HTTP service reached through smol's private bearer-authenticated port bridge;
- stop/start preserving the workspace marker; and
- explicit `network.mode: "blocked"` preventing outbound resolution while the control plane
  remained usable.

Cold installation of Docker, Compose, Chromium, and supporting tools took about 130 seconds. A real
profile should use a pinned prepared OCI or `.smolmachine` artifact instead of repeating that setup.
All disposable machines were deleted, and a final tenant inventory found no Dorf proof machine.
Detailed machine-readable evidence remains only under the ignored local `.dorf/smol-cloud-spike/`
path. No API key, Provider Route, upstream credential, machine ID, request ID, or ownership nonce is
retained here.

## Important findings

- Omitting `network` allowed outbound access in the live service, contrary to the current
  [Cloud API](https://smolmachines.com/docs/cloud-api) statement that the omitted default is
  blocked. Explicit blocked mode worked. A Dorf adapter must always send and later attest an
  explicit policy; it must never depend on a provider default.
- Native Chromium execution passed, but external HTTPS navigation did not. One bounded attempt
  returned repeated connection resets, so this proof does not claim general browser egress.
- PID 1 was `tail`, not systemd. The environment has a real Linux kernel and was capable of nested
  Docker, but it is not equivalent to Dorf's conventional Incus/systemd workstation profile.
- The authenticated HTTP and WebSocket port bridge is promising for Harness control traffic. It
  does not by itself solve the opposite-direction path from a remote Sandbox to Dorf's owner-hosted
  Provider Gateway.

## What remains unproved

- A prepared combined Codex/Pi Harness image and one real Dorf coding Job.
- Lost-create-response and lost-exec-response fault injection rather than ordinary exact-name
  adoption.
- External browser navigation, long-running process recovery, snapshot/fork behavior, sustained
  cost, regional behavior, and service reliability.
- A secure remote route from the Sandbox to Dorf's Provider Gateway followed by exact revocation.
- The integration form: official SDK process, direct/generated Go REST client, or a future official
  Go SDK.

Do not call smol cloud a supported provider until the same coding-to-publication terminal used for
Incus preserves Dorf-owned Action settlement, AgentRun recovery, Provider Route custody, evidence,
outcome, and confirmed cleanup.
