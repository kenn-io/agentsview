---
title: Configuration
description: Config file, default paths, and runtime settings
---

## Data Directory

AgentsView stores all persistent data under a single directory, defaulting to
`~/.agentsview/`. Override with the `AGENTSVIEW_DATA_DIR` environment variable.

!!! note

    `AGENT_VIEWER_DATA_DIR` is still accepted as a legacy fallback when
    `AGENTSVIEW_DATA_DIR` is unset, but new setups should use `AGENTSVIEW_DATA_DIR`.

```
~/.agentsview/
├── sessions.db      # SQLite database (WAL mode)
├── assets/          # Images stored outside SQLite
├── vectors.db       # Semantic-search vector index (when [vector] is enabled)
├── usage-cache-v6-<id>.db # Disposable usage-aggregate cache
├── telemetry-install-id # Application installation ID
├── telemetry-daemon-active.lock  # Serializes daily daemon reports
├── telemetry-setup-finished.lock # Serializes the one-time setup report
├── telemetry-screen-views.lock   # Serializes screen view claims
├── config.toml      # Configuration file
├── config.toml.lock # Serializes concurrent config writers
├── db.write.lock    # Per-data-dir SQLite write-owner lock
├── debug.log        # Log of the server, sync, and other commands
├── debug.log.1      # Previous oversized debug log
├── debug.log.rotate.lock
├── run/             # Windows only: daemon runtime records and locks
├── serve.log        # Detached daemon output
├── tmp/             # Temporary files, only when the system temp directory is unusable
└── uploads/         # Uploaded session files
```

When a command opens `debug.log` larger than 10 MB, it saves the last 10 MB
to `debug.log.1`, replacing the older backup, then clears the active log for
new messages. The backup can begin partway through a line. The `serve` command
captures startup messages immediately and waits until any incumbent daemon
has stopped before rotating. Debug log writes share `debug.log.rotate.lock`
and wait while rotation holds it exclusively. If the lock is busy or the
backup fails, AgentsView keeps the active log intact.

`usage-cache-v6-<id>.db` is a derived cache of usage aggregates, not user data.
It is safe to delete when no AgentsView process is running; the next usage query
rebuilds it automatically. Back up `sessions.db` for session history, `assets/`
for images stored outside SQLite, `config.toml` for settings, and
`telemetry-install-id` for installation identity. Keep the database and assets
together when restoring or moving the archive.

SQLite needs a writable temp directory for session writes and large sorts.
AgentsView uses the system temp directory: `TMP` or `TEMP` on Windows, and
`TMPDIR` elsewhere. If that directory cannot be written, for example inside a
sandbox that confines child processes, AgentsView prints a warning and uses
`tmp/` inside the data directory instead. Its child processes inherit the same
choice. To use a different directory, point those variables at a writable
directory before you start AgentsView.

The desktop app and CLI share a detached local daemon for fresh reads and
writes. A running daemon owns local SQLite writes for this data directory and
self-exits after an idle period. Ordinary session commands require the daemon.
Dedicated diagnostics, including `db adopt-machine --list` and `doctor sync`,
can inspect the archive without starting it. Set `AGENTSVIEW_NO_DAEMON=1` when a
script must never auto-start a daemon; commands that require one will refuse.

The Cursor source in code attribution stats is a live, machine-local read from
`~/.cursor/ai-tracking/ai-code-tracking.db` by default. Set
`AGENTSVIEW_CURSOR_ATTRIBUTION_DB` when Cursor stores that database somewhere
else on the host answering the stats request. The attribution database is not
synced into AgentsView's archive and is not pushed to PostgreSQL.

### Archive content

`archive_content` in `config.toml` controls how much of each session the archive
stores:

```toml
# "full" (default), "transcripts", or "usage"
archive_content = "transcripts"
```

| Value           | What is stored                                                                                                                                                                                                                                                                                                                                     |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `"full"`        | Parsed messages, reasoning, titles, tools, and results. Tool-result category and image settings still apply.                                                                                                                                                                                                                                       |
| `"transcripts"` | Message text, thinking text, titles, and tool call metadata: names, categories, skill names, the file path a call targeted, result lengths, statuses, and subagent links. Tool inputs and tool results, usually most of a large archive, are dropped, and the one-line tool summaries inside message text keep only the tool name and target path. |
| `"usage"`       | Session metadata, usage values, activity timestamps, and the identifiers needed to connect subagents. Transcript text, thinking text, original tool inputs and results, and session titles are dropped.                                                                                                                                            |

The daemon reads the policy when it starts, so a change takes effect after
`agentsview daemon restart`. The policy applies when rows are written, so
changing it does not rewrite rows already in the archive. To apply a narrower
policy to an existing archive, set the key, restart the daemon, and run
`agentsview sync --full`. The rebuild re-parses sessions whose source files
still exist and copies archived sessions whose sources are gone, projecting both
onto the policy. Back up the archive first: dropped content cannot be restored
without the original source files.

`AGENTSVIEW_ARCHIVE_CONTENT` sets the policy when the config file does not,
which suits a dedicated reporting archive in its own data directory:

```bash
AGENTSVIEW_DATA_DIR=~/.agentsview-usage \
AGENTSVIEW_ARCHIVE_CONTENT=usage \
agentsview sync
```

Limits of the narrower policies:

- `"transcripts"` is a storage policy, not a redaction guarantee. It removes the
    tool payload tables, the structured tool inputs, the rows and summaries that
    parsers mark as tool output, and it rewrites the tool summaries parsers
    inline into message text. Text that a provider wrote into a message body in
    its own format is kept as the provider wrote it. When tool output must never
    be present in the archive, use `"usage"`, which stores no transcript text.
- `"transcripts"` keeps session search, transcript viewing, and tool analytics
    working, but signals and secret findings that read tool inputs or results
    (tool failure detection, repeated identical calls, secrets inside tool
    output) see empty payloads. `result_content_blocked_categories` has no
    additional effect under this policy. Archived RooCode, Kilo Legacy, gptme,
    OpenHands, Aider, Codex, TraeX, and Zencoder sessions parsed before data
    version 105 and whose source files are gone cannot be re-parsed, so a
    rebuild drops every row shape those parsers once used for tool output. This
    includes user turns for OpenHands, Codex, and TraeX; assistant replies for
    Aider; and system-flagged notices for Zencoder. Copied OpenHands actions
    with event summaries keep the prose before the tool header and the tool
    label, but lose the summary and all following text, including appended
    thinking, because the archive does not retain the summary boundaries. The
    semantic search mirror keeps previously embedded text until the next embedding
    pass refreshes it. With `[vector.embed] run_after_sync` enabled (the
    default) that pass starts right after the rebuild; otherwise let the
    scheduled pass run before pushing vectors to PostgreSQL.
- `"usage"` supports usage reports such as `agentsview usage daily`. Session
    search, transcript viewing, tool analytics, and content-derived quality
    metrics require a fuller archive. Insights and recall entries are refused,
    since both hold transcript-derived text. Recall extraction stops before
    calling a model, including for old rows awaiting a rebuild. Raw
    `session export` is also disabled. Vector building, search, and export are
    disabled. Opening the writable archive clears existing local message and
    recall indexes, including when `[vector]` is disabled. Run `pg push --full`
    after the archive rebuild to remove indexed content for those sessions from
    PostgreSQL as well. Usage-only pushes also clear session titles, including
    names changed in PostgreSQL. Other sessions in a shared PostgreSQL store are
    unaffected.

### Ingest-time image offload

