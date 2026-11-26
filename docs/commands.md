---
last_edited: 2026-11-17
title: CLI Reference
description: All AgentsView commands, flags, and environment variables
---

## Start from help

Run `agentsview --help` to see a numbered workflow for syncing sources, finding
sessions, reading transcripts, and checking usage. The help text also covers
session IDs, backend selection, output streams, exit codes, and environment
variables. For the provider catalog, built-in source locations, and environment
override names, run `agentsview source list --json`. That output does not
resolve configured directory arrays. To print the API schema, run
`agentsview openapi`.

For common backend and output flags, run `agentsview session --help`. Core
commands also include examples and note their limits.

## Recover from CLI errors

When a command fails, it writes `fatal: <message>` followed by
`hint: <next step>` to stderr. The hint names the CLI commands, flags, or
settings to check, and unknown commands or flags point you to the relevant
command's `--help`.

Commands that accept `--json` or `--format json` write one JSON diagnostic line
to stderr instead:

```json
{"error":"usage","message":"unknown flag: --bogus","hint":"run `agentsview session list --help` for flags and examples"}
```

The `error` field names the error category: `usage`, `validation`,
`unavailable`, `authentication`, `forbidden`, `conflict`, `not_found`, or the
general `error`. When you pass `--json` as true, it takes precedence over
`--format`, and repeated flags use their final valid values. Rejections on
fixed-stream commands also honor the requested JSON error format. Existing
warnings can appear before the diagnostic on stderr, while success reports,
transcript streams, and API responses keep their existing formats on stdout.

If a session is missing, copy a canonical ID from
`agentsview session list --json`. To pick up a new session, run
`agentsview source sync` on the archive host. If you disable autostart with
`AGENTSVIEW_NO_DAEMON=1`, run `agentsview daemon start` before retrying a
daemon read. When you pass an explicit remote `--server`, supply that host's
URL and token with `--server-token-file` or `AGENTSVIEW_SERVER_TOKEN`.

### Exit codes

Existing command-specific outcomes take precedence over the general error
classes. Check the process exit status separately from stdout, because a usage
report can still be present with exit 3.

| Code | Meaning |
| --- | --- |
| 0 | Success. An empty result can still succeed. |
| 1 | General error, authentication or policy rejection, a server conflict, or an existing command-specific failure. |
| 2 | Session not found for `session usage` / `token-use`; `raw-sync backfill` also uses 2 for failure. |
| 3 | `session usage` / `token-use` found no usage figures, or `serve --check-data-version` found a newer database format. |
| 4 | Export cursor reset or conversation reconciliation required. |
| 5 | An exported conversation revision changed. |
| 6 | `export conversations session` found no record of the session. |
| 64 | Command syntax, unknown flags, missing arguments, or incompatible CLI flags. |
| 65 | Malformed flag values or invalid values checked by the CLI. |
| 69 | A daemon, server, or semantic search index is unavailable. |
| 70 | `capture report` failed. |

`capture run` passes through the captured command's exit status, and
`session usage` stays silent on exit 3 after writing its normal report.
`session usage-batch` records per-session exit codes inside its result document
and succeeds when the batch itself completes. Export cursor and revision errors
keep their existing flat JSON documents on stderr, and hidden worker and
browser-host protocols keep their existing diagnostics.
`parse-diff --fail-on-change` writes its normal report and exits 1 when it
finds changes or parse failures. Remote sync failures keep their per-host
summary and add recovery advice.

## Commands

### Find a command by task

Describe what you want to do to get up to five matching commands:

```bash
agentsview cli search "show token cost for a conversation"
agentsview cli search list supported providers --limit 3 --json
```

Search inspects the command tree locally and needs no configuration, database,
daemon, network connection, or credentials. Results use current command paths
and leave out hidden legacy commands, help-only groups, browser setup, and
interactive service installation, while eligible actions inside groups remain
searchable.

`--limit` accepts 1 to 20, with a default of 5. Pass `--json` or `--format json`
to get
`{"query":"...","results":[{"command":"...","summary":"...","usage":"..."}]}`.
If no command matches, the search succeeds with an empty `results` array. Text
output includes a command help hint for flags and examples. For help with this
group, run `agentsview cli`.

Root help lists command domains. For example, run `agentsview source --help` to
list the actions in that domain, then `agentsview source sync --help` to inspect
its flags. These command paths follow `main`; older releases use the legacy
names below.

### Command migration

Related actions now share a noun. Existing command names continue to work for
at least one release, though they are hidden from root help and completion and
print their replacement on stderr when you run them. `agentsview sync` is the
exception: it stays a supported shortcut for `source sync`, appears in root
help, and prints no deprecation notice. Flags, arguments, stdout documents,
exit codes, HTTP routes, and MCP tools keep their behavior, and daemon, SSH
sync, browser host, and parser child protocols keep their existing launch paths.

| Legacy command                             | Preferred command                                                  |
| ------------------------------------------ | ------------------------------------------------------------------ |
| `agents [agent...]`                        | `source list [agent...]`                                           |
| `import`                                   | `source import`                                                    |
| `chrome setup`                             | `source chrome setup`                                              |
| `raw-sync ...`                             | `source raw ...`                                                   |
| `usage cursor`                             | `source cursor sync-usage`                                         |
| `projects`                                 | `project list`                                                     |
| `health [session-id]`                      | `session health [session-id]`                                      |
| `prune`                                    | `session prune`                                                    |
| `stats`                                    | `report stats`                                                     |
| `activity report`                          | `report activity`                                                  |
| `usage daily` / `usage statusline`         | `report usage daily` / `report usage statusline`                   |
| `pg ...` / `clickhouse ...`                | `replica pg ...` / `replica clickhouse ...`                        |
| `embeddings ...`                           | `index embeddings ...`                                             |
| `classifier ...` (recovery only)           | `index classifier ...` (recovery only)                             |
| `parse-diff`                               | `archive diff`                                                     |
| `token-use ID` (already deprecated)        | `session usage ID --json`                                          |

Keep using `agentsview usage grok <csv>` to import xAI billing files. Its
deprecation notice names `report usage grok`, which does not exist in 0.48.0.

`session prune` operates on the local archive only and rejects the session
group's inherited `--server`, `--server-token-file`, `--pg`, `--json`, and
`--format` flags before accessing storage. Pass `--dry-run` to preview what it
would delete.

### `agentsview git-hook`

Tag commits in a repository with the current agent session:

```sh
agentsview git-hook install
```

The `prepare-commit-msg` and `commit-msg` hooks work as a pair.
`prepare-commit-msg` records the message Git prepares, and `commit-msg` adds
`Agent-Session: <agent>:<session-id>` to commits made without an editor,
including `-m`, `-F`, `-C`, `--amend --no-edit`, and `merge --no-edit`. If you
leave a commit template unchanged, the hook leaves it alone so Git can reject
it. A conflicted cherry-pick finished with `--no-edit` or `--continue` also
carries the current session, while editor commits keep their original messages.
Git doesn't run `commit-msg` for `git revert --no-edit` or rebase picks, so
those get no new trailer, though rebase picks keep their existing trailers. If
you amend a commit from a different session, the hook adds a second
`Agent-Session` line.

The hook skips messages that contain only blank lines, Git comments, or
signoffs, and Git still rejects empty messages. A comment-only message such as
`-m '#123 fix'` can still commit when Git retains comments, but it gets no
trailer. Git doesn't tell hooks whether `-s` added a signoff, so when a commit
template already ends with your own signoff and another hook changes that line,
the commit gets no trailer. When you set `core.commentChar=auto`, Git picks its
comment character per commit without telling hooks which one it chose. In that
case, the hook treats lines starting with any of `#;@!$%^&|:` as possible
comments, so a message made only of those lines gets no trailer, and commits
that start from a commit template get none either.

Make sure `agentsview` is on your `PATH`. If the binary is missing or fails, or
if the session is missing, invalid, or ambiguous, the hook leaves your commit
message unchanged.

Discovery reads the session environment variable that each supported harness
sets for commands its agent runs:

| Agent | Variable |
| --- | --- |
| Claude Code | `CLAUDE_CODE_SESSION_ID` |
| Codex | `CODEX_THREAD_ID`, or `CODEX_SESSION_ID` when that is unset or empty |
| Cursor CLI | `CURSOR_CONVERSATION_ID` |
| OpenCode 2.0.19 and later | `OPENCODE_SESSION_ID` |
| Pi | `PI_SESSION_ID` |
| Oh My Pi | `PI_SESSION_ID` with `OMPCODE=1` |
| Qwen Code | `QWEN_CODE_SESSION_ID` |
| Goose | `AGENT_SESSION_ID`, only in Goose's `YYYYMMDD_N` form |
| Hermes | `HERMES_SESSION_ID` |

Claude and Codex IDs must be UUIDs. Commits from unlisted harnesses get no
trailer unless you set the override pair. If an agent runs inside another
agent's shell, such as Pi started from Claude Code, both variables are set, and
the hook leaves the message unchanged and prints a warning. To resolve that
ambiguity or override discovery, set both `AGENTSVIEW_SESSION_AGENT` and
`AGENTSVIEW_SESSION_ID`. Override agent names accept lowercase letters, digits,
and hyphens, up to 32 characters, starting with a letter or digit. IDs for
agents other than Claude and Codex accept letters, digits, dots, underscores,
and hyphens, up to 128 characters, starting with a letter or digit.

Installation writes both hooks into the directory Git uses, and rerunning it
succeeds without changing byte-identical hooks. If you have `core.hooksPath`
set, including globally, installation applies to every repository sharing that
path. The two hooks only work as a pair. When either file already exists with
other content, installation writes nothing and prints the steps for both: the
line to add to each existing hook, and the file to create for a missing one.
Put the `prepare-commit-msg` line right after the hook's first line so it saves
the template before your hook edits it:

```sh
if command -v agentsview >/dev/null 2>&1; then agentsview git-trailer-prepare "$@" || :; fi
```

Put the `commit-msg` line last, before any final `exit`, so it stamps after your
hook's edits. It keeps the status of your hook's last command and doesn't exit,
so a failing check still rejects the commit and a final `exit` still decides:

```sh
agentsview_status=$?; if command -v agentsview >/dev/null 2>&1; then agentsview git-trailer "$1" || :; fi; (exit "$agentsview_status") && :
```

To remove the hooks, delete the installed `prepare-commit-msg` and `commit-msg`
files. For shared hooks, remove the lines you added.

### `agentsview archive`

Available in 0.46.0 and later, these commands let you retain captures from
several machines, recover their original files, and reparse Claude and Codex
sessions after the original directories are gone. `capture` can snapshot live
SQLite databases, but regular files must stay unchanged while copied because a
changing transcript aborts the capture. Stop source writers before taking a
final retirement capture, and stop the destination daemon before running
import, reparse, backup, or extraction, as those archive writes refuse to run
while another process owns the database.

Set up an isolated data directory for the receiving archive and seed it from
the capture of the installation you are retiring, as shown below. Older or
alternate databases stay as supplemental files; these commands do not merge
databases. After a rehearsal, assemble the final archive in a new directory
from final captures. Later imports retain changed versions for recovery, but
they do not update already accepted browsable sessions or deletion policy.
Ongoing collection and source advancement are outside this workflow.

Create a capture on the source machine by selecting roots explicitly. The
`files` root retains supplemental trees without parsing them:

```bash
agentsview archive capture /media/backup/source-capture \
  --root claude="$HOME/.claude/projects" \
  --root codex="$HOME/.codex/sessions" \
  --root files=/path/to/supplemental-history
```

The standard Claude `projects` and Codex `sessions`/`archived_sessions` roots
include their parent directory to keep companion files, though only the
selected session directory is parsed. Selecting a provider home includes its
existing conventional session directories, and no other home-directory scanning
happens. Capture also records other spellings of each root that the captured
database uses, such as a path through a symlink, after checking that they
resolve to the same directory. That lets reparse recognize those seeded
sessions after the originals are gone.

Capture reads the installation ID without creating one in the source and reuses
known raw-sync root IDs. When it generates identities, the JSON report lists
them under `new_identities`; pass `--identity-from PREVIOUS/capture.json` on
later captures to reuse them. Passing a previous descriptor from another
installation or conflicting checkpoint identities causes an error. The
descriptor alone is enough for identity reuse, without reading the older
captured files. If the source AgentsView data directory moved, identity reuse
keeps its original application-root binding and records its current location
separately. Import still requires the complete capture directory.

Capture streams and checks regular files and uses online backups for SQLite
databases, including committed WAL contents. It omits and lists symlinks,
special files, and recognized credential or settings files, and replaces the
original application config with allowlisted recovery settings. This does not
redact transcripts or database content. Exclusions include `.git-credentials`
and Claude's `ide/` runtime metadata. An ordinary artifact vault currently
prevents capture because its stopped-owner copying support is still required.
If the source already contains a raw archive, use `archive backup` to preserve
both its vault and its database, since `capture` refuses that input.

Passing `--writers-stopped` records your attestation that source writers were
stopped; it does not stop them for you. A rolling capture is not a final
retirement cutoff. The destination must be new and outside the selected inputs,
and failed captures are not published. The completed directory contains
`capture.json`, a hashed inventory, and root-relative files; move the directory
as a whole. Import rejects any file not listed in the inventory. On macOS, copy
with `rsync -a` or `COPYFILE_DISABLE=1 tar`, because plain `tar` adds `._*`
metadata files.

Import verifies the generated descriptor and every file before accepting
sources, and handwritten import specifications are no longer accepted. Both the
capture and import reports include counts from the captured database. Missing
or unsupported preflight data, artifact origins or imports, or qualified
session IDs block assembly pending identity integration.

To preserve existing history and curation, create the archive with `--seed`:

```bash
AGENTSVIEW_DATA_DIR=/new/archive agentsview archive import --seed \
  --spec /media/backup/source-capture/capture.json
```

The target directory must not exist and must lie outside the capture. Seed
import preserves the captured database, installation identity, and assets,
including session and message IDs, stars, pins, names, trash, and permanent
exclusions, while also retaining the raw files. It checks the assembled archive
before publishing the directory, so a failed assembly leaves no partial
destination. Leave enough disk space for the database, retained raw files, and
a temporary database copy in case a rebuild is needed.

Seed import enables archive-only mode before any rebuild or startup, and it
does not reparse sources or adopt historical machine names. The report's
`unowned_machines` field lists unresolved machine keys along with session and
worktree-rule counts; run `db adopt-machine` only for keys you explicitly own.
Seed import refuses an existing raw archive, so use its `archive backup`
recovery point instead. Empty and `local` machine labels already denote the
seed's own installation and are recognized during reparse, whereas other named
machines still require proven ownership.

To add another machine's capture, run `archive import --spec OTHER/capture.json`
and then explicitly reparse its accepted sources. A hard import failure can
leave retained raw objects and already accepted sources in the target, and
retrying the same capture is safe and reuses that evidence. Sessions stay
attributed to the source installation even if the archive moves or a display
label changes. These commands require an archive-only destination created by
seed import or restore, and they refuse an everyday database instead of
converting it. Foreign session IDs include that installation ID, so equal
native IDs or paths on different machines stay separate observations, while the
seed keeps its existing session IDs. Unrelated identity collisions stop the
reparse without publishing its scratch database.

Foreign stars, pins, names, and project rules stay in the captured database and
are not merged. Source trash and permanent deletions suppress publication of
those sessions, though their original files remain recoverable, and reparse
reports those intentional skips as `suppressed`. Deletion evidence with
unresolved machine ownership blocks regular import; adopt only proven ownership
on the source and recapture. The first import freezes its deletion policy. A
later capture with changed deletion state retains the new files as evidence and
reports a conflict without changing the accepted projection.

```bash
export AGENTSVIEW_DATA_DIR=/new/archive
agentsview archive sources
agentsview archive verify
agentsview archive backup /media/backup/repository
# Use snapshot_id from the backup report for both commands:
agentsview archive verify --repository /media/backup/repository --snapshot SNAPSHOT_ID
agentsview archive restore /media/backup/repository /restored/archive --snapshot SNAPSHOT_ID
AGENTSVIEW_DATA_DIR=/restored/archive agentsview archive verify
AGENTSVIEW_DATA_DIR=/restored/archive agentsview archive reparse --all
# Use capture_id from the capture or import report to recover one version:
AGENTSVIEW_DATA_DIR=/restored/archive agentsview archive extract /new/capture --capture CAPTURE_ID
```

Both `restore` and `extract` require new destination directories. `backup`
creates a repository or adds a snapshot to an existing one, and both `backup`
and `restore` reject overlapping source and destination directories. Run
`extract --capture ID` to recover that capture's exact files, including
supplemental files and versions that conflicted on import. It recreates
`capture.json`, `inventory.json`, and `roots/` under the original configured
root IDs. The output passes the same complete-file verification as the original
capture and can be imported with `--spec /new/capture/capture.json`. Extraction
does not change accepted sources or browsable history.

Without `--capture`, extraction writes all retained native files under their
archive root IDs, which distinguish installations and providers. If any path
has different retained versions, this mode fails before creating output or
reading content. Repeated captures normally have different database and report
files, so pass `--capture` to choose one complete version. A failed extraction
removes its partial output.

