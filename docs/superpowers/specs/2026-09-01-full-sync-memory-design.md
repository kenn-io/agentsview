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
reaches the 100-session write batch. The bulk writer then prepares database rows
from that retained data. The end-of-pass memory scavenge lowers the settled
footprint, but it runs too late to prevent the peak.

An isolated synthetic profile reproduced this behavior without using private
data. On a generic 16 GB Linux machine, the resync bulk-ingest path produced
these results:

| Workload                         | Wall time | Cgroup peak | Anonymous peak | File-cache peak |
| -------------------------------- | --------: | ----------: | -------------: | --------------: |
| 120 sessions, 600,000 messages   |    9.96 s |    1.46 GiB |       1.06 GiB |        0.40 GiB |
| 120 sessions, 1,200,000 messages |   22.57 s |    2.66 GiB |       1.89 GiB |        0.76 GiB |

Doubling the parsed messages nearly doubled the memory peak. Anonymous memory
was the largest component, so filesystem cache is not the primary cause. A 256
MiB Go soft memory limit was also tested on the non-bulk ingest path. It reduced
resident memory but made the run nearly four times slower because the garbage
collector ran continuously. A soft memory limit is not the fix.

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

Bulk work keeps its existing end-of-pass memory scavenge. Every parse-bearing
bulk pass requests one scavenge after all leases and writes finish. A warm pass
that parses nothing does not force a scavenge. Cancellation and write failures
continue to release every acquired lease.

The bulk capacity remains an internal constant. Do not add a user setting,
environment variable, fallback path, or version-specific behavior. Evaluate 64
MiB, 128 MiB, and 256 MiB capacities against the same workload and select the
lowest-memory candidate that passes the wall-clock gate. This keeps the
implementation focused while accounting for the cost of smaller bulk database
batches.

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
- a parse-bearing bulk pass scavenges exactly once, while a warm pass does not;
- cancellation releases blocked workers and all retained capacity.

Keep wall-clock assertions out of unit tests. Use the existing
`BenchmarkResyncBulkIngest` benchmark because `agentsview sync --full` rebuilds
through that bulk path. Also run `BenchmarkSyncAllColdArchive` to protect the
whole-archive default path that shares the budget.

For each candidate capacity, alternate baseline and candidate runs on the
generic 16 GB machine with the same synthetic fixtures. Use at least five
measured runs per revision and compare medians. Reject a candidate if either
bulk fixture is more than 10% slower than v0.42.0. Among the surviving
candidates, choose the one with the lowest cgroup memory peak. The target for
the complete benchmark workload is below 8 GiB.

Measure cgroup peak, anonymous memory, and file-cache memory separately. Run a
final full benchmark on the authorized 16 GB machine. Keep transcript content,
source paths, stable identifiers, profiles, and scratch databases on that
machine. Only aggregate timing and memory measurements may enter the pull
request.
