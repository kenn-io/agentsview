# Bun storage performance assessment

## Decision

Keep the stack and land the combined feature once the final tip passes CI. The
shared usage query now streams into Go aggregation without retaining all winning
rows. Token interpretation, snapshot ranking, pricing, and billing remain in Go.
The archive backup, rebuild, and rollback procedure remains documented in
`docs/internal/storage-upgrade.md`.

The large concurrent-heap regression came from retaining and pooling the
complete input to the reducers. That storage and the redundant consumer
duplicate maps are removed. PostgreSQL and DuckDB daily usage remain faster than
main on the measured fixture. SQLite unique usage measured 42.19 ms against
37.20 ms on main; repeated usage and bulk writes remain comparable. Repeated
snapshots still incur more driver allocations than main, which removes those
candidates in SQL before transferring them.

## Query and ownership

Ordinary message identities are separate from event and Cursor identities, so
messages can be consumed in their own chronological stream. Usage events and
Cursor charges share duplicate keys; a common `UNION ALL` query orders them
together before Go selects the first in-window occurrence. This preserves which
source supplies tokens and reported costs even when the timestamps differ by one
microsecond. Native timestamp columns retain native ordering. SQLite's canonical
UTC text uses its exact fractional representation rather than `julianday`, whose
precision is insufficient for this ordering.

Claude candidates arrive ordered by the public duplicate key, the actual
message/request pair, and source time. Go holds the current pair's winning
snapshot, earliest attribution, and maximum web-search count. A pending winner
preserves chronological selection if distinct pairs produce the same
colon-joined duplicate key. Completed groups feed the reducer immediately.

The shared stream owns duplicate selection once. Its seen set covers ordinary
source/event keys only; completed Claude groups need no retained identity map.
Daily usage, top-session costs, and billed session counts consume the stream.
Matching-session counts stream candidates without usage deduplication. Every
consistent-view retry constructs fresh aggregate state. Consumer failures stop
scanning and close the rows.

There is no survivor-row array, geometric growth, final Go row sort, or usage
arena pool. Memory still grows with distinct ordinary duplicate keys, sessions,
and output buckets; this is not a claim of constant memory for every workload.
Bun continues to own query execution and transaction guards on every backend.
The query policy is shared rather than copied into backend-specific stores.

## Method

Measured September 7, 2026, Go 1.27, darwin/arm64, CGO, `fts5,benchdb`. Main is
`618f0fabb9471cce346d1c09abd8150f6889671d`; the previous pooled implementation
is `db98c502961f8caf3bee9450298aa97059b29da5`. An isolated copy of main ran the
same benchmark fixture and embedded pricing snapshots. All databases and
sessions were synthetic and disposable.

The fixture has 1,000 sessions and 64 messages per session. Daily timings and
allocations are medians of three samples with five calls each. Repeated
identities use sixteen snapshots per request. MB means decimal megabytes. Other
work ran on the host, so timings establish direction rather than statistical
significance. Pool eviction affected the previous implementation's allocation
samples; its published median is retained below.

## Daily usage

| Fixture  | Backend  | Main ms | Pooled ms | Streaming ms | Main MB/op | Pooled MB/op | Streaming MB/op | Streaming allocs/op |
| -------- | -------- | ------: | --------: | -----------: | ---------: | -----------: | --------------: | ------------------: |
| unique   | sqlite   |   37.20 |     35.31 |        42.19 |      12.27 |        13.00 |           13.00 |              165034 |
| unique   | duckdb   |  338.44 |    193.19 |       157.06 |      63.06 |        80.16 |           55.47 |             2484653 |
| unique   | postgres |  641.30 |    226.53 |       190.45 |      78.70 |       110.48 |           49.03 |             2180320 |
| repeated | sqlite   |   41.38 |     38.48 |        38.87 |      15.26 |        15.98 |           15.98 |              239401 |
| repeated | duckdb   |   96.48 |     80.53 |        84.10 |      10.92 |        49.33 |           47.79 |             2124653 |
| repeated | postgres |  359.34 |    128.59 |       148.16 |      11.69 |        44.22 |           40.39 |             1820319 |

The final implementation keeps token-dependent snapshot selection in Go.
Repeated candidates therefore still cross the driver boundary, unlike main's SQL
ranking path. The remaining repeated-snapshot allocation gap is visible in the
table and is not hidden by the heap improvement.

## Concurrent Go heap

One cold sample at each concurrency, unique identities. Sampling runs every five
milliseconds. Values include fixture Go heap and exclude native-driver and
PostgreSQL-server memory; they are neither exact peaks nor process physical
footprints.

| Implementation | Requests | Sampled peak MB | After one GC MB | After two GCs MB |
| -------------- | -------: | --------------: | --------------: | ---------------: |
| Main           |        1 |           26.51 |            4.68 |             4.67 |
| Pooled         |        1 |          106.30 |           42.04 |             9.53 |
| Streaming      |        1 |           20.06 |            9.57 |             9.51 |
| Main           |        4 |           77.64 |            4.96 |             4.95 |
| Pooled         |        4 |          317.37 |          139.72 |             9.91 |
| Streaming      |        4 |           28.67 |            9.88 |             9.86 |

