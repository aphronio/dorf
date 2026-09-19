# Native Session contract: capability review

Status: native Session API and coordinated client replacement implemented in slice 9 on
2026-09-19. This document owns implementation evidence and limits for
[D148](../project/decisions/D148-thin-native-session-control-plane.md).
The [API contract](../control-api.md) owns current behavior; the
[tracker](session-product-proposals.md) owns sequencing.

## Responsibility and proposed surface

```text
Client -- input / controls / history / events --> Dorf Session API
                                                 |
                  Session configuration          | adapter
                  native Thread binding          v
                  resources and access       Native Harness
                  lifecycle receipts             |
                                           messages and Turns
                                           tools and execution
                                           saved conversation
```

Persist Session and infrastructure facts in Dorf. Read messages, items, and Turns from the Harness.
Do not introduce a Thread table, shared Turn table, durable input queue, or generic operation engine
just to wrap the native protocol. Keep authentication, exact ownership, maintenance gates, and
resource release around every exposed operation; this is not unrestricted JSON-RPC forwarding.

Use Session **events** for submitted input and controls as well as native execution notifications.
Input is an event type, not a separate `input` resource. Turns and saved items remain native read
views. The [OpenAPI document](../../internal/controlapi/openapi.json) defines the event types and wire schemas.

Event vocabulary does not require new streaming work. Dorf already exposes SSE for Session
snapshots and Turn observations containing completed native items and status; see the
[current API](../control-api.md). These are not token-by-token text streams. This scope clarification
does not authorize removing existing streaming support. The existing observation path now uses native Turn identity, and ordinary native Turn/history reads remain available. Any expanded
streaming surface needs a supported delivery and reconnect contract. Event vocabulary alone does
not promise a replayable event collection or require Dorf to retain an event log.

The following describes responsibilities, not final endpoint paths or generated schemas:

| Client operation | Native responsibility | Dorf responsibility |
| --- | --- | --- |
| Create / inspect Session | Create or reopen its native conversation when ready | Configuration, provisioning, exact binding, readiness |
| Send input | Decide start versus steer and acknowledge acceptance | Validate supported input, attest the Session, submit once, expose native result or uncertainty |
| Read history / inspect Turn | Return saved items and native status | Bounded access through the Session binding; expose unavailable history honestly |
| Observe | Produce native events and expose execution/history views | Authenticated native reads and adaptation of existing SSE; no new token-streaming or durable event-log promise |
| Interrupt current execution | Validate and stop the selected native execution | Resolve a fresh native target for that call; do not retry against a successor |
| Configure / refresh | Interpret native settings, tools and skills | Enforce admitted settings and credential ownership |
| Files / commands | Optional native file/process transport | Keep existing path, size, ownership and unknown-command guarantees |
| Recover / release | Supply readable state or stop native processes | Backup compatibility, resource generations, fencing, revocation and cleanup |

Session create remains a lifecycle operation; ordinary input is not an Absurd task. Its response
must distinguish confirmed native acceptance from definite rejection and unknown outcome. A
successful acknowledgement is not a completion result or a promise of persistence before a crash.
Unavailable or maintenance-held Sessions do not accept input for later delivery.

## Pinned Codex evidence

Reviewed upstream tag `rust-v0.154.0`, commit `36eab01061df3cde5f95ec20a526777b430091ba`, matching
the installed proof binary. A refreshed main checkout was used for discovery only; the statements
below were checked against the pinned tag. This is not a claim that every existing deployment has
the same version. Package selection remains in the [package manifest](../../scripts/sandbox/packages/packages.json).

