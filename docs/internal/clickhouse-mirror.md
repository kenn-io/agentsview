# ClickHouse push and serve

SQLite is the archive. `agentsview clickhouse push` copies sessions from that
archive into ClickHouse. `agentsview clickhouse serve` runs the HTTP API and web
UI by querying ClickHouse. The dashboard does not write ClickHouse: rename,
trash, insights, stars, and pins stay on SQLite.

```text
agent files -> SQLite archive -> clickhouse push -> ClickHouse
                                      ^
clickhouse serve <- HTTP API / UI ----+
```

Operator steps, config keys, and commands live in
[ClickHouse Sync](../clickhouse-sync.md). Agent rules live in
[storage](../agents/storage.md#clickhouse-mirror).

## Problem

PostgreSQL already had configure, push, watch or OS service, then serve. DuckDB
is a local file you rebuild and swap, not a remote database. ClickHouse needed
the PostgreSQL operator path with SQL that MergeTree can run.

## Behavior

Configure `[clickhouse]` or `[clickhouse.NAME]`, push once or with `--watch` /
`clickhouse service`, then `clickhouse serve`. Several machines may push into
one database; each archive keeps its own cursor in the mirror's `sync_metadata`.

Serve implements `db.Store` reads: session list and detail, messages, search,
analytics, usage, activity, recent edits, project inventory, identity, stars,
and pins. Writes on that store return `db.ErrReadOnly`. Insights reads are
empty. `HasFTS()` is true (ILIKE). `HasSemantic()` is true once
`clickhouse serve` finds a pushed embedding generation matching the local
`[vector.embeddings]` fingerprint (see the vector decision below).

Content search matches dialogue in `messages` and reasoning in `thinking` for
substring and regex modes. Hybrid keyword hits use dialogue for matching and
snippets; vector-only hits use rendered messages. A per-archive dialogue marker
forces a full push after upgrading. That push re-sends every session in the
configured mirror scope, including sessions without dialogue. Session
fingerprints include nullable dialogue values so later dialogue-only changes
trigger another write. The marker is set after a clean full push.

## Boundaries

- SQLite is never deleted, dropped, truncated, or recreated for a ClickHouse
  schema or data-version change.
- ClickHouse is not the system of record. A destroyed mirror is rebuilt by
  pushing again.
- Hosted raw sync stays off this path.
- Cluster / `ON CLUSTER` DDL is out of scope. ClickHouse Cloud `SharedMergeTree`
  conversion needs no change here.
- Session IDs that appear in two archives are last-writer-wins. There is no
  PostgreSQL-style owner marker.

## Decisions

**Write model from DuckDB, operator model from PostgreSQL.** Whole-session
replace gated by a fingerprint on the session row, cursor in the mirror. Not
DuckDB's rebuild-and-swap file, and not PostgreSQL's local watermark plus remote
reset blob. A missing row reads back as an empty fingerprint, so a reset mirror
repairs on the next push.

**Driver `github.com/ClickHouse/clickhouse-go/v2` through `database/sql`.**
Stores stay in the same `Query`/`Scan` shape as DuckDB and PostgreSQL. Every
connection sets `final=1` so `ReplacingMergeTree` collapses duplicates without
`FINAL` in SQL. Activity usage rows are the exception: serve reads them on a
second pool that uses the driver's native API with the same settings. A
full-history report reads about 1.5 million rows, and `database/sql` boxes each
of their 32 million values and round-trips integers through text; on one
production mirror that left ClickHouse waiting 3.8 of 4.2 seconds for serve to
take the rows. The native read packs each row into the kept form as it arrives
and sorts small keys, so no slice of wide rows is held or sorted. One goroutine
still cannot take rows as fast as ClickHouse produces them, so a range read of
more than 300,000 stored rows is cut at timestamp quantiles into up to eight
slices that are read, sorted, and packed at once and then joined as bytes. On
1.5 million rows a cold range read takes about 1.1 seconds, against 3.5 seconds
for one read and 6 to 10 seconds through `database/sql`.

**Vectors are pushed and searched, keyed by fingerprint and archive.**
`clickhouse push` runs the vector phase after the session phase when the local
archive has an active generation and `push_vectors` is not false. A generation
is keyed by its config fingerprint (ClickHouse has no serial ids), all
generations share one `vector_chunks` table with `Array(Float32)` embeddings,
and per-session state is keyed by source archive so several machines push into
one mirror without owner markers: a session's vectors are written only while its
session row from this archive is resident, and evicted when that row goes.
Search is an exact `cosineDistance` scan ordered nearest first; the query shape
matches what the experimental `vector_similarity` index accelerates, which would
need per-dimension tables and a raised `max_limit_for_vector_search_queries` if
scale ever demands it.

The vector phase writes changed sessions in batches of up to 500 sessions or
about 32 MiB of document text and embeddings, whichever fills first. Each batch
inserts its documents and chunks, deletes the rows its sessions no longer hold,
then inserts their `vector_push_state` rows, all under one version. The delete
covers documents and chunks whose key the batch did not rewrite. A rewritten key
keeps its older row until `ReplacingMergeTree` collapses it, which every
`final=1` read already does. Deleting those rows too would write a patch row for
each one, and every later read of the table pays to apply patch rows until
merges fold them in. The batch sends its rewritten keys as query parameters.
Over HTTP each parameter is a URL field, which the server limits to 128 KiB after
URL encoding, so a batch whose keys pass 96 KiB splits them into sorted key
ranges and runs one pair of deletes per range. One long session can need several
ranges. Evictions run in batches of 500 sessions as well, so the
statement count grows with the number of batches, not sessions. A crash before
the state rows land leaves every session in the batch stale, so the next push
repeats them.

The vector tables are not sorted by session, but every `doc_key` starts with one
of four prefixes that name its session (`vector.DocKeySessionPrefixes`). In a
batch of up to 100 sessions, the flush and eviction statements add those
prefixes to their `session_id` filter, so the primary key skips other sessions'
rows. Each row a statement reads pays to evaluate the prefix terms, and in a
larger batch the sessions are scattered across most of the table. A larger
batch therefore reads the whole table without them. This depends on the push
writing only keys that `vector.DocKey` builds.

**Tables are `ReplacingMergeTree(push_version)`.** `push_version` is the push
start time in Unix nanoseconds. Same-key rows from a retry collapse on merge and
on read. Timestamps are `DateTime64(6, 'UTC')`. `sessions.last_message_at` is
computed at push time so date filters do not need a correlated `MAX`.

**Push order is the consistency mechanism.** ClickHouse has no multi-statement
transactions. Per batch of changed sessions (100 by default, set by the target's
`push_batch_size`), with `v` this push's version:

1. Insert dependent rows (messages, tool calls, result events, usage, findings,
   pins).
1. `DELETE ... WHERE session_id IN (...) AND push_version < v` per dependent
   table and for `usage_messages`, limited to rows whose sort key has no row
   at `v` or later. Its materialized view never sees deletes, so a shorter
   republished session would otherwise leave obsolete usage rows in the
   mirror. Tool result events and tool calls go before messages, so a failure
   between deletes never leaves calls whose copied message fields name a
   deleted message.
1. Insert session rows with fingerprint and `source_archive_id`.

The delete removes only keys the session no longer has, such as messages cut
from a shorter session or a tool call dropped from a kept message. A key the
push rewrote keeps its older row until `ReplacingMergeTree` collapses it, which
every `final=1` read already does, as the vector push does for rewritten
documents. On a server that runs deletes as patch parts
(`lightweight_delete_mode = lightweight_update`), deleting those rows too
wrote one patch row per row of every re-pushed session, and every later read of
the table paid to apply them until a merge or `APPLY PATCHES` folded them in.
Re-pushed sessions usually keep all their keys, so most of these deletes now
match no rows and write nothing.

A crash before step 3 leaves the fingerprint stale, so the next push re-selects
the session. A crash between 1 and 2 leaves extra old rows that step 2 removes
on retry.

**Full pushes resume.** A full push writes a run record under
`agentsview_full_push_run:<archive>` in `sync_metadata` before its first batch
and clears it with the cursor. The record holds the run's starting push version,
the archive database ID, the scope, and the schema and data versions. A later
push that finds the record with matching values resumes the run. It treats a
session as done when this archive's mirror session row has the local fingerprint
and a push version at or after the run's. The vector phase skips a session whose
`vector_push_state` row has a matching hash and a version at or after the run's.
Any mismatch starts a new full push, and so does `--full`. A run that ends with
session errors stays recorded with an `errored` flag. A matching record resumes
only if another trigger already forces a full push, and the push clears it
otherwise. A record from a different push scope, archive, or format keeps the
next push full, and the run that replaces it is marked `must_finish`. The push
never clears a `must_finish` run until it ends without errors: a filtered run
deletes mirror sessions outside its scope, and only a full push writes back
sessions older than the stored cursor. A resumed run checks vectors for every
session even when the caller scopes vectors to changed sessions, because the
earlier process may have stopped before its vector phase. A resumed run skips
the whole-mirror usage repricing that the run's first process already did.
Pushes send sessions newest activity first (`ended_at`, then `started_at`, then
`created_at`). A Codex revert page goes right after its thread's session, which
mirrors read to decide whether the page is trashed. PostgreSQL pushes and DuckDB
rebuilds use the same order.

**Shared filter builder, ClickHouse dialect.** Existing SQLite, PostgreSQL, and
DuckDB SQL stay byte-identical. ClickHouse uses `UNION ALL` in recursive CTEs,
`IN (SELECT)` instead of correlated `EXISTS`,
`parent_session_id IS NULL OR NOT IN (SELECT id FROM sessions)` for orphans
(`NULL NOT IN (...)` is unknown and would hide NULL-parent rows), `ILIKE` with
the default backslash escape and no `ESCAPE` clause (ESCAPE landed in 26.6; the
mirror was written against 25.8), and `toStartOfDay` / `toStartOfWeek` /
`toStartOfMonth` instead of `date_trunc`.

**Bootstrap through `default`.** `OpenForAdmin` pings the server `default`
database, then `CREATE DATABASE IF NOT EXISTS` the mirror name. Pinging the
DSN-path database fails when that database does not exist yet. The mirror name
is `Target.Database`, else the DSN path, else `agentsview`.

**Named targets share the PostgreSQL parser.** `[clickhouse]`,
`[clickhouse.NAME]`, `default_clickhouse`, reserved names, and env
`AGENTSVIEW_CLICKHOUSE_URL|_DATABASE|_MACHINE` on the default target only.
Non-loopback URLs need verified TLS unless `allow_insecure`.

**CLI matches `pg` minus vectors.** `push`, `status`, `serve`, `service`. When a
daemon owns the archive, push posts to `/api/v1/push/clickhouse`. The OS service
kind changes the unit label, command, and log file so it can sit beside the
PostgreSQL watcher.

## Rejected alternatives

- Pasting PostgreSQL or DuckDB SQL. Correlated `EXISTS`, `date_trunc`, and
  `UNION` without ALL fail or return wrong rows on 25.8.
- Plain `MergeTree` with delete-then-insert. A crash between the two leaves a
  session with no content and a matching fingerprint, so the next push skips
  it.
- `CollapsingMergeTree`. Needs the old rows in order to write cancel rows.
- A generic remote-mirror package. DSN, TLS, `Sync`, and SQL are not
  PostgreSQL's. Shared pieces are `db.Store`, dialect hooks, named-target
  parsing, and `serviceKind`.

## Stored usage prices

Push stores exact Go-computed request prices and their pricing contexts in
ClickHouse. Daily usage queries deduplicate events within the requested window,
join those prices, and return grouped totals. Request rounding, historical
rates, token tiers, and provider adjustments still use the Go pricing rules.

Price records are keyed by their normalized inputs and a pricing digest. The
digest covers the price format, the provider billing policies, and, for each
model registered in `usage_price_models`, everything the shared catalog resolves
that model to at any time (`PricingResolver.DependencyFingerprint`, which the
SQLite usage cache also uses). Catalog rows no registered model resolves to do
not count. The mirrored catalog merges what every exporting machine last
fetched, so unrelated rows change on most pushes. A digest over the whole
catalog repriced the whole mirror each time, about nine times a day on a mirror
fed by three machines, and kept every generation.

A pricing pass reads message usage from `usage_messages`, the narrow copy of the
usage columns that the prepared and raw-host readers also read, and gets the
same price keys a read of the token JSON in `messages` would. `usage_messages`
indexes every 64 rows. For a batch of 100 sessions, a pass over `messages` read
whole index granules of the wide table: about 1.5 million rows on a production
mirror, for under 3,000 usage rows.

A push prices the whole mirror when the digest changed, on a full push, and on a
mirror no pass has covered. Otherwise it prices new session batches under the
current digest, and passes over the Cursor rows only when one could find
something:

- `cursor_usage_events` has rows, and
- the mark in `sync_metadata` key `usage_prices_cursor` differs from the
  current one. The mark covers the digest, the active parts of
  `cursor_usage_events` with their file hashes, the lowest block number of the
  digest's `usage_event_prices` parts, and the query that derives Cursor price
  keys. A push reads the parts before pricing and records the mark after the
  pass, or the whole pass, finishes.

New Cursor rows, merges, and mutations change the parts. Dropping the digest's
partition raises its lowest block number. A build that changes how price keys
are derived changes the query. Each forces the next pass. An unchanged mark
means the rows a finished pass priced are still the only rows, and their
records are still there. Reading the parts takes about 2 ms, against about
315 ms and 1.68 million rows for a pass on a production mirror with no Cursor
rows. The first pass under a new digest records a mark from before the
digest's first records, so the next push passes once more.

A usage row whose model the digest does not cover is not priced under it,
because a catalog that resolves that model differently would map to the same
digest:

1. The session batch registers the model as pending and leaves its rows to
   per-request pricing.
1. When its batches are done, the push prices the whole mirror under the digest
   that adds every pending model, then registers the models.
1. Readers derive the digest from the registered models, so they move to the new
   digest only after its records are complete.

A push that stops between the first two steps leaves the models pending, and the
next push adopts them before its batches.

**Pruning.** Each digest is its own partition of `usage_event_prices` and
`usage_price_contexts`. After pricing, every push drops the partitions of
digests that are not the one it priced under, not the mirror's current digest,
not the digest `prepared_usage` was built from, and have had nothing written
under them for 24 hours. Exporters whose catalogs disagree on a priced model, or
that run an older build, alternate between digests; the window keeps those
switches from repricing the whole mirror each time. Listing digests reads two
columns of the price tables (about 0.2 s at 20 million rows), and dropping a
partition rewrites nothing. Lightweight deletes would write patch parts as large
as the deleted rows on servers in `lightweight_update` mode.

A push that revives a digest idle for 24 hours while another push drops it can
lose part of that digest's records, or keep records whose contexts were dropped.
Reads stay correct. They price rows without a record per request. The next push
prices the Cursor rows again; session rows wait for the next whole pass. A read
that still finds a record without its context after three attempts prices every
row per request.

Serve stays read-only. Missing prices and reader-specific custom rates are
computed for that request using the current pricing rules while push fills the
shared records. A response never mixes prices from different pricing digests.
Copilot authoritative costs retain their per-session selection and allocation.

The price tables are derived data; SQLite remains the archive. The schema step
drops price tables that an older version created without partitions, creates
them partitioned, and clears the mirror's current digest, so the next push
prices the mirror again.

## Activity report reads

The Activity report used to rebuild its candidate-session predicate inside every
usage and interval query, parse usage JSON per row, and pair tool events with
their next message through an inequality join that grew with the square of each
session's size. Four changes keep the same output with less work:

**Candidate IDs travel as an external table.** Session discovery runs once. The
selected IDs are attached to the request context as a native external table
named `activity_candidate_ids`, and every later query reads
`SELECT id FROM activity_candidate_ids`. No candidate SQL is repeated and no
`IN (...)` list is inlined into statement text.

**Tool events pair per session in Go.** Serve loads the candidate sessions'
messages and terminal tool events in ordered scans, then pairs each tool event
with the next stamped message or the next tool event per session. A segment tree
over message timestamps finds the lowest eligible ordinal even when timestamps
run backwards, so ordinal and timestamp behavior matches the old SQL, including
reversed clocks and the tail pairing after the last message.

One goroutine scanning millions of message rows kept ClickHouse waiting to send
them for seconds. So the sessions are dealt into one slice per 4,096 sessions,
up to two, and each slice's messages are read at once on the native connection
pool, beside the tool event read. The usage read's eight slices usually finish
last, and more pairing slices slowed them on the server without making pairing
ready sooner. Each session's messages arrive together, so a slice appends them
without a map lookup per row. Pairing a session reads only its own inputs, so
sessions are paired on every available core.

**Each session keeps its runs.** After pairing a session over all time, serve
caps each interval at the gap, trims it against the session's earlier activity,
and joins touching pieces with the same model into runs. It keeps those runs
and the times of the session's counted user and assistant messages in memory,
keyed by session, push version, and gap cap, within a 256 MiB budget. A refresh
reads and pairs only the sessions whose push version changed, clips every kept
session's runs to the range, and sweeps the runs in start order through
`activity.BuildRunArtifactsWithSurvivorUsage`. A long range has about twenty
times fewer runs than interval candidates. A run's pieces do not depend on the
range: clipping after the trim gives the pieces that clipping each candidate
first gives, and the candidates a range leaves out of pairing are ones the
aggregator rejects for that range anyway. Runs keep their piece boundaries, so a
range end inside a run still finds the start of the last piece left, which
bucket membership needs. The shared aggregator sums interval durations as
integers and converts to minutes once, so minutes do not depend on how activity
was split into intervals, on every backend. Storing runs in ClickHouse at push
time was rejected: it needs a schema change, upgraded pushers on every machine,
and a re-push of existing sessions. A server that has just started still reads
every session once.

**Usage is stored, not parsed.** The insert-maintained `usage_messages` table
stores token counters that its view computes from `token_usage` at insert time,
together with the usage-relevant message columns and a time index. The usage
query reads it instead of `messages`, and `messages` keeps only the raw JSON.
Its replacement key is the source message key, not the timestamp, so a retry
that corrects a timestamp or removes usage still replaces the earlier row.

**Terminal events are snapshotted per session version.** The insert-maintained
`terminal_event_snapshots` table stores the latest terminal tool event timestamp
per `(session_id, push_version)`. Candidate discovery filters on it before
version resolution instead of scanning `tool_result_events`.

Both derived tables use `ReplacingMergeTree(revision)` with
`revision = push_version * 2` for backfilled rows and `push_version * 2 + 1` for
rows written by the materialized view. A live insert therefore beats a
concurrent backfill of the same version. Readers compare each row with
`sessions.push_version`. A push writes messages before it publishes the session
row, and an interrupted push never publishes it, so usage rows count when their
version is at or above the session's; an exact match would report no usage for
that session until its next successful push. Rows that a shorter republished
session left behind stay below the session's version and never surface. Terminal
snapshots are written by the session insert itself and match exactly.

The snapshot view runs inside every `sessions` insert, and ClickHouse reads a
view's joined table in full. The view therefore limits `tool_result_events` to
the sessions in the inserted block, which keeps the cost tied to the push batch
and not to the size of the mirror. Materialized views do not see deletes, so
removing a session from the mirror also clears its rows in both tables. The
startup fills record completion in `sync_metadata` only after the work finishes,
so an interrupted startup repeats the fill without touching source tables. A
read-only role cannot run the fill and fails the compatibility check until a
capable push completes it.

**Freshness is checked from part metadata.** Every report request first asks
whether the mirror changed. The store hashes the active `system.parts` rows of
`sessions`, `messages`, `usage_events`, both pricing tables, `sync_metadata`,
`usage_session_snapshots`, and `prepared_usage`, which is metadata and reads
no data, and reuses the last probe result while the hash is unchanged. A merge
changes the parts without changing the data, so the hash is never the report
token: on a hash miss the store recomputes the original probe and caches it
under the new hash, and only a real data change moves the token or resets
pagination. The mutex protects only the cached pair, never a query, and an
error is not cached.

A hash miss reads what changed, not whole tables. The highest message and usage
event ids come from the 16 highest ids of each part, which the store keeps per
part name: a lightweight delete can hide rows of a part but every other write
makes a new part, so a FINAL read of the sessions above the highest kept
threshold finds the maximum unless all those rows were replaced, and only then
does the probe read every id. Usage coverage comes from the row trackers
described in [Kept reads](#kept-reads). Reading `system.parts` needs its own
grant, described in [ClickHouse
sync](../clickhouse-sync.md#3-serve-the-dashboard).

## Prepared usage

Usage, analytics, and Activity reports read date-ordered usage rows. Computing
those rows from `messages` and `usage_events` on every request dominated report
time on small hosts, so the mirror prepares them ahead of time.

**Complete snapshots.** Each push reads a session and its usage from the archive
in one transaction and writes them to `usage_session_snapshots` as one versioned
snapshot. A retry of a failed batch reuses the payload it already loaded, so one
push version never mixes two archive states.

**Views keep the query in the code.** `CREATE MATERIALIZED VIEW IF NOT EXISTS`
leaves an existing view alone, so a mirror created by an older release would
keep that release's query. Each view therefore carries a comment with a hash of
its query. Every schema step compares the live comment with the code's and, when
they differ, runs `ALTER TABLE ... MODIFY QUERY` and then updates the comment.
`MODIFY QUERY` replaces the query in place without stopping inserts, so no
insert runs without the view and no derived rows go missing. It keeps the view's
source table, columns, and clauses (`TO`, `REFRESH`); a change to those needs a
new view. `usage_messages_mv`, `terminal_event_snapshots_mv`, and
`prepare_usage` use this check. For `prepare_usage` it also catches a change to
the refresh settings, which the `prepared_usage` table comment does not cover.

**Refreshed table.** The refreshable materialized view `prepare_usage` turns the
snapshots into `prepared_usage` when a push finds a refresh due, and once a day
as a backstop; see "When a push rebuilds" and "The view's schedule" below. Rows
are sorted by their stored UTC time, so a day read touches only that day's
granules. Each row stores its price model, price key, and a copy of its price
record, stamped with the pricing digest it was priced under. The table comment
names the query that built it; a table built by another query is dropped and
rebuilt on the next schema step, and a rebuild that stopped halfway finishes on
the next start. The schema step adds a missing projection only with the refresh
stopped, and always starts it again.

**Refresh without a table exchange.** Every refresh of the view ends with
`EXCHANGE TABLES`, which ClickHouse runs as a `renameat2` call with the
`RENAME_EXCHANGE` flag on the database's metadata files and has no fallback for.
Filesystems without that flag, such as NFS, fail every refresh with
`UNSUPPORTED_METHOD`, and the table is never filled. The schema step therefore
exchanges two empty scratch tables in the mirror database. When that fails, it
does not create the view, drops one an earlier version created, and creates the
first rebuild lock if none exists. Probe tables that a killed push left behind
are dropped once they are an hour old.

A failed probe logs an `UNSUPPORTED_METHOD` error on the server, and a push
watch runs the schema step before every push. The answer depends on the
filesystem under the server's metadata directory, which changes only across a
restart. The schema step therefore records the answer, the server's
`serverUUID()`, and the server time of the probe under
`agentsview_table_exchange_probe` in `sync_metadata`. It probes again when the
server started after that time, or when the record names another server. The
start comes from `now() - uptime()`, which counts whole seconds, so a probe up
to a second before the computed start still counts. `RESTORE` runs on a live
server and can copy in another server's record that is newer than this server's
start; the server check catches that case. The probe tables a killed push left
behind are dropped at every schema step, because a stored answer skips the probe
until the next restart.

The lock table is what tells pushes to rebuild. A push without the view rebuilds
only while a lock exists and the newest one is not retired. A database that can
swap tables has no lock. A push there that finds the view missing, as while a
schema step recreates it, never rebuilds.

Each push in rebuild mode rebuilds the table itself before it returns, once a
rebuild is due. It runs the view's query into a new `prepared_usage_build_<n>`
table and moves that table's parts into `prepared_usage` with `REPLACE
PARTITION`, which replaces all the parts at once. Readers see the previous rows
or the new ones, never a mix, and read them as they read rows the view prepared.
A push checks that a rebuild is due both before and after it takes the lock.
After publishing, the holder rebuilds once more if the snapshots that changed
meanwhile make one due. If they make one due again, it logs that and leaves the
rest to the next push that writes usage; see "What the check reads" below.
Without the view, nothing refreshes on a schedule, and a failed rebuild waits
for that push. Because `prepared_usage` is then not a
refreshable view's target, native backups include its rows without
`backup_data_from_refreshable_materialized_view_targets`.

Rebuilds take turns, as the view's refreshes do. A read checks freshness and
fetches rows in separate queries, so an older build must never replace a newer
one between them. The lock is a numbered `prepared_usage_rebuild_lock_<n>`
table. A push takes the lock by creating the table with the next number, which
only one push can do. The push drops the older lock tables; the newest stays
after release, so one lock table persists and numbers never repeat. A push that
finds the lock held skips its rebuild, and reads prepare the changed sessions
themselves meanwhile.

A lock's comment holds its state as JSON, which ClickHouse changes atomically.
`status` is `held`, `released`, or `retired`. A held lock also carries
`renewed_at`, the ClickHouse server's Unix time when its holder last renewed it.
A comment that is not valid JSON or has another status fails every read of the
newest lock. Each push logs the error and skips its rebuild, and the schema step
fails. The error names the lock and ends with the statement that repairs it.

A lock can outlive its holder:

- The holder renews the lock's timestamp every ten minutes. A lock not renewed
  for an hour belongs to a stopped push, and the next push takes the next
  number.
- A push that paused between reading the locks and creating its own can create a
  number a newer push already retired. Right after creating its lock, a push
  checks for a higher number and gives its own up if one exists.
- The build table carries the lock's number, and only a lock holder creates one.
  A new holder drops every build table with a lower number. A publish still
  reading one finishes before the drop returns, and a later one fails without
  its table, so older rows never land after newer ones. A push also checks for
  a newer lock before publishing and discards its build if one exists.
- A push drops its own build table when it finishes, so a leftover exists only
  between a killed push and the next rebuild.
- When a later schema step finds that the database can swap tables, as when the
  data directory moved off NFS, it first creates one more lock number with
  status `retired` and drops the older locks and every build table. A rebuild
  that already took a lock finds the newer lock and discards its build;
  dropping its build table waits for a publish already running and fails a
  later one. A push that tries to take the lock afterwards finds it retired
  and skips. Only then does the schema step create the view. Because a push
  rebuilds only while a lock exists, a database with no lock has no rebuild to
  fence.
- If the schema step stops after retiring the lock but before creating the view,
  a push finds the newest lock retired and no view. The push first checks that
  the database can still swap tables. If it can, the push retires again,
  creates the view, and starts its refresh. If it cannot, the push releases
  the retired lock and rebuilds.
- Retirement can pause after reading the locks while pushes take newer numbers,
  so it checks for a higher number right after creating its retired lock, as a
  push does after taking a lock. If one exists, it retires again above it.
- If the database later loses the ability to swap tables, the schema step marks
  the retired lock released and then drops the view. From then on, each push
  rebuilds. A schema step that also replaces `prepared_usage` drops the view
  before it releases the lock. A push in that gap finds the newest lock
  retired and no view, and its swap check sends it to rebuild instead of
  creating the view.

**Currency is proven per read.** A read uses prepared rows only when every
session has a current snapshot. Sessions pushed since the last refresh form a
delta: their rows are prepared and priced in Go once per snapshot, sent to the
read as an external table, and replace that session's stored rows. A refresh
empties the delta. A snapshot from which the refresh keeps no row, such as a
trashed session's or one whose messages name a model but report no tokens, has
nothing to prepare and never joins the delta. Each snapshot stores that answer
in its `prepares_usage` column, so the check reads no usage arrays. Before the
first refresh, or while a session lacks a current snapshot, reads use raw rows.
When the stored price copies no longer match the price records, for example
after the records were cleared, reads keep the prepared rows and price them
through the per-request join.

**When a push rebuilds.** Because reads stay exact while the table is behind, a
rebuild only changes what reads cost, and it rewrites the whole table. On a
mirror whose data directory is on NFS, rebuilding on every push wrote the whole
table hundreds of times a day. A push therefore rebuilds, or starts the view's
refresh, only when one of these holds:

- The table is empty.
- The pricing digest changed, or the price record count fell below the stored
  one, so the stored price copies no longer serve reads.
- The rows reads prepare themselves reach one `chPreparedUsageBehindShare`
  (one-fiftieth) of the stored rows.

The push counts those rows over the snapshots that no stored row was derived
from, with the delta query's message and event eligibility, so a snapshot that
prepares no rows counts nothing. The count reads only the usage array fields
that eligibility checks, and a test holds it equal to the rows the delta query
returns. A rebuild then rewrites at most fifty times the rows that
changed, and reads prepare at most a fiftieth of what they read. On a
300,000-row test table, a 5,000-row delta added about 0.13 seconds to the first
read after a push. Deleted sessions' rows stay until the next
rebuild, and reads skip them.

**What the check reads.** A push compares the stamp stored on the rows with the
stamp a refresh would store now, and counts rows only when the two differ. It
reads the snapshot count and hash without FINAL, from the highest revision of
each session, which gives the values the refresh's FINAL read gives. It reads
the price record count from part metadata, which counts a key twice while two
pushes that priced it at once both hold it unmerged. That count can only be too
high, so a count below the stored one still proves the records were cleared; a
clearing refilled with enough duplicates to reach the stored count goes unseen.
On a copy of the production layout, the check read 323,000 rows (17 MiB) in 22
ms, against 2.56 million rows (234 MiB) in 188 ms with FINAL: 8.5x faster.

A push that wrote no snapshots, deletions, price records, or pricing digest
checks only that the table is not empty. A push that changed those inputs
checks after its own writes. When that push ends with session errors, which
skip the check, or a rebuild leaves the next push to rebuild, the rebuild
waits for the next push that writes, or for the view's daily refresh. Reads
stay exact meanwhile, only slower.

**The view's schedule.** The view refreshes on its own once a day
(`chPreparedUsageRefresh`, `EVERY 1 DAY`), at midnight in the ClickHouse
server's time zone. Every refresh rewrites the whole table, and a push already
starts a refresh whenever one is due, so the schedule only covers a due refresh
that no push started: a push that ended with session errors, or a refresh
request that failed. The next push that writes checks again and catches both,
so the schedule matters only when no push writes for a day. The table comment
hashes the schedule, so changing it drops and rebuilds `prepared_usage` once on
the next schema step.

Until November 2026 the schedule was `EVERY 15 MINUTE`. On a production mirror
with 1.56 million prepared rows, each refresh read 4.5 million rows (1.8 GiB),
wrote every row, and took about 6 seconds. The schedule ran it 96 times a day
whether or not anything changed, and the build table took about 16 GiB of new
parts a day.

A simulated day on a scratch server with 1.5 million prepared rows ran 27
pushes every 15 minutes, each republishing 3 sessions, and one full push of
22,000 sessions in 10 pushes. The view's clock was faked with
`SYSTEM TEST VIEW ... SET FAKE TIME`:

| Schedule | Refreshes | Started by pushes | Rows written | Bytes written |
| --- | --- | --- | --- | --- |
| `EVERY 15 MINUTE` | 106 | 10 | 159 million | 53.2 GiB |
| `EVERY 1 DAY` | 14 | 13 | 21 million | 7.0 GiB |

The daily schedule wrote 7.6x less. Without the 15-minute reset, the steady
pushes reached the one-fiftieth limit three times, so pushes started three
more refreshes.

Bursts of push-started refreshes follow a full push from another host. After
a deploy, a second host's full push republishes every session with a new
revision, about 4,000 sessions a minute. Each push from a host that is already
done finds the table one-fiftieth behind again and starts a refresh, about
every 33 seconds until the full push ends. Each refresh in those bursts was
due by the rule above, and none was requested while another was running.

## Dashboard facts

The Analytics panels used to rebuild per-session numbers from `messages` and
`tool_calls` on every load. On a mirror of about 107,000 sessions, 2.5 million
messages, and 1.7 million tool calls, that meant FINAL scans of both tables for
Activity, a transfer of every message's role, time, and length for Velocity, a
join of every tool call to its message for Tools and Skills, and, after each
serve restart, a 22 s scan of every user prompt's text for Signals.

Push already holds each session's messages and tool calls, so it computes these
numbers once and stores them on the session row (`dashboard_facts.go`):

| Column | Counts | Replaces the scan in |
| --- | --- | --- |
| `fact_user_messages` | user rows that are not system rows or tool results | Activity, Session Shape autonomy |
| `fact_assistant_messages`, `fact_thinking_messages` | assistant rows, rows with thinking | Activity |
| `fact_tool_use_messages` | assistant rows with tool use | Session Shape autonomy |
| `fact_tool_calls` | tool calls | Activity, Velocity |
| `fact_frustration_markers` | prompts `signals.IsFrustrationMarker` flags | Signals |
| `fact_velocity_*` | the session's `db.SessionVelocity` over messages without Claude hooks | Velocity |

`stored_message_count` already gives the message total. Each tool call row also
carries its message's `message_timestamp` and `message_model`, so Tools and
Skills group calls without joining `messages`.

**Reads fall back per row.** A sessions row whose `dashboard_facts_version` is
not `dashboardFactsVersion`, or a tool call row whose `message_facts` is false,
came from an older writer. Reads split each panel into a part that reads facts
and a part that scans messages for exactly those sessions or calls, then
combine them. The scan part prunes by session ID, so it reads nothing once
every row carries facts. Results never depend on the backfill finishing.

**Facts match the scan they replace.** Each fact uses the scan's predicate and
the same Go function the scan feeds (`IsFrustrationMarker`,
`db.ComputeSessionVelocity`). Velocity facts use timestamps truncated to
microseconds, as `DateTime64(6)` stores them, so turn cycles match a scan of the
stored rows exactly. `TestDashboardFactsMatchMessageScans` compares every
affected panel with facts, with some sessions scanned, and with all scanned.
While a push is between its inserts and its session row, the facts describe the
published session version and a scan would read the newer messages; both
settle when the session row lands.

**Backfill is one full push.** A push that finds rows of its archive below the
current facts version, or without `stored_message_count`, runs one full push
(`publishing stored session counts`) and repeats it until one finishes without
errors. Bump `dashboardFactsVersion` when a fact's definition changes.

**FINAL stays on.** The panels that used facts now read `sessions` with FINAL
instead of `messages` and `tool_calls`, which removes most of FINAL's cost
from them.

**The Signals scan judges most prompts in ClickHouse.** For sessions without a
current `fact_frustration_markers`, Signals runs one query
(`chFrustrationPromptsSQL`) with the session IDs as an external table. A prompt
made only of ASCII and a fixed set of symbols (dashes, quotes, arrows, check
marks, box drawing, emoji, private use) is judged in SQL and comes back only
when it is a marker, without its text. Over such text ClickHouse's RE2 and Go's
`regexp` see the same letters, spaces, and word boundaries, so the SQL follows
`IsFrustrationMarker` step by step with the same pattern and thresholds, which
`internal/signals` exports. Any other prompt, including invalid UTF-8, comes
back with its text and Go judges it. The set is `signals.PlainPromptSymbols`,
which DuckDB shares (see `internal/duckdb/README.md`), and a unit test checks
every symbol in it against Go's Unicode tables. ClickHouse tests compare the counts with
`IsFrustrationMarker` on edge cases and on a random corpus, and require that
only prompts outside the set come back with text.

The query reads without FINAL, because FINAL would merge the text of every
message in the sessions, assistant replies included. A subquery with
`final = 1` finds the newest version of each prompt from key and filter
columns, and the outer read takes text for those versions only. On the
production mirror, for the 6,811 sessions of one Signals load, the old chunked
scan ran 8 queries that read 10.6 GiB, sent 1.13 GiB to Go, and peaked at
1.21 GiB of memory per query. The new query reads 3.3 GiB, sends 1.6 MiB, and
peaks near 290 MiB, with the same per-session counts.

**Rejected alternatives.**

- Partitioning the large tables and setting
  `do_not_merge_across_partitions_select_final`. ClickHouse cannot add a
  partition key in place, so every large table would be copied, and every
  version of a row must stay in one partition.
- Storing the frustration count in SQLite as quality signal version 8. That
  recomputes every session's signals and republishes every replica, for one
  count only ClickHouse needs to precompute.
- Porting all of `IsFrustrationMarker` to ClickHouse SQL. Unicode letters,
  spaces, and case folding differ between Go and ClickHouse, so only prompts in
  the shared alphabet above are judged in SQL.
- Filtering prompts in SQL with a looser test that keeps every possible
  marker, such as matching the phrase words alone. Long pasted prompts contain
  those words, so on the production mirror such a filter dropped almost none of
  the 4.25 GiB of prompt text.

SQLite keeps its scan in Go, which skips system rows in SQL. DuckDB and
PostgreSQL judge plain prompts in SQL as well; see
[PostgreSQL frustration marker reads](../agents/storage.md#postgresql-frustration-marker-reads).

**Go judges each prompt in a few passes.** SQLite sends every prompt of a
session its memo misses to `IsFrustrationMarker`, and PostgreSQL, DuckDB, and
ClickHouse send the prompts they cannot judge in SQL. The memo lives in memory,
so the first Signals load after a restart judges every prompt in range. The
function strips code fences once. One pass then builds the normalized prompt and
counts the capitalized words, and another finds the first phrase literal. The
regular expression starts one byte before that literal, because no match can
begin earlier. Tests compare the function with its original step-by-step form on
edge cases, a random corpus, and a fuzz target.

On a scratch SQLite archive of 7,000 sessions with 140,000 prompts (594 MiB of
prompt text), the first Signals load after a restart fell from 9.2 s to 2.2 s
with the file in the page cache, and from 10.4 s to 3.3-3.6 s with it evicted.
Allocation fell from 12.4 GiB to 3.2 GiB, and the count stayed the same. SQLite
returns the prompt text in 0.42 s; the judge took 8.3 s before and 2.0 s after.
A load the memo answers takes 15 ms either way.

Rejected for SQLite:

- Storing a marker flag per message, or a count per session, when messages are
  written. The first load would then read stored numbers. But every path that
  writes or replaces messages would have to keep them current, and existing
  archives would need a backfill that judges every prompt once anyway. With the
  faster judge, the cost it removes is about 2 s once per restart.
- Judging prompts in SQL. SQLite has no `REGEXP`, and a function registered from
  Go would run the same Go check behind SQL.

## Kept reads

Serve keeps the results of recent reads in memory so a repeat request between
pushes reads no rows from ClickHouse; it still runs the small metadata queries
that check whether its inputs changed. Each kept read has one slot per selection
(filter, range, and similar inputs), and its key names the rows it read: the
prepared usage stamp and row identities for fully prepared range reads, the
candidate sessions and their push versions, or the parts of the tables it
scanned. Row identity probes run only for tables whose parts changed. The
sessions, snapshot, and price record tables, which hold most of the rows a check
would hash, are followed by row trackers (`finalRows`) that keep the count and
hash sum of the rows FINAL returns, and each key's push version, machine,
revision, and `prepares_usage` flag for usage coverage. A tracker compares the
active parts with the parts it last read. Block numbers grow with every insert
and mutation, and a merge keeps the block range of the parts it combines, so a
new part whose range the vanished parts cover exactly at the same mutation is a
merge, and costs nothing. Any other new part holds new rows: the tracker reads
the keys it holds and a FINAL read of those keys for the row each returns now.
The sessions and snapshot trackers keep each key's row, which is the row FINAL
returned before. The price tracker keeps no per-key rows, since one digest holds
millions of records; it reads the keys' rows in the parts it had already seen,
and keeps the rows of parts up to 8,192 rows, so the earlier row is still at
hand after a merge folds a small part into a new one. A mutation, a patch part,
a dropped part, a new part over 8,192 rows, or a price merge of a large part
with unread rows rereads the whole table. Every read names the parts it was
planned against, and the parts are read again afterwards; when they moved,
nothing is kept. The price record table keeps the records of every pricing
digest, so its tracker covers only the partition of the digest a read joins;
hashing the whole table took two minutes on a mirror holding 534 digests. A
failed probe or parts moving during it serves that read uncached. Raw, mixed,
and single-session Usage reads keep parts versions. A push that changes those
versions replaces the slot; a push that does not leaves it in place. Usage reads
filtered by `active`, `stale`, or `unclean` termination compare session times
with the current time, so their rows are never kept. Kept reads include usage
rows, analytics session listings, Activity runs per session version,
candidate listings, project label maps, and the whole report of an ended range.

**Usage of a range in progress.** An Activity report over a range that has not
ended keeps every session's prepared usage rows in the range, plus the usage
survivors derived from them. Survivors are kept per dedup token: a Claude
message and request pair, a source UUID, or a usage key. Each candidate session
keeps its survivors summed per model and bucket window. A push re-reads only the
sessions it changed. The report then re-derives only the tokens those sessions'
rows carry, and only the sums of the sessions that share those tokens. A session
entering or leaving the candidate set counts as changed, and so does a session
with a row that the moving effective end passes. A new pricing digest or bucket
size re-prices and re-sums every survivor, but keeps the token selection. A
range of 1.5 million rows keeps about 250 MiB. A refresh after a push of a few
sessions derives its usage in tens of milliseconds, against more than a second
for a selection over every kept row.

Ungrouped Top sessions are the exception. Each request reloads display metadata
for its ranked winners. Grouped Top sessions keep member metadata with their
totals.

Nothing rebuilds kept reads in the background. The first request after a push
that changes a read's rows pays for that read.

**Usage on disk.** Fully prepared Usage selections keep their ordered priced
rows and Top session totals across restarts. Memory and disk share a selection,
so a memory hit refreshes its file or restores it if it was removed. Serve
removes the least recently opened files first and checks the source and pricing
before reusing or publishing a selection. A range ending after the latest usage
can reuse the same rows on a later day. A changed source rebuilds the selection
on the next read; pushes do no cache rebuilding. Raw, mixed, and time-dependent
selections stay on their existing read path. The cache limits are in
[ClickHouse sync](../clickhouse-sync.md#agentsview-clickhouse-serve).

**Reports on disk.** When a client opens an ended day, its report is written to
the report cache described in
[ClickHouse sync](../clickhouse-sync.md#agentsview-clickhouse-serve), one file
per selection and one directory per mirror. Nothing prepares days ahead of time,
so a day no one opens costs no disk. A file names the binary that wrote it and
the report's key, so a new build or a change to that day's rows rebuilds it.
Every open, whether served from memory or from the file, refreshes the file's
modification time. At startup and about once a day serve removes reports no one
has opened for 30 days, and temporary files that interrupted writes left more
than an hour ago; there is no other size or count limit.

## Tradeoffs

Push copies stars and pins from SQLite, but the ClickHouse UI cannot change
them. PostgreSQL serve does allow those writes in the shared database. The UI
already hides the controls when settings report read-only.

Several machines can share one database without ownership conflict detection.
Two archives that reuse a session ID overwrite each other.

Until a merge, both old and new `push_version` rows can exist on disk. `final=1`
hides the stale ones from readers.

## Tests

Unit tests cover dialect rendering, config, TLS checks, and fingerprints.
Integration tests use the `chtest` tag against
`clickhouse/clickhouse-server:26.8` or `TEST_CLICKHOUSE_URL`. Tests of the
refresh without a table exchange start a second server under a seccomp profile
that fails every `renameat2` call with a flag, as NFS does, or use
`TEST_CLICKHOUSE_NO_EXCHANGE_URL`. `make test-clickhouse` is the suite. Do not
point it at a live mirror.
