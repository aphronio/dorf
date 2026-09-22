# Provider Gateway authority

This document owns the Provider Gateway's stable responsibility and security boundaries.
[Getting started](../getting-started.md) owns setup procedures. [Support](../support.md) owns current
support and diagnostics. Code owns exact broker versions, filesystem paths, configuration fields,
and protocol behavior.

## Ownership

The Provider Gateway is a sibling application subsystem. It owns upstream AI Connections and
revocable consumer-specific Inference Routes. It does not own Session sequencing, Sandbox lifecycle,
agent transcripts, review, or repository policy.

An AI Connection retains one upstream authentication method and its default Harness model in
protected host storage. Dorf verifies a candidate before it becomes the deployment default. A failed
candidate does not replace the last verified default.

An Inference Route grants one consumer access through an opaque model name and a revocable key. A
Sandbox receives only that route and its Harness configuration. It never receives an upstream
credential or Gateway management authority.

The Dorf Session path creates, observes, and revokes Routes through stable Actions. Dorf retains the
Route identity and settlement facts required for reconciliation. The live Gateway remains the
authority for whether it can route an advertised model.

Codex route installation owns separate guest route settings and credential files, not the client's
native configuration file. The app-server launcher passes the route settings through native
configuration overrides; Dorf's provider selection takes precedence for that process while unrelated
client settings remain native. Install, reinstall, and removal never rewrite or delete client
configuration. Invalid native configuration remains a native readiness failure, not permission to
replace it. Remote revocation precedes local route-file removal.

Updated route settings and credentials take effect at the next app-server launch, not by modifying
an already-running process. Reconnection retains that process's existing configuration. Older
workspaces without separate route settings continue to launch using their retained native provider
configuration until the next route installation. Legacy configuration is left untouched, including
on removal; retained provider settings do not grant access after the scoped credential is revoked.

At app-server launch, Codex also receives `OPENAI_BASE_URL` and `OPENAI_API_KEY` for SDK-based
tools, derived from the same route options and scoped credential. The API-key variable contains a
revocable Gateway credential, not an upstream OpenAI key. Native shell-environment restrictions
still apply. This supports existing API clients, including Codex's bundled image-generation script,
when the selected connection provides the requested endpoint/model. It does not enable Codex's
separately gated native image tool, alter bundled skills, or authorize additional spending. Older
workspaces without route options gain these settings after route installation and app-server restart.

## Network boundary

Every Sandbox Profile names one exact guest-reachable Gateway URL. Profile verification proves that
the selected provider, route, and Harness work together. Provider-specific network requirements
belong in [Support](../support.md), and their setup belongs in [Getting started](../getting-started.md).

Controller-to-Sandbox traffic and Sandbox-to-Gateway traffic are separate paths. A provider adapter
may implement the first path. It must not infer or silently create the second.

The Control API and the Provider Gateway are separate authenticated origins. The checked-in
[`deploy/compose.yaml`](../../deploy/compose.yaml) owns their exact process and network topology.
Getting started owns the current guided and operator-managed ingress procedures.

## Selection and observation

Setup selects one default AI Connection. A Session may select another named connection when the public
contract permits it. Admission resolves and retains both the exact connection and the exact model,
so later default changes cannot reinterpret an existing Session.

New OpenAI and ChatGPT connections use the recommended model defined in
[`internal/gateway/gateway.go`](../../internal/gateway/gateway.go). Updating that recommendation
also applies to connections without a saved model; it does not replace a saved connection model.
An explicit Session model overrides the connection default. Verify the selected model through the live
Gateway before relying on it, since upstream access can differ by connection.

[Support](../support.md) owns the current readiness command and its interpretation. Observation must
not start the broker, repair ingress, infer a private route, or create an Inference Route.

## Security and recovery

Codex owns bounded inference retries and retains completed tool results during recovery. The
Gateway keeps a connection eligible after transient upstream failures so those retries can reach
upstream within the same native Turn. Authentication and quota failures retain their
Gateway cooldowns.

- Upstream authentication and Gateway management state stay in protected host storage.
- Dorf attests the broker executable and its launch inputs. The running broker owns its mutable
  active configuration.
- Route keys are consumer capabilities, never upstream credentials.
- A remote Profile admits one exact protected Gateway URL, not a wildcard listener.
- Route creation and revocation use stable Action identities and authenticated management calls.
- Remote readiness probes send no Gateway credential and reject an open endpoint.
- Missing, ambiguous, stale, or unsupported authentication fails before Sandbox mutation.
- Cleanup remains incomplete until the consumer's exact Route is revoked or the failure remains
  visible as attention.
- Logs and CLI output never render upstream, management, Route, GitHub, or Harness control
  credentials.

Do not add connection pooling, fallback, quotas, an ingress registry, another broker, or another
wire protocol until a verified consumer requires it. The [Decision Log](decisions.md) preserves the
rationale for accepted implementations; this document remains the current subsystem authority.
