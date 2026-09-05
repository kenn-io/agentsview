# Steady-state performance measurements

Measurements collected on macOS arm64 with Go 1.27.0 and SQLite FTS5. These are
local observations, not portable latency targets. Live logs and a native process
sample guided the investigation; branch code ran on isolated database and source
clones or synthetic data. No collector was needed for these runs.

## Measured changes

| Operation                        |       Before |                  After | Workload                                            |
| -------------------------------- | -----------: | ---------------------: | --------------------------------------------------- |
| SQLite sidebar stats             |     27.99 ms |               10.16 ms | Local archive snapshot                              |
| PostgreSQL sidebar stats         |      11.1 ms |                 4.7 ms | Anonymized session metadata                         |
| Codex source discovery           |  about 90 ms |            about 56 ms | 12,656 cloned source files, warm filesystem         |
| Changed-path append batch        |     12.60 ms |                5.72 ms | 1,000 sessions, 10,000 empty sources, 25 iterations |
| Unchanged skip-cache persistence | about 8.7 ms | about 0.3 microseconds | Focused benchmark, 10,000 cached entries            |

Stats now calculate five totals in one filtered aggregate. Codex discovery
reuses directory-entry file types, avoiding a repeated stat per source. Its
allocation dropped from about 16.4 MB to 11.4 MB per discovery pass.

Claude duplicate resolution probes the requested transcript filenames in each
project instead of enumerating unrelated transcripts. Append allocation fell
from 11.14 MB to 0.266 MB per batch (33,912 to 2,091 allocations). Nested
subagent candidates still require traversal. The regression test compares 8 with
8,000 unrelated files, checks duplicate selection and deletion, and fails
against the previous implementation.

An unchanged skip cache no longer copies its map and replaces all persistent
rows. The focused benchmark went from about 60,059 allocations to one
assertion-related allocation. The simulator's empty files become empty sessions,
so they do not populate this cache; the append improvement above must not be
attributed to skip-cache persistence. A separate durable-cache regression checks
unchanged work, updates, deletion, and engine restart.

## Larger workload and remaining costs

Run this workload with the retained [simulator](performance-simulator.md):

```bash
make perf-sim PERF_SIM_FLAGS='--sessions 10000 --turns 20 --active-turns 1000 --iterations 5'
```

It starts with 403,920 messages. Two active sessions receive a new pair per
iteration. The following are medians after initial ingest and query warmup;
queries run after each append, so usage reads include refreshing changed data.

| Operation                     | Median duration | Median allocated memory |
| ----------------------------- | --------------: | ----------------------: |
| Stats                         |         3.63 ms |                1.68 KiB |
| Sidebar index                 |        43.69 ms |               15.71 MiB |
| Analytics summary             |       202.82 ms |                6.64 MiB |
| Daily activity                |       264.92 ms |               11.55 MiB |
| Daily usage                   |       435.70 ms |              152.34 MiB |
| Common-term full-text search  |       852.94 ms |                0.19 MiB |
| Unchanged full reconciliation |     1,232.65 ms |              159.46 MiB |
| Long-session append batch     |        90.64 ms |               33.97 MiB |

Allocation is process-wide work during each operation, not retained heap or
resident memory. The CPU profile spent about 60% of samples in C calls, so SQL
query plans or native profiling are needed to explain that work further. Search
uses a deliberately common term and is not representative of rare-term queries.
These observations identify further work; no optimization of these analytical
queries is claimed here.

The live daemon also repeatedly logged a malformed Harness source failure at
roughly seven-second intervals. That retry behavior remains unresolved. The
simulator currently excludes malformed records and watcher scheduling, so these
results do not establish an idle-daemon CPU reduction or a long-duration
memory-retention result. Add those workloads before using it to gate those
behaviors.

## OpenCode SQLite scans

The retained OpenCode workload uses the producer's indexed session/message/part
schema in WAL mode. Both runs below used 20 turns per session, two active
sessions with 1,000 initial turns, and five iterations. The producer database
and archive both grew with session count; active session length and update count
stayed constant. These measurements preceded the virtual-event deletion-check
fix.

| Operation                            | 1,000 sessions | 10,000 sessions |
| ------------------------------------ | -------------: | --------------: |
| Session metadata scan                |        1.37 ms |        10.79 ms |
| Full child-digest scan               |       52.13 ms |       523.47 ms |
| Poll two active sessions after edits |        1.46 ms |         1.57 ms |
| Unchanged container event            |        4.42 ms |        37.04 ms |
| Append two long sessions             |      121.23 ms |       163.55 ms |
| Archive two child-only edits         |      333.50 ms |     2,428.55 ms |
| Unchanged reconciliation             |      388.49 ms |     4,260.64 ms |

The profile attributed about 30% of total CPU samples to source-missing
reconciliation. Its per-member provider lookups repeatedly opened the producer
database to check session existence. Changed-path preparation treated virtual
paths as missing filesystem entries even after the provider resolved them to
existing sessions. Those apparent deletions triggered container-wide ownership
and existence checks on active-session events.

