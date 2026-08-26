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
has no named contexts. `dorf auth status --output json` returns the Deployment, effective Principal,
Client, and credential source without returning the credential. Client lifecycle remains host-owned:

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

## Resources and compatibility

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
PUT   /v1/jobs/{job}/cleanup
GET   /v1/sandboxes/{sandbox}/files?path=PATH
```

Job listing is newest-first keyset traversal of current facts, not a frozen snapshot. `limit`
defaults to 50 and accepts 1–100. Each item is deliberately only `id`, `kind`, and `admitted_at`;
read the Job for mutable execution and cleanup state. Pass `next_cursor` back unchanged. Cursors are
opaque, and malformed or altered cursors return the published `invalid_cursor` Problem. The index
contains only Job kinds understood by this API revision; a retained host-local investigation bundle
does not cross the remote boundary.

Job and Message admission and explicit retry take caller-known request identity before transmission.
Direct HTTP callers supply `Idempotency-Key`; the CLI generates it, retries one ambiguous transport
or server failure with the same key, and includes it in structured receipts. Exact replay returns the
same resource, while changed input returns `idempotency_conflict`. Cleanup is inherently idempotent.
A server-generated response key would not resolve a lost admission response.

Job inspection is the canonical snapshot and supports representation ETags. Watch is an SSE delivery
optimization over complete canonical snapshots: it may coalesce intermediate values and reconnects
by reading current truth rather than replaying a second event log. Follow is durable FIFO input;
steer remains bound to the exact active Turn and never degrades into Follow. Retry accepts only an
eligible failed execution. Cleanup is an explicit idempotent request, separate from execution or a
workflow Outcome.

Sandbox files are exact, caller-selected, workspace-relative regular-file reads at Sandbox level.
The server enforces Job custody and the cleanup fence; the response includes exact bytes, length, and
digest. There is no remote file write, listing, glob, archive, or directory API. Evidence responses
contain verified immutable metadata, not arbitrary result blobs or internal recovery identities.

Dorf-origin failures use RFC 9457 Problem Details. Stable `code`, `retryable`, and `details` fields
let automation avoid parsing prose. The same central catalog constructs runtime responses and is
published as `x-dorf-problems` in the OpenAPI document. Failures generated by an ingress may not use
that representation, so clients must also handle transport and generic HTTP server failure.

## Deployment services

The supported deployment runs two separately supervised systemd units:

```text
operator-owned HTTPS ingress
            |
            v
dorf-control-api.service  -- private HTTP on 127.0.0.1:8745
            |
            +------------------- PostgreSQL + Absurd
                                |
dorf-worker.service ------------+
```

`dorf setup` converges both units after durable deployment configuration, even when no Sandbox
provider is selected yet. It shows the exact plan, obtains administrator authorization, installs or
updates only Dorf-owned units, restarts worker then API, and waits for them to become ready. The
units run as the exact setup operator, use the resolved Dorf executable and protected default
deployment configuration, declare systemd notification readiness, restart after failure, and carry
a bounded systemd hardening envelope.

Use the host-only lifecycle commands rather than supervising `dorf serve` and `dorf worker` in
ordinary deployments:

```text
dorf service reconcile [--yes] [--existing]
dorf service status [--output json]
dorf service restart <api|worker|all>
dorf service logs <api|worker> [--lines N]
```

Status distinguishes desired-unit convergence from runtime readiness and checks the private API's
discovery and authentication boundary. Reconcile refuses a foreign or locally edited unit instead
of overwriting it. `--existing` makes an absent pair a no-op rather than installing one.
`dorf update` uses that gate when handing an installed pair to the new binary for reconciliation
and restart, so a remote CLI-only installation remains service-free.

Managed units require the default protected deployment configuration rather than process-only
`DORF_*`, `XDG_*`, or credential environment authority. Setup reports when that boundary prevents
managed installation; such a custom deployment must provide its own explicit supervision and
configuration custody.

HTTPS ingress remains operator-owned and distinct from these two units. Dorf never infers, installs,
or mutates the public control origin. The control API must also use a different origin from the
Provider Gateway's OpenAI-compatible `/v1` service; the two authorities are not interchangeable.
See [Getting started](getting-started.md#3-connect-one-remote-cli-client) for the operator and client
procedure and [Support](support.md) for fault attribution.

## Deliberately deferred

Dorf does not yet add multiple saved Deployment contexts, a browser UI, browser login, multi-user
identity, workload identity, mTLS, MCP, A2A, hand-written SDK families, webhooks, a copied event
store, writable or listable Sandbox files, public workflow registration, a workflow DSL, or a
high-availability hosted control-plane topology. A concrete client must earn the next smallest
surface.
