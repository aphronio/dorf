# Internal Worker, Room, and Job Runtime

This document records the current **responsibility and dependency boundary**. It is deliberately not
an API inventory. Exact Python surfaces live in code, command syntax lives in CLI help, and protected
behavior lives in tests. Consequential rationale belongs in [decisions.md](decisions.md).

## Status and compatibility

`dorf.runtime` is an internal, experimental, portable building block for binding a durable
Worker to an isolated Room and running Jobs there. `dorf.sdk` is the concrete in-process
application facade over that building block. Portability means that the core does not depend on
coding-to-PR, GitHub, Incus, or Codex policy; neither package is yet a public compatibility promise.

Python types, Job-directory schemas, transaction indexes, adapter protocols, and rendered output may
change with dogfood evidence. The public alpha does not yet make these surfaces a third-party
compatibility promise.

## Responsibility boundary

| Package | Responsibility |
| --- | --- |
| `dorf.runtime` | Independent Worker and Room identity, goal-backed Job and Assignment lifecycle, per-conversation input admission, and environment/agent contracts. |
| `dorf.adapters.agents` | Harness-specific preparation, native conversation continuity, turns, and diagnostics. |
| `dorf.adapters.environments` | Room identity, provisioning, execution, stop, and destruction. |
| `dorf.workflows` | Coding-to-PR semantics and policy, including Git, checks, review, repair, and publication. |
| `dorf.provider_gateway` | Shared provider connections, consumer-scoped inference routes, and concrete broker lifecycle outside the portable runtime. |
| `dorf.sdk` | Concrete local composition of the store, built-in adapters, resource operations, waits, and replaceable controller launch. |
| `dorf.cli` | Command parsing, interactive terminal behavior, exit codes, and human/JSON rendering over the SDK and coding workflow. |

The runtime imports only its own modules and the Python standard library. Adapters depend on runtime
contracts, never the reverse. Caller metadata is opaque to the runtime: it does not interpret tasks,
repositories, branches, GitHub state, verification policy, or acceptance.

## Durable authority

The Job directory is the document plane for material humans or agents should consume with ordinary
tools: pinned goals, curated provenance-labelled timeline events, and content-addressed evidence.
Runtime SQLite is authoritative for transactional Worker, Room, Job, Assignment, human presence,
conversation, input, and native-delivery state. Workflow-owned Job-keyed tables separately hold
coding repository, branch, PR, check, review, AFK, and command facts. Do not mirror the same mutable
state into either
the document plane or a replacement aggregate. Git and GitHub remain authoritative for coding
deliverables.

Queued client messages are Dorf-owned control inputs until delivery. The harness remains
authoritative for its transcript, tool items, and native turn history. Dorf records the binding
and facts it owns rather than normalizing a duplicate transcript.

A logical Worker and its retained history outlive loss of the current Room and harness control
processes, but executable continuity currently requires that exact Room body and disk to survive. A
Job exists only with complete goal version 1 and is connected to one Worker by an explicit
Assignment. Both the
Worker-general and Job-native conversation bindings outlive any initiating client, dispatcher,
harness control process, manually started terminal, or SSH connection. Those processes are
replaceable operational
details.

## Runtime invariants

- One native turn may mutate each conversation at a time; later accepted inputs retain FIFO order
  within that conversation. A Worker's general and assigned-Job conversations are independent and
  may execute concurrently.
- Explicit Worker recovery restores only the exact recorded Room when its provider body and disk
  survive. Unsettled turns are reconciled against harness-native IDs and baseline history before any
  input is eligible for redispatch. An absent body is recorded lost, leaves the Worker offline and
  Roomless, and never creates a replacement Room, Assignment generation, coding clone, or native
  thread. Durable identity and queued input remain inspectable; executable continuity requires a
  fresh Worker and Job.
- One synchronous human attachment may enter a Worker's exact current Room at `/workspace` without
  pausing either conversation or changing Worker, Room, Job, Assignment, workspace, or native
  identity. Presence exists only while that interactive shell is open; shell exit is implicit
  detachment. Process ownership is held by an advisory lock, so inspection ignores a stale row and a
  later attachment reclaims it after a client crash. A separate forced-detach operation and
  multi-human presence are not current behavior.
- Input identity is independent of text, so retries and intentionally repeated text are distinct
  concepts. The Assignment's first Job input is exact goal version 1; ordinary messages never revise
  that goal.
- Runtime observations are facts; validated Worker self-reports remain claims and are never inferred
  from chat.
- Approved context crosses into a Room as a detached, read-only goal-version snapshot under
  `/run/dorf/jobs/NAME/context/VERSION/`, with no write-back path. Job-native processes receive
  explicit Job, Assignment, workspace, and scoped outbox configuration; Worker-general processes do
  not receive those values or reporting guidance.
