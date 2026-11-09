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

The Signals panel counts each session's frustration markers, the prompts
`signals.IsFrustrationMarker` flags. Counts are kept in memory per push
fingerprint, so a load reads only sessions that changed since the last one.

For those sessions one query (`duckFrustrationPromptsSQL`) judges most prompts
in DuckDB:

- A prompt made only of ASCII and `signals.PlainPromptSymbols` (dashes, quotes,
  arrows, check marks, box drawing, emoji, private use) is judged in SQL. It
  comes back only when it is a marker, without its text. ClickHouse uses the
  same set and the same steps.
- Any other prompt comes back with its text, and Go judges it. DuckDB stores
  only valid UTF-8, so no prompt is sent back for its encoding.

The SQL follows `IsFrustrationMarker` with its exported patterns and thresholds.
DuckDB's regular expressions are RE2: they match UTF-8, fold case under `(?i)`
as Go does, and keep `\b` and `\w` to ASCII. `strlen` counts bytes as Go's `len`
does; `length` would count characters. The query comment lists the three steps
and why each matches Go over a plain prompt.

The query ends in `ORDER BY session_id`. Without that pipeline breaker, DuckDB
evaluates the marker expression on one thread as the result streams to Go.

The patterns are parameters, never part of the query text. Quack renders
parameters into the SQL by replacing each question mark, and the patterns
contain question marks. The session ID list renders as a `VARCHAR[]` literal.

Tests compare the counts with `IsFrustrationMarker` on hand-labeled edge cases
and on a seeded random corpus, require that only prompts outside the set come
back with text, and check that DuckDB's `lower`, `upper`, and `(?i)` leave every
symbol in the set alone. A Quack test runs the same read over a served mirror.

On a generated mirror of 2,000 sessions with 379 MB of prompt text and 1.57 GB
on disk, judging in DuckDB took the read from 3.93 s to 1.22 s, Go allocations
from 3.80 GB to 134 MB, and warm peak RSS growth from about 400 MB to 30-170 MB,
with the same counts and about 3% more CPU time. On a mirror whose prompts fill
only four row groups, wall time is unchanged, because DuckDB parallelizes by row
group.
