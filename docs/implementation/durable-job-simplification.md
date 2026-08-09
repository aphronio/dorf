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
  +-- user text, Check output, or reviewer text
  +-- target implementation Session
```

### Slice 3A: Use one AgentRun runner

Goal: make submission, native recovery, waiting, and terminal recording one durable mechanism.

- [ ] Define one small AgentRun execution contract containing only the durable run identity and the
      concrete harness operations needed to inspect, submit, and wait.
- [ ] Use one runner for initial and follow-up implementation, general review, and specialist review.
- [ ] Keep Role-specific prompt construction outside the runner.
- [ ] Keep reviewer Sandbox creation, immutable checkout, scoped route creation, and capability
      validation outside the runner as preparation facts.
- [ ] Keep reviewer output as opaque text; persist the cross-AgentRun handoff as a Message, with no
      review-result parser or finding schema.
- [ ] Reconcile missing acknowledgements through the same native-history path for every AgentRun.
- [ ] Give every run one stable `dorf/agent-run/v1/<AgentRun ID>` Absurd Step.
- [ ] Delete the separate implementation and review submission/recovery state machines.
- [ ] Replace duplicated native-status and uncertainty tests with one runner recovery contract plus one
      reviewer ownership/capability test.
- [ ] Dogfood one normal implementation AgentRun and one selected read-only review AgentRun.

Terminal: implementation and selected review both execute through the same AgentRun runner without
weakening reviewer isolation or exact-Revision ownership.

### Slice 3B: Put the review feedback loop directly in `RunJob`

Goal: make ReviewPolicy, review, Message delivery, and Revision observation one readable product path.

- [ ] Replace the coarse review continuation with explicit Revision-scoped Steps in `RunJob`.
- [ ] Keep `ReviewPolicy(ChangeFacts) -> ReviewPlan` pure and deterministic.
- [ ] Run one general read-only reviewer when policy reports unknown risk; do not use reviewer prose
      as a router protocol.
- [ ] Run each selected specialist through the shared AgentRun runner in its prepared read-only
      workspace.
- [ ] Turn each reviewer's returned text into one stable Message to the original implementation Session.
- [ ] Deliver user, Check, and reviewer Messages through the same implementation AgentRun path.
- [ ] Let the implementation agent decide whether to act, ignore, or explain; Dorf does not classify
      the text as clear, material, a suggestion, or a finding.
- [ ] Observe the implementation AgentRun's Git result: a new commit loops through Checks and policy;
      a clean unchanged checkout completes review for that Revision.
- [ ] Use one deterministic readiness calculation for no-review and completed-review Revisions.
- [ ] Delete `advanceReview`, the coarse review Step, redundant review phases, and duplicated readiness
      branches once `RunJob` is authoritative.
- [ ] Delete review-result structs, finding persistence, output parsers, adjudication states, and
      review-specific repair counters once this path is authoritative.
- [ ] Dogfood no-review and selected-review paths; let later dogfood expose further feedback edge cases.

Terminal: no-review, known-role review, and general review all follow the visible `RunJob` story; review
text reaches the original implementation Session through Message and no parser decides what it means.

## Slice 4: Simplify Proposal and Outcome

Goal: make an exact-Revision Proposal the direct successor of readiness.

- [ ] Execute publication through named, Revision-scoped Actions and Steps.
- [ ] Express publication visibly as `push exact Revision -> create or adopt exact PR`.
- [ ] Reconcile Git and GitHub before repeating an uncertain effect.
- [ ] Use Absurd retry instead of a Dorf publication retry scheduler.
- [ ] Keep Proposal and Outcome as Dorf product facts.
- [ ] Replace publication-specific task attachment fields with the smallest task handle needed for
      correlation or cancellation.
- [ ] Delete publication polling, attachment verification, mirrored task state, and custom retry state.
- [ ] Consolidate duplicate publication adoption tests.

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
