# D066: One credential-free image carries both verified Harnesses

- **Applicability:** current
- **Areas:** release, harnesses, sandboxes
- **Read when:** Changing combined Harness image packaging, promotion proof, or credential-free artifact contents.
- **Decision history:** Accepted and proven by the immutable v0.2.0 release — 2026-08-14
- **Decision:** Publish one Debian 13 Incus image containing exact Codex and Pi npm packages over the
  shared workstation baseline. `DORF_HARNESS` selects one runtime adapter; it does not select another
  image. The image contains no Harness authentication, Provider Route key, session, or configuration.
  Manifest schema 5 records both package versions and npm integrities and uses the exact assets
  `dorf-incus-vm-v5-x86_64.tar.gz` and `dorf-incus-vm-v5-x86_64.json`. The installer converges the
  neutral `dorf` alias on the manifest's immutable fingerprint.
- **Promotion boundary:** One candidate fingerprint must complete separate real Codex and Pi
  no-change coding Jobs, including exact native Thread/Turn binding, Revision Evidence, route
  revocation, and Sandbox cleanup, before publication. Pin-advance proof and publication remain an
  explicit local boundary; when the proven pin is reused, hosted Actions owns application/GHCR
  publication. The repository release command remains the only procedure authority for both paths.
- **Proof:** The immutable v0.2.0 release targets source commit
  `c9d597f21068bacf5650939781b5f2ad8d3b854d`; its signed GitHub
  attestation binds the manifest and archive, and the manifest records combined-image fingerprint
  `ea537b1b6d5aa503eb5a1728988f31d05ce37984635303f1eae5bf3640748781`. Separate
  Codex and Pi Jobs completed against that fingerprint with exact Revision Evidence, route
  revocation, and Sandbox cleanup before publication.
- **Why:** The shared Debian and cross-repository toolchain dominate image size. Duplicating that
  baseline for two small Harness packages increases release, download, storage, rollback, and
  operator-selection surface without adding isolation: the Job-owned Sandbox and scoped Provider
  Route remain the security boundaries, and only the selected Harness is started and configured.
- **Supersedes and refines:** Supersedes D065's separate-image packaging choice, refines D038's
  schema-4 Codex artifact identity, and leaves D064's shared Debian/toolchain baseline intact.
- **Reconsider when:** Co-installation causes measured dependency conflicts, materially expands a
  Harness-specific attack boundary, prevents independent security updates, or separate delta images
  become cheaper than one shared artifact in real release and installation evidence.
