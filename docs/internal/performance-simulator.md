# Performance simulator

`cmd/perfsim` creates synthetic Claude/Codex JSONL sources or an OpenCode SQLite
store and feeds them through the real parsers, SQLite archive, sync engine,
usage cache, and query implementations. It checks session/message totals after
ingest and appends so a parser failure cannot look like a performance
improvement.

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

# SQLite container scans, long active sessions, and analytical queries.
make perf-sim PERF_SIM_FLAGS='--source-format opencode --sessions 10000 --turns 20 --active-turns 1000 --iterations 5'

# Larger part payloads to exercise SQLite overflow pages.
make perf-sim PERF_SIM_FLAGS='--source-format opencode --sessions 1000 --message-bytes 16384 --iterations 5'
```

The default `--source-format jsonl` alternates between Claude and Codex.
`--active` selects how many of the first sessions receive a new user/assistant
pair per iteration, default two. `--active-turns` changes their initial length
without making every archived session long. `--message-bytes` controls
approximate content size. `--empty` adds empty Claude source files; current
parsers archive these as empty sessions, so they stress discovery and
bookkeeping without adding analytical messages.

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
after N completed update iterations, then after each additional N. For example,
N=3 runs after updates 3, 6, and 9. Each observation records the number of
completed updates when measurement began. Zero disables that repeated phase.
First-query warmup still runs before the steady-state CPU profile starts.

## Comparing results

The output directory contains:

- `results.json`: configuration, build revision and dirty state when available,
  executable SHA-256, Go version/platform, actual archive totals, and every
  operation's duration, allocated bytes, allocation count, and heap.
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

## OpenCode SQLite workload

`--source-format opencode` creates a separate producer database in WAL mode and
keeps its writer open while Agentsview reads it. Only this synthetic database
receives producer writes; the archive is populated through the normal provider.
The fixture includes the session, project, message, and part table columns and
indexes from the pinned upstream schema. It deliberately adds no timestamp
indexes that would make production scans look cheaper. `--empty` applies only to
JSONL and is rejected for this workload.

Each append inserts a user/assistant pair and text parts in a transaction,
advancing the session timestamp. The container path is then passed to the
engine, exercising its session-metadata scan and stored-freshness comparison.
Each active session also receives an in-place edit to the last assistant text
part. That edit advances only the part timestamp. The simulator polls the
session's virtual source, syncs it, and verifies that the archive contains the
edited text. Thus a session-row-only detector cannot silently pass the workload.

Additional observations distinguish the SQLite read paths:

- `warm_metadata_scan`: streamed session/project watermarks.
- `warm_full_digest_scan`: streamed metadata with child identity checks.
- `warm_session_poll`: per-session composite timestamp reads for active
  sessions.
- `warm_container_event`: an unchanged container notification through the
  engine.
- `active_session_poll`: the same active-session reads after child-only edits.
- `active_child_edit`: engine processing of those edited virtual sources.
- `recovery_closed_writer_event`: three container notifications after closing
  the producer writer, including a disappeared WAL sidecar.

Warm scans and unchanged container events follow `--reconcile-every`; active
polls and child edits run every iteration. Full digest scans are diagnostic
provider calls, separate from the engine's five-minute verification schedule.
They warm SQLite/filesystem caches, so compare equivalent workloads. Source
writes and parsed-content validation are outside operation timings but inside
the steady CPU profile. The normal analytical query suite runs for this format
too. Session/message count and literal usage/content assertions cover both
layouts in the normal Go suite.

This is the OpenCode `message`/`part` layout consumed by Agentsview, not an
OpenCode process or an exhaustive producer emulator. It omits the newer
`session_message` projection, tools, concurrent writers, and deletions. Schema
and write provenance are recorded in
[session format sources](session-format-sources.md).

## Generating sources for a real server

Use `--generate-only --keep-data --output-dir /tmp/new-corpus` with the normal
workload flags to retain producer files without running the in-process engine.
The new directory contains `sources.json` with source paths, provider roots,
workload options, and generator build provenance. Point a separately configured
branch server at those roots and a new archive directory. Disable all other
providers and update checks, build with telemetry disabled, and bind to
loopback. Do not use the installed server's config or archive.

For OpenCode, keep a SQLite writer open in WAL mode, append message/part rows,
and separately edit a part without advancing the session timestamp. Use current
timestamps for daemon activity tests. The generated June corpus is historical.
Open the active session's `/api/v1/sessions/{id}/watch` SSE stream, and verify
new content through `/api/v1/sessions/{id}/messages?direction=desc&limit=10`.
Record commit-to-visible latency separately from engine batch duration.

A real server launches sync-worker subprocesses. Capture parent and worker CPU
and memory separately; parent `/debug/pprof` profiles omit worker execution.
Record forced-GC heaps and macOS `vmmap -summary` alongside RSS. Keep raw
captures in a durable output directory, not only an operating-system temporary
directory.

`make perf-sim` enables VCS build metadata. Direct builds should use
`go build -buildvcs=true` or `go run -buildvcs=true`. Missing revision metadata
means unknown, not a clean build. Build overlays are not described by VCS
metadata; use `--provenance-note` to record the overlay and retain its patch
with the results. Executable hashes identify binaries but cannot reconstruct
them.

## Sidecar events and deleted members

For OpenCode, Kilo, MiMoCode, and Icodemate SQLite containers, disappearance of
WAL/SHM files beside an existing database is housekeeping, not evidence that
sessions disappeared. A database/sidecar changed-path batch processes surviving
sources. It does not promise immediate `source_missing_at` updates for sessions
deleted inside that database. The next successful authoritative reconciliation
of the provider root or container detects those deletions and retains the
archived messages. An explicitly missing virtual-member event still takes the
member absence path. Removal of the database itself still takes source-missing
reconciliation when the database removal event is processed.

The sidecar scaling regression compares 8 and 800 OpenCode sessions for both
suffixes. Separate two-session cases cover all four provider layouts: they
delete a member, close its writer, process the database/WAL/SHM event batch, and
verify the eventual missing-source state after authoritative reconciliation.
This defines consistency, not a fixed time bound: reconciliation must
successfully run before that state changes.
