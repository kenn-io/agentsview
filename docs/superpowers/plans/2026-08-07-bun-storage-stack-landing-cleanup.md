# Bun Storage Stack Landing Cleanup Implementation Plan

**Goal:** Close the confirmed correctness and ownership gaps in PRs #1343–#1347
without expanding the landing stack into the separately tracked incremental
archive-writer refactor.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-02-unified-bun-storage-design.md`, supplemented
by the user-provided review at `bun-storage-stack-review.html`.

**Architecture:** Put each fix on the branch that introduces the affected
contract, then cascade-rebase the upper branches. Shared `BunStore` methods keep
observable policy and composite snapshots; adapters own only lifecycle,
timestamp rendering, and narrowly scoped search/vector semantics.

**Execution:** Small branch-specific tasks may run in parallel in isolated
worktrees. Each worker commits only to its assigned branch; the stack is
restacked after every lower branch is complete.

**Tech Stack:** Go, Uptrace Bun, SQLite/FTS5, PostgreSQL, DuckDB/Quack, Kata,
`gh stack`.

## Global Constraints

- SQLite remains the persistent archive and must never be rebuilt or truncated.
- DuckDB remains a disposable mirror and Quack remains read-only.
- New or changed tests must assert observable behavior and must fail before the
  corresponding production change.
- Backend-specific FTS/vector SQL and ranking are allowed; shared filtering,
  hydration, identity, and public pagination remain canonical.
- The existing shipped migrations are immutable.
- RoboRev hooks stay snoozed while branch commits and restacks are in progress;
  unsnooze every affected branch after final verification.
- Kata parent `h26h` tracks landing work. Follow-up `h381` owns the remaining
  incremental archive-writer migration and does not block this stack.

______________________________________________________________________

### Task 1: Foundation timestamp and stamped-schema invariants (`pa87`)

**Files:**

- Modify: `internal/db/bunmodel/models.go`
- Test: `internal/db/bunmodel/registry_test.go`
- Modify: `internal/db/bun_schema.go`
- Test: `internal/db/legacy_schema_test.go`
- Test: `internal/db/bun_schema_test.go`

**Interfaces:**

- Consumes: shipped SQLite RFC3339 text and empty timestamp sentinels.

- Produces: `bunmodel.Timestamp.Scan`/`Value` persistence compatibility and a
  stamped-open path that validates without repair or full-table invariant
  scans.

- [x] Add literal tests asserting that scanning `""` yields the zero timestamp
  and `Value()` yields UTC RFC3339Nano text; run the focused bunmodel test and
  observe the expected failures on PR #1343.

- [x] Move the existing scanner/value correction into PR #1343 and rerun the
  focused bunmodel tests.

- [x] Add a stamped-archive test that deliberately replaces one canonical
  trigger, reopens the archive, expects a drift error, and verifies the
  trigger SQL was not repaired.

- [x] Split structural validation from one-time row invariants so convergence
  runs both while stamped reopen runs only structural, trigger, and index
  checks.

- [x] Run
  `go test -tags fts5 ./internal/db/bunmodel ./internal/db -run 'Timestamp|CommonSchema|StampedCommonSchema' -count=1`.

- [x] Commit on `t3code/bun-storage-foundation` with a focused conventional
  message.

### Task 2: Foundation Quack and adapter-boundary cleanup (`pa87`)

**Files:**

- Modify: `internal/duckdb/quack_bun.go`
- Test: `internal/duckdb/quack_bun_test.go`
- Modify: `docs/agents/storage.md`
- Modify: `docs/superpowers/specs/2026-08-02-unified-bun-storage-design.md`

**Interfaces:**

- Consumes: Bun-formatted Quack SQL (zero driver arguments).

- Produces: explicit errors for unsupported direct arguments and a closed list
  of sanctioned adapter seams.

- [x] Add a resolver test that calls `QueryContext` with a direct argument and
  expects an unused/unsupported argument error without forwarding the query.

- [x] Reject non-empty argument lists in `quackBunConn.QueryContext` and
  `QueryRowContext`; retain Bun-generated formatted SQL behavior.

- [x] Tighten the storage guide to lifecycle, schema/convergence, sync/metadata,
  timestamp rendering, connection-local commands/probes, and search/vector
  capabilities; remove the open-ended “query plans” allowance.

- [x] Reconcile the design text with the implemented split: session FTS may
  return hydrated session hits, while content/vector capabilities return
  stable coordinates for shared hydration; engine relevance ranking may differ
  where the engines expose genuinely different ranks.

- [x] Run `go test -tags fts5 ./internal/duckdb -run 'QuackBun' -count=1`.

### Task 3: Composite reads and dead PostgreSQL renderer (`asej`)

**Files:**

- Modify: `internal/db/bun_sessions.go`
- Test: `internal/db/bun_sessions_test.go`
- Modify: `internal/db/bun_messages.go`
- Test: `internal/db/bun_messages_test.go`
- Delete: `internal/postgres/usage.go`
- Delete or trim: `internal/postgres/usage_unit_test.go`
- Modify or delete: `internal/postgres/filter_usage_test.go`

**Interfaces:**

- Consumes: replayable `BunBackend.ConsistentView` callbacks.

- Produces: accepted-attempt-only session pages, sidebars, hydrated messages,
  and timing responses.

- [x] Add replaying-backend tests where the first and second stores contain
  different rows and assert only the second attempt is published.

- [x] Replace `view` with `consistentView` only for `ListSessions`,
  `GetSidebarSessionIndex`, message reads that hydrate tool rows, and
  `GetSessionTiming`.

- [x] Prove with `rg` that the PostgreSQL usage renderer has no production
  callers, then delete it and tests that only assert its unused SQL text.

- [x] Run
  `go test -tags fts5 ./internal/db ./internal/postgres -run 'BunStore.*(ListSessions|Sidebar|Messages|Timing)|Usage' -count=1`.

- [x] Commit on `t3code/bun-store-reads`.

### Task 3b: Bound transcript analytics memory (`asej`)

**Files:**

- Modify: `internal/db/bun_analytics.go` and/or `internal/db/bun_trends.go`
- Test and benchmark: focused analytics files

**Interfaces:**

- Consumes: the same complete candidate-session set and transcript content.

- Produces: identical signal and trend totals with bounded peak transcript
  retention.

- [x] Add a focused regression test and benchmark for a candidate set spanning
  multiple chunks.

- [x] Stream or reduce transcript content by bounded chunk without imposing a
  hard result limit or changing the effective analytics date range.

- [x] Run the affected analytics tests and benchmark, then commit separately on
  `t3code/bun-store-reads`.

### Task 4: Search capability cleanup (`4n8v`)

**Files:**

- Modify: `internal/duckdb/store.go`
- Test: focused DuckDB mirror/search contract test
- Modify: narrowly related `internal/db/bun_search_content.go`,
  `internal/db/bun_recent_edits.go`, and adapter capability definitions.

**Interfaces:**

- Consumes: canonical `(session_id, message_ordinal)` tool relationships and
  adapter-provided search/timestamp expressions.

- Produces: tool-only session hits independent of physical message IDs and no
  avoidable engine-name switches in common search methods.

- [x] Add a mirror-path test whose only matching text is in tool call/result
  content and observe failure with a deliberately absent physical message ID
  relationship where the schema permits it.

- [x] Join DuckDB session-search tool rows by session plus ordinal.

- [x] Replace literal `julianday(sort_ts)` selection with
  `TimestampOrderExpr("sort_ts")` and move system-prefix/portable search
  choices behind small adapter hooks or capabilities.

- [x] Preserve SQLite Unicode confirmation and engine-specific relevance ranks;
  document any remaining name switch that cannot be removed without enlarging
  the capability surface.

- [x] Run focused SQLite, PostgreSQL-unit, and DuckDB search tests and commit on
  `t3code/bun-search-unification`.

### Task 5: Canonical write fixes (`5bsh`)

**Files:**

- Modify: `internal/postgres/push.go` and focused tests
- Modify: `internal/db/pricing.go`, `internal/db/bun_usage.go`
- Test: `internal/db/bun_store_contract_external_test.go`
- Modify: `internal/db/usage_events.go` and focused tests

**Interfaces:**

- Consumes: guarded Bun update handles and canonical pricing/usage rows.

- Produces: collision-resistant ownership serialization, SQLite pricing writes
  through Bun, and chronological replication snapshots.

- [x] Add a lock-key test that uses session IDs sharing the first two digest
  bytes and asserts different full keys; move the existing full-digest helper
  into PR #1346.

- [x] Add SQLite to `RunPricingWriteContract`, then move the promoted public
  pricing write to `BunStore` using `CanonicalModelPricingRows` and
  `UpsertModelPricingRows` under `WriteArchive`.

- [x] Add a usage-event test with mixed RFC3339 offsets and assert chronological
  order; render SQLite ordering through its timestamp expression rather than
  text order.

- [x] Run focused db/PostgreSQL write tests and commit on
  `t3code/bun-write-unification`.

### Task 6: Restack, verify, and publish (`1rv0`)

**Files:**

- Modify: this plan’s checkbox state as work completes.
- External: PR descriptions #1343–#1347, summary-only and footer-preserving.

**Interfaces:**

- Consumes: one focused commit per branch.

- Produces: linear local/remote stack ready for human landing.

- [x] Create recovery refs, rebase each upper branch onto its updated parent,
  and resolve duplicate patches by preserving the lower introducing fix.

- [x] Run focused tests after each rebase, then `go fmt ./...`, `go vet ./...`,
  `make test-short`, relevant DuckDB-tagged tests, and PostgreSQL integration
  if the dedicated test container is available.

- [x] Treat the unrelated macOS FSEvents baseline timeouts separately; do not
  weaken them as part of storage cleanup.

- [ ] Update the #1347 body from “byte-identical” one-off comparison to
  contract-verified dollar parity and keep all five PR bodies aligned with the
  final diffs.

- [ ] Push only after verification, close completed Kata children with commit
  evidence, and unsnooze RoboRev on every affected branch.

## Self-review

- Spec coverage: landing correctness findings are assigned to their introducing
  layers; the incremental writer migration is explicitly owned by Kata `h381`.
- Scope exclusions: Store interface splitting, generated converters, registry-
  driven relationship walks, package splitting, and context retrofits are not
  landing cleanup.
- Test quality: every new behavior test has a concrete production mutation it
  catches; deleted-code absence is verified by compiler/search, not a test.
- Placeholder scan: no `TBD`, speculative compatibility shim, or unowned task
  remains.
