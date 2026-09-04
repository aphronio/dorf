# D084: Coding no longer owns an implicit repository setup/check contract

- **Applicability:** current
- **Areas:** workflows, core
- **Read when:** Changing coding repository setup, deterministic evaluation, or Check ownership.
- **Decision history:** Accepted deletion after Core/workflow boundary review — 2026-08-20
- **Decision:** Remove `.dorf.toml`, repository setup commands, coding Check records, Check Evidence,
  setup retry, and the associated coordinator stages. The coding workflow retains exact Git
  Revision observation, deterministic ReviewPolicy over observed changed paths, selected review,
  exact-Revision publication, and cleanup. No hidden convention or replacement configuration is
  introduced.
- **Why:** The repository file was an implicit Dorf-specific workflow input and its command/check
  pipeline had become mistaken for a Core facility. A general image cannot promise arbitrary
  repository dependencies, while silently asking repositories to adopt a Dorf contract is the
  wrong boundary. Removing the unearned machinery is smaller and more truthful than relocating it.
- **Proof:** The baseline schema contains no setup action, repository command, Check, or Check-linked
  Evidence tables or columns. Coding reaches review directly after exact Git observation. Inspect,
  follow, publication, release proof, and current authority docs contain no setup/check phase, and
  the full Go, SQL, and PostgreSQL verification contract passes.
- **Reconsider when:** A concrete workflow needs deterministic evaluation as an explicit accepted
  input with its own result and recovery semantics. Add that workflow-owned contract from the real
  use case; do not restore an ambient repository file or a generic Core Check primitive.
- **Implementation reference:** Commit `36d8a01` is the last tree before deletion. Treat it as
  non-normative source material when a workflow earns verification: retain exact-Revision binding,
  stable Absurd Step identities, bounded command observations, content-addressed Evidence, and
  retry-safe settlement. Do not recover `.dorf.toml`, implicit setup discovery, or Core-owned Check
  policy merely because the old implementation contained them.
