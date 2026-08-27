# Private provider attachment playbook

Use this playbook when D101's remote provider attachment trigger fires. It covers a Sandbox
provider on a private host that the Dorf worker cannot reach directly. Examples include a remote
Incus host, a local VM service on dedicated metal, and a future Mac or smol machine provider.

This document records a starting point, not an accepted `Node` concept or support claim. The
[architecture](../project/architecture.md#current-dogfood-deployment-terminals) and
[D101](../project/decisions.md#d101--compose-owns-deployment-lifecycle-bootstrap-privilege-stays-explicit)
remain authoritative for the supported remote Incus topology. Update those authorities before
replacing that topology.

## Classify the provider endpoint first

Do not design provider-specific remote networking before classifying the endpoint:

```text
Does the provider expose a stable official API that the Dorf worker can reach?
  yes -> use a direct provider adapter
  no
    Does the provider run on the Dorf worker host?
      yes -> use its Unix socket or loopback API
      no
        Is this one proven private endpoint for one operator?
          yes -> use a standard private overlay such as Tailscale
          no  -> evaluate an outbound private-provider connector
```

Keep provider lifecycle and provider reachability separate. Incus, smol, and later runtimes keep
their concrete APIs and adapters. A connector supplies a secure path to a named private endpoint.
It does not become another Sandbox provider.

## Keep each responsibility with one owner

Use these boundaries when comparing designs:

| Responsibility | Owner |
| --- | --- |
| Host virtualization, provider installation, and guest isolation | Host administrator and the concrete provider procedure |
| Connector enrollment, identity, revocation, and allowed endpoint names | Dorf |
| Encryption, multiplexing, flow control, and keepalives | A maintained transport dependency |
| Sandbox ownership, deterministic names, retries, recovery, and cleanup | Dorf Core and the concrete provider adapter |
| Provider capability and guest-route proof | The Sandbox Profile verification terminal |

Do not add another durable command queue. Absurd already owns Dorf task retries and recovery. A
provider request remains an idempotent external operation. If the connector is offline, the current
task retries after the connection returns.

## Reuse transport before writing one

Evaluate transport choices in this order when the trigger fires:

1. Keep Tailscale or use its embedded [`tsnet`](https://tailscale.com/docs/features/tsnet) package
   if its tailnet ownership and policy remain acceptable. This reuses NAT traversal, device
   identity, encryption, and access rules.
2. Spike [Rancher remotedialer](https://github.com/rancher/remotedialer). It lets a private
   connector open an outbound WebSocket session so the server can dial a connector-local service.
   The library supplies sessions and backpressure. Rancher uses it with registration, reconnect
   loops, health checks, and multi-server routing.
3. Use [Kubernetes Konnectivity](https://github.com/kubernetes-sigs/apiserver-network-proxy) only if
   its standalone proxy and agent are smaller than a Dorf-owned integration. Its reverse gRPC and
   mTLS design is relevant, but its release and deployment model follows Kubernetes.
4. Use [HashiCorp yamux](https://github.com/hashicorp/yamux) only if the higher-level choices fail.
   Yamux supplies bidirectional streams, flow control, and keepalives. Dorf would still own the
   public listener, authentication, session registry, reconnect policy, and target authorization.

Before selecting a dependency, recheck its maintenance, security history, license, API stability,
and transitive dependency cost. Do not preserve this ranking if the evidence changes.

## Keep the first connector narrow

Treat `Dorf Node` and `Provider Connector` as provisional names until a live slice earns one. Do not
reuse Dorf Client authority. A Client submits and observes Jobs. A private-provider connector
carries provider operations and Sandbox byte streams.

The first design should have these limits:

- Reuse Dorf's one-time enrollment shape, but issue a separate connector identity and permission
  set. Generate the private key on the private host.
- Let each Sandbox Profile name one exact connector. Do not add node pools, placement, scheduling,
  discovery, or multi-hop routing.
- Expose symbolic endpoint names such as `incus`, not arbitrary addresses or Unix paths. Map each
  name to one administrator-prepared loopback service.
- Reject host shell execution and every destination outside the exact endpoint map.
- Keep the provider SDK in the Dorf worker when an injected network dialer can carry its existing
  protocol. Do not create a second provider RPC contract without evidence that raw provider
  transport is unsafe or insufficient.
- Keep connection state ephemeral. Jobs and Profiles pin durable identities, not a live socket.

Use this data-shape hypothesis to test the design. Do not add it to the Dorf schema before the
transport spike passes:

```text
ConnectorIdentity   durable: id, public identity, enrollment, revocation
ConnectorSession    ephemeral: current connection and observed liveness
ProviderEndpoint    durable: provider kind, connector id, symbolic target, authority hash
SandboxProfile      durable: endpoint and exact verified provider contract
```

## Wait for the accepted trigger

D101 owns the reconsideration trigger. Until it fires, collect evidence instead of adding connector
code. Useful evidence includes the number of private providers and remote hosts, operators who
cannot use Tailscale, recurring overlay maintenance failures, and a local provider that has passed
its capability proof but needs remote control.

One operator's preference for fewer setup commands does not justify a connector. A connector adds a
public service, long-lived sessions, identity rotation, reconnection, version compatibility, and
another failure boundary.

## Run one transport spike before changing architecture

Use the existing Incus adapter as the first oracle because its direct remote terminal already
passes. The spike should:

1. Start one outbound connector session from a private Linux host.
2. Permit only one symbolic `incus` target mapped to a loopback Incus endpoint.
3. Inject the connector dialer into the existing Incus SDK path.
4. Create a VM, open the Harness stream, run one real turn, read one exact file, and delete the VM.
5. Restart the Dorf worker and the connector separately, then prove recovery through a new session.
6. Attempt to dial the host, the LAN, the tailnet, and an unlisted loopback port. Require every
   attempt to fail.

Accept the connector only if the terminal proves all of these facts:

```text
no inbound listener on the private host
no arbitrary host, LAN, or Unix socket access
no new durable task queue
one-time enrollment and exact revocation
existing provider adapter remains authoritative
worker and connector restart recover
Gateway route and VM cleanup leave no resource
operator flow is materially shorter than the Tailscale path
```

If the spike fails, keep the proven overlay and record the failed assumption. If it passes, update
the Architecture and Decision Log before implementation. Deliver one provider and one connector in
a vertical slice. Migrate the remote Incus caller and delete the obsolete remote transport in the
same change instead of retaining two supported paths.

For a new Sandbox runtime, prove the provider's local lifecycle and isolation before testing this
connector. The [Sandbox and VM watchlist](sandbox-vm-watchlist.md) owns provider evaluation order
and its capability bar.
