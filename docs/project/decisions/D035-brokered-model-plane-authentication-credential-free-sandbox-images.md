# D035: Brokered model-plane authentication; credential-free sandbox images

- **Applicability:** current
- **Areas:** model-access, sandboxes
- **Read when:** Changing how Sandboxes authenticate to model providers without storing provider credentials.
- **Decision history:** Accepted — 2026-07-29 (supersedes D008 for Codex); OpenAI API-key path implemented,
  live credential proof pending — 2026-08-21
- **Decision:** A single host-side broker (pinned, vendored CLIProxyAPI) holds the ChatGPT OAuth
  bundle as the sole refresh writer. Sandboxes run real Codex app-server (D003) pointed at the
  broker via `model_providers` with a per-sandbox scoped key; sandboxes contain no OpenAI
  credentials and may be egress-limited to the broker alone. Incus images are credential-free for
  the Codex leg. Login is a one-time device-code flow; identical sandbox wiring serves
  ChatGPT-subscription or API-key billing. Chosen/rejected/parked options:
  `docs/history/model-auth-broker.md`. Validation: `docs/research/codex-auth-multi-sandbox.md`
  (2026-07-29 experiment).
- **Why:** Cloned secret-bearing images proved unreliable under concurrent refresh (#117, from
  #112/#114 Droid evidence). Brokered custody removes refreshable state from sandboxes by
  construction, preserves D003 session semantics, and matches the shape Amp operates in production
  for linked ChatGPT subscriptions. The undocumented-upstream risk is accepted and its maintenance
  delegated to a widely used OSS project.
- **Reconsider when:** OpenAI publishes a supported individual-account non-interactive path
  (`chatgptAuthTokens`, `personalAccessToken`); the undocumented upstream breaks or its terms
  posture changes; Dorf becomes remote or multi-user (add OIDC-style sandbox identity); or the
  Droid-leg validation produces contradicting evidence.
