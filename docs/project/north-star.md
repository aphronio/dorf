# Dorf North Star — Portable, Controlled Agent Execution

Dorf is the open-source control plane for running agent harnesses on infrastructure you control.

**Your agents. Your infrastructure. One API.**

The direction is to bring a verified Harness version and configuration, its skills, extensions or
plugins, project instructions, workspace image or setup and dependencies, and vendor-supported
connection into compatible isolated infrastructure. Operators should be able to choose verified
local, bring-your-own-cloud, or managed profiles without rebuilding the agent system they trust.
Connection custody does not mean copying raw user secrets into a Sandbox; each profile and adapter
must define scoped routing or injection.

Dorf owns the control-plane guarantees around the Harness: accepted intent, AgentRun and Sandbox
custody, external-effect reconciliation, recovery, Evidence, durable attachment of the
workflow-defined Outcome, and cleanup. A Harness may provide its own durable sessions; Dorf does not
compete by duplicating them.

This is the product and experience direction. It is not an API inventory, schema, package plan, or
issue backlog. The currently verified product is narrower: one coding-to-PR workflow using Codex in
local Incus on the supported host, with Git and GitHub as deliverable authorities. Multiple
Harnesses, Sandbox providers, and workflow-authoring surfaces remain direction until real
implementations validate their seams.

## Ten-second model

```text
A client selects a verified Harness and Sandbox profile, then delegates a goal-backed Job.
Dorf carries the profile's supported setup and owns controlled execution around it.
The Job owns isolated resources, recovery, and external effects.
Dorf returns an outcome with observed evidence and honest cleanup state.
```

## Product boundary

Dorf Core is the product. Its intended contract gives clients one controlled boundary for supported
existing Harnesses on chosen compatible infrastructure. Portability is capability-based: admission
selects a verified profile and rejects combinations whose configuration, dependencies, credentials,
host constraints, tools, isolation, recovery, or observation contract has not been proved.

Dorf does not own the user's memory, priorities, or cross-Job life. A personal assistant such as
Agent0, a CLI, CI, or another trusted client decides what to delegate and how separate Jobs compose.
A workflow owns Job semantics, policy, evaluation, and what its Outcome means. Dorf owns custody of
execution and durable attachment of that Outcome. Native Dorf workflows should dogfood the same
intended Core contract that ordinary clients and other products may later embed; they must not have
a privileged hidden path.

This gives each layer one job:

```text
Client       chooses goals and composes Jobs
Workflow     owns semantics, policy, evaluation, and Outcome meaning
Dorf core    owns Job-wide run custody, Outcome attachment, recovery, evidence, and cleanup
Adapters     translate Harnesses, Sandboxes, providers, and external authorities
```

## Vocabulary

| Term | Meaning |
| --- | --- |
| **Job** | One durable bounded goal, its workflow version, lifecycle, and explicit outcome |
| **Workflow** | Ordinary versioned policy that composes deterministic operations and AgentRuns for one kind of Job |
| **Sandbox** | An isolated mutable workstation owned for a Job's lifetime |
| **Message** | Durable input from a human, agent, or workflow |
| **AgentRun** | One bounded delivery of a Message to an agent, with exact Harness binding and outcome |
| **Harness** | Software hosting an agent, such as Codex app-server |
| **Thread** | Continuing conversation context owned by a Harness |
| **Turn** | One request/response cycle in a Harness Thread |
| **Role** | The bounded responsibility and capability envelope of an AgentRun |
| **Action** | Code-owned work that changes external state and must be reconciled safely |
| **Check** | Code-owned observation or assertion |
| **Evidence** | Immutable observed proof tied to the fact it supports |
| **Outcome** | The workflow-specific terminal result, separate from resource cleanup |

Coding adds workflow facts such as Revision, ReviewPlan, Proposal, and GitHub acceptance. Research
may add a scoped request, captured sources, and a report outcome. Those facts do not become core
vocabulary merely because one workflow needs them.

