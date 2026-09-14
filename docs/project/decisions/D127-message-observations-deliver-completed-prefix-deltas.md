# D127: Message observations deliver completed prefix deltas

- **Applicability:** current
- **Areas:** client-api, harnesses, interaction
- **Read when:** Changing Message observation, native connection reuse, stream replay, or idle behavior.
- **Decision history:** Accepted coherent observation and native event delivery, 2026-09-15.
- **Decision:** A Message observation combines durable delivery state with its exact native completed
  input/reply prefix. A cursor selects the unconsumed suffix. Terminal outcome and a final prefix
  watermark let a client establish completeness without retrieving concatenated answer text again.
  Existing terminal results and explicit full timelines retain their separate contracts.
- **Ownership:** Core owns accepted Message custody and terminal outcome. The Harness owns the
  retained transcript and ordering. Its adapter projects completed items into bounded ephemeral
  worker memory. The control reader validates current custody and input origin before exposing an
  observation. Clients own publication, deduplication, and durable acknowledgement of their cursor.
- **Native lifecycle:** An ordinary existing-thread follow may reuse one authenticated native
  connection across Core's history baseline and submission. The scope preserves durable ordering,
  exact owner/thread checks, and claim fencing. An ambiguous submission requires fresh authenticated
  history recovery; transport failure never authorizes transparent mutation replay. Accepted native
  observation may retain the connection until settlement independently of that operation scope.
- **Delivery:** SSE forwards native projection changes through the private worker reader. A compact
  durable-state check joins later execution settlement to the native prefix. Fresh turns append
  completed items directly. Recovered native subscriptions reconcile the full prefix on item events
  over their existing connection, because replayed native IDs do not establish canonical indexes.
  Neither path polls full native history on a timer. Native completion summaries do not define item
  indexes; authoritative full-prefix reconciliation establishes the final watermark.
- **Sleep and replay:** A subscription owns neither a Sandbox activity lease nor a native connection.
  Heartbeats and passive reconnects never resume the Sandbox. Missing or invalidated ephemeral
  history returns a deferred-resync or gap state. Explicit inspection or actual work can reconstruct
  that history. No additional durable transcript is introduced to guarantee replay after a restart.
- **Why:** Repeated terminal-result and timeline reads duplicate native history work. Retaining the
  required baseline while sharing its authenticated connection removes repeated setup. Delivering
  completed-item deltas avoids both repeated full snapshots and polling delay.
- **Extends:** D123 with coherent status, prefix cursors, and an independent event subscription.
  D123's normalized final-reply selection, input attribution, and retained-session identity remain
  current. Commentary remains part of explicit raw timeline inspection.
- **Proof:** Verification covers exact native identity, connection reuse and ambiguous submission,
  cursor replay and prefix drift, completion ordering, authenticated streaming, and publication
  under durable claims. Native runtime checks use the supported app-server with a fake model
  backend; they do not measure real model inference or imply deployment of the change.
