# D131: Application observations reuse automatic delivery

- **Applicability:** current
- **Areas:** client-api, harnesses, interaction
- **Read when:** Delivering application updates into active work or recovering native start-or-steer races.
- **Decision history:** Extended D125's Follow-only boundary to existing Auto delivery, 2026-09-15.
- **Decision:** Text-only application observations allow Auto and Follow. No new queue, intent,
  event policy, or application envelope belongs in Core. Explicit Steer remains unsupported:
  Codex native tool output has no exact-target precondition.
- **Adapter:** Auto uses native `turn/start.toolOutput` with empty user input. Codex joins active
  work or starts idle work. The embedded AgentRun identity supplies acceptance proof on cold reads.
  If the selected target finishes in flight, exact acceptance in a later Turn atomically changes
  the same Message to Follow and binds that Turn without resubmission. Conflicting attribution
  retains uncertainty. Cleanup must adopt accepted work before deciding it is safe to remove.
- **Why:** Timely comments need the existing automatic delivery behavior without gaining human
  authority. Native acceptance, not a guessed timing window, establishes delivery ownership.
- **Proof:** Adapter tests cover active native tool output; PostgreSQL covers immutable Auto
  admission and lost-acknowledgement adoption without duplicate submission. Codex 0.154.0 against
  a deterministic local Responses server proves idle start, active joining, tool-role model input,
  and restart retention. No live model or external account is used.
