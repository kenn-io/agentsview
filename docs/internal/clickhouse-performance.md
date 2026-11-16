# ClickHouse Read Performance

## Summary

On a clean mirror, ClickHouse already beats SQLite on most pages: Activity by
1.3x to 3.4x, Analytics by 3x to 10x, Trends by 4x, and search by 1.2x to 1.8x.
The production mirror loses most of that lead for one reason: it stores 4.4
copies of every message row. Whole-archive rewrites leave superseded row
versions that background merges never collapse, and every read pays to
deduplicate them with `FINAL`. Compacting a copy of the production tables made
cold pages 1.5x to 4.2x faster.

Usage is the one area where SQLite stays ahead on clean data, by 3x to 7x.
SQLite and PostgreSQL serve usage from a daily aggregate cache; ClickHouse ranks
and deduplicates every token-bearing message on each request.

[ClickHouse push and serve](clickhouse-mirror.md) describes the current design.
Pricing-table reads during push are out of scope here; separate work makes the
pusher check the pricing document by hash, send it once per push, and keep the
parsed copy.

## How this was measured

- Production: the serve database's `system.query_log`, `system.parts`, and
  read-only queries with `FINAL` added where serve relies on `final=1`.
- Clean mirror: a snapshot of a 43,611-session archive (1.33M messages) pushed
  to a local ClickHouse 26.10.3 server, served by the `main` binary next to a
  SQLite serve of the same snapshot.
- Production copy: every mirror table copied from production with its stored
  duplicates (merges stopped during the copy), served by the `main` binary with
  serve's settings (`final=1`, `max_threads=8`), then compacted with
  `OPTIMIZE TABLE ... FINAL` and measured again.
- "Cold" is the first request after a serve restart; "warm" is the second. The
  test machine runs the same production queries 2.4x to 3x faster than the
  production server, so compare speed-up factors, not absolute times.

## Findings

### Clean data: ClickHouse against SQLite

The speed-up column is SQLite time divided by ClickHouse time; values below 1
mean SQLite is faster.

| Page                         | SQLite cold (s) | ClickHouse cold (s) | Speed-up | SQLite warm (s) | ClickHouse warm (s) | Speed-up |
| ---------------------------- | --------------- | ------------------- | -------- | --------------- | ------------------- | -------- |
| Activity, one year           | 2.921           | 2.237               | 1.31x    | 0.097           | 0.055               | 1.77x    |
| Activity, one week           | 0.295           | 0.138               | 2.14x    | 0.076           | 0.023               | 3.35x    |
| Usage summary, one year      | 0.145           | 0.642               | 0.23x    | 0.016           | 0.104               | 0.15x    |
| Usage summary, 30 days       | 0.100           | 0.285               | 0.35x    | 0.008           | 0.028               | 0.27x    |
| Usage top sessions, 30 days  | 0.080           | 0.235               | 0.34x    | 0.076           | 0.012               | 6.40x    |
| Session list                 | 0.015           | 0.026               | 0.59x    | 0.013           | 0.017               | 0.74x    |
| Sidebar index                | 0.055           | 0.070               | 0.78x    | 0.055           | 0.071               | 0.78x    |
| Analytics summary, one year  | 0.694           | 0.148               | 4.69x    | 0.174           | 0.057               | 3.03x    |
| Analytics activity, one year | 0.462           | 0.047               | 9.84x    | 0.302           | 0.044               | 6.89x    |
| Trends, one year             | 1.536           | 0.383               | 4.01x    | 1.525           | 0.348               | 4.38x    |
| Session search               | 1.807           | 1.011               | 1.79x    | 1.796           | 1.001               | 1.79x    |
| Content search               | 2.563           | 1.540               | 1.66x    | 1.863           | 1.519               | 1.23x    |

The session list and sidebar are small keyed reads; both backends answer them in
tens of milliseconds, and network round trips decide the gap.

### Production stores 4.4 copies of each row

| Table                | Stored rows | Live rows | Copies | On disk   |
| -------------------- | ----------- | --------- | ------ | --------- |
| `messages`           | 12,602,838  | 2,866,285 | 4.4    | 6.49 GiB  |
| `tool_calls`         | 8,111,753   | 1,776,567 | 4.6    | 2.05 GiB  |
| `tool_result_events` | 6,811,786   | 1,828,659 | 3.7    | 10.86 GiB |

