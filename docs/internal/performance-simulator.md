# Performance simulator

`cmd/perfsim` creates synthetic Claude and Codex JSONL sources and feeds them
through the real parsers, SQLite archive, sync engine, usage cache, and query
implementations. It checks session/message totals after ingest and appends so a
parser failure cannot look like a performance improvement.

Run the default workload:

```bash
make perf-sim
```

The command creates a private temporary output directory and prints its path. It
removes synthetic source/database files on completion but keeps measurements and
profiles. `--keep-data` retains those files for query inspection. `--output-dir`
must name a new directory; the command refuses an existing one. It does not load
the user's app configuration or run against a live archive.

## Workloads

```bash
# Small archive with steady updates and analytics after each update.
make perf-sim PERF_SIM_FLAGS='--sessions 100 --turns 20 --iterations 10'

# Larger archive; the active sessions have much longer histories.
make perf-sim PERF_SIM_FLAGS='--sessions 10000 --turns 20 --active-turns 1000 --iterations 10'

# Isolate append work from periodic full reconciliation and analytics.
make perf-sim PERF_SIM_FLAGS='--sessions 1000 --empty 10000 --iterations 25 --reconcile-every 0 --query-every 0'
```

Sessions alternate between Claude and Codex. `--active` selects how many of the
first sessions receive a new user/assistant pair per iteration, default two.
`--active-turns` changes their initial length without making every archived
session long. `--message-bytes` controls approximate content size. `--empty`
adds empty Claude source files; current parsers archive these as empty sessions,
so they stress discovery and bookkeeping without adding analytical messages.

The corpus uses fixed dates across June 2026, unique session/message/request
IDs, and usage-bearing assistant turns. It does not contact model providers. The
default query window covers that month. Extremely long sessions may extend
outside it, just as real sessions can extend beyond a query window.

Each run records:

- Cold ingest and first execution of each query.
- Warm full reconciliation, requiring zero rewritten sessions.
- Changed-path sync after appending to the active sessions.
- Stats, sidebar index, analytics summary, daily activity, daily token usage,
  and full-text search after each update.

Use `--reconcile-every N` and `--query-every N` to schedule those operations
once per N update iterations. Zero disables that repeated phase. First-query
warmup still runs before the steady-state CPU profile starts.

## Comparing results

The output directory contains:

- `results.json`: configuration, Go version/platform, actual archive totals, and
  every operation's duration, allocated bytes, allocation count, and heap.
- `bench.txt`: non-cold observations in Go benchmark format.
- `steady.cpu.pprof`: CPU samples collected after ingest and first-query warmup.

Use identical flags, source revision except for the measured change, toolchain,
and machine for before/after runs. Avoid simultaneous benchmark runs. Collect at
least five iterations. Appends grow the active sessions, so iteration counts
must match. Compare cold and steady measurements separately.

```bash
go tool pprof -top /tmp/run-after/steady.cpu.pprof
go run ./cmd/benchgate -alloc-floor 1e18 \
  -old /tmp/run-before/bench.txt -new /tmp/run-after/bench.txt
```

The comparison command disables the allocation-count gate because it compares
the worst candidate sample with the baseline median. Process-wide simulator
counts include asynchronous signal work and can fail that rule even when a file
is compared with itself. Time and allocated-byte gates still compare medians
with a significance check. Keep the default allocation-count gate for focused
benchmarks with stable work.

The simulator reports process-wide allocations, including asynchronous signal
work. These samples can vary; use them to find expensive paths, then add a
focused benchmark or work-count regression for a confirmed fix. The regular
suite checks the simulator's parsed output and usage totals. It does not enforce
machine-dependent latency thresholds.

## Limits

This measures completed engine batches and direct store queries. It excludes
filesystem watcher debounce, HTTP serialization, frontend rendering, network
latency, and PostgreSQL. An unchanged reconciliation is a scheduled pass, not a
measurement of a daemon sleeping with no events. CPU profiles include corpus
append generation as well as sync; per-operation timings exclude generation. The
corpus currently has no tool calls, forks, subagents, or malformed records. Use
real-data clones or additional producer fixtures for those cases. Query content
deliberately contains repeated search terms, stressing common-term FTS.
