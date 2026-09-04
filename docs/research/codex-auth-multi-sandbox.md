# Codex Authentication for Multi-Sandbox Fan-Out — Research Notes

Non-normative research captured 2026-07-24. Supports issue-driven research into whether Dorf
can let a user sign in once with a ChatGPT subscription and then spawn many sandboxes, each running
its own real Codex app-server, without repeating login per sandbox.

**Outcome (2026-07-29):** decided. See
[D035](../project/decisions/D035-brokered-model-plane-authentication-credential-free-sandbox-images.md)
and [`model-auth-broker.md`](../history/model-auth-broker.md). This document remains the evidence
base.

## Two independent layers

Codex authentication questions conflate two layers that are actually decoupled:

1. **Control plane** — the app-server WebSocket protocol Dorf drives per sandbox (threads,
   turns, approvals, interruption, steering). This is local to each VM and is not affected by
   anything below. Dorf's D003 decision already fixes this to WebSocket.
2. **Model plane** — Codex's own outbound call to whichever backend serves the model, configured
   in `config.toml` via `model_provider` / `[model_providers.<id>]` (base URL, wire API,
   authentication). This is the layer any auth-delegation design actually touches.

Any solution to "log in once, spawn many sandboxes" only needs to change the model plane. The
control plane, and therefore full native app-server functionality, survives untouched regardless of
which model backend or auth mode is configured.

## Current documented auth options (Codex CLI / app-server)

| Option | Mechanism | Billing | Fits ephemeral multi-sandbox fan-out? |
| --- | --- | --- | --- |
| `codex login` (browser OAuth) | Interactive browser sign-in, cached in `~/.codex/auth.json` or OS keyring | ChatGPT subscription | No — one interactive login per machine |
| `codex login --device-auth` | Device-code flow, no local browser required (beta; can be enabled for personal accounts in ChatGPT security settings, or workspace-wide by an admin) | ChatGPT subscription | Poor — still needs one human approval per VM |
| `codex login --with-api-key` | Reads `OPENAI_API_KEY` from stdin | OpenAI Platform API (usage-based) | Good — key can be injected non-interactively per VM |
| `codex login --with-access-token` | Reads `CODEX_ACCESS_TOKEN` from stdin; token minted centrally in a ChatGPT **Business/Enterprise** workspace | ChatGPT workspace entitlement | Best fit *if* the workspace is Business/Enterprise-eligible; real non-interactive login, no proxy needed |
| Host-supplied ChatGPT tokens (`chatgptAuthTokens`) | Real app-server request (`account/login/start`, `type: "chatgptAuthTokens"`); confirmed in the shipped protocol schema — see below | ChatGPT subscription | Mechanism exists and matches the "login once, delegate to many" shape, but OpenAI's own schema marks it internal-only; do not build on it |

None of the individually-documented options give an **individual** ChatGPT Plus/Pro subscription a
non-interactive way to authenticate an unbounded number of freshly created sandboxes. That gap is
only closed for Business/Enterprise workspaces (access tokens) or by taking on undocumented-endpoint
risk (see below).

## Survey of existing "Codex CLI proxy" projects

Several public projects claim to let a ChatGPT subscription serve multiple clients. None solve the
sandbox-fan-out problem as designed:

- **`Securiteru/codex-openai-proxy`**, **`sheikhuzairhussain/codex-cursor-proxy`** — read the local
  `~/.codex/auth.json` access token and call the private `chatgpt.com/backend-api/codex/responses`
  endpoint directly, bypassing app-server entirely. Single already-authenticated host only; no
  credential delegation to new sandboxes.
- **`vkop007/codex-app-proxy`** — wraps one already-authenticated local `codex app-server` process as
  an OpenAI-compatible HTTP endpoint. Assumes the host already ran `codex login`; still 1:1, not a
  broker.
- **`router-for-me/CLIProxyAPI`** (44k+ stars, the well-known one) — a central Go server that does its
  own OAuth/device-code login (once, or for a small pool of accounts) and exposes an
  OpenAI/Claude/Gemini-compatible HTTP API that many clients can call with a locally-issued API key,
  never touching OpenAI credentials themselves. This is the closest real-world precedent to
  "login once, serve many consumers". Used as a *replacement* for app-server it would lose the
  native session semantics (threads/turns/approvals/interruption) that Dorf's D003 depends on —
  but see the 2026-07-29 experiment below: used as a *model-plane backend* behind a real per-sandbox
  app-server, D003 is preserved. It inherits the same undocumented-endpoint risk called out below.

## Confirmed: `chatgptAuthTokens` exists in the shipped app-server protocol

Generating the protocol schema locally against the installed CLI confirms the mechanism is real,
not speculative:

```
codex app-server generate-json-schema --out /tmp/codex-schema --experimental
```

(Tested against `codex-cli 0.138.0`, already logged in with ChatGPT on this machine.)

- `ClientRequest` defines `account/login/start` with a `type: "chatgptAuthTokens"` variant
  (`ChatgptAuthTokensLoginAccountParams`), requiring `accessToken` (JWT) and `chatgptAccountId`, with
  an optional `chatgptPlanType`.
