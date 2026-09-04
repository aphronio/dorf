# D008: Local authenticated Incus image for ChatGPT subscription

- **Applicability:** partial
- **Areas:** model-access, sandboxes, deployment
- **Read when:** Changing Sandbox image credentials for Droid or reviewing the retired Codex image flow.
- **Decision history:** Superseded for Codex by D035 — 2026-07-29 (implemented and validated under the
  completed #159 umbrella); the secret-bearing image remains the private default for Droid and any
  other non-Codex agent state (accepted for the local single-user phase — 2026-07-22; made the
  private default — 2026-07-26)
- **Decision:** Use the local secret-bearing `dorf-codex-droid-authenticated-local` Incus image
  containing `/root/.codex` ChatGPT device-login state for Codex CLI and app-server. While the code,
  image, and deployment remain private and single-user, this image is the default Room template;
  configured repositories may override it. Once the D035 umbrella ships, Codex provisioning follows
  D035 instead: credential-free images plus broker-issued scoped keys.
- **Why:** This supports the owner's ChatGPT subscription inside the current local trust boundary
  without requiring usage-based API credentials. A vanilla Ubuntu default cannot satisfy Worker
  readiness and makes the zero-configuration `spawn` path predictably fail.
- **Reconsider when:** Dorf becomes remote or multi-user, images must be distributed, Workers
  need distinct credentials, credentials require scoped injection, or cloned-image token refresh is
  unreliable.
