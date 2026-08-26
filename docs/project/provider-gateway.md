# Shared Provider Gateway

The Provider Gateway is a sibling application subsystem. It owns durable upstream Provider
AI connections and revocable consumer-specific Inference Routes; it does not own Job sequencing,
Sandbox lifecycle, transcripts, review, or repository policy.

The supported AI connection is either a ChatGPT subscription or one OpenAI API key through the pinned
broker. Codex consumes its scoped route through Responses WebSockets; Pi consumes the same scoped
route as an OpenAI Responses provider. `dorf setup` offers subscription device confirmation or a
masked API-key input. The standalone `provider connect` command accepts the same choices and reads
API keys only from a file or standard input. The protected host state retains authenticated
connection candidates and the selected default; a deployment currently admits one unprefixed
OpenAI authentication mode at a time.
`provider connect` first prepares that retained candidate and publishes its protected environment
and profile facts for the shipped static Compose project. When the running project does not yet
reflect those facts, it stops at the invoking operator's Compose handoff. Rerunning the command
after that handoff verifies the candidate through the live Gateway; only complete success selects it
as the deployment default. Publication or verification failure preserves the previous healthy
default. The command neither runs Compose nor treats prepared state as runtime readiness.

Executable code owns the exact broker version and artifact integrity. Each Sandbox receives only a
scoped route and its selected Harness configuration.

The Go Job path creates, observes, and revokes routes directly. AI connection data, broker route
configuration and keys, and the broker executable live under Dorf's resolved XDG host data layout
at `dorf/provider-gateway`. The overall XDG data root remains configurable through its standard
authority; the Gateway-specific `DORF_PROVIDER_GATEWAY_STATE` name is container-only and ignored as
an ambient host relocation.
Compose bind-mounts the resolved host source at the fixed container path
`/var/lib/dorf/.local/share/dorf/provider-gateway` (and its `cloudflare` child); neither enters a
Sandbox image. Dorf retains only the exact route identity derivation and Action settlement state
needed for reconciliation.
Route creation asks the live scoped Gateway whether it advertises the Job's opaque model and fails
before Harness submission when it cannot route it. Dorf retains no model catalog; the Harness and
upstream Provider remain authoritative for actual execution.
An unadvertised model remains visible as exact Action-sourced Job attention while the durable task
retries. Inspection names the model and offers repair or cleanup; a later successful route check
clears only that Action's attention.

Every Sandbox Profile owns one exact guest-reachable `/v1` Gateway URL. E2B requires HTTPS. An Incus
Profile may use an operator-routed private or VPN address or public HTTPS. Guided local setup has one
narrow convenience: through the configured local Unix endpoint, it may resolve one unambiguous
prepared bridge IPv4 once, publish the Gateway there, and persist the exact
`http://IP:8317/v1` value in the new Profile. Multiple candidate networks stop setup. Admission and
runtime consume that persisted value and never resolve or infer a bridge route again.

The Incus adapter reaches guest app servers through its Deployment's one configured local Unix or
remote HTTPS endpoint; that controller path does not create the separate guest-to-Gateway path. A
remote endpoint never uses guided bridge inference and remains unsupported until the complete
endpoint, port-forward, and explicit guest-route terminal passes.

An existing operator-owned HTTPS URL is the universal remote contract. Guided setup asks for the
intended hostname and discovers its nearest public DNS delegation. When the hostname is unused and
every authoritative nameserver is Cloudflare, it offers to reconcile one named, outbound-only
Cloudflare Tunnel. A hostname with existing address records is reusable only when it resolves
exactly to Dorf's retained Tunnel hostname; otherwise setup keeps the existing-HTTPS-ingress path
and never replaces DNS it cannot prove is available. Browser authorization creates a broad
Cloudflare account certificate only
for Tunnel and DNS reconciliation; setup removes it after those account-level mutations settle.
The Gateway and configured `cloudflared` process are foreground siblings in Dorf's static Compose
project; there is no host Cloudflare service. The Tunnel receives no upstream Provider credential
and exposes only `/v1` plus one random nonsecret deployment probe. Setup and status require that
probe to return
HTTP 204 and separately require anonymous `/v1/models` access to return the Gateway's HTTP 401. This
proves the configured hostname reaches this Dorf-owned Tunnel rather than merely some protected
service. Operator-owned HTTPS ingress retains only the universal protected-API check because Dorf
does not own its routing configuration.

The E2B adapter defaults to restricting Sandbox egress to the configured Gateway hostname, and the
Gateway's revocable consumer key remains the request capability. A repository profile may
explicitly admit general internet egress when clean setup and agent work require changing package,
redirect, or documentation hosts; that broader policy is visible deployment configuration and does
not give the Sandbox an upstream credential. Disposable Quick Tunnels remain proof tooling only;
they have no stable hostname or uptime guarantee.

Setup selects one deployment-default AI connection. New Jobs use that default unless the caller
passes `--ai-connection`; either way, the admitted Job durably pins the resolved connection name.
`dorf provider status --profile NAME [--ai-connection CONNECTION]` is the observational deployment check. It
verifies the selected AI connection and private broker locally, then requests the Profile's exact
configured `/v1/models` path without a credential and requires the Gateway's HTTP 401 rejection. For
the guided direct Incus route it first requires the publish address retained in protected `.env` to
equal the Profile URL; for HTTPS it also attests Dorf-owned Tunnel identity when applicable. It
never starts the broker, repairs ingress, resolves an Incus bridge, or creates a consumer route.
Profile verification remains historical artifact proof; status reports current Gateway reachability
separately and exits unsuccessfully when either authority is not ready. Use `--json` for the same
machine-readable facts.

## Security and recovery

- Upstream OAuth or API-key state stays in protected host storage.
- Route keys are broker-local capabilities, never upstream credentials.
- Compose keeps the broker private; any guest-reachable route is an explicit verified Profile field,
  never a wildcard or inferred public listener.
- The guided Cloudflare route forwards only the exact `/v1` API path; its one nonsecret
  deployment-probe path terminates with HTTP 204 and every other public path terminates with HTTP
  404.
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
records Pi's reuse of the route, D070 owns Profile fields, and D101 owns deployment supervision.
D047 changes the control plane from Python to Go, not this connection/route authority model.
