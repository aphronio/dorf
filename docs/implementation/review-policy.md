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
detached worktree, a distinct Codex native Session discovered through that exact worktree path, a
stable Session Action, and a stable turn Action. Codex receives `approvalPolicy=never` and a
read-only sandbox policy. The implementation and repair turns alone use the original writable
workspace and implementation Session.

Git worktree registration is serialized because it mutates shared repository metadata. After every
selected run has a distinct worktree, Revision, Action, and native binding, immutable read-only
review turns may overlap. Any other capability class is serialized. The native accept/before-bind
retry reads the exact worktree's thread history and converges on the existing turn.

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
deleted. Cleanup refuses to finish while a review AgentRun is unsettled, removes each exact detached
worktree through its cleanup Action, revokes the route, and deletes the Sandbox. Plans, claims,
observations, latency, usage availability, yield, adjudication, and cleanup facts remain retained.

## Existing-Job dogfood activation

The narrow activation command consumes the durable `review-activation` boundary reached after
exact-Revision Checks. It verifies the current Check Evidence blobs before changing the phase,
persists the implementation request once, and runs the existing Job synchronously under its Job
fence. It does not create a Job, branch, Sandbox, or implementation Session.

After rebuilding and applying migrations through 008, use the exact identifiers printed by
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
