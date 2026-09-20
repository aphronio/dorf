# Remote Control API

Dorf exposes one authenticated boundary for operating a configured Deployment. The embedded
[OpenAPI document](../internal/controlapi/openapi.json), served at `GET /v1/openapi.json`, owns the
exact operation, schema, limit and Problem inventory. Discovery links to it.

Session is the durable resource for configuration, one native Thread binding, compute and scoped
model access. The Harness owns conversation input, Turns and history. Dorf stores neither an input
inbox nor a second transcript. Native subagent threads remain Harness-owned.

## Client and authentication boundary

One Deployment currently has one `deployment-operator` Principal and may have several independently
revocable Clients. A host operator creates a short-lived, one-use Enrollment with
`dorf client enroll`; `dorf connect` redeems it while binding a client-generated opaque bearer
credential. Enrolled credentials expire after 90 days by default. For an unattended integration,
the host can provision an ordinary Client key with explicitly no expiry. Its identity and inventory
report `expires_at: null`; it remains valid until revoked. Both paths use the same bearer
authentication and Client revocation. Dorf stores only the credential digest. Enrollment codes and credentials never belong
in URLs, Sessions, Profiles, logs, or provider configuration.

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

Revocation rejects new authenticated requests immediately. An already-authenticated Session watch may
remain open only until its existing authentication deadline, at most one minute after connection;
afterward the watch closes and reconnecting with the revoked credential fails.

Agents use this same boundary: an ordinary skill or runbook can invoke the structured CLI, while
code mode can consume the OpenAPI document and call HTTPS directly. Dorf does not require a second
agent-specific protocol.

## Resources

Create a Session, wait for resource readiness, prepare its workspace, then submit a Session event.
Creation does not start a model Turn. The admitted profile selects the Harness and immutable provider
configuration. New native-event Sessions currently require Codex; Pi support needs a new adapter proof.
Profile listing exposes recorded verification, not a live readiness guarantee. Exact Session creation
replay retains its original profile and creator. `client_reference` is opaque client attribution.
All deployment Clients share the deployment-operator authority; creator attribution is not an ACL.

### Input and controls

Submit `input.message` with text and optional ordered attachments, or `input.tool_output` with
application-generated text. Supply a `client_id` for native attribution. It is **not an idempotency
key**. Input uses one native `turn/start` call: the Harness decides whether it joins active work or
starts a new Turn. There is no Follow/Steer mode, FIFO queue or offline inbox.

A successful `input.accepted` response includes Harness, Thread and Turn IDs. Several inputs can
return the same Turn. Acceptance does not prove a durable native history write or completion.
Repeating an input, including its `client_id`, may execute it again. Neither the API nor SDK retries
native mutations. Session creation and fixed lifecycle operations keep their own documented retry
identities; those identities do not extend to subsequent input.

Preparing, unavailable and maintenance-held Sessions reject input with `native_unavailable`.
Clients retain unsent input. A timeout, broken response or `native_outcome_unknown` requires native
evidence inspection, not resubmission. Missing history is not proof that input was rejected. Dorf
retains an unresolved mutation guard to prevent unsafe maintenance or another ambiguous dispatch;
only exact positive native evidence can settle it automatically.

`input.cancel` selects the currently active native Turn once and sends its exact native interruption
request. Its acknowledgement reports the selected Turn, or no Turn when none was active. It does
not promise completion. Observe that Turn afterward. A new cancel request can target newer work;
do not retry an ambiguous cancel or treat it as an idempotent stop handle.

Attachments travel inline by value as base64 bytes. The server validates bounded filenames, bytes
and supported image decoding, then writes files under the visible workspace-relative path
`attachments/<unique-id>/<ordinal>/<filename>`, includes their local paths in the input, and passes
supported images natively. Files and native history follow workspace retention; Dorf does not
retain attachment blobs for replay after cleanup.

`developer_instructions` and `refresh_skills` are per-request native adapter options. Instructions
are injected before that input; skill refresh uses the native catalog reload. Neither creates a
Dorf delivery envelope or a deferred refresh queue. Workspace AGENTS.md and SOUL.md remain client-owned.
Application tool output uses native tool-output attribution and is omitted from the conversational
raw-history view; completed-item observations retain its correlation for client publication.

### Turns, history and events

Turn listing returns native IDs, status, accepted input correlation and available output. The
history view returns bounded native conversation items. An explicit Turn selects that Turn; omission
selects the latest native Turn once. Unavailable, incomplete or oversized history fails honestly.
A native Thread ID is a binding observed through a Session, not a caller-created Dorf resource.

