# Go Review Policy and Revision-bound AgentRuns

Issue #42 extends the explicit Go coding phase machine after independently verified, exact-Revision
Checks. It does not add a generic workflow graph or coordinator. Review selection is a persisted
application decision; reviewer prose is retained as claim Evidence and never satisfies a Check.

## Deterministic selection

`internal/review` owns the pure `ChangeFacts -> ReviewPlan` rules. Change paths come from the Git
diff between the admitted starting Revision and the current clean full commit. The current
`.dorf.toml` may declare `review.performance = true`; this can only add review. Green documentation-
only changes select an explicit `no-review` result. Browser/UI, authentication/authority, and
declared-performance facts add their mandatory allowlisted Roles. An implementation-requested Role
is unioned with that floor and attributed to the original implementation AgentRun. Unknown paths
admit exactly one `review-triage` AgentRun, whose bounded JSON result may only add allowlisted Roles.

After exact-Revision Checks are independently verified, the Job durably waits in
`review-activation`. The orchestrator invokes `dorf review activate` with the implementation
AgentRun's allowlisted requests, or with no requested Roles to bind an explicit empty set. That
activation and attribution are persisted atomically before policy evaluation. The first policy
result and its digest are atomic for `(Job, Revision)`; a retry either observes the same activation
and result or stops on conflict. Invalid or unsafe requested Roles block visibly. No-review is a
final persisted plan, not the absence of review rows.

## Native and Evidence boundaries

Each triage or review AgentRun has a stable identity derived from Job, Revision, and Role. It owns a
fresh Incus reviewer Sandbox that is distinct from the implementation Sandbox and every other Role.
Host-owned Incus metadata and durable facts bind that VM to the Job, AgentRun, exact Revision, and an
unpredictable ownership nonce. A foreign, stale, duplicated, or ambiguously owned VM is never
adopted. The implementation and repair turns alone use the original writable workspace and
implementation Session.

Before installing a provider route, Dorf exports only the Git objects reachable from the admitted
clean implementation HEAD and materializes them in a detached reviewer checkout. It removes remote
and ref reachability, prunes unreachable objects, and observes exact HEAD, tree, and clean state.
The reviewer VM never runs repository setup, Check, or smoke commands. Each VM receives its own
scoped provider route and independently controlled Codex app-server. Its durable logical controller
identity is derived from the AgentRun, reviewer Sandbox, and host-attested ownership nonce; the
random WebSocket control token is only a rotating authentication capability. Exact HEAD/tree/clean
state is observed again after the native turn and before any claim Evidence can be recorded.

Before native submission, Dorf durably records a random stable submission nonce and the exact input
digest. Review-only recovery performs bounded Session discovery and accepts exactly one turn only
when its persisted user message has that nonce and byte-exact input. It also attests the bound
Session, app-server control identity, model, reasoning effort, `approvalPolicy=never`, and read-only
policy. Every Initial/Turns/Wait operation re-attests reviewer Sandbox ownership, including after an
app-server process replacement. Missing or mismatched identity, prompt, policy, extra turns, or
competing Sessions stop with attention and cannot produce review Evidence. This strict path does not
change legitimate recovery for the original implementation Session.

After every selected run has a distinct reviewer Sandbox, route, immutable checkout, Action set,
and native binding, read-only review turns may overlap. Any other capability class is serialized.

A terminal JSON finding is retained as `claim` Evidence. A separate `observed` artifact records the
native Session, turn, outcome, capability, bounded timing, and token/cost fields when the harness
provides them. Inspection reports usage availability explicitly. Material-finding yield and
adjudication are stored separately from Check observations.

## One adjudication and targeted repair

Exactly one material finding may return once through a workflow-owned message to the original
implementation Session. A clean workspace after adjudication records a rejected/false-positive
claim and keeps the Revision. A changed workspace records a new deterministic Git Revision. Every
declared Check reruns because Check Evidence is exact-Revision proof; only the affected review Role,
plus any mandatory deterministic floor, is selected on the repaired Revision. There is no broad
review loop, and a second material result or a failed Check after that bounded review repair blocks.

Historical Checks and review claims remain inspectable and are marked historical/stale rather than
deleted. Cleanup refuses unsafe mutation recovery, but an isolated read-only reviewer run stopped
with attention can still have its exact resources reclaimed. Cleanup revokes each reviewer route,
removes its exact checkout, and deletes its metadata-attested reviewer Sandbox through stable scoped
Actions. Original implementation route/Sandbox cleanup remains separate. Plans, claims,
observations, latency, usage availability, yield, adjudication, resource ownership, and cleanup
facts remain retained.

## Existing-Job dogfood activation

The narrow activation command consumes the durable `review-activation` boundary reached after
exact-Revision Checks. It verifies the current Check Evidence blobs before changing the phase,
persists the implementation request once, and runs the existing Job synchronously under its Job
fence. It does not create a Job, branch, Sandbox, or implementation Session.

After rebuilding and applying the final review-policy schema through `007_review_policy.sql` (migration 007), use the exact identifiers printed by
`dorf inspect --json JOB_ID`:

```bash
go build -o .dorf/bin/dorf ./cmd/dorf
.dorf/bin/dorf migrate
.dorf/bin/dorf review activate --revision EXACT_FULL_COMMIT_OID --requested-role critical-boundary JOB_ID
```

Omit `--requested-role` unless the implementation AgentRun requested that additional allowlisted
Role. `--requested-by-agent-run ORIGINAL_IMPLEMENTATION_AGENT_RUN_ID` is optional; when omitted,
Dorf resolves and verifies the original completed implementation AgentRun. On controller loss,
rerun the byte-identical activation command. A changed Revision, requested Role set, or attribution
conflicts instead of creating new identity.

Then inspect retained claims versus observations and independently verify Evidence:

```bash
.dorf/bin/dorf inspect --json JOB_ID
.dorf/bin/dorf evidence verify JOB_ID
```

The issue's real Incus/Codex interruption, material-finding repair, measurements, and cleanup remain
dogfood evidence; unit or PostgreSQL row state does not substitute for that terminal.
