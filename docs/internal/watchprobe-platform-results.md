# Returned watcher diagnostics

The Go tester ran on Linux, macOS/APFS, and Windows/NTFS. Every recorded
scenario check passed. The results support fresh, bounded source observation,
but do not qualify the proposed production coordinator. In particular, the macOS
sustained run lost most native notices and recovered only at shutdown.

Mac and Windows reports identify clean build
`fd06b89e720c5cf386712c85c91b52f83c33758d`, Go 1.27.0 and 1.27.1 respectively.
Linux used the same tester source. Returned raw bundles and profiles remain
private; this document records aggregates. The two remote hosts had substantial
concurrent application activity. Neither report includes a human assessment of
desktop responsiveness.

## Full-size scan and resource costs

Each full run used 50,000 files in 196 directories. Native configurations
allocated 64 directory watches, leaving 132 units to coverage. Warm times below
are averages of three complete passes. Uncached timing uses the same run and
files, with only persistence temporarily disabled.

| Run                               | Cold scan s | Warm scan s/pass | Uncached scan s | Coverage s/pass, 33,616 files | Peak Go heap MiB | Post-GC heap MiB | Peak RSS MiB |
| --------------------------------- | ----------: | ---------------: | --------------: | ----------------------------: | ---------------: | ---------------: | -----------: |
| Linux core                        |       0.358 |            0.261 |           0.085 |                         0.184 |              8.7 |              2.2 |         40.5 |
| macOS, ordinary heavy activity    |       0.468 |            0.252 |           0.131 |               0.169 sustained |             20.6 |             10.6 |         59.8 |
| Windows, ordinary activity        |       4.059 |            2.370 |           2.068 |               1.574 sustained |             12.4 |              6.5 |         91.4 |
| Windows, synthetic competing load |       6.868 |            3.248 |           2.807 |               1.858 sustained |             12.5 |              6.6 |         54.0 |

All warm passes wrote zero cache rows and read zero content bytes. Persistence
added metadata lookup overhead on every platform. A single-directory cache
reopen is not a full application restart. No returned run establishes saved
parsing, archive work, or startup latency sufficient to justify persistence.
Keep the coordinator functional without a persistent cache.

Windows competing load used eight CPU workers with 1 GiB of buffers in total,
plus a bounded scratch disk worker. Host CPU averaged 52.4% busy. This was a
separate synthetic follow-up, not a substitute for the ordinary workload.
Scheduling-proxy p95 was about 15 ms before and during the run; small-file
round-trip p99 rose from 11.5 ms to 38.7 ms. That proxy does not measure the
user's desktop and does not establish which process caused the difference.

Windows ordinary/synthetic runs used 108.3/128.8 process CPU seconds through the
final snapshot. Mac used 16.1 seconds. CPU profiles locate sampled stacks; the
Windows synthetic CPU profile totals 462.9 sampled seconds, which differs
substantially from the process counter. Do not substitute that profile total or
its flat function percentages for measured process CPU usage.

## Native loss and latency

| Sustained run                              | Native received | Dropped | Serviced notices | End-of-run recovery          | Mean receipt-to-verification | Mean uncovered-mutation-to-coverage |
| ------------------------------------------ | --------------: | ------: | ---------------: | ---------------------------- | ---------------------------: | ----------------------------------: |
| Linux, 35 s window                         |              57 |       0 |               34 | None                         |             See local report |                    See local report |
| macOS, 300 s window                        |          15,880 |  10,504 |            5,079 | 57 directories, 14,592 files |                       483 ms |                              5.06 s |
| Windows, 300 s window                      |             614 |       0 |              314 | None                         |                       163 ms |                              6.86 s |
| Windows under synthetic load, 300 s window |             592 |       0 |              325 | None                         |                       476 ms |                              7.10 s |

The Mac queue reached its 128-entry limit and dropped 66% of received notices.
The tester drains on a one-second activity tick and between scan pages. It marks
losses immediately but performs forced recovery at the end. Completing forced
recovery does not demonstrate acceptable recovery latency. Scenario checks were
populated earlier; the recovery loop hashes files but does not compare every
digest with an expected value. Completion-based coverage also adds scan duration
to the configured five-second interval. A Windows full pass cannot be treated as
negligible work.

The fsnotify 1.10.1 kqueue source expands watched directories into file watches.
The 64 synthetic directory watches therefore include 16,384 initial file
watches. The returned Mac heap profile attributes an estimated 9.2 MiB
cumulatively to kqueue Add, about 48% of its sampled in-use total. Profile
allocation estimates and exact MemStats heap are different measurements. The Mac
report observed roughly 52-53 MB of physical footprint/dirty memory. Do not
replace physical footprint with RSS or sampled Go heap.

External file readers or attribute updates are hypotheses for the Mac burst. The
old tester omitted event operation bits, so it cannot identify the cause. Do not
filter attribute notices or increase the queue solely on that hypothesis. Darwin
kqueue results do not qualify the production FSEvents backend.

## What changes in qualification

1. Report operation counts before admission, including dropped notices, and
   distinguish content verification with equal versus changed signatures.
   Sample backlog and the oldest pending loss so bursts remain observable.