- Worker reports leave through `/run/dorf/jobs/NAME/outbox/` and become Job documents only after
  the controller validates both the named Job and its current Assignment. Report publication without
  explicit valid Job, Assignment, and outbox scope fails rather than inferring identity from a Worker.
- Timeline events and artifact blobs are immutable Job documents. Runtime and workflow observations
  remain facts; accepted Worker summaries and artifacts remain claims with exact Assignment
  provenance. Artifact digests identify the accepted bytes, not the truth of a Worker's conclusion.
- The SDK projects retained artifacts as a path-free Job manifest with stable artifact references.
  Its model-readable operation accepts only exact Job plus artifact-reference selection, verifies
  recorded size and SHA-256 custody, and returns at most 64 KiB of valid UTF-8 `text/*` or JSON.
  Missing, cross-Job, unsupported-media, oversized, corrupt, invalid-encoding, and invalid-JSON
  outcomes remain typed and never fall back to Room or host filesystem access.
- The same manifest and stable reference drive standalone human export. The SDK and CLI stream the
  original bytes into an existing caller-selected directory under the recorded safe filename,
  verify size and SHA-256 before atomic publication, and support binary or larger artifacts without
  using the model-read limit. Existing files require explicit overwrite; failed verification never
  publishes a partial destination.
- Assignment-scoped report collectors are replaceable, non-blockingly singleton per Job plus
  Assignment, and recheck current identity before acceptance. Stale collectors leave another
  Assignment's outbox untouched.
- Read operations do not ingest reports, recover collectors, or create Worker turns or workflow runs.
- Job ending closes admission, uses one stable cooperative cleanup input when settled, removes exact
  Assignment Room state/workspace, ends the Assignment, and retains Job documents and native binding.
  Explicit interrupt cancels unsettled work and bypasses cooperation. If that Room is proven absent,
  interrupted ending treats its local workspace and processes as gone without issuing provider
  execution. Worker ending requires no open Job and reports success only after exact Room cleanup—or
  provider-confirmed absence—is recorded; failures remain retryable.
- The external Job document directory is never writable-mounted into its Room.

See D005–D007 and D019–D030 in [decisions.md](decisions.md) for the rationale and reconsideration
triggers behind these invariants.

## Composition and non-goals

The in-process `Dorf` facade selects the application store plus the concrete Incus Environment
and Codex Agent adapters. The Room integration composes the sibling Provider Gateway to
obtain and revoke Room-scoped inference routes without teaching `dorf.runtime` about provider
connections or broker implementations. The CLI's resource commands and embedded SDK clients use
the Dorf facade; trusted host clients may import the Provider Gateway facade directly for their own
model routes. No caller should reconstruct resource-runtime wiring or read SQLite directly.
Coding workflow behavior is a separate application layer over the same store and runtime because it
must order repository setup around Worker and Job admission. Coding admission creates a
provenance-labelled dedicated Worker, a goal-backed Job and Assignment, and an independent clone at
`/workspace/jobs/JOB`. The Assignment remains non-admitting and non-deliverable while the workflow
clones and runs repository preparation, then opens immediately before initial goal dispatch.
Issue-backed coding delegation first completes one workflow-owned admission proof in an unrecorded
disposable VM. The proof pins the exact issue, repository head, GitHub App authority, official image,
provider route, repository preparation/smoke path, dry-run Git write, and real implementation and
reviewer turns; it revokes the route and destroys the VM before any AFK coordinator, coding Job,
branch, Worker, or durable Room is admitted. A successful proof is consumed by that same invocation
and recorded as a workflow fact; repeated delegation reuses the admitted identity and proof.
Repository commands are workflow facts; model implementation, repair, and follow-up instructions
are Job FIFO inputs.
Worker-addressed attachment is an Environment operation; the Incus adapter opens a direct
interactive shell while raw provider access remains break-glass. Concrete constructors, retries,
locks, polling, process launching, file layouts, and output shapes remain in code and tests.

A remote Room does not imply a remotely hosted Dorf. A future cloud Environment adapter may
submit to and observe a remote VM entirely behind the same SDK facade. A network transport may wrap
the facade later if an observed requirement needs an always-on or shared Dorf authority; that
transport must not define different Worker or Job semantics.

The runtime and coding workflow share the application SQLite file but own disjoint tables. The
superseded Session tables and dispatchers are not migrated or retained behind adapters.

Do not add provider registries, plugin loading, capability matrices, transport selection, or remote
coordination to `dorf.runtime`. The sibling Provider Gateway currently has one concrete broker
backend and one validated ChatGPT-to-Codex route; add the smallest selection seam only when another
provider or backend is validated. Tmux and SSH remain break-glass observation and takeover tools;
they do not identify a Job or define reconnect.
