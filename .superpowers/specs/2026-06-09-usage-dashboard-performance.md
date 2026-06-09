# Usage Dashboard — Performance & Memory Improvements

Status: DRAFT for review (revised) Date: 2026-06-09 Backend in scope: local
SQLite (PostgreSQL/Cockroach parity required per AGENTS.md)

## 1. Purpose

The usage dashboard is slow to load (~10s) and pegs a CPU continuously while
open. This spec catalogs the distinct problems (de-conflated), records the
measured baseline, and defines the improvements we want to make. It is a
design/scope document, not an implementation plan.

The headline finding: the "slow + CPU hammer" complaint is dominated by the
*frontend* doing redundant work — re-running scans and re-fetching a 30 MB
payload on every live session update — not by per-query latency. Per-query
latency is real but secondary, and the part of it that remains after a covering
index is CPU-bound and only movable by the deferred C2 change.

## 2. Measured baseline (evidence)

All numbers from the user's real DB (1,475,661 messages; 977K token-bearing;
106,565 sessions; 2.36 GB message `content`; 187 MB `token_usage`;
`usage_events` empty), measured warm via the `REAL_DB` harness, plus Playwright
against a server serving a copy. Machine: 137 GB RAM, so the DB stays fully in
page cache.

Backend query latency (all-time, automated included; warm, current schema —
which already has the partial index at db.go:773):

| query                                                | current (warm)         | note                                                                                                       |
| ---------------------------------------------------- | ---------------------- | ---------------------------------------------------------------------------------------------------------- |
| usage/summary `GetDailyUsage` (all-time)             | ~3.0–3.6s              | this is /usage/summary                                                                                     |
| usage/top-sessions `GetTopSessionsByCost` (all-time) | ~3.0–3.6s              |                                                                                                            |
| usage/summary `GetDailyUsage` (30-day, default view) | ~0.99s                 | already ~sub-second                                                                                        |
| usage/comparison (prior window of an all-time view)  | ~0.3s                  | empty far-past window                                                                                      |
| harness concurrent fan-out (all-time)                | ~5.5s                  | summary+top+comparison+counts at once; includes the diagnostic counts scan, so it overstates the live path |
| metadata (stats/projects/agents/machines)            | 0.03–0.11s             |                                                                                                            |
| sessions/sidebar-index                               | 30.71 MB / 92,221 rows | ~333 bytes/row                                                                                             |
| `GetUsageSessionCounts` (all-time)                   | ~2.6–3.5s              | DIAGNOSTIC ONLY — see note                                                                                 |

Note (session counts): `/usage/summary` does **not** call
`GetUsageSessionCounts`. `humaUsageSummary` calls `GetDailyUsage` only and reads
`SessionCounts` from that single result (huma_routes_usage.go:92);
usage_internal_test.go:66 asserts the handler makes zero count calls. The
`GetUsageSessionCounts` timing above is kept only as a diagnostic data point; it
is not on the dashboard's fan-out and is not a target of this work.

Covering index, measured on a copy with the existing index widened to cover the
read columns: the planner uses it; 30-day drops ~0.99s → ~0.88s; **all-time
wall-clock stays ~3s, roughly unchanged.** On this 137 GB-RAM machine the table
rows the non-covering index currently misses are already in page cache, so
eliminating that table I/O barely moves wall-clock. The covering index's value
here is therefore structural (bounded ~590 MB working set, no table touch, and
it may or may not remove the temp sort — verify with EXPLAIN) and matters most
as the DB grows past RAM or on disk-bound machines. Its real benefit must be
re-validated in the D-3 re-measure, including whether ~590 MB of index is
justified.

Browser (Playwright, usage tab, all-time):

- Cold load to networkidle: 10.7s.
- Main-thread long tasks during load: 0 (0 ms blocking) — no render freeze.
- sidebar-index fetched 3× on a single load = ~90 MB transferred.
- 30 MB JSON parse + 92K-object hydrate in V8: ~57 ms.

Ruled out as causes (measured): browser render/reactivity freeze (0 long tasks);
write contention (~10–15% under a continuous writer; WAL readers don't block on
writers); cold OS cache (137 GB RAM); server reader-pool saturation (fetchAll
wave every 3s stayed ~6s); single-request 60s hang (server WriteTimeout = 30s).

