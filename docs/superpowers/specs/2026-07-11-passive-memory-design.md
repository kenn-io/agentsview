# Passive Daemon Memory Design

## Goal

Keep the passive daemon within a few hundred megabytes of physical memory while
preserving parser correctness and avoiding a significant sync-performance
regression. The change targets four measured costs on a production-scale
archive: per-connection SQLite page caches, unconditional automation-audit text
transfer, archive-wide content hashing on every scheduled sync, and full session
loads for Codex title checks.

## Baseline and acceptance criteria

The isolated baseline at current-main revision `354a2164d` uses a
production-scale SQLite clone and source-tree clones. It contains roughly
136,000 stored sessions and 47,000 discovered Claude, Codex, and Gemini sources.
The unchanged scheduled sync currently takes about 9.6 seconds, allocates about
2.5 GB, and hashes almost every discovered source. A matching-classifier
automation audit transfers about 2.8 GB of message text into Go on every
database open.

The implementation is accepted only when:

- warm unchanged scheduled-sync content hashing for local Claude and Codex
  sources is bounded independently of total source bytes;
- same-size, same-mtime rewrites, changed-path events, watcher overflow,
  deletion, tombstone, resync, and remote-source behavior remain covered;
- representative sync and query benchmarks do not regress by more than 5%;
- the production-scale warm scheduled sync is no slower than the baseline;
- forced-GC Go heap and macOS physical/dirty memory remain within the passive
  budget; and
- SQLite and PostgreSQL automation classification remain behaviorally equal.

Raw RSS is recorded but is not the acceptance metric because it includes clean,
reclaimable mappings.

## SQLite connection memory

Change the connection page-cache target from 64 MiB to 8 MiB. Keep the existing
four-reader concurrency ceiling so analytics and UI requests do not serialize.
Set a finite reader connection idle lifetime so burst-created reader caches are
released between periods of activity; do not reduce the active concurrency
limit.

The current `_mmap_size` DSN argument is not recognized by the pinned go-sqlite3
driver and the profiled connections have `mmap_size=0`. Remove the ineffective
argument rather than treating it as performance protection. Do not add a real
mmap window in this change: it would change cross-platform memory accounting and
could inflate raw RSS with clean pages. The 8 MiB cache is gated by
representative cold and warm query and sync measurements, with 16 MiB as the
fallback if the 5% threshold is exceeded.

## Verified local-source gate

Add an in-memory gate for local, single-file Claude and Codex sources. A trusted
entry records the source path and a stat signature captured before a successful
content verification whose result the archive absorbed. The signature includes
size, mtime, inode/device where available, a platform change-time value, and the
Codex effective sidecar mtime when applicable.

The first pass after process start still performs the existing content hash. A
later full or scheduled pass may skip before hashing only when the signature
matches a trusted entry. Changed-path classification invalidates the affected
entry before processing. Watcher overflow, resync, force-parse, unreliable or
remote source identity, and an unavailable change-time value bypass or clear
trust and retain the deep verification path.

Promotion uses the same capture/invalidation-generation pattern as the existing
OpenCode storage gate so an event racing with a full pass cannot resurrect stale
trust. Full passes mark entries they observe and prune entries not seen in the
pass. One compact record holds the signature, invalidation generation, and
last-seen pass for each active gateable source; there is no second historical
generation map. Retained state therefore scales with the active local Claude and
Codex file set, not stored-session count, source bytes, or files deleted in
earlier passes. The implementation records and regression-tests its per-entry
allocation cost against small and large active sets. At the profiled 47,000-file
cardinality, the gate must stay within a 16 MiB retained-memory budget.

Unix builds obtain nanosecond ctime from `stat`. Windows uses the native file
change-time API when available; if it cannot obtain a reliable value, the gate
does not trust that file. A same-size rewrite with restored mtime therefore
changes ctime and re-hashes, while a watcher event remains the backstop for
filesystems with unusual stat behavior.

Gemini remains on its existing composite fingerprint because its transcript,
`projects.json`, and trusted-folder metadata form one freshness identity; the
profiled archive had fewer than 1,000 Gemini sources and they were not a
material hashing cost. Remote sources also retain deep verification because each
run materializes new temporary files whose local stat identity is not a durable
source signal.

## Automation audit

Keep Go's `IsAutomatedSession` logic authoritative for both SQLite and
PostgreSQL. The classifier hash regains a useful distinction:

- a missing or different hash runs the existing full audit, which is the
  recovery path for classifier changes and explicit rebuilds;
- a matching hash runs an exact two-phase integrity audit.

Phase one reads session metadata plus bounded prefixes and complete byte lengths
for the first user message and fallback first message. The shared Go matcher can
conclude true when a configured prefix or substring is already visible, and can
conclude either result when the bounded value is the complete text. It does not
conclude false for truncated text, so any matching rule that could be affected
by the unseen suffix takes phase two. Multi-turn rows are conclusively
non-automated without reading message text.

Rows that remain unresolved are fetched by ID in bounded batches and evaluated
with the existing full-text Go classifier. This retains exact, substring,
Unicode-whitespace, stale-positive, and stale-negative behavior without
duplicating the classifier in SQL. SQLite and PostgreSQL share the evidence and
verdict helpers while retaining dialect-specific queries.

Orphan copies continue to call the forced backfill, which clears the classifier
hash and therefore takes the full recovery path. PostgreSQL continues auditing
matching-hash rows so stale clients pushed after a prior schema audit are
repaired.

## Codex title lookup

Replace `GetSessionFull` in the Codex index-title comparison with a narrow query
that returns only `session_name`. Preserve ID prefixing, path rewriting, missing
rows, explicit refreshes, and title-only index changes. This removes large row
and schema allocations without changing title-refresh decisions.

## Hash buffers

Do not make buffer pooling a required part of the design. Re-profile after the
verified-source gate. Add shared hash-buffer reuse only if warm scheduled-sync
allocation profiles still show material `io.copyBuffer` churn. This keeps the
change focused on avoiding work rather than optimizing work that should no
longer run.

## Validation

Use test-driven changes for each component. Tests compare small and large
archives and observe fingerprint calls or bytes read rather than implementation
strings. They cover gate promotion, invalidation races, pruning/capping,
same-stat rewrites, watcher overflow, deletion, tombstones, remote bypass, and
Codex title refreshes. Automation tests cover matching-hash bounded transfer,
full recovery on hash mismatch, prefix, substring beyond the prefix, exact
matches, Unicode whitespace, stale positives, and SQLite/PostgreSQL parity.

After unit tests, formatting, vet, and the relevant full suite pass, build a
branch binary into scratch space and repeat allocation, CPU, live/forced-GC
heap, SQLite heap, and macOS `vmmap` measurements against isolated clones. Do
not install or restart the production daemon in this change.
