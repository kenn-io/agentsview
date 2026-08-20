# Usage Aggregate Cache Implementation Plan

## Outcome

Replace request-time normalized-fact aggregation with exact daily rollups while
keeping the facts cache as a disposable build substrate. Daily usage, top
sessions, billed counts, and relaxed matching counts use rollups. Session detail
and PostgreSQL keep the live implementation. Aggregate cache failure is an
error, never a live-path fallback.

## Constraints

- Preserve arbitrary date ranges, timezones, filters, window-scoped dedup,
  per-row money rounding, authoritative allocation, and Cursor behavior.
- A live transcript change or resync must invalidate its source-session facts
  and every timezone rollup built from them before the next result.
- Bake exactly `agent` and `started_at`; join other session metadata live.
- Resolve pricing in Go and fingerprint the canonical effective pricing data.
- Recheck full source fingerprints before install. Do not order by
  `sync_marker`, which can decrease.
- Keep the SQLite/PostgreSQL complete-result parity test.
- Warm 30-day CLI release gate: at most two seconds on the protected clone.

## Execution record

### 1. Disposable schema and identities

- Add timezone records and exact local-day UTC intervals.
- Add per-session rollup installs keyed by source fingerprint, fact revision,
  baked metadata, pricing digest, and install revision.
- Add daily, activity, and narrow exception rows with covering window indexes.
- Use canonical IANA identity or a stable rule fingerprint for anonymous local
  zones.
- Keep fail-closed file recognition and database-ID/schema generations.

### 2. Go builder

- Load only narrow normalized facts for stale source sessions.
- Convert ordinary token facts directly to `(session, day, model, rate)` daily
  rows using existing Go pricing and money functions.
- Store every dedup-capable fact in the exception tier. This subsumes the more
  complicated connected-component classifier while preserving exact
  window-scoped semantics.
- Build model-aware user activity rows and a synthetic Cursor exception install.
- Replace each source session atomically after the archive fingerprint recheck.

### 3. Aggregate reads

- Ensure candidate facts, then requested-timezone rollups.
- Verify required installs in one pinned cache transaction.
- Read indexed daily rows and only in-window exception rows.
- Resolve snapshot/general dedup and price exception survivors in Go.
- Apply live filters and authoritative session-cost allocation.
- Route all aggregate consumers to this path; retain the bounded live path for
  session detail and PostgreSQL.

### 4. Background lifecycle

- Backfill newest sessions first in 256-session batches after daemon readiness.
- Rewarm process-local plus eight most recently requested named timezones.
- Share detached foreground/background fills and retry moving fingerprints at
  most three times.
- Sweep deletion hygiene, optimize between batches, incrementally vacuum a large
  freelist, and analyze after complete backfill.

### 5. Remove superseded machinery

- Delete the request-time normalized-facts SQL engine and its temp-table
  population.
- Delete SQLite pricing UDFs and SQL-specific rounding tests.
- Keep fact extraction/invalidation tests because facts remain the build
  substrate.
- Repoint public consumer and PostgreSQL parity tests at rollup-served SQLite
  results.

### 6. Verification

- Run focused lifecycle, mutation, randomized parity, and aggregate consumer
  tests.
- Run `go fmt ./...`, `go vet ./...`, the full fts5 test suite, and lint.
- Run `BenchmarkGetDailyUsage` and the benchmark gate.
- On the protected clone, require byte-identical 7-day, 30-day, and all-history
  output and measure cold/warm 1-, 7-, 30-day, and all-history behavior.
- Run the explicitly requested `roborev-fix` pass and address actionable
  findings before delivery.

## Measured result

The final warm CLI path reuses rollups across processes. The canonical pricing
digest fixes nondeterministic whole-cache invalidation, and stable process-local
identity prevents one daemon backfill from splitting across timezone keys.
Observed protected-clone warm CLI times are approximately 0.19 seconds (1 day),
0.35 seconds (7 days), and 0.85 seconds (30 days). Cold construction and
all-history remain separately reported background-scale work.