- The `AuthMode` enum includes a matching `chatgptAuthTokens` value, meaning app-server can report
  this as the *active* auth mode once a host supplies tokens this way.
- `ServerRequest` defines the refresh round-trip, `account/chatgptAuthTokens/refresh`: when a
  backend call gets `401 Unauthorized`, Codex sends this request to the connected client and expects
  a response with a fresh `accessToken` + `chatgptAccountId` (+ optional `chatgptPlanType`). A
  comment in `ClientRequest` confirms the design intent explicitly: *"In external auth mode this flag
  is ignored. Clients should refresh tokens themselves and call `account/login/start` with
  `chatgptAuthTokens`."*
- Both the request variant and the `AuthMode` value carry the identical, verbatim doc-comment:
  `"[UNSTABLE] FOR OPENAI INTERNAL USE ONLY - DO NOT USE. The access token must contain the same
  scopes that Codex-managed ChatGPT auth tokens have."`

This is a real, functioning integration point OpenAI ships in every Codex CLI/app-server build, and
it matches exactly the "host owns the ChatGPT session, delegates short-lived tokens to many spawned
Codex instances" shape Dorf would want. It is also explicitly and repeatedly marked unstable and
internal-only in the same schema that defines it. Treat this as confirmed-to-exist, not
confirmed-to-be-usable: building product behavior on it means depending on an interface OpenAI has
stated, in its own generated docs, is not meant for external use and can change or disappear without
notice.

**New finding, not previously tracked:** the `AuthMode` enum also lists two modes with no
corresponding `account/login/start` request variant in this schema — `agentIdentity`
("Programmatic Codex auth backed by a registered Agent Identity") and `personalAccessToken`
("Programmatic Codex auth backed by a personal access token"). Neither can be initiated over the
app-server protocol itself, so they must be established some other way (config, CLI flag, or Codex
Cloud/agent registration not yet located). `personalAccessToken` in particular looks like it could be
an individual-account analog of the Business/Enterprise-only access tokens already covered above and
is worth a dedicated follow-up.

## The `model_providers` hook: an officially documented delegation point

Codex's config system explicitly supports pointing the model plane at a custom `base_url` with one
of three auth modes, independent of app-server:

- `requires_openai_auth = true` — Codex attaches its **own local** OpenAI auth to requests forwarded
  through the proxy. Does not remove the need for a local login.
- `env_key = "<VAR>"` — a provider-specific API key from a local environment variable.
- Neither set (or `[model_providers.<id>.auth]` with a command-backed token fetcher) — Codex sends
  requests with no attached credential and trusts the proxy/backend to authenticate.

The third mode is the useful one: a sandbox's real local `codex app-server` (full native
functionality) can be configured to call a central proxy with no credential at all, while the proxy
holds the only real credential and injects it when forwarding upstream.

- **For API-key billing**, this is fully legitimate today: one centrally-held OpenAI Platform API
  key, one proxy, many sandboxes each running a genuine local app-server with zero credential
  exposure. No ToS ambiguity — this is the same shape as standard LLM gateways (LiteLLM, Portkey).
- **For ChatGPT-subscription billing**, the mechanical hook is identical, but making the proxy's
  upstream call requires either (a) reimplementing the undocumented
  `chatgpt.com/backend-api/codex/responses` protocol — the same risk flagged for the proxy projects
  above — or (b) the experimental host-supplied-token contract, which OpenAI has not published as a
  supported third-party integration.

## Transport notes

- **Control plane** (Dorf ↔ app-server): always WebSocket (D003); unaffected by model-provider
  configuration.
- **Model plane** (Codex ↔ configured provider): Codex's own telemetry documents a WebSocket-first
  transport for the Responses API wire protocol, with an explicit HTTP fallback
  (`transport.fallback_to_http`, `codex.websocket_request`/`codex.websocket_event` metrics). A proxy
  implementation should support `wss://` for parity and `https://` (SSE) as the documented fallback.
  This transport behavior is documented for the general `model_providers` system; it is not verified
  against the private ChatGPT-subscription backend path, which is undocumented.

## Anti-patterns (confirmed, not just theorized)