`tool_result_images = "offload"` stores supported PNG, JPEG, WebP, and GIF
tool-result images in `{dataDir}/assets/<sha256hex><ext>` before SQLite
publishes their references. Restart the daemon to apply the setting. Failed
writes keep inline content, and archives that omit tool content write no assets.
See [image storage](/docs/data/#ingest-time-image-offload) for retries, backups,
and remote-backend limits.

You can also select **Offload** under **Settings > Archive content**. The
restart notice appears after you save a changed policy.

![Tool-result image policy in Settings](/docs/assets/generated/screenshots/settings-archive-content.png)

## Installation Identity

Local sessions use a random installation ID saved in `telemetry-install-id` in
this data directory. AgentsView reuses an existing ID from that file or creates
one, even when telemetry is disabled. PostHog consumes the same ID. Read-only
commands leave the file unchanged.

The ID identifies an installation, not physical hardware. It survives updates,
binary replacements, restarts, and network or hostname changes. A fresh data
directory creates a new ID; copying the data directory copies the identity.
Restore the original `telemetry-install-id` from a backup to keep that identity.
Deleting it deliberately creates a distinct installation; that new installation
does not automatically take ownership of the old one's sessions.

The display label defaults to the current hostname. Set `local_machine_name` in
`config.toml` for a fixed label, then run `agentsview daemon restart`. Any
non-empty label is allowed, including `local`. Changing the label leaves session
keys unchanged; PostgreSQL and DuckDB receive the label on the next push.
Creating the identity does not rewrite `config.toml` or its comments. A missing
cursor secret is still generated and saved.

Session filters accept an exact ID or alias, a comma-separated list of IDs and
aliases, or one display label matched as the whole value, case-sensitively.
Unknown names and labels shared by several machines return an error that lists
known machines. Use `/api/v1/machines` to look up IDs, display labels, and aliases.

Project rules and worktree mappings accept only IDs and aliases.

### Upgrading Historical Machine Keys

At writable startup, AgentsView moves sessions from the archive's recorded local
machine to the installation ID. The saved ownership record was introduced in
v0.40.0; v0.39.0 archives do not have it. Older `local` rows move too. Session
IDs, messages, stars, pins, and other curation remain intact. Worktree rules and
project and source metadata move with the sessions. Conflicting worktree rules
stop the migration so you can reconcile them without losing edits.

Adopted hostnames remain aliases for old filters and URLs. Display labels do not
establish ownership, and other historical hostnames remain separate until you
explicitly select them. Alias keys are reserved redirects: use a peer's
installation ID for a remote source and omit `machine` for a local root.

If an archive has named machines but no saved local ownership, startup keeps
them under their existing keys and logs the keys once. Inspect them with
`agentsview db adopt-machine --list`, which prints machine keys and session
counts without starting a daemon or changing configuration. Stop the daemon,
then run
[`agentsview db adopt-machine`](/docs/commands/#agentsview-db-adopt-machine)
with the old machine keys you own. Keys that belong to other installations need
no action.

PostgreSQL publishes the migrated sessions and metadata on the next incremental
push. DuckDB rebuilds its mirror once when the default machine key changes. The
SQLite archive stays intact. If two installations publish the same old hostname
alias to PostgreSQL, the latest push determines its filter target. Use
installation IDs to select machines unambiguously in a shared mirror.

## Config File

The config file at `~/.agentsview/config.toml` is auto-created on first run. It
stores persistent settings that survive restarts.

Commands validate the settings they use. For example, `export sessions` warns on
stderr about invalid insights, vector, recall-extraction, or public/proxy settings
and still exports the archive. It preserves those settings in `config.toml`.
TOML syntax and type errors still fail loading, and unknown-key handling is
unchanged. Server startup and reloads validate the complete configuration, so
starting `serve` also checks settings unused by exports.
Automatic daemon startup and replacement validate the effective server
configuration, including deployment overrides, before launching or stopping a
daemon.

The full container deployment environment applies to `serve`, remote `serve`
commands, and `pg push`, including its watch mode. Other PostgreSQL clients honor
`AGENTSVIEW_PG_ALLOW_INSECURE` for the default target. Hosted raw reads and reparse
commands honor `AGENTSVIEW_REQUIRE_AUTH` without reading the server token file.
MCP HTTP listeners and daemon clients load the deployment authentication they
use. Semantic and hybrid PostgreSQL reads, including `mcp --pg`, load deployment
embeddings for published recipe adoption without reading listener token files.
Direct archive exports skip deployment token files and
`AGENTSVIEW_EMBEDDINGS_*` settings.

!!! note

    The config format changed from JSON to TOML. Existing `config.json` files are
    automatically migrated to `config.toml` on first run (the JSON file is renamed
    to `config.json.bak`).

```toml
cursor_secret = "base64-encoded-secret"
require_auth = true
cursor_admin_api_key = "key_xxxxx"
daemon_idle_timeout = "20m"
chart_palette = "agentsview"
zoom_level = 120
```

| Field                               | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ----------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `local_machine_name`                | Optional display-name override; takes effect after a daemon restart                                                                                                                                                                                                                                                                                                                                                                                                              |
| `cursor_secret`                     | Auto-generated HMAC key for pagination cursor signing                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `cursor_admin_api_key`              | Cursor Admin API key used by `agentsview usage cursor`                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `cursor_admin_email`                | Optional default Cursor Admin usage filter by member email                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `cursor_admin_user_id`              | Optional default Cursor Admin usage filter by member user ID                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `github_token`                      | Optional saved GitHub token for Gist publishing                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `result_content_blocked_categories` | Tool categories whose result content is not stored (default: `["Read", "Glob"]`). Changes apply to new ingestion and full rebuilds; see [storage maintenance](/docs/data/#storage-maintenance) for existing source-backed sessions.                                                                                                                                                                                                                                              |
| `tool_result_images`                | Retain supported inline tool-result image blocks with `"keep"` (default), or store readable `agentsview_image` placeholders with `"drop"`, or move supported images to the local asset store with `"offload"`. The setting affects future ingestion and full resyncs; use `db migrate --images` to move existing images or `db strip --images` to remove them from stored results; also configurable under **Settings > Archive content**, and changes require a daemon restart. |
| `archive_content`                   | How much of each session the archive stores: `"full"` (default), `"transcripts"`, or `"usage"`; changes require a daemon restart — see [Archive content](#archive-content)                                                                                                                                                                                                                                                                                                       |
| `host`                              | Interface the server binds to (default `127.0.0.1`); non-loopback values require `require_auth = true`                                                                                                                                                                                                                                                                                                                                                                           |
| `require_auth`                      | Require bearer-token authentication for API access                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `auth_token`                        | Auto-generated 256-bit bearer token for remote access; overridden by `AGENTSVIEW_AUTH_TOKEN`, or by `AGENTSVIEW_AUTH_TOKEN_FILE` under a [container deployment](#container-deployment)                                                                                                                                                                                                                                                                                           |
| `public_url`                        | Browser URL, trusted origin, and managed Caddy site address                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `public_origins`                    | Additional trusted origins for request Host/Origin checks                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `daemon_idle_timeout`               | Idle timeout for detached writable daemons; set to `"0s"` to keep them alive                                                                                                                                                                                                                                                                                                                                                                                                     |
| `chart_palette`                     | Server-wide categorical chart colors: `"agentsview"` (default), `"matplotlib"`, or `"colorblind"`; also configurable under **Settings > Appearance**                                                                                                                                                                                                                                                                                                                                              |
| `zoom_level`                        | Default interface zoom for clients without a local preference: `67`, `75`, `80`, `90`, `100`, `110`, `120`, `125`, `130`, `150`, `175`, or `200`. Defaults to `100` when omitted. Restart the daemon after manual edits                                                                                                                                                                                                                                                          |
| `disabled_agents`                   | Session providers to exclude from local filesystem scanning — see [Disabling Session Providers](#disabling-session-providers)                                                                                                                                                                                                                                                                                                                                                    |
| `[proxy]`                           | Managed proxy configuration table — see [Remote Access](/docs/remote-access/)                                                                                                                                                                                                                                                                                                                                                                                                    |
| `disable_update_check`              | Disable the automatic update check (see [Privacy](#privacy-and-telemetry))                                                                                                                                                                                                                                                                                                                                                                                                       |
| `scan_protected_paths`              | Allow Git discovery inside macOS privacy-protected folders and volumes under `/Volumes`, accepting one consent prompt per location — see [macOS Protected Folders](#macos-protected-folders)                                                                                                                                                                                                                                                                                                                    |
| `[pg]`                              | PostgreSQL sync configuration — see [PostgreSQL Sync](/docs/pg-sync/)                                                                                                                                                                                                                                                                                                                                                                                                            |
| `[duckdb]`                          | DuckDB mirror configuration — see [DuckDB Mirror](/docs/duckdb/)                                                                                                                                                                                                                                                                                                                                                                                                                 |
| `[clickhouse]`                      | ClickHouse sync configuration — see [ClickHouse Sync](/docs/clickhouse-sync/)                                                                                                                                                                                                                                                                                                                                                                                                    |
| `[vector]`                          | Opt-in semantic-search index; model settings live in `[vector.embeddings]`, named endpoints in `[vector.embeddings.servers.<name>]`, embedding schedule in `[vector.embed]` — see [Semantic Search](/docs/semantic-search/#enabling-vector) for every key                                                                                                                                                                                                                        |
| `[recall.extract]`                  | Opt-in model-backed recall extraction; named endpoints in `[recall.extract.servers.<name>]`, prompt selection in `[recall.extract.prompts]`, request overrides in `[recall.extract.request]` — see [Recall](/docs/recall/#automatic-extraction)                                                                                                                                                                                                                                  |
| `[insights]`                        | Optional generated-insights endpoint and model, plus `default_agent`, the agent CLI new reports start with; local loopback HTTP is allowed, remote plaintext requires `allow_http = true`, and endpoint failures do not retry through a CLI — see [Recall](/docs/recall/#current-surface)                                                                                                                                                                                          |
| `[friction]`                        | Opt-in Friction Log review; set `enabled = true` to review changed sessions in the background after each sync — see [Friction Log](#friction-log) |
| `[[remote_hosts]]`                  | Remote machines synced by a bare `agentsview sync` — see [CLI Reference](/docs/commands/#agentsview-sync)                                                                                                                                                                                                                                                                                                                                                                        |
| `[[session_sources]]`               | Additional filesystem session roots with per-root machine keys — see [Filesystem Session Sync](/docs/filesystem-sync/)                                                                                                                                                                                                                                                                                                                                                           |
| `[automated]`                       | Custom automated-session patterns — see [Automated Session Detection](#automated-session-detection)                                                                                                                                                                                                                                                                                                                                                                              |
| `[custom_model_pricing]`            | Per-model price overrides for usage reports — see [Custom Model Pricing](/docs/token-usage/#custom-model-pricing)                                                                                                                                                                                                                                                                                                                                                                |
| `[model_aliases]`                   | Model names mapped to display groups. See [Model aliases](/docs/token-usage/#model-aliases).                                                                                                                                                                                                                                                                                                                                                                                     |

The `cursor_secret` is generated automatically on first run. For Gist
publishing, AgentsView first uses a saved `github_token`. For local browser
requests, if no token is saved, it then tries `AGENTSVIEW_GITHUB_TOKEN` and then
`gh auth token` from the GitHub CLI. Local users usually only need to run
`gh auth login`. For remote or proxied access, save a `github_token` via the web
UI Settings page or the API endpoint `POST /api/v1/config/github` when you want
AgentsView to publish gists. Remote access fields can be configured via the
Settings page or CLI flags — see [Remote Access](/docs/remote-access/) for
details.

`agentsview daemon start` and `agentsview daemon restart` load the normal
effective configuration from this file and supported environment variables; they
accept no serve-specific flags. `--no-sync` is a runtime-only `serve` option and
cannot be stored in `config.toml`.

Unknown keys anywhere under `[vector]` stop configuration loading, even when
vector search is disabled. After upgrading, correct misspelled keys and move
endpoint settings such as `max_batch_tokens` under
`[vector.embeddings.servers.<name>]`. See the
[vector configuration reference](/docs/semantic-search/#enabling-vector).

When `require_auth` is enabled, the browser login prompt accepts the configured
`auth_token`. The value can come from `~/.agentsview/config.toml` or from the
`AGENTSVIEW_AUTH_TOKEN` environment variable; the environment variable wins when
both are set. Under a [container deployment](#container-deployment),
`AGENTSVIEW_AUTH_TOKEN_FILE` can supply it from a mounted secret file instead.
When authentication needs a token and none is configured, AgentsView generates
one and saves it as `auth_token` in `config.toml`.

!!! note

    Older configs may still contain `remote_access = true`. AgentsView still reads
    that legacy key for backward compatibility, but new setups should use
    `require_auth = true`.

### Container deployment

The published image sets `AGENTSVIEW_MODE`, which turns on a small set of
deployment environment variables. They apply only when `AGENTSVIEW_MODE` is set
to a non-blank value, so a desktop or native install ignores them, including
invalid values and missing files. You can also set `AGENTSVIEW_MODE` yourself to
use them with the native binary.

An invalid `AGENTSVIEW_MODE` stops `serve` and `pg serve`. Other commands apply
no deployment variables under it. A command that would read one, such as a
daemon client with `AGENTSVIEW_AUTH_TOKEN_FILE` set, fails with the invalid
mode instead.

| Variable                             | Behavior                                                                                                                                                                                    |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `AGENTSVIEW_MODE`                    | `serve` or `pg-serve`. When the command line is empty or starts with a flag other than `--help` or `--version`, the image runs `agentsview serve` or `agentsview pg serve` with those flags |
| `PG_SERVE`                           | `1`, `true`, `yes`, or `on` selects `pg serve` when `AGENTSVIEW_MODE=serve`; kept for older deployments                                                                                     |
| `AGENTSVIEW_HOST`                    | Listener address. A non-loopback value needs `AGENTSVIEW_REQUIRE_AUTH=true` or `require_auth = true`                                                                                        |
| `AGENTSVIEW_REQUIRE_AUTH`            | Require bearer authentication for API access                                                                                                                                                |
| `AGENTSVIEW_NO_BROWSER`              | Do not open a browser on start                                                                                                                                                              |
| `AGENTSVIEW_AUTH_TOKEN_FILE`         | File holding the bearer token; overrides `auth_token` in `config.toml`. `~/` expands to the home directory. Cannot be combined with `AGENTSVIEW_AUTH_TOKEN`                                 |
| `AGENTSVIEW_PG_ALLOW_INSECURE`       | Sets `allow_insecure` for the default PostgreSQL target, for a database on a private network without TLS                                                                                    |
| `AGENTSVIEW_EMBEDDINGS_ENDPOINT`     | OpenAI-compatible embeddings endpoint for recipe adoption in `pg push --embed` and `pg serve`; local `serve` ignores it. See [semantic search on PostgreSQL](/docs/semantic-search/)        |
| `AGENTSVIEW_EMBEDDINGS_API_KEY`      | Inline API key for that endpoint; requires `AGENTSVIEW_EMBEDDINGS_ENDPOINT` and cannot be combined with the file form                                                                        |
| `AGENTSVIEW_EMBEDDINGS_API_KEY_FILE` | File holding that endpoint's API key; requires `AGENTSVIEW_EMBEDDINGS_ENDPOINT`                                                                                                             |
| `AGENTSVIEW_EMBEDDINGS_BATCH_SIZE`   | Inputs per embeddings request; positive integer, default `32`; requires `AGENTSVIEW_EMBEDDINGS_ENDPOINT`. Lower it for providers with request token limits.                                 |

Anyone who can inspect the container can read `AGENTSVIEW_EMBEDDINGS_API_KEY`,
and every process AgentsView starts, such as an agent CLI, inherits it. Use
`AGENTSVIEW_EMBEDDINGS_API_KEY_FILE` to keep the key out of the environment.
Unlike `AGENTSVIEW_AUTH_TOKEN`, an empty `AGENTSVIEW_EMBEDDINGS_API_KEY` still
counts as set: it stops startup on its own or next to the file form, so unset
the form you don't use. Because the name belongs to the deployment server, a
`[vector]` server can't read its key from it; startup stops while it is set, so
give that server's `api_key_env` a different name.

The image sets `AGENTSVIEW_MODE=serve`, `AGENTSVIEW_HOST=0.0.0.0`,
`AGENTSVIEW_REQUIRE_AUTH=true`, and `AGENTSVIEW_NO_BROWSER=true`. An explicit
subcommand such as `pg push --watch` or `mcp` runs as given.

Precedence, highest first: command flags, deployment variables, `config.toml`,
built-in defaults. A host set through `AGENTSVIEW_HOST` counts as persistent
configuration, so a non-loopback value without authentication stops `serve`;
only the `--host` flag allows a one-off unauthenticated bind. `pg serve` and
`duckdb serve` ignore network settings in `config.toml` but apply these
variables.

These rules fail closed:

- Boolean variables accept `true`, `false`, `1`, `0`, and the other values Go's
  `strconv.ParseBool` accepts. Any other value, an unknown `AGENTSVIEW_MODE`, or
  a blank `AGENTSVIEW_HOST` stops startup with an error naming the variable.
- A missing, unreadable, or empty `AGENTSVIEW_AUTH_TOKEN_FILE` or
  `AGENTSVIEW_EMBEDDINGS_API_KEY_FILE` stops startup. A blank
  `AGENTSVIEW_EMBEDDINGS_API_KEY`, or setting both API-key forms, also stops
  startup. AgentsView never falls back to another secret source.
- Any `AGENTSVIEW_EMBEDDINGS_*` variable together with a `[vector]` section
  stops startup, so one place owns the embedding recipe.

The image's API requires a bearer token, so an unauthenticated
`GET /api/ping` returns 401. Use a TCP check on the listener port as the
container health check, or treat 401 as alive.

To keep the PostgreSQL password out of the URL, mount a
[passfile](https://www.postgresql.org/docs/current/libpq-pgpass.html) and set
`PGPASSFILE` to its path, or add `?passfile=/run/secrets/pgpass` to
`AGENTSVIEW_PG_URL`. A file containing the single line `*:*:*:*:<password>`
matches every connection. The PostgreSQL driver ignores a missing passfile, so a
missing mount shows up as a PostgreSQL authentication error rather than an error
naming the file.

## Remote Hosts

Add `[[remote_hosts]]` entries when a bare `agentsview sync` should include raw
session files from other machines. `transport` is `"http"` (the default when
omitted) or `"ssh"`. An HTTP entry needs an AgentsView daemon on the remote host
and a private network such as Tailscale to reach it:

```toml
[[remote_hosts]]
host = "devbox1"
transport = "http" # optional; default
url = "http://devbox1.tailnet.ts.net:8080"
token = "remote-token"
interval = "5m" # optional; zero or omitted means manual sync only
```

An SSH entry needs no daemon and no install on the remote host. It runs your
`ssh` client. SSH transport is available in 0.46.0 and later:

```toml
[[remote_hosts]]
host = "devbox2"
transport = "ssh"
destination = "user@devbox.example.com" # optional; defaults to host
interval = "15m" # optional; zero or omitted means manual sync only
```

`destination` is the argument passed to `ssh`: a `~/.ssh/config` alias or
`user@hostname`. When omitted, `host` is used. Set the port, user, identity, and
`ProxyJump` in `~/.ssh/config`. Setup, the files placed on the remote, and
limits are in
[Remote Access — SSH Remote Sync](/docs/remote-access/#ssh-remote-sync).

Configuration is rejected when:

- `transport` is anything other than `http` or `ssh`.
- An HTTP entry, including one with `transport` omitted, has no `url` or no
  `token`, or sets `destination`.
- An SSH entry sets `url` or `token`.
- An SSH entry's destination starts with `-`. The destination is `destination`,
  or `host` when `destination` is omitted.

HTTP remote sync calls the remote daemon's archive endpoints and always uses a
bearer token, even when the rest of that daemon has `require_auth = false`. The
per-host `token` must match the remote daemon's `auth_token`. Do not reuse the
collector daemon's own `auth_token` for untrusted remote endpoints. HTTP and
SSH transfers use a persistent per-host mirror and fetch only changed files. A
full archive is used only when every manifest file needs fetching or a delta
request is refused; see
[Remote Access — Incremental Sync](/docs/remote-access/#incremental-sync).

When a full or automatic data-version rebuild includes local sources, configured
remote hosts, HTTP and SSH, join the same temporary-database bulk ingest and
atomic swap. `--full` reparses the complete local and remote corpus without
retransferring unchanged files from manifest-capable spokes. HTTP remote sync
requires the collector and remote daemon to use the same remote-sync protocol
version. After upgrading either host, upgrade the other before syncing again;
incompatible peers fail before targets or archive data are exchanged. SSH hosts
need no manual upgrade: AgentsView places a matching binary on the remote; see
[SSH Remote Sync](/docs/remote-access/#ssh-remote-sync).

Each `remote_hosts.host` value must be unique and stable. It namespaces imported
session IDs, the database skip cache, and the persistent mirror; changing it for
the same machine can duplicate sessions, while reusing it for another machine
can reuse stale state. Select a configured host with
`agentsview sync --host <name>`, including for direct sync with
`AGENTSVIEW_NO_DAEMON=1`. Unknown host names are rejected; ad hoc remotes are
not supported. HTTP failures are summarized with actionable messages for common
cases such as token rejection, missing remote archive endpoints, connection
refusal, DNS failures, and timeouts. For how SSH failures are reported, see
[Offline Hosts](/docs/remote-access/#offline-hosts).

Set `interval` to a positive duration such as `"5m"` to have a running collector
daemon sync that host periodically. Zero or omitted disables the per-host
schedule; manual `agentsview sync` still includes the host.

The remote daemon must also listen on an interface the collector can reach. The
server binds `127.0.0.1` by default, so set `host` in the remote machine's
`config.toml` with `require_auth = true` for a persistent node:

```toml
host = "0.0.0.0"
require_auth = true
```

Then start or restart the config-driven writable daemon:

```bash
agentsview daemon start
# After later configuration changes:
agentsview daemon restart
```

For a one-off flag override, `agentsview serve --background --host 0.0.0.0`
remains available, including without auth. Prefer authenticated persistent
configuration for an always-available remote node.

Detached writable daemons started by `agentsview daemon start`, automatic CLI
startup, or `agentsview serve --background` exit after `daemon_idle_timeout`
when idle. Set it to zero on machines that should stay available for HTTP remote
sync:

```toml
daemon_idle_timeout = "0s"
```

Supervised daemons run under systemd, launchd, Docker, or a foreground shell do
not create the detached-daemon idle tracker, so they do not idle-exit regardless
of this setting.

## Cursor Admin Usage API

`agentsview usage cursor` imports Cursor Admin API usage events into the local
archive so Cursor's billed usage can appear in the Usage dashboard and
`agentsview usage daily` reports. Set the API key in
`~/.agentsview/config.toml`:

```toml
cursor_admin_api_key = "key_xxxxx"
cursor_admin_email = "you@example.com" # optional
cursor_admin_user_id = "152683922"     # optional
```

Environment variables take precedence over the config file:

```bash
export AGENTSVIEW_CURSOR_ADMIN_API_KEY=key_xxxxx
export AGENTSVIEW_CURSOR_ADMIN_EMAIL=you@example.com
export AGENTSVIEW_CURSOR_ADMIN_USER_ID=152683922
```

The legacy unprefixed names `CURSOR_ADMIN_API_KEY`, `CURSOR_ADMIN_EMAIL`, and
`CURSOR_ADMIN_USER_ID` are also accepted when the matching `AGENTSVIEW_`
variable is unset. The email and user ID values are default filters; pass
`--email` or `--user-id` to `agentsview usage cursor` to override them for one
import.

## Session Discovery

AgentsView auto-discovers session files from the following agent sources. Amp
support is deprecated because current Amp releases may keep full threads
server-side and leave only local stubs; historical local Amp thread JSON files
can still be parsed.

The matching environment variable and `agents.<id>.dirs` configuration key
override an agent's default directories. Environment variables take precedence
when both are set. An explicit empty `agents.<id>.dirs` array, such as
`agents.grok.dirs = []`, clears that agent's default local directories, so local
discovery finds nothing there. Matching `session_sources` entries for that agent
still apply. Provider-wide exclusion is documented under
[Disabling Session Providers](#disabling-session-providers). Omitting the key
keeps its default directories.

| Agent                 | Default Directory                                                                                                                                                | File Format                                                                                                                                                   |
| --------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Aider                 | No default; opt in with `AIDER_DIR` or `agents.aider.dirs`                                                                                                       | `.aider.chat.history.md` Markdown history files                                                                                                               |
| Amp (deprecated)      | `~/.local/share/amp/threads/`                                                                                                                                    | Historical local JSON thread files                                                                                                                            |
| Antigravity (IDE)     | `~/.gemini/antigravity/`, `~/.gemini/antigravity-ide/`                                                                                                           | SQLite database per session, or the plaintext `brain/<uuid>/.system_generated/logs/transcript.jsonl` for a conversation with no database                      |
| Antigravity CLI       | `~/.gemini/antigravity-cli/`                                                                                                                                     | SQLite `conversations/<uuid>.db`, `<uuid>.trajectory.json` sidecars, or encrypted `.pb` files plus `brain/` and `history.jsonl`                               |
| Augure Code           | `~/.augure/sessions/`                                                                                                                                            | Codex-format JSONL per session                                                                                                                                |
| Augure Desktop 3 beta | (platform-specific, see below)                                                                                                                                   | Hermes-format `state.db` and `sessions/` transcripts                                                                                                          |
| Claude Code           | `~/.claude/projects/`                                                                                                                                            | JSONL per session                                                                                                                                             |
| OpenClaude            | `~/.openclaude/projects/`                                                                                                                                        | JSONL per session                                                                                                                                             |
| Claude Cowork         | (platform-specific, see below)                                                                                                                                   | Claude Desktop cowork sessions                                                                                                                                |
| Codebuff / Freebuff   | `~/.config/manicode/projects/`                                                                                                                                   | Per-session `chat-messages.json` + `run-state.json` with subagent transcripts                                                                                 |
| Codex                 | `~/.codex/sessions/` and `~/.codex/archived_sessions/`                                                                                                           | JSONL per session                                                                                                                                             |
| Codex Cloud           | Fetched from OpenAI when `[agents.codex-cloud] enabled = true`; see [Codex Cloud](#codex-cloud)                                                                  | Compressed JSONL copy per chat under `<data dir>/sources/codex-cloud/`                                                                                      |
| Cline CLI             | `~/.cline/data/sessions/` or `~/.cline/`                                                                                                                         | Paired `<id>.json` metadata and `<id>.messages.json` transcript files                                                                                         |
| Command Code          | `~/.commandcode/projects/`                                                                                                                                       | JSONL per session, optional `.meta.json` sidecar                                                                                                              |
| Copilot CLI           | `~/.copilot/`                                                                                                                                                    | JSONL per session under `session-state/`                                                                                                                      |
| Crush                 | (platform-specific, see below)                                                                                                                                   | Per-project SQLite `crush.db` with transcripts, tool activity, relationships, and recorded session costs                                                      |
| Devin CLI             | `~/.local/share/devin/` (Linux), `~/Library/Application Support/devin/` (macOS)                                                                                  | Local CLI data rooted at the directory that contains `cli/`; session data is discovered under `<root>/cli/...`                                                |
| Evener                | `~/.local/state/evener/` (or `$XDG_STATE_HOME/evener/`)                                                                                                          | Semantic v2 `*.transcript.jsonl` and optional `*.meta.json`                                                                                                   |
| Cortex Code           | `~/.snowflake/cortex/conversations/`                                                                                                                             | JSON / JSONL per session                                                                                                                                      |
| Cursor                | `~/.cursor/projects/`                                                                                                                                            | JSONL or plain-text transcripts                                                                                                                               |
| Cursor IDE            | (platform-specific, see below)                                                                                                                                   | Shared VS Code-style `globalStorage/state.vscdb` database, one session per Composer                                                                           |
| DeepSeek TUI          | `~/.codewhale/sessions/` and `~/.deepseek/sessions/`                                                                                                             | JSON per session                                                                                                                                              |
| DeepSeek Harness      | `~/.dsh/sessions/` (or `$DSH_HOME/sessions/`)                                                                                                                    | Plain or multi-frame zstd JSONL per session                                                                                                                   |
| Forge                 | `~/.forge/`                                                                                                                                                      | SQLite database (`.forge.db`)                                                                                                                                 |
| Gemini CLI            | `~/.gemini/`                                                                                                                                                     | JSONL in `tmp/` subdirectory                                                                                                                                  |
| Goose                 | (platform-specific, see below)                                                                                                                                   | SQLite `sessions.db` with transcripts, tool activity, relationships, usage, and recorded costs                                                                |
| gptme                 | `~/.local/share/gptme/logs/`                                                                                                                                     | JSONL logs                                                                                                                                                    |
| Grok                  | `~/.grok/sessions/`                                                                                                                                              | `summary.json` + optional `signals.json` + `chat_history.jsonl` transcript when present                                                                       |
| Hermes Agent          | `~/.hermes/sessions/` (macOS and Linux), `~/AppData/Local/hermes/sessions/` (Windows)                                                                            | SQLite `state.db`; JSONL / JSON transcripts remain supported                                                                                                 |
| iFlow                 | `~/.iflow/projects/`                                                                                                                                             | JSONL per session                                                                                                                                             |
| IcodeMate             | `~/.local/share/icodemate/` and `~/.icodemate/cli/projects/`                                                                                                     | OpenCode-family storage, including per-session usage events                                                                                                   |
| Junie                 | `~/.junie/sessions/` (or `$JUNIE_HOME/sessions/`)                                                                                                             | Per-session `events.jsonl` plus sibling `index.jsonl` metadata; CLI `SessionStore` only                                                                     |
| Kilo                  | `~/.local/share/kilo/`                                                                                                                                           | SQLite DB or `storage/` JSON files                                                                                                                            |
| Kimi                  | `~/.kimi/sessions/` and `~/.kimi-code/sessions/`                                                                                                                 | JSONL per session                                                                                                                                             |
| Kimi Work             | (platform-specific, see below)                                                                                                                                   | JSONL per session (kimi-code kernel wire logs)                                                                                                                |
| Kiro CLI              | `~/.kiro/sessions/cli/` and `~/.local/share/kiro-cli/`                                                                                                           | JSONL per session and SQLite database                                                                                                                         |
| Kiro IDE              | (platform-specific, see below)                                                                                                                                   | JSON / chat files                                                                                                                                             |
| Kilo (legacy)         | (platform-specific, see below)                                                                                                                                   | `tasks/<uuid>/{task_metadata.json,ui_messages.json,api_conversation_history.json}`                                                                            |
| MiMoCode              | `~/.local/share/mimocode/`                                                                                                                                       | SQLite DB or `storage/` JSON files                                                                                                                            |
| Mistral Vibe          | `~/.vibe/logs/session/`                                                                                                                                          | Legacy `messages.jsonl` plus `meta.json`, or a `unified/<session-id>/` store                                                                                  |
| Muse Code             | `~/.local/share/muse/sessions/`                                                                                                                                  | `YYYY/MM/DD/<session>/session.jsonl` event logs, subagents under `subagent/`                                                                                  |
| OMO                   | `~/.omo/agent/sessions/`                                                                                                                                         | Pi-family JSONL per session                                                                                                                                   |
| OhMyPi                | `~/.omp/agent/sessions/`                                                                                                                                         | JSONL per session                                                                                                                                             |
| OpenClaw              | `~/.openclaw/agents/` and `~/.kimi_openclaw/agents/`                                                                                                             | JSONL per session or SQLite per agent (`agent/openclaw-agent.sqlite`)                                                                                        |
| OpenCode              | `~/.local/share/opencode/`                                                                                                                                       | SQLite DB or `storage/` JSON files                                                                                                                            |
| Open Code Review      | `~/.opencodereview/sessions/`                                                                                                                                    | One JSONL file per review under an encoded project directory                                                                                                  |
| OpenHands CLI         | `~/.openhands/conversations/`                                                                                                                                    | Per-conversation `base_state.json` + `events/*.json`                                                                                                          |
| Omnigent              | `~/.omnigent/`                                                                                                                                                   | SQLite `chat.db`, one session per conversation                                                                                                                |
| Pi                    | `~/.pi/agent/sessions/`                                                                                                                                          | JSONL per session                                                                                                                                             |
| fx                    | `~/.fx/sessions/`                                                                                                                                                | Schema 4 session folders with `events.jsonl`, `session.json`, and optional `usage-v2.json` (older fx)                                                                           |
| Tau                   | `~/.tau/sessions/`                                                                                                                                               | JSONL transcripts under `<project>/`, with metadata `index.jsonl` excluded                                                                                    |
| Prime Agent           | `~/.prime/agent/sessions/`                                                                                                                                       | Flat Pi-family JSONL sessions                                                                                                                                 |
| StepCode              | `~/.stepcode/agent/sessions/`                                                                                                                                    | JSONL per session                                                                                                                                             |
| Poolside              | `~/Library/Application Support/poolside/trajectories/` (macOS), `~/.local/state/poolside/trajectories/` (Linux), `%APPDATA%\\poolside\\trajectories\\` (Windows) | NDJSON trajectory files                                                                                                                                       |
| Piebald               | `~/.local/share/piebald/`                                                                                                                                        | SQLite database (`app.db`)                                                                                                                                    |
| Posit Assistant       | `~/.posit/assistant/workspaces/`                                                                                                                                 | Per-conversation `conversation.json` tree plus `lm-messages.jsonl` transcript                                                                                 |
| Positron Assistant    | (platform-specific, see below)                                                                                                                                   | JSON / JSONL per session                                                                                                                                      |
| QClaw                 | `~/.qclaw/agents/`                                                                                                                                               | JSONL per session                                                                                                                                             |
| Qoder                 | Legacy export roots, Qoder CLI CN, plus platform-specific `SharedClientCache` (see below)                                                                        | JSONL project transcripts plus sidecar metadata                                                                                                               |
| Qwen Code             | `~/.qwen/projects/`                                                                                                                                              | JSONL per session                                                                                                                                             |
| QwenPaw               | `~/.copaw/workspaces/`                                                                                                                                           | JSON session files                                                                                                                                            |
| Reasonix              | `~/.reasonix/` and `~/AppData/Roaming/reasonix/`                                                                                                                 | JSONL sessions plus `.jsonl.meta` sidecars                                                                                                                    |
| RooCode               | (platform-specific, see below)                                                                                                                                   | `history_item.json` + `ui_messages.json` per task                                                                                                             |
| Shelley               | `~/.config/shelley/`                                                                                                                                             | SQLite database (`shelley.db`)                                                                                                                                |
| Visual Studio Copilot | (platform-specific, see below)                                                                                                                                   | Trace JSONL files                                                                                                                                             |
| VS Code Copilot       | (platform-specific, see below)                                                                                                                                   | JSON / JSONL per session                                                                                                                                      |
| Windsurf              | (platform-specific, see below)                                                                                                                                   | SQLite `workspaceStorage/<hash>/state.vscdb` workspace chat data                                                                                              |
| Trae                  | (platform-specific, see below)                                                                                                                                   | Legacy inline chat data in SQLite `workspaceStorage/<hash>/state.vscdb` and `globalStorage/state.vscdb`; modern encrypted layouts are detected as unsupported |
| TraeX (TRAE CLI)      | `~/.trae/cli/sessions/` and `~/.trae/cli/archived_sessions/`                                                                                                     | Codex-compatible rollout JSONL per session                                                                                                                    |
| Warp                  | (platform-specific, see below)                                                                                                                                   | SQLite database                                                                                                                                               |
| WorkBuddy             | `~/.workbuddy/projects/`                                                                                                                                         | JSONL per session                                                                                                                                             |
| CodeBuddy             | (platform-specific, see below)                                                                                                                                   | Hierarchical session JSON manifest (`index.json`) and message files (`messages/*.json`)                                                                       |
| ZCode                 | `~/.zcode/cli/db/` or `~/.zcode/cli/`                                                                                                                            | SQLite database (`db.sqlite`) with usage rows                                                                                                                 |
| Zed                   | (platform-specific, see below)                                                                                                                                   | SQLite database (`threads/threads.db`)                                                                                                                        |
| Zencoder              | `~/.zencoder/sessions/`                                                                                                                                          | JSONL per session                                                                                                                                             |

Mistral Vibe labels each unified session with its last recorded model pin in
`runtime-state.json`. A subagent session takes its parent's model each time the
subagent changes and keeps it after later parent turns. Sessions with no
recorded model keep their tokens, but usage reports leave them out. A running
Vibe turn appears as soon as Vibe journals it, before the session is idle.

OpenClaw checks both representations under each configured agent directory.
The SQLite layout is one `agent/openclaw-agent.sqlite` file per agent. AgentsView
reads it without writing or migrating the database.

**Augure Code** reads Codex-format JSONL from `~/.augure/sessions/` and appears
under the separate `augure-code` agent ID. Set `AUGURE_CODE_SESSIONS_DIR` or
`agents.augure-code.dirs` for another sessions root. Resume these sessions with
`augure resume`, not `codex resume`.

**Augure Desktop 3 beta** reads `state.db` and its `sessions/` sibling from
`~/.augure-desktop/` on macOS and Linux, or `~/AppData/Local/augure-desktop/` on
Windows. Set `AUGURE_DESKTOP_DIR` or `agents.augure-desktop.dirs` to use another
data root. These sessions appear as `augure-desktop`, separately from Augure
Code and Hermes. Local browsing, search, exports, and recorded usage are
supported; remote source-file sync is disabled for this provider.

**Hermes Agent** stores current session state in the SQLite `state.db` file.
AgentsView checks `~/.hermes/sessions/` on macOS and Linux and
`~/AppData/Local/hermes/sessions/` on Windows. The sessions root resolves its
sibling `state.db`, so discovery still works when the `sessions/` directory does
not exist. If Windows `LOCALAPPDATA` is redirected or a named Windows profile
uses another Hermes home, set `HERMES_SESSIONS_DIR` or `hermes_sessions_dirs`
explicitly.

Cron runs stay under `hermes-cron`, with stable job groups in Usage attribution.
Renames keep costs together, and jobs with the same name stay separate.
Compressed sessions follow parent links available in `state.db` when parsed.
A non-empty source in `state.db` takes precedence over the transcript's platform.
Archived runs whose sources were gone before upgrading stay individual sessions
unless they already have a stored job ID.

**Cline support covers the CLI**, not the VS Code extension. Set `CLINE_DIR` or
`agents.cline.dirs` to its data root or directly to its sessions directory.
AgentsView reads the CLI's conversations, tool activity, usage, and recorded
costs, including through remote sync. Continued teammate runs remain separate
sessions, matching Cline's own store.

DeepSeek Harness sessions are read from its default JSONL persistence backend,
including released format versions 0 through 4 in plain or zstd-compressed
files. Version 0 uses `session.jsonl[.zstd]`; later versions use
`session.vN.jsonl[.zstd]`. When a session directory retains multiple immutable
generations, AgentsView reads the numerically newest supported generation.
Inherited parent transcript content is excluded from child sessions. `DSH_HOME`
re-roots the default `<home>/sessions` path; set `DEEPSEEK_HARNESS_SESSIONS_DIR`
or `agents.deepseek-harness.dirs` to point directly at one or more session
roots. The optional SQLite persistence backend is not supported.

Prime Agent support targets the current flat session layout in v0.7.0. That
release migrates the older per-project layout when Prime Agent opens its session
store, so open the current Prime Agent once before syncing a legacy archive with
AgentsView.

fx sessions appear from `~/.fx/sessions/` when `session.json` uses schema 4.
Subagent runs appear as children of their parent session. Usage comes from
`usage-v2.json` when present (older fx only) as cumulative totals per model,
dated at the earlier of the session's last event and the usage file's
modification time.
Tool results use larger `.txt` files from `tool-results/` only when they are
regular files with valid UTF-8 and match the saved byte count or reach the
16 MiB cap. Other files keep the stored preview. Restoring a missing file
refreshes the result on the next sync. Freshness checks cover entry names,
sizes, and modification times in `tool-results/`. This suffices because fx
names result files by content hash.
Costs use AgentsView's pricing catalog. Set `FX_SESSIONS_DIR` or `agents.fx.dirs`
to scan another root, including a WSL path on Windows. The legacy `fx_dirs` key
converts to `agents.fx.dirs`.

Tau stores multiple session transcripts in each project directory under
`~/.tau/sessions/`. AgentsView reads `.jsonl` files directly below those project
directories, excludes the exact `index.jsonl` metadata file, and follows the
latest `leaf` entry when selecting the active history. Set `TAU_SESSIONS_DIR` or
`agents.tau.dirs` to use another sessions root.

User-run shell commands (`bashExecution`) and messages with the `custom`,
`branchSummary`, or `compactionSummary` role are not shown. Separate
`branch_summary` and `compaction` entries are shown.

Muse Code keeps one event log per session under
`~/.local/share/muse/sessions/YYYY/MM/DD/<session-id>/session.jsonl`, or under
`$XDG_DATA_HOME/muse/sessions/` when `XDG_DATA_HOME` is set, matching Muse.
AgentsView rebuilds the conversation from those events: prompts, replies,
reasoning summaries, tool calls, tool results, and per-reply token usage.
Subagent runs under `subagent/<child-id>/` appear as child sessions of the
session that holds them. Muse's generated session names such as `quiet-lyra`
are not used as titles; a name you set is. Set `MUSE_SESSIONS_DIR` or
`agents.muse.dirs` to use another sessions root.

**Qoder default directories** include the legacy `~/.qoder/projects/` and
`~/.qoderwork/projects/` export roots, the Qoder CLI CN store, and the current
IDE store:

- **Qoder CLI CN:** `~/.qoder-cn/projects/`

- **macOS:**
    `~/Library/Application Support/Qoder/SharedClientCache/cli/projects/`

- **Linux:** `~/.config/Qoder/SharedClientCache/cli/projects/`

- **Windows:** `%APPDATA%\Qoder\SharedClientCache\cli\projects\`

Set `QODER_PROJECTS_DIR` or `agents.qoder.dirs` to replace these defaults with
one or more explicit roots.

Grok sessions are read from `summary.json` (title, timestamps, project),
optional `signals.json` (token counters), and `chat_history.jsonl` when present
for the full transcript (user turns, assistant replies, thinking, and tool
calls). If `chat_history.jsonl` is missing, AgentsView falls back to
summary-only mode. Set `GROK_DIR` or `agents.grok.dirs` to override the default
directory.

**Goose default directories** are:

- **macOS and Linux:** `~/.local/share/goose/sessions/`
- **Windows:** `%APPDATA%/Block/goose/data/sessions/`

`GOOSE_PATH_ROOT` follows Goose's own path-root convention and resolves
`<root>/data/sessions/sessions.db`. A `agents.goose.dirs` entry may instead
point directly to that sessions directory, its parent data directory, or the
database file.

**Crush default directories** are:

- **macOS and Linux:** `~/.local/share/crush/`
- **Windows:** `%LOCALAPPDATA%/crush/`

AgentsView reads `projects.json` in that directory and discovers one SQLite
`crush.db` per project under each listed `<project>/.crush/` data directory. Set
`CRUSH_DIR` or `agents.crush.dirs` to override the default with one or more
directories: each entry may be a Crush data directory (containing
`projects.json`), a `<project>/.crush` directory, or a `crush.db` file. A
project added after AgentsView starts is picked up by the next scheduled
reconciliation pass.

Omnigent sessions are read from `~/.omnigent/chat.db`. Set `OMNIGENT_DIR` or
`agents.omnigent.dirs` to override the default directory. AgentsView creates one
session per conversation and supports the split text-ID and current binary-UUID
schema generations; the older single-table schema is detected and reported as
unsupported without losing sessions already synced from it. HTTP remote sync
stays disabled for Omnigent because `chat.db` co-locates transcripts with
authentication secrets. A metadata-only edit made directly in `chat.db` can be
deferred by the immediate filesystem-event sync; the next scheduled
reconciliation pass or an explicit resync picks it up.

**VS Code Copilot default directories** vary by platform:

- **macOS:** `~/Library/Application Support/Code/User/`
- **Linux:** `~/.config/Code/User/`
- **Windows:** `%APPDATA%/Code/User/`

Code Insiders and VSCodium variants are also discovered automatically.

**Visual Studio Copilot default directories** vary by platform:

- **macOS:** `~/Library/Caches/VSGitHubCopilotLogs/traces/`
- **Linux:** `~/.cache/VSGitHubCopilotLogs/traces/`
- **Windows:** `%LOCALAPPDATA%/Temp/VSGitHubCopilotLogs/traces/`

This is separate from VS Code Copilot. Visual Studio Copilot stores trace files
named like `*_VSGitHubCopilot_traces.jsonl`; set `VISUALSTUDIO_COPILOT_DIR` or
`agents.visualstudio-copilot.dirs` if your installation writes them elsewhere.

**Windsurf default directories** vary by platform:

- **macOS:** `~/Library/Application Support/Windsurf/User/` and
    `~/Library/Application Support/Windsurf - Next/User/`
- **Linux:** `~/.config/Windsurf/User/` and `~/.config/Windsurf - Next/User/`
- **Windows:** `%APPDATA%/Windsurf/User/` and `%APPDATA%/Windsurf - Next/User/`

Windsurf stores workspace chats in `workspaceStorage/<hash>/state.vscdb`.

**Trae default directories** vary by platform:

- **macOS:** `~/Library/Application Support/Trae/User/`, `Trae CN/User/`, and
    `TRAE SOLO CN/User/`
- **Linux:** `~/.config/Trae/User/`, `Trae CN/User/`, and `TRAE SOLO CN/User/`
- **Windows:** `%APPDATA%/Trae/User/`, `Trae CN/User/`, and `TRAE SOLO CN/User/`

Trae stores chats in `workspaceStorage/<hash>/state.vscdb` and
`globalStorage/state.vscdb`. Override these roots with `TRAE_DIR` or the
`agents.trae.dirs` configuration key. AgentsView watches `workspaceStorage` and
`globalStorage`, then reads chat records from those SQLite stores.

Trae legacy inline-message parsing is supported. Modern encrypted transcript
layouts are detected and reported as unsupported. HTTP remote target
resolution is still disabled. A Trae root is a full user profile, and AgentsView
does not archive or ship that profile wholesale. The follow-up path is
Windsurf-style curated file targets only: `state.vscdb`, `state.vscdb-wal`, and
`workspace.json` for each supported workspace store.

**Kimi Work default directories** vary by platform. Kimi Work is the
kimi-desktop app (the "daimon" runtime); it stores conversations as kimi-code
kernel wire logs under
`<root>/wd_<workspace>_<hash>/<session>/agents/<agent>/wire.jsonl`:

- **macOS:**
    `~/Library/Application Support/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/sessions/`
- **Linux:**
    `~/.config/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/sessions/`
    (or `~/.local/share/...` on some installs)
- **Windows:**
    `%APPDATA%/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/sessions/`

Only `conv-*` session directories are user conversations; auxiliary internal
sessions (`ctitle-*`, `sklsum-*`, `dvlt-*`) are excluded from discovery. Set
`KIMI_WORK_DIR` or `agents.kimi-work.dirs` if your installation stores them
elsewhere.

**Positron Assistant default directory** (macOS only):

- **macOS:** `~/Library/Application Support/Positron/User/`

Positron is an IDE built on VS Code, so sessions use the same
`workspaceStorage/<hash>/chatSessions/` layout as VS Code Copilot. As of
v0.20.0, Positron Assistant has a built-in default path only on macOS — on Linux
and Windows, set `POSITRON_DIR` or `agents.positron.dirs` to point at your
Positron user directory (for example, `~/.config/Positron/User` on Linux or
`%APPDATA%\Positron\User` on Windows).

**Posit Assistant** (posit-dev/assistant, also known as Databot) is a separate
product from the Positron IDE's built-in Assistant above. It stores one
directory per conversation under
`~/.posit/assistant/workspaces/<workspaceId>/<conversationId>/`, containing a
`conversation.json` message tree and an append-only `lm-messages.jsonl`
transcript; subagent runs nest under a `subagents/` subdirectory of their parent
conversation. All Posit Assistant hosts (Positron/VS Code extension, standalone,
desktop, TUI) share this location. Set `POSIT_ASSISTANT_DIR` or
`agents.posit-assistant.dirs` if your installation stores its workspaces
elsewhere.

**Cursor IDE** is the graphical editor, distinct from the Cursor command-line
agent above. AgentsView reads Composer sessions from Cursor's shared
`globalStorage/state.vscdb` database. The default follows Cursor's normal user
data directory on macOS, Linux, and Windows; set `CURSOR_IDE_DIR` or
`agents.cursor-ide.dirs` to override it. This database also contains
authentication and extension state, so Cursor IDE is deliberately excluded from
remote source-file sync. Parsed sessions still stay in the local archive and can
be shared through the normal PostgreSQL or DuckDB paths.

**Claude Cowork default directories** follow Claude Desktop's Electron user-data
location:

- **macOS:** `~/Library/Application Support/Claude/local-agent-mode-sessions/`
- **Linux:** `~/.config/Claude/local-agent-mode-sessions/`
- **Windows:**
    `%LOCALAPPDATA%\Packages\Claude_pzs8sxrjxfjjc\LocalCache\Roaming\Claude\local-agent-mode-sessions\`
    or `%APPDATA%\Claude\local-agent-mode-sessions\`

Set `COWORK_DIR` or `agents.cowork.dirs` when Claude Desktop stores
local-agent-mode sessions somewhere else.

**Codebuff / Freebuff sessions:** Codebuff and Freebuff share the same on-disk
layout under `~/.config/manicode/projects/`. Each session is a timestamped
directory containing `chat-messages.json` (the full transcript with subagent
invocations), `run-state.json` (metadata including the agent type and model),
and optional `chat-meta.json`.

AgentsView auto-classifies each session as **Codebuff** (paid) or **Freebuff**
(free tier) based on the `agentType` field in `run-state.json`: sessions whose
`agentType` contains `"free"` are filed under Freebuff. Both agent types appear
as separate filters in the session list so you can view paid and free sessions
independently.

Subagent tool calls (basher, code-searcher, file-picker, code-reviewer, etc.)
are parsed from the AI message blocks and displayed inline in the transcript
view. The agent template (e.g. `base2-free-deepseek`, `base2-free-mimo`) is read
from run-state.json's `agentType` field and shown in the session detail header.
This is the classification label used server-side to pick the per-step LLM; the
literal LLM is not persisted by the CLI and is not visible in the UI. Project
names are derived from the session's working directory via git-root detection.

Codebuff and Freebuff sessions report cost only, as one reported-cost row per
prompt. The CLI's on-disk format does not persist per-message input/output/cache
tokens. A row is attributed to the model the turn ran when the session records
one, such as a bring-your-own-key model, and otherwise to the agent template
(e.g. `base2-deepseek`, `base2-free-minimax-m3`). Per-model rates for `base2-*`
templates are not in the embedded pricing tables, so cache savings for these
rows are zero.

Each subagent a Codebuff or Freebuff session spawned is stored as its own
session, linked to its parent. Attached images, pasted text, and files appear
as labeled markers such as `[Image: ...]` and `[Text attachment: N chars]`.

Freebuff has no config key of its own. It shares the Codebuff provider, and the
parser classifies each session. Both CLIs move their config directory to
`FREEBUFF_CONFIG_DIR` when it is set, and AgentsView then discovers sessions
under `<FREEBUFF_CONFIG_DIR>/projects` instead of `~/.config/manicode/projects`.
`CODEBUFF_DIR` or `agents.codebuff.dirs` take precedence over both.

**OpenHands CLI shallow watch:** OpenHands stores each conversation in its own
subdirectory, which would consume one recursive file watch per session and can
exhaust inotify limits on Linux. AgentsView watches the root
`~/.openhands/conversations/` directory non-recursively and relies on the
15-minute periodic sync to pick up changes inside existing conversations. New
conversation directories are still detected immediately. The server's startup
log reports how many directories are watched this way:

```
Watching 74 directories for changes (2 shallow) (76ms)
```

**Devin CLI root:** Point `DEVIN_DIR` or `agents.devin.dirs` at the local root
that contains Devin's `cli/` directory, not at copied config or OAuth files. The
default roots are `~/Library/Application Support/devin` on macOS and
`~/.local/share/devin` on Linux, and AgentsView discovers session data under
`<root>/cli/...`. When sharing a path publicly, redact parent directories and
keep only the relevant tail, for example `.../Application Support/devin` or
`.../.local/share/devin`.

AgentsView intentionally ignores copied config/OAuth locations because those
paths are not the session archive source and may contain sensitive account
material. When filing bugs, share only the redacted local-share root and
directory shape, never pasted tokens, OAuth files, or other secrets.

**OpenCode storage backend:** As of 0.24.0, AgentsView reads both of OpenCode's
layouts. If a `storage/session/` directory exists under the OpenCode root,
sessions are parsed from the per-file JSON layout (`storage/session`,
`storage/message`, `storage/part`); otherwise the legacy `opencode.db` SQLite
file is used. Detection is automatic and requires no configuration. In storage
mode, the file watcher scopes itself to the `storage/` subtree rather than the
entire OpenCode directory, so unrelated OpenCode state like binaries, logs, and
caches no longer trigger sync events. In SQLite mode, it watches the
`opencode.db` parent.

Kilo and MiMoCode use the same OpenCode-format storage reader. Kilo reads from
`storage/session`, while MiMoCode reads from `storage/session_diff` when
present; both fall back to their SQLite databases when the file-backed storage
layout is absent.

**aider discovery:** aider writes one `.aider.chat.history.md` file per
repository instead of a central session directory. AgentsView does not scan for
Aider logs unless you opt in with `AIDER_DIR` or `agents.aider.dirs`. Always-on
home-directory discovery has caused unwanted macOS privacy prompts from
background refreshes, so Aider discovery is limited to roots you explicitly
configure. On macOS, broad home roots still skip protected top-level folders
unless one of those folders is configured directly.

**Warp default directories** vary by platform:

- **macOS:**
    `~/Library/Group Containers/2BBY89MBSN.dev.warp/Library/Application Support/dev.warp.Warp-Stable/`
- **Linux:** `~/.local/state/warp-terminal/`
- **Windows:** `~/AppData/Local/warp/Warp/data/`

**Zed default directories** vary by platform:

- **macOS:** `~/Library/Application Support/Zed/`
- **Linux:** `~/.local/share/zed/`
- **Windows:** `~/AppData/Local/Zed/`

Zed stores all assistant threads in a single `threads/threads.db` SQLite
database under its data directory. AgentsView reads it directly, including model
names and per-request token usage.

**Kiro IDE default directories** vary by platform:

- **macOS:**
    `~/Library/Application Support/Kiro/User/globalStorage/kiro.kiroagent/`
- **Linux:** `~/.config/Kiro/User/globalStorage/kiro.kiroagent/`
- **Windows:** `~/AppData/Roaming/Kiro/User/globalStorage/kiro.kiroagent/`

**RooCode default directories** vary by platform:

- **macOS:**
    `~/Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline/`
- **Linux:** `~/.config/Code/User/globalStorage/rooveterinaryinc.roo-cline/`
- **Windows:**
    `~/AppData/Roaming/Code/User/globalStorage/rooveterinaryinc.roo-cline/`

RooCode (rooveterinaryinc.roo-cline) is a VSCode extension that stores sessions
under `tasks/<taskId>/` in VSCode's globalStorage directory. Each task directory
contains `history_item.json` (metadata including task description, model name,
workspace path, token counts, and recorded cost) and `ui_messages.json` (the
Cline-format transcript with user prompts, assistant responses, reasoning
blocks, and tool calls). AgentsView parses the `apiConfigName` field from
`history_item.json` as the session model, extracts project names from the
workspace path via git-root detection, and emits the recorded `totalCost` as a
usage event for cost tracking.

RooCode was shut down on May 15, 2026. ZooCode (Zoo-CodeInc.zoo-cline) is the
active community fork and will be supported separately. Set `ROOCODE_DIR` or
`agents.roocode.dirs` if your VSCode globalStorage directory is elsewhere.

**Kilo (legacy) default directories** vary by platform, all rooted at the
canonical lowercase `kilocode.kilo-code` global storage directory that VSCode
writes on disk:

- **macOS:**
    `~/Library/Application Support/Code/User/globalStorage/kilocode.kilo-code/`
- **Linux:** `~/.config/Code/User/globalStorage/kilocode.kilo-code/`
- **Windows:** `%APPDATA%/Code/User/globalStorage/kilocode.kilo-code/`

Each `<root>/tasks/<uuid>/` task directory carries three JSON files:
`task_metadata.json` (only stores `files_in_context`), the Claude-shaped
`api_conversation_history.json`, and the Cline-shaped `ui_messages.json`.
AgentsView folds the latter two into a composite fingerprint with
`task_metadata.json` as the source anchor so changes to any of the three trigger
a reparse. Sessions are parsed through RooCode-descended Cline message handling
(tool-call and result pairing, reasoning pipeline, compact boundaries, error
linking). Set `KILO_LEGACY_DIR` or `agents.kilo-legacy.dirs` when the legacy
extension stores its data outside the standard locations.

**Kilo (legacy) vs Kilo.** These are two different agents. *Kilo* (the `kilo`
agent) is the OpenCode-based core at `~/.local/share/kilo/`; it covers both the
Kilo CLI and the rebuilt Kilo Code VS Code extension, which share that same
`kilo.db`. *Kilo (legacy)* (the `kilo-legacy` agent) is the legacy
RooCode-derived VS Code extension that wrote per-task JSON under
`kilocode.kilo-code/tasks/` and stopped receiving new sessions after Kilo
rebuilt the extension on OpenCode (public beta 2026-03-10, GA 2026-04-02). The
`kilo-legacy` agent is frozen at that legacy format and only archives older
sessions; newer Kilo VS Code activity appears under `kilo`.

**Antigravity CLI transcript sources:** Antigravity CLI has used both SQLite
databases and AES-encrypted `.pb` files. AgentsView reads whichever source is
richest, in this order:

1. **Decrypted trajectory sidecar.** For either format, if a
   `<uuid>.trajectory.json` file sits next to the source `.db` or `.pb` file
   (under `conversations/` or `implicit/`) and covers the session, AgentsView
   uses it as the source of truth for the full structured transcript — messages,
   tool calls, tool results, reasoning, and diffs. This is the highest-fidelity
   source for both formats. These sidecars are written out-of-process by
   [agy-reader](https://github.com/mjacobs/agy-reader), which performs the
   decryption; AgentsView reads the resulting plain JSON as untrusted input and
   needs no `ANTIGRAVITY_KEY` in this mode.
1. **Plaintext brain transcript.** Since 0.46.1, AgentsView reads
   `brain/<uuid>/.system_generated/logs/transcript.jsonl`. For SQLite sessions,
   it uses this transcript when it covers every database step. Model and token
   usage from the database attach to the matching model replies. Prompts from
   `history.jsonl` that the transcript has not recorded yet are added back;
   repeated prompts stay separate turns. For older `.pb` sessions, a brain
   transcript can supply the conversation without a decryption key.
1. **SQLite trajectory database.** Newer Antigravity CLI releases write
   `conversations/<uuid>.db`. Without a covering sidecar or brain transcript,
   AgentsView opens the database read-only and decodes the trajectory steps
   directly. This direct decode is heuristic: it recovers prompts and tool-call
   names but not full structured tool results, reasoning, or diffs — a degraded
   **summary mode** transcript. If both `conversations/<uuid>.db` and
   `conversations/<uuid>.pb` exist, the SQLite database wins. Change detection
   includes data-bearing `<uuid>.db-wal` changes. SQLite's temporary
   `<uuid>.db-shm` index does not trigger a re-import.
1. **In-process `.pb` decryption.** With no sidecar present, set
   `ANTIGRAVITY_KEY` (base64-encoded AES key, 16/24/32 bytes after decoding)
   before starting AgentsView and it decrypts the `.pb` payloads itself,
   mirroring the upstream Python tool
   [`antigravity_decryptor`](https://github.com/arashz/antigravity_decryptor).
1. **Plaintext summary mode.** Otherwise AgentsView reads only `history.jsonl`
   and the `brain/` summaries — enough to populate session metadata and a
   high-level transcript.

If the database cannot be read, AgentsView keeps archived replies and reports
the parse error unless a trajectory sidecar supplies the transcript. A brain
transcript alone cannot establish coverage of an unreadable database. Partial
transcripts from readable databases remain eligible for retry.

Antigravity IDE and CLI databases work on read-only mounts when their pending
writes have been checkpointed into the database. AgentsView does not ignore
uncheckpointed writes to make a read succeed. The 0.46.1 upgrade reprocesses
existing Antigravity CLI sessions to recover missing repeated turns.

Heuristic `.db` decode, in-process `.pb` decryption, and plaintext summary mode
show a "Summary mode" badge in the detail header. Covering trajectory sidecars
and brain transcripts show the full transcript. Install `agy-reader` when you
need a structured trajectory sidecar for `.db` or `.pb` sessions:

```bash
go install github.com/mjacobs/agy-reader@latest
agy-reader --sync
agy-reader --watch
```

Override any default with an environment variable (single directory). For Aider,
this opt-in is required because there is no default discovery root:

```bash
export AIDER_DIR=~/code
export AMP_DIR=~/custom/amp # historical local Amp threads only
export AUGURE_CODE_SESSIONS_DIR=~/custom/augure/sessions
export AUGURE_DESKTOP_DIR=~/custom/augure-desktop
export ANTIGRAVITY_DIR=~/custom/antigravity
export ANTIGRAVITY_CLI_DIR=~/custom/antigravity-cli
export CLAUDE_PROJECTS_DIR=~/custom/claude
export CLAUDE_CONFIG_DIR=~/custom/claude-home # re-roots the default projects/ path
export OPENCLAUDE_PROJECTS_DIR=~/custom/openclaude/projects
export OPENCLAUDE_CONFIG_DIR=~/custom/openclaude
export COWORK_DIR=~/custom/cowork
export CODEBUFF_DIR=~/custom/manicode/projects
export FREEBUFF_CONFIG_DIR=~/custom/freebuff-config # re-roots the default projects/ path
export CODEX_SESSIONS_DIR=~/custom/codex
export CODEX_HOME=~/custom/codex-home # re-roots the default sessions/ paths
export CLINE_DIR=~/custom/cline/data/sessions
export COMMANDCODE_PROJECTS_DIR=~/custom/commandcode
export COPILOT_DIR=~/custom/copilot
export CRUSH_DIR=~/custom/crush
export DEVIN_DIR=~/Library/Application\ Support/devin
export CORTEX_DIR=~/custom/cortex
export CURSOR_PROJECTS_DIR=~/custom/cursor
export DEEPSEEK_TUI_SESSIONS_DIR=~/custom/deepseek/sessions
export DEEPSEEK_HARNESS_SESSIONS_DIR=~/custom/deepseek-harness/sessions
export FORGE_DIR=~/custom/forge
export GEMINI_DIR=~/custom/gemini
export GOOSE_PATH_ROOT=~/custom/goose
export GPTME_DIR=~/custom/gptme/logs
export GROK_DIR=~/custom/grok/sessions
export HERMES_SESSIONS_DIR=~/custom/hermes
export IFLOW_DIR=~/custom/iflow
export JUNIE_DIR=~/custom/junie/sessions
export JUNIE_HOME=~/custom/junie-home # re-roots the default sessions/ path
export KILO_DIR=~/custom/kilo
export KIMI_DIR=~/custom/kimi
export KIMI_WORK_DIR=~/custom/kimi-work
export KIRO_SESSIONS_DIR=~/custom/kiro
export KIRO_IDE_DIR=~/custom/kiro-ide
export KILO_LEGACY_DIR=~/custom/kilo-legacy
export MIMOCODE_DIR=~/custom/mimocode
export MUSE_SESSIONS_DIR=~/custom/muse/sessions
export VIBE_SESSIONS_DIR=~/custom/vibe/logs/session
export OMP_DIR=~/custom/omp
export OPENCLAW_DIR=~/custom/openclaw
export OPENCODE_DIR=~/custom/opencode
export OPENCODEREVIEW_DIR=~/custom/opencodereview/sessions
export OPENHANDS_CONVERSATIONS_DIR=~/custom/openhands
export PI_DIR=~/custom/pi
export PI_CODING_AGENT_DIR=~/custom/pi-home # sessions are under this home
export PI_CODING_AGENT_SESSION_DIR=~/custom/pi-sessions # direct session directory
export FX_SESSIONS_DIR=~/custom/fx/sessions
export TAU_SESSIONS_DIR=~/custom/tau/sessions
export PIEBALD_DIR=~/custom/piebald
export POOLSIDE_DIR=~/custom/poolside/trajectories
export POSIT_ASSISTANT_DIR=~/custom/posit-assistant/workspaces
export POSITRON_DIR=~/custom/positron
export QCLAW_DIR=~/custom/qclaw
export QODER_PROJECTS_DIR=~/custom/qoder/projects
export QWEN_PROJECTS_DIR=~/custom/qwen
export QWENPAW_DIR=~/custom/qwenpaw
export REASONIX_DIR=~/custom/reasonix
export ROOCODE_DIR=~/custom/roocode
export SHELLEY_DIR=~/custom/shelley
export TRAE_DIR=~/custom/trae/User
export TRAEX_SESSIONS_DIR=~/custom/trae/cli/sessions
export VISUALSTUDIO_COPILOT_DIR=~/custom/visualstudio-copilot/traces
export VSCODE_COPILOT_DIR=~/custom/vscode
export WINDSURF_DIR=~/custom/windsurf/User
export WARP_DIR=~/custom/warp
export WORKBUDDY_PROJECTS_DIR=~/custom/workbuddy
export CODEBUDDY_DIR=~/custom/codebuddy
export ZCODE_DIR=~/custom/zcode/cli
export ZED_DIR=~/custom/zed
export ZENCODER_DIR=~/custom/zencoder
```

### CodeBuddy

Tencent CodeBuddy CN uses the `codebuddy` agent ID and reads `history` beneath
the configured `CodeBuddyExtension/Data` directory. On Windows the default data
directory follows `%LOCALAPPDATA%`, with `~/AppData/Local` as a fallback when
that variable is unset or not absolute. macOS uses
`~/Library/Application Support/CodeBuddyExtension/Data`; Linux uses
`~/.config/CodeBuddyExtension/Data`. `CODEBUDDY_DIR` replaces these defaults.

Session projects use the working directory when present, falling back to the
workspace identifier. Message changes, including deletion, refresh their owning
session; workspace index changes refresh sessions in that workspace. See the
[format evidence](https://github.com/kenn-io/agentsview/blob/main/docs/internal/session-format-sources.md#codebuddy)
for usage accounting assumptions and verification limits.

### Evener

Evener discovery reads semantic transcript **format v2** under
`$XDG_STATE_HOME/evener/projects/<project-id>/sessions/`, falling back to
`~/.local/state/evener/projects/`. Override the state root with `EVENER_DIR` or
`agents.evener.dirs`; an explicit project state directory or sessions directory
also works. Older transcript versions are unsupported; existing archive data is
not deleted.

```toml
[agents.evener]
dirs = ["~/session-sources/evener"]
```

The provider reads messages, thinking, tools, recorded usage, session names, and
fork/subagent relationships. Metadata-only edits refresh the session. Verified
copied fork prefixes are omitted from child sessions, following the Codex
provider policy. If the parent is missing or cannot be verified, child history
is retained and shared usage may appear in both sessions.

Costs represent recorded conversation usage, not provider invoices. Model switch
records with structured identities update the model context; an older prose-only
switch cannot establish a fallback billing model. Explicit per-response model
identities still take precedence. Media that cannot be represented by the
existing transcript view is shown as a descriptive placeholder, without fetching
referenced files or URLs.

Remote sync uses AgentsView's HTTP transport. This provider does not connect to
Evener hubs or add an S3 transport. Remote sync skips files deleted after
discovery, including metadata left behind when its transcript is deleted.

### Codex Cloud

The `codex-cloud` source imports the chats that ChatGPT dot, Codex web, and
Codex cloud tasks keep on OpenAI's servers. These chats never exist as local
files, so AgentsView fetches them with the ChatGPT login of a local Codex home
and keeps a compressed copy under `<data dir>/sources/codex-cloud/`. Each chat
becomes a session.

The source is off until you turn it on. It is configured in `config.toml` only;
the Settings page does not show it.

```toml
[agents.codex-cloud]
enabled = true
# live_tokens = true    # listen in on running chats (off by default)
```

| Key              | Default                                                              | Meaning                                                 |
| ---------------- | -------------------------------------------------------------------- | ------------------------------------------------------- |
| `enabled`        | `false`                                                              | Fetch chats and capture tokens from OpenAI              |
| `live_tokens`    | `false`                                                              | Listen in on running chats to record their token counts |
| `endpoint`       | `wss://codex-cloud-backend.chatgpt.com/`                             | Chat service; for tests only                            |
| `usage_endpoint` | `https://chatgpt.com/backend-api/wham/usage/thread_usage/query_v2`   | Plan usage report; for tests only                       |

Live capture is off by default, so `enabled = true` only imports chats. Set
`live_tokens = true` to also listen in on your running chats. Read
[Tokens and plan usage](#tokens-and-plan-usage) first: listening in can change
a chat on OpenAI's side.

An endpoint override must use `wss` or `https` on `chatgpt.com`, `openai.com`,
or a subdomain of either. Plain `ws` and `http` are accepted only for loopback
addresses. Neither client follows redirects, so the login is never sent to a
host these rules did not check.

An unknown key or a bad value turns this source off and leaves the rest of the
config working. The error names `config.toml`, the key, and the problem, and
shows in the status bar and in `agentsview serve status`.

Listing `codex-cloud` in `disabled_agents` stops fetching, live capture, and
parsing even when `enabled = true`. The daemon re-reads
`[agents.codex-cloud]`, `disabled_agents`, and `[agents.codex] homes` at the
start of each run, so a hand edit turns fetching and live capture on or off
without a restart. Parsing of chats already fetched follows the daemon's
provider list instead: adding `codex-cloud` to `disabled_agents` by hand stops
parsing only after a restart or a save on the Settings page. If `config.toml`
cannot be read at all, the source keeps its last good settings and the status
says so.

#### What it does with your login

AgentsView reads the ChatGPT access token from the `auth.json` of each Codex
home: `CODEX_HOME` (or `~/.codex` when it is unset) and each entry in
`[agents.codex] homes`. It sends the token only to the endpoints above, never
writes to a Codex home, and never stores the token. Homes signed in to the same
ChatGPT account share one import.

- If OpenAI does not accept a home's login, or asks for a fresh one, the status
  tells you to run `codex login` for that home. AgentsView tries the home again
  when its `auth.json` changes, or after one hour.
- If OpenAI refuses the account's requests, check that the ChatGPT account can
  use Codex. AgentsView waits one hour, then doubles the wait up to one day. A
  refusal does not show that the login is wrong, so the status does not suggest
  `codex login`.
- Codex can keep the login in the system keyring instead of `auth.json`, or not
  save it at all. AgentsView does not read the keyring. To let AgentsView use
  such a home, set `cli_auth_credentials_store = "file"` in that home's
  `config.toml`: change the existing line, or add one near the top, before any
  `[section]`. Keep only one line for that key, because Codex cannot load a
  `config.toml` that sets it twice. Then run `codex login`. Codex then keeps the
  ChatGPT login in `auth.json`, a plain file that any program running as you can
  read. With the default `auto` store, a refused login can mean either an
  expired login in `auth.json` or a current login in the keyring; the status
  names both and suggests `codex login` first.

#### Private endpoints

OpenAI does not document these endpoints, and they may change or stop working
without notice. OpenAI may also treat third-party clients differently from its
own apps. When a reply no longer matches what AgentsView expects, the status
says "OpenAI's service answered in a way this version of AgentsView can't read"
and asks you to update AgentsView, or to check an `endpoint` or
`usage_endpoint` override. What stops depends on which request failed:

- A reply from the chat service while importing pauses imports from that
  account. AgentsView tries again after one hour, then at longer waits up to
  one day.
- A reply AgentsView cannot read while it looks for running chats, connects to
  one, or listens in on one stops live capture for that account. Imports
  continue. To listen in again, restart AgentsView, or set
  `live_tokens = false`, wait until the status says "Live tokens: off", then set
  it back to `true`. A quick toggle can be missed, because AgentsView checks the
  setting every 30 seconds.
- A plan usage reply AgentsView cannot read is retried like any failed plan
  usage request.

Sessions already imported are kept.

#### Tokens and plan usage

The hosted chat history has no token counts. OpenAI reports token counts only
to a client connected to a chat while it runs. With `live_tokens = true`, the
daemon checks every 30 seconds for chats updated in the last 5 minutes whose
newest turn is still running. It listens in on each one, up to 10 per account,
records every token update, and disconnects when the turn ends or after 6
hours.

Listening in is the only request that touches a chat on OpenAI's side.
AgentsView connects the way Codex reopens a chat, with the request OpenAI calls
`thread/resume`:

- While AgentsView is connected, the running task reports AgentsView as its
  client, and later turns' starting context can mention it.
- If a turn ends just before AgentsView connects, connecting loads the stopped
  chat on OpenAI's side and starts its MCP servers, the tool servers the chat
  is set up to use. In testing nothing ran and
  the chat did not change. The chat may move to the top of your chat list in
  OpenAI's apps. AgentsView disconnects at once and does not connect for that
  turn again. To make this less likely, AgentsView checks again right before
  connecting that the chat was updated in the last 5 minutes.
- In Codex, connecting to a chat whose turn has ended runs the chat's idle
  step. That step continues an active goal and sends any queued messages, so it
  can start a turn you did not ask for. AgentsView reads the chat's goal first
  and does not connect to a chat with an active goal. It cannot check for
  queued messages: if a turn ends just before AgentsView connects while you
  have messages queued, for example after you stopped the turn, connecting can
  send them.
- Not tested: AgentsView never answers approval prompts. If a chat asks for
  approval while AgentsView is connected, it is unknown whether your app still
  shows that prompt after AgentsView disconnects.

Remove `live_tokens = true` or set it to `false` to stop listening in. Fetching
and plan usage continue. The status bar and `agentsview serve status` show a
live tokens line for each account. It says how many running chats AgentsView is
listening in on, that it is listening in on none right now, that live tokens are
off and why, or that live tokens stopped after a reply AgentsView could not
read.

A detached daemon normally exits once it has been idle for `daemon_idle_timeout`.
While AgentsView is listening in on a running chat, the daemon stays up past that
timeout, for at most 6 hours per chat. A daemon that is shutting down does not
start listening in on new chats.

Each token update carries the chat's running total, so an update also covers
every turn that finished before it, back to the last time OpenAI restarted that
total. A finished turn counts as captured when AgentsView received a token
update for it and saw it end on the same connection, or when a token update
from a later turn covers it. Any other finished turn, such as a chat's last
turn that ran while AgentsView was not connected, may be missing some or all of
its tokens from the chat's counts. A session with such turns and with recorded
token counts stays in the token and cost totals, which then run low for that
session.

A chat that AgentsView never listened in on, or listened in on without receiving
a token update, has no token counts. For those, AgentsView asks OpenAI's usage
report for the chat's share of your plan's weekly and five-hour limits, and the
credits figure the report includes. That plan usage shows in the session
details. Such sessions are left out of token and cost totals; `agentsview usage`
reports how many were left out. That count includes a chat AgentsView has not
asked about yet, such as one waiting while plan usage is paused. Its session
details say "Plan usage: not checked yet" until the first answer arrives.

AgentsView asks for plan usage again after a failed request or an incomplete
report, waiting longer each time, up to 6 hours between requests. It refreshes
complete figures once a day. Refreshes stop 7 days after the chat's last change,
and the session keeps the last complete figures.

For a chat still waiting for complete figures, AgentsView stops after the first
answer that arrives 7 days or more after the chat's last change.
If that answer includes the chat, AgentsView keeps what it reports, even partial
figures or none. If it leaves the chat out, AgentsView keeps the figures it
stored before, or shows none if it never got any. If 3 requests in a row fail
after those 7 days, AgentsView stops asking, and the session details say it
could not check plan usage.

Some problems pause plan usage for the whole account instead: OpenAI limiting
requests, OpenAI saying the account has no usage report, or no Codex home with
a usable login. While paused, no request is sent, so a chat can wait more than 7
days after its last change. The first answer after the pause ends is
the last one AgentsView takes for that chat, under the same rules.

#### Limits

- Live capture listens in on a chat only within 5 minutes of the chat's last
  update. A turn that ends before AgentsView connects gets no live tokens. If
  the daemon is down, or all 10 connections for the account are busy, for
  longer than that while a turn runs, that turn is not captured live either.
- The 5 minutes compare OpenAI's update time with this computer's clock. A
  clock that runs more than a few minutes fast makes running chats look old,
  so AgentsView skips them. A clock that runs slow makes stopped chats look
  recent, so AgentsView connects to more stopped chats.
- A chat's first token record is dated when AgentsView saw it, even when the
  earlier turns it covers ran on an earlier day.
- Live capture does not see two kinds of model change. After a change reported
  only as a chat settings update, tokens count toward the earlier model until
  AgentsView next connects. After a reroute that happened before AgentsView
  connected, tokens count toward the chat's configured model.
- When one token update covers several model calls, it is priced at base rates.
  This can understate the cost of long-context calls inside it.
- If OpenAI's running counter restarts in a way AgentsView does not see as a
  decrease, the turns in between are undercounted.
- AgentsView does not listen in on running sub-agent chats yet, so their tokens
  are not captured. Dot-delegated tasks are not sub-agent chats in this sense,
  and AgentsView listens in on them like any other chat.
- A chat that fails to import for 7 days without changing is no longer
  retried, even while OpenAI lists it as active. The AgentsView log, `debug.log`
  in the data directory, names it. AgentsView tries it again when it changes.
- ChatGPT dot's own conversation has no turns and is never imported. Its
  delegated tasks appear as top-level sessions.

#### Use one machine

Every machine produces the same session ID for the same chat. Enable this
source, or at least live capture, on one machine only:

- PostgreSQL keeps the first machine's copy of each session and skips the other
  machine's pushes of it, so tokens or plan usage captured only on the second
  machine never arrive.
- ClickHouse keeps whichever machine pushed last. The other machine's copy
  returns only after its next local change to that session.
- A hosted hub keeps raw uploads per device, so two machines uploading one chat
  can produce two sessions and count its tokens twice.
- Both machines would listen in on the same running chats.

### Disabling Session Providers

Exclude session providers you do not use by listing their IDs in
`disabled_agents`:

```toml
disabled_agents = ["gemini"]
```

Because Freebuff shares the Codebuff provider, listing `"codebuff"` disables
local filesystem ingestion for both Codebuff and Freebuff.

The setting applies only to local filesystem discovery, targeted local file
sync, file watching, and scheduled polling. It does not affect HTTP remote
imports, and it does not restrict HTTP, PostgreSQL, DuckDB, or archive exports.
`RemoteSyncExcluded` is the separate provider capability that keeps unsafe
source trees out of remote exports.

A change saved on the Settings page applies to the running daemon right away:
file watching and polling switch to the new provider set. Sessions already on
disk for a newly enabled provider arrive with the next sync. The daemon reads
edits made directly to `config.toml` at its next start. Restart any separate
`pg push --watch`, `clickhouse push --watch`, or `duckdb push --watch` process
after changing the setting. Previously archived sessions from a disabled
provider remain available and exportable, including during archive rebuilds.
The setting does not disable that provider as a Recall execution backend.

### Multiple Directories

To scan more than one directory per agent — for example, when running Windows
and WSL side by side — add array fields to `~/.agentsview/config.toml`:

```toml
[agents.claude]
dirs = [
  "~/.claude/projects",
  "/mnt/c/Users/you/.claude/projects",
]

[agents.codex]
dirs = [
  "~/.codex/sessions",
]

[agents.opencodereview]
dirs = ["~/.opencodereview/sessions"]
```

Every locally discovered provider uses the same `[agents.<id>]` table with a
`dirs` array. Use its provider ID, such as `claude`, `codex`, `pi`, or `gemini`.
Environment variables take precedence over `dirs`. A non-empty array replaces
the default paths, and an explicit empty array clears the default local roots.

On normal startup, AgentsView converts existing top-level directory and home
keys to these tables and saves the result. Read-only commands interpret the old
format without rewriting it. If both forms set the same field, startup reports
the conflict and leaves the file unchanged; remove one of the entries. Settings
writes only the new format. Conversion preserves unrelated settings, but
rewrites TOML formatting and comments.

All listed directories are discovered, watched, and synced independently.

Pi also honors its native `PI_CODING_AGENT_DIR` and
`PI_CODING_AGENT_SESSION_DIR` variables in local discovery. The agent
home variable changes the default to `<agent-home>/sessions`; the session
variable points directly at a session directory. `PI_DIR` takes precedence over
`PI_CODING_AGENT_SESSION_DIR`, which takes precedence over `agents.pi.dirs` in
`config.toml`. A configured `agents.pi.dirs` array replaces the home-derived
default; an empty array clears it. With no overrides, Pi uses
`~/.pi/agent/sessions`.

### Alternate Agent Homes

Claude Code, Codex, Junie, and Pi support alternate homes. Each home can hold a
separate account or settings profile. Register the home directories themselves
in `homes`; AgentsView derives their native session directories:

```toml
[agents.claude]
homes = ["~/.claude-work", "~/.t3code/instances/alpha/claude"]

[agents.codex]
homes = ["~/.codex-work", "~/.t3code/instances/alpha/codex"]

[agents.junie]
homes = ["~/.junie-work", "~/.junie-personal"]

[agents.pi]
homes = ["~/.pi-work/agent", "~/.pi-personal/agent"]
```

| Agent       | Home variable         | Transcripts scanned                             | Sidecars read from each home           |
| ----------- | --------------------- | ----------------------------------------------- | -------------------------------------- |
| Claude Code | `CLAUDE_CONFIG_DIR`   | `<home>/projects/`                              | none                                   |
| Codex       | `CODEX_HOME`          | `<home>/sessions/`, `<home>/archived_sessions/` | `history.jsonl`, `session_index.jsonl` |
| Junie       | `JUNIE_HOME`          | `<home>/sessions/*/events.jsonl`                | `sessions/index.jsonl`                |
| Pi          | `PI_CODING_AGENT_DIR` | `<home>/sessions/`                              | none                                   |

Homes are additive to defaults, environment overrides, the same table's `dirs`,
and `[[session_sources]]`. To scan only the listed homes, set `dirs = []` and
leave directory environment overrides and `session_sources` entries unset. Homes
must be local directories; use `dirs` for `s3://` roots on providers that
support S3. Sessions from each home appear under their native provider. Pi's
`homes` values correspond to `PI_CODING_AGENT_DIR`, not its parent `.pi`
directory or a direct session directory.

Homes added or removed on the Settings page apply to the running daemon right
away. Sessions already in an added home arrive with the next sync. The daemon
reads `homes` edited directly in `config.toml` at its next start.

Other providers support multiple explicit `dirs`; they do not yet accept
`homes`. A home mapping must match the provider's native layout before it can be
enabled.

At configuration load, local session roots become absolute paths with symbolic
links resolved. Duplicate roots are removed before scanning or watching starts.
Links to directories that do not exist yet retain their resolved destination, so
creating the directory later does not register a second scan root. Each home's
metadata paths remain separate from the shared transcript root.

The Session Providers section of the Settings page edits the same lists. Adding
or removing a home there updates `homes` in that provider's `[agents.<id>]`
table in `config.toml`. Like the provider enable toggles, the change takes
effect after the AgentsView daemon and any separate push-watch process restart.

![Codex alternate homes in Session Providers settings](/docs/assets/generated/screenshots/settings-agent-homes.png)

#### Choosing a layout

There are two sensible ways to run a second home. Pick based on whether the
transcripts should be shared.

**Fully separate homes.** Each home owns its own transcripts. Use this when the
instances must not see each other's history, such as a personal and a work
account. Register every home and AgentsView scans each one:

```bash
mkdir -p ~/.codex-work
CODEX_HOME=~/.codex-work codex login
```

```toml
[agents.codex]
homes = ["~/.codex-work"]
```

**Shared transcripts, separate profile.** The second home keeps its own
`config.toml`, skills, and per-instance metadata, but its session directories
are symbolic links into the primary home. Use this when you want different
settings or skill profiles while keeping one searchable history. This is the
recommended layout for profile switching, because nothing is copied and no
session exists in two places:

```bash
primary=~/.codex
alt=~/.codex-profile
mkdir -p "$alt"
ln -s "$primary/sessions" "$alt/sessions"
ln -s "$primary/archived_sessions" "$alt/archived_sessions"
ln -s "$primary/history.jsonl" "$alt/history.jsonl"
# leave config.toml, skills, session_index.jsonl, and the *.sqlite state files
# unlinked so the profile keeps its own settings and metadata
CODEX_HOME="$alt" codex
```

```toml
[agents.codex]
homes = ["~/.codex-profile"]
```

Codex writes thread titles to `session_index.jsonl` in whichever home the rename
happened in. Do not link that file; AgentsView reads every home's copy. HTTP
remote sync also transfers these indexes and preserves their associations with
the shared transcripts, so imported sessions retain titles from alternate homes.
When an index is removed or loses an entry, the next sync uses a title from the
remaining configured indexes. If none names the session, AgentsView keeps its
last known title. Upgrade both ends of HTTP sync together; older protocol
versions are rejected.

The same shape works for Claude Code by linking `<alt>/projects` to
`~/.claude/projects`. Claude keeps no title index, so there is nothing else to
leave unlinked for AgentsView's sake.

#### How links and duplicates are handled

- Roots that resolve to the same directory, including through symbolic links,
    are scanned once. Configuration loading stores the resolved absolute root
    and its provider metadata paths. A matching `[[session_sources]]` entry
    still supplies the machine key.
- Metadata belongs to the configured transcript root. If two homes share
    `sessions/` but keep separate `archived_sessions/`, their shared sessions
    read both homes' metadata; each archive reads only its own home's metadata.
- Codex sidecars are read from the effective root's home and from every alias
    home. Activity hints come from each distinct `history.jsonl`; a linked copy
    is read once. Thread titles concatenate every `session_index.jsonl`, and
    when two homes name the same session the most recently written index wins.
- Changing a title in an alias home is picked up live. Each home's directory is
    watched for `session_index.jsonl` changes and mapped back to the shared
    transcripts.
- A home whose link target does not exist yet is kept by its configured path and
    starts working once the target appears.

### Machine-Labeled Filesystem Sources

Use `[[session_sources]]` when a root was produced on another machine and
transported to this AgentsView host:

```toml
[[session_sources]]
agent = "copilot"
dir = "/srv/session-archive/buildbox/copilot"
machine = "0123456789abcdef0123456789abcdef" # Peer installation ID
```

The fields are `agent`, `dir`, and optional `machine`. Entries are additive to
the per-agent arrays, defaults, and environment variables above. Equivalent
roots are deduplicated; a structured entry supplies the machine key when it
duplicates a shorthand root. Use the peer's ID from `telemetry-install-id` for a
remote root. For a local root, omit `machine` to use this installation's ID. If
an existing local source sets `machine` to a hostname, remove that setting.
Source keys are used literally; filter aliases and display labels do not make a
source local.

Machine attribution is captured when each session is first ingested. Changing an
entry's `machine` value affects newly discovered sessions but does not relabel
existing sessions during ordinary syncs or `agentsview sync --full`. To adopt
old keys belonging to this installation, use
[`agentsview db adopt-machine`](/docs/commands/#agentsview-db-adopt-machine).

See [Filesystem Session Sync](/docs/filesystem-sync/) for multi-machine
examples, transport safety, ID deduplication, watcher behavior, and the
comparison with PostgreSQL.

### S3-Compatible Session Sources

Claude, Codex, Cursor, and IcodeMate session roots can also be `s3://` URIs.
These are the providers whose current formats can be discovered and parsed as
individual objects; future providers opt in through the same capability. This is
useful when several machines push their raw session files to object storage and
one central AgentsView instance reads them without SSH access to those machines.

```toml
[agents.claude]
dirs = [
  "~/.claude/projects",
  "s3://agent-archive/laptop/raw/claude",
]

[agents.codex]
dirs = [
  "~/.codex/sessions",
  "s3://agent-archive/laptop/raw/codex",
]

[agents.cursor]
dirs = [
  "~/.cursor/projects",
  "s3://agent-archive/laptop/raw/cursor",
]

[agents.icodemate]
dirs = [
  "~/.local/share/icodemate",
  "s3://agent-archive/laptop/raw/icodemate",
]
```

S3 sources are read-only inputs to the normal local sync. AgentsView lists
matching objects, fetches each changed object to a temporary file, parses it
with the existing per-agent parser, records the original `s3://` URI as
`file_path`, and removes the temporary file. No persistent local mirror is
created.

Credentials and endpoint configuration use standard AWS-style environment
variables:

```bash
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1
export AWS_S3_ENDPOINT=https://s3.amazonaws.com
```

`AWS_S3_ENDPOINT` is optional for AWS S3. Set it for S3-compatible services such
as MinIO, Aliyun OSS, or Cloudflare R2. `http://` endpoints are accepted only
for loopback hosts such as `localhost` or `127.0.0.1`. For a non-loopback HTTP
endpoint, set `AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT=true`; use that override
only for trusted private networks because session transcripts travel without
TLS.

Expected object layouts:

```text
s3://bucket/.../<machine>/raw/claude/<project>/<uuid>.jsonl
s3://bucket/.../<machine>/raw/claude/<project>/subagents/.../agent-*.jsonl
s3://bucket/.../<machine>/raw/codex/2026/06/24/rollout-*.jsonl
s3://bucket/.../<machine>/raw/cursor/<project>/<uuid>.jsonl
s3://bucket/.../<machine>/raw/icodemate/<project>/<session>.jsonl
```

The machine name is derived from the path segment immediately before `raw`. If
no such segment exists, sessions use the local AgentsView machine label. Codex
discovery only imports rollout files under the configured root plus a trailing
slash, so sibling prefixes such as `raw/codex-backup` are ignored.

S3 object `Size`, `LastModified`, and available object fingerprints (`ETag`,
version ID, and checksum headers) are stored in the session row and used for
unchanged-object skip checks. A later sync therefore lists object metadata first
and downloads only objects whose source metadata changed or whose stored parser
data is stale.

S3 roots are not watched with fsnotify. They are picked up by initial sync,
manual sync, and the periodic directory scan.

### Worktree Project Mappings

The parser infers a session's project from its `cwd`. It recognizes common
worktree manager layouts, including the generic
`worktrees/github.com/<owner>/<repository>/<worktree>` convention, where the
repository segment becomes the project. Layouts it does not recognize — such as
`~/code/{project}.worktrees/feat/<branch>/` — otherwise group sessions under
`<branch>` rather than `{project}`. For those, register manual **path-prefix →
project** rules from the **Rules** view on the [Data page](/docs/data/#rules),
or let the [mapping editor](/docs/data/#create-a-project-mapping) create one
from a project's observed session folders:

![Worktree mapping rules on the Data page](/docs/assets/generated/screenshots/worktree-mappings.png)

- Mappings are explicit; there is no auto-discovery.
- Each rule is scoped to one machine. The machine selector manages rules for the
    local machine and for any remotely synced machine. Rules live in the
    writable archive that ingests that machine's sessions, which may be the
    source machine's local SQLite archive or a separate collector archive.
- Each rule applies whenever a session's `cwd` falls under the configured
    prefix, on both new sessions as they sync and (via the **Apply** button)
    already-imported sessions. Prefixes match on directory boundaries, so
    `/worktrees/service` does not match `/worktrees/service-old`.
- Enabled mappings run after parser inference, so an explicit rule always wins
    when the two disagree.
- The default `explicit` layout maps every matching path to the project name
    stored on the rule. The `repo_dot_worktrees` layout derives the project from
    the first path segment under the prefix when it is named `<repo>.worktrees`,
    so a path like `/code/agentsview.worktrees/feature/frontend` resolves to
    project `agentsview`.
- Rules created from the Data mapping editor record the mislabeled project they
    corrected, shown as the rule's **original label**. The value is
    informational and set once; to manually revert a reclassification, edit the
    rule's target back to that original label and apply again.
- Disabling or deleting a rule does not rewrite sessions by itself. Sessions
    whose source files still exist revert to parser-derived names on a later
    reparse or full resync, while orphaned sessions keep their stored
    classification.
- Excluded, trashed, and skipped session files are left alone.

Mappings only mutate the session's `project` field; the rest of the session
record is preserved through the bulk-resync rebuild-and-copy path.

## Automated Session Detection

Roborev-tagged runs are automated code reviews, including runs with multiple
user messages. Other sessions are automated when they have one or fewer real
user messages and their first user message matches the automation classifier.
Automated sessions, such as roborev reviews, title generation, warmup pings, and
changelog generation, are filtered out of session lists, counts, and analytics
by default. The **Include automated** toggle in the session filter dropdown
opts them back in. See [Activity](https://agentsview.io/docs/activity/) for
headless worker classification.

A set of built-in patterns covers the roborev family and AgentsView's own
internal prompts. To teach AgentsView about first-message patterns unique to
your own automation, add them to `~/.agentsview/config.toml`:

```toml
[automated]
prefixes = [
  "You are summarizing a nightly batch run.",
  "INTERNAL-AUTOMATION:",
]
substrings = [
  "This is an automated repository maintenance run.",
]
exact_matches = [
  "Nightly automation completed.",
]
```

User-configured entries are case-sensitive and are matched against the session's
first user message:

| Key             | Match behavior                                               |
| --------------- | ------------------------------------------------------------ |
| `prefixes`      | `HasPrefix` against the first user message                   |
| `substrings`    | `Contains` anywhere in the first user message                |
| `exact_matches` | trims the first user message, then compares the whole string |

Entries are trimmed, deduplicated, and capped at 1024 characters. Entries that
duplicate a built-in pattern in the same category are silently dropped.

**Reclassification on config change.** AgentsView stores a hash of the active
classifier (built-in patterns + your configured patterns) with the database. On
startup, it rechecks stored `is_automated` values against the active classifier
and re-stamps the hash, so edits to `[automated]` patterns apply to history
immediately — no manual resync required. The same backfill also corrects rows
pulled in from PostgreSQL sync or copied from other archives.

## Database

The SQLite database uses WAL mode for concurrent reads and includes FTS5
full-text search indexes on message content.

### CJK full-text search

To add character and phrase matching for Chinese, Japanese, and Korean text,
including Chinese word segmentation, build and install the pinned
`simple`/cppjieba sidecar with `make install-cjk-fts`. Building it requires Git,
CMake 3.19 or newer, and a C++14 compiler. AgentsView discovers it next to the
binary or under the sibling `lib/agentsview/simple` directory. A custom path can
be selected with `AGENTSVIEW_SIMPLE_DIR`.

The sidecar indexes individual CJK characters and routes queries containing Han,
Hiragana, Katakana, or Hangul through that index. Query preparation depends on
the scripts in the query:

- Queries containing Japanese kana or Korean Hangul preserve character order and
    adjacency within each whitespace-separated search term. For example, `かな`
    does not match `なか`, and `검색` does not match separate occurrences of `검` and
    `색`. Separate terms can match anywhere in the same message.
- Queries containing Han without kana or Hangul use Chinese word segmentation
    through cppjieba. Japanese queries written entirely in kanji take this same
    path because the scripts alone do not distinguish the languages. Quote a
    kanji phrase, such as `"検索方法"`, to require its characters in order.
- A leading double quote opts into explicit FTS5 expressions, including phrases
    and operators, in any language. For example, `"검색 기능"` requires the two
    terms together, while `검색 기능` allows intervening text.

Japanese and Korean matching is character-based; it does not analyze grammatical
word forms or expand readings, romanizations, or spelling variants. Chinese word
segmentation also stays off when a query mixes Han with kana or Hangul.

ASCII-only searches continue to use the existing Porter index, so searches such
as `run` retain English stemming. The `messages_cjk_fts` index is derived data:
if the sidecar is removed, AgentsView drops that optional index and continues
with the standard FTS5 path; reinstalling the sidecar backfills it on the next
writable open. AgentsView fingerprints the native library and all cppjieba
dictionaries, atomically rebuilding the index when that fingerprint changes.
Writers running with another fingerprint leave a freshness marker instead of
mixing incompatible token streams. Pinyin expansion is disabled in the derived
index because ASCII-only queries continue to use the Porter index.

CJK full-text search is specific to SQLite message search, including the HTTP,
CLI, and MCP search paths. PostgreSQL/CockroachDB and DuckDB do not load this
SQLite extension and keep their existing search behavior. Substring and
regular-expression searches are unchanged. Session search result snippets
highlight the segmented matches; highlighting inside an opened transcript uses
the original query and may miss separated Chinese words.

The first backfill, a changed fingerprint, or any pending session requires a
full index rebuild before startup completes. AgentsView logs this wait. The
freshness ledger stores session IDs rather than old message IDs and token
content, so it cannot remove stale entries for individual replaced or deleted
messages. Removing the sidecar drops the CJK index but retains the
`messages_cjk_fts_pending_sessions` ledger and three persistent session
triggers. The ledger holds at most one row per touched session ID until the next
successful CJK index rebuild clears it.

Index maintenance uses TEMP triggers on the writer connection. Writes made
without these triggers or with another sidecar fingerprint leave the index
stale. CJK search then falls back to standard FTS5 and logs a warning once per
database handle. Reopening the archive with the sidecar restores the index and
its triggers.

### Schema tables

| Table                | Purpose                                                                      |
| -------------------- | ---------------------------------------------------------------------------- |
| `sessions`           | Session metadata (project, agent, timestamps, file info, user message count) |
| `messages`           | Message content with role, ordinal, timestamps                               |
| `tool_calls`         | Tool invocations with normalized category taxonomy                           |
| `tool_result_events` | Chronological status events for tool calls (e.g. Codex subagent updates)     |
| `insights`           | AI-generated session analysis and summaries                                  |
| `starred_sessions`   | Server-side star persistence (replaces localStorage)                         |
| `pinned_messages`    | Pinned message references with session linkage                               |
| `stats`              | Aggregate counts (session_count, message_count)                              |
| `skipped_files`      | Cache of non-interactive session files                                       |
| `source_failures`    | Cache of session files whose last parse failed                               |
| `messages_fts`       | FTS5 virtual table for full-text search                                      |
| `messages_cjk_fts`   | Optional CJK FTS5 index using the `simple` character tokenizer               |

The database is automatically migrated on startup when the schema changes.
When a parser upgrade affects only some agents and the archive syncs source
files, AgentsView reparses those agents' sessions in place; failed sources
retry on later syncs. Other stale data versions require a full resync into a
fresh temporary database. The resync then copies preserved/orphaned session data
from the previous database before swapping atomically. If the full resync
aborts, AgentsView falls back to an incremental sync and leaves the
data-version marker stale. The running daemon retries the rebuild after an hour,
backing off to daily; a restart also retries.

## History from a saved archive

This feature is unreleased. It lets an ordinary AgentsView installation browse
and search a retired machine's history alongside current local sessions. The
original Claude and Codex files stay in their Docbank storage; you can attach a
backup snapshot or an existing archive directory. Neither requires a permanent
extracted transcript directory.

Copy an AgentsView archive backup repository to a local disk or mounted share.
Add its path and the exact snapshot ID from the backup report to `config.toml`:

```toml
[[archive_sources]]
name = "retired-desktop"
repository = "/mnt/archives/desktop/backup-repo"
snapshot = "<snapshot ID>"
```

If you already have an archive directory, use `directory` instead of
`repository` and `snapshot`:

```toml
[[archive_sources]]
name = "saved-history"
directory = "/mnt/archives/restored"
machine_name = "Previous workstation" # Optional; defaults to the captured name.
```

The directory can be an original AgentsView archive, the output of
`agentsview archive restore`, or a Docbank-restored vault that retains its
`application/sessions.db`. A vault containing only blobs is insufficient: the
application database records accepted sources and their machine identities.
Stop the source archive's AgentsView or Docbank process before attaching it.
The directory must be writable for Docbank's catalog, locks, and maintenance;
it is held exclusively for each import pass. Opening it may apply Docbank
catalog upgrades. AgentsView opens the source `sessions.db` read-only and writes
parsed results to the receiving installation.

Restart a running daemon after editing the file, then run `agentsview sync`. The
first sync reads the supported sources and reports pending work, captured
deletion rules, and supplemental files. Sessions retain the original machine's
identity and label. `name` labels the configuration entry; changing it does not
change session ownership.

A daemon started with `--no-sync` still handles manual `agentsview sync` and
`agentsview sync --full`; automatic ingestion stays disabled.

Pushes to PostgreSQL, ClickHouse, and DuckDB also read pending attached history
after successful local sync, including on a `--no-sync` daemon. A push watch
that owns the local archive reads attachments at startup and on each periodic
sync. Its `--full` option reparses available attachments.

An unavailable attachment produces a warning during a push from a current SQLite
archive. Stored history and new local sessions still reach the destination;
unfinished attachment work is retried on a later sync. Changes committed before an
attachment error also queue a push in watch mode.

Use the optional `machine_name` to change the display name of a single-machine
archive. Restart the daemon and sync to apply a name change; no reparse is
needed. Omit it for an archive containing several machines, which retain their
individual captured names. Removing it restores the captured name. The
underlying installation IDs come from the archive automatically and stay
unchanged when you rename or move it.

Later syncs skip transcripts whose parser and content-policy versions are
current. The snapshot stays fixed until you change the configuration. A later
snapshot containing a different version of an already accepted source is a
conflict; this feature does not advance captures or follow the newest backup.

### Space, reconnecting, and reparsing

A backup source temporarily copies the snapshot's application database to build
a small source index on its first sync. A directory source reads acceptance
records directly from its application database on each pass, without copying
that database or its transcripts. Backup bootstrap reports the required bytes
before copying and removes the database afterward. Each parse also needs
temporary space for one source and its companion files, up to 16 GiB, plus parser
staging. Parsed sessions and retained images occupy the receiving installation's
normal database and assets directory.

Disconnecting the archive leaves already indexed history browsable. Sync reports
the unavailable source and retries on a later pass. When a source fails to copy
or parse, the next scheduled sync tries it again. If that retry also fails,
scheduled syncs and pushes wait an hour, doubling to a day, and report the
recorded error meanwhile. A manual `agentsview sync` retries immediately;
restarting the daemon clears the wait. To move the archive, change `repository`
and restart the daemon. A copied repository with the same repository ID and
pinned snapshot continues the same history. A different repository at that path
is refused. For a directory source, update `directory`; the archive identity and
accepted source records remain independent of its path.

Parser upgrades reparse available sources. Use `agentsview sync --full` to
request a full reparse; individual session refresh directs attached history to
that command. Local ingestion runs first and gets a turn between archive
sources. It can still wait for one large source's parse: 16 GiB is a size limit,
not a time limit.

During a full rebuild, the old database stays readable, but writes pause with a
retryable HTTP 503. Available archives extend that pause. Offline or removed
sources keep their indexed history and unfinished parser work. If a backup
starts between sources, the rebuild waits with progress and resumes afterward;
cancelling leaves the original database in place. Ordinary sync defers a busy
repository to a later pass. A backup blocked by an active reader can itself fail
after 60 seconds and need a retry.

### What is included

- Accepted Claude and Codex sources become browsable. Supplemental originals
  from other providers remain in the repository but are not parsed by this
  feature.
- The receiver's content and image policies apply. Its stars, names, pins, and
  deletions survive reparsing. Curation from the saved database is not merged.
- Capture-time deletion rules are honored. Sessions deleted in the saved archive
  *after* capture can reappear; those later deletions are not imported.
- History already received through artifacts or HTTP/SSH sync can appear again
  under a different identity. Matching native session IDs does not prove that
  two machines hold the same history. Exact-ID ownership conflicts are refused.
- Attachments belong in an ordinary installation, not an archive-only one. The
  receiving machine's own identity and aliases cannot be attached as foreign
  history. Credentials and telemetry identity are never adopted from the source.
- Both storage forms use the same source identities, content policies and
  parser. Directory sources can discover additional accepted sources on a later
  pass; changing an already accepted source remains a conflict. Concurrent
  access to a running vault, direct object-storage access, and archive
  configuration in the UI are outside this delivery.

### Which sync feature should I use?

| Feature                                   | Purpose                                                                                                                                                        |
| ----------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `archive_sources`                         | Read a backup snapshot or archive directory alongside local sessions, retaining its original machine attribution.                                                          |
| `session_sources`                         | Scan original session files already accessible in folders or mounted shares.                                                                                   |
| HTTP/SSH remote sync                      | Collect session files from another running machine.                                                                                                            |
| Artifact exchange                         | Exchange parsed session data and curation between installations. Attached originals are not republished as local artifacts.                                    |
| Hosted raw sync                           | Capture and publish provider files to a hosted service for server-side parsing. Attached archives do not become local capture roots.                           |
| Archive capture, backup, restore, extract | Preserve originals and recovery state, recover a standalone archive, or export captured files. These commands are not required to browse an attached snapshot. |

## Sync Behavior

AgentsView keeps the database in sync with session files through three
mechanisms:

1. **File watcher** — uses FSEvents on macOS and fsnotify elsewhere to detect
    file changes. An isolated edit is batched for 500ms; watcher-driven sync
    start times remain at least five seconds apart. Common dependency and build
    folders (`node_modules`, `__pycache__`, `.git`, `vendor`, `dist`, etc.) are
    automatically skipped to reduce noise and overhead. On macOS, FSEvents
    reports a file that its producer keeps open only when the file is created
    and closed, not as it grows.
1. **Scheduled reconciliation** — every 15 minutes, a scoped scan of the
    agents whose watch coverage is known to be partial, such as shallow
    directory watches or database change cursors. Other agents rely on the
    watcher, the fallback polling described under
    [Large Watch Trees](#large-watch-trees), and the daily archive audit.
1. **Codex live-activity polling** — every 30 seconds, the daemon checks file
    metadata for a bounded set of recently active rollouts and syncs the ones
    that changed. A rollout is recently active when `history.jsonl` names it or
    when its stored session ended within the last 24 hours. The second source
    covers producers such as Codex Desktop that write no `history.jsonl` and
    keep the active rollout open. The daemon checks at most 256 of these stored
    sessions per poll, newest first. They can come from any configured Codex
    root, including a `[[session_sources]]` root labeled with another
    machine's key, such as a shared mount. This is a freshness backstop for
    already indexed sessions, not a session source: normal discovery and sync
    still own ingestion, deletion, and canonical-path selection.

Recent-session polling has two known gaps on macOS. In both, Codex Desktop
writes no `history.jsonl` hint, macOS does not report appends to the open file,
and AgentsView picks up the new activity only when Codex closes the session
file:

- You resume a Codex Desktop session that has been idle for more than 24 hours.
  Its stored end time is outside the 24-hour window.
- The session is stored under a machine label that is no longer in your
  configuration. A session keeps the label it was first stored under, so this
  happens after you change a `[[session_sources]]` root's `machine` value, or
  for an old hostname label you have not adopted with
  [`db adopt-machine`](/docs/commands/#agentsview-db-adopt-machine).

Change detection uses file size, mtime, inode, and device tracking to validate
incremental parses more reliably. A pool of 8 workers processes files in
parallel during sync.

Codex history hints are available when the producing frontend writes
`history.jsonl`. In Codex configuration, `[history] persistence = "none"`
disables those writes; sessions whose stored activity is recent still get
live-activity polling. AgentsView reads only
session identity and timestamp metadata from accepted hint records and neither
stores nor logs submitted prompt text.

For each configured local Codex session root, AgentsView probes `history.jsonl`
in the cleaned root's parent. For example, a custom `/data/custom/sessions` root
probes `/data/custom/history.jsonl`. It does not search ancestors or the rollout
archive for another history file. Missing hint files remain cheap probes.

The initial daemon poll bootstraps at most the newest 4 MiB of each history file
and accepts records at most 24 hours old. If AgentsView restarts during a long
autonomous run whose last persisted prompt is outside either bound, that rollout
stays hot while its stored session ended within the last 24 hours.

For `s3://` Claude, Codex, and Cursor roots, change detection uses object size,
`LastModified`, and available object fingerprints such as ETag, version ID, and
checksums from listing or stat calls. Object content is downloaded only after
that metadata shows a parse may be needed.

Files that produce no session, such as those with no interactive content, are
cached in the `skipped_files` table. Source files that are missing or whose
content is malformed are cached in the `source_failures` table, even when the
sync pass that found them did not complete. An unchanged cached failure counts
as a skip, allowing watcher retries to finish. Failure identities include the
provider's pre-parse fingerprint, including content hashes and companion files
where available. SQLite schema failures also track database and WAL changes, so
repairing a schema retries the source even if its session timestamps stay the
same. Missing files are retried when they appear. A full resync retries cached
failures so a parser upgrade can reach a source that never produced a session.
If that rebuild is discarded, the incremental catch-up still retries them.
Failures that can clear up on their own, such as a timeout or a permission error,
are not cached and are retried on every pass.

Sync summaries include a `Parser anomalies (this run)` section whenever the
current run observes parser or sanitizer anomalies. The section can include
malformed-line counts, unrecognized Antigravity schema sessions, sanitized-field
counts, and Antigravity `gen_metadata without usage` counts. A
`gen_metadata without usage` entry means Antigravity supplied generation
metadata for one or more records, but AgentsView could not derive normalized
usage totals from those records during that sync.

Full parser rebuilds serve the retained archive while rebuilding; the dashboard
and CLI report stale results. Scoped upgrades and schema repairs finish before
HTTP readiness. Their startup output prints phase and completion lines, and
`agentsview daemon status` shows the starting PID, elapsed time, current phase,
progress detail, and log path while the daemon holds the start lock.

### Friction Log

The Friction Log records moments a session went wrong: corrections, failed
commands, workarounds, deferrals, repeated patterns, frustration, and
interruptions. Review is off by default because it reads every changed
transcript a second time.

```toml
[friction]
enabled = true
backfill_days = 7
timezone = "" # Local time on SQLite, UTC on PostgreSQL; or an IANA zone.
seat_patterns = [] # Optional path globs with one whole-segment {seat} capture.
```

Restart the daemon after changing this setting. When enabled:

- Sync stays as fast as with review off. It marks changed sessions as needing
  review, and the daemon reviews them in the background after each sync.
- Turning review on for an existing archive reviews every session in the
  background. Sync keeps running while it catches up.
- A PostgreSQL push first reviews every session still waiting, so the mirror
  receives current findings. It holds sync until that finishes.
- Sessions above 2,000 message, tool-call, and result-event rows or 4 MiB of
  stored text are marked skipped rather than reviewed.
- A daemon without automatic sync does not review in the background. That
  includes a daemon started with `--no-sync` and one serving a seeded or
  restored archive-only database. Its sessions are reviewed before a
  PostgreSQL push. A `--no-sync` daemon also catches up after it restarts with
  automatic sync.

An hourly job builds one stored digest per completed local day, catching up
within `backfill_days` (1–3650, default 7). It never builds today. Set
`AGENTSVIEW_FRICTION_TZ` to override the configured timezone. Seat patterns label
digest findings from session paths without changing the stored detections.

Digests keep Markdown, schema 1 summary JSON, frozen findings and usage,
session membership, and pattern recurrence. SQLite keeps this state through a
full resync when the archive retains transcript text. Usage-only SQLite archives
do not build or update digests. PostgreSQL builds its own digests from pushed
findings when its serving role can write the review tables.

Read digests, findings, and recurring patterns on the **Friction Log** page (`/friction`),
with [`agentsview friction`](/docs/commands/#agentsview-friction), the
`/api/v1/friction` routes, or the `get_friction_digest` and
`list_friction_patterns` [MCP tools](/docs/mcp/). `agentsview friction run`
builds missing days on demand. Findings are heuristics: check the session
before acting on one.

On the Friction Log page, **Generate summary** asks the configured `[insights]`
endpoint or agent CLI for an optional model-written overview of one digest.
Nothing generates summaries on a schedule. The model receives the digest counts,
P0 alerts, and up to 20 pattern titles ranked by occurrences. Pattern titles can
contain up to 80 characters of the text that triggered a correction, workaround,
or deferral; frustration titles are withheld because they quote the user. The
model does not receive finding text, evidence, or transcripts. The summary is
labeled as model-written and cannot change findings, digests, or recurrence.
Repeating a request returns the saved summary until the digest changes, and
saved summaries also appear under **Recall → Generated insights** as **Friction
Review**.

Local days wait until their sessions have current findings; the wait does not
expire. Other completed days can build while review catches up. PostgreSQL uses
the findings pushed by each source, including other rules versions, and skips
sessions whose source has not enabled review. Sessions over the review budget
are excluded immediately.

Catch-up fills missing dates throughout `backfill_days`, including empty days,
and revisits dates with unrecorded sessions inside that window, so late
syncs and pushes update an existing digest. A session belongs to only one digest
even if it resumes later, and rebuilding does not count its patterns twice.
Sessions that arrive or finish review outside the window require a larger
`backfill_days` setting. Each daily build holds the sync lock only for that day.
PostgreSQL defaults to UTC; configure the same timezone on every process serving
the same hub.

Hosted raw sync reviews sessions as it projects them, whatever this setting
says. See [Hosted Raw Sync](/docs/hosted-raw-sync/).

### Restricting Ingestion by Working Directory

By default every discovered session is ingested. To limit the archive to
sessions from specific workspaces — for example on a machine shared across
multiple clients where transcripts from one workspace should never appear
alongside another — set `sync_include_cwd_prefixes` in
`~/.agentsview/config.toml`:

```toml
sync_include_cwd_prefixes = [
  "/home/me/work/client-a",
  "/home/me/oss",
]
```

When the list is non-empty, a session is ingested only if its recorded working
directory equals one of the prefixes or lives underneath one. Prefixes and
session directories are lexically cleaned before matching: trailing separators
are ignored and `..` components are resolved, so `/home/me/oss/../other` does
not match a `/home/me/oss` prefix. Matching is path-boundary aware
(`/home/me/oss` matches `/home/me/oss/repo` but not `/home/me/oss-other`),
case-sensitive, and uses the local operating system's path separator — on Linux
and macOS a backslash is an ordinary filename character, not a directory
boundary. Use absolute paths; `~` is not expanded.

Notes:

- Sessions without a recorded working directory (a few agents do not store one)
    are skipped while the filter is set.
- The filter gates ingestion only. Sessions already in the archive are preserved
    (the SQLite database is a persistent archive); remove unwanted existing
    sessions explicitly with `agentsview prune`.
- Remote-host sync is unaffected: the prefixes describe local paths, so they are
    not applied to sessions pulled from `[[remote_hosts]]` entries.

### macOS Protected Folders

To label a session with its Git remote, worktree, and branch, AgentsView reads
Git metadata from the session's recorded working directory. It also reads
`SKILL.md` files referenced by recorded tool calls to infer skill names. On macOS,
some locations are guarded by a privacy consent prompt, and reading inside one makes
macOS attribute the request to the AgentsView app and ask the user for access.
Because the first sync walks every session in the archive at once, this produced
a burst of prompts at first launch for folders the user never pointed AgentsView
at.

AgentsView no longer touches these locations during discovery:

- `~/Desktop`, `~/Documents`, `~/Downloads`, `~/Movies`, `~/Music`, `~/Pictures`
- `~/Library/CloudStorage` (Dropbox, OneDrive, Google Drive, Box) and
    `~/Dropbox`
- `~/Library/Mobile Documents` (iCloud Drive)
- Removable and network volumes mounted under `/Volumes`, such as
    `/Volumes/External`

The `/Volumes` rule matters most when AgentsView runs as a background service.
macOS shows the consent prompt on the console, so a `launchd` agent on a Mac
with nobody logged in at the screen waits on it forever and startup sync never
finishes.

Sessions whose working directory lives in one of these folders — including
directories that only reach one through a symlink — keep **path-only project
identity**: they are still ingested, listed, searched, and grouped by path, but
they carry no Git remote, worktree relationship, or branch, and their project
name comes from the path rather than the enclosing Git repository. Sessions
anywhere else are unaffected.

Skill files in these locations also keep path-derived names; AgentsView skips
their frontmatter without reading the file, including through directory
symlinks.

If you keep code or skills in one of these folders and want their metadata, opt in:

```toml
scan_protected_paths = true
```

macOS then prompts once per folder or volume on the next sync; granting access
restores full identity for those sessions, and denying leaves them path-only.
The option has no effect on other platforms.

Enabling the option applies to sessions parsed after the change. Sessions
already in the archive keep their path-only identity until their session files
change; run `agentsview sync --full` to reparse the archive and pick up Git
detail for existing sessions.

### Large Watch Trees

The recursive watcher has a hard budget of 8192 directories per process. If a
session root is larger than the remaining budget, or if registering watches hits
the operating system's inotify or file-descriptor limit (`ENOSPC` / `EMFILE`),
as of 0.27.0 AgentsView **degrades** that root to polling instead of aborting
startup. The HTTP listener is now bound before any watches are registered, so
the server still comes up cleanly.

Providers can also cap how deep a recursive root is watched when they read only
a fixed depth. Antigravity and Antigravity CLI `brain` folders are watched one
level deep, so the generated trees inside each `brain/<id>` do not count
against the budget. The Antigravity IDE watcher also watches
`brain/<id>/.system_generated/logs`, which holds the plaintext transcript.
This cap applies to the Linux and Windows watcher. The macOS FSEvents watcher
has no per-directory budget.

Runtime folder watch failures keep healthy folders on native watches. Claude
polling covers the affected project folder. Other providers poll the root's
configured scopes so changes to companion files reach their owning sessions.
Retries start after 10 seconds and back off to two minutes. The next shared poll
pass batches recovered folders and reconciles their original provider root
scopes before releasing polling. A failed reconciliation keeps polling active
for the next retry. A deferred folder keeps polling until its whole subtree is
watched or the directory disappears.
New native events sync on the normal interval, apart from failed discovery,
which keeps backing off up to two minutes.
A regular file replacing the directory or an ancestor releases polling.
A directory symlink keeps polling. Removing a root from the watch plan releases
its polling obligations. Budget or native watch resource exhaustion keeps
root-level polling. Event-buffer overflow puts every recursive root on polling.

Roots that fall back to polling are picked up by:

- the existing 15-minute periodic full sync, plus
- a new 2-minute fallback sync loop that runs whenever any roots are unwatched
    (it reconciles the affected scopes)

The startup polling summary counts roots handed to fallback polling. It excludes
folders already covered by separate polling obligations. Runtime
`polling N unwatched root(s)` logs count distinct polling scopes, including
Claude project folders. Per-root and startup summary lines look like:

```
Couldn't watch 12500 directories under /home/me/.claude/projects, will poll every 2m0s
Polling 1 roots every 2m0s for changes
```

No configuration is required, but on Linux you can still raise the global cap to
keep more roots watched in real time:

```bash
sudo sysctl fs.inotify.max_user_watches=524288
```

## Memory Behavior

The serve daemon bounds its own memory, so no tuning is required. It installs a
256 MiB Go soft memory limit, and on Linux it also limits glibc to two malloc
arenas and pins the heap trim threshold at 1 MiB. Pinning the threshold turns
off glibc's dynamic adjustment, which otherwise lets a long-running process stop
trimming until as much as 64 MiB is free, so memory freed by the SQLite driver
goes back to the operating system instead of accumulating as fragmentation.
Setting `GOMEMLIMIT`, `MALLOC_ARENA_MAX`, or `MALLOC_TRIM_THRESHOLD_` in the
daemon's environment overrides the corresponding default.

## Manual Sync

Trigger a sync from the API:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/sync
```

Trigger a full resync (re-parses all session files from scratch):

```bash
curl -X POST http://127.0.0.1:8080/api/v1/resync
```

Both endpoints stream progress via Server-Sent Events when accessed from a
browser or SSE-capable client.

Check sync status:

```bash
curl http://127.0.0.1:8080/api/v1/sync/status
```

The JSON response includes `startup_pending` and `archive_stale` alongside
sync progress.

## Privacy and Telemetry

By default, all session data stays on your local machine in SQLite. Normal
browsing, search, and reports do not send session content, project names,
prompts, file paths, or hostnames anywhere.

Optional features that send data externally when you enable them:

- [Hosted Raw Sync](/docs/hosted-raw-sync/) sends original provider files to a
    hosted custody service configured by your deployment operator.
- [PostgreSQL sync](/docs/pg-sync/) (`pg push`) sends session data to a
  PostgreSQL database you configure.
- [ClickHouse sync](/docs/clickhouse-sync/) (`clickhouse push`) sends session
  data to a ClickHouse database you configure.
- The [DuckDB mirror](/docs/duckdb/) writes a local DuckDB file by default; data
    only leaves the machine if you expose the mirror over a remote Quack
    endpoint.
- [Generated insights](/docs/recall/#current-surface) sends scoped session
    content to the configured endpoint when `[insights]` sets `endpoint` and
    `model`, or to the selected agent CLI when neither is set.
- [Publish to Gist](/docs/usage/#publish-to-gist) uploads a session to GitHub.

The automatic outbound requests are update checks and anonymous telemetry:

- **CLI and web UI** — on startup, the server contacts the GitHub API to check
    for new releases. No identifying information is sent beyond what a standard
    GitHub API request includes (IP address, user-agent).
- **Desktop app** — uses Tauri's native updater, which checks the GitHub release
    feed independently.
- **Anonymous daemon telemetry** — see below.

### Anonymous Daemon Telemetry

As of 0.33.0, the server sends an anonymous `daemon_active` liveness ping on
each UTC day it runs. Restarts and other servers sharing the data directory do
not send another ping that day. When telemetry is disabled, the server skips
hourly telemetry wake-ups. The web UI also reports an anonymous
`app_opened` event on load or focus for each selected server and UTC day;
reloads and focus don't repeat it that day. These claims live in browser local
storage. The server sends it to PostHog with the same fields and opt-out as the
ping. The browser never contacts PostHog.
Two tabs whose initial checks overlap can each send `app_opened`, such as when
a browser restores several tabs together.
After a disabled receipt, the browser stops sending reports and tracking visits.
It checks again on its first load or focus on a later UTC day. Re-enabling
telemetry takes effect after that check.
An unauthorized check (HTTP 401) or exhausted retries also stop visit tracking
and discard its accumulated duration. Tracking starts fresh when a later check
enables reporting. Outcomes that occur while reporting is unavailable are
dropped. A fresh page retries after authentication; otherwise, the next UTC
day's load or focus starts a new check.
HTTP 401 makes reporting unavailable immediately. Server errors (HTTP 5xx) and
network failures retry up to five times with increasing delays before reporting
becomes unavailable. Other HTTP errors settle without retries and don't grant
permission to report.
Each browser telemetry request has a 10-second deadline, including reading its
receipt.
The browser retains each daily claim's 35 most recently accepted UTC dates.
Rolling the clock back to an evicted date can count it again.

The server sends `setup_finished` once per installation when the archive first
has a session in the default session list. Sessions need messages to count;
automated sessions and trash are excluded. Child sessions count when their
parent is missing and the list shows them as roots. It checks at startup, after
session changes, and every hour. After a failed attempt it waits for the hourly
check before trying again. The `telemetry-setup-finished` file beside the
installation identity records the claim across restarts and servers sharing the
data directory. Captures, seeds, backups, and restores preserve the claim with
the installation identity when the snapshot contains it. Restoring a snapshot
from before the claim can queue `setup_finished` again for the same installation
ID. Ordinary imports keep the receiving installation's claim. The event carries
no session details and follows the same telemetry opt-out. Existing installs
with such a session send it on their first start after upgrading.

The claim records that the event entered the telemetry queue, not that PostHog
received it. Delivery is best effort: if the server stops or the network fails
before the queue flushes, the event is lost and is not sent again. Only the
server sends `setup_finished`; the web UI's telemetry route rejects that event.

The web UI reports `visit_ended` with `surface: web` and a `duration_bucket`
of `under_1m`, `1_to_5m`, `5_to_30m`, or `over_30m`. These mean below 1 minute,
1 to below 5 minutes, 5 to below 30 minutes, and 30 minutes or more. Visible
time adds up across tab switches and includes idle visible time; hidden time
adds nothing. Closing or reloading the page, or leaving it hidden for 30
minutes, ends a visit. Switching servers discards the old visit and starts a
new tracker immediately for the selected server in the same tab. Each visit
can send one event, with no daily limit.
While permission is pending, the browser holds visit events and other outcomes
in page memory for the selected server, up to 64 events total. Additional
events are dropped. An enabled receipt releases the held events; a disabled
receipt discards them. A visit ending after UTC midnight also waits in page
memory while that day's check is pending. Closing the page before an enabled
receipt can lose those events.
Delivery is best effort: a visit can be lost if the browser crashes, quits, or
discards the tab before the event is sent. Long-lived tabs are the most exposed
to this, so `over_30m` can undercount.

The web UI also reports `screen_viewed` with `surface: web`. The server counts
each screen once per installation per UTC day, across tabs and server restarts.
The browser also deduplicates screen reports per selected server and local
storage, so separate browsers can check the same installation independently.
The server stores versioned JSON claims in `telemetry-screen-views` beside the
installation identity. For each installation and screen, it retains the 35 most
recently accepted UTC dates. A clock rollback to an evicted date can count that
screen again. When telemetry is enabled, the server checks the file
once in the background after it starts serving; a web UI report that arrives
first waits for that check. The check moves older plain-text claims, malformed
claims, or claims from a newer format to a unique
`telemetry-screen-views.unreadable-*` file and logs the move. Earlier copies
stay intact. Valid plain-text counts carry over for the current installation. If
the check fails, the server logs the error and continues forwarding reports. It
tries the check only once per process; restarting the server starts a fresh
attempt. Forwarded reports can replace malformed or plain-text claims with a
fresh file, losing their counts. Reports return 500 while the file cannot be
read or written, or uses a newer format. When counts cannot carry over, kit
starts a fresh file, which can count each screen one more time that day.
Requests must use `application/json` with exactly one JSON value; a missing or
unknown `screen` returns 400. The server reserves each claim before enqueueing
its event. If reservation storage fails, the request returns 500 without
enqueueing; a later visit retries.

The web UI reports five core actions the same way. Each carries one property,
and the server drops any value outside its fixed list. A search counts once per
mode used during each command palette open. Typing pauses, sorting, and retries
in the same mode do not add events. An insight counts once it finishes
generating; opening a cached insight does not count.

Downloads and Gist publishes count export attempts, including attempts that
fail. Markdown links count after the link is copied successfully.

| Event               | Property     | Allowed values                                                                                                                    |
| ------------------- | ------------ | --------------------------------------------------------------------------------------------------------------------------------- |
| `search_run`        | `query_type` | `text`, `semantic`, `hybrid`                                                                                                      |
| `session_viewed`    | `agent`      | the session's agent type                                                                                                          |
| `export_run`        | `format`     | `html`, `insight_html`, `friction_markdown`, `csv`, `markdown_link`, `gist`, `insight_gist`                                       |
| `insight_generated` | `kind`       | `daily_activity`, `agent_analysis`, or a generated-insight template name                                                          |
| `analytics_viewed`  | `page`       | `usage`, `activity`, `trends`, `quality`                                                                                          |
| `screen_viewed`     | `screen`     | `sessions`, `usage`, `activity`, `trends`, `recall`, `quality`, `pinned`, `trash`, `recent-edits`, `friction`, `data`, `settings` |

Feature outcomes use fixed categories; unknown strings become `other`.

| Event          | Property | Allowed values                                                                            | Count unit                                                                                                                                                                                                                                                                                                                                              |
| -------------- | -------- | ----------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `search_empty` | none     | none                                                                                      | Each command palette open that closes on an accepted empty result, so typing through empty prefixes counts once at most. Compare with `search_run`, which also counts per open. Opens that close on results, a blank query, a pending search, or a failure send nothing.                                                                                                                                                                                                            |
| `error_shown`  | `kind`   | `search`, `session`, `insight`, `analytics`, `export`, `other`                            | Each appearance of a core-action error in search, session views, generated insights including their supporting tool sequences, or exports. Analytics counts each independently requested panel on Usage, Activity, Trends, and Quality. Continuous display counts once; hiding and showing the error counts again.                                                                                |
| `sync_failed`  | `reason` | `request`, `sign_in_required`, `browser_extension`, `already_running`, `other` | Each failed foreground attempt after its terminal result, without `error_shown`. `request` covers uncoded request or processing failures; unsupported explicit codes use `other`. Completed Claude.ai imports with item errors send nothing. Cancellation, polling, and read-only refreshes send nothing. |

Each accepted failed search counts once, including a semantic search that
shows setup guidance. Internal setup probes and build retries add no events.

Hidden failures, cached warnings, the Sessions dashboard, settings, data
management, pins, stars, and in-session find send no outcomes. Query and error
text, paths, and identifiers stay out of outcome properties.

Every event contains only:

- app version and git commit
- the distribution that built the binary, such as `homebrew` for Homebrew or
  `kenn` for official downloads; empty for source builds
- operating system and CPU architecture
- the application-owned installation ID stored in
  `~/.agentsview/telemetry-install-id`
- for the five UI actions, the one listed value above
- for feature outcomes, the listed kind or reason
- for screen views, the listed screen name and `surface: web`
- for visits, the duration bucket and `surface: web`, without exact times

It contains no session data, prompts, project names, file paths, account
information, or hostname, and the events are sent with person-profile processing
and GeoIP lookup disabled. The ping runs in the background and never blocks
startup or operation.

A build sends telemetry only when it has the PostHog project key and a release
version such as `0.33.0`. Release builds and the `make` build targets set the key
at link time. Plain `go build` and `go test` binaries have no key and send
nothing. Telemetry also stays off in CI. Disable it with an environment
variable, then restart the server:

```bash
export AGENTSVIEW_TELEMETRY_ENABLED=0  # or DO_NOT_TRACK=1
```

This disables outbound telemetry, not the installation identity used for local
session attribution.

### Disabling Update Checks

Disable the CLI/web UI update check with any of:

| Method               | Value                                                        |
| -------------------- | ------------------------------------------------------------ |
| Config file          | `disable_update_check = true` in `~/.agentsview/config.toml` |
| Environment variable | `AGENTSVIEW_DISABLE_UPDATE_CHECK=1`                          |
| CLI flag             | `--no-update-check`                                          |

The desktop app's auto-updater is controlled separately via
`AGENTSVIEW_DESKTOP_AUTOUPDATE=0`.
