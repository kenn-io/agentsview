# Steady-state performance measurements

Measurements collected on macOS arm64 with Go 1.27.0 and SQLite FTS5. These are
local observations, not portable latency targets. Live logs and a native process
sample guided the investigation; branch code ran on isolated database and source
clones or synthetic data. No collector was needed for these runs.

## Measured changes

| Operation                        |       Before |                  After | Workload                                            |
| -------------------------------- | -----------: | ---------------------: | --------------------------------------------------- |
| SQLite sidebar stats             |     27.99 ms |               10.16 ms | Local archive snapshot                              |
| PostgreSQL sidebar stats         |      11.1 ms |                 4.7 ms | Anonymized session metadata                         |
| Codex source discovery           |  about 90 ms |            about 56 ms | 12,656 cloned source files, warm filesystem         |
| Changed-path append batch        |     12.60 ms |                5.72 ms | 1,000 sessions, 10,000 empty sources, 25 iterations |
| Unchanged skip-cache persistence | about 8.7 ms | about 0.3 microseconds | Focused benchmark, 10,000 cached entries            |

Stats now calculate five totals in one filtered aggregate. Codex discovery
reuses directory-entry file types, avoiding a repeated stat per source. Its
allocation dropped from about 16.4 MB to 11.4 MB per discovery pass.

Claude duplicate resolution probes the requested transcript filenames in each
project instead of enumerating unrelated transcripts. Append allocation fell
from 11.14 MB to 0.266 MB per batch (33,912 to 2,091 allocations). Nested
subagent candidates still require traversal. The regression test compares 8 with
8,000 unrelated files, checks duplicate selection and deletion, and fails
against the previous implementation.

An unchanged skip cache no longer copies its map and replaces all persistent
rows. The focused benchmark went from about 60,059 allocations to one
assertion-related allocation. The simulator's empty files become empty sessions,
so they do not populate this cache; the append improvement above must not be
attributed to skip-cache persistence. A separate durable-cache regression checks
unchanged work, updates, deletion, and engine restart.

## Larger workload and remaining costs

Run this workload with the retained [simulator](performance-simulator.md):

```bash
make perf-sim PERF_SIM_FLAGS='--sessions 10000 --turns 20 --active-turns 1000 --iterations 5'
```

It starts with 403,920 messages. Two active sessions receive a new pair per
iteration. The following are medians after initial ingest and query warmup;
queries run after each append, so usage reads include refreshing changed data.

| Operation                     | Median duration | Median allocated memory |
| ----------------------------- | --------------: | ----------------------: |
| Stats                         |         3.63 ms |                1.68 KiB |
| Sidebar index                 |        43.69 ms |               15.71 MiB |
| Analytics summary             |       202.82 ms |                6.64 MiB |
| Daily activity                |       264.92 ms |               11.55 MiB |
| Daily usage                   |       435.70 ms |              152.34 MiB |
| Common-term full-text search  |       852.94 ms |                0.19 MiB |
| Unchanged full reconciliation |     1,232.65 ms |              159.46 MiB |
| Long-session append batch     |        90.64 ms |               33.97 MiB |

Allocation is process-wide work during each operation, not retained heap or
resident memory. The CPU profile spent about 60% of samples in C calls, so SQL
query plans or native profiling are needed to explain that work further. Search
uses a deliberately common term and is not representative of rare-term queries.
These observations identify further work; no optimization of these analytical
queries is claimed here.

The live daemon also repeatedly logged a malformed Harness source failure at
roughly seven-second intervals. That retry behavior remains unresolved. The
simulator currently excludes malformed records and watcher scheduling, so these
results do not establish an idle-daemon CPU reduction or a long-duration
memory-retention result. Add those workloads before using it to gate those
behaviors.