| Surface | Evidence and implication |
| --- | --- |
| `thread/start`, `thread/resume` | Native conversation lifecycle. Separate binding establishment from input submission; prove initial creation/restart before deleting the old first-Message binding path. |
| `turn/start` | Calls native `start_or_steer_turn`; returns the selected Turn ID for either outcome. Ordinary send needs no Dorf read-active-then-select-Follow/Steer loop. |
| `turn/steer` | Has an explicit `expectedTurnId` precondition. It is a different operation, unnecessary for ordinary native start-or-steer input. |
| `turn/interrupt` | Accepts Thread and Turn IDs and validates the target. An adapter can hide target lookup from ordinary clients without inventing retargeting/retry semantics. |
| `clientUserMessageId` | Carries input attribution. Repeating it is not deduplication; do not advertise the retired Message same-key replay guarantee on this basis. |
| `thread/read`, `thread/turns/list`, `thread/items/list` | Native history and execution views. Paginated APIs and storage modes have version-specific constraints; use the already verified timeline path before adding another one. |
| Turn/item/status notifications | Native live observations. A reconnect needs history reconciliation where supported, not replay of a Dorf Message. Connection and process lifetime need their own proof. |
| `turn/start.toolOutput` | Supports standalone application tool output and start-or-steer placement. It cannot be combined with nonempty user input. Its attribution is not the user-message correlation field. |
| `skills/list`, `config/mcpServer/reload`, configuration reads/writes | Existing native capabilities to evaluate for explicit refresh/configuration. They do not justify a universal configuration language. |
| `fs/*`, `command/exec` and command controls | Potential transport reuse. Do not replace existing workspace operations before comparing bounds, process custody and uncertain effects. |
| Experimental `thread/queue/*` | Native queued input is a distinct capability with a service dependency. Ordinary `turn/start` does not imply durable queuing. No queue API is adopted by this decision. |

Source: [turn request processor][turn-processor], [input parameter types][turn-types],
[native input decision][native-input], [registered protocol operations][protocol],
[queue processor][queue-processor].

The native input decision explicitly replies before prompt hooks, model-context updates, rollout
persistence, or sampling finish. Therefore successful native acceptance cannot establish a durable
history write. Absence from history, especially during active work, is not proof of nonacceptance.
A timeout or server error alone is likewise not proof that nothing was submitted.

