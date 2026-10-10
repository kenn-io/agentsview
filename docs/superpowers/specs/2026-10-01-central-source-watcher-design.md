# Central source watching design

Status: proposed, unimplemented. This refines
[KENN-7](https://linear.app/kenn-io/issue/KENN-7/agentsview-replace-per-provider-watch-limits-with-a-scan-first-watcher).
The source baseline is `e6004b6ae6f94721adbfc932d6b8a774e55d1462`.
The companion [implementation plan](../plans/2026-10-01-central-source-watcher.md)
describes delivery and verification. The user requested both artifacts without
additional conversational approval stops; implementation is not authorized.

## Intended outcome

Adding a provider should require describing its physical inputs and interpreting
its format, not inventing another watcher, polling strategy, or recovery path.
Watch exhaustion should change latency, not switch to a different ingestion
algorithm. Persistent scanner metadata must have a finite disk and memory cost.

The selected boundary is a central coordinator plus declarative source topology.
The coordinator owns scheduling, coverage, routing, batching, retries, watch
allocation, and recovery. Providers retain format decoding, logical source
identity, canonical-source selection, and native change-journal interpretation.
Manual imports, source-deletion policy, mirror schemas, and the daily archive
audit are outside this change.

## What the earlier attempts establish

These are reported historical mechanisms, checked against PR descriptions and
the current tree, not new reproductions of the original production incidents.
Historical session searches through the local ClickHouse-backed AgentsView
instance supplied additional context; private transcript excerpts are omitted.

| Evidence | Failure to avoid | Design consequence |
| --- | --- | --- |
| [#1090](https://github.com/kenn-io/agentsview/pull/1090) | A path event loaded unrelated stored source hints | Route before constructing providers or querying source hints |
| [#1154](https://github.com/kenn-io/agentsview/pull/1154) | Watch recovery, archive memory, and deletion proof became intertwined | Separate physical observation from logical absence authority |
| [#1307](https://github.com/kenn-io/agentsview/pull/1307) | Polling widened one coverage gap across providers and immediately repeated long passes | Preserve unit ownership and schedule from completion |
| [#1319](https://github.com/kenn-io/agentsview/pull/1319) | Traversal scope was mistaken for deletion authority | Keep the existing explicit reconciliation proof contract |
| [#1455](https://github.com/kenn-io/agentsview/pull/1455) | Native event intake called Add/Remove and deadlocked on Windows | Keep the event pump separate from watch management |
| [#956](https://github.com/kenn-io/agentsview/pull/956), [#2030](https://github.com/kenn-io/agentsview/pull/2030) | SQLite reader SHM activity fed back into ingestion | Share SQLite physical-input policy; exclude SHM from data signals |
| [#1208](https://github.com/kenn-io/agentsview/issues/1208) | A watcher-only optimization was bypassed by expensive fallback polling | Feed every recurring trigger through the same unit dispatcher |
| [#2035](https://github.com/kenn-io/agentsview/pull/2035) | Generated Antigravity trees consumed the shared directory budget | Prune before descending or allocating native watches |

PR #2035 was open when researched. Its `MaxDepth` and `ExtraDirectories` fields
are absent from this baseline. Do not plan removal of nonexistent baseline
symbols. If it lands before implementation, recheck its overlap explicitly.

The current `parser.ResolveWatchRoots` strips include/exclude globs, and
`classifyProviderChangedPath` constructs providers before checking root
ownership. `rawwatch.Worker.HandleBatch` resolves `WatchPlan` repeatedly and
widens some uncertain batches to all-provider audits. These are integration
points, not reasons to rewrite parsers.

## Measurements and estimates

Measurements taken on Linux/XFS on 2026-10-01 used Python 3.14.7 and SQLite
3.53.1. Live source access was limited to directory enumeration and metadata.
Synthetic files and cache databases lived in a private scratch directory.
No live daemon was restarted, source content read, or archive modified.

The accessible source sample covered Claude, Codex, Pi, Cursor, and OpenCode.
Candidate matching was deliberately broader than final provider declarations;
it is an inventory estimate, not a proof of parser coverage. Symlinks were not
followed. The mirrored history contained about 20,000 visible sessions across
machines; that is not the local filesystem inventory.

| Measurement | Result |
| --- | --- |
| Local candidate files / relevant ancestor directories | 396 / 137 |
| Local scratch SQLite inventory, with primary and parent indexes | 288 KiB for 533 rows |
| Local relative path lengths | Mean 97 bytes; maximum 195 bytes |
| Local scratch schema bytes per row | 553 bytes |
| Synthetic tree | 50,000 empty files in 500 directories |
| Warm metadata-stat pass, median of five passes | 71 ms |
| Warm directory listings only, median of five passes | 24 ms |
| Warm enumeration plus file stats, median of five passes | 78 ms |
| Synthetic file metadata, 50,000 / 250,000 rows | 16.1 MiB / 80.7 MiB |
| Synthetic bytes per row | 337 / 339 bytes |
| Synthetic metadata build, 50,000 / 250,000 rows | 71 ms / 369 ms |
| Database size after deleting half the synthetic rows | Unchanged at both scales |
| Admission under a 48 MiB main-file cap | SQLITE_FULL after 148,736 committed rows |
| Main file after ten replacement-churn rounds at capacity | 48 MiB |
| Sampled journal peak during capped insertion | About 81 KiB; not a universal bound |
| Linux max_user_watches | 1,003,696 |

The noise experiment added 16,000 directories beneath one irrelevant `scratch`
child. A pruned parent-listing pass still admitted exactly 50,000 candidate
files and took 27 ms. Descendant visits were avoided; the extra parent entry
still had to be examined. Do not assert that all irrelevant entries cost zero.

These are metadata probes on warm filesystem caches, not production scanner
benchmarks or cold boot measurements. They do not demonstrate a large restart
speedup. The original full-path prototype duplicates prefixes and indexes;
its 32-53 MiB extrapolation for 100,000 rows is superseded by the compact-layout
comparison below. A 50,000-file stat pass every 30 seconds would consume
about 0.24% of one core if its measured wall cost translated entirely into CPU;
that is an estimate, not measured daemon CPU. Slow disks, APFS, network mounts,
longer paths, and live container queries remain unmeasured.

A second Linux/XFS representation experiment used 256 files per directory,
identical signatures, and TRUNCATE journaling with incremental auto-vacuum.
At 50,000 / 250,000 files, the full-path baseline used 16.10 / 80.84 MiB;
directory IDs plus basenames used 5.62 / 28.04 MiB; directory IDs plus
prefix-coded listing pages used 3.95 / 19.70 MiB. Each compact layout includes
directory metadata. All layouts preserved every filename and signature through
deletion, refill, and reopen. Prefix-coded pages cut the baseline by about 75%.
These prototypes omit production generation and routing metadata. Their repeated
rollout-style names favor prefix coding; sparse trees need separate measurement.
The baseline numbers are evidence of redundant storage, not an acceptable
production representation or a reason to enlarge the budget. See the
[portable measurement handoff](../plans/2026-10-01-source-watcher-mac-measurements.md)
for the script, workload, and Mac report contract.

The returned Mac report repeats the full experiment during normal concurrent
work on APFS: 19,922 source candidates, synthetic 50,000-file warm stat passes
of 102-112 ms, and matching compact-layout disk sizes. Compact rows filled
faster than prefix-coded pages; prefix-coded pages were smaller and read faster.
Use compact rows as the first format to qualify, retaining prefix coding as a
measured alternative rather than implementing both. The report is a short run
on a high-capacity machine and supplies aggregate tables rather than raw JSON.
It establishes metadata feasibility on that host, not production watcher or
restart qualification. See the
[returned Mac results](../plans/2026-10-01-source-watcher-mac-measurements.md#returned-mac-results)
for workload context, calculations, tradeoffs, and remaining limits.

The returned Windows report includes raw aggregate JSON and native-query
canaries. Individual 50,000-file path checks took 3.624-3.670 s; full native
signatures took 5.650 s versus 87.6 ms for 768 files in three directories.
Fast enumeration returned stale sizes in all three open-writer append canaries,
so it cannot replace fresh known-file checks. High-entropy names reduced prefix
coding's additional saving over compact rows to 2.49%. These results strengthen
physical scoping and selective updates, and require complete Windows identity
representation. See the
[returned Windows results](../plans/2026-10-01-source-watcher-windows-measurements.md#returned-windows-results)
for checked aggregates, source inspection, method limits, and follow-up gates.

## Ownership and declarations

Each provider exposes a `CoveragePlan(context.Context)` returning source
coverage units. A unit has a provider-local key, physical root, configured-root
provenance, and named input groups. A group describes source-file patterns,
companion patterns, container inputs, or plan inputs. Import-only providers
declare no local units.

The coordinator derives identity from provider, configured root, and unit key.
Its plan fingerprint includes the physical path, patterns, group semantics, and
applicable exclusions. Cache reuse requires the same fingerprint. Filesystem
device numbers are not persisted as identity across boots.

Declarations contain no scheduling functions, recursive-watch flags, custom
depth limits, or provider-specific eviction rules. Root-layout discovery may
read bounded markers or a registry such as Crush's projects file, but may not
enumerate transcripts merely to construct patterns. Registry changes replan
only the owning provider. Existing dynamic provider settings remain effective.

Source identity and companion-to-transcript mapping remain in
`SourcesForChangedPath`; generic scheduling no longer repeats those mappings.
Container declarations distinguish physical inputs from logical members and
state whether member edits/deletions require an authoritative member audit.
Existing insert-only row cursors do not qualify as complete member change feeds.

## Interest matching and routing

Use relative slash-separated paths. Literal segments and `path.Match` segment
syntax are supported. `**` matches zero or more directory segments, and
`**(N)` matches zero through N directory segments. Represent alternation as
multiple patterns; do not add a brace-expansion language in the first version.
This covers the rough spec's proposed alternatives without another parser.

The compiled matcher answers `MatchFile(rel)` and `CanContain(relDir)`. The
latter is boolean: true permits descent, never declares all files interesting.
In particular, reaching `**` in `**/agent-*.jsonl` must not admit unrelated files.
Empty pattern sets are invalid for local units. Truly unbounded layouts declare
`**/*` explicitly. Matching within segments remains case-sensitive; root
containment follows existing `filepath.Rel` platform semantics.

Physical watches merge by path. Logical units do not merge away ownership.
The routing index returns every matching `(unit, input group)` across same-path
and nested roots. Deduplicate logical source work after provider resolution.
Explicit root paths are not pruned because their ancestor names match excludes.
Exclusions apply below roots and never establish deletion proof. Manual import
and the existing archive audit retain their current discovery semantics.

## Scanner and acknowledgement

The scanner owns filesystem traversal, not parsing or archive locks. It starts
from declared roots and descends only into potentially interesting directories.
A trusted cached listing can avoid `ReadDir` when directory identity, mtime, and
change time match. Known candidate files still receive fresh individual checks for appends.
Windows bulk/DirEntry metadata may be stale while writers are open, even when
parent stamps match. Use enumeration for names and discovery, not as proof
that known-file signatures are unchanged. Query size, write time, actual change
time, volume, and full file ID from one fresh Windows handle. Keep unsupported
fields explicit; creation time is not change time. File identity is an opaque
128-bit value plus volume identity, with narrower platforms zero-extending their
native identity without losing bits. Never narrow Windows IDs to signed int64.

An explicit native dirty path reaches owning content verification even if its
fresh signature matches the acknowledged one. Retain this intent through retry
and only acknowledge after successful downstream work. Ordinary unchanged
coverage scans may skip parsing; lost-history recovery still uses scoped content
verification. Neither timestamps nor cached listing trust prove unchanged
content after notifications or event-history loss.
SQLite groups additionally use the existing shared database/WAL header probe.

Capture directory state before and after listing. If either differs, or the
timestamp lies within two seconds of the listing window, do not trust the new
listing for skipping. Missing change-time support does not manufacture proof.
Permission failures, transient stats, and missing roots preserve previous state
and report incomplete or unavailable scope rather than removals.

Read and deliver work in pages of at most 256 records and 2 MiB of serialized
metadata. No whole-root candidate slice or archive-sized in-memory map is
allowed. A directory listing exceeding the page budget is streamed and left
uncached unless it can be represented with paged, complete listing metadata.

Observations are staged until downstream success. A failed dispatch must leave
the earlier baseline usable on retry and restart. A crash after archive commit
but before cache commit may replay already-applied work; existing idempotent
source processing handles that replay. Stable, recorded parser failures use the
existing failure-cache policy rather than causing an immediate retry loop.
Cache writes never participate in the archive transaction. An acknowledged
signature change to an existing member updates only its row or affected page.
It does not require a new whole-directory listing generation. Membership,
incomplete scope, or changed directory proof still uses complete-listing rules.

Cache absence means unknown, not empty. On a cold or evicted scope, enumerate
its interesting physical inputs and request one scoped completeness pass before
claiming absence. The existing provider scope resolver determines the necessary
traversal and proof. Such passes may cost more than warm scans; coalesce them
and keep their retry ownership local. Never turn cache pressure into a repeated
all-provider audit. Aggregate members and cross-root replacements continue to
use their existing spool and replacement-proof rules.

## Returned Go watcher qualification

The [returned platform results](../../internal/watchprobe-platform-results.md)
exercise the isolated Go tester from `fd06b89e`. Cached warm metadata scans were
slower than uncached scans on every host, so persistence remains conditional on
saved downstream work. No startup-saving claim follows from cache reopening.
Windows fresh full scans took 2-3 seconds per pass under the tested loads.
Page fairness and event wakeups must therefore remain explicit contracts.

The Mac kqueue run dropped 66% of native notices and recovered 57 units only
at the end. Passing final checks is not timely recovery. Native intake must
wake coordinator work independently of coverage timers; scoped loss recovery
must progress during operation. Record operation mix, pending-loss age, and
physical descriptors/handles. Do not equate 64 logical directory allocations
with 64 kernel resources. The event-burst cause is unconfirmed. FSEvents needs
its own qualification; neither queue inflation nor attribute filtering is
approved by this result.

## Persistent cache and finite growth

If persistence qualifies, use one disposable `watch-cache.sqlite` outside
`sessions.db`. SQLite is a candidate for the disposable cache, not a requirement
for scanner correctness or a settled optimal representation. Its SQLite
application ID is `0x41565743`, cache kind is `source-watch-cache`, and cache
format version starts at 1. It is local,
never replicated, and contains no transcript content or history. It stores
current complete directory listings and acknowledged physical file signatures,
plus unit plan fingerprints and activity timestamps. This is an optimization,
not an archive or a durable work queue.

Store physical directory prefixes once, using parent-directory IDs and raw
basenames. File records reference directory IDs instead of repeating roots,
relative paths, and parent paths. Do not duplicate inventories per provider
when units share a physical directory. Prefer the compact primary key over a
redundant full-path index. Evaluate front coding of sorted basenames within
bounded listing pages; decoding one page must remain within the existing
256-record/2 MiB limits. Seek pages by directory, generation, and basename
boundary without loading an entire listing. Reconstruct full paths only for
the dispatched page. Preserve exact filename bytes and signature fields.
Collect unused directory-prefix records during bounded eviction maintenance;
the prefix dictionary must obey the same disk envelope as file listings.

Compare compact row storage and prefix-coded pages before choosing the cache
format. Qualify sparse directories, deeply nested and long paths, less
repetitive basenames, selective reads, changed-page writes, and reopen costs.
Choose persistence only if measured restart savings justify disk, memory, and
maintenance costs. The scanner must work without it. Do not add a separate
compressed-file implementation just to conduct this comparison.

The provisional maximum total storage envelope is 128 MiB. Its main database
is capped at 48 MiB using SQLite page limits; the remaining 80 MiB is reserved for rollback
journal and metadata. Use TRUNCATE journaling, incremental auto-vacuum, and no
WAL. SQL temporary storage stays in memory with bounded queries. Main-file page
limits alone are not sufficient evidence for the total envelope: qualification
must observe main, journal, and every auxiliary file during fill, eviction,
failure rollback, and churn. If the implementation cannot establish the envelope,
ship the coordinator without persistent caching rather than claiming a bound.

Use one cache writer, an 8 MiB SQLite page cache, and bounded page transactions.
Only complete listings may become trusted. Evict least recently active complete
directory inventories, including their file rows. Root removal and changed plan
fingerprints invalidate owned rows. Eviction is not a filesystem removal and
cannot tombstone sessions. Free pages are reused; incremental vacuum runs only
in bounded maintenance batches. Never perform a whole-file VACUUM or retain
backup generations as ordinary cache maintenance.

An unavailable, incompatible, full, or corrupt cache disables cache admission
for the affected pass and uses scoped scans. It does not stop ingestion or touch
the archive. Identify the cache by SQLite application ID and cache-kind metadata
before replacing it. A persistent-cache reset targets only that identified file.
No second unbounded in-memory inventory is allowed when disk caching is disabled.

## Coordinator and timing

There is one coordinator per local source consumer, with bounded serial dispatch
and fair progress between units. The daemon and raw-sync worker share its
planning, scanner, and routing implementation, but do not share archive-specific
state. Each process owns its cache; do not share writable cache connections
between those processes. Raw-sync stores its cache under its own existing data
directory and has the same envelope.

Retain the existing 500 ms first-event batching window, five-second dispatch
floor, 8,192-path limit, and 2 MiB path-byte limit. Events arriving during work
coalesce without postponing the first deadline. Entry overflow collapses to unit
scan requests rather than a pathless global FullSync. The five-second floor applies between logical work passes, not between
the 256-record pages inside a scan; otherwise a cold scan would take minutes
solely due to artificial page delays. Root/unit bookkeeping is
bounded by the configured plan; unknown-owner loss requests all local units.

Coverage scans run 30 seconds after completion for units with unavailable native
coverage. Safety listing sweeps run hourly, jittered between 45 and 75 minutes,
for all units. Targeted scans enter the same queue. Neither timer replaces
pending event work or bypasses backoff. Service pending owners between bounded
coverage pages; a large fallback inventory must not monopolize unrelated event
dispatch. Qualify cancellation and fairness against full fresh Windows checks.
Use at most one paused-at-page background scan producer with a one-page
acknowledgement channel. Even unchanged pages yield a checkpoint. The coordinator
services pending owners between pages, and cancellation joins its producer.
Keep serial archive dispatch initially. Qualify at most a small bounded stat pool only
if production measurements justify the CPU and latency tradeoff. Native callbacks perform no filesystem,
cache, Add/Remove, or provider work.

Providers with incomplete member change feeds retain 15-minute scoped member
audits, scheduled by the coordinator from completion. Keep existing
`PeriodicReconcile` behavior until its declared replacement is proven to detect
edits and deletes. Do not infer complete logical coverage from a watched DB file.
The daily archive content audit remains unchanged.

## Native coverage and lost events

Start with the existing backend technology: fsnotify on Linux/Windows and
FSEvents for recursive macOS roots. Read Linux's per-user watch limit and use
`min(65536, limit/2)` as the default total native-directory target. A failed limit
read uses a conservative target of 8192. Windows starts at 1024 directory
watches. Count root and ancestor watches inside the actual allocation ceiling;
they are preferred, not an unlimited exemption.

Initial allocation is round-robin across units, preferring roots and ancestors.
Do not add a global mtime-ranking scan or activity-based eviction yet. Native
coverage is a hint optimization; the 30-second scanner covers unallocated units.
Add an activity-based pool only after native activity measurements establish its
benefit. ENOSPC/EMFILE makes admission stop and activates scanner coverage;
promotion must not loop on a permanently exhausted shared OS budget.

Loss of a native watch and loss of event history are distinct. Ordinary coverage
loss schedules scans. Event-history loss also invalidates affected engine
freshness trust and schedules scoped verification. The current
`clearWatcherOverflowCaches`, verified-source records, SQLite trust gates, and
Codex checkpoints are not all interchangeable. Inventory each and preserve
same-stat rewrite behavior; do not promise that stat-only recovery replaces it.
Unknown-owner overflow may require verification across all local units, but
never automatically across remote or unrelated providers.

FSEvents drop flags retain live streams when the root remains valid and enqueue
the affected stream/path. RootChanged keeps the existing ancestor and availability
lifecycle. Lifecycle acknowledgements occur only after successful scope work.
The Windows pump/Add/Remove separation remains intact. FSEvents replay after
restart and Windows recursive subtree handles are separate optional work.

## Delivery and gates

The implementation plan has four dependent delivery stages. Each must leave
the existing application usable; unconnected building blocks are tested before
the coordinator replaces current runtime paths. No dual-running coordinators,
legacy aliases, parser rewrites, or new native change journals are included.

1. Compile declarations and route physical changes. Migrate each source family
   against its existing discovery and changed-path behavior.
2. Implement the bounded disposable cache and paged scanner independently.
3. Connect one coordinator to daemon startup, events, fallback scans, scoped
   member audits, and raw capture. Retire replaced paths only after parity.
4. Qualify native allocation and scoped recovery across platforms and publish
   the cost model and configuration.

Correctness gates include create/append/remove, companion-only changes, directory
replacement, cache eviction, failed dispatch/restart replay, overlapping units,
missing mounts, aggregate membership, cross-root moves, persistent archives,
SQLite reader feedback, provider setting changes, and lifecycle acknowledgements.
Cardinality gates compare fixed changed batches against small/large archives;
scanner no-op work may scale with candidate files and interesting directories.
Track directory entries examined separately from descendants visited.

The memory gate is a few hundred MiB for the whole passive daemon, with at most
32 MiB of incremental scanner/coordinator/cache working memory in the 50,000-file
scenario. This is a proposed release gate, not a measured production result.
Track source stats, directory listings, cache hits/evictions/admission refusals,
unit scan/verification/audit counts, native allocation, dispatch retries, and
maximum retained bytes through existing sync diagnostics. No frontend work is
required to expose these counters in existing diagnostics.

Qualification must include representative concurrent work on ordinary developer
Macs, such as builds, containers, browser activity, and active agent sessions.
Use the same 50,000-file workload under load; reducing cardinality or requiring
an idle machine is not an acceptable way to meet the design's gates. Record
scan completion and event-to-dispatch latency distributions, CPU time, peak and
retained memory, retries, and backlog over repeated runs. Completion-based
scheduling must not overlap scans or accumulate unbounded work when the machine
is busy. If the implementation cannot meet the stated correctness, coverage,
and memory requirements under this load, revise the design before cutover.
Python metadata timings identify qualification risks; they are not production
watcher latency evidence. Compare with an optional idle baseline and report
load context rather than discarding slow runs.

## macOS qualification access and remaining limits

The [Mac measurement handoff](../plans/2026-10-01-source-watcher-mac-measurements.md)
and standalone script can be run by an agent on the machine and returned as an
aggregate report. This covers metadata and storage representation experiments
without remote access or a watcher build. The
[Windows handoff](../plans/2026-10-01-source-watcher-windows-measurements.md)
uses the same script and workload with PowerShell commands. Native Windows
watch delivery and recovery remain separate implementation qualification.

For later native watcher qualification, needed access is an SSH destination
and account, or an existing remote-execution connection, to a Mac with a substantial local source collection on APFS. Read
provider directory metadata and create a private scratch directory for synthetic
files, a disposable cache, and an isolated binary. No sudo, transcript content,
live archive writes, installed-binary replacement, or live daemon restart is
needed. Every process created for measurement has its own cleanup ownership.

Repeat inventory sizes, path lengths, warm and cold-process enumeration/stat
passes, cache loading and churn, and native event counts on APFS. Cold process
does not imply a cold kernel cache. Run controlled create/append/rename/drop and
root-reappearance scenarios against scratch data. Record Go heap/allocations,
forced-GC heap, and `vmmap` physical footprint over a multi-hour retention run.
Use offline format fixtures or separately authorized source clones for parsing.

macOS and Windows metadata, representation, and isolated Go fsnotify sustained
measurements have been returned. Production coordinator and native FSEvents
qualification remain outstanding. Network and FUSE
timestamp reliability,
large changed-container enumeration, actual activity concentration, and cold
storage behavior remain explicit qualification work. They do not block this
design artifact; they gate performance claims and any later adaptive watch pool.
