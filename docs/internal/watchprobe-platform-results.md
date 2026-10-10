# Returned watcher diagnostics

The Go tester ran on Linux, macOS/APFS, and Windows/NTFS. Every recorded
scenario check passed. The results support fresh, bounded source observation,
but do not qualify the proposed production coordinator. In particular, the
macOS sustained run lost most native notices and recovered only at shutdown.

Mac and Windows reports identify clean build `fd06b89e720c5cf386712c85c91b52f83c33758d`,
Go 1.27.0 and 1.27.1 respectively. Linux used the same tester source. Returned
raw bundles and profiles remain private; this document records aggregates.
The two remote hosts had substantial concurrent application activity. Neither
report includes a human assessment of desktop responsiveness.

## Full-size scan and resource costs

Each full run used 50,000 files in 196 directories. Native configurations
allocated 64 directory watches, leaving 132 units to coverage. Warm times below
are averages of three complete passes. Uncached timing uses the same run and
files, with only persistence temporarily disabled.

| Run | Cold scan s | Warm scan s/pass | Uncached scan s | Coverage s/pass, 33,616 files | Peak Go heap MiB | Post-GC heap MiB | Peak RSS MiB |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| Linux core | 0.358 | 0.261 | 0.085 | 0.184 | 8.7 | 2.2 | 40.5 |
| macOS, ordinary heavy activity | 0.468 | 0.252 | 0.131 | 0.169 sustained | 20.6 | 10.6 | 59.8 |
| Windows, ordinary activity | 4.059 | 2.370 | 2.068 | 1.574 sustained | 12.4 | 6.5 | 91.4 |
| Windows, synthetic competing load | 6.868 | 3.248 | 2.807 | 1.858 sustained | 12.5 | 6.6 | 54.0 |

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

Windows ordinary/synthetic runs used 108.3/128.8 process CPU seconds through
the final snapshot. Mac used 16.1 seconds. CPU profiles locate sampled stacks;
the Windows synthetic CPU profile totals 462.9 sampled seconds, which differs
substantially from the process counter. Do not substitute that profile total
or its flat function percentages for measured process CPU usage.

## Native loss and latency

| Sustained run | Native received | Dropped | Serviced notices | End-of-run recovery | Mean receipt-to-verification | Mean uncovered-mutation-to-coverage |
| --- | ---: | ---: | ---: | --- | ---: | ---: |
| Linux, 35 s window | 57 | 0 | 34 | None | See local report | See local report |
| macOS, 300 s window | 15,880 | 10,504 | 5,079 | 57 directories, 14,592 files | 483 ms | 5.06 s |
| Windows, 300 s window | 614 | 0 | 314 | None | 163 ms | 6.86 s |
| Windows under synthetic load, 300 s window | 592 | 0 | 325 | None | 476 ms | 7.10 s |

The Mac queue reached its 128-entry limit and dropped 66% of received notices.
The tester drains on a one-second activity tick and between scan pages. It
marks losses immediately but performs forced recovery at the end. Completing forced recovery does not demonstrate acceptable recovery latency.
Scenario checks were populated earlier; the recovery loop hashes files but
does not compare every digest with an expected value. Completion-based coverage also adds scan duration to the configured
five-second interval. A Windows full pass cannot be treated as negligible work.

The fsnotify 1.10.1 kqueue source expands watched directories into file watches.
The 64 synthetic directory watches therefore include 16,384 initial file
watches. The returned Mac heap profile attributes an estimated 9.2 MiB
cumulatively to kqueue Add, about 48% of its sampled in-use total. Profile
allocation estimates and exact MemStats heap are different measurements.
The Mac report observed roughly 52-53 MB of physical footprint/dirty memory.
Do not replace physical footprint with RSS or sampled Go heap.

External file readers or attribute updates are hypotheses for the Mac burst.
The old tester omitted event operation bits, so it cannot identify the cause.
Do not filter attribute notices or increase the queue solely on that hypothesis.
Darwin kqueue results do not qualify the production FSEvents backend.

## What changes in qualification

1. Report operation counts before admission, including dropped notices, and
   distinguish content verification with equal versus changed signatures.
   Sample backlog and the oldest pending loss so bursts remain observable.
