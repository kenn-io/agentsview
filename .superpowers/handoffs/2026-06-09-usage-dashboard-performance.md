# Handoff — Usage Dashboard Performance (Phase 1)

**Date:** 2026-06-09 **Branch:** `fix/slow-usage-dashboard` **Status:** Spec
approved ✓ · Plan written & approved to implement ✓ · Implementation **not
started** (0/9 tasks).

## TL;DR for resuming on another machine

1. Read `.superpowers/specs/2026-06-09-usage-dashboard-performance.md` (the
   approved spec).
1. Read `.superpowers/plans/2026-06-09-usage-dashboard-performance.md` (the
   approved, no-placeholder implementation plan — 9 TDD tasks with full code).
1. Pick an execution mode (subagent-driven recommended) and start at **Task 1**.

Nothing in the plan has been implemented yet — no `internal/db`,
`internal/postgres`, `internal/server`, or `frontend/` source has been changed
for Phase 1. The only code artifact is the measurement harness (see below).

## What's in this commit

| Path                                                              | What it is                                                            | Tracked how |
| ----------------------------------------------------------------- | --------------------------------------------------------------------- | ----------- |
| `.superpowers/specs/2026-06-09-usage-dashboard-performance.md`    | Approved spec                                                         | force-added |
| `.superpowers/plans/2026-06-09-usage-dashboard-performance.md`    | Approved plan                                                         | force-added |
| `.superpowers/handoffs/2026-06-09-usage-dashboard-performance.md` | This file                                                             | force-added |
| `internal/db/usage_perf_test.go`                                  | Read-only perf/payload harness, gated by `REAL_DB` (skips without it) | normal      |

Note: `.superpowers/` is gitignored (`.gitignore:58`). The three docs above were
committed with `git add -f` purely to transfer this work between machines. After
you've pulled on the other machine you can leave them tracked, or run
`git rm --cached <path>` to return them to ignored status — they don't belong in
the eventual PR.

## Phase 1 scope (full detail in the plan)

9 tasks, each independently committable, TDD:

1. C1 — SQLite covering index `idx_messages_usage_covering` + migration
   (`db.go`).
1. C1 — PG/Cockroach covering index (parity) (`schema.go`), `pgtest`.
1. W1 — gzip response middleware, SSE-bypass (`compress.go` + `Handler()`).
1. A2/A3 — gate `sessions.load()` to the sessions route, in the store
   (`sessions.svelte.ts`, `App.svelte`).
1. A4 — coalesce + cancel in-flight sidebar requests (`sessions.svelte.ts`).
1. D1/D3 — usage store `lastUpdated`/`hasNewData`/`busy`/`markStale`
   (`usage.svelte.ts`).
1. D2 — parallel summary + top-sessions (`usage.svelte.ts`).
1. A1/D1/D3 — manual-refresh usage page + staleness UI (`UsagePage.svelte`).
1. Verification / re-measure (harness, EXPLAIN, Playwright).

Dependencies: backend (1–3) and frontend (4–8) are independent. Within frontend:
4 before 5 (both edit `load()`); 6 before 8 (8 uses `markStale`/`lastUpdated`/
`busy`). If you want to feel the CPU-hammer fix soonest, do 4 → 8 first.

## Locked decisions (do not relitigate)

- **D-1:** covering index only this round; no C2 (integer token columns + SQL
  aggregation).
- **D-2:** sidebar this round = gzip + stop usage-tab sidebar fetch +
  coalesce/cancel. Pagination/virtualization + smaller rows are Phase 2.
- **D-3:** ship Phase 1 (A + D + gzip + widened covering index + coalescing),
  re-measure, then decide whether C2 or sidebar pagination is still worth it.

## Intentional backend divergence (documented, keep)

The PG/Cockroach covering index **omits `token_usage`** from its key — a
PG/Cockroach btree tuple cannot safely hold an unbounded TEXT blob; SQLite has
no such limit and includes it. Observable query results are identical
(`token_usage` is still read from the heap in PG). See plan Task 2 and spec
§5/§8. This is the only place the two backends differ.

