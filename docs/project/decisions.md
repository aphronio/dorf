# Dorf Decision Log

This log records consequential product, architecture, and technology choices whose rationale would
not be recoverable from the code. It is not a specification or inventory of current behavior.

Add an entry in the same change that makes a new consequential choice. When evidence changes an
accepted choice, preserve its history, mark it superseded, link the replacement, and remove code and
tests made obsolete by the new direction. Track open questions and implementation work in GitHub
issues rather than here.

## D001 — Durable logical agent session

- **Status:** Superseded by D025 — 2026-07-27
- **Decision:** The durable product identity is a logical agent session bound to an isolated
  environment and an agent-native conversation identity. OS processes and terminal panes are
  replaceable operational details.
- **Why:** Process identity cannot survive crashes, hibernation, or different environment providers,
  while the user needs stable conversational and workspace continuity.
- **Reconsider when:** A supported agent cannot resume native context and a resident process must
  become an explicit part of that driver's behavior.

## D002 — Workflow, runtime, agent driver, and environment seams

- **Status:** Accepted responsibility boundary — 2026-07-22; package-import direction clarified by
  D019 — 2026-07-23
- **Decision:** Responsibility flows from the coding workflow through the durable Worker, Room, Job,
  and Assignment lifecycle to an agent driver operating through an Environment adapter. This is not
  a package-import graph; D019
  records that boundary.
- **Why:** The current runner mixes GitHub, repository, Incus, tmux, and Codex concerns. Separating
  them lets the coding workflow dogfood reusable primitives without leaking PR policy into agent or
  environment implementations.
- **Reconsider when:** A working vertical slice demonstrates a smaller interface with fewer concepts
  and no workflow leakage.

## D003 — Codex app-server first; ACP later

- **Status:** Accepted — 2026-07-22
- **Decision:** Codex is the first interactive agent. Use a thin driver over authenticated Codex
  app-server WebSocket; do not add ACP before a second interactive harness driver is required.
- **Why:** App-server exposes Codex-native threads, turns, approvals, history, interruption, and live
  updates directly. A thin driver keeps its experimental protocol replaceable without prematurely
  creating an agent-plugin framework.
- **Reconsider when:** App-server cannot satisfy reconnect or security needs, or a concrete second
  interactive agent such as Claude Code or Kimi CLI enters the supported coding workflow. One-shot
  reviewer commands do not meet this trigger.

## D004 — Tmux and SSH remain break-glass tools

- **Status:** Accepted — 2026-07-22; built-in tmux runner removed at resource cutover — 2026-07-27
- **Decision:** Keep SSH/direct Room access and manually started tmux available for operational
  takeover alongside the semantic harness driver. Do not make a resident tmux process part of
  durable identity or retain an unused built-in tmux runner.
- **Why:** Agent-native history does not replace the ability to inspect the VM, recover a stuck
  process, open a shell, or repair work when Dorf's control path fails.
- **Reconsider when:** Another proven operational mechanism offers equally simple and reliable local
  observation and takeover.

## D005 — The agent owns conversation history

- **Status:** Accepted — 2026-07-22; controller-owned input boundary clarified by D022 — 2026-07-26
- **Decision:** Codex remains authoritative for its transcript, turns, tool items, and context
  management. Dorf stores native IDs plus the lifecycle, run, workflow, and cleanup facts it
  owns; it does not duplicate the full transcript in SQLite or documents. Pinned goals and
  queued human/client messages are Dorf-owned control inputs required for durable delivery, not
  copies of agent-owned history.
- **Why:** Duplicating history creates synchronization and compatibility problems without serving the
  current coding workflow.
- **Reconsider when:** A real client needs lossless replay that agent-native history cannot supply, or
  history must remain available after environment destruction.

## D006 — Turns are serialized

- **Status:** Superseded by D022 — 2026-07-26
- **Decision:** One turn may actively mutate an agent session at a time. Later messages are delivered
  sequentially. The original decision deferred a durable FIFO until a concrete workflow needed to
  accept a message during active work; #125 supplied that requirement.
- **Why:** Concurrent turns against one context have ambiguous ordering and workspace effects.
- **Reconsider when:** A supported agent has defined concurrent-turn semantics and a real workflow
  benefits from them.

## D007 — Cleanup has an explicit, retryable outcome

- **Status:** Accepted — 2026-07-22
- **Decision:** Workflow completion and environment cleanup are separate facts. Cleanup is
  idempotent, retryable, and visibly failed until resources are released.
- **Why:** A completed or discarded proposal does not prove that a local VM or future billable
  environment was deleted.
- **Reconsider when:** The representation may change, but cleanup failure must remain observable.

## D008 — Local authenticated Incus image for ChatGPT subscription

