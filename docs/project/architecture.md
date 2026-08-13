# Greenfield Architecture — Go + Absurd

This document records Dorf's accepted Go, PostgreSQL, and Absurd boundaries. It defines authority,
recovery, composition, and evolution constraints. Code owns concrete package, schema, task, step,
query, and API shape; GitHub issues own temporary implementation scope and acceptance criteria.

The superseded Python implementation is historical evidence, not a compatibility target. Old local
state, SQLite schemas, Python APIs, CLI shapes, and document formats are not supported.

## Product and implementation scope

Dorf Core is the open-source control plane for running supported agent Harnesses on infrastructure
its owner controls. The portability direction covers a verified Harness version and configuration,
skills, extensions or plugins, project instructions, workspace image or setup and dependencies,
vendor-supported connection, host constraints, tools, isolation, recovery, and observation. Dorf
owns accepted intent, AgentRun and Sandbox custody, external-effect reconciliation, recovery,
Evidence, durable attachment of the workflow-defined Outcome, and cleanup. Harness-native session
durability remains authoritative behind its adapter.

The verified implementation is deliberately narrower: one Go application, one coding-to-PR
workflow, PostgreSQL, Absurd, local Incus on the supported host, Codex, Git, and GitHub. This
architecture must keep that real path clear without treating its coding-specific records as the
permanent public workflow API.

## System shape

```mermaid
flowchart LR
    Client["Client or external product"] --> Core["Dorf Core"]
    Workflow["Native Dorf workflow"] --> Core
    Core --> Custody["Job-wide custody"]
    Custody --> PG[("PostgreSQL facts")]
    Custody --> Absurd["Absurd durable execution"]
    Absurd --> Edge["Actions · Checks · AgentRuns"]
    Edge --> Sandbox["Sandbox provider"]
    Edge --> External["Workflow external authorities"]
    Sandbox --> Harness["Agent Harness"]
```

Absurd owns when durable work is eligible, claimed, checkpointed, retried, sleeping, waiting, or
cancelled. It does not own Dorf's product vocabulary or become the only place where a Job's truth can
be understood.

The workflow owns semantic ordering and terminal meaning. The durable custody layer owns stable Job
identity, accepted input, resource ownership, AgentRun and external-effect facts, evidence custody,
attention, outcome attachment, and cleanup state. Adapters translate existing authorities; they do
not invent another workflow.

## Authority model

| Fact | Authority |
| --- | --- |
| Job identity, bounded goal, workflow identity/version, accepted input, durable lifecycle, and cleanup | Dorf-owned PostgreSQL facts |
| Workflow-specific facts and outcome | Workflow-owned PostgreSQL tables or typed records |
| Task claims, checkpoints, retry schedule, sleeps, waits, and cancellation | Absurd schema in the same PostgreSQL deployment |
| Agent transcript, tool items, Thread, Turn, and native history | The selected Harness |
| Mutable files, running processes, and local tool output | A Job-owned Sandbox |
| External objects and their mutable state | Their external authority, such as GitHub or another service |
| Retained observed proof | Content-addressed Evidence linked to the fact it proves |

The same mutable fact must not be mirrored into multiple authorities. Read models may project facts
for inspection, but they are disposable and rebuildable. Agent prose and workflow reports are claims
or results; Evidence proves only what Dorf or an adapter actually observed.

Resource ownership follows lifetime. A Job is the aggregate owner of every Sandbox allocated for
it. A Sandbox owns or deterministically identifies its scoped provider route and injected authority.
AgentRuns use a Sandbox but never own it. Cleanup begins at the Job and reconciles resources against
their external authorities before declaring them removed.

## Execution model

One admission creates one Job with complete bounded intent, a stable idempotency identity, and one
pinned workflow version. If recording and scheduling cannot share one transaction, recovery
reconciles the boundary rather than assuming both happened.

Each workflow has one readable coordinator over its natural facts. It asks what work is currently
missing, performs one bounded operation, records the resulting fact, reloads, and continues or waits.
Execution and human inspection derive from the same authoritative facts. Dorf does not persist a
second program counter merely to describe what those facts already imply.

The coordinator is ordinary Go, not a reusable graph interpreter. Absurd supplies durable tasks,
steps, events, retries, waits, claims, heartbeats, and cancellation. Dorf does not rebuild those
mechanics in product tables or query Absurd's private schema as workflow authority.

### Messages and AgentRuns

Accepted client input receives immutable Job-local identity and order. A follow preserves FIFO
order. A steer is an explicit priority intent targeting active work and must remain observable as
such. Wake events make work eligible; they do not replace durable delivery facts.

An AgentRun is one bounded delivery of one Message to an agent in a named Role and capability
envelope. It retains the exact Harness, Thread, Turn, submission, observation, and terminal facts
needed to reconcile uncertain delivery. Harness transcript and workspace details remain behind
their adapters. Retrying uncertain delivery reconciles the same AgentRun; it does not silently
create another judgment attempt.

