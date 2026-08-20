# Showcase Ideals — Workflow-Layer DX on Top of the Durable Core

Non-normative product exploration for workflow-layer DX, retained as idea material rather than Dorf
direction or requirements. The [North Star](../project/north-star.md) owns product direction and
experience.

Apply the authoritative [North Star product boundary](../project/north-star.md#product-boundary). Every ideal
below names the Core mechanism it builds on and the consuming layer that supplies policy or
presentation. If an ideal needs a mechanism Core does not have, that is an architecture conversation
first, not a workflow feature reaching inward.

## Contract-first jobs

"Make checkout feel instant, prove it" compiles into a pinned acceptance checklist, machine-checkable
where possible: bench thresholds, green checks, required artifacts. The pulse then shows
`proven 3/5` instead of vibes, and the evidence wall stops being something you read and becomes a
scoreboard that fills itself. Trust becomes computable.

This is [repo contracts over environment cleverness](../project/principles.md) applied at assign time: the
managed repo's own setup, check, and smoke contracts are what turn goal prose into checkable
items.

Core mechanisms: durable workflow input and attachment of workflow-owned typed facts. Compiling
prose, defining checklist semantics, proving items, and rendering the scoreboard are workflow or
client work; Core does not acquire a generic acceptance-checklist model.

## Verification ladder: claims into facts

The core rule is that claims are never presented as facts. The workflow's ambition is to make
converting claims into facts cheap: every agent statement that matters ("checks pass", "p95 is
120ms", "this is done") should have a repo-owned command that proves or refutes it without a human
reading a transcript.

The more of a job that is verifiable, the more of it can be approved after the fact instead of
supervised live. Verification capacity, not model quality, is the practical ceiling on detached
work.

Core mechanisms: Message provenance for claims and Evidence for observed facts.

## Calibrated isolated verification roles

Verification should not collapse every question into one generic reviewer. Acceptance/QA, diff
correctness and security, simplification and architecture, and performance are distinct roles only
when each has a concrete question, capability envelope, and evidence contract. A different prompt
or model alone does not earn another role. The task's material risks select the smallest useful
set; routine work may need only deterministic checks and one diff review.

Example roles make the boundaries concrete:

| Role | Question | Capability and evidence posture |
| --- | --- | --- |
| Acceptance/QA | Does the exact result fulfill the issue through real behavior? | Run the app, tests, browser, or API probes; retain observed outputs and artifacts |
| Diff correctness and security | Did the patch introduce defects, regressions, unsafe authority, or missing tests? | Read the exact commit, diff, relevant code, and tests; return actionable findings |
| Simplification and architecture | Can complexity be removed without losing proven behavior? | Read code and decisions; propose bounded deletions or restructuring; advisory by default |
| Performance | Does the result satisfy a declared resource or latency target? | Run pinned benchmarks or profiles; block only on an accepted measurable threshold |

A plausible first execution shape is not a mandatory order:

```text
                         ┌─ acceptance/QA AgentRun ─────────────┐
implementation commit ──┼─ diff correctness AgentRun ─────────┼─ findings and Evidence
                         └─ simplification AgentRun (optional) ┘          │
                                                                       ▼
                                                         implementation Thread
```

Give each selected Role a bounded AgentRun against the exact Revision, with role-appropriate tools
and a scoped provider route. The first product gives every selected Role its own
disposable Sandbox, including read-only reviewers. That uniform isolation boundary is an intentional
simplicity trade: one Role, one environment, one route, one pair of cleanup Actions when the
workflow or client requests cleanup. Reconsider shared or
Sandbox-free review only when measured startup cost or latency outweighs the clarity and isolation.

The original coding Job, branch, and proposal remain authoritative. A verifier AgentRun is attached
to one Revision, not another Job, branch, or proposal. Its feedback returns through an ordinary
Message to the implementation Thread. Agent conclusions remain claims;
workflow-observed commands, interactions, measurements, and retained artifacts become facts.

Promote a role through evidence rather than enthusiasm:

1. blind historical evaluation against known failures and clean cases;
2. live shadow execution that cannot affect readiness;
3. opt-in advisory feedback;
4. default advisory use for matching risk classes;
5. blocking authority only after calibration; and
6. demotion or removal when its marginal value disappears.

Measure unique material findings or newly proven acceptance items, overlap, false positives,
reviewer-induced regressions, repair churn, latency, provider and Sandbox cost, cleanup, and escaped
defects. Roles may run in parallel against one immutable commit when their environments and
capabilities permit it. A repair creates a new commit and invalidates affected evidence; rerun only
the roles whose evidence became stale. Ordering remains an empirical workflow choice rather than a
generic graph scheduler built in advance.

Core mechanisms: isolated Sandbox lifecycle and capability boundaries, durable Job input, and
Revision-pinned claim/observed Evidence. If bounded AgentRuns cannot compose through those
mechanisms, settle that Core boundary before encoding Role semantics into the workflow.

## Autonomy labels and roads

Named autonomy levels (propose, act in room, act and publish, act and spend) are workflow labels
printed on the runtime's boundary envelope. The workflow may suggest a level per task from track
record, and should explain what would earn more autonomy.

Roads before self-driving: autonomy is widened by building better roads, not by trusting the driver
more. Deterministic setup, exhaustive checks, and smoke contracts are the roads; they widen what
after-the-fact approval can safely cover. Models improving moves the default dial position; it
never removes the dial, because irreversible and spending actions stay gateable by construction.

Core mechanism: enforcement and observation of the admitted boundary envelope (egress, injected
secrets, brokered verbs). A workflow defines what authority it needs; a client gathers any human
approval and decides whether to admit or continue the Job.

## Calibrated agent configurations

A Role, model, and reasoning configuration accrues a per-repository track record. The pulse can then
say: "self-estimate: done in 12m; historically 30% optimistic here." The DX of trust is not only
seeing what an agent claims but knowing what claims from that configuration have historically been
worth. That calibration helps the orchestrator and human allocate attention without inventing a
cross-Job Worker identity.

Core mechanisms: provenance and durable attachment for workflow-owned estimate and outcome facts.
Defining those facts, computing cross-Job calibration, and displaying it are workflow or client
work.

## Coding attention semantics

The Core attention contract retains a factual reason and scope. The coding
workflow decides what requires attention; the client chooses urgency, batching, notification, and
where the human responds. A red check mid-job may be digest material, while an irreversible choice
may be a ping, but neither presentation policy belongs in Core.

At publish time the evidence pack is projected into the PR (body, comments, checks) so review
happens where reviewing already lives. GitHub gets the story at the moment it matters; the job
facts retain the full story and inspection derives its history.

Core mechanisms: explicit attention, the inbox, and typed facts and Artifacts with provenance. The
coding workflow derives current work without storing a second state channel; the client renders and
routes the resulting history and attention.

## Dogfood bar: the phone-only week

The north star's harsher stranger test is completing the loop from a phone through an ordinary
client. The showcase bar is
crueler: run a real week of coding work phone-first, with the laptop as break-glass only. Every
step that forces a terminal is a finding. Passing the loop test is necessary; surviving the week is
the evidence that AFK capacity is real.

## How to use this document

| Do | Do not |
| --- | --- |
| Judge workflow-layer PRs against these ideals | Encode any of them into runtime surfaces |
| Name the Core mechanism and policy-owning layer | Reach into Core with workflow or interaction policy |
| Let dogfood evidence promote an ideal into work | Treat this document as a backlog |
| Record accepted choices in the [Decision Log](../project/decisions.md) | Build showcase polish before the stranger loop is excellent |
