# Support and diagnostics

The default supported deployment is x86_64 Linux. Ubuntu 24.04 is the proven clean-machine path.
Local profiles require a directly usable Incus daemon and KVM; cloud-only E2B deployments do not.
Other Linux distributions are capability-compatible only after their operator supplies the host
requirements selected during setup.

E2B requires an exact qualified template and a stable deployment-owned HTTPS Provider Gateway route.
Guided setup supports a named Cloudflare Tunnel; any operator-owned route satisfying the same exact
HTTPS `/v1` contract is valid. Disposable Quick Tunnels are proof-only. macOS, Windows, ARM, remote
Incus daemons, and host Docker-socket isolation are not supported. Custom Sandbox artifacts may be
admitted through an explicitly created and functionally verified profile, but carry no Dorf release
provenance. An E2B profile that blocks general internet access can consume a retained local Git
source, but cannot run coding or investigation work that must clone a remote Git source; Dorf rejects
that combination before admitting a Job.

The remote control API is a separate authority on a separate hostname from the Provider Gateway.
Its public boundary is one exact HTTPS Deployment origin backed by the private loopback-only
managed API; the managed worker separately owns durable task execution and recovery. After
Enrollment, a remote CLI Client needs network and TLS access to that origin and only its own Dorf
Client credential; it never needs PostgreSQL, provider, Harness, Gateway, or Sandbox credentials.
Fixed remote coding and investigation admission reuse the same boundary. The
[remote-client setup procedure](getting-started.md#3-connect-one-remote-cli-client) owns the current
workflow inputs; the [Remote Control API](control-api.md) owns the service and transport contract.

Run the Go CLI's direct diagnostic boundary:

```bash
dorf doctor --profile SANDBOX_PROFILE
```

Output is bounded JSON. Each item is `ready` or `failed` and
contains one concrete repair. Omit `--profile` to use the verified default. Dorf reports PostgreSQL,
Absurd, queue, selected Sandbox profile and its base verification,
Provider Gateway, and selected AI connection separately. Incus checks its command, access, network,
and credential-free image; E2B checks its exact profile configuration and host-only API key.

Optional external integrations have their own readiness boundary. For GitHub, rerun
`dorf integration github setup` to prove the deployment-default App identity and that it has at least
one installation; a missing installation resumes the operator handoff at its reusable URL. Exact
repository access and least permission scope are verified by the runtime operation that needs them.
[Getting started](getting-started.md) contains the setup procedure.

For a remote Client, start with `dorf auth status`. If `dorf connect` fails during discovery, the
failure belongs to DNS, TLS, ingress, or the private control listener. An `unauthenticated` response
means the saved Client credential is invalid, expired, or revoked; a deployment-host operator must
issue a new Enrollment. Use `dorf auth status --output json` for automation. `invalid_cursor` means a
Job-list cursor was not passed back unchanged; begin a fresh traversal rather than altering it.

On the deployment host, start service diagnosis with:

```bash
dorf service status --output json
```

This fails unless both managed units are current, enabled, runtime-ready, and the private API passes
discovery and authentication probes. Use `dorf service logs api` or `dorf service logs worker` for
bounded journal output, `dorf service restart <api|worker|all>` for an explicit restart, and
`dorf service reconcile` to preview and repair Dorf-owned unit drift. A foreign or locally edited
unit is refused rather than overwritten. If setup reports that process-only configuration prevents
managed services, that custom deployment must repair its own supervisor and configuration custody.
A remote client must not perform any of these host actions.

If admission succeeds but a Job does not progress while the managed pair is ready, continue with the
Profile, Sandbox, Harness, Provider Gateway, and integration checks below rather than attributing the
failure to ingress.

A watch reconnecting after API interruption is expected; its next value is a canonical snapshot,
not replay from an event log. `steer_unavailable` means the exact active Turn no longer exists and
must not be resent as Follow. `retry_unavailable` means there is no eligible failed execution.
`file_unavailable` after cleanup begins is expected. `evidence_unverified` or a file digest mismatch
is a Dorf control-path integrity failure. An `idempotency_conflict` means the same request key was
reused with changed complete input; replay only the original request or choose a fresh key.

Ownership guide:

- a minimal Incus command failing outside Dorf is an Incus or host-distribution problem;
- an E2B API or provider-resource failure outside Dorf is an E2B account, network, or service
  problem;
- the official image failing credential or Harness checks is a Dorf image or Harness compatibility
  problem;
- broker authentication failing independently is Provider Gateway/upstream provider work;
- control discovery or TLS failing independently is control ingress or deployment-service work;
- a non-current, inactive, failed, or non-notifying Dorf systemd unit is managed-service work;
- an expired or revoked Client being denied is expected control authentication behavior;
- incorrect durable facts, duplicate effects, leaked secrets, or incomplete cleanup are Dorf bugs;
- absent KVM or disabled virtualization is host configuration;
- another OS or architecture is unsupported, not silently equivalent.

Never attach Enrollment codes, Client configuration, Provider Gateway state, credentials,
environment dumps, Harness transcript contents, complete inspection output, watch snapshots, or
Message output to a report. Those surfaces may contain the caller's full goal or agent output.
Report only the needed Job ID and reviewed state, attention, and cleanup facts; redact caller input
first.