Import reports retained files and bytes separately from accepted provider
sources, and files outside Claude and Codex capture plans remain supplemental.
When a source conflicts, import keeps it as evidence and reports a coverage gap
without replacing the accepted source automatically. An import with gaps prints
its report and exits with an error. No files are pruned automatically.

Pass `reparse --manifest ID` to select specific accepted sources; you can
repeat the flag or pass comma-separated IDs. `--all` must be explicit. Reparse
applies the receiving archive's `result_content_blocked_categories`, clones
SQLite once for the batch, and runs the normal sync engine, including its
large-Codex streaming path. It installs the replacement only after every
selected source succeeds. If anything fails, reparse leaves the previous
browsable archive in place and records an error. Session ownership conflicts
are reported without merging identities. Leave scratch space for the full
database, its WAL, and materialized source files. By default, `--scratch-bytes`
limits each materialized source to 16 GiB, though it does not cap total scratch
usage.

`verify` reads every retained object and accepted manifest. `backup` checks
that closure, holds the writer and raw-vault locks, and creates a consistent
SQLite snapshot with Docbank's portable backup API. It keeps assets,
installation identity, content and image retention policy, and the original
machine label. An existing ordinary artifact vault is currently rejected
because its stopped-owner capture is not implemented; it is never silently
omitted.

Record the backup report's repository ID, snapshot ID, application build, and
minimum reader version. Both `restore` and repository verification require
`--snapshot ID` even when the repository holds only one snapshot. Application
extras over 64 MiB require Kit reader version 6, so older binaries cannot use
any part of a repository once it contains such a snapshot.

`restore` stages the selected snapshot and checks SQLite integrity, every
accepted source object, and referenced message and tool images before
publishing a new directory. Missing or corrupt referenced images also prevent
`backup`. An older database is rebuilt from its stored sessions with all live
providers disabled. Leave disk space for the restored data and temporary
complete copies of large extras.

Original runtime configuration and connection credentials are not copied from
configuration; seed import and restore generate fresh local authentication and
loopback settings. Previously retained raw files stay unredacted and may
contain credentials. Seeded and restored databases are permanently
archive-only: foreground starts, background starts, and restarts do not scan
the receiving machine. Ordinary sync, source transfers over HTTP or SSH,
ChatGPT, Claude.ai, Gemini Apps, and Grok web imports and Sync, artifact
exchange, mirror publication, and raw-sync watch are all refused. Browsing,
curation, archive import, verification, extraction, backup, and explicit
reparse stay available. To collect new local sessions, use a separate data
directory; no runtime flag or config setting disables archive-only mode.

Startup resync keeps archive-only sessions and their acceptance records without
opening the raw vault, and reparsing them always requires an explicit command.
Continuous capture, cross-device merging, team permissions, cloud storage, and
online backup are outside this first implementation.

______________________________________________________________________

### `agentsview capture`

Capture and export the usage of one exact non-interactive automation run
without starting the daemon, server, web interface, or watchers:

```bash
agentsview capture run \
  --provider claude \
  --occurrence build-42 \
  --capture-dir ./capture-build-42 \
  --result ./usage-build-42.json \
  -- claude -p "Diagnose the build failure."

agentsview capture report \
  --capture-dir ./capture-build-42 \
  --result ./usage-build-42.json
```

`capture run` supports direct `claude -p` and `codex exec --json` producer
adapters, leaving the child process's standard input, output, error, and exit
outcome intact. The result always goes to a separate file, while recovery-only
`capture report` can use `--result -` to write to standard output. See
[One-shot CI capture](/docs/one-shot-capture/) for the result contract, retry
behavior, failure codes, and a complete GitHub Actions job.

______________________________________________________________________

<a id="agentsview-raw-sync"></a>

### `agentsview source raw`

Capture supported local provider sources and upload their raw generations to
hosted custody:

```bash
export AGENTSVIEW_RAW_SYNC_URL=https://agents.example.com
export AGENTSVIEW_RAW_SYNC_DEVICE_ID=device-id
export AGENTSVIEW_RAW_SYNC_CREDENTIAL=device-credential
agentsview source raw watch
agentsview source raw status
agentsview source raw server-status
```

`raw-sync watch` runs an initial bounded audit, watches for filesystem changes,
and retries interrupted uploads from its durable local checkpoint. Pass the
device credential only through `AGENTSVIEW_RAW_SYNC_CREDENTIAL`, never as a
command-line flag. You can override the server URL and device ID environment
variables with `--server` and `--device-id`.

| Flag                    | Default     | Description                           |
| ----------------------- | ----------- | ------------------------------------- |
| `--server`              | environment | Raw-sync server URL                   |
| `--device-id`           | environment | Provisioned device ID                 |
| `--allow-insecure-http` | `false`     | Allow HTTP for a loopback server only |
| `--debounce`            | `2s`        | Watch only, filesystem coalescing     |
| `--interval`            | `15m`       | Watch only, bounded audit interval    |
| `--audit-limit`         | `128`       | Watch only, max source work per audit |

`raw-sync status` reads the checkpoint without creating one and prints
path-free JSON with capture, queue, retry, failure, and coverage state. Raw
sync does not capture S3 roots. `raw-sync server-status` reads hosted status
with the `status` token scope and prints the server fields along with
`pipeline_depth` and `last_parse_latency_seconds`. That latency measures the
most recently completed current head, not the age of pending work. It reads its
credential only from `AGENTSVIEW_RAW_SYNC_CREDENTIAL`. A server HTTP 404 is an
error; to read local state, run `raw-sync status`. A deployment operator must
provision the device ID and credential, because there is no public enrollment
command yet. See [Hosted Raw Sync](/docs/hosted-raw-sync/) for the current
boundary.

Run `raw-sync clean-uploads` to execute one server-side cleanup pass for the
PostgreSQL upload sessions and private spool used by
`agentsview replica pg serve`:

```bash
agentsview source raw clean-uploads
```

This command uses the effective PostgreSQL target and AgentsView data directory
from your current configuration, and you can run it while `pg serve` is
stopped. Each pass uses the existing limit of up to 128 rows per SQL batch and
128 spool entries per directory scan. Because the command starts a fresh spool
cursor on every run and inspects at most the first 128 entries in that scan
window, repeating it can revisit the same preserved entries. Spool entries
beyond that window require the long-running server, which keeps its cursor
between its startup pass and its 15-minute cleanup passes. The command prints
`Raw upload cleanup pass completed.` only after the pass succeeds. Your
PostgreSQL target must have the provisioned raw-sync schema and write
privileges, which the command checks before creating the upload spool.

______________________________________________________________________

### `agentsview why`

Find the recorded sessions and prompts behind a commit or specific file lines:

```bash
agentsview why internal/db/messages.go:120-140
agentsview why --commit 1a2b3c4
agentsview why main.go --format json
```

Pass `path:N` for a single line, `path:N-M` for an inclusive range, or `path`
for the whole file. Line numbers start at 1. AgentsView blames the file in its
own Git repository, even when you run the command from another directory.
Whitespace changes count, and uncommitted lines show up as a separate count. A
file name ending in `:digits` is treated as a line range and can't be blamed by
that name.

`--commit` accepts a SHA or prefix and works outside a checkout; ambiguous
prefixes return every recorded candidate. For scripts, add `--format json` or
`--json`. Each commit keeps its own session links and the prompt before each
commit call, even when one session made several commits.

This command does not call a model. If you need newer records, run
`agentsview source sync` first. A transcript link identifies the session that
ran the commit, not who wrote each line. Deleted or trashed sessions appear as
missing records without a prompt.

______________________________________________________________________

### `agentsview daemon`

Manage the writable SQLite server, which runs the web UI, API, session sync,
and file watchers in a single process. `daemon start` launches the same server
as `serve --background`, using your saved configuration and without opening a
browser. You do not need a separate `serve` command. Unlike
`serve --background`, which exits after `daemon_idle_timeout` when idle,
`daemon start` stays running until you run `daemon stop`.

```bash
agentsview daemon start
agentsview daemon status
agentsview daemon restart
agentsview daemon stop
```

| Command   | Behavior                                                                   |
| --------- | -------------------------------------------------------------------------- |
| `start`   | Start a persistent daemon, or make the running automatic daemon persistent |
| `status`  | Show writable daemon state, URL, PID, version, and uptime                  |
| `restart` | Stop and restart the writable daemon; start it if it is already stopped    |
| `stop`    | Stop the writable daemon, or report that it is already stopped             |

On writable detached daemons and foreground servers, **Settings** includes a
**Restart now** button. It keeps your launch options, can interrupt an active
sync, and reloads configuration inside the same process without changing its
process ID, foreground attachment, or loaded executable. To pick up a newly
installed binary, use the external restart or update workflow instead. After
the identified replacement answers, the settings page reloads and returns to the panel you
had open. Read-only servers cannot restart from **Settings**, and
authentication changes take effect right away.

An explicit `--require-auth` flag takes precedence until you save
authentication in **Settings**. Later restarts from **Settings** use the latest
saved value, including later edits to the config file, while a fresh CLI start
or restart uses normal deployment and flag precedence.

If the page cannot reconnect within 30 seconds, click **Check connection** to
poll for another 30 seconds. To find the server address, run
`agentsview serve status` on the server machine and open the address it prints.
If the server stopped or never came back up, start it the same way you started
it originally. An automatically chosen port can change if the old port is busy,
while an explicitly chosen port must be free.

`daemon start` and `daemon restart` load the effective configuration from
`config.toml` and supported environment variables. They take no serve-specific
flags because persistent daemon settings belong in configuration. `--no-sync`
is runtime-only and is not a `config.toml` setting. For a one-off detached
server with sync disabled, run `agentsview serve --background --no-sync`. In
the same way, run `agentsview serve --background --host <address>` for a
one-off unauthenticated non-loopback bind; a persistent non-loopback `host` in
`config.toml` requires `require_auth = true`.

These commands manage only the writable SQLite daemon for the current data
directory, including a foreground `serve` process or a daemon auto-started by a
CLI command. `daemon stop` stops the web UI and background sync together. The
`daemon` commands ignore read-only `agentsview replica pg serve` and
`agentsview replica clickhouse serve` processes. If only read-only servers are
running, `daemon status` reports that no daemon is running, and `daemon stop`
and `daemon restart` leave those servers running.

`daemon status` distinguishes running, starting, and stopped states.
`daemon start` returns after its initial readiness wait even if a long
migration or sync is still running, leaving the child process to finish in the
background. `daemon restart` stays attached and prints phase, detail, and
elapsed-time updates until the replacement daemon is ready; pressing `Ctrl+C`
cancels the wait without stopping the child. When startup feels slow, run
`agentsview daemon status` and check the `serve.log` path it reports. If
startup stays stuck, follow the error message to verify the owning process
before stopping it manually and retrying.

During startup, the daemon reports database opening, schema and index updates,
and column migrations before each step begins. Those messages appear in your
terminal and in `serve.log`, which also records elapsed times. If an upgrade
needs a full resync, the daemon announces it as soon as the database version or
schema check detects it, and session counters start once database preparation
finishes.

______________________________________________________________________

### `agentsview serve`

Start the HTTP server with the embedded web UI, session sync, and file watchers
in the foreground. Unless you pass `--background`, the server stays attached to
your terminal until you press `Ctrl+C`. This is the same writable server that
`agentsview daemon` manages; the web UI and sync share one lifecycle.

