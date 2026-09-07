# Bun storage performance assessment

## Decision

Keep the stack and land the combined feature after the final revision passes CI
and the documented archive-upgrade procedure is reviewed. Performance is
sufficient to continue; allocation parity is not established and is not the
basis for this recommendation.

The shared implementation materially reduces PostgreSQL and DuckDB daily-usage
latency. SQLite warm daily usage and bulk insertion remain comparable to main.
The tools-report regression found by the benchmark gate is repaired. The
tradeoff is higher allocation volume and transient Go heap for live usage
reports, particularly with repeated snapshots. These costs are explicit below;
pools do not make them disappear.

## Implementation and maintenance

Claude rows are ordered by message/request identity, then by the original
chronological source order. The reader keeps one group's winner, earliest
attribution, and maximum billed web searches. Only surviving usage rows enter
the pooled arena. This removes the map and projection blocks previously held for
every distinct request. The final sort preserves source order even when
attribution gives two winners the same session, timestamp, and ordinal.

Survivor storage grows geometrically with exact capacity steps to avoid repeated
copies and compounded allocator rounding. Release clears all references. The 128
MiB retention cap is per arena, not a global limit or a limit on an active
request. The Go collector can discard idle pool entries.

Pricing resolves a computed row once and retains that exact lookup for
provenance, including provider billing adjustments. Token extraction no longer
formats and reparses timestamps whose values the caller already holds. Small
pricing alias lists use stack arrays. The existing catalog normalization
optimization remains in place.

SQLite analytics uses its canonical UTC timestamps without a Go timezone
callback for UTC reports. The SQL representation preserves fractional seconds
and correct last-used ordering. Non-UTC conversion retains one timezone per
connection rather than reloading its file for every row. PostgreSQL and DuckDB
keep their existing native timezone expressions within the same shared queries.

These changes keep snapshot ranking, billing, filtering, and result ownership in
the common implementation. They add no backend-local Store methods, unsafe
allocator, alternate query engine, or caller-visible configuration. Backend
adapters still own connection lifecycle and the documented capability seams.

## Method

Measured September 7, 2026, Go 1.27, darwin/arm64, CGO, `fts5,benchdb`. The
baseline is current main `618f0fabb9471cce346d1c09abd8150f6889671d`. The
previous published stack is `088c01e3e247ee8bd741c7409bb77479395acb3d`. An
isolated source copy of main ran the same backend benchmark fixture as the
candidate, with identical embedded pricing snapshots. All databases and sessions
were synthetic and disposable. No live archive was used.

The fixture has 1,000 sessions and 64 messages per session. Timings and
allocations are medians of three samples, five calls per sample. Repeated
identities use 16 snapshots per request. MB means decimal megabytes. Other work
ran on the host; these timing samples establish direction, not statistical
significance. GC and pool eviction also affect bytes per call. For example,
final unique PostgreSQL samples allocated 97.90, 110.48, and 123.10 MB.

### Daily usage

| Identities | Backend  | Main ms | Published ms | Final ms | Main MB | Published MB | Final MB | Main allocations | Final allocations |
| ---------- | -------- | ------: | -----------: | -------: | ------: | -----------: | -------: | ---------------: | ----------------: |
| Unique     | sqlite   |   37.20 |        35.30 |    35.31 |   12.27 |        13.07 |    13.00 |          162,204 |           165,033 |
| Unique     | duckdb   |  338.44 |       255.63 |   193.19 |   63.06 |       107.38 |    80.16 |        2,246,794 |         2,421,245 |
| Unique     | postgres |  641.30 |       309.93 |   226.53 |   78.70 |       136.44 |   110.48 |        1,523,125 |         2,116,894 |
| Repeated   | sqlite   |   41.38 |  not sampled |    38.48 |   15.26 |  not sampled |    15.98 |          236,958 |           239,394 |
| Repeated   | duckdb   |   96.48 |  not sampled |    80.53 |   10.92 |  not sampled |    49.33 |          189,356 |         2,120,777 |
| Repeated   | postgres |  359.34 |  not sampled |   128.59 |   11.69 |  not sampled |    44.22 |          142,619 |         1,816,413 |