Conclusion: the dashboard is not frozen by the browser. It is (a) re-running
scans and re-fetching the 30 MB sidebar index on every live session update, (b)
shipping an oversized, unbounded sidebar payload, (c) running a non-covering
range scan whose all-time residual is CPU-bound, and (d) giving no progressive
feedback, so the wait feels like a hang.

## 3. Problem catalog (distinct, de-conflated)

### Group A — Wasted/repeated work (the CPU "hammering")

Two independent loops both fire on every live session update; both must be cut.

- **A1. Usage page auto-refetches usage scans on every live session update.**
  `UsagePage` wires `usage.fetchAll()` to an SSE subscription
  (`events.subscribeDebounced`, 300 ms) and a 5-minute interval. With agents
  always running, SSE batches re-run the CPU-bound ~3s summary + top-sessions
  scans back-to-back, pinning a core. An idle page with no events is quiet, so
  this is event-driven refetch, not a reactive loop.
- **A2. The global `sessions` store re-fetches and re-hydrates the 30 MB index
  regardless of route, including the usage tab where it is never rendered.**
  `App.svelte:238` calls `sessions.load()` (plus `loadProjects`/`loadAgents`)
  unconditionally on every route change. `sessions.load()`
  (sessions.svelte.ts:319) calls `startLiveRefresh()` and fetches the full
  sidebar index. `sync.onSyncComplete` (sessions.svelte.ts:1541) also calls
  `sessions.load()` directly, and ~15 internal callers (filter setters, URL
  init) call it too. The usage tab renders a bare `<UsagePage/>` with no
  `SessionList`, so this 30 MB is fetched and hydrated but never displayed.
  Because so many paths (route effect, sync event, internal setters) reach
  `load()`, the gate must live in/near the sessions store, not only in
  `App.svelte`.
- **A3. A single usage load triggers `sessions.load()` multiple times.** The
  route effect plus init paths fire it repeatedly, so Playwright sees the index
  fetched 3× (~90 MB) for one page open.
- **A4. No request coalescing or cancellation.** `load()` guards results with a
  `loadVersion` check *after* the `await` (sessions.svelte.ts:~328). A
  superseded request still downloads and parses the full 30 MB before being
  discarded — the guard prevents a stale render, not the wasted transfer/parse.
  Requirement: at most one in-flight sidebar-index request per filter signature
  — coalesce identical concurrent loads, and `AbortController`-cancel a
  superseded in-flight request so its payload never finishes downloading or
  parsing.

### Group B — Sidebar payload / memory (the 30 MB)

- **B1. The sidebar-index returns every session (92,221 rows) with no
  windowing/pagination.** This grows unbounded with history and is the root of
  both the client heap cost and the wire cost.
- **B2. The per-row payload is fat:** 15 JSON fields, ~333 bytes/row.

(Compression is a wire-transfer concern, not a memory fix — see W1.)

### Group C — Query latency

- **C1. The usage queries use a non-covering index.** A partial index already
  exists:
  `idx_messages_usage_timestamp ON messages(timestamp, session_id, ordinal) WHERE token_usage != '' AND model != '' AND model != '<synthetic>'`
  (db.go:773). The usage query additionally reads `model`, `claude_message_id`,
  `claude_request_id`, and `token_usage`, none of which are in that index. So
  the index supplies the timestamp range but the engine does table I/O into the
  2.36 GB content-laden rows for the missing columns (and a temp B-tree sort
  depending on the chosen plan). This is a non-covering range scan, **not** a
  full table scan and **not** an absent index.
- **C2. The query ships ~1M rows to Go and aggregates in app memory.** Dedup
  (`claude_message_id` + `claude_request_id`) and timezone-local-date bucketing
  are done in Go (~0.7s), which is why this is not a SQL `GROUP BY`. On the
  cached machine the residual ~3s all-time cost is this CPU-bound row
  processing, not I/O — which is why a covering index alone does not move
  all-time wall-clock and only C2 (push counts/aggregation into SQL) would.

### Group D — Perceived performance (UX)

- **D1. No loading indicators.** `loading.{summary,topSessions}` is set only on
  first load (data === null); refreshes replace data in place with no feedback.
