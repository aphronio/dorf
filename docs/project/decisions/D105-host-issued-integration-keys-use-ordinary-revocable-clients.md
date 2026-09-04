# D105: Host-issued integration keys use ordinary revocable Clients

- **Applicability:** current
- **Areas:** client-api, deployment
- **Read when:** Provisioning bearer credentials for unattended integrations.
- **Decision history:** Accepted implementation direction — 2026-09-04
- **Decision:** The deployment host can issue an ordinary Client credential with explicitly no
  expiry. PostgreSQL stores its digest in the existing Client table and represents absent expiry
  with NULL. Enrollment retains its existing default lifetime. Identity and inventory JSON expose
  absent expiry as null, and ordinary Client revocation invalidates the key.
- **Custody:** A host command generates the proof and writes it exclusively to a new owner-only
  file before registration. It never prints the proof. On an uncertain database failure it retains
  the protected file so a potentially committed credential remains recoverable. The host operator
  owns delivery, rotation, and revocation.
- **Why:** An unattended client needs independent revocation without periodic interactive enrollment.
  This is deployment authentication custody within the existing Client boundary. It adds no user,
  scope, workflow policy, second token table, or alternative authentication framework.
- **Relationship:** Extends D097's host-owned Client lifecycle and preserves D103's ordinary Client
  authority. Remote Client administration remains absent.
- **Proof:** Focused tests cover owner-only exclusive file creation, symlink refusal, explicit
  no-expiry selection, digest-backed authentication, nullable HTTP identity, independent revocation,
  and unchanged enrollment expiry. Migration coverage retains the published baseline unchanged.
- **Reconsider when:** A supported integration requires a separately justified authority boundary.