`Role` is a field, not an executing object. There is no first-class Worker until personality,
capability, reputation, ownership, or memory must persist across Jobs. A standing "researcher" or
"simplifier" may begin as client configuration that creates bounded Jobs; repeated use must earn a
durable cross-Job identity.

## High-level flow

```mermaid
flowchart TD
    Intent["Bounded intent"] --> Admit["Validate and admit Job"]
    Admit --> Contract["Pin workflow version, capabilities, budget, and expected outcomes"]
    Contract --> Sandbox["Create isolated Sandbox when needed"]
    Sandbox --> Work["Run deterministic operations and bounded AgentRuns"]
    Work --> Observe["Observe facts, Checks, artifacts, and external effects"]
    Observe --> Decide{"Workflow decision"}
    Decide -->|"more work"| Work
    Decide -->|"human judgment"| Attention["Ask one precise question"]
    Attention --> Work
    Decide -->|"terminal"| Outcome["Record workflow Outcome"]
    Outcome --> Cleanup["Reconcile cleanup separately"]
    Cleanup --> Receipt["Return result, Evidence, and cleanup state"]
```

Each workflow keeps its decision small and explicit in ordinary code. Dorf is not a configurable DAG
engine: Absurd owns generic task execution mechanics, while Dorf and the workflow retain the product
facts needed to explain and recover the Job.

## Two examples

### Coding to a verified proposal

A client delegates a complete coding goal. The coding workflow creates an isolated clone and branch,
runs repository setup, lets an implementation AgentRun commit, observes an exact Revision, runs
deterministic Checks, selects only useful review, and publishes an exact-Revision pull request.
GitHub merge, close, or explicit abandonment supplies the workflow outcome; cleanup remains a
separate observable fact.

### Research to an evidence-backed answer

A personal assistant delegates a bounded question with source policy, budget, and report contract.
The research workflow captures sources and deterministic metadata, uses an AgentRun for search and
synthesis, checks citations and output shape programmatically where possible, and returns either a
report, an honest no-result, or precise attention. It owns no branch or pull request. The client may
use the result to answer the user or admit another Job.

The second example is direction, not current support. If implemented later, it can test which
coding-era mechanisms form a smaller common application boundary.

## Deterministic and agentic boundary

| Programmatic and deterministic | Agentic judgment |
| --- | --- |
| Validate input, identity, authority, capabilities, and budget | Understand an ambiguous goal and unfamiliar material |
| Create, inspect, and destroy Sandboxes | Choose an approach within the accepted envelope |
| Sequence durable input and reconcile retries | Implement, investigate, synthesize, or review |
| Run declared commands, schemas, probes, and policy rules | Interpret evidence that has no complete mechanical rule |
| Observe external authorities and retain artifacts | Explain uncertainty and material decisions |
| Hash, pin, invalidate, and render Evidence | Decide how to respond to human, Check, or reviewer Messages |
| Reconcile external effects and cleanup | Request human judgment when no safe default exists |

This boundary is a product promise: agent context is not spent rediscovering facts that code can
establish, and deterministic mechanisms do not pretend to answer questions requiring judgment.

Agent prose remains a Message or workflow result, not Evidence. Evidence proves observed facts: a
Harness completed a Turn, a command returned an exit status, a source was captured, a Revision was
observed, or an external authority contains an exact object. Fluent output never becomes its own
proof.

## Desired experience

- **One delegation:** the client supplies a complete bounded goal, not orchestration instructions.
- **Low-friction isolation:** a verified profile should carry the agent setup that already works into
  chosen isolated infrastructure instead of making the operator rebuild it in a new framework.
- **Detached by default:** watching a terminal or token stream is optional.
- **Calm recovery:** client, controller, executor, and agent process loss do not erase accepted input
  or create competing work.
- **Dangerous work, bounded:** secrets, network, filesystem access, external writes, spend, and
  destructive operations are explicit capabilities.
- **Situation first:** inspection shows the goal, observed history, current work or attention,
  result, Evidence, and cleanup without exposing executor internals by default.
