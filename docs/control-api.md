# Remote Control API

Dorf exposes one deliberately narrow HTTPS boundary for operating one configured Deployment. The
machine-readable authority is the embedded OpenAPI 3.1 document served by that Deployment at
`GET /v1/openapi.json`; discovery at `GET /v1` links to it and advertises supported capabilities.

This is a projection of Dorf's existing Job custody, not a network serialization of Core. A Job is
the long-running resource. PostgreSQL rows, Absurd tasks, AgentRuns, Threads, Turns, Actions,
providers, Harnesses, Profiles, and integration credentials are not public resources.

## Client and authentication boundary

One Deployment currently has one `deployment-operator` Principal and may have several independently
revocable Clients. A host operator creates a short-lived, one-use Enrollment with
`dorf client enroll`; `dorf connect` redeems it while binding a client-generated opaque bearer
credential. Dorf stores only the credential digest. Enrollment codes and credentials never belong
in URLs, Jobs, Profiles, logs, or provider configuration.

The remote CLI retains one normalized Deployment URL and its credential in an owner-only file. It
has no named contexts. Setup also enrolls one ordinary deployment-host Client and stores its proof
in protected host state. That Client uses only the fixed `http://127.0.0.1:8745` origin. A saved
remote `client.json` takes precedence over the host Client. `dorf auth status --output json` returns
the Deployment, effective Principal, Client, and credential source without returning the credential.
Client lifecycle remains host-owned:

```text
dorf client enroll [--output json]
dorf client list [--output json]
dorf client show [--output json] CLIENT_ID
dorf client revoke [--output json] CLIENT_ID
```

Revoke is idempotent. There are no remote Client-administration routes, Dorf passwords, Dorf-issued
JWTs, OIDC, RBAC, teams, or organizations.

Revocation rejects new authenticated requests immediately. An already-authenticated Job watch may
remain open only until its existing authentication deadline, at most one minute after connection;
afterward the watch closes and reconnecting with the revoked credential fails.

Agents use this same boundary: an ordinary skill or runbook can invoke the structured CLI, while
code mode can consume the OpenAPI document and call HTTPS directly. Dorf does not require a second
agent-specific protocol.

## Resources

[`internal/controlapi/openapi.json`](../internal/controlapi/openapi.json) is the checked-in authority
for the complete HTTP operation, request, response, schema, status, and Problem inventory. Each
Deployment serves that same OpenAPI document at `GET /v1/openapi.json`; discovery at `GET /v1`
links to it. Use the document served by the Deployment when generating a client or making direct
HTTP calls. The prose below explains behavior that clients need to handle, but it is not an
operation or schema inventory.

Job listing is newest-first keyset traversal of current facts, not a frozen snapshot. `limit`
defaults to 50 and accepts 1–100. Each item is deliberately only `id`, `kind`, and `admitted_at`;
read the Job for mutable execution and cleanup state. Pass `next_cursor` back unchanged. Cursors are
opaque, and malformed or altered cursors return the published `invalid_cursor` Problem. The index
contains only Job kinds understood by this API revision. Investigation admission requires a
credential-free reachable HTTPS repository and an exact Revision.

Direct and workflow admission may select a named AI connection. Omission uses the deployment
default, and the admitted Job retains the resolved connection. Model is also optional. Omission
uses that resolved connection's default, while an explicit model overrides it for this Job. The
admitted Job always returns and retains the exact resolved model. Job and Message admission and
explicit retry take caller-known request identity before transmission.
Direct HTTP callers supply `Idempotency-Key`; the CLI generates it, retries one ambiguous transport
or server failure with the same key, and includes it in structured receipts. Exact replay returns the
same resource, while changed input returns `idempotency_conflict`. Cleanup is inherently idempotent.
A server-generated response key would not resolve a lost admission response.

Job inspection is the canonical snapshot and supports representation ETags. Watch is an SSE delivery
optimization over complete canonical snapshots: it may coalesce intermediate values and reconnects
by reading current truth rather than replaying a second event log. Follow is durable FIFO input;
steer remains bound to the exact active Turn and never degrades into Follow. Retry accepts only an
eligible failed execution. Abandon records an idempotent `abandoned` Outcome only for a coding Job,
then requests cleanup. Cleanup remains separate from execution and Outcome. A settled Message whose
internal encoded JSON observation exceeds 16 MiB returns
the published `message_unavailable` Problem rather than a partial result.

