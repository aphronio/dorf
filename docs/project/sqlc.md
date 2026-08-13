# sqlc working guide

Read this before changing Dorf's PostgreSQL schema, named queries, or `postgres.Store` mappings.

## Boundary

```text
Store method -> generated query -> PostgreSQL
     |
     +-- owns transactions, invariants, errors, and domain mapping
```

- The baseline schema and `internal/postgres/queries/*.sql` are handwritten source.
- `internal/postgres/dbsql` is private generated code. Commit it, but never edit it directly.
- Generated rows and parameter structs stay inside `internal/postgres`; Store methods return `spine`
  types.
- Store methods start transactions and bind queries with `dbsql.New(s.DB).WithTx(tx)`.
- Direct SQL is reserved for Absurd/bootstrap, schema application, and PostgreSQL advisory locks.

## Query rules

- Give every parameter a stable name with `sqlc.arg(...)`. Use `sqlc.narg(...)` only when null is
  genuinely part of the input contract.
- Choose the narrow annotation that matches the result: `:one`, `:many`, `:exec`, or `:execrows`.
  Use `:execrows` whenever affected-row count enforces a compare-and-set invariant.
- Keep nullable database values explicit with `sql.Null*` at the Store boundary. Use `coalesce` only
  when the domain intentionally treats null as a concrete zero or empty value.
- Use column type overrides only for existing non-null domain scalars. Do not generate domain records,
  add PostgreSQL enums for Go typing, or globally override PostgreSQL primitives.
- Use `sqlc.embed` only for a real reusable full-row projection. Prefer explicit query results for
  ordinary domain views.
- Do not add generated interfaces, JSON tags, prepared queries, pointer results, plugins, or cloud
  features without a concrete consumer.

## Feedback loop

```bash
.dorf/bin/mise run sql:generate  # after schema or query edits
.dorf/bin/mise run sql:check     # generated files match their inputs; no database needed
.dorf/bin/mise run check         # live query preparation, Go tests, and Go vet
```

`sqlc.yaml` deliberately uses the local analyzer for generation and diffing. Live `sqlc/db-prepare`
vetting uses the migrated disposable PostgreSQL database. `sqlc push` and `sqlc verify` are not part
of Dorf because they require sqlc Cloud.

## Official reference

- [Configuration](https://docs.sqlc.dev/en/stable/reference/config.html)
- [Named parameters](https://docs.sqlc.dev/en/stable/howto/named_parameters.html)
- [Query annotations](https://docs.sqlc.dev/en/stable/reference/query-annotations.html)
- [Transactions](https://docs.sqlc.dev/en/stable/howto/transactions.html)
- [Type overrides](https://docs.sqlc.dev/en/stable/howto/overrides.html)
- [PostgreSQL data types and nullability](https://docs.sqlc.dev/en/stable/reference/datatypes.html)
- [Vetting queries](https://docs.sqlc.dev/en/stable/howto/vet.html)
- [CI and generated-code checks](https://docs.sqlc.dev/en/stable/howto/ci-cd.html)
