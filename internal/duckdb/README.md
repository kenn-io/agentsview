# DuckDB Backend

This package implements the optional DuckDB mirror backend. SQLite remains the
primary archive for parser ingestion and file sync; DuckDB is populated by
`agentsview duckdb push` and read by `agentsview duckdb serve`.

The backend supports two read paths:

- local file mode via `[duckdb].path` or `AGENTSVIEW_DUCKDB_PATH`
- remote Quack mode via `AGENTSVIEW_DUCKDB_URL` plus `AGENTSVIEW_DUCKDB_TOKEN`

Quack is treated as beta infrastructure. Serving defaults to loopback, uses an
explicit or generated token, and rejects plain non-loopback binds unless
`--allow-insecure` is set. Remote operators should prefer TLS or a trusted
tunnel/proxy.

The DuckDB schema intentionally avoids `TIMESTAMP DEFAULT current_timestamp`
columns because current Quack attach rejects catalogs with those dynamic
defaults. Writers supply `current_timestamp` explicitly where the mirror needs a
created timestamp. A schema-version change builds a fresh mirror, validates it,
and swaps it into place atomically. `EnsureSchema` initializes fresh mirrors; it
is not an in-place production migration path.

## Mirror writes

Full and incremental pushes replace complete sessions in the existing
session-batch transactions. Messages, tool calls, result events, and usage
events use multi-row inserts capped at 100 rows or 1 MiB of variable-sized
payload per statement. An oversized row is inserted alone, without truncation.
These limits do not bound the existing whole-session source load.

Errors and cancellation roll back the whole transaction; chunks never commit
independently.

The generated full-mirror benchmark checks stored row counts outside its timed
`Push` calls. Run it with:

```bash
go test -tags fts5 ./internal/duckdb -run '^$' -bench '^BenchmarkMirrorPush$' -benchtime=1x -count=3
```

Search currently keeps substring/regex fallback behavior. The DuckDB FTS
extension is available locally in the pinned runtime, but BM25 lookup does not
resolve through Quack-attached catalogs, so indexed DuckDB search is deferred
until local and remote behavior line up.

Run the local file smoke test with:

```bash
go test -tags fts5 ./internal/duckdb -v
```

Run the Quack loopback integration test with:

```bash
go test -tags duckdbtest ./cmd/agentsview ./internal/duckdb -run Quack -v
```

The Quack test starts a loopback server with an explicit token, attaches it from
a second DuckDB connection, verifies a remote read, writes through the attached
catalog, and stops the server in test cleanup.

Run the DuckDB-backed browser smoke test with:

```bash
make e2e-duckdb
```
