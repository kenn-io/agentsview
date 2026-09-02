# Full-Sync Memory Bound

## Goal

Prevent an AgentsView full sync from putting a 16 GB machine under severe memory
pressure without materially slowing the sync. Keep parsing and archive output
identical. Treat peak memory and wall-clock time as co-equal acceptance
criteria.

## Evidence and root cause

AgentsView v0.42.0 installs a special parse-retention budget for whole-archive
passes. Unlike the bounded budget used by incremental work, the bulk budget has
no weighted semaphore. It admits every parse immediately. Up to eight workers
can therefore parse concurrently while the collector retains results until it
reaches the 100-session write batch.

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

This is high live memory during a pass, not a persistent post-pass Go heap leak.
Garbage-collector tracing on the released path held about 347 MiB live across
collections during a 600,000-message run. Its existing end-of-pass forced
collection then reduced the live heap to 1 MiB and eagerly returned about 688
MiB to the operating system.

A separate aborted host diagnostic also distinguished process memory from the
benchmark harness. The candidate process had not started, while the enclosing
service reached 10.80 GB of file cache with only 61 MB of anonymous memory.
Prewarming the corpus inside the measured service had charged reclaimable page
cache to its cgroup. Final target measurements therefore prewarm outside the
candidate service and report target anonymous memory separately from file cache.

Three alternating pairs on a generic 16 GB Linux machine produced these
synthetic medians. Each fixture generates its own data and validates the full
session count.

| Path and fixture            | v0.42.0 wall | Candidate wall | Wall delta | v0.42.0 cgroup | Candidate cgroup | v0.42.0 anonymous | Candidate anonymous |
| --------------------------- | -----------: | -------------: | ---------: | -------------: | ---------------: | ----------------: | ------------------: |
| Cold archive, 120 x 10,000  |     27.236 s |       27.283 s |     +0.17% |      1.905 GiB |        0.687 GiB |         1.388 GiB |           0.205 GiB |
| Resync ingest, 120 x 5,000  |     12.409 s |       12.607 s |     +1.60% |      1.409 GiB |        0.546 GiB |         1.003 GiB |           0.255 GiB |
| Resync ingest, 120 x 10,000 |     27.379 s |       29.202 s |     +6.66% |      2.827 GiB |        0.827 GiB |         2.059 GiB |           0.220 GiB |

A 256 MiB Go soft memory limit was also tested on the non-bulk ingest path. It
reduced resident memory but made the run nearly four times slower because the
garbage collector ran continuously. A soft memory limit is not the fix.

## Design

Keep a dedicated budget for archive-scale work, but make it byte-weighted like
the existing incremental budget. A worker acquires its lease after skip and
freshness checks but before parsing. The source size determines the weight,
using the existing fixed allowance and four-times-source-size estimate. A source
whose estimated weight reaches the budget runs exclusively.

The worker holds the lease through parsing, result queuing, and the database
write. When another worker waits for capacity, the collector flushes its current
pending writes even when it has not reached 100 sessions. Releasing those leases
admits the waiting parse. This bounds the retained pipeline by bytes instead of
session count while preserving parallelism for small sources.

After a flush or a cancelled write, clear the collector's pending writes,
leases, and cache-write entries before reslicing them for reuse. This removes
stale references as soon as the batch is complete and lets ordinary collections
reclaim its parsed payload during the pass.

Bulk work keeps its existing end-of-pass memory scavenge. Every parse-bearing
bulk pass requests one scavenge after all leases and writes finish. A warm pass
that parses nothing does not force a scavenge. Cancellation and write failures
continue to release every acquired lease.

The bulk capacity remains an internal constant. Do not add a user setting,
environment variable, fallback path, or version-specific behavior. The 64 MiB,
128 MiB, and 256 MiB candidates all stayed within 1% of each other on the cold
archive fixture. Their median cgroup peaks were 0.729 GiB, 0.997 GiB, and 1.292
GiB respectively before the stale-reference fix. Select 64 MiB because it has
the lowest memory peak without a material wall-clock cost.

## Alternatives considered

A Go soft memory limit reacts after allocation. The reproduced run showed
garbage-collection thrashing and an unacceptable wall-clock regression.

Reducing worker count or the 100-session batch size would bound counts, not
bytes. Different transcript sizes would still produce different and potentially
unsafe peaks. Either change would also give up parallelism for small sources.

Streaming every message directly into SQLite could further reduce the memory
needed for one exceptionally large transcript. It would require a larger parser
and transaction redesign. The weighted admission fix addresses the observed
multi-source retention problem without changing parsing or archive semantics.
One source can still exceed the estimate, but it runs without other parsed
sources competing for the same allowance.

## Verification

Add deterministic behavior tests for the contracts AgentsView owns:

- a full-sync pass installs a weighted bulk budget;
- two sources that each consume the budget cannot parse concurrently;
- admission pressure flushes a partial bulk batch before another parse starts;
- a flushed batch becomes collectible while the collector waits for more work;
- a parse-bearing bulk pass scavenges exactly once, while a warm pass does not;
- cancellation releases blocked workers and all retained capacity.

Keep wall-clock assertions out of unit tests. Use the existing
`BenchmarkResyncBulkIngest` benchmark because `agentsview sync --full` rebuilds
through that bulk path. Also run `BenchmarkSyncAllColdArchive` to protect the
whole-archive default path that shares the budget.

For each candidate capacity, alternate baseline and candidate runs on the
generic 16 GB machine with the same synthetic fixtures. Use at least three
measured runs per revision and compare medians. Reject a candidate if either
bulk fixture is more than 10% slower than v0.42.0. Among the surviving
candidates, choose the one with the lowest cgroup memory peak. The target for
the complete benchmark workload is below 8 GiB.

Measure cgroup peak, anonymous memory, and file-cache memory separately. Run a
final full benchmark on the authorized 16 GB machine. Keep transcript content,
source paths, stable identifiers, profiles, and scratch databases on that
machine. Only aggregate timing and memory measurements may enter the pull
request.
