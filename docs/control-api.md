# Remote Control API

Dorf exposes one deliberately narrow HTTPS boundary for operating one configured Deployment. The
machine-readable authority is the embedded OpenAPI 3.1 document served by that Deployment at
`GET /v1/openapi.json`; discovery at `GET /v1` links to it and advertises supported capabilities.

This is a projection of Dorf's existing Job custody, not a network serialization of Core. A Job is
the long-running resource. Clients can also discover narrow Sandbox profile summaries for Job
selection. PostgreSQL rows, Absurd tasks, AgentRuns, Threads, Turns, Actions, providers, Harnesses,
profile configuration, and integration credentials are not public resources.

## Client and authentication boundary

One Deployment currently has one `deployment-operator` Principal and may have several independently
revocable Clients. A host operator creates a short-lived, one-use Enrollment with
`dorf client enroll`; `dorf connect` redeems it while binding a client-generated opaque bearer
credential. Enrolled credentials expire after 90 days by default. For an unattended integration,
the host can provision an ordinary Client key with explicitly no expiry. Its identity and inventory
report `expires_at: null`; it remains valid until revoked. Both paths use the same bearer
authentication and Client revocation. Dorf stores only the credential digest. Enrollment codes and credentials never belong
in URLs, Jobs, Profiles, logs, or provider configuration.

The remote CLI retains one normalized Deployment URL and its credential in an owner-only file. It
has no named contexts. Setup also enrolls one ordinary deployment-host Client and stores its proof
in protected host state. That Client uses only the fixed `http://127.0.0.1:8745` origin. A saved
remote `client.json` takes precedence over the host Client. `dorf auth status --output json` returns
the Deployment, effective Principal, Client, and credential source without returning the credential.
Client lifecycle remains host-owned:

