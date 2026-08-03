# Dorf North Star — Workers, Rooms, and Jobs

Durable remote workers in owned sandboxes; AFK capacity and coding-to-PR are showcases, not the core.

This document is the approved product north star for the **agent runtime / control plane building
block**. It paints the experience we are aiming at: agent-first, human-smooth, calm under failure.
The direction, vocabulary, and experience target are accepted; the implementation remains
aspirational. This is **not** an API spec, package inventory, issue backlog, or license to build
speculative platforms.

| Current implementation | Approved product direction |
| --- | --- |
| [runtime.md](runtime.md) — portable contracts that exist | Experience, metaphors, ideal loop |
| [decisions.md](decisions.md) — concrete choices already made | Worker, Room, Job, and shared control verbs |
| [principles.md](principles.md) — how we build | Why the control plane should feel like an office |

**Accepted vocabulary, refactorable implementation.** Worker, Room, Job, Assignment, and the typed
shared verbs below are how we talk about and expose the primitive. Environment remains the internal
Room adapter seam; native turns remain harness operations. When implementation reveals a conflict,
discuss it and record any deliberate change rather than silently diverging from the north star.

---

## One sentence

Dorf is a local-first runtime for durable workers: summon them into rooms, assign jobs, visit
anytime, never lose the thread, never babysit by default.

The primitive is not “launch a harness process in an environment.”
The primitive is: **a living worker with a desk, a job, and a doorbell.**

---

## What this north star is for

### Scope

This is the north star for **L1 — the runtime control plane**. Everything else hangs off that.

```text
L0  agent harnesses      Codex, Pi, Claude Code, OpenClaw, …     (not Dorf)
L1  runtime control      workers, rooms, jobs, remote verbs      ← THIS DOCUMENT
L2  showcase apps        coding-to-PR now; AFK capacity later    dogfood / proof
L3  later composition    boards, fleets, org-style orchestration only after L1 is excellent
```

| This north star is | This north star is not |
| --- | --- |
| The goal for the **agent runtime / control plane** | A product vision for coding-to-PR as the company |
| The building block other apps compose | An org-chart / “AI company” product (Paperclip) |
| Agent-first and human-smooth remote control | A vendor coding-agent cloud clone |
| Direction for best-in-class DX on this task | Permission to ship multi-agent fantasy before the loop works |

The layer rule that keeps this document honest:

> **The runtime owns mechanisms, recording, and rendering. Workflows own semantics, policy, and
> verification.**

Coding-flavored examples below (checks, benches, PRs) illustrate mechanisms; the semantics they
imply belong to the showcase layer. Workflow-layer ideals that compose on top of this control plane
(contract-first acceptance, verification ladders, autonomy labels, calibration) live in
[showcase-ideals.md](showcase-ideals.md), not here. Build bottom-up: the best building block first,
the coding workflow dogfooded on top.

### Coding-to-PR and AFK are showcases

**Coding-to-PR** is the current dogfood application. It pressures the runtime with a real vertical
slice and proves the block under daily use. It must **not** define the runtime API. Runtime surfaces
should not speak branch, PR, review, check policy, or GitHub. Those belong in the workflow layer.

**AFK personal capacity** (many parallel jobs, review on your schedule, developer as scheduler) is
the natural showcase once the building block is excellent. It is product composition on top of L1.
It is not a reason to weaken L1 or to encode “fleet dashboard” concepts into the core.

If someone only ever used the runtime to run a research agent, a triage bot, or a long-lived
experiment in a room — with no PR in sight — the control plane should still feel complete.

---

## Core objects

Three resources need human-readable names; Assignment is their durable typed relation:

| Object | Feels like | Is |
| --- | --- | --- |
| **Worker** | someone you summoned | durable harness identity + general conversation |
| **Room** | their private office | replaceable isolated machine/workspace boundary |
| **Job** | what you asked them to do | complete goal + documents + dedicated conversation |
| **Assignment** | who owns the Job now | immutable generation linking one Job, Worker, and Room |

