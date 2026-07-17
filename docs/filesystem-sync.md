---
title: Filesystem Session Sync
description: View sessions from multiple machines by transporting native agent session directories to one AgentsView instance
---

AgentsView can label filesystem session roots with the machine that produced
them. This supports a simple multi-machine topology without PostgreSQL:

1. Each source machine writes its normal agent session files.
1. Git, rsync, a file-copy job, or a shared filesystem transports those native
   layouts out of band.
1. One primary AgentsView instance scans the received roots into its local
   SQLite archive.

This is a **primary-viewer topology**. Use [PostgreSQL sync](/pg-sync/) instead
when several viewers need a shared live database or a read-only database-backed
endpoint.

## Configure Session Sources

Add one `[[session_sources]]` table per received agent root:

```toml
[[session_sources]]
agent = "copilot"
dir = "/srv/session-archive/buildbox/copilot"
machine = "buildbox"

[[session_sources]]
agent = "claude"
dir = "/srv/session-archive/buildbox/claude/projects"
machine = "buildbox"

[[session_sources]]
agent = "codex"
dir = "/srv/session-archive/laptop/codex/sessions"
machine = "laptop"
```

`agent` must be a supported AgentsView parser name. `dir` must be a filesystem
root in that agent's native layout. `machine` uses the same machine label shown
in session filters and configured by `[pg].machine_name`. If `machine` is
omitted, AgentsView uses the primary viewer's hostname.

Existing per-agent arrays and environment variables remain supported. Structured
sources are additive:

```toml
copilot_dirs = ["/home/viewer/.copilot"]

[[session_sources]]
agent = "copilot"
dir = "/srv/session-archive/buildbox/copilot"
machine = "buildbox"
```

AgentsView normalizes and deduplicates exact duplicate roots. When a structured
source names the same root as a per-agent array, default, or environment
variable, the structured entry supplies the machine label.

`session_sources` accepts filesystem roots only. Keep using the existing
Claude/Codex per-agent arrays for `s3://` roots; S3 ingestion has established
machine-derived ID-prefix behavior that differs from filesystem labeling. Native
SQLite-backed agent stores are supported when `dir` points at their filesystem
root.

## Transport Rules

Sync the **source agent's session files and native directory layout**. Never
copy AgentsView's `sessions.db`, `sessions.db-wal`, `sessions.db-shm`, or
`vectors.db`. Those files belong to the primary viewer and copying a live SQLite
database or WAL can corrupt or fork the archive.

For every destination source tree:

- Have one writer. Multiple transport jobs must not update the same tree.
- Transfer into a staging path, then rename or switch the completed tree into
  place when possible.
- Preserve filenames, relative paths, and companion metadata files.
- Avoid exposing partially copied files. AgentsView retries changed files, but
  atomic publication prevents transient parse errors and incomplete sessions.
- Treat the primary copy as read-only input to AgentsView.

## Transport Examples

The transport is independent of AgentsView. These examples show the directory
shape; adapt scheduling and authentication to your environment.

### Git

Commit native session trees on the source machine, pull into a staging checkout
on the primary machine, then atomically replace the published checkout:

```text
session-archive/
├── buildbox/
│   ├── claude/projects/...
│   └── copilot/session-state/...
└── laptop/
    └── codex/sessions/...
```

Git is most suitable for modest archives where commit history is useful. Avoid
running AgentsView against a checkout while `git pull` is rewriting it; publish
a completed checkout or worktree instead. Session files can contain prompts,
tool output, and source excerpts, so use access controls appropriate for
sensitive data.

### rsync or File Copy

Copy each machine into its own destination tree. `--delay-updates` reduces the
window in which completed files appear partially updated:

```bash
rsync -a --delete --delay-updates \
  source-host:/home/user/.copilot/ \
  /srv/session-archive/buildbox/copilot/
```

For transports without delayed updates, copy to a sibling staging directory and
rename it into place. Do not let two sources use the same destination tree.

### NFS or Shared Mount

Mount each source machine's exported session directory on the primary viewer,
preferably read-only:

```toml
[[session_sources]]
agent = "claude"
dir = "/mnt/sessions/buildbox/claude/projects"
machine = "buildbox"

[[session_sources]]
agent = "copilot"
dir = "/mnt/sessions/laptop/copilot"
machine = "laptop"
```

Shared mounts remove the copy step, but freshness and watcher behavior depend on
the filesystem. Some network filesystems do not deliver local filesystem events
reliably; the periodic sync remains the backstop.

## Identity, Filtering, and Freshness

- The machine label is stored per discovered source root. Filter sessions by
  `machine` in the session browser, API, and analytics views.
- Filesystem machine labels do **not** namespace session IDs. If the same native
  session is copied into two configured roots, AgentsView continues to
  deduplicate it by the agent's native session ID.
- A newly transported or changed file is normally detected by the filesystem
  watcher. AgentsView also performs a full periodic sync every 15 minutes.
- Roots that cannot be watched fall back to polling, as described under
  [Large Watch Trees](/configuration/#large-watch-trees).
- A machine label changes attribution, not source identity or conflict
  resolution. Do not intentionally place different sessions with the same
  native ID in separate roots.
- Deleting a transported source file does not automatically erase the archived
  session. The local SQLite database is a persistent archive; use pruning
  tools when removal is intended.

Filesystem sync is intentionally simple: one primary AgentsView owns the archive
and UI. PostgreSQL remains the better fit for independently running AgentsView
instances that must contribute to or read from a shared live store.