```text
dorf client enroll [--output json]
dorf client issue-key --name NAME --credential-file PATH --no-expiry [--output json]
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

Authenticated Clients can list all configured Sandbox profiles with `dorf profile list`, including
`--output json` for automation. Discovery advertises this operation as `profile_list`. The response
contains each profile's name, Sandbox provider, Harness, default status, and `verified` flag, sorted
by name. Empty deployments return an empty array. Credentials, artifacts, Gateway URLs, host
configuration, and verification diagnostics remain private. Profile creation, inspection of full
configuration, verification, updates, and default selection remain deployment-host operations.

`verified` reports whether stored proof matches the current definition and verification contract,
with a completed probe and cleanup and no recorded error. Listing performs no live provider or model
check and does not guarantee admission or execution. A profile may change between listing and Job
admission; admission remains authoritative. An unknown explicit profile returns the
`profile_not_found` Problem, and the CLI directs the caller to `dorf profile list`. Exact Job replay
continues to use the admitted profile without rechecking its current verification eligibility.

Job listing is newest-first keyset traversal of current facts, not a frozen snapshot. `limit`
defaults to 50 and accepts 1–100. Each item includes `id`, `kind`, `admitted_at`, and creator attribution;
read the Job for mutable execution and cleanup state. Pass `next_cursor` back unchanged. Cursors are
opaque, and malformed or altered cursors return the published `invalid_cursor` Problem. The index
contains only Job kinds understood by this API revision. Investigation admission requires a
credential-free reachable HTTPS repository and an exact Revision.

Job admission records the authenticated Client as `created_by_client`, with its ID and name.
Inspection, watch, and listing expose that creator even after credential expiry or revocation.
Older Jobs and internal admissions without a Client return null. Replaying an admission with another
Client preserves the original creator, including an unknown creator. Attribution does not restrict
which Jobs another authenticated deployment Client can inspect or clean up.

Callers may supply `client_reference` to correlate a Job with a thread or task. It is an opaque,
optional string; omission and empty mean no reference. Dorf retains it exactly and includes it in
admission replay equality, so changing it with the same idempotency key returns a conflict. It
carries no authority and must not contain credentials. The API derives creator identity from bearer
authentication and rejects caller-supplied creator fields.

Job creation prepares its execution configuration and resources without starting a conversation.
All input, including the first, uses Message admission. Direct clients may supply workspace
`AGENTS.md` contents at creation; Dorf installs the file before starting the Harness. After setup
settles, retries do not overwrite changes the agent makes to that file. The client owns the
instructions and the Harness interprets them.

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

The Job snapshot's optional `latest_reply_id` identifies the latest settled reply in its main
Sandbox. It derives from retained Message and AgentRun facts; queued follow-ups and steer delivery
acknowledgements do not replace it. The Message inspection path accepts `latest` in place of a
Message ID and resolves the same reply. A Job with no settled reply returns `message_not_found`.

Message requests default to `auto`: choose steer against the active Turn at admission, otherwise
admit a follow. The stored request intent distinguishes automatic selection from explicit follow
or steer, so replay cannot change either the request or its resolved target. A delivered steer
can have a null result while its Turn is still active; clients wait for the result, not merely
delivery acknowledgement, before presenting the final answer.

A client that changes installed skills can set `refresh_skills` on its next Message. A Follow
reloads before starting its fresh Turn. A Steer leaves the active Turn unchanged and retains the
request for the next fresh Turn, including a Follow that was queued before the Steer arrived.
Automatic intent uses the same existing delivery selection. Starting or recovering work keeps
later Follows queued. A failed refresh remains pending; an accepted fresh Turn proves the refresh
ran. The original request flag stays immutable on replay. Unsupported profiles reject the request
before admission; currently only Codex supports it. A completed Steer result confirms its target
Turn's outcome and does not acknowledge a skill refresh. Clients own safe file activation and
must not treat refresh admission as permission to replace software during active work.

A direct Codex Message can request interruption of its exact native Turn. This idempotent request
also accepts a steer attached to that Turn; it never targets a successor and does not close Job
admission or clean up the Sandbox. Dorf stores acceptance before contacting Codex, prioritizes Stop
over pending message delivery, and reconciles the native outcome after an uncertain acknowledgement.
`interrupt_requested` is acceptance, while the Message result is the observed outcome. An already
terminal target is a no-op. Unbound Messages and unsupported Job or Harness combinations return
`interrupt_unavailable`. Interruption is available through the `message_interrupt` discovery capability.

Sandbox files are exact, caller-selected regular files inside a Job-owned Sandbox. Paths may be
absolute, relative to the workspace, or start with `~/` for the Sandbox execution user's home.
Paths never refer to the deployment host. Symlinks and non-canonical paths are rejected.
The server enforces Job custody and the cleanup fence; the response includes exact bytes, length, and
digest. A bounded write can atomically replace one regular file, creating missing parent directories.
Files use mode 0600 and new directories use mode 0700. Create-only
writes preserve an existing file, including an intentionally empty file. The same custody and
cleanup fence apply. There is no listing, glob, archive, or directory API. Evidence responses
contain verified immutable metadata, not arbitrary result blobs or internal recovery identities.

Sandbox exec runs caller-supplied argv and optional stdin inside the same attested Job-owned
Sandbox, under the existing authentication and cleanup fence. It supports bounded setup commands
such as installing a CLI without rebuilding an image. The caller must explicitly invoke a shell
when shell interpretation is needed. Responses include the exit code, capped stdout and stderr,
and a truncation flag; nonzero exit codes are command results, not transport failures. The OpenAPI
document owns input, output, and timeout limits. Exec has no replay identity: after an ambiguous
transport failure, inspect the effect or repeat only an operation the caller knows is idempotent.
Clients own installed software and configuration. Exec does not admit a Message or start an
AgentRun.

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
Job Proposal observation, exact Job-owned Sandbox file reads and bounded Sandbox file writes, and one
settled Message result. It has no generic proxy, provider selector, or credential response.

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
store, listable Sandbox files, public workflow registration, a workflow DSL, or a
high-availability hosted control-plane topology. A concrete client must earn the next smallest
surface.
