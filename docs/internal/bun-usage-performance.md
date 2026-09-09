# Bun storage performance assessment

## Decision

Keep the stack. The rebased local fresh-sync measurements support continuing
with the combined feature; the external BenchDB run remains the acceptance check
for its machine and corpus. The shared usage query now streams into Go
aggregation without retaining all winning rows. Token interpretation, snapshot
ranking, pricing, and billing remain in Go. The archive backup, rebuild, and
rollback procedure remains documented in `docs/internal/storage-upgrade.md`.

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

## Fresh-sync follow-up

The separate production-scale benchmark reported fresh sync at 437.70 seconds
for `6a81b90eb0c07ad2a2dc4ba92fbe35255b50e1e1`, against 383.96 seconds for
`e1e2eaf81f4d11b6b8e08fb36d17817a8c61d319`. This 14% regression was not covered
by the earlier usage measurements or the passing repository benchmark gate. The
same report measured daily usage within 3% of its baseline.

Native samples of a synthetic full rebuild showed substantial time in repeated
UTF-8 and control-character scans of clean transcript bodies. The shared
sanitizer now checks clean input in one pass, decoding only non-ASCII text.
Inputs needing repair retain the existing normalization behavior. Archive,
PostgreSQL, and fingerprint callers use the same implementation.

The clean-text microbenchmark improved from a median 20.84 microseconds to 9.53
microseconds per 16 KiB, with zero allocations in both versions. The
full-rebuild fixture uses 100 sessions, 300 messages per session, and 16 KiB of
additional assistant text. Three-iteration compiled-binary runs measured 3.84
seconds before and 3.21 seconds after the change. The reverse-order run measured
3.62 seconds before and 3.24 seconds after, a 10% improvement. Host contention
affected earlier samples; these local measurements do not establish that the
production-scale regression is resolved. That requires a new run of the external
fresh-sync benchmark against the pushed revision.

The full-rebuild fixture can be reproduced with
`AGENTSVIEW_BENCH_SYNC_SESSIONS=100`, `AGENTSVIEW_BENCH_SYNC_MESSAGES=300`, and
`AGENTSVIEW_BENCH_SYNC_REPLY_BYTES=16384`, running
`BenchmarkResyncBulkContributorIngestUsage` with `-benchtime 3x`.

### Further fresh-sync tuning

A follow-up comparison against `6d935f4e0` isolated SQL/index work, repeated
text conversion, and pending parsed-result retention. The same 100-session,
300-message, 16 KiB reply fixture ran as compiled binaries with three iterations
per sample, then repeated in reverse variant order. Runs overlapping other Go
test jobs were discarded.

| Variant                            | First sample | Reverse sample | Peak RSS samples |
| ---------------------------------- | ------------ | -------------- | ---------------- |
| Before this follow-up              | 3.230 s      | 3.219 s        | 627 / 632 MB     |
| Skip the final validated-body scan | 3.076 s      | 3.154 s        | 641 / 636 MB     |
| Lower pending budget to 128 MiB    | 3.140 s      | 3.300 s        | 399 / 398 MB     |
| Both changes                       | 3.084 s      | 3.250 s        | 398 / 400 MB     |

The combined change reduces process peak RSS by about 37%. Its average elapsed
time is about 2% lower, within the variation between local samples. This does
not establish parity with the external baseline. Allocation volume remains
roughly 2.28 GB per rebuild: smaller batches shorten the lifetime of retained
parser data rather than eliminating its allocation.

Separate diagnostic runs forced collection at batch preparation and write
checkpoints. Maximum sampled live heap fell from 242 MB to 120 MB. The larger
heap attributed about 86% to retained parser content and 3% to converted
database messages. Existing model-row pools already clear references and cap
retained backing storage. No additional pool was introduced.

Archive-scale passes now retain an estimated 128 MiB of completed parsed
results, down from 512 MiB, while keeping the 256 MiB active-parser budget.
Oversized or unknown sources remain indivisible and may exceed the pending
budget before their standalone batch is written. The fixture consequently uses
eight transactions instead of two. The worker-admission limit is unchanged.

Validated session batches also avoid scanning Content and ThinkingText again
when building canonical message rows. Direct incremental writes, repairs, and
mirror conversion still sanitize those fields at their existing boundaries; all
paths keep the same canonical field projection and graph writes.

Deferring the additional timestamp index until the end of the rebuild did not
improve the local paired measurements, so index handling remains unchanged. A
256 MiB pending-budget alternative retained more memory without a consistent
speed advantage. External benchmark pickup after push remains the acceptance
check for production-scale fresh-sync time and memory.

