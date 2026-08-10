# Durable Job simplification checklist

Status: proposed follow-up work from Issue #94. Discuss the details of each slice before starting it.

Baseline commit: `d2e9d07` (`refactor: adopt Absurd public durability APIs`).

## Target flow

```text
Goal
  -> durable Job
  -> provision Incus Sandbox
  -> Implementation AgentRun
       -> the agent decides when and what to commit
  -> Dorf observes and validates the committed Revision
  -> deterministic Checks
  -> deterministic ReviewPolicy
       -> no review
       -> selected specialist AgentRuns
       -> one general review AgentRun for unknown risk
  -> reviewer text becomes a Message to the original implementation Session
  -> implementation AgentRun decides whether to act
       -> committed change: observe a new Revision; loop
       -> clean unchanged checkout: continue
  -> exact-Revision PR Proposal
  -> accepted, rejected, or abandoned Outcome
  -> separate observable, retryable Cleanup
```

Dorf never creates an implementation commit on the agent's behalf. An AgentRun in the implementation
Session may create one commit or several. Dorf validates the branch, clean workspace, commit and tree,
and proves that the final Git HEAD descends from the previously accepted Revision. It then records that
final HEAD as the next immutable Revision.

## Working rules

- [ ] Discuss and agree on the exact boundary before starting each slice.
- [ ] Keep the complete coding workflow runnable after every slice.
- [ ] Delete superseded code and tests in the same slice that replaces them.
- [ ] Do not maintain old and new workflow paths in parallel.
- [ ] Keep PostgreSQL for product facts and external Action receipts only.
- [ ] Keep Absurd responsible for task eligibility, claims, checkpoints, waits, retries, and cancellation.
- [ ] Run the portable and PostgreSQL-backed suites after every slice.
- [ ] Do not add data compatibility work for the prototype schema.
- [ ] Report production, schema, and test LOC change after every slice, both for that slice and
      cumulatively from the baseline commit.

## Slice 1: Remove unquestionably dead state

Goal: reduce noise without changing workflow behavior.

- [x] Confirm every candidate has no production decision-making caller.
- [x] Remove unused general Job state and observation fields.
- [x] Remove Dorf-owned Action attempt counters and unused timestamps.
- [x] Remove dead enums, legacy setters, unused Check attention, and stored derivable flags.
- [x] Remove duplicated starting-Revision fields where the Job is already authoritative.
- [x] Remove tests that only assert the deleted storage details.

Terminal: the existing workflow behaves identically with a smaller schema and read model.

## Slice 2: Make the coding path explicit

Goal: make ownership obvious before simplifying the full workflow.

```text
Coding workflow
  -> asks the durable core to retain Job, Message, AgentRun, Action, Check, Evidence,
     Attention, and Outcome facts
  -> uses Absurd for durable execution
  -> uses concrete Incus, Codex, Git, and repository-command edges
```

The durable core does not know the order of a coding workflow. `Revision`, `ReviewPolicy`, and
`Proposal` are coding concepts. An `Action` intentionally changes external state. A `Check` verifies
something without intending to change lasting external state. A repository `CommandRun` is one way
the coding edge can execute a Check; it is not a new durable-core primitive.

Do not build a generic workflow engine, adapter registry, or abstract second workflow in this slice.
Keep the implementation concrete until another real workflow proves a reusable seam.

### Slice 2A: Let the AgentRun own commits

Goal: replace Dorf-created commits with a clear observation boundary.

- [x] Update the North Star and decision log: AgentRuns in the implementation Session decide when and
      what to commit; Dorf observes and validates their result.
- [x] Allow an AgentRun to create one commit or several.
- [x] After the AgentRun completes, inspect the coding branch and require a clean workspace.
- [x] Prove that final Git HEAD differs from and descends from the previously accepted Revision.
- [x] Validate that the final commit and tree exist, then record final HEAD as the next immutable
      Revision with Evidence.
- [x] Rename any stored or API relationship that incorrectly promises a direct parent; retain the
      previous accepted Revision as the comparison base.
- [x] Delete the Dorf commit Action, commit command/script, commit workflow phase, and their recovery
      machinery.
- [x] Delete tests whose promise was that Dorf creates an implementation commit or that every Revision
      has exactly one new commit.
- [x] Keep the complete coding workflow runnable and run Checks against the observed exact Revision.

Terminal: a real AgentRun in the implementation Session creates one or more commits, Dorf records its
clean final descendant as one immutable Revision, and Checks run against that exact Revision.

### Slice 2B: Move coding order into one readable workflow

Goal: express `provision Sandbox -> setup -> Implementation AgentRun -> observe Revision -> Checks`
as one concrete coding coordinator backed by named Absurd Steps.

