# Session product: proposed slices

Status: iterative tracker. Slices 1–3, 5, and 5a have agreed implementation scopes; other slices remain
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
| 3. Remove coding application | Verified | Retire coding, review, and publication policy while keeping direct execution. | Discuss the smallest concrete removal; add client primitives only for a proven need. Settle further Session vocabulary and ownership changes in their implementing slices. |
| 4. Dependable setup and activation | Proposed | Hold native delivery until an exact configuration revision is ready; validate provider and harness options in their selected adapters. | Define lifetime-pinned versus changeable settings, profile/package compatibility, field ownership, quiescent updates, and uncertain setup-command outcomes. |
| 5. Job owns its Thread | Verified | Store the authoritative native conversation binding directly on the execution owner. | Prove uncertain initial acceptance and queued Follow recovery; define legacy binding conversion and conflict handling. Decide when public and internal Job naming changes. |
| 5a. Session naming | Verified | Rename the existing execution context and its client contract; retain one primary Thread and derive Harness from the pinned profile. | Agreed scope is recorded below. |
| 6. Separate delivery from Turn execution | Proposed | Several accepted Message receipts reference one Turn outcome; interruption targets that exact Turn. | Prove accepted, rejected, and uncertain Steers, Auto successor adoption, and preserved native submission attribution. |
| 7. Finish application removal | Dropped | Application evidence and application-only tables are removed with coding. | Remaining generic Job/AgentRun fields belong to their future ownership slices. |

