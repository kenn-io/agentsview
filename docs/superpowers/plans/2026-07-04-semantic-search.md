# Semantic Search Implementation Plan

> **Archival note:** this plan is a historical execution checklist, retained for
> provenance. The feature it describes has been implemented; where this plan's
> task-level detail differs from what shipped, the code and
> `docs/semantic-search.md` are authoritative.

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.
>
> **Review cadence:** invoke the `roborev-fix` skill after Tasks 5, 10, and 15,
> and once more after Task 17 (the final task). Fix everything it surfaces
> before moving on.

**Goal:** Semantic (vector) and hybrid search over user/assistant message
content as new content-search modes, with cursor-based context-window retrieval
(`--around`, `--context`), an `embeddings` CLI lifecycle, and a daemon
incremental fill — per
`docs/superpowers/specs/2026-07-04-semantic-search-design.md` (the spec; read it
before starting any task).

**Architecture:** A new `internal/vector` package owns a separate `vectors.db`
(mirror table + kit-managed vector tables), an OpenAI-compatible embeddings
client, and build/search orchestration on top of `go.kenn.io/kit/vector` +
`vector/sqlitevec`. The SQLite `db.DB` gains a pluggable `VectorSearcher` for
`semantic`/`hybrid` content-search modes; PG/DuckDB return a capability error.
Service/HTTP/CLI/MCP surfaces thread the new modes and window params through the
existing transport-neutral seam.

**Tech Stack:** Go, SQLite (mattn/go-sqlite3, fts5 tag, CGO), sqlite-vec via
`go.kenn.io/kit/vector/sqlitevec`, cobra CLI, huma HTTP, testify.

## Global Constraints

- `go.kenn.io/kit` pinned to exactly `v0.2.0`.
- All builds/tests: `CGO_ENABLED=1` and `-tags "fts5"` (use `make test` /
  `make test-short`).
- Go tests use testify (`require` for aborting checks, `assert` otherwise);
  table-driven where natural; `t.TempDir()` for temp dirs; never
  `if got != want { t.Fatalf }`.
- After Go changes run `go fmt ./...` and `go vet ./...` before committing.
- Commit every task; conventional commit messages; never amend; pre-commit hooks
  must pass without `--no-verify`.
- Backend parity: `GetMessagesWindow` is implemented on SQLite, PostgreSQL, and
  DuckDB with identical observable behavior. Semantic/hybrid modes are
  SQLite-only by design (documented divergence): PG/DuckDB return
  `db.ErrSemanticUnavailable`.
- The main SQLite archive is never modified by this feature (no schema changes
  to it). All new state lives in `vectors.db`.
- No frontend/SPA changes. No changes to session-level search (`/api/v1/search`,
  MCP `search_sessions`).
- No emojis in code or output.

## File Structure