Turn listing also projects native token usage, execution settings and attributable response
measurements when available. Detail counters are subsets of their parent counts; absent usage
means unavailable, and null counters remain unknown. Values reflect what the Harness retained,
including any normalization it applied to provider data. The Codex adapter reads persisted usage
records from the exact rollout returned by `thread/read`; it uses native Turn aggregates rather
than thread-total differences or notification accumulation. Repeated reads can recover measurements
after disconnection or native process restart while that state remains available. Dorf does not
retain a second usage ledger or calculate prices. Native provider names may be route aliases.
Clients retain measurements before releasing the Session if they need them afterward.

The existing SSE observation transport now watches a native Turn through Session events. It emits
completed items and status, not token-by-token text. `turn.updated` and `turn.completed` are the
payload types. Ordinary Turn observation reads remain available without SSE. Session watch remains
a separate stream of resource snapshots.

Observation cursors bind the exact Session, Thread, Turn and completed-item prefix. Supply the cursor
unchanged; streaming also accepts the matching `Last-Event-ID`. A gap requires an explicit native
read and reconciliation of the published prefix. `resync_deferred` means the passive cache cannot
answer yet. Completion is proven only with a terminal native status and completion watermark.
There is no durable Dorf event log or promise to replay every missed notification. Worker observers
outlive HTTP clients; disconnecting does not stop accepted native work.

### Lifecycle and workspace access

New Sessions use `/workspace` as their working directory. Attachments are visible beneath that
root. Session IDs are opaque: newly admitted identities use the `session-` prefix, while replay
returns the identity already retained for that admission key.

Session inspection reports resource preparation, attention, maintenance and cleanup receipts.
Its resource-ready `idle` state is not a native Turn completion claim; inspect native Turns for work.
Resource history preserves exact reservations, provider locators and deletion receipts without
exposing ownership tokens. A missing locator or receipt does not prove provider absence.

Maintenance gates native mutation, file, command and native-history access. Passive Session and
resource inspection remain available. New input stays with the client. Recovery and package upgrade
receipts identify their exact resource and checkpoint; success never synthesizes native output.

The default idle policy can pause supported E2B compute after the grace period when native work is
settled and no mutation remains uncertain. `keep_running` preserves background compute between
Turns, subject to provider deadlines. Native/file/command access can resume compute and extends its
activity window; passive resource status and Session watch do not. Polling for lifecycle reconciliation
does not wake paused compute. A shell process is not itself an idle-policy exemption.

File access preserves bounded exact bytes, path checks, integrity and ownership. Paths may be
workspace-relative, absolute or `~/`-relative; traversal, symlinks and directories are rejected.
Exec accepts bounded argv and optional stdin, returning exit status and capped stdout/stderr. It has
no replay identity: inspect external effects after ambiguous failure before attempting another command.

Release closes admission, revokes model authority and deletes all exact owned resources. It is
terminal for that Session. Native files and history need not remain readable afterward. Metadata and
lifecycle receipts remain; there is no retained input archive. Retrieve application artifacts first.

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
through the protected Compose environment. It receives no Incus
socket or identity, E2B key, Gateway state, or provider configuration. The worker's narrow reader answers
only default and named AI-connection observation, exact Session-owned Sandbox file reads and bounded Sandbox file writes, and one
native Session event submission or native conversation read. It has no generic proxy, provider selector,
or credential response.

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

Sandbox status reads return a fresh provider name and normalized machine state. They use provider
metadata without starting, connecting to, pausing, or reconciling the machine. Reads are fenced
against cleanup. Session execution being idle does not imply that its machine is paused. A missing
owned resource is reported explicitly; an unavailable provider check returns a retryable Problem.
These observations are not stored and may change immediately after a response.

### Package upgrade inspection

Session Sandbox projections include retained `upgrades` alongside resource history. Upgrade records
identify the requested package version, source and optional destination resource, checkpoint,
verification time, terminal outcome, and failure code. Status is derived from recovery receipts and
current upgrade attention. Native input is unavailable while held; clients retain unsent work.

Package admission is currently operator-only through `dorf upgrade request`. API clients can inspect
progress but cannot install packages. `dorf upgrade show SESSION` includes the detailed retained receipt.

## Workspace persistence inspection

The workspace read exposes current configured backup coverage and the last published checkpoint
through the existing worker observation boundary. It performs no native execution or capture.
The [checkpoint contract](implementation/session-checkpoints.md#workspace-inspection) owns the
coverage and freshness semantics; OpenAPI owns the response fields.
