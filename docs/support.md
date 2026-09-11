# Support and diagnostics

## Optional Codex execution logs

Set `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` in the worker process to enable OTLP/HTTP execution logs.
An empty endpoint disables this observer. `OTEL_EXPORTER_OTLP_LOGS_HEADERS` supplies exporter
authentication; `OTEL_RESOURCE_ATTRIBUTES` can set `deployment.environment.name`. For the managed
Compose worker, put these variables in `${XDG_CONFIG_HOME:-$HOME/.config}/dorf/telemetry.env`,
owned by the deployment operator with mode `0600`, then recreate the worker. This optional file
survives setup and application updates; do not edit the generated Compose `.env`. Standalone workers
read their process environment. The official OpenTelemetry exporter owns batching and bounded retries.

Each selected native notification carries the exact Dorf Job, Message, and AgentRun IDs plus
the native Thread and Turn IDs. The Turn comes from the submission acknowledgement or the existing
durable binding. This covers Follow turns, including an initial direct Message. Steers do not
take ownership of another Message's model usage. API response types are unchanged.

The selected events are turn start/settlement, completed user and assistant messages, tool
start/completion, and token-usage updates. They can contain prompts, tool arguments, and outputs.
Only events exposed by the native app-server protocol are available; this is not a complete model
request or a record of every code-mode wrapper. The observer retains the authenticated connection
after submission so completion and interruption do not depend on another user message.

These logs are best-effort diagnostics. Worker reconciliation can reconnect to a still-active,
durably bound Turn. A disconnect is reported when observed, but missed tools and usage are not
replayed from session files or reconstructed from history. A terminal snapshot after reconnect
records only the exact Turn's status. Export loss does not change Message results or execution.
`event.id` identifies a repeated diagnostic payload for query deduplication; it is not a durable
delivery receipt.

Token-usage `last` describes the native latest model response, while `total` is conversation
cumulative. Do not label `total` as per-Message usage or sum replayed snapshots. These observations
do not establish complete billing or cost accounting. Configure the receiving backend to join its
client's Run-to-Message records by `dorf.message_id`; timestamps and prompt text are unnecessary.

This path exports from the Dorf host. It requires no exporter credential, plugin, collector, or
telemetry configuration inside a Sandbox. It does not enable native internal spans or metrics.

