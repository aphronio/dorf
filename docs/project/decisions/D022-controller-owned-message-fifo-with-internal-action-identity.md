# D022: Controller-owned message FIFO with internal action identity

- **Applicability:** partial
- **Areas:** core, persistence, client-api
- **Read when:** Changing durable message admission, action identity, ordering, dispatch, or wait semantics.
- **Decision history:** Accepted — 2026-07-26; storage revised by D023 and conversation scope by D025
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
