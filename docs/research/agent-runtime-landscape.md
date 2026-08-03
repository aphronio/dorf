# Agent Runtime Landscape

## Purpose

This is non-normative research about adjacent projects and potential competitors. It exists to
support periodic market and ecosystem review.

Entries here are sources of useful evidence and ideas, but they are not design precedents. Their
tradeoffs should be studied. Their concepts, terminology, APIs, and architecture should not be
copied into Dorf without independent evidence from Dorf's own requirements, dogfooding,
and implementation pressure.

Use this document to ask what already exists and where Dorf may be meaningfully different. Do
not use it to infer Dorf requirements.

## Evaluation Rules

- Begin with Dorf's product boundary and observed user needs.
- Treat similarities as evidence that a problem is real, not that another project's solution is
  correct for Dorf.
- Prefer the smallest design that works for Dorf over compatibility with an adjacent project.
- Adopt an external protocol or dependency only after evaluating its cost, constraints, and fit.
- Revisit observations because products and APIs in this area change quickly.

Useful learning includes identifying which problems recur, which distinctions survive production
use, which features users depend on, and which complexity appears avoidable. Learning does not
require converging on the same design.

## OpenHands

Last reviewed: 2026-07-21

- Project: [OpenHands](https://github.com/OpenHands/OpenHands)
- Documentation: [OpenHands SDK](https://docs.openhands.dev/sdk/index)
- Category: broad software-agent platform and SDK
- Relationship: closest known open-source overlap with the proposed durable agent-session runtime

OpenHands is worth monitoring because it combines persistent conversations, local and remote
workspaces, event-driven control, and support for multiple coding agents. Its scope is substantially
broader than the intended Dorf runtime.

Questions to monitor:

- How does OpenHands separate conversation identity from workspace or sandbox identity?
- How does it operate existing agent processes without requiring applications to adopt the complete
  OpenHands framework?
- Which parts of its local and remote execution model users actually adopt?
- Does its breadth create demand for a smaller, local-first, composable alternative?

This entry does not establish OpenHands' object model, protocols, or interfaces as a starting point
for Dorf.

## Kimi Agent SDK and Kimi CLI

Last reviewed: 2026-07-21

- Project: [Kimi Agent SDK](https://github.com/MoonshotAI/kimi-agent-sdk)
- Runtime: [Kimi CLI](https://github.com/MoonshotAI/kimi-cli)
- Category: Kimi-specific agent runtime, SDK, and CLI
- Relationship: adjacent implementation of resumable multi-turn sessions and sandbox-routed tools

Useful areas to study include durable session history, event replay, approvals, cancellation, and
the distinction between starting a new turn and steering active work. Kimi's KAOS examples are also
relevant to understanding the limits of a shared filesystem and process interface across different
execution environments.

Kimi remains tied to its own agent runtime. Dorf should not assume Kimi's session hierarchy,
wire protocol, or tool-routing boundary is appropriate for supervising other agent processes.

## Kimi Agent Swarm

Last reviewed: 2026-07-21

- Research: [Kimi K2.5 technical report](https://arxiv.org/html/2602.02276)
- Category: multi-agent orchestration and training research
- Relationship: potential future consumer of durable sessions, not part of the runtime boundary

Useful ideas to study include context sharding, selective result routing, dynamic delegation, and
optimizing critical-path latency instead of maximizing agent count.

Swarm policy should remain above the Dorf runtime. Task decomposition, subagent roles,
scheduling, aggregation, and decisions about parallelism are workflow or orchestration concerns.

## OpenAI Agents SDK

Last reviewed: 2026-07-21

- Project: [OpenAI Agents SDK](https://github.com/openai/openai-agents-python)
- Documentation: [Sandbox agents](https://openai.github.io/openai-agents-python/sandbox/guide/)
- Category: agent-framework SDK with sandbox execution
- Relationship: adjacent provider-neutral sandbox integration inside a specific agent framework

Useful areas to study include the separation of conversation state from sandbox state, explicit
sandbox sessions, snapshot and resume behavior, and how provider-specific capability differences
are exposed.

The OpenAI Agents SDK owns the agent loop. Dorf intends to supervise existing agent processes,
so it should not inherit the SDK's framework abstractions by default.

## Codex App Server

Last reviewed: 2026-07-22

- Project: [Codex](https://github.com/openai/codex)
- Documentation: [Codex app-server](https://developers.openai.com/codex/app-server)
- Category: native Codex control protocol for rich clients
- Relationship: selected first control surface behind Dorf's Codex driver

App-server exposes Codex-native threads, turns, history, approvals, interruption, steering, and live
notifications. Dorf will use its authenticated WebSocket transport behind a thin, version-aware
driver while retaining tmux and SSH for operational takeover. App-server's schema is not Dorf's
public runtime API, and Codex remains authoritative for its transcript.

## Agent Client Protocol

Last reviewed: 2026-07-22

- Project: [Agent Client Protocol](https://agentclientprotocol.com/)
- Category: open protocol for communication between agent clients and coding agents
- Relationship: preferred future driver protocol when Dorf supports a second required agent

ACP standardizes client-agent session and prompt interaction but does not provide Dorf's
environment lifecycle, isolation, backend placement, or coding workflow. Do not implement it while
Codex is the only required agent. When another ACP-capable agent such as Claude Code or Kimi CLI is
selected, add an ACP driver alongside the native Codex app-server driver rather than replacing the
runtime boundary.

## Kubernetes Agent Sandbox

Last reviewed: 2026-07-21

- Project: [Kubernetes Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox)
- Documentation: [Agent Sandbox documentation](https://agent-sandbox.sigs.k8s.io/docs/)
- Category: open infrastructure for isolated, stateful agent environments
- Relationship: potential environment substrate below the durable agent-session runtime

Useful areas to study include stable sandbox identity, templates, claims, warm pools, hibernation,
and isolation-runtime choices. These are infrastructure concerns; the project does not define the
messageable agent-session semantics Dorf is exploring.
