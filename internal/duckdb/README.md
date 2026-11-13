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
session-batch transactions. Sessions, messages, tool calls, result events, and
usage events use native appenders with a shared 1 MiB payload flush bound. An
oversized row is flushed alone, without truncation. These limits do not bound
the existing whole-session source load.

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

## Signals frustration markers

Push stores frustration marker counts for each session. A change to the marker
algorithm rebuilds the mirror on the next push.

## Usage rollup

Usage summary, Top sessions, and usage session counts read a rollup that a push
builds, so a read after a push does not rank and price every usage row again.
Per-session usage and Activity reports read the live tables.

Usage summary and Top sessions keep their per-session results in the on-disk
usage cache. The rollup fills that cache when it is empty; a later read
recomputes only the sessions whose push fingerprint changed, from the live
tables.

The rollup holds priced usage summed by session, 15-minute UTC bucket, model,
and provider, the pricing provenance of those rows, raw copies of the rows it
could not sum, and the fingerprint of every session it covers. Reads sum the
buckets into local days for the requested zone.

A push rebuilds the whole rollup when the mirror has none, when its format or
the mirror's pricing changed, or when the usage rows of sessions changed since
the build reach 1/50 of the rows it covers. Between rebuilds, reads take new and
changed sessions from the live tables and skip the rollup rows of deleted ones.

A dedup group is summed only when no window, filter, or later row can change
which of its rows wins:

- all its rows belong to one session and one bucket;
- a non-Claude group has one model, because model filters apply before its
  ranking;
- no row carries a Copilot-reported session cost; and
- no Cursor account row shares its identity.

Other groups keep their rows, which reads rank with the live rows. A read also
sends live the (session, bucket) units whose summed groups now share an identity
with a changed session's row or a Cursor account row.

A read uses the live path when the rollup's pricing differs from its own (for
example, custom pricing), or when the zone's offset over the read's range is not
a multiple of 15 minutes or changes off a 15-minute UTC boundary.