`serve` reuses or replaces an existing local writable daemon according to the
[replacement rules below](#background-mode), and leaves read-only replica
servers alone. When `serve` reuses a running server, it prints the URL and
exits with status 1 so systemd's `Restart=on-failure` retries, while
`--background` exits with status 0. Open that URL to use the web UI. Stopping
the server with either `daemon stop` or `serve stop` also stops its sync and
file watchers. Passing `--no-sync` disables automatic sync in that process
without creating a separate sync daemon. If an upgrade needs a full archive
rebuild, `--no-sync` skips it at startup and prints a notice. Until that
rebuild runs, sessions that require the new parsers stay missing; click
**Sync** in the web UI or run `agentsview source sync` to rebuild.

```bash
agentsview serve [flags]
```

Starting with 0.23.0, you must pass the `serve` subcommand to start the server.
Running plain `agentsview` prints help instead of launching the web UI.

| Flag                | Default            | Description                                              |
| ------------------- | ------------------ | -------------------------------------------------------- |
| `--host`            | `127.0.0.1`        | Host to bind to                                          |
| `--port`            | `8080`             | Explicit nonzero port must be free; `0` selects any      |
| `--no-browser`      | `false`            | Don't open browser on startup                            |
| `--no-sync`         | `false`            | Disable initial, watched, and periodic sync              |
| `--no-update-check` | `false`            | Disable automatic update checks                          |
| `--require-auth`    | `false`            | Require a bearer token for API requests                  |
| `--background`      | `false`            | Start `agentsview serve` as a managed background process |
| `--replace`         | `false`            | Replace a running local daemon before starting           |
| `--base-path`       |                    | URL prefix for a reverse-proxy subpath                   |
| `--public-url`      |                    | Browser URL, also added to trusted origins               |
| `--public-origin`   |                    | Trusted browser origin (repeatable/comma-separated)      |
| `--proxy`           |                    | Managed proxy mode (`caddy`)                             |
| `--caddy-bin`       | `caddy`            | Caddy binary path                                        |
| `--proxy-bind-host` | `127.0.0.1`        | Interface for managed proxy                              |
| `--public-port`     | URL port or `8443` | Managed Caddy HTTP/HTTPS listener and URL port           |
| `--tls-cert`        |                    | TLS certificate path                                     |
| `--tls-key`         |                    | TLS key path                                             |
| `--allowed-subnet`  |                    | Client CIDR allowlist (repeatable/comma-separated)       |

If the default port `8080` or a port set in `config.toml` is busy, the server
automatically picks an available port. An explicit nonzero `--port` exits if
that port is occupied, while `--port 0` selects any free port. If you run a
supervised daemon that needs a fixed port, pass `--port` in its launch command.

When you replace a running daemon, an occupied fixed positive port is rejected
before the old daemon stops. You can reuse the daemon's existing host and port,
including narrowing a wildcard bind to loopback. To widen a bind on the same
explicit port, stop the daemon first with `agentsview daemon stop`.

A restart from **Settings** tries to reuse an automatically selected address so
the page can reconnect, falling back to another port if that address is busy.
`agentsview update` keeps the original `--port` choice recorded by the running
daemon, including `0`. Without an explicit port, a restart tries the previous
listening port and keeps automatic fallback. For details on remote access and
proxy flags, see [Remote Access](/docs/remote-access/).

**Examples:**

```bash
agentsview serve                                # defaults
agentsview serve --port 9090                    # custom port
agentsview serve --no-browser                   # disable browser auto-open
agentsview serve --background                   # start managed background server
agentsview serve --replace                      # replace an existing daemon
agentsview serve --public-url https://agents.example.com
agentsview serve --base-path /agentsview --public-url https://example.com
```

The server:

- Loads or creates `~/.agentsview/sessions.db`.
- Syncs discovered session directories at startup and every 15 minutes.
- Starts the file watcher with 500ms event batching. While nobody is watching
  the archive, watcher sync starts stay at least ten minutes apart by default;
  see [`background_sync_interval`](/docs/configuration/#config-file).
- Serves the Svelte SPA and REST API.

On ordinary restarts, saved sessions are available while initial sync runs.
Archive writes may return HTTP 503 with `Retry-After` during that sync; retry
the write after it finishes.

During a full parser or archive-only preservation rebuild, the daemon serves
the retained archive with stale warnings. Archive writes return HTTP 503 with
`Retry-After`, and the CLI prints the server's message and exits. Scoped
upgrades and schema repairs finish before the daemon starts listening.

CLI commands write one stale warning to stderr, and the dashboard, MCP, and the
`X-Agentsview-Archive-Stale` HTTP header also flag stale results.

Press `Ctrl+C` for a clean shutdown that flushes the database and stops file
watchers.

On current `main`, watcher batches link subagent relationships only for
affected sessions, and unchanged polls skip archive-wide linking unless a
failed or canceled batch left unfinished links. Poll logs name the provider
roots checked and report how long each pass took.

During polling, changes to working-directory metadata refresh clients without
triggering global parent linking. Worker processes return link repairs and
unfinished linking to the daemon, so clients see repaired links and later polls
retry failed linking even when an unrelated source fails to process. Because
workers confirm their link state separately from source errors, completed
repairs clear obsolete retries even when another source fails, and installed
rebuilds clear completed retries as well. Audit workers receive pending links
from the daemon even when sources have not changed, while missing or invalid
worker results keep the retry pending. Startup transfers pending links before
reconciling the worker-to-watcher gap, and repairs queued in the archive run
even when discovery finds no source files. Repairs committed to the live
archive refresh clients even if sync is canceled. If relinking copied
sessions fails during a full resync, the resync aborts before replacing the
archive; discarded replacements do not report their repairs and leave pending
retries intact for the live archive.

Unchanged broken or missing source files are skipped using the failure cache
described in [Sync Behavior](configuration.md#sync-behavior). Grok
companion-file events use normal content-fingerprint checks, so repeated
companion removal events do not clear a cached missing-summary failure, while
actual companion edits still trigger sync.

#### Background Mode

You can still use the `serve` background and lifecycle subcommands:

```bash
agentsview serve --background
agentsview serve status
agentsview serve restart
agentsview serve stop
```

The parent command starts a detached `agentsview serve` process and waits for
it to publish its runtime record. Its five-second readiness window tracks
startup inactivity, so active startup progress can keep the parent waiting
longer. Once ready, it prints the URL, PID, and log path, with background
output written to `~/.agentsview/serve.log`. `serve status` shows the preferred
managed process, URL, version, uptime, and read-only mode when available. For a
writable server, `serve status` also queries the
[Codex Cloud](/docs/configuration/#codex-cloud) source status. When that source
is configured, it prints a status line for each ChatGPT account or Codex home,
followed by its live tokens and plan usage lines whenever they have something
to report. When the source is off or misconfigured, it prints one status line
for the whole source and a "Live tokens: off." line. If the server cannot
return the status or fails while answering, `serve status` prints one line
explaining why, even if the source is not configured. Read-only servers and
older servers without this status add no extra lines. `serve stop` keeps its
broad scope: it gracefully stops confirmed writable SQLite and read-only
PostgreSQL or ClickHouse server processes for the data directory and cleans up
their runtime records.

`serve restart` is narrower and config-driven. It restarts only the writable
SQLite daemon, leaves read-only servers running, and starts the writable daemon
if it was stopped. It takes no serve flags and uses the same effective
configuration as `daemon restart`. It is not equivalent to the broader
`serve stop` followed by a foreground `serve` start.

`serve` and ordinary CLI commands automatically replace an older release daemon
with a newer release, while an older release reuses a compatible newer daemon
instead of downgrading it. When either binary is a development build, any
different version string triggers replacement. Repeated dirty builds from the
same commit can share a version string and require `agentsview daemon restart`.
If two development installations share a data directory, they can replace each
other when invoked; use separate data directories to keep them independent.

Long-running clients, including `mcp` and push commands with `--watch` as well
as installed push services, start a missing daemon and reconnect after daemon
restarts, but never replace a running daemon. They keep using it as long as its
API and data versions stay compatible. If they are incompatible, the client
reports the conflict and asks you to restart it with the current binary.

To replace a daemon on purpose, including for a release downgrade, run
`serve --replace` or `agentsview daemon restart`. Automatic replacement keeps
the daemon's launch options. Every replacement path checks archive
compatibility before stopping the daemon, so an older binary cannot replace a
daemon whose SQLite data version it cannot open. When a live daemon is
incompatible, `serve status` prints both the daemon and binary versions along
with `daemon restart` or `daemon stop` guidance.

Background servers also act as the shared local daemon for the desktop app and
CLI. Because the daemon owns local SQLite writes for its data directory, common
write and freshness-sensitive commands proxy to it instead of opening the
archive as a second writer. An automatically started daemon exits after the
idle timeout once no external request or daemon-owned job is active. Periodic
sync and file-watcher work delay exit while they run, but do not keep the
daemon alive indefinitely by themselves.

`sync_idle_after = "10m"` pauses local collection while idle, and `"0"`
disables that pause. `background_sync_interval = "10m"` spaces out syncs while
nobody is watching the archive, and `"0s"` syncs at `sync_interval` all the
time.

#### CLI daemon behavior

Ordinary session commands, including `session list`, `session get`, and
`session messages`, need a compatible local daemon and start one when needed.
To inspect historical machine keys or run diagnostics without starting a daemon
or changing configuration, use `db adopt-machine --list` or `doctor sync`.

Commands that need fresh data or write to the archive auto-start the detached
daemon when no compatible daemon is running. That covers local `sync`,
`session sync`, `token-use`, normal `usage` refresh paths,
`pg push`/`pg push --watch`, and `clickhouse push`. If a writable daemon is
known to own the archive but cannot be reached, these commands refuse instead
of writing directly.

When you pass `--no-start-daemon`, `pg push --watch` and
`clickhouse push --watch` require an existing writable daemon.

To disable daemon auto-start, set `AGENTSVIEW_NO_DAEMON=1`. Commands that
require the daemon refuse to run while it is disabled. Direct write commands
acquire the local write-owner lock before opening SQLite and refuse if another
process holds that lock. If `agentsview daemon status` shows a daemon, run
`agentsview daemon stop`; otherwise find the agentsview process holding the
lock file and wait for it to finish or stop it. Auto-start for MCP and CLI
commands that start a server on their own reports the lock holder the same way
instead of launching a server that could not open the archive, while
`daemon start` and `serve --background` report the child's failure as before.

______________________________________________________________________

<a id="agentsview-sync"></a>

### `agentsview source sync`

Refresh the local archive. For a local sync, the CLI talks to the running
daemon or starts a detached one so SQLite writes stay owned by a single
process. That daemon also serves the web UI and keeps running after the CLI
exits until you stop it or its idle timeout expires. The command prints the
server URL and how to stop it.

When the daemon is already running, an incremental sync applies only the
changes its file watchers collected, including earlier failures waiting for a
retry, and retries unfinished archive imports. Because it skips rescanning
every session source, it stays quick on large archives. The daemon runs a full
discovery pass instead before its startup sync finishes, when started with
`--no-sync`, when the archive needs a rebuild, and after a **Settings** change
to session sources until a full pass completes. Roots that no watcher covers
follow the daemon's polling schedule. To rescan every source now, pass
`--rescan`.

An incremental local-only sync through the default daemon waits if another sync
or maintenance pass is active, then runs its pass. It prints a waiting status,
and pressing `Ctrl+C` cancels your wait without stopping the active work. You
see that waiting message when no remote hosts are configured. Full resyncs,
syncs with remote hosts, and servers started with `--no-sync` also wait for
exclusive access, though without printing that message. If the wait drags on,
check `agentsview daemon status` and `serve.log`. A daemon launched for this
local sync skips its redundant startup sync.

To run a one-shot offline sync, stop the writable daemon first, then run:

```bash
agentsview daemon stop
AGENTSVIEW_NO_DAEMON=1 agentsview source sync
```

Setting `AGENTSVIEW_NO_DAEMON=1` disables daemon auto-start without stopping an
existing daemon or bypassing its write-owner lock. The offline command acquires
that lock, syncs directly, and exits without leaving a server running.

```bash
agentsview source sync [flags]
```

| Flag       | Default | Description                                                  |
| ---------- | ------- | ------------------------------------------------------------ |
| `--full`   | `false` | Force a full resync regardless of data version               |
| `--rescan` | `false` | Rescan every session source instead of the collected changes |
| `--target` |         | Exchange normalized artifacts with a trusted folder          |
| `--host`   |         | Configured remote host name                                  |

**Examples:**

```bash
agentsview source sync           # incremental sync and exit
agentsview source sync --full    # full resync and exit
agentsview source sync --rescan  # rescan every session source and exit
agentsview source sync --target /path/to/shared-folder
agentsview source sync --host devbox1
```

After a discovery pass, AgentsView prints a summary of session counts to
stdout, while a sync that applied only collected changes says so instead. If
you pass `--target`, AgentsView also runs one bounded normalized-artifact
exchange; see [Artifact Folder Sync](/docs/artifact-sync/) for the trust model,
first-use requirements, and exclusions. You cannot combine `--target` with
`--host`.

When you pass `--host`, AgentsView syncs only the matching `[[remote_hosts]]`
entry from your config and fails fast on error. That works in offline mode with
`AGENTSVIEW_NO_DAEMON=1` too, where the local command contacts the configured
remote directly. Unknown host names are rejected, and ad hoc remotes are not
supported. `--host` runs an entry with `transport = "ssh"` the same way as an
HTTP entry; see [SSH Remote Sync](/docs/remote-access/#ssh-remote-sync).

Local sync can also read Claude, Codex, and Cursor roots from S3-compatible
object storage. Add `s3://` entries to `agents.claude.dirs`,
`agents.codex.dirs`, or `agents.cursor.dirs` in `~/.agentsview/config.toml`,
then run `agentsview source sync` as usual. AgentsView treats object storage as
a read-only source, using object size and `LastModified` metadata to skip
unchanged sessions and downloading only objects that need parsing. See
[Configuration — S3-Compatible Session Sources](/docs/configuration/#s3-compatible-session-sources).

#### Configured Remote Hosts

Declare remote hosts in `~/.agentsview/config.toml` so a bare
`agentsview source sync` syncs an entire fleet at once. Each entry uses
`transport = "http"` by default to talk to a remote daemon, or
`transport = "ssh"` to pull from a machine over `ssh` without running
AgentsView there:

```toml
[[remote_hosts]]
host = "devbox1"
transport = "http" # optional; default
url = "http://devbox1.tailnet.ts.net:8080"
token = "remote-token"

[[remote_hosts]]
host = "devbox2"
transport = "ssh"
destination = "user@devbox.example.com" # optional; defaults to host
```

Once hosts are configured, running `agentsview source sync` without `--host`
pulls local sources and all configured HTTP and SSH hosts in one coordinated
sync. During a full or automatic data-version rebuild, AgentsView prepares
every remote mirror, bulk-ingests local and remote sources into a temporary
database with FTS updates suspended, rebuilds FTS once, and atomically swaps
the finished archive into place.

Passing `--full` reparses every discovered local and remote session, though it
does not re-transfer unchanged manifest-capable files. Directory-scoped and
verbatim curated content still use delta transfer, while Windsurf's sanitized
curated export transfers as a separate full archive on every sync. HTTP
collectors and spokes must share the same remote-sync protocol version;
incompatible peers fail before exchanging targets or archive data. AgentsView
skips a configured HTTP host if it is offline, cannot resolve through DNS, is
unreachable, or times out, and skips an SSH host when `ssh` reports one of the
network errors listed under [Offline Hosts](/docs/remote-access/#offline-hosts).
Local sources and reachable hosts still join the combined rebuild, and archived
sessions from skipped hosts stay intact. Any other remote preparation or
contributor failure aborts the combined rebuild without replacing the active
archive. The command exits non-zero for every failure except an unavailable
configured host. See [Incremental Sync](/docs/remote-access/#incremental-sync).

HTTP remote sync needs a reachable remote daemon, preferably over a private
network such as Tailscale, and remote archive endpoints always require bearer
auth. Each host needs a `url` and a `token` matching the remote daemon's
`auth_token`; do not reuse the collector daemon's own token for untrusted
remote endpoints. Ad hoc HTTP remotes are not supported. Because remote
sessions are namespaced by host, host names must be unique in the list.

During an HTTP remote sync, the collector prints durable phase lines for
resolving remote roots, fetching and comparing the manifest, transferring and
extracting changed files, planning pending paths, processing affected sources,
rebuilding FTS, and swapping the database. Pending paths can outnumber
processed sources because deletions participate in recovery and cache
invalidation without usually producing an import source. Provider fallback is
named explicitly, and its discovered sources set the processing denominator.

The final per-host line reports changed sessions, ordinary unchanged skips, and
error-suppressed pending entries. A retained-journal line means recovery work
will be replayed. The output reports cancellation, processing failure,
cache-persistence failure, retirement failure, and a rebuild awaiting its
database swap as distinct outcomes. Normal output omits individual source
paths. Archive downloads show live compressed-byte progress whenever the remote
daemon sends a `Content-Length` header. These phases come from the collector,
and you need a spoke upgrade only for manifest-delta transfer. If an upgraded
binary does not show them, restart the local collector daemon.

______________________________________________________________________

<a id="agentsview-prune"></a>

### `agentsview session prune`

Delete sessions that match one or more filters. You must supply at least one
filter.

```bash
agentsview session prune [flags]
```

| Flag              | Default | Description                                         |
| ----------------- | ------- | --------------------------------------------------- |
| `--agent`         |         | Exact agent name, including matching trash          |
| `--project`       |         | Sessions whose project contains this substring      |
| `--max-messages`  | `-1`    | Sessions with at most N user messages               |
| `--before`        |         | Sessions that ended before this date (`YYYY-MM-DD`) |
| `--first-message` |         | Sessions whose first message starts with this text  |
| `--dry-run`       | `false` | Show what would be pruned without deleting          |
| `--yes`           | `false` | Skip confirmation prompt                            |

Filters combine with AND, and a single prune removes matching parents along
with their descendants. Any descendant that fails a filter keeps all of its
ancestors. Without `--agent`, prune skips trash, so a trashed descendant keeps
its ancestors; with `--agent`, prune includes matching sessions in trash and
shows in the summary how many matches came from trash.

Prune keeps deleted sessions out of later syncs. To stop new sessions from that
agent from entering the archive, add it to
[`disabled_agents`](/docs/configuration/#disabling-session-providers).

**Examples:**

```bash
# Preview what would be deleted
agentsview session prune --project "scratch" --dry-run

# Preview cleanup of an agent after stopping the daemon
agentsview session prune --agent mimocode --dry-run

# Delete short sessions from before 2025
agentsview session prune --max-messages 2 --before 2025-01-01

# Delete sessions starting with a specific message
agentsview session prune --first-message "test" --yes

# Combine filters (AND logic)
agentsview session prune --project "old-project" --max-messages 5 --before 2025-06-01
```

Prune deletes each matching session's source file, except the shared `state.db`
used by apps such as Hermes and Augure Desktop, which stays in place. The
summary reports how many sessions were deleted, how much disk space was
reclaimed, and how many source files stayed in place. Run with `--dry-run`
first to preview the matches.

______________________________________________________________________

### `agentsview db adopt-machine`

Assign historical local machine keys to this installation. Run this when an
upgrade could not determine which archived sessions are local, or when you want
to adopt older hostnames you own. Inspect the archive first, then stop the
daemon:

```bash
agentsview db adopt-machine --list
agentsview daemon stop
agentsview db adopt-machine host-a.example host-b.example
agentsview daemon start
```

Run `--list` on its own to print machine keys along with session and
worktree-rule counts. It does not start a daemon, write configuration, or adopt
sessions.

Choose only keys whose sessions belong to this installation. The command moves
their sessions, worktree rules, and project and source metadata to the current
installation ID while keeping session IDs, messages, stars, pins, and other
curation intact. Old named keys remain aliases for filters and URLs. If
worktree rules conflict, the command stops so you can reconcile them before
retrying.

You do not need to touch keys from other installations. Legacy `local` rows
always belong to this archive and are assigned to its installation ID at
startup.

This command records ownership in the archive, whereas changing
`local_machine_name` only changes a display label. For automatic adoption and
mirror updates, see
[Upgrading Historical Machine Keys](/docs/configuration/#upgrading-historical-machine-keys).

______________________________________________________________________

### `agentsview db repair-projects`

Available in 0.47.0 and later, this command previews corrections for temporary
worktree project names using repository URLs already saved in the archive:

```bash
agentsview db repair-projects
agentsview db repair-projects --project sample_feature --format json
agentsview db repair-projects --project sample_feature --apply
```

By default, the command is read-only. Pass `--project` with an exact current
project label, or omit it to inspect every project. Because `--apply` needs
exclusive archive access, stop the daemon or desktop app before running it.
JSON output lists each repository, old and new project names, reason, session
count, and the number of sessions left unresolved because checkout names
conflict. See
[repair worktree project names](/docs/data/#repair-worktree-project-names) for
the naming rules and preservation guarantees.

______________________________________________________________________

### `agentsview db compact`

Reclaim unused space in the local SQLite archive. The command writes and checks
a compacted database before replacing the original, and truncates the
write-ahead log, or WAL. Your stored sessions stay intact, and future imports
keep the same retention behavior.

Before shrinking the file, compaction removes deleted-message records from the
conversation export index, except records for messages with a native source ID,
which stay so a returning message keeps its export ID. The daemon removes those
same records on its own once they are a week or two old, though only compaction
shrinks the database file. Change-stream and conversation-export readers keep
these records only so they can learn about deletions. A reader whose position
predates a removed record restarts: the change stream sends `reset`, and
`export conversations changes` returns `reconciliation_required`. Readers that
have already moved past every removed record continue unchanged. The
`--dry-run` estimate does not count the space these records free.

```bash
agentsview db compact [flags]
```

| Flag            | Default | Description                                      |
| --------------- | ------- | ------------------------------------------------ |
| `--staging-dir` |         | Filesystem location for the staged database      |
| `--dry-run`     | `false` | Report the estimate without changing the archive |
| `--keep-backup` | `false` | Keep the original database backup                |
| `--yes`         | `false` | Skip the confirmation prompt                     |
| `--format`      | `human` | Use `json` for machine-readable output           |

In JSON mode, the command writes only the final result to stdout, requires
`--yes`, and sends prompts and human progress messages to stderr. When a
writable daemon owns the archive, `--staging-dir` is rejected because the
daemon chooses its own staging directory and the compact endpoint is restricted
to localhost. The command checks for an existing daemon and never starts one,
because maintenance must not trigger a daemon's startup sync. If no daemon owns
the archive, the command acquires the direct write lock and compacts in
process.

On a shared filesystem, peak extra space covers the original backup, the
compacted candidate, a second candidate copy beside the live database, and a
safety margin. On separate filesystems, staging needs room for the backup plus
one candidate, while the database filesystem needs room for the installation
copy. The live source stays in place until the final rename, and it is never
counted as free space. For a large archive, stage on a separate volume with
enough capacity:

```bash
agentsview db compact --dry-run
agentsview daemon stop  # required for --staging-dir
agentsview db compact \
  --staging-dir /mnt/cache/data-cache/agentsview-compact \
  --keep-backup --yes
agentsview daemon start
```

If a writable daemon is running, the command sends the compact request to that
daemon, and direct file access is refused while another daemon owns the
archive. Reads continue during the staged build, while writes return a
retryable archive-maintenance error until the verified replacement commits;
connections pause only for the final swap. Compactions are serialized per
archive, so requesting a compaction during a sync, resync, or another
compaction fails right away with a conflict instead of queueing.

If compaction fails or is interrupted partway through, the archive stays safe.
A recovery manifest named `compact-recovery.json` beside the database records
the operation, and the next writable start finishes or rolls back the
replacement before opening the archive. Until that happens, the daemon can keep
refusing writes with an error naming the manifest; restarting the daemon
resolves it. Once a compaction commits, recovery only cleans up leftover
staging files and never replaces the archive, so sessions ingested afterward
stay safe. Do not delete the manifest or database files by hand.

______________________________________________________________________

### `agentsview db strip --images`

Replace supported inline images and offloaded image references in stored tool
results with text descriptions. You must pass `--images` and confirm the
prompt. Provider source files and standalone image files stay untouched. To
measure reclaimed SQLite file space, run `db compact` separately. Changed
sessions are automatically rescanned for secrets so detections match the
remaining content.

Preview the selection with `--dry-run`, then stop the daemon with
`agentsview daemon stop` before applying changes. The CLI writes directly to
the archive, refuses to run while a writable daemon owns it, and does not start
a daemon. Start the daemon again once the command finishes.

```bash
agentsview db strip --images [flags]
```

| Flag        | Default | Description                                                                                                          |
| ----------- | ------- | -------------------------------------------------------------------------------------------------------------------- |
| `--images`  | `false` | Required image cleanup operation                                                                                     |
| `--project` |         | Sessions whose project contains this substring                                                                       |
| `--before`  |         | Before this date (`YYYY-MM-DD`), using end time, then start time, then creation time when earlier fields are missing |
| `--dry-run` | `false` | Preview selected sessions and byte counts                                                                            |
| `--yes`     | `false` | Skip confirmation                                                                                                    |
| `--format`  | `human` | Use `json` for machine-readable output                                                                               |

Applying in JSON mode requires `--yes`. Both `--dry-run` and a declined
confirmation leave the archive unchanged. The reported stored-content bytes and
decoded image bytes measure content size, not reclaimed disk space.

______________________________________________________________________

### `agentsview db migrate --images`

Move retained inline tool-result image payloads from currently stored archive rows into
the asset store at `{dataDir}/assets`. AgentsView writes each payload as a
content-addressed file named `<sha256hex><ext>`. Before updating a row, it
verifies that any existing object matches the expected byte count and SHA-256
digest, replacing a missing or corrupt object while the source bytes are still
available. It then replaces the inline `input_image` block with an
`agentsview_image` placeholder whose `image_ref` field points to the `asset://`
reference. `GET /api/v1/assets/{filename}` and `renderMarkdown` resolve those
references from the local assets directory.

Migrated images display in formatted tool output alongside text and other
supported inline or offloaded images. Raw mode still shows the stored text,
including the markdown reference, as does any result that keeps an unmigrated
image block, such as an SVG or an `image/bmp` payload next to a migrated PNG.
When `require_auth` is enabled, the asset route rejects browser image requests
because an `img` element sends no `Authorization` header, the same limit that
chat-imported images already have.

Migration updates images already in the archive. To store future supported
images in the same asset directory, set `tool_result_images = "offload"` and
restart the daemon. With `keep`, a later reparse or full resync can restore
inline images from provider source files; with `drop`, ingestion and full
resync replace images with text descriptions. `db strip --images` also removes
offloaded references from selected results, though neither operation deletes
asset files. You must pass `--images`, and the command never modifies provider
source files. A separate serving host needs the matching `{dataDir}/assets`
directory along with the copied database. Run `db compact` separately to
measure SQLite file-space reclamation after migration, and back up
`{dataDir}/assets` together with the archive.

The raw `agentsview session export` command streams provider source with
detected secrets masked unless you pass `--reveal`, and keeps inline image
payloads. For export and remote-backend limits, see
[image storage](/docs/data/#ingest-time-image-offload).

If a session transaction fails after writing assets, its rows stay unchanged
while complete, unreferenced asset files remain on disk. Retrying migration
reuses matching files, and there is no automatic cleanup of unreferenced
assets. Both `db migrate --images` and `db strip --images` report the sessions
that committed before a later failure.

JPEG assets now use `.jpg` filenames, including `.jpeg` files copied from chat
imports. Existing `asset://<hash>.jpeg` references still resolve, though
re-importing an export with those files can write a second copy under `.jpg`.

Migration moves only the four passive image media types: `image/png`,
`image/jpeg`, `image/webp`, and `image/gif`. Every other payload stays inline,
including SVG, which the serving route rejects as active content, and
near-misses such as the non-canonical `image/jpg` spelling. `db strip --images`
is broader and replaces any `image/*` payload with a placeholder, so the two
commands do not select the same rows.

Preview with `--dry-run`, then run `agentsview daemon stop` before applying
changes. The CLI writes directly to the archive, refuses while a writable
daemon owns it, and does not start a daemon. Restart the daemon after the
command finishes.

```bash
agentsview db migrate --images --dry-run
agentsview daemon stop
agentsview db migrate --images
agentsview daemon start
```

| Flag        | Default | Description                                                                                                          |
| ----------- | ------- | -------------------------------------------------------------------------------------------------------------------- |
| `--images`  | `false` | Required image migration operation                                                                                   |
| `--project` |         | Sessions whose project contains this substring                                                                       |
| `--before`  |         | Before this date (`YYYY-MM-DD`), using end time, then start time, then creation time when earlier fields are missing |
| `--dry-run` | `false` | Preview selected sessions and byte counts                                                                            |
| `--yes`     | `false` | Skip confirmation                                                                                                    |
| `--format`  | `human` | Use `json` for machine-readable output                                                                               |

Applying in JSON mode requires `--yes`. Both `--dry-run` and a declined
confirmation leave the archive and assets directory unchanged. The reported
stored-content bytes and decoded image bytes measure content size, not
reclaimed disk space.

______________________________________________________________________

### `agentsview version`

Print the version, git commit, and build date. Pass `--json` for a stable,
machine-readable response that works without a running daemon, configuration,
or database.

```bash
agentsview version
agentsview version --json
agentsview version --format json
```

```
agentsview v0.38.0 (commit 5b42bf1c, built 2026-07-13T15:37:17Z)
```

```json
{
  "schema_version": 1,
  "name": "agentsview",
  "version": "v0.38.0",
  "commit": "5b42bf1c",
  "build_date": "2026-07-13T15:37:17Z"
}
```

The JSON contract uses these fields:

| Field            | Type    | Meaning                                      |
| ---------------- | ------- | -------------------------------------------- |
| `schema_version` | integer | Version of this JSON contract; currently `1` |
| `name`           | string  | Canonical tool name, always `agentsview`     |
| `version`        | string  | Build version                                |
| `commit`         | string  | Source commit recorded at build time         |
| `build_date`     | string  | UTC build timestamp, or an empty string      |

Consumers should require the expected `schema_version` and ignore unknown
fields. Adding an optional field does not require a schema bump; removing or
renaming a field, changing a field's type or meaning, or making a previously
valid response invalid does.

______________________________________________________________________

<a id="agentsview-agents"></a>

### `agentsview source list`

List every agent AgentsView can read, where it looks for each agent's sessions,
and which session details each parser extracts. Pass one or more agent types to
filter the output. This command works without a running daemon, configuration,
or database.

```bash
agentsview source list
agentsview source list codex claude
agentsview source list --json
```

```
codex (Codex)
  sessions: ~/.codex/sessions, ~/.codex/archived_sessions
  override: CODEX_SESSIONS_DIR
  reads: first_message, session_name, cwd, git_branch, relationships, subagents, thinking, tool_calls, tool_results, tool_result_events, per_message_token_usage, termination_status, model
```

The `sessions` line lists default locations relative to your home directory,
and `override` names the environment variable that replaces them. Import-only
agents show `imported with agentsview source import` instead.

The JSON contract includes `schema_version`, currently `1`, and an `agents`
array. Each agent has these fields:

| Field           | Type    | Meaning                                                                           |
| --------------- | ------- | --------------------------------------------------------------------------------- |
| `type`          | string  | Agent type used by `--agent` filters and session IDs                              |
| `display_name`  | string  | Human-readable agent name                                                         |
| `import_only`   | boolean | Sessions arrive only through `agentsview source import`                           |
| `file_based`    | boolean | Sessions are files rather than an application database                            |
| `default_dirs`  | array   | Default session locations, relative to the home directory                         |
| `data_dir_root` | string  | Location under the AgentsView data directory, for agents AgentsView stores itself |
| `env_var`       | string  | Environment variable that overrides `default_dirs`                                |
| `content`       | object  | Session detail name mapped to `supported`, `unsupported`, or `not_applicable`     |

New content details may appear without a schema bump, so consumers should ignore
names they do not recognize.

______________________________________________________________________

<a id="agentsview-usage-daily"></a>

### `agentsview report usage daily`

Report token usage and estimated cost grouped by local-time day, covering the
last 30 days by default. For full reporting behavior and agent coverage, see
[Token Usage & Costs](/docs/token-usage/).

This report reads committed archive data. If it starts the daemon, session sync
runs in the background; to include fresh source changes right away, run
`agentsview source sync` first. When an older archive needs reparsing, the
daemon upgrades it before serving and prints startup progress in your terminal.
A cold usage cache prepares only the sessions needed for the report instead of
waiting for the full archive backfill. Slow reports write the current
preparation phase and elapsed time to stderr, even with `--json`, and usage
preparation is exempt from the server's normal write timeout. Press `Ctrl+C` to
stop waiting; shared cache work can continue in the daemon.

After upgrading, restart older daemons so they expose the usage progress
endpoint. `usage statusline` still waits for initial sync, including when it
reuses a daemon started by daily usage, and caps the combined wait and report
request at 30 seconds.

Offline reads require an archive at the current data version. If an upgrade
needs a resync, run `agentsview daemon restart` and wait for the resync to
finish before retrying the offline command.

```bash
agentsview report usage daily [flags]
```

| Flag          | Default       | Description                                                              |
| ------------- | ------------- | ------------------------------------------------------------------------ |
| `--format`    | `human`       | Output format: `human` or `json`                                         |
| `--json`      | `false`       | Alias for `--format json`                                                |
| `--since`     | `30 days ago` | Start of window, a duration like `28d` or a `YYYY-MM-DD` date, inclusive |
| `--until`     |               | End of window, a duration like `28d` or a `YYYY-MM-DD` date, inclusive   |
| `--all`       | `false`       | Scan all history; overrides the default 30-day window                    |
| `--agent`     |               | Filter by agent name                                                     |
| `--breakdown` | `false`       | Show per-model rows and populate detailed JSON breakdown arrays          |
| `--offline`   | `false`       | Read the archive directly without sync or pricing fetches                |
| `--no-sync`   | `false`       | Skip source refresh; a new daemon starts without automatic sync          |
| `--timezone`  | system        | IANA timezone name for date bucketing                                    |

When the window includes Codex Cloud sessions that have only plan usage, the
report leaves them out of the totals and prints "note: N sessions billed as plan
usage, not priced, are left out of these totals." Sessions whose turns were
captured only in part add a second note that their token counts are low. With
`--json`, the counts are `planOnlySessions` and `partlyCapturedSessions`.

**Examples:**

```bash
agentsview report usage daily                           # last 30 days
agentsview report usage daily --all                     # full history
agentsview report usage daily --since 14d               # last 14 days
agentsview report usage daily --since 2026-04-01 --breakdown
agentsview report usage daily --json --agent claude
```

______________________________________________________________________

<a id="agentsview-usage-statusline"></a>

### `agentsview report usage statusline`

Print today's total estimated cost as a single line, for shell prompts and tmux
status lines.

```bash
agentsview report usage statusline [flags]
```

| Flag        | Default | Description                                      |
| ----------- | ------- | ------------------------------------------------ |
| `--format`  | `human` | Output format: `human` or `json`                 |
| `--json`    | `false` | Alias for `--format json`                        |
| `--agent`   |         | Filter by agent name                             |
| `--offline` | `false` | Read the archive without sync or pricing fetches |
| `--no-sync` | `false` | Skip source refresh                              |

**Example:**

```bash
$ agentsview report usage statusline
$9.61 today
```

```bash
$ agentsview report usage statusline --json
{
  "date": "2026-08-04",
  "cost": {
    "microdollars": 9610000
  }
}
```

See [Token Usage & Costs](/docs/token-usage/#agentsview-usage-statusline) for
Starship and tmux integration examples.

______________________________________________________________________

<a id="agentsview-usage-cursor"></a>

### `agentsview source cursor sync-usage`

Fetch Cursor Admin API usage events and store them in the local archive so they
appear in the Usage dashboard and daily reports.

```bash
agentsview source cursor sync-usage [flags]
```

| Flag          | Default       | Description                          |
| ------------- | ------------- | ------------------------------------ |
| `--since`     | `30 days ago` | Start date (`YYYY-MM-DD`), inclusive |
| `--until`     | today         | End date (`YYYY-MM-DD`), inclusive   |
| `--all`       | `false`       | Include all history                  |
| `--page-size` | `100`         | Events requested per Cursor API page |
| `--email`     | config        | Filter by Cursor team member email   |
| `--user-id`   | config        | Filter by Cursor team member user ID |

Cursor allows 60 requests per minute to this API per team, shared across every
tool using the team's admin keys. The command spaces requests, including
retries, at least one second apart. If Cursor responds with
`429 Too Many Requests` or a server error, the command waits and requests the
same page again, up to 5 total attempts per page. It honors Cursor's
`Retry-After` header up to 60 seconds, or backs off starting around 2 seconds
when that header is missing, and prints each wait to stderr. Any other error,
such as a rejected key, stops the run immediately. Nothing is stored unless
every page is fetched, so a larger `--page-size`, which needs fewer requests,
makes a long sync less likely to fail.

**Examples:**

```bash
agentsview source cursor sync-usage
agentsview source cursor sync-usage --since 2026-05-01 --until 2026-05-31
agentsview source cursor sync-usage --all --email you@example.com
```

For setup and reporting behavior, see
[Cursor Admin Usage Events](/docs/token-usage/#cursor-admin-usage-events).

______________________________________________________________________

<a id="agentsview-activity-report"></a>

### `agentsview report activity`

Report active time, concurrency, cost, tokens, breakdowns, and session rows for
a resolved date range, using the same report model as the web UI's
[Activity](/docs/activity/) page.

```bash
agentsview report activity [flags]
```

| Flag         | Default   | Description                                           |
| ------------ | --------- | ----------------------------------------------------- |
| `--preset`   |           | Range preset: `day`, `week`, `month`, or `custom`     |
| `--date`     | today     | Anchor date for day/week/month presets (`YYYY-MM-DD`) |
| `--from`     |           | Start instant for custom range (RFC3339)              |
| `--to`       |           | End instant for custom range (RFC3339)                |
| `--timezone` | system    | IANA timezone for range bucketing                     |
| `--bucket`   | automatic | Bucket size: `5m`, `15m`, `1h`, `1d`, or `1w`         |
| `--project`  |           | Filter by project                                     |
| `--agent`    |           | Filter by agent name                                  |
| `--machine`  |           | ID, alias, ID/alias list, or unique display name      |
| `--format`   | `human`   | Output format: `human` or `json`                      |
| `--json`     | `false`   | Alias for `--format json`                             |
| `--no-sync`  | `false`   | Skip on-demand sync before querying                   |
| `--offline`  | `false`   | Use fallback pricing only                             |

**Examples:**

```bash
agentsview report activity --preset day --date 2026-06-20
agentsview report activity --preset week --date 2026-06-20 --json
agentsview report activity --preset custom \
  --from 2026-06-20T14:00:00Z \
  --to 2026-06-20T18:00:00Z \
  --bucket 15m
```

Human output prints totals, peak concurrency, top project, model, and agent
breakdowns, and top sessions. JSON output includes the dense bucket timeline
and session rows used by the web UI.

______________________________________________________________________

### `agentsview token-use`

!!! note "Deprecated"

    Starting with 0.30.0, `agentsview token-use` is a deprecated alias for
    [`agentsview session usage`](/docs/session-api/#agentsview-session-usage).
    Both commands take the same `<session-id>` argument, and `token-use` always
    emits the same JSON shape as `session usage --format json`, now extended
    with a cost estimate. Use `agentsview session usage` in new scripts.

Print machine-readable token usage data and a cost estimate for a single
session.

```bash
agentsview token-use <session-id>
```

Session ID format depends on the agent. For example, Claude root sessions
usually use UUIDs like `550e8400-e29b-41d4-a716-446655440000`, Claude subagents
use IDs like `agent-a86574e`, and some other agents add prefixes such as
`codex:my-session-id`. You can also pass raw session IDs emitted by the
underlying agent when AgentsView can resolve them to the canonical stored
session.

This command talks to a compatible local daemon, starting one if needed, and
follows the same refresh and missing-session behavior as
[`agentsview session usage`](/docs/session-api/#agentsview-session-usage).

Totals include archived subagent spend, and `token-use` has no opt-out. For
own-session numbers only, run `agentsview session usage <id> --own-only`.

**Example:**

```bash
agentsview token-use 550e8400-e29b-41d4-a716-446655440000
```

```json
{
  "session_id": "550e8400-e29b-41d4-a716-446655440000",
  "agent": "claude",
  "project": "my-project",
  "total_output_tokens": 15230,
  "peak_context_tokens": 84000,
  "has_token_data": true,
  "cost": {"microdollars": 2410000},
  "has_cost": true,
  "cost_usd": 2.41,
  "models": ["claude-opus-4-7"],
  "server_running": false
}
```

`cost_usd` is a deprecated compatibility alias for `cost.microdollars / 1e6`
and is scheduled for removal in a future release. Read `cost.microdollars`
directly in new scripts.

For the full field reference and exit-code contract, see
[`agentsview session usage`](/docs/session-api/#agentsview-session-usage).

______________________________________________________________________

<a id="agentsview-pg-push"></a>

### `agentsview replica pg push`

Sync sessions from local SQLite to PostgreSQL. For full documentation, see
[PostgreSQL Sync](/docs/pg-sync/).

```bash
agentsview replica pg push [target] [flags]
```

| Flag                 | Default | Description                                                                                                        |
| -------------------- | ------- | ------------------------------------------------------------------------------------------------------------------ |
| `--full`             | `false` | Force full local resync and re-push                                                                                |
| `--no-vectors`       | `false` | Skip the semantic-search vector phase for this run                                                                 |
| `--embed`            | `false` | Push sessions first, then build and push vectors; watch builds after syncs and pushes vectors on the next interval |
| `--projects`         |         | Comma-separated projects to push (inclusive)                                                                       |
| `--exclude-projects` |         | Comma-separated projects to exclude from push                                                                      |
| `--all-projects`     | `false` | Ignore configured project filters for this run                                                                     |
| `--all`              | `false` | Push every configured PostgreSQL target sequentially                                                               |
| `--watch`            | `false` | Run continuously, pushing on change plus a periodic floor                                                          |
| `--no-start-daemon`  | `false` | With `--watch`, require a writable daemon; exit if none is running; incompatible with `--embed`                    |
| `--debounce`         | `30s`   | Coalesce window after a change before pushing (`--watch` only)                                                     |
| `--interval`         | `15m`   | Periodic floor push interval (`--watch` only)                                                                      |

For details on how project filtering interacts with the push watermark, see
[PostgreSQL Sync — Project Filtering](/docs/pg-sync/#project-filtering).

Passing `--embed` pushes sessions first, then builds and pushes embeddings in
the same run using `[vector]` or a recipe published to PostgreSQL. Adding
`--full --embed` repairs vector rows as well. In watch mode, it builds in the
background after each sync that commits changes and pushes the built vectors on
the next interval. `--embed` never starts a daemon, refuses to run while a
daemon owns the archive, and cannot be combined with `--no-vectors`, `--all`,
or a target with `push_vectors = false`. See
[Build and push embeddings in a container](/docs/pg-sync/#build-and-push-embeddings-in-a-container).

______________________________________________________________________

<a id="agentsview-pg-status"></a>

### `agentsview replica pg status`

Show PostgreSQL sync status.

```bash
agentsview replica pg status [target] [flags]
```

| Flag                 | Default | Description                                                      |
| -------------------- | ------- | ---------------------------------------------------------------- |
| `--all`              | `false` | Show status for every configured PostgreSQL target               |
| `--projects`         |         | Comma-separated projects whose push status to show               |
| `--exclude-projects` |         | Comma-separated excluded projects whose push status to show      |
| `--all-projects`     | `false` | Ignore configured project filters for this status                |
| `--max-stale`        |         | Exit non-zero when the push watcher has stalled longer than this |

______________________________________________________________________

<a id="agentsview-pg-serve"></a>

### `agentsview replica pg serve`

Start a read-only session web UI backed by PostgreSQL. On a writable PostgreSQL
schema, the same server also registers the
[hosted raw-sync control plane](/docs/hosted-raw-sync/#http-control-plane). For
full server documentation, see [PostgreSQL Sync](/docs/pg-sync/).

```bash
agentsview replica pg serve [flags]
```

```bash
agentsview replica pg serve --push-watch
```

Pass `--push-watch` to ingest mounted sessions and publish them in the same
process; see
[PostgreSQL Sync](https://agentsview.io/docs/pg-sync/#agentsview-pg-serve) for
setup and shutdown details.

This command accepts the same serve flags, such as `--host`, `--port`, and
`--proxy`, plus PostgreSQL configuration from `config.toml`. When the host's
`[vector]` config matches a generation pushed to PostgreSQL, semantic and
hybrid search run on pgvector. See
[Semantic Search — PostgreSQL](/docs/semantic-search/#postgresql).

______________________________________________________________________

<a id="agentsview-pg-service"></a>

### `agentsview replica pg service`

Install and manage the PostgreSQL auto-push service, which runs
`agentsview replica pg push --watch` in the background using launchd on macOS
or `systemd --user` on Linux. For setup notes, see
[PostgreSQL Sync — `agentsview replica pg service`](/docs/pg-sync/#agentsview-pg-service).

If you run your own collector, install with `service install --no-start-daemon`.
The push service then exits when no writable daemon is running, and the service
manager restarts it until the collector responds. Reinstalling restarts the
service with the new arguments. The default installation still starts a missing
daemon, and reinstalling without `--no-start-daemon` restores daemon autostart.

```bash
agentsview replica pg service install
agentsview replica pg service status
agentsview replica pg service logs [-f]
agentsview replica pg service start
agentsview replica pg service stop
agentsview replica pg service uninstall
```

| Command     | Description                                               |
| ----------- | --------------------------------------------------------- |
| `install`   | Generate the service unit, enable it, and start it        |
| `status`    | Show the service-manager status plus last successful push |
| `logs -f`   | Follow `pg-watch.log` under the AgentsView data directory |
| `start`     | Start the installed service                               |
| `stop`      | Stop the installed service                                |
| `uninstall` | Stop and remove the service unit                          |

______________________________________________________________________

<a id="agentsview-pg-vectors"></a>

### `agentsview replica pg vectors`

Inspect and drop semantic-search embedding generations stored in PostgreSQL. See
[Semantic Search — Maintenance](/docs/semantic-search/#maintenance) for details.

```bash
agentsview replica pg vectors list [flags]
agentsview replica pg vectors drop <id> [flags]
```

| Command     | Description                                                                              |
| ----------- | ---------------------------------------------------------------------------------------- |
| `list`      | List generations with model, dimension, document/chunk counts, and contributing machines |
| `drop <id>` | Drop a generation and all of its embeddings (prompts for confirmation)                   |

| Flag       | Default | Description                                             |
| ---------- | ------- | ------------------------------------------------------- |
| `--target` |         | PG target name (default: the default configured target) |
| `--yes`    | `false` | Skip the confirmation prompt (`drop` only)              |

______________________________________________________________________

<a id="agentsview-clickhouse"></a>

### `agentsview replica clickhouse`

Push the local SQLite archive to ClickHouse and serve the read-only web UI from
it. For full documentation, see [ClickHouse Sync](/docs/clickhouse-sync/).

```bash
agentsview replica clickhouse push [target] [flags]
agentsview replica clickhouse status [target] [flags]
agentsview replica clickhouse serve [flags]
agentsview replica clickhouse service install
```

`clickhouse push` accepts the same `--full` / `--no-vectors` / `--projects` /
`--exclude-projects` / `--all-projects` / `--all` / `--watch` / `--debounce` /
`--interval` flags as `pg push` and includes a semantic-search vector phase.
The only `pg push` flag `clickhouse push` lacks is `--embed`, and
`--all --watch` is rejected.

`clickhouse serve` accepts the same serve flags as `pg serve`.
`clickhouse service` installs `clickhouse push --watch` as a launchd or systemd
user unit and writes `clickhouse-watch.log`. If you run your own collector,
install with `clickhouse service install --no-start-daemon`.

______________________________________________________________________

<a id="agentsview-projects"></a>

### `agentsview project list`

List all projects in the local database with their session counts.

```bash
agentsview project list [flags]
```

| Flag       | Default | Description                      |
| ---------- | ------- | -------------------------------- |
| `--format` | `human` | Output format: `human` or `json` |
| `--json`   | `false` | Alias for `--format json`        |

**Examples:**

```bash
agentsview project list         # tabular output
agentsview project list --json   # JSON array
```

______________________________________________________________________

<a id="agentsview-health"></a>

### `agentsview session health`

Inspect session intelligence in a human-friendly CLI view. See
[Session Intelligence](/docs/session-intelligence/) for the scoring and signal
model.

```bash
agentsview session health [session-id] [flags]
```

| Flag       | Default | Description                                            |
| ---------- | ------- | ------------------------------------------------------ |
| `--format` | `human` | Output format: `human` or `json`                       |
| `--json`   | `false` | Alias for `--format json`                              |
| `--limit`  | `20`    | Number of sessions to list when no session ID is given |

**Examples:**

```bash
agentsview session health
agentsview session health --limit 50
agentsview session health 550e8400-e29b-41d4-a716-446655440000
agentsview session health agent-a86574e --json
```

Without a session ID, the command lists recent sessions with their grade and
outcome columns. Pass a session ID to print the detailed signal counts for that
session.

______________________________________________________________________

<a id="agentsview-stats"></a>

### `agentsview report stats`

View experimental, window-scoped workspace analytics across sessions and git
activity. For the full write-up, see [Stats](/docs/stats/).

```bash
agentsview report stats [flags]
```

| Flag                | Default | Description                                                           |
| ------------------- | ------- | --------------------------------------------------------------------- |
| `--format`          | `human` | Output format: `human` or `json`                                      |
| `--json`            | `false` | Alias for `--format json`                                             |
| `--since`           | `28d`   | Start of window, either `YYYY-MM-DD` or a compact duration like `28d` |
| `--until`           |         | End of window as `YYYY-MM-DD`                                         |
| `--agent`           | `all`   | Restrict to one agent or use `all`                                    |
| `--include-project` |         | Repeatable project allowlist                                          |
| `--exclude-project` |         | Repeatable project blocklist                                          |
| `--timezone`        | local   | Timezone used for temporal reporting                                  |

**Examples:**

```bash
agentsview report stats
agentsview report stats --format json --since 2026-04-01 --until 2026-04-15
agentsview report stats --agent claude --include-project agentsview
```

This command is experimental: the exact human output can change, and the JSON
format is not stable yet even though it currently carries `schema_version: 1`.

______________________________________________________________________

### `agentsview doctor sync`

Collect read-only diagnostics for startup sync decisions without creating or
migrating config files. The command loads your current config read-only,
inspects the SQLite archive, checks configured agent roots, and prints recent
sync-related lines from `debug.log`.

If startup rotated the log, check `debug.log.1` in your data directory for the
previous tail.

```bash
agentsview doctor sync
```

The report includes:

- data directory and SQLite database path
- whether the database exists and is readable
- SQLite `user_version` and the binary data version
- the startup sync decision
- session counts by stored data version
- leftover resync temp files
- configured/default agent roots and whether each exists
- recent debug lines mentioning sync, data versions, warnings, or failures
- Antigravity CLI summary-mode counts and Antigravity sessions decoded from
    unrecognized `agy-schema:` fingerprints
- a likely-cause summary when startup sync behavior looks abnormal

______________________________________________________________________

<a id="agentsview-parse-diff"></a>

### `agentsview archive diff`

Validate parser changes against the real session archive already in your local
SQLite database. The command re-parses source files with the current binary,
normalizes them through the sync path, and reports field-level differences from
the stored rows without writing updated session data.

```bash
agentsview archive diff [flags]
```

| Flag               | Default | Description                                                             |
| ------------------ | ------- | ----------------------------------------------------------------------- |
| `--agent`          |         | Restrict to one agent; repeatable                                       |
| `--limit`          | `0`     | Maximum number of source files to inspect, newest first (`0` means all) |
| `--fail-on-change` | `false` | Exit non-zero when changes or parse errors are found                    |
| `--format`         | `human` | Output format: `human` or `json`                                        |
| `--json`           | `false` | Alias for `--format json`                                               |
| `--verbose` / `-v` | `false` | Include more detail in the human report                                 |

**Examples:**

```bash
agentsview archive diff --agent claude --limit 100
agentsview archive diff --agent codex --fail-on-change
agentsview archive diff --json > parser-report.json
```

`parse-diff` is built for parser development and release QA. Run it against a
quiet, freshly synced archive for the cleanest signal. It skips import-only
sources because there is no source file to re-parse, and covers provider-backed
stores with authoritative local sources, including Warp, Forge, Piebald, and
Devin, alongside normal file-backed agents.

The report separates parser drift from comparison-basis skew:

- `raced` means the source changed while `parse-diff` was running. It appears
  in the report for review, but does not fail `--fail-on-change`.
- `incremental_skew` means an incremental-append sync last wrote the stored row, so
  a fresh full re-parse can legitimately differ on append-path metadata. It
  also appears in the report without failing `--fail-on-change`.
- `pending_resync` means the stored data version is behind the running binary;
  sync reparses available stale sources and rebuilds the archive when needed.

If the report shows `incremental_skew`, run a full resync before treating the
archive as a clean parser-drift baseline. A full resync rewrites those rows
through the normalization path so `parse-diff` can check them strictly.

______________________________________________________________________

<a id="agentsview-chrome-setup"></a>

### `agentsview source chrome setup`

Install the native host and unpacked extension so you can sync Claude.ai,
ChatGPT, Gemini Apps, and Grok web chats through Google Chrome. The command
prints the extension folder path. Because AgentsView opens its Chrome
connection only after setup, restart AgentsView if it is already running.

1. Open `chrome://extensions` in Google Chrome.
1. Enable **Developer mode**.
1. Click **Load unpacked** and select the printed extension folder.

Keep Chrome open during Sync. Registration is per OS user, the last setup wins,
and AgentsView uses the first connected Chrome profile. If your executable path
or data directory changes, rerun setup and reload the extension. After an
upgrade mismatch, restart AgentsView if you upgraded it, rerun setup, reload
the extension at `chrome://extensions`, and run Sync again.

For connection and removal steps, see
[Sync Claude.ai chats](https://agentsview.io/docs/chat-import/#sync-claudeai-chats).

______________________________________________________________________

<a id="agentsview-import"></a>

### `agentsview source import`

Import Claude.ai, ChatGPT, Gemini Apps, or Grok web conversations into your
local database. For full documentation, see [Chat Import](/docs/chat-import/).

```bash
agentsview source import --type <type> <path>
agentsview source import --type <type> --sync
```

| Flag        | Default | Description                                                                                                                              |
| ----------- | ------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `--type`    |         | Import type: `claude-ai`, `chatgpt`, or `gemini-apps`; with `--sync`, also `grok-web` (required)                                         |
| `--replace` |         | Session ID to replace when the default import refuses it; repeatable (claude-ai, chatgpt)                                                |
| `--sync`    | `false` | Sync Claude.ai, ChatGPT, Gemini Apps, or Grok web through Chrome; requires a running server and Chrome host, with no path or `--replace` |

Set up the
[Chrome host](https://agentsview.io/docs/commands/#agentsview-chrome-setup)
once and keep Chrome open when running
`agentsview source import --type <type> --sync`, where `<type>` is `claude-ai`,
`chatgpt`, `gemini-apps`, or `grok-web`. Because AgentsView imports Grok web
chats only through Sync, `grok-web` requires `--sync`.

The `<path>` argument can be a `.zip` file, a Claude.ai `conversations.json`
file, a Gemini Apps `MyActivity.html` file, or a directory containing the
extracted export.

**Examples:**

```bash
agentsview source import --type claude-ai ~/Downloads/claude.zip
agentsview source import --type chatgpt ~/Downloads/chatgpt.zip
agentsview source import --type claude-ai ./conversations.json
agentsview source import --type gemini-apps ~/Downloads/takeout.zip
agentsview source import --type grok-web --sync
agentsview source import --type chatgpt --replace 'chatgpt:<conversation-id>' ~/Downloads/chatgpt.zip
```

______________________________________________________________________

### `agentsview export sessions`

Export content-free session summaries from the local archive. See
[Session Export](/docs/session-export/) for the full JSON/NDJSON contract,
cursor semantics, pricing provenance, and project identity rules.

```bash
agentsview export sessions [flags]
```

| Flag                  | Default | Description                                               |
| --------------------- | ------- | --------------------------------------------------------- |
| `--format`            | `json`  | Output format: `json` or `ndjson`                         |
| `--limit`             | `500`   | Maximum sessions to return; max `db.MaxSessionLimit`      |
| `--cursor`            |         | Opaque cursor from a previous response                    |
| `--all`               | `false` | Export every eligible page as one output stream           |
| `--wait`              | `false` | Wait for project identification                           |
| `--project`           |         | Filter by exact project name                              |
| `--exclude-project`   |         | Exclude sessions from the given project                   |
| `--machine`           |         | ID, alias, ID/alias list, or unique display name          |
| `--git-branch`        |         | Filter by project/branch token                            |
| `--agent`             |         | Filter by agent name                                      |
| `--date`              |         | Include sessions active on `YYYY-MM-DD`                   |
| `--date-from`         |         | Include sessions active on or after `YYYY-MM-DD`          |
| `--date-to`           |         | Include sessions active on or before `YYYY-MM-DD`         |
| `--active-since`      |         | Include sessions active since an RFC3339 timestamp        |
| `--min-messages`      | `0`     | Minimum total message count                               |
| `--max-messages`      | `0`     | Maximum total message count                               |
| `--min-user-messages` | `0`     | Minimum user message count                                |
| `--include-one-shot`  | `false` | Include one-shot sessions, which are excluded by default  |
| `--include-automated` | `false` | Include automated sessions, which are excluded by default |
| `--include-children`  | `false` | Include subagent/child sessions                           |
| `--outcome`           |         | Comma-separated outcome filter                            |
| `--health-grade`      |         | Comma-separated health grade filter                       |
| `--min-tool-failures` |         | Minimum tool-failure signal count                         |
| `--has-secret`        | `false` | Only sessions with detected secret leaks                  |

When you pass `--wait`, export starts or finds the local daemon, waits for
project identity evidence to finish, and writes progress to standard error.
Cancel the command to stop waiting. If the backfill fails or the daemon is
unavailable, the error includes recovery commands. Archive-only databases
cannot run the backfill; finish it on the writable source and restore an
archive that already contains the evidence.

Because one-shot and automated sessions are excluded by default, token and cost
totals can be lower than the full archive unless you pass the corresponding
include flags.

**Examples:**

```bash
agentsview export sessions --format json
agentsview export sessions --format ndjson --limit 100
agentsview export sessions --all --format ndjson --project agentsview
```

The JSON top level includes `schema_version`, `archive_id`, `database_id`,
`cursor`, `pricing`, `projects`, and `sessions`. NDJSON writes that same
metadata on the first line, followed by one session row per line. Current
builds emit `schema_version: 7`; for the v1 and transitional 0.38 release
history, see [Session Export](/docs/session-export/#versioning). Both the
default and maximum page size are `db.MaxSessionLimit`, currently 500.

To profile performance, `export sessions`, `export hour`, `export day`, and
`export digest` accept `--cpuprofile <file>`, `--memprofile <file>`, and
`--trace <file>`, writing profiling data to files while keeping the export on
stdout.

When you pass `--cursor`, you can combine it only with `--format`, `--json`,
`--limit`, `--wait`, and the profiling flags. With a cursor, `--wait`
suppresses progress output so stderr stays machine-readable. Cursor reset
errors write structured JSON to stderr, leave stdout empty, and exit with
code 4:

```json
{"error":"cursor_reset","message":"session export cursor is no longer valid; restart the export","database_id":"..."}
```

______________________________________________________________________

### `agentsview export conversations`

List conversation changes without message text, then fetch selected visible
user and assistant messages in bounded chunks. For identity, content,
checkpoint, and coverage rules, see
[Conversation Export](/docs/conversation-export/).

```bash
agentsview export conversations changes
agentsview export conversations changes --checkpoint SAVED_CHECKPOINT
agentsview export conversations changes --tool-summaries
agentsview export conversations message SESSION_ID MESSAGE_ID \
  --database-id DATABASE_ID --revision REVISION
agentsview export conversations session SESSION_ID --text-mode visible
```

`session` exports one complete session from a single archive snapshot as JSON
lines; see
[Read one whole session](/docs/conversation-export/#read-one-whole-session).
These commands read the local archive and do not send content to another
system. Pass `--tool-summaries` to include each session's tool call totals and
skills in the listing; see
[Session tool summaries](/docs/conversation-export/#session-tool-summaries).
`message` and `session` mask detected secrets by default and mark redacted
bodies with `redacted: true`; pass `--reveal` to return the stored text
unchanged. Existing session summary and activity exports stay content-free.

______________________________________________________________________

### `agentsview export hour|day|digest|range`

Export hourly activity and usage, complete days, or digests that identify
changed hours from your local SQLite archive. A digest is a checksum of the
exported content. For JSON fields, version rules, and the correction workflow,
see [Reporting Export](/docs/reporting-export/).

```bash
agentsview export range
agentsview export hour 2026-07-28-13
agentsview export day 2026-07-28
agentsview export day --schema-version 5 --bucket 1m 2026-07-28
agentsview export day --schema-version 5 --by-machine 2026-07-28
agentsview export digest --from 2026-06-28 --to 2026-07-27
```

Hour and date keys must be exact, zero-padded UTC values, and open or future
hours are rejected. The current UTC date includes only closed hours and has no
day digest. Digest ranges include both endpoints and can span at most 31 days.

Version 3 is the default. Pass `--schema-version 5` to group activity and usage
by project, agent, model, and activity category together while keeping
repositories that share a project name separate. Version 5 also accepts
repeatable `--project-key` filters and a `--bucket` duration such as `1m`,
`5m`, or `15m`, which must be a positive whole-minute divisor of one hour;
version 3 rejects both options. Versions 1, 2, and 4 are no longer available.
Validate your chosen `schema_version` and content digest before accepting a
document, and refresh saved digests whenever you change versions.

______________________________________________________________________

### `agentsview session`

Access session data programmatically from scripts, automation agents, and CI
jobs. For full documentation, including stability guarantees, transport
auto-detection, and every subcommand, see [Session API](/docs/session-api/).

Machine filters accept exact IDs, aliases, comma-separated ID or alias lists,
or a unique whole display label. A valid 32-hex installation ID takes
precedence even when it has no stored sessions, while unknown or ambiguous
names return an error with choices.

```bash
agentsview session get <id>              # metadata + signals
agentsview session list [flags]          # filtered list
agentsview session list --resume         # recently-active resume table
agentsview session messages <id>         # paginated messages
agentsview session compactions <id>      # compaction summaries
agentsview session compactions --active-since 2024-01-01T00:00:00Z --json
agentsview session commits <id>          # commits and their prompts
agentsview session tool-calls <id>       # flat tool-call list
agentsview session export <id>           # stream raw source, secrets masked
agentsview session sync <path-or-id>     # parse + insert
agentsview session watch <id>            # NDJSON event stream
agentsview session search <pattern>      # content search across sessions
agentsview session diagnose <id> --format json  # archived usage and diagnostic evidence
agentsview session usage <id>            # token usage and cost estimate
agentsview session usage <id> --own-only # exclude subagent transcripts
agentsview session usage <id> --wait     # wait up to 30 seconds after daemon discovery
agentsview session usage-batch <id>...   # ordered usage results for several sessions
agentsview session label <id> k=v ...    # add or remove labels
agentsview session parent <id> <parent>  # record the launching session
```

In `session compactions`, `--limit` counts sessions per page rather than
response bytes, and `--limit 0` uses the default of 20. Keep the same
`--active-since` when resuming with `--cursor`, even if a page has no
summaries. Ambiguous older Evener labels appear separately with their session
ID and ordinal.

`agentsview session commits <id>` lists transcript and Git trailer commits
linked to a session. The `SOURCE` column separates `transcript` from
`commit message`, while JSON keeps `source: "trailer"` for commit message
links added by [`agentsview git-hook`](#agentsview-git-hook). Pass
`--format json` for structured output. The table prints the session, exact
message ordinal, call time, source, recorded SHA, branch, subject, and user
turn, shortened to 100 terminal columns. Transcript links use the last real
prompt before the commit call, whereas trailer links use the last real prompt
at or before Git's author time and mark the turn as estimated. Trailer links
require a session message within the five minutes before that time, so an idle
session gets no suggested turn; the prompt itself can be older than five
minutes if later messages keep the session active. Trashed or permanently
deleted sessions include `missing: true` and omit the turn. For retention and
lookup limits, see
[commit lookups](https://agentsview.io/docs/session-api/#commit-lookups).
Transcript links require stored Git summaries with SHAs of 7 to 64 characters;
quiet commits get no transcript link. Links also include Codex poll results
paired with the original call. Calls that span repositories use the first
directory, adjusted by the last `cd` before the first commit invocation.
Archives that retain only transcripts keep trailer links, while usage-only
archives have no links. Hosted PostgreSQL returns HTTP 501, and sessions
permanently deleted before this release have no transcript links. JSON output
keeps `call_index`. Code-mode scripts with exactly one tool call and a fixed command
literal can link commits, including output without a process ID, though command
literals containing `${` get no links. A single-call `write_stdin` script can
poll an earlier commit execution, while scripts with multiple tool calls cannot
start new commit links and need output process IDs matching an earlier commit
call to poll.

Pass `session list --label k=v` and `--pr owner/repo#123` to filter by
[labels](/docs/session-api/#agentsview-session-label) and
[pull request links](/docs/session-api/#pull-request-links).

`session usage` includes archived subagent spend in the parent total. For
refresh behavior, missing-session exit codes, and the `refresh_incomplete`
JSON field, see the
[session usage contract](/docs/session-api/#agentsview-session-usage).

When the first page of `session search` is empty, it keeps `(no matches)` on
stdout and writes measured hints to stderr if one-shot, automated, or child
sessions have matches. Each hint gives you a command that preserves your
filters and adds the required include flags, and JSON adds `hints` entries
with `code`, `message`, and a full `next` command. Counts come from a bounded
search and say `more than N` when additional matches exist, or `at least N`
for semantic and hybrid searches or when filtering or candidate limits prevent
an exact count. `excluded_combined` gives a retry with the remaining include
flags for matches beyond the individual class hints, and failed diagnostic
searches add no hint.

`session search` supports substring by default along with `--regex`, `--fts`,
`--semantic`, and `--hybrid` modes. On SQLite, PostgreSQL, and ClickHouse, a
default multi-word search puts exact-text matches first from every selected
source, followed by messages containing every word, then messages containing
fewer words; `matched_words` in JSON counts those words, excluding unquoted
stop words. Regular word forms match, so `invoice` also finds `invoices`. A
single word, a fully quoted query such as `'"rounding of invoices"'`, or a
search whose `--in` leaves out `messages` matches exact text only, and a
quoted phrase inside a longer query requires its words together and in order.
`--fts` and hybrid's keyword matches rank messages by words the same way, and
hybrid keyword matches report their word count. An `--fts` query made only of
quoted phrases, or one that uses FTS5 operators after a leading quote such as
`'"fix" OR "missing"'`, keeps its FTS5 meaning. Pass repeatable
`--exclude-session <id>` to drop matches from those sessions before `--limit`.
While the embedding index is unavailable, hybrid returns keyword matches with
a note. You can scope semantic and hybrid results with
`--scope top|all|subordinate`, defaulting to `all`, to include or exclude
sidechain and subagent content; see
[Semantic Search](/docs/semantic-search/#scoping-results-scope). HTTP and MCP
also accept `scope` for substring and regex searches, whereas CLI lexical
searches use `--include-children`. The human-readable table includes an `AGE`
column built from each match's timestamp so you can spot recent evidence
without opening the session, and matches without a usable timestamp show `—`.

Structured response commands accept `--format json` or its short alias
`--json`. `session export` and `session watch` are the exceptions because they
stream raw bytes and NDJSON respectively, so they reject `--format` and
`--json`. Pass `--server <url>` to query an explicit running daemon, supply
`AGENTSVIEW_SERVER_TOKEN` or `--server-token-file <path>` when that daemon
requires auth, or pass `--pg` to read from configured PostgreSQL.

`agentsview session list` hides one-shot, automated, and child sessions by
default. Whenever that hides matching sessions on the first page, it prints
the counts and the corresponding `--include-one-shot`, `--include-automated`,
or `--include-children` flags to stderr, even when the list already has
visible results. Child counts include sessions hidden by multiple defaults,
and the notice lists every remaining include flag to combine. Because child
counts use one bounded page, a positive count from a partial page says
`at least N`. Stdout, including `--format json`, keeps its existing shape for
pipelines.

If an older daemon returns sessions without exclusion counts, the CLI lists
those sessions without printing an exclusion notice. Cursor pages and lists
with all three `--include-*` flags also omit the notice.

`AGENTSVIEW_PG_URL`, a legacy `[pg].url`, and the effective default target
from `default_pg` plus `[pg.NAME]` configure sync only and do not change the
default read path. Read commands use local SQLite unless you pass `--pg`, in
which case they fail fast if no connection URL is configured. Mutating
commands such as `session sync` and local-only raw source export still use the
local archive.

For a human-first signal view, run
[`agentsview session health`](#agentsview-health); for the full programmatic
contract, including daemon-first transport behavior and markdown export
details, see [Session API](/docs/session-api/).

By default, `agentsview session list` prints a resume-oriented table showing a
recently-active marker, session ID, age, agent, project, branch, message
count, title, and working directory. Pass `--resume` or `--active` to filter
to sessions active in the last 15 minutes, and combine either flag with
`--active-since <RFC3339>` to pick a wider or narrower window.

______________________________________________________________________

<a id="agentsview-embeddings"></a>

### `agentsview index embeddings`

Manage the local semantic search embedding index. Running `build` turns search
on when needed, while the other subcommands require it to be enabled. For
configuration and search details, see
[Semantic Search](/docs/semantic-search/).

```bash
agentsview index embeddings build           # build or refresh the index
agentsview index embeddings list            # list embedding generations
agentsview index embeddings activate <id>   # activate a generation
agentsview index embeddings retire <id>     # retire a generation
```

______________________________________________________________________

### `agentsview recall`

Inspect the experimental durable-knowledge layer over your local session
archive. Recall is active research, and you may need to rebuild its corpus as
the schema, scoring, and trust policy change. For current guarantees and
limitations, see [Recall (Experimental)](/docs/recall/).

```bash
agentsview recall list [--updated-since <timestamp>] [--created-since <timestamp>] [--cursor <cursor>]
agentsview recall get <id>
agentsview recall query <text>
agentsview recall brief <task>
agentsview recall stats
agentsview recall extract run [--session <id>] [--full] [--limit <n>]
agentsview recall extract status
agentsview recall extract activate
agentsview recall extract retire <fingerprint> [--force]
agentsview recall extract doctor
agentsview recall extract preview --session <id>
agentsview recall import <accepted-recall.jsonl> --dry-run
```

`--updated-since` and `--created-since` accept inclusive RFC3339 timestamps. A
bounded list returns the oldest change first and includes archived entries;
pass the returned `next_cursor` to `--cursor` for the next page. If Recall
changes while you walk pages, restart from the first page with the same
original time bounds. See [Recall](/docs/recall/) for following changes over
time.

Model-backed extraction requires an enabled `[recall.extract]` config section;
see [Recall](/docs/recall/#automatic-extraction). `preview` replaces the
earlier `extract --session <id> --dry-run` form, which still works as a
fallback. Extraction subcommands work on the local archive only and reject
`--server`. While a daemon owns the archive, it runs extraction passes itself
and refuses manual `run`, `activate`, and `retire`.

Use an isolated `AGENTSVIEW_DATA_DIR` for Recall population experiments. Test
imports with `--dry-run` first; writing requires `--yes`, and a remote write
also requires `--allow-remote-import`. Importing into the default production
directory is refused unless you pass `--allow-production-import` explicitly.
None of these flags bypass Recall's trust or evidence checks.

When you target an explicit daemon with `--server <url>`, pass remote
credentials with `AGENTSVIEW_SERVER_TOKEN` or `--server-token-file <path>`.
Recall never sends the local daemon token from `config.toml` to an explicitly
supplied server.

______________________________________________________________________

### `agentsview kata status`

Check whether the configured Kata instance is reachable, compatible with API
schema 0.21.0 or newer, authenticated, and has the configured project. When a
daemon is running, the command reports the daemon's view; when no daemon owns
the data directory, it probes from local config without starting one. An
unreachable owner returns an error. Pass `--format json` or `--json` for the
status object, or `--format human` for the default text output. To query an
explicit AgentsView daemon, pass `--server URL` along with
`AGENTSVIEW_SERVER_TOKEN` or `--server-token-file PATH`. See
[Kata](/docs/kata/).

______________________________________________________________________

### `agentsview insight`

Generate and inspect stored Activity Insights through the daemon API.

```bash
agentsview insight list [--type <type>] [--project <project>] \
  [--date-from <date>] [--date-to <date>]
agentsview insight get <id>
agentsview insight generate --type daily_activity --date-from <date> \
  --date-to <date> [flags]
agentsview insight generate --type tool_effectiveness --session-id <id>
```

`insight list` accepts `--type`, `--project`, `--date-from`, and `--date-to`,
and `insight get` prints a single saved insight. Pass `--format json` or
`--json` for the API envelope and full stored rows; human output prints a
table for `list` and the saved Markdown content for `get`.

`insight generate` accepts `--type`, `--date-from`, `--date-to`, `--project`,
`--prompt`, `--session-id`, `--agent`, `--automated-scope`, and `--timezone`,
with `--type` defaulting to `daily_activity`. The server validates fields,
picks the configured agent or endpoint, and decides whether the selected
backend can save the result. Local commands find or start the configured
daemon, while an explicit `--server <url>` targets an already running server.

Run `tool_effectiveness` with `--session-id` to evaluate one session's tool
calls. The saved report labels conclusions as helped, did not help, or unclear
and cites the calls behind them. For input limits and what gets sent to the
configured model, see
[Tool effectiveness reports](/docs/recall/#tool-effectiveness-reports).

Generation status and log events go to stderr so only the saved insight writes
to stdout, keeping JSON output clean for scripts. For an explicit server,
supply its bearer token with `AGENTSVIEW_SERVER_TOKEN` or
`--server-token-file <path>`; the local daemon token from `config.toml` is
never sent to an explicitly supplied server.

______________________________________________________________________

### `agentsview agent setup`

Install the conversation search skill, a SessionStart hook, and the focused
MCP profile for installed Claude Code and Codex clients:

```bash
agentsview agent setup [--agent claude|codex]... [--check] [--force] [--mcp=false] [--print] [--format json]
```

Setup refuses to replace a different `agentsview` MCP entry unless you pass
`--force`, and leaves other entries intact. Pass `--force` to replace the
entry and overwrite edited skill files. `--check` makes no changes, `--print`
shows the MCP commands to run by hand, and `--mcp=false` installs or checks
the skill without inspecting or changing the MCP entry.

The SessionStart hook refreshes memory and links runs to their launching
session on Linux and Windows. Parent linking on macOS is unverified. Codex
launchers link to the thread. Codex requires you to trust a new hook in
`/hooks`.

A ClickHouse sidebar hides automated launched runs.

Setup registers `agentsview mcp --profile memory` without server flags. To use
a remote archive, set `AGENTSVIEW_MEMORY_SERVER` or `AGENTSVIEW_MEMORY_PG`
where the agent starts the MCP server, and point the skill at the same server
with [`agentsview skills install --server`](#agentsview-skills).

______________________________________________________________________

### `agentsview doctor memory`

Inspect the selected conversation-memory target and local client package:

```bash
agentsview doctor memory
agentsview doctor memory --server <url> [--server-token-file <path>]
agentsview doctor memory --pg
agentsview doctor memory --plugin-root <path> --format json
```

The target section uses the same readiness provider as MCP `get_memory_status`
and search-result coverage. It reports the authenticated archive identity,
backend, read-only mode, lexical availability, semantic generation coverage,
source telemetry, and server version when available. An older remote server
reports `unknown` with reason `unsupported`, while authentication and
transport failures stay command errors instead of looking like an empty
archive.

By default, the client section checks installed Claude Code and Codex
integrations. When `--plugin-root`, `PLUGIN_ROOT`, or `CLAUDE_PLUGIN_ROOT`
identifies a native package, it checks that package's recall skill, focused
MCP configuration, and SessionStart hook. It also detects standalone
AgentsView skills and warns when their baked target differs from the selected
archive. Both human and JSON output omit plugin paths, server URLs, and token
values.

Local diagnostics open the archive read-only. The client check can start the
configured client and MCP process, though it calls no data tools and does not
start the local archive daemon, sync transcripts, or rebuild vectors. Codex
can create its own runtime state. The diagnostic does not probe an embedding
provider or modify client configuration. `--server`, `--pg`, and the
`AGENTSVIEW_MEMORY_SERVER`, `AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE`, and
`AGENTSVIEW_MEMORY_PG` environment variables select the same read target used
by `agentsview mcp --profile memory`.

______________________________________________________________________

### `agentsview memory session-start`

Run the bounded conversation-memory lifecycle action used by native agent
packages:

```bash
agentsview memory session-start
agentsview memory session-start --mode hosted-contributor [--target <pg-name>]
agentsview memory session-start --mode hosted-reader --server <url> \
  [--server-token-file <path>]
agentsview memory session-start --mode hosted-reader --pg [--target <pg-name>]
agentsview memory session-start --hook [--plugin-root <path>]
```

The default `local` mode makes sure the writable local daemon is available,
then asks it to run a coalesced background reconciliation.
`hosted-contributor` notifies the already running PostgreSQL push watcher for
the selected target without starting another writer; that owner keeps its
existing debounce, credentials, embedding work, and push cadence.
`hosted-reader` checks an explicit authenticated daemon or configured
PostgreSQL read target without starting a local archive or claiming to refresh
hosted data.

The refresh returns within 1.9 seconds and does not wait for archive-scale
work. With `--hook`, recording a launcher link can take another 6 seconds.
The hook records links only at startup, so resuming a run does not restore a
missing link. If the Codex index or thread stays missing after a short retry,
or its index or schema is newer than AgentsView supports, the link points to
the original thread page; after a revert, that may be an earlier page. Set
`AGENTSVIEW_DISABLE_AUTO_SYNC=1` to skip the automatic request.
Launcher links, explicit sync commands, and searches still work. Restart a
contributor owner started by an older binary once so it advertises the
lifecycle wake endpoint. Contributor wake works on macOS and Linux, while
Windows contributors stay on the watcher's normal event and interval cadence.

Native packages can configure these flags with `AGENTSVIEW_MEMORY_MODE`,
`AGENTSVIEW_MEMORY_TARGET`, `AGENTSVIEW_MEMORY_SERVER`,
`AGENTSVIEW_MEMORY_SERVER_TOKEN_FILE`, and `AGENTSVIEW_MEMORY_PG`, and
explicit target flags override the package environment as a group. Passing
`--hook` writes any failure to stderr and exits cleanly so startup issues do
not block an agent session from opening. `--plugin-root` also checks for
standalone skill copies that would load alongside the native package without
modifying those files.

______________________________________________________________________

### `agentsview diagnostics append`

Record a failure that happened outside an agent session for the Friction Log:

```bash
agentsview diagnostics append --source worker --message "launch failed" [--at RFC3339] [--id UUID] [--server URL] [--json]
```

The occurrence time defaults to now. Pass `--at` to record a delayed report on
the day it happened, up to five minutes in the future; the zero timestamp
`0001-01-01T00:00:00Z` is rejected. Source names can be up to 64 characters and
messages up to 500. The command prints the event ID and occurrence time, and
you can reuse both with the same source and message to retry without double
counting. If you pass `--id` without `--at`, reusing the ID and content also
returns the original event. Local appends reach a PostgreSQL hub on the next
replica push, including a project-filtered push, while an explicit `--server`
writes to that daemon instead. Read-only servers and `--pg` cannot append.
Events show up in digests only when Friction review is enabled on the archive
that builds them.

`POST /api/v1/diagnostics` takes a required UUID `id`, `source`, `message`,
and an optional RFC 3339 `occurred_at`. It requires an authenticated server or
a localhost request, and reusing an ID with different content returns HTTP 409.

______________________________________________________________________

### `agentsview friction`

Build and read the [Friction Log](/docs/configuration/#friction-log). To use
it, enable review with `[friction] enabled = true`.

```bash
agentsview friction run [--date YYYY-MM-DD] [--rebuild] [--dry-run] [--format human|json | --json]
agentsview friction digest [--date D | --from D --to D] [--format md|json | --json]
agentsview friction findings [--date D] [--kind K] [--session ID] [--limit N] [--cursor C] [--format table|json | --json]
agentsview friction patterns [--kind K] [--since D] [--limit N] [--cursor C] [--format table|json | --json]
agentsview friction file <fingerprint> [--dry-run] [--format human|json | --json]
agentsview friction link <fingerprint> <issue>
agentsview friction unlink <fingerprint>
```

- `friction run` without flags builds every complete local day that has no
  digest yet. Pass `--date` to build one day, `--rebuild` to rebuild an
  existing day while keeping the sessions it already covered, and `--dry-run`
  to compute without writing. It prints one summary line per day, or the
  summary JSON with `--json`.
- `friction digest` prints the latest digest as Markdown unless you pass
  `--date` or a range; pass `--format json` to print the stored summary JSON.
- `friction findings` lists stored findings, while `friction patterns` ranks
  recurring patterns by occurrences and shows each pattern's Kata issue.
  `--kind` accepts `correction`, `error`, `workaround`, `deferral`, `pattern`,
  `frustration`, and `interruption`.
- `friction file` files a pattern to [Kata](/docs/kata/) or links the issue
  Kata already holds for it, and `friction link` and `friction unlink` set or
  remove a pattern's issue by hand. Only the AgentsView hub files.

Read commands use the running daemon, starting one when needed, or an explicit
`--server <url>` or `--pg` for the configured PostgreSQL hub. Supply a
`--server` token with `AGENTSVIEW_SERVER_TOKEN` or
`--server-token-file <path>`. `friction run`, `friction file`, `friction link`,
and `friction unlink` use the daemon when one is running and write the local
archive directly otherwise.

______________________________________________________________________

### `agentsview mcp`

Run a read-only Model Context Protocol server for assistant clients that can call
MCP tools. The server exposes session search, listing, overview, message
retrieval, content search, and usage-summary tools over the same service layer
as the CLI and HTTP API. For setup examples and operational guidance, see
[MCP Server](/docs/mcp/).

```bash
agentsview mcp
agentsview mcp --profile memory
agentsview mcp --http 127.0.0.1:8085
agentsview mcp --server http://127.0.0.1:8080
agentsview mcp status --json
agentsview mcp calls --since 7d
```

Run `agentsview mcp status --json` to list local HTTP MCP listeners without
starting one; stdio connections do not appear. For endpoint fields and
token-file paths, see
[listener discovery](/docs/mcp/#discover-running-http-listeners).

To summarize local call outcomes over inclusive UTC dates, run
`agentsview mcp calls`. The summary includes empty and truncated result shares
and rejected argument names. For the recorded fields, see
[call outcomes](/docs/mcp/#review-call-outcomes).

By default, `agentsview mcp` uses stdio, the standard transport for local MCP
clients such as Claude Desktop, Claude Code, and Codex. Pass `--http <addr>`
to serve StreamableHTTP instead. Bare ports and `:PORT` values bind to
`127.0.0.1`, while non-loopback binds require `--http-allow-insecure` along
with a configured bearer token.

Local MCP mode reads through the AgentsView daemon, starts it when needed, and
reuses the connection across tool calls without opening the local SQLite
archive directly. See [daemon-backed reads](/docs/mcp/#daemon-backed-reads)
for when it rediscovers the daemon.

Pass `--profile memory` when you want the client to see only the
`get_memory_status`, `search_content`, `get_messages`, and
`get_compaction_summaries` conversation-memory tools. `get_memory_status`
reports archive, lexical, semantic, vector coverage, and source-telemetry
readiness without running a search probe. The default `full` profile keeps the
complete MCP tool set. Profile selection works with both stdio and
StreamableHTTP and does not change backend selection.

To point at an explicit running daemon, pass `--server <url>`, and provide
`AGENTSVIEW_SERVER_TOKEN` or `--server-token-file <path>` when that daemon
requires auth. Pass `--pg` to read from configured PostgreSQL directly, or run
[`agentsview replica pg serve`](/docs/pg-sync/#agentsview-pg-serve) and pass
its URL with `--server`.

| Flag                         | Default | Description                                         |
| ---------------------------- | ------- | --------------------------------------------------- |
| `--http <addr>`              |         | Serve StreamableHTTP instead of stdio               |
| `--http-allow-insecure`      | `false` | Allow non-loopback HTTP binds; requires bearer auth |
| `--server <url>`             |         | Explicit daemon URL for MCP tool calls              |
| `--server-token-file <path>` |         | Bearer token file for an explicit daemon URL        |
| `--pg`                       | `false` | Read from configured PostgreSQL                     |
| `--profile <name>`           | `full`  | Advertise the `full` or focused `memory` tool set    |

______________________________________________________________________

### `agentsview secrets`

Scan sessions for secret leaks and list stored findings, with matches redacted
by default. For the full detector set, storage shape, and HTTP API, see
[Secret Scanning](/docs/session-api/#secret-scanning).

```bash
agentsview secrets scan [flags]   # scan sessions for leaks
agentsview secrets list [flags]   # list stored findings
```

`secrets scan` walks the archive, runs both definite and candidate rules, and
writes new findings to the `secret_findings` table. Pass `--backfill` to scan
only sessions that have not yet been scanned at the current ruleset version.
Because the inline scan during sync records only definite-tier findings,
`--backfill` lets you collect heuristic candidates without rescanning the
whole archive.

`secrets list` returns redacted findings by default. To print raw values, pass
`--reveal`; this works only against a localhost-bound daemon and writes a
warning to stderr.

| Flag           | Used by | Description                                                       |
| -------------- | ------- | ----------------------------------------------------------------- |
| `--format`     | both    | `human` or `json` (inherited from `secrets`)                      |
| `--json`       | both    | Alias for `--format json` (inherited from `secrets`)              |
| `--backfill`   | scan    | Scan only sessions not yet scanned at the current ruleset version |
| `--project`    | both    | Limit to a project                                                |
| `--agent`      | both    | Limit to an agent                                                 |
| `--date-from`  | both    | Sessions on or after `YYYY-MM-DD`                                 |
| `--date-to`    | both    | Sessions on or before `YYYY-MM-DD`                                |
| `--rule`       | list    | Filter by rule name (e.g. `aws-access-key`)                       |
| `--confidence` | list    | `definite`, `candidate`, or `all` (default `definite`)            |
| `--reveal`     | list    | Show unredacted values (localhost-only)                           |
| `--limit`      | list    | Max findings (default 50, max 500)                                |
| `--cursor`     | list    | Pagination cursor from a previous response                        |

______________________________________________________________________

### `agentsview skills`

Install or list the bundled recall artifacts that teach coding-agent harnesses
to search AgentsView conversation history. Claude Code gets the
`agentsview-finding-history` skill and the `agentsview-search-conversations`
agent, while Codex and other `.agents/skills` readers get the skill with the
same direct MCP workflow as its fallback. For the workflow and upgrade notes,
see [Semantic Search](/docs/semantic-search/#skills-for-coding-agents).

```bash
agentsview skills install [--harness claude|agents] [--project] [--force]
    [--server URL] [--server-token-file PATH]
agentsview skills list [--project] [--format json]
    [--server URL] [--server-token-file PATH]
```

`install` renders the package for each `--harness`, defaulting to both. Skills
are written to `~/.claude/skills/agentsview-finding-history/`,
`~/.agents/skills/agentsview-finding-history/`, or both, each with `SKILL.md`
and a `LICENSE` sidecar, and Claude's search agent is written to
`~/.claude/agents/agentsview-search-conversations.md`. Because that agent
allowlists only `mcp__agentsview__search_content` and
`mcp__agentsview__get_messages`, name your Claude MCP server entry
`agentsview`. Pass `--project` to use the equivalent paths at the current git
root. Each artifact carries its own generated hash: `install` updates safe
generated files, refuses hand-edited or foreign files unless you pass
`--force`, continues through the remaining artifacts, and exits non-zero if
anything was refused. `list` prints HARNESS, ARTIFACT, LEVEL, STATE (`missing`,
`current`, `stale`, `modified`, or `foreign`), and PATH for every artifact.
When local semantic search is off, a successful install prints how to turn it
on.

Passing `--server` and `--server-token-file`, or setting
`AGENTSVIEW_SKILLS_SERVER` and `AGENTSVIEW_SKILLS_SERVER_TOKEN_FILE`, bakes
those flags into every example command so a remote-daemon install does not
teach the local SQLite default. Values are shell-quoted, so a token path with
spaces stays a single argument.

Precedence runs from explicit flags, to whatever the installed skill already
bakes, to the environment. Because an installed skill wins even when it bakes
no remote, exporting `AGENTSVIEW_SKILLS_SERVER` never marks an existing
package stale in `list`; those variables only seed a skill that is not
installed yet. The endpoint-neutral Claude agent never includes server flags.
To remove a baked remote and return the skill to local SQLite, pass
`--server ""`.

These variables apply only to skills and are kept separate from
`AGENTSVIEW_SERVER_TOKEN` on purpose: they do not change the CLI's default
read path, so `session search` still needs `--server` unless the baked
examples supply it.

______________________________________________________________________

### `agentsview completion`

Generate a completion script for Bash, Fish, PowerShell, or Zsh. The command
writes the script to standard output without touching your shell startup
files. To omit command descriptions, pass `--no-descriptions` after the shell
name.

Load completion for the current shell session:

=== "Bash"

    ```bash
    source <(agentsview completion bash)
    ```

=== "Fish"

    ```fish
    agentsview completion fish | source
    ```

=== "PowerShell"

    ```powershell
    agentsview completion powershell | Out-String | Invoke-Expression
    ```

=== "Zsh"

    ```zsh
    source <(agentsview completion zsh)
    ```

For persistent completion, install the generated script in the location for the
shell:

- Bash requires the `bash-completion` package. Write the script to
  `/etc/bash_completion.d/agentsview` on Linux or
  `$(brew --prefix)/etc/bash_completion.d/agentsview` on macOS.
- Fish loads `~/.config/fish/completions/agentsview.fish` automatically.
- Zsh requires completion to be enabled with `compinit`. Write the script to
  `${fpath[1]}/_agentsview` on Linux or
  `$(brew --prefix)/share/zsh/site-functions/_agentsview` on macOS.
- PowerShell users can add the current-session command above to their profile.

Run `agentsview completion <shell> --help` for the exact installation command
and shell-specific prerequisites. Start a new shell after installing a generated
script.

______________________________________________________________________

### `agentsview help`

Print usage information.

```bash
agentsview help
```

## Environment Variables

| Variable                              | Default                                                                         | Description                                                                                                     |
| ------------------------------------- | ------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `AIDER_DIR`                           | unset                                                                           | Aider discovery root; set this to opt into scanning a code root                                                 |
| `AMP_DIR`                             | `~/.local/share/amp/threads`                                                    | Deprecated; historical local Amp thread JSON files only                                                         |
| `ANTIGRAVITY_DIR`                     | `~/.gemini/antigravity`, `~/.gemini/antigravity-ide`                            | Google Antigravity IDE sessions directories                                                                     |
| `ANTIGRAVITY_CLI_DIR`                 | `~/.gemini/antigravity-cli`                                                     | Google Antigravity CLI sessions directory                                                                       |
| `ANTIGRAVITY_KEY`                     |                                                                                 | Optional key for decrypting Antigravity CLI `.pb` transcripts (defaults to summary mode without it)             |
| `AUGURE_CODE_SESSIONS_DIR`            | `~/.augure/sessions`                                                            | Augure Code sessions directory                                                                                  |
| `AUGURE_DESKTOP_DIR`                  | (platform-specific)                                                             | Augure Desktop data root containing `state.db` and `sessions/`                                                  |
| `CLAUDE_PROJECTS_DIR`                 | `~/.claude/projects`                                                            | Claude Code projects directory                                                                                  |
| `CLAUDE_CONFIG_DIR`                   | unset                                                                           | Claude Code config home that re-roots the default `projects/` discovery path                                    |
| `OPENCLAUDE_PROJECTS_DIR`             | `~/.openclaude/projects`                                                        | OpenClaude projects directory                                                                                   |
| `OPENCLAUDE_CONFIG_DIR`               | unset                                                                           | OpenClaude config home that re-roots the default `projects/` discovery path                                     |
| `COWORK_DIR`                          | (platform-specific)                                                             | Claude Desktop cowork sessions directory                                                                        |
| `CODEX_SESSIONS_DIR`                  | `~/.codex/sessions`                                                             | Codex sessions directory                                                                                        |
| `CODEX_HOME`                          | unset                                                                           | Codex home that re-roots the default `sessions/` and `archived_sessions/` discovery paths                       |
| `INTERPRETER_HOME`                    | unset                                                                           | Open Interpreter home that re-roots the default `sessions/` and `archived_sessions/` paths                      |
| `CLINE_DIR`                           | `~/.cline`                                                                      | Cline CLI sessions directory (discovers under `<root>/data/sessions/` or direct sessions root)                  |
| `CODEBUFF_DIR`                        | `~/.config/manicode/projects`                                                   | Codebuff/Freebuff sessions directory                                                                            |
| `COMMANDCODE_PROJECTS_DIR`            | `~/.commandcode/projects`                                                       | Command Code projects directory                                                                                 |
| `COPILOT_DIR`                         | `~/.copilot`                                                                    | Copilot CLI sessions directory                                                                                  |
| `CRUSH_DIR`                           | (platform-specific)                                                             | Crush registry, project data directory, or `crush.db` path                                                      |
| `CRUSH_GLOBAL_DATA`                   | unset                                                                           | Absolute path overriding the default Crush global data directory                                                |
| `CORTEX_DIR`                          | `~/.snowflake/cortex/conversations`                                             | Cortex Code conversations directory                                                                             |
| `CURSOR_PROJECTS_DIR`                 | `~/.cursor/projects`                                                            | Cursor transcripts directory                                                                                    |
| `DEEPSEEK_TUI_SESSIONS_DIR`           | `~/.codewhale/sessions` and `~/.deepseek/sessions`                              | DeepSeek TUI sessions directory                                                                                 |
| `DEEPSEEK_HARNESS_SESSIONS_DIR`       | `~/.dsh/sessions`                                                               | DeepSeek Harness sessions directory                                                                             |
| `DSH_HOME`                            | unset                                                                           | DeepSeek Harness home that re-roots the default `sessions/` discovery path                                      |
| `FACTORY_DIR`                         | `~/.factory/sessions`                                                           | Factory Droid sessions directory                                                                                |
| `FORGE_DIR`                           | `~/.forge`                                                                      | Forge directory (contains `.forge.db`)                                                                          |
| `FREEBUFF_CONFIG_DIR`                 | unset                                                                           | Freebuff config home that re-roots the default `projects/` discovery path                                       |
| `GEMINI_DIR`                          | `~/.gemini`                                                                     | Gemini CLI directory                                                                                            |
| `GOOSE_PATH_ROOT`                     | (platform-specific)                                                             | Goose path root; sessions are read from `<root>/data/sessions/sessions.db`                                      |
| `GPTME_DIR`                           | `~/.local/share/gptme/logs`                                                     | gptme logs directory                                                                                            |
| `GROK_DIR`                            | `~/.grok/sessions`                                                              | Grok sessions directory                                                                                         |
| `HERMES_SESSIONS_DIR`                 | `~/.hermes/sessions` (macOS/Linux), `~/AppData/Local/hermes/sessions` (Windows) | Hermes Agent sessions root; sibling SQLite `state.db` is discovered even when `sessions/` is absent             |
| `IFLOW_DIR`                           | `~/.iflow/projects`                                                             | iFlow projects directory                                                                                        |
| `FX_SESSIONS_DIR`                     | `~/.fx/sessions`                                                                | fx sessions directory                                                                                           |
| `JUNIE_DIR`                           | `~/.junie/sessions`                                                             | Junie CLI session store directory                                                                               |
| `JUNIE_HOME`                          | `~/.junie`                                                                      | Junie home that re-roots the default `sessions/` discovery path                                                 |
| `KILO_DIR`                            | `~/.local/share/kilo`                                                           | Kilo data directory                                                                                             |
| `KILO_LEGACY_DIR`                     | (platform-specific)                                                             | Kilo legacy VS Code extension data directory                                                                    |
| `KIMI_DIR`                            | `~/.kimi/sessions` and `~/.kimi-code/sessions`                                  | Kimi sessions directory                                                                                         |
| `KIMI_WORK_DIR`                       | (platform-specific)                                                             | Kimi Work (kimi-desktop daimon) sessions directory                                                              |
| `KIRO_SESSIONS_DIR`                   | `~/.kiro/sessions/cli` and `~/.local/share/kiro-cli`                            | Kiro CLI sessions directory (JSONL and SQLite)                                                                  |
| `KIRO_IDE_DIR`                        | (platform-specific)                                                             | Kiro IDE sessions directory                                                                                     |
| `MIMOCODE_DIR`                        | `~/.local/share/mimocode`                                                       | MiMoCode data directory                                                                                         |
| `MUSE_SESSIONS_DIR`                   | `~/.local/share/muse/sessions`                                                  | Muse Code sessions directory; defaults to `$XDG_DATA_HOME/muse/sessions` when set                               |
| `MUSE_PERSONAL_AGENTS_DIR`            | `~/agents`                                                                      | Muse Personal agents directory; scans `agent-<uuid>/sessions/` beneath it                                       |
| `VIBE_SESSIONS_DIR`                   | `~/.vibe/logs/session`                                                          | Mistral Vibe sessions directory                                                                                 |
| `OMP_DIR`                             | `~/.omp/agent/sessions`                                                         | OhMyPi sessions directory                                                                                       |
| `OPENCLAW_DIR`                        | `~/.openclaw/agents` and `~/.kimi_openclaw/agents`                              | OpenClaw agents directory                                                                                       |
| `OPENCODE_DIR`                        | `~/.local/share/opencode`                                                       | OpenCode data directory                                                                                         |
| `OPENCODEREVIEW_DIR`                  | `~/.opencodereview/sessions`                                                    | Open Code Review sessions directory                                                                             |
| `OPEN_INTERPRETER_SESSIONS_DIR`       | `~/.openinterpreter/sessions` and `archived_sessions`                           | Open Interpreter sessions directory                                                                             |
| `OPENHANDS_CONVERSATIONS_DIR`         | `~/.openhands/conversations`                                                    | OpenHands CLI conversations directory                                                                           |
| `PI_DIR`                              | `~/.pi/agent/sessions`                                                          | Pi sessions directory                                                                                           |
| `PI_CODING_AGENT_DIR`                 | unset                                                                           | Pi agent home that re-roots the default `sessions/` discovery path                                              |
| `PI_CODING_AGENT_SESSION_DIR`         | unset                                                                           | Pi session directory override; `PI_DIR` takes precedence                                                        |
| `DIRECT_API_DIR`                      | `~/.agentsview/direct-api`                                                      | Direct API sessions directory                                                                                   |
| `STEPCODE_DIR`                        | `~/.stepcode/agent/sessions`                                                    | StepCode sessions directory                                                                                     |
| `STEP_CODING_AGENT_DIR`               | unset                                                                           | StepCode agent home that re-roots the default `sessions/` discovery path                                        |
| `STEP_CODING_AGENT_SESSION_DIR`       | unset                                                                           | StepCode session directory override; `STEPCODE_DIR` takes precedence                                            |
| `PRIME_AGENT_SESSION_DIR`             | `~/.prime/agent/sessions`                                                       | Prime Agent sessions directory                                                                                  |
| `PIEBALD_DIR`                         | `~/.local/share/piebald`                                                        | Piebald directory (contains `app.db`)                                                                           |
| `POOLSIDE_DIR`                        | (platform-specific)                                                             | Poolside Agent CLI trajectory directory                                                                         |
| `POSIT_ASSISTANT_DIR`                 | `~/.posit/assistant/workspaces`                                                 | Posit Assistant workspaces directory                                                                            |
| `POSITRON_DIR`                        | (platform-specific)                                                             | Positron Assistant user directory                                                                               |
| `QCLAW_DIR`                           | `~/.qclaw/agents`                                                               | QClaw agents directory                                                                                          |
| `QODER_PROJECTS_DIR`                  | Legacy and platform-specific roots                                              | Qoder projects directory; see [Session Discovery](/docs/configuration/#session-discovery)                       |
| `QWEN_PROJECTS_DIR`                   | `~/.qwen/projects`                                                              | Qwen Code projects directory                                                                                    |
| `QWENPAW_DIR`                         | `~/.copaw/workspaces`                                                           | QwenPaw workspaces directory                                                                                    |
| `REASONIX_DIR`                        | `~/.reasonix` and `~/AppData/Roaming/reasonix`                                  | Reasonix data directory                                                                                         |
| `ROOCODE_DIR`                         | (platform-specific)                                                             | RooCode VS Code extension data directory                                                                        |
| `SHELLEY_DIR`                         | `~/.config/shelley`                                                             | Shelley data directory                                                                                          |
| `TRAE_DIR`                            | (platform-specific)                                                             | Trae editor user-data directory                                                                                 |
| `VISUALSTUDIO_COPILOT_DIR`            | (platform-specific)                                                             | Visual Studio Copilot traces directory                                                                          |
| `VSCODE_COPILOT_DIR`                  | (platform-specific)                                                             | VS Code Copilot sessions directory                                                                              |
| `WINDSURF_DIR`                        | (platform-specific)                                                             | Windsurf user-data directory                                                                                    |
| `WARP_DIR`                            | (platform-specific)                                                             | Warp database directory                                                                                         |
| `WORKBUDDY_PROJECTS_DIR`              | `~/.workbuddy/projects`                                                         | WorkBuddy projects directory                                                                                    |
| `CODEBUDDY_DIR`                       | (platform-specific)                                                             | Tencent CodeBuddy CN data directory                                                                             |
| `ZCODE_DIR`                           | `~/.zcode/cli/db` and `~/.zcode/cli`                                            | ZCode data directory (contains `db.sqlite`)                                                                     |
| `ZED_DIR`                             | (platform-specific)                                                             | Zed data directory (contains `threads/threads.db`)                                                              |
| `ZENCODER_DIR`                        | `~/.zencoder/sessions`                                                          | Zencoder sessions directory                                                                                     |
| `AGENTSVIEW_DATA_DIR`                 | `~/.agentsview`                                                                 | Data directory (database, config)                                                                               |
| `AGENTSVIEW_AUTH_TOKEN`               |                                                                                 | Bearer token for `require_auth`; overrides `auth_token` in `config.toml`                                        |
| `AGENTSVIEW_MODE`                     |                                                                                 | `serve` or `pg-serve`; turns on the [container deployment](/docs/configuration/#container-deployment) variables |
| `AGENTSVIEW_HOST`                     |                                                                                 | Listener address under `AGENTSVIEW_MODE`; non-loopback needs authentication                                     |
| `AGENTSVIEW_REQUIRE_AUTH`             |                                                                                 | Require bearer authentication under `AGENTSVIEW_MODE`                                                           |
| `AGENTSVIEW_NO_BROWSER`               |                                                                                 | Do not open a browser under `AGENTSVIEW_MODE`                                                                   |
| `AGENTSVIEW_AUTH_TOKEN_FILE`          |                                                                                 | Bearer token file under `AGENTSVIEW_MODE`; overrides `auth_token` in `config.toml`                              |
| `AGENTSVIEW_SKILLS_SERVER`            |                                                                                 | Remote daemon URL baked into `skills install` examples; not a default for `session` commands                    |
| `AGENTSVIEW_SKILLS_SERVER_TOKEN_FILE` |                                                                                 | Token file path baked into `skills install` examples with `AGENTSVIEW_SKILLS_SERVER`                            |
| `AGENTSVIEW_PG_URL`                   |                                                                                 | PostgreSQL connection URL                                                                                       |
| `AGENTSVIEW_PG_MACHINE`               |                                                                                 | Machine name for PG push sync                                                                                   |
| `AGENTSVIEW_PG_SCHEMA`                | `agentsview`                                                                    | PostgreSQL schema name                                                                                          |
| `AGENTSVIEW_PG_ALLOW_INSECURE`        |                                                                                 | Allow a plaintext default PostgreSQL target under `AGENTSVIEW_MODE`                                             |
| `AGENTSVIEW_EMBEDDINGS_ENDPOINT`      |                                                                                 | Embeddings endpoint for adopting a recipe published to PostgreSQL, under `AGENTSVIEW_MODE`                      |
| `AGENTSVIEW_EMBEDDINGS_API_KEY`       |                                                                                 | Inline API key for `AGENTSVIEW_EMBEDDINGS_ENDPOINT`; cannot be combined with the file form                      |
| `AGENTSVIEW_EMBEDDINGS_API_KEY_FILE`  |                                                                                 | API key file for `AGENTSVIEW_EMBEDDINGS_ENDPOINT`                                                               |
| `AGENTSVIEW_EMBEDDINGS_BATCH_SIZE`    | `32`                                                                            | Positive number of inputs per embeddings request when adopting a published PostgreSQL recipe                    |
| `AGENTSVIEW_CLICKHOUSE_URL`           |                                                                                 | ClickHouse connection URL for the default target                                                                |
| `AGENTSVIEW_CLICKHOUSE_DATABASE`      | `agentsview`                                                                    | ClickHouse database name for the default target                                                                 |
| `AGENTSVIEW_CLICKHOUSE_MACHINE`       |                                                                                 | Machine name for ClickHouse push                                                                                |
| `AGENTSVIEW_GITHUB_TOKEN`             |                                                                                 | GitHub token used for local Gist publishing fallback and `agentsview report stats` PR aggregation               |
| `AGENTSVIEW_DISABLE_UPDATE_CHECK`     |                                                                                 | Set to `1` to disable the update check                                                                          |
| `AGENTSVIEW_ARCHIVE_CONTENT`          | `full`                                                                          | Archive storage policy when `config.toml` sets none: `full`, `transcripts`, or `usage`                          |
| `AGENTSVIEW_NO_DAEMON`                |                                                                                 | Set to `1`, `true`, `yes`, or `on` to disable CLI daemon auto-start                                             |
| `AGENTSVIEW_DAEMON_IDLE_TIMEOUT`      | `20m`                                                                           | Override idle self-shutdown duration for automatically started and `serve --background` daemons                 |

Environment variables override the built-in defaults. Set them in your shell
profile or pass them inline:

```bash
AGENTSVIEW_DATA_DIR=/tmp/av-test agentsview serve
```

### Search text in images

AgentsView can index text inside retained tool-result images using a local
GLM-OCR service. Enable it in [configuration](configuration.md#image-text) and
restart the daemon.

The endpoint must be an `http://` URL on a loopback host with an explicit port
and the path `/glmocr/parse`. It defaults to
`http://127.0.0.1:30004/glmocr/parse` and must serve the
[Docbank v0.17.0 deployment](https://github.com/kenn-io/docbank/tree/v0.17.0/deploy/glmocr),
which requires an NVIDIA GB10 host. AgentsView reads PNG, JPEG, and WebP
images up to 64 MiB stored inline or in the asset directory. It skips and logs
larger images, and it skips images in other formats. Indexing starts with the
daemon and continues in the background, so searches can return partial results
at first. AgentsView checks for new images every minute and retries missing or
damaged assets and failed extractions with backoff. You can search image text
from the web palette or the CLI, for example with
`agentsview session search 'invoice 42' --in tool_result`. Substring and regex
`tool_result` searches include image text, while other `--in` sources and
in-session find search only text shown in the transcript. Each new extraction
goes out on the next incremental push to PostgreSQL and ClickHouse, so
`pg serve` and `clickhouse serve` return the same matches. Removing the last
retained copy of an image deletes its extracted text, and disabling image text
and restarting clears extracted text and removes replica matches on the next
push. Session and artifact exports carry image references or placeholders
instead of extracted text.
