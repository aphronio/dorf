# Greenfield Epic Orchestration

This is the durable operating protocol for the agent coordinating Dorf's Go and Absurd replacement.
The GitHub epic and child issues own current scope, dependencies, acceptance criteria, and progress.
The North Star, architecture, principles, and decision log own enduring direction.

## Sources of truth

- GitHub issues own slice scope, dependency order, acceptance criteria, and the live dogfood ledger.
- The `greenfield` integration branch owns the cumulative replacement until cutover.
- Each issue branch, commit, checks, and pull request own implementation evidence for that slice.
- Dorf and Absurd records own observed Job, Action, Check, AgentRun, and recovery facts once the new
  runtime exists.
- The isolated Sandbox, Git branch, GitHub pull request, and native agent Session own the external
  facts in their respective domains.
- Project documents own durable product and architecture decisions, not task status.

When sources disagree, prefer direct runtime and repository evidence. Correct the issue ledger; do
not change evidence to match a narrative.

## Branch and issue protocol

1. Create the `greenfield` integration branch from the documented base commit.
2. Freeze feature development on the Python implementation; only critical repairs belong on `main`.
3. Take the next unblocked child issue from the epic.
4. Create one short-lived issue branch from the current `greenfield` head and open its pull request
   against `greenfield`, not `main`.
5. Merge only after the issue's real terminal and required evidence are satisfied.
6. Delete Python implementation made redundant by the working Go path. Do not retain compatibility
   merely so the old CLI still runs on the integration branch.
7. After the epic cutover terminal passes, open one final `greenfield` to `main` pull request and
   remove remaining superseded Python runtime, packaging, tests, and documentation.

Do not create a second architecture branch, a dual-write bridge, or parallel issues that mutate the
same authority or files without a proven safe boundary.

## Orchestrator responsibilities

For every slice, the orchestrator:

1. reads the epic, active issue, relevant project documents, current integration-branch state, and
   any predecessor evidence;
2. writes a short invariant and proof map for the authorities, identities, external effects,
   recovery paths, cleanup, and provenance touched by the slice;
3. chooses the lowest agent capability likely to complete the slice and records that choice;
4. starts exactly one implementation Job unless recovery proves an existing Job owns the work;
5. observes implementation, deterministic checks, selected review, repair, dogfood, publication,
   and cleanup;
6. classifies failures before retrying or raising model capability;
7. updates the issue and epic ledgers after meaningful state changes; and
8. proposes merge only after the smallest real runnable outcome satisfies the issue.

The human remains the acceptance boundary for consequential architecture changes and the final
cutover. Agent success, green unit tests, or a fluent completion message is not acceptance.

## Agent configuration and review

Choose model and reasoning from specification ambiguity, blast radius, reversibility, authority
risk, recovery complexity, and verification cost. Infrastructure, repository, external-provider,
and orchestration failures are not evidence that the implementation model needed more reasoning.

After deterministic checks:

- apply the greenfield ReviewPolicy when it exists;
- before it exists, use at most one material independent review for a high-risk slice;
- send findings to the original implementation Session for adjudication and repair;
- use targeted follow-up proof for repaired findings rather than another full-context review; and
- reach a real dogfood terminal earlier than a third broad review.

Review conclusions are claims. Retain the exact diff, commands, outputs, artifacts, and external
state needed to establish facts.

## Dogfood ledger

Maintain one table in the epic. At minimum record:

| Field | Meaning |
| --- | --- |
| Slice | Child issue and bounded outcome |
| Job / PR | Durable Job, issue branch, and pull request |
| Selection | Agent/model/reasoning source and concise rationale |
| Run | Real terminal, elapsed time, and exact Revision |
| Review / repair | Selected Roles, material findings, adjudication, and reruns |
| Acceptance | Ready, needs human, revised, accepted, rejected, or abandoned |
| Failure class | Agent quality, specification, repository, infrastructure, orchestration, external, or unknown |
| Deletion | Superseded Python code, tests, and decisions removed by the slice |
| Notes | Only evidence necessary to interpret or reconsider the result |

Derive mechanical facts from the runtime, Git, checks, and GitHub rather than copying transcripts.

## Recovery after interruption or context compaction

1. Re-read this document, the epic, its ledger, the active issue, and the latest issue comments.
2. Inspect local Git state, the integration branch, issue branch, pull request, Sandbox, Job, Session,
   Absurd task, and external Action receipts before starting work.
3. Reconcile recorded state with commits, checks, native agent history, GitHub, and cleanup facts.
4. Resume the existing implementation Job when ownership is established. Never create a competing
   Job merely because the orchestrator process or context disappeared.
5. If an external effect is uncertain, inspect its authority before retrying.
6. Record recovered truth and the next action in the ledger.

If identity or ownership cannot be established, stop mutation and request human direction.

## Epic completion

The epic is complete only when:

- every required slice has reached its real terminal;
- the greenfield cutover terminal in the architecture document passes on a clean machine;
- the final Go path has no Python process in its critical execution path;
- all old local data may be discarded and no compatibility mechanism remains;
- superseded Python runtime, packaging, tests, and temporary implementation documents are removed;
- the final diff and repository remain first-class readable rather than preserving slice scaffolding;
- consequential decisions discovered through dogfood are recorded with reconsideration triggers;
  and
- the final branch is accepted into `main` with an evidence-backed outcome and complete resource
  cleanup.