- **D2. The two heavy fetches are serial for no real reason.** `fetchAll` awaits
  `fetchSummary`, then awaits `fetchTopSessions(summary.params)`, then fires
  `fetchComparison` fire-and-forget (not awaited) (usage.svelte.ts:394).
  top-sessions only needs the filter params, not the summary result, so the
  serial dependency between the two ~3s scans is artificial and roughly doubles
  time-to-complete.
- **D3. No staleness signal.** With auto-refresh removed (A1), the user needs to
  know how old the data is and when new data exists.

## 4. Goals / non-goals

Goals:

- Usage page does no background work while idle (no CPU hammer).
- Usage page never fetches the sidebar index.
- Sidebar index is fetched at most once per filter signature, with superseded
  requests cancelled.
- Clear loading + staleness feedback; the two heavy panels fill independently.
- Sidebar payload is small and bounded regardless of history size (Phase 2).
- SQLite and PostgreSQL/Cockroach stay at parity (observable behavior + query
  shape) per AGENTS.md.

Non-goals (this effort):

- Reworking the analytics/trends pages.
- Changing dedup/cost/timezone semantics (must remain identical; guarded by
  existing tests).
- A pre-aggregated rollup table or a result cache (previously rejected as
  over-complicated).

## 5. Proposed improvements

Each maps to a problem above.

### A — Stop wasted work

- **A1 → manual refresh only.** Remove the SSE subscription and the 5-minute
  interval `fetchAll` from `UsagePage`. Keep user-initiated fetches (date/window
  change, project/model toggle, refresh button). Result: an idle usage page runs
  zero queries.
- **A2/A3 → gate the global sessions load.** The sessions live-refresh + 30 MB
  `load()` runs only when the sidebar is actually mounted (sessions route). The
  gate lives in/near the sessions store so it also covers the
  `sync.onSyncComplete` path and internal callers, not only the `App.svelte`
  route effect. The usage page keeps the cheap `projects`/`agents` metadata it
  needs for its filter dropdowns but never fetches the index.
- **A4 → coalesce + cancel.** Ensure at most one in-flight sidebar-index request
  per filter signature: dedupe identical concurrent `load()` calls onto one
  promise, and abort a superseded in-flight request (`AbortController`) so its
  30 MB never finishes downloading/parsing.

### B — Shrink the sidebar payload (Phase 2)

- **B1/B2 → bound the payload.** Paginate/window the index server-side (recent N
  - load-more/search) with client-side virtualization so it is never 30 MB, and
    trim per-row fields to what the list actually renders. Deferred to Phase 2
    (see D-2): the windowing has to preserve current grouping, parent/child
    threading, and active-session reveal behavior, which needs its own design
    pass.

### C — Faster queries

- **C1 → widen the existing index to cover the read columns.** Extend
  `idx_messages_usage_timestamp` (db.go:773) so it covers `model`,
  `claude_message_id`, `claude_request_id`, and `token_usage` in addition to
  `timestamp, session_id, ordinal`, keeping the same partial `WHERE`. This
  targets table I/O / index-only reads; temp-sort removal is a separate
  verification item and may require query-shape changes (the `UNION` +
  `COALESCE(u.ts, ...)` ordering is not satisfiable by this index alone).
  Measured wall-clock benefit on the cached machine is marginal (see §2);
  include it for the structural benefit and validate in the D-3 re-measure.
  Apply the equivalent widening to the PostgreSQL/Cockroach schema for parity.
- **C2 → sub-second all-time (deferred).** To remove the residual CPU-bound ~3s
  (gjson parse + Go dedup + bucketing over ~1M rows): extract token counts into
  integer columns (`input_tokens`, `cache_creation_tokens`, `cache_read_tokens`;
  `output_tokens` exists) and push aggregation/dedup into SQL. Requires a resync
  backfill and careful preservation of dedup + timezone semantics; higher risk.
  Out of scope this round per D-1.

### D — Perceived performance

- **D1 → per-panel loading.** Show a skeleton when a panel has no data (first
  load) and a subtle in-place indicator on refresh (keep old numbers visible).
  Add a small shared loading/skeleton component (none exists today).
