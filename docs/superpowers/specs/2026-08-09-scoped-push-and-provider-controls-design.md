# Scoped Push and Session Provider Controls Design

## Summary

`agentsview pg push --watch` currently discards the watcher batch that caused a
push. The daemon consequently performs an unscoped sync before every push,
rediscovering every configured session provider and walking the existing archive
even when only a few sessions changed. Gemini makes this especially expensive
because its default root can exist for unrelated tools and project metadata
resolution probes missing repositories even when there are no Gemini CLI
sessions.

This change preserves watcher scope through the push pipeline, adds an explicit
way to disable session providers, exposes those controls in Settings, and makes
empty Gemini discovery avoid project metadata work. Existing archived sessions
remain visible and unchanged.

## Goals

- Keep watch-triggered PostgreSQL pushes proportional to the changed batch.
- Retain authoritative reconciliation after watcher overflow, lost events,
  directory renames, and other ambiguous changes.
- Let users explicitly disable expensive or unused session providers.
- Expose the same provider controls in the web Settings page.
- Avoid Gemini project resolution when no Gemini session files exist.
- Preserve manual and full-push behavior.

## Non-goals

- Removing or hiding sessions already archived for a disabled provider.
- Disabling agent execution used by Recall or other interactive features.
- Hot-swapping the running sync engine and filesystem watcher configuration.
- Adding a persistent Gemini project cache.
- Changing PostgreSQL schemas, ownership, or incremental-push semantics.

## Approaches Considered

### Preserve watcher batches end to end

This is the selected approach. The watch-mode process retains and coalesces the
actual `WatchBatch`, sends its public reconciliation data to the daemon, and the
daemon performs a scoped sync followed by the push under the sync engine's
serialization boundary. It fixes the root cause without assuming that a separate
watcher has already committed the same event.

### Assume the daemon watcher already synchronized the change

The watch-mode process could mark automatic pushes as already synchronized and
skip the daemon's pre-push sync. This is smaller, but the two watchers and the
HTTP request are independent. The push could win the race and publish stale
archive state, so this approach is rejected.

### Add provider disabling only

An explicit Gemini disable switch removes the worst local offender, but every
watch push would still rediscover all remaining providers. This is useful as a
control but incomplete as the CPU fix, so it is included only as one layer of
the selected design.

## Configuration Contract

A top-level `disabled_agents` array contains canonical session-provider IDs:

```toml
disabled_agents = ["gemini"]
```

Values are trimmed, deduplicated, and stored in stable registry order. Unknown
or non-session provider IDs produce a clear configuration error, including on a
Settings API update, so a typo cannot appear to disable work while doing
nothing.

Disabling a provider excludes it from local discovery, sync, watch plans,
watcher fallback polling, explicit session-source diagnostics, and remote
session-source collection and ingestion. It does not delete archived data and
does not affect agent binaries used outside session-source processing, such as
Recall execution. The enabled-provider predicate is centralized and applied at
sync engine and watch-plan boundaries rather than repeated as ad hoc checks in
individual providers.

Configuration-file and Settings changes take effect for ingestion after the
relevant long-running ingestion processes restart: the daemon and any separate
`pg push --watch` or `duckdb push --watch` process. The running daemon retains
an immutable startup provider set even though GET Settings immediately returns
the new persisted selection. This keeps engines, watcher coverage, polling, and
remote collection consistent. Other persisted settings continue to apply
according to their existing behavior.

Disabled-provider content already present in a persistent HTTP mirror is
retained but ignored. It is not refreshed while disabled; re-enabling the
provider refreshes its manifest and files before ingestion.

## Watch Batch Transport and Coalescing

Watch-mode push notifications carry a transport form of `WatchBatch` containing
changed paths, rename metadata, reconciliation roots, `FullSync`, and
`LostEvents`. Backend lifecycle tokens remain local to the watcher and are not
serialized.

The push loop keeps one bounded pending batch while a push is running:

- ordinary paths, reconciliation roots, and renames are deduplicated and merged;
- `LostEvents` is sticky;
- any full-sync input promotes the pending batch to full sync;
- exceeding the existing watcher batch count or byte bounds promotes to
  `FullSync: true, LostEvents: true`, because discarding the individual paths
  also invalidates path-dependent freshness caches;
- the push loop owns retry: acknowledgements and their exact scope remain
  pending across failed attempts and complete only after eventual success,
  caller cancellation, or final shutdown failure;
- a failed push restores its pending batch ahead of concurrent arrivals without
  changing waiter order.

Startup, manual, and other unscoped dirty notifications keep their existing
unscoped synchronization behavior. Only notifications originating in a known
watch batch use the scoped path.

## Daemon Synchronization Flow

