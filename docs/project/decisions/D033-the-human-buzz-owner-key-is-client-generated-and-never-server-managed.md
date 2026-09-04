# D033: The human Buzz owner key is client-generated and never server-managed

- **Applicability:** current
- **Areas:** deployment, interaction
- **Read when:** Changing Buzz owner enrollment, identity bootstrap, or private-key custody.
- **Decision history:** Accepted after first desktop onboarding — 2026-07-28
- **Decision:** Generate and retain the main community owner's private identity only in the
  first-party Buzz client under the human's control. Relay provisioning accepts only that identity's
  public key as `RELAY_OWNER_PUBKEY`; it must not generate, display, transfer, or retain a human
  owner private key. The relay's service-signing key remains a distinct deployment secret inside
  the VM.
- **Bootstrap posture:** A closed relay may wait for the human's public key before its first usable
  start. Do not temporarily open membership or create a server-side human identity merely to make
  provisioning unattended. An explicitly disposable automated fixture may own a fixture key, but
  that exception does not apply to the durable personal deployment.
- **Why:** The first desktop onboarding proved that Buzz can generate the human identity before
  joining a community and expose only its safe `npub` for enrollment. This produces one human, one
  owner identity, avoids a private-key transfer ceremony, and keeps the server from holding a
  credential it does not need.
- **Migration evidence:** The Mac-generated identity is the sole active relay owner. The temporary
  server-generated bootstrap identity was demoted, removed from membership after the desktop
  authenticated, and its private/public key files were deleted. It authored no conversation.
- **Reconsider when:** Buzz introduces a server-mediated owner bootstrap that never exposes or
  escrows the human private key, or a concrete non-human fixture needs an explicitly scoped
  operator identity.