- Copying `~/.codex/auth.json` into every sandbox: every clone gets a reusable personal credential;
  rotation/revocation become unmanageable; concurrent refresh behavior is unverified (see issue
  #117's Droid evidence of refresh-token rotation problems under concurrent clones).
- Calling `chatgpt.com/backend-api/codex/responses` directly: several public "CLI proxy" projects
  already do this in production, which shows it works today, but it depends on an undocumented,
  revocable, ToS-uncertain endpoint that OpenAI could change or block at any time.
- Sharing one mounted `CODEX_HOME` across concurrently active sandboxes: not documented as a safe
  concurrent-credential or distributed-history mechanism.

## Open questions for the research issue

- Is this Dorf account/workspace eligible for ChatGPT Business/Enterprise Codex access tokens?
  If yes, that is the cleanest, fully-supported path today.
- Is prototyping a `model_providers`-based proxy (API-key billing case) worth the engineering cost
  relative to injecting an API key per VM directly, given Dorf is currently single-user/local?
- Should Dorf track OpenAI's `chatgptAuthTokens` host-integration contract for future
  reconsideration once/if it becomes a published, supported integration path?
- What are `personalAccessToken` and `agentIdentity` (`AuthMode` values with no discovered
  `account/login/start` variant) and do either give an individual (non-Enterprise) account a
  supported non-interactive login path?

## Validation experiment: CLIProxyAPI as model-plane broker (2026-07-29)

Live experiment on the maintainer's machine. Setup: codex-cli 0.138.0; CLIProxyAPI v7.2.104
(pinned, SHA-256-verified against the release checksums); isolated `CODEX_HOME` containing only a
`config.toml` — no `auth.json`, no OpenAI login of any kind on the client side.

- **Login once, headless-capable.** CLIProxyAPI's `-codex-device-login` runs OpenAI's device-code
  flow with the same OAuth client as `codex login` (`client_id=app_EMoamEEZ73f0CkXaXp7hrann`,
  `auth.openai.com/codex/device`). The token bundle lives only in the broker's `auth-dir`; the
  broker refreshes it on a 15-minute timer as the single writer.
- **Wire parity confirmed.** Client config: `model_provider` with
  `base_url = "http://127.0.0.1:8317/v1"`, `wire_api = "responses"`, and `env_key` naming a
  broker-issued key. `codex exec` works with zero local OpenAI auth: correct responses, token usage
  reported, and `model_reasoning_effort=high` verified on an arithmetic probe. Provider debug shows
  `requires_openai_auth: false`.
- **No transport problem either way.** Custom providers default to `supports_websockets: false`,
  so codex uses plain HTTP/SSE `POST /v1/responses`; the WebSocket-first concern in "Transport
  notes" does not apply to custom providers. Verified with codex-cli 0.146.0 that opting in via
  `supports_websockets = true` on the provider also works end-to-end through the broker
  (Responses-over-WebSocket upgrade on `GET /v1/responses`; broker logs
  `responses websocket: client connected` / `upstream execution session closed`). Note:
  `wire_api = "chat"` is removed in codex-cli 0.138.0; Responses-only matches the broker's
  endpoint.
- **No model mapping needed.** codex 0.138.0 defaults to `gpt-5.5` and 0.146.0 defaults to
  `gpt-5.6-sol`; both are served by the broker. 10 subscription models are exposed in total
  (gpt-5.4, gpt-5.4-mini, gpt-5.5, gpt-5.6-luna/-sol/-terra, gpt-5.3-codex-spark, codex-auto-review,
  gpt-image-1.5, gpt-image-2). Explicit `model =` overrides work.
- **Stray calls identified and proven harmless.** Even with a custom provider and no local auth,
  codex makes unauthenticated metadata calls to chatgpt.com domains: `backend-api/plugins/featured`
  (fails 401, logs a WARN, continues), `ab.chatgpt.com` feature flags, and a remote models-catalog
  refresh (`online_if_uncached` with cache fallback). Running codex under a macOS `sandbox-exec`
  profile denying all egress except `localhost:8317` still completes the task, so broker-only
  egress policies in Dorf sandboxes are safe for the Codex leg.
- **Per-sandbox key scoping works.** The broker rejects keyless calls (401) and supports multiple
  local API keys, so Dorf can issue one scoped broker key per sandbox for revocation and audit.
  These keys contain no OpenAI material; a leaked sandbox key cannot reach the ChatGPT account
  directly.

Consequences for D008 and issue #117:

- The secret-bearing Incus image can be retired for the Codex leg: nothing refreshable enters any
  sandbox, so the concurrent-clone invalidation observed with Droid in #112/#114 cannot occur.
- Residual accepted risk: the broker's upstream remains the undocumented
  `chatgpt.com/backend-api/codex/responses` path, but its maintenance burden moves from Dorf to
  a widely used OSS project (45k+ stars, multiple releases per week). Exit ramps remain
  Business/Enterprise access tokens and a possible future official `chatgptAuthTokens` /
  `personalAccessToken` path for individual accounts.

## References

- [Codex authentication](https://learn.chatgpt.com/docs/auth)
- [Codex advanced configuration — custom model providers](https://learn.chatgpt.com/docs/config-file/config-advanced)
- [Codex access tokens](https://learn.chatgpt.com/docs/enterprise/access-tokens)
- [`router-for-me/CLIProxyAPI`](https://github.com/router-for-me/CLIProxyAPI)
- [`Securiteru/codex-openai-proxy`](https://github.com/Securiteru/codex-openai-proxy)
- [`vkop007/codex-app-proxy`](https://github.com/vkop007/codex-app-proxy)
- [`sheikhuzairhussain/codex-cursor-proxy`](https://github.com/sheikhuzairhussain/codex-cursor-proxy)
- Related: issue #117 (concurrent authentication for cloned Incus sessions), decision D008 (local
  authenticated Incus image for ChatGPT subscription)