Main's PostgreSQL query ranks and removes duplicate snapshots in SQL before
scanning results. The shared Bun reader transfers candidates and selects them in
Go. This explains the remaining repeated-snapshot allocation gap; pooling
survivors cannot remove driver allocations for discarded input. The final
repeated PostgreSQL report is nevertheless substantially faster on this fixture.
This is an accepted latency/temporary-allocation tradeoff, not a claim that the
two implementations have equal memory cost.

### Concurrent Go heap

One cold sample per concurrency level, unique identities, same fixture. These
are sampled Go heap figures, not process physical memory or PostgreSQL server
memory.

| Requests | Main peak MB | Final peak MB | Main after one GC MB | Final after one GC MB | Main after two GCs MB | Final after two GCs MB |
| -------: | -----------: | ------------: | -------------------: | --------------------: | --------------------: | ---------------------: |
|        1 |        26.51 |        106.30 |                 4.68 |                 42.04 |                  4.67 |                   9.53 |
|        4 |        77.64 |        317.37 |                 4.96 |                139.72 |                  4.95 |                   9.91 |

The four-request peak remains about four times main's. This is the largest
remaining cost of doing candidate selection and retaining sorted survivors in
Go. Pool retention drops after the second collection; there is no claim of
process-wide memory parity or a global allocation bound. I accept this tradeoff
for the measured latency gain and common behavior implementation, rather than
making exact heap parity a reason to keep rebasing the stack. Deployments that
routinely issue concurrent all-history reports should account for this transient
memory cost when sizing the service.

### Why the Go heap is higher

A follow-up allocation profile of the same unique-identity PostgreSQL fixture
locates the cost in materialization, driver decoding, and object lifetime. Main
aggregates each returned row inside its database iterator. The shared reader's
`streamDailyUsageRowsFrom` first calls `loadDailyUsageRowsFrom`, which retains
all surviving rows and sorts them before invoking the aggregate consumer.
Grouping discarded snapshots does not make that final stage stream.

Each `dailyUsageScanRow` occupies 400 bytes on the measured arm64 toolchain,
excluding the data referenced by strings. A 65,536-row backing array therefore
occupies 26.2 MB. It includes three `time.Time` values, a formatted timestamp,
token JSON, counters, identities, and session metadata. Growing this array also
allocates replacement arrays; previous copies await collection. Across the
profiled request stacks, slice growth accounted for about 36% of allocation
bytes, PostgreSQL string/timestamp decoding for 35%, and the aggregate consumer
itself for 14%. These are allocation-volume shares, not retained-heap shares.

The arena remains reachable throughout aggregation. On release it clears the
referenced strings, but the pool can retain the entire empty backing array.
Consequently pooling reduces repeat allocation while keeping substantial storage
after a request. The one-GC and two-GC measurements above distinguish this
temporary pool retention from the final report's much smaller result. They do
not establish a long-running process memory bound or rule out every possible
leak. For repeated snapshots, the driver additionally allocates for candidates
that main's SQL removes before returning them.

The next memory optimization should reduce the rows that must remain live for
aggregation, rather than merely raise the pool cap or add more pools. Any such
change must preserve cross-source deduplication and chronological tie order;
feeding identity-ordered rows directly into the existing consumer would change
which duplicate wins. Compacting the surviving row representation and releasing
consumed references are narrower options to measure against true streaming.

### Database sorting and assessment limits

`EXPLAIN ANALYZE` of the actual grouped Claude query on the same isolated
64,000-message fixture confirms that both PostgreSQL and DuckDB sort candidate
rows. PostgreSQL used an external merge sort with 19,192 KiB of disk space and
returned 64,000 rows. The sort node completed at 66.8 ms, including its child
work. DuckDB's plan includes an `ORDER_BY` operator; its complete profiled query
took 11.8 ms. These were warm diagnostic runs. They are not a main-versus-stack
comparison and do not measure native process memory.

The measured envelope is 64,000 messages, unique or sixteen-fold repeated
identities, with one and four concurrent requests for the unique PostgreSQL
case. No larger archive or higher concurrency has been qualified by this
assessment. Do not extrapolate its latency or memory ratios into a supported
archive-size limit. The existing benchmark gate is the latency/allocation
acceptance check for its own workloads; it does not enforce total process memory
or database spill limits.