A push that rewrites a session inserts a new version of every row under a new
`push_version`. `ReplacingMergeTree` drops the older version only when a merge
puts both into one part, and the merge selector rarely merges a multi-gigabyte
part with smaller ones. On 2026-11-10 pushes wrote 7.31M message rows (10.69
GiB), 2.5 times the live table; on 2026-11-12 they wrote 4.41M rows (5.56 GiB).
Those were full pushes, which rewrite every session in an archive. A push starts
a full push when the archive is rebuilt or resynced, when a feature marker is
missing (usage snapshots, commit links, message dialogue, stored message
counts), when the deletion journal resets, or when scope changes. Each archive
that pushes runs its own.

Every serve query reads with `final=1`, so it merges all stored versions at read
time. On production, a one-year scan of `messages` takes 433 ms with `FINAL` and
35 ms without it (12x). Even on the clean local mirror, a second full push (1.48
stored copies) slowed cold pages 1.1x to 3.0x; content search went from 1.54 s
to 2.90 s.

### Compaction on the production copy

`OPTIMIZE TABLE ... FINAL` took 65 s for `messages` and `tool_result_events` on
the test machine. `messages` shrank from 6.45 GiB to 794 MiB (8.1x) and
`tool_result_events` from 9.70 GiB to 2.00 GiB (4.9x).

| Page                        | Stored copies cold (s) | Compacted cold (s) | Speed-up | Stored copies warm (s) | Compacted warm (s) | Speed-up |
| --------------------------- | ---------------------- | ------------------ | -------- | ---------------------- | ------------------ | -------- |
| Activity, one year          | 8.176                  | 5.413              | 1.51x    | 0.415                  | 0.184              | 2.25x    |
| Activity, one week          | 1.440                  | 0.390              | 3.69x    | 0.056                  | 0.035              | 1.61x    |
| Usage summary, one year     | 1.439                  | 0.436              | 3.30x    | 0.234                  | 0.202              | 1.16x    |
| Usage summary, 30 days      | 0.729                  | 0.202              | 3.61x    | 0.056                  | 0.046              | 1.22x    |
| Session list                | 0.038                  | 0.037              | 1.03x    | 0.037                  | 0.031              | 1.21x    |
| Sidebar index               | 0.170                  | 0.148              | 1.15x    | 0.147                  | 0.142              | 1.04x    |
| Analytics summary, one year | 0.322                  | 0.182              | 1.77x    | 0.177                  | 0.075              | 2.35x    |
| Trends, one year            | 2.394                  | 0.764              | 3.13x    | 1.609                  | 0.703              | 2.29x    |
| Content search              | 10.909                 | 2.581              | 4.23x    | 10.058                 | 2.291              | 4.39x    |

Single production queries on the same copy:

| Query                            | Stored copies (s) | Compacted (s) | Speed-up |
| -------------------------------- | ----------------- | ------------- | -------- |
| Activity message read            | 0.251             | 0.100         | 2.51x    |
| Activity tool event read         | 0.128             | 0.040         | 3.20x    |
| Trends message read              | 1.183             | 0.506         | 2.34x    |
| Usage summary (`prepared_usage`) | 0.713             | 0.743         | 0.96x    |

The usage query reads `prepared_usage`, a plain `MergeTree` that holds no stored
copies, so compaction leaves it as it was; the usage page still got faster cold
because its other reads did.

A read that filters each table to the live version with
`(session_id, push_version) IN (SELECT id, push_version FROM sessions)` instead
of `FINAL` took 194 ms against 433 ms on production (2.2x). Compaction removes
the cost instead of working around it, so this rewrite is not proposed.

### Activity spends most of a cold year in Go

On the compacted copy, a cold one-year Activity report takes 5.41 s. Reading
`prepared_usage` returns 1.58M rows (397 MB) in 1.84 s. The serve process then
spends about 3.3 s in Go: `selectActivityUsage` picks the surviving usage row
per message and `clickActivityUsageCost` prices each one, with garbage
collection taking 26% of CPU. The ClickHouse queries finish before that work
starts.

### Usage has no aggregate cache on ClickHouse

The usage summary query ranks every token-bearing message in the range to
deduplicate repeated usage records, then prices and groups them. On production,
the ranking window and its self-join take about 1.3 s of the query's 2.16 s. Two
rewrites returned the same rows and gained nothing: a `GROUP BY` on the dedup
keys (2.8 s against 2.2 s) and a 128-bit hash of the keys (2.13 s against 2.17
s). SQLite and PostgreSQL avoid the work on warm requests with the
[usage aggregate cache](usage-aggregate-cache.md); ClickHouse has no equivalent.

### Push bookkeeping reads

