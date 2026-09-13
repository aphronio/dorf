# D120: Default idle pause with an admitted keep-running override

- **Applicability:** partial
- **Areas:** core, sandboxes, client-api
- **Read when:** Changing Sandbox idle policy, E2B retention, or admission defaults.
- **Decision history:** Accepted — 2026-09-13; idle grace and activity custody revised by D122
- **Decision:** Direct, coding, and investigation clients default to pausing idle Sandboxes on
  providers with a proved memory-pause capability. An immutable `keep_running` Job input disables
  that policy. E2B is the first implementation; other providers retain their existing behavior.
- **Ownership:** Admission retains the caller's policy. Core reconciles it against durable
  Message/AgentRun custody under the existing Job effect fence. Providers own actual power state
  and snapshot bytes. Pause neither closes admission nor requests cleanup or changes a Turn outcome.
- **Why:** Retained assistants and delegated workers need one lifecycle while avoiding idle compute
  charges. A background service can opt out without introducing a separate assistant implementation.
- **Recovery:** New input can be accepted during pause, but native execution shares the effect fence.
  Subsequent delivery resumes the same owned Sandbox. Every pause retry checks current work again.
  Transient pause failures are logged and retried by the existing durable wait loop. Native reads
  may wake a Sandbox and reconcile idle state after releasing their read fence.
- **Limits:** Paused background processes make no progress, and external connections must reconnect.
  Provider timeout limits remain. New E2B Sandboxes use timeout auto-pause as a fallback; it is not
  a guarantee of uninterrupted long turns or of memory preservation under every provider failure.
  There is no additional transcript store, snapshot table, or copied session archive.
- **Proof:** PostgreSQL tests cover retained overrides, replay conflicts and admission during a
  fenced pause. Unit tests cover active/uncertain/queued work, stale retries, ownership and a lost
  pause acknowledgement. A disposable live E2B test performed two memory pause/resume cycles via
  Dorf's Go adapter, preserving process PID, boot identity and a nonce held only in RAM; exact
  ownership cleanup was independently confirmed. This proof did not make model calls.
