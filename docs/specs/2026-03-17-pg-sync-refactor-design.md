# PostgreSQL Sync Refactor Design

## Overview

Refactor the PostgreSQL sync feature to improve ergonomics, fix
semantic issues, consolidate packages, and enable a deployable
read-only server mode backed by PostgreSQL.

## Changes

### 1. CLI Command Structure

Replace overloaded `sync --pg*` flags and `serve --pg-read` with a
unified `pg` command group:

```
agentsview pg push [flags]     Push local data to PostgreSQL
agentsview pg status           Show sync state (last push, row counts)
agentsview pg serve [flags]    Start read-only server backed by PostgreSQL
```

**`pg push`**: Runs a local sync first (to discover new sessions), then
pushes to PG. Accepts `--full` to bypass the per-message fingerprint
heuristic and force a complete re-push.

**`pg status`**: Shows last push timestamp, PG session/message counts,
machine name. Read-only, no side effects.

**`pg serve`**: Starts an HTTP server serving the SPA and API from
PostgreSQL. No local SQLite, no sync engine, no file watcher.

**Removed behaviors**:

- `sync --pg` and `sync --pg-status` flags removed from the `sync`
  subcommand. The `sync` command returns to its original purpose:
  syncing local session data into SQLite.
- `serve --pg-read <url>` flag removed from the `serve` subcommand.
- Automatic periodic PG push in server mode removed. The server no
  longer starts a background PG sync goroutine. Pushing is always
  explicit via `pg push`. Users who want periodic push use cron.

### 2. Config Format Migration (JSON to TOML)

Migrate the config file from `config.json` to `config.toml`.

**File**: `~/.agentsview/config.toml`

**Auto-migration**: On startup, if `config.json` exists and
`config.toml` does not, read the JSON file, write equivalent TOML,
rename the JSON file to `config.json.bak`. If both exist, TOML wins
and JSON is ignored.

**PG section** (modeled after roborev's `[sync]` section):

```toml
[pg]
url = "${AGENTSVIEW_PG_URL}"
schema = "agentsview"
machine_name = "west-mbp"
allow_insecure = false
```

Fields:

- `url`: PostgreSQL connection string. Supports `${VAR}` expansion
  (matching roborev's convention).
- `schema`: PG schema name (default `"agentsview"`). Created
  automatically by `pg push` if it does not exist.
- `machine_name`: Identifier for this machine in multi-machine setups.
  Defaults to `os.Hostname()` if omitted.
- `allow_insecure`: Permit non-TLS connections to non-loopback PG
  hosts (default `false`).

Removed fields (no longer needed):

- `enabled`: Push is on-demand, so there is no daemon to enable or
  disable.
- `interval`: No periodic push.

**Environment variables** (override config file values):

- `AGENTSVIEW_PG_URL` — connection string
- `AGENTSVIEW_PG_SCHEMA` — schema name
- `AGENTSVIEW_PG_MACHINE` — machine name

All existing config fields (`host`, `port`, `proxy.*`,
`watch_exclude_patterns`, directory overrides, etc.) migrate 1:1 from
JSON keys to TOML keys with no structural changes.

### 3. Package Consolidation

Merge three packages into one:

```
internal/pgdb/      ─┐
internal/pgsync/    ─┼─→  internal/postgres/
internal/pgutil/    ─┘
```

File layout:

| File | Contents |
|------|---------|
| `postgres/connect.go` | Connection setup, SSL checks, DSN redaction, `search_path` on connect |
| `postgres/schema.go` | DDL, `EnsureSchema`, `CheckSchemaCompat` |
| `postgres/push.go` | Push logic, fingerprinting, boundary state |
| `postgres/sync.go` | `PGSync` struct, `Push` entry point |
| `postgres/store.go` | `Store` struct (read-only `db.Store` implementation) |
| `postgres/sessions.go` | Session list/detail queries |
| `postgres/messages.go` | Message queries, ILIKE search |
| `postgres/analytics.go` | Analytics queries |
| `postgres/time.go` | Timestamp parse/format utilities |

Both `PGSync` (push side) and `Store` (read side) share connection
setup from `connect.go`. The schema name flows through as a parameter
to connection setup, which sets `search_path = <schema>` on each
connection. All queries use unqualified table names.

### 4. Native PostgreSQL Timestamps

Replace `TEXT` timestamp columns with `TIMESTAMPTZ`.

**Affected columns** (all tables):

- `sessions`: `created_at`, `started_at`, `ended_at`, `deleted_at`,
  `updated_at`
- `messages`: `timestamp`
- `sync_metadata`: no timestamp columns (key-value text, unchanged)

**Push side**: SQLite stores timestamps as ISO-8601 text. On push,
parse to `time.Time` in Go and pass to pgx. Null or empty strings
become SQL `NULL`.

**Read side**: pgx scans `TIMESTAMPTZ` into `time.Time`. Format back
to ISO-8601 strings for JSON API responses. The frontend API contract
does not change.

**Benefits**:

- Analytics queries use native PG time functions (`date_trunc`,
  `EXTRACT`, `AT TIME ZONE`) instead of `SUBSTR`/`LEFT`/`CASE` string
  manipulation.
- Proper time-range indexing.
- The timestamp normalization code in `time.go` and format version
  tracking in `sync_metadata` are deleted. PG handles it natively.

There are no existing PG databases to migrate, so the schema is
defined with `TIMESTAMPTZ` from the start.

### 5. pg serve and Deployment Model

`agentsview pg serve` starts a read-only HTTP server backed by
PostgreSQL.

```bash
agentsview pg serve --port 8090
```

**Behavior**:

- Reads `[pg]` config section for `url` and `schema` (same config
  source as `pg push`).
- Binds to `127.0.0.1` by default. `--host` overrides with no
  restriction — auth is the deployer's responsibility.
- No sync engine, no file watcher, no local SQLite.
- Write API endpoints return HTTP 501 (stars, pins, insights, rename,
  delete, trash, etc.).
- Read endpoints work: sessions, messages, search (ILIKE), analytics,
  SSE version polling.
- Frontend receives `read_only: true` in the version API response to
  disable write UI controls.

**Intended deployment** (example with Tailscale + Caddy):

```
[Tailscale] → [Caddy on host] → [agentsview pg serve on 127.0.0.1:8090]
                handle_path /agentsview/*
```

agentsview does not manage TLS, auth, or subpath routing. The
deployer's existing reverse proxy and network security layer handle
those concerns.

### 6. Not in Scope

These are tracked as follow-up GitHub issues, not part of this
refactor:

- **Write support in pg serve**: Implementing stars, pins, insights,
  rename, delete, and trash operations against PostgreSQL. The
  `db.Store` interface already defines the boundaries for these.
- **Subpath-aware routing**: The Go server serves at `/`. Subpath
  stripping is handled by the deployer's reverse proxy
  (`handle_path`).
- **Managed Caddy for pg serve**: pg serve relies on external
  proxy infrastructure. The existing managed Caddy mode continues to
  work for normal (SQLite-backed) server mode.
- **Database identity tracking**: Detecting when the PG URL changes
  and clearing watermarks to trigger a full re-push. For now, use
  `pg push --full` after re-pointing.