The managed deployment shape is x86_64 Linux with an operator-prepared Docker Engine and Compose
plugin. The current early release line is proven on the live controller, remote Incus workstation,
and public Control API path recorded by
[D101](project/decisions/D101-compose-owns-deployment-lifecycle-bootstrap-privilege-stays-explicit.md).
Its frozen clean-host reproduction remains deferred until the first external operator or a material
installation, bootstrap, Compose, or packaging change. Local profiles additionally require an
operator-prepared usable Incus endpoint and KVM; cloud-only E2B deployments do not. Setup does not
prepare missing host prerequisites; follow the
[deployment-host setup procedure](getting-started.md#1-install-the-application-initialize-a-deployment-host)
for the exact administrator-helper or manual handoff.

E2B requires an exact qualified template and a stable deployment-owned HTTPS Provider Gateway route.
The official Incus image includes pinned `browser-use` and headless Chromium, with the browser-use
skill installed for Codex. The same CLI is available to Pi. Each Sandbox boots its own fresh browser
profile and exposes CDP only at `127.0.0.1:9222` inside the VM. Browser state survives subsequent
Messages in that Sandbox. Local browser recordings are enabled; `browser-use recordings disable`
turns them off for that Sandbox. Cleanup removes the browser profile and recordings with the VM.
This capability does not use the operator's desktop browser or a browser cloud service. Existing
E2B templates do not gain it when the Incus image changes.

Guided setup routes the model hostname through the same named Cloudflare Tunnel as the separate
Control API hostname; any operator-owned route satisfying the exact HTTPS `/v1` Gateway contract is
also valid. The [deployment-host procedure](getting-started.md#1-install-the-application-initialize-a-deployment-host)
owns the domain, exact hostname pair, replay, and replacement flow. Disposable Quick Tunnels are
proof-only. macOS, Windows, and ARM are not supported. Remote Incus is supported only through the
fixed Tailscale, restricted-project, isolated-bridge, and stable HTTPS Gateway procedure in
[Getting started](getting-started.md#prepare-a-remote-incus-workstation). Arbitrary public Incus
listeners, Tailscale Funnel or Serve, subnet routers, and inferred guest routes remain unsupported.
Docker authority
follows the deployment-host setup procedure, and the socket is never mounted into a Dorf workload
or Sandbox. Custom Sandbox artifacts may be admitted through an explicitly created and functionally
verified profile, but carry no Dorf release provenance. An E2B profile that blocks general internet
access cannot run coding or investigation work that must clone its remote Git source; Dorf rejects
that combination before admitting a Job.

The remote control API is a separate authority on a separate hostname from the Provider Gateway.
Its public boundary is one exact HTTPS Deployment origin. Guided Cloudflare reaches container port
`8745` over the Compose ingress network and publishes the exact pair selected during setup; custom
operator-owned ingress reaches the API's published loopback host port. The Compose-managed worker
separately owns durable task execution and recovery. After Enrollment, a remote CLI Client needs
network and TLS access to the Control API origin and only its own Dorf Client credential; it never
needs PostgreSQL, provider, Harness, Gateway, or Sandbox credentials. Fixed remote coding and
investigation admission reuse the same boundary. The
[remote-client setup procedure](getting-started.md#3-connect-one-remote-cli-client) owns the current
workflow inputs; the [Remote Control API](control-api.md) owns the service and transport contract.

Run the Go CLI's direct diagnostic boundary:

```bash
dorf doctor --profile SANDBOX_PROFILE
```

Output is bounded JSON. Each item is `ready` or `failed` and
contains one concrete repair. Omit `--profile` to use the verified default. Dorf reports PostgreSQL,
Absurd, queue, selected Sandbox profile and its base verification,
Provider Gateway, and selected AI connection separately. Incus checks its configured endpoint,
profile-owned project, pool, network, exact image, guest route, and credential-free image; E2B
checks its exact profile configuration and host-only API key.

Optional external integrations have their own readiness boundary. For GitHub, rerun
`dorf integration github setup` to prove the deployment-default App identity and that it has at least
one installation; a missing installation resumes the operator handoff at its reusable URL. Exact
repository access and least permission scope are verified by the runtime operation that needs them.
[Getting started](getting-started.md) contains the setup procedure.

For any Client, start with `dorf auth status`. If `dorf connect` fails during discovery, the
failure belongs to DNS, TLS, ingress, or the control API on host port `8745`. An `unauthenticated`
response means the saved Client credential is invalid, expired, or revoked. Rerun `dorf setup` for
the setup-owned host Client, or issue a new Enrollment for a remote Client. Use
`dorf auth status --output json` for automation.
`invalid_cursor` means a Job-list cursor was not passed back unchanged; begin a fresh traversal
rather than altering it.

On the deployment host, start service diagnosis with the direct Compose status and log operations in
the [deployment-host procedure](getting-started.md#1-install-the-application-initialize-a-deployment-host).
The Compose-owned PostgreSQL service must be healthy, the one-shot migration must have succeeded,
and the worker and control API must be healthy. Worker readiness includes its narrow authenticated
reader. A configured Gateway or Cloudflare Tunnel must also be running under its selected Compose
profile. For guided Cloudflare, setup separately proves the public Control API origin and model
origin. Rerun `dorf setup` to apply the exact installed project as needed and probe the control API
and other prepared authorities. Setup does not install host prerequisites, repair arbitrary Docker
resources, or replace the direct advanced Compose operations in the deployment-host procedure.

The checked-in [`deploy/compose.yaml`](../deploy/compose.yaml) owns the exact managed service and
network inventory. The [deployment service explanation](control-api.md#deployment-services) covers
the operator-facing boundary, while Getting started alone owns lifecycle commands. A deployment using
`DORF_DATABASE_URL` or separately supervised processes is outside this managed topology and must
own its own supervision and configuration custody. A remote client must not perform any of these
host actions.

If admission succeeds but a Job does not progress while the managed project is ready, continue with
the Profile, Sandbox, Harness, Provider Gateway, and integration checks below rather than
attributing the failure to ingress.

A watch reconnecting after API interruption is expected; its next value is a canonical snapshot,
not replay from an event log. `steer_unavailable` means the exact active Turn no longer exists and
must not be resent as Follow. `retry_unavailable` means there is no eligible failed execution.
`file_unavailable` after cleanup begins is expected. `evidence_unverified` or a file digest mismatch
is a Dorf control-path integrity failure. An `idempotency_conflict` means the same request key was
reused with changed complete input; replay only the original request or choose a fresh key.

Ownership guide:

- the configured Incus endpoint failing independently is an Incus, network, or host problem;
- an E2B API or provider-resource failure outside Dorf is an E2B account, network, or service
  problem;
- the official image failing credential or Harness checks is a Dorf image or Harness compatibility
  problem;
- broker authentication failing independently is Provider Gateway/upstream provider work;
- control discovery or TLS failing independently is control ingress or deployment-service work;
- a non-current, inactive, or failed Dorf Compose service is deployment-service work;
- an expired or revoked Client being denied is expected control authentication behavior;
- incorrect durable facts, duplicate effects, leaked secrets, or incomplete cleanup are Dorf bugs;
- absent KVM or disabled virtualization is host configuration;
- another OS or architecture is unsupported, not silently equivalent.

Never attach Enrollment codes, Client configuration, Provider Gateway state, credentials,
environment dumps, Harness transcript contents, complete inspection output, watch snapshots, or
Message output to a report. Those surfaces may contain the caller's full goal or agent output.
Report only the needed Job ID and reviewed state, attention, and cleanup facts; redact caller input
first.
