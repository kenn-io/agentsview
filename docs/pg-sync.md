---
title: PostgreSQL Sync
description: Share sessions across machines with PostgreSQL push sync, an auto-push service, and a read-only server
---

AgentsView stores sessions locally in SQLite by default. PostgreSQL
sync lets you push sessions from one or more machines into a shared
PostgreSQL database, keep that database current with an optional
auto-push watcher or OS service, then serve a read-only web UI from
it — useful for team dashboards or multi-machine setups.

The sync direction is one-way: SQLite to PostgreSQL. Each machine
pushes its own sessions; `pg serve` reads from the shared database.
The resulting UI includes the session browser, analytics dashboard,
search, and, as of 0.23.0, the Usage dashboard as well.

## Quick Start

### 1. Configure PostgreSQL

Add a `[pg]` section to `~/.agentsview/config.toml`:

```toml
[pg]
url = "postgres://user:pass@host:5432/dbname?sslmode=require"
machine_name = "my-laptop"
```

The `machine_name` identifies which machine pushed each session.
It defaults to the system hostname if omitted. It must not be
`"local"` (reserved for the local SQLite sentinel).

For multiple PostgreSQL destinations, use named `[pg.NAME]` blocks and
`default_pg` instead of the legacy single `[pg]` block. Named target names
are normalized case-insensitively, and `all`, `local`, plus the legacy `[pg]`
field names `url`, `schema`, `machine_name`, `allow_insecure`, `projects`, and
`exclude_projects` are unavailable as `[pg.NAME]` names.

### 2. Push Sessions

```bash
agentsview pg push
```

This one-shot command syncs all local sessions, messages, and tool
calls to PostgreSQL. The schema is created automatically on first
push.

To keep PostgreSQL current automatically, run the foreground watcher:

```bash
agentsview pg push --watch
```

Or install it as a per-user background service on macOS or Linux:

```bash
agentsview pg service install
```

### 3. Serve the Dashboard

```bash
agentsview pg serve
```

Opens the read-only web UI at `http://127.0.0.1:8080`, backed
entirely by PostgreSQL. No local SQLite, file watching, or uploads
— just the viewer.

---

## Commands

### `agentsview pg push`

Sync sessions from the local SQLite database to PostgreSQL.

```bash
agentsview pg push [target] [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--all` | `false` | Push every configured PG target sequentially |
| `--full` | `false` | Force full local resync and re-push, bypassing change detection |
| `--no-vectors` | `false` | Skip the semantic-search vector phase for this run |
| `--projects` | | Comma-separated projects to push (inclusive) |
| `--exclude-projects` | | Comma-separated projects to exclude |
| `--all-projects` | `false` | Ignore configured project filters for this run |
| `--watch` | `false` | Run continuously, pushing on change plus a periodic floor |
| `--debounce` | `30s` | Coalesce window after a filesystem change before pushing (`--watch` only) |
| `--interval` | `15m` | Periodic floor push interval (`--watch` only) |

Without `--watch`, push is on-demand — run it whenever you want
to sync. With `--watch`, the command stays in the foreground and
keeps pushing until interrupted.

When no target is passed, `pg push` uses the effective default target.
Pass one named target explicitly to push just that destination, or use
`--all` to fan out across every configured target. `--all --watch` is
rejected.

**What happens on push:**

1. Runs a local sync to pick up any new or modified session files
2. Compares local sessions against the PostgreSQL watermark to
   find what changed since the last push