- [x] Introduce one readable `RunJob` coordinator in the coding workflow layer for this path.
- [x] Keep Absurd task, checkpoint, heartbeat, wait, retry, and cancellation mechanics localized at
      the workflow/runtime boundary.
- [x] Give repeated Steps explicit stable names based on Message, AgentRun, Revision, Check, or Action
      identity; never rely on call occurrence counters.
- [x] Use small typed Step results that point back to authoritative Dorf facts.
- [x] Centralize the external Action pattern: reserve stable identity, reconcile external truth,
      perform only if missing, perform a final claim check, record the receipt, then complete the Step.
- [x] Use the same Action executor for Sandbox, route, and other code-owned external mutations in this
      path.
- [x] Execute repository setup and declared repository checks as concrete CommandRuns at the coding
      edge, represented durably as Actions or Checks according to whether lasting mutation is intended.
- [x] Keep locks and database transactions short; do not hold them across Incus, Codex, Git, or command
      execution.
- [x] Delete the matching orchestration branches from the mixed service layer as the coordinator
      becomes authoritative. `workflow_phase` remains a transitional domain guard and handoff projection
      until Slice 6; there is no second service-layer coordinator interpreting it.
- [x] Delete private phase-transition and duplicate recovery tests replaced by product-path and shared
      Action-reconciliation tests.

Terminal: the first half of `RunJob` reads in product order, survives interruption through stable
Steps, and contains no second Dorf-owned program counter for the replaced path.

The workflow currently keeps `workflow_phase` because domain transactions, review, and publication still
use it as a guard and handoff projection. Slice 6 deletes it after those downstream paths are explicit.

Live proof (2026-08-10): Job `job-b3fc6dbd0069a0574f5a` ran an implementation AgentRun, observed
Revision `f5efad68821fa0edabe7898fd13d19bb5d06d9e0`, passed `check` and `smoke`, selected no review,
created exact-Revision PR #102, then completed abandoned-Outcome cleanup with its route revoked and
Sandbox deleted.

## Slice 3: Use one AgentRun mechanism and one Message follow-up path

Implementation and review are not different execution systems. They are AgentRuns with different
Roles, prompts, Revision inputs, workspaces, and capability envelopes. User text, Check output, and
reviewer text are not different follow-up primitives. They are Messages to the original implementation
Session.

```text
AgentRun
  +-- Role
  +-- prompt
  +-- input Revision
  +-- capability envelope
  +-- workspace

Message
  +-- text
  +-- from: human, agent, or workflow
  +-- from ID: request, AgentRun, or Check ID
  +-- Job-local sequence
```

`Feedback` describes how a Message is being used; it is not another durable primitive. `FromKind`
names the sender: a human, an agent, or the workflow. `FromID` retains the exact request, AgentRun, or
Check that caused the Message. The AgentRun that consumes the Message supplies the other half of the
history: `sender -> Message -> handling AgentRun`. Add reply or thread fields only when a real
outward-response feature needs them.

### Slice 3A: Use one AgentRun runner

Goal: make submission, native recovery, waiting, and terminal recording one durable mechanism.

- [x] Define one small AgentRun execution contract containing only the durable run identity and the
      concrete harness operations needed to inspect, submit, and wait.
- [x] Use one runner for initial and follow-up implementation, general review, and specialist review.
- [x] Keep Role-specific prompt construction outside the runner.
- [x] Keep reviewer Sandbox creation, immutable checkout, scoped route creation, and capability
      validation outside the runner as preparation facts.
- [x] Keep reviewer output as opaque text; persist the cross-AgentRun handoff as a Message, with no
      review-result parser or finding schema.
- [x] Give every Message explicit `FromKind` and `FromID`; use the same pair as its stable
      admission identity and remove the caller-only naming.
- [x] Reconcile missing acknowledgements through the same native-history path for every AgentRun.
- [x] Give every run one stable `dorf/agent-run/v1/<AgentRun ID>` Absurd Step.
- [x] Delete the separate implementation and review submission/recovery state machines.
- [x] Replace duplicated native-status and uncertainty tests with one runner recovery contract plus one
      reviewer ownership/capability test.
- [x] Dogfood one normal implementation AgentRun and one selected read-only review AgentRun.

Terminal: implementation and selected review both execute through the same AgentRun runner without
weakening reviewer isolation or exact-Revision ownership.

### Slice 3B: Put the review feedback loop directly in `RunJob`

Goal: make ReviewPolicy, review, Message delivery, and Revision observation one readable product path.

- [x] Replace the coarse review continuation with explicit Revision-scoped Steps in `RunJob`.
- [x] Keep `ReviewPolicy(ChangeFacts) -> ReviewPlan` pure and deterministic.
- [x] Run one general read-only reviewer when policy reports unknown risk; do not use reviewer prose
      as a router protocol.
- [x] Run each selected specialist through the shared AgentRun runner in its prepared read-only
      workspace.
