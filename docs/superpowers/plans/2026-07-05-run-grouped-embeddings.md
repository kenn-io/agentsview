# Run-Grouped Embeddings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development to implement this plan task-by-task.
> Invoke the roborev-fix skill after Task 5 and after Task 9, and resolve all
> findings.

**Goal:** Replace per-message assistant embeddings with run-grouped unit
documents per
`docs/superpowers/specs/2026-07-05-run-grouped-embeddings-design.md`.

**Architecture:** A unit reducer in `internal/db` streams user docs and
assistant-run docs; `internal/vector` mirrors units into a v2 `vectors.db`
schema with versioned reset; search resolves chunk hits to anchored ordinal
ranges; hybrid fusion maps FTS message hits to units and applies a subordinate
rank penalty in a local RRF merge; `--scope` supersedes `include_children` for
semantic/hybrid.

**Tech Stack:** Go (CGO, `fts5` tag), go.kenn.io/kit v0.2.0 vector/sqlitevec (no
kit changes), testify, Svelte frontend client regen.

## Global Constraints

- The spec at
  `docs/superpowers/specs/2026-07-05-run-grouped-embeddings-design.md` is the
  requirements document. Exact values below are copied from it.
- Run boundary = embeddable user row: `role = 'user' AND is_system = 0 AND` not
  system-prefixed (`SystemPrefixSQL`); session edges; `is_sidechain`
  transitions. System-prefixed user rows do NOT split runs.
- Run doc_key: `r:<escaped session_id>:<escaped source_uuid>[#n]` from the run's
  FIRST message; ordinal fallback `ro:<escaped session_id>:<ordinal>`. User
  doc keys `u:`/`o:` unchanged.
- Run content: message texts joined with exactly `"\n\n"`; no markers or
  metadata in embedded text.
- Subordinate iff: any member has `is_sidechain = 1`, OR session
  `relationship_type IN ('subagent','fork')`, OR (`parent_session_id <> ''`
  AND `relationship_type <> 'continuation'`). Continuations and
  forks-of-nothing are top-level.
- Mirror schema version key `mirror_schema_version` = `2`; mismatch (or absent
  key with existing mirror state) on the WRITE path drops every table in
  vectors.db (`vector_messages`, `vector_meta`, all `message_vectors*` kit
  tables) and recreates v2; READ path returns typed
  `vector.ErrMirrorVersionMismatch`, surfaced like the stale-fingerprint gate
  (wired, rebuild-required), never silently unwired.
- Generation fingerprint Params: `max_input_chars`,
  `doc_unit_scheme = "run_v1"`, `chunk_overlap_chars = <computed>`.
- Chunk overlap = `maxInputChars * 15 / 100` (replaces `maxInputChars / 30`).
- Anchor = message whose rune span contains `chunk_start + len(chunk_runes)/2`
  (actual chunk rune length); earlier message on ties; FTS-matched message
  overrides the anchor when the FTS leg contributes.
- Hit shape: existing `ordinal` field KEPT, redefined as anchor ordinal;
  `ordinal_start`/`ordinal_end` additive; lineage fields additive.
- `--scope top|all|subordinate` (default `all`) governs semantic/hybrid unit
  visibility; `include_children` is accepted but superseded in those modes
  (both hybrid legs see the same universe); automated/one-shot/project/
  agent/date filters still apply in every mode; FTS-only/substring/regex modes
  keep today's `include_children` semantics.
- Subordinate penalty: local RRF merge, subordinate contributions use
  `rank + 5`, rank constant stays 60. Semantic-only search routes its single
  result list through the same merge as a one-leg fusion so the penalty
  applies identically — one implementation, no hybrid-only special case.
- The unique `(session_id, ordinal)` mirror index is retained unchanged; the
  resolver does point lookups (greatest `ordinal <= x`, verify
  `x <= ordinal_end`); no new index.
- `offsets` column: JSON `[{"o":..,"r":..,"b":..}, ...]`, `DEFAULT '[]'`.
- `sessions.db` schema is never touched. `vectors.db` may be reset.
- Backend parity: message-window and FTS behavior unchanged on SQLite/PG/DuckDB;
  vector search stays SQLite-local.
- Tests: testify (`require`/`assert`), table-driven, `t.TempDir()`, existing
  `testDB(t)` helper. Run package tests per task; `make lint && make test` at
  Task 9.
