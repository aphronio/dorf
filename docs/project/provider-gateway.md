# Shared Provider Gateway

The Provider Gateway is a sibling application subsystem. It owns durable upstream Provider
Connections and revocable consumer-specific Inference Routes; it does not own Job sequencing,
Sandbox lifecycle, transcripts, review, or repository policy.

The supported Connection is a ChatGPT subscription through the pinned broker. Codex consumes its
scoped route through Responses WebSockets; Pi consumes the same scoped route as an OpenAI Responses
provider. `dorf provider connect chatgpt` installs the checksum-verified broker artifact, binds it to
the selected private Incus bridge rather than wildcard/LAN, completes one device login, and records
protected connection metadata. Executable code owns the exact broker version and artifact integrity.
The broker is the sole upstream credential and refresh writer. Each Sandbox receives only a scoped
route and its selected Harness configuration.

The Go Job path creates, observes, and revokes routes directly. Provider connection data, broker
route configuration and keys, and the broker executable live under the configured
`DORF_PROVIDER_GATEWAY_STATE`; they never enter a Sandbox image. Dorf retains only the exact route
identity derivation and Action settlement state needed for reconciliation.

Remote Sandbox profiles supply one exact HTTPS `/v1` Gateway URL whose transport is owned by the
deployment, not by the Sandbox adapter. The broker remains bound to its private address behind an
outbound tunnel; Dorf neither opens a public listener nor gives the tunnel upstream credentials.
The E2B adapter defaults to restricting Sandbox egress to the configured Gateway hostname, and the
Gateway's revocable consumer key remains the request capability. A repository profile may
explicitly admit general internet egress when clean setup and agent work require changing package,
redirect, or documentation hosts; that broader policy is visible deployment configuration and does
not give the Sandbox an upstream credential. The E2B wire and no-change Job proofs used a disposable
TryCloudflare Quick Tunnel only to validate this path. Quick Tunnels are not a supported deployment:
they have no stable hostname or uptime guarantee, and durable tunnel/domain selection remains a
deployment decision.

`dorf provider status --profile NAME [--name CONNECTION]` is the observational deployment check. It
verifies the named Provider Connection and private broker locally, then, for a remote profile,
requests the exact configured `/v1/models` path without a credential and requires the Gateway's HTTP
401 rejection. It never starts the broker, repairs a tunnel, or creates a consumer route. Profile
verification remains historical proof of the selected runtime artifact; status reports current
Gateway reachability separately and exits unsuccessfully when either authority is not ready. Use
`--json` for the same machine-readable facts.

## Security and recovery

- Upstream OAuth state stays in the host broker's protected `auth` directory.
- Route keys are broker-local capabilities, never upstream credentials.
- The broker binds to loopback or one exact private Incus bridge IPv4, never wildcard.
- A remote route is admitted only as an exact HTTPS `/v1` URL; query credentials and userinfo are
  rejected. E2B egress is default-deny unless its selected profile explicitly admits internet access.
- Route creation and revocation use stable Action identities and authenticated management calls.
- Remote status probes send no Gateway credential and reject open or intermediary-blocked endpoints.
- Missing, ambiguous, stale, or non-WebSocket authentication fails before Sandbox mutation.
- Cleanup is incomplete until the exact Sandbox route is revoked or attention remains observable.
- Logs and CLI output never render upstream, management, guard, route, GitHub, or Harness control
  credentials.

Do not add provider pooling, fallback, quotas, a registry, another broker, or a wire dialect until a
concrete validated consumer requires it. D036 remains the governing Gateway decision; D065 records
Pi's reuse of the route. D047 changes the control plane from Python to Go, not this connection/route
authority model.
