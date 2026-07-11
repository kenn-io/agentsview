# Passive Daemon Memory Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Reduce passive daemon physical memory and eliminate archive-byte work
from warm scheduled syncs without weakening source-change detection or sync
throughput.

**Architecture:** Shrink per-connection SQLite caches, replace full-row Codex
title reads with a scalar query, make matching-hash automation audits two-phase,
and add a pruned in-memory stat/ctime trust gate for local Claude and Codex
sources. Cold starts and all invalidated or remote sources retain the existing
content-verification path.

**Tech Stack:** Go, `database/sql`, SQLite/FTS5, PostgreSQL/pgx, filesystem stat
APIs, testify, Go pprof, macOS `vmmap`/`heap`.

______________________________________________________________________

## File map

- `internal/db/db.go`: SQLite DSN cache target and reader idle lifetime.
- `internal/db/db_test.go`: real-connection pragma and pool behavior tests.
- `internal/db/automated.go`: shared bounded-prefix automation verdict helper.
- `internal/db/automated_audit.go`: SQLite matching-hash two-phase audit.
- `internal/db/automated_backfill_test.go`: audit correctness and allocation
  regressions.
- `internal/db/sessions.go`: scalar session-name lookup.
- `internal/db/sessions_test.go`: lookup missing/null/value behavior.
- `internal/postgres/schema.go`: PostgreSQL two-phase audit parity.
- `internal/postgres/automated_pgtest_test.go`: PostgreSQL audit behavior
  parity.
- `internal/parser/capabilities.go`: declarative verified-stat gate capability.
- `internal/parser/claude_provider.go`, `codex_provider.go`: capability opt-in.
- `internal/parser/provider_test.go`: capability default and provider
  declarations.
- `internal/sync/verified_source_gate.go`: compact trust record, promotion,
  invalidation, pass pruning, and entry accounting.
- `internal/sync/change_time_darwin.go`, `change_time_linux.go`,
  `change_time_windows.go`, `change_time_other.go`: platform change-time
  extraction.
- `internal/sync/verified_source_gate_internal_test.go`: trust-state and race
  invariants.
- `internal/sync/engine.go`: gate capture/use/promotion and lifecycle hooks.
- `internal/sync/engine_test.go`, `engine_integration_test.go`: warm
  cardinality, same-stat rewrite, changed path, overflow, deletion/tombstone,
  remote bypass, and Codex title behavior.

### Task 1: Re-baseline current main and reduce SQLite connection residency

**Files:**

- Modify: `internal/db/db.go`

- Test: `internal/db/db_test.go`

- [ ] **Step 1: Preserve the current-main performance baseline**

Run the `354a2164d` scratch binary against the isolated production-scale clone.
Capture startup allocation/heap/`vmmap` and the 15-minute unchanged scheduled
sync duration and allocation delta under
`/tmp/agentsview-profile.PXkwqx/baseline-354a/`.

- [ ] **Step 2: Write a failing real-connection cache test**

Add a table-driven test that opens a production DB through `Open` and
`OpenReadOnly`, obtains a connection, and queries `PRAGMA cache_size`. Assert
the agentsview connection contract is `-8192`, not the driver's default. Query
`PRAGMA mmap_size` and assert only that opening remains valid; do not test the
upstream driver's URI parsing.