Pushers' bookkeeping queries (final-row hashes, `_part` scans, vector push
state, stored message counts) read 1,009 GiB on 2026-11-10, 125 GiB on
2026-11-11, and 262 GiB on 2026-11-12, for 1,549 s, 191 s, and 404 s of server
time. These scans read stored rows, duplicates included, and run on the server
that answers page reads.

### Trends and search

Counting trend terms in ClickHouse SQL instead of in Go took 2.0 s against 11.6
s on production's stored copies (5.7x). On the compacted copy at serve's
settings, the two queries take the same time (0.50 s against 0.51 s), and Go
counting adds about 0.26 s to a 0.76 s page. Open pull request #3084 reuses
finished Trends reports on repeat visits.

Content search scans 12 GiB of text per query on the stored copies and 2.6 s
after compaction. Production served no searches in the four days its query log
covers.

## Proposed changes

Ranked by measured gain on production data against cost.

| Rank | Change                                       | Measured gain                               | Cost   | Backends     |
| ---- | -------------------------------------------- | ------------------------------------------- | ------ | ------------ |
| 1    | Compact after a push that rewrote many rows  | Cold pages 1.5x to 4.2x faster              | Small  | ClickHouse   |
| 2    | Rewrite only changed sessions in a full push | Fewer stored copies between compactions     | Medium | ClickHouse   |
| 3    | Price Activity usage in SQL                  | Not prototyped; up to 3.3 s of a 5.4 s page | Medium | ClickHouse   |
| 4    | Daily usage aggregate table                  | Not prototyped; warm gap to SQLite is 6.6x  | Large  | ClickHouse   |
| 5    | Re-measure push bookkeeping after compaction | Not measured                                | Small  | ClickHouse   |
| -    | Trends counting in SQL                       | 1.0x after compaction                       | -      | Not proposed |
| -    | Content search index                         | No production searches                      | -      | Not proposed |
| -    | Usage dedup rewrite                          | 0.8x to 1.0x                                | -      | Not proposed |

### 1. Compact after a large rewrite

After a push finishes, if it wrote more than a set share of a table's live rows,
run `OPTIMIZE TABLE <table> FINAL` on the `ReplacingMergeTree` tables it
rewrote, largest first. A full push qualifies; a routine incremental push does
not. ClickHouse's guidance against routine `OPTIMIZE ... FINAL` exempts a
one-time merge after a bulk rewrite, which is this case.

- Gain: the compaction table above.
- Cost: the merge took 65 s on the test machine, so about 3 minutes on the
  production server, two or three times a week. It needs free disk for the
  merged part (under 11 GiB for the largest table) and the `OPTIMIZE` grant on
  the pushing role. Reads keep working during the merge.
- Applies to ClickHouse only; the other backends update rows in place.

### 2. Rewrite only changed sessions in a full push

A full push rewrites every session even when most rows are unchanged. Compare
each session's content fingerprint, as incremental pushes already do, and
rewrite only sessions whose stored rows differ; keep the full push's deletion
sweep. Feature markers that add a column to every row still need a rewrite of
the tables that carry the column, which this change must handle per marker.

- Gain: fewer stored copies and less write volume (10.69 GiB on 2026-11-10); it
  makes change 1 run less often but does not replace it.
- Cost: changes how each full-push reason decides what to rewrite; the test
  matrix covers every reason.

### 3. Price Activity usage in SQL

Move survivor selection and pricing for Activity's usage rows into the
ClickHouse query, so serve receives per-session totals instead of 1.58M rows.
`prepared_usage` already stores a cost per row, but Activity prices with the
rate in effect at billing time, so the query must reproduce that rule exactly.

- Gain: not prototyped. The Go work is about 3.3 s of a 5.4 s cold year.
- Cost: a second implementation of the billed-at pricing rule that must match
  the Go one; tests compare both on the same fixtures.

### 4. Daily usage aggregate table

Build the ClickHouse counterpart of the usage aggregate cache: a table of
deduplicated, priced daily contributions per session, model, and time zone,
refreshed by the push.

- Gain: not prototyped. Warm usage summaries on clean data take 0.104 s against
  SQLite's 0.016 s.
- Cost: a new derived table, its refresh rules, and its time zone handling.

### 5. Re-measure push bookkeeping after compaction

Bookkeeping scans read stored rows, so compaction should shrink them in
proportion. Measure them after change 1 ships before changing their queries.

## Limits

- Absolute times come from a faster machine than the production server.
- The SQLite comparison uses the clean snapshot only; the production mirror also
  holds sessions from archives the snapshot lacks.
- Changes 3 and 4 are estimates from timings of the current code, not
  prototypes.