1. Record process descriptors on Linux/Mac and process handles on Windows.
   Logical directory allocation counts are not physical resource bounds.
1. Report zero-duration observations explicitly. Windows v2 reports contain many
   1 ns samples because the tester clamped zero elapsed time to one. Such
   percentiles are below clock resolution. Use phase totals for costs.
1. Repeat the Mac run with v3 telemetry on the same normally indexed scratch
   filesystem. Record operation mix and timing without changing indexing. If
   needed, compare separately authorized scratch locations to discriminate the
   external-reader hypothesis. Qualify FSEvents separately.
1. Before production cutover, service notices from a wakeup independently of
   coverage timers and perform scoped recovery during operation. Verify owner
   fairness and cancellation between pages under Windows competing load. Bound
   and measure oldest dirty/loss age, not only eventual final correctness.

The v3 tester adds these diagnostics while retaining v2 scheduling so returned
runs remain comparable. It does not claim to fix the Mac burst, choose a cache
format, or implement the production coordinator. Multi-hour retention, real
parser/archive costs, eviction/churn, total auxiliary-file disk bounds, and
native FSEvents remain open qualification work.

## Report identity

The private raw sustained `report.json` files have these SHA-256 digests:

| Report                 | SHA-256                                                            |
| ---------------------- | ------------------------------------------------------------------ |
| macOS                  | `ef579aa02a862429bc2ba288f2b31c700e78794420b8fb10a05d91799495dbd0` |
| Windows ordinary       | `c46e28b3f0a18212c54e26c4509e7b1692ca868a2922528d7c5192b3c32ab50d` |
| Windows competing load | `25bf7c5c5a5abbf49c42c3518733e2a8f923647e7970809d4606ad8e191f391c` |

## Production scanner

The draft uses shared metadata scans for Claude transcripts, Codex rollouts, and
Codex title indexes. Native events still use the existing production
fsnotify/FSEvents backends. The scanner has no persistent database.

Each cached file costs one fixed 16-byte row: a 64-bit hash of its basename and
a 64-bit hash of its size, modification time, and file identity. Rows are sorted
per directory, and each directory path is stored once. Hashes use a per-process
seed and are never persisted. A basename collision only repeats dispatch; a
signature collision can hide one change with odds of 2^-64. The cache holds up
to 1,048,576 rows and 8 MiB of directory paths.

On Windows, size, write time, and identity come from one fresh file handle.
Directory listings can report stale sizes while a writer keeps the file open.
Scanner file and directory handles share delete access, so agents can rename or
remove sources and folders while a scan holds them.

Coverage runs 30 seconds after the previous pass completes. Scans read at most
256 entries at a time outside the native event loop. Changes enter the existing
serialized dispatcher; bounded scan pages bypass the native-event dispatch
floor. Successful archive acknowledgement advances signatures. Failed callbacks
let scans continue to other roots while watcher retries remain pending. Missing
roots defer reconciliation. Providers still supply complete discovery before
disappearance can affect archive state.

Dangling project links are skipped. An unreadable subtree keeps its cached rows,
marks the pass incomplete, and does not stop coverage of its siblings.

When the cache is full, the scanner cannot compare uncached files. It does not
resubmit them every pass. Instead, provider reconciliation of the affected scope
audits them every tenth pass, about every five minutes. Cached files keep
30-second coverage. Saturation is recomputed each pass; once the collection fits
again, one final reconciliation covers any uncached deletions.

Logs report scan duration, files examined, changed paths, retained rows,
directory bytes, saturation, and cumulative failures. `Watcher.SourceScanStats`
exposes the last completed pass. These diagnostics contain counts only.
Production Mac and Windows runs, sustained bursts, and multi-hour retention
remain qualification work.

### Scanner memory

`BenchmarkSourceScan50K` measures 50,000 synthetic files in 196 directories:

| Cache layout                                      | Retained bytes | Bytes per file | Unchanged pass |
| ------------------------------------------------- | -------------: | -------------: | -------------: |
| Per-file Go maps with basename strings (rejected) |      7,647,912 |            153 |          84 ms |
| Hashed 16-byte rows (current)                     |        857,432 |             17 |          77 ms |

At the 1,048,576-row limit, the current layout extrapolates to about 17 MiB.
Each pass still allocates about 21 MiB temporarily, mostly directory entries
returned by `ReadDir`.

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync -run '^$' \
  -bench '^BenchmarkSourceScan50K$' -benchtime=3x -benchmem