- [x] Turn each reviewer's returned text into one stable Message to the original implementation Session.
- [x] Deliver human, workflow, and agent Messages through the same implementation AgentRun path.
- [x] Let the implementation agent decide whether to act, ignore, or explain; Dorf does not classify
      the text as clear, material, a suggestion, or a finding.
- [x] Observe the implementation AgentRun's Git result: a new commit loops through Checks and policy;
      a clean unchanged checkout completes review for that Revision.
- [x] Use one deterministic readiness calculation for no-review and completed-review Revisions.
- [x] Delete `advanceReview`, the coarse review Step, redundant review phases, and duplicated readiness
      branches once `RunJob` is authoritative.
- [x] Delete review-result structs, finding persistence, output parsers, adjudication states, and
      review-specific repair counters once this path is authoritative.
- [x] Dogfood no-review and selected-review paths; let later dogfood expose further feedback edge cases.

Terminal: no-review, known-role review, and general review all follow the visible `RunJob` story; review
text reaches the original implementation Session through Message and no parser decides what it means.

Live proof (2026-08-10): Job `job-dd819a66732c3e7c556c` ran an implementation AgentRun, observed
agent-created Revision `395362a33eff41cec67f746259b29df00d36e87c`, passed `check` and `smoke`,
ran a selected general reviewer in an isolated read-only Sandbox, delivered its opaque text as an
agent Message to the original implementation Session, created exact-Revision PR #103, then completed
abandoned-Outcome cleanup with both routes revoked and both Sandboxes deleted. The run exposed and
closed one shared-runner adapter bug: the adopted reviewer controller must remain visible during an
active native wait.

### Slice 3C: Add compiler-checked SQL queries before further storage changes

Goal: get compiler-like early feedback for PostgreSQL query and schema changes while preserving the
existing Store, transaction, domain, and behavioral-test boundaries.

`sqlc` is a query compiler for this slice, not an ORM, migration owner, repository layer, or source of
domain types. The generated package remains a private implementation detail of `postgres.Store`.
Store methods continue to own transactions, state-transition checks, compare-and-set expectations,
error translation, and conversion to `spine` types. PostgreSQL integration tests continue to prove
locking, constraints, concurrency, transaction behavior, and recovery semantics that generation
cannot establish.

- [x] Agree on the exact generated-package and query-file boundary before adding the tool.
- [x] Pin `sqlc` as repository-owned development tooling and add deterministic generate and stale-code
      check commands. Do not add a runtime service, cloud dependency, or migration framework.
- [x] Generate against the clean Dorf baseline schema using the existing `database/sql` surface.
- [x] Keep generated records and parameter structs inside `internal/postgres`; do not expose them from
      Store methods or replace `spine` domain types with database-generated types.
- [x] Prove the approach first on the representative `Job`, `Messages`, `Actions`, `Checks`, and
      `Evidence` read paths. Delete each replaced inline query, manual `Scan` list, and row loop in the
      same change; do not retain parallel generated and handwritten implementations of one query.
- [x] Prove one representative transactional path using generated queries bound to the Store-owned
      `*sql.Tx`. Keep transaction begin, commit, rollback, and product invariants in the handwritten
      Store method.
- [x] Record the final integrated broad-pass measurement: handwritten production Go fell from 12,062
      to 11,973 lines (-89), tests fell from 7,670 to 7,651 lines (-19), ten named query files contain
      1,210 lines, the private generated package contains 4,960 lines, and the three new tool/config
      entry points contain 81 lines. Local generation takes about 0.63 seconds and stale-code diffing
      about 0.61 seconds. All 188 stable product query call sites moved behind sqlc; the 12 remaining
      direct Store calls are Absurd/bootstrap, schema application, and the Job advisory lock. No
      supported product query or type was blocked; nullable timestamps and the repeated review
      projection required explicit Store mapping and one database view.
- [x] Continue after the accepted trial demonstrated useful schema/query drift failures and removed
      embedded SQL and handwritten scan plumbing while preserving the Store boundary.
- [x] Convert the remaining stable Dorf-owned queries in bounded groups and
      delete their superseded scanners. Keep schema bootstrap, migrations, Absurd diagnostics, and
      genuinely unsupported exceptional SQL explicit.
- [x] Run the portable and PostgreSQL integration/fault suites and keep the complete coding workflow
      runnable throughout the conversion.

Terminal: an incompatible Dorf schema or query change fails deterministically before tests; Store
methods still express the same transactions and product invariants; no generated database type leaks
into the domain; the PostgreSQL-backed coding workflow behaves identically; and the measured
handwritten persistence surface is materially smaller. If those conditions are not met, `sqlc` is
removed without residue.

## Slice 4: Simplify Proposal and Outcome

Goal: make an exact-Revision Proposal the direct successor of readiness.

