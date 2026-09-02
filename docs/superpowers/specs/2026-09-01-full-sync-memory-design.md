# Full-Sync Memory Bound

## Goal

Prevent an AgentsView full sync from putting a 16 GB machine under severe memory
pressure without materially slowing the sync. Keep parsing and archive output
identical. Treat process memory and wall-clock time as co-equal acceptance
criteria, and distinguish process memory from reclaimable filesystem cache.

## Evidence and root cause

AgentsView v0.42.0 installs a special parse-retention budget for whole-archive
passes. Unlike the bounded budget used by incremental work, the bulk budget has
no weighted semaphore. It admits every parse immediately. Up to eight workers
can therefore parse concurrently while the collector retains completed results
until it reaches the 100-session write batch.

The write path temporarily amplifies that retained data. The sync engine
converts parsed messages into database messages, then the database validation
boundary copies the message and usage-event slices before writing them. A
controlled 200,000-message probe measured about 131 MiB after parsing, 205 MiB
after preparing database rows, and a 537 MiB sampled heap peak during the
database write.

The collector also reused its pending slices with `pending = pending[:0]`
without clearing the pointer-bearing backing arrays. A completed batch therefore
remained reachable until a later batch overwrote every slot or the whole pass
returned. A behavior test reproduced that reachability while the collector was
waiting for its next result.

This is high transient memory during a pass, not a persistent Go heap leak.
Garbage-collector tracing on the released path held about 347 MiB live across
collections during a 600,000-message run. Its existing end-of-pass forced
collection then reduced the live heap to 1 MiB and eagerly returned about 688
MiB to the operating system.

A separate host diagnostic distinguished AgentsView memory from the benchmark
harness. The candidate process had not started, while the enclosing service had
reached 10.80 GB of file cache with only 61 MB of anonymous memory. Prewarming
the corpus inside the measured service charged reclaimable page cache to its
cgroup and caused the original machine-wide pressure. Later measurements
therefore prewarmed outside the candidate service and recorded process heap,
resident set size (RSS), target anonymous memory, and file cache separately.

The complete v0.42.0 workload finished its fresh-sync phase in 531.493 seconds,
then completed its analytics. Its 8.43 GiB cgroup peak failed the harness's 8
GiB gate even though the worker's anonymous-memory peak was below 1 GiB. The
remaining cgroup total was dominated by SQLite and source-file cache, so that
gate does not measure the AgentsView heap in isolation.

## Design

Use separate limits for the two ownership stages in an archive-scale pass:

- A 128 MiB weighted semaphore bounds active and queued parse results. A worker
  acquires a lease after skip and freshness checks but before parsing. The
  existing fixed allowance plus four-times-source-size estimate determines the
  weight. A source whose estimate reaches the limit runs exclusively.
- A 512 MiB byte estimate bounds completed parsed results owned by the bulk
  collector. When a result reaches the collector, the collector transfers it
  to the pending database batch and releases the parse lease. It flushes
  before adding a result that would exceed the pending limit, after adding one
  that reaches the limit, or when the existing 100-session batch is full.

Keeping the limits separate prevents database transaction size from depending on
active parser admission. An unusually large source runs alone in the parse stage
and is flushed alone in the pending stage. A source whose size cannot be
resolved counts as the full pending limit. Incremental work keeps the existing
lease-through-write behavior and pressure-triggered partial flushes because its
smaller budget protects a long-running daemon rather than a dedicated bulk pass.

After a flush or a cancelled write, clear the collector's pending writes,
leases, and cache-write entries before reslicing them for reuse. This removes
stale references as soon as the batch is complete and lets ordinary collections
reclaim parsed payloads during the pass.

Bulk work keeps its existing end-of-pass memory scavenge. Every parse-bearing
bulk pass requests one scavenge after all writes finish. A warm pass that parses
nothing does not force a scavenge. Cancellation and write failures continue to
release every acquired lease.

Both capacities remain internal constants. Do not add a user setting,
environment variable, fallback path, or version-specific behavior.

## Capacity and wall-clock evidence

Initial synthetic screening made a shared 64 MiB lease-through-write budget look
promising: it cut anonymous memory by 85% on a 120-source cold archive and added
0.17% wall time. The production-scale corpus was heavy-tailed, however, and
exposed a coupling that the uniform fixture did not. Shared admission pressure
forced 11,680 small database transactions instead of 555 and increased the main
parse/write phase by 56%. Shared 256 MiB and 512 MiB variants still created
2,525 and 1,144 transactions and were 13.9% and 10.3% slower. Those variants
were rejected rather than weakening the wall-clock gate.

The separate-budget candidate kept normal transaction batching. The final run
wrote 608 batches averaging 91.2 sessions, compared with 555 batches averaging
100 sessions in both bracketing v0.42.0 runs. The write phase matched the faster
baseline at about 3 minutes 52 seconds.

The following aggregate measurements came from the same isolated full-sync
workload on a generic 16 GB Linux machine. Host I/O varied substantially, so the
candidate is compared with bracketing baseline runs rather than only the
adjacent run.

| Measurement           | v0.42.0 A |   v0.42.0 B | Candidate |
| --------------------- | --------: | ----------: | --------: |
| Worker elapsed        | 530.861 s |   603.099 s | 569.777 s |
| Peak Go heap          | 735.3 MiB |   684.0 MiB | 617.5 MiB |
| Peak worker RSS       | 978.3 MiB | 1,017.3 MiB | 815.6 MiB |
| Peak worker anonymous | 925.3 MiB |   964.7 MiB | 762.4 MiB |
| Peak target anonymous | 978.3 MiB | 1,012.8 MiB | 812.5 MiB |
| Final Go heap         |  71.8 MiB |    75.5 MiB |  52.0 MiB |
| Target cgroup total   |  8.25 GiB |    8.42 GiB |  9.19 GiB |

Against the midpoint of the bracketing baselines, elapsed time changed by
+0.49%, peak heap fell 13.0%, peak worker RSS fell 18.3%, and peak worker
anonymous memory fell 19.3%. The candidate completed and sealed the fresh
archive successfully. Its higher total cgroup peak was file cache, not anonymous
process memory; this change does not claim to make the harness's current 8 GiB
total-memory gate pass.

The residual peak is dominated by the largest single source because parsers
materialize a whole source before returning it. Streaming parser output could
lower that floor, but it would require a larger parser and transaction redesign.
The chosen limits keep the measured AgentsView process below 1 GiB on the 16 GB
host without that broader semantic change.

## Alternatives considered

A Go soft memory limit reacts after allocation. The reproduced run reduced
resident memory but made the run nearly four times slower because the garbage
collector ran continuously.

Reducing worker count or the 100-session batch size would bound counts, not
bytes. Different transcript sizes would still produce different peaks, and
either change would give up parallelism or transaction efficiency for small
sources.

A single lease-through-write budget is simpler, but the production-scale run
showed that it converts parser backpressure directly into small SQLite
transactions. Separate ownership-stage limits preserve both bounds without that
coupling.

## Verification

Behavior tests cover weighted bulk admission, exclusive oversized sources,
conservative unknown-size accounting, independence between bulk parser admission
and write batching, the pending-result byte limit, prompt release of collector
backing arrays, end-of-pass scavenging, warm no-op passes, and cancellation.
Wall-clock assertions stay out of unit tests and come from the isolated
full-sync measurements above.

Keep transcript content, source paths, stable identifiers, profiles, and scratch
databases on the measurement machine. Only aggregate timing and memory
measurements may enter public artifacts.
