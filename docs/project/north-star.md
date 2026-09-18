# Dorf North Star

**Your agents. Your infrastructure. One API.**

Dorf is a stateful, self-hostable control plane for supported agent Harnesses on compatible isolated
infrastructure. It keeps accepted input, native conversation bindings, resource ownership, and
recovery dependable after the client disconnects. It does not replace the native Harness.

Current support belongs in [Support](../support.md), operator steps in
[Getting started](../getting-started.md), and technical authority in [Architecture](architecture.md).

## Product boundary

Clients choose goals, supply instructions and tool configuration, prepare application files,
interpret results, compose independent execution contexts, and decide when to release resources.
Coding, repository selection, review policy, publication, GitHub credentials, and business outcomes
belong to the client. Dorf ships no built-in application workflow.

Dorf owns accepted immutable Message input and attachments, ordered delivery, exact native
acceptance reconciliation and interruption, attested compute ownership, scoped model access,
supported recovery, and requested cleanup. Harnesses own native execution and conversation history.
Dorf exposes observations and durable receipts without treating agent prose as proof of success.

Apply this test before adding a concept: if it interprets business success, acceptance, rejection,
human judgment, cross-Job composition, or release timing, place it in the client. Keep only the
execution custody or lifecycle mechanism that remains after that policy is removed.

## Vocabulary

| Term | Meaning |
| --- | --- |
| **Job** | The current durable execution handle with admitted configuration, Messages, owned resources, and lifecycle |
| **Sandbox** | An isolated mutable workstation with exact resource ownership |
| **Message** | Durable text and optional ordered attachments with delivery intent and an immutable request identity |
| **AgentRun** | The current internal delivery and native execution recovery record for one Message |
| **Harness** | Native software hosting an agent, such as Codex app-server or Pi |
| **Thread / Turn** | The Harness's continuing conversation and individual execution identities |
| **Action** | A fixed compute or model-route lifecycle effect with stable identity and reconciliation |

Session naming and revised Thread/Turn ownership remain [proposals](../implementation/session-product-proposals.md).
Removing application policy does not itself change the current Job or AgentRun model.

## Message semantics

While admission is open, Follow joins the FIFO, reuses the retained Thread, and starts a distinct
Turn. Steer captures the exact active Turn and may overtake queued Follows. Explicit Steer never
silently becomes Follow. Auto returns the same Message to the FIFO only after proof that its
selected Turn ended without accepting it. Interruption targets an exact Turn. Clients choose
input and intent; Dorf reconciles delivery and preserves these rules.

## Workflow examples

An external coding client creates a direct Job, prepares its checkout, and sends instructions.
It retrieves files before cleanup and owns revision selection, isolated reviews, publication,
credentials, and the meaning of merge or close events. Several independently controlled agents
use separate Jobs composed by that client. Native Harness subagents remain native behavior.

An investigation client similarly supplies its source and instructions, chooses report paths,
retrieves needed files, and decides whether to continue. Neither application needs Dorf to assign
meaning to its output. Public primitives must earn any additional guarantees through concrete use.

## Desired experience

- Supported agent setups run on verified owner-selected infrastructure.
- Accepted input and recoverable execution survive client and worker process loss.
- Inspection reports execution, delivery uncertainty, resource state, and cleanup honestly.
- Model and provider credentials remain behind their defined authority boundaries.
- Release follows an explicit client request; idle does not mean that the goal is complete.

## Layers and ownership

```text
Client        Goals, application setup, evaluation, composition, release timing
Dorf          Input and execution custody, resources, supported recovery, cleanup
Adapters      Native Harness protocols, compute access, and model authority
Harness       Conversation history, agent execution, native tools and subagents
```

## Non-goals until evidence demands them

Dorf is not a workflow engine, agent builder, transcript replacement, application evidence store,
skills marketplace, or universal provider/harness compatibility layer. A profile is a verified
combination, not a promise that every provider supports every Harness or recovery mechanism.

## Proof that the North Star is real

A client can admit input, disappear, and later observe exact delivery and native execution without
duplicate unsafe effects. Messages remain ordered; ambiguous acceptance is reconciled against its
authority. Cleanup accounts for exact owned resources and scoped access. Provider replacement and
checkpoint recovery expose only their verified continuity guarantees. Agent output never becomes
its own success authority.
