# D046: AFK diff review is one bounded DeepSeek advisory cycle

- **Applicability:** historical
- **Areas:** workflows, harnesses, model-access
- **Read when:** Reviewing the former fixed DeepSeek advisory review cycle for AFK Jobs.
- **Decision history:** Superseded by D047 — 2026-08-06
- **Decision:** After deterministic gates pass, run the DeepSeek diff role in a fresh disposable
  verifier Room pinned to the implementation commit. Retain its command result and always remove
  its Room and scoped route. Findings are claims admitted once through the original Job FIFO; after
  one implementation decision and new commit, one fresh verifier Room may confirm the result.
  No-findings permits publication but does not prove acceptance. A verifier infrastructure failure
  retains one exact repair-or-decline decision. Repair retries in a fresh Room; decline leaves the
  PR draft with missing advisory evidence stated.
- **Why:** A separate read-only model supplies useful diff pressure without giving it implementation
  authority, creating another Job, or turning advisory output into readiness fact. Existing command
  runs, Job FIFO, and Worker/Room cleanup provide the required custody and crash recovery.
- **Reconsider when:** Repeated dogfood shows one repair is insufficient, or another concrete
  verifier needs coordination that cannot compose through the same bounded role.
