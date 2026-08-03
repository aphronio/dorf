# Epic Orchestration

This document is the durable operating protocol for an agent coordinating a multi-issue epic. It
defines how to work; the epic's GitHub ledger records current state. Product requirements and
architecture remain in the linked issues, principles, and decisions rather than being duplicated
here.

## Sources Of Truth

- GitHub issues own task scope, dependencies, acceptance criteria, and the live dogfood ledger.
- Dorf runtime state owns Worker, Room, Job, Assignment, input, and native-turn facts.
- Coding workflow state owns command runs, repository policy, and PR coordination facts.
- The isolated Job workspace, branch, commits, checks, and pull request own implementation evidence.
- The native Job conversation owns transcript history.
- Project principles and the decision log own durable product and architecture direction.

When sources disagree, prefer direct runtime and repository evidence for what happened. Correct the
ledger rather than changing evidence to match it.

## Orchestrator Responsibilities

The orchestrator:

1. reads the epic, active child issue, relevant project documents, and current repository state;
2. selects the next unblocked vertical slice and records its intended agent configuration;
3. starts exactly one implementation Job for that slice unless recovery proves none exists;
4. observes setup, implementation, checks, review, repair, publication, and follow-up outcomes;
5. diagnoses failures before retrying or increasing model reasoning;
6. updates the ledger after meaningful state changes and before handing off; and
7. proposes completion only after the issue's evidence satisfies its acceptance criteria.

The human remains the acceptance boundary. The orchestrator may prepare, verify, publish, and revise
a proposal, but does not treat an agent's successful exit or a green check as human acceptance.

## Agent Configuration

Choose the lowest model capability and reasoning effort that is likely to complete the slice
reliably. Consider specification ambiguity, blast radius, reversibility, domain risk, verification
cost, and evidence from comparable completed slices. Record the resolved model, reasoning effort,
selection source, and a short rationale before work starts.

Do not automatically escalate because a run failed. First classify the failure. Repository setup,
sandbox infrastructure, task specification, orchestration, and external failures are not evidence
that the model needed more reasoning. Current model names and defaults belong in executable
configuration and the live ledger, not this document.

## Dogfood Ledger

Maintain one ledger in a GitHub issue associated with the epic. Add a row before each implementation
Job and complete it after the outcome is known. At minimum record:

| Field | Meaning |
| --- | --- |
| Slice | Child issue and narrow task shape |
| Job | Dorf Job and Worker names plus pull request when available |
| Selection | Resolved model, reasoning effort, source, and concise rationale |
| Run | Implementation outcome and elapsed time |
| Repair | Verification or follow-up rounds required |
| Acceptance | Ready, needs human, revised, accepted, rejected, or abandoned |
| Failure class | Agent quality, specification, repository, infrastructure, orchestration, external, or unknown |
| Notes | Only evidence needed to interpret the result or reconsider a decision |

Use runtime Job/turn and workflow command records to derive mechanical facts instead of copying logs
into the ledger.
Aggregate usage data may be recorded when the agent driver exposes it, but the ledger must not
duplicate transcripts or become a normalized event store.

## Recovery After Interruption Or Compaction

1. Re-read this document, the epic, its ledger, and the active child issue.
2. Inspect local Git status plus the recorded Worker, Job, Assignment, Room, branch, and PR before
   starting new work.
3. Follow the ledger's typed identities to the runtime records, isolated Job workspace, branch, and
   pull request.
4. Reconcile recorded state with native turns, workflow commands, commits, checks, review feedback,
   and Room lifecycle.
5. Resume the existing Job and Worker when it is safe. Never create another implementation Job
   merely because conversational context was compacted.
6. Record the recovered state and next action in the ledger.

If identity or ownership cannot be established safely, stop mutation, record the uncertainty, and
request human direction.

## Epic Completion

Before closing an epic, verify that all required slices and cleanup gates are complete, temporary
implementation documents are retired, and consequential decisions discovered through dogfooding
are captured with reconsideration triggers. Summarize the ledger evidence and distinguish observed
results from tentative conclusions based on a small sample.