- **D2 → parallel + progressive.** Run `fetchSummary` and `fetchTopSessions`
  concurrently (compute the shared filter params once instead of threading them
  through the summary result); each panel fills as its data returns; comparison
  continues to fold in fire-and-forget. The live path today is serial — summary
  (~3.0–3.6s) then top (~3.0–3.6s) ≈ ~6–7s before both panels are populated;
  running them concurrently bounds it by the slower scan (≈ ~3.0–3.6s, the
  4-conn pool has spare capacity for two heavy scans plus the cheap comparison),
  with content appearing incrementally.
- **D3 → staleness UI.** Add `lastUpdated` to the usage store; show "Updated X
  ago" next to the refresh button (a 30–60s ticker updates only the label, no
  fetch). On an SSE event indicating sessions changed, show a subtle "new data —
  refresh" hint on the button (no auto-fetch).

### W — Wire transfer (not memory)

- **W1 → gzip API responses.** Add response compression (server middleware).
  This is a wire-transfer win only: it does **not** reduce server marshal CPU,
  client parse time, or client heap/hydration cost — those are B1/B2. Sidebar 30
  MB → ~3 MB on the wire; also shrinks the summary payload. Cheap and broad, and
  useful even after the usage tab stops fetching the index (it still helps the
  sessions route and the summary endpoint). Backend-agnostic.

## 6. Decisions (locked)

- **D-1 (query depth): covering index only this round.** Widen the existing
  index (C1); do **not** do C2 (integer-column extraction + SQL aggregation)
  now.
- **D-2 (sidebar this round): gzip + stop the usage-tab sidebar fetch +
  coalesce/cancel sidebar loads.** Pagination/virtualization and smaller rows
  (B1/B2) move to Phase 2.
- **D-3 (phasing): ship Phase 1 = A + D + gzip (W1) + widened covering index
  (C1) + sidebar load coalescing (A4). Re-measure, then decide** whether C2 or
  sidebar pagination is still worth the added complexity.

## 7. Phasing

**Phase 1 (this effort):**

- A1 — manual refresh only on the usage page.
- A2/A3 — gate the global sessions `load()` to the sessions route, in the store.
- A4 — coalesce + cancel sidebar-index requests (one per filter signature).
- D1 — per-panel loading/skeleton.
- D2 — parallel summary + top-sessions; progressive fill.
- D3 — staleness "Updated X ago" + new-data hint.
- C1 — widened covering index (SQLite + PG/Cockroach parity).
- W1 — gzip API responses.

**Phase 2 (deferred, decided after the Phase 1 re-measure):**

- B1/B2 — sidebar pagination/windowing + virtualization + smaller rows.
- C2 — integer token columns + SQL aggregation for sub-second all-time.

## 8. Backend parity

Any query/index/schema change (C1 now, C2 later) must be applied to both SQLite
(`internal/db`) and PostgreSQL/Cockroach (`internal/postgres`) with matching
access patterns and identical observable results, per the AGENTS.md "Backend
Parity" rule. Gzip (W1) and all frontend changes (A, D) are backend-agnostic.

## 9. Verification

Reuse the harnesses built during diagnosis:

- Go: `internal/db/usage_perf_test.go` (gated by `REAL_DB`) — times each query
  isolated + concurrent, before/after. Capture `EXPLAIN QUERY PLAN` for the
  usage query before/after the C1 widening to confirm the covering index is used
  and the table lookups become index-only reads. Do not require the temp B-tree
  sort to disappear: with the current `UNION` + `COALESCE(u.ts, ...)` shape it
  may remain, and removing it is a separate, optional query-shape change — only
  assert its removal if the implementation explicitly reshapes the query to that
  end.
- Browser: Playwright script — networkidle, long tasks, per-request transfer
  sizes; assert no background fetches while idle on the usage tab, no
  sidebar-index request on the usage tab, exactly one sidebar request per filter
  change on the sessions route, and progressive panel fill on manual refresh.
- Targets: idle usage page = 0 queries; usage tab = 0 sidebar fetches; all-time
  manual refresh ~3.0–3.6s (down from ~6–7s serial) with panels filling
  independently; sidebar payload ~3 MB on the wire (gzip); re-measure C1
  wall-clock to decide on Phase 2.
