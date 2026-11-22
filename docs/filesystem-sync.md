---
title: Filesystem Session Sync
description: View sessions from multiple machines by transporting native agent session directories to one AgentsView instance
---

You can view sessions from multiple machines without PostgreSQL by transporting
native agent session directories to one AgentsView instance and labeling each
filesystem root with the machine that produced it:

1. Each source machine writes its normal agent session files.
1. Git, rsync, a file-copy job, or a shared filesystem transports those native
   layouts out of band.
1. One primary AgentsView instance scans the received roots into its local
   SQLite archive.

This setup is a **primary-viewer topology**. If several viewers need a shared
live database or a read-only database-backed endpoint, use
[PostgreSQL sync](/docs/pg-sync/) instead.

## Configure session sources

Add one `[[session_sources]]` table for each received agent root:

```toml
[[session_sources]]
agent = "copilot"
dir = "/srv/session-archive/buildbox/copilot"
machine = "0123456789abcdef0123456789abcdef" # Buildbox installation ID

[[session_sources]]
agent = "claude"
dir = "/srv/session-archive/buildbox/claude/projects"
machine = "0123456789abcdef0123456789abcdef" # Buildbox installation ID

[[session_sources]]
agent = "codex"
dir = "/srv/session-archive/laptop/codex/sessions"
machine = "fedcba9876543210fedcba9876543210" # Laptop installation ID
```

Set `agent` to a supported AgentsView parser name and `dir` to a filesystem root
in that agent's native layout. For a remote root, set `machine` to the peer's ID
from `telemetry-install-id`. For a local root, omit `machine` to use the primary
viewer's installation ID. Display labels are separate from these keys; setting
`local_machine_name` changes the displayed name after a daemon restart.

If an existing local source sets `machine` to a hostname, remove that setting.
AgentsView uses source keys literally, and adopted hostname aliases apply only
to filters and URLs. A matching display label does not make a source local.

Per-agent `dirs` arrays and environment variables select session roots, and
structured sources add to them:

```toml
[agents.copilot]
dirs = ["~/.copilot"]

[[session_sources]]
agent = "copilot"
dir = "/srv/session-archive/buildbox/copilot"
machine = "0123456789abcdef0123456789abcdef" # Buildbox installation ID
```

AgentsView expands `~/` and resolves local roots to absolute paths with symbolic
links resolved, scanning equivalent roots once. When a structured source names
the same root as a per-agent array, default, or environment variable, the
structured entry supplies the machine key.

`session_sources` accepts filesystem roots only. For `s3://` roots, keep using
the existing `agents.<id>.dirs` arrays; S3 ingestion has established
machine-derived ID-prefix behavior that differs from filesystem labeling. Native
SQLite-backed agent stores work when `dir` points at their filesystem root.

## Transport rules

Sync the **source agent's session files and native directory layout**. Never
copy AgentsView's `sessions.db`, `sessions.db-wal`, `sessions.db-shm`, or
`vectors.db`. Those files belong to the primary viewer, and copying a live
SQLite database or WAL can corrupt or fork the archive.

For every destination source tree, follow these rules:

- Use one writer. Multiple transport jobs must not update the same tree.
- When possible, transfer into a staging path, then rename or switch the
  completed tree into place.
- Preserve filenames, relative paths, and companion metadata files.
- Avoid exposing partially copied files. AgentsView retries changed files, but
  atomic publication prevents transient parse errors and incomplete sessions.
- Treat the primary copy as read-only input to AgentsView.

## Transport examples

The transport is independent of AgentsView. These examples show the directory
shape; adapt scheduling and authentication to your environment.

### Git

Commit native session trees on the source machine, pull into a staging checkout
on the primary machine, and then atomically replace the published checkout:

```text
session-archive/
├── buildbox/
│   ├── claude/projects/...
│   └── copilot/session-state/...
└── laptop/
    └── codex/sessions/...
```

Git fits modest archives where commit history is useful. Avoid running
AgentsView against a checkout while `git pull` is rewriting it; publish a
completed checkout or worktree instead. Session files can contain prompts, tool
output, and source excerpts, so use access controls appropriate for sensitive
data.

### rsync or file copy

Copy each machine into its own destination tree. Passing `--delay-updates`
reduces the window in which completed files appear partially updated:

```bash
rsync -a --delete --delay-updates \
  source-host:/home/user/.copilot/ \
  /srv/session-archive/buildbox/copilot/
```

For transports without delayed updates, copy to a sibling staging directory and
rename it into place. Do not let two sources use the same destination tree.

### NFS or shared mount

Mount each source machine's exported session directory on the primary viewer,
preferably read-only:

```toml
[[session_sources]]
agent = "claude"
dir = "/mnt/sessions/buildbox/claude/projects"
machine = "0123456789abcdef0123456789abcdef" # Buildbox installation ID

[[session_sources]]
agent = "copilot"
dir = "/mnt/sessions/laptop/copilot"
machine = "fedcba9876543210fedcba9876543210" # Laptop installation ID
```

Shared mounts remove the copy step, though freshness and watcher behavior depend
on the filesystem. Some network filesystems do not deliver local filesystem
events reliably, and the daily archive audit remains the backstop. On battery,
the routine audit waits for AC power, while required archive rebuilds still run.

## Identity, filtering, and freshness

- AgentsView stores the machine key per discovered source root. You can filter
  sessions by `machine` in the session browser, API, and analytics views.
- A session keeps the machine key it received when it was first ingested.
  Editing a source's `machine` value affects newly discovered sessions but does
  not retroactively relabel existing ones, even when their source files later
  change. `agentsview source sync --full` also preserves the stored key. The
  [installation upgrade](/docs/configuration/#upgrading-historical-machine-keys)
  adopts recorded local keys; use
  [`db adopt-machine`](/docs/commands/#agentsview-db-adopt-machine) to select
  additional historical keys you own.
- Filesystem machine keys do **not** namespace session IDs. If you copy the same
  native session into two configured roots, AgentsView still deduplicates it by
  the agent's native session ID.
- The filesystem watcher normally detects a newly transported or changed file.
  Agents with partial watch coverage are also reconciled every 15 minutes, and a
  daily archive audit covers the rest; see
  [Sync Behavior](/docs/configuration/#sync-behavior).
- Roots that cannot be watched fall back to polling, as described under
  [Large Watch Trees](/docs/configuration/#large-watch-trees).
- A machine key changes attribution, not source identity or conflict resolution.
  Do not intentionally place different sessions with the same native ID in
  separate roots.
- Deleting a transported source file does not automatically erase the archived
  session. The local SQLite database is a persistent archive; use pruning tools
  when you want to remove sessions.

Filesystem sync is intentionally simple: one primary AgentsView stays in charge
of the archive and UI. PostgreSQL remains the better fit for independently
running AgentsView instances that must contribute to or read from a shared live
store.
