# D039: Initial core setup supports only the official Sandbox image

- **Applicability:** historical
- **Areas:** product, sandboxes
- **Read when:** Reviewing why guided setup originally admitted only the official Sandbox image.
- **Decision history:** Superseded by D070, 2026-08-26
- **Decision:** The guided core setup uses only the Dorf-published credential-free Room image.
  It does not offer a custom-image selector or claim compatibility with arbitrary Incus images.
  The global profile records the selected official image's immutable fingerprint, and existing
  Rooms retain their recorded image.
- **Why:** A custom image creates a second credential boundary, Codex/tool compatibility contract,
  update policy, validation path, and support surface. None is required to prove the first public
  one-command setup terminal, and supporting it now would make that terminal harder to maintain.
- **Reconsider when:** A concrete user need cannot be met by the official image and justifies the
  additional validation, compatibility, update, and support burden.
