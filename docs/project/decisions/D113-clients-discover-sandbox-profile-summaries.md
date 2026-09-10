# D113: Clients discover Sandbox profile summaries

- **Applicability:** current
- **Areas:** client-api
- **Read when:** Changing remote Sandbox profile discovery or Job profile selection errors.
- **Decision history:** Accepted, 2026-09-11.
- **Decision:** Authenticated Clients can list all configured Sandbox profile names with their
  provider, Harness, default status, and current recorded verification status. The CLI routes
  `profile list` through the same Control API for remote and deployment-host Clients.
- **Why:** Job admission accepts profile names, so callers need to discover valid choices without
  deployment-host access or guessing. Unknown explicit profiles receive a specific
  `profile_not_found` Problem with a listing handoff.
- **Boundary:** The response is a selection summary. It exposes no credentials, artifacts, Gateway
  URLs, host configuration, or verification diagnostics. Profile administration and full inspection
  remain host-owned. Listing performs no live readiness check, and admission remains authoritative.
- **Replay:** Existing admission replay resolves the retained Job without consulting current profile
  eligibility. Discovery does not add a preflight check that could break replay.
- **Proof:** HTTP tests cover authentication and strict read semantics. CLI tests prove listing runs
  without host configuration. PostgreSQL-backed HTTP tests cover stored verification, default
  selection, omission of private fields, admission using a discovered profile, and unknown profiles
  across direct, coding, and investigation Jobs.
- **Reconsider when:** Clients need profile administration or profile-specific authorization.
