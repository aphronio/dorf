# D146: Name the execution context Session

- **Applicability:** current
- **Areas:** client-api, persistence, harnesses
- **Read when:** Changing execution resource names, Thread ownership, or upgrading clients.
- **Decision history:** Renames the direct execution context and refines D145's binding storage,
  2026-09-18.
- **Decision:** Use Session across the public API, CLI, Go model, and SQL tables. Session-level
  input targets one native Thread. Store only its `ThreadID`; derive the Harness
  from the immutable admitted profile. Retain AgentRun native attribution for delivery recovery.
- **API shape:** One concrete Session object, without a kind discriminator, one-member union,
  or common/concrete wrappers. Creation, inspection, watch, and cleanup use the same shape.
- **Why:** Creation, continuation, and cleanup operate on one durable context. A second stored
  Harness name duplicates the admitted configuration. ThreadID names the conversation currently
  receiving input; no multi-Thread selection exists.
- **Migration:** Rename existing tables and columns in a new migration. Preserve opaque IDs,
  request keys, queue identities and payloads, provider ownership labels, and checkpoint identities.
  Validate the existing Thread Harness against the pinned profile before removing it. Stop old
  API and worker processes, apply the migration, and deploy updated clients and services together.
  Old public routes and CLI names have no aliases.
- **Scope:** Profile naming, configuration activation, Turn ownership, controller replacement,
  and multiple explicitly controlled Threads remain separate decisions. Native subagent threads
  remain Harness-owned. A Session ID is distinct from native Thread or session IDs.
- **Verification:** Existing API/client, PostgreSQL migration and continuation, acceptance recovery,
  cleanup, retry, and ownership tests exercise the renamed contract. No native protocol or provider
  transport is changed.
- **Authority:** [North Star vocabulary](../north-star.md#vocabulary),
  [Architecture](../architecture.md#execution-model),
  [Remote Control API](../../control-api.md).
