# D104: Technology choices stay replaceable

- **Applicability:** current
- **Areas:** product, core
- **Read when:** Reviewing a technology replacement, migration, or compatibility obligation.
- **Decision history:** Accepted product and engineering direction, 2026-09-04.
- **Decision:** Apply [Vertical slices, replaceable technology, and preserved
  evidence](../principles.md#vertical-slices-replaceable-technology-and-preserved-evidence) to models,
  dependencies, languages, abstractions, schemas, and legacy code.
- **Why:** Dorf's implementation exists to provide controlled agent execution. Preserving an older
  choice after it obstructs that result increases the context and maintenance required from every
  later human or agent.
- **Consequences:** A replacement begins from the smallest design that fits the current product.
  Before deleting data, it identifies and carries forward the Job history, Evidence, evaluations,
  dogfood proof, and operating observations that improve later work. Compatibility work must protect
  a real consumer, a retained deployment, or a published contract.
- **Reconsider when:** Dorf has no safe way to preserve a required record, or a real compatibility
  duty makes an in-place migration part of the product outcome.