```text
Worker ──current Room──> Room
   ▲
   └── Assignment ── Job
```

IDs exist for machines. Humans usually speak in Worker and Job names; exact Assignment identity is
carried at context, reporting, and lifecycle boundaries. A stranger’s mental model in ten seconds:

```text
Workers do Jobs in Rooms.
I can message or inspect a Worker and its Job independently.
If a Room or client dies, durable identity and admitted input remain.
```

`worker spawn` creates a Worker and Room without a placeholder Job. `job assign` atomically creates
a Job only with complete goal version 1, an Assignment, its dedicated conversation defaults, and the
first exact-goal input. Harness type belongs to Worker identity; model and reasoning defaults belong
to each conversation and may be overridden per turn.

### How the objects feel in practice

**Worker.** You do not “select adapter codex_app_server.” You summon someone who knows how to work.
Today that someone might be Codex; later Pi or another harness. The control plane cares that they
show up, accept jobs, can be messaged, and can be interrupted — not that you memorize protocol
details.

**Room.** A private office, not a shared host shell. Their files, processes, toolchains, and
accidents stay in the room. When you fire them and destroy the room, the office is empty — no
zombie disks, mystery ports, or leaked credentials on your laptop.

**Job.** The durable unit of truth. The doorbell on the office door. You talk to the job when you
want status. You do not hunt tmux panes, VM names, and chat threads and hope they still refer to the
same reality.

The approved representation direction has two deliberate surfaces. Human- and agent-readable Job
documents—the pinned goal and, as reviewed boundaries land, approved context, assumptions, claims,
and evidence—live in a directory that `grep`, `diff`, Git, editors, and other agents can consume.
Transactional operational state—lifecycle, bindings, message admission, and delivery—is SQLite-owned
and accessed programmatically rather than mirrored into competing files. Discarding a Job removes
both surfaces: zero ghost state made physical. GitHub stays the source of truth for the deliverable
(branch, commits, PR). Concrete schemas carry no compatibility claim.

---

## Building block done (near-term goal)

A stranger (or future you) can do the following **without caring about PRs**. This is the acceptance
bar for “L1 is real.” A harsher form of the same bar: the whole loop is completable from a phone,
with no terminal in reach. If this loop is excellent, AFK capacity is mostly composition. If this
loop is mediocre, no fleet vision and no coding workflow will save the product.

Optional thin epic: *Runtime building block: stranger loop*, with one issue per capability below.
Do not turn this whole document into a mega-epic of metaphors.

### 1. Summon — create worker + room

```text
you:  "Open a room for checkout-perf and put Codex on it."
sys:  Room ready. Worker online. Desk is clean.
```

No image-bake anxiety, no adapter-flag novel, no auth yak-shave on the default path.
Subscription and household auth are one-time setup (like connecting iCloud), not part of every spawn.

Under the hood this is still provision + bind. In the experience it should feel like unlocking an
office and finding someone already at the desk.

### 2. Assign — start detached work

```text
you:  "Make checkout feel instant. Prove it. Leave evidence."
sys:  Job accepted. Working. I’ll interrupt only when a decision needs you.
```

You are **not** dropped into a live token stream unless you ask. Default mode is async employment,
not pair-programming theater. Detached is the happy path; watching is cinema on demand.

Assigning also sets the job's seatbelts: which secrets enter the room, what the network may reach,
and which boundary crossings (publish, spend, destroy) the system may perform without asking.
Inside the room the worker is always fully autonomous; the dial only governs what leaves it.

When the worker would otherwise stall on a question, it does not stall. It picks a documented
default, records it in an **assumption ledger**, and continues; you review assumptions alongside
the evidence. Blocking is reserved for irreversible or boundary-crossing decisions.

### 3. Reconnect — another client, same reality

Phone, second laptop, another agent, a wall display — same doorbell.