### Tool-call message lookup

Tool-call writes resolve parent message IDs by selecting `id` and `ordinal`. The
scan now stores those two fields instead of allocating full message model
structs. The canonical Bun model still owns the query, and the selected rows and
parent-ID mapping are unchanged.

An isolated SQLite comparison covered 300 and 3,000 messages with tools on 10%,
50%, and 100% of messages, including repeated calls and noncontiguous physical
IDs. Three 100-operation samples per case, repeated in reverse variant order,
measured 52–74% fewer allocated bytes and 6–16% lower lookup time. At 3,000
messages with tools on every message, allocation fell from 4.666 MB to 1.201 MB
per lookup. Median time fell from 4.072 ms to 3.773 ms in the first order and
from 3.953 ms to 3.616 ms in reverse order.

These are lookup-only measurements. The earlier full-rebuild fixture has no tool
calls and cannot measure this change. External fresh-sync results are still
required to establish its effect on the complete workload.

## Rebased local fresh sync

Measured September 9, 2026 against baseline
`e1e41fd0e31c41b7a6a0cd76c1f61a0cb2da4147`, with the stack rebased onto that
revision. The private copied corpus contains about 14 GB of Claude and Codex
JSONL. Each run uses a fresh archive, read-only source mounts, and the real
`sync --full` daemon/worker path. Source files are read before timing, matching
the benchmark harness's prewarming. Worker CPU and allocation profiles are
captured on both baseline and candidate.

Runs execute locally in Linux ARM64 containers with Go 1.27, CGO, `fts5`, GCC
12.2, eight virtual CPUs, and about 32 GB of VM memory. The external benchmark
uses a different CPU architecture, compiler, and corpus. Local host variation
also matters: prewarmed baseline samples ranged from 143.27 to 166.20 seconds.
These results establish comparable local performance, not a proven speedup or an
external benchmark pass.

| Variant                                 |    Wall seconds | Peak anonymous GB | Allocated GiB |
| --------------------------------------- | --------------: | ----------------: | ------------: |
| Baseline, repeated with memory sampler  |          166.20 |             0.782 |         59.68 |
| Rebased integration, original formatter | 142.46 / 146.93 |     1.296 / 1.337 |         82.94 |
| Preallocation alone                     |          171.83 |             1.398 |         79.84 |
| Chunk formatter, 16 MiB statements      |          172.59 |             1.491 |         80.21 |
| Chunk formatter, 1 MiB statements       | 149.58 / 160.81 |     0.810 / 0.773 |         78.92 |

GB is decimal; GiB is binary. Anonymous memory is sampled from the container
cgroup every 250 ms across the CLI, daemon, and worker. It includes native
allocations and is not an exact Go heap peak. Cgroup total memory includes
roughly 20 GB of file cache on many runs, so it must not be described as
retained application memory. Allocation volume comes from the worker's
`alloc_space` profile; it is cumulative allocation, not retained memory.

The SQLite formatter now reserves room for each string and copies spans between
quotes, preserving NUL and invalid UTF-8 key bytes. This reduces allocation in
the tool-payload microbenchmark from 26.69 to 15.31 MB per operation. It did not
establish a full-sync speed improvement by itself. Bun still starts insert SQL
with a small buffer; growing formatted statements accounted for about 20 GiB of
allocation in the full-corpus profile.

Canonical writes now target 1 MiB of estimated row payload per SQL statement,
down from 16 MiB. Transactions and row order are unchanged, and oversized rows
remain whole. The smaller statements bring peak anonymous memory close to the
baseline in both samples. Total allocation remains about 32% higher than
baseline. The change shortens transient buffer lifetimes rather than removing
Bun's SQL formatting cost; an additional pool is not claimed to solve it.

On the tool-payload microbenchmark, the final 1 MiB statements allocate about
5.02 MB per operation, measured with three samples of three iterations each on
darwin/arm64. The 15.31 MB measurement above isolates the formatter change with
the earlier 16 MiB statements. Both variants check persisted result contents
outside the timer.

Staged Codex publication retains the upstream streaming path while committing
session content, usage, signals, and checkpoints together through Bun. Its
scratch store also uses Bun. SQLite-only raw-result metadata survives
replacement and remains excluded from portable mirrors. All compared archives
contain 13,967 sessions, 816,220 messages, 628,068 tool calls, and 567,881
result events, with no missing raw-result metadata.

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
intermediate stack PRs need not independently pass all checks. The rebase
retains upstream archive and parser-version changes; pricing policy remains
unchanged.

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

## Profiling spawned sync workers

