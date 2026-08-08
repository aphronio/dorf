# Shared Provider Gateway

The Provider Gateway is a sibling application subsystem. It owns durable upstream Provider
Connections and revocable consumer-specific Inference Routes; it does not own Job sequencing,
Sandbox lifecycle, transcripts, review, or repository policy.

The supported route is ChatGPT subscription to Codex Responses WebSockets through pinned
CLIProxyAPI 7.2.104. `dorf provider connect chatgpt` installs the checksum-verified x86_64 Linux
broker, binds it to the selected private Incus bridge rather than wildcard/LAN, completes one
device login, and records protected connection metadata. The broker is the sole upstream credential
and refresh writer. Each Sandbox receives only a scoped broker route and Codex configuration.

The Go Job path creates, observes, and revokes routes directly. Provider connection state, route
state, keys, and the broker executable live under the configured
`DORF_PROVIDER_GATEWAY_STATE`; they are not PostgreSQL facts and never enter a Sandbox image.

## Security and recovery

- Upstream OAuth state stays in the host broker's protected `auth` directory.
- Route keys are broker-local capabilities, never upstream credentials.
- The broker binds to loopback or one exact private Incus bridge IPv4, never wildcard.
- Route creation and revocation use stable Action identities and authenticated management calls.
- Missing, ambiguous, stale, or non-WebSocket authentication fails before Sandbox mutation.
- Cleanup is incomplete until the exact Sandbox route is revoked or attention remains observable.
- Logs and CLI output never render upstream, management, guard, route, GitHub, or Codex control
  credentials.

Do not add provider pooling, fallback, quotas, a registry, another broker, or a wire dialect until a
concrete validated consumer requires it. D036 remains the governing decision; D047 changes the
control plane from Python to Go, not this connection/route authority model.
