# Doctor Sync Design

## Goal

Add a human-readable support command that explains why agentsview startup will
run a full data-version resync, or confirms that the user is only seeing the
normal incremental startup sync.

## Command

`agentsview doctor sync`

The command is read-only. It must not open the sync engine, mutate the SQLite
database, run migrations beyond normal DB open behavior, start watchers, or
trigger file parsing.

## Report

The report should include:

- agentsview version.
- effective data directory and SQLite database path.
- database existence/readability.
- SQLite `PRAGMA user_version`.
- binary data version.
- startup decision: full data-version resync or normal initial sync.
- session counts grouped by `sessions.data_version`.
- leftover resync temp files matching `sessions.db-resync*`.
- configured agent roots, grouped by agent, with missing roots marked.
- recent relevant `debug.log` lines for data-version and resync failures.
- a likely-cause summary.

## Diagnosis Rules

- If the DB is missing, report that the next startup will create it and run the
  normal initial sync.
- If `user_version` is lower than the binary data version, report that startup
  will run a full data-version resync.
- If stale version plus relevant debug lines mention abort/swap/failure, report
  that a previous resync likely aborted before the version marker could advance.
- If stale version plus missing configured roots are present, report that
  missing roots may be causing empty discovery or failed sync work.
- If `user_version` is current or newer, report that data-version resync is not
  expected and that `Running initial sync...` is normal incremental startup
  work.

## Testing

Add focused CLI tests using existing command-test helpers. Tests should cover at
least a current database and a stale `user_version` database. Test helper
assertions should use testify.