- Commit per task with conventional messages; no attribution footers.

______________________________________________________________________

### Task 1: Unit reducer in internal/db

**Files:**

- Modify: `internal/db/messages.go` (add below `ScanEmbeddableMessages`, which
  stays untouched until Task 3 removes it)
- Test: `internal/db/messages_test.go`

**Interfaces (Produces):**

```go
// EmbeddableUnit is one embedding document: a single embeddable user
// message, or a run of contiguous embeddable assistant messages.
type EmbeddableUnit struct {
	SessionID   string
	Kind        string // "user" | "run"
	SourceUUID  string // first member's source_uuid ("" legacy)
	Ordinal     int    // first member's ordinal (ordinal_start)
	OrdinalEnd  int    // last member's ordinal (== Ordinal for user docs)
	Subordinate bool
	Content     string       // members joined with "\n\n"
	Offsets     []UnitOffset // one per member; nil for user docs
}

// UnitOffset locates one member message inside a run's joined content.
type UnitOffset struct {
	Ordinal   int `json:"o"`
	RuneStart int `json:"r"`
	ByteStart int `json:"b"`
}

func (db *DB) ScanEmbeddableUnits(
	ctx context.Context, since string, includeAutomated bool,
	fn func(EmbeddableUnit) error,
) (maxEnded string, err error)
```

**Steps:**

- [ ] **Step 1: failing tests.** In `messages_test.go`, seed sessions with the
  existing helpers and cover, table-driven where natural:

    - user/assistant alternation → user docs + one run per gap, correct
      Ordinal/OrdinalEnd and `"\n\n"` joins;
    - a system-prefixed user row (e.g. content `"<task-notification> x"`) between
      assistant messages does NOT split the run;
    - an `is_system = 1` user row does not split; a plain user row does;
    - `is_sidechain` transition splits runs and marks the sidechain run
      subordinate;
    - subordinate classification: session with `relationship_type = 'subagent'`
      (all units subordinate), `'fork'` (subordinate), `'continuation'` + parent
      (top-level), parent-linked with empty relationship (subordinate);
    - offsets: multi-byte content (e.g. `"héllo…"`), assert RuneStart and
      ByteStart per member and that `Content[ByteStart:]` starts with the
      member's text;
    - single-message run degenerates to Ordinal == OrdinalEnd, Offsets len 1;
    - `since` filtering and `maxEnded` behave exactly as `ScanEmbeddableMessages`
      (same NULLIF/datetime predicate);
    - `includeAutomated=false` excludes automated sessions.

- [ ] **Step 2: run, verify failure** (`ScanEmbeddableUnits` undefined).

- [ ] **Step 3: implement.** Reuse the existing scan SQL but select additionally
  `m.is_sidechain`, `s.relationship_type`, `s.parent_session_id`, and drop the
  assistant/user doc distinction from SQL — the WHERE keeps the embeddable
  predicate for BOTH roles, plus a second cheaper predicate set: run-boundary
  detection needs non-embeddable user rows to be invisible (they already are —
  excluded rows simply don't appear, which is exactly "do not split"; document
  this). Reducer state per session: accumulate assistant members until a user
  row / session change / sidechain flip, then emit. Emit user docs
  immediately. Compute offsets while joining (`utf8.RuneCountInString` running
  totals). Compute subordinate once per session from relationship fields; OR
  with per-run sidechain.

    Boundary subtlety to encode: because non-embeddable user rows are filtered out
    by SQL, "user row splits run" is implemented as "embeddable user row emits a
    user doc and closes any open run" — no separate boundary logic exists to get
    wrong.

- [ ] **Step 4: tests pass.** Run:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db -run TestScanEmbeddableUnits -v`

- [ ] **Step 5: Commit**
  `feat(db): embeddable unit reducer for run-grouped embeddings`

______________________________________________________________________

### Task 2: Mirror v2 schema, version gate, reset

**Files:**

- Modify: `internal/vector/index.go` (DDL, Open, meta helpers)
- Modify: `internal/vector/errors.go` (or wherever sentinels live) — add
  `ErrMirrorVersionMismatch`
- Test: `internal/vector/index_test.go`

**Interfaces (Produces):**

```go
// ErrMirrorVersionMismatch reports a vectors.db written by a different
// mirror schema version. Write-path opens reset the DB instead; read-path
// opens return an Index whose Search fails with this sentinel until a
// build recreates the mirror.
var ErrMirrorVersionMismatch = errors.New(
	"vector index was built by an incompatible version: run `agentsview embeddings build`")