Thread reuse is a workflow choice. Coding may reuse an implementation Thread and isolate reviewers;
research may choose a different pattern. That choice does not create a second durable conversation
primitive.

### Deterministic operations

A Check observes or asserts without mutating an external authority. An Action is a code-owned
external mutation with stable identity, intended scope, settlement state, and a reconciliation path.
Before repeating an unsettled Action, Dorf inspects the actual authority. Immutable success makes an
identical retry a no-op.

Agent tool calls and agent-authored files are AgentRun work, not automatically Actions or Evidence.
A workflow observes relevant results at the AgentRun boundary and records natural typed facts.
Generic result strings, arbitrary metadata bags, and copied external state are not substitutes for
domain records.

### Evidence and inspection

Evidence is immutable observed proof linked to an AgentRun, Action, Check, artifact, or
workflow-specific fact. Its validity follows the claim it supports: a coding Revision change may
invalidate Revision-bound evidence, while a captured source or lifecycle observation may remain
valid.

Inspection projects one situation-first view from Dorf and workflow facts: accepted goal, observed
history, current work or attention, outcome, evidence, and cleanup. Raw Absurd attempts, leases,
checkpoints, and waits remain operator diagnostics through Absurd's tools rather than being copied
into Dorf's product history.

## Durable core and workflow facts

The intended reusable custody concepts are Job identity, admission, Messages, AgentRuns, Sandbox
ownership, stable external effects, evidence storage, attention, recovery, and cleanup. Their exact
public shape is not yet accepted.

The current implementation still places repository, branch, Revision, review, Proposal, and GitHub
assumptions in records near the spine. That is evidence from the first workflow, not a reason to make
every field nullable or introduce generic payload tables. A second workflow should first add its
natural facts and coordinator. Only duplicated behavior with the same authority and recovery meaning
earns extraction.

Workflow facts remain specific:

- coding owns repository authority, Revisions, Checks tied to a Revision, review policy, Proposal,
  GitHub outcome, and coding inspection;
- a candidate research workflow would own its source policy, captured-source facts, report contract,
  result or no-result outcome, and research evaluation; and
- future workflows must not inherit Git or research semantics merely to reuse durable custody.

Do not introduce a polymorphic fact owner, generic JSON result, arbitrary Action registry, or common
workflow phase to make unlike facts look similar.

## Client boundary

Clients submit bounded intent, inspect, send Messages, receive results, and request explicit terminal
operations. They do not open PostgreSQL, drive Absurd, construct adapters, or become workflow
authorities.

The CLI with stable structured output is the first client boundary and the smallest integration for
a same-host orchestrator such as Agent0. A thin language SDK may wrap the proven application
contract. HTTP is justified when a real remote or untrusted client needs it. CI, GitHub, webhooks,
MCP, schedules, Slack, and user interfaces are adapters that translate events into idempotent Job
admission and render the same facts; they are not separate workflow engines.

## Native workflow composition

Native workflows are Core dogfood. They should compose AgentRuns with deterministic code, Checks,
budgets, approvals, Evidence, and explicit Outcomes through the same intended Core contract that
ordinary clients and other products may later embed. They must not depend on a privileged internal
path. Transport, SDK, and public compatibility promises remain uncommitted until real portability
implementations and external-client use prove them.

The desired authoring unit is ordinary versioned source plus a small machine-readable contract:

- typed input and workflow-specific outcomes;
- required Harness, Sandbox, provider connections, secrets, and capabilities;
- deterministic operations and bounded AgentRun judgment;
- budgets and attention boundaries;
- per-run Checks and offline evaluation cases; and
- provenance, immutable version, promotion, rollback, and upgrade policy.

Agent-friendly authoring means scaffolding, examples, schemas, deterministic fixtures, local
evaluation, and diagnostics that another agent can use. Agents propose reviewable source changes;
they do not activate new powers, credentials, or production workflow versions without policy and
human approval.

Dynamic agent-authored recipes are a later UX layer. They do not justify a generic automation
canvas, graph framework, agent builder, model/tool Harness, registry, or marketplace.

## Current coding composition

One admitted coding request owns its Sandboxes, clone, branch, Revision line, implementation Thread,
selected review resources, exact pull-request Proposal, explicit outcome, and cleanup. Its explicit
coordinator orders deterministic setup, implementation AgentRuns, Git observation, repository
Checks, selected review, publication, feedback, terminal observation, and cleanup from natural
facts.

Review is selected rather than ritualized. Deterministic policy supplies mandatory review for known
risks and may select one bounded general reviewer for unknown work. Each reviewer consumes a
workflow Message in an isolated capability envelope. Reviewer prose returns as a Message to the
implementation path; it is not parsed into a universal policy result or copied into Evidence.