```text
later: "How’s checkout-perf?"
sys:   Since you last looked: goal unchanged. Two milestones landed, one
       approach abandoned, one assumption recorded. Now verifying the fix.
       3 new artifacts. ETA ~12m.
```

There is no ritual of “which VM, which pane, which thread ID.” Opening the job **is** continuity.

The default reconnect view is not the full story but the **delta**: what changed since you last
looked. Beliefs revised, assumptions taken, artifacts added, state flipped. Timelines are for
archaeology; a returning manager wants the diff of the situation.

### 4. Steer — another turn, exactly once

```text
you: "Stop micro-optimizing React. Profile the API first."
sys: Acknowledged. Discarding current patch set. New plan pinned.
```

Messages are natural language. Each accepted client action has durable identity and FIFO position
within its own conversation, so client retry cannot fork the Job into competing realities.

Goal version 1 stays sticky. Ordinary messages cannot revise it. A future explicit goal-revision
operation must create a new immutable version and show the exact replacement before it sticks; the
current runtime deliberately has no automatic goal mutation.

### 5. Inspect — manager view, not debugger dump

One glance answers:

- what are they doing right now?
- what do they believe the goal is?
- what changed in the room?
- what evidence exists?
- what do they need from me?

Not logs first. **Situation first.** Logs behind a tap.

```text
checkout-perf · working
goal: make checkout feel instant
now:  running integration checks
need: nothing
evidence: 3
updated: 20s ago
```

One rule keeps this view honest: **claims are not facts.** “Running integration checks” is the
worker’s claim; “process alive, last command exited 0, 14 files changed” is what the control plane
observed. The pulse interleaves both and labels which is which, so a worker that is confidently
wrong about its own state cannot silently lie to the card.

### 6. Enter — take over without breaking the spell

```text
you: "Let me drive."
```

You are suddenly in the room: terminal, browser, editor, running services. The worker watches or
sleeps. When you leave:

```text
you: "Your turn — finish from here."
```

Identity never forks. There is no “human patch branch” vs “agent brain transplant.” Same job, same
room, same worker binding — presence just changed. The current local implementation enters the Room
at `/workspace`; Job work remains available under `/workspace/jobs/JOB`. Exiting the shell is leaving
the Room, so there is no separate detach command until a real remote forced-detachment need appears.

### 7. Recover — weather, not an incident

Laptop sleep, Wi‑Fi drop, process crash, host reboot:

```text
sys: Back. Job uninterrupted. Worker resumed at last safe point.
```

If the human client dies, the job does not die. If the worker process dies, the job still knows who
it was and where the room is. A cloud or otherwise remote Room keeps executing without the local
machine; a local Room naturally needs its host powered on, but its Job, Room, and Worker binding
recover after the host returns. Recovery is boring. Boring is the product.

### 8. Finish — keep or fire, no ghosts

```text
sys: Done. Evidence pack ready.
you: Keep room / freeze room / discard room.
```

- **Keep / freeze** — closed laptop lid; resume tomorrow without archaeology.
- **Discard / fire** — office emptied; no residue on disk, network, or credentials.
- Cleanup is retryable. “Destroyed” in the UI means destroyed in reality.

Direction to aim at: a room should be re-derivable from its recorded provisioning recipe plus the
job record, so freezing becomes an optimization, never a necessity.

---

## Ideal control surfaces

One Job object. Many lenses. Progressive disclosure staircase:

```text
pulse → timeline → evidence → room → raw harness
```

Beginners live on the left. Gods may open the right. Never force everyone through raw mode.

### Two channels: state and attention

Everything a job emits travels on one of two channels:

- **State is pushed, continuous, and free to ignore.** Workers keep a structured self-report
  current (doing, believed goal, unsure about, assumptions taken, next). Nobody asks for status;
  humans and agents read the latest report. Pulse and timeline are renderings of this channel.
- **Attention is discrete, scarce, and never ignorable.** An inbox holds only items that require a
  decision, each exactly once, each answerable in one tap, delivered under a user-set interruption
  budget (urgency tiers, batching, quiet hours).

