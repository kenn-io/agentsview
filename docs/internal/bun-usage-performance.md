# Bun daily usage performance

## September 2026 streaming snapshot selection

Continue the Bun consolidation. These changes reduce allocation and retain the
shared query and billing rules, but do not establish performance parity with
main or make the stack ready to merge.

Previously, daily usage retained every Claude candidate in both a full query
projection and a usage row before selecting survivors. It now keeps one winning
projection per message/request pair, plus compact state for earliest attribution
and maximum web-search billing. The existing activity comparisons still own
ranking and tie rules. A sorted index preserves winner scan order before the
stable daily ordering and general usage deduplication.

Projections use fixed-size arena blocks. This avoids copying large projections
as distinct requests accumulate without reserving memory for discarded
snapshots. The request still owns its scratch storage through reduction; public
results do not borrow it. Released blocks are cleared. The existing 128 MiB
retention limit remains per arena, not a process-wide memory bound.

The Claude query no longer needs a candidate-count window, and selection uses
one parsed usage fact for both output tokens and web-search counts.

A CPU profile then identified repeated lowercasing in catalog matching as a
larger cost: 2.91 seconds of the 5.99 sampled CPU seconds under the daily-usage
call. Catalog loading now normalizes immutable substring patterns once, and
resolution normalizes each model name once for all rules. Equality still uses
Unicode case folding; regular expressions still receive the original text. This
changes matching work without changing selected prices, temporal rules, provider
fallback, or persisted catalog content.

### Measurements

Synthetic fixture: 1,000 sessions, 64 messages per session, disposable DuckDB
and PostgreSQL stores. Baseline: `19dfbbc68856d29fd1359bb55d312c7101984919`.
Measurements used Go 1.27, darwin/arm64, CGO and `fts5,benchdb` on September 7,
2026\. The same harness was used for both revisions. MB means decimal megabytes.

Unique identities use the median of three samples, five calls per sample.
Repeated identities use one five-call sample with 16 snapshots per request.
Timing is directional: other work ran on the shared host. Pool reuse and garbage
collection also introduce variation in bytes per call. One final DuckDB timing
sample was 796 ms; the other two were 363 and 333 ms. These are comparisons
against the previously pushed Bun tip, not a new comparison against main.

| Identities | Backend    | Before ms/call | After ms/call | Before MB/call | After MB/call | Before allocations/call | After allocations/call |
| ---------- | ---------- | -------------: | ------------: | -------------: | ------------: | ----------------------: | ---------------------: |
| Unique     | DuckDB     |          671.1 |         363.5 |         116.81 |        107.38 |               3,318,899 |              3,190,804 |
| Unique     | PostgreSQL |          733.7 |         378.7 |         138.23 |        124.61 |               3,014,420 |              2,886,463 |
| Repeated   | DuckDB     |          163.4 |         139.6 |          67.68 |         55.40 |               2,597,928 |              2,469,843 |
| Repeated   | PostgreSQL |          296.6 |         189.5 |          70.84 |         51.20 |               2,293,449 |              2,165,478 |

One cold PostgreSQL heap sample per concurrency level, using unique identities,
measured after the arena change and before the catalog optimization:

| Concurrent requests | Before sampled peak MB | After sampled peak MB | Before MB after one GC | After MB after one GC | Before MB after two GCs | After MB after two GCs |
| ------------------: | ---------------------: | --------------------: | ---------------------: | --------------------: | ----------------------: | ---------------------: |
|                   1 |                 135.03 |                122.91 |                  79.22 |                 68.71 |                    9.48 |                   9.48 |
|                   4 |                 503.65 |                437.69 |                 288.45 |                246.57 |                    9.87 |                   9.86 |

The four-request peak improved by 13%, and retained heap after one collection by
15%. Concurrent usage still needs hundreds of megabytes; this change does not
establish a global retention bound or a physical-memory result.

### Reproduction

Run `BenchmarkStoreBackends/DailyUsage/(duckdb|postgres)$` in
`internal/backendbench` with
`-tags fts5,benchdb -run '^$' -benchmem -benchtime 5x -count 3` and
`CGO_ENABLED=1`. Docker must be available for the disposable PostgreSQL fixture.
Set `AGENTSVIEW_BENCH_SNAPSHOT_REPETITIONS=16` for repeated snapshots; the
default is one. Session and message counts remain configurable through
`AGENTSVIEW_BENCH_SESSIONS` and `AGENTSVIEW_BENCH_MESSAGES_PER_SESSION`.

`BenchmarkDailyUsageHeap/postgres/` in the same package measures one and four
simultaneous requests. Run it with `-benchtime 1x -count 1`. It samples Go heap
every 5 ms and reports heap after one and two forced collections. Its figures
include fixture heap, exclude native driver memory, and are not exact peaks or
process physical-footprint measurements.

### Regression coverage

The snapshot-storage test increases one request from 16 to 4,096 candidates
while retaining one arena block and the fullest output count. Arrival-order
coverage verifies that token selection, earliest attribution and maximum billed
searches can come from three different rows. Existing cross-session, filter,
web-search and result-lifetime tests exercise the shared public usage APIs.

The focused 256-rule catalog benchmark reduced an uppercase model lookup from
about 16.9 microseconds and 258 allocations to 0.76 microseconds and two
allocations. This isolates repeated normalization from database and host I/O; it
is not an end-to-end speedup claim.

### Remaining work

The allocation profile points to PostgreSQL row-value conversion and timestamp
formatting as the next targets. The catalog change reduces CPU work but does not
remove those allocations. Keep further changes inside the shared Bun query and
reduction path, and preserve attribution, window filtering, and pricing
provenance. Repeat the main comparison before deciding to merge; the numbers
above establish progress against the pushed stack, not that all gates pass.
