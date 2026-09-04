# D031: Buzz relay is the first remote client transport; ACP remains a separate seam

- **Applicability:** historical
- **Areas:** interaction, client-api, deployment
- **Read when:** Reviewing the superseded Buzz relay transport and coordinator design.
- **Decision history:** Superseded by D034 after the application-boundary review — 2026-07-28
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
