# Durable Job simplification checklist

Status: implemented and verified from Issue #94.

Baseline commit: `d2e9d07` (`refactor: adopt Absurd public durability APIs`).

## Target flow

```text
Goal
  -> durable Job
       -> explicit abandon at any nonterminal point: abandoned Outcome
  -> provision Incus Sandbox
  -> Implementation AgentRun
       -> the agent decides when and what to commit
  -> Dorf observes and validates the committed Revision
  -> deterministic Checks
  -> deterministic ReviewPolicy
       -> no review
       -> selected specialist AgentRuns
       -> one general review AgentRun for unknown risk
  -> reviewer text becomes a Message through the implementation AgentRun path
  -> implementation AgentRun decides whether to act
       -> committed change: observe a new Revision; loop
       -> clean unchanged checkout: continue
  -> exact-Revision PR Proposal
  -> observe exact pull request
       -> trusted owner/collaborator comment: human Message; loop
       -> merge: accepted Outcome
       -> close without merge: rejected Outcome
  -> separate observable, retryable Cleanup
```

Dorf never creates an implementation commit on the agent's behalf. An AgentRun in the implementation
Thread may create one commit or several. Dorf validates the branch, clean workspace, commit and tree,
and proves that the final Git HEAD descends from the previously accepted Revision. It then records that
final HEAD as the next immutable Revision.

## Working rules

- Discuss and agree on the exact boundary before starting each slice.
- Keep the complete coding workflow runnable after every slice.
- Delete superseded code and tests in the same slice that replaces them.
- Do not maintain old and new workflow paths in parallel.
- Keep PostgreSQL for product facts and external Action identity, scope, and settlement only.
- Keep Absurd responsible for task eligibility, claims, checkpoints, waits, retries, and cancellation.
- Run the portable and PostgreSQL-backed suites after every slice.
- Do not add data compatibility work for the prototype schema.
- Report production, schema, and test LOC changes at slice terminals and cumulatively from the
  baseline commit.

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

- [x] Update the North Star and decision log: implementation AgentRuns decide when and
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

Terminal: a real implementation AgentRun creates one or more commits, Dorf records its
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
      perform only if missing, perform a final claim check, record success, then complete the Step.
- [x] Use the same Action executor for Sandbox, route, and other code-owned external mutations in this
      path.
- [x] Execute repository setup and declared repository checks as concrete CommandRuns at the coding
      edge, represented durably as Actions or Checks according to whether lasting mutation is intended.
- [x] Keep locks and database transactions short; do not hold them across Incus, Codex, Git, or command
      execution.
- [x] Delete the matching orchestration branches from the mixed service layer as the coordinator
      becomes authoritative. `workflow_phase` remained a transitional domain guard and handoff
      projection until the fact-derived cut in Slice 8; there is no second service-layer coordinator
      interpreting it.
- [x] Delete private phase-transition and duplicate recovery tests replaced by product-path and shared
      Action-reconciliation tests.

Terminal: the first half of `RunJob` reads in product order, survives interruption through stable
Steps, and contains no second Dorf-owned program counter for the replaced path.

The workflow kept `workflow_phase` because domain transactions, review, and publication still used it
as a guard and handoff projection. Slice 8 deletes it after those downstream paths became explicit.

Live proof (2026-08-10): Job `job-b3fc6dbd0069a0574f5a` ran an implementation AgentRun, observed
Revision `f5efad68821fa0edabe7898fd13d19bb5d06d9e0`, passed `check` and `smoke`, selected no review,
created exact-Revision PR #102, then completed abandoned-Outcome cleanup with its route revoked and
Sandbox deleted.

## Slice 3: Use one AgentRun mechanism and one Message follow-up path

Implementation and review are not different execution systems. They are AgentRuns with different
Roles, prompts, Revision inputs, adapter-supplied Sandbox contexts, and capability envelopes. User
text, Check output, and
reviewer text are not different follow-up primitives. They are Messages to the original implementation
Thread.