The daemon request accepts the transport batch as an optional field. A new sync
engine operation performs the same classification and reconciliation choices as
a local watcher callback, then runs the PostgreSQL push while still serialized
against other sync work. Direct local watch pushes call the same engine
operation and do not run the old follow-up `SyncAll`; daemon-delegated pushes
transport the batch so the daemon can perform that operation instead.

For ordinary file batches, only the changed sources are classified, parsed, and
committed. Explicit reconciliation roots and rename metadata use their existing
provider-aware reconciliation paths. Full sync, lost events that cannot be
narrowed safely, overflow, directory renames, a stale archive, and an explicit
full push retain authoritative archive-scale reconciliation.

The daemon treats malformed or internally contradictory batch metadata as a bad
request rather than silently widening or narrowing the sync. Older clients that
omit the field retain current behavior. A new client detects a daemon whose API
predates scoped batches and retries that push without the optional fields, so a
user-managed daemon upgrade cannot wedge watch mode. PostgreSQL project filters
and vector-generation promotion remain independent of sync scope.

## Gemini Empty-Source Fast Path

Both slice-based and streaming Gemini discovery defer loading project metadata
until the first actual session file is found. An existing Gemini root, project
metadata, or project directory without a matching session file therefore returns
no sources without hashing `projects.json`, resolving missing Git roots, or
scanning sibling worktrees.

When sessions do exist, metadata is still resolved once per root or lazy
discovery map and reused for all sources in that pass. A persistent cache is not
added: correct invalidation spans project metadata, repository layout, and
worktree changes, while scoped synchronization and the empty-source fast path
remove the measured repeated work without stale project mappings.

## Settings API and UI

The Settings response adds `disabled_agents`; the update request accepts the
complete desired array. The API validates the array before persisting it and
returns the normalized value. Read-only servers reject the update through the
existing settings mutation behavior.

The existing read-only Agent Directories panel becomes a Session Providers
panel. It lists every configurable session provider in stable registry order,
including disabled providers, with:

- the product/provider label;
- an enabled toggle;
- configured directories, or the existing not-configured state;
- disabled controls in read-only mode;
- the existing saving and error feedback.

A toggle saves immediately. All provider toggles remain disabled while one save
is pending so complete-array requests cannot race and overwrite each other. If
saving fails, the control returns to the last server-confirmed state. After a
successful provider change, the panel displays a localized notice that the
daemon and any separate watch processes must be restarted before ingestion
changes take effect. The UI does not restart or stop a process itself.

All new user-facing copy uses Paraglide messages in English, Simplified Chinese,
Traditional Chinese, Korean, and French. Generated API types are regenerated
from the server schema rather than edited by hand.

## Error Handling and Observability

Configuration and API errors name the invalid provider ID. A scoped sync or push
failure follows existing push-loop retry and acknowledgement behavior; it does
not acknowledge watcher lifecycle work prematurely. Promotion from a bounded
batch to full reconciliation is logged with its reason. Routine scoped push logs
report aggregate counts, not individual paths, avoiding high-cardinality output.

No new metrics labels contain paths, project names, or session IDs. Existing
sync and push timing remains sufficient to compare discovery work with actual
PostgreSQL work.

## Testing

Behavior tests will cover:

- parsing, normalization, persistence, and invalid values for `disabled_agents`;
- exclusion of disabled providers from discovery, watch plans, polling, and
  pre-transfer remote collection without deleting archived sessions;
- preservation of disabled-provider sessions, messages, and push eligibility
  across local and multi-source archive rebuilds;
- transport serialization and bounded coalescing of paths, roots, renames,
  lost-event markers, acknowledgements, and full-sync promotion;
- daemon selection of scoped synchronization and authoritative fallbacks;
- Gemini slice and streaming discovery doing zero project-map work when there
  are no matching session files, while retaining correct project resolution
  when sessions exist;
- Settings loading, toggle saves, rollback on failure, read-only behavior, and
  restart notice;
- presence and compilation of all localized messages.

Tests will assert observable decisions and stored results rather than duplicate
implementation details. Go changes receive formatting, vetting, and focused
tests with the repository's FTS5/CGO configuration. Frontend validation includes
i18n compilation, static checks, component tests, and the relevant generated API
check.

## Compatibility and Rollout

The empty `disabled_agents` default is returned as `[]` and preserves all
current providers. Existing configuration files and daemon-push clients remain
valid. The optional batch field lets new clients use scoped pushes while omitted
fields preserve the existing daemon contract; new clients fall back to omission
when a daemon reports an older API. No database migration is required.

After upgrading, users may disable Gemini or any other unused session provider
in Settings or TOML and restart the daemon plus any separate push-watch process.
Existing Gemini sessions remain queryable; only future Gemini session-source
work stops.
