# Conversation-Unit Citations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking. Invoke the roborev-fix skill
> after Task 5 and after Task 9 and resolve all findings. After Task 9, run the
> final whole-branch review on the most capable model.

**Goal:** Every content-search match, in every mode and on every backend,
carries an `ordinal_range: [start, end]` conversation-unit citation plus
subordinate/lineage metadata, per
docs/superpowers/specs/2026-07-06-conversation-unit-citations-design.md.

**Architecture:** A pure Go derivation pass in `internal/db`
(`DeriveUnitRanges`) classifies each match's anchor message row and computes the
enclosing conversation unit through a backend-neutral two-method seam
(`UnitBoundsQuerier`) that each store implements with its own batched SQL.
`ContentMatch` swaps `OrdinalStart`/`OrdinalEnd` for an always-present
`OrdinalRange [2]int`. Semantic/hybrid rows keep mirror-unit spans; lexical rows
and hybrid unit-less rows get structurally derived spans.

**Tech Stack:** Go + database/sql (SQLite mattn, PG pgx-stdlib, DuckDB), huma
v2, Svelte generated client, testify.

## Global Constraints

Copied from the spec; every task's requirements include these.

- `OrdinalRange [2]int` with tag `json:"ordinal_range"` — always present, never
  omitempty; `[ordinal, ordinal]` when the anchor is its own unit; a range
  starting at ordinal 0 serializes as `[0, N]`.
- `ordinal` stays the anchor / exact matched message in every mode. Row
  cardinality is unchanged: lexical = one row per occurrence, semantic = one
  row per embedded unit, hybrid = one row per unit with the FTS-anchor
  override untouched.
- Derived-unit rules (spec "Derived-unit definition"): embeddable user row →
  `[o, o]`; embeddable assistant row → maximal same-`is_sidechain` stretch of
  embeddable assistant rows bounded exclusively by the nearest embeddable user
  rows, session edges, and nearest sidechain-flip assistant row, with
  endpoints at the first/last MEMBER ordinals; anything else → `[o, o]`.
  Anchor row missing → `[o, o]`, `is_sidechain` false. Invariant: derivation
  at any member ordinal of any unit from
  `ScanEmbeddableUnits(include_automated = true)` returns exactly that unit's
  `[Ordinal, OrdinalEnd]`.
- "Embeddable user/assistant row" =
  `role IN ('user','assistant') AND is_system = 0` AND content not
  system-prefixed (dialect `SystemPrefixSQL` — the predicate is TRUE for
  embeddable rows).
- `subordinate` = session-subordinate
  (`relationship_type IN ('subagent','fork')` OR parent-linked with
  `relationship_type <> 'continuation'`) OR anchor `is_sidechain`. Populated
  with `relationship`, `parent_session_id`, `is_sidechain` in ALL modes; those
  keep `omitempty`.
- Lexical derivation never touches vectors.db: structure-only, from
  messages/sessions. `tool_result_events` matches locate their anchor row via
  post-scan secondary lookup — never an inner join (cardinality must not
  change).
- Performance: derivation is post-scan, O(page); per-page batched statements
  (VALUES/UNION probe lists, chunked like `enrichHitsChunk`); page-level
  memoization (anchors inside an already-derived range reuse it). Benchmark
  acceptance is MANUAL: baseline recorded at Task 1, final comparison at Task
  8, budget ≈ 10% on a 50-hit substring/FTS page.
- Backend parity: SQLite, PostgreSQL, DuckDB produce identical observable
  output. `internal/db` never imports the other stores; they implement the
  seam.
- `scope=top|all|subordinate` stays semantic/hybrid-only. No lexical collapse.
  Lexical snippets unchanged. `score` stays semantic/hybrid.
- Repo rules: testify (`require`/`assert`), table-driven where natural,
  ≤100-line functions, `go fmt ./...` + `make vet` + `make lint` clean, new
  commit per task (never amend), CGO_ENABLED=1 with `-tags fts5`.

______________________________________________________________________

### Task 1: Content-search benchmark (pre-change baseline)

**Files:**

- Create: `internal/db/search_content_bench_test.go`

