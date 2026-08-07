# Storage Rules

Read this file before changing SQLite, PostgreSQL, CockroachDB, DuckDB, archive
resync, or storage queries.

## SQLite Archive

SQLite is the persistent archive. Never delete, drop, truncate, or recreate it
to handle a data-version change.

Use non-destructive schema migrations such as `ALTER TABLE` and `UPDATE`. A
parser change that needs a full resync must build a fresh database, sync source
files, copy orphaned sessions from the old database, and swap the files
atomically. Preserve sessions even when their source files no longer exist.

## Backend Parity

- Keep observable behavior and query shape aligned between SQLite and
  PostgreSQL/CockroachDB when practical. Match queries, indexes, aggregations,
  filters, and ordering unless a documented constraint requires a difference.
- Do not fix correctness or performance in only one primary backend unless the
  user limits the task to that backend. If implementations must differ,
  explain why and preserve the same behavior.
- DuckDB is a derived mirror and is not part of this parity rule.

## Canonical Bun Ownership

- The Bun model registry in `internal/db/bunmodel` owns the logical schema
  shared by SQLite, PostgreSQL, and DuckDB: table names, columns, types,
  defaults, constraints, and indexes. It does not require every operational
  query to have identical SQL. Do not add a backend-local copy of a common
  table or column projection.
- `db.BunStore` owns every server-facing `db.Store` query, scan, reduction, and
  supported mutation. Concrete stores may add lifecycle, synchronization,
  operational metadata, and narrowly scoped full-text or vector capabilities;
  they must not shadow a common Store method.
- All application query execution and transactions flow through guarded
  `bun.IDB` handles. Raw SQL constructed with `bun.IDB.NewRaw` is still
  Bun-owned execution: it retains dialect formatting, query hooks, and the
  backend's snapshot or serialization guard. SQLite's `Reader` and writer
  facade are Bun-backed for the same reason.
- Direct `database/sql` access is limited to opening and configuring driver
  pools, connection-local commands such as SQLite `PRAGMA` and `ATTACH` or
  DuckDB `USE`, handle swap/drain/close lifecycle, connector state, and
  unavoidable compatibility or capability probes. Keep each such seam inside
  its backend adapter and document why Bun cannot own it.
- Backend-specific query construction is limited to this closed set of seams:
  lifecycle and connection-local operations; canonical schema creation,
  convergence, and validation; replication or mirror synchronization and
  operational metadata; adapter-supplied timestamp ordering and compatibility
  or capability probes; and narrowly scoped full-text or vector
  implementations. Keep each difference behind the backend adapter or search
  capability boundary. The server-facing Store policy, filtering, hydration,
  and reduction remain shared. A new non-search seam requires an explicit
  design update; a backend-specific query plan alone is not one.

### Bun placeholders

- Write Bun placeholders (`?` or indexed `?0`, `?1`, and so on) in every query
  executed through Bun. Never pass driver-native placeholders such as
  PostgreSQL `$1`; Bun must format values for the active dialect.
- Escape a literal question mark as `\?` so Bun does not consume it as a
  placeholder. Use indexed placeholders when one argument is referenced more
  than once.
- Use `bun.List` for portable bounded value lists. PostgreSQL-native arrays use
  `pgdialect.Array` with forms such as `= ANY(?0)` when the adapter genuinely
  needs array semantics; do not pass an ordinary Go slice to a scalar
  placeholder.
- Bun formats arguments into the SQL sent to the driver. Large lists therefore
  enlarge the formatted query and its hook/log record instead of becoming a
  driver-side bind array. Chunk bounded reads and writes, keep sensitive
  values out of ad hoc logging, and inspect the formatted query when
  diagnosing placeholder or dialect failures.

## DuckDB Mirror

- Treat DuckDB as a disposable read mirror of SQLite, never as a system of
  record. Deleting the mirror must lose nothing.
- Do not add in-place mirror migrations. A schema or source-data version change
  must bump `internal/duckdb.SchemaVersion`, rebuild a fresh file, validate
  it, and swap it atomically. Do not add `ALTER` migrations, version-bridging
  reads, or compatibility shims for old mirrors.
- Store every DuckDB push cursor and version in the mirror's `sync_metadata`.
  Never store DuckDB sync state in SQLite.
- Replace whole sessions during incremental updates and gate them with
  per-session fingerprints. Do not add per-table, per-column, or diff-based
  updates.
- Keep Quack read-only. `duckdb push` writes the local mirror; it never writes
  to a remote DuckDB service.
- Replace a file only after identifying it as an agentsview DuckDB mirror. Fail
  closed for unknown files.

## PostgreSQL Integration Tests

Run PostgreSQL integration tests only against a dedicated test database. The
tests create and drop the `agentsview` schema.

Use `make test-postgres` to start the test container and run the suite. It
leaves the container running. If you started that container, use
`make postgres-down` when it is no longer needed.

To use an existing dedicated instance, run:

```bash
TEST_PG_URL="postgres://user:pass@host:5432/dbname?sslmode=disable" \
  CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/... -v
```
