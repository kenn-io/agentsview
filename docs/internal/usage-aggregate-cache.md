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

The cache has three internal layers:

1. Narrow, timezone-neutral facts contain parsed message and usage-event fields
   but no transcript content. They make rebuilding another timezone
   independent of the 39 GB archive and its JSON columns.
1. Timezone rollups contain daily contributions at logical grain
   `(session_id, local_day, model)` plus activity rows and narrow dedup
   exceptions.
1. Day totals sum non-authoritative daily rows by date, filter dimensions,
   model, provider, rate, and band. Row triggers add and subtract contributions
   in the install transaction. Authoritative-cost sessions and dedup exceptions
   keep their existing per-session allocation and ranking.

The facts layer is a build substrate only. Warm aggregate requests read rollups
and exceptions; they never fall back to the live aggregate path.

### Daily rows

Each daily row stores token categories, web-search requests, pricing identity,
per-row-rounded estimated cost, savings, authoritative cost markers, request
counts, and discarded-snapshot output. Go owns model resolution, rate-band
selection, money rounding, and result assembly.

Rollup installs bake `agent`, `started_at`, `project`, `machine`, `git_branch`,
`group_key`, the derived cron label, `is_automated`, and `user_message_count`. A
journaled metadata change rebuilds that session's rows and moves its day totals
to the new dimensions. Termination and `active_since` filters still use archive
metadata and per-session rows.

After a completed timezone coverage pass, daily reads apply session and model
filters to day totals in SQL. They load install metadata only for authoritative
sessions and exception owners. SQL counts ordinary sessions by project and
agent; surviving exception owners add their distinct counts. Warm Top sessions
reads fold exception owners first, then rank their contributions together with
ordinary session rows in one SQL query. Grouped reads select representatives for
the agent and latest start, and retain contributor IDs for returned groups. The
archive selects each group's latest currently named contributor. Those
representatives and returned individual sessions share the bounded metadata
load. SQL computes the trailing remainder from the same units. Daily reads and
billed-session counts share the distinct count reader. Direct count reads skip
the day-total cost and token groups. Warm preparations skip window-session
discovery. Each archive snapshot loads its pricing catalog and computes its
digest.

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
a cross-session winner still changes immediately: the next request refreshes
journaled sessions and resolves the group from the latest exception rows of all
members.

Cursor usage uses the same exception representation under a synthetic source
install keyed by the Cursor high-water mark. Matching-session activity has
compact `(session_id, day, model)` rows for relaxed counts. An eligible fact
with neither its own timestamp nor a session start is retained with an empty
day: unbounded matching counts include it, while bounded reads exclude it.

## Freshness contract

Without a trusted timezone claim, an aggregate request:

1. captures the archive database ID, candidate sessions, source fingerprints,
   exact `agent` and `started_at`, live filter metadata, pricing rows, Cursor
   high-water mark, and requested timezone;
1. closes the archive snapshot before acquiring cache write locks;
1. opens the generation named by cache format and archive database ID;
1. fills missing or stale normalized facts for only the candidate sessions;
1. builds missing or stale rollups for the requested timezone in batches of at
   most 256 sessions, each with its own install transaction;
1. rechecks source fingerprints and baked metadata before installing; and
1. verifies all required installs inside one pinned cache read transaction
   before assembling the result.

After a completed timezone coverage pass, an aggregate request reads the archive
change journal and refreshes only journaled sessions and cached sessions that
lost their rollup install. Deleting an install queues its session in an indexed
worklist in the same transaction. Reinstalling it clears that entry, and
deleting its cached facts removes it. Requests read the worklist and, except for
daily totals, window IDs in a cache transaction, then revisions, pricing, session
metadata, and changed-session fingerprints in an archive transaction.
Deletion sweeps and notifications lower claims to the revision captured before
their archive deletion check in the same transaction that removes cached facts.
That preserves journal replay if another handle has already restored or
reimported the session.

Every install records the archive revision and catalog digest used to build it.
Installing a build older than the timezone's claim lowers the claim in the same
transaction, whether the build replaces a newer install or one that invalidation
removed. The next read replays the journal from that revision and repairs it. A
timezone claim records both an archive revision and an install counter. A
compare-and-set advances the claim from actual installed stamps; concurrent
claim losers leave it alone. A completed pass uses the revision and install
counter captured at its start. It records its catalog only when no intervening
install used another catalog. An unchanged revision, install counter, and
catalog need no claim write, so warm reads can proceed while another cache
writer holds its transaction.

A timezone without a completed pass, a new cache generation, a changed pricing
catalog, or coverage outside the archive's revision range takes the full path.
An untrusted request starts a coverage pass when background backfill is enabled
and idle, and the pass retains its timezone. Other zones use the full path;
serving a request updates their recency for the next pass. Catalog checks
include handle-specific custom pricing. The captured catalog digest determines
whether a pass can vouch for the current rates.

Before reading rows, the pinned cache transaction checks timezone coverage,
pricing, intervening installs for captured sessions, and missing installs. Empty
windows run the same checks. After a conflicting install or invalidated dedup
peer, daily totals retry once with a fresh journal capture; further conflicts
and other reads use the full path. Activity matching counts always take the full
path. Covered daily totals read ordinary usage from day totals and load only
window exception owners and matching authoritative sessions. Other warm window
reads load filter and attribution metadata, including the display fields shared
with Top sessions.

Within one Pairwise request, concurrent and sequential reads share one capture,
fill check, and rollup check when their window, session predicate, and Cursor
need match. Project sides prepare separately because their session predicates
differ. Each side reads rollups from the request's captured snapshot with its own
filter. Cache retirement or a changed install triggers a fresh capture that
bypasses the shared result. Separate requests never share a preparation.

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
running so retry storms converge. A large window fills in units of 1,024
sessions, at most three at once, so one unit installs while others extract.

One rollup request per timezone builds at a time, and identical concurrent
requests share their work. Inside a request, two batches of 256 sessions build
at once, each on its own cache connection, and they install one at a time in
batch order. When every caller has left, the batch that is installing finishes
for the next request to reuse, and batches that have not started to install are
dropped. Cursor history stays in one install with the first batch. `cached_at`
is diagnostic only.

## Files and generations

The filename contains the cache schema version and archive `database_id`. Schema
or database-ID changes select a fresh generation rather than migrating the
archive. Before deleting or replacing any generation, both SQLite
`application_id` and `usage_cache_metadata.cache_kind` must identify it as an
agentsview usage cache; a filename match is insufficient. Version 8 and newer
generations also carry a retirement-protocol marker and hold a shared
cross-process lease for the lifetime of every open SQLite pool. An opener may
remove another recognized generation only after it takes the exclusive lease,
rechecks the format and source database ID against the exact filename, and
closes its own handles. Cache database, WAL, and shared-memory files are then
removed together. The tiny lease file remains so a racing opener cannot lock a
replacement inode. Pre-protocol generations and files with mismatched identity
remain untouched because an older process may still hold them open. Generations
newer than the running format also remain, so a downgraded binary does not force
the newer one to rebuild.

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