**Interfaces:** none (test-only). Must build under plain `fts5` tag —
`bench-gate` runs `go test -tags "fts5"`; do NOT use the `benchdb` tag.

- [ ] **Step 1:** Write two benchmarks modeled on `messages_bench_test.go`
  (`b.N` + `b.ReportAllocs()`, seed outside the timer, `testDB(b)`):

```go
// seedContentSearchBench builds a corpus that stresses the citation
// derivation: long assistant runs (the monologue case), sidechain
// stretches, system rows inside runs, and a term that matches broadly.
// 40 sessions x 300 messages; in each session ordinals 10..260 form one
// assistant run (with a system row every 50 ordinals inside it), the rest
// alternate user/assistant. Every assistant message contains "needle".
func seedContentSearchBench(b *testing.B, d *DB) { ... }

func BenchmarkSearchContentSubstringPage(b *testing.B) {
    d := testDB(b)
    seedContentSearchBench(b, d)
    f := ContentSearchFilter{Pattern: "needle", Limit: 50}
    b.ReportAllocs()
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        page, err := d.SearchContent(context.Background(), f)
        if err != nil || len(page.Matches) == 0 {
            b.Fatalf("search: %v (%d matches)", err, len(page.Matches))
        }
    }
}

func BenchmarkSearchContentFTSPage(b *testing.B) { /* Mode: "fts", same shape */ }
```

- [ ] **Step 2:** Run and record the baseline with the gate's settings:
  `CGO_ENABLED=1 go test -tags fts5 -run '^$' -bench 'SearchContent' -benchmem -count 6 -benchtime 20x ./internal/db | tee .superpowers/sdd/citations-bench-baseline.txt`
  Copy the medians into the task report. This file is the Task 8 baseline —
  do not delete it.
- [ ] **Step 3:** `make vet && make lint`; commit
  `test(db): content-search page benchmarks for citation baseline`.

______________________________________________________________________

### Task 2: Derivation core + SQLite seam

**Files:**

- Create: `internal/db/unit_range.go`, `internal/db/unit_range_test.go`

**Interfaces (Produces — later tasks use these verbatim):**

```go
// UnitAnchor classifies one match's anchor message row.
type UnitAnchor struct {
    SessionID  string
    Ordinal    int
    Role       string // "user"/"assistant"/other; "" when Missing
    Sidechain  bool
    Embeddable bool // is_system = 0 AND content not system-prefixed
    Missing    bool // anchor row absent (tool_result_events orphan)
}

// UnitProbe asks for the nearest embeddable-user boundaries around Ordinal.
type UnitProbe struct {
    SessionID string
    Ordinal   int
}

// UnitBounds carries exclusive user boundaries; sentinel values when absent:
// Prev = -1, Next = unitOrdinalMax.
type UnitBounds struct{ Prev, Next int }

// ExtentProbe asks for the first/last member ordinals of the anchor's
// same-sidechain run within the exclusive interval (Lo, Hi).
type ExtentProbe struct {
    SessionID string
    Ordinal   int
    Lo, Hi    int // sentinels as above
    Sidechain bool
}

// unitOrdinalMax bounds Hi sentinels; ordinals are int32-safe on all
// backends (PG INTEGER).
const unitOrdinalMax = 1<<31 - 1

// UnitBoundsQuerier is the backend seam. Both methods are BATCHED: one or
// two SQL statements per call, probe lists chunked to the dialect's
// variable limit. Results align 1:1 with probes.
type UnitBoundsQuerier interface {
    NearestUserBoundaries(ctx context.Context, probes []UnitProbe) ([]UnitBounds, error)
    RunExtents(ctx context.Context, probes []ExtentProbe) ([][2]int, error)
}

// DeriveUnitRanges applies the spec's rules 1-3 + missing-anchor fallback,
// memoized per session. Result aligns 1:1 with anchors.
func DeriveUnitRanges(ctx context.Context, q UnitBoundsQuerier, anchors []UnitAnchor) ([][2]int, error)

// SubordinateSession reports the session-level subordinate classification
// (subagent/fork-typed, or parent-linked non-continuation). Exported
// wrapper over the existing isSubordinateSession logic so the PG and
// DuckDB packages compute the same subordinate flag.
func SubordinateSession(relationshipType, parentSessionID string) bool
```