```text
AgentRun
  +-- Role
  +-- Message
  +-- input Revision
  +-- capability envelope

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

### Slice 3A: Use one AgentRun execution mechanism

Goal: make submission, harness recovery, waiting, and terminal recording one durable mechanism.

- [x] Define one small AgentRun execution contract containing only the durable run identity and the
      concrete harness operations needed to inspect, submit, and wait.
- [x] Use one execution mechanism for initial and follow-up implementation, general review, and
      specialist review.
- [x] Keep Role-specific prompt construction outside the shared mechanism.
- [x] Keep reviewer Sandbox creation, immutable checkout, scoped route creation, and capability
      validation outside the mechanism as preparation facts.
- [x] Keep reviewer output as opaque text; persist the cross-AgentRun handoff as a Message, with no
      review-result parser or finding schema.
- [x] Give every Message explicit `FromKind` and `FromID`; use the same pair as its stable
      admission identity and remove the caller-only naming.
- [x] Reconcile missing acknowledgements through the same harness-history path for every AgentRun.
- [x] Give every run one stable `dorf/agent-run/v1/<AgentRun ID>` Absurd Step.
- [x] Delete the separate implementation and review submission/recovery state machines.
- [x] Replace duplicated harness-status and uncertainty tests with one execution recovery contract plus one
      reviewer ownership/capability test.
- [x] Dogfood one normal implementation AgentRun and one selected read-only review AgentRun.

Terminal: implementation and selected review both execute through the same AgentRun mechanism without
weakening reviewer isolation or exact-Revision ownership.

### Slice 3B: Put the review feedback loop directly in `RunJob`

Goal: make ReviewPolicy, review, Message delivery, and Revision observation one readable product path.

- [x] Replace the coarse review continuation with explicit Revision-scoped Steps in `RunJob`.
- [x] Keep `ReviewPolicy(ChangeFacts) -> ReviewPlan` pure and deterministic.
- [x] Run one general read-only reviewer when policy reports unknown risk; do not use reviewer prose
      as a router protocol.
- [x] Run each selected specialist through the shared AgentRun mechanism in its prepared read-only
      workspace.
- [x] Turn each reviewer's returned text into one stable Message through the implementation AgentRun path.
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
text reaches the implementation AgentRun path through Message and no parser decides what it means.

Live proof (2026-08-10): Job `job-dd819a66732c3e7c556c` ran an implementation AgentRun, observed
agent-created Revision `395362a33eff41cec67f746259b29df00d36e87c`, passed `check` and `smoke`,
ran a selected general reviewer in an isolated read-only Sandbox, delivered its opaque text as an
agent Message through the implementation AgentRun path, created exact-Revision PR #103, then completed
abandoned-Outcome cleanup with both routes revoked and both Sandboxes deleted. The run exposed and
closed one shared-execution adapter bug: the adopted reviewer controller must remain visible during
an active harness wait.

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

### Slice 4B: Keep Gateway deployment state out of the Job

Goal: make one deployment configuration the obvious Provider Gateway authority.

- [x] Restore the established XDG data-directory default used by provider setup.
- [x] Use the same configured Gateway adapter for setup, doctor, admission, route creation, and cleanup.
- [x] Check the selected Provider Connection before creating a durable Job.
- [x] Retain only the Provider Connection name in Job data.
- [x] Delete the Gateway filesystem locator from the domain, baseline schema, SQL projections, inspect
      output, runtime overrides, and tests.
- [x] Remove the temporary local compatibility symlink and dogfood a small Job without a path override.

Terminal: a fresh default installation connects and runs a Job through one Gateway location, while no
host filesystem path is stored in or exposed by the Job.

Live proof (2026-08-10): with no path override and no compatibility symlink, Job
`job-ee3649bcad1a008434f8` found `personal-chatgpt` through the default XDG data location, created a
scoped route and Sandbox, observed agent-created Revision
`3e9facdc3db1f61e3069624eb02864857ed04292`, passed both checks, and created exact-Revision PR #107.
Closing the disposable PR recorded a rejected Outcome, revoked the route, and deleted the Sandbox.
The durable Job retained the Provider Connection name and no host filesystem locator.

## Slice 5: Let AgentRun own its harness execution

Goal: make one AgentRun the complete durable record of one Message delivery, without a paired
Action describing the same submission.

```text
Message -> AgentRun -> Harness / Thread / Turn
                    -> observed Evidence linked to the AgentRun
                    -> feedback Message when the Role is review