Git and GitHub remain authoritative for branch, commits, pull request, comments, merge, and close.
Exact external observations produce the coding outcome. Explicit abandonment is human authority and
may occur before a Proposal. Outcome and cleanup remain separate.

## Failure and code evolution

- **Process loss:** Absurd makes unfinished work eligible elsewhere; Dorf reconciles Actions and
  AgentRuns against their authorities before continuing.
- **Sandbox loss:** report the loss honestly. Replace it only when authoritative retained state makes
  continuity truthful.
- **External ambiguity:** inspect the external authority; never infer success from timeout or retry
  blindly.
- **Poison work:** bounded attempts, time, cost, and attention stop infinite agent or provider spend.
- **Code changes:** prefer short-lived Jobs, additive compatible task results where practical, and
  versioned workflow code. Let active Jobs drain on their pinned version rather than translating
  opaque execution history.

## Local, on-premise, and hosted shapes

The first product is local-first. PostgreSQL, Dorf executors, Incus, the Provider Gateway when used,
and agent services run on infrastructure controlled by the owner. The ordinary supported path
requires no Dorf-hosted durability account or host Docker socket.

"Infrastructure you control" may later include a local machine, bring-your-own-cloud or on-premise
execution, or a deliberately selected managed Sandbox provider. Support is expressed through
verified profiles, not a promise that every Harness works on every provider. Multi-tenant
authentication, billing, quotas, hosted secrets, and untrusted extension execution are separate
product requirements; they are not implied by choosing Absurd.

Deployment configuration owns host locations and credentials. Durable Jobs retain stable logical
connection and provider identities, not controller filesystem paths or copied secrets.

## Harness and Sandbox portability

Incus is the only verified Sandbox provider and Codex the only verified Harness. Their
adapters should not leak vendor protocol into workflow facts, but interfaces alone do not justify an
agnostic support claim.

A verified profile covers the Harness version and configuration, skills, extensions or plugins,
project instructions, workspace image or setup and dependencies, vendor-supported credential or
subscription connection, host constraints, tools, isolation, recovery, interruption, and terminal
observation. Connection custody does not imply copying raw user secrets into the Sandbox; scoped
routing or injection remains adapter- and profile-specific. Common consumer and workflow code must
have no Harness- or Sandbox-specific branches beyond profile selection and capability admission.
D063 records the current proof order. Mac-like environments and sensitive enterprise experimentation
are motivating future scenarios, not verified capabilities.

Go remains the core. A language-specific executor is justified only when a concrete provider SDK or
workflow authoring need makes that boundary materially smaller. It consumes a dedicated queue through
a small versioned contract and may not leak vendor types into core Job authority.

## Dependency budget

Use the Go standard library for ordinary HTTP/JSON, process control, hashing, configuration,
structured logging, concurrency, and tests. Prefer direct, programmatic boundaries to wrapper
frameworks whose model would become another authority to understand.

Absurd and the PostgreSQL driver surface are accepted core dependencies. One maintained WebSocket
implementation is acceptable for a Harness transport. Do not add an ORM, dependency-injection
container, web framework, message bus, migration framework, CLI framework, workflow DSL, or
observability distribution until a concrete terminal proves explicit code materially worse.

Every added module must name the real terminal it enables and remain removable behind a narrow
boundary. Transitive dependency count is a design signal, not a score to optimize at the expense of
correctness.

## Replacement and portability

- Do not build a Python compatibility facade, dual-write SQLite and PostgreSQL, or migrate old local
  data.
- Do not create a generic durable-engine interface. Keep Absurd sequencing localized and domain
  facts, deterministic policy, Actions, Checks, and reconciliation independent.
- Use Absurd's public APIs for production behavior. Raw tables may support version-pinned tests or
  operator diagnostics but are not workflow authority.
- Preserve useful provisioning assets and observed behavior, not obsolete package or schema shape.
- Delete implementation and coupled tests after the replacing vertical slice reaches the same real
  terminal.

If Dorf outgrows Absurd, completed Jobs remain historical domain records, active short-lived Jobs can
drain, and new Jobs can begin on the replacement. Raw checkpoint history is not a portability
format.

## Current verified terminal

The Go replacement is proven only when a clean supported host can complete one real coding Job:

1. converge setup without a Dorf cloud account or host Docker socket;
2. admit a complete goal and create one isolated Sandbox and branch;
3. start a real implementation AgentRun and accept input while it is active;
4. survive controller and executor loss without duplicating a Sandbox, Message, Turn, or Action;
5. run deterministic repository Checks and retain Revision-bound Evidence;
6. select useful review and return its text through the implementation Message path;
7. publish and observe an exact-Revision pull-request Proposal;
8. record accepted, rejected, or abandoned outcome; and
9. reconcile cleanup to an observable terminal.

This terminal must run without Python in the critical path. Feature parity with the deleted Python
implementation is not required. Future portability terminals and any later non-coding workflow must
be specified and proven separately; they do not weaken this coding proof.
