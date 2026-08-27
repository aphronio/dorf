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

The published OpenAPI document is the exact authority for request and response schemas, required
headers, enum values, content types, status codes, and the closed `direct`, `coding`, and
`codebase-investigation` Job union. Its current resource shape is:

```text
GET   /v1
GET   /v1/openapi.json
POST  /v1/auth/enrollments/redeem
GET   /v1/me

GET   /v1/jobs
POST  /v1/jobs
POST  /v1/workflows/coding/jobs
POST  /v1/workflows/codebase-investigation/jobs
GET   /v1/jobs/{job}
GET   /v1/jobs/{job}/watch
POST  /v1/jobs/{job}/messages
GET   /v1/jobs/{job}/messages/{message}
POST  /v1/jobs/{job}/retries
GET   /v1/jobs/{job}/evidence
PUT   /v1/jobs/{job}/abandon
PUT   /v1/jobs/{job}/cleanup
GET   /v1/sandboxes/{sandbox}/files?path=PATH
```

Job listing is newest-first keyset traversal of current facts, not a frozen snapshot. `limit`
defaults to 50 and accepts 1–100. Each item is deliberately only `id`, `kind`, and `admitted_at`;
read the Job for mutable execution and cleanup state. Pass `next_cursor` back unchanged. Cursors are
opaque, and malformed or altered cursors return the published `invalid_cursor` Problem. The index
contains only Job kinds understood by this API revision. Investigation admission requires a
credential-free reachable HTTPS repository and an exact Revision.

Direct and workflow admission may select a named AI connection. Omission uses the deployment
default, and the admitted Job retains the resolved connection. Job and Message admission and
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

The accepted managed deployment is one versioned Docker Compose project. Its complete supported
terminal remains gated by
[D101's live proof](project/decisions.md#d101--compose-owns-deployment-lifecycle-bootstrap-privilege-stays-explicit):

```text
Remote Client -> HTTPS Deployment origin
                 | guided cloudflared -> control-api:8745 (Compose ingress)
                 ` custom host ingress -> 127.0.0.1:8745
Sandbox       -> HTTPS model origin -> same Tunnel -> provider-gateway:8317

PostgreSQL -> migrate -> worker + authenticated narrow reads <- control-api
```

PostgreSQL, the one-shot migration, the control API, and the durable worker are always in the base
project, even when no Sandbox provider is selected. Migration must complete successfully before
the worker or API starts. The worker hosts the independently authenticated, fixed read endpoint
needed by the API; it is not another service or lifecycle. The Provider Gateway and guided named
Cloudflare Tunnel are optional foreground services under the same Compose supervisor. Guided setup
attaches only the control API and Provider Gateway to the Tunnel's ingress network.

The API receives its database URL, read-only API state, and an independently derived reader token
through the protected Compose environment. It receives no Incus socket or identity, E2B key,
GitHub credential, Gateway state, or provider configuration. The worker's narrow reader answers
only default and named AI-connection observation, GitHub installation discovery, one exact stored
Job Proposal observation, an exact Job-owned Sandbox file read, and one settled Message result. It
has no generic proxy, provider selector, credential response, or mutation operation.

Bridge networks keep database access, API-to-worker reads, worker-to-Gateway calls, runtime egress,
and Gateway ingress explicit. PostgreSQL and the control API publish only on host loopback; the
control API also joins the optional guided ingress network. The Gateway publishes only an explicitly
selected Profile route. The one-shot
migration uses runtime egress only to retrieve its checksum-pinned Absurd schema before exiting. The
project uses no host networking and mounts no host Docker socket. The optional local-Incus overlay
gives only the worker the exact configured Incus Unix socket; a remote Incus endpoint uses its
explicit HTTPS and mTLS adapter instead, but guided remote deployment remains unsupported.

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
