# Dorf architecture

The [North Star](north-star.md#product-boundary) owns responsibilities. The
[Remote Control API](../control-api.md) owns client semantics. The pinned
[native contract](../implementation/native-session-contract.md) owns Harness evidence.

## Accepted native boundary

Dorf owns Session configuration, one native Thread binding, exact resource custody, scoped model
access, maintenance and release. The Harness owns input placement, Turns, history and execution.
Clients own unsent application input and business outcomes. There is no Dorf Message or Turn table.

## System shape

```text
Client -> authenticated API -> privileged worker -> native Harness
                 |                    |                 |
                 +--- PostgreSQL -----+           mutable workspace
                        |             |
                   Absurd lifecycle   +--- provider and model adapters
```

The API has no provider or model credentials. Fixed worker routes attest exact Session resources;
they do not expose arbitrary native JSON-RPC. Lifecycle work remains on the existing Absurd loop.
Ordinary input is one bounded native request, not a queued workflow or a lifetime replay step.

## Authority model

PostgreSQL retains Session admission, immutable profile selection, Thread binding, resource
generations, fixed lifecycle effects, maintenance holds and recovery receipts. Native history owns
execution results. Ephemeral observations accelerate reads but cannot reconstruct lost native work.

Each native dispatch increments a Session revision and records its exact unresolved input correlation
or cancel Turn before writing to the Harness. Input correlation includes a private random dispatch
suffix so an older use of the caller's correlation cannot settle a later unknown request. An
acknowledgement clears the guard by revision compare-and-set. Response loss leaves the guard; exact
positive native evidence may settle it. Missing history never authorizes a resend.

The revision invalidates older checkpoints. It is a maintenance cutoff, not an input record or a
native execution sequence. Accepted payloads, outcomes and attachment bytes are not stored in Dorf.

The Session effect fence serializes native mutations, maintenance and resource release separately
from queue claims. A lease or connection failure cannot recall an already dispatched request.
Resource-generation ownership and pending-effect reconciliation remain necessary after claim expiry.

## Execution model

Session creation atomically admits resource intent and schedules its existing lifecycle task through
Absurd's public SQL API. Input waits on resource readiness at the caller. The native adapter creates
and binds a Thread before dispatch. Only a binding with no previously authorized input may be replaced
when an empty native Thread disappears after restart.

Task startup validates the committed current task ID, name and lifecycle state under the Session
fence. It cannot attach itself or repair scheduling. Creation and cleanup scheduling own atomic
spawn and attachment; explicit retries retain the same task identity.

One authenticated native connection covers configuration and input, then transfers observation to
the worker. Native `turn/start` selects start versus steer. Cancellation captures the current Turn
once and calls exact native interrupt without retargeting. Neither path retries a native mutation.

The controller observes native activity and fixed maintenance work. It never selects input or copies
Turn outcomes. Completion notifications wake it as hints; bounded observations repair missed hints.
Pause requires native idle and no unresolved mutation. Provider status prevents lifecycle polling
from waking paused compute. User file/history/command operations retain activity semantics.

### Native observations

The bounded in-memory reply cache is scoped to resource ownership, Thread and Turn. Completed native
items carry input correlation and a stable ordered prefix; a cursor includes its prefix digest.
A reconnect uses native history where available. Cache gaps and incomplete history remain explicit.
No durable event store, duplicated transcript, input receipt aggregate or shared Turn table exists.

### Deterministic operations

Compute create/delete and model-route create/revoke retain stable effect identities and observed
receipts. Queue success does not prove an external effect. Retry exhaustion belongs to the existing
lifecycle operation and must not be reset by an unrelated native input. Cleanup closes admission
and accounts for every retained owned generation, including failed replacements.

### Workspace files and inspection

Input attachments are transient request bytes materialized into disposable workspace files. Native
images use the Harness input format. Provider file/process access retains path, byte, integrity and
unknown-command constraints. Arbitrary shell effects are not made idempotent by a command ID.

### Recovery continuity

A checkpoint carries its exact native revision and resource/package compatibility. The adapter
writes a bounded Thread/settled-Turn manifest into the captured workspace and verifies it after
restore or package transition. This manifest contains no transcript and is not execution authority.
A backup predating accepted or uncertain dispatch cannot be used to replay external work. The
native-contract migration retires older local checkpoint references without deleting remote backups.

## Durable core and workflow facts

Core contains fixed platform operations only. Its retained state exists to establish resource or
recovery authority. Application evidence, review, publication and outcomes belong to clients.

## Client boundary

The API exposes native events, Turns and history through the Session, alongside lifecycle and
workspace access. Adapters validate supported native settings. Unsupported Harness combinations
must not appear to implement the contract through another Dorf scheduler.

## Application composition

Coding, review, publication, GitHub access, and business outcomes belong to external clients.
The worker composes direct execution with selected provider and Harness adapters. Core exposes
fixed lifecycle operations and does not offer an arbitrary workflow Action callback.

## Failure and code evolution

- **Process loss:** Absurd makes unfinished work eligible elsewhere; Dorf reconciles lifecycle Actions and unresolved native mutations against their authorities before continuing.
- **Sandbox loss:** report the loss honestly. Replace it only when authoritative retained state makes
  continuity truthful.
- **External ambiguity:** inspect the external authority; never infer success from timeout or retry
  blindly.
- **Poison work:** bounded attempts, time, cost, and attention stop infinite agent or provider spend.
- **Operator recovery:** the [Remote Control API](../control-api.md) owns current recovery operations,
  and [Support](../support.md) owns their diagnosis.
- **Code changes:** prefer short-lived Sessions, additive compatible task results where practical, and
  versioned lifecycle code. Let active Sessions drain on their pinned version rather than translating
  opaque execution history.

## Deployment shapes

Dorf separates its owner-controlled control plane from Profile-selected Sandbox providers. A
provider may be local, remote, or managed without becoming the authority for Sessions or workflow
policy. The [North Star](north-star.md) owns that product direction, and [Support](../support.md) owns
the combinations currently proved.

Deployment configuration owns host locations and credentials. Durable Sessions retain stable logical
connection and provider identities, not controller filesystem paths or copied secrets.

PostgreSQL owns immutable Sandbox profile revisions: provider, exact artifact, Harness, and provider
settings. A profile name holds a candidate revision, an optional active revision, and default
selection. `profile update` stages a candidate without changing the active revision or default.
Successful functional verification and confirmed proof-Sandbox cleanup atomically promote that
candidate. A failed candidate leaves the active revision eligible for new Sessions. An unsettled
verification Sandbox must still be cleaned up before staging another candidate.

Admission locks the profile name, reads its current successful verification, and pins the exact
revision in the same transaction as the Session. Promotion affects only future admissions. Execution,
recovery, inspection, capabilities, and cleanup resolve the Session's retained revision; they never
follow the name's current pointer. Admission replay retains the original binding. Revision
records remain available after Session cleanup, and a failure of an old artifact invalidates only its
own verification. Credentials remain host configuration.

Verification receipts belong to exact revisions. A current successful receipt gates default
selection and new admission; it is not a runtime lease for already-admitted Sessions. Explicitly
re-verifying the active revision fences new admission until its new probe and cleanup settle,
while Sessions continue against their pinned definition. To roll back future admissions, stage the
old exact settings and verify them again. This does not upgrade packages or restart processes in
existing VMs.

The profile-revision migration pins existing Sessions to the definition retained at migration time.
Definitions overwritten before this migration cannot be reconstructed from a profile name alone.

Provider adapters own their endpoint identity, topology, transport, and credentials. A Profile
retains only the exact provider and route configuration required to reproduce its verified behavior.

### Deployment proof

[Support](../support.md) owns current platforms, deployment shapes, and host requirements.
[Getting started](../getting-started.md) owns their procedures. A live proof must exercise the
authority changed by the slice. A provider-neutral claim needs proof on every Profile that the claim
names; unrelated slices need one relevant end-to-end terminal.

## Harness and Sandbox adapters

Sandbox and Harness implementations meet provider-neutral custody contracts. Every external
operation carries exact Dorf ownership. Core retains opaque provider locators for investigation;
their interpretation, lifecycle APIs, command transports,
topology, and connection capabilities remain adapter-private. Consumer code selects a verified
profile rather than branching on provider or Harness identity.

Within one bounded native operation under the existing Session fence, an adapter may resolve exact
Sandbox ownership and provider connection capabilities once for adjacent access. That access ends
with the callback, honors cancellation, and cannot be reused for another owner or durable attempt.
Lifecycle attestation remains fresh. This does not remove native-history recovery
or permit replay of an ambiguously accepted command. Live instruction files may be read together
while preserving each file's validation and missing-file semantics.

Execution uses the Sandbox and Harness contracts. Add an extension only when a concrete retained
requirement and an adapter proof justify it.

A profile is not usable until Dorf's functional probe and exact proof-resource cleanup complete. A
provider/profile is not supported until its required route and Harness capabilities are admitted
and proved end to end. Current support claims belong in operator documentation, not this boundary.

Go is the current core language. A language-specific executor is justified only when a concrete provider SDK or
workflow need makes that boundary materially smaller. It consumes a dedicated queue through a small
versioned contract and may not leak vendor types into core Session authority.

## Dependency budget

Use the Go standard library for ordinary HTTP/JSON, process control, hashing, configuration,
structured logging, concurrency, and tests. Prefer direct, programmatic boundaries to wrapper
frameworks whose model would become another authority to understand.

Absurd and the PostgreSQL driver surface are accepted core dependencies. One maintained WebSocket
implementation is acceptable for a Harness transport. Do not add an ORM, dependency-injection
container, web framework, message bus, migration framework, CLI framework, workflow DSL, or
observability distribution until a concrete terminal proves explicit code materially worse.

Optional execution diagnostics observe the existing Session/input-correlation/native-Turn binding at
the Harness adapter. They never decide execution, delivery, or cleanup. A transient execution
context carries those existing IDs to the adapter; a long-lived native process is not assigned one
client Run ID. Native notifications own tool and model diagnostics, and PostgreSQL retains product
and execution authority. [Support](../support.md#optional-codex-execution-logs) owns selection,
configuration, and recovery limits.

Every added module must name the real terminal it enables and remain removable behind a narrow
boundary. Transitive dependency count is a design signal, not a score to optimize at the expense of
correctness.

## PostgreSQL schema evolution

Published Dorf migrations are immutable and append-only. A retained deployment records each exact
migration filename in `dorf.schema_migrations`; `dorf migrate` takes one PostgreSQL advisory lock and
applies every missing known file in one transaction. The explicit runner orders repair 029 before
the obsolete intermediary constraint in 026 so completed application records can reach input-table
retirement; deployments already past that table retirement record the repair without changing data. A baseline is a historical starting point, not
a mutable description that can silently drift after a release. Unsupported migration identities
fail closed instead of guessing at schema state.

Each migration owns any bounded transformation required by current durable facts and has a
PostgreSQL integration proof from the previously published shape, including replay. Removing an
obsolete product fact may deliberately drop its retired table when the owning decision already
removed that contract; this is not a reason to retain dual reads or a compatibility facade. Dorf
keeps this explicit runner until concrete migration volume proves a framework smaller.

## Replacement and portability

Apply [Vertical slices, replaceable technology, and preserved
evidence](principles.md#vertical-slices-replaceable-technology-and-preserved-evidence) before
preserving an implementation choice.

- Do not create a generic durable-engine interface. Keep Absurd sequencing localized and domain
  facts, deterministic policy, Actions, observations, and reconciliation independent.
- Use Absurd's public APIs for production behavior. Raw tables may support version-pinned tests or
  operator diagnostics but are not workflow authority.

If Dorf outgrows Absurd, completed Sessions remain historical domain records, active short-lived Sessions can
drain, and new Sessions can begin on the replacement. Raw checkpoint history is not a portability
format.

### Persistent package recovery

A direct Session's existing durable task also reconciles admitted package upgrades. PostgreSQL retains
immutable package intent and observed effects; no second persisted phase counter or competing
upgrade task owns the Session. The delivery hold allows earlier native work to settle while new input
remains with the client. Every upgrade effect runs under the Session fence and current Absurd claim, with
heartbeats across provider calls.

Recovery reserves exact destination ownership before creating a replacement. Native verification
resumes retained Threads and checks settled Turn history without starting new agent work. The
verified binding, exact hold release, and execution wake commit together. Failed recovery retains
the maintenance hold and exposes attention. Cleanup handles every reserved resource and retained checkpoint,
including a lost checkpoint response and E2B's backing-snapshot dependency. D135 records the choice.

Shared guest images install Codex through a pinned Nix generation. Package staging uses the guest's
existing Internet access before delivery is held; it does not change the active executable. Nix
selects immutable executables while provider checkpoints recover matching mutable state. Image
replacement and live-process migration are not prerequisites for an ordinary package update.
D136 records the package boundary; the active implementation plan tracks deployment proof.

Both provider recipes also select one identical Nix workstation closure for developer and browser
tools. Codex's separate runner profile remains the unit of the existing live update operation.
Browser processes are agent-owned work inside the Sandbox, with no Dorf service lifecycle. D137
records this boundary; provider OS bootstrapping remains outside workstation package parity.