The pooled implementation used 400-byte rows plus referenced strings. Its
65,536-row backing array alone occupied 26.2 MB; growing it temporarily retained
old copies, and release kept the cleared array in the pool. A follow-up profile
attributed about 36% of request allocation bytes to array growth and 35% to
PostgreSQL decoding. Those are allocation-volume shares, not retained-heap
shares. Streaming removes the array's lifetime altogether.

These measurements qualify only the stated cardinality and concurrency. The
queries can still require database sorting, and ordinary duplicate identities
and aggregate groups consume Go memory. Larger archives, higher concurrency,
native memory, and temporary-file spills need separate measurements. The CI
benchmark gate does not enforce a global memory budget.

### Other reads

Same fixture and sampling method. These measurements cover the final reader;
small differences remain sensitive to shared-host load.

| Operation           | Backend  | Main ms | Final ms | Main MB | Final MB |
| ------------------- | -------- | ------: | -------: | ------: | -------: |
| ListSessions        | sqlite   |   1.149 |    1.395 |   0.235 |    0.250 |
| ListSessions        | duckdb   |   1.051 |    1.052 |   0.257 |    0.264 |
| ListSessions        | postgres |   1.730 |    2.371 |   0.248 |    0.253 |
| SidebarSessionIndex | sqlite   |   2.106 |    2.508 |   1.409 |    1.574 |
| SidebarSessionIndex | duckdb   |   4.180 |    3.180 |   1.511 |    1.547 |
| SidebarSessionIndex | postgres |  17.871 |   19.148 |   1.531 |    1.520 |
| Search              | sqlite   |  22.920 |   27.519 |   0.130 |    0.159 |
| Search              | duckdb   |  99.572 |  111.163 |   0.106 |    0.121 |
| Search              | postgres |  30.650 |   31.836 |   0.117 |    0.139 |
| GetAllMessages      | sqlite   |   0.193 |    0.224 |   0.138 |    0.153 |
| GetAllMessages      | duckdb   |   1.856 |    1.276 |   0.132 |    0.161 |
| GetAllMessages      | postgres |   1.483 |    1.828 |   0.183 |    0.154 |
| AnalyticsSummary    | sqlite   |  19.539 |   20.817 |   0.010 |    0.022 |
| AnalyticsSummary    | duckdb   |   9.892 |    4.203 |   2.502 |    0.028 |
| AnalyticsSummary    | postgres |  16.064 |   13.591 |   1.037 |    0.024 |

## Tools and writes

The earlier stack's year-range tools report failed the benchmark gate at 139.6
ms, 13.62 MB, and 662,300 allocations. The UTC-expression change brought the
prior tip's CI result to 37.44 ms, 0.260 MB, and 5,014 allocations, against
19.74 ms, 0.406 MB, and 9,784 allocations on its baseline. That passed the
existing 2x time, 1.35x bytes, and 1.25x allocation limits. This query change
does not relax those thresholds.

Bulk insertion measured 3.31 ms and 0.487 MB on main versus 3.32 ms and 0.393 MB
on the stack, using the same 200-message fixture and twenty iterations.
Allocations fell from 1,756 to 915. The streaming-query change does not alter
that writer. Earlier noisy 77/39 ms samples are not evidence of a write speedup.

## Correctness and delivery

The shared contract exercises all three engines. It preserves exact costs,
pricing bands and provenance, snapshot winners and attribution, date/model
filters, source duplicates, and Cursor/event ordering. New cases distinguish
one-microsecond ordering from session-ID ordering, both directions of a
Cursor/event duplicate, and distinct Claude identity pairs with the same public
key. Focused tests also cover interrupted consumers, independent results, and
consistent-view replay.

The full database and DuckDB suites, PostgreSQL usage/analytics cases including
complete SQLite/PostgreSQL result parity, focused race checks, formatting, and
vet are the local checks. The combined tip is the CI acceptance target;
intermediate stack PRs need not independently pass all checks. No archive,
provider format, pricing policy, or persisted cache format changes here.

## Reproduction

Run `BenchmarkStoreBackends` in `internal/backendbench` with
`CGO_ENABLED=1 go test -tags fts5,benchdb ./internal/backendbench -run '^$' -bench BenchmarkStoreBackends/DailyUsage -benchmem -benchtime 5x -count 3`
as one command. Docker must be available for the disposable PostgreSQL fixture.
With a Docker VM, point `DOCKER_HOST` at its host socket and
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` at the socket inside that VM.

Set `AGENTSVIEW_BENCH_SNAPSHOT_REPETITIONS=16` for repeated identities.
`AGENTSVIEW_BENCH_SESSIONS` and `AGENTSVIEW_BENCH_MESSAGES_PER_SESSION` adjust
fixture size. Run `BenchmarkDailyUsageHeap/postgres` with
`-benchtime 1x -count 1` for one and four concurrent cold requests and post-GC
heap measurements. Use identical fixture source on main and the candidate.
