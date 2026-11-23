---
title: Configuration
description: Config file, default paths, and runtime settings
---

## Data Directory

AgentsView keeps all of its persistent data in one directory, which defaults to
`~/.agentsview/`. You can move it by setting the `AGENTSVIEW_DATA_DIR`
environment variable.

!!! note

    AgentsView still accepts `AGENT_VIEWER_DATA_DIR` as a legacy fallback when
    `AGENTSVIEW_DATA_DIR` is unset, though new setups should use
    `AGENTSVIEW_DATA_DIR`.

```
~/.agentsview/
├── sessions.db      # SQLite database (WAL mode)
├── assets/          # Images stored outside SQLite
├── vectors.db       # Semantic-search vector index (when [vector] is enabled)
├── usage-cache-v24-<id>.db # Disposable usage-aggregate cache
├── usage-postgres/  # Disposable PostgreSQL usage caches
├── telemetry-install-id # Application installation ID
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

When a command opens a `debug.log` larger than 10 MB, it copies the last 10 MB
into `debug.log.1` (replacing the older backup) and clears the active log for
new messages. That backup can start partway through a line. The `serve` command
captures startup messages right away and waits until any running daemon stops
before it rotates the log. Log writers share `debug.log.rotate.lock` and wait
while rotation holds it exclusively. If the lock is busy or the backup fails,
AgentsView leaves the active log as it is.

`usage-cache-v24-<id>.db` and `usage-postgres/` hold derived usage aggregates.
You can safely delete them when no AgentsView process is running, and the next
usage query rebuilds them automatically. When you back up your data, include
`sessions.db` for session history, `assets/` for images stored outside SQLite,
`config.toml` for settings, and `telemetry-install-id` for installation
identity. Keep the database and `assets/` together when you restore or move an
archive.

Term trends store per-session counts under `$CACHE_DIRECTORY/sqlite-trends`, or
`agentsview/sqlite-trends` in your user cache directory, up to 256 MiB per
archive. You can safely delete them, and the next term-trends request
counts messages again. Search pages can also hold session snippets in
`sqlite-trends`, `postgres-trends`, or `clickhouse-usage` under
`$CACHE_DIRECTORY` (or under `agentsview/` in your user cache directory). After
you delete a session, its snippets can remain there until the same search
overwrites the page or the cache is trimmed.

SQLite needs a writable temp directory for session writes and large sorts.
AgentsView uses the system temp directory: `TMP` or `TEMP` on Windows, and
`TMPDIR` elsewhere. If that directory is not writable, for example inside a
sandbox that confines child processes, AgentsView prints a warning and falls
back to `tmp/` inside the data directory, and its child processes inherit that
choice. To use a different location, point those variables at a writable
directory before you start AgentsView.

The desktop app and CLI share a detached local daemon for fresh reads and
writes, and a running daemon owns local SQLite writes for this data directory.
Daemons that start automatically exit after an idle period, while `daemon start`
and `daemon restart` stay running until you stop them. Ordinary session commands
need the daemon, though dedicated diagnostics such as `db adopt-machine --list`
and `doctor sync` can inspect the archive without starting it. If a script must
never auto-start a daemon, set `AGENTSVIEW_NO_DAEMON=1`; commands that require
one then refuse to run.

For code attribution stats, the Cursor source performs a live, machine-local
read from `~/.cursor/ai-tracking/ai-code-tracking.db` by default. If Cursor
keeps that database somewhere else on the host answering the stats request, set
`AGENTSVIEW_CURSOR_ATTRIBUTION_DB`. AgentsView does not sync the attribution
database into its archive or push it to PostgreSQL.

### Archive content

Set `archive_content` in `config.toml` to control how much of each session the
archive keeps:

```toml
# "full" (default), "transcripts", or "usage"
archive_content = "transcripts"
```

| Value           | What is stored                                                                                                                                                                                                                                                                                                                                     |
| --------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `"full"`        | Parsed messages, reasoning, titles, tools, and results. Tool-result category and image settings still apply.                                                                                                                                                                                                                                       |
| `"transcripts"` | Message text, thinking text, titles, and tool call metadata: names, categories, skill names, the file path a call targeted, result lengths, statuses, and subagent links. Tool inputs and tool results, usually most of a large archive, are dropped, and the one-line tool summaries inside message text keep only the tool name and target path. |
| `"usage"`       | Session metadata, usage values, activity timestamps, and the identifiers needed to connect subagents. Transcript text, thinking text, original tool inputs and results, and session titles are dropped.                                                                                                                                            |

The daemon reads this policy at startup, so changes take effect after you run
`agentsview daemon restart`. Because the policy applies when rows are written,
changing it does not rewrite rows already in the archive. To apply a narrower
policy to an existing archive:

1. Back up the archive first. You cannot restore dropped content without the
   original source files.
1. Set `archive_content` in `config.toml`.
1. Restart the daemon with `agentsview daemon restart`.
1. Run `agentsview source sync --full`.

That rebuild re-parses sessions whose source files still exist and copies
archived sessions whose sources are gone, projecting both onto the new policy.

If `config.toml` does not set a policy, `AGENTSVIEW_ARCHIVE_CONTENT` supplies
it. That works well for a dedicated reporting archive in its own data directory:

```bash
AGENTSVIEW_DATA_DIR=~/.agentsview-usage \
AGENTSVIEW_ARCHIVE_CONTENT=usage \
agentsview source sync
```

Keep these limits in mind when you use the narrower policies:

- **`"transcripts"` is a storage policy, not a redaction guarantee.** It removes
    the tool payload tables, the structured tool inputs, and the rows and
    summaries that parsers mark as tool output, and it rewrites the tool
    summaries parsers inline into message text. Text that a provider wrote into
    a message body in its own format stays as the provider wrote it. When tool
    output must never appear in the archive, use `"usage"`, which stores no
    transcript text.
- **Signals and older sessions under `"transcripts"`.** Session search,
    transcript viewing, and tool analytics still work, but signals and secret
    findings that inspect tool inputs or results see empty payloads.
    Content-based tool failure detection and secrets inside tool output find
    nothing, and no calls count as repeated identical calls.
    `result_content_blocked_categories` has no extra effect under this policy.
    Archived RooCode, Kilo Legacy, gptme, OpenHands, Aider, Codex, TraeX, and
    Zencoder sessions parsed before data version 105 whose source files are gone
    cannot be re-parsed, so a rebuild drops every row shape those parsers once
    used for tool output. This includes user turns for OpenHands, Codex, and
    TraeX, assistant replies for Aider, and system-flagged notices for Zencoder.
    Copied OpenHands actions with event summaries keep the prose before the tool
    header and the tool label, but lose the summary and all following text,
    including appended thinking, because the archive does not retain the summary
    boundaries. The semantic search mirror keeps previously embedded text until
    the next embedding pass refreshes it. With `[vector.embed] run_after_sync`
    enabled (the default), that pass starts right after the rebuild; otherwise
    let the scheduled pass run before you push vectors to PostgreSQL.
- **What `"usage"` disables.** `"usage"` supports usage reports such as
    `agentsview report usage daily`, while session search, transcript viewing,
    tool analytics, and content-derived quality metrics require a fuller
    archive. Insights and recall entries are refused because both hold
    transcript-derived text. Recall extraction stops before calling a model,
    including for old rows waiting for a rebuild. Raw `session export`, vector
    building, vector search, and vector export are also disabled. Opening the
    writable archive clears existing local message and recall indexes, even when
    `[vector]` is disabled. After the archive rebuild, run `pg push --full` to
    remove indexed content for those sessions from PostgreSQL too. Usage-only
    pushes also clear session titles, including names changed in PostgreSQL.
    Other sessions in a shared PostgreSQL store stay unaffected.

### Ingest-time image offload

Set `tool_result_images = "offload"` to store supported PNG, JPEG, WebP, and GIF
tool-result images in `{dataDir}/assets/<sha256hex><ext>` before SQLite
publishes their references, then restart the daemon to apply the change. Failed
writes keep inline content, and archives that omit tool content write no assets.
See [image storage](/docs/data/#ingest-time-image-offload) for retries, backups,
and remote-backend limits.

You can also pick **Offload** under **Settings > Tool-result images**. After you
save a changed policy, click **Restart now** in the notice to apply it.

![Tool-result image policy in Settings](/docs/assets/generated/screenshots/settings-archive-content.png)

## Installation Identity

Local sessions use a random installation ID saved in `telemetry-install-id`
inside the data directory. AgentsView reuses the existing ID from that file or
creates one if it is missing. Read-only commands leave the file untouched.

This ID identifies an AgentsView installation rather than physical hardware. It
survives updates, binary replacements, restarts, and network or hostname
changes. A fresh data directory gets a new ID, and copying a data directory
copies its identity. To keep an installation's identity across a restore,
restore the original `telemetry-install-id` from your backup. Deleting the file
deliberately creates a distinct installation, and that new installation does not
automatically take ownership of the old one's sessions.

By default, the display label is your current hostname. To set a fixed label,
add `local_machine_name` to `config.toml` and run `agentsview daemon restart`.
Any non-empty label works, including `local`. Changing the label leaves session
keys alone, and PostgreSQL receives the new label on the next push. Creating the
identity does not rewrite `config.toml` or its comments, though AgentsView still
generates and saves a missing cursor secret.

Session filters accept an exact ID or alias, a comma-separated list of IDs and
aliases, or one display label matched as the whole value, case-sensitively. If
you pass an unknown name or a label shared by several machines, AgentsView
returns an error that lists the known machines. You can query
`/api/v1/machines` to look up IDs, display labels, and aliases.

Project rules and worktree mappings accept only IDs and aliases.

### Upgrading Historical Machine Keys

At writable startup, AgentsView moves sessions from the archive's recorded local
machine to the installation ID. The saved ownership record arrived in v0.40.0,
and v0.39.0 archives do not have it. Older `local` rows move too. Session IDs,
messages, stars, pins, and other curation stay intact, and worktree rules along
with project and source metadata move with the sessions. If worktree rules
conflict, the migration stops so you can reconcile them without losing edits.

Adopted hostnames remain aliases for old filters and URLs. Display labels do not
establish ownership, and other historical hostnames stay separate until you
explicitly select them. Alias keys are reserved redirects: use a peer's
installation ID for a remote source and omit `machine` for a local root.

If an archive has named machines without saved local ownership, startup leaves
them under their existing keys and logs those keys once. You can inspect them
with `agentsview db adopt-machine --list`, which prints machine keys and session
counts without starting a daemon or changing configuration. To claim keys you
own, stop the daemon and run
[`agentsview db adopt-machine`](/docs/commands/#agentsview-db-adopt-machine)
with those old machine keys. Keys belonging to other installations need no
action.

PostgreSQL publishes the migrated sessions and metadata on the next incremental
push, while the SQLite archive stays intact. If two installations publish the
same old hostname alias to PostgreSQL, the latest push sets its filter target.
Use installation IDs when you need to select machines unambiguously in a shared
database.

## Config File

AgentsView creates `~/.agentsview/config.toml` automatically on first run to
store persistent settings across restarts.

Each command validates the settings it uses. For example, `export sessions`
warns on stderr about invalid insights, vector, recall-extraction, or
public/proxy settings while still exporting the archive and preserving those
settings in `config.toml`. TOML syntax and type errors still fail loading;
unknown-key handling stays unchanged. Server startup and reloads validate the
entire configuration, so starting `serve` also catches settings that exports
ignore. Automatic daemon startup and replacement validate the effective server
configuration, including deployment overrides, before launching or stopping a
daemon.

The full container deployment environment applies to `serve`, remote `serve`
commands, and `pg push` (including its watch mode). Other PostgreSQL clients
honor `AGENTSVIEW_PG_ALLOW_INSECURE` for the default target. Hosted raw reads
and reparse commands honor `AGENTSVIEW_REQUIRE_AUTH` without reading the server
token file, while MCP HTTP listeners and daemon clients load the deployment
authentication they use. Semantic and hybrid PostgreSQL reads, including
`mcp --pg`, load deployment embeddings for published recipe adoption without
reading listener token files. Direct archive exports skip deployment token files
and `AGENTSVIEW_EMBEDDINGS_*` settings.

!!! note

    The config format changed from JSON to TOML. AgentsView migrates existing
    `config.json` files to `config.toml` automatically on first run and renames
    the old file to `config.json.bak`.

```toml
cursor_secret = "base64-encoded-secret"
require_auth = true
cursor_admin_api_key = "key_xxxxx"
daemon_idle_timeout = "20m"
sync_idle_after = "10m"
sync_interval = "5s"
background_sync_interval = "10m"
chart_palette = "agentsview"
zoom_level = 120
```

`background_sync_interval` defaults to `"10m"`. When nobody is watching the
archive, local `serve` syncs changed session files at most that often. The first
change after a quiet period syncs right away, and later changes wait for the
interval. Three things pull changes in sooner:

- An open web UI syncs at `sync_interval` (default `"5s"`) while its tab is
  visible.
- An open [change stream](/docs/conversation-export/#follow-changes-live) syncs
  up to once a second.
- Any other archive request, such as a CLI or MCP read, keeps syncing at
  `sync_interval` for two minutes afterward, the same as an open web UI. The
  first request after a quiet stretch also syncs files that changed since the
  last sync and checks sessions still being written before it answers. It waits
  up to two seconds for that, sharing the wait budget described below. Later
  requests within those two minutes start no sync of their own and can miss up
  to `sync_interval` of changes. They answer right away unless the first
  request's sync is still running, in which case they share its remaining wait.

A session watch stream keeps its own session current without changing the
interval for anything else. Polling of live agent activity and of roots that
cannot be watched follows the same rule: its usual pace while someone is
watching, and the background interval otherwise. Because a read does not poll
roots that cannot be watched, their sessions can lag by up to one background
interval. To sync at `sync_interval` all the time, set
`background_sync_interval` to `"0s"` and restart the server.

`sync_idle_after` defaults to `"10m"`. After that much time without a client
request or an open event stream, local `serve` pauses local sync. It keeps
watching session files and remembers which ones change, but syncs nothing and
pauses its polling timers, remote-source sync, S3 imports, and attachment
imports. Remote-host polling, Codex Cloud capture, replica push, and raw uploads
stay on their own schedules. The next archive request kicks off catch-up: the
daemon syncs the files that changed during the pause without scanning every
session directory, and runs the periodic sync if the pause skipped one. Requests
share a two-second wait budget from the start of catch-up, and after that reads
return right away with a stale-results warning while catch-up finishes in the
background. The warning clears once catch-up finishes or fails, and failures are
logged. Every client request except `/api/ping` counts as activity, while
version and sync-status probes report readiness without starting catch-up, and
desktop health probes keep the daemon alive without restarting sync. To keep
syncing while idle, set `sync_idle_after` to `"0"` and restart the server.

| Field                               | Description                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| ----------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `local_machine_name`                | Optional display-name override; takes effect after a daemon restart                                                                                                                                                                                                                                                                                                                                                                                                              |
| `cursor_secret`                     | Auto-generated HMAC key for pagination cursor signing                                                                                                                                                                                                                                                                                                                                                                                                                            |
| `cursor_admin_api_key`              | Cursor Admin API key used by `agentsview source cursor sync-usage`                                                                                                                                                                                                                                                                                                                                                                                                               |
| `cursor_admin_api_key_file`         | File holding the Cursor Admin API key, as an alternative to `cursor_admin_api_key`; see [Read secrets from files](#read-secrets-from-files) |
| `cursor_admin_email`                | Optional default Cursor Admin usage filter by member email                                                                                                                                                                                                                                                                                                                                                                                                                       |
| `cursor_admin_user_id`              | Optional default Cursor Admin usage filter by member user ID                                                                                                                                                                                                                                                                                                                                                                                                                     |
| `github_token`                      | Optional saved GitHub token for Gist publishing                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `result_content_blocked_categories` | Tool categories whose result content is not stored (default: `["Read", "Glob"]`). Changes apply to new ingestion and full rebuilds; see [storage maintenance](/docs/data/#storage-maintenance) for existing source-backed sessions.                                                                                                                                                                                                                                              |
| `tool_result_images`                | Retain supported inline tool-result image blocks with `"keep"` (default), or store readable `agentsview_image` placeholders with `"drop"`, or move supported images to the local asset store with `"offload"`. The setting affects future ingestion and full resyncs; use `db migrate --images` to move existing images or `db strip --images` to remove them from stored results; also configurable under **Settings > Tool-result images**, and changes require a daemon restart. |
| `archive_content`                   | How much of each session the archive stores: `"full"` (default), `"transcripts"`, or `"usage"`; changes require a daemon restart — see [Archive content](#archive-content)                                                                                                                                                                                                                                                                                                       |
| `host`                              | Interface the server binds to (default `127.0.0.1`); non-loopback values require `require_auth = true`                                                                                                                                                                                                                                                                                                                                                                           |
| `require_auth`                      | Require bearer-token authentication for API access                                                                                                                                                                                                                                                                                                                                                                                                                               |
| `auth_token`                        | Auto-generated 256-bit bearer token for remote access; overridden by `AGENTSVIEW_AUTH_TOKEN`, or by `AGENTSVIEW_AUTH_TOKEN_FILE` under a [container deployment](#container-deployment)                                                                                                                                                                                                                                                                                           |
| `public_url`                        | Browser URL, trusted origin, and managed Caddy site address                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `public_origins`                    | Additional trusted origins for request Host/Origin checks                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `daemon_idle_timeout`               | Idle timeout for automatically started and `serve --background` writable daemons; set to `"0s"` to keep them alive                                                                                                                                                                                                                                                                                                                                                               |
| `sync_idle_after`                   | Pause local collection after this long without client activity; defaults to `"10m"`, with `"0"` disabling the pause                                                                                                                                                                                                                                                                                                                                                              |
| `sync_interval`                     | Minimum time between watcher-triggered session sync starts while the web UI is open, and in archive push commands. Default `"5s"`; use `"60s"` for less frequent syncs. Zero or negative durations disable the minimum interval. While a [change stream](/docs/conversation-export/#follow-changes-live) is open, `serve` uses at most `"1s"`. Applies on restart or when provider changes rebuild watchers. Raw sync and periodic catch-up keep their own timers.               |
| `background_sync_interval`          | Minimum time between watcher-triggered session sync starts in `serve` while nobody is watching the archive. Default `"10m"`. Archive requests sync pending changes first. `"0s"` uses `sync_interval` all the time, and a longer `sync_interval` takes precedence. Applies on restart.                                                                                                                                                                                           |
| `chart_palette`                     | Server-wide categorical chart colors: `"agentsview"` (default), `"matplotlib"`, or `"colorblind"`; also configurable under **Settings > Appearance**                                                                                                                                                                                                                                                                                                                             |
| `zoom_level`                        | Default interface zoom for clients without a local preference: `67`, `75`, `80`, `90`, `100`, `110`, `120`, `125`, `130`, `150`, `175`, or `200`. Defaults to `100` when omitted. Restart the daemon after manual edits                                                                                                                                                                                                                                                          |
| `disabled_agents`                   | Session providers to exclude from local filesystem scanning — see [Disabling Session Providers](#disabling-session-providers)                                                                                                                                                                                                                                                                                                                                                    |
| `[proxy]`                           | Managed proxy configuration table — see [Remote Access](/docs/remote-access/)                                                                                                                                                                                                                                                                                                                                                                                                    |
| `disable_update_check`              | Disable the automatic update check (see [Privacy](#privacy-and-telemetry))                                                                                                                                                                                                                                                                                                                                                                                         |
| `scan_protected_paths`              | Allow Git discovery inside macOS privacy-protected folders and volumes under `/Volumes`, accepting one consent prompt per location — see [macOS Protected Folders](#macos-protected-folders)                                                                                                                                                                                                                                                                                     |
| `[pg]`                              | PostgreSQL sync configuration — see [PostgreSQL Sync](/docs/pg-sync/)                                                                                                                                                                                                                                                                                                                                                                                                            |
| `[clickhouse]`                      | ClickHouse sync configuration — see [ClickHouse Sync](/docs/clickhouse-sync/)                                                                                                                                                                                                                                                                                                                                                                                                    |
| `[vector]`                          | Opt-in semantic-search index; model settings live in `[vector.embeddings]`, named endpoints in `[vector.embeddings.servers.<name>]`, embedding schedule in `[vector.embed]` — see [Semantic Search](/docs/semantic-search/#enabling-vector) for every key                                                                                                                                                                                                                        |
| `[recall.extract]`                  | Opt-in model-backed recall extraction; named endpoints in `[recall.extract.servers.<name>]`, prompt selection in `[recall.extract.prompts]`, request overrides in `[recall.extract.request]` — see [Recall](/docs/recall/#automatic-extraction)                                                                                                                                                                                                                                  |
| `[integrations.docbank]` | Optional lookup of transcript image digests in Docbank; see [Find transcript images in Docbank](/docs/configuration/#find-transcript-images-in-docbank) |
| `[image_text]` | Opt-in search of text inside tool-result images; see [Image text](/docs/configuration/#image-text) |
| `[insights]`                        | Optional generated-insights endpoint and model, plus `default_agent`, the agent CLI new reports start with; local loopback HTTP is allowed, remote plaintext requires `allow_http = true`, and endpoint failures do not retry through a CLI — see [Recall](/docs/recall/#current-surface)                                                                                                                                                                                        |
| `[friction]`                        | Opt-in Friction Log review; set `enabled = true` to review changed sessions in the background after each sync — see [Friction Log](#friction-log)                                                                                                                                                                                                                                                                                                                                |
| `[kata]`                            | Optional Kata issue-tracker connection; off by default; HTTPS needs `token_env` or `token_file` — see [Kata](/docs/kata/)                                         |
| `[[remote_hosts]]`                  | Remote machines synced by a bare `agentsview source sync` — see [CLI Reference](/docs/commands/#agentsview-sync)                                                                                                                                                                                                                                                                                                                                                                 |
| `[agents.claude-ai]`                | Organizations Claude.ai Sync imports — see [Choose which organizations Sync imports](/docs/chat-import/#choose-which-organizations-sync-imports) |
| `[[session_sources]]`               | Additional filesystem session roots with per-root machine keys — see [Filesystem Session Sync](/docs/filesystem-sync/)                                                                                                                                                                                                                                                                                                                                                           |
| `[automated]`                       | Custom automated-session patterns — see [Automated Session Detection](#automated-session-detection)                                                                                                                                                                                                                                                                                                                                                                              |
| `[custom_model_pricing]`            | Per-model price overrides for usage reports — see [Custom Model Pricing](/docs/token-usage/#custom-model-pricing)                                                                                                                                                                                                                                                                                                                                                                |
| `[model_aliases]`                   | Model names mapped to display groups. See [Model aliases](/docs/token-usage/#model-aliases).                                                                                                                                                                                                                                                                                                                                                                                     |

AgentsView generates `cursor_secret` automatically on first run. For Gist
publishing, it checks a saved `github_token` first. For local browser requests
with no saved token, it then tries `AGENTSVIEW_GITHUB_TOKEN` and finally
`gh auth token` from the GitHub CLI. Local users usually only need to run
`gh auth login`. For remote or proxied access, save a `github_token` on the web
UI **Settings** page or send it to `POST /api/v1/config/github` when you want
AgentsView to publish gists. You can also set remote access fields on the
**Settings** page or with CLI flags. See [Remote Access](/docs/remote-access/)
for details.

`agentsview daemon start` and `agentsview daemon restart` load the normal
effective configuration from `config.toml` and supported environment variables,
and accept no serve-specific flags. `--no-sync` is a runtime-only `serve` option
and cannot be stored in `config.toml`.

Any unknown key under `[vector]` stops configuration loading, even when vector
search is disabled. After upgrading, fix misspelled keys and move endpoint
settings such as `max_batch_tokens` under `[vector.embeddings.servers.<name>]`.
See the
[vector configuration reference](/docs/semantic-search/#enabling-vector).

When `require_auth` is enabled, the browser login prompt accepts the configured
`auth_token`. You can set it in `~/.agentsview/config.toml` or with the
`AGENTSVIEW_AUTH_TOKEN` environment variable, which takes precedence when both
are set. Under a [container deployment](#container-deployment),
`AGENTSVIEW_AUTH_TOKEN_FILE` can supply it from a mounted secret file instead.
If authentication needs a token and none is configured, AgentsView generates one
and saves it as `auth_token` in `config.toml`.

!!! note

    Older configs may still contain `remote_access = true`. AgentsView still
    reads that legacy key for backward compatibility, though new setups should
    use `require_auth = true`.

### Read secrets from files

You can keep connection secrets in separate files instead of environment
variables or `config.toml`. This suits a long-running server because the secret
stays out of `config.toml` and out of whatever starts the process. Each secret
has a `*_file` key alongside its existing key:

| Secret                                     | Existing key           | File key                    |
| ------------------------------------------ | ---------------------- | --------------------------- |
| `[kata]` bearer token                      | `token_env`            | `token_file`                |
| `[integrations.docbank]` API key           | `api_key_env`          | `api_key_file`              |
| `[insights]` API key                       | `api_key_env`          | `api_key_file`              |
| `[vector.embeddings.servers.<name>]` key   | `api_key_env`          | `api_key_file`              |
| `[recall.extract.servers.<name>]` key      | `api_key_env`          | `api_key_file`              |
| Cursor Admin API key (top level)           | `cursor_admin_api_key` | `cursor_admin_api_key_file` |

```toml
[kata]
enabled = true
endpoint = "https://kata.example.com"
token_file = "~/.config/agentsview/kata-token"
```

- Set only one source per secret. Setting both keys for the same secret is a
  configuration error, and AgentsView never picks one for you.
- Paths starting with `~/` expand to your home directory.
- AgentsView strips trailing line breaks (CR and LF) and keeps every other byte.
  An empty or blank file, a file over 64 KiB, or a file containing a NUL byte
  causes an error.
- The file must be private. On macOS and Linux, a file you own needs mode
  `0600` or `0400`, and every directory on its path must belong to you or root
  and not be writable by group or others. A umask of `002` creates
  group-writable directories, so check the parent directory too. Files owned by
  root, such as systemd credentials and Kubernetes secret mounts, work when only
  root can write them. On Windows, only your user may have access to the file.
- If a file is missing or fails these checks, the error names the setting, the
  path, and the reason. Errors and logs never include the secret.

When AgentsView reads each file depends on the feature:

- `[kata]`, `[integrations.docbank]`, `[insights]`, `[vector]`, and
  `[recall.extract]` read their files when configuration loads, skipping
  sections that are turned off. AgentsView treats an unusable file like any
  other invalid setting: it stops the server and commands that use the section,
  and other commands print a warning. Because the server reads these files at
  startup, restart it after you replace one.
- If a command did not load an embeddings or recall extraction section up front,
  that server reads its file when the command starts using it. An unusable file
  then stops that work with an error rather than falling back to an anonymous
  request.
- `cursor_admin_api_key_file` is read when `source cursor sync-usage` runs.
- `AGENTSVIEW_CURSOR_ADMIN_API_KEY` overrides the config-file value, so
  AgentsView skips reading the file while that variable is set.

The `AGENTSVIEW_*_FILE` variables of a
[container deployment](#container-deployment) follow their own rules, covered in
the next section.

### Container deployment

The published container image sets `AGENTSVIEW_MODE`, which enables a small set
of deployment environment variables. They apply only when `AGENTSVIEW_MODE` is
set to a non-blank value, so a desktop or native install ignores them, including
invalid values and missing files. You can also set `AGENTSVIEW_MODE` yourself to
use these variables with the native binary.

An invalid `AGENTSVIEW_MODE` stops `serve` and `pg serve`. Other commands ignore
deployment variables when `AGENTSVIEW_MODE` is invalid; a command that would
read one, such as a daemon client with `AGENTSVIEW_AUTH_TOKEN_FILE` set, fails
with the invalid mode error instead.

| Variable                             | Behavior                                                                                                                                                                                            |
| ------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `AGENTSVIEW_MODE`                    | `serve` or `pg-serve`. When the command line is empty or starts with a flag other than `--help` or `--version`, the image runs `agentsview serve` or `agentsview replica pg serve` with those flags |
| `PG_SERVE`                           | `1`, `true`, `yes`, or `on` selects `pg serve` when `AGENTSVIEW_MODE=serve`; kept for older deployments                                                                                             |
| `AGENTSVIEW_HOST`                    | Listener address. A non-loopback value needs `AGENTSVIEW_REQUIRE_AUTH=true` or `require_auth = true`                                                                                                |
| `AGENTSVIEW_REQUIRE_AUTH`            | Require bearer authentication for API access                                                                                                                                                        |
| `AGENTSVIEW_NO_BROWSER`              | Do not open a browser on start                                                                                                                                                                      |
| `AGENTSVIEW_AUTH_TOKEN_FILE`         | File holding the bearer token; overrides `auth_token` in `config.toml`. `~/` expands to the home directory. Cannot be combined with `AGENTSVIEW_AUTH_TOKEN`                                         |
| `AGENTSVIEW_PG_ALLOW_INSECURE`       | Sets `allow_insecure` for the default PostgreSQL target, for a database on a private network without TLS                                                                                            |
| `AGENTSVIEW_EMBEDDINGS_ENDPOINT`     | OpenAI-compatible embeddings endpoint for recipe adoption in `pg push --embed` and `pg serve`; local `serve` ignores it. See [semantic search on PostgreSQL](/docs/semantic-search/)                |
| `AGENTSVIEW_EMBEDDINGS_API_KEY`      | Inline API key for that endpoint; requires `AGENTSVIEW_EMBEDDINGS_ENDPOINT` and cannot be combined with the file form                                                                               |
| `AGENTSVIEW_EMBEDDINGS_API_KEY_FILE` | File holding that endpoint's API key; requires `AGENTSVIEW_EMBEDDINGS_ENDPOINT`                                                                                                                     |
| `AGENTSVIEW_EMBEDDINGS_BATCH_SIZE`   | Inputs per embeddings request; positive integer, default `32`; requires `AGENTSVIEW_EMBEDDINGS_ENDPOINT`. Lower it for providers with request token limits.                                         |

Anyone who can inspect the container can read `AGENTSVIEW_EMBEDDINGS_API_KEY`,
and every process AgentsView starts (such as an agent CLI) inherits it. To keep
the key out of the environment, use `AGENTSVIEW_EMBEDDINGS_API_KEY_FILE`. Unlike
`AGENTSVIEW_AUTH_TOKEN`, an empty `AGENTSVIEW_EMBEDDINGS_API_KEY` still counts
as set and stops startup on its own or next to the file form, so unset whichever
form you do not use. Because that name belongs to the deployment server, a
`[vector]` server cannot read its key from it; startup stops while it is set, so
give that server's `api_key_env` a different name.

The image sets `AGENTSVIEW_MODE=serve`, `AGENTSVIEW_HOST=0.0.0.0`,
`AGENTSVIEW_REQUIRE_AUTH=true`, and `AGENTSVIEW_NO_BROWSER=true`. An explicit
subcommand such as `pg push --watch` or `mcp` runs as given.

Settings take precedence in this order, from highest to lowest: command flags,
deployment variables, `config.toml`, and built-in defaults. A host set through
`AGENTSVIEW_HOST` counts as persistent configuration, so a non-loopback value
without authentication stops `serve`; only the `--host` flag allows a one-off
unauthenticated bind. `pg serve` ignores network settings in `config.toml` but
applies these variables.

These validation rules fail closed:

- Boolean variables accept `true`, `false`, `1`, `0`, and the other values Go's
  `strconv.ParseBool` accepts. Any other value, an unknown `AGENTSVIEW_MODE`, or
  a blank `AGENTSVIEW_HOST` stops startup with an error naming the variable.
- A missing, unreadable, or empty `AGENTSVIEW_AUTH_TOKEN_FILE` or
  `AGENTSVIEW_EMBEDDINGS_API_KEY_FILE` stops startup. A blank
  `AGENTSVIEW_EMBEDDINGS_API_KEY`, or setting both API-key forms, also stops
  startup. AgentsView never falls back to another secret source.
- Combining any `AGENTSVIEW_EMBEDDINGS_*` variable with a `[vector]` section
  stops startup, so one place owns the embedding recipe.

Because the image's API requires a bearer token, an unauthenticated
`GET /api/ping` returns 401. For a container health check, use a TCP check on
the listener port or treat 401 as alive.

To keep the PostgreSQL password out of the URL, mount a
[passfile](https://www.postgresql.org/docs/current/libpq-pgpass.html) and set
`PGPASSFILE` to its path, or append `?passfile=/run/secrets/pgpass` to
`AGENTSVIEW_PG_URL`. A file with the single line `*:*:*:*:<password>` matches
every connection. The PostgreSQL driver ignores a missing passfile, so a missing
mount shows up as a PostgreSQL authentication error rather than an error naming
the file.

## Remote Hosts

Add `[[remote_hosts]]` entries when you want a bare `agentsview source sync` to
pull raw session files from other machines. Set `transport` to `"http"` (the
default when omitted) or `"ssh"`. An HTTP entry needs an AgentsView daemon on
the remote host and a private network such as Tailscale to reach it:

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
`user@hostname`. When omitted, AgentsView uses `host`. Configure the port, user,
identity, and `ProxyJump` in `~/.ssh/config`. For setup, the files placed on the
remote, and limits, see
[Remote Access — SSH Remote Sync](/docs/remote-access/#ssh-remote-sync).

AgentsView rejects the configuration if any of these apply:

- `transport` is anything other than `http` or `ssh`.
- An HTTP entry (including one with `transport` omitted) has no `url` or no
  `token`, or sets `destination`.
- An SSH entry sets `url` or `token`.
- An SSH entry's destination starts with `-` (where the destination is
  `destination`, or `host` when `destination` is omitted).

HTTP remote sync calls the remote daemon's archive endpoints and always uses a
bearer token, even when that daemon sets `require_auth = false`. Your per-host
`token` must match the remote daemon's `auth_token`. Do not reuse the collector
daemon's own `auth_token` for untrusted remote endpoints. Both HTTP and SSH
transfers keep a persistent per-host mirror and fetch only changed files. A full
archive is used only when every manifest file needs fetching or a delta request
is refused; see
[Remote Access — Incremental Sync](/docs/remote-access/#incremental-sync).

When a full or automatic data-version rebuild includes local sources, configured
remote hosts (both HTTP and SSH) join the same temporary-database bulk ingest
and atomic swap. Running with `--full` reparses the complete local and remote
corpus without retransferring unchanged files from manifest-capable spokes. HTTP
remote sync requires the collector and remote daemon to use the same remote-sync
protocol version. After you upgrade either host, upgrade the other before
syncing again; incompatible peers fail before exchanging targets or archive
data. SSH hosts need no manual upgrade because AgentsView places a matching
binary on the remote; see
[SSH Remote Sync](/docs/remote-access/#ssh-remote-sync).

Keep each `remote_hosts.host` value unique and stable. It namespaces imported
session IDs, the database skip cache, and the persistent mirror, so changing it
for the same machine can duplicate sessions, and reusing it for another machine
can reuse stale state. To sync one configured host, run
`agentsview source sync --host <name>` (which also works for direct sync with
`AGENTSVIEW_NO_DAEMON=1`). Unknown host names are rejected, and ad hoc remotes
are not supported. HTTP failures report actionable messages for common issues
such as token rejection, missing remote archive endpoints, connection refusal,
DNS failures, and timeouts. For how SSH failures are reported, see
[Offline Hosts](/docs/remote-access/#offline-hosts).

To have a running collector daemon sync a host on a schedule, set `interval` to
a positive duration such as `"5m"`. Leaving `interval` zero or omitted turns off
the per-host schedule, though manual `agentsview source sync` still includes the
host.

The remote daemon also needs to listen on an interface the collector can reach.
By default the server binds `127.0.0.1`, so set `host` with
`require_auth = true` in the remote machine's `config.toml` for a persistent
node:

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

For a one-off flag override, you can still run
`agentsview serve --background --host 0.0.0.0`, including without auth, though
authenticated persistent configuration is better for an always-available remote
node.

Explicit `agentsview daemon start` and `daemon restart` stay running until you
stop them, while automatic CLI startup and `agentsview serve --background` exit
after `daemon_idle_timeout` when idle. Set `daemon_idle_timeout` to zero on
machines that need to stay available for HTTP remote sync:

```toml
daemon_idle_timeout = "0s"
```

Supervised daemons running under systemd, launchd, Docker, or a foreground shell
do not create the detached-daemon idle tracker, so they do not idle-exit
regardless of this setting.

## Cursor Admin Usage API

`agentsview source cursor sync-usage` imports Cursor Admin API usage events into
your local archive so Cursor's billed usage can appear in the Usage dashboard
and `agentsview report usage daily` reports. Add your API key to
`~/.agentsview/config.toml`:

```toml
cursor_admin_api_key = "key_xxxxx"
cursor_admin_email = "you@example.com" # optional
cursor_admin_user_id = "152683922"     # optional
```

To keep the key out of `config.toml`, set `cursor_admin_api_key_file` to a
private file holding it instead; see
[Read secrets from files](#read-secrets-from-files).

Environment variables override the config file:

```bash
export AGENTSVIEW_CURSOR_ADMIN_API_KEY=key_xxxxx
export AGENTSVIEW_CURSOR_ADMIN_EMAIL=you@example.com
export AGENTSVIEW_CURSOR_ADMIN_USER_ID=152683922
```

AgentsView also accepts the legacy unprefixed names `CURSOR_ADMIN_API_KEY`,
`CURSOR_ADMIN_EMAIL`, and `CURSOR_ADMIN_USER_ID` when the matching `AGENTSVIEW_`
variable is unset. The email and user ID values act as default filters; pass
`--email` or `--user-id` to `agentsview source cursor sync-usage` to override
them for a single import.

## Session Discovery

AgentsView discovers session files automatically from the agent sources below.
Amp support is deprecated because current Amp releases may keep full threads
server-side and leave only local stubs, though historical local Amp thread JSON
files can still be parsed.

You can override an agent's default directories with its matching environment
variable or `agents.<id>.dirs` in `config.toml`; environment variables take
precedence when both are set. An explicit empty `agents.<id>.dirs` array, such
as `agents.grok.dirs = []`, clears that agent's default local directories so
local discovery finds nothing there, while matching `session_sources` entries
for that agent still apply. For provider-wide exclusion, see
[Disabling Session Providers](#disabling-session-providers). Omitting the key
keeps the default directories.

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
| Factory Droid         | `~/.factory/sessions/`                                                                                                                                           | JSONL per session plus a `<id>.settings.json` token-usage sidecar                                                                                             |
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
| Muse Personal         | `~/agents/` on your Muse's hosted machine                                                                                                                        | `agent-<uuid>/sessions/<session-uuid>.jsonl` with optional sibling `sessions.json`; override with `MUSE_PERSONAL_AGENTS_DIR`                                  |
| OMO                   | `~/.omo/agent/sessions/`                                                                                                                                         | Pi-family JSONL per session                                                                                                                                   |
| OhMyPi                | `~/.omp/agent/sessions/`                                                                                                                                         | JSONL per session                                                                                                                                             |
| OpenClaw              | `~/.openclaw/agents/` and `~/.kimi_openclaw/agents/`                                                                                                             | JSONL per session or SQLite per agent (`agent/openclaw-agent.sqlite`)                                                                                        |
| OpenCode              | `~/.local/share/opencode/`                                                                                                                                       | SQLite DB or `storage/` JSON files                                                                                                                            |
| Open Code Review      | `~/.opencodereview/sessions/`                                                                                                                                    | One JSONL file per review under an encoded project directory                                                                                                  |
| Open Interpreter      | `~/.openinterpreter/sessions/`, `archived_sessions/`                                                                                                             | Codex-format JSONL per session                                                                                                                                |
| OpenHands CLI         | `~/.openhands/conversations/`                                                                                                                                    | Per-conversation `base_state.json` + `events/*.json`                                                                                                          |
| Omnigent              | `~/.omnigent/`                                                                                                                                                   | SQLite `chat.db`, one session per conversation                                                                                                                |
| Pi                    | `~/.pi/agent/sessions/`                                                                                                                                          | JSONL per session                                                                                                                                             |
| fx                    | `~/.fx/sessions/`                                                                                                                                                | Schema 4 session folders with `events.jsonl`, `session.json`, and optional `usage-v2.json` (older fx)                                                                           |
| Tau                   | `~/.tau/sessions/`                                                                                                                                               | JSONL transcripts under `<project>/`, with metadata `index.jsonl` excluded                                                                                    |
| Prime Agent           | `~/.prime/agent/sessions/`                                                                                                                                       | Flat Pi-family JSONL sessions                                                                                                                                 |
| StepCode              | `~/.stepcode/agent/sessions/`                                                                                                                                    | JSONL per session                                                                                                                                             |
| Direct API            | `~/.agentsview/direct-api/`                                                                                                                                      | Body-free JSONL usage files                                                                                                                                   |
| Poolside              | `~/Library/Application Support/poolside/trajectories/` (macOS), `~/.local/state/poolside/trajectories/` (Linux), `%APPDATA%\\poolside\\trajectories\\` (Windows) | NDJSON trajectory files                                                                                                                                       |
| Piebald               | `~/.local/share/piebald/` (Linux), `~/Library/Application Support/piebald/` (macOS), `~/AppData/Roaming/piebald/` (Windows)                                    | SQLite database (`app.db`)                                                                                                                                    |
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

Mistral Vibe tags each unified session with its last recorded model pin in
`runtime-state.json`. A subagent session inherits its parent's model whenever
the subagent changes and keeps it after later parent turns. Sessions with no
recorded model keep their tokens, though usage reports leave them out. A running
Vibe turn appears as soon as Vibe journals it, before the session goes idle.

OpenClaw checks both representations under each configured agent directory. The
SQLite layout uses one `agent/openclaw-agent.sqlite` file per agent, which
AgentsView reads without writing to or migrating the database.

**Augure Code.** Augure Code reads Codex-format JSONL from
`~/.augure/sessions/` and appears under the separate `augure-code` agent ID. To
use another sessions root, set `AUGURE_CODE_SESSIONS_DIR` or
`agents.augure-code.dirs`. Resume these sessions with `augure resume`, not
`codex resume`.

**Open Interpreter.** Open Interpreter reads Codex-format JSONL from
`~/.openinterpreter/sessions/` and `~/.openinterpreter/archived_sessions/` and
appears under the separate `open-interpreter` agent ID. To move both defaults,
set `INTERPRETER_HOME` as Open Interpreter itself does, or set
`OPEN_INTERPRETER_SESSIONS_DIR` or `agents.open-interpreter.dirs` for other
sessions roots. `CODEX_HOME` does not affect these paths. Resume these sessions
with `interpreter resume`. AgentsView titles each session from its first prompt
and does not read names set with Open Interpreter's rename command.

**Augure Desktop 3 beta.** Augure Desktop 3 beta reads `state.db` and its
`sessions/` sibling from `~/.augure-desktop/` on macOS and Linux, or
`~/AppData/Local/augure-desktop/` on Windows. To use another data root, set
`AUGURE_DESKTOP_DIR` or `agents.augure-desktop.dirs`. These sessions appear as
`augure-desktop`, separate from Augure Code and Hermes. Local browsing, search,
exports, and recorded usage work, while remote source-file sync is disabled for
this provider.

**Hermes Agent.** Hermes Agent stores current session state in the SQLite
`state.db` file. AgentsView checks `~/.hermes/sessions/` on macOS and Linux and
`~/AppData/Local/hermes/sessions/` on Windows. The sessions root resolves its
sibling `state.db`, so discovery still works even when the `sessions/` directory
does not exist. If Windows `LOCALAPPDATA` is redirected or a named Windows
profile uses another Hermes home, set `HERMES_SESSIONS_DIR` or
`hermes_sessions_dirs` explicitly.

Cron runs stay under `hermes-cron` with stable job groups in Usage attribution.
Renames keep costs together, and jobs with the same name stay separate.
Compressed sessions follow parent links available in `state.db` when parsed, and
a non-empty source in `state.db` takes precedence over the transcript's
platform. Archived runs whose sources were gone before upgrading stay individual
sessions unless they already have a stored job ID.

**Cline CLI.** Cline support covers the CLI, not the VS Code extension. Point
`CLINE_DIR` or `agents.cline.dirs` at its data root or directly at its sessions
directory. AgentsView reads the CLI's conversations, tool activity, usage, and
recorded costs, including through remote sync. Continued teammate runs stay
separate sessions, matching Cline's own store.

**DeepSeek Harness.** AgentsView reads DeepSeek Harness sessions from its
default JSONL persistence backend, including released format versions 0 through
4 in plain or zstd-compressed files. Version 0 uses `session.jsonl[.zstd]`, and
later versions use `session.vN.jsonl[.zstd]`. When a session directory keeps
multiple immutable generations, AgentsView reads the numerically newest
supported generation. Inherited parent transcript content is left out of child
sessions. `DSH_HOME` re-roots the default `<home>/sessions` path; to point
directly at one or more session roots, set `DEEPSEEK_HARNESS_SESSIONS_DIR` or
`agents.deepseek-harness.dirs`. The optional SQLite persistence backend is not
supported.

**Prime Agent.** Prime Agent support targets the current flat session layout in
v0.7.0. That release migrates the older per-project layout when Prime Agent
opens its session store, so open the current Prime Agent once before you sync a
legacy archive with AgentsView.

**fx.** fx sessions appear from `~/.fx/sessions/` when `session.json` uses
schema 4, and subagent runs appear as children of their parent session. Usage
comes from `usage-v2.json` when present (older fx only) as cumulative totals per
model, dated at the earlier of the session's last event and the usage file's
modification time. Tool results use larger `.txt` files from `tool-results/`
only when they are regular files with valid UTF-8 and match the saved byte count
or reach the 16 MiB cap; other files keep the stored preview. Restoring a
missing file refreshes the result on the next sync. Freshness checks cover entry
names, sizes, and modification times in `tool-results/`, which suffices because
fx names result files by content hash. Costs use AgentsView's pricing catalog.
To scan another root, including a WSL path on Windows, set `FX_SESSIONS_DIR` or
`agents.fx.dirs`. The legacy `fx_dirs` key converts to `agents.fx.dirs`.

**Tau.** Tau stores multiple session transcripts in each project directory under
`~/.tau/sessions/`. AgentsView reads `.jsonl` files directly below those project
directories, skips the exact `index.jsonl` metadata file, and follows the latest
`leaf` entry when selecting active history. To use another sessions root, set
`TAU_SESSIONS_DIR` or `agents.tau.dirs`.

User-run shell commands (`bashExecution`) and messages with the `custom`,
`branchSummary`, or `compactionSummary` role are not shown. Separate
`branch_summary` and `compaction` entries are shown.

**Muse Code.** Muse Code keeps one event log per session under
`~/.local/share/muse/sessions/YYYY/MM/DD/<session-id>/session.jsonl`, or under
`$XDG_DATA_HOME/muse/sessions/` when `XDG_DATA_HOME` is set, matching Muse.
AgentsView rebuilds the conversation from those events: prompts, replies,
reasoning summaries, tool calls, tool results, and per-reply token usage.
Subagent runs under `subagent/<child-id>/` appear as child sessions of the
session that holds them. AgentsView ignores Muse's generated session names such
as `quiet-lyra` as titles, while keeping any name you set. To use another
sessions root, set `MUSE_SESSIONS_DIR` or `agents.muse.dirs`.

**Qoder default directories.** Qoder discovery includes the legacy
`~/.qoder/projects/` and `~/.qoderwork/projects/` export roots, the Qoder CLI CN
store, and the current IDE store:

- **Qoder CLI CN.** `~/.qoder-cn/projects/`

- **macOS.**
    `~/Library/Application Support/Qoder/SharedClientCache/cli/projects/`

- **Linux.** `~/.config/Qoder/SharedClientCache/cli/projects/`

- **Windows.** `%APPDATA%\Qoder\SharedClientCache\cli\projects\`

To replace these defaults with one or more explicit roots, set
`QODER_PROJECTS_DIR` or `agents.qoder.dirs`.

**Grok.** AgentsView reads Grok sessions from `summary.json` (title, timestamps,
and project), optional `signals.json` (token counters), and `chat_history.jsonl`
when present for the full transcript (user turns, assistant replies, thinking,
and tool calls). Because Grok trims `chat_history.jsonl` each time it compacts a
session, AgentsView rebuilds the turns before each compaction from
`updates.jsonl` and marks each compaction in the transcript. If
`chat_history.jsonl` is missing, AgentsView falls back to summary-only mode. To
override the default directory, set `GROK_DIR` or `agents.grok.dirs`.

**Goose default directories.**

- **macOS and Linux.** `~/.local/share/goose/sessions/`
- **Windows.** `%APPDATA%/Block/goose/data/sessions/`

`GOOSE_PATH_ROOT` follows Goose's own path-root convention and resolves
`<root>/data/sessions/sessions.db`. An `agents.goose.dirs` entry may instead
point directly at that sessions directory, its parent data directory, or the
database file.

**Crush default directories.**

- **macOS and Linux.** `~/.local/share/crush/`
- **Windows.** `%LOCALAPPDATA%/crush/`

AgentsView reads `projects.json` in that directory and discovers one SQLite
`crush.db` per project under each listed `<project>/.crush/` data directory. To
override the default with one or more directories, set `CRUSH_DIR` or
`agents.crush.dirs`. Each entry can be a Crush data directory containing
`projects.json`, a `<project>/.crush` directory, or a `crush.db` file. A project
added after AgentsView starts is picked up on the next scheduled reconciliation
pass.

**Omnigent.** AgentsView reads Omnigent sessions from `~/.omnigent/chat.db`. To
override the default directory, set `OMNIGENT_DIR` or `agents.omnigent.dirs`.
AgentsView creates one session per conversation and supports both the split
text-ID and current binary-UUID schema generations; it detects the older
single-table schema and reports it as unsupported without losing sessions
already synced from it. HTTP remote sync stays disabled for Omnigent because
`chat.db` stores authentication secrets alongside transcripts. A metadata-only
edit made directly in `chat.db` can be deferred by the immediate
filesystem-event sync and picked up by the next scheduled reconciliation pass or
an explicit resync.

**VS Code Copilot default directories.**

- **macOS.** `~/Library/Application Support/Code/User/`
- **Linux.** `~/.config/Code/User/`
- **Windows.** `%APPDATA%/Code/User/`

AgentsView also discovers Code Insiders and VSCodium variants automatically.

**Visual Studio Copilot default directories.**

- **macOS.** `~/Library/Caches/VSGitHubCopilotLogs/traces/`
- **Linux.** `~/.cache/VSGitHubCopilotLogs/traces/`
- **Windows.** `%LOCALAPPDATA%/Temp/VSGitHubCopilotLogs/traces/`

This is separate from VS Code Copilot. Visual Studio Copilot writes trace files
named like `*_VSGitHubCopilot_traces.jsonl`. If your installation writes them
elsewhere, set `VISUALSTUDIO_COPILOT_DIR` or `agents.visualstudio-copilot.dirs`.

**Windsurf default directories.**

- **macOS.** `~/Library/Application Support/Windsurf/User/` and
    `~/Library/Application Support/Windsurf - Next/User/`
- **Linux.** `~/.config/Windsurf/User/` and `~/.config/Windsurf - Next/User/`
- **Windows.** `%APPDATA%/Windsurf/User/` and `%APPDATA%/Windsurf - Next/User/`

Windsurf stores workspace chats in `workspaceStorage/<hash>/state.vscdb`.

**Trae default directories.**

- **macOS.** `~/Library/Application Support/Trae/User/`, `Trae CN/User/`, and
    `TRAE SOLO CN/User/`
- **Linux.** `~/.config/Trae/User/`, `Trae CN/User/`, and `TRAE SOLO CN/User/`
- **Windows.** `%APPDATA%/Trae/User/`, `Trae CN/User/`, and `TRAE SOLO CN/User/`

Trae stores chats in `workspaceStorage/<hash>/state.vscdb` and
`globalStorage/state.vscdb`. You can override these roots with `TRAE_DIR` or
`agents.trae.dirs`. AgentsView watches `workspaceStorage` and `globalStorage`,
then reads chat records from those SQLite stores.

Trae legacy inline-message parsing is supported, while modern encrypted
transcript layouts are detected and reported as unsupported. HTTP remote target
resolution is still disabled. A Trae root is a full user profile, and AgentsView
does not archive or ship that profile wholesale. The follow-up path is
Windsurf-style curated file targets only: `state.vscdb`, `state.vscdb-wal`, and
`workspace.json` for each supported workspace store.

**Kimi Work default directories.** Kimi Work is the kimi-desktop app (the
"daimon" runtime). It stores conversations as kimi-code kernel wire logs under
`<root>/wd_<workspace>_<hash>/<session>/agents/<agent>/wire.jsonl`:

- **macOS.**
    `~/Library/Application Support/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/sessions/`
- **Linux.**
    `~/.config/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/sessions/`
    (or `~/.local/share/...` on some installs)
- **Windows.**
    `%APPDATA%/kimi-desktop/daimon-share/daimon/runtime/kimi-code/home/sessions/`

Only `conv-*` session directories are user conversations; auxiliary internal
sessions (`ctitle-*`, `sklsum-*`, `dvlt-*`) are excluded from discovery. If your
installation stores them elsewhere, set `KIMI_WORK_DIR` or
`agents.kimi-work.dirs`.

**Positron Assistant default directory (macOS only).**

- **macOS.** `~/Library/Application Support/Positron/User/`

Because Positron is built on VS Code, its sessions use the same
`workspaceStorage/<hash>/chatSessions/` layout as VS Code Copilot. As of
v0.20.0, Positron Assistant has a built-in default path only on macOS. On Linux
and Windows, point `POSITRON_DIR` or `agents.positron.dirs` at your Positron
user directory, for example `~/.config/Positron/User` on Linux or
`%APPDATA%\Positron\User` on Windows.

**Posit Assistant.** Posit Assistant (posit-dev/assistant, also known as
Databot) is separate from the Positron IDE's built-in Assistant above. It stores
one directory per conversation under
`~/.posit/assistant/workspaces/<workspaceId>/<conversationId>/`, containing a
`conversation.json` message tree and an append-only `lm-messages.jsonl`
transcript; subagent runs nest under a `subagents/` subdirectory of their parent
conversation. All Posit Assistant hosts (Positron/VS Code extension, standalone,
desktop, and TUI) share this location. If your installation stores its
workspaces elsewhere, set `POSIT_ASSISTANT_DIR` or `agents.posit-assistant.dirs`.

**Cursor IDE.** Cursor IDE is the graphical editor, separate from the Cursor
command-line agent above. AgentsView reads Composer sessions from Cursor's
shared `globalStorage/state.vscdb` database, following Cursor's normal user data
directory on macOS, Linux, and Windows. To override that path, set
`CURSOR_IDE_DIR` or `agents.cursor-ide.dirs`. Because this database also holds
authentication and extension state, Cursor IDE is excluded from remote
source-file sync. Parsed sessions still live in the local archive and can be
shared through PostgreSQL or ClickHouse.

**Claude Cowork default directories.** These follow Claude Desktop's Electron
user-data location:

- **macOS.** `~/Library/Application Support/Claude/local-agent-mode-sessions/`
- **Linux.** `~/.config/Claude/local-agent-mode-sessions/`
- **Windows.**
    `%LOCALAPPDATA%\Packages\Claude_pzs8sxrjxfjjc\LocalCache\Roaming\Claude\local-agent-mode-sessions\`
    or `%APPDATA%\Claude\local-agent-mode-sessions\`

If Claude Desktop stores local-agent-mode sessions somewhere else, set
`COWORK_DIR` or `agents.cowork.dirs`.

**Codebuff / Freebuff sessions.** Codebuff and Freebuff share the same on-disk
layout under `~/.config/manicode/projects/`. Each session is a timestamped
directory with `chat-messages.json` (the full transcript with subagent
invocations), `run-state.json` (metadata including agent type and model), and
optional `chat-meta.json`.

AgentsView classifies each session as **Codebuff** (paid) or **Freebuff** (free
tier) from the `agentType` field in `run-state.json`: sessions whose `agentType`
contains `"free"` go under Freebuff. Both appear as separate filters in the
session list so you can browse paid and free sessions independently.

AgentsView parses subagent tool calls (`basher`, `code-searcher`, `file-picker`,
`code-reviewer`, and similar) from the AI message blocks and displays them
inline in the transcript view. The agent template (for example,
`base2-free-deepseek` or `base2-free-mimo`) comes from `run-state.json`'s
`agentType` field and appears in the session detail header. That label is used
server-side to choose the per-step LLM; the CLI does not persist the literal
LLM, and it is not visible in the UI. Project names come from the session's
working directory using git-root detection.

Codebuff and Freebuff sessions report cost only, as one reported-cost row per
prompt. The CLI's on-disk format does not record per-message input,
output, or cache tokens. A row is attributed to the model the turn ran when the
session records one (such as a bring-your-own-key model), and otherwise to the
agent template (for example, `base2-deepseek` or `base2-free-minimax-m3`).
Embedded pricing tables do not include per-model rates for `base2-*` templates,
so cache savings for these rows are zero.

Each subagent spawned by a Codebuff or Freebuff session is stored as its own
session and linked to its parent. Attached images, pasted text, and files show up
as labeled markers such as `[Image: ...]` and `[Text attachment: N chars]`.

Freebuff has no config key of its own. It shares the Codebuff provider, and the
parser classifies each session. Both CLIs move their config directory to
`FREEBUFF_CONFIG_DIR` when set, and AgentsView then looks under
`<FREEBUFF_CONFIG_DIR>/projects` instead of `~/.config/manicode/projects`.
`CODEBUFF_DIR` or `agents.codebuff.dirs` take precedence over both.

**OpenHands CLI shallow watch.** OpenHands stores each conversation in its own
subdirectory, which would use one recursive file watch per session and can
exhaust inotify limits on Linux. AgentsView watches `~/.openhands/conversations/`
non-recursively and relies on the 15-minute periodic sync to pick up changes
inside existing conversations, while still detecting new conversation
directories right away. The server's startup log reports how many directories
are watched this way:

```
Watching 74 directories for changes (2 shallow) (76ms)
```

**Devin CLI root.** Point `DEVIN_DIR` or `agents.devin.dirs` at the local root
that contains Devin's `cli/` directory, not at copied config or OAuth files. The
defaults are `~/Library/Application Support/devin` on macOS and
`~/.local/share/devin` on Linux, and AgentsView discovers session data under
`<root>/cli/...`. When you share a path publicly, redact parent directories and
keep only the relevant tail, for example `.../Application Support/devin` or
`.../.local/share/devin`.

AgentsView ignores copied config and OAuth locations because those paths are not
the session archive source and may contain sensitive account material. When you
file a bug, share only the redacted local-share root and directory shape, never
pasted tokens, OAuth files, or other secrets.

**OpenCode storage backend.** As of 0.24.0, AgentsView reads both of OpenCode's
layouts automatically with no configuration. If a `storage/session/` directory
exists under the OpenCode root, it parses sessions from the per-file JSON layout
(`storage/session`, `storage/message`, and `storage/part`); otherwise it reads
the legacy `opencode.db` SQLite file. In storage mode, the file watcher watches
only the `storage/` subtree rather than the whole OpenCode directory, so
unrelated OpenCode state such as binaries, logs, and caches no longer triggers
sync events. In SQLite mode, it watches the parent directory of `opencode.db`.

Kilo and MiMoCode use the same OpenCode-format storage reader. Kilo reads from
`storage/session` and MiMoCode reads from `storage/session_diff` when present,
and both fall back to their SQLite databases when the file-backed storage layout
is absent.

**aider discovery.** aider writes one `.aider.chat.history.md` file per
repository rather than using a central session directory. AgentsView skips Aider
logs unless you opt in with `AIDER_DIR` or `agents.aider.dirs`. Always-on
home-directory discovery caused unwanted macOS privacy prompts during background
refreshes, so Aider discovery is limited to roots you configure explicitly. On
macOS, broad home roots still skip protected top-level folders unless you
configure one of those folders directly.

**Warp default directories.**

- **macOS.**
    `~/Library/Group Containers/2BBY89MBSN.dev.warp/Library/Application Support/dev.warp.Warp-Stable/`
- **Linux.** `~/.local/state/warp-terminal/`
- **Windows.** `~/AppData/Local/warp/Warp/data/`

**Zed default directories.**

- **macOS.** `~/Library/Application Support/Zed/`
- **Linux.** `~/.local/share/zed/`
- **Windows.** `~/AppData/Local/Zed/`

Zed keeps all assistant threads in a single `threads/threads.db` SQLite database
under its data directory. AgentsView reads it directly, including model names
and per-request token usage.

**Kiro IDE default directories.**

- **macOS.**
    `~/Library/Application Support/Kiro/User/globalStorage/kiro.kiroagent/`
- **Linux.** `~/.config/Kiro/User/globalStorage/kiro.kiroagent/`
- **Windows.** `~/AppData/Roaming/Kiro/User/globalStorage/kiro.kiroagent/`

**RooCode default directories.**

- **macOS.**
    `~/Library/Application Support/Code/User/globalStorage/rooveterinaryinc.roo-cline/`
- **Linux.** `~/.config/Code/User/globalStorage/rooveterinaryinc.roo-cline/`
- **Windows.**
    `~/AppData/Roaming/Code/User/globalStorage/rooveterinaryinc.roo-cline/`

RooCode (`rooveterinaryinc.roo-cline`) is a VSCode extension that stores
sessions under `tasks/<taskId>/` in VSCode's globalStorage directory. Each task
directory holds `history_item.json` (metadata including task description, model
name, workspace path, token counts, and recorded cost) and `ui_messages.json`
(the Cline-format transcript with user prompts, assistant responses, reasoning
blocks, and tool calls). AgentsView reads `apiConfigName` from
`history_item.json` as the session model, derives project names from the
workspace path using git-root detection, and records `totalCost` as a usage
event for cost tracking.

RooCode shut down on May 15, 2026. ZooCode (`Zoo-CodeInc.zoo-cline`) is the
active community fork; separate support is planned. If your VSCode globalStorage
directory is elsewhere, set `ROOCODE_DIR` or `agents.roocode.dirs`.

**Kilo (legacy) default directories.** All three platforms root these at the canonical
lowercase `kilocode.kilo-code` global storage directory that VSCode writes on
disk:

- **macOS.**
    `~/Library/Application Support/Code/User/globalStorage/kilocode.kilo-code/`
- **Linux.** `~/.config/Code/User/globalStorage/kilocode.kilo-code/`
- **Windows.** `%APPDATA%/Code/User/globalStorage/kilocode.kilo-code/`

Each `<root>/tasks/<uuid>/` task directory holds three JSON files:
`task_metadata.json` (which stores only `files_in_context`), the Claude-shaped
`api_conversation_history.json`, and the Cline-shaped `ui_messages.json`.
AgentsView folds the latter two into a composite fingerprint anchored on
`task_metadata.json` so changes to any of the three trigger a reparse. Sessions
go through RooCode-descended Cline message handling (tool-call and result
pairing, reasoning pipeline, compact boundaries, and error linking). If the
legacy extension stores its data outside the standard locations, set
`KILO_LEGACY_DIR` or `agents.kilo-legacy.dirs`.

**Kilo (legacy) vs Kilo.** These are two distinct agents. *Kilo* (the `kilo`
agent) is the OpenCode-based core at `~/.local/share/kilo/`, covering both the
Kilo CLI and the rebuilt Kilo Code VS Code extension, which share `kilo.db`.
*Kilo (legacy)* (the `kilo-legacy` agent) is the older RooCode-derived VS Code
extension that wrote per-task JSON under `kilocode.kilo-code/tasks/` and stopped
receiving new sessions after Kilo rebuilt the extension on OpenCode (public beta
2026-03-10, GA 2026-04-02). The `kilo-legacy` agent stays frozen at that legacy
format and only archives older sessions, while newer Kilo VS Code activity shows
up under `kilo`.

**Antigravity CLI transcript sources.** Antigravity CLI has used both SQLite
databases and AES-encrypted `.pb` files. AgentsView reads whichever source is
richest, in this order:

1. **Decrypted trajectory sidecar.** For either format, if a
   `<uuid>.trajectory.json` file sits next to the source `.db` or `.pb` file
   under `conversations/` or `implicit/` and covers the session, AgentsView uses
   it as the source of truth for the full structured transcript: messages, tool
   calls, tool results, reasoning, and diffs. This is the highest-fidelity
   source for both formats. [agy-reader](https://github.com/mjacobs/agy-reader)
   handles the decryption and writes these sidecars out-of-process; AgentsView
   reads the resulting plain JSON as untrusted input and needs no
   `ANTIGRAVITY_KEY` in this mode.
1. **Plaintext brain transcript.** Since 0.46.1, AgentsView reads
   `brain/<uuid>/.system_generated/logs/transcript.jsonl`. For SQLite sessions,
   it uses this transcript when it covers every database step, attaching model
   and token usage from the database to the matching model replies. Prompts from
   `history.jsonl` that the transcript has not recorded yet are added back, and
   repeated prompts stay separate turns. For older `.pb` sessions, a brain
   transcript can supply the conversation without a decryption key.
1. **SQLite trajectory database.** Newer Antigravity CLI releases write
   `conversations/<uuid>.db`. Without a covering sidecar or brain transcript,
   AgentsView opens the database read-only and decodes the trajectory steps
   directly. That heuristic decode recovers prompts and tool-call names but not
   full structured tool results, reasoning, or diffs, producing a degraded
   **summary mode** transcript. If both `conversations/<uuid>.db` and
   `conversations/<uuid>.pb` exist, the SQLite database wins. Change detection
   tracks data-bearing `<uuid>.db-wal` changes, while SQLite's temporary
   `<uuid>.db-shm` index does not trigger a re-import.
1. **In-process `.pb` decryption.** When no sidecar is present, set
   `ANTIGRAVITY_KEY` (a base64-encoded AES key of 16, 24, or 32 bytes after
   decoding) before you start AgentsView so it can decrypt `.pb` payloads
   itself, mirroring the upstream Python tool
   [`antigravity_decryptor`](https://github.com/arashz/antigravity_decryptor).
1. **Plaintext summary mode.** Otherwise AgentsView reads only `history.jsonl`
   and the `brain/` summaries, which is enough for session metadata and a
   high-level transcript.

If the database cannot be read, AgentsView keeps archived replies and reports
the parse error unless a trajectory sidecar supplies the transcript. A brain
transcript alone cannot establish coverage of an unreadable database. Partial
transcripts from readable databases remain eligible for retry.

Antigravity IDE and CLI databases work on read-only mounts when their pending
writes have been checkpointed into the database; AgentsView does not ignore
uncheckpointed writes to make a read succeed. The 0.46.1 upgrade reprocesses
existing Antigravity CLI sessions to recover missing repeated turns.

Heuristic `.db` decode, in-process `.pb` decryption, and plaintext summary mode
all show a "Summary mode" badge in the detail header, while covering trajectory
sidecars and brain transcripts show the full transcript. If you need a
structured trajectory sidecar for `.db` or `.pb` sessions, install `agy-reader`:

```bash
go install github.com/mjacobs/agy-reader@latest
agy-reader --sync
agy-reader --watch
```

You can override any default with an environment variable (one directory per
variable). For Aider, this opt-in is required because there is no default
discovery root:

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
export FACTORY_DIR=~/custom/factory/sessions
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
export MUSE_PERSONAL_AGENTS_DIR=~/custom/agents
export VIBE_SESSIONS_DIR=~/custom/vibe/logs/session
export OMP_DIR=~/custom/omp
export OPENCLAW_DIR=~/custom/openclaw
export OPENCODE_DIR=~/custom/opencode
export OPEN_INTERPRETER_SESSIONS_DIR=~/custom/openinterpreter/sessions
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

### Muse Personal

Muse Personal is Meta's personal assistant, separate from Muse Code.

**AgentsView does not collect Muse Personal sessions on your laptop.** Your
Muse runs on a machine that Meta hosts for you and writes its transcripts there.
The Muse desktop, web, and phone apps only connect to that machine and keep no
transcripts on your computer, so an AgentsView installed on your laptop finds no
Muse Personal sessions. AgentsView needs to run on your Muse's own machine.

To collect your Muse Personal sessions:

1. Ask your Muse to install the AgentsView CLI on its machine:

   ```bash
   curl -fsSL https://agentsview.io/install.sh | bash
   ```

   The installer fetches the latest release. Muse Personal requires 0.48.0 or
   later.

1. Ask your Muse to run `agentsview source sync` on that machine. AgentsView
   reads `~/agents/agent-<uuid>/sessions/<session-uuid>.jsonl` under your
   Muse's home directory.

1. To browse these sessions alongside your other agents, configure `[pg]` on
   your Muse's machine and ask your Muse to run
   `agentsview replica pg service install`. That service keeps pushing new
   sessions to the PostgreSQL database your other machines push to. Your Muse's
   machine needs network access to that database; see
   [PostgreSQL Sync](/docs/pg-sync/#multi-machine-workflow).

Each transcript must have a version-1 `session_header` with a session ID
matching its filename. The optional sibling `sessions.json` index can extend the
session's end time, while missing, stale, or unknown-version indexes do not
prevent collection.

To set the root explicitly, set `MUSE_PERSONAL_AGENTS_DIR` or configure it in
`config.toml`:

```toml
[agents.muse-personal]
dirs = ["/example/agents"]
```

Use an explicit root when the collecting process's `HOME` differs from the
transcript owner's. Run `agentsview doctor sync` to see the resolved root.

AgentsView collects both sessions your Muse started on a schedule and ones you
started. Raw capture keeps the transcript and any regular index as original
bytes.

Items appear in stable `seq` order, with same-sequence occurrences keeping their
file order. Tool outputs pair only with a unique preceding unmatched call;
ambiguous or unmatched outputs stay as separate tool rows. Compaction summaries
appear as boundaries without deleting earlier history. File references stay as
inert text: AgentsView never opens their recorded paths. A final line still
being written delays publication until it is complete.

AgentsView does not infer titles, parent links, public model names, token usage,
or costs from these files. Context-window snapshots and opaque metadata stay in
raw custody. The maintainer
[format evidence](https://github.com/kenn-io/agentsview/blob/main/docs/internal/session-format-sources.md#muse-personal-muse-personal)
documents the projection's limits.

### CodeBuddy

Tencent CodeBuddy CN uses the `codebuddy` agent ID and reads `history` under the
configured `CodeBuddyExtension/Data` directory. On Windows, the default follows
`%LOCALAPPDATA%`, falling back to `~/AppData/Local` when that variable is unset
or not absolute. macOS uses
`~/Library/Application Support/CodeBuddyExtension/Data`, and Linux uses
`~/.config/CodeBuddyExtension/Data`. Set `CODEBUDDY_DIR` to replace these
defaults.

Session projects use the working directory when present, falling back to the
workspace identifier. Message changes, including deletion, refresh their owning
session, and workspace index changes refresh sessions in that workspace. See the
[format evidence](https://github.com/kenn-io/agentsview/blob/main/docs/internal/session-format-sources.md#codebuddy)
for usage accounting assumptions and verification limits.

### Evener

Evener discovery reads semantic transcript **format v2** under
`$XDG_STATE_HOME/evener/projects/<project-id>/sessions/`, falling back to
`~/.local/state/evener/projects/`. You can override the state root with
`EVENER_DIR` or `agents.evener.dirs`; an explicit project state directory or
sessions directory works too. Older transcript versions are not supported, and
existing archive data is not deleted.

```toml
[agents.evener]
dirs = ["~/session-sources/evener"]
```

The provider reads messages, thinking, tools, recorded usage, session names, and
fork/subagent relationships, and metadata-only edits refresh the session.
Following the Codex provider policy, AgentsView omits verified copied fork
prefixes from child sessions. If the parent is missing or cannot be verified,
child history is kept and shared usage may appear in both sessions.

Costs reflect recorded conversation usage rather than provider invoices. Model
switch records with structured identities update the model context, while older
prose-only switches cannot establish a fallback billing model. Explicit
per-response model identities still take precedence. Media that the transcript
view cannot display appears as a descriptive placeholder without fetching
referenced files or URLs.

Remote sync uses AgentsView's HTTP transport; this provider does not connect to
Evener hubs or add an S3 transport. Remote sync skips files deleted after
discovery, including metadata left behind when its transcript is deleted.

### Codex Cloud

The `codex-cloud` source imports the chats that ChatGPT dot, Codex web, and
Codex cloud tasks keep on OpenAI's servers. Because those chats never exist as
local files, AgentsView fetches them using the ChatGPT login of a local Codex
home and stores a compressed copy under `<data dir>/sources/codex-cloud/`, with
each chat becoming a session.

This source is off by default and configured only in `config.toml`; the
**Settings** page does not show it.

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

Because live capture is off by default, `enabled = true` only imports chats. Set
`live_tokens = true` if you also want AgentsView to listen in on your running
chats. Read [Tokens and plan usage](#tokens-and-plan-usage) first, because
listening in can change a chat on OpenAI's side.

Any endpoint override must use `wss` or `https` on `chatgpt.com`, `openai.com`,
or a subdomain of either; plain `ws` and `http` work only for loopback
addresses. Neither client follows redirects, so your login is never sent to a
host these rules did not check.

An unknown key or bad value turns this source off while leaving the rest of your
config working. The error names `config.toml`, the key, and the problem, and
appears in the status bar and in `agentsview serve status`.

Adding `codex-cloud` to `disabled_agents` stops fetching, live capture, and
parsing even when `enabled = true`. Because the daemon re-reads
`[agents.codex-cloud]`, `disabled_agents`, and `[agents.codex] homes` at the
start of each run, a hand edit turns fetching and live capture on or off without
a restart. Parsing of already-fetched chats follows the daemon's provider list
instead, so adding `codex-cloud` to `disabled_agents` by hand stops parsing only
after a restart or a save on the **Settings** page. If `config.toml` cannot be
read at all, the source keeps its last good settings and the status reports
that.

#### What it does with your login

AgentsView reads your ChatGPT access token from the `auth.json` of each Codex
home: `CODEX_HOME` (or `~/.codex` when unset) and each entry in
`[agents.codex] homes`. It sends the token only to the endpoints above, never
writes to a Codex home, and never stores the token. Homes signed in to the same
ChatGPT account share one import.

- If OpenAI rejects a home's login or asks for a fresh one, the status tells you
  to run `codex login` for that home. AgentsView tries the home again when its
  `auth.json` changes, or after one hour.
- If OpenAI refuses the account's requests, check that the ChatGPT account can
  use Codex. AgentsView waits one hour, then doubles the wait up to one day. A
  refusal does not mean the login is wrong, so the status does not suggest
  `codex login`.
- Codex can store the login in the system keyring instead of `auth.json`, or not
  save it at all, and AgentsView does not read the keyring. To let AgentsView
  use such a home, set `cli_auth_credentials_store = "file"` in that home's
  `config.toml` (update the existing line or add one near the top before any
  `[section]`), keeping only one line for that key because Codex cannot load a
  `config.toml` that sets it twice. Then run `codex login`. Codex then stores
  the ChatGPT login in `auth.json`, a plain file that any program running as you
  can read. With the default `auto` store, a refused login can mean either an
  expired login in `auth.json` or a current login in the keyring; the status
  names both and suggests `codex login` first.

#### Private endpoints

OpenAI does not document these endpoints, and they may change or stop working
without notice. OpenAI may also treat third-party clients differently from its
own apps. If a reply no longer matches what AgentsView expects, the status says
"OpenAI's service answered in a way this version of AgentsView can't read" and
asks you to update AgentsView or check any `endpoint` or `usage_endpoint`
override. What stops depends on which request failed:

- An unreadable reply from the chat service during an import pauses imports for
  that account. AgentsView retries after one hour, then backs off up to one day.
- An unreadable reply while looking for running chats, connecting to one, or
  listening in on one stops live capture for that account while imports
  continue. To turn live capture back on, restart AgentsView, or set
  `live_tokens = false`, wait until the status says "Live tokens: off", and then
  set it back to `true`. A quick toggle can be missed because AgentsView checks
  the setting every 30 seconds.
- An unreadable plan usage reply is retried like any other failed plan usage
  request.

Sessions you have already imported stay in your archive.

#### Tokens and plan usage

Hosted chat history carries no token counts; OpenAI reports token counts only to
a client connected to a chat while it runs. With `live_tokens = true`, the
daemon checks every 30 seconds for chats updated in the last 5 minutes whose
newest turn is still running. It listens in on each one, up to 10 per account,
records every token update, and disconnects when the turn ends or after 6 hours.

Listening in is the only request that touches a chat on OpenAI's side.
AgentsView connects the same way Codex reopens a chat, using OpenAI's
`thread/resume` request:

- While AgentsView is connected, the running task reports AgentsView as its
  client, and later turns' starting context can mention it.
- If a turn ends right before AgentsView connects, connecting loads the stopped
  chat on OpenAI's side and starts its MCP servers (the tool servers the chat is
  set up to use). In testing, nothing ran and the chat did not change, though
  the chat may move to the top of your chat list in OpenAI's apps. AgentsView
  disconnects right away and does not connect for that turn again. To make this
  less likely, AgentsView checks again right before connecting that the chat was
  updated in the last 5 minutes.
- In Codex, connecting to a chat whose turn has ended runs the chat's idle step,
  which continues an active goal and sends any queued messages, so it can start
  a turn you did not ask for. AgentsView checks the chat's goal first and skips
  chats with an active goal. It cannot check for queued messages: if a turn ends
  right before AgentsView connects while you have messages queued (for example,
  after you stopped the turn), connecting can send them.
- **Not tested.** AgentsView never answers approval prompts. If a chat asks for
  approval while AgentsView is connected, it is unknown whether your app still
  shows that prompt after AgentsView disconnects.

To stop listening in, remove `live_tokens = true` or set it to `false`; fetching
and plan usage continue. The status bar and `agentsview serve status` show a
live tokens line for each account, reporting how many running chats AgentsView
is listening in on, whether it is listening in on none right now, whether live
tokens are off and why, or whether live tokens stopped after an unreadable
reply.

An automatically started daemon normally exits once it has been idle for
`daemon_idle_timeout`. While AgentsView is listening in on a running chat, the
daemon stays up past that timeout for at most 6 hours per chat. A daemon that is
shutting down does not start listening in on new chats.

Because each token update carries the chat's running total, an update also
covers every turn that finished before it, back to the last time OpenAI
restarted that total. A finished turn counts as captured when AgentsView
received a token update for it and saw it end on the same connection, or when a
token update from a later turn covers it. Any other finished turn, such as a
chat's last turn that ran while AgentsView was not connected, may be missing
some or all of its tokens. A session with such turns and with recorded token
counts stays in the token and cost totals, which then run low for that session.

A chat that AgentsView never listened in on, or listened in on without receiving
a token update, has no token counts. For those chats, AgentsView asks OpenAI's
usage report for the chat's share of your plan's weekly and five-hour limits and
the credits figure the report includes, then shows that plan usage in the
session details. Those sessions stay out of token and cost totals, and
`agentsview report usage` reports how many were left out. That count includes
chats AgentsView has not asked about yet, such as one waiting while plan usage
is paused; its session details say "Plan usage: not checked yet" until the first
answer arrives.

AgentsView retries plan usage after a failed request or incomplete report,
waiting longer each time up to 6 hours between requests. It refreshes complete
figures once a day and stops refreshing 7 days after the chat's last change,
keeping the last complete figures.

For a chat still waiting for complete figures, AgentsView stops after the first
answer that arrives 7 days or more after the chat's last change. If that answer
includes the chat, AgentsView keeps what it reports, even partial figures or
none. If it omits the chat, AgentsView keeps any figures it stored earlier, or
shows none if it never got any. If 3 requests in a row fail after those 7 days,
AgentsView stops asking and the session details say it could not check plan
usage.

Some problems pause plan usage for the whole account instead: OpenAI rate
limiting, OpenAI reporting that the account has no usage report, or no Codex
home having a usable login. No requests go out while paused, so a chat can wait
more than 7 days after its last change; the first answer after the pause ends is
the last one AgentsView takes for that chat, under the same rules.

#### Limits

- Live capture listens in on a chat only within 5 minutes of the chat's last
  update. A turn that ends before AgentsView connects gets no live tokens, and
  if the daemon is down (or all 10 connections for the account are busy) for
  longer than that while a turn runs, that turn is not captured live either.
- Those 5 minutes compare OpenAI's update time with your computer's clock. A
  clock running more than a few minutes fast makes running chats look old, so
  AgentsView skips them; a clock running slow makes stopped chats look recent,
  so AgentsView connects to more stopped chats.
- A chat's first token record is dated when AgentsView saw it, even when the
  earlier turns it covers ran on an earlier day.
- Live capture misses two kinds of model change. After a change reported only as
  a chat settings update, tokens count toward the earlier model until AgentsView
  next connects. After a reroute that happened before AgentsView connected,
  tokens count toward the chat's configured model.
- When one token update covers several model calls, AgentsView prices it at base
  rates, which can understate the cost of long-context calls inside it.
- If OpenAI's running counter restarts in a way AgentsView does not see as a
  decrease, the turns in between are undercounted.
- AgentsView does not listen in on running sub-agent chats yet, so their tokens
  are not captured. Dot-delegated tasks are not sub-agent chats in this sense,
  and AgentsView listens in on them like any other chat.
- If a chat fails to import for 7 days without changing, AgentsView stops
  retrying it even while OpenAI lists it as active, and logs it in `debug.log`
  inside the data directory. AgentsView tries it again if it changes.
- ChatGPT dot's own conversation has no turns and is never imported; its
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

To exclude session providers you do not use, list their IDs in
`disabled_agents`:

```toml
disabled_agents = ["gemini"]
```

Because Freebuff shares the Codebuff provider, listing `"codebuff"` turns off
local filesystem ingestion for both Codebuff and Freebuff.

This setting applies only to local filesystem discovery, targeted local file
sync, file watching, and scheduled polling. It does not affect HTTP remote
imports, and it does not restrict HTTP, PostgreSQL, ClickHouse, or archive
exports. `RemoteSyncExcluded` is the separate provider capability that keeps
unsafe source trees out of remote exports.

Changes saved on the **Settings** page apply to the running daemon right away,
switching file watching and polling to the new provider set. Sessions already on
disk for a newly enabled provider arrive on the next sync. If you edit
`config.toml` directly, the daemon picks up the change the next time it starts.
Sessions already archived from a disabled provider stay browsable and
exportable, even after an archive rebuild. Disabling a provider here does not
disable it as a Recall execution backend.

Daemon-backed `pg push --watch` and `clickhouse push --watch` processes follow
committed archive changes from the daemon. Because only the daemon discovers and
watches source roots, you do not need to restart those push watchers after
changing providers. Startup, lifecycle, periodic catch-up, and shutdown pushes
still sync sources through the archive owner. If a push watcher owns the archive
without a daemon, restart that process after changing this setting.

### Multiple Directories

To scan more than one directory per agent, for example when running Windows and
WSL side by side, add array fields to `~/.agentsview/config.toml`:

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
`dirs` array, named by its provider ID such as `claude`, `codex`, `pi`, or
`gemini`. Environment variables take precedence over `dirs`. A non-empty array
replaces the default paths, and an explicit empty array clears the default local
roots.

On normal startup, AgentsView converts older top-level directory and home keys
to these tables and saves the result, while read-only commands read the old
format without rewriting the file. If both forms set the same field, startup
reports the conflict and leaves the file untouched. Remove one of the entries.
**Settings** writes only the new format. Conversion keeps unrelated settings
intact, though it rewrites TOML formatting and comments.

AgentsView discovers, watches, and syncs each listed directory independently.

Pi also honors its native `PI_CODING_AGENT_DIR` and
`PI_CODING_AGENT_SESSION_DIR` variables during local discovery: the agent home
variable shifts the default to `<agent-home>/sessions`, and the session variable
points directly at a session directory. `PI_DIR` takes precedence over
`PI_CODING_AGENT_SESSION_DIR`, which takes precedence over `agents.pi.dirs` in
`config.toml`. A configured `agents.pi.dirs` array replaces the home-derived
default, and an empty array clears it. With no overrides, Pi uses
`~/.pi/agent/sessions`.

### Alternate Agent Homes

Claude Code, Codex, Junie, and Pi support alternate homes. Each one can hold a
separate account or settings profile. List the home directories themselves in
`homes`, and AgentsView derives their native session directories:

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

Homes add to defaults, environment overrides, the same table's `dirs`, and
`[[session_sources]]`. To scan only the listed homes, set `dirs = []` and leave
directory environment overrides and `session_sources` entries unset. Homes must
be local directories; use `dirs` for `s3://` roots on providers that support S3.
Sessions from each home appear under their native provider. For Pi, `homes`
values match `PI_CODING_AGENT_DIR`, not its parent `.pi` directory or a direct
session directory.

Homes added or removed on the **Settings** page apply to the running daemon
right away, and sessions already in an added home arrive on the next sync. If
you edit `homes` directly in `config.toml`, the daemon reads the change the next
time it starts.

Other providers support multiple explicit `dirs` but do not yet accept `homes`.
A home mapping must match the provider's native layout before it can be enabled.

When configuration loads, local session roots become absolute paths with
symbolic links resolved, and duplicate roots are removed before scanning or
watching starts. Links to directories that do not exist yet keep their resolved
destination so creating the directory later does not register a second scan
root. Each home's metadata paths stay separate from the shared transcript root.

The **Session Providers** section of the **Settings** page edits the same lists.
Adding or removing a home there updates `homes` in that provider's
`[agents.<id>]` table in `config.toml`. Provider-home changes take effect after
the AgentsView daemon restarts, and a push watcher that owns the archive without
a daemon must also restart.

![Codex alternate homes in Session Providers settings](/docs/assets/generated/screenshots/settings-agent-homes.png)

#### Choosing a layout

There are two ways to run a second home, depending on whether you want to share
transcripts between them.

**Fully separate homes.** Each home owns its own transcripts. Use this when two
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
`config.toml`, skills, and per-instance metadata, while its session directories
link symbolically into the primary home. Use this when you want different
settings or skill profiles with one searchable history. This is the recommended
layout for profile switching because nothing is copied and no session exists in
two places:

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
happened in. Leave that file unlinked; AgentsView reads every home's copy. HTTP
remote sync also transfers these indexes and keeps their associations with the
shared transcripts, so imported sessions keep titles from alternate homes. If an
index is removed or loses an entry, the next sync uses a title from the
remaining configured indexes, or keeps the session's last known title if none
names it. Upgrade both ends of HTTP sync together because older protocol
versions are rejected.

The same setup works for Claude Code if you link `<alt>/projects` to
`~/.claude/projects`. Because Claude keeps no title index, there is nothing else
to leave unlinked for AgentsView.

#### How links and duplicates are handled

- Roots that resolve to the same directory, including through symbolic links,
    are scanned once. Configuration loading stores the resolved absolute root
    and its provider metadata paths, and a matching `[[session_sources]]` entry
    still supplies the machine key.
- Metadata belongs to the configured transcript root. If two homes share
    `sessions/` but keep separate `archived_sessions/`, their shared sessions
    read both homes' metadata, while each archive reads only its own home's
    metadata.
- Codex sidecars come from the effective root's home and every alias home.
    Activity hints come from each distinct `history.jsonl`, with linked copies
    read once. Thread titles combine every `session_index.jsonl`, and when two
    homes name the same session, the most recently written index wins.
- Changing a title in an alias home shows up live: AgentsView watches each
    home's directory for `session_index.jsonl` changes and maps them back to the
    shared transcripts.
- A home whose link target does not exist yet stays under its configured path
    and starts working once the target appears.

### Machine-Labeled Filesystem Sources

Use `[[session_sources]]` when a root comes from another machine and has been
copied or mounted onto this AgentsView host:

```toml
[[session_sources]]
agent = "copilot"
dir = "/srv/session-archive/buildbox/copilot"
machine = "0123456789abcdef0123456789abcdef" # Peer installation ID
```

Each entry takes `agent`, `dir`, and an optional `machine`, and adds to the
per-agent arrays, defaults, and environment variables above. AgentsView
deduplicates equivalent roots, using the structured entry's `machine` key when
it duplicates a shorthand root. For a remote root, pass the peer's ID from
`telemetry-install-id`. For a local root, omit `machine` to use this
installation's ID, and remove `machine` from any existing local source that sets
it to a hostname. Source keys are used literally, so filter aliases and display
labels do not make a source local.

AgentsView records machine attribution when it first ingests each session.
Changing an entry's `machine` value affects newly discovered sessions without
relabeling existing ones during ordinary syncs or `agentsview source sync --full`.
To adopt old keys that belong to this installation, use
[`agentsview db adopt-machine`](/docs/commands/#agentsview-db-adopt-machine).

See [Filesystem Session Sync](/docs/filesystem-sync/) for multi-machine
examples, transport safety, ID deduplication, watcher behavior, and how this
compares with PostgreSQL.

### S3-Compatible Session Sources

Claude, Codex, Cursor, and IcodeMate session roots can also be `s3://` URIs.
These are the providers whose current formats can be discovered and parsed as
individual objects, and future providers opt in through the same capability. Use
this when several machines push their raw session files to object storage and
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

S3 sources are read-only inputs to normal local sync. AgentsView lists matching
objects, downloads each changed object to a temporary file, parses it with the
existing per-agent parser, stores the original `s3://` URI as `file_path`, and
deletes the temporary file without keeping a persistent local mirror.

Codex forks and subagents also need their parent's rollouts to filter replayed
turns. Each sync downloads a parent once and shares it across every child it
processes; if a parent object changes mid-sync, later children get the new
version. The sync keeps roughly 512 MiB of recent parent rollouts in a temporary
directory and removes that directory when it finishes.

Configure credentials and endpoints with standard AWS-style environment
variables:

```bash
export AWS_ACCESS_KEY_ID=...
export AWS_SECRET_ACCESS_KEY=...
export AWS_REGION=us-east-1
export AWS_S3_ENDPOINT=https://s3.amazonaws.com
```

`AWS_S3_ENDPOINT` is optional for AWS S3; set it for S3-compatible services such
as MinIO, Aliyun OSS, or Cloudflare R2. AgentsView accepts `http://` endpoints
only for loopback hosts such as `localhost` or `127.0.0.1`. For a non-loopback
HTTP endpoint, set `AGENTSVIEW_ALLOW_INSECURE_S3_ENDPOINT=true`, and use that
override only on trusted private networks because session transcripts travel
without TLS.

Expected object layouts:

```text
s3://bucket/.../<machine>/raw/claude/<project>/<uuid>.jsonl
s3://bucket/.../<machine>/raw/claude/<project>/subagents/.../agent-*.jsonl
s3://bucket/.../<machine>/raw/codex/2026/06/24/rollout-*.jsonl
s3://bucket/.../<machine>/raw/cursor/<project>/<uuid>.jsonl
s3://bucket/.../<machine>/raw/icodemate/<project>/<session>.jsonl
```

AgentsView takes the machine name from the path segment right before `raw`, or
falls back to the local AgentsView machine label if no such segment exists.
Codex discovery imports only rollout files under the configured root plus a
trailing slash, so sibling prefixes such as `raw/codex-backup` are ignored.

AgentsView saves each S3 object's `Size`, `LastModified`, and available
fingerprints (`ETag`, version ID, and checksum headers) in the session row for
unchanged-object skip checks. A later sync lists object metadata first and
downloads only objects whose source metadata changed or whose stored parser data
is stale.

AgentsView does not watch S3 roots with fsnotify; initial sync, manual sync, and
the periodic directory scan pick them up instead.

### Worktree Project Mappings

The parser infers each session's project from its `cwd`. It recognizes common
worktree manager layouts, including the generic
`worktrees/github.com/<owner>/<repository>/<worktree>` convention, where the repository
segment becomes the project. Unrecognized layouts, such as
`~/code/{project}.worktrees/feat/<branch>/`, otherwise group sessions under
`<branch>` instead of `{project}`. For those layouts, add manual **path-prefix →
project** rules in the **Rules** view on the [Data page](/docs/data/#rules), or
let the [mapping editor](/docs/data/#create-a-project-mapping) build one from a
project's observed session folders:

![Worktree mapping rules on the Data page](/docs/assets/generated/screenshots/worktree-mappings.png)

- Mappings are explicit; AgentsView does not auto-discover them.
- Each rule applies to one machine. Use the machine selector to manage rules for
    the local machine and any remotely synced machine. Rules live in the
    writable archive that ingests that machine's sessions, which may be the
    source machine's local SQLite archive or a separate collector archive.
- A rule matches whenever a session's `cwd` falls under the configured prefix,
    both for new sessions as they sync and (when you click **Apply**) for
    already-imported sessions. Prefixes match on directory boundaries, so
    `/worktrees/service` does not match `/worktrees/service-old`.
- Enabled mappings run after parser inference, so an explicit rule always wins
    when the two disagree.
- The default `explicit` layout maps every matching path to the project name on
    the rule. The `repo_dot_worktrees` layout takes the project from the first
    path segment under the prefix when it is named `<repo>.worktrees`, so a path
    such as `/code/agentsview.worktrees/feature/frontend` resolves to project
    `agentsview`.
- Rules created from the **Data** mapping editor record the mislabeled project
    they replaced as the rule's **original label**. That value is informational
    and set once; to revert a reclassification manually, edit the rule's target
    back to that original label and apply again.
- Disabling or deleting a rule does not rewrite sessions on its own. Sessions
    whose source files still exist return to parser-derived names on a later
    reparse or full resync, while orphaned sessions keep their stored
    classification.
- Excluded, trashed, and skipped session files are left alone.

Mappings change only the session's `project` field, and the rest of the session
record stays intact through the bulk-resync rebuild-and-copy path.

## Automated Session Detection

Roborev-tagged runs count as automated code reviews, including runs with
multiple user messages. Other sessions count as automated when they have one or
fewer real user messages and their first user message matches the automation
classifier. By default, automated sessions such as roborev reviews, title
generation, warmup pings, and changelog generation stay hidden from session
lists, counts, and analytics. Turn on **Include automated** in the session
filter dropdown to show them. See
[Activity](https://agentsview.io/docs/activity/) for headless worker
classification.

Built-in patterns cover the roborev family and AgentsView's own internal
prompts. To teach AgentsView about first-message patterns from your own
automation, add them to `~/.agentsview/config.toml`:

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

User-configured entries are case-sensitive and match against the session's first
user message:

| Key             | Match behavior                                               |
| --------------- | ------------------------------------------------------------ |
| `prefixes`      | `HasPrefix` against the first user message                   |
| `substrings`    | `Contains` anywhere in the first user message                |
| `exact_matches` | trims the first user message, then compares the whole string |

AgentsView trims and deduplicates entries, and drops any entry longer than 1024
bytes. Entries that duplicate a built-in pattern in the same category are
silently dropped.

**Reclassification on config change.** AgentsView stores a hash of the active
classifier (built-in patterns plus your configured patterns) in the database.
For archives that keep transcript text, startup rechecks changed and imported
sessions. When the classifier changes, startup rechecks every session so edits
to `[automated]` apply to your history right away without a manual resync. The
same backfill corrects rows pulled in from PostgreSQL sync or copied from other
archives.

## Database

The SQLite database uses WAL mode for concurrent reads and includes FTS5
full-text search indexes on message content.

### CJK full-text search

To add character and phrase matching for Chinese, Japanese, and Korean text,
including Chinese word segmentation, build and install the pinned
`simple`/cppjieba sidecar by running `make install-cjk-fts`. Building it needs
Git, CMake 3.19 or newer, and a C++14 compiler. AgentsView looks for the sidecar
next to the binary or under the sibling `lib/agentsview/simple` directory; to
use another path, set `AGENTSVIEW_SIMPLE_DIR`.

The sidecar indexes individual CJK characters and routes queries containing Han,
Hiragana, Katakana, or Hangul through that index. Query preparation depends on
the scripts in your query:

- Queries with Japanese kana or Korean Hangul preserve character order and
    adjacency within each whitespace-separated search term. For example, `かな`
    does not match `なか`, and `검색` does not match separate occurrences of `검`
    and `색`, while separate terms can match anywhere in the same message.
- Queries with Han and no kana or Hangul use Chinese word segmentation through
    cppjieba. Japanese queries written entirely in kanji follow this same path
    because the scripts alone do not distinguish the languages; quote a kanji
    phrase such as `"検索方法"` to require its characters in order.
- A leading double quote switches to explicit FTS5 expressions, including
    phrases and operators, in any language. For example, `"검색 기능"` requires
    the two terms together, while `검색 기능` allows text between them.

Japanese and Korean matching is character-based: it does not analyze grammatical
word forms or expand readings, romanizations, or spelling variants. Chinese word
segmentation also stays off when a query mixes Han with kana or Hangul.

ASCII-only searches keep using the existing Porter index, so searches such as
`run` still use English stemming. Because `messages_cjk_fts` is derived data,
removing the sidecar makes AgentsView drop that optional index and fall back to
the standard FTS5 path, and reinstalling it backfills the index on the next
writable open. AgentsView fingerprints the native library and all cppjieba
dictionaries and rebuilds the index atomically whenever that fingerprint
changes; writers running with a different fingerprint leave a freshness marker
instead of mixing incompatible token streams. Pinyin expansion stays off in the
derived index because ASCII-only queries continue to use the Porter index.

CJK full-text search applies only to SQLite message search, including the HTTP,
CLI, and MCP search paths. PostgreSQL/CockroachDB and ClickHouse do not load
this SQLite extension and keep their existing search behavior, and substring and
regular-expression searches are unchanged. Session search result snippets
highlight the segmented matches, while highlighting inside an opened transcript
uses the original query and may miss separated Chinese words.

The first backfill, a changed fingerprint, or any pending session triggers a
full index rebuild after the daemon starts listening. Until it finishes, CJK
full-text searches return the "full-text search is unavailable" error. They
work again after the repair finishes.
If the substring index is missing, substring searches scan messages until the
repair finishes. Sessions shows the saved archive while repair runs. Archive
writes, including startup sync, wait while the repair holds the writer. During
the message usage counter upgrade, Usage requests wait for the fill; read-only
Usage commands ask you to retry after the daemon finishes.
Because the freshness ledger records session IDs rather than old message IDs and
token content, it cannot remove stale entries for individual replaced or deleted
messages. Removing the sidecar drops the CJK index but keeps the
`messages_cjk_fts_pending_sessions` ledger and three persistent session
triggers; the ledger holds at most one row per touched session ID until the next
successful CJK index rebuild clears it.

Index maintenance relies on TEMP triggers on the writer connection. Writes made
without those triggers or with another sidecar fingerprint leave the index
stale, causing CJK search to fall back to standard FTS5 and log a warning once
per database handle. Reopening the archive with the sidecar restores the index
and its triggers.

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

AgentsView migrates the database automatically on startup when the schema
changes. When a parser upgrade affects only some agents and the archive syncs
source files, AgentsView reparses those agents' sessions in place and retries
failed sources on later syncs. Other stale data versions trigger a full resync
into a fresh temporary database, which copies preserved and orphaned session
data from the previous database before swapping atomically. If a full resync
aborts, AgentsView falls back to an incremental sync and leaves the data-version
marker stale. The running daemon retries the rebuild after an hour, backing off
to daily, and a restart also retries.

## History from a saved archive

You can browse and search a retired machine's history alongside your current
local sessions. The original Claude and Codex files stay in their Docbank
storage, and you can attach either a backup snapshot or an existing archive
directory without keeping a permanent extracted transcript directory.

Copy an AgentsView archive backup repository to a local disk or mounted share,
then add its path and the exact snapshot ID from the backup report to
`config.toml`:

```toml
[[archive_sources]]
name = "retired-desktop"
repository = "/mnt/archives/desktop/backup-repo"
snapshot = "<snapshot ID>"
```

If you already have an archive directory, set `directory` instead of
`repository` and `snapshot`:

```toml
[[archive_sources]]
name = "saved-history"
directory = "/mnt/archives/restored"
machine_name = "Previous workstation" # Optional; defaults to the captured name.
```

That directory can be an original AgentsView archive, the output of
`agentsview archive restore`, or a Docbank-restored vault that still has its
`application/sessions.db`. A vault with only blobs is not enough because the
application database records accepted sources and their machine identities. Stop
the source archive's AgentsView or Docbank process before attaching it. The
directory must be writable for Docbank's catalog, locks, and maintenance, and
AgentsView holds it exclusively during each import pass (opening it may apply
Docbank catalog upgrades). AgentsView opens the source `sessions.db` read-only
and writes parsed results to the receiving installation.

After editing `config.toml`, restart any running daemon and run
`agentsview source sync`. The first sync reads the supported sources and reports
pending work, captured deletion rules, and supplemental files. Sessions keep the
original machine's identity and label; `name` only labels the configuration
entry, so changing it does not change session ownership.

A daemon started with `--no-sync` still runs manual `agentsview source sync` and
`agentsview source sync --full`, while automatic ingestion stays off.

Pushes to PostgreSQL and ClickHouse also read pending attached history after a
successful local sync, including on a `--no-sync` daemon. A push watch that owns
the local archive reads attachments at startup and on each periodic sync, and
its `--full` option reparses available attachments.

If an attachment is unavailable during a push from a current SQLite archive,
AgentsView produces a warning while stored history and new local sessions still
reach the destination, and retries unfinished attachment work on a later sync.
Changes committed before an attachment error also queue a push in watch mode.

To change the display name of a single-machine archive, set the optional
`machine_name`, restart the daemon, and sync (no reparse is needed). Omit
`machine_name` for an archive with several machines so each keeps its captured
name; removing it restores the captured name. The underlying installation IDs
come from the archive automatically and stay the same when you rename or move
it.

Later syncs skip transcripts whose parser and content-policy versions are
current. The snapshot stays pinned until you change the configuration; a later
snapshot with a different version of an already accepted source is treated as a
conflict, because this feature does not advance captures or follow the newest
backup.

### Space, reconnecting, and reparsing

On its first sync, a backup source temporarily copies the snapshot's application
database to build a small source index, reporting the required bytes before
copying and removing the copy afterward. A directory source reads acceptance
records directly from its application database on each pass without copying that
database or its transcripts. Each parse also needs temporary space for one
source and its companion files (up to 16 GiB) plus parser staging. Parsed
sessions and retained images go into the receiving installation's normal
database and `assets/` directory.

If you disconnect the archive, already-indexed history stays browsable; sync
reports the unavailable source and retries on a later pass. When a source fails
to copy or parse, the next scheduled sync retries it. If that retry also fails,
scheduled syncs and pushes wait an hour (doubling up to a day) while reporting
the recorded error. A manual `agentsview source sync` retries right away, and
restarting the daemon clears the wait. To move a backup archive, update
`repository` and restart the daemon: a copied repository with the same
repository ID and pinned snapshot continues the same history, while a different
repository at that path is refused. For a directory source, update `directory`;
the archive identity and accepted source records stay independent of its path.

Parser upgrades reparse available sources. To force a full reparse, run
`agentsview source sync --full`; individual session refreshes direct attached
history to that command. Local ingestion runs first and gets a turn between
archive sources, though it can still wait for one large source to finish parsing
because 16 GiB is a size limit, not a time limit.

During a full rebuild, the old database stays readable while writes pause with a
retryable HTTP 503, and available archives extend that pause. Offline or removed
sources keep their indexed history and unfinished parser work. If a backup
starts between sources, the rebuild waits with progress and resumes afterward,
and cancelling leaves the original database intact. Ordinary sync defers a busy
repository to a later pass. A backup blocked by an active reader can fail after
60 seconds and need a retry.

### What is included

- Accepted Claude and Codex sources become browsable. Supplemental originals
  from other providers stay in the repository, though this feature does not
  parse them.
- The receiver's content and image policies apply, and its stars, names, pins,
  and deletions survive reparsing. Curation from the saved database is not
  merged.
- Capture-time deletion rules are honored. Sessions deleted in the saved archive
  *after* capture can reappear because those later deletions are not imported.
- History already received through artifacts or HTTP/SSH sync can appear again
  under a different identity. Matching native session IDs does not prove that
  two machines hold the same history. Exact-ID ownership conflicts are refused.
- Attachments belong in an ordinary installation, not an archive-only one. You
  cannot attach the receiving machine's own identity or aliases as foreign
  history, and AgentsView never adopts credentials or installation identity from
  the source.
- Both storage forms use the same source identities, content policies, and
  parser. Directory sources can pick up additional accepted sources on a later
  pass, while changing an already accepted source remains a conflict. Concurrent
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

AgentsView keeps the database in sync with session files in three ways:

1. **File watcher.** AgentsView uses FSEvents on macOS and fsnotify elsewhere to
    detect file changes. Isolated edits batch for 500ms; watcher-driven sync
    start times stay at least ten minutes apart while nobody is watching the
    archive, and five seconds apart while the web UI is open (see
    [`background_sync_interval`](#config-file)). Common dependency and build
    folders (`node_modules`, `__pycache__`, `.git`, `vendor`, `.venv`, and so
    on) are skipped automatically to reduce noise and overhead. On macOS,
    FSEvents reports a file that its producer keeps open only when the file is
    created and closed, not as it grows.
1. **Scheduled reconciliation.** Every 15 minutes, AgentsView runs a scoped scan
    of agents whose watch coverage is known to be partial, such as shallow
    directory watches or database change cursors. Other agents rely on the
    watcher, the fallback polling described under
    [Large Watch Trees](#large-watch-trees), and the daily archive audit.
1. **Codex live-activity polling.** Every 30 seconds, the daemon checks file
    metadata for a bounded set of recently active rollouts and syncs any that
    changed. A rollout counts as recently active when `history.jsonl` names it
    or when its stored session ended within the last 24 hours; the second check
    covers producers such as Codex Desktop that write no `history.jsonl` and
    keep the active rollout open. The daemon checks at most 256 of these stored
    sessions per poll, newest first, from any configured Codex root (including a
    `[[session_sources]]` root labeled with another machine's key, such as a
    shared mount). This is a freshness backstop for already-indexed sessions
    rather than a session source: normal discovery and sync still own ingestion,
    deletion, and canonical-path selection.

Recent-session polling has two known gaps on macOS. In both, Codex Desktop
writes no `history.jsonl` hint, macOS does not report appends to the open file,
and AgentsView picks up new activity only when Codex closes the session file:

- You resume a Codex Desktop session that has been idle for more than 24 hours.
  Its stored end time falls outside the 24-hour window.
- The session is stored under a machine label that is no longer in your
  configuration. Because a session keeps the label it was first stored under,
  this happens after you change a `[[session_sources]]` root's `machine` value,
  or for an old hostname label you have not adopted with
  [`db adopt-machine`](/docs/commands/#agentsview-db-adopt-machine).

Change detection tracks file size, mtime, inode, and device to validate
incremental parses more reliably, and a pool of 8 workers processes files in parallel
during sync.

Shared JSONL discovery can isolate an inaccessible file or subtree when the
provider supports scoped traversal. A dangling symlink
stays incomplete, while retries inspect only the unfinished scope; archived
sessions beneath that scope stay available, and unrelated session changes keep
importing. Restoring access lets the retained retry finish without a daemon
restart. Actual source removal still follows the normal source-missing policy.
Opaque failures and passes with more than 256 reported failure scopes keep the
original conservative retry scope.

Codex history hints work when the producing frontend writes `history.jsonl`.
Setting `[history] persistence = "none"` in Codex's configuration disables those
writes, though sessions whose stored activity is recent still get live-activity
polling. AgentsView reads only session identity and timestamp metadata from
accepted hint records and never stores or logs submitted prompt text.

For each configured local Codex session root, AgentsView checks `history.jsonl`
in the cleaned root's parent (for example, `/data/custom/sessions` checks
`/data/custom/history.jsonl`). It does not search ancestors or the rollout
archive for another history file, and missing hint files stay cheap probes.

On startup, the initial daemon poll bootstraps at most the newest 4 MiB of each
history file and accepts records at most 24 hours old. If AgentsView restarts
during a long autonomous run whose last persisted prompt is outside either
bound, that rollout stays hot as long as its stored session ended within the
last 24 hours.

For `s3://` Claude, Codex, and Cursor roots, change detection checks object
size, `LastModified`, and available object fingerprints such as ETag, version
ID, and checksums from listing or stat calls, downloading object content only
when that metadata shows a parse may be needed.

Files that produce no session, such as ones with no interactive content, are
cached in `skipped_files`. Source files that are missing or have malformed
content are cached in `source_failures`, even when the sync pass that found them
did not complete. An unchanged cached failure counts as a skip so watcher
retries can finish. Failure identities include the provider's pre-parse
fingerprint (including content hashes and companion files where available), and
SQLite schema failures also track database and WAL changes so repairing a schema
retries the source even if its session timestamps stay the same. Missing files
are retried when they appear. A full resync retries cached failures so a parser
upgrade can reach a source that never produced a session, and if that rebuild is
discarded, incremental catch-up still retries them. Transient failures that can
clear up on their own, such as timeouts or permission errors, are not cached and
retry on every pass.

Sync summaries include a `Parser anomalies (this run)` section whenever the run
sees parser or sanitizer anomalies. It can report malformed-line counts,
unrecognized Antigravity schema sessions, sanitized-field counts, and
Antigravity `gen_metadata without usage` counts (meaning Antigravity supplied
generation metadata for one or more records, but AgentsView could not derive
normalized usage totals from them during that sync).

During full parser rebuilds, AgentsView keeps serving the retained archive while
the dashboard and CLI report stale results. Scoped upgrades and schema repairs
finish before HTTP readiness: their startup output prints phase and completion
lines, and `agentsview daemon status` shows the starting PID, elapsed time,
current phase, progress detail, and log path while the daemon holds the start
lock.

### Friction Log

The Friction Log records moments where a session went wrong: corrections, failed
commands, workarounds, deferrals, repeated patterns, frustration, and
interruptions. Review is off by default because it reads every changed
transcript a second time. To see how to read a digest, visit
[Friction Log](/docs/quality/#friction-log).

```toml
[friction]
enabled = true
backfill_days = 7
timezone = "" # Local time on SQLite, UTC on PostgreSQL; or an IANA zone.
seat_patterns = [] # Optional path globs with one whole-segment {seat} capture.

[friction.kata]
auto_file = false # The hub files recent patterns to Kata every hour and reopens recurring ones.
kinds = [] # Kinds to file automatically; empty means all that can be filed.
min_sessions = 2 # File only patterns seen in at least this many sessions.
```

Restart the daemon after changing this setting. When enabled:

- Sync stays as fast as with review off: sync marks changed sessions for review,
  and the daemon reviews them in the background afterward.
- Turning review on for an existing archive reviews every session in the
  background while sync keeps running.
- A PostgreSQL push reviews every waiting session first so the mirror receives
  current findings, holding sync until that finishes.
- Sessions over 2,000 message, tool-call, and result-event rows or 4 MiB of
  stored text are marked skipped instead of reviewed.
- A daemon without automatic sync does not review in the background, including a
  daemon started with `--no-sync` and one serving a seeded or restored
  archive-only database. Its sessions are reviewed before a PostgreSQL push, and
  a `--no-sync` daemon also catches up once it restarts with automatic sync.

An hourly job builds one stored digest per completed local day, catching up
across `backfill_days` (1 to 3650, default 7) without ever building today. Set
`AGENTSVIEW_FRICTION_TZ` to override the configured timezone. Seat patterns
label digest findings from session paths without changing stored detections.
`[friction.kata]` requires `[kata]` enabled on the hub; see
[File patterns automatically](/docs/kata/#file-patterns-automatically).

Digests store Markdown, schema 1 summary JSON, frozen findings and usage,
session membership, and pattern recurrence. SQLite preserves this state across a
full resync when the archive keeps transcript text, while usage-only SQLite
archives do not build or update digests. PostgreSQL builds its own digests from
pushed findings when its serving role can write the review tables.

You can read digests, findings, and recurring patterns on the **Friction Log**
page (`/friction`), with
[`agentsview friction`](/docs/commands/#agentsview-friction), through the
`/api/v1/friction` routes, or with the `get_friction_digest` and
`list_friction_patterns` [MCP tools](/docs/mcp/). Run `agentsview friction run`
to build missing days on demand. Because findings are heuristics, check the
session before acting on one.

To record a failure outside an agent session, run
`agentsview diagnostics append` or call `POST /api/v1/diagnostics`. Friction
review must be enabled on the archive building the digest for those events to
appear; the digest groups matching sources and messages in a Diagnostics
section. Diagnostic events do not create P0 alerts or recurring patterns, and a
delayed event rebuilds its completed day, including dates older than the session
backfill window.

On the **Friction Log** page, **Generate summary** asks your configured
`[insights]` endpoint or agent CLI for an optional model-written overview of one
digest (nothing generates summaries on a schedule). The model receives the
digest counts, P0 alerts, up to 20 pattern titles ranked by occurrences, and up
to 20 grouped diagnostic sources and messages. Pattern titles can include up to
80 characters of the text that triggered a correction, workaround, or deferral;
frustration titles are withheld because they quote the user. The model never
receives session finding text, evidence, or transcripts. The summary is labeled
as model-written and cannot alter findings, digests, or recurrence. Repeating
the request returns the saved summary until the digest changes, and saved
summaries also appear under **Recall → Generated insights** as **Friction
Review**.

Local days wait until their sessions have current findings, and that wait does
not expire; other completed days can still build while review catches up.
PostgreSQL uses the findings pushed by each source (including other rules
versions) and skips sessions whose source has review disabled. Sessions over the
review budget are excluded right away.

Catch-up fills missing dates across `backfill_days` (including empty days) and
revisits dates with unrecorded sessions inside that window, so late syncs and
pushes update existing digests. Each session belongs to only one digest even if
it resumes later, and rebuilding does not double-count its patterns. Sessions
that arrive or finish review outside the window need a larger `backfill_days`
value. Each daily build holds the sync lock only for that day. PostgreSQL
defaults to UTC. Configure the same timezone on every process serving the
same hub.

Hosted raw sync reviews sessions as it projects them regardless of this setting.
See [Hosted Raw Sync](/docs/hosted-raw-sync/).

### Restricting Ingestion by Working Directory

By default, AgentsView ingests every discovered session. To limit the archive to
sessions from specific workspaces, for example on a machine shared across
clients where transcripts from one workspace must stay separate from another,
set `sync_include_cwd_prefixes` in `~/.agentsview/config.toml`:

```toml
sync_include_cwd_prefixes = [
  "/home/me/work/client-a",
  "/home/me/oss",
]
```

When this list is non-empty, AgentsView ingests a session only if its recorded
working directory matches one of the prefixes or sits beneath one. Prefixes and
session directories are lexically cleaned before matching: trailing separators
are ignored and `..` components are resolved, so `/home/me/oss/../other` does
not match `/home/me/oss`. Matching is path-boundary aware (`/home/me/oss` matches
`/home/me/oss/repo` but not `/home/me/oss-other`), case-sensitive, and uses your
operating system's path separator (on Linux and macOS, a backslash is an
ordinary filename character rather than a directory boundary). Use absolute
paths because `~` is not expanded.

Keep these details in mind:

- While the filter is set, AgentsView skips sessions without a recorded working
    directory (a few agents do not store one).
- The filter gates ingestion only and keeps sessions already in the archive
    (SQLite is a persistent archive). To remove unwanted existing sessions, run
    `agentsview session prune`.
- Remote-host sync is unaffected: because prefixes describe local paths,
    AgentsView does not apply them to sessions pulled from `[[remote_hosts]]`.

### macOS Protected Folders

To label a session with its Git remote, worktree, and branch, AgentsView reads
Git metadata from the session's recorded working directory, and it reads
`SKILL.md` files referenced by recorded tool calls to infer skill names. On macOS, some
locations sit behind a privacy consent prompt; reading inside one makes macOS
attribute the request to the AgentsView app and ask you for access. Because the
first sync walks every session in the archive at once, that used to trigger a
burst of prompts at first launch for folders you never pointed AgentsView at.

AgentsView skips these locations during discovery:

- `~/Desktop`, `~/Documents`, `~/Downloads`, `~/Movies`, `~/Music`, `~/Pictures`
- `~/Library/CloudStorage` (Dropbox, OneDrive, Google Drive, Box) and
    `~/Dropbox`
- `~/Library/Mobile Documents` (iCloud Drive)
- Removable and network volumes mounted under `/Volumes`, such as
    `/Volumes/External`

The `/Volumes` rule matters most when AgentsView runs as a background service:
macOS displays the consent prompt on the console, so a `launchd` agent on a Mac
with nobody logged in at the screen would wait on it forever and startup sync
would never finish.

Sessions whose working directory lives in one of those folders, including
directories that only reach one through a symlink, keep **path-only project
identity**. AgentsView still ingests, lists, searches, and groups them by path,
but omits their Git remote, worktree relationship, and branch, deriving the
project name from the path instead of the enclosing Git repository. Sessions
anywhere else are unaffected.

Skill files in those locations also keep path-derived names; AgentsView skips
their frontmatter without reading the file, including through directory
symlinks.

If you keep code or skills in one of these folders and want their metadata, opt
in:

```toml
scan_protected_paths = true
```

On the next sync, macOS prompts once per folder or volume; granting access
restores full identity for those sessions, and denying leaves them path-only.
This option has no effect on other platforms.

Enabling `scan_protected_paths` applies to sessions parsed after the change.
Sessions already in the archive keep their path-only identity until their
session files change; to reparse the archive and pick up Git detail for existing
sessions, run `agentsview source sync --full`.

### Large Watch Trees

On Linux, the recursive watcher watches every directory until the kernel's
`fs.inotify.max_user_watches` limit runs out. Windows and other non-macOS
platforms keep a budget of 8,192 directories per process, while the macOS
FSEvents watcher has no per-directory budget. If a session root exceeds the
remaining budget, or registering watches hits the operating system's inotify or
file-descriptor limit (`ENOSPC` or `EMFILE`), AgentsView polls that root
instead. The HTTP listener binds before registering any watches, so the server
still starts.

Providers can also cap how deep a recursive root is watched when they read only
a fixed depth. Antigravity and Antigravity CLI `brain` folders are watched one
level deep so the generated trees inside each `brain/<id>` do not count against
the budget, and the Antigravity IDE watcher also watches
`brain/<id>/.system_generated/logs` for the plaintext transcript. This depth cap
applies wherever the fsnotify watcher runs.

If a folder watch fails at runtime, healthy folders stay on native watches.
Claude polling covers the affected project folder, while other providers poll
the root's configured scopes so changes to companion files reach their owning
sessions. Retries start after 10 seconds and back off to two minutes, and the
next shared poll pass batches recovered folders and reconciles their original
provider root scopes before releasing polling. A failed reconciliation keeps
polling active for the next retry, and a deferred folder keeps polling until its
whole subtree is watched or the directory disappears. New native events sync on
the normal interval, apart from failed discovery, which keeps backing off up to
two minutes. Replacing the directory or an ancestor with a regular file releases
polling, while a directory symlink keeps polling. Removing a root from the watch
plan releases its polling obligations. Budget or native watch resource
exhaustion keeps root-level polling, and an event-buffer overflow puts every
recursive root on polling. On macOS, when FSEvents reports dropped or
must-rescan events for one session folder, AgentsView rescans only that folder
and keeps watching it natively without affecting other folders or moving them to
polling; if the drop hid the folder's own removal or replacement, only that
folder is polled until it can be watched again.

Roots that fall back to polling are picked up by a 2-minute fallback sync loop
that runs whenever any roots are unwatched and reconciles the affected scopes.

For Claude Code, Codex, and providers that store each session in its own file, a
fallback pass first compares each root's session files against the previous pass
by size, modification time, change time, and file identity. Those providers are
Amp, Codex Cloud, DeepSeek TUI, Direct API, gptme, iFlow, Kimi Work, Kiro IDE,
Muse Code, OhMyPi, OMO, Open Code Review, Pi, Prime Agent, Qwen Code, QwenPaw,
StepCode, Tau, WorkBuddy, and Zencoder. It syncs only the files that changed,
the same way it handles a native file event. The pass reconciles the whole root
instead on the first poll of that root, when a file has disappeared, when more
than 1,024 files changed, or when it cannot compare a file (such as a symlinked
project folder or an unreadable folder). A disappeared file is always left to
that reconciliation, which checks whether the folder is available before marking
a session's source missing.

After a fallback pass finishes, the loop waits 2 minutes or as long as that pass
took, whichever is longer, so polling uses at most about half of wall-clock
time. Each pass reconciles one provider at a time, and waiting watcher syncs run
between providers so live sessions keep updating during a long pass.

The startup polling summary counts roots handed to fallback polling and excludes
folders already covered by separate polling obligations. Runtime
`polling N unwatched root(s)` logs count distinct polling scopes, including
Claude project folders. Per-root and startup summary lines look like this:

```
Couldn't watch 12500 directories under /home/me/.claude/projects, will poll every 2m0s
Polling 1 roots every 2m0s for changes
```

No configuration is required, though on Linux you can raise the global cap to
keep more roots watched in real time:

```bash
sudo sysctl fs.inotify.max_user_watches=524288
```

## Memory Behavior

The serve daemon sets a 4 GiB Go soft memory limit as a backstop so large
requests run with normal GC pacing; setting `GOMEMLIMIT` lower trades GC time
for a smaller heap. On Linux, every agentsview process limits glibc to two
malloc arenas and pins the heap trim threshold at 1 MiB. Pinning that threshold
disables glibc's dynamic adjustment, which otherwise lets a long-running process
stop trimming until up to 64 MiB is free, so memory freed by the SQLite driver
returns to the operating system instead of accumulating as fragmentation. You
can override any of these defaults by setting `GOMEMLIMIT`, `MALLOC_ARENA_MAX`,
or `MALLOC_TRIM_THRESHOLD_` in the process's environment.

## Manual Sync

Trigger a sync through the API:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/sync
```

Trigger a full resync (re-parses all session files from scratch):

```bash
curl -X POST http://127.0.0.1:8080/api/v1/resync
```

Both endpoints stream progress over Server-Sent Events when called from a
browser or SSE-capable client.

Check sync status:

```bash
curl http://127.0.0.1:8080/api/v1/sync/status
```

The JSON response includes `startup_pending`, `archive_stale`, and (starting
with daemon API 15) `sync_pending` alongside sync progress. See the
[session usage contract](/docs/session-api/#agentsview-session-usage) for how
`session usage --wait` uses these states.

## Privacy and Telemetry

By default, all session data stays on your local machine in SQLite. Normal
browsing, search, and reports never send session content, project names,
prompts, file paths, or hostnames anywhere.

Optional features that send data externally when you enable them:

- [Hosted Raw Sync](/docs/hosted-raw-sync/) sends original provider files to a
    hosted custody service configured by your deployment operator.
- [PostgreSQL sync](/docs/pg-sync/) (`pg push`) sends session data to a
  PostgreSQL database you configure.
- [ClickHouse sync](/docs/clickhouse-sync/) (`clickhouse push`) sends session
  data to a ClickHouse database you configure.
- [Generated insights](/docs/recall/#current-surface) sends scoped session
    content to the configured endpoint when `[insights]` sets `endpoint` and
    `model`, or to the selected agent CLI when neither is set.
- [Publish to Gist](/docs/usage/#publish-to-gist) uploads a session to GitHub.

Automatic outbound requests include update checks and pricing refreshes:

- **CLI and web UI.** When the web UI loads, the server requests
    `github.com/kenn-io/agentsview/releases/latest` to check for the newest
    release and caches the answer for an hour. No identifying information is
    sent beyond standard HTTPS request metadata (IP address and user-agent).
- **Desktop app.** Tauri's native updater checks the GitHub release feed
    independently.
- **Pricing.** The server downloads public model-pricing catalogs from GitHub
    and OpenRouter at startup and daily; see
    [Pricing Source](/docs/token-usage/#pricing-source).

### Usage Reporting

AgentsView can send general, anonymous usage telemetry, which stays off unless
you opt in or set `TELEMETRY_ENABLED=1`. Development builds never send it.

Any of these environment variables turns telemetry off even after you opt in:
`AGENTSVIEW_TELEMETRY_ENABLED=0`, `TELEMETRY_ENABLED=0`, `DO_NOT_TRACK=1`, or
setting `CI` to any value. Restart the server after changing them.

### Disabling Update Checks

You can turn off the CLI and web UI update check in any of three ways:

| Method               | Value                                                        |
| -------------------- | ------------------------------------------------------------ |
| Config file          | `disable_update_check = true` in `~/.agentsview/config.toml` |
| Environment variable | `AGENTSVIEW_DISABLE_UPDATE_CHECK=1`                          |
| CLI flag             | `--no-update-check`                                          |

To turn off the desktop app's auto-updater separately, set
`AGENTSVIEW_DESKTOP_AUTOUPDATE=0`.

## Find transcript images in Docbank

Configure your Docbank server to locate where it holds the exact bytes of a
transcript image. Results show current files, prior versions, and trash, and
setting `web_url` links each match to its exact version in the browser (exact-version
links require [docbank#1024](https://github.com/kenn-io/docbank/pull/1024)).

```toml
[integrations.docbank]
url = "https://docbank.example.com"
web_url = "https://docbank.example.com"
api_key_env = "DOCBANK_API_KEY"
```

Set `DOCBANK_API_KEY` in the environment of each consumer (the daemon, CLI, and
MCP server), or replace `api_key_env` with `api_key_file` pointing to a private
file that holds the key; see [Read secrets from files](#read-secrets-from-files).
Lookup needs `url` and either `api_key_env` or `api_key_file`; omit the section
to leave it off. An invalid section stops the server from starting, while other
commands print a warning and leave lookup off. CLI commands that talk to a
daemon use the daemon's integration configuration.

MCP exposes `get_session_file_references` when its backend has Docbank lookup
enabled, while connections through a signed reader listener omit this tool.

| Setting                            | Purpose                                                              |
| ---------------------------------- | -------------------------------------------------------------------- |
| `integrations.docbank.url`         | Docbank API base URL. HTTPS, or HTTP under the policy below.         |
| `integrations.docbank.web_url`     | Optional browser base URL for version links.                         |
| `integrations.docbank.api_key_env` | Environment variable containing the API key.                         |
| `integrations.docbank.api_key_file` | Private file containing the API key, instead of `api_key_env`.      |
| `integrations.docbank.allow_http`  | Allow plaintext HTTP to any host. Default: `false`.                  |

HTTPS and loopback HTTP work by default. For a trusted private container
network, opt in explicitly:

```toml
[integrations.docbank]
url = "http://docbank:8485"
api_key_env = "DOCBANK_API_KEY"
allow_http = true
```

`allow_http` applies to both `url` and the optional `web_url`, matching the
Insights transport policy. Because HTTP sends the API key and requested digests
in plaintext, enable it only on a network you trust. API keys stay in request
headers and redirects are refused.

Lookup covers retained tool-result images (including offloaded assets), inline
OpenCode `file.uri` payloads such as PDF and text files, and Markdown `asset://`
images in message text. Claude pasted images are not covered yet, and native
Codex attachments and files handled by tools need exact bytes in the archive
(paths and references alone are not covered).

Only SHA-256 digests leave your machine. Docbank file names, paths, and trash
entries return with the results, so anyone who can read sessions on this server
can see them.

Transcript reads run independently of Docbank, and database reads and Docbank
requests each get their own five-second budget. Each lookup resolves up to 50
distinct digests with four requests at a time and shows up to ten matches per
digest. Docbank lookup failures report Docbank unavailable and can be retried;
after the first failed lookup, a request stops asking Docbank and reports its
remaining digests unavailable. Archive read failures report session files
unavailable. Digests beyond the cap report `limit_exceeded` without Retry, and
images dropped by the image policy have no bytes to hash so they report
`bytes_not_kept` without a lookup.

Results refresh when a session reopens and when its transcript changes, at most
every 15 seconds while matches are already shown, and successful Docbank answers
are reused for one minute. No migration or resync is needed.

## Image text

Enable image text extraction when a local GLM-OCR service is available:

```toml
[image_text]
enabled = true
# endpoint = "http://127.0.0.1:30004/glmocr/parse"
```

Restart the daemon after changing this setting. Images removed by the `drop`
storage policy cannot be indexed. Disabling image text clears extracted text,
and enabling it again extracts images from scratch. The endpoint must be an
`http://` URL on a loopback host with an explicit port and the path
`/glmocr/parse`. Image text needs an `archive_content` setting that keeps tool
results; otherwise it stays off and logs a warning. See
[search text in images](commands.md#search-text-in-images) for supported images
and search modes.
