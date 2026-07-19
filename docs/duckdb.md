---
title: DuckDB Mirror
description: Mirror the local SQLite archive into DuckDB and serve it locally or over the Quack remote protocol
---

As of 0.33.0, AgentsView can mirror its local SQLite archive into a DuckDB
database and serve the read-only web UI from that mirror — either from the local
file or remotely over DuckDB's Quack protocol. SQLite remains the source of
truth for ingestion; the mirror is populated by `duckdb push` or
`duckdb push --watch`, the same one-way model as [PostgreSQL sync](/pg-sync/).

This is useful when you want a portable single-file analytics copy of your
archive, or want to query your sessions with DuckDB directly, without standing
up a PostgreSQL server.

!!! warning "Experimental"
    The DuckDB backend is new in 0.33.0, and Quack is a beta DuckDB
    core extension. Expect rough edges, and treat the Quack remote
    path as suitable for trusted networks only.

## Quick Start

```bash
# Mirror local SQLite into ~/.agentsview/sessions.duckdb
agentsview duckdb push

# Check mirror state
agentsview duckdb status

# Serve the read-only web UI from the mirror
agentsview duckdb serve

# Keep the mirror current in the foreground
agentsview duckdb push --watch
```

`duckdb push` accepts the same project-filter and foreground watcher flags as
[`pg push`](/pg-sync/#project-filtering):

| Flag                 | Default | Description                                                    |
| -------------------- | ------- | -------------------------------------------------------------- |
| `--full`             | `false` | Force full local resync and DuckDB push                        |
| `--projects`         |         | Comma-separated projects to push (inclusive)                   |
| `--exclude-projects` |         | Comma-separated projects to exclude from push                  |
| `--all-projects`     | `false` | Ignore configured project filters for this run                 |
| `--watch`            | `false` | Run continuously, pushing on change plus a periodic floor      |
| `--debounce`         | `30s`   | Coalesce window after a change before pushing (`--watch` only) |
| `--interval`         | `15m`   | Periodic floor push interval (`--watch` only)                  |

With `--watch`, AgentsView performs one initial sync and DuckDB push, then keeps
running until interrupted. Shutdown via `Ctrl+C` or `SIGTERM` cancels the
watcher cleanly.

`duckdb push` always writes the local mirror file at `[duckdb].path`; it never
pushes to a remote Quack endpoint. If `[duckdb].url` or
`AGENTSVIEW_DUCKDB_URL` is configured, push fails immediately with an error
telling you to unset it for pushes and expose the mirror remotely with
`agentsview duckdb quack serve` instead. `url`, `token`, and `allow_insecure`
apply only to `duckdb status` and `duckdb serve`, the read side.

When `[duckdb].path` or `AGENTSVIEW_DUCKDB_PATH` is configured, all local
DuckDB commands use that same mirror file by default, including
`duckdb quack serve`. Use `duckdb quack serve --path ...` only when you want to
expose a different mirror than the one used by `duckdb push`, `duckdb status`,
and `duckdb serve`.

`duckdb serve` accepts the same serve flags as
[`pg serve`](/pg-sync/#agentsview-pg-serve) (`--host`, `--port`, `--base-path`,
proxy and TLS flags) and is read-only in the same way — no uploads, file
watching, or local sync.

## Quack Remote Access

[Quack](https://duckdb.org/docs/current/core_extensions/quack) is DuckDB's
remote-access extension: it turns a DuckDB instance into a server that other
DuckDB clients can attach to over `quack:` URIs. AgentsView uses it to serve the
web UI on one machine from a mirror that lives on another:

```bash
# Machine A: expose the local mirror over loopback Quack
agentsview duckdb quack serve \
  --bind quack:127.0.0.1:9494 \
  --token "$AGENTSVIEW_DUCKDB_TOKEN"

# Machine B (or another terminal): serve the UI from that endpoint
AGENTSVIEW_DUCKDB_URL='quack:127.0.0.1:9494' \
AGENTSVIEW_DUCKDB_TOKEN="$AGENTSVIEW_DUCKDB_TOKEN" \
agentsview duckdb serve
```

The client `url` uses the native `quack:HOST:PORT` form. The Quack extension
attaches only that authority form; URL-scheme forms such as
`quack:http://HOST:PORT` are rejected at attach time, so AgentsView refuses them
up front. A non-loopback client host requires `allow_insecure` (Quack speaks
plain HTTP — put it behind TLS, a VPN, or an SSH tunnel first).

`duckdb push` never targets a remote endpoint, even when `[duckdb].url` is
set: run push on the machine where the mirror file lives, then use
`duckdb quack serve` to expose that file to other machines, which read it
through `duckdb status` or `duckdb serve` pointed at the Quack URL.

`duckdb quack serve` flags:

| Flag               | Default                      | Description                |
| ------------------ | ---------------------------- | -------------------------- |
| `--bind`           | `quack:127.0.0.1:9494`       | Quack bind URI             |
| `--path`           | `[duckdb].path`              | DuckDB mirror file to expose |
| `--token`          | (required unless configured) | Quack authentication token |
| `--allow-insecure` | `false`                      | Allow binding beyond loopback |

Safety defaults:

- The Quack listener binds to loopback (`127.0.0.1`) by default.
- A token is required from `--token`, `AGENTSVIEW_DUCKDB_TOKEN`, or
  `[duckdb].token`; the token value is never printed.
- Binding to a non-loopback address requires the explicit
  `--allow-insecure` flag. Quack speaks plain HTTP, so put it
  behind TLS, a VPN, or an SSH tunnel before exposing it beyond
  the local machine.

## Configuration

DuckDB settings live in a `[duckdb]` section of `~/.agentsview/config.toml`:

```toml
[duckdb]
path = "~/.agentsview/sessions.duckdb"
url = "quack:127.0.0.1:9494"
token = "..."
machine_name = "my-laptop"
allow_insecure = false
projects = ["alpha", "beta"]
# or: exclude_projects = ["scratch"]
```

| Field              | Default                         | Description                                                                                 |
| ------------------ | ------------------------------- | ------------------------------------------------------------------------------------------- |
| `path`             | `~/.agentsview/sessions.duckdb` | Local DuckDB mirror file                                                                    |
| `url`              |                                 | Remote Quack endpoint for `duckdb status` and `duckdb serve` (`quack:` URI); read side only — `duckdb push` rejects it |
| `token`            |                                 | Quack authentication token                                                                  |
| `machine_name`     | OS hostname                     | Identifies the pushing machine                                                              |
| `allow_insecure`   | `false`                         | Allow plain-HTTP Quack beyond loopback                                                      |
| `projects`         |                                 | Array of project names to include in push                                                   |
| `exclude_projects` |                                 | Array of project names to exclude from push                                                 |

Environment variables override the config file:

| Variable                    | Description                   |
| --------------------------- | ----------------------------- |
| `AGENTSVIEW_DUCKDB_PATH`    | Local DuckDB mirror file path |
| `AGENTSVIEW_DUCKDB_URL`     | Remote Quack endpoint URL for status and serve (read side only) |
| `AGENTSVIEW_DUCKDB_TOKEN`   | Quack authentication token    |
| `AGENTSVIEW_DUCKDB_MACHINE` | Machine name override         |

## Limitations

- **One-way mirror** — data flows from SQLite to DuckDB only, via `duckdb push`
  or the foreground `duckdb push --watch` process. DuckDB and Quack are read
  backends; they do not ingest session files directly.
- **Search is unindexed** — DuckDB-backed search uses substring/regex matching
  rather than the FTS5 index that local SQLite serving uses, so content search
  is slower on large archives.
- **No Windows ARM64 support** — the upstream `duckdb-go-bindings` ship no
  prebuilt DuckDB library for `windows/arm64`, so `agentsview duckdb`
  subcommands report a clear error on that platform. Everything SQLite-backed
  works normally. On all other platforms the DuckDB driver is linked into the
  standard binary (which grows it considerably — this is expected).
- **The mirror is a disposable derived artifact** — `duckdb push` rebuilds it
  from scratch (temp file, atomic swap) when the file is missing or damaged,
  `--full` is passed, the mirror's schema version does not match the running
  binary, the local SQLite data version has changed (for example after a
  resync), or the project-filter scope has changed. Otherwise push applies a
  bounded incremental update that replaces only changed sessions and applies
  recorded deletions.
- **Scope changes rebuild the mirror** — `--projects` / `--exclude-projects`
  (or the configured `projects` / `exclude_projects`) scope is a property of
  the whole mirror file. Changing it rebuilds the mirror to contain exactly
  the new scope; sessions that were in scope for an earlier push are no
  longer preserved once they fall out of scope.
- **`duckdb serve` and `duckdb quack serve` never create or migrate the
  mirror** — a missing or schema-incompatible file makes them exit with an
  error to run `duckdb push --full`. Both detect when a later push replaces
  the file and reopen automatically: `duckdb serve` keeps serving the old
  data until a compatible replacement appears, while `duckdb quack serve`
  stops serving during the switch and, if the replacement is incompatible,
  stays down (retrying with backoff) until a good file shows up. Run pushes
  before or between serve sessions when that timing matters.
