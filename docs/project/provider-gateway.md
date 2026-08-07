# Shared Provider Gateway

- **Status:** Accepted direction; ChatGPT-to-Codex Room path validated
- **First validated route:** D035, ChatGPT subscription to Codex app-server through CLIProxyAPI
- **Implemented control plane:** named ChatGPT-subscription and OpenAI-Platform connections plus
  scoped Responses routes; the Go Job spine reconciles per-Sandbox routes directly

The Provider Gateway connects upstream model providers and issues scoped routes to trusted host
clients and isolated Rooms. It is a sibling application subsystem rather than part of the portable
Worker and Job runtime.

## Vocabulary

| Concept | Meaning |
| --- | --- |
| **Provider Gateway** | Application subsystem for connection, route, health, and broker lifecycle operations |
| **Provider Connection** | Durable upstream authentication profile, such as a ChatGPT subscription or OpenAI API key |
| **Inference Route** | Revocable downstream endpoint and broker credential issued to one consumer |
| **Broker backend** | Persistent data-plane process that holds upstream credentials, refreshes them, and forwards inference |

CLIProxyAPI is the first concrete broker backend and remains an implementation detail. The
subsystem lives beside, not inside, the durable runtime. The retained Python connection setup is
legacy operational evidence; the issue #40 Go path reads that same protected authority and
reconciles only its consumer-specific route without starting Python.

## Boundary and data flow

```text
                         one provider connection
                    "connect my ChatGPT account"
                                │
                                ▼
┌──────────────────── ProviderGateway facade ────────────────────┐
│ connect · disconnect · list · status · create route · revoke  │
└────────────────┬─────────────────────────────┬─────────────────┘
                 │                             │
          trusted host clients          composed by Dorf
                 │                             │
                 ▼                             ▼
        client inference routes         per-Room inference routes
                 │                             │
                 └──────────────┬──────────────┘
                                ▼
                    supervised broker daemon
                     sole upstream credential
                         and refresh owner
                                │
                                ▼
                         model provider
```

The Python facade is the control plane. It never becomes an inference proxy or transcript owner.
The supervised daemon is the model data plane. Consumers send inference directly to their routes;
Dorf controls Worker, Room, Job, Assignment, and native-turn lifecycle through its SDK and adapters.

## Chosen

1. **One authority per deployment profile.** Connecting through the Dorf CLI or application facade
   reaches the same gateway state and broker. Upstream credentials are never cloned into Rooms.
2. **Programmatic first, CLI as an adapter.** Connection and route operations share one protected
   broker authority rather than maintaining client-specific stores. The greenfield Go Job path
   composes route operations directly; retained Python connection commands are not its runtime.
3. **Connections and routes are distinct.** A Provider Connection owns upstream authentication.
   Each consumer receives a separate Inference Route so it can be revoked independently. A Room
   route ends with Room cleanup.
4. **Concrete broker, isolated implementation detail.** The current backend is a pinned, supervised
   CLIProxyAPI daemon. Raw daemon names, endpoints, and errors do not cross the user-facing boundary.
5. **Runtime remains provider-blind.** `dorf.runtime` owns durable Worker and Job mechanisms. Provider
   registries, selection policy, quota scheduling, and fallback do not enter the runtime.
6. **Explicit selection, no implicit pooling.** A caller chooses a named Provider Connection and
   compatible model. Sharing one subscription shares its quota; the gateway does not imply extra
   capacity, automatic failover, or cheapest-model routing.
7. **Local first.** Host-only consumers may use loopback. Room composition binds the broker to the
   selected private Incus bridge, reachable from the host and attached Rooms but never from a
   wildcard or LAN address. Remote exposure requires a private authenticated transport and concrete
   deployment evidence.
8. **Provider support is evidence-backed.** Every provider, authentication mode, and consumer wire
   dialect must pass login, refresh, streaming, model-selection, failure, and concurrency validation
   before Dorf advertises support.

## Current vertical slice

```text
ChatGPT subscription or OpenAI API key
                  │
                  ▼
       pinned CLIProxyAPI daemon
                  │
                  ▼
         Codex Room routes
                  │
                  ▼
        real codex app-server
```

D035 validates the Codex Responses path. One unprefixed implementation connection may coexist
with one `deepseek/`-prefixed scoped client connection. CLIProxyAPI's `force-model-prefix` setting
keeps prefixed credentials out of unprefixed requests; ambiguous duplicates still fail closed.
Route keys remain broker credentials rather than provider-specific allowlists.

## Failure and security semantics

- Upstream OAuth bundles and provider API keys stay in protected host state.
- Inference Routes contain broker-local credentials, not upstream provider credentials.
- Missing or stale authentication fails before Room provisioning and carries typed remediation.
- Route rejection, upstream-auth failure, upstream unavailability, and broker unavailability remain
  distinct typed outcomes.
- Room cleanup is incomplete until its route is revoked or revocation remains observably retryable.
- Logs, exceptions, and status output never contain upstream or route credentials.

## Reconsider when

- A second validated broker backend needs a smaller common facade.
- A real remote Room requires an authenticated network authority.
- Provider-specific wire behavior cannot be represented honestly as a connection plus route.
- Multiple accounts or subscription limits create an observed need for pooling, quotas, fallback,
  or scheduling.