The invariant: nothing on the state channel demands action, and everything demanding action is in
the inbox exactly once. Dashboards that mix the channels train people to babysit; this split is
what makes detached work calm.

### Pulse (default)

A living card — the glanceable truth:

```text
checkout-perf · working
goal: make checkout feel instant
now: running integration checks
need: nothing
evidence: 3
updated: 20s ago
```

Blocked is loud and actionable, not a silent stall:

```text
checkout-perf · blocked
need: prod API fixture or mock permission
[Mock it]  [I will provide]  [Skip and narrow scope]
```

Blocked is also rare: assumable questions become ledger entries, not stalls. A blocked ask is a
structured object (question, options, default, expiry). The runtime carries and renders it; what
the options mean comes from the workflow layer.

### Timeline

A readable story, not a raw token hose:

```text
14:01  accepted job
14:02  room provisioned
14:04  found N+1 in cart totals
14:11  patch + bench: p95 900ms → 120ms
14:18  checks red: flaky address validation
14:19  investigating flake
```

Entries are rendered from the pushed self-reports, not scraped from a token stream. Worker
self-estimates and actual outcomes land as ordinary entries, so later layers can compute how much
a worker’s claims have historically been worth.

### Evidence wall

Artifacts are first-class citizens, not chat attachments lost in scrollback:

- diffs and patches
- benches and profiles
- logs and failing command output
- recordings / screenshots when relevant
- the assumption ledger (“what I decided without you, and why”)
- “why I think this is done”
- commands the **system** already ran (so humans do not redo deterministic setup)

To the runtime these are artifacts with provenance: who produced each one, when, attached to which
timeline entry. Diff, bench, and check semantics belong to the coding showcase.

### Room view

Only when you care about the body:

- files changing
- processes alive
- services up
- network boundary
- what is installed
- seatbelts (secrets scope, write scope, egress)

### Presence

Who is in the office:

- worker active
- you attached
- another human watching
- a child worker visiting

Presence makes takeover and collaboration feel like a shared office, not process surgery.

### Raw harness

Break-glass only: native Codex/Pi stream, protocol guts, adapter diagnostics. Available, never the
default front door.

---

## DX bar — best in industry for this task

Best-in-class here does not mean “prettiest chat.” It means the body feels:

| Feeling | Means in practice |
| --- | --- |
| **Calm** | No fear of losing state when the laptop sleeps |
| **Fast** | Summon-to-first-progress without ceremony |
| **Clear** | Always know what matters now |
| **Forgiving** | Steer, retry, discard cheaply |
| **Intimate on demand** | Enter the room, hands on glass |
| **Invisible when not** | Work continues without you |
| **Agent-native** | Other agents use the same doorbell |
| **Owned** | Your rooms, your subscriptions, your boundaries |

### Buttery control principles

1. **Zero ghost state.** If the UI says done, failed, or destroyed, reality matches. No
   “status green, VM haunted.”
2. **Names over handles** in human flows. `checkout-perf`, not `sess_17f2…`, until you ask for IDs.
3. **Detached default, intimate on demand.** Async employment first; live stream is opt-in cinema.
4. **Progressive disclosure.** Pulse → timeline → evidence → room → raw harness.
5. **Sticky goal.** The Job goal is pinned and versioned. Side messages do not silently replace it,
   and a steer that changes the goal shows its interpretation before it sticks.
6. **Assume, don’t stall.** A question the worker can answer with a documented default becomes an
   assumption-ledger entry, not a stall. Blocking is reserved for irreversible or boundary-crossing
   decisions, and a blocked ask is loud, structured, and answerable in one tap.
7. **Safe by construction.** Rooms start least-privilege, and safety is enforced at the room
   boundary, never inside the harness: which secrets enter, what egress is open, and which boundary
   verbs (publish, spend, destroy) the system performs unasked. Exceeding the envelope queues a
   one-tap approval instead of stalling the job. The dial controls **when you approve**, not what
   the worker can do inside its room.
