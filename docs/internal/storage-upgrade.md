# Storage upgrade and rollback

The Bun transition applies non-destructive schema changes on writable open.
Parser resync is a separate operation: it builds a temporary archive, copies
orphaned sessions and user-managed metadata, validates it, and atomically swaps
it into place. Do not delete the persistent archive to trigger resync.

## Before opening the new version

Stop the daemon with `agentsview daemon stop`. Also stop foreground servers,
sync commands, and any other process using the same data directory. Keep them
stopped throughout backup or restoration. Locate the configured `data_dir`; the
SQLite archive is its `sessions.db` file.

Copy the entire stopped data directory to a new backup directory outside it.
Include any `sessions.db-wal` and `sessions.db-shm` files: copying only
`sessions.db` can omit committed transactions. Do not copy a running archive
with ordinary filesystem copy commands.

For example, on macOS or Linux, replace these paths with the configured data
directory and a backup destination that does not yet exist:

```sh
cp -a /path/to/agentsview-data /path/to/agentsview-pre-bun-backup
sqlite3 -readonly /path/to/agentsview-pre-bun-backup/sessions.db 'PRAGMA quick_check;'
```

Require `ok` from the integrity check. Keep the old binary or its exact release
available with this backup. Preserve the original data directory until the
backup is complete and checked.

The archive filesystem needs free space for a complete replacement archive, its
indexes, and temporary journal/WAL files while the original still exists. The
backup needs its own space as well. An exact multiplier cannot be promised: new
parser output and source growth can change replacement size. If space is
insufficient, free unrelated space or move the backup to another filesystem; do
not remove the archive to make room.

## Complete the resync

Run `agentsview sync` with the new version and the same configuration. It
selects full resync automatically when the archive data version requires it;
writable server startup also checks for this condition. Opening the schema alone
does not mean the parser resync has finished.

Wait for the full-resync completion output and check for errors or a reported
safety abort. An aborted resync can fall back to incremental sync, which does
not establish completion of the upgrade. A subsequent `agentsview sync` should
no longer request the data-version resync. Check known sessions, including
sessions whose source files are gone, and user-managed names and pins before
discarding the pre-upgrade backup.

## Restore the previous version

Stop all processes using the data directory again. Retain the upgraded data
separately, then restore the entire backup into the configured location; do not
copy the old database over a directory containing the upgraded WAL files. For
example, with a retention destination that does not yet exist:

```sh
mv /path/to/agentsview-data /path/to/agentsview-upgraded-retained
cp -a /path/to/agentsview-pre-bun-backup /path/to/agentsview-data
sqlite3 -readonly /path/to/agentsview-data/sessions.db 'PRAGMA quick_check;'
```

Require `ok`, then start the previous binary and verify known sessions and
user-managed metadata. Keep both the backup and retained upgraded directory
until verification succeeds. PostgreSQL mirrors require the matching binary;
DuckDB mirrors are disposable and can be rebuilt from the restored archive.