`DeriveUnitRanges` logic: rule 1/3/missing anchors resolve to `[o, o]` without
queries. Embeddable-assistant anchors first check the per-session memo (an
anchor with matching sidechain whose ordinal falls inside an already-derived
range reuses it), then batch `NearestUserBoundaries` for the remainder, then
batch `RunExtents` with the returned bounds. Keep each helper under the 100-line
limit (classification pass, probe construction, memo lookup as separate funcs).

SQLite implements the seam on `*DB` (reader connection). `RunExtents` SQL per
probe — this exact shape, mirrored later by PG/DuckDB with their dialect
predicate and placeholders:

```sql
SELECT
  (SELECT MIN(m.ordinal) FROM messages m
    WHERE m.session_id = ?1 AND m.ordinal <= ?2
      AND m.ordinal > COALESCE((SELECT MAX(f.ordinal) FROM messages f
            WHERE f.session_id = ?1 AND f.ordinal > ?3 AND f.ordinal < ?2
              AND f.role = 'assistant' AND f.is_system = 0
              AND <SystemPrefixSQL(f.content, f.role)>
              AND f.is_sidechain <> ?5), ?3)
      AND m.role = 'assistant' AND m.is_system = 0
      AND <SystemPrefixSQL(m.content, m.role)>
      AND m.is_sidechain = ?5),
  (SELECT MAX(m.ordinal) FROM messages m
    WHERE m.session_id = ?1 AND m.ordinal >= ?2
      AND m.ordinal < COALESCE((SELECT MIN(f.ordinal) FROM messages f
            WHERE f.session_id = ?1 AND f.ordinal < ?4 AND f.ordinal > ?2
              AND f.role = 'assistant' AND f.is_system = 0
              AND <SystemPrefixSQL(f.content, f.role)>
              AND f.is_sidechain <> ?5), ?4)
      AND m.role = 'assistant' AND m.is_system = 0
      AND <SystemPrefixSQL(m.content, m.role)>
      AND m.is_sidechain = ?5)
```

(`?1`=session, `?2`=anchor, `?3`=Lo, `?4`=Hi, `?5`=anchor sidechain.)

The anchor itself always qualifies, so both endpoints are non-NULL.
`NearestUserBoundaries` per probe:
`MAX(ordinal) WHERE ordinal < anchor AND role='user' AND is_system=0 AND <SystemPrefixSQL>`
/ `MIN(ordinal) WHERE ordinal > anchor ...`, `COALESCE`d to the sentinels. Batch
probes with UNION ALL (or a VALUES CTE), chunked at `enrichHitsChunk`-style
limits.

- [ ] **Step 1:** Write the failing tests first. Required cases:
    - **Reducer equivalence (the invariant test):** seed one DB with a corpus
      covering: plain runs, runs at session start and session end, sidechain
      stretches inside a session (flip mid-run), system rows and system-prefixed
      rows inside runs and adjacent to run boundaries, system-prefixed user rows
      (must NOT act as boundaries), a single-assistant-message run, automated
      sessions, subagent/fork/ continuation-child sessions. Collect units via
      `ScanEmbeddableUnits(ctx, "", true, ...)`; for EVERY unit and EVERY member
      ordinal (walk `Offsets`), assert `DeriveUnitRanges` returns exactly
      `[unit.Ordinal, unit.OrdinalEnd]`; for every user unit assert `[o, o]`.
    - Rule-3 anchors (system row inside a run, tool-role row, system-prefixed user
      row) → `[o, o]`. A system-prefixed ASSISTANT row is NOT a rule-3 case: the
      prefix predicate constrains only user rows, so a prefixed assistant row
      stays embeddable and derives its run span (this is how it was implemented
      and tested).
    - Missing anchors → `[o, o]`.
    - Memoization: a `UnitBoundsQuerier` wrapper that counts calls; 20 anchors
      inside one run must trigger exactly one `NearestUserBoundaries` + one
      `RunExtents` batch with one probe.
    - Non-member-span check: run whose members are ordinals {5, 7} with a system
      row at 6 → `[5, 7]`; anchors 5 and 7 both return it; system rows at 3-4
      and 8-9 between the user boundaries must NOT widen it.