```

## Production parser and archive experiment

Central metadata coverage saves substantial archive work at every measured size,
including when the cache overflows.

### Linux measured results

Runs used Linux/amd64, Go 1.27.2, and an AMD Ryzen AI Max+ 395. Every old/new
comparison within a run uses the same binary and source files. Results are
medians of five passes, except the single 512-source burst. CPU seconds include
all threads in the tester process.

|  Files | Scenario    | Prior wall s | Scan wall s | Prior CPU s | Scan CPU s |
| -----: | ----------- | -----------: | ----------: | ----------: | ---------: |
| 70,000 | Unchanged   |       13.605 |       0.144 |      16.318 |      0.162 |
| 70,000 | 20 appends  |       13.937 |       0.451 |      16.648 |      0.481 |
| 70,000 | 512 appends |       20.934 |       7.768 |      24.026 |      8.219 |

For unchanged work, `4 * scan CPU / prior CPU` is 0.040: about 96% less routine
CPU after accounting for the scanner running four times as often. Every archive
assertion passed, as did the real Linux watcher append, with 0.511 seconds of
native mutation-to-archive latency. Scanner startup added 1.70 MB of forced-GC
retained heap, including warmed engine state. Twenty further change rounds added
none.

The capacity control holds the same 70,000 sources and archive constant. It
forces one scanner past its limit at 63,000 rows and compares it with a scanner
holding every row and with prior polling. Each scanner sample spans one full
ten-pass audit cycle, five minutes of scheduled coverage. Prior polling runs 2.5
times in that window. Medians of three rounds:

| Method                    | CPU s per five minutes | Relative to prior polling |
| ------------------------- | ---------------------: | ------------------------: |
| Scanner holding every row |                   1.59 |                      0.04 |
| Scanner past its limit    |                   9.92 |                      0.24 |
| Prior polling, 2.5 passes |                  41.56 |                      1.00 |

An uncached source's append reached SQLite through the overflow audit, and
deleting it set the archived source-missing timestamp.

An earlier policy resubmitted every uncached file and reconciled the whole scope
on every pass. At 70,000 files with a 65,536-row limit it cost about 2.1 times
prior polling, with 633 MiB of temporary allocations per unchanged pass. That
result is why overflow now audits on a slower cadence. Raising the limit alone
would only move that failure to a larger collection.

### Repeat the experiment

`TestSourceScanQualification` compares the shared scanner with the prior grouped
provider-root polling path. Both use the real Claude and Codex parsers and
separate disposable SQLite archives over the same synthetic sources. It never
reads local transcripts or a live archive. The test skips by default.

Build once, then run the two sizes sequentially without concurrent builds or
benchmarks:

```bash
CGO_ENABLED=1 go test -c -tags fts5 \
  -o /tmp/source-scan-qualification ./internal/sync
umask 077
for count in 50000 70000; do
  AGENTSVIEW_SOURCE_SCAN_QUALIFY=1 \
  AGENTSVIEW_SOURCE_SCAN_FILES="$count" \
  AGENTSVIEW_SOURCE_SCAN_REVISION="$(git rev-parse HEAD)" \
  AGENTSVIEW_SOURCE_SCAN_REPORT="/tmp/source-scan-$count.json" \
    /tmp/source-scan-qualification \
      -test.run '^TestSourceScanQualification$' -test.timeout 30m -test.v \
      > "/tmp/source-scan-$count.log" 2>&1 || break
done
```

The separate capacity control holds 70,000 sources and one archive constant. It
forces one scanner's row limit to 90% of the sources and alternates it with a
production-limit scanner over full ten-pass audit cycles, plus one prior polling
pass per round. Only the tester changes the limit.

After timing, it selects a source absent from the overflowed cache and checks
its append and deletion through SQLite. Heap deltas and native latency are
measured by the qualification run only, not by this CPU control.

```bash
AGENTSVIEW_SOURCE_SCAN_CAPACITY_CONTROL=1 \
AGENTSVIEW_SOURCE_SCAN_FILES=70000 \
AGENTSVIEW_SOURCE_SCAN_REVISION="$(git rev-parse HEAD)" \
AGENTSVIEW_SOURCE_SCAN_REPORT=/tmp/source-scan-capacity-control.json \
  /tmp/source-scan-qualification \
    -test.run '^TestSourceScanCapacityControl$' -test.timeout 30m -test.v \
    > /tmp/source-scan-capacity-control.log 2>&1
```

This Linux-only tester reports process CPU, elapsed time, allocated bytes,
forced-GC heap changes, cache saturation, and native mutation-to-archive
latency. Sample times use milliseconds; byte counters use bytes;
`ScannerStats.LastDuration` uses nanoseconds. Five unchanged passes alternate
method order. Five rounds change 20 sources; a separate burst changes 512.
Archive assertions cover every changed source, an atomic replacement with
unchanged size and mtime, a Codex title update, reported event-loss recovery for
an in-place equal-stat edit, and source disappearance. A selected source gets an
additional edit and deletion check, with its initial cache membership in the
report. When saturated, selection requires observed cache absence. Twenty change
rounds measure short-run heap retention. A real Linux watcher must then commit
an append to SQLite.

The decision requires correct archive results and cheaper routine coverage at
both sizes. Compare median CPU per pass after accounting for the 30-second
scanner interval and prior 120-second polling interval. A faster individual pass
can still fail if it runs four times as often. Saturation must not create a
sustained cost regression. Compare overflow cost per audit cycle with 2.5 prior
polling passes. The retained-heap delta includes engine state warmed by scanner
startup, rather than measuring only scanner objects.

The files contain short synthetic conversations, split equally between Claude
and Codex, in grouped directories. This experiment tests production code paths
and collection cardinality. It does not measure a busy desktop, long
transcripts, continuous native bursts, or multi-hour retention.