2. Record process descriptors on Linux/Mac and process handles on Windows.
   Logical directory allocation counts are not physical resource bounds.
3. Report zero-duration observations explicitly. Windows v2 reports contain
   many 1 ns samples because the tester clamped zero elapsed time to one.
   Such percentiles are below clock resolution. Use phase totals for costs.
4. Repeat the Mac run with v3 telemetry on the same normally indexed scratch
   filesystem. Record operation mix and timing without changing indexing.
   If needed, compare separately authorized scratch locations to discriminate
   the external-reader hypothesis. Qualify FSEvents separately.
5. Before production cutover, service notices from a wakeup independently of
   coverage timers and perform scoped recovery during operation. Verify
   owner fairness and cancellation between pages under Windows competing load.
   Bound and measure oldest dirty/loss age, not only eventual final correctness.

The v3 tester adds these diagnostics while retaining v2 scheduling so returned
runs remain comparable. It does not claim to fix the Mac burst, choose a cache
format, or implement the production coordinator. Multi-hour retention, real
parser/archive costs, eviction/churn, total auxiliary-file disk bounds, and
native FSEvents remain open qualification work.

## Report identity

The private raw sustained `report.json` files have these SHA-256 digests:

| Report | SHA-256 |
| --- | --- |
| macOS | `ef579aa02a862429bc2ba288f2b31c700e78794420b8fb10a05d91799495dbd0` |
| Windows ordinary | `c46e28b3f0a18212c54e26c4509e7b1692ca868a2922528d7c5192b3c32ab50d` |
| Windows competing load | `25bf7c5c5a5abbf49c42c3518733e2a8f923647e7970809d4606ad8e191f391c` |

## First production scanner measurement

The draft now uses shared metadata scans for Claude transcripts, Codex rollouts,
and Codex title indexes. Native events still use the existing production
fsnotify/FSEvents backends. The scanner has no persistent database. It retains
filenames under shared directory prefixes, with limits of 65,536 signatures and
8 MiB of name strings. Saturation keeps memory bounded but causes authoritative
reconciliation of affected scopes on subsequent passes, so it can cost more
archive work.

A Linux synthetic measurement of 50,000 files in 196 directories recorded an
unchanged pass around 86 ms, about 7.3 MiB of retained Go heap, and 0.92 MiB of
retained name strings. Each pass allocated about 27.4 MiB temporarily. These
numbers cover metadata observation, not parser/archive work or desktop impact.
Repeat the measurement with:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/sync -run '^$' \
  -bench '^BenchmarkSourceScan50K$' -benchtime=3x -benchmem
