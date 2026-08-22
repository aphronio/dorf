# Core Application Refactor Roadmap

Status: temporary implementation aid. Delete this file when the replacement is authoritative and
move any consequential, still-relevant choice to the
[Decision Log](../project/decisions.md).

This roadmap sequences a refactor toward the application experience below. It is subordinate to the
[North Star product boundary](../project/north-star.md#product-boundary),
[Architecture](../project/architecture.md), [Principles](../project/principles.md), and accepted
[Decisions](../project/decisions.md). It does not create a public compatibility promise.

## Target application experience

The ownership sequence and behavior below are agreed. The private Go spellings, option encoding,
receipt types, and return timing are illustrative and remain implementation choices. Brackets show
an optional behavior, not a proposed Go overload or public compatibility promise.

```text
application.Admit(ctx, complete-core-intent) -> Job
job.EnsureSandbox(ctx[, sandbox-name]) -> Sandbox
sandbox.Agent() -> Agent
agent.Message(send-key, text[, Steer])
sandbox.ReadFile(workspace-relative-path)
job.RequestCleanup
```

- `application.Admit` atomically accepts complete Core intent under one stable caller key and returns
  the durable Job handle. Workflow-specific typed input remains owned by its module rather than
  becoming optional Core fields or a generic payload. The concrete transaction composition is an
  implementation seam for the first slice, not a workflow-author burden.
- `Job` is the durable aggregate and owner of every Sandbox.
- `EnsureSandbox` reconciles one Job-owned Sandbox using the verified profile pinned when the Job is
  admitted. Default and named Sandbox behavior is validated during the first slice rather than
  frozen as an option syntax here.
- `Sandbox.Agent()` is the ordinary convenience handle for the profile-selected Harness in that
  Sandbox. It is not a new durable Agent identity.
- Each `Agent.Message` send has a caller-retained per-send idempotency key. An upstream module may
  pass through an already-stable operation ID, but the boundary must not derive identity from content
  or sender or hide it behind an unrecoverable generated value. The key binds the complete admitted
  request: exact Sandbox, text, follow or steer intent and target, authorized Role, capability and
  input Revision when used, and Thread reuse choice. A send defaults to follow. `Steer` is explicit
  priority intent against active work and retains the existing observable targeting and fallback
  semantics.
- `AgentRun` remains the internal durable recovery record for exactly one Message delivery, including
  its Harness, Thread, Turn, role/capability envelope, and uncertain-submission reconciliation. An
  ordinary workflow author does not create, bind, poll, or interpret AgentRuns directly.
- `Sandbox.ReadFile` returns the exact bytes of one clean workspace-relative regular file from that
  exact Job-owned Sandbox while cleanup remains pending. A caller or workflow knows the path and
  reads it before requesting cleanup; Core does not discover, interpret, or retain outputs.
- `job.RequestCleanup` records caller policy. Core reconciles route revocation and Sandbox deletion;
  it does not decide when cleanup should be requested.
- An open Job may be idle without a workflow-owned `WaitForInput` operation. Durable Message
  admission and wake/recovery machinery resume work when input arrives.

This is an application boundary for the deployed control plane. It is not a workflow DSL, dynamic
registry, plugin contract, public network API, or language SDK.

## Invariants

- Core owns durable custody; a client or workflow owns intent, interaction policy, result meaning,
  and cleanup timing.
- A Job owns Sandbox lifetime. Agent and AgentRun handles never own infrastructure.
- One admitted Message selected for agent delivery maps to one internal AgentRun. Recovery reconciles
  that identity instead of creating another judgment attempt.
- Every Message send has a caller-retained per-send idempotency key. An exact retry returns the same
  durable input; changing any part of its complete admitted request conflicts; a different key may
  admit identical text.
- A Sandbox handle is bound to that exact Sandbox for file reads, and its Agent handle is bound for
  submission, history reconciliation, waiting, and steering. Neither falls back to the Job's
  default Sandbox.
- Follow order remains Job-local FIFO. A steer remains explicit, targeted, observable priority input.
- Harness Threads and Turns remain Harness authority. Provider locators, command transports, and
  workspace paths remain adapter-private.
- Sandbox file reads are live operations against Sandbox authority, execute under the Job cleanup
  fence, and become unavailable after cleanup. Workflow-owned typed results provide durability when
  the workflow needs it; file content is not Evidence merely because a caller read it.
- Cleanup is explicit, idempotent, retryable, ordered route-before-Sandbox, and visibly incomplete or
  failed until external authority confirms release.
- PostgreSQL owns Dorf product facts; Absurd owns task claims, retries, waits, wake events, and
  cancellation. Do not duplicate either authority.
- Native workflows and direct clients use the same Core application behavior. Compiling a workflow
  into the binary grants no hidden authority.
- Common workflow code contains no Harness- or Sandbox-specific branch beyond verified profile
  selection and capability admission.

## Forbidden directions

- Do not move Git, coding, review, HITL, GitHub, investigation, acceptance, or publication meaning
  into Core. Modules such as `gitrepo`, `coding`, HITL, and GitHub compose over Core.
- Do not add a DSL, generic DAG, workflow or provider registry, plugin system, marketplace, public
  SDK, network transport, authentication model, or generic result/metadata bag.
- Do not expose AgentRun as the normal workflow-authoring surface or create a second durable Agent,
  Worker, conversation, phase, wait, or status primitive around it.
- Do not make idle input a workflow phase or require every workflow to spell a `WaitForInput` step.
  Internal Absurd waits remain execution mechanics, not workflow meaning.
- Do not make Core discover, interpret, or retain generic output files, inject output conventions
  into stock agent behavior, or add listing, batch, archive, or directory-download file APIs.
- Do not mirror lifecycle state beside settled Actions, provider identity in Core, mutable state in
  two authorities, or adapter-private workspace paths in durable records.
- Do not preserve internal package shapes, compatibility facades, dual paths, old schemas, or coupled
  tests after their replacement proves the same behavior.

## Slice protocol

Every slice starts with seam discovery. Before editing, trace the current call path, durable facts,
Absurd boundary, adapter authority, tests, operator surface, and deletion candidates for that slice.
Record only conflicts or newly consequential choices below; current code shape is evidence, not a
constraint.

Each slice must then:

1. replace the narrowest complete behavior through the application boundary;
2. add behavior-level and recovery/fault coverage at the changed authority;
3. run the repository verification contract in [AGENTS.md](../../AGENTS.md#verification), including
   PostgreSQL integration when durable facts, transactions, or sequencing change;
4. reach the smallest real terminal claimed by the slice; and
5. delete the replaced path, redundant state, adapters, tests, and narration before proceeding.

Internal Go types, interfaces, stores, schema, task arrangement, and package boundaries may change
freely when that makes the slice smaller. They may not change the agreed ownership semantics or the
North Star ownership boundary.

## Vertical slices

### 1. Existing workflow Job to claimed Job-owned Sandbox application path

**Seam discovery:** Trace complete Job input, atomic Core and workflow-fact insertion, idempotent
replay, scheduling attachment, profile pinning, Sandbox create/reconcile Actions, route ownership,
cleanup inventory, provider adapters, and every workflow-specific wrapper over those paths.

Start from the existing typed workflow admission transactions. Open their durable Jobs through an
opaque Job handle, reconcile the default or named Job-owned Sandbox only from the exact attached
Absurd task through its pinned verified profile, and request cleanup through the same handle.
Preserve atomic workflow admission replay, predecessor-bound task attachment, Sandbox ownership,
Action reconciliation across process loss, and route-before-delete cleanup. Workflow authors must
not know PostgreSQL, Absurd, raw provider types, or a workflow registry.

Prove default and named Sandbox identity, idempotent ensure, foreign-resource refusal, pinned-profile
resolution, claimed execution authority, stale-task refusal, ensure-versus-cleanup serialization,
cleanup-request recovery, and route-before-Sandbox cleanup in PostgreSQL-backed tests. Delete the
superseded native-workflow Sandbox-create path once coding and investigation both use the handle.

This slice does not introduce `Application.Admit`, workflow-less durable Jobs, or a synchronous
direct-client execution path. Direct admission and its live terminal proof are deferred until an
actual trusted client or transport earns a durable scheduling and claim-recovery path. That
sequencing narrows the implementation slice; it does not change D088's target Core ownership.

### 2. Agent Message convenience with hidden AgentRun recovery

**Seam discovery:** Trace typed Message authorization, transactional Message/AgentRun insertion,
follow FIFO, steer targeting and fallback, Harness submission/observation, Thread reuse, wake events,
and workflow code that currently manipulates AgentRuns.

Make `sandbox.Agent().Message(...)` the ordinary path while retaining AgentRun as the internal
delivery and recovery record. Preserve a caller-retained per-send idempotency key for exact replay,
default to follow, and expose steer only as the explicit option. Whether the private Go method
returns an immutable admission receipt or waits for settled work is decided from the existing
recovery seam; cancellation must never retract accepted input. Keep workflow-owned role, capability,
and revision policy outside the convenience handle, behind narrow module composition where needed.

Prove follow, active-turn steer, terminal-target fallback, concurrent admission, uncertain submission,
process restart, and no duplicate Turn. Exercise two named Sandboxes in one Job and prove that
submission, lost-ack recovery, steer, wait, and file reads stay bound to the selected one;
replaying its Message key through the other must conflict. Reach a live initial Message, follow, and
active steer through one supported Harness. Delete ordinary authoring access to prepare/bind/poll
AgentRun operations and any duplicate delivery façade after all call sites move.

### 3. Exact caller-selected Sandbox file reads

**Seam discovery:** Trace current workspace conventions, provider command transports, exact Sandbox
ownership, cleanup fencing, and callers that already know a produced file path. Keep file discovery
and interpretation at the calling workflow or client boundary.

Expose one provider-neutral `Sandbox.ReadFile` operation for a clean workspace-relative regular
file. Preserve exact arbitrary bytes through provider transports, reject traversal, symlinks, and
resolved escapes, and perform the read under the same Job fence as cleanup. Multiple files are
repeated calls; discovery uses the existing Sandbox command seam rather than another filesystem API.

Prove exact text and binary bytes, repeated reads, cross-Job and cross-Sandbox refusal, path escape
refusal, the read-versus-cleanup fence, and unavailability after cleanup. Reach a live caller-known
file, retrieve it exactly before cleanup, clean up the Sandbox, and prove a later read fails. Remove
the generic Artifact domain, storage, and CLI; retain only workflow-owned typed result facts and
existing blob custody for Evidence and retained Git input.

### 4. Open-idle Job and input resumption

**Seam discovery:** Trace each coordinator's no-current-work branch, Absurd task attachment and waits,
Message wake emission, retry, attention, cleanup fencing, and every `WaitForInput`-shaped workflow
operation or presentation state.

Let a coordinator return with an open, idle Job when no operation is currently required. Core input
admission must durably attach or wake the required execution without a workflow-specific wait step.
Cleanup must close admission and win safely against concurrent input according to existing custody
rules.

Prove idle persistence, later follow resumption on the same Thread, steer during active work, restart
while idle, concurrent Message/cleanup fencing, bounded lost-wake reload without a separate or busy
poller, and no duplicate task attachment. Reach a live Job that becomes idle, receives a later
follow, lets its client read the current workflow-owned output, and then cleans up. Delete workflow wait
operations, phase-like projections, and tests that assert their
existence rather than observable idleness.

### 5. Compose repository and interaction modules over Core

**Seam discovery:** Trace Git checkout/materialization, coding policy, investigation policy, review,
human-attention boundaries, GitHub Actions/observations, typed workflow facts, and their imports of
Core internals. Mark every place physical reuse is being mistaken for Core ownership.

Move consumers to static, typed composition over Job, Sandbox, Agent, exact file reads, and cleanup
capabilities. Message remains immutable input or an admission receipt returned through the Agent
handle, not a separate lifecycle interface. A repository module owns exact checkout/observation;
coding owns revision, review, proposal, and outcome policy; HITL remains client/workflow interaction
policy; GitHub owns its
external-authority adapter and coding-facing composition. Investigation retains its own source and
`REPORT.md` prompt/path policy while callers use the shared exact-file read. No module receives
authorities it does not need.

Prove each native workflow independently against narrow Core fakes and PostgreSQL, then run its real
end-to-end terminal. Delete horizontal composition packages, central workflow switches, optional
authority bundles, and Core shortcuts used by only one module.

### 6. Converge, dogfood, and remove the refactor scaffolding

**Seam discovery:** Re-scan imports, schema, generated queries, task registrations, CLI/operator
presentation, authority docs, tests, and dead code for the old application shape. Compare inspection
and recovery behavior with the same authoritative facts, not old type names.

Make the smallest final corrections required by live evidence. Update the existing operator
authority and Agent Guide in the implementing slice when setup, profile readiness, Job operation,
Messages, retry, file retrieval, or cleanup UX changed. Do not preserve temporary adapters to make the
diff easier to merge.

Prove the complete target below, run the full repository contract, regenerate/check SQL when its
sources changed, and remove compatibility layers, dead schema/query sources, coupled tests, stale
documentation, feature flags, and this roadmap. Any accepted consequential change discovered during
the work belongs in the Decision Log before this file is deleted.

## Live proof target

The refactor is complete only when fresh real Jobs prove the same application path on the relevant
current dogfood terminals:

- workstation: one verified Incus profile;
- cloud controller: one verified E2B profile; and
- across those runs, both supported Harnesses are exercised so provider/Harness variation occurs
  only through profile selection.

Before the refactor is complete, an earned trusted-client or transport slice must admit at least one
direct Job without workflow identity through the Core application path; slice 1 deliberately does
not manufacture that caller or its scheduling authority.
At least one run must become open and idle, accept a follow on its continuing Thread, accept an active
steer without a duplicate Turn, expose a caller-known agent-written file through an exact read before
cleanup, survive controller
restart or forced executor loss at an AgentRun reconciliation boundary, accept an explicit cleanup
request, confirm route and Sandbox absence, and prove the file is unavailable afterward. The
coding and investigation workflows must also retain their existing real terminal behavior through
module composition over the same Core path. This is a refactor proof, not permission to add another
provider, Harness, workflow, or public surface.

## Decision and assumption log

Locked for this refactor:

- Job is the durable owner; Agent is a convenience handle; AgentRun is internal recovery truth.
- Core admits direct Jobs; workflow-driven Jobs add typed module-owned input without exposing
  PostgreSQL or Absurd to workflow authors.
- Message sends use a caller-retained per-send idempotency key; default intent is follow and steer is
  explicit.
- A Sandbox exposes one exact caller-selected regular-file read while cleanup remains pending; Core
  does not prescribe an output directory or retain generic outputs.
- Cleanup is requested by a client/workflow and executed by Core.
- Open idleness does not require a workflow-owned wait step.
- Workflow/domain modules compose over Core; no DSL, registry, plugin contract, or public SDK is in
  scope.

Working assumptions to validate during seam discovery:

- Omitting a Sandbox name maps to the existing deterministic main/default Sandbox identity; an
  explicit name is unique within the Job.
- `Sandbox.Agent()` resolves the Harness pinned by the Job's verified profile and creates no durable
  Agent row.
- The Job's admitted profile applies to every Sandbox in this refactor; independently selecting a
  profile per named Sandbox is a future contract, not an implied option.
- File reads use the exact Job-owned Sandbox and the Job cleanup fence. They reject traversal,
  symlinks, resolved workspace escapes, directories, and non-regular entries without inventing a
  broader output policy.
- Removing workflow `WaitForInput` does not remove Absurd's internal durable wait/wake mechanics or
  change workflow-owned attention semantics.

## Escalation criteria

Stop the affected slice and ask the user before implementation diverges when discovery indicates:

- the locked call shape cannot preserve current recovery, authority, or supported profile behavior;
- exact file reads cannot preserve arbitrary bytes, Sandbox ownership, or the cleanup fence through
  a supported provider transport;
- a proposed durable fact interprets workflow result, acceptance, rejection, human judgment,
  cross-Job composition, or cleanup timing;
- a supported Harness cannot fit the hidden AgentRun boundary, or a provider cannot preserve
  Job-owned Sandbox custody;
- removing workflow `WaitForInput` would lose accepted input, create competing execution, or require
  a new product-level idle state rather than ordinary Absurd mechanics;
- a public compatibility promise, transport, SDK, registry, plugin system, DSL, or new product term
  appears necessary; or
- live proof requires materially broader infrastructure, credentials, spend, or external effects
  than the current verified terminal and workflow authorize.

Ordinary internal reshaping, schema replacement before release, deletion of coupled tests, and
selection among equivalent private Go signatures do not require escalation when all invariants and
observable behavior remain intact.

## Coordinator and subagent operating protocol

- The coordinator owns this roadmap, slice order, product-boundary review, integration, final live
  proof, and deletion decisions. Only one slice is integrated at a time.
- Begin each slice with read-only scouts where parallel discovery is useful. Give each scout one
  disjoint seam and ask for current call paths, authorities, invariants, tests, deletion candidates,
  and unresolved choices. Scouts do not make product decisions or edit files.
- If implementation is delegated, assign non-overlapping file/module ownership and one observable
  terminal. Tell every agent it is not alone in the codebase, must preserve others' edits, and must
  not add compatibility layers outside its slice.
- Every handoff reports files changed, behavior proved, commands run, live evidence, deletions made,
  assumptions validated, and remaining escalation items. A passing mocked seam is not a completed
  slice.
- The coordinator reviews the combined diff for forbidden directions and duplicate authorities,
  runs integration verification, and removes superseded code before opening the next slice.
- Parallel work may investigate later slices, but must not implement against an application boundary
  that the current slice has not yet made authoritative.