- **Precise interruption:** humans are asked only for consequential decisions or genuine ambiguity
  without a safe default.
- **Honest terminal:** workflow outcome and cleanup remain separate until both have converged.
- **Owner-controlled deployment:** local, on-premise, or deliberately chosen managed infrastructure
  may host the system without making Dorf's cloud an authority.

## Layers and ownership

```text
L0  Existing tools       Harnesses, Sandbox providers, source hosts, APIs, provider SDKs
L1  Deterministic edge   Actions, Checks, capability enforcement, adapters
L2  Durable custody      Job identity, inbox, AgentRuns, Evidence, recovery, cleanup
L3  Core consumers        clients, external products, and native workflows
L4  Triggers and views    translate intent and render the same Job facts
```

Clients translate intent or external events into idempotent Job admission; they do not become
workflow authorities. Trigger breadth is not a core feature. A CLI, GitHub event, schedule, Slack
command, or personal assistant should eventually invoke the same application boundary.

## Native workflows as Core dogfood

The intended authoring unit is a versioned, inspectable workflow contract:

- typed input and workflow-specific outcomes;
- required Harness, Sandbox, connections, credentials, and capabilities;
- deterministic operations and external effects;
- bounded AgentRun judgment points;
- budgets and human-attention boundaries;
- Checks, evaluation cases, and honest terminal conditions; and
- source, version, provenance, and upgrade policy.

Agents and developers should author ordinary code with excellent scaffolding, machine-readable
contracts, fixtures, local evaluation, and diagnostics. An agent may propose workflow changes, but a
new version must pass its checks and evaluations and receive any required capability approval before
activation. Humans must be able to inspect, edit, fork, pin, and roll back what the agent built.

Native workflows should compose the same intended Core contract that ordinary clients and other
products may later embed. Transport, SDK, and public compatibility promises remain uncommitted until
real portability implementations and external-client use prove them. Dynamic agent-authored recipes
are a later UX layer, not the requirements driver for Core. Dorf does not become a generic
automation canvas, graph framework, agent builder, or model/tool Harness.

## Current verified slice

The supported product today is one Go application using PostgreSQL and Absurd, one coding workflow,
local Incus on the supported host as its Sandbox provider, Codex as its Harness, and Git/GitHub as
proposal and acceptance authorities. This narrow support claim is evidence, not the permanent
product boundary.

The current coding implementation may contain repository, Revision, review, and GitHub fields in
records that look core-shaped. Do not preserve or make them optional merely to claim generality. A
second workflow should add its natural facts first; only observed duplication earns extraction.

## Non-goals until evidence demands them

- a visual automation canvas or user-programmable DAG;
- an AI-company org chart, autonomous hierarchy, or swarm runtime;
- opaque prompt-generated workflows that humans cannot inspect or edit;
- a plugin or workflow marketplace before versioning, capabilities, provenance, and evaluations;
- speculative matrices of Harnesses, Sandboxes, models, languages, and providers;
- one mutable Sandbox shared by unrelated simultaneous Jobs;
- persistent Worker personalities or cross-Job memory without a real consumer;
- a mandatory agent review ritual when deterministic Checks suffice;
- compatibility with superseded Python, SQLite, Worker, Room, or Assignment representations; and
- preservation or migration of pre-release local data.

## Proof that the North Star is real

Dorf's durable custody is real when a client can admit a bounded Job, disappear, and later recover
an evidence-backed outcome without duplicate agent Turns, Sandboxes, external effects, or results.
Messages accepted during work remain ordered, ambiguous effects reconcile against their authority,
and cleanup is retryable and honest.

Dorf's portability claim is real when common consumer and workflow code has no Harness- or
Sandbox-specific branches beyond profile selection and capability admission. D063 records the
current proof order.

Mac-like environments and sensitive enterprise experimentation motivate future profiles, but they
are not current support claims. General workflow authoring follows portability proof; it does not
lead it.