- [ ] **Step 2:** Run them: expected FAIL (functions undefined).
- [ ] **Step 3:** Implement `unit_range.go` (derivation + SQLite seam).
- [ ] **Step 4:**
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run UnitRange -v` → PASS;
  then the full package.
- [ ] **Step 5:** `make vet && make lint`; commit
  `feat(db): conversation-unit range derivation with SQLite seam`.

______________________________________________________________________

### Task 3: Wire contract — OrdinalRange swap (semantic/hybrid + CLI)

**Files:**

- Modify: `internal/db/search_content.go` (struct + semantic/hybrid populate
  sites), `cmd/agentsview/session_search.go`, all tests naming
  `ContentMatch.OrdinalStart/OrdinalEnd` (list in Task-2 fact section of the
  brief: internal/db/search_content_hybrid_test.go,
  search_content_semantic_test.go, search_content_semantic_vector_test.go,
  internal/server/search_scope_test.go, cmd/agentsview/session_search_test.go)

**Interfaces:**

- Consumes: nothing new.

- Produces: `ContentMatch.OrdinalRange [2]int` with `json:"ordinal_range"`
  replacing `OrdinalStart`/`OrdinalEnd` (delete both — they never shipped to
  main). Internal structs (`VectorHit`, `hybridDisplay`, `UnitRef`,
  `EmbeddableUnit`) keep their field names.

- [ ] **Step 1:** Swap the struct field with the spec's doc comment. Populate
  `OrdinalRange: [2]int{h.OrdinalStart, h.OrdinalEnd}` in
  `searchContentSemantic` and from `hybridDisplay` in `enrichHybridMatches`.
  Lexical scan sites (`scanContentMatches`, regex confirm loop, FTS scan) set
  `OrdinalRange: [2]int{ordinal, ordinal}` — a placeholder self-range that
  Task 4 upgrades to derived values; the always-present contract holds from
  this task on.

- [ ] **Step 2:** CLI: `formatMatchOrdinal` branches on
  `m.OrdinalRange[1] > m.OrdinalRange[0]` → `#start-end @anchor`, else
  `#ordinal`. Update its tests.

- [ ] **Step 3:** Update the two omission pins:
  `TestContentMatchJSONUnitFieldsOmittedForLexicalMatches` becomes an
  always-present pin — a substring match's JSON must contain
  `"ordinal_range":[N,N]` and must NOT contain `score`, `ordinal_start`, or
  `ordinal_end`; the `internal/server` zero-value test asserts a semantic hit
  at ordinal 0 serializes `"ordinal_range":[0,0]` (or `[0, N]` for a run
  fixture).