```

- [x] Remove the paired `codex-turn-start` Action and `agent_runs.action_id` synchronization.
- [x] Remove the separate `codex-session-start` Action and Thread record; derive implementation
      continuity from prior implementation AgentRuns, with every binding stored only on AgentRun.
- [x] Tie harness-observation Evidence directly to its AgentRun.
- [x] Store selected review input only as an ordinary workflow Message and require every AgentRun to
      consume exactly one Message.
- [x] Keep reviewer output only as the ordinary agent Message delivered through the implementation
      AgentRun path; delete the duplicate Evidence copy of that prose.
- [x] Retain real AgentRun inputs and recovery identities: exact Message ID, Role, Revision,
      capability, Harness/Thread/Turn IDs, harness baseline, ownership nonce, and submission nonce.
- [x] Delete adapter-owned or derived AgentRun fields: persisted workspace path, input digest, copied
      controller identity, and unused review token/cost/yield telemetry.
- [x] Shrink review readiness to completed AgentRun, exact feedback Message, verified harness Evidence,
      and the required read-only capability/Revision facts.
- [x] Delete Action/AgentRun state synchronization tests and replace them with one shared harness
      submission/recovery contract.
- [x] Dogfood one implementation AgentRun and one selected review AgentRun whose text returns through
      Message.

Terminal: implementation and review use one AgentRun durability mechanism; no second Action or prose
artifact describes the same delivery.

Live proof (2026-08-10): Job `job-c02ec75d991e1e3ccee2` recovered a temporary provider failure by
creating a new implementation AgentRun on the same Thread. Revision `39e81363` selected a general
reviewer on its own read-only Thread; that reviewer's text became an agent Message handled by the
implementation Thread. The resulting Revision `6334f112` repeated Checks and selected review, then
created exact-Revision PR #108. Closing the disposable PR recorded a rejected Outcome, revoked all
three provider Routes, and deleted the main Sandbox plus both reviewer Sandboxes. The first live
attempt also exposed and fixed one missing-Message projection at the reviewer adapter boundary.

## Slice 6: Unify owned resources and isolate Cleanup

Goal: leave Cleanup as the only independently scheduled lifecycle task and give it one inventory of
exact resources owned by the Job aggregate.

```text
Job
  -> Sandboxes
       -> owned provider Route
  AgentRuns use a Sandbox
  -> stable create and cleanup Actions
