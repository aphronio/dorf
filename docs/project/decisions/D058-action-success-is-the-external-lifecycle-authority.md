# D058: Action success is the external lifecycle authority

- **Applicability:** current
- **Areas:** sandboxes, persistence
- **Read when:** Changing how Sandbox or Provider Route lifecycle state is represented and recovered.
- **Decision history:** Accepted lifecycle-authority simplification — 2026-08-10
- **Decision:** Sandbox rows retain only durable identity, Job ownership, and the nonce required to
  attest the exact external resource. Sandbox and Provider Route lifecycle is read from immutable
  Sandbox-scoped Action success. Dorf does not persist parallel pending/created/deleted or
  pending/active/revoked state machines. Provider Route identity is derived from the Sandbox's stable
  route-create Action identity rather than stored in a separate row.
- **Why:** The copied states could only agree with their Actions, so every completion, cleanup query,
  inspection view, and test had to synchronize two descriptions of the same event. One authority makes
  retries and cleanup easier to read: revoke Action succeeded, then delete Action succeeded.
- **Reconsider when:** A provider returns a non-deterministic Route identity or a lifecycle fact exists
  independently of any Dorf Action and has a concrete product consumer.
