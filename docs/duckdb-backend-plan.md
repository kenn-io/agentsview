# DuckDB Backend Plan

Status: planned

Last checked: 2026-06-08

## Context

agentsview currently has two storage modes:

- SQLite is the local primary archive. File sync, parser writes, full resync,
  trash, insights, upload, and per-session repair all write through
  `internal/db`.
- PostgreSQL is an optional shared backend. `agentsview pg push` mirrors local
  SQLite into PG, and `agentsview pg serve` exposes the web UI from a read-only
  `db.Store` implementation under `internal/postgres`.

DuckDB should follow the PostgreSQL shape first: keep SQLite as the primary
ingestion source, mirror data into DuckDB, and serve the UI from a DuckDB
read-store. That makes local DuckDB files and remote Quack endpoints useful
without forcing the parser/sync engine to support multiple primary write paths
up front.

## Quack Notes

DuckDB released Quack on 2026-05-12. The protocol turns a DuckDB instance into
an HTTP-accessible server that other DuckDB instances can connect to. DuckDB
v1.5.3 ships Quack as a core extension, but it is still beta/experimental and
the protocol, function names, and defaults may change before DuckDB v2.0.0.

Useful current behavior:

- Quack uses `quack:` URIs and defaults to port `9494`.
- Local URIs use plain HTTP by default; non-local URIs use HTTPS by default.
- Clients authenticate with either a Quack secret or explicit `TOKEN` on
  `ATTACH` / `quack_query`.
- `ATTACH 'quack:host' AS remote` exposes remote tables as a DuckDB catalog and
  forwards transactions.
- The current DuckDB Go driver is `github.com/duckdb/duckdb-go/v2`; the first
  tag carrying DuckDB v1.5.3 is `v2.10503.0`.

Primary references:

- https://duckdb.org/docs/current/quack/overview
- https://duckdb.org/docs/current/core_extensions/quack
- https://duckdb.org/2026/05/20/announcing-duckdb-153
- https://github.com/duckdb/duckdb-go

## Recommended Shape

Add a new `internal/duckdb` package, mirroring the responsibilities of
`internal/postgres` without copying implementation blindly.

The package should provide:

- Connection helpers for local DuckDB files and remote `quack:` endpoints.
- Non-secret diagnostics that redact tokens and local filesystem details.
- DuckDB DDL and schema compatibility checks.
- Push sync from SQLite to DuckDB with the same machine/project filtering
  semantics as PG push.
- A `db.Store` implementation for `duckdb serve`.
- Quack serving support for exposing a local DuckDB file when a user opts in.

Before the DuckDB `db.Store` port, extract the narrow query helpers that are
already duplicated between SQLite and PostgreSQL. This should cover dialect
differences such as placeholder style, boolean literals, timestamp/date
expressions, LIKE/ILIKE behavior, regex predicates, pagination, NULL ordering,
and safe catalog/table qualification. It should not grow into an ORM, and
backend-specific SQL should stay local where the engines genuinely differ.

The CLI should add a sibling command group:

```text
agentsview duckdb push
agentsview duckdb status
agentsview duckdb serve
agentsview duckdb quack serve
```

Configuration should be explicit and separate from PG:

```toml
[duckdb]
path = "~/.agentsview/sessions.duckdb"
url = "quack:localhost"
token = "$AGENTSVIEW_DUCKDB_TOKEN"
machine_name = "my-machine"
allow_insecure = false
projects = []
exclude_projects = []
```

Environment variables should follow the existing naming style:

```text
AGENTSVIEW_DUCKDB_PATH
AGENTSVIEW_DUCKDB_URL
AGENTSVIEW_DUCKDB_TOKEN
AGENTSVIEW_DUCKDB_MACHINE
```

## Implementation Phases

1. Prove the DuckDB driver and Quack protocol from Go. Create a small
   integration spike that opens DuckDB through `database/sql`, creates a local
   DuckDB file, starts a Quack server from one connection, attaches it from
   another connection, and verifies a read/write round trip.

1. Add config and CLI skeleton. Add `DuckDBConfig`, config-file/env loading,
   resolver defaults, command help, and empty command handlers that fail clearly
   when required settings are missing.

1. Add DuckDB schema and push sync. Port the PG mirror schema to DuckDB SQL,
   preserve local SQLite watermarks, implement full and incremental push, and
   keep project include/exclude filtering aligned with PG.

1. Add backend contract tests and shared query helpers. Use a contract suite to
   compare `db.Store` behavior across backends, then extract the common
   session-filter, child-expansion, cursor-pagination, and content-search
   scoping fragments needed before DuckDB becomes a third SQL implementation.

1. Add the DuckDB read store. Implement `db.Store` over DuckDB for sessions,
   messages, tool calls, search, secrets, metadata, analytics, trends, usage,
   stars, and pins. Start read-only for destructive session-management methods.

1. Add remote Quack serving and consuming. Support `duckdb serve` from either a
   local DuckDB file or a remote `quack:` URI. Add `duckdb quack serve` for
   exposing the local DuckDB mirror with explicit token handling and safe local
   defaults.

1. Add operational docs, Docker examples, and end-to-end validation. Document
   the beta Quack constraints, TLS expectations, remote token handling, and how
   DuckDB differs from SQLite primary mode and PG shared mode.

## Important Non-goals For The First Cut

- Do not replace SQLite as the primary file-sync archive.
- Do not make parser writes target DuckDB directly.
- Do not expose Quack beyond loopback without an explicit token and TLS/proxy
  story.
- Do not require Quack for local DuckDB file mode.
- Do not depend on DuckDB FTS parity before the read backend works; use
  substring/regex search first and add a separate search optimization task if
  needed.

## Validation Targets

- `go test ./internal/duckdb/...` with a local DuckDB file.
- `go test -tags "duckdbtest" ./internal/duckdb/...` for Quack integration.
- `go test ./internal/config ./cmd/agentsview`.
- A fixture push from SQLite to DuckDB followed by `duckdb serve` API checks for
  session list, session detail, messages, search, analytics, usage, stars, and
  pins.
- Manual Quack smoke test: local server on loopback, token-protected remote
  attach, and `duckdb serve` against the `quack:` endpoint.
