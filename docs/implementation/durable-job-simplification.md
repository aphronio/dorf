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
       -> bounded triage, then selected specialist AgentRuns
  -> material finding?
       -> yes: original Session repairs and commits a new Revision; loop
       -> no: continue
  -> exact-Revision PR Proposal
  -> accepted, rejected, or abandoned Outcome
  -> separate observable, retryable Cleanup
```

Dorf never creates an implementation commit on the agent's behalf. An implementation or repair AgentRun
may create one commit or several. Dorf validates the branch, clean workspace, commit and tree, and proves
that the final Git HEAD descends from the previously accepted Revision. It then records that final HEAD as
the next immutable Revision.

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

- [x] Update the North Star and decision log: implementation and repair AgentRuns decide when and what
      to commit; Dorf observes and validates their result.
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

Terminal: a real implementation or repair AgentRun creates one or more commits, Dorf records its clean
final descendant as one immutable Revision, and Checks run against that exact Revision.

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

## Slice 3: Centralize review and repair

Goal: make ReviewPolicy, triage, specialist review, and repair one readable product path.

- [ ] Use one AgentRun runner for implementation, repair, triage, and specialist review.
- [ ] Use one deterministic readiness calculation.
- [ ] Keep ReviewPolicy pure and Revision-pinned.
- [ ] Retain isolated reviewer workspaces and security attestations.
- [ ] Return material findings to the original implementation Session.
- [ ] Observe the agent's repair commit as a new Revision and loop through Checks and policy.
- [ ] Delete duplicated implementation/review native-turn recovery.
- [ ] Delete redundant review phases, resource projections, and duplicate uncertainty tests.

Terminal: no-review, known-role review, triage-selected review, and one repair loop all reach the next
correct product state.

## Slice 4: Simplify Proposal and Outcome

Goal: make an exact-Revision Proposal the direct successor of readiness.

- [ ] Execute publication through named, Revision-scoped Actions and Steps.
- [ ] Reconcile Git and GitHub before repeating an uncertain effect.
- [ ] Use Absurd retry instead of a Dorf publication retry scheduler.
- [ ] Keep Proposal and Outcome as Dorf product facts.
- [ ] Delete publication polling, attachment verification, mirrored task state, and custom retry state.
- [ ] Consolidate duplicate publication adoption tests.

Terminal: a ready Revision produces at most one exact-Revision PR Proposal and can reach an explicit
Outcome.

## Slice 5: Isolate Cleanup

Goal: leave Cleanup as the only independently scheduled, observable lifecycle task.

- [ ] Close admission and request cancellation before cleanup starts destructive reconciliation.
- [ ] Reconcile unsettled AgentRuns and Actions before deleting resources.
- [ ] Revoke reviewer and main provider routes.
- [ ] Delete reviewer and main Sandboxes.
- [ ] Record cleanup attention or completion as a Dorf product fact.
- [ ] Use stable cleanup Action identities and Absurd retry.
- [ ] Delete duplicated cleanup preflight, broad Job fencing, and task-state mirrors.
- [ ] Consolidate cleanup tests around partial success and retry.

Terminal: cleanup can fail visibly, retry safely, and eventually prove that all owned resources are
gone.

## Slice 6: Delete the old program counter and shrink the tests

Goal: leave one obvious workflow story with no second durable scheduler in Dorf.

- [ ] Remove `workflow_phase` as sequencing authority.
- [ ] Remove `RunUntilIdle`, cycle dispositions, cycle results, and synthetic cycle checkpoints.
- [ ] Derive bounded repair history from product facts where practical.
- [ ] Remove task attachment and cancellation plumbing made unnecessary by the final task shape.
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
