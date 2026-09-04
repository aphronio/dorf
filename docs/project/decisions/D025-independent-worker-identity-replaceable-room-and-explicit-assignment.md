# D025: Independent Worker identity, replaceable Room, and explicit Assignment

- **Applicability:** historical
- **Areas:** product, core, persistence
- **Read when:** Reviewing the replaced durable Worker, Room, Job, and Assignment model.
- **Decision history:** Superseded by D047 — 2026-08-06
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
