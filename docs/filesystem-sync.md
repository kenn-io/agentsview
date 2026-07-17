---
title: Session Source Sync
description: View sessions from multiple machines through transported filesystem roots or direct S3 ingestion
---

AgentsView can label session roots with the machine that produced them. This
supports a simple multi-machine topology without PostgreSQL:

1. Each source machine writes its normal agent session files.
1. Git, rsync, a file-copy job, or a shared filesystem transports those native
   layouts out of band, or Claude/Codex sessions are uploaded to S3-compatible
   storage.
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

`agent` must be a supported AgentsView parser name. `dir` can be a filesystem
root in that agent's native layout. Claude and Codex also accept an `s3://`
root. `machine` uses the same machine label shown in session filters and
configured by `[pg].machine_name`.

For filesystem roots, an omitted `machine` uses the primary viewer's hostname.
For S3 roots, an omitted `machine` retains the existing machine derivation from
the segment before `raw`, with the existing local-host fallback when that
segment is unavailable.

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
variable, its explicit machine label overrides the shorthand root's attribution.

S3 roots can be configured either through `session_sources` or the existing
Claude/Codex per-agent arrays:

```toml
[[session_sources]]
agent = "codex"
dir = "s3://agent-archive/laptop/raw/codex"
machine = "laptop"
```

An explicit S3 machine overrides the path-derived label and changes the
established machine-based S3 session ID prefix consistently. Other agents do
not support direct S3 ingestion and must use filesystem roots. Native
SQLite-backed agent stores are supported when `dir` points at their filesystem
root.

## Filesystem Transport Rules

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

## Filesystem Transport Examples

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
- S3 sources retain machine-based session ID prefixes. An explicit structured
  label replaces the path-derived prefix; omitting it preserves existing S3
  derivation and fallback behavior.
- A newly transported or changed file is normally detected by the filesystem
  watcher. AgentsView also performs a full periodic sync every 15 minutes.
- S3 roots are not watched. Initial, manual, and periodic syncs list object
  metadata and download only changed or stale sessions.
- Roots that cannot be watched fall back to polling, as described under
  [Large Watch Trees](/configuration/#large-watch-trees).
- A machine label changes attribution, not source identity or conflict
  resolution. Do not intentionally place different sessions with the same
  native ID in separate roots.
- Deleting a transported source file does not automatically erase the archived
  session. The local SQLite database is a persistent archive; use pruning
  tools when removal is intended.

Session source sync is intentionally simple: one primary AgentsView owns the
archive and UI. PostgreSQL remains the better fit for independently running
AgentsView instances that must contribute to or read from a shared live store.
