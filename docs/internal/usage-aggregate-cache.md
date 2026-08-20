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

Deduplication groups are classified per group when a session's rollup is built.
A group is finalized into daily rows only when its resolution provably cannot
vary with the query window or live filters:

- every member belongs to the building session and shares one local date (query
  windows are whole local days, so such a group is inside or outside any
  window as a unit);
- for general `source:`/`usage:` groups, every member also shares one model and
  headless state, because live model and automation filters apply before
  general ranking;
- no member links snapshot and general dedup (a snapshot survivor carrying a
  usage key re-enters general ranking at read time);
- no member carries a Copilot authoritative cost, whose per-session selection is
  order-dependent across the whole window; and
- the group's identity appears in no other cached session and, for usage keys,
  not in the Cursor fact store. Partial identity indexes on `usage_facts`
  serve this check conservatively (for example, `source:` identity ignores the
  baked agent).

Finalization applies the same ranking rules as the read path: the
greatest-output snapshot wins, attribution follows the earliest row, the maximum
web-search count carries over, and losing snapshot output is recorded as
discarded. Only irreducible groups are stored as narrow exceptions, so exception
volume scales with genuine cross-session, cross-day, and cross-model duplicates
rather than with token-bearing messages. Cursor facts stay entirely on the
exception tier: their keys may collide with session usage keys and their filters
depend on per-row headless state.

At read time the query loads only exception rows intersecting the requested
window from the currently verified candidate sessions. It then applies the
existing window-scoped ranking, attribution, filtering, and pricing rules in Go.
Finalized groups and ordinary facts stay on the indexed daily path.

Because rollups are installed per source session, changing one transcript
rebuilds that session. Whenever a fill, Cursor batch, or deletion changes the
set of dedup identities a session contributes, the same cache transaction
deletes the timezone rollup installs of every other session holding a changed
identity, and rollup installation re-verifies inside its transaction that no
finalized identity gained an outside member, retrying as a moved source
otherwise. A finalized daily row therefore never survives gaining a sibling, and
a cross-session winner still changes immediately: the next request verifies
every candidate session and resolves the group from the latest exception rows of
all members.

Cursor usage uses the same exception representation under a synthetic source
install keyed by the Cursor high-water mark. Matching-session activity has
compact `(session_id, day, model)` rows for relaxed counts. An eligible fact
with neither its own timestamp nor a session start is retained with an empty
day: unbounded matching counts include it, while bounded reads exclude it.

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

Timezone identity is stable across request windows. Named zones combine their
IANA name with a fingerprint of timezone rules from 1970 through 2100, so a
zoneinfo update cannot reuse obsolete day buckets. When the process-local zone
reports only `Local`, agentsview resolves the platform zoneinfo symlink when
possible and otherwise uses the same rule fingerprint. This prevents lazy
`time.Local` initialization or a different request range from creating another
generation.

## Background maintenance

After HTTP readiness, a writable daemon fills sessions newest-first in batches
of at most 256. It builds the process-local rollups and up to eight most
recently requested named timezones. Mutation work is enqueued only after archive
commit; the cache is never filled inside an archive write transaction.

The worker runs `PRAGMA optimize` between batches, performs bounded incremental
vacuum when the freelist is large, and runs full `ANALYZE` after generation
creation and completed backfill. Deletion-journal sweeps are hygiene; aggregate
reads require current archive candidates and cannot expose orphaned cache rows.
If source data moves during a pass, the worker recaptures and retries the whole
snapshot at most three times. Installed fingerprints let unchanged batches be
reused across attempts.

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

Warm reads must scale with daily rows and genuine duplicate groups, not with
token-bearing messages; the release check reports exception cardinality along
with timings. Slow foreground requests log privacy-safe phase timings and row
counts. Cold construction remains proportional to uncached candidate-session
history and is intentionally handled newest-first in the background.
Protected-clone timings are re-measured for each release against the two-second
warm 30-day gate rather than recorded here.