const mirrorSchemaVersion = "2"
const mirrorSchemaVersionKey = "mirror_schema_version"
```

**Steps:**

- [ ] **Step 1: failing tests.**

    - Write-path: create a vectors.db, write the v1-shaped `vector_messages` (old
      DDL without new columns) plus a fake `message_vectors_generations` table
      and meta keys (watermark, scope); reopen writable → all tables
      dropped/recreated with v2 columns (`ordinal_end`, `subordinate`, `offsets`
      DEFAULT `'[]'`), meta cleared except the stamped version key.
    - Write-path: fresh DB → version stamped `2`, nothing dropped.
    - Write-path: current-version DB with data → untouched on reopen.
    - Read-path: version-mismatched DB → `Open(..., readOnly=true, ...)` succeeds
      but `Search` returns `ErrMirrorVersionMismatch` (`errors.Is`); it must NOT
      satisfy `ErrNoActiveGeneration`.
    - Reset drops stray per-generation `message_vectors*` tables (create a fake
      `message_vectors_gen7` table first).

- [ ] **Step 2: verify failure.**

- [ ] **Step 3: implement.** v2 DDL per the spec (keep table name
  `vector_messages`; retain the unique `(session_id, ordinal)` index exactly).
  On writable Open: read `mirrorSchemaVersionKey`; mismatch OR (absent AND any
  of the known tables exist) → enumerate `sqlite_master` for
  `vector_messages`, `vector_meta`, `message_vectors%` and drop each, then run
  DDL and stamp the version. On read-only Open: record the mismatch in the
  Index and have `Search` (and `QueryGeneration` users) return the sentinel
  before touching tables.

- [ ] **Step 4: tests pass.** Run:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/vector -run TestMirrorSchemaVersion -v`

- [ ] **Step 5: Commit**
  `feat(vector): versioned mirror schema with reset and read sentinel`

______________________________________________________________________

### Task 3: Refresh over units

**Files:**

- Modify: `internal/vector/mirror.go` (`MessageSource` → `UnitSource`, doc keys,
  upsert columns)
- Modify: `internal/vector/build.go` (call-site types only)
- Modify: `internal/db/messages.go` (DELETE `ScanEmbeddableMessages` +
  `EmbeddableMessage` once nothing references them)
- Test: `internal/vector/mirror_test.go`

**Interfaces:**

- Consumes: `db.EmbeddableUnit`, `db.ScanEmbeddableUnits` (Task 1); v2 schema
  (Task 2).
- Produces:

```go
type UnitSource interface {
	ScanEmbeddableUnits(ctx context.Context, since string, includeAutomated bool,
		fn func(db.EmbeddableUnit) error) (string, error)
}
```

Doc keys (reusing the existing escape helper and occurrence counter): user docs
unchanged (`u:`/`o:`); runs `r:<sid>:<uuid>[#n]` / `ro:<sid>:<ordinal>`.

**Steps:**

- [ ] **Step 1: failing tests.** Port the existing mirror tests to units and
  add:

    - run rows persist Ordinal/OrdinalEnd/subordinate/offsets JSON (round-trips
      through the column, `[]` for user docs);
    - appending a message to a session's trailing run keeps the doc_key and
      changes content_hash (re-embed pending), while untouched runs keep
      `embed_gen`;
    - a new embeddable user row landing mid-run on rescan splits the run: old key
      shrinks (content_hash changes), second half appears under a new key,
      evicted vectors handled by the existing two-phase path (assert vectors
      deleted before mirror row on removal — existing tests cover the mechanism;
      assert it still passes over runs);
    - duplicate `source_uuid` first-messages get `#n` occurrence suffixes
      deterministically;
    - sentinel-parking still protects displaced rows within one scan.

- [ ] **Step 2: verify failure.**

- [ ] **Step 3: implement.** Mechanical: iterate units instead of messages; key
  by kind; marshal offsets with `encoding/json`. Update `build.go`
  `countPending`'s chunk estimate call sites for the new column set only
  (overlap changes in Task 4). Delete
  `ScanEmbeddableMessages`/`EmbeddableMessage` and fix compile errors — no
  other callers should exist (verify with grep before deleting).

