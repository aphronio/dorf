# D116: Clients bootstrap Sandboxes and observe the latest reply

- **Applicability:** current
- **Areas:** core, client-api
- **Read when:** Changing client Sandbox setup or reply notification integrations.
- **Decision history:** Accepted, 2026-09-11.
- **Decision:** Expose bounded Sandbox exec through the existing authenticated custody and cleanup
  boundary. Derive the latest settled main-Sandbox reply from retained Messages and AgentRuns,
  expose its ID on Job inspection, and accept `latest` for Message inspection. Client configuration
  can default the opaque reference for new Jobs.
- **Why:** Clients can install a matching CLI without rebuilding Sandbox images and route reply
  notices without reconstructing execution or maintaining another Job registry.
- **Boundary:** Exec uses explicit argv, bounded input and timeout, and capped response streams.
  It has no automatic replay. Clients own installation policy and safe retry. Latest-reply
  observation adds no durable state and excludes pending follow-ups and steer acknowledgements.
  References remain correlation metadata and confer no authority.
- **Proof:** API and reader tests cover authentication, custody, cleanup fencing, literal command
  input, nonzero exit codes, and escaped output. PostgreSQL integration covers latest-reply
  selection across retained Message states; the CLI journey covers default references and reply
  inspection without a Message ID. Live Sandbox exec verification remains incomplete.
