# Performance Gates

agentsview has repeatedly shipped performance regressions where sync work
stopped scaling with *new data* and started scaling with *archive size*. This
document records the regression classes we have actually hit, and the gates that
now guard each one. When you touch a sync or DB hot path, know which gate covers
you; when you fix a new class of regression, add a gate here.

The watcher scheduler, bounded watcher batches, and Codex continuation cursor
contracts are documented in
[Background Sync Efficiency](background-sync-efficiency.md).

## Regression history

| Class                          | What happened                                                                                                                                                                                           | Fixed in                                       |
| ------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------- |
| Discovery O(sources) root work | Gemini rebuilt its project map per session; positron/vscode-copilot re-read `workspace.json` per session. A large store spent 2m47s in discovery.                                                       | #912                                           |
| Unchanged sources reparsed     | The provider migration dropped pre-parse DB-freshness skips; every full sync reparsed and rewrote untouched sessions.                                                                                   | `providerSourceUnchangedInDB` (#883 follow-up) |
| O(history) incremental appends | Every streamed line ran a full signal recompute (reload all messages, secret regex scan) and chunk merges delete+reinserted every message row. ~4,700 session updates/day each paid O(session history). | #954                                           |
| O(file) warm Codex appends     | A warm append still hashed the full source for the fingerprint and re-hashed the committed prefix after every write; late tool outputs forced a full transcript reparse, and unchanged startup hashed every source before the DB freshness skip. | persisted parser checkpoints (this PR)         |
| Bulk ingest throughput         | Full resync ran per-row inserts and rebuilt FTS incrementally; 26.7k sessions took 1m17s.                                                                                                               | #411                                           |
| Event storms                   | One SSE emit per watcher flush drove ~1/s dashboard refetch; SQLite WAL sidecar events fanned out to every session in a shared DB.                                                                      | #367, #956                                     |
| Per-row query shape            | `GetDailyUsage` ran 1.2M `json_extract` calls per scan and had no date pushdown.                                                                                                                        | #309                                           |

## Two layers of gates

### 1. Deterministic work-count invariants (run in `make test`)

These count work units instead of measuring time, so they are immune to CI
runner noise and fail loudly:

- `TestWarmFullSyncDoesNoBulkWriteWork` (`internal/sync/perf_invariant_test.go`)
  — a second full sync over an unchanged Claude archive must skip everything
  and run zero bulk-write batches (`Engine.PhaseStats`).
- `TestProviderAuthoritativeUnchangedSessionSkipsOnResync`
  (`internal/sync/provider_freshness_integration_test.go`) — the generic
  freshness skip for provider-authoritative agents, Vibe as representative.
- `TestWriteIncrementalDebouncesSignalRecompute` and the rest of
  `internal/sync/signal_schedule_test.go` — streaming appends must debounce
  the O(history) signal recompute.
- `TestCodexCheckpoint*` in `internal/sync/checkpoint_test.go` — a full Codex
  parse persists a checkpoint, an append resumes from it and advances it
  atomically with the delta, truncation/anchor mismatch force an
  authoritative rebuild, and an unchanged checkpointed source is trusted on
  stat alone (append-trust; the audit path still catches rewrites).
- `TestCodexCheckpointStaleCannotResumeFromNewerDBOffset`,
  `TestCodexCheckpointHashStateBoundedToCommittedOffset`,
  `TestCodexCheckpointColdRestartResumeParity`, and
  `TestCodexCheckpointAuditRepairsSameStatRewrite`
  (`internal/sync/checkpoint_review_test.go`) — a surviving checkpoint must
  agree with the committed DB offset/ordinal/hash, hash state covers exactly
  the committed prefix, cold restarts resume with parity, and the audit
  repairs same-stat rewrites.
- `TestParserCheckpointRoundTrip` and
  `TestWriteSessionIncrementalPersistsCheckpointInSameTx` (`internal/db`) —
  checkpoint rows round-trip and are committed in the same transaction as the
  incremental delta.
- `TestFullSyncPassIsByteBudgeted`, `TestBulkParseRetentionBudgetUsesWeightedAdmission`,
  and `TestCollectAndBatchFlushesOnByteCap` (`internal/sync`) — bulk passes
  and write batches are bounded by estimated bytes, not session count alone.
- The count-based seam tests in `internal/parser`
  (`discovery_workspace_manifest_test.go`, gemini/antigravity provider tests)
  — root-derived project info is built once per root, not once per source.
- `internal/server/broadcaster_test.go` — SSE emits coalesce to at most one
  broadcast per window.
- `TestWatcherBatchesPathsAndEnforcesDispatchFloor`,
  `TestWatcherSustainedWritesProgress`,
  `TestWatcherContinuesIntakeDuringCallback`, and
  `TestWatcherOverflowRunsFullSyncThenRetainsLaterBatch`
  (`internal/sync/watcher_test.go`) — watcher callbacks remain throttled and
  serialized, sustained writes make progress, event intake continues during a
  sync, and an event storm becomes a bounded full rescan.
- `TestCodexCursorCache`, `TestCodexCursorWarmColdParity`, and the cursor
  boundary tests in `internal/parser` — continuation state stays bounded and
  warm/cold parsing remains equivalent at safe offsets.
- `TestIncrementalSync_CodexAppend`,
  `TestIncrementalSync_CodexLifecycleTailUpdatesTermination`, the partial-tail
  tests, the late tool-result append test
  (`TestIncrementalSync_CodexExecAppendRetainsEvents`), and the
  late-update/title tests in
  `internal/sync/engine_integration_test.go` — safe Codex growth appends only
  new rows, late tool results update stored calls in place, and lifecycle
  metadata, incomplete records, title changes, and other retroactive updates
  preserve full-parse behavior.
- `TestCountDuplicatePromptsAllocationGrowthStaysNearLinear`
  (`internal/signals/heuristics_test.go`) — session-quality analysis must not
  rebuild token sets for every pair of user prompts.

When you fix a performance bug, prefer adding a gate at this layer: expose or
reuse a counter (`SyncStats`, `PhaseStats`, `AnomalyStats`, a swappable
package-var seam) and assert the invariant, e.g. "second sync parses zero
sessions" or "the manifest is read once per root regardless of session count".

### 2. Benchmark gate (runs on every PR via `bench.yml`)

`.github/workflows/bench.yml` runs `make bench-gate` — the single source of
truth for the gated package list, sample count, and per-tier iteration counts —
on the PR head and its merge base on the same runner, then compares the outputs
with `cmd/benchgate`:

- `BenchmarkSyncAllWarmNoop` — full sync over an already-synced archive (stat +
  skip work only; also self-asserts nothing is re-synced or bulk-rewritten).
- `BenchmarkSyncPathsIncrementalAppend` — absorb one appended line into a
  1,000-message session.
- `BenchmarkCodexCheckpointAppendResume` — the source-reading pipeline of a
  checkpoint-resumed Codex append: checkpoint gate (stat + anchor digest +
  fingerprint resume), seeded tail parse, committed-prefix resume hash, next
  anchor digest, and next checkpoint assembly. Bounded by the anchor window
  plus the tail — see
  [Background Sync Efficiency](background-sync-efficiency.md) for the
  cost-model boundary.
- `BenchmarkCodexLateToolOutputDebouncedBurst` — a checkpoint-resumed Codex
  append stream with the signal debounce stretched, so only the first
  iteration pays the O(history) recompute and the rest are amortized. It
  guards the late-result update path's per-append cost; it is deliberately
  NOT the quiet-session gate (see the `BenchmarkCodexQuietAppendSignals*`
  trio below).
- `BenchmarkCodexQuietAppendSignals500` / `...5000` / `...15000` — the
  non-amortized quiet-session append gate: every iteration appends a new
  function_call plus the previous call's late output and pays the full
  inline signal/secret maintenance (debounce disabled). The three sizes
  prove per-append latency does not scale with stored history; each
  self-asserts zero `GetAllMessages` calls in the timed loop.
- `BenchmarkCodexColdFullSync` — a fresh database and engine ingesting the
  transcript from scratch, including the single-pass checkpoint capture.
  Per-op cost deliberately exceeds the micro-benchmark band; it exists to
  catch a regression that adds a source read pass to the cold pipeline.
- `BenchmarkSyncAllColdArchive` — first-sync ingest throughput through the
  default per-session write path.
- `BenchmarkResyncBulkIngest` — the same archive through the resync bulk-write
  pipeline (`writeBatchBulk` / `DB.WriteSessionBatch`, the #411 regression
  class); self-asserts every session took the batch path.
- `BenchmarkReplaceSessionMessagesStreamingMerge` — the streaming chunk-merge
  diff path (one UPDATE, not a full delete+reinsert).
- `BenchmarkInsertMessagesBatch` — multi-row batched ingest.
- `BenchmarkResyncBulkContributorIngest` — the same archive entering the atomic
  rebuild through a contributor engine.
- `BenchmarkSearchContentSubstringPage` / `BenchmarkSearchContentFTSPage` — one
  page of content search through the substring and FTS paths.
- `BenchmarkGetDailyUsage` — usage aggregation over 100k message rows.
- `BenchmarkSQLiteActivityReportCandidateSource100K`,
  `BenchmarkSQLiteActivityReportCandidateSourceLongSession`, and
  `BenchmarkSQLiteActivityReportArtifacts100K` — activity-report candidate
  streaming and artifact building over 100k sessions or a 100k-message
  session.
- `BenchmarkScan` / `BenchmarkScanDefinite` — secret-scan regex throughput.
- `BenchmarkCountDuplicatePromptsLargeSession` — duplicate-prompt analysis over
  a long session with shared vocabulary and distinct prompt context.

`benchgate` builds on `golang.org/x/perf`: `benchfmt` parses the output and
`benchmath` — the statistics engine behind `benchstat` — summarizes samples and
tests significance (Mann-Whitney U). benchgate adds only the policy benchstat
does not provide: thresholds, floors, and a failing exit code. Gating is per
benchmark — any single benchmark over its threshold fails the PR; nothing is
averaged across benchmarks. It gates hard on `allocs/op` (limit 1.25x) and
`B/op` (1.35x), which are deterministic for the same code and iteration count —
an O(archive)-instead-of-O(delta) regression always blows them up. Those two
compare the candidate's *worst* `-count` run against the baseline median, so
even an intermittent extra-allocation path fails. That is intentionally
asymmetric: the baseline is treated as the historical reference, and candidate
instability is what blocks the PR (failure lines include the baseline's worst
run so pre-existing instability is visible). Time (`sec/op`) compares medians
with a loose 2.0x limit and must additionally be a statistically significant
difference before it fails, so a single slow run on a noisy runner cannot flake
a PR but algorithmic blowups still do. Time gating requires at least 5 candidate
samples; fewer is reported as a configuration error (the candidate run is under
the workflow's control), while a baseline with fewer than 5 samples — a
legitimately partial base run — is reported and not gated. Baselines below a
per-metric floor are not gated. Benchmarks that exist on only one side are
reported but never fail, so adding or removing benchmarks cannot wedge a PR.
Only `allocs/op`, `B/op`, and `sec/op` are gated: custom `b.ReportMetric` units
are collected and reported as ungated, never enforced.

Two failure modes are treated as loud configuration errors (exit 2) rather than
silent gaps: a capture whose result lines fail to parse (for example test log
output interleaved into a `Benchmark...` line — the sync benchmarks silence the
engine's logger for exactly this reason), and a gated unit present in the
baseline but missing from the candidate (for example a candidate captured
without `-benchmem`), which would otherwise silently disable that gate for good.
The reverse — a gated unit missing from the baseline, which may legitimately be
older or partial — is reported as not gated.

The gate always runs with a fixed `-benchtime=Nx` iteration count (not a
duration): two of the benchmarks grow their fixture as they iterate, so the
baseline and candidate must run the same number of iterations to measure
identical workloads. Iterations come in two tiers. Benchmarks whose single
iteration costs hundreds of milliseconds to seconds (the 100k-row usage and
activity-report fixtures and cold-archive ingest, matched by `BENCH_GATE_HEAVY`)
run in a second `go test` pass with `BENCH_GATE_HEAVY_TIME` iterations; every
other benchmark runs with `BENCH_GATE_TIME`. Per-op ratios at that scale do not
need the averaging that millisecond-scale samples do, and at the full iteration
count those few benchmarks were most of the gate's wall clock.
`BENCH_GATE_COUNT` samples are taken per benchmark, and every sample rebuilds
the fixture, so the count is kept at benchgate's significance minimum. CI
evaluates `make bench-gate-config` on the PR head and passes the count, both
iteration counts, and the heavy-tier regex into the merge-base run, so a PR that
changes those defaults still compares identical workloads; do the same locally
if you override them.

Report identifiers are package-qualified benchmark names
(`go.kenn.io/agentsview/internal/db.InsertMessagesBatch-18`) when the captured
output carries `pkg:` metadata, falling back to the bare name when it does not
(e.g. hand-trimmed captures). Do not mix captures with and without `pkg:` lines:
the same benchmark would key differently and be treated as removed/new.

Run locally, comparing your working tree against a baseline commit. Like CI, use
a worktree for the baseline — checking out or stashing in place can leave
candidate files (or your commits) in the baseline run:

```bash
make bench-gate > new.txt                # candidate: current tree
git worktree add /tmp/bench-base "$(git merge-base HEAD origin/main)"
make -C /tmp/bench-base bench-gate > old.txt
git worktree remove /tmp/bench-base
go run ./cmd/benchgate -old old.txt -new new.txt
```

Cross-backend query benchmarks live separately in `internal/backendbench`
(`make bench-backends`, requires Docker) and are not part of the PR gate.

`BenchmarkCodexIncrementalCursor` lives in `internal/parser` and compares cold
prefix reconstruction with an exact warm cursor. It is diagnostic rather than
PR-gated: `BENCH_GATE_PACKAGES` currently contains `./internal/sync`,
`./internal/db`, `./internal/secrets`, and `./internal/signals`.

### 3. Macro ratio gate (run manually, build tag `macrobench`)

The 10MB-vs-1GB same-append p95 ratio (`< 2x`) is not a CI benchmark: the
1GB fixture takes minutes per run. `internal/sync/codex_macro_bench_test.go`
is excluded from the PR gate by the `macrobench` build tag:

```bash
cd internal/sync
go test -tags 'fts5,macrobench' -run '^$' \
  -bench 'BenchmarkMacroCodexQuietAppend' -benchmem -count=6 -benchtime=5x
```

The gate: the p95 `sec/op` of `BenchmarkMacroCodexQuietAppend1GB` must stay
within 2x of `BenchmarkMacroCodexQuietAppend10MB` for the same
quiet-append shape (call + late output, full inline signal/secret
maintenance).

## Adding a benchmark to the gate

Every benchmark in a gated package is gated — there is no per-name allowlist to
maintain. A benchmark added by a PR has no baseline, so its first run is
reported without gating; it gates automatically once merged.

1. Write the benchmark next to the code it guards (`*_bench_test.go`,
   `b.ReportAllocs()`, self-assert the invariant it protects where possible).
   If the code under test logs, silence the logger in the benchmark (see
   `silenceBenchLogs` in `internal/sync/engine_bench_test.go`): interleaved
   log output corrupts result lines and benchgate fails on the corruption.
1. If its package is not already gated, add it to `BENCH_GATE_PACKAGES` in the
   Makefile — a benchmark outside the gated packages silently never runs, so
   it looks gated while measuring nothing. CI picks the list up from the
   Makefile; each side of the comparison benchmarks its own commit's list, so
   growing the gate cannot break the base run.
1. Keep per-op cost roughly in the 100µs–100ms band: below the benchgate floors
   nothing is gated, and far above it the job gets slow. A benchmark that
   needs a large fixture to expose per-row scaling belongs in
   `BENCH_GATE_HEAVY` so it runs with the reduced iteration count; a
   fixture-growing benchmark does not, because its per-op cost depends on the
   iteration count. Keep fixture seeding cheap too: it is repeated once per
   `-count` sample.
1. Keep per-iteration setup out of the timed region (`b.ResetTimer`, pre-built
   fixtures): helper allocations inside the loop are gated as if they were
   product cost and dilute or distort the ratio.
1. If the benchmark's fixture grows across iterations, say so in its comment;
   the fixed `-benchtime=Nx` keeps both sides comparable, but readers need to
   know per-op cost depends on the iteration count.