The heap table records the current memory cost being assessed, not memory
parity: approximately 106 MB for one request and 317 MB for four, falling below
10 MB after two collections in those samples. A revision that exceeds those
costs on the same fixture worsens the documented tradeoff and needs a new
assessment. Reducing that cost remains performance work even though CI and the
latency gate pass. Claims about larger archives require new cardinality and
concurrency measurements, including database-side resources.

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

### Tools and writes

The published head's CI benchmark gate reported the year-range tools query at
139.6 ms, 13.62 MB, and 662,300 allocations, against main's 25.51 ms, 0.406 MB,
and 9,784 allocations on that runner. Its only three gate failures were this
query's time, bytes, and allocation count. The final local query measures 22.11
ms, 0.260 MB, and 5,014 allocations; local main measures 11.59 ms, 0.406 MB, and
9,784 allocations. This removes the allocation regression and brings the local
time ratio below the existing 2x gate without changing limits. The final CI gate
confirmed 37.44 ms against 19.74 ms on its baseline, a 1.90x time ratio, with
0.260 MB and 5,014 allocations. It passed the existing thresholds without
changing them.

Bulk insertion uses the same 200-message fixture and 20 iterations per sample.
Local main is 3.31 ms and 0.487 MB; the stack is 3.32 ms and 0.393 MB, with
allocations reduced from 1,756 to 915. The published CI run independently showed
5.749 ms versus 5.743 ms and 0.487 MB versus 0.389 MB. Earlier local 77/39 ms
samples were distorted by host variability and are not evidence of a speedup.

The production atomic streaming-tail writer measured 11.37 ms for a 1,000-row
session with one changed tail. Main has no equivalent `WriteSessionAtomic` entry
point, so this is not presented as a before/after comparison.

## Correctness and delivery

The full database, DuckDB, export, activity, pricing, and PostgreSQL integration
suites passed during this work. The final grouped reader passed PostgreSQL's
usage and analytics cases, including complete SQLite/PostgreSQL result parity.
Focused race checks cover arena result ownership, snapshot selection, pricing,
and per-connection timezone conversion. Formatting and vet passed.

Regression coverage preserves winning tokens, earliest attribution, billed
searches, date/model/session filters, provider provenance, final-microsecond UTC
filtering, timezone changes on one connection, result lifetime after pool reuse,
and the original ordering of tied attributed winners. Survivor capacity stays
constant when one request grows from 16 to 4,096 snapshots.

The stack still requires its documented data-version rebuild. This performance
assessment does not replace archive backup, recovery, and upgrade review in
`docs/internal/storage-upgrade.md`. The combined tip is the acceptance target;
intermediate PRs need not independently pass all checks.

## Reproduction

Run `BenchmarkStoreBackends` in `internal/backendbench` with
`CGO_ENABLED=1 go test -tags fts5,benchdb ./internal/backendbench -run '^$' -bench BenchmarkStoreBackends -benchmem -benchtime 5x -count 3`
as one command. Docker must be available for the disposable PostgreSQL fixture.
With a Docker VM, point `DOCKER_HOST` at its host socket and
`TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` at the socket inside that VM.

Set `AGENTSVIEW_BENCH_SNAPSHOT_REPETITIONS=16` for repeated identities.
`AGENTSVIEW_BENCH_SESSIONS` and `AGENTSVIEW_BENCH_MESSAGES_PER_SESSION` adjust
fixture size. Run `BenchmarkDailyUsageHeap/postgres` with
`-benchtime 1x -count 1` for one and four concurrent cold requests. It samples
Go heap every 5 ms and reports heap after one and two collections. Those
measurements include fixture heap, exclude native driver/server memory, and are
neither exact peaks nor process physical-footprint measurements.

Run `BenchmarkGetAnalyticsToolsYearRange` and `BenchmarkInsertMessagesBatch` in
`internal/db` with CGO, `fts5`, `-run '^$' -benchmem -benchtime 20x -count 3`.
Use the same fixture source on both revisions. Do not compare a session-batch
writer with a lower-level message-only API as though their work were identical.
