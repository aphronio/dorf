# Dorf dogfood resistance backlog

This is a non-normative record of resistance observed while operating Dorf against Dorf. It is not
a product requirement, accepted design, or implementation plan. Discuss each item separately and
earn its smallest vertical slice before changing Core, deployment tooling, or client integrations.

## Observed terminals

On 2026-08-18, the first local `codebase-investigation` dogfood Job reached Sandbox creation but
could not clone its repository. The Incus guest had no forwarded DNS or TCP connectivity because
UFW allowed `incusbr0` traffic through stale interface `wlan0` while the host used `wlp7s0`. The
attached task exhausted its five attempts. Dorf then recorded an abandoned outcome, revoked its
route, deleted its Sandbox, and completed cleanup without a Report.

After replacing only that stale UFW route, a fresh Incus VM proved numeric TCP, DNS, HTTPS, and Git
access. A second investigation Job completed its AgentRun, retained an 8,312-byte Markdown Report,
completed its Absurd task with `report-recorded`, revoked its route, deleted its
Sandbox, and reached complete cleanup.

## Resistance to discuss

### P0 — Deployment configuration has competing authorities

The deployment record selected image `dorf-codex`, while commands without reconstructed environment
variables selected default image `dorf`. The development database also required repeating an exact
DSN for every command.

Discuss one deployment configuration authority consumed consistently by setup, doctor, admission,
workers, inspection, retry, and cleanup. Do not add another source of derived configuration.

### P0 — Readiness does not prove the path a workflow will use

Ordinary doctor checks proved Incus daemon access, bridge presence, image presence, and Gateway
authority but did not detect that a fresh guest could not resolve or reach its repository. The Job
spent all bounded attempts discovering a host forwarding problem.

Discuss an explicit, mutating profile proof that creates a disposable Sandbox, exercises the exact
repository and Gateway paths required by the selected workflow, and proves cleanup. Keep ordinary
doctor observational and do not silently rewrite operator firewall policy.

### P0 — Inspection under-reports attached task failure

After the main Absurd task exhausted its attempts, human inspection still emphasized the projected
workflow Action. Discovering the terminal failure, attempt count, and last error required scheduler
inspection.

Discuss rendering the attached task state, bounded last error, attempt exhaustion, and exact
`dorf retry JOB_ID` remediation in ordinary Job inspection without duplicating scheduler authority.

Smallest slice implemented on 2026-08-18: while admission remains open, human inspection now leads
with actionable `workflow stopped` attention, the derived current operation, a bounded one-line
reason, and the truthful retry command. Closed Jobs do not present historical execution failure as
current attention. JSON exposes the sanitized execution facts regardless. Exact attempt counts
remain in Absurd because its public v0.5 inspection contract does not expose them; Dorf does not
query private queue tables or copy scheduler state merely to print `5/5`.

### P0 — There is no durable follow experience

Following the Job required repeated `inspect` invocations or an external polling loop. Worker output
was intentionally quiet but did not offer another bounded transition view.

Discuss `dorf inspect --follow JOB_ID` or an equivalent client projection that emits durable state
transitions without streaming transcripts, guessing progress, or becoming a second authority.

Smallest slice implemented on 2026-08-18: `inspect --follow` tails workflow-owned chronological
history and exits on actionable attention or complete cleanup. An interactive terminal refreshes a
fixed live block every second with human current state, per-AgentRun elapsed time, and Sandbox role,
provider profile, and provisioned time; redirected output keeps a bounded one-minute append-only
pulse. It does not yet expose Harness activity, provider running/paused intervals, billable time,
notifications, or pause policy; those remain separate dogfood-driven slices.

### P1 — Development dogfood can collide with integration fixtures

The disposable test database contained many scheduled fixture Jobs. Starting a real Worker against
that database could claim test work, so the run needed a separately initialized `dorf_dogfood`
database.

Discuss a repository-owned, isolated dogfood environment with an unambiguous database and queue.
It must not make tests and real dogfood mutually destructive.

### P1 — Identical infrastructure failures consume attempts quickly

The same clone/DNS error consumed five attempts in roughly ninety seconds. Existing retry semantics
remained truthful, but the operator had little time to intervene.

Discuss backoff and observable repeated-failure attention at the task boundary. Do not add workflow
branches that reinterpret Absurd attempt authority or claim a failed task resumed automatically.

### P1 — Provider Gateway process lifecycle is not obvious

`provider connect` reconciled and started the private broker, but discovering whether the broker was
stopped, healthy, or intentionally persistent required knowledge beyond the command surface.

Discuss explicit provider status and lifecycle ergonomics or supervised deployment ownership.
Retain the Gateway as a sibling authority rather than folding it into Job sequencing.

### P1 — Unpublished local revisions cannot be investigated

The isolated workflow could clone only a revision reachable through the admitted remote repository.
It therefore investigated `origin/main`, not the unpushed feature implementation being dogfooded.

Discuss a later content-addressed Git bundle or checkout snapshot as an alternate typed repository
source. Preserve exact identity, clean-checkout proof, provider neutrality, and cleanup; do not add
ambient host mounts.

### P1 — Workflow result retrieval is not obvious

The investigation Report was durable and inspectable, but retrieving its Markdown required
`dorf inspect --json JOB_ID | jq -r .report_markdown`.

Discuss a workflow-owned result projection or a discoverable Artifact command. Dorf should
expose the result contract without becoming the interaction layer that decides where to publish it.

Smallest slice implemented on 2026-08-19: workflow deliverables are immutable named Artifacts, not
Evidence or a generic result bag. Investigation records `report.md`; `dorf artifact list JOB_ID`
lists metadata and `dorf artifact get ARTIFACT_ID` emits exact verified bytes. Inspection
points to retrieval without embedding potentially large or binary content.

### P2 — Report citations are Sandbox-local

The useful Report cited exact files and lines, but links used `/workspace/job/...`, which is not a
durable consumer location after Sandbox deletion.

Discuss repository-relative citations or canonical links pinned to the admitted revision.

### P2 — Empty capability lists serialize as `null`

Admission reported `required_provider_capabilities: null` when the workflow required none.

Discuss normalizing collection-shaped public output to `[]` while leaving the internal absence of
optional capabilities semantically unchanged.

### P2 — Visible client coordination repeats deployment details

Creating a followable Herdr run required manually reproducing database, image, provider, Job,
Worker, and observer commands.

Discuss a thin client or skill driven by the workflow input/output contract. Keep presentation,
Slack/GitHub tags, Herdr layout, and notification policy outside Dorf Core.

## Deliberate non-goals

Do not treat this backlog as justification to:

- install every repository tool in the generic Sandbox artifact;
- add a provider registry or fine-grained capability matrix;
- make investigation run arbitrary repository setup or tests by default;
- move interaction-channel policy into Dorf Core;
- generalize explicit workflows merely to reduce line count;
- silently change host firewall, network, or provider configuration; or
- move Harness custody out of the Sandbox before a concrete use case earns that boundary.

## Suggested discussion order

Start with truthful failure visibility and follow ergonomics because they reduce operator
babysitting without changing workflow or provider authority:

1. attached task failure in `inspect`;
2. durable `inspect --follow` behavior;
3. obvious workflow result retrieval;
4. deployment configuration authority;
5. explicit live profile proof.

Re-rank the remaining items only from additional dogfood evidence.