- [ ] **Step 4: tests pass.** Run:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/vector ./internal/db`

- [ ] **Step 5: Commit** `feat(vector): mirror unit documents with run doc keys`

______________________________________________________________________

### Task 4: Overlap and fingerprint

**Files:**

- Modify: `internal/vector/index.go` (overlap constant)
- Modify: `cmd/agentsview/embeddings.go` (`vectorGeneration`)
- Test: `cmd/agentsview/embeddings_test.go`, `internal/vector/index_test.go`

**Steps:**

- [ ] **Step 1: failing tests.** `vectorGeneration` Params equal
  `{"max_input_chars": "<n>", "doc_unit_scheme": "run_v1", "chunk_overlap_chars": "<n*15/100>"}`;
  Index split options use `MaxRunes: n, Overlap: n*15/100` (export a small
  accessor or test via the existing seam that reads split options).

- [ ] **Step 2: verify failure.**

- [ ] **Step 3: implement.** `overlap := maxInputChars * 15 / 100` in `Open`;
  add the two Params. The changed fingerprint cutting a new generation needs
  no new code — assert via existing generation tests that a differing
  fingerprint triggers the new-generation path.

- [ ] **Step 4: tests pass.** Run:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/vector ./cmd/agentsview -run 'TestVectorGeneration|TestOpen'`

- [ ] **Step 5: Commit**
  `feat(vector): run_v1 fingerprint params and 15 percent chunk overlap`

______________________________________________________________________

### Task 5: Anchored search hits

**Files:**

- Modify: `internal/vector/search.go` (anchor computation, hit fields)
- Modify: `internal/db/vector.go` (`VectorHit` fields)
- Modify: `internal/db/search_content.go` (semantic enrichment: lineage join,
  ordinal=anchor, range fields)
- Test: `internal/vector/search_test.go`, `internal/db/search_content_test.go`

**Interfaces (Produces):**

```go
type VectorHit struct {
	SessionID    string
	Ordinal      int // anchor ordinal (backward compatible)
	OrdinalStart int
	OrdinalEnd   int
	Subordinate  bool
	Score        float32
	Snippet      string
}
```

Lineage (`relationship_type`, `parent_session_id`, anchor `is_sidechain`) is
joined from `sessions.db` during enrichment in `internal/db`, not stored per-hit
in vectors.db.

Anchor algorithm in `internal/vector`:

```go
// anchorOrdinal maps a matched chunk back to the member message whose rune
// span contains the chunk's center rune; earlier member wins a boundary tie.
func anchorOrdinal(offsets []db.UnitOffset, contentRunes int,
	chunkIndex, maxRunes, overlap int) int {
	stride := maxRunes - overlap
	start := chunkIndex * stride
	end := min(start+maxRunes, contentRunes)
	center := start + (end-start)/2
	anchor := offsets[0].Ordinal
	for _, o := range offsets {
		if o.RuneStart <= center {
			anchor = o.Ordinal
		} else {
			break
		}
	}
	return anchor
}
```

(Must mirror `kitvec.Split`'s windowing exactly — same stride semantics; add a
cross-check test that re-Splits the content and asserts the window.)

**Steps:**

- [ ] **Step 1: failing tests.** Anchor cases: chunk fully inside one member;
  center on a member boundary (earlier wins); short final chunk (center from
  actual length, not max_runes — construct content where the two differ);
  single-chunk run (ChunkIndex 0); user doc (offsets nil → Ordinal passes
  through). Snippet slicing uses byte offsets on multi-byte content.
  Enrichment: hit carries range + subordinate + relationship from a seeded
  subagent session.

- [ ] **Step 2: verify failure.**

- [ ] **Step 3: implement.** Search reads the mirror row (offsets, ordinals,
  subordinate) for each rolled-up kit hit, computes the anchor from
  `Hit.ChunkIndex`, slices the snippet from the matched chunk's byte window.
  Enrichment keeps existing chunked-bind conventions (`maxSQLVars`).

