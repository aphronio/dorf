# Dorf Principles

This document contains enduring, repository-wide product and engineering judgment. The North Star
defines the desired experience; the architecture records the selected technical shape;
GitHub issues own temporary implementation scope.

## Build a small thing that composes

Product direction and vocabulary live in the [North Star](north-star.md). Build the smallest
reliable control-plane primitive required by a proven client or workflow, then let real use earn
broader seams. Do not turn possible Harnesses, Sandboxes, clients, or workflows into speculative
abstractions.

Existing clients supply evidence of a need, not application-specific requirements for Core.
For each API change, identify what becomes simpler for a real client, the underlying execution or
resource responsibility, and whether another client could use the same operation without knowing
that application's domain. Prove the smallest general primitive through current use. Keep goals,
policy, and application identities with the client; possible future clients do not justify a framework.

## Build from conviction, not competitor parity

Competitors are evidence about the market, not a specification for Dorf. Inspect an adjacent product
when choosing a protocol or dependency, solving a concrete problem Dorf has encountered, or
substantiating a public comparison. Do not turn feature lists, categories, or architecture into our
roadmap.

Dorf earns its identity through consistent choices: supported existing harnesses, owner-chosen
isolated infrastructure, explicit capability admission, and control-plane custody across each Session.
Build the smallest useful expression of that belief, then let dogfood and real users reveal what
comes next. If a proposed feature only makes Dorf resemble another agent platform, that is not a
reason to build it.

## Durable Sessions, replaceable processes, isolated Sandboxes

A Session is the durable unit of user intent. Its initiating client, controller, task-executor process, and
current agent process may disappear without erasing accepted input or observed progress. A Session owns
one or more Sandboxes; each Sandbox is an isolated mutable workstation and has one deterministically
named Provider Route. Immutable Action success records the Route and Sandbox lifecycle. AgentRuns use
a Sandbox rather than owning infrastructure. The Session owns the binding to one harness
Thread for client input. Native subagent threads remain the Harness's responsibility. Every AgentRun
consumes one durable Message and retains its exact Turn binding. Every Message selected for agent delivery has one AgentRun record. While admission is open, a follow joins
the FIFO, reuses the authoritative retained Thread, and creates a distinct Turn. A steer atomically
targets the exact active Turn and may overtake queued follows. Explicit steer never falls back to a
new Turn and fails honestly when that target becomes terminal. Automatic intent preserves eventual
input delivery: after proof that its selected Turn terminated without accepting the exact Message,
the same Message returns to FIFO as a follow. Harness protocol and transcripts remain behind the adapter.

Do not introduce a durable Worker merely as a synonym for a process or AgentRun. Add Worker only
when persistent personality, capability, reputation, ownership, or memory across Sessions becomes a
real product requirement.

## Deterministic before agentic

