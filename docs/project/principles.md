# Dorf Principles

This document contains enduring, repository-wide product and engineering judgment. The North Star
defines the desired experience; the greenfield architecture records the selected technical shape;
GitHub issues own temporary implementation scope.

## Build a small thing that composes

Dorf is a dependable durable coding primitive, not a universal agent organization. Coding-to-PR is
the only current workflow requirements driver. A seam is justified when it makes that real workflow
clearer or when a second concrete implementation proves it; hypothetical consumers do not.

## Durable Jobs, replaceable processes, isolated Sandboxes

A Job is the durable unit of user intent. Its initiating client, controller, task-executor process, and
current agent process may disappear without erasing accepted input or observed progress. A Sandbox
is the isolated mutable workstation in which the Job acts. A Job-local Session supplies
conversation continuity when the harness supports it.

Do not introduce a durable Worker merely as a synonym for a process or AgentRun. Add Worker only
when persistent personality, capability, reputation, ownership, or memory across Jobs becomes a
real product requirement.

## Deterministic before agentic

Anything that can be derived or executed programmatically should be. Admission, identity,
sequencing, setup, policy facts, Checks, evidence hashing, publication, retry, and cleanup are
code-owned. Implementation and repair AgentRuns own the change itself, including one or many Git
commits. When they change code, Dorf validates the clean final checkout and records its descendant
commit as the next Revision. A review repair may instead leave clean `HEAD` unchanged when it rejects
a false-positive finding. Agents are otherwise reserved for ambiguity, triage, review, and other
judgment.

Review authority starts with deterministic mandatory policy. An implementation AgentRun may also
make a structured, bounded request for an allowlisted review Role or focus, and an unknown
classification may invoke bounded triage. Unstructured agent prose is not policy input; no agent
request can waive a Check, mandatory Role, capability boundary, or spend limit.

## Disposable developer workstations

Each coding Job should feel like a fresh developer workstation: isolated checkout, explicit branch,
deterministic setup, checks, smoke tests, evidence, and a clear accept or discard path.

The guiding question is:

```text
Can a fresh disposable machine clone this repo, start the full app, run checks, make a branch,
prove the change, and disappear without leaving ambiguous state?
```

## Repository contracts over environment cleverness

Managed repositories expose explicit setup, check, smoke, and service commands. Dorf executes them
programmatically instead of asking an agent to rediscover them through conversation. The contract
must remain useful to humans and CI; Dorf-specific coupling in product code is a smell.

## Boring before fancy

Prove the direct Incus and Codex coding path before adding another Sandbox provider, agent harness,
language-specific executor, cache, scheduler, or plugin mechanism. Prefer a small concrete implementation over
a registry or framework whose second member does not exist.

## Vertical slices, deletion, and no compatibility tax

Architecture advances through narrow slices that end in real Job behavior. A schema, abstraction,
or mocked adapter is not a terminal. Dogfood the smallest new path, use its evidence to shape the
next slice, and delete redundant implementation when the replacement is authoritative.

There are no existing users or local data to preserve during the Go and Absurd replacement. Do not
add migrations, dual writes, facades, deprecated commands, or compatibility tests for the Python
and SQLite implementation.

## Tests buy confidence, not inventory

Test substantial product decisions, plausible regressions, and rare high-impact invariants such as
authority, concurrency, idempotency, recovery, and cleanup. Prefer behavior through the public
boundary and fault injection over tests coupled to database rows or private functions. Delete tests
with superseded behavior.

Agents run deterministic tests locally for fast feedback before pushing. CI independently repeats
the portable unit and PostgreSQL integration suites so merge confidence does not depend on one
workstation's state or on whether a local command was skipped. Live Incus, Codex, and GitHub proofs
remain targeted terminals for changes that touch those authorities, not default CI simulations.

## Evidence over narration

Agent output is a claim. Process state, command results, commits, external authority, and retained
artifacts are observed facts. Evidence must identify its provenance and Revision. A fluent agent
must never silently become the authority for its own success.

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
