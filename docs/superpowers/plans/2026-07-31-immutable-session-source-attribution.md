# Immutable Session Source Attribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (- [ ]) syntax for tracking.

**Goal:** Finish PR #1170 as a filesystem-only source-label feature whose
machine attribution is immutable after first ingestion.

**Architecture:** Existing archive rows are authoritative for machine
attribution, including during refreshes and full database rebuilds. Source
configuration supplies a label only when a session is new. Comparison-only path
cleanup is separate from stored path spelling, and the already-merged DuckDB
implementation remains owned by main.

**Tech Stack:** Go, SQLite/FTS5, TOML configuration, filesystem-backed parser
providers, testify, Git, and Markdown.

## Global Constraints

- Preserve behavior and query-shape parity between SQLite and PostgreSQL where
  both backends participate; this change does not add PostgreSQL behavior.
- Never delete, recreate, or destructively migrate the persistent SQLite
  archive.
- Keep watcher and periodic sync work bounded by the changed batch.
- Keep session_sources filesystem-only and additive to legacy source arrays.
- Preserve native session-ID deduplication.
- Use testify for all new or modified Go assertions.
- Every behavior test must assert persisted or returned behavior and fail before
  its production fix is applied.
- Run go fmt ./... and go vet ./... after Go changes.
- Do not push or change branches as part of this local implementation.

______________________________________________________________________

### Task 1: Synchronize main and remove duplicate DuckDB work

**Files:**

- Merge: current origin/main
- Restore from origin/main: internal/duckdb/connect.go
- Restore from origin/main: internal/duckdb/probe.go
- Restore from origin/main: internal/duckdb/push.go
- Restore from origin/main: internal/duckdb/rebuild.go
- Restore from origin/main: internal/duckdb/schema.go
- Restore from origin/main: internal/duckdb/sync.go
- Restore from origin/main: internal/duckdb/sync_test.go

**Interfaces:**

- Consumes: merged PR #1302 on origin/main

- Produces: a branch with no DuckDB diff relative to its updated base

- [ ] **Step 1: Fetch and merge current main without changing branches**

  ```
    git fetch origin main
    git merge --no-commit --no-ff origin/main
  ```

  Inspect every conflict before resolving it.

- [ ] **Step 2: Resolve DuckDB files to origin/main**

  Use the origin/main versions of all seven DuckDB files. Resolve other
  conflicts by preserving current-main behavior plus the filesystem-source
  feature.

- [ ] **Step 3: Verify the DuckDB overlap is gone**

  ```
    git diff --exit-code origin/main -- internal/duckdb
  ```

  Expected: no output.

- [ ] **Step 4: Commit the synchronization**

  ```
    git commit -m "merge: sync machine source branch with main"
  ```

______________________________________________________________________

### Task 2: Protect immutable attribution with observable sync tests

**Files:**

- Modify: internal/sync/session_source_machine_test.go
- Modify: internal/sync/provider_process_test.go
- Modify: internal/db/source_path_hints_test.go
- Modify: cmd/agentsview/sync_worker_test.go when full-resync coverage belongs
  at the command boundary after the merge

**Interfaces:**

- Consumes: Engine.SyncAllSince, Engine.SyncPathsContext, provider-backed
  writes, and the full-resync archive-copy path

- Produces: regression coverage that reads persisted session rows and identity
  state after configured labels change

- [ ] **Step 1: Write or revise the active-session regression**

  Create a session under a root labeled oldbox, sync it, change the configured
  root label to newbox, modify the file, sync again, and assert the persisted
  session still reports oldbox. The production mutation this catches is
  assigning the currently configured label on an existing-row upsert.

- [ ] **Step 2: Run the active-session test and verify RED**

  ```
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync \
      -run 'Test.*Machine.*Immutable' -count=1
  ```

  Expected: FAIL because an existing write path still adopts newbox.

- [ ] **Step 3: Write or revise trash and full-resync regressions**

  Trash a labeled session, change its configured root label, and run both an
  ordinary source refresh and the command full-resync path. Assert the session,
  trash timestamp, project-identity snapshot, and observation retain oldbox. The
  production mutation this catches is any machine-only update during trash
  handling or orphan copying.

- [ ] **Step 4: Run the new cases and verify RED**

  ```
    CGO_ENABLED=1 go test -tags fts5 ./internal/sync ./cmd/agentsview \
      -run 'Test.*(Trashed|FullResync).*Machine' -count=1
  ```

  Expected: at least one assertion sees newbox before production cleanup.

- [ ] **Step 5: Confirm newly discovered sessions use the current label**

  After changing the configured label, add a second session and assert its
  stored machine is newbox while the first remains oldbox.

______________________________________________________________________

### Task 3: Remove continuous reattribution machinery

**Files:**

- Modify: internal/sync/engine.go
- Modify: internal/db/sessions.go
- Modify: internal/db/orphaned.go
- Modify: internal/db/schema.sql
- Modify: internal/db/source_path_hints_test.go
- Modify: internal/sync/session_source_machine_test.go
- Modify: internal/sync/provider_process_test.go

**Interfaces:**

