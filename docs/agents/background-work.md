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
