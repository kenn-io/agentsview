# Background Work and Memory

Read this file before changing watchers, polling, sync scheduling, or other
long-running background work. Also read it before investigating memory growth.

- Keep passive daemon memory within a few hundred megabytes on macOS, Linux, and
  Windows. Treat sustained growth beyond that range as a regression.
- Bound watcher, polling, and sync work by the changed batch, not the full
  archive. Do not scan or load every stored session for each filesystem event.
- Declare costly scheduling inputs as provider capabilities. Compute them only
  for providers that use them, and default new capabilities to unsupported.
- Add cardinality-scaling regressions for background paths. Compare small and
  large archives and prove that unchanged work per event stays bounded. Cover
  deletion, tombstones, and persistent archives in the same tests.
- Diagnose long-running memory with allocation and CPU profiles, live heap,
  forced-GC heap, and operating-system physical or dirty memory. Raw RSS does
  not prove live memory because it includes clean reclaimable mappings.
- Profile branch binaries only against isolated, production-scale database and
  source clones. Never use live archives or agent transcripts.
- Observe retention long enough to reproduce the reported growth window. On
  macOS, record `vmmap` physical footprint and dirty memory. Use portable Go
  allocation and heap metrics on Linux and Windows.

## Usage cache backfill

- Start usage-cache backfill only after the writable archive transaction that
  changed a session has committed. Mutation hooks enqueue session IDs; they
  never fill while holding the archive writer.
- Foreground fills are per-session single-flight and detached from request
  cancellation. Cancelling one waiter must not cancel shared progress.
- Detached fills and rollup builds hold their own cache-generation lease.
  Retirement cancels their coordinator context and waits for those leases
  before closing SQLite handles.
- A writable daemon runs one newest-usage-first coverage pass after HTTP
  readiness. It installs at most 256 sessions per cache transaction and yields
  between batches. The pass fills normalized facts and daily rollups for the
  process-local timezone plus up to eight retained recently requested explicit
  timezones. Installed source and aggregate fingerprints, not a progress
  cursor, are the authoritative coverage records.
- If a source fingerprint changes during a pass, recapture and restart the
  snapshot pass at most three times. Already installed current fingerprints
  make stable batches reusable; never pair facts from the newer source with
  rollup metadata from the older snapshot.
- Sweep the archive deletion journal before and after the pass and between
  install batches. Queries also inner-join current archive sessions before
  ranking, so tombstone processing is hygiene rather than a correctness
  dependency.
- Run incremental vacuum between batches only when the cache freelist exceeds
  4,096 pages, and reclaim at most 256 pages per call.
- Run `PRAGMA optimize` between substantial batches and after install-heavy
  foreground fills. Run full `ANALYZE` after generation creation and complete
  initial backfill, not after every batch.
- Backfill logs aggregate counts and elapsed time only. Do not log session IDs,
  projects, paths, prompts, or fact contents.
- Keep newest-first fact plus process-local-rollup coverage within 30 seconds
  and complete fact plus process-local-rollup archive coverage within five
  minutes on the protected production-scale benchmark clone. These are release
  gates, not reasons to delay daemon readiness. A foreground request for an
  unbuilt timezone or all-history coverage remains exact and may pay the
  remaining `fill facts -> build rollups -> read` cold cost.
