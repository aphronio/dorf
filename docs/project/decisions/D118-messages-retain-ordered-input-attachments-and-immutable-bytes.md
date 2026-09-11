# D118: Messages retain ordered input attachments and immutable bytes

- **Applicability:** current
- **Areas:** core, persistence, harnesses
- **Read when:** Changing Message attachments, input blob custody, native image delivery, or attachment replay.
- **Decision history:** Accepted Message attachment custody — 2026-09-11
- **Decision:** Keep the JSON text-only Message request. Add multipart Message admission for text
  and ordered file bytes. The server derives each attachment's kind, safe filename, media type,
  digest, and byte size. Core receives the verified ordered manifest and retains it with the Message.
  The same send key binds the exact text, manifest, intent, and skill-refresh request.
- **Custody:** The content-addressed blob store retains the accepted bytes for the Message lifetime.
  Before each native submission, the delivery adapter rehashes those bytes and restores every
  attachment to a deterministic path in the Job-owned Sandbox. Images also enter a supported
  Harness through its native image input. Sandbox cleanup removes these working copies but leaves
  the durable Message and blobs available for exact receipt replay. A replay after cleanup does not
  create or wake work.
- **Admission boundary:** Clients send files by value and do not supply hashes, sizes, ordinals, or
  URLs. The HTTP boundary bounds and validates the bytes before Core sees them. It rejects an image
  when the selected profile lacks native image support. Generic files remain available through the
  prompt's working paths. Accepted-turn recovery reads native Harness history before considering
  another submission and does not depend on the working copy.
- **Scope:** Input attachment custody does not retain agent-authored files or discover generic
  deliverables. D089 still requires a client or workflow to read those files before cleanup. This
  slice adds no upload resource, attachment task, or garbage-collection lifecycle. A rejected
  admission may leave an unreferenced content-addressed blob after publication and before the SQL
  transaction. Request authentication and byte limits bound that tradeoff.
- **Refines:** D088's Message custody contract and D089's statement that only Evidence uses the blob
  store. It preserves D096's follow and steer semantics, D107's task attachment rules, and D110's
  separation of Job setup from Message delivery.
- **Proof:** PostgreSQL coverage preserves exact ordered manifests across reload, replay, and
  completed cleanup. Control API coverage sends attachment-only multipart input and detects changed
  bytes. Native tests restore all working files and pass image bytes on initial, follow, and steer
  delivery while recovery uses the accepted Turn.