A normal `sync --full` can send work through a daemon to a separate sync worker.
The CLI's `--cpuprofile`, `--memprofile`, and `--trace` flags capture only the
CLI process. To capture the worker, set `AGENTSVIEW_SYNC_PROFILE_DIR` before
starting an isolated daemon. Spawned workers inherit it and write CPU and memory
profiles into a separate private `sync-worker-<mode>-<pid>-<suffix>` directory
under that root. Filenames are `cpu.pprof` and `memory.pprof`. The memory
profile is captured after a final garbage collection; use its `alloc_space`
sample for allocation volume rather than treating it as peak live memory.

Set `AGENTSVIEW_SYNC_PROFILE_TRACE=true` as well to write `runtime.trace`.
Tracing is separate because a full archive rebuild can produce a large trace.
Profiling is disabled by default. An invalid trace value or inaccessible output
directory logs a diagnostic and leaves the sync pass running without worker
profiling. Existing daemons must restart in the isolated environment to inherit
these settings. Use disposable source and archive clones for profiling.

## Tool analytics benchmark gate

The year-range tool analytics gate measured 39.22 ms against a 19.61 ms
baseline. The query computed weekly buckets per tool call in SQL, then the Go
response builder bucketed the aggregate dates into weeks again.

Group by local date in SQL and keep weekly response bucketing in Go. SQLite UTC
date extraction now reads the canonical timestamp's date directly, without
normalizing its separator and suffix. Other timezones still use the existing
local-time conversion. A session spanning several days can return up to seven
daily groups per former weekly group; individual tool calls remain aggregated in
the database.

A local macOS arm64 comparison used the same year-range fixture, ten iterations
per sample, and three samples per version. The original query took 21.57–22.57
ms before the candidate run. Repeating in reverse order measured 13.40–13.98 ms
for the candidate and 21.53–22.27 ms for the original query, about 38% lower at
the median. Allocation was about 260 KB per operation versus 262–263 KB. These
local measurements do not establish a CI gate pass; the next pushed commit must
run through the gate unchanged.

Reproduce with `BenchmarkGetAnalyticsToolsYearRange` in `internal/db`, using
`CGO_ENABLED=1 go test -tags fts5 ./internal/db -run '^$' -bench '^BenchmarkGetAnalyticsToolsYearRange$' -benchtime=10x -count=3 -benchmem`.

## Fresh-sync text validation

A local Linux comparison against commit `6bd839629` used the same copied
Claude/Codex corpus, an empty isolated archive per run, warmed source files, and
worker CPU/heap profiling. Unchanged controls took 143.216 and 143.145 seconds.
Skipping eight printable ASCII bytes per sanitizer check took 133.896 and
126.578 seconds, about 9.0% less time on average. Sampled peak anonymous memory
was 813–822 MB for the controls and 776–795 MB for the candidate. These are
local incremental measurements, not a comparison with the current default branch
or proof that the external regression is resolved.

The fast path skips only printable ASCII. Unicode, controls, invalid UTF-8, and
short tails retain the existing scalar validation and repair behavior. The
transcript sanitizer microbenchmark fell from about 9.49 to 3.31 microseconds
without allocations. A standard-library alternative measured 4.26 microseconds
and 136.672–138.691 seconds on the same full corpus, so the block scan is
retained despite its less obvious byte arithmetic.

Archive session upserts also reuse registry-derived preserved columns and
conflict SQL. Three fixed-iteration samples reduced allocation from 391–394 KB
and 918–920 allocations per message-batch operation to about 376 KB and 901–902
allocations. Timing was noisy and does not establish a separate speed
improvement. Reducing statement payload limits from 1 MiB to 256 or 64 KiB did
not improve the tested writes consistently and increased short-message cost; the
1 MiB limit remains.

The combined block scan and cached session SQL completed a final local run in
126.981 seconds, 11.3% below the unchanged control average, with 799 MB sampled
peak anonymous memory. All runs imported 13,967 sessions and reported 3,954
sanitized fields. External BenchDB results for the preceding commit still
measured fresh sync at 433.982 seconds against 398.332 seconds on its latest
default baseline, an 8.95% regression. The combined change requires a new
external run before claiming that gap is closed.

Sorted SHA-256 comparisons of stored message text, tool-call payloads, and
tool-result payloads and raw digests matched the unchanged control: 816,220
messages, 628,068 tool calls, and 567,881 result events. Full database, sync,
DuckDB, and PostgreSQL unit suites and focused PostgreSQL sanitization and
curation integration tests passed, along with formatting, vet, and CI lint.
