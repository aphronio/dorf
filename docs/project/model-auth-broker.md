# Brokered Model Authentication for Sandboxes

- **Status:** Accepted 2026-07-29 (decision D035; supersedes D008 for the Codex leg)
- **Resolves:** the Codex half of [#117](https://github.com/aphronio/dorf/issues/117)
- **Evidence:** `docs/research/codex-auth-multi-sandbox.md`, section "Validation experiment:
  CLIProxyAPI as model-plane broker (2026-07-29)", plus the #117 comment thread
- **Shared boundary:** `docs/project/provider-gateway.md` and decision D036

This page separates what is chosen from what was rejected with evidence and what remains parked
research. Implementation work is tracked in GitHub issues, not here.

## Chosen

One host-side Provider Gateway broker per user machine (pinned, vendored CLIProxyAPI) holds the only
ChatGPT OAuth bundle and is the single writer for token refresh. Every sandbox runs a real
`codex app-server` (D003 unchanged) whose *model plane* points at the broker. Nothing refreshable
and no OpenAI credential ever enters a sandbox. D036 names the shared programmatic facade,
connection, and route boundary used by Dorf and trusted host clients; this document remains the
evidence-backed ChatGPT-to-Codex slice.

```
browser device login (once)
        │
        ▼
┌─────────────────────────── host ───────────────────────────┐
│  Provider Gateway broker (CLIProxyAPI, pinned version)     │
│  - sole holder of ChatGPT OAuth bundle, single-writer      │
│    refresh                                                 │
│  - serves the private Incus bridge to host + Rooms         │
│  - issues per-sandbox scoped keys; startup auth probe      │
│        ▲                    ▲                              │
│        │ Responses/SSE      │ Responses/SSE or WebSocket   │
│   sandbox A (real      sandbox B (real                     │
│   codex app-server,    codex app-server,                   │
│   scoped key A)        scoped key B)                       │
└────────────────────────────────────────────────────────────┘
                     │ broker only
                     ▼
     chatgpt.com/backend-api/codex/responses
```

Concrete choices:

1. **Broker custody.** The OAuth bundle lives only in the broker's auth dir. The broker refreshes
   on a timer as the sole writer, so concurrent sandboxes cannot invalidate each other (the failure
   observed with cloned Droid state in #112/#114). Dorf retains the broker-issued auth filename
   unchanged and uses CLIProxyAPI's authenticated management API to enable capabilities or delete
   the bundle; it does not rename, rewrite, chmod, or unlink the OAuth file directly.
2. **Sandbox wiring.** Per sandbox, provisioning writes `config.toml` with a custom
   `model_provider` (`base_url`, `env_key`, `wire_api = "responses"`) and injects one scoped
   broker key as the named env var. `requires_openai_auth` stays false. Responses WebSockets are
   enabled only when the broker reports `websockets = true` for the selected ChatGPT auth record;
   otherwise the Room retains HTTP/SSE without a failed WebSocket attempt. Sandboxes carry no
   `auth.json` and no OpenAI material; a leaked scoped key cannot reach the ChatGPT account.
3. **Credential-free images.** D008's secret-bearing Incus image is retired for the Codex leg.
4. **Broker-only egress.** Sandboxes may be network-restricted to the broker alone; validated that
   codex completes work under a deny-all-except-broker sandbox profile. The Incus-side enforcement
   mechanism is implementation work.
5. **One-time device-code login.** Headless-capable (no localhost-callback coupling), same OAuth
   client as `codex login`. The resource-oriented UX is
   `dorf provider connect chatgpt --subscription`; trusted applications may call the same
   programmatic facade.
6. **Billing-mode-agnostic route.** Identical sandbox wiring whether the selected Provider
   Connection uses a ChatGPT subscription or an OpenAI Platform API key; the choice is made when
   connecting the provider rather than during Room provisioning.
7. **Explicit failure vocabulary.** Startup auth probe per sandbox; stale auth fails fast as
   `needs-human` with a reconnect remediation naming the Provider Connection. Broker errors are
   translated into Dorf vocabulary; the string "CLIProxyAPI" never reaches users.
8. **Pin and deliberate upgrades.** The broker's upstream is an undocumented endpoint; track the
   vendored project's releases deliberately rather than auto-updating.

## Validated properties (2026-07-29 experiment)

Each was exercised live against codex-cli 0.138.0 and 0.146.0 with an isolated, login-free
`CODEX_HOME`:

- Wire parity: correct responses, token-usage reporting, high reasoning effort, streaming.
- Transport: HTTP/SSE by default; Responses-over-WebSocket after `supports_websockets = true`
  (0.146.0), both through the broker.
- Model mapping: none needed; codex defaults (`gpt-5.5` on 0.138.0, `gpt-5.6-sol` on 0.146.0) are
  served; 10 subscription models exposed; explicit `model =` overrides work.
- Stray calls: three unauthenticated metadata calls to chatgpt.com (featured plugins, feature
  flags, models catalog) all degrade gracefully; task completion unaffected under broker-only
  egress.
- Broker auth model: keyless calls rejected (401); multiple local keys supported, enabling
  per-sandbox scoping.

## Rejected with evidence

- **Copying `~/.codex/auth.json` into sandboxes / cloning secret-bearing images.** Concurrent
  clones race on refresh-token rotation; observed invalidation of sibling sessions (#117, from
  #112/#114 Droid dogfood).
- **Sharing one mounted `CODEX_HOME` across concurrent sandboxes.** Not documented as a safe
  concurrent-credential or distributed-history mechanism.
- **Reimplementing the `chatgpt.com/backend-api/codex/responses` upstream ourselves.** The
  maintenance burden of an undocumented, moving protocol now sits with a 45k-star OSS project
  shipping multiple releases per week instead of with Dorf.
- **CLIProxyAPI as an app-server replacement.** That role would lose D003's native session
  semantics (threads, turns, approvals, interruption). It is used only as a model-plane backend
  behind a real per-sandbox app-server.

## Not chosen (parked research, with revisit triggers)

- **`chatgptAuthTokens` host token vending.** Confirmed present in the shipped app-server schema
  but marked `[UNSTABLE] FOR OPENAI INTERNAL USE ONLY`. Revisit if OpenAI publishes it as a
  supported integration; it would then be the cleanest path (no protocol reimplementation at all).
- **`personalAccessToken` / `agentIdentity` auth modes.** Present in the schema without a protocol
  entry point; investigate if an official individual-account non-interactive path appears.
- **Business/Enterprise access tokens.** Fully supported by OpenAI today; add when a business
  workspace user appears. Not a fit for individual Plus/Pro subscribers.
- **OIDC-style workload identity for sandbox-to-broker auth** (the pattern Amp uses for its orbs).
  Phase-2 hardening; static scoped keys are sufficient inside the current local single-user trust
  boundary. Revisit when Dorf becomes remote or multi-user.
- **Multi-account pooling and quota scheduling.** The broker supports it; excluded from the default
  UX until single-subscription rate limits actually bite.
- **Droid, other agents, and other providers.** The shared Provider Gateway is the intended
  boundary, but each combination needs its own validation pass: Codex evidence does not prove Droid,
  Kimi Code, Grok, OpenRouter, or another consumer/provider dialect. See D036 for the capability
  posture.

## Accepted residual risk

The broker's upstream remains the undocumented `chatgpt.com/backend-api/codex/responses` path, and
OpenAI may change or block it. This is the same risk class Amp accepted commercially for its
linked-subscription feature. Mitigation: pinned broker version, explicit failure vocabulary, and
the exit ramps above (Enterprise tokens; future official individual paths). If the upstream breaks,
users fall back to API-key billing with zero sandbox-side changes.
