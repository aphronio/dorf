# Shared Provider Gateway

The Provider Gateway is a sibling application subsystem. It owns durable upstream Provider
AI connections and revocable consumer-specific Inference Routes; it does not own Job sequencing,
Sandbox lifecycle, transcripts, review, or repository policy.

The supported AI connection is either a ChatGPT subscription or one OpenAI API key through the pinned
broker. Codex consumes its scoped route through Responses WebSockets; Pi consumes the same scoped
route as an OpenAI Responses provider. `dorf setup` offers subscription device confirmation or a
masked API-key input. The standalone `provider connect` command accepts the same choices and reads
API keys only from a file or standard input. The protected host state retains the selected upstream
credential; a deployment currently admits one unprefixed OpenAI authentication mode at a time.
Executable code owns the exact broker version and artifact integrity. Each Sandbox receives only a
scoped route and its selected Harness configuration.

The Go Job path creates, observes, and revokes routes directly. AI connection data, broker
route configuration and keys, and the broker executable live under the configured
`DORF_PROVIDER_GATEWAY_STATE`; they never enter a Sandbox image. Dorf retains only the exact route
identity derivation and Action settlement state needed for reconciliation.
Route creation asks the live scoped Gateway whether it advertises the Job's opaque model and fails
before Harness submission when it cannot route it. Dorf retains no model catalog; the Harness and
upstream Provider remain authoritative for actual execution.

Remote Sandbox profiles supply one exact HTTPS `/v1` Gateway URL whose transport is owned by the
deployment, not by the Sandbox adapter. An existing operator-owned URL is the universal contract.
Guided setup asks for the intended hostname and discovers its nearest public DNS delegation. When
the hostname is unused and every authoritative nameserver is Cloudflare, it offers to reconcile one
named, outbound-only Cloudflare Tunnel; otherwise it keeps the universal existing-HTTPS-ingress
path and never replaces DNS it cannot prove is available. The broker remains bound to loopback on a cloud-only host or
to the selected private Incus bridge when local and cloud profiles coexist; it never opens a public
listener. The Tunnel exposes only `/v1`, retains one exact Tunnel credential and a Dorf-owned host
service, and receives no upstream Provider credential. Browser authorization creates a broad
Cloudflare account certificate only for Tunnel and DNS reconciliation; setup removes it after those
account-level mutations settle. The managed ingress also serves one random, nonsecret deployment
probe path directly from `cloudflared`; setup and status require that exact path to return HTTP 204
and separately require anonymous `/v1/models` access to return the Gateway's HTTP 401. This proves
the configured hostname reaches this Dorf-owned Tunnel rather than merely some protected service.
Operator-owned HTTPS ingress retains only the universal protected-API check because Dorf does not
own its routing configuration.

The E2B adapter defaults to restricting Sandbox egress to the configured Gateway hostname, and the
Gateway's revocable consumer key remains the request capability. A repository profile may
explicitly admit general internet egress when clean setup and agent work require changing package,
redirect, or documentation hosts; that broader policy is visible deployment configuration and does
not give the Sandbox an upstream credential. Disposable Quick Tunnels remain proof tooling only;
they have no stable hostname or uptime guarantee.

Setup selects one deployment-default AI connection. New Jobs use that default unless the caller
passes `--ai-connection`; either way, the admitted Job durably pins the resolved connection name.
`dorf provider status --profile NAME [--ai-connection CONNECTION]` is the observational deployment check. It
verifies the selected AI connection and private broker locally, then, for a remote profile,
requests the exact configured `/v1/models` path without a credential and requires the Gateway's HTTP
401 rejection. It never starts the broker, repairs a tunnel, or creates a consumer route. Profile
verification remains historical proof of the selected runtime artifact; status reports current
Gateway reachability separately and exits unsuccessfully when either authority is not ready. Use
`--json` for the same machine-readable facts.

## Security and recovery

- Upstream OAuth or API-key state stays in protected host storage.
- Route keys are broker-local capabilities, never upstream credentials.
- The broker binds to loopback or one exact private Incus bridge IPv4, never wildcard.
- The guided Cloudflare route forwards only the exact `/v1` API path and runs as
  `dorf-cloudflared.service`; its one nonsecret deployment-probe path terminates with HTTP 204 and
  every other public path terminates with HTTP 404.
- A remote route is admitted only as an exact HTTPS `/v1` URL; query credentials and userinfo are
  rejected. E2B egress is default-deny unless its selected profile explicitly admits internet access.
- Route creation and revocation use stable Action identities and authenticated management calls.
- Remote status probes send no Gateway credential and reject open or intermediary-blocked endpoints.
- Missing, ambiguous, stale, or non-WebSocket authentication fails before Sandbox mutation.
- Cleanup is incomplete until the exact Sandbox route is revoked or attention remains observable.
- Logs and CLI output never render upstream, management, guard, route, GitHub, or Harness control
  credentials.

Do not add provider pooling, fallback, quotas, an ingress registry, another broker, or a wire dialect
until a concrete validated consumer requires it. D036 remains the governing Gateway decision; D065
records Pi's reuse of the route. D047 changes the control plane from Python to Go, not this
connection/route authority model.
