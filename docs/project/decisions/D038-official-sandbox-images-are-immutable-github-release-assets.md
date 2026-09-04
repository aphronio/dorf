# D038: Official Sandbox images are immutable GitHub Release assets

- **Applicability:** partial
- **Areas:** release, sandboxes, deployment
- **Read when:** Changing how official Sandbox images are built, promoted, verified, or distributed.
- **Decision history:** Accepted and implemented — 2026-07-31; local-first and combined product-release
  boundaries revised during public activation — 2026-08-04; Go artifact boundary at D047 cutover
  — 2026-08-08; Go-required schema 4 after issue #38 dogfood — 2026-08-08; base and inventory
  clauses superseded by D064 and current artifact identity refined by D066 — 2026-08-13; installer
  asset and change-driven image promotion added — 2026-08-21
- **Decision:** Every immutable Dorf product release contains one Go x86_64 Linux archive/checksum
  and its small installer. GitHub Releases remain the credential-free x86_64 Sandbox VM store, but
  an Incus archive/compatibility manifest is attached only to the product release that promotes a
  changed image. Later application releases pin and reuse that exact earlier immutable image rather
  than rebuilding or republishing unchanged bytes. The installer selects the exact release's Go
  archive, verifies it against that release's checksum, and installs only the verified binary. The
  native `dorf update` command verifies the latest immutable installer asset and delegates to this
  same path rather than owning a second replacement implementation. One
  repo-owned local command builds a promoted image from an immutable base fingerprint, records its exact Harness packages,
  proves the credential boundary, and completes a real coding tracer for every declared Harness from clone and
  repo-owned preparation through an implementation turn, checks, scoped routing,
  content-addressed evidence, and exact cleanup. The image includes Git, Go, Node, uv, and its
  declared Harness executables but removes build-only npm. The command exports the untouched candidate and
  publishes it with GitHub CLI. The consumer accepts only a
  published immutable release and requires agreement among GitHub's asset digests, the manifest,
  the downloaded archive SHA-256, and the post-import Incus fingerprint.
- **Artifact identity:** Attach each newly promoted image to a normal immutable `vX.Y.Z` Dorf
  product release instead of creating machine-only releases in the human-facing release feed. The
  application and official image release pins are independent; the repository check compares
  declared image inputs with the immutable pinned release tag and fails on drift. Advancing the pin
  explicitly can also earn a deliberate security refresh without a source-input change. The current
  exact schema and asset identity live in D066. The manifest requires `environment: incus`, the complete
  coding-workstation inventory, and verified pinned tool release-archive digests. Its
  candidate proof executes `go`, `gofmt`, and the repository's declared preparation in a fresh
  Sandbox. Issue #38 dogfood showed that the historical schema-3 image could reach a Go repository
  without Go installed, so the Go installer accepts only the current schema. Old clients and image
  schemas are not a compatibility target.
- **Promotion boundary:** The repository must be public and GitHub immutable releases must be
  enabled before the first image is promoted. The publisher records that reviewed repository
  setting in an explicit variable, requires a clean source commit already available from GitHub,
  creates a complete draft, publishes it once, and verifies its release attestation and all assets.
  The owner's provider credential remains in the local Provider Gateway; only a scoped route enters
  a disposable validation Sandbox, and neither enters the image or GitHub.
- **Why:** GitHub Releases reuse the project's source authority, provide static anonymous HTTPS
  downloads, protected tags and assets, release attestations, and API-visible SHA-256 digests
  without operating a public Incus daemon or a separate image-index service. Verifying every layer
  keeps the friendly alias out of the trust boundary and lets setup converge idempotently on one
  exact local fingerprint.
- **Publication ownership:** When the declared Incus image pin is reused, hosted GitHub Actions owns
  application and GHCR publication with narrowly scoped `contents:write` and `packages:write`
  permissions. The workflow requires successful CI for the exact dispatch commit, checks out that
  commit, and invokes the repository command; it does not duplicate release logic. A pin advance
  remains an explicit local proof and publication boundary because its real Codex/Pi proof requires
  the owner's Provider Gateway credentials; those credentials are never moved to hosted Actions.
- **Compatibility:** The repository path, release tag shape, asset names, manifest schema, and
  installer module are pre-release implementation details. Existing Sandboxes remain bound to the image
  they were created from. The first image is x86_64-only; GitHub Releases are not a claim of support
  for another architecture or Environment.
- **Reconsider when:** Release size or bandwidth makes GitHub unsuitable, a second architecture
  needs a real distribution index, Incus simplestreams materially reduces setup complexity, GitHub
  cannot preserve the required immutability/digest guarantees, or a concrete remote Environment
  requires a different image authority. Reconsider hosted pin-advance publication when a scoped
  non-personal provider credential and isolated ephemeral runner make it safer without weakening the
  real Job terminal.
