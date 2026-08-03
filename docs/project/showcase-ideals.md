# Showcase Ideals — Workflow-Layer DX on Top of the Runtime

Aspirational direction for the applications that compose on the L1 control plane: coding-to-PR
today, AFK personal capacity later. Companion to [north-star.md](north-star.md), which owns the
runtime experience. Same status as the north star: direction and taste, not an API spec, backlog,
or license to build ahead of the stranger loop.

The layer rule, shared with the north star:

> **The runtime owns mechanisms, recording, and rendering. Workflows own semantics, policy, and
> verification.**

Every ideal below names the runtime hook it builds on. If an ideal needs a hook the runtime does
not have, that is a north-star conversation first, not a workflow feature reaching into L1.

## Contract-first jobs

"Make checkout feel instant, prove it" compiles into a pinned acceptance checklist, machine-checkable
where possible: bench thresholds, green checks, required artifacts. The pulse then shows
`proven 3/5` instead of vibes, and the evidence wall stops being something you read and becomes a
scoreboard that fills itself. Trust becomes computable.

This is [repo contracts over environment cleverness](principles.md) applied at assign time: the
managed repo's own setup, check, and smoke contracts are what turn goal prose into checkable
items.

Runtime hook: a job may carry structured acceptance items whose proven/unproven state the runtime
stores and renders. Compiling prose into items and running the verification are workflow work.

## Verification ladder: claims into facts

The runtime's rule is that claims are never presented as facts. The workflow's ambition is to make
converting claims into facts cheap: every worker statement that matters ("checks pass", "p95 is
120ms", "this is done") should have a repo-owned command that proves or refutes it without a human
reading a transcript.

The more of a job that is verifiable, the more of it can be approved after the fact instead of
supervised live. Verification capacity, not model quality, is the practical ceiling on detached
work.

Runtime hook: claim/observed provenance on state and evidence.

## Autonomy labels and roads

Named autonomy levels (propose, act in room, act and publish, act and spend) are workflow labels
printed on the runtime's boundary envelope. The workflow may suggest a level per task from track
record, and should explain what would earn more autonomy.

Roads before self-driving: autonomy is widened by building better roads, not by trusting the driver
more. Deterministic setup, exhaustive checks, and smoke contracts are the roads; they widen what
after-the-fact approval can safely cover. Models improving moves the default dial position; it
never removes the dial, because irreversible and spending actions stay gateable by construction.

Runtime hook: the boundary envelope (egress, injected secrets, brokered verbs) and its approval
queue.

## Calibrated workers

A worker accrues a per-repo track record. The pulse can then say: "self-estimate: done in 12m;
historically 30% optimistic here." The DX of trust is not only seeing what the worker claims but
knowing what its claims have historically been worth. That calibration is what a scheduler-human
actually needs to allocate attention.

Runtime hook: self-estimates and outcomes recorded as ordinary timeline entries; computing and
displaying calibration is workflow work.

## Coding attention semantics

The runtime's attention contract carries urgency tiers and batching; the coding workflow decides
what maps to which. A red check mid-job is digest material; a boundary approval or an irreversible
choice is a ping. Review requests batch to the human's schedule.

At publish time the evidence pack is projected into the PR (body, comments, checks) so review
happens where reviewing already lives. GitHub gets the story at the moment it matters; the job
record keeps the full history.

Runtime hook: the state/attention two-channel contract, the inbox, and artifacts with provenance.

## Dogfood bar: the phone-only week

The north star's harsher stranger test is completing the loop from a phone. The showcase bar is
crueler: run a real week of coding work phone-first, with the laptop as break-glass only. Every
step that forces a terminal is a finding. Passing the loop test is necessary; surviving the week is
the evidence that AFK capacity is real.

## How to use this document

| Do | Do not |
| --- | --- |
| Judge workflow-layer PRs against these ideals | Encode any of them into runtime surfaces |
| Name the runtime hook before building on it | Reach into L1 for a missing hook |
| Let dogfood evidence promote an ideal into work | Treat this document as a backlog |
| Record accepted choices in [decisions.md](decisions.md) | Build showcase polish before the stranger loop is excellent |