8. **Claims are not facts.** The control plane never presents a worker’s claim as an observed fact.
   Every surface labels which is which.
9. **Honest progress.** No fake spinners. Staged readiness is fine: early shell access while heavy
   toolchain warms; worker announces when fully armed.
10. **Same verbs everywhere.** CLI, GUI, voice, phone, agent API:

```text
worker spawn|message|inspect|wait|attach|recover|end
job assign|message|inspect|wait|end
```

If verbs diverge by surface, DX is broken. Resource namespaces make identity explicit; there is no
slash target syntax and no ambiguous top-level compatibility alias. Mutations are retry-safe,
`inspect` is a free read with explicit lenses, and `wait` pins admitted input rather than requesting
status. Attachment changes human presence only, and shell exit clears that presence. There is no
status-prompt verb: observed facts and provenance-labelled Worker claims provide the situation,
while a silent Worker is an attention condition rather than something to poll conversationally.

### North-star tests for every runtime feature

Before adding a control-plane feature, ask:

1. Does a human need **fewer glances** to trust what is happening?
2. Can an **agent** invoke it as cleanly as a human?
3. Does crash / sleep / reconnect remain a **non-event**?
4. Can someone **discard everything** with no residue?
5. Does it make **detached work** feel safer than live watching?
6. Does it respect the **attention contract**, pushing state freely but interrupting only for a
   real decision?

If not, it is noise on the control plane — even if it demos well.

---

## Agent-first clients

Humans get beauty. Agents get the same power with zero ceremony.

The control plane is not “an API for web UIs.” It is the **nervous system other agents use to employ
workers safely**. Any worker (or external automation) should be able to:

```text
worker spawn specialist
job assign auth-flake --to specialist --goal "reproduce flaky auth test and isolate cause"
job wait auth-flake
job inspect auth-flake --evidence
job message auth-flake "try with redis flushed"
worker inspect specialist
```

That is how personal capacity compounds without turning the human into a process babysitter:

- you summon one lead worker
- the lead spawns specialists in fresh rooms
- you only meet escalations and finished evidence packs

One fence keeps this from reimporting an org plane: to the runtime, a lead worker is just another
client speaking the same verbs. Escalation policies, budgets, hierarchies, and roles are
application concerns, fenced out exactly like coding-to-PR.

Human-friendly and agent-first are the **same design** if the primitive is clean. If agents need a
secret second API full of opaque conversation IDs and provider handles, the human API will rot
into the same mess.

---

## Step-function direction (aspirational, not backlog)

The runtime exists so a developer’s job can shift from foreground pair-programmer to
**scheduler + reviewer + takeover operator**.

That step function is not “a better chat with a coding model.” It is:

> I can safely run more concurrent engineering work than my attention can supervise live,
> with higher confidence in outcomes and lower context-switch cost,
> on infrastructure I control.

Most tools today still accelerate the middle of:

```text
think → open IDE → prompt agent → watch/fix → review diff → push → context-switch
```

The developer remains the foreground process. Cloud agents often make a faster pair programmer.
A step function changes the unit of work from **conversation** to **managed personal capacity**:

```text
assign goals → workers act in rooms → you review outcomes on your schedule → accept / steer / discard
```

### What must be true for that day to feel real

1. **Parallelism without mental collapse** — many jobs alive; each glanceable; no full-chat replay
   to remember what a job was.
2. **Async by default, sync by exception** — attach is for stuck or deep work; live babysitting is a
   failure mode.
3. **Trustworthy disposable rooms** — clean desk, deterministic setup where apps need it, clear keep
   vs destroy.
4. **Human time only on high leverage** — framing, risky judgment, final review — not steering every
   diff.
5. **Continuity across days** — two-hour or two-day jobs keep identity; sleep and travel are normal.

### Ideal day sketch (experience target, not a roadmap)

