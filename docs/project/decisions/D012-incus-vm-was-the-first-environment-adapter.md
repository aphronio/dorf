# D012: Incus VM was the first environment adapter

- **Applicability:** partial
- **Areas:** sandboxes
- **Read when:** Changing Sandbox isolation or reviewing why Incus was the first provider.
- **Decision history:** Retained pre-consolidation decision.
- **Decision:** Incus VM was the first environment adapter.
- **Why:** Its exclusive-adapter choice is superseded by D067's provider-neutral Incus/E2B seam; its isolation boundary remains accepted.
- **Reconsider when:** The remaining isolation boundary no longer fits a proved provider.