```

Coverage runs 30 seconds after the previous pass completes. Scans read at most
256 entries at a time outside the native event loop. Changes enter the existing
serialized dispatcher; bounded scan pages bypass the native-event dispatch
floor. Successful archive acknowledgement advances signatures.
Failed callbacks let scans continue to other roots while watcher retries remain
pending. Missing roots defer reconciliation. Providers still supply complete
discovery before disappearance can affect archive state.

Logs report scan duration, files examined, changed paths, retained signatures,
name bytes, saturation, and cumulative failures. `Watcher.SourceScanStats`
exposes the last completed pass. These diagnostics contain counts only.
Production Mac and Windows runs, sustained bursts, and multi-hour retention
remain qualification work. Metadata timings alone do not establish an archive
ingestion advantage; the following experiment measures those code paths.

## Production parser and archive experiment

Do not ship the current saturation policy. Central metadata coverage saves
substantial archive work below its signature limit. Above that limit, repeated
whole-root reconciliation makes the configured schedule more expensive than
prior polling. Keep the draft open to change that policy.

### Linux measured results

Both runs used Linux/amd64, Go 1.27.0, and an AMD Ryzen AI Max+ 395. The
50,000-file run tested `cb674e4f` with the experiment added. The worktree was then
updated to the already-rebased PR head, `9dd584e8`, for the 70,000-file run.
Every old/new comparison within a run uses the same binary and source files.
Results below are medians of five passes, except the single 512-source burst.
CPU seconds include all threads in the tester process.

| Files | Scenario | Prior wall s | Scan wall s | Prior CPU s | Scan CPU s |
| ---: | --- | ---: | ---: | ---: | ---: |
| 50,000 | Unchanged | 8.233 | 0.104 | 10.201 | 0.120 |
| 50,000 | 20 appends | 8.653 | 0.371 | 10.579 | 0.405 |
| 50,000 | 512 appends | 13.933 | 6.127 | 16.175 | 6.518 |
| 70,000 | Unchanged | 13.978 | 8.160 | 16.677 | 9.901 |
| 70,000 | 20 appends | 14.380 | 8.565 | 17.140 | 10.244 |
| 70,000 | 512 appends | 21.748 | 16.090 | 24.945 | 18.105 |

For unchanged work, `4 * scan CPU / prior CPU` is 0.047 at 50,000 files and
2.375 at 70,000. Including pass duration in both completion-based intervals,
the ratios are 0.050 and 2.084. The smaller collection uses about 95% less
routine CPU; the saturated collection uses about twice as much. At 70,000,
each unchanged scanner pass allocates a median 633 MiB temporarily.

Both qualification runs passed their archive assertions and the real Linux
watcher append. Scanner startup added 13.044 seconds and 8.98 MiB of forced-GC
retained heap at 50,000 files, and 21.531 seconds and 11.67 MiB at 70,000.
Those heap deltas include warmed engine state. After twenty change rounds,
additional forced-GC heap was 0.186 MiB and 0.020 MiB respectively. Native
mutation-to-archive latency was 0.720 and 0.511 seconds. These short runs do not
establish multi-hour retention or native-burst fairness.

The original 70,000-file run reported that its final-source probe was cached.
Filesystem directory order did not put that source beyond the limit. The
tester now selects a probe from observed cache absence when saturated, and the
separate capacity control exercises that corrected selection.

The 70,000-file capacity control used the same `9dd584e8` production code, one
archive, and unchanged sources. Five passes per scanner alternated order.
The production limit took a median 8.164 seconds and 9.833 CPU-seconds per
pass. Expanding only the tester's signature capacity to 70,000 reduced that to
0.144 seconds and 0.167 CPU-seconds, about 59 times less CPU. Its complete cache
held 70,000 signatures and did not saturate. This isolates saturation from file
count or archive contents as the cause of the cost jump.

The control then selected a source absent from the production-capacity cache.
Its append reached SQLite, and deleting it set the archived source-missing
timestamp. Both assertions passed. The fallback preserves these tested
results, but pays repeated full-root archive work to do so. The capacity
override is a diagnostic control, not a proposed production fix.

This is enough to reject the current cutover. Further Mac data cannot turn a
reproduced Linux cost regression into a passing release decision. A revised
policy must pass the same archive checks and cadence-adjusted CPU comparison
on both sides of the capacity limit. Increasing a fixed limit alone moves the
failure to a larger collection.

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

The separate capacity control holds 70,000 sources and one archive constant.
It alternates five unchanged passes with production capacity and five with
capacity expanded to retain all 70,000 signatures. Only the tester changes the
entry limit; the production defaults stay unchanged.

After timing, it selects a source absent from the production-capacity cache and
checks its append and deletion through SQLite. Heap deltas and native latency
are measured by the qualification run only, not by this CPU control.

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
Archive assertions cover every
changed source, an atomic replacement with unchanged size and mtime, a Codex
title update, reported event-loss recovery for an in-place equal-stat edit,
and source disappearance. A selected source gets an additional edit and
deletion check, with its initial cache membership in the report. When saturated,
selection requires observed cache absence. Twenty change rounds measure
short-run heap retention. A real Linux watcher must then commit
an append to SQLite.

The decision requires correct archive results and cheaper routine coverage at
both sizes. Compare median CPU per pass after accounting for the 30-second
scanner interval and prior 120-second polling interval. A faster individual
pass can still fail if it runs four times as often. Saturation must not create
a sustained cost regression. The retained-heap delta includes engine state
warmed by scanner startup, rather than measuring only scanner objects.

The files contain short synthetic conversations, split equally between Claude
and Codex, in grouped directories. This experiment tests production code paths
and collection cardinality. It does not measure a busy desktop, long
transcripts, continuous native bursts, or multi-hour retention.
