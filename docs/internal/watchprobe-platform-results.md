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
remain qualification work. No end-to-end performance advantage is claimed yet.
