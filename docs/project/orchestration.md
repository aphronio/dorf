# Orchestrating Dorf Work

This is the stable operating protocol for an agent coordinating a bounded Dorf issue or epic.
Issues own changing scope and status. Code, tests, runtime inspection, Git, GitHub, and external
authorities own observed facts. Project documents retain only product principles, architecture, and
decisions whose rationale would not be evident from those sources.

## Sources of truth

- The active issue owns the desired outcome, acceptance criteria, dependencies, and current plan.
- The repository contract and CI configuration own deterministic verification commands.
- The current branch, commits, and pull request own implementation and reviewable diff state.
- Dorf and Absurd inspection own durable Job, Action, Check, AgentRun, task, and recovery facts.
- Incus, the agent harness, Git, and GitHub own their respective external facts.
- Project documents own enduring boundaries and consequential decisions, not an execution diary.

When sources disagree, inspect the authority and correct the stale projection. Do not make code,
runtime state, or evidence match a narrative.

Use Dorf inspection for product facts. Use `absurdctl list-tasks` and `absurdctl dump-task` for
attempts, checkpoints, waits, and task history while debugging Absurd. Raw Absurd tables are a
version-pinned white-box test or operator diagnostic surface, not production workflow authority.

## Take one bounded slice to a terminal

1. Read the issue, relevant project decisions, repository guidance, and current observed state.
2. Confirm whether an existing Job, Sandbox, Session, branch, or pull request already owns the work.
3. State the invariant being changed and the smallest real behavior that can prove it.
4. Use one goal-backed implementation Job for that slice. Keep mechanical setup and verification in
   repository-owned commands; reserve AgentRuns for judgment.
5. Reconcile uncertain external effects against their authority before retrying.
6. Run deterministic checks locally before push. Changes to PostgreSQL facts, Absurd sequencing, or
   recovery include the PostgreSQL integration suite.
7. Let CI repeat portable unit and PostgreSQL integration coverage as an independent merge gate.
   Use real Incus, Codex, provider, or GitHub terminals when the slice changes those boundaries.
8. Apply deterministic review policy, union any valid structured implementation review request,
   and return material findings to the original implementation Session.
9. Retain only the evidence needed to establish the acceptance criteria, then reconcile cleanup.
10. Update the issue from observed facts and propose merge only after the bounded terminal passes.

Prefer a new issue over silently expanding a slice. Consequential product or architecture changes
also update the decision log in the same change.

## Recovery after interruption

Before starting or retrying mutation:

1. inspect local Git state, the remote branch and pull request, the active issue, and recent comments;
2. inspect the existing Dorf Job, Sandbox, Session, Absurd task, and unsettled Actions;
3. compare those facts with the relevant external authority and retained Evidence; and
4. resume the existing owner when identity is established.

Never create a competing Job merely because the coordinating process or context disappeared. If
ownership remains ambiguous after inspection, stop mutation and ask for human direction.

## Completion

A slice is complete when its issue criteria are proven through the real boundary it changes, local
checks and required CI pass, review findings are adjudicated, no uncertain external effect is hidden,
and temporary resources are either cleaned or have precise observable attention. A fluent agent
completion message, green unit tests alone, or copied logs are not substitutes for that terminal.