- [ ] **Step 4: tests pass.** Run:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/vector ./internal/db`

- [ ] **Step 5: Commit** `feat(vector): anchor semantic hits to ordinal ranges`

**Checkpoint: invoke the roborev-fix skill after Task 5 and resolve all findings
before Task 6.**

______________________________________________________________________

### Task 6: Unit resolver and penalized local RRF merge

**Files:**

- Modify: `internal/db/vector.go` (interface + `MessageRef`/`UnitRef`)
- Modify: `internal/vector/search.go` (resolver implementation)
- Modify: `internal/db/search_content.go` (hybrid: map FTS leg to units, local
  merge replacing `kitvec.Merge`)
- Test: `internal/db/search_content_test.go`, `internal/vector/search_test.go`

**Interfaces (Produces):**

```go
type MessageRef struct {
	SessionID string
	Ordinal   int
}

type UnitRef struct {
	DocKey       string
	SessionID    string
	OrdinalStart int
	OrdinalEnd   int
	Subordinate  bool
}

type VectorSearcher interface {
	SemanticSearch(ctx context.Context, query string, limit int) ([]VectorHit, error)
	ResolveMessageUnits(ctx context.Context, refs []MessageRef) ([]UnitRef, error)
}
```

`ResolveMessageUnits` result is parallel to refs; a ref with no containing unit
(message outside the embeddable universe) yields a zero `UnitRef`
(`DocKey == ""`) — the hybrid path then keeps that FTS hit as its own
message-granularity fusion key, so exact-string hits on tool output never
vanish.

Resolver lookup per ref (point lookup on the retained unique index):

```sql
SELECT doc_key, ordinal, ordinal_end, subordinate FROM vector_messages
WHERE session_id = ? AND ordinal <= ? ORDER BY ordinal DESC LIMIT 1
```

then verify `ref.Ordinal <= ordinal_end` in Go.

Local merge in `internal/db` (replaces `kitvec.Merge` for hybrid only):

```go
// rrfMerge fuses per-leg unit rankings with reciprocal-rank fusion,
// penalizing subordinate units by shifting their effective rank.
func rrfMerge(legs [][]unitRanked, limit int) []mergedUnit {
	const rankConstant = 60
	const subordinatePenalty = 5
	scores := map[string]*mergedUnit{}
	for _, leg := range legs {
		for i, u := range leg {
			rank := i + 1
			if u.Subordinate {
				rank += subordinatePenalty
			}
			m := scores[u.Key]
			if m == nil {
				m = &mergedUnit{unit: u}
				scores[u.Key] = m
			}
			m.score += 1.0 / float64(rankConstant+rank)
		}
	}
	// sort by score desc, tie-break deterministically by Key; truncate.
}
```

**Steps:**

- [ ] **Step 1: failing tests.**

    - Resolver: hit inside a run; exact first/last ordinal; ordinal in a gap
      (between units) → zero UnitRef; unknown session → zero; chunked >999 refs
      (follow existing chunk-bind tests).
    - Merge: identical unit in both legs outranks single-leg; subordinate unit
      ranked at i is beaten by top-level unit at i (penalty works); determinism
      on ties; limit honored; semantic-only (one-leg) path applies the same
      penalty — a subordinate semantic hit ranked above a top-level one drops
      below it after the one-leg merge.
    - Hybrid end-to-end: FTS-matched message inside a run fuses with the run's
      semantic hit into ONE result whose anchor is the FTS message; FTS hit with
      no unit stays message-granularity.

- [ ] **Step 2: verify failure.**

- [ ] **Step 3: implement.** Note the FTS-anchor override: when the FTS leg
  contributed a hit to a fused unit, set the merged hit's `Ordinal` to the FTS
  message ordinal (Global Constraints).

- [ ] **Step 4: tests pass.** Run:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/vector`

- [ ] **Step 5: Commit**
  `feat(search): unit-granularity hybrid fusion with subordinate penalty`

______________________________________________________________________

### Task 7: Scope filter and precedence

**Files:**

- Modify: `internal/db/search_content.go` (`ContentSearchFilter.Scope`,
  visibility predicates, child-exclusion lift for semantic/hybrid legs)
- Modify: `internal/server/huma_routes_search.go` (`scope` query param,
  enum-validated)
- Modify: `cmd/agentsview/session_search.go` (`--scope` flag)
- Test: `internal/db/search_content_test.go`, `internal/server/server_test.go`,
  `cmd/agentsview/session_search_test.go`

**Steps:**

