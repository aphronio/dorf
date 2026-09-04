# D036: Shared Provider Gateway for trusted clients and Dorf Sandboxes

- **Applicability:** current
- **Areas:** model-access, sandboxes, deployment
- **Read when:** Changing Provider Gateway connections, consumer routes, or deployment ownership.
- **Decision history:** Accepted direction — 2026-07-29; Go control plane at D047 cutover — 2026-08-08;
  remote E2B route wire proved — 2026-08-14; named Cloudflare route implemented, live hostname proof
  pending — 2026-08-21; deployment supervision and Incus route custody refined by D101 — 2026-08-26;
  broker-state ownership and explicit stale-DNS repair refined — 2026-08-27; guided public origins
  refined by D102 — 2026-08-27; remote Incus guest route proved — 2026-08-27
- **Decision:** Keep the Provider Gateway as a sibling application subsystem outside the durable
  Job core. Its programmatic boundary manages durable upstream Provider
  AI connections and revocable consumer-specific Inference Routes over a supervised broker backend.
  Dorf composes it for Sandbox routes; trusted host applications may use the same authority for their
  own model routes. Connecting through either surface reaches one backing authority, so upstream
  subscription or API credentials are never copied into clients or Sandboxes. CLIProxyAPI is the first
  concrete daemon backend; D035 is the first validated ChatGPT-to-Codex route.
- **Location authority:** Deployment configuration resolves one Gateway data directory using
  `XDG_DATA_HOME` (falling back to `~/.local/share`). The Gateway-specific
  `DORF_PROVIDER_GATEWAY_STATE` name is container-only and cannot relocate setup-owned host state.
  Provider setup, doctor, admission, and task executors use that same adapter. Admission checks the
  named connection before creating a durable Job; the Job stores only the connection name, never a
  host path.
- **Local and remote posture:** Every Sandbox Profile owns one exact guest-reachable `/v1` Gateway
  URL; E2B requires HTTPS, while Incus may use an operator-routed private/VPN address or HTTPS. The
  adapter verifies that persisted route rather than inferring it at runtime. Guided setup may
  resolve one unambiguous prepared bridge IPv4 through the configured local Unix endpoint once and
  persist the exact `http://IP:8317/v1` route in the new Profile. A remote endpoint never uses that
  convenience. A remote Sandbox's scoped route key authenticates Gateway requests, and exact-host
  default-deny egress remains adapter-owned.
  Any exact stable HTTPS `/v1` ingress remains valid deployment input. Guided setup owns one narrower
  convenience: D102's named outbound-only Cloudflare Tunnel publishes the separate Provider Gateway
  and Control API origins defined by the [Provider Gateway authority](../provider-gateway.md). Only the
  model origin reaches the private broker. The broad Cloudflare account certificate used to create
  the Tunnel and its two DNS routes is removed after readiness. Operator-owned Gateway ingress
  retains the universal protected-API check because Dorf cannot attest routing it does not own.
  Disposable Quick Tunnels remain proof-only. Workload identity beyond the scoped route and
  multi-user authority remain unimplemented until a concrete deployment requires them.
- **Broker-state ownership:** Dorf attests the pinned broker executable and its own launch inputs.
  The running broker owns and may normalize its protected active configuration; Dorf does not hash
  those mutable bytes as a second launch authority or prevent setup from resuming after a valid
  runtime rewrite.
- **Provider posture:** The gateway is intended to admit validated subscription providers such as
  ChatGPT, Kimi Code, or Grok and API-key providers such as OpenAI or OpenRouter. Names are direction,
  not support claims. Validate each provider, auth mode, consumer wire dialect, refresh path, and
  concurrency behavior before advertising it. ChatGPT subscription and one OpenAI API key are the
  supported choices; both retain the upstream credential on the host and use the same scoped
  Sandbox route. The deployment currently admits only one unprefixed OpenAI authentication mode at
  a time. Do not add automatic pooling, fallback, quota scheduling, or a speculative capability
  matrix.
- **Why:** Host applications and Dorf Sandboxes are distinct consumers of the same model-provider
  connection. Sharing a typed facade and broker authority gives them login-once behavior without
  coupling provider state to Job semantics, duplicating credentials, or forcing model streams
  through the durable Job worker.
- **Default selection:** Guided setup and an explicit successful AI connection select one
  deployment-default AI connection. New Jobs use that name unless `--ai-connection` overrides it,
  then durably pin the resolved name. This default is deployment-wide rather than part of a Sandbox
  profile, so one authenticated model route can serve multiple Incus, E2B, Codex, and Pi profiles.
- **Reconsider when:** A second broker backend proves a smaller shared interface; an actual remote
  deployment requires a network authority; a provider cannot fit connection-plus-route semantics
  without distortion; or observed multi-account pressure justifies routing policy.