- [ ] **Step 4:**
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db/... ./internal/server/... ./cmd/agentsview/... ./internal/service/...`
  → PASS; `make vet && make lint`.

- [ ] **Step 5:** Commit
  `feat(api): ordinal_range citation field replaces ordinal_start/end`.

______________________________________________________________________

### Task 4: SQLite lexical derivation + lineage + hybrid unit-less injection

**Files:**

- Modify: `internal/db/search_content.go` (substring/regex/FTS branches, scan
  funcs, hybrid `appendHybridFTSHits`/`enrichHybridMatches` path)
- Test: `internal/db/search_content_test.go`,
  `internal/db/search_content_hybrid_test.go`

**Interfaces:**

- Consumes: `DeriveUnitRanges`, `UnitAnchor`, the `*DB` seam (Task 2);
  `IsSystemPrefixed(content, role) bool` (search.go:174) if classifying in Go
  instead of SQL.
- Produces: lexical `ContentMatch` rows with derived `OrdinalRange`,
  `Subordinate`, `Relationship`, `ParentSessionID`, `Sidechain`.

Implementation shape (all three lexical modes share it):

- Every SELECT branch gains columns: `s.relationship_type`,
  `s.parent_session_id`, plus anchor-classification columns — messages/ FTS
  branches:
  `m.is_sidechain, m.is_system, CASE WHEN <SystemPrefixSQL(m.content, m.role)> THEN 1 ELSE 0 END`;
  tool_input and canonical tool_result branches: the same from the
  already-joined `mm`/`m` alias plus `mm.role` (the wire `role` stays the
  hard-coded `'assistant'`; the anchor classification uses the REAL row's
  role); `tool_result_events` branches emit typed NULLs for the anchor columns
  (`CAST(NULL AS ...)` as needed) — cardinality untouched.

- Scan funcs read the extra columns into a small per-row sidecar struct (do not
  widen `ContentMatch`).

- Post-scan pass (one shared helper, used by substring/regex/FTS): rows with
  NULL anchor columns (events) get one batched anchor lookup —
  `SELECT role, is_sidechain, is_system, <prefix CASE> FROM messages WHERE (session_id, ordinal) IN (...)`
  chunked like `enrichSemanticHits`; absent rows → `Missing: true`. Build
  `[]UnitAnchor`, call `DeriveUnitRanges`, assign `OrdinalRange` +
  `Subordinate` (`isSubordinateSession(rel, parent) || sidechain`) + lineage
  fields.

- Hybrid: unit-less FTS rows (`DocKey == ""` in `appendHybridFTSHits`) get the
  same anchor-lookup + `DeriveUnitRanges` classification (range AND
  subordinate flag) inside `appendHybridFTSHits`, BEFORE scope filtering and
  fusion — landed early as
  `fix(search): classify hybrid unit-less hits before scope and fusion`; do
  not re-add an enrich-time pass (mirror-unit rows are untouched).

- [ ] **Step 1:** Failing tests first:

    - Substring: three matches inside one assistant run → three rows, each with
      the run's `OrdinalRange` and correct anchor `ordinal`; a match on a user
      row → `[o, o]`; a match on a system row inside the run → `[o, o]`; a match
      in a sidechain run → `Subordinate` true + `Sidechain` true; a match in a
      subagent-session → `Subordinate` true with
      `Relationship`/`ParentSessionID` populated.
    - tool_input and tool_result (canonical): anchor classified from the real
      message row; range = the enclosing run.
    - tool_result_events: seeded event WITHOUT a matching message row still
      returns its match (cardinality pin!) with `[o, o]`; with a matching
      message row inside a run → the run's range.
    - regex + fts: one spot-check each that derived ranges appear (implementation
      is shared; no need to re-cover every case).
    - Hybrid unit-less: FTS hit outside the mirror gets the derived run range, not
      `[o, o]` (extend the existing unit-less test).
    - ExcludeSystem interplay: with `ExcludeSystem` set, behavior is unchanged
      apart from the new fields.

- [ ] **Step 2:** Run → FAIL. Implement. Keep the enrichment helper and scan
  changes within function-size limits (extract branch-column builders if
  needed).

- [ ] **Step 3:** Full package + consumers:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db/... ./internal/server/... ./internal/service/... ./cmd/agentsview/...`

- [ ] **Step 4:** `make vet && make lint`; commit
  `feat(db): derived conversation-unit citations on lexical search`.

______________________________________________________________________

### Task 5: PostgreSQL backend

**Files:**

- Modify: `internal/postgres/search_content.go`
- Create: `internal/postgres/unit_range.go`
- Test: `internal/postgres/search_content_pgtest_test.go` (extend; pgtest build
  tag)

**Interfaces:**

- Consumes: `db.UnitAnchor`, `db.UnitProbe`, `db.UnitBounds`, `db.ExtentProbe`,
  `db.UnitBoundsQuerier`, `db.DeriveUnitRanges`, `db.SubordinateSession`,
  `db.PostgresSystemPrefixSQL` (all from Task 2).

- Produces: `postgres.Store` implements `db.UnitBoundsQuerier` with `$N`
  placeholders via `store.pg` (`QueryContext`).