- [ ] **Step 1: failing tests.**

    - `scope=all` (default): a subagent-session unit IS returned by
      semantic/hybrid even though `include_children` defaults false (the
      precedence rule — this is the test that fails against today's
      metadata-scope drop);
    - `scope=top`: subordinate units excluded entirely;
    - `scope=subordinate`: only subordinate units;
    - explicit `include_children=false` + semantic mode: superseded (unit still
      returned);
    - FTS-only mode: `include_children` behavior unchanged (regression guard);
    - automated/one-shot/project filters still drop sessions in semantic mode
      (regression guard);
    - invalid scope value → 422/usage error; CLI `--scope` only valid with
      `--semantic`/`--hybrid`.

- [ ] **Step 2: verify failure.**

- [ ] **Step 3: implement.** Scope filtering happens on enriched hits
  (subordinate flag from the mirror/resolver), not by SQL predicate — the
  sessions-metadata drop keeps every filter EXCEPT the sidebar-child predicate
  in semantic/hybrid modes.

- [ ] **Step 4: tests pass.** Run:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/server ./cmd/agentsview`

- [ ] **Step 5: Commit**
  `feat(search): scope filter with include_children precedence`

______________________________________________________________________

### Task 8: Surface and wiring

**Files:**

- Modify: `internal/server/huma_routes_search.go` + response structs
  (`ordinal_start`, `ordinal_end`, `subordinate`, `relationship` fields)
- Modify: `cmd/agentsview/embed_scheduler.go` (direct-install
  `ErrMirrorVersionMismatch` case: wire a searcher that surfaces
  rebuild-required instead of the swallow-and-unwire path; build summaries say
  "units")
- Modify: CLI hit rendering in `cmd/agentsview/session_search.go` (range display
  `#12-40`, anchor, subordinate marker)
- Modify: `frontend/` regenerate client (`npm run generate:api`)
- Test: `internal/server/server_test.go`,
  `cmd/agentsview/embed_scheduler_test.go`

**Steps:**

- [ ] **Step 1: failing tests.** API response carries the additive fields with
  `ordinal` = anchor; direct-install path with a version-mismatched vectors.db
  serves semantic search returning the rebuild-required error (assert on the
  HTTP error body, not a log line); JSON CLI output includes the range fields.

- [ ] **Step 2: verify failure.**

- [ ] **Step 3: implement**, then regenerate the frontend client and run
  `npm run check` from `frontend/`.

- [ ] **Step 4: tests pass.** Run:
  `CGO_ENABLED=1 go test -tags fts5 ./internal/server ./cmd/agentsview`

- [ ] **Step 5: Commit**
  `feat(api): ordinal-range semantic hits and rebuild-required wiring`

______________________________________________________________________

### Task 9: Docs, skill template, burn-down, full gate

**Files:**

- Modify: `docs/semantic-search.md` (corpus/units section, `--scope`, hit-shape
  examples, cost note)
- Modify: `docs/semantic-search-internals.md` (unit model, doc_key scheme,
  mirror v2 + versioning/reset, anchor policy, fusion/penalty, scope
  precedence)
- Modify: `docs/session-api.md`, `docs/commands.md` (`--scope` rows/entry)
- Modify: `internal/skills/templates/finding-history.md.tmpl` (cite session +
  ordinal range; `--scope top` for decision reconstruction; subordinate hits
  need parent corroboration)
- Delete: `docs/superpowers/specs/2026-07-05-run-grouped-embeddings-design.md`,
  `docs/superpowers/plans/2026-07-05-run-grouped-embeddings.md` (burn-down
  after folding durable content into the internals page)
- Test: existing docs tests; skill render tests pick up the template hash change
  automatically

**Steps:**

- [ ] **Step 1:** write docs; keep mdformat clean
  (`uvx --from mdformat==0.7.22 --with mdformat-frontmatter --with mdformat-mkdocs --with mdformat-tables==1.0.0 mdformat --wrap 80 --align-semantic-breaks-in-lists <files>`;
  `commands.md`/`session-api.md` are hook-excluded, hand-format them).
- [ ] **Step 2:** delete the spec and this plan.
- [ ] **Step 3: full gate.** `make lint && make test`; from `frontend/`:
  `npm run check` and the touched vitest files.
- [ ] **Step 4: Commit** `docs: run-grouped embeddings docs, drop working spec`

**Checkpoint: invoke the roborev-fix skill after Task 9 and resolve all
findings. Then run the final whole-branch review on the most capable model
before superpowers:finishing-a-development-branch.**
