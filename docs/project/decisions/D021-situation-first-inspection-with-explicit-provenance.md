# D021: Situation-first inspection with explicit provenance

- **Applicability:** current
- **Areas:** product, interaction, workflows
- **Read when:** Changing Job inspection, pulse composition, provenance, or raw diagnostic lenses.
- **Decision history:** Accepted — 2026-07-26; product-history shape refined by D061 — 2026-08-10
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
