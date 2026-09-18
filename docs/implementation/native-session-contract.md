# Native Session contract: capability review

Status: boundary agreed; Codex source review and isolated native input probe completed on
2026-09-18. Runtime/API replacement remains a separate slice. This document owns the implementation
evidence and open details for [D148](../project/decisions/D148-thin-native-session-control-plane.md),
not the currently shipped API. The [tracker](session-product-proposals.md) owns sequencing.

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

The following describes responsibilities, not final endpoint names or generated schemas:

| Client operation | Native responsibility | Dorf responsibility |
| --- | --- | --- |
| Create / inspect Session | Create or reopen its native conversation when ready | Configuration, provisioning, exact binding, readiness |
| Send input | Decide start versus steer and acknowledge acceptance | Validate supported input, attest the Session, submit once, expose native result or uncertainty |
| Read history / inspect Turn | Return saved items and native status | Bounded access through the Session binding; expose unavailable history honestly |
| Observe | Produce native events | Authenticated stream and documented reconnect behavior; no durable event-log promise |
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
| `clientUserMessageId` | Carries input attribution. Repeating it is not deduplication; do not advertise Dorf's current same-key replay guarantee on this basis. |
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

## Client change and remaining proof

The inspected client already retains application input and prepares workspace capabilities before
submission. Its conversation and task paths nevertheless depend on Dorf Message IDs, same-key
replay, effective Follow/Steer intent, Message-specific observation cursors, and Message-targeted
interruption. Replacing those uses is part of the API slice, not a drop-in server change.
Consumer-specific source paths and operational details remain outside this public repository.

The expected simplification is one Session input path and native execution/history observation.
Remove effective-intent reconciliation, Message-to-Turn discovery, duplicate reply ownership based
on Follow versus Steer, and retries justified only by Dorf's old Message idempotency contract.
Application publication and its own delivery deduplication still belong to the client.

Before replacing the path, prove these concrete obligations:

1. **Initial binding:** establish one Thread without unsolicited model work and account for loss
   between native creation, local binding and first input. Never create another conversation merely
   because a send acknowledgement was lost.
2. **Send and uncertainty:** concurrent native input, explicit rejection, response loss, correlation
   visibility, and no automatic resubmission. Do not add a permanent payload store to preserve a
   retired promise. Any small retained uncertainty fact must have a concrete lifecycle purpose.
3. **Observation and interruption:** live and cold history, reconnect gaps, native ID stability,
   exact cancellation races, and work continuing after the public client disconnects. An ephemeral
   observation cache must not become a second outcome authority.
4. **Existing input capabilities:** attachments, application tool output, developer instructions,
   workspace instructions and skill refresh. Map each to a supported native capability or explicitly
   retire it with its consumer; do not silently change its authority or defer it to a later Turn.
5. **Maintenance and release:** stop accepting native writes under the existing fence before
   pause, upgrade, restore or cleanup. Native idle status alone does not prove no unresolved call
   exists. Checkpoint safety currently uses Message sequences; replace that cutoff before deleting
   the ledger. Native history and external tool effects cannot be recovered by replaying old input.

Implement through one coordinated public/API/adapter/client path, then delete the old Message
admission, FIFO/Auto selection, AgentRun outcome propagation, delivery wakes, dedicated schemas and
tests of retired semantics. Preserve published migrations. Do not keep two production execution
paths or aliases for hypothetical compatibility. Keep lifecycle orchestration until evidence shows
which parts become unnecessary; a queue rewrite or guest daemon is not a prerequisite.

[turn-processor]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/app-server/src/request_processors/turn_processor.rs
[turn-types]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/app-server-protocol/src/protocol/v2/turn.rs
[native-input]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/core/src/session/turn_input.rs
[protocol]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/app-server-protocol/src/protocol/common.rs
[queue-processor]: https://github.com/openai/codex/blob/36eab01061df3cde5f95ec20a526777b430091ba/codex-rs/app-server/src/request_processors/thread_queue_processor.rs