- **Status:** Superseded for Codex by D035 — 2026-07-29 (implemented and validated under the
  completed #159 umbrella); the secret-bearing image remains the private default for Droid and any
  other non-Codex agent state (accepted for the local single-user phase — 2026-07-22; made the
  private default — 2026-07-26)
- **Decision:** Use the local secret-bearing `dorf-codex-droid-authenticated-local` Incus image
  containing `/root/.codex` ChatGPT device-login state for Codex CLI and app-server. While the code,
  image, and deployment remain private and single-user, this image is the default Room template;
  configured repositories may override it. Once the D035 umbrella ships, Codex provisioning follows
  D035 instead: credential-free images plus broker-issued scoped keys.
- **Why:** This supports the owner's ChatGPT subscription inside the current local trust boundary
  without requiring usage-based API credentials. A vanilla Ubuntu default cannot satisfy Worker
  readiness and makes the zero-configuration `spawn` path predictably fail.
- **Reconsider when:** Dorf becomes remote or multi-user, images must be distributed, Workers
  need distinct credentials, credentials require scoped injection, or cloned-image token refresh is
  unreliable.

## D009 — Coding workflow is the only requirements driver

- **Status:** Accepted — 2026-07-22; second-workflow gate refined by D062 — 2026-08-11
- **Decision:** Extract and dogfood the runtime through coding-to-PR before selecting another workflow,
  environment provider, or interactive agent.
- **Why:** A dependable building block emerges from real pressure, not speculative generality.
- **Reconsider when:** The coding workflow is fully routed through the runtime and dogfood evidence
  justifies one concrete next consumer.

## D010 — Vertical slices, KISS, and deletion are preferred

- **Status:** Accepted — 2026-07-22
- **Decision:** Migrate through thin end-to-end slices. Current code and tests may be refactored or
  removed when a simpler implementation replaces them.
- **Why:** Preserving accidental structure creates adapters around adapters. Tests protect required
  product behavior, not superseded implementation shape.
- **Reconsider when:** An explicit compatibility promise covers the affected interface.

## Additional durable choices

These choices predate the 2026-07-22 consolidation. Their original implementation details remain in
Git history; only the rationale needed to avoid accidental reversal is retained here.

| ID | Decision and why | Reconsider when |
| --- | --- | --- |
| D011 | **One coding task slice per Job, Assignment, isolated clone, branch, and PR proposal.** Revision retains those identities; merge, rejection, or abandonment is workflow-terminal. | A concrete workflow needs different cardinality with equally clear acceptance semantics. |
| D012 | **Incus VM is the only current environment adapter.** It provides local isolation without exposing the host Docker daemon or home directory. | The Incus lifecycle is dogfooded and a meaningfully different environment is deliberately selected. |
| D013 | **GitHub PR is the acceptance primitive.** Git and GitHub already provide durable diffs, review, merge, and rejection. | A non-GitHub coding workflow becomes a real requirement. |
| D014 | **SQLite state lives outside managed repos.** Local runtime and coding workflow indexing remains durable without modifying target repositories. | Multi-host coordination requires another store. |
| D015 | **The Dorf control plane owns coding-branch authentication through the GitHub App.** It delivers short-lived installation tokens through the Environment seam without borrowing ambient controller-machine Git credentials, credential stores, or checkout state. | Another source-control host is supported or a narrower equally usable credential flow is proven. |
| D016 | **The Room-native `/workspace/jobs/JOB` clone is authoritative for a coding Job.** The host checkout is orchestration context only; each Job is an independent clone, not a worktree or the Worker-general `/workspace` directory. | An environment without a durable native filesystem proves another workspace model necessary. |
| D017 | **Managed repositories expose explicit development-tooling contracts.** Repo-owned commands and allowlisted environment bindings keep app semantics in the repo without Dorf coupling in product code. | A compatible repo-owned standard or a repeated cross-repo primitive proves a smaller contract. |

## D018 — Stabilize only the dogfooded internal runtime boundary

- **Status:** Superseded by D025 — 2026-07-27
- **Decision:** The then-supported repository-internal runtime surface was the logical-session lifecycle
  exercised by the coding workflow: create or retry environment provisioning, start and observe the
  initial native turn, reconnect and inspect, continue or recover one serialized turn, and end with
  observable retryable cleanup. Incus and Codex remain direct built-in adapters. Provider selection,
  generalized runner and agent registries, capability matrices, worker artifact paths, and
  app-server-specific public errors are outside that surface. GitHub, repository, check, review,
  repair, publication, and acceptance policy remain in the coding workflow.
- **Why:** The [#94 ledger](https://github.com/aphronio/dorf/issues/94) directly observed
  later-client reconnect without new runs, three
  sequential turns on one Codex thread, and successful retry after partial cleanup. Every slice used
  Incus and Codex; no second workflow, environment, or interactive agent supplied evidence for a
  generalized selection layer. The registry had no second implementation, and current setup
  recovery no longer needed the old run-kind reclassification shim. Process-liveness and
  app-server error names also leaked replaceable implementation details.
- **Compatibility:** This surface is internal and experimental. Python types, SQLite representation
  and migration policy, CLI rendering, and opaque Codex-native inspection payloads may change with
  further dogfood.
  Public packaging, licensing, releases, and external compatibility commitments are deferred.
- **Reconsider when:** A second real workflow needs the same lifecycle with different caller facts;
  a deliberately selected second environment or interactive agent proves a shared selection seam;
  agent-native history cannot satisfy a real inspection need; or the owner chooses to prepare a
  licensed public release with an explicit compatibility policy.

## D019 — Portable runtime core with built-in adapters

- **Status:** Superseded by D047 — 2026-08-06
- **Decision:** Keep the durable lifecycle and generic persistence in the self-contained
  `dorf.runtime` package. Put concrete implementations in
  `dorf.adapters.agents` and `dorf.adapters.environments`, in the same distribution for now.
  Keep coding-to-PR state and behavior in `dorf.workflows`, outside the runtime and adapters.
  Caller metadata is opaque to the runtime. Do not add registries, plugin loading, provider
  configuration, networking policy, or a separate distribution yet.
- **Why:** Durable Worker, Room, Job, Assignment, and conversation bindings are useful beyond
  the current application and should be inexpensive to extract into another monorepo package or
  repository. In-package adapters make the implementation seams and future extension points clear
  without preserving the deleted single-implementation registries or creating a speculative plugin
  framework. Keeping Git and GitHub behavior in the coding workflow lets a future environment
  adapter reuse the lifecycle without inheriting coding policy.
- **Compatibility:** The prior experimental SQLite schema and top-level implementation modules are
  replaced rather than migrated. No external compatibility promise covered them.
- **Reconsider when:** The runtime is deliberately published or extracted; a second real adapter
  proves a common selection/configuration seam; or packaging adapters separately solves a concrete
  dependency or release problem.

## D020 — Approved Worker, Room, and Job control-plane direction

- **Status:** Superseded by D025 — 2026-07-27; storage authority refined by D023 — 2026-07-26
- **Decision:** Adopt [north-star.md](north-star.md) as approved product direction for the L1 control
  plane, including Worker, Room, and Job as product vocabulary and the shared control verbs. `spawn`
  may provision and bind a Worker to a Room before `assign` pins and delivers the Job goal; the
  implementation may represent that interval as an unassigned Job rather than introducing an
  independent worker registry. Aim for a tangible Job document directory alongside transactional
  operational state as refined by D023. Existing session/environment/turn names, SQLite schemas,
  and internal APIs are implementation evidence,
  not compatibility constraints, and may be replaced rather than migrated. Concrete API and schema
  details remain decisions to make in working vertical slices.
- **Why:** The finalized north star is the accepted destination, not a speculative alternative to
  the current implementation. Preserving experimental representations would invert that priority.
  Keeping concrete details refactorable still allows implementation evidence to expose genuine
  conflicts without silently weakening the product direction.
- **Local and remote durability:** A remote or cloud Room should continue when local clients are
  offline. A local Room requires its host to be powered on, but client/process failure or host
  restart must not erase the durable Job, Room, Worker binding, or recovery path.
- **Compatibility:** There is no current external compatibility promise. Rewrite or delete
  superseded runtime code and tests while retaining coverage for behavior the north star still
  requires.
- **Reconsider when:** A working slice exposes a concrete conflict in the accepted vocabulary,
  object lifecycle, directory representation, or control verbs. Discuss and record the deliberate
  revision; do not diverge merely to preserve current implementation shape.

## D021 — Situation-first inspection with explicit provenance

- **Status:** Accepted — 2026-07-26; product-history shape refined by D061 — 2026-08-10
- **Decision:** `inspect` defaults to a read-only Job pulse built from Dorf-owned lifecycle and
  run facts plus a fresh Room availability observation. Worker claims are a separate provenance
  channel and remain explicitly absent until a structured self-report boundary exists; Dorf
  does not infer them from native conversation history. Opaque native history and adapter
  diagnostics are available only through the explicit raw lens. An unavailable Room is a pulse
  fact, not a reason to hide the durable Job; the raw lens may fail when it cannot reconnect.
- **Why:** A returning manager needs an honest, glanceable situation before protocol history. Keeping
  claims separate prevents fluent Worker output from becoming control-plane fact, while preserving
  raw diagnostics supports break-glass investigation without making it the front door. A pulse that
  survives Room unavailability makes reconnect useful precisely when operational state is degraded.
- **Current shape:** Assignment-fenced structured Worker claims and evidence now enrich the pulse.
  Coding Jobs compose workflow-owned outcome and attention facts into the same default pulse, with
  terminal workflow outcome or runtime lifecycle taking precedence over stale AFK progress while
  retaining its own source and provenance. Timeline and evidence are explicit read-only lenses;
  native transcript history remains separate.
- **Reconsider when:** A reviewed self-report/evidence ingestion boundary lands, a real environment
  cannot support a cheap side-effect-free availability observation, or a client needs another lens
  backed by concrete workflow evidence.

## D022 — Controller-owned message FIFO with internal action identity

- **Status:** Accepted — 2026-07-26; storage revised by D023 and conversation scope by D025
- **Decision:** Preserve one active native turn per conversation and admit subsequent public messages
  into that conversation's transactional SQLite FIFO. Every admitted user action receives a random
  internal message ID;
  internal callers retain that ID across transport retries. Message text is never identity, so two
  identical messages are two valid queue entries. The internal message ID becomes the existing
  native turn key. A replaceable
  local dispatcher drains the FIFO in order and stops at a failed delivery. `wait` only observes
  durable outcomes and current adapter attention; it does not dispatch, send a status turn, or
  create polling runs.
- **Why:** FIFO admission makes ordering durable before transport and lets a dispatcher failure leave
  recoverable work instead of losing input. Content deduplication would incorrectly collapse valid
  repeated instructions such as two consecutive “continue” messages. Stable per-action identity is
  the only honest way to distinguish retries from repeated content without asking humans to manage
  idempotency keys.
- **Local durability:** Once the SQLite admission transaction commits, the message is accepted even
  if detached dispatcher launch fails. A later recovery path may restart delivery. Local host
  shutdown still pauses local dispatch under D020; it does not erase the Job or queue.
- **Compatibility:** Message-table schema, receipt JSON, wait outcome shape, polling interval, and
  dispatcher process are experimental and replaceable. This decision does not select the
  Worker-to-control-plane context, evidence, timeline, artifact, or ingestion protocol reserved for
  #126.
- **Reconsider when:** A remote control plane requires a multi-host queue/lease, a concrete client
  cannot retain action identity across its own transport retries, or native harness semantics permit
  useful concurrent turns with defined ordering.

## D023 — Job document plane; SQLite operational authority

- **Status:** Accepted — 2026-07-26
- **Decision:** Use the external Job directory for durable material that humans or agents should
  consume through ordinary file tools: the pinned goal and, after reviewed boundaries land,
  approved context, claims, and evidence. Use SQLite as the authority for transactional operational
  state including Worker, Room, Job, Assignment, conversation, FIFO admission, and delivery
  indexing. Do not maintain mutable copies of the same state in both surfaces. Native conversation
  history remains harness-owned and coding deliverables remain Git/GitHub-owned.
- **Why:** Documents benefit from `grep`, editors, shell tools, diffs, and bounded read-only exposure
  to a Room. Concurrent message admission and lifecycle transitions benefit from database
  transactions and programmatic queries; representing them as JSON merely because SQLite is also a
  file adds locking, crash-recovery, and synchronization code without helping an agent consume the
  data.
- **Room boundary:** The external Job document directory is never writable-mounted into the Room.
  Approved documents may be projected inward read-only. Worker-authored changes leave the
  Room only through the reviewed validation and ingestion boundary reserved for #126.
- **Compatibility:** Existing duplication between `job.json` and SQLite is refactorable evidence,
  not a schema promise. Remove duplication incrementally when a working slice owns the affected
  lifecycle behavior; do not expand this message slice into a broad Job-state rewrite.
- **Reconsider when:** Agents concretely need direct file-tool access to operational state, a
  multi-host control plane replaces SQLite, or a smaller single authority can preserve both
  transactional correctness and the document-tool experience.

## D024 — Controller-mediated Job context and validated Worker reports

- **Status:** Superseded by the Go core in D047, Message/AgentRun boundary in D052, and derived product
  history in D061 — 2026-08-10
- **Decision:** Project approved Job context into the Room as a detached copy with no write-back path;
  never mount the external Job directory writable. Give the Worker a Room-local `dorf-report`
  command, described through harness-level runtime capability guidance that contains no task or
  alternate goal. The command publishes milestone, assumption, completion, and artifact claims to a
  Maildir-style `tmp` then `new` spool. A replaceable controller collector pulls each declared file
  individually into quarantine, validates it, streams it under fixed quotas, computes its digest,
  stores immutable per-Job content-addressed bytes, appends one immutable provenance-labelled timeline
  event as the acceptance commit, and writes an acknowledgement. Reports require the exact current
  Job, Assignment, and absolute Room outbox scope. `inspect` reads accepted documents only; it never
  ingests or restarts the collector.
- **Delivery and recovery:** Report IDs are independent of content. Ingestion is at least once and
  idempotent by Job plus report ID; a retry with different accepted content is rejected. A collector
  crash before the timeline event leaves only disposable quarantine or an unreferenced blob. A crash
  after the event but before acknowledgement reuses that event and completes the acknowledgement.
  Event files are sequenced under a local Job lock and atomically renamed into the timeline. Automatic
  process recovery composes with #128; the durable Room spool does not depend on one collector process.
- **Provenance:** Worker summaries and artifacts remain claims after safe ingestion. Runtime-observed
  lifecycle/turn outcomes and workflow-observed command exits are facts. Artifact digests bind the
  exact accepted bytes but do not validate their interpretation. Runtime event kinds and artifact
  metadata remain generic; coding-specific check, benchmark, diff, branch, and PR meaning stays in the
  coding workflow.
- **Security and limits:** Accept only fixed-schema manifests, bounded one-line summaries, safe display
  names, sequential fixed spool names, and regular files. Reject traversal, links, special files,
  archives-as-transport, excessive file count, files over 100 MiB, and Jobs over 500 MiB. Transfers do
  not follow links and stream to controller-owned quarantine rather than buffering artifact contents.
  The Worker never chooses a controller destination path.
- **Research basis:** Controller file APIs in [Incus](https://linuxcontainers.org/incus/docs/main/howto/instances_access_files/)
  and [E2B](https://e2b.dev/docs/quickstart/upload-download-files) support provider-mediated copy
  rather than shared authority. [Maildir](https://cr.yp.to/proto/maildir.html) demonstrates complete
  publication through temporary files and atomic rename. The
  [transactional outbox](https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/transactional-outbox.html)
  and [Temporal idempotency guidance](https://temporal.io/blog/idempotency-and-durable-execution)
  support stable identities and idempotent at-least-once consumers. Bazel's
  [CAS model](https://bazel.build/remote/caching) separates digest-addressed bytes from metadata;
  [in-toto](https://github.com/in-toto/attestation/blob/main/spec/README.md) supports binding subjects
  to provenance; and [OpenHands events](https://docs.openhands.dev/sdk/arch/events) reinforce explicit
  immutable source attribution. OWASP upload guidance motivates strict path, link, type, and quota
  validation.
- **Rejected alternatives:** Reject writable authoritative mounts and writable host staging mounts
  because they weaken the boundary and couple the design to local Incus. Defer Codex dynamic tools or
  MCP as the primary transport because they add harness-specific protocol work and do not carry large
  artifacts. Do not adopt Temporal/Postgres, a global object store, full in-toto signing, transcript
  scraping, or archive extraction for this local-first slice; each adds machinery or false authority
  without improving the current coding workflow.
- **Compatibility:** Timeline/report schemas, quotas, collector process shape, Room paths, capability
  wording, and rendering are internal and replaceable. The enduring boundary is detached approved
  context inward, validated claims outward, immutable provenance, and no writable authoritative mount.
- **Reconsider when:** A remote Room needs push delivery instead of polling, a second harness proves a
  smaller report-tool seam, artifact volume justifies a global store and garbage collector, signed
  attestations become a real workflow requirement, or fixed quotas block observed coding evidence.

## D025 — Independent Worker identity, replaceable Room, and explicit Assignment

- **Status:** Superseded by D047 — 2026-08-06
- **Decision:** Worker, Room, Job, and Assignment are separate durable concepts. A Worker is an
  independently named identity that may be idle, assigned, or offline and survives loss or
  replacement of its current Room. A Room is a separately recorded isolated execution body operated
  through the Environment adapter. A Job comes into existence only when complete goal version 1 is
  atomically pinned. Assignment is the temporal relation connecting an open Job to one Worker; the
  first implementation permits one open Job per Worker and no cross-Worker reassignment. D020's
  permitted unassigned placeholder Job is rejected rather than retained as the Worker registry.
- **Conversation topology:** A Worker owns one lazy harness-native general conversation, while every
  Job owns a separate harness-native conversation. Inputs are admitted durably and ordered FIFO per
  conversation, not across the Worker. Independent general and Job turns may overlap when the
  harness supports independent threads; Dorf adds no Worker-wide scheduler or mutex. Native
  transcript history remains harness-owned. The Worker-general conversation initially has no
  timeline/evidence document plane and receives no Job goal or reporting identity.
- **Configuration:** Harness type belongs to Worker identity. Model and reasoning defaults belong to
  a conversation and supported turns may override them. `worker spawn` therefore provisions and
  validates the harness without pinning model/reasoning choices to the Worker.
- **Lifecycle:** `worker spawn` creates the Worker and its initial ready Room without a Job or Job
  documents. Open offline Workers admit direct messages for later delivery. `job assign` requires an
  existing ready Worker with no open Job and never spawns implicitly. Sequential Jobs reuse the
  Worker's Room but use independent `/workspace/jobs/NAME` workspaces. Explicit ending, same-Worker
  recovery, and attachment are owned by #129, #128, and #127 respectively.
- **CLI direction:** L1 uses typed resource groups (`worker ...` and `job ...`); `message` replaces
  `send`. Worker and Job names occupy separate namespaces. Rooms are initially addressed through
  Worker identity rather than a public Room command group. The completed cutover removes superseded
  top-level commands without compatibility aliases.
- **Why:** #122–#126 proved detached provisioning, messaging, reconnect, and evidence but exposed
  that an unassigned Job cannot honestly represent a reusable Worker, a replaceable Room, two
  independent conversations, or sequential Jobs. Making the identities explicit removes ownership
  ambiguity before attachment, recovery, and cleanup harden it into more state transitions.
- **Compatibility:** Runtime types, SQLite tables, Job documents, command grammar, and the old
  one-to-one Session representation were internal and experimental. The development database and
  generated runtime state are deliberately reset at final cutover rather than automatically
  migrated; no compatibility adapter or destructive migration ships.
- **Reconsider when:** A concrete workflow requires multiple simultaneous Jobs per Worker,
  cross-Worker handoff, public Room management, curated Worker memory, or a harness that cannot keep
  independent general and Job threads. Each requires a reviewed lifecycle/fencing decision rather
  than weakening identity boundaries implicitly.

## D026 — Coding workflow composes runtime resources without a replacement aggregate

- **Status:** Superseded by D047 — 2026-08-06
- **Decision:** A coding slice creates deterministic Worker `coder-JOB` with explicit
  `coding-workflow` provenance and `dedicated` lifecycle policy, then creates one exact-goal Job and
  Assignment. Workflow-owned SQLite tables are keyed by Job and contain only repository, branch, PR,
  command, review, AFK, and terminal workflow facts. They do not duplicate current Worker, Room,
  Assignment, conversation, input, turn, or runtime lifecycle state into a renamed coding Session.
- **Workspace and turns:** Coding repositories are independent clones at `/workspace/jobs/JOB`, never
  worktrees or the Worker-general root. Repo setup and checks are workflow-observed commands;
  implementation, repair, and follow-up are messages on the Job FIFO and native conversation. Review
  agents remain workflow-owned one-shot commands rather than durable Workers.
- **Terminal policy:** PR creation, passing checks, successful turns, and Worker completion claims do
  not close runtime resources. Merge, explicit rejection, and abandonment are terminal coding
  workflow conditions; resource ending and Room cleanup remain the explicit lifecycle operation
  owned by #129. Revision preserves the Job, Assignment, Worker, Room, clone, branch, and PR.
- **Credentials:** Repository clone and Job-workspace fetch/push use short-lived GitHub App
  installation tokens passed through the current Assignment/Room execution boundary. Before a Job or
  Room exists, the controller may use a separately minted installation token to read/create the
  coding branch ref and fetch only the recorded remote base object into the orchestration checkout so
  it can pin the exact base; recovery uses the same scoped ref API. GitHub PR API calls likewise use
  controller-side installation tokens for the recorded repository. No path borrows ambient
  controller Git credentials or credential stores.
- **Compatibility:** The superseded Session runtime, tables, dispatchers, top-level L1 commands, and
  coding-session compatibility paths are deleted, not wrapped. Experimental development state is
  reset once out of band; no destructive migration is shipped.
- **Why:** Keeping a second aggregate would restore the ownership ambiguity D025 removed and permit
  runtime and workflow state to disagree. Explicit provenance prevents lifecycle policy from being
  inferred from the `coder-` name, while Job-keyed workflow facts preserve coding-specific recovery
  without leaking repository policy into the runtime.
- **Reconsider when:** A second workflow demonstrates a concrete shared orchestration record that
  cannot be represented by resource identity plus workflow-owned facts, or coding tasks need a
  deliberately reviewed Worker-sharing policy.

## D027 — Worker-addressed attachment is synchronous presence with implicit detach

- **Status:** Accepted during stranger-loop attachment implementation — 2026-07-27
- **Decision:** `worker attach NAME` resolves the Worker's exact current Room at invocation, records
  one transactional human-presence claim fenced to that Room, and opens a direct interactive shell at
  `/workspace`. It does not select the assigned Job workspace, pause native turns, or change Worker,
  Room, Job, Assignment, conversation, workspace, or branch identity. Exiting the shell with
  `Ctrl-D`, `exit`, or ordinary client interruption clears presence. A process-held advisory lock
  distinguishes a live owner from a stale row after an ungraceful crash; inspection reports the
  latter detached and the next attachment reclaims it. A second concurrent attachment fails
  honestly. Do not add a separate `worker detach` command until a concrete remote
  forced-detachment need exists; direct provider access remains the break-glass fallback.
- **Why:** The current local single-user workflow needs a simple door into the Room, not a resident
  terminal service or remote session-management protocol. Starting consistently at `/workspace`
  keeps attachment independent of Assignment state, while shell lifetime already supplies an honest
  presence boundary. A separate detach verb would require durable remote process handles and orphan
  reconciliation without serving an observed workflow.
- **Reconsider when:** A remote client must evict an attachment it does not own, multi-human presence
  becomes real, a non-Incus Environment cannot expose a synchronous terminal, or shell interruption
  cannot provide reliable presence cleanup.

## D028 — Recovery preserves identity and reconciles native delivery before redispatch

- **Status:** Accepted during stranger-loop recovery implementation — 2026-07-27; absent-Room
  replacement superseded by D030
- **Decision:** `worker recover NAME` is the explicit recovery boundary. It restores the exact current
  Room and provider identity when the Incus VM still exists, including after a host restart. If the
  provider body is absent, it retains Worker and Job identity, records the old Room as absent, creates
  one replacement Room, and rolls an open Job to one new same-Worker Assignment generation. Generic
  Assignment workspace/reporting scope and coding clones are rebuilt in the replacement Room before
  delivery resumes. Replaceable Worker/Job dispatchers and the exact current Assignment collector
  are restarted.
- **Native delivery:** A transport or controller failure after native submission begins records
  `recovery-required`, not failure. Recovery inspects the bound harness thread and uses the recorded
  baseline and native turn IDs to distinguish no submission, one submitted turn, completion,
  interruption, failure, active work, pending approval, and uncertainty. Input is resubmitted only
  after native history proves no turn was submitted. Worker-general and Job-native reconciliation
  remain independent. Native transcript bytes stay harness-owned.
- **Continuity limit:** Room replacement preserves the Dorf conversation binding but cannot
  promise that Codex can load a thread whose harness state existed only in the lost Room. That case
  remains visibly blocked with the native error; Dorf does not copy transcripts or silently start
  a replacement thread and call it continuity.
- **Why:** Clients, dispatchers, collectors, and app-server control processes are replaceable, while
  blind resend can duplicate side effects. Provider disk survival is the strongest available source
  of workspace and native-history continuity. An immutable Assignment generation records a changed
  Room honestly without reassigning the Job to a different Worker.
- **Reconsider when:** The harness offers native idempotency keys or cloud-restorable history, a
  remote control plane needs leases rather than local process locks, or another Environment provides
  a materially different restore/replacement boundary.

## D029 — Job and Worker ending retain identity and require observable cleanup truth

- **Status:** Accepted during stranger-loop ending implementation — 2026-07-27
- **Decision:** `job end NAME` closes new admission and requires settled prior input. It admits one
  stable cleanup input, requires its native turn to succeed, removes the exact Assignment workspace
  and Room-local reporting scope, ends the current Assignment, returns a caller-managed Worker idle,
  and retains the Job, goal, timeline, evidence, and native binding. `--interrupt` explicitly marks
  unsettled work interrupted and bypasses cooperative cleanup. `worker end NAME` requires no open Job,
  stops and destroys the exact current Room, clears its binding, and retains an ended Worker tombstone.
- **Retry and policy:** Ending and cleanup are separate durable facts. Workspace or Room cleanup
  failure remains `ending`/`cleanup-failed`; retry reconciles the same cleanup input and exact Room.
  Coding merge, explicit rejection, and abandonment force-end their Job. A dedicated coding Worker is
  then ended; a caller-managed Worker remains ready for another Job. Lifecycle policy is never
  inferred from its name.
- **Why:** Conversation success and PR state cannot prove process, workspace, credential, or VM
  cleanup. Retained identities preserve history and prevent accidental name reuse, while exact
  provider/workspace fencing prevents a retry from deleting another resource generation.
- **Reconsider when:** Keep/freeze Room behavior is implemented, permanent deletion/name reuse is
  required, or a harness exposes a stronger cooperative process-shutdown primitive.

## D030 — Recovery does not replace a lost Room without restorable native history

- **Status:** Accepted after complete stranger-loop dogfooding — 2026-07-27
- **Decision:** `worker recover NAME` restores and reconciles only the exact recorded Room while its
  provider body and disk survive. Provider-confirmed absence marks that Room absent, clears the
  Worker's current Room, and leaves the Worker offline. Dorf retains Worker, Job, Assignment,
  goal, documents, queued input, and typed native bindings, but does not provision a replacement
  Room, roll Assignment generation, rebuild a coding clone, or start a fresh native thread and call
  it continuity. `job end --interrupt` may acknowledge proven Room loss without attempting native or
  Room-local cleanup; the Roomless Worker can then be ended.
- **Why:** Authenticated replacement dogfooding preserved control-plane IDs but Codex could not load
  history stored only in the deleted VM. Continuing would therefore require distributed transcript
  or harness-state storage, harness-supported history restoration, or an explicit new-conversation
  handoff protocol. Those mechanisms are disproportionate at the current local coding-to-PR stage,
  and replacement without them creates misleading continuity plus Assignment, workspace, reporting,
  credential, and retry complexity.
- **Compatibility:** This narrows D025's currently implemented replaceable-Room posture and supersedes
  D028's absent-provider replacement path. Same-Room restoration, process recovery, native-turn
  reconciliation, durable offline inspection, and queued admission remain supported.
- **Reconsider when:** Codex or another current harness can restore native history independently of
  Room disk, Dorf deliberately owns sufficient conversation state, or a concrete workflow needs
  and validates an explicit handoff into a new native conversation.

## D031 — Buzz relay is the first remote client transport; ACP remains a separate seam

- **Status:** Superseded by D034 after the application-boundary review — 2026-07-28
- **Decision:** Make Buzz the first phone-facing client through a transport adapter that speaks the
  Buzz relay's signed Nostr event protocol over its WebSocket and REST surfaces. Do not put Buzz,
  Nostr, channels, threads, relay identities, or observer events in `dorf.runtime`. Present one
  stable Dorf coordinator identity in Buzz rather than projecting every Worker as a separate
  Buzz agent. The coordinator is an ordinary long-lived Dorf Worker using its general
  conversation. It may employ other Workers through the same typed control verbs available to any
  agent-first client; it is not a new runtime resource or a privileged replacement authority.
- **Identity and capability boundary:** The host-side adapter owns the coordinator's Nostr
  credential, verifies and publishes transport events, and never places that key in the
  coordinator's Room. The coordinator receives narrowly brokered, typed Dorf operations rather
  than the host shell, Incus socket, SQLite file, or transport credential. Per-Worker Buzz
  identities are not the default. They may be reconsidered only for a concrete, deliberately
  published long-lived Worker whose direct persona is valuable enough to justify separate
  enrollment.
- **Conversation and attention:** Natural-language conversation is the first remote interaction.
  A Buzz conversation addresses the coordinator and may discuss or create zero, one, or many Jobs;
  it is not exclusively bound to one Job. The adapter supplies exact event, reply, and known
  resource context. The coordinator interprets intent and selects typed operations, while Dorf
  validates every Worker, Job, Assignment, admission, and lifecycle transition. Ambiguity around an
  irreversible or boundary-crossing action requires explicit confirmation. Structured choices may
  later optimize narrow approvals, but are not a prerequisite for the first phone dogfood loop.
- **Durability boundary:** A durable adapter-owned mapping from Buzz event ID to the coordinator's
  internal input ID makes relay replay harmless while distinct events with repeated text remain
  distinct inputs. Concrete application records associate a Buzz conversation with every Job
  introduced there and record the exact outcome of coordinator tool actions; they do not create a
  second runtime aggregate or make thread identity authoritative for Job lifecycle. Deterministic
  identity, ordering, and typed execution protect the machinery even though semantic interpretation
  is conversational.
- **First-party Buzz experience:** Agent identity, membership, profile, presence, mentions, DMs,
  threads, messages, and audit history come from signed relay events rather than ACP. After the
  basic loop works, the adapter may emit Buzz's encrypted owner-scoped observer frames to render
  Dorf turns and activity in Buzz's existing desktop/mobile agent surfaces. Those frames are a
  transport rendering of Dorf-owned facts and claims, never a second authority.
- **ACP boundary:** Buzz's current ACP harness is a live, process-owning bridge that spawns an agent,
  turns relay mentions into ACP prompts, and expects the agent to publish through Buzz separately.
  Dorf already owns durable Worker, Room, Job, Assignment, and Codex app-server lifecycle, so
  that harness is not the remote-control foundation. D003 remains unchanged: ACP may be added later
  for a concrete second harness or ACP-native client such as Zed, independently of the Buzz
  transport.
- **Why:** Buzz explicitly supports agents and scripts through its agent-first CLI and relay APIs;
  ACP is one convenience harness, not the source of first-class agent identity. Direct relay
  integration preserves both systems' ownership boundaries, provides the native phone/team
  experience, and reaches a useful conversational demo before building a generalized protocol
  adapter or structured inbox UI. One coordinator matches how a human lead works in Slack: a single
  conversation can create and manage several independent pieces of work without requiring every
  subordinate to join the chat system. It also follows the north star's rule that a lead Worker is
  just another client speaking the same verbs.
- **Compatibility:** Buzz event kinds, observer payloads, key storage, profile registration, and
  adapter persistence are external and replaceable implementation details. The enduring boundary is
  the same typed Dorf verbs, durable internal admission identity, and no transport concepts in
  the runtime.
- **Reconsider when:** Buzz removes or materially restricts direct agent relay participation, its
  mobile client cannot support the real coding dogfood loop, observer formats cannot be consumed
  without runtime leakage, or another phone client proves a substantially smaller transport while
  preserving a first-class coordinator identity. Discuss a new runtime primitive before adding one
  if dogfood proves that an ordinary Worker general conversation plus typed client tools cannot
  express durable coordination without distortion.

## D032 — One durable Buzz instance is the main personal deployment

- **Status:** Accepted after provisioning the first owned relay; integration ownership revised by
  D034 — 2026-07-28
- **Decision:** Treat the persistent `dorf-buzz` Incus VM as the single developer's main Buzz
  deployment, not as a disposable fixture or a staging environment. Develop and validate client
  integrations against this instance, promote changes in place through small runnable demonstrations,
  and keep its identity, messages, and state across iterations. Do not introduce a staging/production
  split without an observed need.
- **Operating posture:** Pin source and container revisions, keep deployment secrets stable, make
  exposure and lifecycle changes programmatic and reversible, validate health and the real client
  path after changes, and take coherent backups before upgrades that put durable state at risk.
  Desktop is the first client for iterative integration work. Trusted WSS, a proper domain or other
  mobile-compatible exposure is a later deployment slice, after a real channel loop works and is
  useful.
- **Why:** There is one developer and operator. A second environment would add resource use,
  configuration drift, promotion ceremony, and false confidence without protecting other users.
  Using the actual long-lived instance makes early dogfood honest and follows the
  small-demonstration, early-adoption execution posture in the project principles.
- **Compatibility:** The VM remains shared infrastructure outside Dorf Room and Worker
  lifecycle. This decision does not make Buzz authoritative for Dorf state and does not weaken
  D034's ownership boundary.
- **Reconsider when:** Other users depend on the instance, public ingress or uptime expectations
  appear, an upgrade cannot be rolled back from a coherent backup, destructive migration testing
  becomes recurrent, or real usage demonstrates that pre-production validation is cheaper than
  recovery.

## D033 — The human Buzz owner key is client-generated and never server-managed

- **Status:** Accepted after first desktop onboarding — 2026-07-28
- **Decision:** Generate and retain the main community owner's private identity only in the
  first-party Buzz client under the human's control. Relay provisioning accepts only that identity's
  public key as `RELAY_OWNER_PUBKEY`; it must not generate, display, transfer, or retain a human
  owner private key. The relay's service-signing key remains a distinct deployment secret inside
  the VM.
- **Bootstrap posture:** A closed relay may wait for the human's public key before its first usable
  start. Do not temporarily open membership or create a server-side human identity merely to make
  provisioning unattended. An explicitly disposable automated fixture may own a fixture key, but
  that exception does not apply to the durable personal deployment.
- **Why:** The first desktop onboarding proved that Buzz can generate the human identity before
  joining a community and expose only its safe `npub` for enrollment. This produces one human, one
  owner identity, avoids a private-key transfer ceremony, and keeps the server from holding a
  credential it does not need.
- **Migration evidence:** The Mac-generated identity is the sole active relay owner. The temporary
  server-generated bootstrap identity was demoted, removed from membership after the desktop
  authenticated, and its private/public key files were deleted. It authored no conversation.
- **Reconsider when:** Buzz introduces a server-mediated owner bootstrap that never exposes or
  escrows the human private key, or a concrete non-human fixture needs an explicitly scoped
  operator identity.

## D034 — Human-facing applications remain SDK clients

- **Status:** Superseded by D047 — 2026-08-06
- **Decision:** Human-facing applications own their conversation, memory, semantic intent, channel
  integration, tool selection, and approval UX. They employ Dorf Workers through the same typed
  Worker and Job verbs available to humans and other clients. Dorf remains authoritative for Worker,
  Room, Job, Assignment, durable input, native delivery, observed facts, claims, evidence, recovery,
  and cleanup.
- **SDK boundary:** Trusted same-host applications call the concrete in-process Python SDK; the CLI
  is a sibling adapter over the same facade. Clients never implement lifecycle behavior, open Dorf
  SQLite, or construct Incus and Codex adapters themselves. A network transport may wrap the same
  SDK if an observed multi-host or untrusted-caller requirement appears, without changing Worker or
  Job semantics.
- **Compatibility:** The Python facade, any future request envelope, authentication method, and
  deployment topology are not yet stable public contracts. The enduring boundary is typed resource
  verbs, explicit provenance, retry-safe mutations, and separate ownership of application
  conversation versus runtime lifecycle.
- **Reconsider when:** A concrete client proves the existing verbs cannot express a required
  operation without distorting Worker or Job semantics, a second external client proves a shared
  protocol need, or multi-host operation invalidates the current authority model.

## D035 — Brokered model-plane authentication; credential-free sandbox images

- **Status:** Accepted — 2026-07-29 (supersedes D008 for Codex)
- **Decision:** A single host-side broker (pinned, vendored CLIProxyAPI) holds the ChatGPT OAuth
  bundle as the sole refresh writer. Sandboxes run real Codex app-server (D003) pointed at the
  broker via `model_providers` with a per-sandbox scoped key; sandboxes contain no OpenAI
  credentials and may be egress-limited to the broker alone. Incus images are credential-free for
  the Codex leg. Login is a one-time device-code flow; identical sandbox wiring serves
  ChatGPT-subscription or API-key billing. Chosen/rejected/parked options:
  `docs/history/model-auth-broker.md`. Validation: `docs/research/codex-auth-multi-sandbox.md`
  (2026-07-29 experiment).
- **Why:** Cloned secret-bearing images proved unreliable under concurrent refresh (#117, from
  #112/#114 Droid evidence). Brokered custody removes refreshable state from sandboxes by
  construction, preserves D003 session semantics, and matches the shape Amp operates in production
  for linked ChatGPT subscriptions. The undocumented-upstream risk is accepted and its maintenance
  delegated to a widely used OSS project.
- **Reconsider when:** OpenAI publishes a supported individual-account non-interactive path
  (`chatgptAuthTokens`, `personalAccessToken`); the undocumented upstream breaks or its terms
  posture changes; Dorf becomes remote or multi-user (add OIDC-style sandbox identity); or the
  Droid-leg validation produces contradicting evidence.

## D036 — Shared Provider Gateway for trusted clients and Dorf Sandboxes

- **Status:** Accepted direction — 2026-07-29; Go control plane at D047 cutover — 2026-08-08;
  remote E2B route wire proved — 2026-08-14
- **Decision:** Keep the Provider Gateway as a sibling application subsystem outside the durable
  Job core. Its programmatic boundary manages durable upstream Provider
  Connections and revocable consumer-specific Inference Routes over a supervised broker backend.
  Dorf composes it for Sandbox routes; trusted host applications may use the same authority for their
  own model routes. Connecting through either surface reaches one backing authority, so upstream
  subscription or API credentials are never copied into clients or Sandboxes. CLIProxyAPI is the first
  concrete daemon backend; D035 is the first validated ChatGPT-to-Codex route.
- **Location authority:** Deployment configuration resolves one Gateway data directory using
  `XDG_DATA_HOME` (falling back to `~/.local/share`) or an explicit operator override. Provider setup,
  doctor, admission, and task executors use that same adapter. Admission checks the named connection
  before creating a durable Job; the Job stores only the connection name, never a host path.
- **Local and remote posture:** The current broker has one concrete bind address. Host-only use may
  bind loopback; Codex Sandbox composition binds the selected private Incus bridge address so both the
  host and attached Sandboxes can reach it, never a wildcard or LAN address. An interface-scoped
  firewall rule admits the gateway port only from the bridge. Connection and route semantics do not
  depend on that location. A remote Sandbox may use the same route shape through an exact
  deployment-supplied HTTPS `/v1` URL while the broker remains private behind an outbound tunnel.
  The Sandbox adapter owns topology validation and exact-host default-deny egress; the revocable
  consumer route key authenticates Gateway requests. A disposable TryCloudflare tunnel proved that
  wire path for E2B, but it is not a durable deployment choice. Stable tunnel/domain operations,
  workload identity beyond the scoped route, and multi-user authority remain unimplemented until a
  concrete deployment requires them.
- **Provider posture:** The gateway is intended to admit validated subscription providers such as
  ChatGPT, Kimi Code, or Grok and API-key providers such as OpenAI or OpenRouter. Names are direction,
  not support claims. Validate each provider, auth mode, consumer wire dialect, refresh path, and
  concurrency behavior before advertising it. Do not add automatic pooling, fallback, quota
  scheduling, or a speculative capability matrix.
- **Why:** Host applications and Dorf Sandboxes are distinct consumers of the same model-provider
  connection. Sharing a typed facade and broker authority gives them login-once behavior without
  coupling provider state to Job semantics, duplicating credentials, or forcing model streams
  through the durable Job worker.
- **Reconsider when:** A second broker backend proves a smaller shared interface; an actual remote
  deployment requires a network authority; a provider cannot fit connection-plus-route semantics
  without distortion; or observed multi-account pressure justifies routing policy.

## D037 — New core Rooms use a global deployment profile, never a repository contract

- **Status:** Superseded by D047 — 2026-08-08
- **Decision:** New caller-managed Rooms use one host-local deployment profile under the XDG config
  boundary. The profile selects the built-in Environment configuration and names the default
  Provider Connection; it contains no provider credential, route key, Room lifecycle, or Job state.
  A successful `provider connect` selects that connection for new Rooms. `worker spawn NAME` uses
  the profile with no required option, while an explicit provider override remains available for
  current dogfood and repair. Generic Worker and Job commands never consult `.dorf.toml`;
  repository contracts remain workflow-owned inputs for coding setup, checks, review, and
  publication.
- **Why:** Room creation needs stable host deployment choices, but the caller's current directory is
  neither an authority for a Worker nor a safe source of environment/model behavior. Keeping
  provider credentials in the Provider Gateway, lifecycle in runtime SQLite, and only references in
  the profile preserves existing authorities while making summon repository-neutral.
- **Compatibility:** The profile path and JSON shape are internal and replaceable before the public
  release. Existing recorded Rooms remain self-describing and do not require the profile for
  inspection, messaging, assignment, recovery, or cleanup.
- **Reconsider when:** A second Environment proves a concrete selection seam, multiple validated
  Provider Connections require an interactive default chooser, or a remote/multi-user authority
  makes a per-host XDG profile the wrong ownership boundary.

## D038 — Official Sandbox images are immutable GitHub Release assets

- **Status:** Accepted and implemented — 2026-07-31; local-first and combined product-release
  boundaries revised during public activation — 2026-08-04; Go artifact boundary at D047 cutover
  — 2026-08-08; Go-required schema 4 after issue #38 dogfood — 2026-08-08; base and inventory
  clauses superseded by D064 and current artifact identity refined by D066 — 2026-08-13
- **Decision:** Publish the credential-free x86_64 Sandbox VM as an immutable GitHub Release containing
  exactly one Go x86_64 Linux archive/checksum and one Incus archive/compatibility manifest. One repo-owned
  local command builds from an immutable base fingerprint, records its exact Harness packages,
  proves the credential boundary, and completes a real coding tracer for every declared Harness from clone and
  repo-owned preparation through an implementation turn, checks, scoped routing,
  content-addressed evidence, and exact cleanup. The image includes Git, Go, Node, uv, and its
  declared Harness executables but removes build-only npm. The command exports the untouched candidate and
  publishes it with GitHub CLI. The consumer accepts only a
  published immutable release and requires agreement among GitHub's asset digests, the manifest,
  the downloaded archive SHA-256, and the post-import Incus fingerprint.
- **Artifact identity:** Attach the image to the normal immutable `vX.Y.Z` Dorf product release
  instead of creating machine-only releases in the human-facing release feed. The current exact
  schema and asset identity live in D066. The manifest requires `environment: incus`, the complete
  coding-workstation inventory, and verified pinned tool release-archive digests. Its
  candidate proof executes `go`, `gofmt`, and the repository's declared preparation in a fresh
  Sandbox. Issue #38 dogfood showed that the historical schema-3 image could reach a Go repository
  without Go installed, so the Go installer accepts only the current schema. Old clients and image
  schemas are not a compatibility target.
- **Promotion boundary:** The repository must be public and GitHub immutable releases must be
  enabled before the first image is promoted. The publisher records that reviewed repository
  setting in an explicit variable, requires a clean source commit already available from GitHub,
  creates a complete draft, publishes it once, and verifies its release attestation and all assets.
  The owner's provider credential remains in the local Provider Gateway; only a scoped route enters
  a disposable validation Sandbox, and neither enters the image or GitHub.
- **Why:** GitHub Releases reuse the project's source authority, provide static anonymous HTTPS
  downloads, protected tags and assets, release attestations, and API-visible SHA-256 digests
  without operating a public Incus daemon or a separate image-index service. Verifying every layer
  keeps the friendly alias out of the trust boundary and lets setup converge idempotently on one
  exact local fingerprint.
- **Why local publication:** A GitHub-hosted runner would require moving or recreating a provider
  credential to complete the real Job terminal, while a persistent self-hosted runner adds a
  needless public-repository execution surface. The current manual release cadence does not justify
  either cost. The version-controlled command retains deterministic CI-style proof without moving
  the owner's ChatGPT subscription boundary.
- **Compatibility:** The repository path, release tag shape, asset names, manifest schema, and
  installer module are pre-release implementation details. Existing Sandboxes remain bound to the image
  they were created from. The first image is x86_64-only; GitHub Releases are not a claim of support
  for another architecture or Environment.
- **Reconsider when:** Release size or bandwidth makes GitHub unsuitable, a second architecture
  needs a real distribution index, Incus simplestreams materially reduces setup complexity, GitHub
  cannot preserve the required immutability/digest guarantees, or a concrete remote Environment
  requires a different image authority. Reconsider unattended publication when a scoped
  non-personal provider credential and isolated ephemeral runner make it safer without weakening the
  real Job terminal.

## D039 — Initial core setup supports only the official Sandbox image

- **Status:** Accepted for initial open-source setup — 2026-07-31
- **Decision:** The guided core setup uses only the Dorf-published credential-free Room image.
  It does not offer a custom-image selector or claim compatibility with arbitrary Incus images.
  The global profile records the selected official image's immutable fingerprint, and existing
  Rooms retain their recorded image.
- **Why:** A custom image creates a second credential boundary, Codex/tool compatibility contract,
  update policy, validation path, and support surface. None is required to prove the first public
  one-command setup terminal, and supporting it now would make that terminal harder to maintain.
- **Reconsider when:** A concrete user need cannot be met by the official image and justifies the
  additional validation, compatibility, update, and support burden.

## D040 — Rename the product and complete namespace to Dorf

- **Status:** Accepted for initial open-source distribution — 2026-08-03; Python distribution
  portion superseded by D047 — 2026-08-08
- **Decision:** Rename the product to Dorf and use `dorf` consistently for the Go application,
  CLI command, repository contract, local configuration and state paths,
  environment-variable prefix, image and release artifacts, and repository identity. Do not retain
  compatibility aliases or migrate private dogfood state from the former pre-release namespace.
- **Why:** The selected identity should remain coherent across the installed artifact and every
  user-facing surface. The cutover has no old package compatibility obligation.
- **Compatibility:** This intentionally breaks private source imports, commands, configuration,
  state paths, environment variables, image names, and integrations that used the former namespace.
  Existing dogfood resources must be ended or recreated explicitly; Dorf does not guess ownership
  of or mutate the old namespace. The `dorf` identity becomes a public compatibility concern only
  after the first release.
- **Reconsider when:** A credible developer-tool naming conflict is discovered. After publication,
  require a deliberate migration decision.

## D041 — Host setup is capability-first with narrow native-package recipes

- **Status:** Accepted for initial open-source setup — 2026-08-04; automatic recipe narrowed to the
  proven Ubuntu 24.04 cutover host — 2026-08-08
- **Decision:** Accept any x86_64 Linux host whose local Incus daemon is already usable, but perform
  automatic package, service, and root-equivalent group mutations only through exact clean-host
  validated recipes. The Go cutover retains Ubuntu 24.04 LTS as the automatic clean-machine recipe;
  it uses native distribution packages, systemd's `incus.service`, and `incus-admin`, while pristine daemons
  delegate storage and private-network creation to `incus admin init --minimal`. Setup reinspects
  real state on every run and requests approval before package, service, or group mutation. Arch and
  other distributions remain capability-first: their operator follows the upstream/native Incus
  installation path and reruns the same readiness command afterward. This narrows the support claim
  rather than carrying a deleted Python host recipe into the Go product.
- **Storage default:** Retain Incus's directory-backed minimal default for the first stranger path.
  In a clean nested Ubuntu 24.04 host, three cached-VM guest-readiness samples had a 15.490-second
  median on `dir` and a 12.425-second median on a disposable loop-backed Btrfs pool. That gain does
  not yet justify installing another filesystem tool or choosing storage on the user's behalf.
- **Why:** Dorf should provide one calm setup experience without becoming a package manager or
  filesystem provisioner. Capability-first inspection preserves portability for users who already
  operate Incus; small evidence-backed mutation recipes keep the recommended path resumable and
  supportable. Delegating initialization to Incus and preferring the least invasive storage choice
  minimizes maintenance and host risk.
- **Reconsider when:** Another distribution completes the clean-host terminal; Incus publishes a
  reviewed universal daemon installer; native package/service semantics diverge enough to require a
  different recipe; or promoted Dorf-image measurements on non-nested supported hosts repeatedly
  exceed ten seconds for warm Room readiness and prove storage is the dominant cost.

## D042 — Issue-backed coding delegation has one disposable admission proof

- **Status:** Superseded by D047 — 2026-08-08
- **Decision:** Before an issue-backed `start` or `afk` invocation may reserve AFK ownership, create
  a coding Job or branch, or provision a durable Worker Room, run one workflow-owned proof against
  the exact repository head and issue. Aggregate independently discoverable repository, GitHub App,
  official-image, Incus, provider, and reviewer failures. When discovery succeeds, use one
  unrecorded disposable VM and scoped routes to clone the target branch, run repository preparation
  and smoke contracts, dry-run a Git push, and complete bounded real implementation-model and
  DeepSeek diff-review turns. Revoke the routes and destroy the VM before admission. The same
  invocation consumes the proof, records its non-secret facts, and repeated AFK delegation reuses
  the admitted Job and proof rather than duplicating identity or authority.
- **Why:** Metadata and isolated health probes can report healthy while Git credentials, repository
  preparation, the selected model, or the reviewer consumer path still fails. A single disposable
  terminal gives the owner one complete repair list and prevents known prerequisites from surfacing
  only after externally visible coding state exists.
- **Compatibility:** The proof schema, failure codes, disposable VM name, and CLI rendering remain
  internal alpha surfaces. Explicit task-text `start` remains the lower-level manual path until a
  concrete authority replaces the issue identity used by AFK delegation.
- **Reconsider when:** A non-GitHub coding authority becomes real, repository smoke proves too broad
  for admission and a narrower repo-owned readiness contract is observed, or a future Environment
  can provide the same exact disposable consumer proof without a local Incus VM.

## D043 — Coding acceptance is pinned at admission and proven from retained observations

- **Status:** Evidence authority retained by D047; Python AFK checklist/dossier mechanics
  superseded — 2026-08-08
- **Decision:** Compile the pinned issue acceptance criteria plus configured repository check and
  smoke obligations into a small workflow-owned checklist when a
  coding Job is reserved. The checklist remains explicitly human-correctable as a draft until the
  first verification attempt freezes it as the completion contract. Compute proven/unproven state
  from successful workflow-observed command records whose before/after Git
  commit is the exact dossier commit; Worker reports remain claims and never prove an item. Project
  those records, Runtime identity and Room image metadata, content-addressed Job artifacts,
  assumptions, risks, and cleanup state into one compact Markdown/JSON dossier. Publish that
  dossier to the PR as a view while the Job, workflow command records, and Job documents remain the
  authorities and audit layer. When a generic repository supplies no observable check, smoke, or
  verifier, do not manufacture a manual gate that the workflow has no mechanism to satisfy;
  preserve the existing PR-proposal path and leave final acceptance to the human GitHub boundary.
- **Why:** Reviewers need one ordered, commit-pinned assessment rather than reconstructing readiness
  through SQLite, Job-document, and artifact directories. Deriving proof from existing immutable
  facts preserves D024's provenance boundary and D026's workflow ownership without creating a
  competing evidence store or leaking coding semantics into the runtime.
- **Compatibility:** The checklist table, item verifier vocabulary, Markdown layout, JSON shape, and
  CLI names are pre-release workflow surfaces. Historical command records remain visible but cannot
  satisfy a new commit. Reports without an exact commit or turn association are displayed as
  unpinned claims and unresolved risk, never silently upgraded to fact.
- **Reconsider when:** A repository contract supplies a stronger machine-readable mapping from
  individual issue criteria to commands, a real workflow needs human attestation as durable proof,
  or repeated dossiers show that the compact selection hides evidence reviewers routinely need.

## D044 — Missing repository authority pauses inside the original delegation

- **Status:** Superseded by D047's explicit Go readiness/admission boundary — 2026-08-08
- **Decision:** When the exact issue-backed admission proof receives GitHub's not-found response
  before it can resolve the target branch, treat that one result as absent repository selection on
  the already configured Dorf GitHub App installation. Retain one deterministic, non-secret
  admission attempt keyed by the original command, local checkout, exact GitHub repository and
  GitHub App installation ID, starting commit, branch, issue, provider, and model inputs;
  create no Job, branch, Room, route, or AFK reservation. Open the installation's GitHub settings
  page with an attention item that accurately describes the persistent repository-wide grant and
  configured metadata-read, issues-read, contents-write, and pull-requests-write permissions.
  Observe only when branch authority appears through that same installation, then record idempotent
  approval and rerun the complete exact admission proof against the pinned repository and
  installation identities. The attempt expires after one hour, and decline or expiry is terminal
  for that generation; a later explicit retry creates a new generation without erasing the terminal
  record. The first coding Job reservation consumes approval and records admission in the same
  transaction so retries or a replaced controller process cannot create another Job.
- **Why:** Repository selection is one important, actionable authority decision that cannot safely
  be automated or replaced by a generic setup error. Keeping it inside the pinned delegation lets
  the owner approve once in GitHub while Dorf remains responsible for context retention, readiness,
  and continuation. The selected-repository grant persists beyond this delegation until a GitHub
  owner changes it; persisting only its non-secret installation ID, never its token or private key,
  keeps retained intent exact without turning durable workflow state into credential storage.
- **Compatibility:** Pending-attempt schema, expiry, failure code, installation URL, polling cadence,
  and CLI rendering are internal alpha surfaces. Other admission failures remain ordinary repair
  results; this does not introduce a general approval or workflow engine.
- **Reconsider when:** GitHub exposes a narrower repository-access callback than authority polling,
  an organization-request flow needs a distinct approval state, or a second concrete authority
  interruption proves a smaller shared primitive without leaking workflow policy into the runtime.

## D045 — Job execution composition is a public SDK handle

- **Status:** Superseded by D047 — 2026-08-06
- **Decision:** `Dorf.job_execution(JOB)` is the one public composition point for a recorded Job's
  runtime, Room environment, Codex driver, repository command execution, provider-route command
  rewriting, and Git-credential refresh. The coding workflow consumes that bound handle and keeps
  GitHub, Git, repository-contract, and coding-store policy; it does not construct runtime or
  adapter implementations. Disposable admission uses the same facade-owned environment and driver
  composition without creating a second workflow abstraction.
- **Why:** Coding setup, admission, active workflow commands, and detached input delivery had each
  reconstructed overlapping pieces of the same Job execution stack. Their concrete repetition
  earns this seam, while a bound handle remains smaller than a registry, plugin system, or workflow
  base class and leaves runtime semantics unchanged.
- **Reconsider when:** A second real environment or agent implementation proves the concrete facade
  needs selection policy, or a non-coding workflow demonstrates a smaller shared operation than the
  bound Job handle.

## D046 — AFK diff review is one bounded DeepSeek advisory cycle

- **Status:** Superseded by D047 — 2026-08-06
- **Decision:** After deterministic gates pass, run the DeepSeek diff role in a fresh disposable
  verifier Room pinned to the implementation commit. Retain its command result and always remove
  its Room and scoped route. Findings are claims admitted once through the original Job FIFO; after
  one implementation decision and new commit, one fresh verifier Room may confirm the result.
  No-findings permits publication but does not prove acceptance. A verifier infrastructure failure
  retains one exact repair-or-decline decision. Repair retries in a fresh Room; decline leaves the
  PR draft with missing advisory evidence stated.
- **Why:** A separate read-only model supplies useful diff pressure without giving it implementation
  authority, creating another Job, or turning advisory output into readiness fact. Existing command
  runs, Job FIFO, and Worker/Room cleanup provide the required custody and crash recovery.
- **Reconsider when:** Repeated dogfood shows one repair is insufficient, or another concrete
  verifier needs coordination that cannot compose through the same bounded role.

## D047 — Replace the Python runtime with a greenfield Go and Absurd system

- **Status:** Accepted foundation — 2026-08-06; review-request and Absurd-usage clauses superseded
  by D048 — 2026-08-09; commit ownership clarified by D050 — 2026-08-10; review handoff and unknown
  review selection superseded by D052 — 2026-08-10; harness execution vocabulary clarified by
  D055 — 2026-08-10
- **Decision:** Replace the current Python and SQLite implementation with a Go application using
  Absurd on PostgreSQL for durable execution. Dorf-owned PostgreSQL tables retain product facts;
  Absurd owns task claims, checkpoints, retries, waits, and wake events. Keep external-effect
  reconciliation in Dorf because no workflow engine can atomically commit an Incus, agent, Git, or
  GitHub effect together with its own checkpoint. Incus remains the first Sandbox, Codex app-server
  the first agent Harness, and GitHub pull requests the first acceptance surface.
- **Product vocabulary:** A coding request is a `Job`; its isolated execution body is a `Sandbox`;
  a bounded invocation of an agent in a named `Role` is an `AgentRun`. A `Harness` hosts agents, a
  `Thread` is its continuing conversation context, and a
  `Turn` is one request/response cycle. `Action`, `Check`, `Revision`, and `Evidence` name
  deterministic work and proof. Do not
  create a durable `Worker` identity until a real product requirement needs personality or memory
  across Jobs. Do not introduce `Assignment` or `RoleRun` as aliases for facts that these names
  already express.
- **Review policy (superseded by D048):** Start with a pure deterministic classifier over observed
  change facts. Mandatory rules select security, browser, performance, or other bounded review Roles
  when their explicit conditions match; documentation-only changes with green Checks may select
  none. Only an unknown classification may invoke one bounded semantic triage AgentRun.
  Implementation prose is not a policy input, and there is no optional-request mechanism. The
  durable Job coordinates mechanics, so there is no default Coordinator Agent and no
  review-after-every-change ritual.
- **Replacement strategy:** Build vertical slices on a `greenfield` integration branch. Each slice
  must reach the smallest real Incus, Codex, repository, and GitHub terminal it claims, then delete
  the Python component and implementation-coupled tests it replaces. The old implementation is
  behavioral evidence, not an API, schema, CLI, packaging, or document-format compatibility target.
  There are no users or old data to preserve: discard SQLite state, do not migrate or dual-write it,
  and do not add a Python facade.
- **Portability boundary:** Do not design a generic durable-engine interface. Localize Absurd task
  sequencing while keeping domain facts, policy, stable Action identities, and external
  reconciliation independent. If Absurd is outgrown, let short-lived active Jobs drain and schedule
  new Jobs on the replacement; completed Dorf domain records remain readable without treating
  Absurd checkpoint history as a portable format.
- **Polyglot boundary:** Go owns the core. Add a language-specific executor only when a concrete SDK,
  such as a TypeScript Sandbox provider or Python environment provider, makes that boundary
  materially smaller. It consumes a dedicated queue through a small versioned contract and may not
  leak vendor types into Job semantics. Do not add a plugin registry or second language in advance.
- **Why Go and Absurd:** Go supplies a small deployment artifact, explicit concurrency and process
  control, strong standard-library coverage, and readable deterministic policy around the Incus,
  agent, Git, and GitHub boundaries. Absurd supplies the durable queue, checkpoint, retry, sleep,
  event, and recovery machinery that Dorf should not reimplement, while retaining a local feedback
  loop and inspectable PostgreSQL authority. Temporal provides a broader and more operationally
  mature platform but adds a server, SDK, event-history, and deployment model beyond the current
  single-product need. Restate provides a polished durable-object and service model but makes that
  runtime a larger architectural center and is less aligned with the chosen fully local,
  application-owned feedback loop.
- **Supersedes:** D014's SQLite choice; D019's Python package topology; D025's mandatory durable
  Worker, Room, and Assignment model; D026's Python/SQLite composition; D034's in-process Python SDK
  boundary; D045's public Python execution facade; and D046's mandatory fixed DeepSeek review cycle.
  D011's one-task/one-branch/one-proposal shape, D012's Incus choice, D013's GitHub acceptance
  boundary, and D015's control-plane-owned GitHub authentication remain useful product constraints,
  expressed through the new vocabulary.
- **Rejected alternatives:** Do not rewrite the Python runtime in place behind compatibility
  adapters, start from an empty repository that loses working operational evidence, adopt Dagger for
  ordinary repository commands, or build a custom database scheduler and recovery controller.
  Dagger may be reconsidered for a repository that already owns a Dagger contract or needs one
  reproducible cross-host build graph; direct repo-owned commands are simpler for the first system.
- **Reconsider when:** Absurd cannot survive the real crash and redelivery terminals, its evolution
  or license makes self-hosting unsuitable, multi-region or high-volume hosted operation requires a
  more mature distributed control plane, a second concrete language or agent Harness proves a
  smaller stable seam, a real cross-Job identity requirement earns `Worker`, or repeated dogfood
  identifies a concrete authoritative input that deterministic facts plus bounded triage cannot
  express and that justifies a structured additional-review contract rather than parsing agent
  prose.

## D048 — Simplify the post-cutover core around Absurd and explicit workflow semantics

- **Status:** Accepted audit correction — 2026-08-09
- **Durable sequencing:** Use Absurd's public task, named-step, event, cancellation, and inspection
  surfaces for generic execution mechanics. Dorf retains Job and Revision facts, deterministic
  policy, stable external Action identity, scope, settlement state, and reconciliation because those are product
  semantics or cross-system uncertainty. Production behavior and authority must not query or mutate
  Absurd's raw internal tables or mirror its checkpoints, retries, leases, task state, or recovery
  controller into Dorf-owned schema. Version-pinned white-box tests and operator diagnostics may
  inspect those tables without making them product authority.
- **Message order:** Every accepted message keeps a monotonic Job-local admission sequence. Follow-up
  Turns are FIFO. A `steer` is an explicit priority lane for the active harness Turn and may overtake
  queued follow-ups. Default text and structured inspection, command help, and the admission
  acknowledgement must expose its intent, target, original sequence, and priority effect; an
  architecture document alone is not adequate observability.
- **Review composition (superseded by D052):** Deterministic policy supplies the mandatory Role floor. An implementation
  AgentRun may additionally make a structured, bounded request for an allowlisted Role and optional
  focus. The request cannot remove mandatory review, change capabilities, grant authority, create a
  Role, or authorize recursive or unbounded work. Each selected Role receives its own disposable
  Sandbox and scoped provider route, including read-only review, and those live resources are
  reclaimed after the Role's Evidence is retained.
- **Greenfield schema:** Before the first release, replace the prototype migration chain with one
  clean baseline schema; there is no data or upgrade path to preserve. Defer `sqlc` until after the
  Absurd realignment and schema squash. Generated type-safe query wrappers can remove scan
  boilerplate, but they do not simplify the state model, perform migrations, or replace behavioral
  PostgreSQL tests. Reconsider only if substantial repetitive query plumbing remains in the smaller
  store.
- **Verification:** Agents run deterministic unit and PostgreSQL integration tests locally before
  pushing relevant changes. CI repeats those portable suites as an independent merge gate. Real
  Incus, Codex, provider, and GitHub terminals remain targeted dogfood for changes to those
  authorities rather than simulated requirements for every CI run.
- **Cutover correction:** The final acceptance proposal remains blocked until the audited functional
  gaps are closed: private-repository clone authority, exact initial-turn identity, lease-safe
  mutation ownership, human-requested same-Job revisions, explicit pre-proposal terminal outcome,
  and prompt reviewer-resource cleanup. The correction must preserve the proven exact-Revision,
  Action-reconciliation, publication, outcome, and zero-ghost properties while materially reducing
  the application-owned durable state machine.
- **Why:** The greenfield implementation proved the complete shape, but the proof also exposed that
  Dorf rebuilt generic durable mechanics beside Absurd and accumulated prototype schema and
  implementation narration. Returning those mechanics to the chosen library leaves Dorf's code
  focused on product facts and unavoidable external-system boundaries. Explicit steer and review
  request semantics preserve useful intentional behavior without hiding it behind an inaccurate
  FIFO or prose-policy description.
- **Reconsider when:** Absurd's public APIs cannot express a proven recovery terminal without losing
  required evidence, measured reviewer isolation cost justifies a different model, or the smaller
  post-realignment store still contains enough repetitive typed query plumbing for `sqlc` to remove
  more code than its generator and generated surface add.

## D049 — Repositories own their development-tool setup

- **Status:** Accepted setup boundary — 2026-08-09; fixed image-inventory sentence superseded by
  D064 and D066 while repository ownership remains accepted — 2026-08-13; PostgreSQL installation
  changed from a Mise source build to the native distribution package — 2026-08-14
- **Decision:** Do not expand the shared Incus image for Dorf-specific Go, Absurd, PostgreSQL, or
  inspection needs. D064 and D066 own the current shared image profile. Dorf's declared
  `commands.prepare` invokes a repository-owned script that installs missing pinned repository
  tools through Mise and installs PostgreSQL from the native distribution package when a local
  database is needed. The supported Debian workload uses its package-managed cluster; development
  hosts may instead provide `DORF_TEST_DATABASE_URL`. The script converges the disposable database
  and Absurd schema and downloads modules. Installation and initialization are deterministic
  Actions; no AgentRun decides or performs them conversationally.
- **Cost:** Every fresh Sandbox may pay package-download and initialization time. That cost is
  accepted now because the setup remains visible, editable with the repository, and independent of
  a Dorf-wide image release.
- **Why:** Different repositories need different toolchains. Encoding all of them in one shared
  image couples repository evolution to image publication and grows a supposedly reusable base.
  A repository contract is the simplest correct ownership boundary. PostgreSQL's Mise backends
  compile upstream source, while the supported Sandbox distribution supplies a maintained binary
  package; using that package removes repeated compilation without coupling PostgreSQL to the image.
- **Reconsider when:** Repeated measurements show setup materially dominates Job latency or network
  reliability. First add a content-addressed package cache; if that is insufficient, snapshot a
  successfully prepared repository environment behind the same `commands.prepare` contract.

## D050 — Implementation AgentRuns own commits

- **Status:** Accepted workflow correction — 2026-08-10; terminology clarified by D052 and D055
- **Decision:** An implementation AgentRun owns the code change and, when it changes
  code, creates one or many commits in the Job checkout. Its successful change contract requires a clean
  checkout and a final `HEAD` that is a proper descendant of the AgentRun's input Revision. Dorf
  validates those facts and records the observed `HEAD` as the next exact Revision; it does not
  manufacture, squash, or amend the agent's commits. A follow-up Message may instead be handled with
  a clean unchanged `HEAD`; that creates no new Revision.
- **Action boundary:** An Action is a code-owned external mutation such as creating a Sandbox or
  route, pushing Git, or publishing a pull request. An AgentRun owns submission and recovery of its
  harness Turn directly; it is not paired with an Action. Tool calls and commits made inside that
  AgentRun are also not separate Actions. Their transcript remains harness-owned, their commits
  remain Git-owned, and Dorf retains the observed Revision and Revision-pinned Evidence it needs.
- **Why:** Commit structure is part of implementation judgment and may naturally require more than
  one commit. Treating commit creation as a later deterministic Dorf step both erases that judgment
  and misstates the recovery boundary. Validating clean descendant Git state gives the workflow an
  exact handoff without making Dorf the author of the change.
- **Reconsider when:** A supported harness cannot reliably commit inside the Sandbox, or a concrete
  acceptance surface requires a separately reviewed normalization step that preserves authorship
  and exact source-Revision provenance.

## D051 — One explicit coding coordinator uses stable Absurd Steps

- **Status:** Accepted workflow boundary — 2026-08-10; transitional phase removed by D061
- **Decision:** The coding path is ordered by one readable `workflow.RunJob` coordinator. It invokes
  single spine operations in product order. `CurrentWork` selects the exact owning fact. Each external
  Action runs in its own `dorf/action/v1/<ActionID>` Step and returns
  `ActionStepResultV1{ActionID}`; AgentRun, Revision, Check, and policy operations retain their own
  stable versioned Step names and typed results. Absurd owns durable execution mechanics; PostgreSQL
  remains authoritative for Job facts and Action settlement.
- **Boundary:** Ordinary Incus, Git, Codex, and command work is not held under one long Job fence.
  Each code-owned external mutation reserves and reconciles its stable Action, performs a final
  claim check, records Action success, and then completes its Step. Each AgentRun instead reconciles
  its own Harness/Thread/Turn identity. D061 removes the transitional `workflow_phase`; the mixed
  service-layer coordinator that interpreted it was already deleted. The application constructs one
  compile-time `ServiceStore` and `ServiceExternals` boundary; runtime capability assertions do not
  select coding behavior.
- **Why:** The flow is understandable in one place, and interruption recovery comes from the chosen
  durable runtime rather than a second Dorf-owned program counter.
- **Reconsider when:** D061 absorbed the original trigger after review and publication became direct
  fact-ordered operations. Reconsider this remaining boundary only when real dogfood shows that a stable
  Step identity or local fact guard cannot represent recovery truthfully.

## D052 — Feedback is a Message to the implementation AgentRun path

- **Status:** Accepted workflow simplification — 2026-08-10
- **Decision:** User text, Check output, ReviewPolicy prompts, and reviewer text use the same durable
  `Message` primitive. Every AgentRun consumes exactly one Message. ReviewPolicy creates the stable
  workflow Message consumed by a selected review AgentRun; reviewer output is an ordinary agent
  Message consumed through the implementation AgentRun path. Dorf does not parse a universal
  `ReviewResult`, classify findings, or create separate respond-to-review, respond-to-check, or
  repair AgentRun types.
- **Message identity:** A Message records text, Job-local sequence, `FromKind`, and `FromID`.
  `FromKind` names the sender: human, agent, or workflow. `FromID` retains the exact request, AgentRun,
  or Check that caused the Message, and the consuming AgentRun points back to the Message. A Check or
  observation does not send a Message; the workflow turns its result into one. `Feedback` is a use of
  Message, not another stored type. Do not add reply or thread fields until an outward-response
  feature needs them.
- **Review selection:** Deterministic policy selects known specialist Roles. Unknown risk selects one
  bounded general read-only reviewer rather than a triage AgentRun whose prose must be parsed to route
  more work. Role, Revision, and capability envelope specialize an AgentRun; its prompt is the input
  Message. The adapter supplies its Sandbox workspace without persisting that path on the run. These
  inputs do not create a new execution primitive.
- **Outcome:** The implementation agent decides whether to act, ignore, or explain. Dorf observes the
  resulting Git state. A committed descendant becomes a new Revision and loops through Checks and
  policy. A clean unchanged checkout means the Message was handled without a code change. A failed
  mandatory Check still blocks readiness until it passes, but it reaches the agent through the same
  Message mechanism.
- **Authority:** Reviewer prose is an advisory Message, not Evidence. The Message is the durable
  handoff between agent Threads. One observed Evidence record proves the review AgentRun's harness
  execution; the prose is not duplicated as Evidence. Git remains authoritative for commits,
  and deterministic Checks remain authoritative for their results.
- **Why:** This matches ordinary human collaboration: one worker receives feedback from different
  people and tools, decides what it means, and either changes the work or explains why not. One
  AgentRun mechanism, one inbox, and opaque text remove parsers and review-specific state without
  weakening deterministic gates.
- **Reconsider when:** A concrete acceptance surface requires a machine-readable reviewer decision, or
  dogfood proves that ordinary text cannot safely carry a specific cross-agent contract.

## D053 — Compile stable PostgreSQL queries with sqlc

- **Status:** Accepted persistence-tooling boundary — 2026-08-10
- **Decision:** Use repository-pinned `sqlc` 1.31.1 to compile stable Dorf-owned PostgreSQL queries
  into a committed private `database/sql` package. Keep the baseline schema and named query files as
  generator inputs. `postgres.Store` remains the application boundary: handwritten Store methods own
  transactions, compare-and-set expectations, product invariants, error translation, and explicit
  conversion to `spine` types. Generated records and parameter structs do not cross that boundary.
- **Type boundary:** Narrow column overrides may reuse existing non-null `spine` scalar types for
  cleanup, Message sender and intent, Action kind and state, AgentRun state, and Job outcome. This is
  an inward adapter dependency and does not make generated records into domain types. Nullable values,
  projections, and timestamps continue to be mapped explicitly.
- **Tooling and verification:** Generation and stale-code comparison use the local schema analyzer and
  do not require a database. Strict function and `ORDER BY` checks are enabled. The repository check
  also runs `sqlc vet` with `sqlc/db-prepare` against the already migrated disposable PostgreSQL
  database, followed by the live PostgreSQL Go suite and `go vet`. CI starts a fresh PostgreSQL
  service and initializes the pinned Absurd and Dorf schemas before running the same check. No sqlc
  Cloud project, token, `push`, `verify`, managed database, migration ownership, or runtime service is
  introduced. This follows sqlc's official [CI guidance](https://docs.sqlc.dev/en/stable/howto/ci-cd.html),
  [configuration reference](https://docs.sqlc.dev/en/stable/reference/config.html), and
  [override guidance](https://docs.sqlc.dev/en/stable/howto/overrides.html) while keeping all committed
  paths repository-relative.
- **Why:** The trial removed embedded SQL and manual scan plumbing from representative reads, made
  schema/query drift fail during generation, and kept the transaction and domain boundaries readable.
  The fixed configuration and generated volume are accepted because generated code is mechanically
  maintained; the decision is based on safer refactoring and a faster compiler-like feedback loop,
  not on counting generated lines as handwritten maintenance.
- **Measurement:** The integrated broad pass reduced handwritten production Go from 12,062 to 11,973
  lines (-89) and tests from 7,670 to 7,651 lines (-19). Ten named query files contain 1,210 lines;
  the committed private generated package contains 4,960 lines; and the three new tool/config entry
  points contain 81 lines. Local generation and stale-code diffing each take about 0.6 seconds. All
  188 stable product query call sites moved behind sqlc. The 12 remaining direct Store calls are the
  explicit Absurd/bootstrap, schema-application, and Job advisory-lock exceptions.
- **Reconsider when:** A supported query cannot be expressed without retaining a parallel handwritten
  implementation, generator churn repeatedly obscures review, or future measurements show the
  handwritten configuration and conversion surface outweighs the query plumbing it replaces without
  delivering useful drift failures.

## D054 — The main Job task publishes and observes the exact proposal

- **Status:** Accepted workflow simplification — 2026-08-10
- **Decision:** The main Job task runs two direct, Revision-scoped Steps: push the exact Revision, then
  create or adopt its exact pull request. Stable Actions reconcile Git and GitHub before an uncertain
  effect repeats. There is no publication child task, attachment field, or mirrored task state.
  D068 later adds a thin Dorf command over Absurd's public retry without changing this ownership.
- **Acceptance UI:** The same task observes the exact pull request. A comment from the repository
  `OWNER` or `COLLABORATOR` becomes one idempotent human Message whose `FromID` is the GitHub comment
  identity. Dorf acknowledges it with an eyes reaction. Once the same implementation flow has handled
  the Message and republished, Dorf posts one completion comment naming the exact Revision. GitHub's
  idempotent reaction endpoint and a stable invisible completion marker make replay safe without a
  new core fact. Merge records acceptance, close without merge records rejection, and explicit Dorf
  abandonment remains available. Dorf stores Messages, Proposal, and Outcome, but does not mirror a
  comment cursor or mutable pull-request state. Every durable wake or timeout performs a fresh
  reconciliation pass; no in-memory poll counter or checkpointed GitHub observation is part of the
  workflow contract.
- **Why:** Push, propose, wait, and continue are one product story. Giving publication its own durable
  scheduler duplicated retry and attachment mechanics already owned by Absurd. Treating GitHub input
  like any other Message also lets the next implementation AgentRun decide whether to act without
  a new review-result or response type.
- **Reconsider when:** GitHub polling is measurably wasteful enough to justify a webhook wake-up. A
  webhook should wake the same observer; it must not create a second workflow authority.

## D055 — AgentRun owns its harness execution binding

- **Status:** Accepted execution simplification — 2026-08-10
- **Decision:** `AgentRun` is Dorf's complete durable delivery record for one durable Message.
  Submitting, reconciling, waiting for, and recording the harness Turn are its lifecycle, not a
  paired `Action`. Every AgentRun consumes exactly one durable Message; every Message selected for
  agent delivery has exactly one AgentRun. The AgentRun retains the Message identity, Harness,
  Thread, Turn, Role, Revision, capability, Turn outcome, recovery baseline, and the nonces required
  to prove ownership and submission. Implementation continuity comes from reusing the Thread bound
  to prior implementation AgentRuns; there is no separate Thread row or binding on Job. A follow
  normally creates a new Turn; a steer normally binds to its target Turn and creates a new Turn only
  on terminal-target fallback.
- **Vocabulary:** A Harness is software that hosts an agent and exposes Threads and Turns; Codex
  app-server is the first Harness. A Thread is a continuing harness conversation that one or more
  AgentRuns may use. A Turn is one request/response cycle inside a Thread. These known terms replace
  ambiguous session/execution vocabulary without leaking Codex protocol types into the core.
- **Action boundary:** Actions record code-owned external mutations such as creating or destroying a
  Sandbox or route, pushing Git, and publishing a pull request. Agent execution has one durable
  identity and state machine: AgentRun. There is no turn-start Action, separate
  `codex-session-start` Action, or Action/AgentRun synchronization.
- **Evidence and feedback:** Agent prose is advisory text carried by Message. Evidence for a harness
  observation links directly to the AgentRun whose Harness/Thread/Turn binding and Turn outcome it
  records. A completed review retains one such observed Evidence record; its prose is delivered as
  the exact feedback Message through the implementation AgentRun path and is not copied into
  Evidence. Atomic feedback recording makes the completed AgentRun, observed Evidence, and exact
  Message one idempotent handoff.
- **Review input:** Deterministic ReviewPolicy creates one stable workflow Message for each selected
  Role. The review AgentRun consumes that Message like any other AgentRun, and review prompt text is
  stored only on the Message. This keeps future reviewer follow-up and Thread reuse inside the same
  Message-to-AgentRun model. The current coding workflow routes ordinary follow-ups only to the
  implementation Role; adding reviewer follow-up later is a routing decision, not a new durable
  primitive or table.
- **Adapter boundary:** The Harness adapter owns app-server protocol, transcript, tool items, and the
  transient Sandbox workspace and controller process. Dorf does not persist adapter-owned workspace
  paths, derived input digests or controller identities, or unused token, cost, and yield telemetry.
  Adding another Harness should implement the same narrow execution boundary; it does not justify a
  registry or speculative portability layer.
- **Why:** One agent delivery previously had overlapping AgentRun, turn-start Action, and
  `codex-session-start` Action state. Recovery had to synchronize records that described the same
  work, while review copied input text onto AgentRun and output prose into Evidence beside the
  Messages that could carry both. One input Message, one AgentRun, one observed fact and, for review,
  one output Message state the product story directly.
- **Reconsider when:** A second concrete Harness cannot express a resumable Thread and bounded Turn,
  or dogfood proves that one AgentRun cannot retain enough identity to reconcile uncertain
  submission without a distinct durable effect record.

## D056 — Jobs own Sandbox lifetimes and Sandboxes identify Provider Routes

- **Status:** Accepted resource-lifecycle simplification — 2026-08-10
- **Decision:** The Job is the aggregate and lifetime owner of every Sandbox created for the coding
  task, including isolated review Sandboxes. Each Sandbox deterministically identifies its one scoped
  Provider Route, so Dorf does not store a second Route row. AgentRuns use a Sandbox and record that
  binding, but do not own infrastructure. Cleanup walks Job → Sandboxes and records each Route revoke
  before Sandbox deletion as an immutable Action success. There is no polymorphic owner kind/id.
- **Why:** Ownership follows the longest relevant lifetime, keeps database relationships concrete,
  permits AgentRun retries and follow-ups to reuse a Sandbox, and gives one cleanup inventory without
  copied reviewer-resource state or role-specific cleanup algorithms.
- **Reconsider when:** A concrete workflow requires resources to outlive their Job, or a Sandbox must
  be shared safely by multiple Jobs; either case would require an explicit new aggregate and custody
  rules rather than a polymorphic owner shortcut.

## D057 — Ordinary external Actions target one exact Sandbox

- **Status:** Accepted Action-scope simplification — 2026-08-10
- **Decision:** Sandbox creation, repository clone, provider-route creation and revocation, exact
  review checkout preparation, and Sandbox deletion use one Sandbox-scoped Action path. Setup keeps
  its generation-aware operation and publication keeps its exact-Revision operations; there is no
  generic Job-Action API or polymorphic target abstraction. The first reconciled Action success is
  immutable and an identical retry is a no-op. Exact external-result validation belongs to the
  adapter before that success is recorded.
- **Why:** These ordinary mutations all change or serve one exact Sandbox. A generic Job path hid a
  redirect to the main Sandbox and created a category with only repository clone as a real member.
  Explicit Sandbox scope makes Action identity, Absurd Step identity, reconciliation, and cleanup
  tell the same story while preserving the crash boundary between execution and external truth.
- **Reconsider when:** A second ordinary external mutation genuinely targets only the Job aggregate,
  or an external system returns a non-Sandbox identity that cannot live in its natural product fact.

## D058 — Action success is the external lifecycle authority

- **Status:** Accepted lifecycle-authority simplification — 2026-08-10
- **Decision:** Sandbox rows retain only durable identity, Job ownership, and the nonce required to
  attest the exact external resource. Sandbox and Provider Route lifecycle is read from immutable
  Sandbox-scoped Action success. Dorf does not persist parallel pending/created/deleted or
  pending/active/revoked state machines. Provider Route identity is derived from the Sandbox's stable
  route-create Action identity rather than stored in a separate row.
- **Why:** The copied states could only agree with their Actions, so every completion, cleanup query,
  inspection view, and test had to synchronize two descriptions of the same event. One authority makes
  retries and cleanup easier to read: revoke Action succeeded, then delete Action succeeded.
- **Reconsider when:** A provider returns a non-deterministic Route identity or a lifecycle fact exists
  independently of any Dorf Action and has a concrete product consumer.

## D059 — Actions retain settlement, not generic result strings

- **Status:** Accepted Action-result simplification — 2026-08-10
- **Decision:** An Action retains its stable identity, kind, scope, and settlement state: unsettled,
  succeeded, or failed. The external adapter validates exact identity and authority before recording
  success. Durable facts returned by an operation live in their natural typed owner: setup output in
  Evidence, pull-request identity in Proposal, terminal disposition in Outcome, and exact Sandbox or
  Revision targets in Action scope. Dorf does not copy those facts into generic `external_id` or
  `external_outcome` Action strings.
- **Why:** Generic result strings repeated facts already known from Action scope or a typed product
  record, forced central parsing of adapter-specific formats, and made inspection look authoritative
  in two places. Settlement plus the natural owner states the same recovery story directly.
- **Reconsider when:** A concrete external mutation returns a stable, non-derivable fact required for
  reconciliation that has no honest typed product owner.

## D060 — GitHub authority is stored once

- **Status:** Accepted Proposal/Outcome simplification — 2026-08-10
- **Decision:** The Job owns immutable GitHub repository, installation, base branch, and head branch
  authority. Proposal retains only pull-request number, URL, exact proposed Revision, and body digest.
  Outcome retains disposition and observation time. Accepted and rejected Outcomes additionally retain
  the exact terminal pull-request observation and optional merge commit. Explicit abandonment is its own
  human authority: it may precede Proposal and therefore retains no invented GitHub state; when a Proposal
  already exists, Dorf still verifies its exact identity and refuses a merged pull request. Publication and
  observation join these facts instead of copying authority or an already-validated remote head. Merge and
  close are observed automatically; `dorf abandon JOB` is the only manual Outcome command.
- **Why:** Proposal and Outcome repeated facts fixed by their Job and predecessor, creating comparison
  code, larger schemas, and inconsistent states that had no product meaning. One owner per fact makes
  publication, inspection, and terminal recovery read in the same order as the workflow.
- **Reconsider when:** A Proposal must survive independently of its Job, one Job can own multiple live
  GitHub authorities, or a terminal authority contains a new fact that has no current owner.

## D061 — One fact-derived coding flow replaces the durable program counter

- **Status:** Accepted workflow-authority and inspection decision — 2026-08-10
- **Decision:** Dorf does not persist `workflow_phase`, `next_work`, or another derived workflow
  status. It loads one concrete coding `Snapshot` and derives a disposable `Projection` containing
  readiness and the pure `CurrentWork` decision. Execution, human history, and structured inspection
  share that same Snapshot rather than loading or interpreting the facts independently. `RunJob`
  reloads after each recorded fact, and every mutation transactionally revalidates its exact owner
  before recording an effect. Absurd alone owns task eligibility, claims, named checkpoints, sleeps,
  waits, retries, and cancellation.
- **Missing recovery fact:** A completed implementation Turn does not prove that Dorf inspected its
  mutable checkout. Bind the implementation AgentRun to its input Revision, then retain the existing
  `git-revision` Evidence for both changed and unchanged clean observations with that AgentRun as its
  owner and observed `HEAD` as its Revision. A changed observation also creates the next immutable
  Revision; equality records an unchanged result. This reuses one natural proof fact rather than
  adding a Revision-observation table or stored changed/unchanged enum, and replaces the only real
  information previously hidden in implementation/review handoff phases.
- **Human view:** Inspection derives both the expected coding dependency chain and chronological
  Message, Action, AgentRun, Revision, `git-revision` Evidence, Check, ReviewPlan, Proposal, Outcome,
  attention, and cleanup facts from the same Snapshot, and marks the same `CurrentWork` used by
  execution. This Projection is disposable. Structured inspection may show attached task correlation
  and terminal result, but it does not copy Absurd attempts, checkpoints, waits, or leases;
  `absurdctl` and Habitat remain the operational execution-history tools.
- **Why:** One source of truth removes phase transitions and impossible phase/fact disagreements,
  makes recovery ask only which authoritative fact is missing, and puts the deterministic flow in one
  readable place. It also makes change local and composable: a new Check adds a Check fact, a selected
  reviewer adds a Message and AgentRun, and another feedback source adds a Message instead of changing
  a schema enum and transition matrix across admission, readiness, publication, inspection, and
  tests. Reordering or extending the coding flow changes explicit dependencies rather than synchronized
  status machinery.
- **Constraint:** This is not permission to build a general DAG engine, configurable workflow DSL,
  generic step registry, persisted status projection, copied event-sourcing layer, or giant SQL
  `next_work` query. Keep the concrete coding decision small and visible in Go. Add a new durable fact
  only for an observed product or recovery need, and keep product chronology derived from natural fact
  timestamps.
- **Reconsider when:** A second real workflow proves that a smaller shared decision primitive exists,
  or measured fact reconstruction becomes a material bottleneck that cannot be addressed by ordinary
  read projections without creating another authority.

## D062 — Dorf is a durable agent-Job control plane proven through concrete workflows

- **Status:** Superseded by D063 — 2026-08-13
- **Positioning:** Dorf is the open-source control plane for durable agent Jobs on infrastructure its
  owner controls. Workflows use deterministic code for knowable work and isolated agents for
  judgment, with recovery and Evidence built in. The supported claim remains transparent: Codex,
  Incus, and coding-to-PR are verified today; multiple Harnesses, Sandboxes, workflows, and trigger
  surfaces are direction until real implementations prove them.
- **Product boundary:** A trusted client owns personal context, priorities, and composition across
  Jobs. A workflow owns the semantics, policy, evaluation, and Outcome of one bounded Job. Dorf owns
  durable custody: admission, Messages, AgentRuns, Sandbox lifetime, stable external-effect
  reconciliation, observed Evidence, attention, recovery, and cleanup. A software factory, personal
  assistant, or agent organization may compose Dorf Jobs, but none becomes Dorf's core metaphor.
- **Next proof:** Coding-to-PR remains the only implementation requirements driver until a bounded
  research-to-report issue deliberately begins. Research is the candidate second workflow because it
  needs the same durable custody while owning no branch, Revision, Proposal, or GitHub Outcome. Agent0
  should first invoke it through the existing same-host structured CLI boundary. Do not add HTTP or
  embed the Go runtime merely to integrate the first trusted client.
- **Extraction gate:** Add the research workflow's natural facts and explicit coordinator before
  changing the coding schema into generic payloads or nullable fields. After coding and research
  work, extract only duplicated behavior with the same authority and recovery meaning into an
  internal application API. Exercise that API through another small workflow or external author
  before declaring a public workflow-authoring compatibility promise.
- **Authoring and evaluation:** The intended shareable unit is ordinary versioned workflow source
  plus typed input and Outcomes, capability and connection requirements, deterministic operations,
  bounded AgentRun judgment, budgets, terminal conditions, and evaluation cases. Evals begin with
  the workflow rather than arriving after the SDK. Agents may author reviewable workflow code,
  manifests, tests, and evals, but may not activate new powers or production versions silently.
- **Clients and distribution:** CLI and Agent0 come first. CI/GitHub is the likely first public
  trigger; HTTP/webhooks and MCP follow real remote-client needs; native Slack and scheduling remain
  later adapters. Share pinned Git-hosted workflow bundles before building a registry or marketplace.
  Triggers translate external events into idempotent admission and never become workflow authority.
- **Why:** The Go rebuild proved durable coding behavior and removed speculative Worker, Room, phase,
  and framework abstractions. It also left coding facts near records that look reusable. Returning to
  a broad framework now would repeat the same mistake. A materially different second workflow is the
  smallest visible result that can prove the building-block thesis while preserving Mitchell
  Hashimoto's posture: make simple dependable pieces, adopt them early, and let real composition earn
  public seams.
- **Refines:** D009's single-driver gate, D047's coding-shaped Go foundation, and D061's explicit
  fact-derived coordinator. It does not weaken their current coding invariants or authorize a generic
  DAG, plugin registry, provider matrix, or compatibility layer.
- **Reconsider when:** Research fails to reuse the proposed custody semantics, another second
  workflow offers a smaller proof, a remote Agent0 deployment justifies network transport, an
  external workflow author exposes a different API boundary, or repeated workflow evidence shows
  that the durable custody model does not reduce attention or improve trustworthy outcomes.

## D063 — Dorf Core portability precedes general workflow authoring

- **Status:** Accepted product direction — 2026-08-13
- **Positioning:** Dorf is the open-source control plane for running agent Harnesses on infrastructure
  its owner controls: your agents, your infrastructure, one API. Core is the product. Whole-setup
  portability is direction; Codex and Pi with local Incus coding-to-PR on the supported host are the
  current verified Harness claims. D065 records the completed second-Harness proof.
- **Profile contract:** A verified profile covers the supported Harness version and configuration,
  skills, extensions or plugins, project instructions, workspace image or setup and dependencies,
  vendor-supported credential or subscription connection, host constraints, tools, isolation,
  recovery, interruption, and observation. Connection custody never implies copying raw user secrets
  into a Sandbox; scoped routing or injection remains adapter- and profile-specific.
- **Authority:** Current Core, workflow, and client ownership is defined only by the
  [North Star product boundary](north-star.md#product-boundary) and corrected by D075. A Harness
  remains authoritative for its native session, transcript, and tool protocol.
- **Composition:** Native workflows are Core dogfood and should use the same intended Core contract
  that ordinary clients and other products may later embed. They receive no privileged hidden path.
  Transport, SDK, and public compatibility promises remain uncommitted until real portability
  implementations and external-client use prove them. Dynamic agent-authored recipes remain a later
  UX layer; Dorf is not a generic automation canvas, graph framework, agent builder, or model/tool
  Harness.
- **Proof order:** Starting from Codex on Incus, D065 proves Pi as the second supported Harness on
  Incus. Next prove Codex on a second Sandbox provider, then cross Pi and that provider. The
  mechanical oracle is
  that common consumer and workflow code has no Harness- or Sandbox-specific branches beyond profile
  selection and capability admission.
- **Supersedes and refines:** Supersedes D062's research-workflow-first proof order. It also refines
  older second-workflow extraction gates, including D009, D047, and D061: a later workflow still adds
  its natural facts before common authoring seams are extracted, but workflow generality is not the
  next product proof. It does not authorize a generic API, provider matrix, plugin system, or
  marketplace.
- **Reconsider when:** A supported Harness cannot fit the AgentRun boundary, a second Sandbox cannot
  preserve the Job authority model, or real external-client use shows that profile selection and
  capability admission do not keep common code independent of Harness and Sandbox.

## D064 — Debian 13 is the shared supported-toolchain Sandbox baseline

- **Status:** Accepted profile baseline; combined Harness packaging refined by D066; second-provider
  template qualified by D067 — 2026-08-14
- **Decision:** The official x86_64 Sandbox profile uses an exact provider-native Debian 13 base
  identity and carries only a cross-repository workstation baseline: Python 3.14 with pip, Node 24
  LTS, pinned Go and uv, Git, the verified Harness executables, native C/C++ build tools, and common
  shell/archive/search utilities. npm is used only to install Harness packages during construction; npm, npx, Corepack, Yarn, pnpm,
  pytest, Ruff, application libraries, GitHub CLI, tmux, PostgreSQL, and Absurd are not shared image
  contents. Managed repositories retain their
  language versions and dependencies in ordinary project metadata and lockfiles and install them
  through `commands.prepare`.
- **Integrity:** One provider-neutral, versioned guest recipe is the complete tool and Harness
  construction authority and copies no host state into the fresh base. Provider packaging supplies
  and records its exact Debian identity: Incus uses the resolved `images:debian/13` VM fingerprint;
  E2B uses an exact `linux/amd64` OCI manifest digest. The guest profile metadata records that base
  identity, every installed bootstrap-tool version, each Harness npm integrity, and verified Node,
  Go, and uv archive digests; provider artifact manifests bind the exact build to the recipe and
  source. Provider release/profile proof verifies the resulting artifact and reconciles its
  disposable Sandbox cleanup.
- **Why:** A small polyglot bootstrap lets the selected Harness inspect and deterministically prepare ordinary Python,
  JavaScript, Go, and native-extension repositories without turning their dependency graphs into a
  Dorf release concern. Debian 13 supplies the current stable/LTS runway. Keeping project packages in
  the repository avoids cross-repository version conflicts and preserves the development-tooling
  seam required by D049. Each provider retains its natural packaging path while consuming the same
  recipe: Incus publishes a configured VM instance, while E2B snapshots a template built from an
  exact OCI base. Packaging identity does not enter the shared runtime contract.
- **Cost:** The shared image and its proof surface are larger than the earlier harness-only image, and
  Python, Node, Go, uv, and Harness support windows require deliberate image refreshes. The supported
  clean-machine host remains Ubuntu 24.04; this decision changes the disposable Sandbox guest only.
- **Reconsider when:** Measured image transfer or cold-start cost dominates Job latency, a bootstrap
  tool has no cross-repository consumer, Debian prevents a real managed-repository terminal, or
  repository preparation repeatedly needs another tool whose inclusion is cheaper and safer than a
  content-addressed setup cache.

## D065 — Pi is the second Harness and reuses Dorf's scoped Provider Gateway

- **Status:** Accepted; second-Harness coding-to-PR terminal proven; image packaging refined by D066 — 2026-08-13
- **Decision:** Pi, distributed as `@earendil-works/pi-coding-agent`, is the deliberately selected
  second Harness for the D063 portability sequence. Its Incus profile uses the shared Debian 13
  image while runtime selection starts Pi rather than Codex. A Sandbox-resident Pi RPC process owns the live native session;
  the native Pi session maps to a Dorf Thread, each accepted RPC prompt starts a Dorf Turn, and Pi's
  queued steering user entries remain within their target Dorf Turn. The RPC process survives
  host-side Worker loss and exposes explicit prompt acceptance, settlement, follow-up, and steering
  operations. Profile selection is a startup choice; common workflow and consumer code remain
  Harness-independent.
- **Connection custody:** Existing named Provider Connections, including the owner's ChatGPT
  subscription connection, remain under Dorf's Provider Gateway. The Pi Sandbox receives only the
  same Job- and Sandbox-scoped route credential used by Codex profiles and addresses that route as an
  OpenAI Responses provider. Dorf does not copy Pi or ChatGPT OAuth bundles into the image or
  Sandbox.
- **Proof boundary:** The accepted evidence currently covers image construction, route creation, one
  clean initial no-change AgentRun, native Thread and Turn observation, and SIGKILL recovery after
  submission but before durable binding with exactly one native user Turn, followed by route
  revocation and Sandbox cleanup. One follow-up Message is also proven to append exactly one native
  user Turn to the same Pi session. The resident RPC transport is proven for one clean initial
  no-change AgentRun with an explicit successful prompt response and final `agent_settled` event.
  Controller loss after an accepted RPC prompt is proven to recover the same native Turn without a
  duplicate prompt. Active-Turn steering is proven to wake the durable workflow, acknowledge the
  exact target Turn, persist its queued native user entry inside that logical Turn, settle both the
  target and steer AgentRuns, and retain an unchanged exact-Revision observation before route and
  Sandbox cleanup. Isolated review is proven on a committed exact Revision: deterministic policy
  selected one general Role, its dedicated Sandbox ran Pi with only `read`, `grep`, `find`, and `ls`,
  the checkout remained clean and immutable, review-observation Evidence was retained, and ordinary
  feedback returned to and settled through the implementation Thread. The coding-to-PR terminal is
  proven by a Pi implementation commit, exact-Revision Checks, that isolated review loop, an
  unchanged feedback follow-up, scoped branch push, and exact open GitHub Proposal. Pi is the first
  verified second Harness for the current Incus coding workflow profile.
- **Why:** Pi's documented RPC mode is its headless JSON protocol for embedding from a non-TypeScript
  control plane. It supports the required OpenAI Responses transport and lets Dorf test Harness
  portability without adding a TypeScript SDK sidecar or another provider-custody system. D066 owns
  the later decision to package both credential-free Harness executables in one image.
- **Reconsider when:** Pi cannot preserve no-duplicate recovery or required intervention semantics,
  its native session format cannot remain authoritative for observation, or a smaller supported
  integration proves the portability boundary with less profile-specific machinery.

## D066 — One credential-free image carries both verified Harnesses

- **Status:** Accepted and proven by the immutable v0.2.0 release — 2026-08-14
- **Decision:** Publish one Debian 13 Incus image containing exact Codex and Pi npm packages over the
  shared workstation baseline. `DORF_HARNESS` selects one runtime adapter; it does not select another
  image. The image contains no Harness authentication, Provider Route key, session, or configuration.
  Manifest schema 5 records both package versions and npm integrities and uses the exact assets
  `dorf-incus-vm-v5-x86_64.tar.gz` and `dorf-incus-vm-v5-x86_64.json`. The installer converges the
  neutral `dorf` alias on the manifest's immutable fingerprint.
- **Promotion boundary:** One candidate fingerprint must complete separate real Codex and Pi
  no-change coding Jobs, including exact native Thread/Turn binding, Revision Evidence, route
  revocation, and Sandbox cleanup, before publication. The repository release command is the only
  procedure authority for that proof and publication.
- **Proof:** The immutable v0.2.0 release targets source commit
  `c9d597f21068bacf5650939781b5f2ad8d3b854d`; its signed GitHub
  attestation binds the manifest and archive, and the manifest records combined-image fingerprint
  `ea537b1b6d5aa503eb5a1728988f31d05ce37984635303f1eae5bf3640748781`. Separate
  Codex and Pi Jobs completed against that fingerprint with exact Revision Evidence, route
  revocation, and Sandbox cleanup before publication.
- **Why:** The shared Debian and cross-repository toolchain dominate image size. Duplicating that
  baseline for two small Harness packages increases release, download, storage, rollback, and
  operator-selection surface without adding isolation: the Job-owned Sandbox and scoped Provider
  Route remain the security boundaries, and only the selected Harness is started and configured.
- **Supersedes and refines:** Supersedes D065's separate-image packaging choice, refines D038's
  schema-4 Codex artifact identity, and leaves D064's shared Debian/toolchain baseline intact.
- **Reconsider when:** Co-installation causes measured dependency conflicts, materially expands a
  Harness-specific attack boundary, prevents independent security updates, or separate delta images
  become cheaper than one shared artifact in real release and installation evidence.

## D067 — E2B is the next Sandbox portability proof target

- **Status:** Accepted; complete second-provider Codex and Pi coding-to-PR terminals proven —
  2026-08-14
- **Decision:** Pursue E2B as D063's second Sandbox provider one earned slice at a time. The first
  implementation is a narrow native Go control-plane client for one create attempt, exhaustive exact
  ownership discovery across running and paused Sandboxes, individual-resource attestation, and
  ownership-guarded deletion. The second implementation vendors one pinned upstream process schema,
  generates its protobuf and Connect-Go client, and adds only Dorf's required argv, stdin, raw output,
  terminal-status, timeout, cancellation, and explicit-kill semantics. It consumes a configured
  prebuilt template reference and does not build templates, transfer files, expose a general provider
  registry, or enter common workflow code. E2B's official JavaScript SDK remains a behavioral oracle,
  not a runtime sidecar or a second Dorf control plane.
- **Common seam:** Incus, E2B, and any later Sandbox provider must satisfy one provider-neutral
  Sandbox contract selected by the admitted profile, just as Codex and Pi satisfy one Harness
  contract. That contract preserves Dorf's stable ownership identity while treating the provider's
  resource locator as opaque; it separates one mutation attempt from read-only reconciliation,
  exposes the bounded command and authenticated-endpoint primitives proven by real consumers, and
  keeps deletion ownership-guarded. Provider-specific lifecycle APIs, envd, Incus commands,
  networking, and connection headers remain inside adapters. Capability admission may reject an
  unsupported Harness/provider combination without adding provider branches to workflow or common
  consumer code. Extract the exact interface only after the E2B endpoint proof completes the second
  implementation's required surface; do not build a provider registry or speculative capability
  matrix.
- **Extracted seam:** `internal/sandbox.Sandbox` is the single contract used by terminal,
  repository, publication, Codex, and Pi. Every execution, endpoint, review, repository, and cleanup
  call carries the complete Dorf ownership tuple; common code never guesses an E2B provider ID or
  relies on the Dorf Sandbox name without its nonce. Incus resolves its deterministic instance while
  E2B exhaustively resolves one opaque provider locator from exact metadata. Authenticated endpoints
  expose separate guest-bind and controller-dial URLs plus cloned provider headers without exposing
  their capability in formatting. Provider-route reachability is also adapter-owned: Incus validates
  the exact configured private bridge, while E2B requires one exact HTTPS `/v1` URL and defaults to
  allowing only that hostname over otherwise denied egress. A selected repository profile may
  explicitly admit general internet egress. Tunnel lifecycle remains deployment-owned. A
  compile-time implementation assertion covers both adapters and an
  import-boundary test rejects Incus or E2B imports from common consumers. Startup selects exactly
  one configured profile and injects its adapter through the common seam. Admission durably pins the
  Job's Sandbox profile; a worker configured for another profile leaves ordinary work or cleanup in
  observable attention before any provider call. This is a scalar authority fence, not a provider
  registry. The admitted E2B profile supports both Codex and Pi without provider branches in shared
  consumers. The proved coding-to-PR terminals do not make the disposable-tunnel profile a supported
  deployment.
- **Recovery boundary:** E2B create has no caller-selected resource ID or documented idempotency key.
  Dorf therefore attaches its Job ID, durable Sandbox ID, and ownership nonce as provider metadata.
  A create request is never automatically replayed after possible dispatch. Reconciliation
  exhaustively queries the provider by that exact identity: one match is adoptable, no match remains
  absent or uncertain according to the caller's durable Action state, and multiple matches require
  attention. Provider-generated IDs remain opaque locators. Cleanup re-attests exact ownership before
  deletion and confirms absence separately.
- **Proof:** A live Hobby-account test let E2B accept a restricted Sandbox create and then discarded
  the successful response locally. The Go client recovered the single resource by exact ownership
  metadata, inspected it by its opaque provider ID, deleted it, and confirmed no ownership match
  remained. Hermetic tests cover the same lost-response path, pagination, duplicate ownership,
  foreign-resource deletion refusal, stale locators, idempotent absence, and provider errors without
  retaining E2B's returned access tokens. A second live test used native binary ConnectRPC against
  envd 0.6.13 and proved literal argv, binary stdin, separate raw stdout and stderr, exact nonzero
  exit status, provider process timeout, caller-cancellation ambiguity, and one-shot PID cleanup. One
  shared guest recipe then produced exact E2B build
  `dorf-debian13-combined:8bb1103a-c331-4098-9dac-b26a2ed31eae` from clean source
  `6e5801b029608656f5b6c2076032befafed4b990`; the native Go adapter qualified its Debian 13 amd64
  identity, envd 0.6.13, complete tool inventory, Codex 0.147.0, Pi 0.84.1, root workspace, and
  credential absence before ownership-guarded deletion. The account had no running or paused
  Sandbox afterward. An authenticated endpoint proof then separated Codex's `ws://0.0.0.0:4500`
  process bind from E2B's provider-resolved `wss` URL, restricted public ingress, and required both
  E2B's scoped traffic header and Codex's independent control capability. After abrupt controller
  loss, a second connection resumed the exact native thread and observed the exact submitted turn;
  no upstream model outcome was claimed. Every proof Sandbox, including failed iterations, was
  ownership-deleted and final account inventory was empty. After extracting the common seam, the
  live proof was rerun entirely through `internal/sandbox.Sandbox`: it reconciled creation without a
  provider locator in Codex, executed the controller commands, resolved the authenticated endpoint,
  recovered the exact thread and turn after abrupt loss, ownership-deleted the resource, and observed
  it absent. The next live proof kept the local broker on its private Incus bridge and placed a
  disposable outbound HTTPS tunnel in front of it. E2B denied all egress except that exact hostname;
  an unauthenticated request from the Sandbox returned 401, common `terminal.Externals.RouteCreate`
  installed only the scoped route key and external URL, and an unchanged Codex Harness completed a
  real `gpt-5.6-sol` turn through the owner Gateway. Cleanup revoked the exact route before removing
  its Sandbox files, ownership-deleted the Sandbox, observed zero matching E2B resources and zero
  proof routes, and stopped the tunnel. No tunnel hostname, route key, E2B key, or upstream credential
  is durable repository state.
  Provider/profile composition then admitted E2B for one durable no-change Codex Job. The common
  workflow created one E2B Sandbox, cloned the exact remote Revision, completed repository setup,
  installed the scoped route, bound and completed one real Codex thread and turn, and recorded exact
  unchanged Git-revision Evidence. The first delivery attempt exposed a missing `sandbox_id` in the
  PostgreSQL delivery projection; after adding that durable field, the same Job reconciled the
  existing Sandbox and route rather than replacing either. Explicit cleanup recorded route revoke
  before ownership-guarded Sandbox delete, completed its Absurd task, and an independent inventory
  check found zero exact E2B matches and zero Gateway routes. The disposable tunnel was then stopped.
  The Job used explicit general internet egress because clean repository setup follows package and
  redirect hosts that are not a stable static allowlist; the model route remained consumer-scoped.
  Two modifying Jobs then completed the remaining terminal. The first produced exact Revision
  `3183e618329abd72f0697fd3a9f83e86a94dc91d`, passed both repository Checks, correctly selected no
  agent review for a documentation-only change, published PR #134, observed its closed rejection,
  and cleaned its route and Sandbox. The second produced exact Revision
  `764f9312dd217b149e393c0ae1f31696894f4174`, passed both Checks, and selected one general reviewer.
  A separately owned E2B Sandbox received the exact checkout by Git bundle, ran Codex with immutable
  read-only capability, and returned no-material-issue feedback with Revision-bound review Evidence.
  The original implementation thread accepted that feedback without changing the Revision, Dorf
  published PR #135, observed merge commit `f6961465f89b383d3ae0e84a207f64c4f2fba925` as an accepted
  Outcome, then revoked each route before ownership-deleting its main and reviewer Sandboxes. Both
  Absurd tasks completed, an independent E2B metadata query found zero Job-owned running or paused
  Sandboxes, the Gateway route inventory was empty, and the disposable tunnel stopped. No provider,
  route, tunnel, GitHub, or upstream credential entered durable evidence or repository state.
  Detailed observations remain in the
  [E2B capability proof](../research/e2b-capability-proof.md).
  A later live E2B Pi Job completed implementation and isolated-review AgentRuns in separately owned
  Sandboxes, exact-Revision Checks, host-side publication, a terminal Outcome, and cleanup through the
  same common workflow. D068 records the exhausted-attempt recovery observed during that terminal.
- **Next earned boundary:** Choose and prove a stable deployment-owned tunnel/domain only when the
  operational E2B profile is ready. Do not add files, snapshots, waiting-Sandbox lifecycle
  optimization, a provider registry, or a capability matrix until a concrete workflow earns it.
- **Why:** E2B passed the bounded VM, Docker Compose, browser, authenticated endpoint, pause/resume,
  network, and cleanup capability spike without a fixed subscription. A handwritten lifecycle
  client keeps Dorf's reconciliation policy visible and small; adopting the full high-churn
  JavaScript SDK through a sidecar would add another protocol and process boundary before command
  streaming has proved that cost necessary.
- **Reconsider when:** The native command protocol cannot remain smaller than a maintained sidecar,
  E2B cannot support Dorf's no-duplicate recovery or scoped Provider Gateway route, managed-service
  cost or reliability fails dogfood, or another provider reaches the complete D063 coding-to-PR
  terminal with materially less authority overlap.

## D068 — Explicit Job retry rearms the current failed Absurd task once

- **Status:** Accepted operator ergonomics, extended by cleanup dogfood — 2026-08-19
- **Decision:** `dorf retry JOB_ID` is a thin operator command for the Job's latest attached Absurd
  task. Dorf records task handoffs as one append-only ordered attachment relation rather than named
  main/cleanup task columns; any future task handoff therefore becomes the retry target without new
  selection logic. Dorf calls Absurd's public in-place retry without overriding `max_attempts`, which
  atomically adds exactly one bounded attempt to the same task and retains its checkpoints. It never
  spawns a replacement task, mutates execution facts, resumes a Sandbox directly, or copies task
  attempts and checkpoint state into Dorf tables. Absurd atomically refuses a missing or non-failed
  task. Sandbox-profile admission remains the worker's responsibility when it claims the new run.
- **Truthful receipt:** The command returns only the Job, task, new run and attempt identities and says
  that retry was scheduled. Current work and continuation remain ordinary inspection projections;
  they become true only when a matching worker runs and new progress is observed.
- **Why:** A real E2B and Pi coding Job exhausted its automatic attempts during a host IPv4 outage
  after durable work had already reached isolated review. Calling Absurd's public retry on the same
  task scheduled attempt 6; the worker recovered the existing Job, Sandboxes, native harness state,
  and workflow facts, then completed review, publication, a terminal Outcome, and cleanup. The recovery
  proved the boundary, while requiring operators to construct a one-off Go client made the ordinary
  repair path unnecessarily obscure. Later investigation dogfood found that the main-task-only mapping
  left an exhausted cleanup task without a repair path even though the same Absurd primitive applied.
- **Refines:** D048's public-Absurd-API boundary and D054's earlier decision not to build a separate
  publication retry mechanism. Absurd still owns retry eligibility, attempts, runs, and checkpoints.
- **Reconsider when:** Different task classes need distinct operator policy, one-attempt rearming is
  insufficient in measured operations, or Absurd exposes a safer first-class repair receipt that
  should replace Dorf's thin projection.

## D069 — Codebase investigation is the second explicit native workflow

- **Status:** Accepted initial implementation slice; interaction boundary refined by D075 — 2026-08-17
- **Decision:** Add `codebase-investigation` as a clean workflow identity, not a top-level
  `investigate` feature and not a generic researcher. One admitted Job pins the exact workflow
  revision, repository Revision, unstructured brief, execution profile, Provider Connection, model,
  and reasoning envelope. A workflow may declare optional broad provider primitives beyond Dorf's
  baseline Sandbox and Harness contracts; admission and Worker claim reject missing primitives
  before a Sandbox call. Repository dependencies remain the responsibility of its setup script or
  custom image. The current investigation needs no optional provider capability.
- **Workflow facts:** The workflow has one ordinary Go coordinator over its own dependency chain:
  main Sandbox create, exact repository checkout, scoped Provider Route, one `investigate` AgentRun
  per accepted initial or follow-up Message, unchanged-checkout verification, and draft recording.
  Each draft points to an immutable `text/markdown` Artifact and remains flexible Markdown grounded
  in repository paths and lines. A draft may plainly state that no useful finding exists; there is
  no synthetic Outcome enum or machine-readable first-line marker. Agent prose remains a result, not
  proof of its claims.
- **Shared seam:** Jobs now durably pin workflow name and revision. Investigation reuses Job custody,
  exact Sandbox Actions, runner-neutral AgentRun reconciliation, content-addressed blob storage,
  profile fencing, and route-before-delete cleanup. Coding and investigation retain separate
  coordinators, snapshots, natural facts, and operation projections. There is no workflow registry, JSON result bag,
  DAG, provider matrix, or workflow DSL.
- **Client boundary:** `dorf workflow run codebase-investigation` is the first CLI projection of the
  workflow-as-durable-function boundary. The workflow itself remains independent of whether a future
  caller is a CLI, schedule, GitHub event, Slack tag, assistant, or another product.
- **Deliberate omissions:** This initial slice had no repository setup, coding Checks, branch mutation,
  review, GitHub authority, publication, external web sources, cron scheduling,
  automatic Job chaining, or persistent researcher identity. Approval and composition remain outside
  this workflow according to the North Star product boundary; later use must earn each additional
  capability.
- **Proof:** Unit coverage proves the independent operation order, flexible nonblank draft edge, and
  optional provider-capability diagnostics. PostgreSQL integration proves immutable workflow admission,
  cross-workflow idempotency conflict, the investigator Role/capability binding, draft Artifact
  attachment, one full fake-Harness draft loop, and explicit shared route-before-Sandbox cleanup.
  Live Codex dogfood produced two same-Thread Markdown drafts and exposed the misplaced
  decision-specific cleanup coupling. The corrected client-requested cleanup terminal remains a D075
  proof follow-up.
- **Why:** Dorf needs concrete workflows that improve its own development and demonstrate what Core
  enables. A repository-grounded investigation differs materially from coding-to-proposal because it
  owns no Revision mutation, Checks, review, Proposal, or GitHub Outcome. That difference is enough
  to expose workflow identity, optional provider-capability admission, Artifact-backed drafts, and role-neutral AgentRun
  observation without generalizing the coding coordinator.
- **Reconsider when:** Live dogfood shows useful investigations require captured external sources or
  deterministic reference validation, another workflow repeats
  the same definition/admission/inspection code with identical authority, or a remote client proves a
  smaller public application boundary.

## D070 — Named Sandbox profiles pin exact artifacts and require Dorf verification

- **Status:** Accepted incremental profile-management slice — 2026-08-18
- **Decision:** PostgreSQL owns named Sandbox profiles. A profile binds one provider, exact provider
  artifact, Harness, provider networking and lifecycle settings, and Dorf verification receipt.
  Provider credentials and host paths remain deployment configuration and never enter the profile.
  One verified profile may be selected explicitly per Job; omission resolves the one deployment
  default. The Job durably pins the profile name. Workers resolve that name through the composition
  root into the existing provider-neutral Sandbox and Harness contracts, so workflows contain no
  Incus, E2B, or Harness selection branches.
- **Artifact boundary:** `profile create` adopts an existing provider artifact; it does not build one.
  Incus aliases are resolved to an exact image fingerprint before persistence. E2B profiles require
  an exact template-build reference. `profile install` is the official Incus release convenience and
  creates the same ordinary profile after verified import. Bring-your-own artifacts receive no Dorf
  provenance or security attestation merely by being admitted.
- **Verification boundary:** Dorf owns one mandatory, versioned `base-1` functional probe before a
  profile may become default or admit a Job. The explicit, potentially billable operation reconciles
  one durably owned disposable Sandbox, verifies a writable workspace, the baseline atomic file
  operation, `bash`, `git`, `rg`, and the selected Harness/version, then ownership-deletes the
  Sandbox and confirms absence. Its stable
  ownership tuple and typed receipt make process-loss recovery converge through cleanup rather than
  leak a proof resource. Repository-specific dependencies remain the repository setup or custom
  artifact's responsibility. Optional capabilities stay broad and are added only when an actual
  workflow requires them.
- **Mutation rule:** `profile update` applies only explicitly supplied fields to the latest locked
  definition; provider changes require a new named profile. A changed profile may be updated only
  while no Job using its name has incomplete cleanup, and the change clears its default and
  verification receipt, forcing explicit re-verification. A no-op patch preserves both. Exact
  profile revisions are deliberately deferred; immutable-while-in-use is sufficient until measured
  operations require historical profile definitions beyond completed Jobs.
- **Proof:** Live E2B dogfood preserved the exact verified definition on a no-op, invalidated only a
  changed Gateway field, failed setup until re-verification, then restored and investigated a
  retained unpublished commit after its source checkout was removed. Cleanup left zero matching E2B
  Sandboxes. The investigation caught an update resetting public `created_at`; the SQL now preserves
  creation time and PostgreSQL integration guards it.
- **Supersedes:** D067's process-wide `DORF_SANDBOX_PROFILE`/`DORF_HARNESS` selection and
  differently-configured-worker fence. The durable Job fence is now the named profile itself, and
  every Worker may recover Jobs across configured Incus and E2B profiles when their deployment
  credentials are available.
- **Why:** Provider kind alone did not identify the exact image, Harness, network, or verified
  runtime contract, and process-wide selection prevented one deployment from truthfully recovering
  Jobs on different providers. Named profiles improve operator UX and retain the narrow adapter seam
  without introducing a provider matrix, plugin registry, image builder, or fine-grained tool claims.
- **Reconsider when:** A real workflow earns browser, nested-container, served-endpoint, snapshot, or
  GPU admission; completed Job audit needs immutable historical profile definitions; or a provider
  cannot support the stable verification ownership and cleanup contract.

## D071 — Setup converges one Docker PostgreSQL deployment

- **Status:** Accepted portability and bootstrap slice, simplified to one setup path — 2026-08-19
- **Decision:** Keep Dorf as a native Go binary with PostgreSQL as its external durability authority.
  The supported ordinary deployment has one database shape: Dorf-owned Docker PostgreSQL. There is
  no backend selection, native PostgreSQL installer, Compose wrapper, or separate host-install
  command. `dorf setup` is the one convergent entry point.
- **Docker custody:** Dorf owns one labeled `dorf-postgres` container and one labeled persistent
  `dorf-postgres-data` volume. It uses the reviewed PostgreSQL 17.10 Bookworm image tag, retains and
  re-attests the exact resolved image identity, binds the database only to loopback, stores its
  generated credential in the mode-0600 deployment record, starts a stopped owned container, and
  refuses same-named resources without exact Dorf ownership or configuration. The host Docker socket
  remains control-plane authority and is never exposed to a Sandbox or AgentRun.
- **Bootstrap:** Setup first observes Docker, Incus, QEMU, services, and required group access. If
  supported Ubuntu 24.04 changes are missing, one Huh prompt renders directly from the exact plan
  that will be applied; `--yes` approves that same plan for automation. A new login is reported only
  when newly granted group membership cannot affect the current process. Already-ready non-Ubuntu
  hosts are accepted, but Dorf does not mutate their packages. Setup then reconciles PostgreSQL and
  migrates Absurd and Dorf storage before profile or Provider Connection validation. With no
  Provider Connection supplied, it truthfully reports the next profile and connection operations;
  a later `dorf setup --provider NAME` proves complete readiness.
- **Deliberate omission:** `DORF_DATABASE_URL` remains the existing advanced and test override, but
  there is no database-provider registry, native database path, Compose project,
  external-database command, database migration command, or automatic backend conversion. Those
  require measured deployment needs.
- **Why:** Clean Omarchy dogfood showed that an otherwise portable binary could not begin because
  PostgreSQL was absent, while adding distribution-specific package installation would expand host
  support without improving reproducibility. Docker supplies one consistent local PostgreSQL
  environment across already-Docker-capable hosts without containerizing Dorf or compromising its
  direct Incus and host authority boundaries.
- **Reconsider when:** Docker is absent on a supported non-Ubuntu host with real users, a remote
  deployment needs first-class database configuration, or container lifecycle or backup
  requirements exceed this bounded local custody.

## D072 — Workflow deliverables are first-class Artifacts

- **Status:** Accepted investigation-result retrieval slice — 2026-08-19
- **Decision:** Add Artifact as the durable, immutable, named workflow-deliverable primitive. A
  workflow-specific typed result points to Artifact IDs; clients list Artifacts by Job and retrieve
  exact bytes by Artifact ID. Artifact metadata lives in PostgreSQL and its bytes share one neutral
  deployment-owned content-addressed blob store with Evidence. Sandbox cleanup does not remove
  Artifacts.
- **First slice, refined by D075:** `codebase-investigation` records numbered Markdown draft
  Artifacts instead of misclassifying agent prose as Evidence. A client decides how to consume a
  draft and when to request cleanup. `dorf artifact list JOB_ID` discovers
  deliverables and `dorf artifact get ARTIFACT_ID` writes exact bytes. Inspection links to retrieval
  but does not inline potentially large or binary content.
- **Boundary:** Artifacts are results or claims, not proof of their own correctness. Evidence remains
  immutable observed proof linked to the fact it supports. There is no generic result JSON bag,
  artifact publication policy, retention service, archive format, streaming API, or interaction-layer
  destination in this slice.
- **Why:** Live investigation dogfood produced a useful durable report but required extracting a
  workflow-specific JSON field. A named retrievable deliverable is the repeated client need, while
  “result” remains workflow-specific semantic meaning and Evidence has a stricter proof role.
- **Reconsider when:** Large or streaming deliverables exceed the bounded blob API, external object
  storage becomes deployment authority, or multiple producers require a richer typed ownership
  relation than the current AgentRun-produced Artifact.

## D073 — Local committed investigation sources are retained Git bundles

- **Status:** Accepted provider-neutral local-source slice — 2026-08-19
- **Decision:** `codebase-investigation` accepts exactly one source: a reachable remote repository
  with an exact full commit OID, or `--local-repo` with an optional revision that defaults to `HEAD`.
  Dorf resolves the local revision to an exact commit, rejects submodules and Git LFS pointers,
  creates a self-contained Git bundle capped at 128 MiB, durably stores its bytes by SHA-256 before
  Job admission, and binds digest, byte size, and Revision as one immutable workflow-specific source.
  Local index, tracked-worktree, and untracked changes are excluded and reported to the caller.
- **Execution:** A retained source projects a distinct `repository-restore` Action. Dorf reads and
  verifies the retained bytes, uses the common Sandbox `PutFile` operation to reconcile an exact
  temporary bundle, verifies it with Git, and converges a detached checkout at the admitted Revision before the
  investigator runs. An ownership marker distinguishes a retryable partial restore from a foreign
  pre-existing checkout. Remote sources keep the existing `repository-clone` Action and use the same
  detached exact-Revision contract. Investigation has no synthetic branch.
- **File boundary:** `PutFile` reconciles one bounded byte slice at one clean absolute regular-file
  destination through an adjacent verified temporary file and atomic rename. Incus and E2B retain
  their private transports. The operation is safe to retry after an indeterminate response, but it
  does not claim streaming, directory transfer, mounts, stat/list, or provider filesystem parity.
- **Authority:** A retained source is accepted input custody, not an Artifact, Result, or Evidence.
  Artifacts remain workflow-produced deliverables; Evidence remains observed proof. A crash after
  blob retention but before admission may leave an unreferenced deduplicated blob, while every
  admitted Job is independent of the original host checkout.
- **Why:** Investigation dogfood against an unpublished local commit otherwise required pushing it
  to a remote authority. The existing review handoff also wrote bundles directly through command
  stdin; the earned atomic file primitive removes that unsafe destination write without turning the
  Sandbox contract into a speculative filesystem API.
- **Reconsider when:** A real input exceeds the bounded in-memory transport, a provider-native
  streaming upload materially reduces memory or protocol risk, submodules or LFS become required,
  or another workflow earns a more general typed input-blob relation.

## D074 — Investigation drafts wait for exact human disposition

- **Status:** Superseded before release by D075 — 2026-08-20
- **Retained finding:** Numbered draft Artifacts, follow-up AgentRuns, and the exact Harness Thread are
  useful reusable mechanisms. Immediate cleanup after the first draft destroys valuable revision
  context.
- **Superseded finding:** Persisting accept/reject decisions and letting them choose cleanup moved
  interaction policy into the wrong layer. D075 is the current authority; Git history retains the
  discarded design details.

## D075 — Core mechanisms do not own workflow or interaction policy

- **Status:** Accepted and implemented — 2026-08-20
- **Decision:** Make the [North Star product boundary](north-star.md#product-boundary) the sole current
  authority for Core, workflow, and client ownership. This entry records the correction and its
  rationale rather than restating that contract.
- **Investigation correction:** `codebase-investigation` produces immutable numbered draft Artifacts
  and accepts follow-up Messages on the same Harness Thread. It does not persist accept/reject
  decisions or infer cleanup timing. Agent0, n8n, a UI, another workflow, or a human-operated client
  may consume a draft, create an Issue, chain work, request another draft, record its own disposition,
  and finally request Dorf cleanup.
- **Coding distinction:** `coding-to-proposal` may interpret GitHub Proposal observations and request
  cleanup because that is workflow policy. Those facts and choices remain outside Core; compiling
  the workflow into Dorf grants no privileged authority or hidden execution path.
- **Refines:** D063's Core authority, D069's investigation terminal, D072's first Artifact consumer,
  and D074's human disposition. It preserves D074's same-Thread revision loop while removing its
  decision authority.
- **Why:** Maintainer-radar dogfood initially encoded accept/reject as a typed investigation decision.
  That made a useful client policy look like a Core requirement and contradicted the intended role of
  workflows as ordinary Core consumers. The reusable primitives are drafts, Messages, AgentRuns,
  retained Harness context, and requested cleanup—not the meaning a caller assigns to a draft.
- **Proof:** The decision table, domain type, CLI command, workflow wake wrapper, automatic cleanup
  branch, and decision rendering are deleted. Unit and PostgreSQL integration coverage prove that
  numbered drafts remain open for same-Thread follow-up, explicit cleanup closes admission and
  schedules shared route-before-Sandbox cleanup, and retained Artifact bytes survive it. Live Incus
  dogfood produced two drafts on one Codex Thread, exposed only follow-up or cleanup while open,
  accepted an explicit cleanup request, confirmed the exact Sandbox absent, and retrieved both
  Artifact digests afterward.
- **Reconsider when:** A reusable custody mechanism cannot support multiple real workflows without
  knowing their terminal policy. A single workflow needing a typed decision is not sufficient; that
  decision remains workflow-owned.

## D076 — Core Jobs and workflow inputs have separate durable types

- **Status:** Accepted first Core/workflow separation slice — 2026-08-20
- **Decision:** Keep `dorf.jobs` limited to shared custody: identity, bounded goal, pinned workflow
  and Sandbox profile, execution attachment, attention, and cleanup lifecycle. Store coding
  repository, starting/current Revision, branch, GitHub authority, and selected setup in
  `coding_to_proposal_inputs`. Store investigation repository, exact Revision, and remote-or-retained
  source identity in `codebase_investigation_sources`. Admission uses explicit typed inputs for each
  workflow; there is no generic input JSON, nullable workflow column set, or compatibility facade.
- **Execution:** Coding continues to own a mutable branch and Revision line. Investigation owns a
  clean detached checkout at one exact Revision and never fabricates a branch. Shared Sandbox,
  AgentRun, route, Artifact, attention, task attachment, and requested-cleanup custody are unchanged.
- **Why:** The first coding workflow had placed its repository and GitHub assumptions on the shared
  Job row. The second workflow proved those were not Core facts and that retaining them would force
  false branches and coding authority onto unrelated work.
- **Proof:** PostgreSQL constraints and integration coverage reject cross-workflow facts, preserve
  complete-input idempotency, admit branchless remote and retained investigations, and keep coding
  revision/setup/publication behavior typed. The full SQL generation/vet, Go unit/integration, and
  vet contract passes against a recreated prototype baseline.
- **Reconsider when:** Another workflow needs one of these facts with the same authority and recovery
  meaning. Repeated field names alone are insufficient reason to move it into Core.
