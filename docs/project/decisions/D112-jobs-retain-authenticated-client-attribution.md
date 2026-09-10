# D112: Jobs retain authenticated Client attribution

- **Applicability:** current
- **Areas:** core, client-api
- **Read when:** Changing Job attribution, admission replay, or operator cleanup inspection.
- **Decision history:** Accepted, 2026-09-11.
- **Decision:** First admission stores the authenticated Client ID as a nullable Job foreign key.
  Public Job reads and listing resolve its name from the Client inventory, including revoked
  Clients. An optional opaque caller reference correlates the Job with an external thread or task.
- **Replay:** Another Client can replay the same admitted input without changing the original
  creator. The caller reference participates in input equality. Legacy Jobs retain unknown
  attribution; migration and replay never guess their creator.
- **Why:** Clients share one deployment authority. Operators need to identify Job origins before
  choosing cleanup targets, without copying credentials into Jobs or interpreting client policy.
- **Boundary:** Attribution grants no additional authority and introduces no per-Client isolation,
  resource quota, retention rule, or automatic cleanup.
- **Proof:** PostgreSQL-backed HTTP tests cover direct and workflow admission, restart, cross-Client
  replay after creator revocation, listing, changed-reference conflicts, and spoof rejection.
  Migration tests preserve existing Jobs with unknown creators.
- **Reconsider when:** A supported consumer needs a distinct authorization or retention boundary.