- [ ] **Step 3: Run the targeted test and verify RED**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db -run TestSQLiteConnectionMemoryPragmas -count=1
```

Expected: fail because the current cache size is `-64000`.

- [ ] **Step 4: Implement the cache and idle-lifetime settings**

Define named constants for an 8 MiB cache and a five-minute reader idle
lifetime. Set `_cache_size=-8192`, remove the ineffective `_mmap_size` URI
argument, and call `SetConnMaxIdleTime` on every reader pool creation path
(`OpenReadOnly`, `openAndInit`, and `reopenLocked`). Keep `SetMaxOpenConns(4)`.

- [ ] **Step 5: Verify GREEN and benchmark representative queries**

Run the targeted test, existing DB tests, and production-clone cold/warm session
list, search, and automation-audit timing probes at both 8 MiB and the recorded
64 MiB baseline. If any representative median regresses by more than 5%, repeat
at 16 MiB and document the selected value.

- [ ] **Step 6: Commit the focused connection-memory change**

Use the mandatory commit workflow and commit only `internal/db/db.go` and its
test with a `perf(db): ...` subject.

### Task 2: Replace full Codex session loads with a scalar name lookup

**Files:**

- Modify: `internal/db/sessions.go`

- Modify: `internal/sync/engine.go`

- Test: `internal/db/sessions_test.go`

- Test: `internal/sync/engine_test.go`

- [ ] **Step 1: Write failing lookup contract tests**

Add table cases for a missing session, a row with `NULL session_name`, and a row
with a stored name. The desired API returns
`(name string, found bool, err error)` so missing and null both compare as an
empty current name while query errors remain observable.

- [ ] **Step 2: Verify RED**

Run the named DB test. Expected: compile failure because the scalar API does not
exist.

- [ ] **Step 3: Implement the minimal DB query**

Add `GetSessionName(ctx, id)` selecting only `session_name` from `sessions`. Use
`sql.NullString`, translate `sql.ErrNoRows` to `found=false`, and retain other
errors.

- [ ] **Step 4: Replace the Codex caller and verify behavior**

Change both `codexIndexSessionNameChanged` and
`codexStoredNameDiffersBySessionID` to call the scalar API. Preserve their
different missing-row behavior (`true` for the direct refresh probe when
requested, `false` for unknown index-only sessions), ID prefixing, and path
rewriting. Run the existing Codex title, path-rewrite, cache, and title-only
refresh tests plus the new DB test.

- [ ] **Step 5: Commit the focused lookup change**

Commit the DB method, engine caller, and tests with a `perf(sync): ...` subject.

### Task 3: Bound matching-hash SQLite automation audit transfer

**Files:**

- Create: `internal/db/automated_audit.go`

- Modify: `internal/db/automated.go`

- Modify: `internal/db/db.go`

- Test: `internal/db/automated_backfill_test.go`

- [ ] **Step 1: Write verdict-helper tests first**

Define the desired helper contract as `(matched, conclusive bool)` over a byte
prefix and full byte length. Add literal cases for built-in and user prefixes,
substrings inside the prefix, exact complete messages, an exact string followed
by an unseen suffix, Unicode whitespace, and a substring located beyond the
prefix. The latter cases must return `conclusive=false`.

- [ ] **Step 2: Verify RED, then implement the shared helper**

Run the named helper test and confirm it fails because the API is missing.
Implement it from the same pattern snapshots used by `IsAutomatedSession`; do
not reproduce patterns or matching policy in SQL.

- [ ] **Step 3: Write a failing matching-hash audit allocation regression**

Seed real SQLite rows with large prefix-matching automated prompts, large human
prompts, a substring beyond the bounded prefix, a stale positive, and a stale
negative. Stamp the current classifier hash. Use `testing.Benchmark` around the
audit and assert allocated bytes grow with bounded evidence plus unresolved
rows, not the large prefix-matching prompt bytes. Also assert every persisted
flag.

- [ ] **Step 4: Verify RED against the current full-text scan**

Run only the new audit test with `-count=1`. Expected: behavior passes but the
allocation ceiling fails because the current query copies every full prompt.

- [ ] **Step 5: Implement the SQLite two-phase audit**

Keep the current full scan as the hash-mismatch/recovery path. For a matching
hash, query ID, count, current flag, bounded BLOB prefixes, and complete byte
lengths. Resolve conclusive rows with the shared helper. Fetch unresolved IDs in
bounded batches with the existing full-text query and pass them through
`isAutomatedFromTextCandidates`. Apply the existing batched set/clear updates
and stamp the hash only after success.

- [ ] **Step 6: Verify behavior and allocations**

Run every `automated_backfill` test, the new allocation regression, and the full
`internal/db` suite. Mentally mutate prefix completeness, substring fallback,
and stale-positive handling and confirm named tests would fail.

- [ ] **Step 7: Commit the SQLite audit optimization**

Commit the helper, audit implementation, and tests with a `perf(db): ...`
subject.

### Task 4: Preserve PostgreSQL automation-audit parity

**Files:**

- Modify: `internal/postgres/schema.go`

- Test: `internal/postgres/automated_pgtest_test.go`

- [ ] **Step 1: Write PostgreSQL parity cases**

Add real-PG cases matching Task 3: prefix match, substring beyond prefix, exact
with Unicode whitespace, multi-turn stale positive, and matching-hash stale
negative. Assert final flags, not SQL shape.

- [ ] **Step 2: Verify RED where the two-phase seam is absent**

Add a small test-visible progress/result struct returned by the internal audit
helper (rows prefetched, rows requiring full text) and assert prefix-heavy rows
avoid phase two. The existing PG audit should fail that assertion.

- [ ] **Step 3: Implement PostgreSQL two-phase querying**

Use PostgreSQL `left` and `octet_length` for bounded evidence, feed the same Go
helper, and fetch unresolved IDs in bounded placeholder batches. Keep the
unconditional matching-hash integrity audit so stale client pushes remain
repairable. Preserve update batching and metadata stamping.

- [ ] **Step 4: Run PG integration and local schema tests**

Run the focused test with `pgtest`, then `make test-postgres` if the dedicated
test database is available. Run non-PG schema unit tests regardless.

- [ ] **Step 5: Commit the PG parity change**

Commit only PostgreSQL audit code and tests with a `perf(postgres): ...`
subject.

### Task 5: Build the compact verified-source trust primitive

**Files:**

- Create: `internal/sync/verified_source_gate.go`

- Create: `internal/sync/change_time_darwin.go`

- Create: `internal/sync/change_time_linux.go`

- Create: `internal/sync/change_time_windows.go`

- Create: `internal/sync/change_time_other.go`

- Create: `internal/sync/verified_source_gate_internal_test.go`

- Modify: `internal/parser/capabilities.go`

- Modify: `internal/parser/claude_provider.go`

- Modify: `internal/parser/codex_provider.go`

- Test: `internal/parser/provider_test.go`

- Modify: `internal/sync/engine.go`

- [ ] **Step 1: Write failing trust-state tests**

Describe the desired API through tests: capture/promotion makes the same
signature fresh; a per-path invalidation or global clear vetoes stale promotion;
unrelated invalidation does not; a new full-pass generation marks seen entries
and prunes unseen entries; one map record carries trust and invalidation state.

- [ ] **Step 2: Verify RED, then implement the in-memory primitive**

Add the compact record and mutex-protected map to `Engine`. Follow the existing
OpenCode gate's capture-before-parse snapshot ordering. Do not integrate it into
file processing yet.

- [ ] **Step 3: Declare the provider capability**

Add a source capability for verified local stat trust. Its zero value remains
unsupported. Enable it only in Claude and Codex capability declarations and add
provider tests proving the default and the two opt-ins.

- [ ] **Step 4: Write platform change-time tests/build checks**

On Unix, write a file, capture change time, rewrite it, restore mtime, and
assert change time differs. Add compile-time platform files: Darwin uses
`Ctimespec`, Linux uses `Ctim`, Windows uses
`GetFileInformationByHandleEx(FileBasicInfo)`, and other targets return
unavailable.

- [ ] **Step 5: Verify native and cross-platform compilation**

Run the native tests and compile the sync package for Linux and Windows with
`CGO_ENABLED=0 go test -c` where build constraints permit. Fix only platform
API/compile issues; do not weaken unavailable-change-time fallback.

- [ ] **Step 6: Add the retained-entry budget regression**

Populate small and 47,000-entry gates with representative paths and signatures,
measure retained allocation with a benchmark helper, and enforce the 16 MiB
large-set budget. Verify pruning returns retained state to the small active set.

- [ ] **Step 7: Commit the trust primitive and capability**

Commit the compact state, platform helpers, provider capability declarations,
and their unit tests with a `perf(sync): ...` subject. Engine processing remains
behaviorally unchanged until Task 6.

### Task 6: Integrate the gate into Claude and Codex sync

**Files:**

- Modify: `internal/sync/engine.go`

- Modify: `internal/sync/verified_source_gate.go`

- Test: `internal/sync/engine_test.go`

- Test: `internal/sync/engine_integration_test.go`

- [ ] **Step 1: Write the warm cardinality regression**

Use a provider boundary spy or counted reader around real local files. Compare a
small and large unchanged archive across two full passes. First pass must deep
verify every source; second pass must perform zero Claude/Codex content
fingerprints for both cardinalities. Assert persisted sessions and skip counts,
not only call order.

- [ ] **Step 2: Verify RED**

Run the focused regression. Expected: second-pass fingerprint calls scale with
the archive because no gate is consulted.

- [ ] **Step 3: Add pre-fingerprint capture and trusted skip**

Gate only local, non-force-parse regular files whose provider declares the
verified-stat capability and whose change time is reliable. Include Codex
effective mtime in its signature. On a trusted match, return the same skip
result the current verified DB path returns. On a miss, execute the current hash
and DB checks and promote only after a clean verified skip.

- [ ] **Step 4: Wire lifecycle invalidation and pruning**

Invalidate resolved sources during changed-path classification. Clear the gate
on watcher overflow and resync. Begin/end a generation around each full pass,
mark processed gateable paths, and prune unseen entries. Remote/path-rewritten
and unavailable-change-time sources bypass trust.

- [ ] **Step 5: Preserve same-stat and event behavior**

Run existing same-size/same-mtime in-place rewrite tests. Add or extend tests
for watcher invalidation, invalidation racing promotion, watcher overflow,
deleted sources, tombstones, force parse, resync, Codex index-only title
changes, and remote materializations. Each must observe parsed/persisted
behavior.

- [ ] **Step 6: Verify the sync suites and benchmark**

Run focused tests, all `internal/sync` tests, and Codex benchmarks. Warm sync
must improve; cold sync must remain within 5% of current-main baseline.

- [ ] **Step 7: Commit the verified-source gate**

Commit the gate, platform helpers, integration, and tests with a
`perf(sync): ...` subject.

### Task 7: Full validation and production-scale profiling

**Files:**

- Modify only if verification exposes a defect.

- [ ] **Step 1: Run repository checks**

Run `go fmt ./...`, `go vet ./...`, `make test-short`, the full relevant Go
tests, and `make lint`. Run PostgreSQL integration tests when the dedicated
container is available.

- [ ] **Step 2: Build and run only in scratch**

Build the branch binary to a new `/tmp` path. Copy the isolated baseline DB with
APFS clone semantics, keep the existing isolated source clones, and start with
its own `HOME`, `AGENTSVIEW_DATA_DIR`, port, and pprof endpoint. Never install
or restart the live daemon.

- [ ] **Step 3: Repeat the retention profile**

Capture startup allocations, CPU, live heap, forced-GC heap, C heap, `ps`, and
`vmmap`. Bracket the 15-minute scheduled sync and continue through 20 minutes.
Compare startup, warm sync, and retained physical/dirty memory directly to the
`354a2164d` baseline.

- [ ] **Step 4: Decide whether hash-buffer pooling remains material**

Inspect the post-gate allocation delta. Only if `io.copyBuffer` remains a
material warm-sync owner, add a pooled shared hash helper with a failing
allocation test and repeat the profile. Otherwise leave it unchanged.

- [ ] **Step 5: Run private-data scrub and final commit audit**

Verify no scratch paths, archive content, credentials, or personal identifiers
entered tracked files or commit messages. Confirm every tracked change is
committed, the worktree is clean, and the live daemon still reports the
installed `0cb491f8e`/`354a2164d` lineage rather than the branch binary.