Sandbox files are exact, caller-selected, workspace-relative regular-file reads at Sandbox level.
The server enforces Job custody and the cleanup fence; the response includes exact bytes, length, and
digest. There is no remote file write, listing, glob, archive, or directory API. Evidence responses
contain verified immutable metadata, not arbitrary result blobs or internal recovery identities.

Dorf-origin failures use RFC 9457 Problem Details. Stable `code`, `retryable`, and `details` fields
let automation avoid parsing prose. The same central catalog constructs runtime responses and is
published as `x-dorf-problems` in the OpenAPI document. Failures generated by an ingress may not use
that representation, so clients must also handle transport and generic HTTP server failure.

## Deployment services

The accepted managed deployment is one versioned Docker Compose project. The checked-in
[`deploy/compose.yaml`](../deploy/compose.yaml) owns the exact managed service and network inventory;
the release installs its versioned static counterpart. The optional local-Incus overlay gives only
the worker access to the configured Incus Unix socket. A remote Incus endpoint instead uses its
HTTPS and mTLS adapter. The supported remote topology and isolation procedure are in
[Getting started](getting-started.md#prepare-a-remote-incus-workstation).

At the operator level, the Deployment has two public flows:

```text
Remote Client -> HTTPS Control API origin
                 | guided deployment ingress
                 ` operator-managed ingress
Sandbox       -> HTTPS model origin -> Provider Gateway
```

The API receives its database URL, read-only API state, and an independently derived reader token
through the protected Compose environment. It receives no Incus socket or identity, E2B key,
GitHub credential, Gateway state, or provider configuration. The worker's narrow reader answers
only default and named AI-connection observation, GitHub installation discovery, one exact stored
Job Proposal observation, an exact Job-owned Sandbox file read, and one settled Message result. It
has no generic proxy, provider selector, credential response, or mutation operation.

The Compose manifest encodes startup dependencies, health checks, published ports, profile-gated
services, and network attachment. The project uses no host networking and mounts no host Docker
socket. [D101](project/decisions/D101-compose-owns-deployment-lifecycle-bootstrap-privilege-stays-explicit.md)
records the live proof for this boundary.

The release installs static `dorf-compose.yaml` and `dorf-compose-incus.yaml` manifests beside the
binary. One continuous `dorf setup` flow writes the protected `.env`, applies only those exact
manifests through Compose as needed, waits for readiness, and continues guided configuration and
verification. It does not render Compose YAML, reconcile arbitrary Docker resources, or provide a
general lifecycle wrapper. A human or deployment agent uses Compose directly from the generated
project directory only for advanced operations. The
[deployment-host procedure](getting-started.md#1-install-the-application-initialize-a-deployment-host)
is the sole authority for operator identity, privilege, installation, setup application, update,
status, restart, logs, and resumability.

The managed project always uses its PostgreSQL service and the protected persisted deployment
configuration as authority. `DORF_DATABASE_URL` remains only a development, test, or explicitly
manually supervised process override; it does not select another managed topology.

Guided Cloudflare setup owns one narrow public-ingress case: one named Tunnel publishes the distinct
Control API and Provider Gateway origins selected during setup. `dorf connect` receives the Control
API origin. Any custom Control API ingress remains operator-owned and reaches host port `8745`;
advanced `--gateway-url` changes only the Provider Gateway route. See
[Getting started](getting-started.md#1-install-the-application-initialize-a-deployment-host) for the
domain and hostname procedure,
[Getting started](getting-started.md#3-connect-one-remote-cli-client) for client connection, and
[Support](support.md) for fault attribution.

## Deliberately deferred

Dorf does not yet add multiple saved Deployment contexts, a browser UI, browser login, multi-user
identity, workload identity, mTLS, MCP, A2A, hand-written SDK families, webhooks, a copied event
store, writable or listable Sandbox files, public workflow registration, a workflow DSL, or a
high-availability hosted control-plane topology. A concrete client must earn the next smallest
surface.
