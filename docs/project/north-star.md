# Dorf North Star

**Your agents. Your infrastructure. One API.**

Dorf is a stateful, self-hostable control plane for supported native agent Harnesses on compatible
isolated infrastructure. It provides a stable client API and manages the configuration, compute,
access, and lifecycle needed to use those Harnesses. The Harness owns the conversation and execution.

This is the accepted direction in [D148](decisions/D148-thin-native-session-control-plane.md).
The [Remote Control API](../control-api.md) defines the native events contract. The
[slice tracker](../implementation/session-product-proposals.md) records implementation and deferred work.

## Product boundary

Clients choose goals, supply instructions and tool configuration, prepare application files,
interpret results, compose independent execution contexts, and decide when to release resources.
Coding, repository selection, review policy, publication, credentials for application services,
and business outcomes belong to the client.

Dorf owns Session identity and configuration, the selected native Thread binding, attested compute,
scoped model access, readiness and maintenance gates, supported recovery, and requested release.
It exposes supported input, controls, events, and history through adapters. A stable API does not
require another database copy of everything that API exposes.

Harnesses own native input placement, messages, tool calls, Turns, conversation history, and agent
execution. Dorf does not schedule Follow/Steer/Auto deliveries or promise an offline input inbox in
the target contract. Adapters translate native capabilities; they do not rebuild a Harness to make
unsupported semantics appear portable. Start with the verified Codex surface. Further Harnesses
must earn support through concrete adapter proofs.

Apply this test before retaining a concept: which authority owns the fact, and which actual
control-plane obligation requires Dorf to persist it? Resource custody and uncertain lifecycle
effects qualify. Merely returning a native message or Turn through the API does not.

## Vocabulary

| Term | Meaning |
| --- | --- |
| **Session** | Dorf's durable handle for admitted configuration, a native Thread binding, owned resources, and lifecycle |
| **Sandbox** | An isolated mutable workstation with exact resource ownership |
| **Harness** | Native software hosting an agent and its conversation, such as Codex app-server |
| **Thread** | The native continuing conversation; one input Thread is bound to each Session today |
| **Message / item** | Native conversation content exposed through the adapter, not an additional Dorf aggregate |
| **Turn** | A native execution whose identity and status can be observed without a Dorf Turn table |
| **Action** | A fixed infrastructure lifecycle effect with stable identity and reconciliation |

Native subagent threads remain Harness-owned. Additional independently addressable Threads need a
real client use case; no Thread collection or multi-Harness dispatch is added now.

## Input semantics

A successful send means the Harness acknowledged acceptance. It does not mean a Dorf queue retained
the payload, a native history write finished, a Turn completed, or a backup captured the input.
If the environment is preparing, unavailable, or held for maintenance, Dorf cannot accept input
for later delivery. Clients retain unsent work and choose when to attempt a new submission.

If an acknowledgement is lost, inspect native evidence where supported or expose an unknown
outcome. Never infer nonacceptance from a missing history item or replay an ambiguous mutation
solely because a request has an ID. A native correlation ID is not necessarily a deduplication key.
The [native capability review](../implementation/native-session-contract.md) owns version-specific
acceptance, persistence, and retry evidence.

Controls act through the Harness. Ordinary input needs no caller-selected Turn; interruption must
still avoid affecting a successor through a delayed or repeated request. The public operation names
and their exact error and retry shapes are settled in the implementing slice.

## Workflow examples

An external client creates a Session, prepares its workspace, waits for readiness, and submits
input. It observes native events and retrieves native history after reconnecting. It chooses the
next task and release timing. Application-specific input queues, reply publication, review evidence,
and success criteria remain with that client.

## Desired experience

- Supported agent setups run on verified owner-selected infrastructure.
- Client disconnection does not itself release compute or terminate accepted native work.
- Input acceptance, native execution, availability, and recovery limits are distinguishable.
- Clients inspect native work through a stable API without reconstructing Dorf delivery states.
- Model and provider credentials remain behind their defined authority boundaries.
- Release follows an explicit client request; idle does not mean the goal is complete.

## Layers and ownership

```text
Client        Goals, application setup, unsent work, evaluation, release timing
Dorf          Stable API, Session binding, resources, access, recovery, cleanup
Adapters      Verified translation of native and provider operations
Harness       Input placement, conversation history, execution, tools, subagents
```

## Non-goals until evidence demands them

Dorf is not a workflow engine, agent builder, durable message broker, transcript replacement,
application evidence store, skills marketplace, or universal compatibility layer. A profile is a
verified combination, not a promise that every provider supports every Harness or recovery mode.
Native queuing, where available, is a distinct optional capability; it is not assumed by ordinary send.

## Proof that the North Star is real

A client can configure a Session, submit input to its ready Harness, disconnect, and later inspect
native work. Dorf preserves resource ownership and reports unknown outcomes without duplicate
submission. Native history availability follows the supported storage and recovery contract;
release does not imply a retained conversation archive. Recovery cannot silently roll back external
work, and cleanup accounts for exact owned resources and scoped access. Agent output never becomes
its own success authority.
