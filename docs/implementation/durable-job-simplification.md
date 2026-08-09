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

Dorf never creates an implementation commit on the agent's behalf. Dorf validates the branch, parent,
commit, tree, and workspace, then records the observed commit as an immutable Revision.

## Working rules

- [ ] Discuss and agree on the exact boundary before starting each slice.
- [ ] Keep the complete coding workflow runnable after every slice.
- [ ] Delete superseded code and tests in the same slice that replaces them.
- [ ] Do not maintain old and new workflow paths in parallel.
- [ ] Keep PostgreSQL for product facts and external Action receipts only.
- [ ] Keep Absurd responsible for task eligibility, claims, checkpoints, waits, retries, and cancellation.
- [ ] Run the portable and PostgreSQL-backed suites after every slice.
- [ ] Do not add data compatibility work for the prototype schema.

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

Goal: express `Sandbox -> Implementation AgentRun -> observed Revision -> Checks` as named Absurd
Steps.

- [ ] Introduce one readable `RunJob` coordinator for this path.
- [ ] Give repeated Steps stable names based on Message, AgentRun, Revision, or Check identity.
- [ ] Use small typed Step results.
- [ ] Centralize the external Action pattern: reserve, reconcile, perform if missing, final claim check,
      record receipt, then complete the checkpoint.
- [ ] Adopt a clean, directly provable agent-created commit as the immutable Revision.
- [ ] Keep locks and transactions short; do not hold them across Incus, Codex, Git, or GitHub calls.
- [ ] Delete Dorf commit creation, commit workflow phases, and their recovery machinery.
- [ ] Delete tests whose promise was that Dorf creates the implementation commit.

Terminal: a real implementation AgentRun may commit, Dorf observes exactly one valid Revision, and
Checks run against that exact Revision.

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