- [x] Execute publication through named, Revision-scoped Actions and Steps.
- [x] Express publication visibly as `push exact Revision -> create or adopt exact PR`.
- [x] Reconcile Git and GitHub before repeating an uncertain effect.
- [x] Use Absurd retry instead of a Dorf publication retry scheduler.
- [x] Keep Proposal and Outcome as Dorf product facts.
- [x] Remove the publication-specific task attachment; the main Job task owns publication and
      observation.
- [x] Delete publication polling tasks, attachment verification, mirrored task state, and custom
      retry state. Keep one bounded exact-PR observation loop in the main task.
- [x] Consolidate duplicate publication adoption tests around Action and Proposal invariants.
- [x] Turn trusted owner/collaborator pull-request comments into idempotent human Messages.
- [x] Acknowledge accepted feedback and reconcile one exact-Revision completion reply at the GitHub edge.
- [x] Re-observe GitHub after each durable wake or timeout without a second poll counter or observation Step.
- [x] Observe merge as acceptance and close without merge as rejection.
- [x] Dogfood comment feedback and a terminal pull-request outcome through one real Dorf Job.

Terminal: a ready Revision produces at most one exact-Revision PR Proposal and can reach an explicit
Outcome.

## Slice 5: Isolate Cleanup

Goal: leave Cleanup as the only independently scheduled, observable lifecycle task.

- [ ] Close admission and request cancellation before cleanup starts destructive reconciliation.
- [ ] Reconcile unsettled AgentRuns and Actions before deleting resources.
- [ ] Represent reviewer and main provider routes as one list of owned resources with revoke Actions.
- [ ] Represent reviewer and main Sandboxes as one list of owned resources with delete Actions.
- [ ] Use the same external Action executor as the ordinary and publication paths.
- [ ] Record cleanup attention or completion as a Dorf product fact.
- [ ] Use stable cleanup Action identities and Absurd retry.
- [ ] Delete duplicated per-resource cleanup orchestration, broad Job fencing, and task-state mirrors.
- [ ] Consolidate cleanup tests around partial success and retry.

Terminal: cleanup can fail visibly, retry safely, and eventually prove that all owned resources are
gone.

## Slice 6: Delete the old program counter and shrink the tests

Goal: leave one obvious workflow story with no second durable scheduler in Dorf.

- [ ] Remove `workflow_phase` as sequencing authority.
- [ ] Derive the next operation from retained product facts: resources, AgentRuns, Revisions, Checks,
      ReviewPlans, Proposals, Outcomes, and cleanup receipts.
- [ ] Remove review and repair counters; derive follow-up history from Messages, AgentRuns, and review
      provenance.
- [ ] Replace copied run/publication/cleanup task columns with one minimal task-handle shape, or remove
      them where Absurd identity is sufficient.
- [ ] Remove duplicated Action outcome prose when immutable Evidence already owns the detail.
- [ ] Remove review resource Action-ID and state columns that are deterministic or already represented
      by Actions and receipts.
- [x] Remove `RunUntilIdle`, cycle results, and synthetic cycle checkpoints in Slice 2B; remove the
      remaining review-oriented disposition once Slice 3B no longer needs it.
- [ ] Remove remaining task attachment and cancellation plumbing made unnecessary by the final task
      shape.
- [ ] Add one central Job inspection read model.
- [ ] Keep a small set of end-to-end product-story tests.
- [ ] Keep pure policy, readiness, identity, and publication decision tests.
- [ ] Keep one shared external-Action crash/reconciliation contract test.
- [ ] Delete private-helper, phase-transition, duplicated status-matrix, and raw-row behavioral tests.
- [ ] Report before/after production LOC and name any remaining complexity centers.

Terminal: the main task reads in the same order as the target flow, and tests protect product promises
rather than internal choreography.

## Issue #94 closure gate

- [ ] The real Job sequence, not only an outer cycle, uses stable typed Absurd Steps.
- [ ] Checkpoint names and result shapes are treated as versioned persisted contracts.
- [ ] Production external effects use stable Dorf Actions and reconcile external truth.
- [ ] Every accepted Message has one immutable wake identity while PostgreSQL remains its authority.
- [ ] Production uses only public Absurd spawn, result, cancel, retry, heartbeat, and event APIs.
- [ ] Production never reads or mutates raw Absurd task tables.
- [ ] The production external-Action path performs a final claim check before recording success.
- [ ] Claim-expiry and cancellation tests exercise that actual production path.
- [ ] Absurd operational history comes from `absurdctl` or Habitat, not Dorf tables.
- [ ] The prototype uses one clean baseline schema pinned to Absurd 0.5.0.
- [ ] Portable and PostgreSQL integration/fault suites pass.
- [ ] The before/after production LOC and remaining complexity centers are reported.
