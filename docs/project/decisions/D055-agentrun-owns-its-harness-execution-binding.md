# D055: AgentRun owns its harness execution binding

- **Applicability:** partial
- **Areas:** harnesses, workflows, persistence
- **Read when:** Changing AgentRun ownership of Message delivery, Harness threads, turns, or execution recovery.
- **Decision history:** Accepted execution simplification; terminal-target fallback superseded by D096 — 2026-08-25
- **Decision:** `AgentRun` is Dorf's complete durable delivery record for one durable Message.
  Submitting, reconciling, waiting for, and recording the harness Turn are its lifecycle, not a
  paired `Action`. Every AgentRun consumes exactly one durable Message; every Message selected for
  agent delivery has exactly one AgentRun. The AgentRun retains the Message identity, Harness,
  Thread, Turn, Role, Revision, capability, Turn outcome, recovery baseline, and the nonces required
  to prove ownership and submission. Implementation continuity comes from reusing the Thread bound
  to prior implementation AgentRuns; there is no separate Thread row or binding on Job. D096 makes
  follow a distinct Turn on the authoritative retained Thread and steer an exact active-Turn binding
  that never falls back to a new Turn.
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