- [ ] **Step 1:** Failing pgtest tests mirroring Task 4's core cases (run-range
  on substring matches, user `[o,o]`, events-orphan cardinality pin, subagent
  lineage), using the `setupContentSearch` / `insertCS*` helpers.

- [ ] **Step 2:** Add the same SELECT columns to every PG branch (messages,
  tool_input, tool_result canonical, tool_result_events with typed NULLs;
  regex candidate branches too); extend `scanPGContentMatches`. Implement the
  two seam methods in `unit_range.go` — the exact SQL from Task 2 with
  `PostgresSystemPrefixSQL` and `$N` placeholders (`unnest`-or-VALUES probe
  batching). Wire the shared post-scan pass (the enrichment helper logic lives
  in `internal/db` if it can be expressed backend-neutrally — anchor-lookup
  SQL differs, so the PG side supplies its own batched anchor-lookup and calls
  `db.DeriveUnitRanges`).

- [ ] **Step 3:** `make test-postgres` (starts the container; tags
  `fts5,pgtest`) → PASS. Also plain `make test-short` for compile safety.

- [ ] **Step 4:** `make vet && make lint`; commit
  `feat(postgres): conversation-unit citations on PG content search`.

**Checkpoint: invoke the roborev-fix skill after Task 5 and resolve all findings
before Task 6.**

______________________________________________________________________

### Task 6: DuckDB backend

**Files:**

- Modify: `internal/duckdb/store.go` (content-search branches + scans)
- Create: `internal/duckdb/unit_range.go`
- Test: `internal/duckdb/store_test.go` /
  `internal/duckdb/store_contract_test.go` (follow the existing
  `newSyncedStore` seeding pattern)

**Interfaces:**

- Consumes: same `internal/db` seam types as Task 5; `db.DuckDBSystemPrefixSQL`;
  queries MUST go through the store's `queryContext` wrapper (quack-remote
  path), not a raw `*sql.DB`.

- Produces: `duckdb.Store` implements `db.UnitBoundsQuerier` (`?` placeholders).

- NOTE (landed early): both DuckDB scan paths already emit the self-range
  placeholder `[ordinal, ordinal]` with a store test
  (`fix(duckdb): self-range ordinal_range placeholder on content search`);
  this task replaces the placeholder with derived ranges, not `[0, 0]`.

- [ ] **Step 1:** Failing tests first (same core cases as Task 5, seeded via
  `newSyncedStore`; events-orphan cardinality pin included).

- [ ] **Step 2:** Columns on every branch (both `scanDuckContentRows` and
  `scanDuckContentCandidateRows` paths), seam implementation, shared post-scan
  pass.

- [ ] **Step 3:** `CGO_ENABLED=1 go test -tags fts5 ./internal/duckdb/...` →
  PASS.

- [ ] **Step 4:** `make vet && make lint`; commit
  `feat(duckdb): conversation-unit citations on DuckDB content search`.

______________________________________________________________________

### Task 7: MCP fields + OpenAPI/client regen

**Files:**

- Modify: `internal/mcp/tools.go` (contentMatch struct + assembly at the
  `res.Matches` loop), `internal/mcp/tools_test.go`
- Regenerate: `frontend/src/lib/api/generated/**` via `npm run generate:api`
  from `frontend/`

**Interfaces:**

- Consumes: `db.ContentMatch.OrdinalRange` and lineage fields.

- Produces: MCP `contentMatch` gains `OrdinalRange [2]int json:"ordinal_range"`,
  `Subordinate bool json:"subordinate,omitempty"`,
  `Relationship string json:"relationship,omitempty"`,
  `ParentSessionID string json:"parent_session_id,omitempty"`,
  `Sidechain bool json:"is_sidechain,omitempty"` — copied verbatim in the
  assembly loop.

- NOTE (landed early): the frontend client regen (Step 4) already ran —
  `chore(frontend): regenerate API client for ordinal_range` — so only the MCP
  fields and OpenAPI check remain; re-run generate:api only if the schema
  changes again.

- [ ] **Step 1:** Failing MCP test: a search_content call over a seeded run
  asserts `ordinal_range` spans the run on every row and `subordinate`/lineage
  fields round-trip; a top-level single-message match asserts
  `ordinal_range == [o, o]`.

