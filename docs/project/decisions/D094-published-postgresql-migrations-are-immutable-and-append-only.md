# D094: Published PostgreSQL migrations are immutable and append-only

- **Applicability:** current
- **Areas:** persistence, release, deployment
- **Read when:** Changing a published PostgreSQL schema or the migration runner.
- **Decision history:** Accepted upgrade correction — 2026-08-25; greenfield reset exercised before users
  — 2026-08-27
- **Decision:** Freeze `001_baseline.sql` at the schema shipped in the v0.3 releases. Every later
  PostgreSQL change is a new ordered migration whose exact filename is recorded in
  `dorf.schema_migrations`. `dorf migrate` retains its small embedded runner, one transaction, and
  deployment-wide advisory lock; it rejects unknown history rather than inferring a version from
  today's table shape. Never edit an already-published migration to describe the latest schema.
- **Greenfield reset:** D103 exercised this decision's destructive-reset condition before Dorf had
  users or retained Jobs. The replacement `001_greenfield.sql` contains only the current remote
  investigation source shape and records a new baseline identity. A database carrying the retired
  identity fails closed with instructions to recreate the prototype database. There is no source
  compatibility migration, dual read, or recovery path. The replacement is frozen at its
  repository-checked digest.
- **Proof:** PostgreSQL integration creates the exact reset baseline inside a rollback-only
  transaction, checks its digest and migration inventory, and replays it idempotently. The earlier
  self-hosted operator database supplied the original failure terminal: its ledger said
  `001_baseline.sql` while its live shape differed, which is why an ordinary deployed upgrade may
  not rewrite history.
- **Refines:** D048's greenfield squash applied before the first released PostgreSQL schema. It does
  not authorize changing a migration after that schema ships. The prohibition on Python/SQLite
  compatibility facades and dual writes remains; this decision preserves Dorf-owned PostgreSQL
  facts across ordinary Dorf upgrades.
- **Reconsider when:** A major release deliberately declares a destructive database reset, retained
  deployment data no longer has product value, or migration volume makes a maintained framework
  materially smaller than the explicit ordered runner and its behavioral tests.