- Removes: DB.UpdateTrashedSessionMachine

- Removes: DB.UpdateTrashedSessionMachineByPath

- Removes: DB.ListActiveSessionSourceOwnershipScopesAllMachinesPage

- Removes: idx_local_source_baselines_source

- Preserves: existing row machine values through upsert and archive copy

- [ ] **Step 1: Remove machine-only trash updates and their call sites**

  Delete both database methods and the engine calls that invoke them during
  cached-skip, provider, batch, and full-resync paths. Do not replace them with
  a different relabel mechanism.

- [ ] **Step 2: Remove the all-machines baseline query and index**

  Return watcher reconciliation to the machine-scoped ownership query. Remove
  query-plan tests that exist only for the private implementation, retaining
  behavior tests for move and delete tombstoning.

- [ ] **Step 3: Make full-resync archive copy preserve stored attribution**

  Remove snapshot and observation SQL whose only purpose is adopting a newly
  configured label. Ensure copied sessions remain authoritative before parser
  writes are considered.

- [ ] **Step 4: Run focused tests and verify GREEN**

  ```
    CGO_ENABLED=1 go test -tags fts5 \
      ./internal/db ./internal/sync ./cmd/agentsview \
      -run 'Machine|SourceOwnership|FullResync|SessionSource' -count=1
  ```

  Expected: PASS, including active, trash, full-resync, move, and delete cases.

______________________________________________________________________

### Task 4: Preserve path spelling and clear lint

**Files:**

- Modify: internal/config/config.go
- Modify: internal/config/config_test.go
- Modify: internal/sync/session_source_machine_test.go

**Interfaces:**

- Consumes: normalizeSessionSourceDir and sessionSourceComparisonKey

- Produces: stored trimmed path spelling plus cleaned comparison keys

- [ ] **Step 1: Add a failing platform-neutral spelling test**

  Pass a structured directory containing dot segments and assert
  ResolveSessionSources retains the trimmed input string while deduplicating it
  against an equivalent legacy root through the comparison key. The production
  mutation this catches is returning filepath.Clean(value) from
  normalizeSessionSourceDir.

- [ ] **Step 2: Run the config test and verify RED**

  ```
    CGO_ENABLED=1 go test -tags fts5 ./internal/config \
      -run 'Test.*SessionSource.*Spelling' -count=1
  ```

  Expected: FAIL because the stored structured source path is cleaned.

- [ ] **Step 3: Return validated spelling from normalization**

  Keep whitespace trimming and NUL/empty validation, but return value. Continue
  calling filepath.Clean only from sessionSourceComparisonKey.

- [ ] **Step 4: Replace repeated string concatenation in the sync test**

  Use one strings.Builder with Grow in
  TestMachineForPathUsesNormalizedRootSpecificity. This is test maintenance only
  and needs no new behavior test.

- [ ] **Step 5: Verify GREEN and lint**

  ```
    CGO_ENABLED=1 go test -tags fts5 ./internal/config ./internal/sync \
      -run 'SessionSource|MachineForPath' -count=1
    make lint
  ```

______________________________________________________________________

### Task 5: Align documentation and pull-request copy

**Files:**

- Modify: README.md
- Modify: docs/configuration.md
- Modify: docs/filesystem-sync.md
- Prepare: replacement body for PR #1170

**Interfaces:**

- Consumes: the immutable first-ingestion contract

- Produces: user guidance with no retroactive-relabel or DuckDB claim

- [ ] **Step 1: Rewrite user documentation**

  State that configuration changes affect only newly discovered sessions and
  that ordinary sync plus sync --full preserve stored labels. Describe
  retroactive relabeling as unsupported; do not promise a command.

- [ ] **Step 2: Draft the synchronized PR description**

  Summarize structured filesystem sources, additive configuration, immutable
  attribution, native-ID deduplication, and the S3 exclusion. Remove the DuckDB
  section and retroactive-rebuild language.

- [ ] **Step 3: Review prose for contradictions**

  Read the README, both guides, and draft together. Confirm every description of
  label changes and full sync states the same behavior.

______________________________________________________________________

### Task 6: Final verification and commit

**Files:** all files changed by Tasks 1-5

**Interfaces:**

- Produces: a clean, committed worktree ready for the PR author's fork

- [ ] **Step 1: Format and vet**

  ```
    go fmt ./...
    go vet ./...
  ```

- [ ] **Step 2: Run the Go suites**

  ```
    make test-short
    make test
  ```

- [ ] **Step 3: Run repository lint**

  ```
    make lint
  ```

- [ ] **Step 4: Inspect the final diff and private-data scrub**

  ```
    git diff --check
    git diff --stat origin/main...HEAD
    git diff origin/main...HEAD
  ```

  Confirm the changed lines contain no private paths, identities, or
  infrastructure names.

- [ ] **Step 5: Commit the completed cleanup**

  ```
    git add README.md docs cmd internal
    git commit -m "fix(sync): keep source attribution immutable"
  ```

- [ ] **Step 6: Report the handoff**

  Report commit IDs, tests run, remaining environmental limitations, and the
  fact that no push or PR mutation was performed.
