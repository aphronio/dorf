# D073: Local investigation source custody is superseded

- **Applicability:** historical
- **Areas:** workflows, persistence
- **Read when:** Reviewing the removed local-source admission and investigation source-custody design.
- **Decision history:** Superseded before release by D103 — 2026-08-27
- **Supersession:** Investigation accepts only a credential-free reachable HTTPS repository and an
  exact full commit OID. D103 removes local-source admission, source-byte storage, source restoration,
  and their tests. No user or admitted Job requires migration or recovery support.
- **Why:** One remote source contract supplies an exact checkout without adding a second transport,
  storage lifecycle, or API path.
