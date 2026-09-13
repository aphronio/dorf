# D119: Native timeline reads preserve Harness history

- **Applicability:** partial
- **Areas:** client-api, harnesses, interaction
- **Read when:** Changing native conversation reads, public native references, or transcript custody.
- **Decision history:** Accepted native timeline projection — 2026-09-13; extended by D123
- **Decision:** Extend the authenticated Job projection with a passive read of one native
  conversation turn. Resolve the Job's default Sandbox and unique retained Harness thread under
  the existing cleanup fence. Expose native references and original conversation objects as read
  results without granting operations on those references or retaining a transcript in Dorf.
- **Native boundary:** Codex supplies the full selected turn through `thread/turns/list`.
  Historical selection follows its ordered turn pages. The adapter preserves user and assistant
  objects, including phase and optional fields, and excludes tool and reasoning items. It opens
  no observation subscription and never resumes a thread. Native item IDs are not a stable client
  cursor or a promise of equality with notification IDs.
- **Why:** A client needs conversation context while work is active and after it settles. The
  Harness already owns that history. Reading one full turn avoids reconstructing a transcript from
  delivery results or stitching separately paginated item snapshots together.
- **Scope:** The optional Harness capability adds no SQL data, public Thread or Turn resource,
  event storage, or Message-to-reply correlation. Missing custody, unsupported history, and exceeded
  read bounds fail explicitly. Cleanup can remove access to native history; clients that need
  durable history own its retention.
- **Extends:** D098's remote interaction projection while preserving its absence of transcript
  persistence and native execution resources. The current behavior and bounds live in the
  [Remote Control API](../../control-api.md), with native prerequisites in
  [Support](../../support.md#native-conversation-timelines).
- **Proof:** PostgreSQL integration coverage verifies persisted custody, passive reads, and
  serialization with cleanup. Native protocol tests cover full turns larger than 100 items,
  historical pagination, raw object preservation, and failed reads for malformed or oversized
  history. Local native proofs on Codex 0.147 and 0.154 cover active and cold history reads.
