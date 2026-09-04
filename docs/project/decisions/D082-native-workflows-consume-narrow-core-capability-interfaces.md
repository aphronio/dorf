# D082: Native workflows consume narrow Core capability interfaces

- **Applicability:** historical
- **Areas:** core, workflows, client-api
- **Read when:** Reviewing the former narrow Core capability interfaces for native workflows.
- **Decision history:** Superseded by D088 — 2026-08-21
- **Decision:** Define provider-neutral Core interfaces for AgentRun delivery and observation,
  stable Sandbox Actions, cleanup reconciliation, and durable Core storage. Native workflows
  consume those interfaces through typed runtime compositions. Repository materialization is a
  workflow-module composition over Core execution, not a Core capability. The composition root
  alone constructs the current `core` and PostgreSQL implementations.
- **Why:** Depending on concrete `ExecutionService`, `RepositoryService`, and `postgres.Store`
  implementations made in-process workflow reuse look like a privileged path and prevented a
  workflow-owned service from importing the Core contract without an import cycle. Interfaces must
  describe capabilities actually consumed, not speculate about a public transport schema.
- **Proof:** Coding and investigation accept a Git-workspace interface composed over Core
  execution; cleanup accepts only cleanup execution; compile-time assertions prove the current
  implementations satisfy each contract. Common runtimes and Core interfaces carry no repository
  capability.
- **Not included:** No network API, authentication model, direct-execution resource, or SDK is
  claimed. A future external client may earn a transport contract over these capabilities without
  exposing adapters, PostgreSQL, or Absurd.
- **Reconsider when:** A real transport needs a materially different resource boundary, or another
  workflow proves one of these capability groupings is too broad.
