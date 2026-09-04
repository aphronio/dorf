# D095: The CLI is Dorf's first direct trusted client

- **Applicability:** partial
- **Areas:** client-api, core, interaction
- **Read when:** Changing direct Job semantics or the CLI's role as a Core client.
- **Decision history:** Accepted concrete client slice; role and message semantics refined by D096 —
  2026-08-25; external transport refined by D097 — 2026-08-26; host transport superseded by D103 —
  2026-08-27
- **Decision:** Add `dorf run` as the first concrete direct CLI client, initially composed
  in-process, that admits complete Job intent without workflow identity. It delivers the caller's
  exact prompt through the `direct` Agent role, leaves successful work open and idle for follow,
  steer, or exact file reads, and releases resources only when the caller requests cleanup. The CLI
  owns result meaning and completion policy. D097 later projects admission, inspection, and cleanup
  over authenticated HTTPS without expanding the remote surface to every direct-client mechanism.
- **Durable identity:** A direct Job records both workflow name and revision as absent together;
  workflow Jobs still require an exact pair. Migration `003_client_directed_jobs.sql` admits the
  absent pair and `direct` role; Messages, AgentRuns, Sandboxes, Actions, task attachment, and cleanup
  remain the same Core facts used by workflows.
- **Why:** This proves D088's trusted-client boundary without a fake workflow. Keeping meaning in the
  CLI while Core owns custody makes that separation concrete. This slice alone did not earn a public
  transport, SDK, plugin contract, client registry, or alternate lifecycle; D097 records the later
  transport evidence.
- **Reconsider when:** Direct clients need durable typed policy beyond their own adapter, or more
  than one direct client proves a smaller shared contract than explicit composition.
