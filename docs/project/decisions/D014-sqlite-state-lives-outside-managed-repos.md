# D014: SQLite state lives outside managed repos

- **Applicability:** historical
- **Areas:** persistence, deployment
- **Read when:** Reviewing the SQLite storage model that preceded PostgreSQL authority.
- **Decision history:** Retained pre-consolidation decision.
- **Decision:** SQLite state lives outside managed repos.
- **Why:** Local runtime and coding workflow indexing remains durable without modifying target repositories.
- **Reconsider when:** Multi-host coordination requires another store.