| Path                                                               | Responsibility                                                                               |
| ------------------------------------------------------------------ | -------------------------------------------------------------------------------------------- |
| `internal/vector/encoder.go` (+`_test`)                            | OpenAI-compatible `/v1/embeddings` client returning a kit `EncodeFunc`                       |
| `internal/vector/index.go` (+`_test`)                              | Open/close `vectors.db`, mirror DDL, kit store construction, generation lifecycle + coverage |
| `internal/vector/mirror.go` (+`_test`)                             | Mirror refresh (scan main DB, reconcile, DeleteVectors)                                      |
| `internal/vector/build.go` (+`_test`)                              | Build orchestration (refresh + EnsureGeneration + kit Fill + auto-activate + progress)       |
| `internal/vector/search.go` (+`_test`)                             | Query embed + kit Search + snippet; implements `db.VectorSearcher`                           |
| `internal/vector/manager.go` (+`_test`)                            | `Manager`: single-writer build job state machine for daemon/CLI                              |
| `internal/config/config.go` (+ existing test file)                 | `[vector]` / `[vector.embeddings]` / `[vector.embed]` TOML config + validation               |
| `internal/db/vector.go`                                            | `VectorSearcher` interface, `VectorHit`, `SetVectorSearcher`, `HasSemantic`                  |
| `internal/db/search_content.go` (+`_test`)                         | `semantic` and `hybrid` modes, score/context fields                                          |
| `internal/db/messages.go` (+`_test`)                               | `EmbeddableMessages` scan, `GetMessagesWindow`                                               |
| `internal/postgres/messages.go`, `internal/duckdb/store.go`        | `GetMessagesWindow` parity + `HasSemantic() false` + semantic-mode error                     |
| `internal/service/service.go`, `direct.go`, `http.go`              | Mode/score/context/window threading, `ErrSemanticUnavailable`                                |
| `internal/server/huma_routes_search.go`, `huma_routes_sessions.go` | mode enum, `context`, `around/before/after/roles` params, 501 mapping                        |
| `internal/server/huma_routes_embeddings.go` (+`_test`)             | build/status/generations/activate/retire endpoints                                           |
| `internal/mcp/tools.go` (+`_test`)                                 | `search_content` mode+context, `get_messages` around window                                  |
| `cmd/agentsview/embeddings.go` (+`_test`)                          | `embeddings build/list/activate/retire` command group                                        |
| `cmd/agentsview/session_search.go`, `session_messages.go`          | `--semantic/--hybrid/--context`, `--around/--before/--after/--role`                          |
| `cmd/agentsview/main.go`                                           | serve wiring: vector index open, searcher install, embed scheduler                           |
| `cmd/agentsview/embed_scheduler.go` (+`_test`)                     | debounced after-sync fill + backstop ticker                                                  |
| `docs/docs/usage/semantic-search.md` (or repo's docs layout)       | user docs                                                                                    |

Key kit v0.2.0 APIs (already verified against the module cache — see spec
"Dependencies" for the full signature list): `vector.Split`,
`vector.EncodeFunc`, `vector.EncodeBatched`, `vector.Generation.Fingerprint`,
`vector.Fill(...) (FillStats, error)`,
`vector.Search(ctx, store, queryText, encFor, o) ([]Hit[K], error)`,
`vector.Merge(perGeneration, MergeOptions{Strategy: MergeReciprocalRank})`,
`sqlitevec.New[K, G](ctx, db, schema)`,
`sqlitevec.Schema{DocsTable, IDColumn, ContentColumn, EmbedGenColumn, RevisionColumn, VectorsPrefix}`,
`(*sqlitevec.Store).EnsureGeneration(ctx, gen, model, state)`,
`SetGenerationState`, `DeleteVectors`, `LiveGenerations`, `QueryGeneration`.
Generation key type `G = string` (the fingerprint is the `gen_key`); document
key type `K = string` (the mirror `doc_key`).

______________________________________________________________________

### Task 1: Pin kit v0.2.0 and smoke-test the sqlitevec store

**Files:**

- Modify: `go.mod`, `go.sum`
- Create: `internal/vector/kit_smoke_test.go`

**Interfaces:**

- Produces: the `go.kenn.io/kit/vector` + `vector/sqlitevec` dependency every
  later task imports; confirms the vec0 extension links under this repo's
  build tags.

- [ ] **Step 1: Bump the pin**

Run:

```bash
go get go.kenn.io/kit@v0.2.0 && go mod tidy
```

Expected: `go.mod` shows `go.kenn.io/kit v0.2.0`; `go.sum` gains
`github.com/asg017/sqlite-vec-go-bindings` (transitive).

- [ ] **Step 2: Write the smoke test**

Create `internal/vector/kit_smoke_test.go` (package `vector`); this is the
API-reality check the spec requires:

```go
package vector

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
	"go.kenn.io/kit/vector/sqlitevec"
)

func TestKitSqlitevecRoundTrip(t *testing.T) {
	sqlitevec.Register()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "v.db"))
	require.NoError(t, err)
	defer db.Close()
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `CREATE TABLE docs (
        doc_key TEXT PRIMARY KEY, content TEXT NOT NULL,
        content_hash TEXT NOT NULL, embed_gen TEXT)`)
	require.NoError(t, err)
	store, err := sqlitevec.New[string, string](ctx, db, sqlitevec.Schema{
		DocsTable: "docs", IDColumn: "doc_key", ContentColumn: "content",
		EmbedGenColumn: "embed_gen", RevisionColumn: "content_hash",
		VectorsPrefix: "docs_vectors",
	})
	require.NoError(t, err)
	gen := kitvec.Generation{Model: "fake", Dimensions: 3}
	fp := gen.Fingerprint()
	require.NoError(t, store.EnsureGeneration(ctx, fp, gen, sqlitevec.StateActive))
	_, err = db.ExecContext(ctx,
		`INSERT INTO docs VALUES ('d1', 'hello world', 'h1', NULL)`)
	require.NoError(t, err)
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}
	stats, err := kitvec.Fill[string, string](ctx, store, fp, enc,
		kitvec.FillOptions[string]{})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Documents)
	hits, err := store.QueryGeneration(ctx, fp, kitvec.Vector{1, 0, 0}, 5)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, "d1", hits[0].Doc)
}
```

- [ ] **Step 3: Run it**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/vector/ -run TestKitSqlitevec -v`
Expected: PASS. If any kit symbol differs from the above, STOP and report the
mismatch (the spec gates on this API), do not improvise.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum internal/vector/kit_smoke_test.go
git commit -m "build: pin go.kenn.io/kit v0.2.0 and smoke-test sqlitevec"
```

______________________________________________________________________

### Task 2: Vector configuration

**Files:**

- Modify: `internal/config/config.go` (Config struct + load/validate path)
- Test: `internal/config/config_vector_test.go`

**Interfaces:**

- Produces:

```go
// in package config
type VectorConfig struct {
	Enabled    bool                   `toml:"enabled"`
	DBPath     string                 `toml:"db_path"`
	Embeddings VectorEmbeddingsConfig `toml:"embeddings"`
	Embed      VectorEmbedConfig      `toml:"embed"`
}
type VectorEmbeddingsConfig struct {
	Endpoint      string `toml:"endpoint"`
	Model         string `toml:"model"`
	Dimension     int    `toml:"dimension"`
	APIKeyEnv     string `toml:"api_key_env"`
	BatchSize     int    `toml:"batch_size"`      // default 32
	Timeout       string `toml:"timeout"`         // default "30s"
	MaxRetries    int    `toml:"max_retries"`     // default 3
	MaxInputChars int    `toml:"max_input_chars"` // default 8192
}
type VectorEmbedConfig struct {
	RunAfterSync     *bool  `toml:"run_after_sync"`    // default true
	BackstopInterval string `toml:"backstop_interval"` // default "24h"; negative disables
}
func (c VectorConfig) Validate() error
func (c VectorConfig) ResolvedDBPath(dataDir string) string // default <dataDir>/vectors.db
func (c VectorEmbeddingsConfig) APIKey() string             // os.Getenv(APIKeyEnv), "" when unset
func (c VectorEmbedConfig) RunAfterSyncEnabled() bool
```

Follow the existing config-section pattern in `internal/config/config.go` (match
how `PGConfig`/`DuckDBConfig` are declared, defaulted, and hung off `Config`);
add a field to `Config`:

```go
Vector VectorConfig `toml:"vector"`
```

`Validate()` is a no-op when `!Enabled`; when enabled it requires
`Embeddings.Endpoint`, `Embeddings.Model`, `Embeddings.Dimension > 0`, and
parseable durations, each failure a distinct actionable message, e.g.
"[vector.embeddings] model is required when [vector] is enabled".

- [ ] **Step 1: Write failing tests** in `internal/config/config_vector_test.go`
  — table-driven `TestVectorConfigValidate` covering: disabled-is-valid;
  enabled-missing-endpoint / -model / -dimension; bad `timeout`; bad
  `backstop_interval`; fully valid. Plus `TestVectorConfigDefaults` (BatchSize
  32, Timeout "30s", MaxRetries 3, MaxInputChars 8192, RunAfterSyncEnabled
  true, ResolvedDBPath fallback and explicit override) and
  `TestVectorConfigAPIKeyEnv` using `t.Setenv`.
- [ ] **Step 2: Run**
  `CGO_ENABLED=1 go test -tags fts5 ./internal/config/ -run TestVectorConfig -v`
  — expect FAIL (types undefined).
- [ ] **Step 3: Implement** the types, defaults (applied where the config load
  path applies other section defaults), `Validate`, and helpers. Call
  `Vector.Validate()` from the same place other sections validate.
- [ ] **Step 4: Run the tests again** — expect PASS; also run
  `CGO_ENABLED=1 go test -tags fts5 ./internal/config/`.
- [ ] **Step 5: Commit** `feat(config): add [vector] configuration section`

______________________________________________________________________

### Task 3: OpenAI-compatible embeddings encoder

**Files:**

- Create: `internal/vector/encoder.go`
- Test: `internal/vector/encoder_test.go`

**Interfaces:**

- Produces:

```go
// in package vector (internal/vector)
type EncoderConfig struct {
	Endpoint   string        // base URL incl. /v1; "/embeddings" appended
	APIKey     string        // "" = anonymous
	Model      string
	Dimension  int           // every returned vector must have this length
	Timeout    time.Duration // per-request
	MaxRetries int           // attempts on 429/5xx/network; 4xx fail fast
}
func NewEncoder(cfg EncoderConfig) kitvec.EncodeFunc
```

The returned func POSTs `{"model": cfg.Model, "input": texts}` to
`strings.TrimRight(cfg.Endpoint, "/") + "/embeddings"`, sets
`Authorization: Bearer <key>` when APIKey non-empty, decodes the standard OpenAI
shape `{"data":[{"index":0,"embedding":[...]}, ...]}`, reorders by `index`, and
errors if `len(data) != len(texts)` or any embedding length `!= cfg.Dimension`
(message names got-vs-want dimension and suggests fixing
`[vector.embeddings] dimension`). Retries with capped exponential backoff (250ms
base, x2, max 5s, honor ctx cancellation) on 429/5xx/transport errors up to
MaxRetries total attempts; 4xx returns immediately with status + body excerpt.
Batching/concurrency stay kit's job (`EncodeBatched`) — one HTTP call per
invocation.

- [ ] **Step 1: Write failing tests** in `encoder_test.go` using
  `net/http/httptest`: happy path (verifies request body, auth header, path
  `/v1/embeddings`, out-of-order `index` reordering); dimension mismatch
  error; count mismatch error; 429-then-200 retry succeeds; 500 exhausts
  MaxRetries and fails; 400 fails without retry (assert exactly 1 request via
  a counter); context cancellation aborts backoff promptly.
- [ ] **Step 2: Run**
  `CGO_ENABLED=1 go test -tags fts5 ./internal/vector/ -run TestEncoder -v` —
  FAIL.
- [ ] **Step 3: Implement** `encoder.go` (single
  `http.Client{Timeout: cfg.Timeout}`, no new deps).
- [ ] **Step 4: Run** — PASS. `go vet ./internal/vector/`.
- [ ] **Step 5: Commit**
  `feat(vector): add OpenAI-compatible embeddings encoder`

______________________________________________________________________

### Task 4: vectors.db index core (schema + generations)

**Files:**

- Create: `internal/vector/index.go`
- Test: `internal/vector/index_test.go`

**Interfaces:**

- Produces:

```go
type Index struct { /* db *sql.DB, store *sqlitevec.Store[string, string],
                       split kitvec.SplitOptions, readOnly bool */ }
// Open opens (creating when rw) vectors.db and the kit store.
// maxInputChars sets SplitOptions{MaxRunes: n, Overlap: n / 30}.
func Open(ctx context.Context, path string, readOnly bool, maxInputChars int) (*Index, error)
func (ix *Index) Close() error

// Generation lifecycle. gen_key G is the fingerprint string.
type GenerationInfo struct {
	ID          int64  `json:"id"` // generations-table ordinal, CLI-facing
	State       string `json:"state"`
	Model       string `json:"model"`
	Dimension   int    `json:"dimension"`
	Fingerprint string `json:"fingerprint"`
	Embedded    int64  `json:"embedded"` // stamped docs
	Missing     int64  `json:"missing"`  // mirror docs not stamped
}
func (ix *Index) EnsureGeneration(ctx context.Context, gen kitvec.Generation, state sqlitevec.State) (fingerprint string, err error)
func (ix *Index) Generations(ctx context.Context) ([]GenerationInfo, error)
func (ix *Index) GenerationByID(ctx context.Context, id int64) (GenerationInfo, error)
func (ix *Index) SetStateByID(ctx context.Context, id int64, state sqlitevec.State) error
func (ix *Index) ActiveFingerprint(ctx context.Context) (string, bool, error)   // state='active'
func (ix *Index) BuildingFingerprint(ctx context.Context) (string, bool, error) // state='building'
```

`Open` calls `sqlitevec.Register()` under a package-level `sync.Once`, opens
`sqlite3` DSN (`?mode=ro` + immutable off for readOnly; WAL + busy_timeout for
rw, mirroring `internal/db.Open` pragmas), creates the mirror DDL (rw only),
then `sqlitevec.New[string, string]` with:

```go
sqlitevec.Schema{
	DocsTable: "vector_messages", IDColumn: "doc_key",
	ContentColumn: "content", EmbedGenColumn: "embed_gen",
	RevisionColumn: "content_hash", VectorsPrefix: "message_vectors",
}
```

Mirror DDL (exact, from the spec):

```sql
CREATE TABLE IF NOT EXISTS vector_messages (
    doc_key      TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL,
    source_uuid  TEXT NOT NULL DEFAULT '',
    ordinal      INTEGER NOT NULL,
    content      TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    embed_gen    TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vector_messages_session_ordinal
    ON vector_messages(session_id, ordinal);
CREATE TABLE IF NOT EXISTS vector_meta (
    key TEXT PRIMARY KEY, value TEXT NOT NULL
);
```

Coverage counts for `Generations()` (join by fingerprint; stamps table is
`message_vectors_stamps(ordinal, doc_key, revision)` where `ordinal` is the
generation ordinal):

```sql
SELECT g.ordinal, g.gen_key, g.fingerprint, g.dimension, g.state,
       (SELECT COUNT(*) FROM message_vectors_stamps s WHERE s.ordinal = g.ordinal),
       (SELECT COUNT(*) FROM vector_messages d WHERE NOT EXISTS
          (SELECT 1 FROM message_vectors_stamps s
           WHERE s.ordinal = g.ordinal AND s.doc_key = d.doc_key))
FROM message_vectors_generations g ORDER BY g.ordinal
```

`GenerationInfo.Model` is stored by us in `vector_meta` under key
`gen_model:<fingerprint>` when EnsureGeneration runs (kit stores only the
fingerprint), so `Generations()` can display it.

- [ ] **Step 1: Write failing tests** in `index_test.go`:
  `TestOpenCreatesSchema` (rw open then query `vector_messages` and
  `message_vectors_generations` exist); `TestOpenReadOnlyOnMissingFileFails`;
  `TestEnsureGenerationLifecycle` (ensure building → Generations shows
  state/model/dimension, ID assigned → `SetStateByID` to active →
  `ActiveFingerprint` returns it → `BuildingFingerprint` now false);
  `TestGenerationCoverageCounts` (insert 2 mirror rows, stamp 1 via `store`
  SaveVectors with a fake vector, assert Embedded=1 Missing=1).
- [ ] **Step 2: Run**
  `... -run 'TestOpen|TestEnsureGeneration|TestGenerationCoverage' -v` — FAIL.
- [ ] **Step 3: Implement `index.go`.** Expose the kit store and split options
  to sibling files via unexported fields (`ix.store`, `ix.split`, `ix.db`).
- [ ] **Step 4: Run** — PASS. `go vet ./internal/vector/`.
- [ ] **Step 5: Commit**
  `feat(vector): add vectors.db index core with generation lifecycle`

______________________________________________________________________

### Task 5: Embeddable-message scan + mirror refresh

**Files:**

- Modify: `internal/db/messages.go` (scan), `internal/db/search.go` only if the
  system-prefix helper needs exporting (it is already shared; reuse it)
- Create: `internal/vector/mirror.go`
- Test: `internal/db/messages_embeddable_test.go`,
  `internal/vector/mirror_test.go`

**Interfaces:**

- Produces (in `internal/db`):

```go
// EmbeddableMessage is one user/assistant message eligible for vector
// indexing (role user|assistant, is_system = 0, not system-prefixed).
type EmbeddableMessage struct {
	SessionID  string
	SourceUUID string
	Ordinal    int
	Content    string
}
// ScanEmbeddableMessages streams the embeddable universe, ordered by
// (session_id, ordinal). since != "" restricts to sessions with
// ended_at >= since (RFC3339) for incremental refresh; "" scans all.
// maxEnded returns the maximum sessions.ended_at seen ("" when none).
func (db *DB) ScanEmbeddableMessages(ctx context.Context, since string,
	fn func(EmbeddableMessage) error) (maxEnded string, err error)
```

SQL:
`SELECT m.session_id, m.source_uuid, m.ordinal, m.content, s.ended_at FROM messages m JOIN sessions s ON s.id = m.session_id WHERE m.role IN ('user','assistant') AND m.is_system = 0 AND NOT (<SystemPrefixSQL on m.content>) [AND s.ended_at >= ?] ORDER BY m.session_id, m.ordinal`
— reuse the existing system-prefix SQL helper from `internal/db/search.go`
exactly as the FTS branch does.

- Produces (in `internal/vector`):

```go
// MessageSource is the slice of the archive the mirror needs (implemented
// by *db.DB).
type MessageSource interface {
	ScanEmbeddableMessages(ctx context.Context, since string,
		fn func(db.EmbeddableMessage) error) (string, error)
}
type RefreshStats struct{ Upserted, Deleted, Unchanged int }
// Refresh reconciles the mirror. full=true scans everything and deletes
// mirror rows (and their vectors, via store.DeleteVectors) whose identity
// was not seen; full=false scans sessions newer than the stored watermark
// and only upserts. The watermark (vector_meta key "refresh_watermark") is
// advanced to the scan's max ended_at afterwards.
func (ix *Index) Refresh(ctx context.Context, src MessageSource, full bool) (RefreshStats, error)
func DocKey(sessionID, sourceUUID string, ordinal, occurrence int) string
```

`DocKey` implements the spec's identity: `"u:" + sessionID + ":" + sourceUUID`
when sourceUUID non-empty (with an occurrence suffix appended for occurrence
greater than 1 — the schema permits duplicate source_uuids within a session;
Refresh assigns occurrences in scan order, which is deterministic), else
`"o:" + sessionID + ":" + strconv.Itoa(ordinal)`. sessionID and sourceUUID are
percent-escaped before joining (the delimiter characters and the percent sign
itself), so a value containing one of them can never be mistaken for key
structure and the encoding stays injective. Upsert semantics per row: compute
`content_hash` as the hex-encoded sha256 of the content, then upsert on conflict
by doc_key, updating session_id, ordinal, content, and content_hash — the hash
change is what invalidates kit's revision stamp; an ordinal-only change updates
the cursor without re-embedding. Guard the `(session_id, ordinal)` unique index:
before upserting, park any existing row occupying the same slot under a
different doc_key at a unique negative ordinal rather than deleting it outright
(an ordinal shift can land a uuid-keyed row on a slot previously held by another
row, and that other row's own doc_key is often reinserted moments later in the
same scan at its new ordinal). Once the whole scan completes, resolve every
parked doc_key: one still at its negative ordinal was never reinserted, so it is
genuinely gone — call `ix.store.DeleteVectors` and then delete the mirror row;
one whose ordinal was overwritten back to a real value was reinserted under its
own doc_key elsewhere in the scan and keeps its row and vectors untouched.
Parking instead of deleting outright matters because a fresh `INSERT` on
reinsertion would reset the embed_gen column kit's SaveVectors stamped it with,
silently uncovering the document; the in-place `ON CONFLICT` update reinsertion
goes through never touches it. Deletion in full mode: collect seen doc_keys in
an in-memory set; then for each mirror doc_key not seen, call
`ix.store.DeleteVectors(ctx, key)` then
`DELETE FROM vector_messages WHERE doc_key = ?` (spec: deleting only the row
leaves orphaned vectors occupying KNN slots).

- [ ] **Step 1: Write failing db-scan tests** in
  `internal/db/messages_embeddable_test.go` using the existing `testDB(t)`
  helper: seed one session with user/assistant/tool/system-role messages plus
  a system-prefixed user message; assert only the clean user/assistant rows
  stream back in ordinal order, `since` filtering excludes an older session,
  and `maxEnded` is the max ended_at.
- [ ] **Step 2: Run**
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestScanEmbeddable -v`
  — FAIL.
- [ ] **Step 3: Implement `ScanEmbeddableMessages`; run — PASS.**
- [ ] **Step 4: Write failing mirror tests** in `mirror_test.go` with a fake
  `MessageSource` (slice-backed): initial full refresh inserts rows with
  correct doc_keys (uuid and legacy forms); content change updates hash;
  ordinal shift on uuid row keeps hash (assert stamp survival: stamp a doc via
  SaveVectors, shift ordinal, refresh, assert it is NOT pending); ordinal
  shift onto a slot held by a stale legacy row evicts that row; full refresh
  deletes vanished identities and their vectors (stamp+chunks gone);
  incremental refresh uses the watermark (fake source asserts the `since` it
  received).
- [ ] **Step 5: Run** `... ./internal/vector/ -run TestRefresh -v` — FAIL.
- [ ] **Step 6: Implement `mirror.go`; run — PASS.** `go vet ./...`.
- [ ] **Step 7: Commit**
  `feat(vector): mirror refresh with resync-stable doc keys`

**Checkpoint: invoke the `roborev-fix` skill now (after Task 5) and resolve all
findings before Task 6.**

______________________________________________________________________

### Task 6: Build orchestration (fill, rebuild, backstop, progress)

**Files:**

- Create: `internal/vector/build.go`
- Test: `internal/vector/build_test.go`

**Interfaces:**

- Produces:

```go
type BuildOptions struct {
	FullRebuild bool
	Backstop    bool // full reconciliation scan (ignore watermark)
	BatchSize   int  // encode batch size (config batch_size)
	Progress    func(BuildProgress) // optional, called at most ~every 2s
}
type BuildProgress struct {
	Phase   string // "scanning" | "embedding"
	Done    int64  // chunks encoded so far
	Total   int64  // pending docs at start (approximate denominator)
}
type BuildResult struct {
	Fingerprint string
	Activated   bool // building generation auto-activated on completion
	Refresh     RefreshStats
	Fill        kitvec.FillStats
}
// Build runs one embedding pass against gen (the desired vector space,
// from config: Model, Dimensions, Params{"max_input_chars": itoa(n)}).
func (ix *Index) Build(ctx context.Context, src MessageSource,
	enc kitvec.EncodeFunc, gen kitvec.Generation, o BuildOptions) (BuildResult, error)
```

Behavior (exact decision table — implement in this order):

1. `Refresh(ctx, src, full: o.FullRebuild || o.Backstop || firstEver)` where
   `firstEver` = no watermark stored yet.
1. Resolve target: `fp := gen.Fingerprint()`. Cases:
    - active exists with same fp and !FullRebuild → target = active fp
      (incremental top-up).
    - active exists with different fp → target must be a building generation for
      fp: `EnsureGeneration(gen, StateBuilding)`. If !FullRebuild, still
      proceed (a config change implies a rebuild; print-level messaging is the
      CLI's job).
    - no active → `EnsureGeneration(gen, StateBuilding)`.
    - FullRebuild with same fp as active → clear that generation's stamps and
      chunks (`DELETE FROM message_vectors_stamps WHERE ordinal = ?`;
      `DELETE FROM message_vectors_chunks WHERE ordinal = ?`; delete the
      matching rows from its vec0 table `message_vectors_v<ordinal>` first via
      `DELETE FROM message_vectors_v<N>`), then fill it again. Keep this in a
      single transaction helper `resetGeneration(ctx, fp)`.
1. Count pending for the denominator:
   `SELECT COUNT(*) FROM vector_messages d WHERE NOT EXISTS (SELECT 1 FROM message_vectors_stamps s WHERE s.ordinal = ? AND s.doc_key = d.doc_key)`.
1. Wrap `enc` in a progress encoder that atomically adds `len(texts)` to a chunk
   counter and invokes `o.Progress` (rate-limited, `time.Since` guard).
1. `kitvec.Fill[string, string](ctx, ix.store, fp, wrappedEnc, kitvec.FillOptions[string]{Split: ix.split, Batch: kitvec.BatchOptions{BatchSize: o.BatchSize, Concurrency: 1}, OnEncodeError: nil})`
   — nil OnEncodeError: transient failures abort, the next run resumes (kit
   stamps done work).
1. If target was building and post-fill Missing == 0: activate — in one
   transaction set old active (if any) to retired and building to active
   (`SetGenerationState` twice; acceptable as two statements guarded by the
   single-writer lock). `Activated = true`.

- [ ] **Step 1: Write failing tests** in `build_test.go` (fake source + fake
  deterministic encoder from the smoke test, dimension 3): first build creates
  \+ activates a generation and embeds all docs; second build with no changes
  fills 0; content change re-embeds exactly 1; model change (new fingerprint)
  builds a second generation building→active and retires the old one;
  FullRebuild with same fingerprint re-embeds everything (fill Documents ==
  corpus size); Progress receives a final Done equal to total chunks; encoder
  error aborts and a retry resumes without re-embedding completed docs.
- [ ] **Step 2: Run** `... -run TestBuild -v` — FAIL.
- [ ] **Step 3: Implement `build.go`; run — PASS.**
- [ ] **Step 4: Commit**
  `feat(vector): build orchestration with generations and progress`

______________________________________________________________________

### Task 7: Semantic search core

**Files:**

- Create: `internal/vector/search.go`
- Test: `internal/vector/search_test.go`

**Interfaces:**

- Produces:

```go
type Hit struct {
	SessionID string
	Ordinal   int
	Score     float32
	Snippet   string
}
// Search embeds the query and returns up to limit message-level hits,
// best first. It returns ErrNoActiveGeneration when no live generation
// exists, and ErrIndexBuilding (with Progress percent) when only a
// building generation exists.
func (ix *Index) Search(ctx context.Context, enc kitvec.EncodeFunc,
	query string, limit int) ([]Hit, error)
var ErrNoActiveGeneration = errors.New("no active embedding generation")
type BuildingError struct{ Percent int }
func (e *BuildingError) Error() string
// StaleActive reports whether the active generation's fingerprint differs
// from want (the fingerprint of the current config) — the "index stale"
// signal surfaced by the db layer.
func (ix *Index) StaleActive(ctx context.Context, want string) (bool, error)
```

Implementation:
`kitvec.Search[string, string](ctx, ix.store, query, func(string) kitvec.EncodeFunc { return enc }, kitvec.SearchOptions{ PerGeneration: limit, Merge: kitvec.MergeOptions{Limit: limit}})`
(kit handles rollup + cross-generation merge). Then one query maps doc_keys to
`(session_id, ordinal, content)` via the mirror, and the snippet is computed by
re-splitting content with `ix.split`, taking the hit's `ChunkIndex` chunk, and
truncating to 200 runes (append `…` when truncated). Hits whose doc_key vanished
from the mirror mid-flight are dropped.

- [ ] **Step 1: Write failing tests**: seed 3 docs via fake source + build with
  a fake encoder that maps distinct known texts to orthogonal vectors (e.g.
  contains "alpha" → {1,0,0}, "beta" → {0,1,0}, else {0,0,1}); query "alpha"
  returns the alpha doc first with Score ≈ 1 and a snippet containing its
  text; limit caps results; empty index (no generations) →
  `ErrNoActiveGeneration`; building-only index → `*BuildingError`;
  `StaleActive` true when fingerprints differ.
- [ ] **Step 2: Run** `... -run TestSearch -v` — FAIL.
- [ ] **Step 3: Implement; run — PASS.** `go vet ./internal/vector/`.
- [ ] **Step 4: Commit** `feat(vector): semantic search over the mirror index`

______________________________________________________________________

### Task 8: db-layer semantic mode + capability gating (all backends)

**Files:**

- Create: `internal/db/vector.go`
- Modify: `internal/db/store.go`, `internal/db/search_content.go`,
  `internal/postgres/messages.go`, `internal/duckdb/store.go`,
  `internal/service/service.go`, `internal/service/direct.go`,
  `internal/service/http.go`, `internal/server/huma_routes_search.go`
- Test: `internal/db/search_content_semantic_test.go`, plus one-line assertions
  in existing PG/DuckDB test files for the stubs

**Interfaces:**

- Produces (in `internal/db`):

```go
// vector.go
var ErrSemanticUnavailable = errors.New(
	"semantic search not available: enable [vector] in config.toml and run 'agentsview embeddings build'")
type VectorHit struct {
	SessionID string
	Ordinal   int
	Score     float32
	Snippet   string
}
type VectorSearcher interface {
	SemanticSearch(ctx context.Context, query string, limit int) ([]VectorHit, error)
}
func (db *DB) SetVectorSearcher(v VectorSearcher)
func (db *DB) HasSemantic() bool // v != nil
```

- Store interface gains `HasSemantic() bool` (next to `HasFTS()`); PG and DuckDB
  implement it returning `false`, and their `SearchContent` returns
  `ErrSemanticUnavailable` for modes `semantic`/`hybrid` before any query.
- `ContentMatch` gains a score field (nil for existing modes):

```go
Score *float64 `json:"score,omitempty"`
```

- `db.SearchContent` mode switch gains:

```go
case "semantic":
	return db.searchContentSemantic(ctx, f)
case "hybrid":
	return db.searchContentHybrid(ctx, f) // Task 9; until then return ErrSemanticUnavailable
```

`searchContentSemantic`: input validation first — sources must be empty or
exactly `{"messages"}` (`SearchInputError` mirroring the fts wording);
`f.Cursor != 0` →
`SearchInputError("semantic search returns a single ranked page; cursor pagination is not supported")`.
Then: no searcher → `ErrSemanticUnavailable`. Over-fetch
`k := max(f.Limit*4, 200)` hits from the searcher; keep hits whose session
passes the metadata filters by loading the allowed session-id set once:

```sql
SELECT id FROM sessions WHERE <buildSessionFilter(sf) via sessionScopeSubquery's SessionFilter mapping>
  AND id IN (<hit session ids>)
```

then enrich surviving `(session_id, ordinal)` pairs in one query:

```sql
SELECT m.session_id, s.project, s.agent, m.role, m.ordinal, m.timestamp
FROM messages m JOIN sessions s ON s.id = m.session_id
WHERE (m.session_id, m.ordinal) IN (VALUES ...)
```

(SQLite lacks row-value IN over VALUES in older versions — use
`WHERE m.session_id = ? AND m.ordinal = ?` batched with OR groups, or a
CARTESIAN-safe join on a `WITH hits(session_id, ordinal) AS (VALUES ...)` CTE;
the CTE form is the one to implement.) Build
`ContentMatch{Location: "message", Snippet: hit.Snippet, Score: ptr(float64(hit.Score))}`
preserving searcher rank order, truncated to `f.Limit`. Secret redaction: pass
each snippet through the same redaction helper the substring path uses on its
snippets unless `f.RevealSecrets` (find it where `contentSnippetRadius` is
consumed; reuse, don't duplicate).

- Produces (in `internal/service`):
  `var ErrSemanticUnavailable = db.ErrSemanticUnavailable`;
  `directBackend.SearchContent` passes the error through;
  `httpBackend.SearchContent` maps HTTP 501 → `service.ErrSemanticUnavailable`
  (mirror the existing `ErrSearchUnavailable` 501 mapping in
  `internal/service/http.go`).

- Produces (in `internal/server/huma_routes_search.go`): mode enum becomes
  `enum:"substring,regex,fts,semantic,hybrid"`; the handler maps
  `errors.Is(err, db.ErrSemanticUnavailable)` → 501 (same pattern as the FTS
  unavailable path).

- [ ] **Step 1: Write failing db tests** in `search_content_semantic_test.go`
  with a fake `VectorSearcher` (canned hits): mode routes to searcher and
  returns matches with Score set and rank order preserved; project filter
  drops non-matching sessions; limit trims; cursor rejected as
  `SearchInputError`; `tool_input` source rejected; no searcher →
  `ErrSemanticUnavailable`; `HasSemantic` flips with `SetVectorSearcher`.

- [ ] **Step 2: Run**
  `CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestSearchContentSemantic -v`
  — FAIL.

- [ ] **Step 3: Implement db layer** (vector.go + search_content.go + store.go
  interface method). Run — PASS.

- [ ] **Step 4: Implement PG/DuckDB stubs** (compile is forced by the Store
  interface — `HasSemantic() bool { return false }` and the early-return in
  each `SearchContent`). Add stub assertions to an existing non-pgtest test
  file in each package (construct nothing new; a unit test on the mode
  early-return with a zero-value store is enough where feasible, otherwise
  behind the pgtest tag next to existing SearchContent tests).

- [ ] **Step 5: Wire service + huma 501 mapping.** Run
  `CGO_ENABLED=1 go test -tags fts5 ./internal/service/ ./internal/server/`.

- [ ] **Step 6: `go build -tags fts5 ./...` then commit**
  `feat(db): semantic content-search mode with capability gating`

______________________________________________________________________

### Task 9: Hybrid mode (RRF)

**Files:**

- Modify: `internal/db/search_content.go`
- Test: `internal/db/search_content_hybrid_test.go`

**Interfaces:**

- Consumes: `kitvec.Merge` with `MergeReciprocalRank` (RankConstant 60),
  `db.VectorSearcher`.
- Produces: `db.searchContentHybrid` — mode `hybrid` returns RRF-fused
  FTS+vector matches, `Score` = RRF score.

Implementation: same validation as semantic (messages-only, no cursor, searcher
required — hybrid additionally requires `db.HasFTS()`). Two legs, each
over-fetched to `k := max(f.Limit*4, 200)`:

1. Vector leg: `searcher.SemanticSearch(ctx, f.Pattern, k)` →
   `[]kitvec.Hit[matchKey]` in rank order (Score from searcher), where
   `type matchKey struct{ SessionID string; Ordinal int }`.

1. FTS leg: a dedicated ranked query restricted to the embedded universe (role
   user/assistant, `is_system = 0`, system-prefix excluded — reuse the scan
   predicate from Task 5), pattern prepared with the existing
   `PrepareFTSQuery`:

    ```sql
    SELECT m.session_id, m.ordinal,
           snippet(messages_fts, 0, '', '', '...', 32) AS snip
    FROM messages_fts f JOIN messages m ON m.id = f.rowid
    WHERE messages_fts MATCH ? AND m.role IN ('user','assistant')
      AND m.is_system = 0 AND NOT (<system-prefix SQL>)
      AND <session scope subquery>
    ORDER BY f.rank LIMIT ?
    ```

1. Fuse:
   `kitvec.Merge([][]kitvec.Hit[matchKey]{vecLeg, ftsLeg}, kitvec.MergeOptions{Strategy: kitvec.MergeReciprocalRank, RankConstant: 60, Limit: f.Limit})`
   — vector leg first so its snippet wins on overlap.

1. Enrich the fused keys with the same CTE join as semantic; snippet: vector
   snippet when the key came from (or also from) the vector leg, else the FTS
   leg's `snip` (redacted like the semantic path). Score = merged Score.

Note the session-scope filter runs inside the FTS leg but only post-hoc on the
vector leg (over-fetch + filter, spec-documented limitation) — apply the
session-set filter to the vector leg BEFORE merging so RRF ranks only eligible
docs.

- [ ] **Step 1: Write failing tests** with fake searcher + real FTS (testDB
  seeds messages, FTS triggers populate): a doc ranked top by both legs
  outranks single-leg docs; a vector-only hit and an FTS-only hit both appear;
  scores strictly descending; project filter constrains both legs; hybrid
  without searcher → `ErrSemanticUnavailable`; cursor rejected.
- [ ] **Step 2: Run** `... -run TestSearchContentHybrid -v` — FAIL.
- [ ] **Step 3: Implement; run — PASS.** `go vet ./internal/db/`.
- [ ] **Step 4: Commit** `feat(db): hybrid RRF content-search mode`

______________________________________________________________________

### Task 10: CLI + HTTP search surface (`--semantic`, `--hybrid`)

**Files:**

- Modify: `cmd/agentsview/session_search.go`
- Test: `cmd/agentsview/session_search_test.go` (extend existing patterns in the
  package's command tests)

**Interfaces:**

- Consumes: modes already accepted end-to-end (Tasks 8–9);
  `service.ErrSemanticUnavailable`.
- Produces: `session search --semantic|--hybrid` flags; human output shows
  `score=0.83` before the snippet for scored matches.

Flag wiring in `newSessionSearchCommand` (extend the existing mutually-exclusive
mode block):

```go
var useSemantic, useHybrid bool
// in RunE, replacing the current pairwise check:
modes := 0
for _, b := range []bool{useRegex, useFTS, useSemantic, useHybrid} {
	if b { modes++ }
}
if modes > 1 {
	return fmt.Errorf("--regex, --fts, --semantic and --hybrid are mutually exclusive")
}
mode := "substring"
switch {
case useRegex: mode = "regex"
case useFTS: mode = "fts"
case useSemantic: mode = "semantic"
case useHybrid: mode = "hybrid"
}
if (useSemantic || useHybrid) && len(sources) > 0 {
	for _, s := range sources {
		if s != "messages" {
			return fmt.Errorf("--%s searches messages only; drop --in", mode)
		}
	}
}
```

plus flag definitions:

```go
flags.BoolVar(&useSemantic, "semantic", false,
	"Semantic (vector) search over user/assistant messages")
flags.BoolVar(&useHybrid, "hybrid", false,
	"Hybrid semantic + full-text search (reciprocal rank fusion)")
```

`printContentMatchesHuman` prints ` score=%.2f` after the ordinal when
`m.Score != nil`.

- [ ] **Step 1: Write failing CLI tests** following the package's existing
  command-test style (look at how `session_search` or sibling commands are
  tested — likely via a fake service): mode mapping for each flag; the
  mutual-exclusion error; `--semantic --in tool_input` rejected; human output
  includes `score=` for scored matches; JSON output round-trips `score`.
- [ ] **Step 2: Run**
  `CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/ -run TestSessionSearch -v`
  — FAIL.
- [ ] **Step 3: Implement; run — PASS.**
- [ ] **Step 4: Run** `make test-short` (catches the OpenAPI/generated-client
  assertions if the mode enum change affects them; follow any failing test's
  own regeneration instructions).
- [ ] **Step 5: Commit**
  `feat(cli): session search --semantic and --hybrid modes`

**Checkpoint: invoke the `roborev-fix` skill now (after Task 10) and resolve all
findings before Task 11.**

______________________________________________________________________

### Task 11: Message windows (`--around/--before/--after/--role`)

**Files:**

- Modify: `internal/db/store.go`, `internal/db/messages.go`,
  `internal/postgres/messages.go` (or sibling PG file holding GetMessages),
  `internal/duckdb/store.go`, `internal/service/service.go`,
  `internal/service/direct.go`, `internal/service/http.go`,
  `internal/server/huma_routes_sessions.go`,
  `cmd/agentsview/session_messages.go`
- Test: `internal/db/messages_window_test.go`, service/HTTP tests in their
  existing test files, `cmd/agentsview/session_messages_test.go`

**Interfaces:**

- Produces (in `internal/db`, and identically in PG/DuckDB — parity):

```go
// MessageWindow parameterises GetMessagesWindow. Exactly one retrieval
// mode: Around non-nil = symmetric window; otherwise linear from/limit.
type MessageWindow struct {
	From      *int
	Limit     int
	Asc       bool
	Around    *int
	Before    int // used only with Around; default handled by caller
	After     int
	Roles     []string // empty = all roles
}
func (db *DB) GetMessagesWindow(ctx context.Context, sessionID string,
	w MessageWindow) ([]Message, error)
```

Store interface gains
`GetMessagesWindow(ctx context.Context, sessionID string, w MessageWindow) ([]Message, error)`.

SQLite implementation: linear mode = the existing `GetMessages` query plus
`AND role IN (...)` when Roles set (delegate to `GetMessages` when Roles is
empty). Around mode = three parts merged ascending:

```sql
-- before (role-filtered rows strictly below the anchor, closest first)
SELECT <cols> FROM messages WHERE session_id = ? AND ordinal < ?
  [AND role IN (...)] ORDER BY ordinal DESC LIMIT ?
-- anchor (always included regardless of role filter)
SELECT <cols> FROM messages WHERE session_id = ? AND ordinal = ?
-- after
SELECT <cols> FROM messages WHERE session_id = ? AND ordinal > ?
  [AND role IN (...)] ORDER BY ordinal ASC LIMIT ?
```

Reverse the before-slice, concatenate before+anchor+after. PG and DuckDB: same
three statements with their placeholder syntax, identical ordering and
anchor-inclusion semantics.

- Produces (in `internal/service`): `MessageFilter` gains

```go
Around *int     `json:"around,omitempty"`
Before *int     `json:"before,omitempty"` // default 5 when Around set
After  *int     `json:"after,omitempty"`  // default 5 when Around set
Roles  []string `json:"roles,omitempty"`
```

`directBackend.Messages` validation:
`Around != nil && (From != nil || Direction != "")` → error
`"around is mutually exclusive with from/direction"`;
`(Before != nil || After != nil) && Around == nil` → error
`"before/after require around"`. Routes to `GetMessagesWindow`;
`MessageList.Count` = `len(Messages)`. `httpBackend.Messages` adds query params
`around`, `before`, `after`, `roles` (comma-joined).

- Produces (HTTP): `messageListInput` gains

```go
Around optionalIntParam `query:"around" minimum:"0" doc:"Center a symmetric window on this ordinal (mutually exclusive with from/direction)"`
Before optionalIntParam `query:"before" minimum:"0" doc:"Messages before the around anchor (default 5)"`
After  optionalIntParam `query:"after" minimum:"0" doc:"Messages after the around anchor (default 5)"`
Roles  string           `query:"roles" doc:"Comma-separated roles to include, e.g. user,assistant"`
```

Handler maps them into `service.MessageFilter`; validation errors from the
service surface as 422/400 per the repo's existing input-error mapping.

- Produces (CLI): `session messages` gains

```go
flags.IntVar(&around, "around", 0, "Center a window on this ordinal (use with --before/--after)")
flags.IntVar(&before, "before", 5, "Messages before --around (default 5)")
flags.IntVar(&after, "after", 5, "Messages after --around (default 5)")
flags.StringVar(&role, "role", "", "Comma-separated roles to include, e.g. user,assistant")
```

`--around` presence via `cmd.Flags().Changed("around")` (ordinal 0 valid);
`--around` with explicit `--from`/`--direction` → the service error surfaces;
`--before/--after` changed without `--around` → CLI error. `--role` parses
comma-separated into `Roles`. Human output unchanged (`printMessagesHuman`).

CRITICAL flag-default gotcha: the existing command always sends
`Direction: direction` where the flag defaults to `"asc"`, which would trip the
around-vs-direction validation on every default `--around` call. When `--around`
is set, the CLI must populate `Direction` (and `From`) ONLY when
`cmd.Flags().Changed("direction")` / `Changed("from")` — the flag default is
never sent on the around path. Add a test: `session messages <id> --around 5`
with no other flags succeeds.

Response window bounds: `MessageList` gains two fields set by
`directBackend.Messages` from the returned slice (both nil when empty) so
callers page on with `from = last_ordinal + 1` (spec: "responses report the
window's first/last ordinals"):

```go
FirstOrdinal *int `json:"first_ordinal,omitempty"`
LastOrdinal  *int `json:"last_ordinal,omitempty"`
```

- [ ] **Step 1: Write failing db window tests** (`testDB(t)`, seed ~12 messages
  with mixed roles): around mid-session returns before+anchor+after ascending;
  role filter counts filtered messages (5 user/assistant before, not 5
  ordinals); anchor included even when its role is filtered out; around at
  ordinal 0 (no before) and at last ordinal (no after); linear mode with
  roles; empty roles = current behavior (equivalence with `GetMessages`).
- [ ] **Step 2: Run** — FAIL. **Implement SQLite; run — PASS.**
- [ ] **Step 3: Implement PG + DuckDB** (compile forced by Store). Add the
  window test as a shared/table-driven parity test following the repo's
  co-located parity-test pattern (see `internal/db` parity tests for Recent
  Edits as the example; PG side runs under the `pgtest` tag).
- [ ] **Step 4: Write failing service/HTTP/CLI tests** (validation matrix; HTTP
  param pass-through in the httpBackend test style; CLI flag parsing incl.
  `--around 0`). **Implement; run** the three packages' tests — PASS.
- [ ] **Step 5: Run** `make test-short && go vet ./...`.
- [ ] **Step 6: Commit**
  `feat: symmetric message windows with role filtering across backends`

______________________________________________________________________

### Task 12: Inline search context (`--context N`)

**Files:**

- Modify: `internal/db/search_content.go` (ContentMatch fields),
  `internal/service/service.go`, `internal/service/direct.go`,
  `internal/service/http.go`, `internal/server/huma_routes_search.go`,
  `cmd/agentsview/session_search.go`
- Test: `internal/service/direct_test.go` (or the package's existing service
  test file), `cmd/agentsview/session_search_test.go`

**Interfaces:**

- Produces: `ContentSearchRequest.Context int` (0 = off, max 10 — clamp and
  reject >10 with an input error `"context: maximum is 10"`), HTTP query param
  `context`; `db.ContentMatch` gains

```go
ContextBefore []Message `json:"context_before,omitempty"`
ContextAfter  []Message `json:"context_after,omitempty"`
```

Enrichment lives in `directBackend.SearchContent` (serves both transports: the
daemon's huma handler and the direct CLI both pass through it): after the store
returns matches, when `req.Context > 0`, for each match with `Ordinal >= 0` call
`GetMessagesWindow(ctx, m.SessionID, db.MessageWindow{Around: &m.Ordinal, Before: req.Context, After: req.Context})`
and split the result around the anchor into ContextBefore/ContextAfter (anchor
itself excluded from both). CLI flag:

```go
flags.IntVar(&contextN, "context", 0,
	"Include N messages of context before and after each match (max 10)")
```

Human output prints context lines indented two spaces with `role:` prefixes
around the existing match line, before/after respectively, each
terminal-sanitized and truncated to 200 chars.

- [ ] **Step 1: Write failing service test**: fake store returns 2 matches;
  Context=2 populates both slices correctly ordered and excludes the anchor;
  Context=0 leaves them nil; Context=11 rejected; a match with Ordinal -1
  (possible from name-branch results in other search paths — defensive)
  skipped.
- [ ] **Step 2: Run — FAIL. Implement direct backend + request threading + huma
  param + httpBackend query param. Run — PASS.**
- [ ] **Step 3: Write failing CLI test** (human output shows indented context;
  JSON round-trips `context_before`). **Implement; run — PASS.**
- [ ] **Step 4: Run** `make test-short`.
- [ ] **Step 5: Commit** `feat: inline context windows on content search`

______________________________________________________________________

### Task 13: MCP surface

**Files:**

- Modify: `internal/mcp/tools.go`, `internal/mcp/server.go` (tool descriptions)
- Test: `internal/mcp/tools_test.go` (extend existing)

**Interfaces:**

- Consumes: everything from Tasks 8–12 via `service.SessionService`.

- Produces:

    - `searchContentIn` gains a `Mode` doc update
      (`substring (default), regex, semantic, or hybrid`) and a context field:

        ```go
        Context int `json:"context,omitempty" jsonschema:"Messages of context before/after each match (max 10)."`
        ```

        `contentMatch` gains `Score *float64` and
        `ContextBefore/ContextAfter []contextMessage` where `contextMessage` is
        `{Ordinal int; Role, Content string}` with content truncated to 500 chars.

    - `getMessagesIn` gains `Around *int`, `Before *int`, `After *int` with
      jsonschema docs matching the spec: `around` mutually exclusive with BOTH
      `from` and `direction` (tool error
      `"around is mutually exclusive with from/direction"`), `before`/`after`
      require `around` and replace `limit` on that path, `next_from` = last
      returned ordinal + 1 on the around path.

    - MCP filtering contract is preserved on the around path: an empty `Roles`
      means the MCP DEFAULT (user and assistant only) — translate it to
      `[]string{"user", "assistant"}` before passing
      `service.MessageFilter.Roles` (an empty service-level Roles means "all
      roles", which would leak tool messages). System and system-prefixed
      messages remain always excluded: post-filter the window's messages through
      the existing `isSystemMessage` check, suppressing any message (including
      the always-included anchor) that violates the role/system contract, and
      count suppressed messages in `Filtered` (do NOT hardcode Filtered to 0).
      The linear path keeps its existing post-fetch behavior unchanged.

    - `search_content` tool errors:
      `errors.Is(err, service.ErrSemanticUnavailable)` → tool error with the
      sentence from `db.ErrSemanticUnavailable` (remediation text identical to
      CLI/HTTP).

- [ ] **Step 1: Write failing MCP tests** in the package's existing test style
  (fake service): semantic mode passes Mode through and maps the unavailable
  error to a tool error containing "embeddings build"; context threading;
  get_messages around window (validation matrix incl. around+direction
  rejection; next_from = last+1; empty roles translated to user/assistant; a
  tool-role or system anchor is suppressed and counted in Filtered).

- [ ] **Step 2: Run** `CGO_ENABLED=1 go test -tags fts5 ./internal/mcp/ -v` —
  FAIL, then implement, then PASS.

- [ ] **Step 3: Commit** `feat(mcp): semantic search mode and around windows`

______________________________________________________________________

### Task 14: Embeddings manager + HTTP endpoints

**Files:**

- Create: `internal/vector/manager.go`,
  `internal/server/huma_routes_embeddings.go`
- Modify: `internal/server/server.go` (route registration + server option)
- Test: `internal/vector/manager_test.go`,
  `internal/server/huma_routes_embeddings_test.go`

**Interfaces:**

- Produces (in `internal/vector`):

```go
// Manager serializes builds over one Index (the single writer).
type Manager struct { /* ix *Index, src MessageSource, enc kitvec.EncodeFunc,
                         gen kitvec.Generation, batchSize int,
                         mu sync.Mutex, running bool, status BuildStatus */ }
func NewManager(ix *Index, src MessageSource, enc kitvec.EncodeFunc,
	gen kitvec.Generation, batchSize int) *Manager
type BuildRequest struct {
	FullRebuild bool `json:"full_rebuild,omitempty"`
	Backstop    bool `json:"backstop,omitempty"`
}
type BuildStatus struct {
	Running    bool   `json:"running"`
	Phase      string `json:"phase,omitempty"`
	Done       int64  `json:"done"`
	Total      int64  `json:"total"`
	LastError  string `json:"last_error,omitempty"`
	LastResult *BuildResult `json:"last_result,omitempty"`
}
var ErrBuildRunning = errors.New("an embeddings build is already running")
// StartBuild launches Build in a goroutine; ErrBuildRunning when one is live.
func (m *Manager) StartBuild(req BuildRequest) error
// TryBuild runs synchronously for the scheduler; returns (false, nil)
// when a build is already running (drop, not queue).
func (m *Manager) TryBuild(ctx context.Context, req BuildRequest) (bool, error)
func (m *Manager) Status() BuildStatus
func (m *Manager) Generations(ctx context.Context) ([]GenerationInfo, error)
func (m *Manager) Activate(ctx context.Context, id int64, force bool) error
func (m *Manager) Retire(ctx context.Context, id int64, force bool) error
```

`Activate` refuses (without force) when the target's Missing > 0
(`"generation %d still has %d messages needing embedding; use --force"`), and on
success retires the previously active generation. `Retire` refuses the active
generation without force. Both refuse while a build is running.

- Produces (in `internal/server`): a `ServerOption`-style hook matching how the
  server currently receives optional dependencies (inspect
  `internal/server/server.go` — follow its existing options/config pattern) to
  accept an `EmbeddingsManager` interface (declared in the server package with
  exactly the five methods above, so `*vector.Manager` satisfies it). Routes,
  registered only when the manager is non-nil:

    - `POST /api/v1/embeddings/build` body `BuildRequest` → 202 `{started: true}`;
      `ErrBuildRunning` → 409.
    - `GET  /api/v1/embeddings/status` → `BuildStatus`.
    - `GET  /api/v1/embeddings/generations` → `{generations: []GenerationInfo}`.
    - `POST /api/v1/embeddings/generations/{id}/activate` body `{force: bool}` →
      204; refusal → 409 with the message.
    - `POST /api/v1/embeddings/generations/{id}/retire` same shape. When the
      manager is nil the paths simply don't exist (404), matching how other
      optional route groups register.

- [ ] **Step 1: Write failing manager tests**: StartBuild sets Running and a
  concurrent StartBuild returns ErrBuildRunning; TryBuild returns false while
  running; status transitions to a LastResult on completion and LastError on
  encoder failure; Activate force/refusal matrix; Retire refusal on active.
  Use the fake source/encoder from Task 6 tests.

- [ ] **Step 2: Run — FAIL. Implement manager. Run — PASS.**

- [ ] **Step 3: Write failing route tests** following the package's existing
  huma handler test style with a fake manager; assert status codes (202, 409,
  204\) and JSON shapes.

- [ ] **Step 4: Run — FAIL. Implement routes + registration. Run — PASS.**

- [ ] **Step 5: Run** `make test-short`. **Commit**
  `feat(server): embeddings build lifecycle endpoints`

______________________________________________________________________

### Task 15: `embeddings` CLI command group

**Files:**

- Create: `cmd/agentsview/embeddings.go`
- Modify: `cmd/agentsview/cli.go` (register command),
  `cmd/agentsview/write_lock.go` (generalize the flock helper to take a
  filename)
- Test: `cmd/agentsview/embeddings_test.go`

**Interfaces:**

- Consumes: `config.VectorConfig`, `vector.Open/NewEncoder/NewManager`,
  `IsLocalDaemonActive` + `FindDaemonRuntime` (daemon discovery,
  `cmd/agentsview/daemon_runtime.go`), the Task 14 HTTP endpoints.
- Produces: `agentsview embeddings build|list|activate|retire`.

Command behavior:

- All subcommands first load config; `!cfg.Vector.Enabled` → error
  `"vector search is not enabled: set [vector] enabled = true in config.toml"`.
- **Daemon path:** when `IsLocalDaemonActive(cfg.DataDir)`, talk HTTP to the
  daemon (`FindDaemonRuntime` gives base URL + token): `build` POSTs
  `/api/v1/embeddings/build` then polls `/status` every 2s printing the
  progress line until `Running == false`, exiting non-zero on `LastError`;
  `list`/`activate`/`retire` call their endpoints. 409 on build → print "a
  build is already running (daemon)" and poll instead of failing.
- **Direct path:** no daemon → acquire `vectors.write.lock` via the generalized
  flock helper:

```go
// write_lock.go: extract the existing body into
func tryAcquireNamedLock(dataDir, filename string) (*writeOwnerLock, error)
// and keep tryAcquireWriteOwnerLock(dataDir) = tryAcquireNamedLock(dataDir, writeOwnerLockFile)
const vectorsWriteLockFile = "vectors.write.lock"
```

then open the Index rw, build a Manager, and run `TryBuild` synchronously with a
progress printer (`progress: %d/%d chunks (%.1f%%)`, ~2s throttle, final
`Embedded %d documents (%d chunks), skipped %d, stale %d` from FillStats, then
`Generation activated.` when `Activated`).

- `build` flags: `--full-rebuild`, `--backstop`, `--yes` (confirmation prompt
  before full-rebuild only:
  `"This re-embeds all N messages. Continue? [y/N]"`; `--yes` skips).

- `list` prints a tabwriter table:
  `ID  STATE  MODEL  DIM  EMBEDDED  MISSING FINGERPRINT` (fingerprint
  truncated to 12 chars). Honors `--format json`.

- `activate <id>` / `retire <id>`: parse int64 arg, `--force` flag, print the
  refusal message verbatim on 409/refusal.

- Encoder is built from config via
  `vector.NewEncoder(vector.EncoderConfig{...})` mapping
  `cfg.Vector.Embeddings` (parse Timeout duration; APIKey from
  `cfg.Vector.Embeddings.APIKey()`); the `kitvec.Generation` is
  `{Model: cfg.Model, Dimensions: cfg.Dimension, Params: map[string]string{"max_input_chars": strconv.Itoa(cfg.MaxInputChars)}}`
  — define this mapping ONCE as
  `func vectorGeneration(c config. VectorEmbeddingsConfig) kitvec.Generation`
  in `embeddings.go` and reuse it from serve wiring (Task 16 imports it).

- [ ] **Step 1: Write failing CLI tests**: disabled config errors; `list`
  against a prepared temp Index renders the table; `build` direct path with a
  fake encoder (config endpoint pointed at an httptest OpenAI stub) embeds a
  seeded temp archive end-to-end and prints the final summary; second
  concurrent direct build fails on the flock (acquire the lock in the test,
  expect the "another process" error).

- [ ] **Step 2: Run — FAIL. Implement. Run — PASS.**

- [ ] **Step 3: Run** `make test-short && go vet ./...`.

- [ ] **Step 4: Commit**
  `feat(cli): embeddings build/list/activate/retire commands`

**Checkpoint: invoke the `roborev-fix` skill now (after Task 15) and resolve all
findings before Task 16.**

______________________________________________________________________

### Task 16: Serve wiring + after-sync scheduler

**Files:**

- Create: `cmd/agentsview/embed_scheduler.go`
- Modify: `cmd/agentsview/main.go` (serve path)
- Test: `cmd/agentsview/embed_scheduler_test.go`

**Interfaces:**

- Consumes: `vector.Manager.TryBuild` (drop-not-queue), the sync engine's
  `Emitter` (see `internal/sync/engine.go:43` — serve already wires one for
  SSE; wrap it).
- Produces:

```go
// embedScheduler debounces sync-completion signals into background builds.
type embedScheduler struct { /* mgr *vector.Manager, debounce time.Duration,
                                backstop time.Duration, dirty chan struct{},
                                stop chan struct{} */ }
func newEmbedScheduler(mgr *vector.Manager, debounce, backstop time.Duration) *embedScheduler
func (s *embedScheduler) Notify()               // non-blocking (select+default)
func (s *embedScheduler) Run(ctx context.Context) // goroutine body
func (s *embedScheduler) Stop()
```

`Run` loop: a debounce timer armed by `Notify` (reset on each signal, fire after
30s of quiet → `TryBuild(ctx, BuildRequest{})`; if TryBuild returned false
because a build was running, re-arm the timer — the dirty pass), plus a backstop
ticker (`BackstopInterval`; disabled when \<= 0) firing
`TryBuild(ctx, BuildRequest{Backstop: true})`. `Notify` must never block:
`dirty` has capacity 1, `select { case s.dirty <- struct{}{}: default: }`.

Serve wiring in `main.go` (only when `cfg.Vector.Enabled` and the archive is
writable): open the Index rw (path `cfg.Vector.ResolvedDBPath(dataDir)`), build
encoder + Manager (reuse `vectorGeneration` from Task 15), then:

1. `db.SetVectorSearcher(searcherAdapter{ix, enc})` — a small adapter in
   `embed_scheduler.go` implementing `db.VectorSearcher` by delegating to
   `ix.Search(ctx, enc, query, limit)` and translating
   `ErrNoActiveGeneration`/`*BuildingError` into wrapped
   `db.ErrSemanticUnavailable` errors whose text carries the cause ("index is
   building: 62% complete" / the run-build remediation), so search callers get
   the spec's error taxonomy. The adapter also checks
   `ix.StaleActive(ctx, vectorGeneration(cfg).Fingerprint())` before querying
   and, when stale, wraps `db.ErrSemanticUnavailable` with "index is stale
   (embedding config changed): run 'agentsview embeddings build --full-rebuild'"
   — the search still runs against the old active generation only if
   StaleActive is false; stale is a hard error per the spec.
1. Pass the Manager into the server option from Task 14.
1. Wrap the existing emitter: `emitter = teeEmitter{existing, scheduler}` where
   `teeEmitter.Emit(scope)` calls both, the scheduler side gated on
   `cfg.Vector.Embed.RunAfterSyncEnabled()`.
1. Start `scheduler.Run` in the serve errgroup/goroutine set; stop it on
   shutdown; `ix.Close()` on shutdown. Also install a **read-only** searcher
   in the non-daemon direct-CLI path (`newService` / direct backend
   construction): when `cfg.Vector.Enabled` and `vectors.db` exists, open the
   Index read-only and `SetVectorSearcher` so `session search --semantic`
   works without a daemon; a missing/empty vectors.db yields the unavailable
   error naturally.

- [ ] **Step 1: Write failing scheduler tests** (fake manager recording TryBuild
  calls, short debounce like 20ms): burst of Notify → exactly one build after
  quiet; Notify during a running build → one follow-up pass; backstop tick
  issues a Backstop build; Stop terminates Run; Notify never blocks (call it
  100x with no reader).
- [ ] **Step 2: Run — FAIL. Implement scheduler + adapter. Run — PASS.**
- [ ] **Step 3: Wire serve + direct paths in main.go.** Manual check:
  `make build` then run `agentsview serve` with a test config (vector
  disabled) — boots clean with no vector log lines; with vector enabled but no
  Ollama — boots clean, `session search --semantic x` returns the unavailable
  error, daemon stays healthy.
- [ ] **Step 4: Run** `make test-short && go vet ./...`.
- [ ] **Step 5: Commit**
  `feat(serve): wire vector index, searcher, and after-sync embed scheduler`

______________________________________________________________________

### Task 17: Docs + full verification gate

**Files:**

- Create: user docs page for semantic search (place it where the docs tree keeps
  usage pages — `ls docs/` and mirror the existing usage/search page's
  location and front-matter; link it from any docs nav index the tree uses)
- Modify: `CLAUDE.md` project-structure table (one row for `internal/vector/`)
- Test: none (docs) — but the full gates below

Docs content: enabling `[vector]` (full TOML example from the spec, Ollama
quickstart with `nomic-embed-text`), `embeddings build/list/activate/retire`,
`session search --semantic/--hybrid/--context`,
`session messages --around --role`, cursor-follow workflow (search → ordinal →
window), error taxonomy table, the documented limitations (metadata filters
post-filter the vector leg; legacy no-uuid rows re-embed on ordinal shifts;
SQLite-only with PG/DuckDB returning 501). Format with `mdformat --wrap 80` if
available.

- [ ] **Step 1: Write the docs page + CLAUDE.md row.**
- [ ] **Step 2: Whole-branch gate (SDD finish gate — mandatory):**

Run: `make lint && make test` Expected: clean. Fix everything (NilAway included)
before proceeding.

- [ ] **Step 3: End-to-end smoke** (manual, no assertions to write): with a
  local Ollama running `nomic-embed-text` OR the httptest stub binary approach
  from Task 15's test if no Ollama is available, on a scratch
  `AGENTSVIEW_DATA_DIR`: `agentsview sync` a fixture session (see
  `cmd/testfixture`), `agentsview embeddings build --yes`,
  `agentsview session search --semantic "<query>" --context 2`,
  `agentsview session messages <id> --around <ordinal> --role user,assistant`.
  Paste the commands + output into the task report.
- [ ] **Step 4: Commit** `docs: semantic search usage and architecture notes`

**Checkpoint: invoke the `roborev-fix` skill now (after Task 17), resolve all
findings, and re-run `make lint && make test` after any fixes.**
