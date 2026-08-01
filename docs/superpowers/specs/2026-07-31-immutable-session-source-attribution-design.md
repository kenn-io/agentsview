# Immutable Session Source Attribution Design

## Summary

Machine-labeled filesystem sources will assign a session's machine when that
session is first ingested. Once persisted, the label is immutable: ordinary
sync, watcher refresh, provider refresh, trash handling, and `sync --full`
preserve the stored value. Editing a configured source label affects only
sessions not already present in the archive.

This cleanup also removes DuckDB work that has already landed independently,
preserves user-entered path spelling on every platform, and updates the
documentation to describe the narrower contract.

## Goals

- Keep `[[session_sources]]` as an additive filesystem-source configuration.
- Assign configured machine labels to newly discovered sessions.
- Preserve a stored machine label through every later write and archive rebuild.
- Preserve trash, source-missing, curation, and project-identity behavior.
- Keep periodic and watcher work bounded by the changed batch.
- Preserve configured path spelling while using cleaned paths only for
  comparison.
- Remove the DuckDB overlap now provided by merged PR #1302.

## Non-goals

- Relabeling sessions already stored in the archive.
- Adding a one-shot relabel command.
- Supporting S3 roots through `session_sources`.
- Changing native session-ID deduplication.
- Changing PostgreSQL ownership or identity semantics.

## Approaches Considered

### Immutable attribution after ingestion

This is the selected approach. Existing rows remain authoritative whenever a
session is refreshed or copied through a full rebuild. It removes continuous
reattribution state and makes configuration changes safe and predictable.

### Continue targeted reattribution fixes

This would repair each newly discovered writer, trash path, snapshot path, and
reconciliation edge independently. The branch history shows that this expands
the persistent-archive surface and repeatedly creates new consistency bugs, so
it is rejected.

### Add an explicit relabel migration now

A one-shot command could eventually update sessions and dependent identity state
transactionally. It is deferred because it needs its own invariants, design, and
recovery tests; it is not required to ship labeled ingestion.

## Storage and Sync Contract

New sessions receive the machine resolved from their originating configured
filesystem root. An upsert for an existing session must retain the database
row's machine instead of adopting a newly configured label. Full resync copies
existing archive rows and metadata into the replacement database and retains
their stored machine labels, including trashed and source-missing sessions.

Because relabeling is not part of sync, the engine no longer needs machine-only
updates for trashed sessions, an all-machines baseline scan, or a second
baseline index keyed without machine. Watch reconciliation continues to use the
stored ownership baseline for the machine that originally admitted the session.

## Configuration Contract

`normalizeSessionSourceDir` validates and trims a structured source directory
but does not rewrite its separators or spelling. A separate comparison key may
use an absolute, cleaned path and case-fold it on Windows to deduplicate
equivalent roots on the current platform. Legacy arrays, environment-derived
paths, and structured entries therefore retain the spelling supplied by the
user while comparisons remain normalized.

## DuckDB Boundary

The branch drops every DuckDB change relative to current `main`. Merged PR #1302
already preserves source-machine attribution, includes the fingerprint change,
removes publisher-machine filtering where required, and bumps the disposable
mirror schema. This PR does not duplicate or modify that behavior.

## Documentation

The README and filesystem/configuration guides will say that a label is fixed at
first ingestion. `sync --full` preserves it and is not a relabel operation. The
pull request description should cover only filesystem source configuration and
immutable source attribution; its DuckDB and retroactive-relabel claims must be
removed.

## Testing

Behavior tests will use real temporary archives and filesystem fixtures to show
that:

- changing a configured label does not change an existing active session;
- changing a label does not change a trashed session;
- `sync --full` retains the stored label while preserving archive state;
- a newly discovered session receives the new configured label;
- moving or deleting files still produces the expected watcher reconciliation;
- structured Windows-style path spelling is retained while duplicate comparison
  stays normalized;
- the branch compiles without the removed reattribution methods and query.

Focused config, database, sync, and command tests run first. The final gate is
`go fmt ./...`, `go vet ./...`, the full Go test suite, and repository lint when
practical.

## Compatibility and Rollout

No new database schema or data-version migration is required for immutable
attribution. Existing archives keep their current labels. Users who change a
source label see that label only on sessions discovered afterward. A future
explicit relabel command can build on this stable contract without making
configuration reloads destructive.
