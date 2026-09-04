# D034: Human-facing applications remain SDK clients

- **Applicability:** historical
- **Areas:** client-api, core, interaction
- **Read when:** Reviewing the superseded in-process Python SDK boundary for human-facing applications.
- **Decision history:** Superseded by D047 — 2026-08-06
- **Decision:** Human-facing applications own their conversation, memory, semantic intent, channel
  integration, tool selection, and approval UX. They employ Dorf Workers through the same typed
  Worker and Job verbs available to humans and other clients. Dorf remains authoritative for Worker,
  Room, Job, Assignment, durable input, native delivery, observed facts, claims, evidence, recovery,
  and cleanup.
- **SDK boundary:** Trusted same-host applications call the concrete in-process Python SDK; the CLI
  is a sibling adapter over the same facade. Clients never implement lifecycle behavior, open Dorf
  SQLite, or construct Incus and Codex adapters themselves. A network transport may wrap the same
  SDK if an observed multi-host or untrusted-caller requirement appears, without changing Worker or
  Job semantics.
- **Compatibility:** The Python facade, any future request envelope, authentication method, and
  deployment topology are not yet stable public contracts. The enduring boundary is typed resource
  verbs, explicit provenance, retry-safe mutations, and separate ownership of application
  conversation versus runtime lifecycle.
- **Reconsider when:** A concrete client proves the existing verbs cannot express a required
  operation without distorting Worker or Job semantics, a second external client proves a shared
  protocol need, or multi-host operation invalidates the current authority model.
