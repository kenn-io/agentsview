# Usage Aggregate Cache

## Purpose

SQLite aggregate usage reads use a disposable sibling database so a warm request
does not rank, price, and transfer every token-bearing message. The archive
remains authoritative; deleting a recognized cache generation loses no user
data.

The cache serves daily usage, top sessions, billed-session counts, and relaxed
matching counts. Per-session detail remains on the bounded live path. PostgreSQL
keeps its live implementation and is checked against SQLite by the
complete-result `pgtest` fixture.

The release target is a complete warm 30-day CLI result in at most two seconds
on the protected production-scale clone, with byte-identical results before and
after cache construction.

## Derived data

The cache has two internal layers:

1. Narrow, timezone-neutral facts contain parsed message and usage-event fields
   but no transcript content. They make rebuilding another timezone
   independent of the 39 GB archive and its JSON columns.
1. Timezone rollups contain daily contributions at logical grain
   `(session_id, local_day, model)` plus activity rows and narrow dedup
   exceptions.

The facts layer is a build substrate only. Warm aggregate requests read rollups
and exceptions; they never fall back to the live aggregate path.

### Daily rows

Each daily row stores token categories, web-search requests, pricing identity,
per-row-rounded estimated cost, savings, authoritative cost markers, request
counts, and discarded-snapshot output. Go owns model resolution, rate-band
selection, money rounding, and result assembly.

Project, machine, automation state, termination state, curation fields, and
other filters remain live archive metadata. Rollup installs bake only `agent`
and `started_at`: `agent` participates in general dedup keys, while `started_at`
supplies the date for facts without their own timestamp. Both exact values are
verified on every read.

### Dedup exceptions

Any fact that can participate in Claude snapshot deduplication or general
`source:`/`usage:` deduplication is stored as a narrow exception instead of a
daily contribution. This conservative boundary makes cross-day, cross-model,
cross-session, and snapshot-to-general cases exact without a second
component-classification system.

At read time the query loads only exception rows intersecting the requested
window from the currently verified candidate sessions. It then applies the
existing window-scoped ranking, attribution, filtering, and pricing rules in Go.
Ordinary facts stay on the indexed daily path.

Because exceptions are installed per source session, changing one transcript
rebuilds only that session. A cross-session winner still changes immediately:
the next request verifies every candidate session and resolves the group from
the latest exception rows of all members.

Cursor usage uses the same exception representation under a synthetic source
install keyed by the Cursor high-water mark. User-message activity has compact
`(session_id, day, model)` rows for relaxed counts.

## Freshness contract

An aggregate request:

1. captures the archive database ID, candidate sessions, source fingerprints,
   exact `agent` and `started_at`, live filter metadata, pricing rows, Cursor
   high-water mark, and requested timezone;
1. closes the archive snapshot before acquiring cache write locks;
1. opens the generation named by cache format and archive database ID;
1. fills missing or stale normalized facts for only the candidate sessions;
1. builds missing or stale rollups for the requested timezone;
1. rechecks source fingerprints and baked metadata before installing; and
1. verifies all required installs inside one pinned cache read transaction
   before assembling the result.

The pricing generation uses the canonical, order-independent effective-pricing
digest. Daily rows also retain the resolved per-model rate hash. Pricing changes
therefore invalidate the appropriate derived data without relying on a
timestamp.

`sync_marker` is not a monotonic version: its trigger recomputes a maximum of
mutable timestamp fields and the value can decrease. Installation compares the
complete source fingerprint after extraction rather than ordering marker values.
A hard-deleted session is dropped as newer state. Other moving-source or
archive-busy races retry at most three times, then fail instead of returning
stale usage.

Request cancellation detaches the waiter from shared fill work. The fill keeps
running so retry storms converge. `cached_at` is diagnostic only.

## Files and generations

The filename contains the cache schema version and archive `database_id`. Schema
or database-ID changes select a fresh generation rather than migrating the
archive. Before deleting or replacing any generation, both SQLite
`application_id` and `usage_cache_metadata.cache_kind` must identify it as an
agentsview usage cache; a filename match is insufficient.

If the sibling directory is unwritable, the process uses the same schema and
read path in a temporary database and warns that it will rebuild after restart.

Timezone identity is stable across request windows. Named zones use their IANA
name. When the process-local zone reports only `Local`, agentsview resolves the
platform zoneinfo symlink when possible and otherwise fingerprints timezone
rules from 1970 through 2100. This prevents lazy `time.Local` initialization or
a different request range from creating another generation.

## Background maintenance

After HTTP readiness, a writable daemon fills sessions newest-first in batches
of at most 256. It builds the process-local rollups and up to eight most
recently requested named timezones. Mutation work is enqueued only after archive
commit; the cache is never filled inside an archive write transaction.

The worker runs `PRAGMA optimize` between batches, performs bounded incremental
vacuum when the freelist is large, and runs full `ANALYZE` after generation
creation and completed backfill. Deletion-journal sweeps are hygiene; aggregate
reads require current archive candidates and cannot expose orphaned cache rows.

## Verification and gates

Correctness tests compare public rollup results with the permanent live oracle
over seeded random windows, timezones, filters, DST boundaries, pricing bands,
reported costs, Cursor rows, null timestamps, and cross-session dedup groups.
Mutation tests cover transcript replacement, resync fingerprints, pricing,
`agent`, `started_at`, deletion, schema generation, and database-ID changes.

The protected-clone release check compares 7-day, 30-day, and all-history
results byte for byte. Performance reporting separates cold construction from
warm reads and reports 1-day, 7-day, 30-day, and all-history results. The warm
path must not scan normalized facts.

Current protected-clone measurements after canonical pricing and timezone
identity fixes are approximately 0.19 seconds for 1 day, 0.35 seconds for 7
days, and 0.85 seconds for 30 days through the offline CLI. All-history warm
reads are approximately six seconds. Cold construction remains proportional to
uncached candidate-session history and is intentionally handled newest-first in
the background.