```

- [x] Agree that the Job owns all Sandboxes, each Sandbox owns its Provider Route, and AgentRuns use
      a Sandbox; no polymorphic owner kind/id is needed.
- [x] Represent main and reviewer Sandboxes through one concrete Job-owned Sandbox shape.
- [x] Represent main and reviewer provider Routes through one concrete Sandbox-owned Route shape.
- [x] Retain exact external identities and lifecycle facts needed for reconciliation; derive stable
      Action IDs instead of copying them into review resource rows.
- [x] Delete copied reviewer Sandbox/Route/checkout/post-review states when Actions or verified Evidence
      already own the fact.
- [x] Close admission and request cancellation before destructive cleanup reconciliation starts.
- [x] Reconcile unsettled AgentRuns and Actions before deleting resources.
- [x] Revoke every owned Route and delete every owned Sandbox through one cleanup loop and the shared
      external Action executor.
- [x] Keep Cleanup as its own observable Absurd task with stable Action identities, attention,
      retry, and explicit completion.
- [x] Delete the separate main/reviewer cleanup algorithms, broad Job fencing, and the superseded
      `review_resources` projection.
- [x] Replace duplicated cleanup choreography tests with exact-identity, partial-success, attention,
      and retry-convergence stories.
- [x] Dogfood terminal Outcome cleanup with at least one reviewer Sandbox present.

Terminal: cleanup can fail visibly, retry safely, and eventually prove that every exact owned resource
is gone through one path.

Live proof (2026-08-10): Job `job-5a08e1e24890e33d7ba7` used one implementation Sandbox and one
selected general-review Sandbox through the same Job-owned resource model. Reviewer text returned as
a Message to the implementation Thread, Checks passed, and exact-Revision PR #109 was proposed.
Closing the disposable PR recorded a rejected Outcome; one cleanup task revoked both Routes, deleted
both Sandboxes, and Incus returned no instance for either exact Sandbox ID. The run also exposed and
fixed incomplete Route reconciliation and removed a second, non-convergent strict-review read path.

Action-scope refinement proof (2026-08-10): Job `job-c9d27acf02fafc3c1d8d` created one Sandbox and
recorded repository clone against that exact Sandbox through the shared Action path. Checks passed,
exact-Revision PR #110 was proposed and closed, the rejected Outcome completed cleanup, and Incus
returned no instance for the exact Sandbox ID.

### Slice 6A: Let Action success own external lifecycle truth

Goal: after ordinary Actions have one explicit Sandbox scope and immutable success, remove stored
projections that repeat the same external fact.

- [x] Keep Actions distinct from Absurd Steps: a Step checkpoints durable execution; an Action records
      one reconciled external mutation.
- [x] Make immutable Action success authoritative for Sandbox and Route lifecycle facts, then delete
      duplicated lifecycle state and branches from Sandbox and Route records.
- [x] Prove that Route identity is deterministic from its Sandbox and delete the Route row if it retains
      no independent external fact.
- [x] Remove generic Action result strings; retain immutable Action success plus the natural product
      record that owns each durable fact.
- [x] Retain Sandbox identity and ownership nonce; they authorize exact external reconciliation and
      cleanup rather than duplicating Action progress.
- [x] Delete row-shape and state-mirroring tests; retain recovery tests for immutable Action success,
      exact ownership, and retry convergence.
- [x] Dogfood the payload-free Action path through exact-Revision Proposal, terminal Outcome, and
      complete Route and Sandbox cleanup.

Terminal: every external lifecycle fact has one durable authority, while an interrupted Action still
reconciles safely through its distinct Absurd Step.

Lifecycle-authority proof (2026-08-10): a fresh PostgreSQL database with no Route table or copied
Sandbox/Route state ran Job `job-d461c7a212d5543f749f` from Sandbox creation through exact-Revision
PR #112. Both Checks passed. Closing the disposable PR recorded a rejected Outcome; immutable
route-revoke and Sandbox-delete Actions completed cleanup, and both Incus and the Provider Gateway
returned no exact resource. This proof predates generic Action payload removal; the simplified Action
record received its fresh live terminal below.

Payload-free proof (2026-08-10): Job `job-85573808ad06d0790f67` ran on a separate fresh PostgreSQL
database through Sandbox creation, clone, setup, Checks, no-review policy, and exact-Revision PR #113.
Closing the disposable PR recorded a rejected Outcome. Route revoke and Sandbox delete Actions reached
success with only identity, kind, scope, and state; Incus and the Provider Gateway returned no exact
resource after cleanup.

## Slice 7: Store GitHub authority once

Goal: keep admitted repository authority on the Job and retain only new Proposal and Outcome facts.

```text
Job      -> repository, installation, base branch, head branch
Proposal -> PR number, URL, exact Revision, body digest
Outcome  -> kind, external observation, merge commit, observed time
```

- [x] Remove repository, installation, base-branch, and head-branch copies from Proposal and Outcome.
- [x] Remove `observed_remote_head`; recording already requires the PR head to equal the proposed
      Revision.
- [x] Keep the PR body digest on Proposal only; do not copy it into the PR Action outcome.
- [x] Remove Outcome copies that are fixed by Proposal identity, while retaining the external
      observation needed to prove accepted, rejected, or abandoned.
- [x] Join immutable Job and Proposal authority when reconciling, rendering, or recording Outcome.
- [x] Align CLI and documentation with the live flow: merge/close is observed automatically and the
      explicit human command is for abandonment.
- [x] Consolidate identity-copy tests into exact-Revision proposal, outcome, and concurrency stories.
- [x] Dogfood one exact-Revision PR through close or merge and terminal cleanup.

Terminal: changing one admitted GitHub authority fact requires changing one row, and Proposal/Outcome
contain no second copies to reconcile.

Single-authority proof (2026-08-10): Job `job-1ac5dc1d1bc21bce80de` produced exact-Revision PR #114
from a separate fresh PostgreSQL database. The Proposal row retained only PR identity, Revision, and
body digest. Closing the PR was observed automatically as a rejected Outcome containing only the
terminal GitHub observation. Cleanup revoked the exact Route and deleted the exact Sandbox; Incus and
the Provider Gateway returned no matching resource.

## Slice 8: Delete the old program counter

Goal: let retained product facts tell `RunJob` what comes next instead of a second durable scheduler.

- [x] Remove `workflow_phase` from Job, PostgreSQL, SQL transitions, readiness, and inspection.
- [x] Bind each selected implementation AgentRun to its input Revision and retain its eligible final
      `git-revision` Evidence for changed and unchanged clean observations. Evidence Revision equal to
      the input means unchanged; a different descendant also creates the next Revision. Do not add a
      second observation table or result enum for facts already carried by AgentRun and Evidence.
- [x] Derive one non-durable, coding-specific `CurrentWork` in ordinary Go from owned resources,
      Messages, AgentRuns, `git-revision` Evidence, Revisions, Checks, ReviewPlans, Proposals, Outcomes,
      and cleanup Actions. Keep its dependency order visible; do not hide it in one giant SQL query.
- [x] Make `RunJob` execute `CurrentWork` and recompute it after each recorded fact. Inspection uses
      the same current-work decision rather than independently interpreting progress.
- [x] Keep explicit workflow attention as an operator-visible fact without coupling it to a phase or
      making it another general program counter. The exact failed, unsettled, or unchanged fact still
      explains what can safely resume.
- [x] Remove `RunDisposition`; admission closure and retained facts already describe whether the task
      should continue, wait, or finish.
- [x] Drive setup retry from the selected failed setup Action and explicit operator Message rather than
      a blocked phase.
- [x] Derive the starting Revision from Revision generation zero instead of storing a second copy on
      Job.
- [x] Store only final ReviewPlans. Delete the pending ReviewPlan and separate Checks-verified handoff;
      passing exact-Revision Checks and Evidence directly admit deterministic ReviewPolicy.
- [x] Keep transactional guards local to the fact being recorded: current Revision, latest accepted
      AgentRun, exact Evidence set, selected ReviewPlan, current Proposal, and absence of newer input.
      Do not replace the phase with another status matrix.
- [x] Ensure natural product facts retain the timestamps needed to derive honest chronology, including
      Action settlement, Revision, and `git-revision` Evidence, without adding a copied event-log table.
- [x] Retain only the main and cleanup Absurd task handles while public cancellation and inspection
      require them; do not mirror task state.
- [x] Remove `RunUntilIdle`, cycle results, and synthetic cycle checkpoints in Slice 2B.
- [x] Delete phase-transition helpers, CAS branches, fixtures, and tests as fact-derived decisions become
      authoritative.
- [x] Dogfood the whole implementation, check, review, proposal, Outcome, and cleanup sequence.

Terminal: the main task and product inspection derive the same current work from facts in visible
target-flow order, and no Dorf-owned phase value decides eligibility.

Guardrail: this slice does not create a generic DAG engine, configurable workflow language, generic
step registry, persisted `next_work`, copied event-sourcing layer, or database-side workflow
interpreter. Those would recreate the deleted authority with more machinery. The coding workflow stays
concrete; a new operation adds its natural fact and one explicit dependency only when dogfood needs it.

Dogfood proof (2026-08-10): Job `job-44939cffcbb165a20996` started at exact source Revision
`da4e97c0d032a5802dad3748f61b8dd4ff3c42fc`, produced Revision
`e034194bc1e74444bfbfbdd783b619f99832da29`, passed both declared Checks, ran the selected general
reviewer in its own Sandbox and Thread, admitted its feedback as an ordinary Message, and handled that
feedback unchanged in the implementation Thread. Dorf proposed GitHub PR #116, observed its real
closed/not-merged state as a rejected Outcome, then revoked both Routes and deleted both Sandboxes;
the main and cleanup Absurd tasks both completed.

Expanded dogfood matrix (2026-08-10):

- Job `job-f91109766db4e3e47967` took the documentation-only/no-review path through PR #117.
- Job `job-4ce4e8197c758896b0dc` accepted a second human Message while implementation was active;
  both ran once in FIFO order on one Thread and produced one batch Revision before PR #120.
- Job `job-ffa3afacbccd28c1a8f6` recorded an unchanged first attempt as derived attention, then a human
  follow resumed the same Thread and continued through PR #118.
- Job `job-af0f0aa9139b8501d5c8` admitted trusted PR comment `5242694451`, reacted to it, reused the
  implementation Thread, produced a second Revision, reran Checks and review, updated PR #119, and
  posted a quoted completion reply naming the exact Revision.
- Job `job-71d7ff0fd285bae302e7` survived a worker stop during an active AgentRun. The replacement
  reconciled the same Thread and Turn, completed two Revision/review cycles, and proposed PR #121.

Every PR was closed to exercise the rejected Outcome. All five main tasks and cleanup tasks completed,
and every exact Route and Sandbox was absent afterward. The matrix found and fixed three fact-boundary
bugs: nonterminal implementation runs could fall through to Checks, reviewer request projections omitted
Message admission time, and publication could begin before full Evidence-blob readiness. It also deleted
two unreachable review proof barriers and obsolete image-validator vocabulary.

## Slice 9: Make inspection and tests tell the product story

Goal: finish with a small human surface and tests that protect promises rather than choreography.

- [x] Make default `inspect` overlay the complete expected coding dependency chain with chronological
      product facts, mark current work or attention, and show the exact Revision, Proposal/Outcome,
      and cleanup. Repeated Revisions should make implementation/check/review loops obvious.
- [x] Derive `WorkflowHistory` from natural Message, Action, AgentRun, Revision, `git-revision`
      Evidence, Check, ReviewPlan, Proposal, Outcome, attention, and cleanup timestamps. Do not store
      a parallel event transcript or use it as recovery authority.
- [x] Keep deep product facts and attached task correlation/results under `inspect --json`; use
      `absurdctl` for task runs, attempts, checkpoints, waits, and leases.
- [x] Stop printing reviewer workspace, controller, token, task, and resource plumbing in normal human
      output.
- [x] Make feedback and delivery explanations derive only from Messages and AgentRuns; delete joined
      `FeedbackMessageID`, role-specific AgentRun identity, and the second FIFO/blocking projection.
- [x] Replace runtime Store/External capability assertions with one compile-time service boundary after
      trimming methods the service does not call.
- [x] Remove copied read-model fields when their source facts are already present in the Snapshot.
- [x] Load one concrete inspection Snapshot for CurrentWork, readiness, history, and JSON instead of
      querying the same facts twice; do not turn it into a generic graph framework.
- [x] Keep compact PostgreSQL-backed stories for Message/Thread, Check feedback, review feedback,
      Proposal/Outcome, and exact cleanup/retry.
- [x] Keep pure policy/readiness tests, exact external-authority tests, and the production claim-loss
      fault tests.
- [x] Delete obsolete workflow-choreography, copied-state, duplicated status-matrix, raw-row, and
      brittle inspect-prose tests. Retain focused pure helper and formatting tests when they protect
      an external contract.
- [x] Audit earlier replacements and remove their matched obsolete tests; retain distinct concurrency,
      recovery, and adapter-authority proofs.
- [x] Correct all setup and lifecycle examples to match automatic PR Outcome observation and cleanup.
- [x] Report final production/test/schema LOC and name any remaining complexity centers.

Terminal: a user can understand a Job without reading implementation state, and the focused test
suite protects the same story and external contracts shown by the workflow.

## Issue #94 closure gate

- [x] The real Job sequence, not only an outer cycle, uses stable typed Absurd Steps.
- [x] Checkpoint names and result shapes are treated as versioned persisted contracts.
- [x] Production external effects use stable Dorf Actions and reconcile external truth.
- [x] Every accepted Message has one immutable wake identity while PostgreSQL remains its authority.
- [x] Production uses only public Absurd spawn, result, cancel, retry, heartbeat, and event APIs.
- [x] Production never reads or mutates raw Absurd task tables.
- [x] The production external-Action path performs a final claim check before recording success.
- [x] Claim-expiry and cancellation tests exercise that actual production path.
- [x] Absurd operational history comes from `absurdctl` or Habitat, not Dorf tables.
- [x] The prototype uses one clean baseline schema pinned to Absurd 0.5.0.
- [x] Portable and PostgreSQL integration/fault suites pass.
- [x] The before/after production LOC and remaining complexity centers are reported.

### Final evidence

The retained PostgreSQL stories cover concurrent Message FIFO and Thread continuation, changed and
unchanged Revision observation plus failed-Check feedback, reviewer feedback as an ordinary Message,
Proposal/Outcome serialization, pre-Proposal abandonment versus publication intent, and exact
route-revoke-before-Sandbox-delete cleanup. Pure tests retain policy and readiness rules; adapter tests
retain exact GitHub, Git, Incus, Gateway, and Codex authority. The production Action path is exercised
by `TestAbsurdCancellationCannotRecordLateActionSuccess` and
`TestAbsurdClaimExpiryReconcilesEffectWithoutLateOverwrite`, including the real claim check immediately
before Action success.

`TestPersistedWorkflowContractsV1`, `TestStepNamesComeFromDurableFactIdentity`, and
`TestWakeEventIsStableAndFIFOScoped` pin the persisted result, Step-name, and wake contracts. A static
source audit found only public Absurd task, Step, spawn, result, cancel, heartbeat, and event calls in
production; the sole raw Absurd lease mutation is isolated in the version-pinned fault test. Fresh-schema
PostgreSQL tests, SQL generation/vetting, `go test ./...`, `go vet ./...`, the repository check script,
and the built `dorf 0.2.0` binary pass against Absurd 0.5.0.

Live Jobs on 2026-08-10 proved both no-review and selected-review paths. Job
`job-518e89c940f3f147ddd6` produced Revision `c68e4b15531967b697ad20a8b3c815a44fbf4d7e`, passed both
Checks, selected no review, proposed PR #123, recorded rejection, and cleaned its exact Route and
Sandbox. Job `job-c7f6919ff693e3c0d045` produced Revision
`d7bc86c92227b8c051082d868850d2aff905c6da`, ran the selected general reviewer in a dedicated
read-only Sandbox, returned feedback as an implementation Message, proposed PR #125, recorded
rejection, and cleaned both Sandboxes and Routes. Their Absurd histories contain singular
`dorf/action/v1/<ActionID>` Steps with `ActionStepResultV1`, alongside typed AgentRun, Revision, Check,
and ReviewPolicy Steps; normal Dorf inspection shows only the shared fact-derived product history.

Terminal-target steer recovery was also exercised through public boundaries. On Job
`job-fcd3196fec2b366c0957`, a steer targeting Turn
`019fecdb-2cd3-7100-b685-30d86ace7e32` recovered on the same Thread as distinct Turn
`019fecdc-f90f-7440-aee7-c2f97e0fdb2c`. Its exact AgentRun-owned `git-revision` Evidence produced
Revision `91585c3b52d41a407a5a8356212649972b09f6ec` before either Check began. Dorf then proposed PR
#126, observed rejection, and completed exact cleanup with no Route or Sandbox left behind.

Final commit `f51b6d9` was dogfooded at the manual stop boundary. Public `dorf abandon` recorded Job
`job-a2e6043bb7696c7107f8` before any worker or Proposal, with no invented GitHub observation, closed
admission, and cancelled the pending main task. Inspection showed `Complete — abandoned`, Proposal
`null`, no Actions, and only the initial Message; the deterministically expected Route and Sandbox and
the Job branch's PR list were all empty. Fresh-PostgreSQL tests separately exercise abandonment with
an active AgentRun and its serialization against publication intent; live worker execution for that
variant was not authorized, so the retained evidence does not claim it occurred live.

From baseline `d2e9d07` to final implementation commit `f51b6d9`, handwritten production Go changed
from 12,371 to 12,298 lines (-73), tests from 8,818 to 6,946 (-1,872), and the baseline schema from
343 to 275 lines (-68). Slice 9 itself started at `51bbf51`: handwritten production Go grew from
12,017 to 12,298 (+281) and tests from 6,278 to 6,946 (+668) because hidden multi-Action coordination,
claim-loss boundaries, and concurrency recovery became explicit and directly proved. Over the same
slice generated sqlc Go fell from 3,711 to 3,530 (-181), named query SQL from 916 to 899 (-17), and
the schema stayed at 275 lines. The remaining complexity centers are concrete external authorities:
Codex app-server delivery/recovery, PostgreSQL concurrency and atomicity, reviewer capability isolation,
and GitHub/provider reconciliation. Snapshot/Projection and exact Action Steps are the simplifying
boundaries, not new workflow frameworks.
