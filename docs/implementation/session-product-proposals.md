# Session product: proposed slices

Status: iterative tracker. Slices 1 and 2 have agreed implementation scopes; later slices remain
proposals requiring their own discussion and agreement.

This tracker explores a smaller product centered on one durable Session, with application goals,
evaluation, and external application effects owned by clients. The current
[North Star product boundary](../project/north-star.md#product-boundary) and
[architecture](../project/architecture.md) remain authoritative until an agreed change updates them.
This document does not change supported behavior or public contracts.

## Discussion and tracking

Discuss each slice with the project owner before starting its implementation. Agree on the concrete
scope, remaining questions, compatibility and retirement policy, and verification required for that
slice. Approval of one slice does not approve another, and inclusion here does not authorize work.

The order below is tentative. After each implementation, revisit the remaining proposals using what
was learned. A slice may be reordered, split, combined, revised, or dropped. Material changes to an
agreed scope require another discussion before implementing the changed scope.

Track each slice as **Proposed**, **Discussing**, **Agreed**, **Implementing**, **Verified**, or
**Dropped**. Record the agreed scope and repository-local implementation or verification references
in its section. Do not mark a slice verified until its agreed checks and relevant live proofs pass.
Keep deployment-specific evidence and operational details outside tracked files.

For an agreed consequential choice, follow the
[decision procedure](../../CONTRIBUTING.md#record-a-decision): update the owning authority and add a
decision record. This proposal tracker is not a substitute for either.

## Tentative sequence

| Slice | Status | Proposed result | Questions and evidence to settle before implementation |
| --- | --- | --- | --- |
| 1. Separate review contracts | Verified | Ordinary execution needs no review methods or review controller; existing coding review uses explicit contracts. | Agreed scope and verification are recorded below. |
| 2. Remove investigation | Verified | Retire the built-in investigation workflow; clients use direct Jobs for repository investigation. | Agreed scope is recorded below. |
| 3. Remove coding application | Proposed | Retire coding, review, and publication policy while keeping direct execution. | Discuss the smallest concrete removal; add client primitives only for a proven need. Settle further Session vocabulary and ownership changes in their implementing slices. |
| 4. Dependable setup and activation | Proposed | Hold native delivery until an exact configuration revision is ready; validate provider and harness options in their selected adapters. | Define lifetime-pinned versus changeable settings, profile/package compatibility, field ownership, quiescent updates, and uncertain setup-command outcomes. |
| 5. Session owns its Thread | Proposed | Store the authoritative native conversation binding directly on the execution owner. | Prove uncertain initial acceptance and queued Follow recovery; define legacy binding conversion and conflict handling. Decide when public and internal Job naming changes. |
| 6. Separate delivery from Turn execution | Proposed | Several accepted Message receipts reference one Turn outcome; interruption targets that exact Turn. | Prove accepted, rejected, and uncertain Steers, Auto successor adoption, and preserved native submission attribution. |
| 7. Finish application removal | Proposed | Retire remaining application evidence and obsolete schema. | Settle drain/export/retirement policy, retained generic helpers, and reference-client needs; preserve retained input, ownership, and recovery receipts through append-only migrations. |

```text
Separate review contracts -> remove investigation -> discuss the next slice

Remaining candidate slices:
  product boundary, application removal, setup/activation,
  Session Thread ownership, delivery/Turn facts, schema retirement

Each arrow is a proposed dependency, not approval to start the next slice.
```

### Slice 1: separate review contracts

- Agreed scope: remove mandatory coding-review methods from the shared Harness and Sandbox
  contracts; reuse coding's review-specific interfaces; construct review controllers only when
  coding work needs them. Preserve strict attestation and existing review execution.
- Preserve the current Job name, schema, AgentRuns, delivery semantics, and Absurd lifetime loop.
  No live resource cleanup or application feature removal is part of this slice.
- Decision: [D142](../project/decisions/D142-review-contracts-are-required-only-for-coding.md).
- Implementation: ordinary [Harness](../../internal/terminal/harness.go) and
  [Sandbox](../../internal/sandbox/sandbox.go) contracts omit review methods;
  [runtime composition](../../cmd/dorf/runtime.go) checks coding's contracts only when needed.
- Verification: `mise run check` passed, including PostgreSQL-backed tests, provider/strict-review
  tests, SQL checks, lint, complexity, and vet. `mise run docs:check` passed. Runtime tests exercise
  ordinary adapters without review methods; Codex and Pi tests reject missing review attestation
  for start, recovery, and reads before native access. Profile verification tests use a provider
  with no review methods. Live provider proofs were not rerun; this slice changes interface
  requirements and composition without changing provider lifecycle or native protocol behavior.

### Slice 2: remove investigation

- Agreed scope: delete the investigation package, admission and Message paths, runtime composition,
  CLI/API/client types and report projection, and workflow-specific tests. Remove its source table
  through a new migration and regenerate SQL. Keep direct execution and coding behavior.
- Repository selection, setup, report instructions, and report consumption belong to the direct
  client. No replacement workflow, client framework, legacy executor, or Job conversion is added.
- Generic Job, Message, and resource receipts remain intact. Published migrations remain unchanged.
- Decision: [D143](../project/decisions/D143-retire-built-in-investigation.md).
- Verification: `mise run check` and `mise run docs:check` passed. PostgreSQL migration/replay
  coverage preserves retained Message input and direct resource ownership. Existing direct/coding
  execution and API tests pass. Workflow-specific tests were removed with their implementation;
  no tests were added solely to assert that retired entry points are absent.
- Net physical Go reduction: 1,017 handwritten implementation lines and 845 test lines, excluding
  generated code. No live provider protocol or deployment change was made.

### Slice 3: coding application

- Agreed scope: pending discussion.
- Implementation and verification: pending.

### Slice 4: setup and activation

- Agreed scope: pending discussion.
- Implementation and verification: pending.
- Baseline to preserve: Codex route installation/removal already uses separate managed files;
  client configuration preservation is covered by
  [route lifecycle tests](../../internal/codex/route_test.go). Activation needs its own proof beyond
  that existing behavior.

### Slice 5: conversation ownership

- Agreed scope: pending discussion.
- Implementation and verification: pending.

### Slice 6: delivery and execution facts

- Agreed scope: pending discussion.
- Implementation and verification: pending.

### Slice 7: remaining application removal

- Agreed scope: pending discussion.
- Implementation and verification: pending.

## Deferred proposals

These require separate discussion and agreement; they are not prerequisites for accepting the
Session ownership model.

| Proposal | Status | Evidence needed to consider replacement |
| --- | --- | --- |
| Bounded Session controller | Proposed, deferred | Fault proofs for atomic scheduling, operation dispatch authority, lost wakes, stale workers, and stable retry exhaustion; compare correctness complexity and queue cost with the existing lifetime loop. |
| Shared guest service | Proposed, deferred | Supported-provider proofs for files, processes, streams, cancellation, reconnect, and ownership; demonstrate that equivalent transports can be removed and maintained complexity improves. |

Measure the direct path before claiming performance gains. Queue or transport replacement should
proceed only if its agreed proof demonstrates a benefit and a concrete retirement path.