Anything that can be derived or executed programmatically should be. Admission, identity,
sequencing, workflow policy, evidence hashing, external-effect reconciliation, retry, and
cleanup execution are code-owned rather than agent judgment. Apply the
[North Star product boundary](north-star.md#product-boundary): “code-owned” identifies automation,
not which layer owns its meaning. Actions record code-owned external mutations; an agent invocation
is instead owned and reconciled by its AgentRun.

Application setup, evaluation, review, and publication are client-owned policy. Native agent output
is input to that policy, not an authority that can waive execution constraints.

## Facts before workflow status

Persist the product facts that actually happened, not a second durable program counter describing
where code believes the workflow is. The execution controller derives one current operation from those
facts and executes it through Absurd. Inspection derives the expected dependency chain,
chronological history, and current work from the same source of truth.

This rule exists for clarity and composition: a new feedback source adds a Message. Neither should require a new phase or a matrix of transitions across
admission, readiness, publication, and inspection. Each client owns decisions over its application facts; the durable core does not interpret their meaning.

Do not turn this into a generic DAG engine, configurable workflow language, copied event log, giant
SQL `next_work` query, or persisted derived status. Keep each proven workflow decision visible in
ordinary code, extract common operations only after repeated use, add a durable fact only when real
recovery cannot be derived without it, and leave task attempts, claims, checkpoints, waits, and
retries to Absurd.

## Contracts and evaluation before autonomy

An application begins with a bounded contract: typed intent, capability envelope, budget, expected
outcomes, deterministic validations, and honest failure or no-result terminals. Its evaluation cases are
part of the workflow, not a platform feature added after authoring. Runtime invariants protect
recovery, idempotency, authority, and requested cleanup execution; workflow evaluations measure
whether the result was useful.

Clients may author and revise application code and evaluations. Agents may not grant themselves
credentials or capabilities or replace inspectable policy with an opaque generated graph. Agent-friendly development means
machine-readable contracts, excellent diagnostics, fixtures, and short feedback loops.

## Disposable developer workstations

A client-directed coding environment should feel like a fresh developer workstation: isolated checkout, explicit branch,
deterministic setup, checks, smoke tests, evidence, and a clear workflow- or client-owned accept or
discard path.

The guiding question is:

```text
Can a fresh disposable machine clone this repo, start the full app, run checks, make a branch,
prove the change, and disappear without leaving ambiguous state?
```

## Boring before fancy

Prove one concrete portability axis at a time before generalizing. A profile is a verified Harness
and Sandbox combination, not a claim that every Harness works everywhere. D063 records the current
proof order and mechanical oracle.

Prefer a small concrete implementation over a registry or framework whose second member does not
exist. Do not generalize workflow authoring before Core portability is proved.

## Vertical slices, replaceable technology, and preserved evidence

Architecture advances through narrow slices that end in real Session behavior. A schema, abstraction,
or mocked adapter is not a terminal. Dogfood the smallest new path, use its evidence to shape the
next slice, and delete redundant implementation when the replacement is authoritative. Choose the
live terminal that exercises the changed authority. [Support](../support.md) owns the current
deployment shapes.

No model, model configuration, library version, framework, language, abstraction, database shape,
or existing code is permanent. Replace or delete one when it blocks the product direction or costs
more to understand and maintain than a better design. Rewrite from scratch when removing old
assumptions produces the smaller and clearer system.

Every extra concept increases the context that a human or agent must load before making a safe
change. Prefer fewer concepts, less code, and direct ownership. Upgrade a dependency when the newer
version removes code or concepts without losing required behavior. Do not maintain compatibility
for a hypothetical consumer.

During the single-user stage, prototype data may be reset after an explicit preservation decision
and the user's approval. Preserve useful Session history, application evidence, evaluations, dogfood proof, observed
failures, and usage, cost, or outcome history when they can improve later work. Delete caches,
rebuildable projections, obsolete schemas, and records that have no remaining product, evaluation,
or audit value.

Deletion is product work. Agents are exceptionally good at adding plausible code, so every workflow
and abstraction must also make redundancy, simplification, and removal visible. A smaller system
with the same proven behavior is an improvement.

Ask two questions before preserving an existing choice: Would Dorf choose it if development started
today? Which accumulated evidence must survive its replacement?

## Tests buy confidence, not inventory

Test substantial product decisions, plausible regressions, and rare high-impact invariants such as
authority, concurrency, idempotency, recovery, and cleanup. Prefer behavior through the public
boundary and fault injection over tests coupled to database rows or private functions. Keep the
smallest strong proof at the boundary that owns each invariant. Avoid tests that merely freeze a
helper call graph, callback wiring, duplicated validation branches, generated implementation shape,
or speculative combinations with no credible failure. When an implementation is consolidated,
delete superseded tests instead of porting them line for line; retain the fault, concurrency, and
workflow-policy cases that would catch a real regression.

Agents run deterministic tests locally for fast feedback before pushing. CI independently repeats
the portable unit and PostgreSQL integration suites so merge confidence does not depend on one
workstation's state or on whether a local command was skipped. Live Incus, Codex, and GitHub proofs
remain targeted terminals for changes that touch those authorities, not default CI simulations.

## Evidence over narration

Agent prose is a Message or application result, not proof. Process state, command results, commits,
harness observation, external authority, and retained content identity are observed facts. Clients retain application evidence; Dorf retains input and execution/resource receipts.
Do not duplicate reviewer prose as platform evidence. A fluent agent must never silently become the authority
for its own success.

Evidence also guides product changes. Dogfood workflows early. Prefer measured human attention,
useful-outcome rate, rework, cost, and observed failures over agent narratives. Change direction
from repeated evidence, not isolated runs.

## No host Docker socket as isolation

Sharing `/var/run/docker.sock` is not a Sandbox boundary. A repository may run containers inside an
Incus VM when its contract requires them, but Dorf does not hand an agent control over the host
container daemon. Dagger or another nested execution engine is adopted only after a concrete
repository proves that direct commands are insufficient.

## Primary influences

Mitchell Hashimoto's [Building Block Economy](https://mitchellh.com/writing/building-block-economy)
frames the product posture: build a small dependable primitive and let real use establish which
seams deserve to become public.

His [approach to building large technical
projects](https://mitchellh.com/writing/building-large-technical-projects) frames execution: choose
small problems with visible results, solve only enough to reach the next runnable demonstration,
adopt the software early, and let dogfood reveal what to build next.

Jason Fried's [advice on building in a competitive
market](https://x.com/jasonfried/status/2087213055881236927) frames competitive posture: learn from
the market without letting competitors define what Dorf should become or why it should exist.