The public [Codex app-server overview](https://learn.chatgpt.com/docs/app-server#api-overview)
is a useful inventory; version-pinned source and native proofs settle behavior for the admitted
profile. Do not infer an idle-only start from the method name `turn/start`.

## Native probe

Run [the isolated input probe](../../scripts/codex/input-contract.py):

```bash
python scripts/codex/input-contract.py --codex codex
```

It creates temporary native state, starts a disposable real app-server, and uses a local synthetic
Responses server. No model credentials, external inference, live Sessions, or provider resources
are used. The local server holds model output to make active input and process-loss cases repeatable.

Observed on Codex 0.154.0:

- An idle send starts a Turn; a second `turn/start` while it is active returns the same Turn ID.
- The second input appears in saved conversation history after processing.
- Repeating the original input and `clientUserMessageId` after completion starts another Turn.
- Completed history remains readable after killing and restarting the app-server.
- Acknowledged input submitted while the model response is held is absent from history both before
  and after the subsequent process kill. The earlier completed history remains readable.

This proves native start-or-steer and demonstrates the limits of acknowledgement and correlation.
It is not a provider recovery, filesystem durability, live model, attachment, instruction-refresh,
WebSocket disconnect, or complete streaming proof. The last case must not be generalized into a
claim that every acknowledged input is lost on restart; it demonstrates a real unpersisted window.

## OpenAI managed Agents API as a reference

The managed API exposes Session input events, saved items, and Turns; OpenAI runs the Harness that
maintains them. Input to an idle Session starts work; input during active work steers it. Clients
retrieve saved items after reconnecting because streams do not replay missed events.
See [architecture](https://developers.openai.com/api/docs/guides/agents-api/architecture),
[Session input](https://developers.openai.com/api/docs/guides/agents-api/sessions), and
[events and items](https://developers.openai.com/api/docs/guides/agents-api/sessions/events).

Adopt the responsibility boundary and simple client vocabulary. Those documents do not reveal the
service's internal storage or prove the same acknowledgement, retry, restart, or retention guarantees
for a self-hosted Codex app-server. The two products are not interchangeable protocol authorities.

## Implementation and verification

The client now submits a Session event once, stores the returned native binding, and observes that
Turn. Several application inputs can bind to one Turn; application reply publication retains one
owner and its own durable deduplication. Uncertain submission is inspected using positive native
correlation evidence; missing history never authorizes another send. Private client implementation
and operational details remain outside this public repository.

Dorf retains a monotonic native mutation revision and, while unresolved, one input dispatch marker
or exact interrupt target. These contain no payload or transcript. The marker is written before
native mutation, uses a fresh dispatch suffix even when the caller repeats a correlation ID, and
blocks subsequent writes and maintenance until acknowledgement or exact positive native evidence.
An empty Thread may be replaced only before any native mutation has begun. Pause, upgrade and
checkpoint safety use this guard and native quiescence. Release retains resource custody.

Checkpoint capture and upgrade quiescence write a bounded workspace manifest of settled native
Thread/Turn identities and statuses. Restore checks that proof against native history. PostgreSQL
retains the checkpoint's native revision. The migration retires pre-contract checkpoint references
and completed recovery receipts; it refuses unfinished recovery and leaves remote backup objects
untouched. No compatibility flag or automatic import remains. No accepted input is reconstructed
or replayed from a backup.

Verification covers:

- PostgreSQL migration/replay, concurrent mutation exclusion, stale completion, initial binding,
  maintenance holds and closed admission.
- Native protocol input, images, instructions, refresh, exact cancellation, observation handoff
  after caller disconnect, and positive-evidence reconciliation of a lost acknowledgement.
- Workspace continuity manifest capture/read, malformed or mismatched proof rejection, and native
  settled-history comparison.
- API acknowledgement and uncertainty responses, correlation without deduplication, native history,
  completed-item SSE, bounded attachments, and public client changes.
- The isolated real Codex probe above was rerun on 2026-09-19 and passed, including start-or-steer,
  repeated correlation IDs, completed-history restart and the acknowledged-but-unpersisted case.

Live verification on 2026-09-19 exercised the native Session boundary:

- The coordinated 0.18.1 migration preserved retained Session identity, Thread binding and resource
  custody while moving lifecycle execution to the Session queue.
- Disposable Incus and E2B Sessions accepted file and native image attachments, exposed completed-item
  SSE, continued the same Thread, and completed cleanup. E2B idle pause and resume also passed.
- `integration:upgrade-worker` passed on Incus and E2B: activation, deliberately failed verification,
  rollback, original conversation context, substantive replies, and resource/checkpoint cleanup.
  The E2B rollback adopted a different provider VM. These coordinator proofs used a deterministic
  model fixture and real native Harnesses and providers.
- The deployed E2B checkpoint path published a backup, restored onto a different VM, verified native
  history, deleted the source, and continued the same Thread through the real model route. The
  restored workspace file matched exactly. After a newer input, recovery from the older checkpoint
  reported attention before creating or verifying a destination. Cleanup completed for the successful
  replacement and for the held stale-recovery request.

The current checkpoint proof exercises operator-requested replacement with its source initially
available. Source loss, lost verification acknowledgements, storage cancellation and cross-session
storage isolation were covered by earlier prototype proofs and were not rerun in this pass.
Deployment-specific identities and receipts remain private.

The old Message/AgentRun schema, delivery controller, queue wakes, multipart payload store,
Follow/Steer selection and public aliases are removed. Published migrations remain intact.
The cleanup also removes retired Pi input execution, Message-shaped consumer fixtures, obsolete
Session execution states, and test adapters that reconstructed AgentRun-era bindings. Queue names,
wake keys and payloads use Session vocabulary; the one-time migration reattaches idle Sessions’ sleeping lifecycle tasks. Outstanding input and
failed or unfinished lifecycle effects must settle before a coordinated worker restart. Existing provider ownership labels still identify retained
compute and are not renamed by a queue transition.

The lifecycle queue and provider transports remain; a scheduler rewrite or guest daemon is not a
prerequisite for native input.

[turn-processor]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/app-server/src/request_processors/turn_processor.rs
[turn-types]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/app-server-protocol/src/protocol/v2/turn.rs
[native-input]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/core/src/session/turn_input.rs
[protocol]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/app-server-protocol/src/protocol/common.rs
[queue-processor]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/app-server/src/request_processors/thread_queue_processor.rs
