# Measure the proposed source watcher

`watchprobe` is a standalone diagnostic application. It creates synthetic
files, measures fresh file metadata and scoped native events, and reports
cache, CPU, memory, and latency costs. Run it while doing ordinary work to
measure behavior on a busy machine.

It does not read sessions, connect to an archive, or change the running
AgentsView service. The production coordinator remains proposed work.

## Build and run

Use Go 1.27 from the repository root. The diagnostic command uses the existing
pure-Go SQLite driver, so its portable build does not require CGO.

```sh
CGO_ENABLED=0 go build -trimpath -o watchprobe ./cmd/watchprobe
./watchprobe --files 50000 --passes 3 --duration 5m --profiles
```

The executable prints the artifact directory. An explicit `--output-dir` must
name a directory that does not exist. Use local storage, then repeat on another
filesystem if its behavior matters. The tester removes its synthetic source
files on normal completion or cancellation and leaves diagnostic artifacts.
A forced process kill can leave its own source directory behind.

Default scans use pages of at most 256 names, a native event queue of 128, and
64 directory watches. Directories contain at most 256 files. The main cache has
a 48 MiB page limit and an 8 MiB SQLite page cache. Refused cache admissions
remain unknown and are checked again. Paths share an integer directory key;
rows store basenames, preserving all 128 file identity bits and the volume.
There is no basename prefix codec in this experiment.

Compare the default run with these separate runs:

```sh
./watchprobe --files 50000 --passes 3 --cache-bytes 0 --max-watches 0
./watchprobe --files 50000 --passes 3 --name-pattern entropy
./watchprobe --files 1024 --passes 1 --cache-bytes 131072 --event-queue 1 --max-watches 1
```

For sustained coverage use `--duration 5m --scan-interval 5s`. Native activity
and an uncovered mutation occur once per second. Coverage of uncovered units
starts at the next tick after the previous pass plus the scan interval.
Default interval is 30 seconds. `--trace` records only the core scenarios;
`--profiles` records CPU through the sustained run and writes a post-GC heap.

## Read the output

- `report.json` contains build identity, workload, scenario checks, aggregated
  phase timings, operations, native watch failures and queue drops, latency
  histograms, sampled peak resources, and final/post-GC memory.
- `metrics.jsonl` contains phase records and resource samples every second and
  after each phase. Host CPU counters are cumulative. Compare their differences
  and available memory to relate tester costs to other running applications.
- `cpu.pprof`, `heap.pprof`, and optional `trace.out` work with `go tool pprof`
  and `go tool trace`. These are local diagnostics; there is no exporter.
- `cache.sqlite` is a disposable tester cache, retained for size inspection.

Warm scans still perform fresh metadata calls for every file. Unchanged scans
write no cache rows and read no contents. `uncached_scan` uses the same files
and metadata work without persistence. Cache reopen timing includes close,
open, and the first directory scan, rather than an application restart.

Explicit notices and loss recovery hash contents. Checks cover an open writer,
equal cached metadata, failed acknowledgement, same-size replacement, rename,
removal, creation, and reopening. Lifecycle checks use explicit notices.
`NativeObservedDuringCoverage` records real notices serviced during that pass;
a zero count means overlap was not observed. Injected retry and loss are
separate from native delivery. Actual queue loss triggers forced verification
of affected directories at the end of the run, so recovery latency during a
long sustained run is not yet modeled.

Latency percentiles are upper bounds from fixed logarithmic buckets. Page
payload bytes are an estimate of names plus serialized signatures, not
allocated memory. Heap, RSS, and queue high-water marks are sampled, so short peaks can be missed.
Cache auxiliary files are sampled after phases; the main-file cap is not a
hard total-disk peak guarantee. CPU phase deltas include resource collection;
wall times cover the phase work. A resource error count marks unavailable host
metrics.

## Platform limits

Linux uses inotify through fsnotify. Windows uses native file handles for fresh
metadata, including open-writer size and full identity. macOS uses fresh stat
calls and fsnotify's kqueue backend. The tester does not exercise the production
FSEvents bridge or Windows change journals. A build for another platform does
not establish its runtime behavior.

RSS includes reclaimable memory. On macOS collect `vmmap -summary PID` and
`footprint PID` during sustained execution when available. Record unavailable
commands without requesting elevated permissions. Report physical footprint
and dirty memory separately from Go heap.

This tester measures synthetic observation costs. It does not model provider
parsing, archive commits, source topology changes, recursive discovery,
tombstones, eviction under prolonged churn, or production scheduling fairness.
Those remain qualification work before a production cutover.

## Initial Linux results

A 50,000-file run with CPU/heap profiles and a core runtime trace completed in
2.14 seconds. All ten applicable scenario checks passed. Cold scanning took
358 ms; three warm passes took 784 ms combined, with zero cache writes or
content bytes. The equivalent uncached metadata pass took 85 ms. This SQLite
lookup implementation adds overhead; persistence needs measured downstream
restart benefits before adoption.

Sampled peak Go heap was 8.7 MiB, post-GC heap 2.2 MiB, and peak RSS 40.5 MiB.
Sampled cache main plus auxiliary disk was 10.3 MiB for repetitive basenames
and 6.9 MiB for entropy basenames. This format removes directory prefixes but
stores each basename. It does not settle the storage-format decision.

A separate 35-second sustained window with five-second coverage cadence
completed with ten checks passing, 34 native observations, no queue drops,
5.7 MiB sampled peak Go heap, and 2.5 MiB post-GC heap. The 128 KiB pressure
run dropped eight notices, refused cache admission, and verified all 256 files
in the affected directory. Its checks passed. These are individual synthetic
Linux observations, not cross-platform performance guarantees.
