# Session product: proposed slices

Status: iterative tracker. Slices 1–3, 5, and 5a are complete. Slice 6 is withdrawn. Slice 8
records the agreed thin native boundary and completed capability review; the runtime replacement
in slice 9 is implemented with local verification; live provider proofs remain. Each implementation slice is discussed before it starts.

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
| 6. Separate delivery from Turn execution | Dropped | Withdraw the uncommitted shared-Turn schema and delivery refactor. | D148 assigns messages and Turns to the Harness; do not add another durable aggregate. |
| 7. Finish application removal | Dropped | Application evidence and application-only tables are removed with coding. | Remaining Message/AgentRun removal belongs to the native API replacement. |
| 8. Native boundary and capability proof | Verified | Record the thin control-plane contract, inspect Codex and client use, and remove the abandoned shared-Turn patch. | Source and isolated-server evidence are recorded below; runtime behavior is unchanged. |
| 9. Replace queued Messages with native input and observation | Implementing | One ready-Session send path, native history and controls, coordinated client replacement, and deletion of the old input pipeline. | Resolve exact event shapes and the capability review's input, uncertainty, observation, maintenance and recovery proofs; no new streaming capability is required. |

```text
Completed: separate review contracts -> remove investigation -> remove coding
Completed next: Job Thread ownership (slice 5, before setup/activation)

Completed: Session naming (slice 5a)
Completed: audited leftover removal
Completed: thin native boundary and capability review (slice 8)
Withdrawn: shared Turn ownership (slice 6)
Implemented: native input/API/client replacement (slice 9); provider proofs outstanding
Deferred: setup/activation, pending what the native surface already provides

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

- Dropped after reconsidering the product boundary. The uncommitted shared-Turn implementation,
  generated SQL, migration, dedicated tests, and completion claims were removed.
- Do not replace the old Message/AgentRun aggregate with new Message-delivery and Turn aggregates.
  The accepted native boundary assigns conversation and execution to the Harness.
- No deployment used the withdrawn migration. The disposable development database was backed up
  outside the repository and rebuilt from the retained migrations, removing the abandoned schema.

### Slice 7: remaining application removal

- Folded into slice 3 to remove application evidence and storage with their final consumers.

### Slice 8: native boundary and capability proof

- Agreed scope: revise responsibilities around confirmed native acceptance; inspect the Codex
  app-server surface, pinned source and current client usage; verify uncertain assumptions against
  an isolated server; remove stale shared-Turn work and update the owning documents.
- Decision: [D148](../project/decisions/D148-thin-native-session-control-plane.md).
- Evidence: [native capability review](native-session-contract.md). The client retains its own
  application input but depends on current Message replay, observations, effective intent and
  interruption. Replacement requires a coordinated client change.
- Verification: the isolated real Codex probe confirms native start-or-steer, repeated-ID duplicate
  submission, cold completed history, and an acknowledged steering input absent after process loss.
  Source establishes acknowledgement before persistence. No production resource or live model
  was used. `mise run docs:check` passes. This verifies the bounded review; it does not claim the
  new API is implemented.

### Slice 9: native input and observation replacement

- Agreed scope: replace the current Message-facing contract and consumer path with ready-Session
  input, native history/events, and controls. Remove FIFO/Auto selection, durable Message input,
  AgentRun outcomes, delivery wakes, public aliases, and tests specific to retired semantics when
  the replacement is authoritative. No shared Turn table or second production execution path.
- Keep configuration, compute, model access, Session binding, exact ownership and release. Keep
  existing file/process operations until native alternatives prove smaller and equally useful.
- Vocabulary: use Session events for input, controls and native notifications, with native Turns
  and history for reads. No separate input resource or required new streaming capability; adapt
  the existing SSE observation path when retiring its Message dependency. The
  [native contract](native-session-contract.md#responsibility-and-proposed-surface) owns these semantics.
- Settle exact event shapes, attachment handling, application tool output, developer
  instructions and refresh behavior against the native capabilities before implementation.
- Prove unknown-send behavior and remove client retries based on the retired Message replay
  contract. Preserve client reply publication without Follow/Steer-based ownership.
- Replace Message-dependent pause, upgrade, checkpoint and release guards before removing their
  storage. Native idle status or missing history alone cannot prove absence of an unresolved send.
- Implemented: Session events, native Turn/history reads and adapted SSE; direct Codex submission
  without an input queue; monotonic mutation/uncertainty guard; native checkpoint continuity proof;
  coordinated consumer replacement; deletion of old Message/AgentRun storage and transport.
- Verification: PostgreSQL and protocol tests plus the isolated real Codex input/restart probe.
  [Implementation evidence](native-session-contract.md#implementation-and-verification) records the
  limits. Live provider pause, upgrade and recovery proofs remain outstanding; no deployment was
  migrated. Native lifecycle scheduling and guest transport replacement remain separate.

## Client API review

Apply [Build a small thing that composes](../project/principles.md#build-a-small-thing-that-composes):
inspect real callers, describe the client simplification, and keep application policy outside Dorf.
Consumer-specific source evidence belongs outside this public repository.

Session naming, application retirement and native input/observation replacement are implemented.
The client no longer reconciles effective delivery intent or discovers a Turn through Message
receipts. Native execution and application reply publication retain their separate responsibilities.
See slice 9 for verification limits.

Workspace access convenience and explicit setup/activation remain proposals. Evaluate the existing
create → prepare → send flow and native configuration capabilities before adding public resources,
barriers, or revision models. Profile naming remains deferred.

## Post-Session cleanup audit

Status: implemented and verified. Cleanup was agreed before implementation and completed in
small commits. No deployment was changed. Decision: [D147](../project/decisions/D147-remove-residual-application-machinery.md).

| Completed cleanup | Result | Retained proof |
| --- | --- | --- |
| Remove the unused FIFO wake channel | Message admission signals only the execution wake consumed by the Session loop. The unused waiter, sequence query and dedicated tests are gone. | Durable Message order, timeout reconciliation and admission replay. |
| Remove unused helper and compatibility paths | No Core file-reading duplicate, named-Sandbox allocator, ordinary scheduling wrapper, or pre-atomic cleanup polling loop. Default provisioning uses the admission reservation. | Atomic admission/cleanup races, public file access ownership and worker cancellation. |
| Remove application selectors | No Session workflow identity or AgentRun role, capability, input revision or strict-review nonce. The envelope and prompt resolver are gone. | Exact Session/Sandbox ownership, native acceptance, interruption, checkpoint boundaries and preserved migration facts. |
| Delete obsolete tests and contract residue | Removed retired coding/investigation fixtures, foreign-workflow rejection cases and the unused `GitCommitOID` schema. | Fresh initialization, idempotent migration, direct input/resource preservation and conflicting native-binding proofs. |

Failure attention remains as `ExecutionAttention`; operation fault barriers still support recovery
proofs. The migration preserves recorded attention and the Session listing index. Full `mise run
check` passed against the migrated disposable PostgreSQL database, including SQL preparation,
Go tests, lint, complexity and vet. The bounded checkpoint scan test now places and retires its own
fixture so accumulated fixtures cannot displace it from the scan page.

After slice 9, queue names, payload fields and wake/fence keys use Session names. The agreed
transition assumes no outstanding old queue work; restart API and workers together. Opaque IDs and
provider ownership labels still identify retained compute. Changing those requires a resource
transition, independent of queue retirement.

The native-contract cleanup removes unused Pi submission/history code and tests, AgentRun-shaped
Codex test bindings, Message-shaped consumer fixtures, and unreachable public execution states.
Pre-contract checkpoint references are retired by the migration; no runtime compatibility flag
remains. Published migration history and meaningful resource/native safety tests remain.

Effect fences, infrastructure retry receipts, resource generations and honest native uncertainty
remain control-plane responsibilities. D148 changes input custody; re-evaluate the existing
controller after removing its delivery work rather than preserving or replacing it by default.

## Deferred proposals

These require separate discussion and agreement; they are not prerequisites for accepting the
Session ownership model.

| Proposal | Status | Evidence needed to consider replacement |
| --- | --- | --- |
| Bounded Session controller | Proposed, deferred | Fault proofs for atomic scheduling, operation dispatch authority, lost wakes, stale workers, and stable retry exhaustion; compare correctness complexity and queue cost with the existing lifetime loop. |
| Shared guest service | Proposed, deferred | Supported-provider proofs for files, processes, streams, cancellation, reconnect, and ownership; demonstrate that equivalent transports can be removed and maintained complexity improves. |

Measure the direct path before claiming performance gains. Queue or transport replacement should
proceed only if its agreed proof demonstrates a benefit and a concrete retirement path.
