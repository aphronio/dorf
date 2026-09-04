# D078: Coding authority is absent from the base runtime

- **Applicability:** historical
- **Areas:** core, workflows, github
- **Read when:** Reviewing the former split between base and coding runtime authority.
- **Decision history:** Superseded by D083 — 2026-08-20
- **Decision:** Resolve a base workflow runtime containing only the selected profile,
  `ExecutionService`, and `RepositoryService`. Resolve a separate `CodingRuntime` for
  `coding-to-proposal`; only that path constructs `CodingService`, the GitHub client, publication,
  Proposal observation, and outcome services.
- **Why:** A common runtime carrying optional coding fields made GitHub authority appear available to
  investigation and cleanup and forced their startup path to construct irrelevant dependencies.
  Runtime types should make granted authority visible and impossible to acquire accidentally.
- **Proof:** Investigation and cleanup call only base resolution; coding calls only typed coding
  resolution. Unit coverage proves the two resolver paths remain distinct, and the complete Go and
  PostgreSQL verification contract passes with unchanged provider behavior.
- **Reconsider when:** Another workflow legitimately needs one of the coding authorities with the
  same lifecycle and recovery meaning. Add a typed composition for that workflow rather than
  widening the base runtime.