Preparation now removes successfully resolved OpenCode virtual members from the
missing-path list. Actually missing members keep the deletion path. A regression
compares 8 with 800 archived sessions, checks unchanged virtual-event work, then
removes the target producer session and verifies source-missing state, retained
archived messages, and an unaffected neighboring session. The previous
implementation allocated 189,332 times for the larger unchanged event, failing
the test's threefold scaling bound of 8,397.

A matched 10,000-session comparison using identical final simulator code and an
overlay of the previous engine measured **2,491.26 ms to 120.17 ms** for the
two-session child-edit batch, about **20.7 times faster**. Median allocation
fell from **596.72 MiB to 77.04 MiB**, and allocation count from 2,824,784 to
327,523. The unchanged reconciliation path remained expensive at 4.10 seconds
and 1.07 GiB after this fix; the improvement is specific to active virtual
events, not full reconciliation or an idle-daemon measurement.

Full reconciliation still performs these member-existence checks. Its measured
10,000-session allocation was about 1.07 GiB per pass; that remains a separate
optimization target. The 10,000-session analytical query medians were 166 ms for
summary, 242 ms for activity, 360 ms for usage, and 753 ms for common-term
search.

## Historical evidence limits

The component timings above were recorded during the investigation, but their
original temporary raw captures are no longer available. They are historical
observations, not independently replayable benchmark evidence. Earlier simulator
reports also omitted build revision and dirty-state metadata. The matched
OpenCode comparison used a source overlay; its quoted numbers should be rerun
with the retained simulator before treating them as a release gate. New reports
record build metadata, executable hashes, and optional overlay notes.

## Real daemon with a synthetic SQLite producer

A separate loopback server built from `a87db9ce0`, which includes the checked
`origin/main`, imported 10,000 sessions and 403,920 messages. Its binary hash,
HTTP samples, and process measurements are retained in
[the daemon capture](performance-data/daemon-opencode-2026-09-04.json). The
installed application and its archive were not changed. Raw profiles and the
local experiment scripts remain in the worktree's ignored `tmp/daemon-perf`
directory; they are not needed to run the retained simulator.

One long session received ten user/assistant pairs and ten child-only edits,
with an SSE watch open. The client checked the newest ten HTTP messages every
250 ms. One preparatory append preceded capture, and the first measured append
timed out during watch setup. The remaining updates appeared in roughly 7–8
seconds through the session fallback. These timings include polling and the
five-second fallback delay, unlike the component measurements above.

Ten requests to each analytical endpoint all succeeded while updates ran. The
median HTTP times were 4.6 ms for stats, 37.7 ms for sidebar index, 161.0 ms for
summary, 238.2 ms for daily activity, 397.3 ms for usage summary, and 747.1 ms
for common-term search. The window covered June through September in UTC. The
first request to each endpoint is included, so these are mixed cold/warm
observations. The parent used about 29.5% of one CPU core during this phase. Its
first 30-second profile contained 10.23 CPU seconds, with 51.7% of samples in C
calls. No sync-worker subprocess was observed during these samples; initial
ingest had used a worker before capture.

RSS peaked near 618 MiB during queries. After 60 seconds of recovery and a
forced collection, sampled live Go heap was about 17.2 MiB and macOS physical
footprint was 76.2 MiB, compared with 51.9 MiB before updates. RSS remained near
469 MiB before collection, and `vmmap` reported 406.4 MiB of written writable
regions. These measures describe different accounting categories. This short run
does not establish a long-duration memory-retention result.

The recovery minute was not idle: after the producer writer closed, native
watcher batches ran every five seconds while the SSE watch remained open. The
profile attributed 3.6 CPU seconds to source-missing reconciliation, primarily
per-session SQLite existence probes. A focused reproduction found 188,690
allocations for a missing WAL event versus 22,797 for the same unchanged
container event with 800 sessions. Removing a WAL or SHM sidecar does not itself
mean that the database or its sessions disappeared.

Changed-path preparation now excludes vanished sidecars of known containers
whose pre-discovery state was captured successfully. Content classification
still runs, and actual container removal keeps source-missing reconciliation.
The regression checks both retained messages and missing-source state after
removing the database. The fixed missing-WAL event made 89 allocations, down
from 188,690 in the old path. The simulator retains a writer-close recovery
phase. Repeated watcher notifications alone do not establish repeated expensive
reconciliation: a later no-writer baseline used only 0.11 CPU seconds across
29.4 seconds. Writer-close cost must be measured separately from quiet polling.

A follow-up with six no-op SQLite writer transactions and closes over thirty
seconds used 0.12 CPU seconds before and 0.09 after. Neither run reproduced the
initial existence-scan spike; that difference is too small to establish an idle
or average writer-cycle improvement. The allocation regression and original
profile support the specific removed-sidecar fix. They do not prove that every
writer close will incur or avoid the original spike.

The retained daemon JSON is explicitly an exploratory capture with incomplete
generator provenance. Its generator was built from uncommitted simulator code;
the exact generator patch, command, and toolchain are unavailable. The generator
hash and workload options do not reconstruct that source. Do not use this
capture as a reproducible benchmark or release gate, or infer that its corpus is
byte-identical to output from a committed simulator. A controlled comparison
requires fresh runs from identified committed builds or fully retained patches.
The provenance requirements in the simulator guide apply to those new runs; they
are not a claim that this earlier capture met them.
