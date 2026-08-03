# Dorf Principles

This document contains enduring, repo-wide product and engineering philosophy.

## Build Small Things That Compose

Dorf should follow the building-block posture described by Mitchell Hashimoto: make a small,
dependable primitive that focused applications can compose instead of growing one universal agent
platform.

The first and only workflow that drives requirements today is the coding-to-PR workflow. Research,
app building, deployment previews, swarms, and other possible applications may validate the same
primitives later, but they do not justify features now. A seam is useful when it makes the current
workflow clearer and simpler; hypothetical consumers are not sufficient evidence.

## Durable Workers and Jobs, Replaceable Processes, Recoverable Rooms

A Worker is a durable identity whose current isolated Room may fail. A Job exists only with a pinned
complete goal and is connected to one Worker by an explicit Assignment. The initiating client,
dispatcher, collector, harness control process, and current Room may leave or fail without erasing
those identities. Harness-native conversations provide conversational continuity while the exact
Room body and disk survive; Dorf records their typed bindings and operational lifecycle without
copying their transcripts. A lost Room is reported honestly rather than replaced until a harness or
Dorf-owned history mechanism can restore real continuity.

SSH and direct Room operations remain deliberate break-glass mechanisms for observation and manual
takeover. They do not define Worker, Room, Job, or Assignment identity.

## Disposable Developer Workstations

Dorf should make each task feel like a fresh developer workstation: isolated workspace, explicit branch, deterministic setup, checks, smoke tests, and a clear accept or discard path.

The guiding question is:

```text
Can a fresh disposable machine clone this repo, start the full app, run checks, make a branch, prove the change, and throw itself away?
```

## Repo Contracts Over Environment Cleverness

Managed repos should expose explicit setup, check, smoke, and service contracts. Dorf should execute those contracts programmatically instead of asking the agent to rediscover them through conversation.

The contract should be useful to humans and CI too. Dorf-specific coupling in product code is a smell unless the same change independently improves normal development.

## Boring Before Fancy

Prove the coding lifecycle with direct, understandable implementations. New environment providers,
agent protocols, caching layers, and orchestration features must solve an observed problem in a
working workflow.

Prefer the smallest interface that hides real complexity. A second concrete implementation is
evidence for a general seam; a hypothetical future implementation is not.

## Vertical Slices Before Frameworks

Architecture should move through narrow, working slices of the coding workflow. Each slice should
remain demonstrable through real Worker and Job behavior rather than landing unused interfaces or a
provider matrix.

Each implementation slice must end in the smallest real runnable outcome it enables—for example, a
real Worker turn or inference rather than only process health, a schema, or a metadata probe. Use
that run's evidence and feedback to shape the next seam; setup that cannot yet reach real behavior
belongs inside the next vertical slice rather than becoming its own terminal.

Follow KISS:

- prefer the smallest coherent interface that the current slice needs;
- let completed slices and dogfood evidence shape later interfaces;
- treat current code as evidence, not as a permanent design;
- refactor or remove code when a simpler boundary replaces it; and
- remove obsolete tests with the behavior they guarded, while retaining or adding tests for the
  behavior that still matters.

## Tests Buy Confidence, Not Exhaustiveness

Add tests when they protect a substantial product decision, a plausible regression, or a rare but
high-impact invariant such as security, concurrency, or durable recovery. Prefer behavior tests
through the module interface over tests coupled to implementation shape or patch targets.

Do not enumerate every low-likelihood error permutation or preserve tests merely because they
already exist. Use compact parameterized cases when each case protects a distinct decision, remove
redundant coverage when a deeper interface replaces it, and judge a test by the confidence it adds
relative to its maintenance cost.

## No Host Docker Socket As Isolation

Sharing `/var/run/docker.sock` is not a real isolation boundary. A sandbox runner may use Docker inside a VM or microVM, but it should not hand the agent control over the host Docker daemon for real isolated Rooms.

## Primary Influences

Mitchell Hashimoto's [Building Block Economy](https://mitchellh.com/writing/building-block-economy)
frames the product posture: build small, dependable primitives that focused applications can
compose, and let real use establish which seams deserve to become public.

His [approach to building large technical
projects](https://mitchellh.com/writing/building-large-technical-projects) frames the execution
posture: choose small problems with visible results, solve only enough of each to reach the next
runnable demo, adopt the software early, and let repeated dogfood reveal what to build next.
Dorf therefore advances through tested vertical slices that produce observable Worker and Job
behavior, not through long infrastructure phases whose value appears only at the end.
