# D053: Compile stable PostgreSQL queries with sqlc

- **Applicability:** current
- **Areas:** persistence
- **Read when:** Changing how stable PostgreSQL queries are authored, generated, or mapped into core types.
- **Decision history:** Accepted persistence-tooling boundary — 2026-08-10
- **Decision:** Use repository-pinned `sqlc` 1.31.1 to compile stable Dorf-owned PostgreSQL queries
  into a committed private `database/sql` package. Keep the baseline schema and named query files as
  generator inputs. `postgres.Store` remains the application boundary: handwritten Store methods own
  transactions, compare-and-set expectations, product invariants, error translation, and explicit
  conversion to `core` types. Generated records and parameter structs do not cross that boundary.
- **Type boundary:** Narrow column overrides may reuse existing non-null `core` scalar types for
  cleanup, Message sender and intent, Action kind and state, AgentRun state, and Job outcome. This is
  an inward adapter dependency and does not make generated records into domain types. Nullable values,
  projections, and timestamps continue to be mapped explicitly.
- **Tooling and verification:** Generation and stale-code comparison use the local schema analyzer and
  do not require a database. Strict function and `ORDER BY` checks are enabled. The repository check
  also runs `sqlc vet` with `sqlc/db-prepare` against the already migrated disposable PostgreSQL
  database, followed by the live PostgreSQL Go suite and `go vet`. CI starts the `compose.dev.yaml`
  PostgreSQL service and runs `mise run db:init` before the same check. No sqlc Cloud project, token,
  `push`, `verify`, managed database, migration ownership, or runtime service is introduced. This
  follows sqlc's official [CI guidance](https://docs.sqlc.dev/en/stable/howto/ci-cd.html),
  [configuration reference](https://docs.sqlc.dev/en/stable/reference/config.html), and
  [override guidance](https://docs.sqlc.dev/en/stable/howto/overrides.html) while keeping all committed
  paths repository-relative.
- **Why:** The trial removed embedded SQL and manual scan plumbing from representative reads, made
  schema/query drift fail during generation, and kept the transaction and domain boundaries readable.
  The fixed configuration and generated volume are accepted because generated code is mechanically
  maintained; the decision is based on safer refactoring and a faster compiler-like feedback loop,
  not on counting generated lines as handwritten maintenance.
- **Measurement:** The integrated broad pass reduced handwritten production Go from 12,062 to 11,973
  lines (-89) and tests from 7,670 to 7,651 lines (-19). Ten named query files contain 1,210 lines;
  the committed private generated package contains 4,960 lines; and the three new tool/config entry
  points contain 81 lines. Local generation and stale-code diffing each take about 0.6 seconds. All
  188 stable product query call sites moved behind sqlc. The 12 remaining direct Store calls are the
  explicit Absurd/bootstrap, schema-application, and Job advisory-lock exceptions.
- **Reconsider when:** A supported query cannot be expressed without retaining a parallel handwritten
  implementation, generator churn repeatedly obscures review, or future measurements show the
  handwritten configuration and conversion surface outweighs the query plumbing it replaces without
  delivering useful drift failures.
