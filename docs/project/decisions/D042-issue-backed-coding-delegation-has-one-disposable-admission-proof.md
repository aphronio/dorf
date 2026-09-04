# D042: Issue-backed coding delegation has one disposable admission proof

- **Applicability:** historical
- **Areas:** workflows, sandboxes, github
- **Read when:** Reviewing the former disposable admission proof for issue-backed coding Jobs.
- **Decision history:** Superseded by D047 — 2026-08-08
- **Decision:** Before an issue-backed `start` or `afk` invocation may reserve AFK ownership, create
  a coding Job or branch, or provision a durable Worker Room, run one workflow-owned proof against
  the exact repository head and issue. Aggregate independently discoverable repository, GitHub App,
  official-image, Incus, provider, and reviewer failures. When discovery succeeds, use one
  unrecorded disposable VM and scoped routes to clone the target branch, run repository preparation
  and smoke contracts, dry-run a Git push, and complete bounded real implementation-model and
  DeepSeek diff-review turns. Revoke the routes and destroy the VM before admission. The same
  invocation consumes the proof, records its non-secret facts, and repeated AFK delegation reuses
  the admitted Job and proof rather than duplicating identity or authority.
- **Why:** Metadata and isolated health probes can report healthy while Git credentials, repository
  preparation, the selected model, or the reviewer consumer path still fails. A single disposable
  terminal gives the owner one complete repair list and prevents known prerequisites from surfacing
  only after externally visible coding state exists.
- **Compatibility:** The proof schema, failure codes, disposable VM name, and CLI rendering remain
  internal alpha surfaces. Explicit task-text `start` remains the lower-level manual path until a
  concrete authority replaces the issue identity used by AFK delegation.
- **Reconsider when:** A non-GitHub coding authority becomes real, repository smoke proves too broad
  for admission and a narrower repo-owned readiness contract is observed, or a future Environment
  can provide the same exact disposable consumer proof without a local Incus VM.