- [ ] **Step 2:** Implement; run `./internal/mcp/...` tests.

- [ ] **Step 3:** Verify the OpenAPI schema renders `[2]int` sanely:
  `go run ./cmd/agentsview openapi | python3 -c "import json,sys; s=json.load(sys.stdin); import re; print(json.dumps(s['components']['schemas']['DbContentMatch']['properties']['ordinal_range'], indent=1))"`
  — expect an array schema with fixed size (minItems/maxItems 2). If huma
  emits something degenerate, fix with a `Schema()` override on a named type
  rather than changing the wire shape.

- [ ] **Step 4:** `cd frontend && npm run generate:api && npm run check` → 0
  errors (generated `DbContentMatch.ts` now has `ordinal_range`; hand-written
  code compiles untouched — nothing outside `generated/` reads the old
  fields).

- [ ] **Step 5:** `make vet && make lint`; commit
  `feat(api): ordinal_range on MCP search results and generated client`.

______________________________________________________________________

### Task 8: Benchmark comparison (manual acceptance)

**Files:**

- No production changes expected; tuning only if the budget is breached.

- [ ] **Step 1:** Re-run exactly the Task-1 command:
  `CGO_ENABLED=1 go test -tags fts5 -run '^$' -bench 'SearchContent' -benchmem -count 6 -benchtime 20x ./internal/db | tee .superpowers/sdd/citations-bench-after.txt`

- [ ] **Step 2:** Compare medians against
  `.superpowers/sdd/citations-bench-baseline.txt` (benchstat if available,
  else by hand). Acceptance: ≤ ~10% ns/op regression on both benchmarks; also
  eyeball allocs/op.

- [ ] **Step 3:** If breached: profile (`-cpuprofile`), and tune inside the
  spec's envelope — batching shape, probe chunking, memo hit-rate — WITHOUT
  changing observable output. Re-run until within budget or escalate to the
  human with numbers if the budget cannot be met inside the spec's design.

- [ ] **Step 4:** Record both result files' medians in the task report and the
  PR description note. Commit only if tuning changed code:
  `perf(db): tune citation derivation batching`.

______________________________________________________________________

### Task 9: Docs, burn-down, full gate

**Files:**

- Modify: `docs/semantic-search.md` (hit-shape section → `ordinal_range`,
  uniform-citation contract, per-mode provenance, lexical lineage fields),
  `docs/session-api.md` (field table: `ordinal_range` on every match; remove
  `ordinal_start`/`ordinal_end` rows; note missing-key conventions now apply
  only to the omitempty lineage fields), `docs/semantic-search-internals.md`
  (new "Conversation-unit citations" section: derived-unit rules, seam,
  memoization, batching, provenance divergence, events fallback),
  `docs/commands.md` if it documents search output fields,
  `internal/skills/templates/finding-history.md.tmpl` (citation guidance: cite
  `session + ordinal_range` with `@ordinal` anchor in every mode)

- Delete:
  `docs/superpowers/specs/2026-07-06-conversation-unit-citations-design.md`,
  `docs/superpowers/plans/2026-07-06-conversation-unit-citations.md`
  (burn-down after folding durable content into the internals page)

- [ ] **Step 1:** Write docs; mdformat the hook-covered files
  (`uvx --from mdformat==0.7.22 --with mdformat-frontmatter --with mdformat-mkdocs --with mdformat-tables==1.0.0 mdformat --wrap 80 --align-semantic-breaks-in-lists <files>`;
  `commands.md`/`session-api.md` are hook-excluded — hand-format). Re-read
  files after any mdformat run before further edits.

- [ ] **Step 2:** Delete the spec and this plan.

- [ ] **Step 3: full gate.** `make lint && make test`; `make test-postgres`;
  from `frontend/`: `npm run check`; `make docs-check`.

- [ ] **Step 4:** Commit `docs: conversation-unit citations, drop working spec`.

**Checkpoint: invoke the roborev-fix skill after Task 9 and resolve all
findings. Then run the final whole-branch review on the most capable model
before superpowers:finishing-a-development-branch.**