```text
Completed: separate review contracts -> remove investigation -> remove coding
Completed next: Job Thread ownership (slice 5, before setup/activation)

Completed: Session naming (slice 5a)
Current: remove audited leftovers before new feature slices
Remaining candidates: setup/activation, delivery/Turn facts

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

- Agreed scope: remove coding, review, publication, outcomes, GitHub integration, their CLI/API
  surfaces, application Evidence storage, and strict-review adapter methods. Keep direct execution.
- Delete dedicated packages and tests. Preserve meaningful FIFO, recovery, retry, and cleanup
  coverage using direct Job fixtures; do not test absence of retired features.
- Add an append-only migration for application tables. Preserve generic input, native attribution,
  resource ownership, lifecycle, and recovery records. No conversion or legacy executor is added.
- Job naming, Thread/Turn ownership, configuration activation, and the existing controller remain
  separate proposals. No replacement workflow framework or reference client is introduced.
- Decision: [D144](../project/decisions/D144-retire-built-in-coding.md).
- Verification: `mise run check` and `mise run docs:check` passed, including SQL regeneration,
  PostgreSQL-backed migration/replay, direct FIFO/steering, cleanup races, retry fencing, provider
  ownership, lint, complexity, and vet. Application-only fixtures were removed; retained durability
  cases now use direct Jobs. Final import/comment cleanup passed `mise run lint`.
- Net physical Go reduction: 6,655 handwritten implementation lines, 5,582 test lines, and 1,383
  generated lines compared with the prior committed slice.
- Direct provider and native protocol behavior is retained. Live provider proofs were not rerun;
  the migration was applied only to the disposable test database. No deployment was changed.

### Slice 4: setup and activation

- Agreed scope: pending discussion.
- Implementation and verification: pending.
- Baseline to preserve: Codex route installation/removal already uses separate managed files;
  client configuration preservation is covered by
  [route lifecycle tests](../../internal/codex/route_test.go). Activation needs its own proof beyond
  that existing behavior.

### Slice 5: conversation ownership

- Agreed scope: one authoritative Harness/Thread binding on each direct Job. Bind it atomically
  with proven native acceptance; use it for subsequent Follows and conversation timeline reads.
- Remove backward Thread discovery and repeated scans of prior runs. Keep exact AgentRun
  attribution, uncertain-submission recovery, FIFO/steering, Job naming, and the existing controller.
- Move this ahead of setup/activation: creation already starts no conversation, so clients can
  prepare their workspace before sending input. Managed route files already preserve client settings.
- Backfill consistent direct history through an append-only migration; reject conflicting bindings.
  Stop old workers for migration rather than retaining a second read/write path.
- Decision: [D145](../project/decisions/D145-direct-job-owns-its-thread.md).
- Verification: `mise run check` passed, including PostgreSQL migration/replay and conflict checks,
  initial-acceptance recovery with queued Follows, concurrent binding/receipt atomicity, and timeline
  custody. `mise run docs:check` passed. Native protocols are unchanged; no live provider proof
  was required or rerun. Only the disposable test database was migrated.
- Net physical Go change: 42 fewer handwritten implementation lines, 45 fewer generated lines,
  and 99 additional test lines. No deployment was changed.

### Slice 5a: Session naming

- Agreed scope: rename Job to Session across Go, SQL storage, public API, CLI, and existing clients.
  Use `ThreadID` for the current input target. Derive Harness from the immutable admitted
  profile; remove the redundant stored binding field. Profile naming is deferred.
- Preserve opaque IDs, request keys, accepted input, native Thread/Turn attribution, resource
  ownership, queued work, and execution behavior. No public aliases or parallel compatibility path.
- Session-level input targets its bound Thread. Additional explicitly addressed Threads can be
  considered later; this slice adds no collection, table, or multi-Harness dispatch.
- Client simplification: one concrete Session response across creation, observation, and cleanup.
  Remove the kind discriminator, common/view wrappers, and kind-based decoding. Configuration,
  delivery/Turn separation, and controller replacement remain separate.
- Decision: [D146](../project/decisions/D146-name-the-execution-context-session.md).
- Verification: `mise run check` and `mise run docs:check` passed. Final OpenAPI/client checks and
  PostgreSQL baseline migration checks passed after the schema-reference and profile-validation
  refinements. The migration preserves the bound Thread and rejects a profile/Harness mismatch.
  Existing client flows, generated contracts, types, and migration checks passed.
- Net physical Go change: 87 fewer handwritten implementation lines, 29 fewer test lines, and
  8 additional generated lines. Published migrations remain unchanged; the new migration renames storage.
  Only disposable test databases were migrated. No deployment was changed.

### Slice 6: delivery and execution facts

- Agreed scope: pending discussion.
- Implementation and verification: pending.

### Slice 7: remaining application removal

- Folded into slice 3 to remove application evidence and storage with their final consumers.

## Client API review: proposals to discuss

Apply [Build a small thing that composes](../project/principles.md#build-a-small-thing-that-composes):
inspect real callers before changing the contract, describe the client simplification, and keep
application policy outside Dorf. Consumer-specific source evidence belongs outside this public
repository. Except for naming in slice 5a, these candidates have not been approved for implementation.

| Candidate | Client simplification | Scope to settle |
| --- | --- | --- |
| Align existing consumers with application retirement | A client can regenerate its models and use the supported direct contract without retired workflow types or dispatch paths. | Audit actual callers before deployment. Retire unused application flows or implement their policy in the client through existing primitives. No workflow restoration in Core. |
| Job → Session vocabulary (agreed in slice 5a) | One consistent name for the durable context across creation, continued input, inspection, and release. | Agree on a coordinated API/client change, retained ID and request-key behavior, and concrete deployment order. A rename alone does not simplify execution or justify another resource. |
| Workspace access through the Session handle | Clients need not repeatedly retrieve a collection and select the default Sandbox before reading files or executing commands. | First compare a small client helper with a public API change. Preserve readiness, delivery holds, exact ownership, bounded files, and unknown command outcomes. Resource generations remain internal custody. |
| Separate Message delivery from Turn observation | Clients can distinguish input acceptance from the shared execution outcome without reconstructing it across steering Messages. | Keep this in slice 6; demonstrate an actual reduction in client reconciliation while retaining reply ordering, cursor gaps, completion watermarks, and exact interruption. |

Use **Session** for the durable public context, **Message** for admitted input, **Turn** for native
execution, and **Sandbox** for supported isolated compute in the proposed vocabulary. **Thread**
identifies the primary native conversation receiving input; native subagent threads remain
Harness-owned. A Session ID need not equal any native Thread or session ID. Saved Agent resources,
a second transcript store, and additional configuration lifecycle states need their own use case.

Consumer contract alignment and naming are complete. Discuss workspace access next only against
a concrete client diff. Setup/activation remains proposed; the existing create → prepare → send
sequence must be evaluated before adding another barrier or configuration revision model.

## Post-Session cleanup audit

Status: cleanup implementation agreed. Remove the audited leftovers in small commits before
resuming feature slices. Preserve active Session custody; no deployment reset or migration is
authorized by this cleanup.

| Proposed cleanup | Evidence in the current implementation | Boundary to preserve |
| --- | --- | --- |
| Remove the unused FIFO wake channel | `EmitMessageWake` emits both Message and execution wakes, but `AwaitMessageWake` has no callers. The direct loop waits only on `AwaitSessionExecutionWake`. `NextWakeSequence` also has no callers. | Retain the execution wake, durable Message order, timeout reconciliation, and admission replay behavior. Delete tests of the retired wake channel with its code. |
| Remove unused application helper paths | `SandboxHandle.ReadFile` and `EnsureNamedSandbox` have only test callers. The public file API uses `controlreader.Service`. The public Core `ScheduleSessionTask` wrapper also has only test callers. | Retain file ownership/cleanup proofs at the live API boundary, default provisioning, atomic admission scheduling, and cleanup task handoff. Resource-generation history is still required. |
| Remove application selectors from current storage and execution | Session workflow name/revision and AgentRun role/capability/input revision still cross admission, SQL, projection, and validation. The sole production envelope fixes the role to direct and leaves capability/revision empty. | Keep exact Session/Sandbox ownership, native delivery attribution, and interruption. Simplify the envelope and prompt resolver as their application branches disappear; do not split Turn storage in this cleanup. |
| Delete obsolete fixtures and contract residue | Admission tests still include a foreign coding workflow; baseline migration tests preserve retired coding/investigation records. OpenAPI `GitCommitOID` is unreachable from all paths. | Keep fresh initialization, idempotent migration, direct input/resource preservation, and conflicting native-binding proofs. Published migration history does not require preserving retired application fixtures. |

Progress: the unused FIFO wake channel and its query/tests are removed. Message admission signals
only the execution wake already consumed by the Session loop. Unused Core file reading, named
Sandbox reservation, ordinary task scheduling, and the old cleanup-request polling loop are removed.
Default provisioning uses the reservation already committed by admission. The cleanup race proof
now enters through atomic admission; file access proofs remain at the active control-reader boundary.

`WorkflowAttention` is a misleading name for still-used execution, provider, upgrade, and recovery
attention. Rename that responsibility rather than delete it. Similarly, fault barriers still support
real failure proofs even where their method names contain Workflow.

Opaque IDs, provider ownership labels, queued task payloads, and task/event names retain older Job
strings. They do not implement a second public API. Distinguish those existing authority references
from unnecessary compatibility branches; any change to retained identities needs an explicit
transition for active resources and work. Do not retain unused fields solely for nonexistent
application history.

The controller, effect fences, retry receipts, resource generations, and ambiguous native acceptance
remain active responsibilities. Their replacement is not justified by a naming or dead-code cleanup.

## Deferred proposals

These require separate discussion and agreement; they are not prerequisites for accepting the
Session ownership model.

| Proposal | Status | Evidence needed to consider replacement |
| --- | --- | --- |
| Bounded Session controller | Proposed, deferred | Fault proofs for atomic scheduling, operation dispatch authority, lost wakes, stale workers, and stable retry exhaustion; compare correctness complexity and queue cost with the existing lifetime loop. |
| Shared guest service | Proposed, deferred | Supported-provider proofs for files, processes, streams, cancellation, reconnect, and ownership; demonstrate that equivalent transports can be removed and maintained complexity improves. |

Measure the direct path before claiming performance gains. Queue or transport replacement should
proceed only if its agreed proof demonstrates a benefit and a concrete retirement path.