## Honest caveat carried from the spec

On the user's 137 GB-RAM machine the DB is fully cached, so the **covering
index's all-time wall-clock benefit is marginal** (warm all-time stays ~3s with
or without it; the residual is CPU-bound Go processing). Its value here is
structural (no table I/O, bounded ~590 MB working set). The clearly-measured
Phase-1 wins are the frontend ones + gzip. Re-measure in Task 9 to decide Phase
2\.

## Hard constraints to preserve (from AGENTS.md / user)

- The SQLite DB is a persistent archive: **never delete/drop/truncate/recreate**
  it. Never mutate `~/.agentsview/sessions.db` — read-only opens or copies only.
- **Backend parity** (SQLite ↔ PostgreSQL/Cockroach) for any query/index/schema
  change unless a divergence is documented (as above).
- Commit every turn; never `--amend`; don't change branches without permission;
  never `--no-verify`. Conventional commit messages, **no** attribution/
  generated-with/footer lines. After Go changes run `go fmt ./...` + `go vet`.
- Write specs/plans/handoffs to `.superpowers/` (gitignored).
- Go tests use testify (`require`/`assert`), table-driven; `t.TempDir()`; the
  `testDB(t)` helper for DB tests.

## Measurement environment — recreate on the new machine

The diagnosis used machine-local artifacts that **do not transfer via git**:

- Prod DB: `~/.agentsview/sessions.db` (each machine has its own; the original
  has ~106K sessions / 1.47M messages).
- A 5.4 GB copy at `/tmp/agytest/sessions.db` with a covering index added, a
  server on `:8199` (`./agentsview serve --no-sync --port 8199`), and a
  Playwright script at `/tmp/agy_perf.js`. **None of these exist on the new
  machine** — recreate them only when you reach Task 9 (re-measure). If the new
  machine has no large real DB, implement Tasks 1–8 there and run Task 9 on a
  machine that has the prod data.

How to run things:

```bash
# Go unit tests (Phase 1 backend tasks)
make test
make lint
make vet

# Perf harness against a COPY of a real DB (never the live archive)
REAL_DB=/path/to/copy/sessions.db CGO_ENABLED=1 \
  go test -tags fts5 -run TestRealDBUsagePerf -v -timeout 1200s ./internal/db/
REAL_DB=/path/to/copy/sessions.db CGO_ENABLED=1 \
  go test -tags fts5 -run TestRealDBUsagePayload -v -timeout 600s ./internal/db/

# PG parity test (Task 2) — needs a Postgres instance
make test-postgres   # or set TEST_PG_URL and use -tags "fts5,pgtest"

# Frontend store tests (Tasks 4–7)
cd frontend && npx vitest run
cd frontend && npx tsc --noEmit && npx oxlint src/ && npm run build
```

## Verified baseline (for the Task 9 before/after comparison)

- sidebar-index: 92,221 rows / 30.71 MB (~333 bytes/row), uncompressed.
- usage all-time (warm, current schema): ~3.0–3.6s; 30-day ~0.99s; comparison
  ~0.3s; harness concurrent fan-out ~5.5s (includes a diagnostic counts scan not
  on the live path).
- Browser: cold usage load ~10.7s; 0 main-thread long tasks; sidebar fetched 3×
  (~90 MB) per load.

## Resume checklist

1. `git pull` on the new machine; confirm the three `.superpowers/` docs +
   `internal/db/usage_perf_test.go` are present.
1. Re-open the spec and plan.
1. Choose execution mode (subagent-driven per task with review between, or
   inline with checkpoints).
1. Start Task 1 (SQLite covering index). Commit each task as you go.
1. Defer Task 9 (re-measure) to a machine with real session data; record results
   into the spec §2 to drive the D-3 decision.
