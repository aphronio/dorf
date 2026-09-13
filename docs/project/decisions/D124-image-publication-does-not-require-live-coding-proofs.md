# D124: Image publication does not require live coding proofs

- **Applicability:** current
- **Areas:** release, sandboxes
- **Read when:** Changing Incus image publication prerequisites or release verification.
- **Decision history:** Accepted, 2026-09-14. Revises the promotion requirements in D066 and D114.
- **Decision:** Build and publish the official Incus image without live coding Jobs, model turns,
  browser navigation proofs, a proof deployment, or a GitHub App installation. Remove these
  prerequisites from the release authority rather than providing a bypass flag.
- **Retained checks:** Exact clean source, pinned recipe inputs and package integrity, image
  metadata and archive validation, temporary build-resource cleanup, successful release CI,
  and signed immutable publication remain required. Deployment profile verification still
  gates admission independently of artifact publication.
- **Why:** A packaging update must not depend on an unrelated deployment's workflow credentials
  or spare Job capacity. GitHub and GHCR publishing credentials remain necessary for publishing.
- **Tradeoff:** Publication establishes artifact provenance and contents, not successful live
  model execution or browser navigation. Release notes must not claim those proofs occurred.
