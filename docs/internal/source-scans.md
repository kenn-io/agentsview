# Claude and Codex source scans

The watcher scans Claude transcripts, Codex rollouts, and Codex title indexes
for metadata changes. These scans back up native fsnotify/FSEvents watching,
which can miss events or run out of watch slots. They replace whole-root
provider polling for those scopes. Other providers keep their polling. The
scanner has no persistent database.

## How the scanner works

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

An unreadable subtree or a dangling Claude project link keeps its cached rows,
marks the pass incomplete, and does not stop coverage of its siblings. The
scanner does not request reconciliation for it, because provider discovery also
reports an unavailable project as incomplete rather than deleted.

When the cache is full, the scanner cannot compare uncached files. It does not
resubmit them every pass. Instead, provider reconciliation of the affected scope
audits them every tenth pass, about every five minutes. Cached files keep
30-second coverage. Saturation is recomputed each pass; once the collection fits
again, one final reconciliation covers any uncached deletions. That
reconciliation is owed from the moment a source is left uncached, even if the
pass then fails.

Logs report scan duration, files examined, changed paths, retained rows,
directory bytes, saturation, and cumulative failures. `Watcher.SourceScanStats`
exposes the last completed pass. These diagnostics contain counts only.
Production Mac and Windows runs, sustained bursts, and multi-hour retention
remain qualification work.

## Scanner memory

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

## Parser and archive experiment

Shared metadata coverage saves substantial archive work at every measured size,
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