3. Upserts sessions, messages, tool calls, and (as of 0.30.0)
   [curation metadata](#curation-metadata) in batches of 50
4. Advances the watermark timestamp on success

Incremental pushes use a two-layer fingerprint to skip
unchanged sessions: first, session metadata fields (project,
agent, timestamps, message counts) are compared; then, per-
session message statistics (count, content length sum/max/min,
system message ordinals, tool call counts) are checked against
PostgreSQL. Use `--full` to bypass both layers and re-push
everything — for example, after a schema reset or when message
content was rewritten in place.

If any sessions fail to push, the watermark is not advanced so
they are retried on the next run. The exit code is 1 when any
errors occur, 0 otherwise.

#### Automatic Push Watcher

As of 0.32.0, `agentsview pg push --watch` runs a long-lived
auto-push daemon in the foreground:

```bash
agentsview pg push --watch
agentsview pg push --watch --debounce 1m
agentsview pg push --watch --interval 5m
```

The watcher performs one initial local sync plus PostgreSQL push,
then pushes again after session-directory changes settle for the
debounce window. The interval acts as a floor: even if filesystem
events are missed or a root cannot be watched because of OS watch
limits, the next interval push catches up.

Operational details:

- Only one watcher can run per AgentsView data directory; a
  runtime lock prevents competing pushes from racing watermarks.
- PostgreSQL connections are opened lazily and reset after errors,
  so a transiently unavailable database is retried on the next
  trigger instead of crashing the watcher.
- On shutdown (`Ctrl+C`, `SIGTERM`), the watcher attempts one
  bounded final flush.
- Logs are written to `pg-watch.log` under the AgentsView data
  directory.
- The watcher uses the selected PostgreSQL target, or the
  `default_pg` target when no name is passed, along with the same
  machine name, project filters, classifier settings, and
  `result_content_blocked_categories` behavior as one-shot
  `pg push`.

#### Project Filtering

By default, `pg push` syncs all projects. Use project filters
to push a subset:

```bash
# Push only these projects
agentsview pg push --projects alpha,beta

# Push everything except this project
agentsview pg push --exclude-projects scratch

# Ignore config-file filters for this run
agentsview pg push --all-projects
```

`--projects` and `--exclude-projects` are mutually exclusive.
`--all-projects` cannot be combined with either.

Project filters can also be set in `config.toml` so you don't
need to pass them on every run:

```toml
[pg]
url = "postgres://..."
projects = ["alpha", "beta"]
# or: exclude_projects = ["scratch"]
```

CLI flags override config values. Use
[`agentsview projects`](/commands/#agentsview-projects) to list
available project names.

Filtered pushes keep their own local push watermark for each
target/filter set. For example, repeated
`agentsview pg push --projects alpha,beta` runs use a different
watermark from unfiltered pushes and from
`agentsview pg push --projects gamma`. This keeps allow-list pushes
incremental without advancing the unfiltered/global cursor.

After upgrading from an older version, the first filtered push for a
given project set may still scan the matching local sessions once to
seed that scoped watermark. Later pushes with the same filter set use
the scoped watermark.

#### Curation Metadata

As of 0.30.0, `pg push` also synchronizes two pieces of
per-user curation state alongside session content:

| PostgreSQL table | What it holds |
|------------------|---------------|
| `starred_sessions` | One row per starred session: `session_id`, `created_at` |
| `pinned_messages` | One row per pin: `id`, `session_id`, `message_id`, `ordinal`, `source_uuid`, `note`, `created_at` |

Stars are keyed by session ID and overwrite cleanly across
machines. Pins are reconciled by `source_uuid` — a stable
identifier derived from the underlying message — so a pin
survives a re-parse that shifts message ordinals. Without
this, pins would silently drift to the wrong message after
any session resync.

Curation tables are populated by the same `pg push` run; no
separate command or flag is required. The
[`agentsview secrets`](/commands/#agentsview-secrets) findings
also push through this codepath, with the same parity
guarantees as session content (`secret_findings` table,
per-session `secret_leak_count`, and the
[`has-secret`](/session-api/#agentsview-session-list) list
filter).

#### Vector Push

When `[vector]` is enabled locally, `pg push` runs a vector phase
after the session and message phases, copying the machine's
active embedding generation from `vectors.db` into pgvector so a
shared PostgreSQL deployment can answer `--semantic`/`--hybrid`.
Only changed sessions are re-sent. Skip the phase for one run
with `--no-vectors`, or disable it persistently with
`push_vectors = false` under `[pg]`. Databases without pgvector
(for example CockroachDB) skip the phase and keep syncing session
content. See
[semantic search: PostgreSQL](/semantic-search/#postgresql) for
the full push, serve, and maintenance workflow.

`pg vectors list` and `pg vectors drop <id>` inspect and remove
pushed generations.

### `agentsview pg status`

Show the current sync state.

```bash
agentsview pg status [target] [flags]
agentsview pg status --all
agentsview pg status --projects alpha,beta
```

Without a target name, `pg status` uses the effective default target.
Pass one named target explicitly to inspect that destination, or use
`--all` to print every configured target sequentially.

Use the same `--projects`, `--exclude-projects`, or `--all-projects`
filter flags as `pg push` to inspect the matching filtered or
unfiltered watermark.

Output:

```
Machine:     my-laptop
Last push:   2026-03-24T10:30:00Z
PG sessions: 1842
PG messages: 47291
```

| Field | Description |
|-------|-------------|
| Machine | Configured machine name or hostname |
| Last push | Timestamp of last successful push ("never" if no push yet) |
| PG sessions | Total session count in PostgreSQL (all machines) |
| PG messages | Total message count in PostgreSQL (all machines) |

### `agentsview pg service`

Install and manage the [`pg push --watch`](#automatic-push-watcher)
auto-push daemon as a per-user OS service.

```bash
agentsview pg service install
agentsview pg service status
agentsview pg service logs -f
agentsview pg service stop
agentsview pg service start
agentsview pg service uninstall
```

Supported service managers:

| Platform | Manager |
|----------|---------|
| macOS | launchd LaunchAgent |
| Linux | `systemd --user` unit |

The generated unit runs `agentsview pg push --watch`, pins
`AGENTSVIEW_DATA_DIR` to the data directory used at install time,
and writes logs to `~/.agentsview/pg-watch.log` unless you changed
the data directory.

`install` requires a literal PostgreSQL URL in the effective default target of
`~/.agentsview/config.toml`, either the legacy `[pg].url` or the target selected
by `default_pg` from named `[pg.NAME]` blocks. It intentionally rejects
`AGENTSVIEW_PG_URL` and environment-expanded URLs such as
`${PG_PASSWORD}` because background services do not inherit your
interactive shell environment. Other session-directory environment
variables are not copied into the unit either; put persistent
settings in `config.toml` before installing.

On headless Linux machines, `systemd --user` services stop at
logout and do not start at boot unless user lingering is enabled.
If lingering is disabled, `install` prints the exact
`loginctl enable-linger "$USER"` command and offers to run it.

### `agentsview pg serve`

Start a read-only web UI backed by PostgreSQL.

```bash
agentsview pg serve [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--host` | `127.0.0.1` | Bind address |
| `--port` | `8080` | Port |
| `--base-path` | | URL prefix for reverse-proxy subpath |
| `--public-url` | | Public-facing URL for proxy access |
| `--public-origin` | | Trusted browser origin (repeatable/comma-separated) |
| `--public-port` | `8443` | External port for managed proxy |
| `--proxy` | | Managed proxy mode (`caddy`) |
| `--caddy-bin` | `caddy` | Caddy binary path |
| `--proxy-bind-host` | `0.0.0.0` | Caddy bind address |
| `--tls-cert` | | TLS certificate path |
| `--tls-key` | | TLS key path |
| `--allowed-subnet` | | CIDR allowlist (repeatable/comma-separated) |

The server is read-only — uploads, file watching, and local
sync are all disabled. Sessions from all machines appear in
a single unified view. The same deployment also serves the
analytics dashboard and the Usage page from PostgreSQL-backed
queries.

On startup, `pg serve` automatically applies any pending
schema migrations to PostgreSQL, creating new tables and
indexes added in newer AgentsView versions. This removes
the need to run `pg push` before starting the server after
an upgrade. If the PostgreSQL role is read-only, the
migration is skipped and the server falls back to the
schema compatibility check.

When `require_auth` is enabled, a bearer token is generated if
needed and printed on startup. Pass it via
`Authorization: Bearer <token>` on API requests. The SSE watch
endpoint also accepts `?token=<token>` as a query parameter since
the `EventSource` API cannot set custom headers.

For LAN access, combine `require_auth = true` with a non-loopback
bind such as `agentsview pg serve --host 0.0.0.0`, or keep the
backend on loopback and expose it through a proxy.

`pg serve` does **not** expose the global live-refresh event stream
used by normal `agentsview serve`, because there is no local sync
engine attached to the server. The session browser, analytics, and
usage views still work normally; they are just not auto-refreshed by
the global SSE path.

As of 0.33.0, the web UI distinguishes a **degraded backend**
(the server responds but PostgreSQL is temporarily unavailable,
surfacing as 5xx errors) from an unreachable one. Instead of
forcing a page reload — which could loop while the database was
down — the app stays interactive with its current data, shows a
compact warning in the status bar, and clears it once a real
data read succeeds. Click the status bar to retry immediately.
True network failures still use the reload-based recovery path.

CockroachDB also works as the shared database. 0.33.0 reworked
the analytics and usage queries to perform well on CockroachDB
(filter pushdown into the source tables, SQL-side tool-call
aggregation, batched pricing writes) without changing any
report semantics on PostgreSQL or SQLite.

!!! warning
    Query-parameter tokens can leak through server logs, browser
    history, and Referer headers. Prefer the `Authorization`
    header for all non-SSE requests, and use TLS (via managed
    Caddy or an external reverse proxy) to protect tokens in
    transit.

For managed Caddy mode, keep the backend `--host` on loopback
and use `--proxy-bind-host` / `--public-port` to expose the
public listener. The `pg serve` and normal `serve` modes keep
separate managed-Caddy state, so both can coexist on one host.

**Examples:**

!!! warning "Enable auth before exposing `pg serve`"
    Only the loopback example below is safe without auth. Every other
    example binds off `127.0.0.1` or fronts a public proxy, so set
    `require_auth = true` in `~/.agentsview/config.toml` before
    starting the server. The same bearer-token mechanism described in
    [Remote Access](/remote-access/#authentication) applies.

```bash
# Local development — loopback, no auth required
agentsview pg serve

# Team viewer with managed Caddy and TLS
# Requires require_auth = true in config.toml
agentsview pg serve \
  --proxy caddy \
  --public-url https://viewer.example.com \
  --public-port 8443 \
  --tls-cert /path/to/cert.pem \
  --tls-key /path/to/key.pem

# Remote access on a trusted private network (no TLS)
# Requires require_auth = true; only use behind a VPN or on a
# private LAN because tokens cross the wire in cleartext.
agentsview pg serve --host 0.0.0.0 --port 8080
```

---

## Machine Labels

When multiple machines push to the same PostgreSQL database,
each session is tagged with its source machine name. In the
web UI, session items show a machine label when the session
did not originate from the local machine. Use the multi-host
filter in the sidebar to show sessions from specific machines.

![Machine labels on session items](/assets/generated/screenshots/machine-labels.png)

---

## Configuration

Single-target PostgreSQL settings can live in the legacy `[pg]` section of
`~/.agentsview/config.toml`:

```toml
[pg]
url = "postgres://user:pass@host:5432/dbname?sslmode=require"
machine_name = "my-laptop"
schema = "agentsview"
allow_insecure = false
```

| Field | Default | Description |
|-------|---------|-------------|
| `url` | (required) | PostgreSQL connection string |
| `machine_name` | OS hostname | Identifies the pushing machine; defaults to `os.Hostname()` if omitted |
| `schema` | `agentsview` | PostgreSQL schema name |
| `allow_insecure` | `false` | Allow non-TLS connections to non-loopback hosts |
| `projects` | | Array of project names to include in push |
| `exclude_projects` | | Array of project names to exclude from push |

To manage more than one PostgreSQL destination, define named `[pg.NAME]` blocks
and select the effective default target with `default_pg`:

```toml
default_pg = "work"

[pg.work]
url = "postgres://user:pass@work-db:5432/agentsview?sslmode=require"
machine_name = "my-laptop"

[pg.archive]
url = "postgres://user:pass@archive-db:5432/agentsview?sslmode=require"
machine_name = "my-laptop-archive"
exclude_projects = ["scratch"]
```

`agentsview pg push` and `agentsview pg status` use the effective default target
when no target name is passed, accept one target name explicitly, and also
support `--all` for sequential multi-target runs. `agentsview pg push --watch`
follows the effective default target unless you pass one named target
explicitly. `agentsview pg serve` and `agentsview pg service` stay on the
effective default target in this release, and `--all --watch` is rejected.

!!! warning
    The `url` field is required for all `pg` commands. If it
    contains credentials, ensure `config.toml` has restricted
    file permissions (`0600`).

The connection string supports standard PostgreSQL parameters.
Use `sslmode=require` or `sslmode=verify-full` for remote
databases. Only use `sslmode=disable` for trusted local
connections.

Environment variables in the URL are expanded using `${VAR}`
syntax:

```toml
[pg]
url = "postgres://${PG_USER}:${PG_PASSWORD}@host:5432/dbname?sslmode=require"
```

### Environment Variables

PostgreSQL settings can also be configured via environment
variables. In legacy single-target mode they override the `[pg]`
values. In named-target mode they apply only to the effective default
target:

| Variable | Description |
|----------|-------------|
| `AGENTSVIEW_PG_URL` | PostgreSQL connection URL |
| `AGENTSVIEW_PG_MACHINE` | Machine name for push sync |
| `AGENTSVIEW_PG_SCHEMA` | Schema name (default `agentsview`) |

---

## Multi-Machine Workflow

A typical team setup:

1. **Each developer** configures `[pg]` in their local
   `config.toml` with a unique `machine_name`
2. **Each developer** installs `agentsview pg service` or runs
   `agentsview pg push --watch` to sync their sessions
3. **One server** runs `agentsview pg serve` pointed at the
   shared PostgreSQL database
4. **The team** opens the shared dashboard to browse everyone's
   sessions, filtered by machine if needed

```bash
# Developer A's machine
agentsview pg service install

# Team server
agentsview pg serve \
  --proxy caddy \
  --public-url https://viewer.team.internal \
  --tls-cert /etc/certs/viewer.pem \
  --tls-key /etc/certs/viewer-key.pem
```

---

## Limitations

- **One-way sync** — sessions flow from SQLite to PostgreSQL
  only. Changes in PostgreSQL do not propagate back to local
  machines.
- **Permanent deletes not propagated** — sessions removed via
  `agentsview prune` are not deleted from PostgreSQL because
  the local rows no longer exist at push time. Use a direct
  SQL DELETE to clean up PostgreSQL if needed. Soft-deleted
  sessions (trash) sync correctly.
- **Schema compatibility** — `pg serve` automatically applies
  pending schema migrations on startup. If the PostgreSQL role
  lacks DDL permissions, run `agentsview pg push` from a
  machine with write access to update the schema.
- **Trigram index bloat on pre-0.33.0 schemas** — the content
  search index was created with GIN `fastupdate` on, which let
  a pending-insert list grow unbounded under continuous ingest.
  0.33.0 creates the index with `fastupdate = off` and alters
  existing indexes automatically, which stops further growth —
  but space already consumed is only reclaimed by a one-time
  `REINDEX INDEX idx_messages_content_trgm;`.
