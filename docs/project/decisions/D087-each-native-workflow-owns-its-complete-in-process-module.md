# D087: Each native workflow owns its complete in-process module

- **Applicability:** partial
- **Areas:** workflows, core
- **Read when:** Changing ownership or composition of a native workflow module.
- **Decision history:** Refined by D088 and D090 — 2026-08-22
- **Decision:** `internal/coding` and `internal/investigation` each own their typed admission input,
  definition and presentation, coordinator, runtime composition, Absurd task registration, Message
  policy, and durable wait loop. Delete the horizontal `internal/workflow` package. Core retains only
  reusable task attachment, Message wake emission, operator retry, and requested-cleanup mechanics.
- **Durability:** Existing workflow names and revisions, Absurd task names, idempotency keys, Step and
  event identities, PostgreSQL facts, retry behavior, and cleanup ordering are unchanged. Moving task
  registration beside its coordinator does not move execution authority out of Absurd.
- **Why:** A central workflow package had become a second composition layer that imported every
  native workflow and translated their types and presentation. It obscured that native workflows are
  independent Core consumers and made adding one require editing a pseudo-registry.
- **Not included:** No dynamic plugin system, workflow DSL, public transport, or new product
  vocabulary is introduced. The binary composition root still selects the compiled workflows.
- **Reconsider when:** Independently distributed workflows earn a loading and compatibility contract,
  or three workflow modules prove a smaller shared registration contract that removes more policy
  than it hides.