**8:10** — Phone wall of overnight jobs: two done, one blocked, one still chewing.
**8:12** — Tap blocked job, approve “use mock payments,” back to coffee.
**8:40** — At desk, open done jobs as evidence packs. Accept two. Reject one with a steer: “wrong UX
tradeoff; try again in a fresh room with this revised brief.”
**9:00–11:00** — Deep human architecture work. No token stream open.
**11:00** — A lead worker pings: needs a design call. You enter the room for eight minutes, sketch,
leave. Worker continues.
**14:00** — “Spawn three approaches for search ranking, different rooms, shared benchmark harness,
report winner.” You do not manage the matrix; you get a comparison board.
**18:00** — Fleet winds down. Hibernated rooms cost near-nothing. Destroyed rooms leave no residue.
Tomorrow inherits continuity, not archaeology.

Emotional shift:

```text
before: "I write and babysit code all day."
after:  "I assign goals, unstick judgment calls, and accept proof."
```

Showcase apps (coding-to-PR today; AFK boards later) should make slices of this day visceral. They
still must not redefine L1 as “the PR product.”

---

## Competitive posture and non-goals

The crowded set for “agent + machine + reconnect” includes Codex cloud, Cursor cloud agents, Amp
orbs, Devin outposts, and various sandbox-agent wrappers. “Subscription + VM + coding agent” alone
is **not** a differentiator in 2026.

Dorf runtime should not try to win as:

| Not us | Why |
| --- | --- |
| **Flue** | App framework for deploying agent products (channels, React, multi-tenant scale) |
| **Paperclip** | Org charts, budgets, goals — “AI company” management plane |
| **Single-vendor clouds** | Cursor / Codex / Amp / Devin polish for one harness in their infra |

Possible durable wedge **once L1 is real**:

> local-first, multi-harness, multi-sandbox **control plane** for durable personal workers —
> subscription-friendly, owned infrastructure, remote inspect / turn / attach, clean lifecycle.

Until a second harness or second sandbox exists, multi-* is aspiration, not proof. Do not advertise
generality the code does not have. Earn it with the stranger loop first.

**Paperclip vs this building block:**

```text
Paperclip  → manage many agents as a company (org plane)
Dorf   → run and remotely control each agent as a durable sandboxed worker (factory floor)
```

They can stack later. They must not be confused now.

---

## Relationship to current repo practice

- [runtime.md](runtime.md) remains the normative portable boundary for what exists now
  (`spawn`, `assign`, `inspect`, `run_turn`, `recover_active_turn`, `end`, adapters).
- [principles.md](principles.md) still governs how we build: small composable primitives, boring
  before fancy, second implementation before broad abstraction.
- [showcase-ideals.md](showcase-ideals.md) collects the workflow-layer ideals (contract-first
  acceptance, verification ladders, autonomy labels, calibration) that compose on this control
  plane and must not leak into it.
- Coding-to-PR remains the active **dogfood path** that forces vertical slices and evidence.
- This north star tells us **what the runtime is for** when those slices land: a control plane
  strangers can operate without PR concepts in the room.

### How to use this document

| Do | Do not |
| --- | --- |
| Reread after context loss for accepted direction and taste | Treat every metaphor as a fully specified API or schema |
| Judge runtime PRs against the stranger loop and DX tests | Open a 40-issue epic of office analogies |
| Keep workflow/PR concepts out of runtime surfaces | Let coding-to-PR leak into L1 “for convenience” |
| Bucket new ideas with the layer rule (mechanism here, policy in [showcase-ideals.md](showcase-ideals.md)) | Let workflow semantics harden into runtime promises |
| Record concrete API, storage, and lifecycle choices in [decisions.md](decisions.md) | Preserve superseded aggregate names or SQLite shapes for compatibility |

This file is the approved picture on the wall. Implement it through working vertical slices, compare
what exists with what the target requires, and freely replace experimental code that no longer
serves the target. When implementation evidence exposes a real conflict, discuss the conflict and
record the resulting choice in the decision and runtime docs rather than quietly weakening the
north star.
