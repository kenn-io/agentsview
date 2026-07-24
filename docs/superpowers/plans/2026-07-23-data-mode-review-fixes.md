# Data Mode Review Fixes Implementation Plan

> **For Codex:** Follow the repository's test-first and verification rules for
> each task. Keep the final implementation in one focused follow-up commit.

**Goal:** Resolve every verified PR #1166 review finding and restore merge
readiness without changing Data mode's core architecture.

**Architecture:** Preserve the existing SQLite/PostgreSQL/DuckDB shared data
models and candidate pipeline. Extend inventory identity metadata to retain
merged keys, keep filtered PostgreSQL publications privacy-safe, and reuse the
incremental append's existing session read.

**Tech stack:** Go, SQLite, PostgreSQL, DuckDB, Svelte 5, TypeScript, Paraglide,
Vitest.

______________________________________________________________________

### Task 1: Lock down the critical regressions

**Files:**

- Modify: `internal/postgres/push_pgtest_test.go`
- Modify: `internal/db/orphaned_test.go`
- Modify: `internal/db/worktree_mappings_test.go`
- Modify: `internal/sync/engine_bench_test.go`

1. Add or tighten tests that reproduce the PostgreSQL INSERT failure, the v70
   snapshot overwrite, normalized persisted paths, and incremental allocation
   regression.
1. Run each focused test or benchmark and confirm the expected failure.
1. Repair the placeholder sequence and upgrade gate.
1. Normalize cross-platform expectations.
1. Remove the duplicate incremental session read and rerun the benchmark.

### Task 2: Preserve merged project selection

**Files:**

- Modify: `internal/db/project_inventory.go`
- Modify: `internal/db/project_inventory_test.go`
- Modify: `internal/db/worktree_candidates.go`
- Modify: `internal/db/worktree_candidates_test.go`
- Modify: PostgreSQL and DuckDB parity implementations/tests as needed
- Regenerate: frontend API models
- Modify: `frontend/src/lib/stores/data.svelte.ts`
- Modify: `frontend/src/lib/stores/data.test.ts`

1. Add tests for colliding display labels with distinct project keys and a deep
   link through a non-canonical key.
1. Add `project_keys` while retaining the canonical `project_key`.
1. Select candidate sessions for every key in a validated merged row.
1. Update frontend row resolution and refresh behavior.
1. Run backend parity and frontend store/component tests.

### Task 3: Repair publication scope and query bounds

**Files:**

- Modify: `internal/postgres/worktree_mappings_push.go`
- Modify: `internal/postgres/worktree_mappings_push_pgtest_test.go`
- Modify: `internal/db/project_inventory.go`
- Modify: `internal/db/project_inventory_test.go`
- Modify: `internal/db/project_identity.go`
- Modify: `internal/db/project_identity_test.go`

1. Add a filtered-push test proving unrelated mapping paths are not published.
1. Skip mapping publication for filtered pushes.
1. Add cardinality tests over backend bind limits.
1. Chunk machine candidate reads and aggregate rebuilds transactionally.
1. Run SQLite and PostgreSQL focused suites.

### Task 4: Align behavior and error handling

**Files:**

- Modify: `internal/db/worktree_candidates.go`
- Modify: `internal/db/worktree_reclassification.go`
- Modify: `internal/server/huma_routes_settings.go`
- Modify: associated Go tests

1. Add tests for zero-message candidates, writer-closed preview, legacy update
   duplicate handling, and token changes.
1. Align session predicates to visible sessions.
1. Map writer closure and uniqueness errors correctly.
1. Include `original_project` in tokens and split the evaluator helper.
1. Run focused DB and server tests.

### Task 5: Surface stale inventory and fix localized copy

**Files:**

- Modify: `frontend/src/lib/stores/data.svelte.ts`
- Modify: `frontend/src/lib/components/data/DataPage.svelte`
- Modify: associated frontend tests
- Modify: all `frontend/messages/*.json`

1. Add a component regression test for a failed foreground reload after a
   successful load.
1. Render the error and retry control above retained stale inventory.
1. Replace the Activity-scoped no-candidate text with archive-wide wording in
   every locale.
1. Run `npm run i18n:compile`, focused Vitest tests, and `npm run check`.

### Task 6: Finish parity and maintenance cleanup

**Files:**

- Modify: inventory timestamp handling and tests across backends
- Modify: PostgreSQL mapping ordering and parity tests
- Modify: stale comments identified by review

1. Add malformed-timestamp and deterministic-order regressions.
1. Make SQLite and PostgreSQL observable behavior match.
1. Correct stale bind-limit and collation comments.
1. Run focused parity suites.

### Task 7: Verify and publish

1. Run Go formatting and vet.
1. Run targeted and broad Go tests, PostgreSQL integration tests, frontend
   tests/checks, and benchmark comparison.
1. Inspect the final diff and run the private-data scrub.
1. Use the commit workflow to create one conventional follow-up commit.
1. Push the current branch.
1. Replace the PR body with a concise rationale-first summary with no test
   section or checklist.
