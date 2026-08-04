# Unified Bun Storage Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the independent SQLite, PostgreSQL, and DuckDB store paths
with one canonical Bun schema and one shared Bun-backed `db.Store` while
preserving each engine's existing role and lifecycle.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-02-unified-bun-storage-design.md`

**Architecture:** `internal/db` owns a shared `BunStore` and canonical row-model
registry. Thin SQLite, PostgreSQL, and DuckDB adapters provide guarded Bun
handles, lifecycle behavior, and narrowly scoped FTS/vector capabilities; DuckDB
uses a dedicated Bun dialect and Quack connection resolver.

**Tech Stack:** Go 1.26, Uptrace Bun v1.2.18, Bun SQLite/PostgreSQL dialects,
duckdb-go v2, pgx v5, SQLite/FTS5, PostgreSQL/pgvector, DuckDB/Quack, testify,
Kata.

## Kata Tracking

| Plan task           | Kata issue |
| ------------------- | ---------- |
| DuckDB dialect      | `8q6m`     |
| Canonical models    | `r758`     |
| Guarded adapters    | `044k`     |
| Schema convergence  | `s0cx`     |
| Core reads          | `5g5j`     |
| Data and curation   | `35m1`     |
| Pricing and usage   | `pjbd`     |
| Analytics           | `97q3`     |
| Search capabilities | `bhr4`     |
| Replication writes  | `63rc`     |
| Legacy cutover      | `k2vq`     |
| Full verification   | `trgg`     |

All are children of epic `fk9t` and carry the dependency order in this plan.
Claim and close each issue as its evidence lands; close the epic only after
`trgg` passes.

## Global Constraints

- SQLite remains the persistent writable archive and must never be deleted,
  dropped, truncated, or recreated for this refactor.
- PostgreSQL must migrate existing installations transactionally in place.
- DuckDB remains a disposable read mirror and must rebuild at a bumped schema
  version; it never receives an in-place migration.
- Existing backend roles, CLI configuration, and transport behavior remain
  unchanged.
- Common application queries, schema operations, transactions, and row scans go
  through Bun. Direct `database/sql` use is limited to driver setup, pool
  configuration, handle draining/swapping, and connector transport.
- Backend-specific SQL is limited to operational metadata plus small FTS/vector
  capabilities.
- Canonical non-search fragments use the shared SQL subset. Timestamp bucketing,
  percentiles, regex normalization, and JSON interpretation move to Go
  reducers when engine semantics differ; common methods do not branch on
  backend name.
- Canonical identity serving uses `source_archives` and the source-scoped
  identity/snapshot/worktree tables on every backend. SQLite's current
  non-source tables are migration inputs, not a permanent second path.
- Do not add a runtime feature flag, dual read/write path, legacy fallback, or
  compatibility wrapper for the superseded stores.
- Existing shipped migrations are immutable. Add one forward convergence
  migration and amend it in place for all PR-local corrections.
- Follow strict TDD: each production behavior starts with a focused failing test
  whose expected value is hand-derived and observable.
- Build Go code with `CGO_ENABLED=1` and the `fts5` tag where applicable.
- Run `go fmt ./...` and `go vet ./...` before every code commit.
- Do not test Bun, database drivers, deleted symbols, or source-code strings;
  test AgentsView's boundary contracts against real temporary databases.
- Tasks 5-10 are Kata umbrellas, not single review diffs. Commit each completed
  Store method family and its cross-backend proof separately, then commit
  legacy deletion only after the replacement contract is green. The final
  commit step in each task is the cleanup checkpoint, not permission to batch
  the whole task.
- Store contracts record Bun query-hook counts for list, usage, analytics, and
  search hot paths. Counts must remain fixed as fixture cardinality grows;
  tests also cap the number of hydrated rows to the requested page/window
  before Go reducers run.

______________________________________________________________________

### Task 1: Add Bun and implement the DuckDB dialect

**Files:**

- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/duckdb/bundialect/dialect.go`
- Create: `internal/duckdb/bundialect/types.go`
- Create: `internal/duckdb/bundialect/dialect_test.go`
- Create: `internal/duckdb/bundialect/execution_test.go`

**Interfaces:**

- Consumes: `github.com/duckdb/duckdb-go/v2`'s registered `database/sql` driver
  and Bun's `schema.Dialect` interface.

- Produces: `func New() *Dialect`, where `*Dialect` implements `schema.Dialect`,
  reports a non-SQLite custom dialect name, and advertises only DuckDB
  features verified by execution tests.

- Produces: Bun dependencies pinned together at `v1.2.18`:
  `github.com/uptrace/bun`, `github.com/uptrace/bun/dialect/pgdialect`, and
  `github.com/uptrace/bun/dialect/sqlitedialect`.

- [ ] **Step 1: Add aligned Bun dependencies without production code**

    ```bash
    go get github.com/uptrace/bun@v1.2.18 \
      github.com/uptrace/bun/dialect/pgdialect@v1.2.18 \
      github.com/uptrace/bun/dialect/sqlitedialect@v1.2.18
    ```

    Expected: `go.mod`/`go.sum` resolve all Bun packages at v1.2.18; no Go
    production file changes yet.

- [ ] **Step 2: Write failing DuckDB dialect contract tests**

    Add table-driven assertions for owned literal/type behavior and one real
    execution test. The execution test must create, insert, conflict-update, and
    select a representative model through Bun:

    ```go
    type dialectFixture struct {
        bun.BaseModel `bun:"table:dialect_fixtures"`
        ID            int64     `bun:",pk"`
        Name          string    `bun:",notnull"`
        Enabled       bool      `bun:",notnull"`
        Payload       []byte
        Metadata      json.RawMessage `bun:",type:JSON"`
        OccurredAt    time.Time `bun:",notnull"`
    }

    func TestDialectExecutesCanonicalBunQueries(t *testing.T) {
        raw, err := sql.Open("duckdb", "")
        require.NoError(t, err)
        t.Cleanup(func() { require.NoError(t, raw.Close()) })

        store := bun.NewDB(raw, New())
        _, err = store.NewCreateTable().Model((*dialectFixture)(nil)).IfNotExists().Exec(t.Context())
        require.NoError(t, err)

        want := dialectFixture{
            ID: 7, Name: "O'Reilly", Enabled: true,
            Payload: []byte{0x00, 0x7f, 0xff},
            Metadata: json.RawMessage(`{"source":"duckdb"}`),
            OccurredAt: time.Date(2026, 8, 2, 12, 30, 0, 123000000, time.UTC),
        }
        _, err = store.NewInsert().Model(&want).Exec(t.Context())
        require.NoError(t, err)
        _, err = store.NewInsert().Model(&dialectFixture{ID: 7, Name: "updated"}).
            On("CONFLICT (id) DO UPDATE").Set("name = EXCLUDED.name").Exec(t.Context())
        require.NoError(t, err)

        var got dialectFixture
        err = store.NewSelect().Model(&got).Where("id = ?", 7).Scan(t.Context())
        require.NoError(t, err)
        assert.Equal(t, "updated", got.Name)
        assert.True(t, got.Enabled)
        assert.Equal(t, []byte{0x00, 0x7f, 0xff}, got.Payload)
        assert.JSONEq(t, `{"source":"duckdb"}`, string(got.Metadata))
        assert.Equal(t, want.OccurredAt, got.OccurredAt)
    }
    ```

    Add a compile-time assertion:

    ```go
    var _ schema.Dialect = (*Dialect)(nil)
    ```

    Add one real execution subtest for every advertised feature:

    - `cte_with_values` selects literal row `(8, "cte")` from a Bun CTE backed by
      `NewValues`;
    - `insert_returning` inserts ID 9 and scans the returned ID;
    - `delete_returning` deletes ID 9 and scans the returned name;
    - `update_from` updates a fixture from a second source table;
    - `table_and_index_if_not_exists` creates the same table and index twice;
    - `select_exists` returns true for ID 7 and false for ID 404;
    - `composite_in` matches literal tuple `(7, "updated")`;
    - `transaction_rollback` inserts ID 10 inside `RunInTx`, returns a sentinel
      error, and then proves ID 10 is absent.

    `returning` is covered by both returning subtests and `insert_on_conflict` by
    the representative model test. Each SQL-feature subtest must also assert
    that `Features` contains the flag whose syntax it exercises;
    `transaction_rollback` is a lifecycle contract and has no feature flag.

    Generate a canonical table with dynamic timestamp defaults, create it in a
    real temporary DuckDB database, and attach it through the Quack-compatible
    path. Assert the resulting catalog has no dynamic default and that an
    explicit-timestamp insert preserves the literal value.

- [ ] **Step 3: Run the focused tests and verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test ./internal/duckdb/bundialect -run 'TestDialect' -count=1
    ```

    Expected: compilation fails because `bundialect.New` and `Dialect` do not
    exist, not because a module is missing.

- [ ] **Step 4: Add the minimal dialect implementation**

    Implement `Dialect` with `schema.BaseDialect`, its own `schema.Tables`, DuckDB
    type mapping, quoted identifiers, no implicit sequence generation, and this
    conservative feature set:

    ```go
    const features = feature.CTE |
        feature.WithValues |
        feature.Returning |
        feature.InsertReturning |
        feature.DeleteReturning |
        feature.InsertOnConflict |
        feature.UpdateFromTable |
        feature.TableNotExists |
        feature.CreateIndexIfNotExists |
        feature.SelectExists |
        feature.CompositeIn
    ```

    Use a private non-built-in `dialect.Name` value, return `"main"` from
    `DefaultSchema`, map text/JSON/blob/bool/timestamp/integer types explicitly,
    and leave `AppendSequence` unchanged because mirror IDs are source-assigned.
    Normalize generated DuckDB table metadata by dropping dynamic timestamp
    defaults that Quack cannot attach; DuckDB writers provide those values
    explicitly. Remove any advertised flag whose representative execution fails.

- [ ] **Step 5: Verify GREEN and tidy dependencies**

    Run:

    ```bash
    go mod tidy
    CGO_ENABLED=1 go test ./internal/duckdb/bundialect -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: dialect tests pass and `go.mod` contains aligned Bun v1.2.18
    modules.

- [ ] **Step 6: Commit the DuckDB dialect**

    ```bash
    git add go.mod go.sum internal/duckdb/bundialect
    git commit -m "feat(storage): add Bun DuckDB dialect"
    ```

______________________________________________________________________

### Task 2: Define canonical Bun row models and schema registry

**Files:**

- Create: `internal/db/bunmodel/models.go`
- Create: `internal/db/bunmodel/session.go`
- Create: `internal/db/bunmodel/message.go`
- Create: `internal/db/bunmodel/usage.go`
- Create: `internal/db/bunmodel/curation.go`
- Create: `internal/db/bunmodel/identity.go`
- Create: `internal/db/bunmodel/registry.go`
- Create: `internal/db/bunmodel/registry_test.go`
- Create: `internal/db/bun_rows.go`
- Create: `internal/db/bun_rows_test.go`
- Modify: `internal/db/sessions.go`

**Interfaces:**

- Consumes: the current physical columns declared in `internal/db/db.go`,
  `internal/postgres/schema.go`, and `internal/duckdb/schema.go`.

- Produces: exported persistence structs in package `bunmodel` for the common
  tables: `Session`, `Message`, `UsageEvent`, `CursorUsageEvent`, `ToolCall`,
  `ToolResultEvent`, `SecretFinding`, `ModelPricing`, `ModelPricingBand`,
  `StarredSession`, `PinnedMessage`, `ExcludedSession`, `SessionAlias`,
  `Insight`, `SourceArchive`, `SourceProjectIdentityObservation`,
  `SourceSessionProjectIdentitySnapshot`, and `SourceWorktreeProjectMapping`.

- Represents `ModelPricing.updated_at` and `ModelPricingBand.updated_at` with
  the canonical timestamp scanner/valuer. Arbitrary pricing refresh state is
  not a common model row.

- Uses the source-scoped identity tables as the only canonical serving models.
  Existing SQLite non-source identity/mapping structs remain domain or
  migration inputs until Task 4 backfills them; they are not registered common
  tables.

- Produces: `func CommonTables() []Table`, where `Table` contains `Model any`
  and the canonical ordinary index definitions for that model.

- Produces: `func ModelColumns(model any) []string`, which returns the Bun
  column names registered for one canonical model in sorted order.

- Produces lossless converters such as
  `func sessionToBunRow(Session) (bunmodel.Session, error)` and
  `func sessionFromBunRow(bunmodel.Session) Session` in package `db`.
  `db.Session` gains internal `SourceArchiveID` and `SourceDatabaseGeneration`
  fields, and `ArchiveIdentity` carries the stable archive ID/database ID that
  the SQLite adapter stamps before parser writes.

- [ ] **Step 1: Write failing canonical-registry tests**

    Assert hand-written expected table names and critical logical columns without
    mirroring the registry's implementation:

    ```go
    func TestCommonTablesContainServingSchema(t *testing.T) {
        sessionColumns := ModelColumns((*Session)(nil))
        assert.Subset(t, sessionColumns, []string{
            "agent", "created_at", "deleted_at", "ended_at", "id", "machine",
            "message_count", "project", "started_at", "transcript_revision",
            "source_archive_id", "source_database_generation",
        })
        assert.Subset(t, servingTableNames(CommonTables()), []string{
            "cursor_usage_events", "excluded_sessions", "insights", "messages",
            "model_pricing", "model_pricing_bands", "pinned_messages",
            "source_project_identity_observations", "secret_findings", "sessions",
            "session_aliases", "starred_sessions", "tool_calls",
            "source_session_project_identity_snapshots",
            "source_worktree_project_mappings", "tool_result_events",
            "usage_events",
        })
    }
    ```

    Define `servingTableNames` in the test as a direct projection/sort helper; it
    does not call production filtering logic.

    Add literal round-trip cases for nullable timestamps, SQLite integer booleans,
    native booleans, JSON, and optional IDs. Name the production break each case
    catches in the test name. Unsupported non-empty timestamps must return a
    field-qualified error; they must never become `NULL` or a zero timestamp.

    Generate `messages` DDL for every dialect and assert the canonical composite
    logical key. Separately characterize the prior SQLite shape as the accepted
    `id INTEGER PRIMARY KEY` plus unique composite alias; do not require or
    perform a table rebuild.

- [ ] **Step 2: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db/bunmodel ./internal/db \
      -run 'Test(CommonTables|BunRow)' -count=1
    ```

    Expected: compilation fails because the model registry and converters do not
    exist.

- [ ] **Step 3: Implement the row models and registry**

    Keep Bun structs persistence-only: use `bun.BaseModel`, explicit table names,
    explicit column tags, `pk`, `notnull`, `nullzero`, and portable logical SQL
    types. Do not place parser, API, cursor, or display behavior in this
    package. Represent timestamps with a shared scanner/valuer that accepts
    existing SQLite text and PostgreSQL/DuckDB native time values and always
    emits UTC.

    Pricing rows and pricing bands always use that timestamp type. Keep pricing
    refresh versions and attempt markers out of the common registry; Task 4
    gives SQLite a dedicated operational metadata table for them.

    Keep engine-operational tables out of `CommonTables`; list them only in the
    schema extension packages that own them.

    Register canonical foreign keys, cascade intent, conditional dedup indexes,
    and ordinal-based tool/pin relationships. Cursor-usage and secret-finding
    IDs are optional source data because DuckDB writers do not allocate archive
    sequences. Use portable `CASE` expression indexes to permit repeated empty
    dedup keys and reject repeated non-empty keys on all three engines.

- [ ] **Step 4: Verify generated schema independently of current physical
  schemas**

    In `bunmodel_test`, create every registered table in temporary SQLite and
    DuckDB databases and render every `NewCreateTable` query with
    `pgdialect.New` without executing it. Assert each model's hand-listed
    critical columns occur in all three generated forms. Do not compare the
    registry with the existing DuckDB/PostgreSQL schema yet; Task 4 owns
    physical convergence. Execute every registered ordinary index, exercise
    non-empty dedup rejection, and prove session/message cascades on SQLite.
    DuckDB omits foreign-key DDL on mutable mirror tables and its atomic writer
    proves the registered relationships through explicit child-first deletion and
    whole-session replacement. SQLite and rendered PostgreSQL DDL retain the
    canonical foreign keys.

- [ ] **Step 5: Verify GREEN**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db/bunmodel ./internal/db \
      ./internal/duckdb/bundialect -run 'Test(CommonTables|BunRow|GeneratedSchema)' -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: registry/converter tests pass, SQLite/DuckDB execute generated DDL,
    and PostgreSQL DDL renders without requiring a live server.

- [ ] **Step 6: Commit the canonical model registry**

    ```bash
    git add internal/db/bunmodel internal/db/bun_rows.go \
      internal/db/bun_rows_test.go
    git commit -m "refactor(storage): define canonical Bun models"
    ```

______________________________________________________________________

### Task 3: Introduce guarded Bun backend adapters

**Files:**

- Create: `internal/db/bun_backend.go`
- Create: `internal/db/bun_store.go`
- Create: `internal/db/bun_backend_test.go`
- Modify: `internal/db/db.go`
- Modify: `internal/db/db_test.go`
- Modify: `internal/postgres/connect.go`
- Modify: `internal/postgres/sessions.go`
- Modify: `internal/postgres/store.go`
- Modify: `internal/postgres/store_unit_test.go`
- Modify: `internal/duckdb/store.go`
- Modify: `internal/duckdb/connect.go`
- Modify: `internal/duckdb/store_reopen_test.go`
- Create: `internal/duckdb/quack_bun.go`
- Create: `internal/duckdb/quack_bun_test.go`
- Create: `internal/duckdb/quack_bun_duckdbtest_test.go`

**Interfaces:**

- Consumes: `bun.IDB`, the upstream SQLite/PostgreSQL dialects, and
  `bundialect.New()`.

- Produces:

    ```go
    type BunBackend interface {
        Name() string
        ReadOnly() bool
        Capabilities() BackendCapabilities
        View(context.Context, func(bun.IDB) error) error
        ConsistentView(context.Context, func(bun.IDB) error) error
        Update(context.Context, func(bun.IDB) error) error
    }

    type WriteOperation uint8

    const (
        WriteArchive WriteOperation = iota
        WriteCuration
        WriteInsight
        WriteInsightDelete
        WriteSessionManagement
        WriteRecall
    )

    type BackendCapabilities struct {
        Recall           bool
        Writes           map[WriteOperation]bool
        SessionMutations SessionMutationAdapter
    }

    func (c BackendCapabilities) AllowsWrite(op WriteOperation) bool

    type BunStore struct {
        backend      BunBackend
        cursorMu     sync.RWMutex
        cursorSecret []byte
        pricingMu    sync.RWMutex
        pricing      pricingState
    }

    func NewBunStore(backend BunBackend) *BunStore

    func (s *BunStore) view(ctx context.Context, fn func(bun.IDB) error) error
    func (s *BunStore) update(ctx context.Context, op WriteOperation, fn func(bun.IDB) error) error
    ```

    `pricingState` owns custom, effective-catalog, and empty-catalog overrides so
    promoted usage methods never depend on fields in an outer concrete wrapper.
    Task 7 moves the existing setters onto `BunStore` and protects them with
    `pricingMu`.

    During the staged cutover, the concrete cursor/pricing fields remain
    authoritative for legacy consumers and every setter updates both copies.
    SQLite injects its constructor-generated cursor secret into `BunStore`;
    PostgreSQL and DuckDB synchronize through their existing configured-secret
    setters. Task 5 moves every cursor consumer and removes the concrete cursor
    fields/setters in its cleanup commit. Task 7 does the same for pricing
    state. `BunStore` does not silently generate an independent fallback secret.

    Session mutation adapters own engine-specific clocks and side effects: SQLite
    supplies application UTC timestamps, clears restore baselines, and removes
    FTS-backed messages before permanent deletion; PostgreSQL uses database time
    and advances revisions monotonically. Adapter hooks run inside the shared
    transaction, and result counts publish only after it succeeds.

- Produces thin composition: SQLite `DB`, `postgres.Store`, and `duckdb.Store`
  embed `*BunStore` but retain their current concrete public types and
  lifecycle APIs.

- Produces a Quack `bun.ConnResolver` whose `bun.IConn` executes formatted
  DuckDB SELECT SQL through `agentsview_remote.query(?)`.

- [ ] **Step 1: Write failing guarded-handle tests**

    Add a fake backend that records whether a Bun operation ran inside its guard.
    Assert the shared store rejects an unauthorized operation while still
    permitting a narrower operation on a backend whose public `ReadOnly()` value
    is true:

    ```go
    func TestBunStoreUpdateUsesOperationCapability(t *testing.T) {
        backend := &recordingBunBackend{
            readOnly: true,
            capabilities: BackendCapabilities{
                Writes: map[WriteOperation]bool{WriteCuration: true},
            },
        }
        store := NewBunStore(backend)
        err := store.update(t.Context(), WriteArchive, func(bun.IDB) error {
            t.Fatal("unauthorized archive callback ran")
            return nil
        })
        assert.ErrorIs(t, err, ErrReadOnly)

        err = store.update(t.Context(), WriteCuration, func(bun.IDB) error {
            return nil
        })
        require.NoError(t, err)
        assert.Equal(t, 1, backend.updateCalls)
    }
    ```

    Extend SQLite reopen and DuckDB mirror-reopen tests to execute a Bun read
    before and after the handle swap and assert both the new data and the
    absence of `database is closed` errors. Add coordinated cases that block an
    in-flight Bun callback, signal on a channel immediately before
    `Reopen`/mirror adoption attempts to acquire the guarded handle, prove the
    swap cannot signal completion, release the callback, and then read from the
    replacement. Existing raw-pool lifecycle tests continue to cover writer
    close/reopen, connection close, full close, and drain failures.

    Add a Quack resolver unit test with a recording `bun.IConn` seam: a generated
    `SELECT id FROM sessions WHERE project = 'alpha'` must be handed to
    `query(?)` as one SQL argument, not executed against the in-memory catalog.
    Add a `duckdbtest` integration case that executes a Bun-generated quoted
    predicate and scans JSON/timestamp data through a real Quack `query()`
    attachment. After a server restart, run a Bun `Count` or `Exists` operation
    so the `QueryRowContext` path proves it uses the same stale-attachment
    retry. Reuse the existing stale-attachment and error-redaction harness.

- [ ] **Step 2: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/postgres ./internal/duckdb \
      -run 'Test(BunStore|.*Bun.*Reopen|QuackBun)' -count=1
    ```

    Expected: compilation fails because `BunBackend`, `BunStore`, and the Quack
    resolver do not exist.

- [ ] **Step 3: Implement guarded adapters and composition**

    SQLite stores Bun wrappers beside each active reader/writer pool and updates
    them atomically under the existing `connMu` during open/reopen/close. `View`
    and `Update` hold the existing guard through Bun query start/scan; `Update`
    also retains the existing write mutex semantics.

    PostgreSQL wraps its stable pool with `pgdialect.New()` and delegates close to
    the existing raw-pool owner. Its operation capabilities allow curation,
    insight insertion and deletion (after their independent capability probes),
    and session-management writes but reject archive/Recall ingestion. DuckDB
    wraps each mirror generation with `bundialect.New()` and swaps raw/Bun
    handles together under `handleMu`.

    Quack `ConsistentView` reads an opaque mirror-generation token before and
    after a callback and may replay that callback when the token changes.
    Callbacks stage attempt-local results and publish only after the guarded
    view succeeds.

    The Quack resolver returns an `IConn` that formats no values itself: Bun has
    already produced safe DuckDB SQL. `QueryContext` and `QueryRowContext` pass
    that complete SQL through the existing reattachment-aware `query()` path;
    the row path eagerly bridges its single result back to `*sql.Row` because
    Bun's `IConn` requires that concrete return type. `ExecContext` returns
    `db.ErrReadOnly`.

- [ ] **Step 4: Verify GREEN and lifecycle behavior**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/postgres ./internal/duckdb \
      -run 'Test(BunStore|.*Bun.*Reopen|QuackBun|StoreReopen)' -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: guarded reads survive handle swaps, writes preserve read-only
    errors, and Quack receives Bun-generated SQL.

- [ ] **Step 5: Commit the backend runtime**

    ```bash
    git add internal/db/bun_backend.go internal/db/bun_store.go \
      internal/db/bun_backend_test.go internal/db/db.go internal/db/db_test.go \
      internal/postgres/connect.go internal/postgres/sessions.go \
      internal/postgres/store.go \
      internal/postgres/store_unit_test.go internal/duckdb/store.go \
      internal/duckdb/connect.go internal/duckdb/store_reopen_test.go \
      internal/duckdb/quack_bun.go internal/duckdb/quack_bun_test.go \
      internal/duckdb/quack_bun_duckdbtest_test.go
    git commit -m "refactor(storage): add guarded Bun backends"
    ```

______________________________________________________________________

### Task 4: Converge schema creation and forward migrations

**Files:**

- Modify: `docs/superpowers/specs/2026-08-02-unified-bun-storage-design.md`
- Modify: `internal/db/bun_rows.go`
- Create: `internal/db/bun_schema.go`
- Create: `internal/db/bun_schema_test.go`
- Modify: `internal/db/bunmodel/generated_schema_duckdb_test.go`
- Modify: `internal/db/bunmodel/identity.go`
- Modify: `internal/db/bunmodel/models.go`
- Modify: `internal/db/bunmodel/registry.go`
- Modify: `internal/db/bunmodel/registry_test.go`
- Modify: `internal/db/bunmodel/session.go`
- Modify: `internal/db/db.go`
- Modify: `internal/db/db_test.go`
- Modify: `internal/db/schema.sql`
- Modify: `internal/db/sessions.go`
- Modify: `internal/db/session_batch.go`
- Modify: `internal/db/project_identity.go`
- Modify: `internal/db/legacy_schema_test.go`
- Modify: `internal/db/link_subagent_nested_test.go`
- Modify: `internal/db/messages.go`
- Modify: `internal/db/messages_test.go`
- Modify: `internal/db/orphaned.go`
- Modify: `internal/db/pricing.go`
- Modify: `internal/db/pricing_list.go`
- Modify: `internal/db/pricing_test.go`
- Modify: `internal/db/read_only_test.go`
- Modify: `internal/db/recall_eval_ingest.go`
- Modify: `internal/db/recall_import.go`
- Modify: `internal/db/usage.go`
- Modify: `internal/postgres/activityreport_pgtest_test.go`
- Modify: `internal/postgres/pricing.go`
- Modify: `internal/postgres/pricing_unit_test.go`
- Modify: `internal/postgres/push_test.go`
- Modify: `internal/postgres/schema.go`
- Modify: `internal/postgres/schema_test.go`
- Modify: `internal/postgres/schema_pgtest_test.go`
- Modify: `internal/postgres/analytics.go`
- Modify: `internal/postgres/usage_pgtest_test.go`
- Modify: `internal/duckdb/analytics_usage.go`
- Modify: `internal/duckdb/bundialect/types.go`
- Modify: `internal/duckdb/schema.go`
- Modify: `internal/duckdb/schema_test.go`
- Modify: `internal/duckdb/rebuild_test.go`
- Modify: `internal/duckdb/analytics_usage_test.go`
- Modify: `internal/duckdb/messages_test.go`
- Modify: `internal/duckdb/probe.go`
- Modify: `internal/duckdb/project_identity_test.go`
- Modify: `internal/duckdb/project_inventory_test.go`
- Modify: `internal/duckdb/project_rules_test.go`
- Modify: `internal/duckdb/push.go`
- Modify: `internal/duckdb/store_test.go`
- Modify: `internal/duckdb/sync.go`
- Modify: `internal/duckdb/sync_test.go`
- Modify: `internal/duckdb/worktree_mappings_push.go`
- Modify: `internal/duckdb/worktree_mappings_push_test.go`

**Interfaces:**

- Consumes: `bunmodel.CommonTables()` and each adapter's `bun.IDB`.

- Produces:

    ```go
    func CreateCommonSchema(ctx context.Context, db bun.IDB) error
    func CheckCommonSchema(ctx context.Context, db bun.IDB) error
    ```

- Produces one forward SQLite/PostgreSQL schema-convergence migration that is
  additive and transactional.

- Produces DuckDB `SchemaVersion = 11` and rebuild-only canonical schema
  creation.

- [ ] **Step 1: Classify the migration history before editing**

    Refresh remote refs and prove the current schema blocks are shipped:

    ```bash
    git fetch --tags origin
    git log --oneline origin/main -- internal/db/db.go internal/postgres/schema.go
    git log --oneline --tags -- internal/db/db.go internal/postgres/schema.go
    git branch -r --list 'origin/release/*'
    ```

    Treat all existing migrations as immutable. Search the branch diff for any
    already-added migration; if one exists, amend that PR-local migration rather
    than adding another.

- [ ] **Step 2: Write failing fresh/upgrade/rebuild tests**

    Add three observable cases:

    1. A fresh SQLite database created through the normal `Open` path accepts a
       canonical Bun insert and query.
    1. A copy of the prior SQLite schema containing a named existing session opens
       in place, retains that session, and accepts canonical queries.
    1. PostgreSQL's prior schema fixture migrates in one transaction and retains a
       named session under `pgtest`.

    Inspect the catalog produced by SQLite's normal `Open` path and assert the one
    accepted physical relationship matrix. Do not substitute an isolated
    Bun-only constructor or claim that fresh archives receive foreign keys the
    bootstrap path does not create.

    Seed the prior SQLite fixture with an aggregate identity observation, a
    session snapshot, a worktree mapping, a tool call, and a pinned message.
    Assert that convergence backfills the source-scoped tables with the archive
    ID/generation before canonical reads switch, and that subsequent writes
    target only those canonical tables. Inject a failure before the
    compatibility stamp on SQLite and PostgreSQL; assert the new rows, stamp,
    and all intermediate DDL roll back together.

    Open an already-stamped PostgreSQL fixture whose canonical catalog is invalid
    and assert convergence fails after taking the advisory transaction lock. The
    initial stamp probe must not bypass normal validation. Make both pricing
    `updated_at` columns textual and assert read compatibility rejects them, the
    push fast path forces writable convergence, and stamped drift fails closed.

    Reopen the stamped SQLite fixture after deliberately diverging its legacy and
    canonical identity rows. Assert canonical rows are unchanged so the one-time
    inputs cannot overwrite or resurrect post-cutover state. A stamped schema
    that fails validation must fail closed rather than replay or repair.

    Give the legacy session empty provenance and assert migration fills
    `source_archive_id` from `GetArchiveID` and `source_database_generation`
    from `GetDatabaseID`. After migration, write a newly parsed session through
    the normal batch API and assert both required values are stamped before its
    canonical identity joins are queried.

    Resolve the shipped SQLite `tool_calls.message_id` and
    `pinned_messages.message_id` aliases from canonical message ordinals in the
    same write transaction. Retain the seeded dependent rows and prove a new
    ordinal-based tool/pin write satisfies both the canonical relationship and
    the non-null SQLite physical aliases. The PostgreSQL prior-schema fixture
    proves convergence drops the old pin alias's NOT NULL constraint and accepts
    an ordinal-only canonical pin.

    Update the DuckDB rebuild test to expect schema version 11, the full canonical
    common table/column set, preserved source rows, and atomic replacement. A
    fresh schema carries a nonempty opaque mirror-generation token.
    Compatibility checks and probes reject version-11 mirrors with a missing or
    empty token, and a metadata failure rolls back every field without
    publishing a new generation.

- [ ] **Step 3: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test(BunSchema|LegacySchema|Rebuild.*Canonical|SchemaVersion)' -count=1
    make test-postgres
    ```

    Expected: fresh/common schema checks fail because creation still uses the
    independent DDL definitions, DuckDB still reports schema version 9, and the
    PostgreSQL prior-schema migration test fails. Leave the dedicated PostgreSQL
    container running for the GREEN rerun because this agent started it.

- [ ] **Step 4: Implement common schema creation and convergence**

    `CreateCommonSchema` must iterate the registry in foreign-key-safe order, call
    Bun `NewCreateTable().Model(...).IfNotExists()`, and create each ordinary
    canonical index. Extension owners run their operational/FTS/vector DDL only
    after common tables exist.

    Add only the missing columns, constraints, or common tables required to make
    prior SQLite/PostgreSQL schemas match the canonical logical schema. Do not
    rewrite equivalent SQLite text timestamps or integer booleans. Stamp schema
    compatibility only in the same transaction as all convergence statements.
    Retain SQLite's old non-source identity tables as inert migration history;
    do not read, write, drop, or use them as a fallback after the cutover.

    Check the SQLite stamp inside the transaction. Copy legacy identity inputs and
    install the transitional canonical publication triggers only when unstamped;
    stamped archives validate and fail closed without replaying inputs. Task 11
    removes those triggers after all application write paths and journal updates
    are centralized. PostgreSQL rechecks its stamp after acquiring the advisory
    transaction lock.

    Move archive-identity retrieval and session provenance stamping into this
    task. Route every SQLite session insertion path, including batch, pending-
    content, project-identity, and placeholder writes, through the same helper
    before shared reads are enabled. The artifact-import coordinator may stage
    an incomplete row during its recoverable pre-publication phase; governed
    reads exclude it until provenance is populated. Shared schema validation
    must not reject that workflow state. Task 10 later replaces the SQL
    implementation with Bun but does not introduce this invariant.

    Move `_fallback_version`, `_litellm_last_attempt`, and
    `_pricing_storage_version` into a dedicated SQLite `pricing_metadata` table
    in this same forward convergence. Delete only those migrated rows from
    `model_pricing`, preserve other underscore-prefixed pricing patterns, use no
    fallback reads, and preserve the metadata explicitly during archive resync.

    Require native PostgreSQL timestamp types for both pricing `updated_at`
    columns in read compatibility, push fast-path, and stamped-schema
    validation. The shipped unstamped text schema converges under the advisory
    transaction lock; read-only and stamped drift fail closed.

    Replace DuckDB's duplicated common `mirrorTables` declarations with registry
    creation. Keep only `sync_metadata`, provenance, and DuckDB-specific indexes
    in `internal/duckdb/schema.go`.

    Create the initial opaque mirror-generation token with the fresh schema.
    Publish later mirror metadata fields and a fresh token in one DuckDB
    transaction so readers never observe new descriptive metadata under the
    previous generation. Missing or empty generation metadata is an incompatible
    schema for local checks, Quack checks, and mirror probing.

- [ ] **Step 5: Verify GREEN on fresh and local upgrade paths**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test(BunSchema|LegacySchema|Rebuild.*Canonical|SchemaVersion)' -count=1
    go fmt ./...
    go vet ./...
    ```

    Expected: fresh/legacy SQLite data survives, DuckDB rebuilds at version 11,
    and all local schema checks pass.

- [ ] **Step 6: Verify PostgreSQL migration GREEN**

    Run against the dedicated test database:

    ```bash
    make test-postgres
    ```

    Expected: PostgreSQL schema and migration tests pass without dropping the
    existing session fixture. Because this agent started the test container, it
    may run `make postgres-down` after the suite.

- [ ] **Step 7: Commit the convergence migration**

    ```bash
    git add docs/superpowers/specs/2026-08-02-unified-bun-storage-design.md \
      internal/db/bun_rows.go internal/db/bun_schema.go \
      internal/db/bun_schema_test.go \
      internal/db/bunmodel/generated_schema_duckdb_test.go \
      internal/db/bunmodel/identity.go internal/db/bunmodel/models.go \
      internal/db/bunmodel/registry.go internal/db/bunmodel/registry_test.go \
      internal/db/bunmodel/session.go \
      internal/db/db.go internal/db/db_test.go internal/db/legacy_schema_test.go \
      internal/db/link_subagent_nested_test.go internal/db/messages.go \
      internal/db/messages_test.go internal/db/project_identity.go \
      internal/db/orphaned.go internal/db/pricing.go \
      internal/db/pricing_list.go internal/db/pricing_test.go \
      internal/db/read_only_test.go internal/db/recall_eval_ingest.go \
      internal/db/recall_import.go internal/db/schema.sql \
      internal/db/session_batch.go internal/db/sessions.go internal/db/usage.go \
      internal/postgres/activityreport_pgtest_test.go \
      internal/postgres/analytics.go internal/postgres/pricing.go \
      internal/postgres/pricing_unit_test.go internal/postgres/push_test.go \
      internal/postgres/schema.go \
      internal/postgres/schema_pgtest_test.go internal/postgres/schema_test.go \
      internal/postgres/usage_pgtest_test.go internal/duckdb/analytics_usage.go \
      internal/duckdb/analytics_usage_test.go \
      internal/duckdb/bundialect/types.go internal/duckdb/messages_test.go \
      internal/duckdb/probe.go internal/duckdb/project_identity_test.go \
      internal/duckdb/project_inventory_test.go \
      internal/duckdb/project_rules_test.go internal/duckdb/push.go \
      internal/duckdb/rebuild_test.go internal/duckdb/schema.go \
      internal/duckdb/schema_test.go internal/duckdb/store_test.go \
      internal/duckdb/sync.go internal/duckdb/sync_test.go \
      internal/duckdb/worktree_mappings_push.go \
      internal/duckdb/worktree_mappings_push_test.go
    git commit -m "refactor(storage): converge Bun schemas"
    ```

______________________________________________________________________

### Task 5: Move sessions, cursors, messages, and metadata into `BunStore`

**Files:**

- Create: `internal/db/bun_sessions.go`
- Create: `internal/db/bun_messages.go`
- Create: `internal/db/bun_metadata.go`
- Create: `internal/db/bun_sessions_test.go`
- Create: `internal/db/bun_messages_test.go`
- Create: `internal/storetest/contract.go`
- Create: `internal/storetest/fixture.go`
- Create: `internal/db/bun_store_contract_external_test.go`
- Create: `internal/postgres/bun_store_contract_pgtest_test.go`
- Modify: `internal/db/store_contract_test.go`
- Modify: `internal/db/sessions.go`
- Modify: `internal/db/messages.go`
- Modify: `internal/db/stats.go`
- Modify: `internal/db/timing.go`
- Modify: `internal/postgres/sessions.go`
- Modify: `internal/postgres/messages.go`
- Modify: `internal/postgres/session_timing.go`
- Modify: `internal/postgres/store.go`
- Modify: `internal/duckdb/store.go`
- Modify: `internal/duckdb/messages.go`
- Modify: `internal/duckdb/store_contract_test.go`

**Interfaces:**

- Consumes: `BunStore.view`, canonical row converters, existing `SessionFilter`,
  `MessageWindow`, cursor payload, and termination semantics.

- Produces shared implementations for cursor signing plus these `db.Store`
  groups: session list/sidebar/get/children/partial-ID/version, message
  pagination/window/all/model counts/activity, session timing, and
  stats/projects/agents/machines/branches.

- Produces reusable test API:

    ```go
    type Backend struct {
        Name                string
        Open                func(*testing.T) db.Store
        Seed                func(*testing.T, db.Store) Fixture
        SupportsLocalWrites bool
    }

    func RunCoreContract(t *testing.T, backend Backend)
    ```

- [ ] **Step 1: Extract a non-tautological cross-backend fixture**

    Move the reusable literal store-contract expectations into
    `internal/storetest`. Keep seeding backend-specific through callbacks, but
    run identical assertions for session filtering/cursors, message
    ordering/tool hydration, and metadata. Do not derive expectations from Bun
    queries or the canonical converters. Invoke the runner for SQLite from
    `bun_store_contract_external_test.go` using package `db_test`, so the
    helper's import of `internal/db` does not create a cycle; leave
    SQLite-private/local- only cases in the existing package-`db` test.

- [ ] **Step 2: Add a failing proof that all backends use the common methods**

    Register SQLite, PostgreSQL, and DuckDB with `RunCoreContract`; PostgreSQL's
    registration lives in `bun_store_contract_pgtest_test.go` and opens a real
    dedicated test schema. Add a `recordingBackend` case that invokes
    `BunStore.ListSessions` and asserts one guarded Bun view plus the literal
    result. The test should fail to compile until `BunStore` owns
    `ListSessions`.

- [ ] **Step 3: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*CoreContract|TestBunStore(ListSessions|GetMessages)' -count=1
    ```

    Expected: compilation fails or the recording backend sees no common query
    because the concrete stores still own these methods.

- [ ] **Step 4: Implement common session/cursor queries**

    Move cursor secret state to `BunStore`. Build session predicates with Bun
    `WhereGroup`, `bun.List`, and portable UTC values. Use one standard SQL CTE
    for canonical root/sidebar traversal and one keyset paginator. Scan
    `bunmodel.Session` and convert once.

    Preserve these literal contracts: soft-deleted rows are excluded by
    `GetSession`, included by `GetSessionFull`; canonical roots and orphans
    match current sidebar semantics; termination filters use the same UTC
    cutoffs; and page totals/cursors remain stable.

- [ ] **Step 5: Implement common message and metadata queries**

    Query `bunmodel.Message` once, batch-load tool calls and result events in
    at-most-500-ordinal `bun.List` chunks, and hydrate through shared maps. Use
    the adapter's narrow timestamp renderer for SQLite text chronology versus
    native PostgreSQL/DuckDB timestamps; activity and timing reduction remains
    shared Go. Implement stats and dimension lists once from canonical rows.

- [ ] **Step 6: Verify the common methods on every backend before deletion**

    Register the embedded `store.BunStore` explicitly with `RunCoreContract` so
    concrete shadowing methods cannot satisfy this checkpoint. Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*CoreContract|Test(StoreContract|Messages|SessionTiming|StoreReopen)' -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    ```

    Expected: the embedded common store passes identical SQLite, DuckDB, and
    PostgreSQL assertions while the legacy concrete methods still exist. Stop
    the PostgreSQL container if this agent started it.

- [ ] **Step 7: Delete the superseded concrete methods in this group**

    Remove the duplicate PostgreSQL and DuckDB query/scan helpers and the SQLite
    receiver methods that shadow the embedded `BunStore`. Retain local-only sync
    helpers in `sessions.go`/`messages.go`; rename them around their archive
    responsibility when a removed Store method shared their old helper. Remove
    each concrete cursor field and encode/decode implementation only after all
    cursor consumers resolve to `BunStore`; concrete setters then forward
    directly without retaining a second key.

- [ ] **Step 8: Re-run final backend contracts and commit**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    git add internal/db internal/postgres internal/duckdb internal/storetest
    git commit -m "refactor(storage): unify core Bun reads"
    ```

    Expected: concrete wrappers now resolve the common methods and all three
    backend contracts remain green. Stop the container with `make postgres-down`
    if this agent started it.

______________________________________________________________________

### Task 6: Move identity, inventory, curation, insights, and mutations into `BunStore`

**Files:**

- Create: `internal/db/bun_data.go`
- Create: `internal/db/bun_curation.go`
- Create: `internal/db/bun_insights.go`
- Create: `internal/db/bun_mutations.go`
- Create: `internal/db/bun_recall.go`
- Create: `internal/db/bun_data_test.go`
- Create: `internal/db/bun_curation_test.go`
- Create: `internal/db/bun_recall_test.go`
- Create: `internal/storetest/recall_contract.go`
- Modify: `internal/db/bun_store_contract_external_test.go`
- Modify: `internal/postgres/bun_store_contract_pgtest_test.go`
- Modify: `internal/duckdb/bun_store_contract_test.go`
- Modify: `internal/storetest/contract.go`
- Modify: `internal/db/project_identity.go`
- Modify: `internal/db/project_inventory.go`
- Modify: `internal/db/project_rules.go`
- Modify: `internal/db/worktree_candidates.go`
- Modify: `internal/db/starred.go`
- Modify: `internal/db/pins.go`
- Modify: `internal/db/insights.go`
- Modify: `internal/db/recall.go`
- Modify: `internal/db/recall_import.go`
- Modify: `internal/db/recall_query_events.go`
- Modify: `internal/db/recall_eval_ingest.go`
- Modify: `internal/db/sessions.go`
- Modify: `internal/postgres/project_identity.go`
- Modify: `internal/postgres/project_inventory.go`
- Modify: `internal/postgres/project_rules.go`
- Modify: `internal/postgres/worktree_candidates.go`
- Modify: `internal/postgres/curation.go`
- Modify: `internal/postgres/store.go`
- Modify: `internal/duckdb/project_identity.go`
- Modify: `internal/duckdb/project_inventory.go`
- Modify: `internal/duckdb/project_rules.go`
- Modify: `internal/duckdb/worktree_candidates.go`
- Modify: `internal/duckdb/curation.go`
- Modify: `internal/duckdb/stubs.go`

**Interfaces:**

- Consumes: `BunStore.view/update`, canonical identity/curation models, and
  existing `db.ErrReadOnly` policy.

- Produces shared data/inventory/project-rule/worktree-candidate reads, shared
  star/pin operations, shared insight reads/writes, and shared session
  rename/trash/restore/delete mutations.

- Produces `UpsertStarredSessionRows` and `UpsertPinnedMessageRows` canonical
  write helpers used by local mutations and the Task 10 cross-target fixture.

- Produces identity and mapping reads exclusively from the source-scoped
  canonical tables. Task 4 already moved active writes off the retired
  migration-input tables; this task does not introduce another write path.

- Produces the `db.Store` Recall methods once on `BunStore`: the SQLite adapter
  advertises `BackendCapabilities.Recall`, while PostgreSQL and DuckDB
  preserve the current `db.ErrReadOnly` behavior without concrete stub
  methods.

- Produces one operation-scoped write-policy check in `BunStore.update`;
  concrete stores do not carry read-only stubs for common methods.

- [ ] **Step 1: Extend the contract with literal data and curation behavior**

    Seed two source-scoped project identities from distinct archives, one mapping,
    one starred session, one pinned message, and one insight. Assert literal
    inventory rollups, rule selection, candidate ordering, star/pin rows, and
    insight retrieval. For each operation not authorized by a backend, assert
    `ErrorIs(db.ErrReadOnly)` and assert reads still return
    mirrored/synchronized curation. Explicitly assert that PostgreSQL remains
    publicly read-only while star, pin, rename, trash, and available insight
    writes succeed. Run Recall contracts for writable SQLite, read-only SQLite,
    and unsupported PostgreSQL/DuckDB: read-only SQLite retains canonical reads
    and non-mutating import dry-runs while every Recall mutation is rejected
    before SQL.

- [ ] **Step 2: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*(Data|Curation)Contract|TestBunStore(ReadOnly|Mutations)' -count=1
    ```

    Expected: common-store method compilation or recording assertions fail while
    concrete implementations/stubs still own the behavior.

- [ ] **Step 3: Implement shared identity and inventory reads**

    Port identity observations/snapshots, inventory aggregation, project rules,
    and worktree candidates to canonical source-scoped Bun rows. Keep path
    normalization and identity merge algorithms in their existing pure helpers;
    only database access moves. Opaque project selectors use the complete
    canonical `source_archives` aggregate scope on every backend; SQLite does not
    retain a local-only selector-key path. Unresolved or ambiguous response-
    scoped keys may change whenever archive membership changes through addition,
    retirement, or replacement; resolved repository identity keys remain stable.
    Do not add a compatibility lookup for prior response-scoped keys. Task 11
    owns the final application-write centralization and removal of trigger-based
    publication bookkeeping.

- [ ] **Step 4: Implement shared curation, insight, and session mutations**

    Use Bun transactions and `RETURNING` for generated IDs on writable engines.
    Place the read-only policy before opening a write callback. Preserve
    PostgreSQL's independent capability probes for insight generation and
    deletion as adapter concerns, but use the shared insight queries after each
    authorized operation succeeds.

    Move Recall Store entry/query/import/event methods onto `BunStore`. Run them
    through the canonical SQLite Bun handle when `Capabilities().Recall` is
    true; otherwise return `db.ErrReadOnly` before issuing SQL. Keep extraction
    jobs, evidence reconciliation, and Recall FTS/vector internals local to
    SQLite. Require `WriteRecall` only for imports that can persist rows;
    dry-run validation uses the read capability. Composite entry/evidence reads
    stage a fresh result on every `ConsistentView` attempt, including not-found,
    and publish only the accepted attempt.

- [ ] **Step 5: Verify the common methods on every backend before deletion**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*(Data|Curation|Recall)Contract|Test(ProjectIdentity|Inventory|Pins|Stars|Insights|SessionMgmt|GetRecallEntry)' -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    ```

    Expected: tests that explicitly call each embedded `BunStore` pass for all
    engines, including PostgreSQL's allowed curation/session writes and rejected
    archive/Recall writes. Stop the PostgreSQL container if started here.

- [ ] **Step 6: Remove concrete duplicates and stubs**

    Delete common-method bodies from PostgreSQL/DuckDB curation, identity,
    inventory, project-rule, candidate, Recall-stub, and stub files. Retain
    operational push functions and adapter capability probes only.

- [ ] **Step 7: Re-run final contracts and commit**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    git add internal/db internal/postgres internal/duckdb internal/storetest
    git commit -m "refactor(storage): unify data and curation stores"
    ```

    Expected: literal read contracts match, allowed PostgreSQL mutations remain
    available, and forbidden operations preserve `db.ErrReadOnly`. Stop the
    PostgreSQL container if this agent started it.

______________________________________________________________________

### Task 7: Move pricing and usage into `BunStore`

**Files:**

- Create: `internal/db/bun_usage.go`
- Create: `internal/db/bun_usage_test.go`
- Modify: `internal/storetest/contract.go`
- Modify: `internal/db/pricing.go`
- Modify: `internal/db/pricing_list.go`
- Modify: `internal/db/usage.go`
- Modify: `internal/db/usage_events.go`
- Modify: `internal/db/cursor_usage_events.go`
- Modify: `internal/db/activityreport.go`
- Modify: `internal/db/reporting_export.go`
- Modify: `internal/db/session_export.go`
- Modify: `internal/db/session_stats.go`
- Modify: `internal/postgres/pricing.go`
- Modify: `internal/postgres/usage.go`
- Modify: `internal/duckdb/analytics_usage.go`
- Modify: `internal/duckdb/usage_pricing_test.go`

**Interfaces:**

- Consumes: canonical usage/pricing rows and the pure pricing/cost helpers
  already owned by `internal/db` and `internal/export`.

- Produces shared `GetDailyUsage`, `GetTopSessionsByCost`,
  `GetUsageSessionCounts`, `GetUsageMatchingSessionCount`, and
  `GetSessionUsage` methods, plus shared model-pricing loads. Moves
  `SetCustomPricing`, `SetEffectivePricing`, and `SetEmptyCatalogPricing` onto
  `BunStore`; concrete wrappers do not retain shadow pricing fields or
  setters.

- Produces `UpsertModelPricingRows` and `ReplaceModelPricingBandRows` canonical
  write helpers; pricing bands replace per model pattern in the same
  transaction as their base pricing row.

- Keeps SQLite pricing refresh state in `pricing_metadata`. `GetPricingMeta` and
  `SetPricingMeta` never read or create sentinel `model_pricing` rows, and
  actual pricing/band writes require valid canonical timestamps.

- Defines the canonical empty-catalog policy: embedded rates remain the base
  catalogue when stored pricing rows are absent, custom rates overlay that
  base, and an explicitly supplied effective catalogue overlays both. Every
  backend reports the same provenance for that state.

- Runs pricing, normalized usage rows, metadata hydration, and project identity
  resolution for each public usage operation inside one replay-safe
  `ConsistentView`. Each callback stages a fresh complete result and publishes
  only the accepted attempt.

- Keeps cursor/provider normalization and exact microdollar calculation in pure
  shared functions; only row selection and aggregation SQL move to Bun.

- [ ] **Step 1: Add literal cross-backend usage cases**

    Extend the contract with a reported-cost event, computed base-rate event,
    pricing-band event, aggregate event without an ordinal, Cursor event, and a
    session whose message timestamp—not session timestamp—places usage in the
    window. Assert exact integer microdollars, counts, ordering, and application
    provenance. Run the same fixture under custom pricing, effective-catalog
    pricing, and explicit empty-catalog fallback to prove promoted methods read
    `BunStore`'s synchronized pricing state.

- [ ] **Step 2: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*UsageContract|TestBunStoreUsage' -count=1
    ```

    Expected: `BunStore` does not yet implement usage and backend-specific
    aggregations still produce/scan independently.

- [ ] **Step 3: Implement shared pricing loads and usage row scopes**

    Use Bun models for pricing/bands and a portable common CTE that normalizes
    usage events, message-level token data, Cursor events, and session
    fallbacks. Keep band selection and money arithmetic in existing Go helpers
    so SQL never performs floating-point cost calculation.

    Route pricing refresh metadata through the dedicated SQLite extension.
    Preserve it explicitly during resync; do not fall back to sentinel pricing
    rows after Task 4's one-time migration.

- [ ] **Step 4: Implement shared usage aggregations**

    Select the smallest normalized row set needed for each API, then aggregate
    through the existing pure reducers. Push both padded time bounds and all
    portable source/session filters into Bun before hydration; retain only the
    final timezone date check and exact reducers in Go. Render SQLite timestamp
    bounds through its `julianday` adapter expression so mixed RFC3339 offsets
    compare as instants. Preserve authoritative reported cost and
    application-count semantics exactly.

- [ ] **Step 5: Verify the common methods on every backend before deletion**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*(UsageContract|Usage|Pricing)' -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    ```

    Expected: explicit embedded-`BunStore` usage contracts return identical exact
    money results on all engines. Stop the PostgreSQL container if started here.

- [ ] **Step 6: Remove backend-specific usage/pricing Store methods**

    Delete PostgreSQL and DuckDB Store receivers and duplicate scanners from
    `pricing.go`, `usage.go`, and `analytics_usage.go`; retain push/population
    helpers until Task 10 moves writes. Remove concrete pricing maps, locks, and
    duplicated setters in the same cleanup after all consumers resolve to
    `BunStore`'s synchronized pricing state.

- [ ] **Step 7: Re-run final contracts and commit**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    git add internal/db internal/postgres internal/duckdb internal/storetest
    git commit -m "refactor(storage): unify Bun usage queries"
    ```

    Expected: all engines return identical exact-money results. Stop the test
    PostgreSQL container if this agent started it.

______________________________________________________________________

### Task 8: Move analytics, trends, activity reports, and recent edits into `BunStore`

**Files:**

- Create: `internal/db/bun_analytics.go`
- Create: `internal/db/bun_activity_report.go`
- Create: `internal/db/bun_recent_edits.go`
- Create: `internal/db/bun_analytics_test.go`
- Modify: `internal/storetest/contract.go`
- Modify: `internal/db/analytics.go`
- Modify: `internal/db/trends.go`
- Modify: `internal/db/activityreport.go`
- Modify: `internal/db/recentedits.go`
- Modify: `internal/postgres/analytics.go`
- Modify: `internal/postgres/trends.go`
- Modify: `internal/postgres/activityreport.go`
- Modify: `internal/postgres/recentedits.go`
- Modify: `internal/duckdb/analytics_usage.go`
- Modify: `internal/duckdb/activityreport.go`
- Modify: `internal/duckdb/recentedits.go`

**Interfaces:**

- Consumes: `AnalyticsFilter`, pure reducers/bucket helpers, canonical session,
  message, tool, and usage rows.

- Produces every analytics `db.Store` method, `GetTrendsTerms`,
  `GetActivityReport`, and `RecentEdits` once on `BunStore`.

- Preserves existing API ordering, bucket boundaries, scope filtering, and
  cancellation behavior.

- [ ] **Step 1: Add a compact literal analytics matrix**

    Seed four sessions spanning two projects, agents, hours, automation states,
    and outcomes, with known tools/skills/edits. Assert hand-calculated summary,
    heatmap, hour-of-week, velocity, signal, trends, activity report, and recent
    edit values. Use one case per observable branch; do not duplicate every
    existing backend-specific test.

- [ ] **Step 2: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*AnalyticsContract|TestBunStoreAnalytics' -count=1
    ```

    Expected: common analytics methods are absent and concrete backends still own
    independent SQL.

- [ ] **Step 3: Implement shared scoped source CTEs**

    Build one Bun-backed session scope and small reusable message/tool/usage CTEs.
    Use portable SQL for grouping and ordering; move timestamp bucketing or
    percentile logic to existing Go reducers when standard engine semantics do
    not match exactly. Do not add dialect switches to common methods.

- [ ] **Step 4: Implement shared analytics/report/edit methods**

    Port each Store method in interface order, reusing the scoped sources and
    canonical scanners. Check `ctx.Err()` at existing cancellation boundaries,
    not inside every row operation.

- [ ] **Step 5: Verify the common methods on every backend before deletion**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*(AnalyticsContract|Analytics|Trends|ActivityReport|RecentEdits)' -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    ```

    Expected: explicit embedded-`BunStore` analytics contracts pass for all
    engines before any legacy receiver is removed. Stop the PostgreSQL container
    if started here.

- [ ] **Step 6: Delete backend-specific analytics receivers**

    Remove the corresponding Store methods and scanners from PostgreSQL and DuckDB
    files. Split any remaining push-time code out of `duckdb/analytics_usage.go`
    so that the file no longer acts as a store.

- [ ] **Step 7: Re-run final contracts and commit**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    git add internal/db internal/postgres internal/duckdb internal/storetest
    git commit -m "refactor(storage): unify Bun analytics queries"
    ```

    Expected: exact contract fixtures match across all engines and existing
    analytics regressions remain green. Stop the PostgreSQL container if started
    here.

______________________________________________________________________

### Task 9: Isolate FTS/vector capabilities and unify search hydration

**Files:**

- Create: `internal/db/bun_search.go`
- Create: `internal/db/bun_search_test.go`
- Create: `internal/db/search_capability.go`
- Modify: `internal/storetest/contract.go`
- Modify: `internal/db/search.go`
- Modify: `internal/db/search_content.go`
- Modify: `internal/db/search_content_semantic.go`
- Modify: `internal/db/secret_findings.go`
- Modify: `internal/db/secret_findings_list.go`
- Modify: `internal/db/vector.go`
- Modify: `internal/postgres/search_content.go`
- Modify: `internal/postgres/search_content_semantic.go`
- Modify: `internal/postgres/search_content_hybrid.go`
- Modify: `internal/postgres/vector_search.go`
- Modify: `internal/postgres/messages.go`
- Modify: `internal/postgres/secret_findings.go`
- Modify: `internal/duckdb/store.go`
- Modify: `internal/duckdb/secrets.go`
- Modify: `internal/duckdb/quack_sql.go`
- Modify: `internal/duckdb/search_content_units_test.go`

**Interfaces:**

- Consumes: canonical filters and common Bun session/message hydration.

- Produces narrow capability contracts:

    ```go
    type SearchHit struct {
        SessionID string
        Ordinal   int
        Snippet   string
        Rank      float64
    }

    type ContentSearchHit struct {
        SessionID   string
        Ordinal     int
        OrdinalStart int
        OrdinalEnd   int
        Subordinate  bool
        DocKey       string
        Location     string
        MessageID    *int64
        CallIndex    *int
        EventIndex   *int
        SourceUUID   string
        ToolName     string
        Snippet      string
        Score        *float64
    }

    type FullTextCapability interface {
        Available() bool
        Search(context.Context, SearchFilter) ([]SearchHit, error)
        SearchSession(context.Context, string, string) ([]int, error)
        SearchContent(context.Context, ContentSearchFilter) ([]ContentSearchHit, error)
    }

    type SemanticCapability interface {
        Available() bool
        SearchContent(context.Context, ContentSearchFilter) ([]ContentSearchHit, error)
        ResolveMessageUnits(context.Context, []MessageRef) ([]UnitRef, error)
    }
    ```

- Produces shared `BunStore.Search`, `SearchSession`, `SearchContent`,
  `ListSecretFindings`, and `SecretFindingSource`; capabilities return only
  canonical hits and the common store owns hydration, filtering, cursors, and
  final ordering.

- [ ] **Step 1: Add literal FTS/secret/semantic contract cases**

    Seed messages containing an exact phrase, regex metacharacters, tool input,
    tool-result events, and a known secret finding. Assert literal session IDs,
    ordinals, snippets, source text, cursor order, and unavailable-semantic
    error behavior. Keep vector setup backend-specific and assert the same
    canonical hit identity/rank inputs rather than engine score decimals.

- [ ] **Step 2: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*SearchContract|TestBunStoreSearch' -count=1
    ```

    Expected: `BunStore` has no capability-backed search and concrete stores still
    own hydration/pagination.

- [ ] **Step 3: Implement capability-backed search**

    Move SQLite FTS5, PostgreSQL FTS/pgvector, and DuckDB/Quack search SQL behind
    the capability interfaces. Each capability accepts canonical filters and
    returns stable `SearchHit`/`ContentSearchHit` identities, including tool
    call/result coordinates and semantic unit ranges. Use `BunStore` to resolve
    lexical hits through `ResolveMessageUnits`, batch hydrate sessions/messages,
    enforce common visibility scopes, sign cursors, fuse on unit identity, and
    reconstruct the exact source for secret redaction.

- [ ] **Step 4: Verify common capability hydration on every backend**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*(SearchContract|Search|Secret|Vector)' -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    ```

    Expected: tests invoking the embedded `BunStore` explicitly prove common
    hydration/cursors around each engine capability. Stop the PostgreSQL
    container if started here.

- [ ] **Step 5: Remove backend search Store methods**

    Delete duplicate search pagination, hydration, scan, and secret-list code.
    Retain only extension/index management and capability query implementations.
    Keep the current semantic-unavailable reason wiring on the PostgreSQL
    adapter.

- [ ] **Step 6: Re-run final search contracts and commit**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    git add internal/db internal/postgres internal/duckdb internal/storetest
    git commit -m "refactor(storage): isolate search capabilities"
    ```

    Expected: search result identity and ordering remain aligned while only the
    small FTS/vector query layer differs. Stop the test PostgreSQL container if
    started here.

______________________________________________________________________

### Task 10: Route archive ingestion and mirror pushes through canonical Bun rows

**Files:**

- Create: `internal/db/bun_write.go`
- Create: `internal/db/bun_write_test.go`
- Modify: `internal/db/session_batch.go`
- Modify: `internal/db/sessions.go`
- Modify: `internal/db/messages.go`
- Modify: `internal/db/usage_events.go`
- Modify: `internal/db/cursor_usage_events.go`
- Modify: `internal/db/secret_findings.go`
- Modify: `internal/db/project_identity.go`
- Modify: `internal/db/worktree_mappings.go`
- Modify: `internal/db/pricing.go`
- Modify: `internal/postgres/push.go`
- Modify: `internal/postgres/project_identity_upsert.go`
- Modify: `internal/postgres/worktree_mappings_push.go`
- Modify: `internal/postgres/vector_push.go`
- Modify: `internal/duckdb/push.go`
- Modify: `internal/duckdb/rebuild.go`
- Modify: `internal/duckdb/schema.go`
- Modify: `internal/duckdb/project_identity_upsert.go`
- Modify: `internal/duckdb/worktree_mappings_push.go`
- Modify: `internal/duckdb/sync.go`

**Interfaces:**

- Consumes: canonical Bun rows/converters and adapter transactions.

- Produces shared write helpers:

    ```go
    func ReplaceSessionRows(ctx context.Context, tx bun.IDB, archive ArchiveIdentity, write SessionBatchWrite) error
    func UpsertSessionRows(ctx context.Context, tx bun.IDB, write ReplicationSessionWrite, policy SessionConflictPolicy) error
    func ReplaceMessageRows(ctx context.Context, tx bun.IDB, sessionID string, rows []bunmodel.Message) error
    func ReplaceUsageEventRows(ctx context.Context, tx bun.IDB, sessionID string, rows []bunmodel.UsageEvent) error
    func ReplaceToolRows(ctx context.Context, tx bun.IDB, sessionID string, calls []bunmodel.ToolCall, results []bunmodel.ToolResultEvent) error
    ```

    `ReplicationSessionWrite` carries source archive, database generation,
    owner-marker, and alias context. `SessionConflictPolicy` is supplied by the
    adapter: SQLite/DuckDB use canonical replacement semantics, while PostgreSQL
    uses a Bun-built conflict clause that rejects excluded IDs and foreign
    owners and preserves target-owned display-name/deletion baselines. This is
    an operational replication policy, not a duplicate common Store query.

    SQLite uses the archive-identity stamping invariant established in Task 4;
    this task replaces its SQL implementation with Bun without adding a second
    retrieval or compatibility path.

    Generated curation targets keep their target-assigned pin IDs on logical
    conflicts. DuckDB mirrors preserve positive source-assigned IDs: before a
    mirrored upsert, move any stale logical owner off a reused source ID inside
    the same transaction, then adopt that ID on the current logical pin. Never
    apply preserve-mode deletion/reconciliation to generated-ID targets; any
    later insert failure rolls the stale owner and current logical pin back.

- Consumes the canonical pricing/band and star/pin write helpers defined in
  Tasks 6-7; the cross-target fixture never relies on an undefined test-only
  writer.

- Preserves SQLite batch atomicity/callback ordering, PostgreSQL push
  fingerprint/watermark behavior, and DuckDB whole-session replacement.

- Identity observations and snapshots publish in 500-row batches, keeping the
  widest canonical identity statement below PostgreSQL's parameter limit while
  bounding full-publication statement count. Their complete portable payload
  is replacement-owned by the source; only declared conflict keys and explicit
  adapter-owned fields are excluded from registry-derived replacement columns.

- PostgreSQL write-contract completion is represented jointly by the target
  fingerprint, watermark, and boundary fingerprints. The target fingerprint
  may persist after a partially successful push; failed session boundaries
  remain retryable until their rows publish successfully.

- [ ] **Step 1: Write failing cross-target write tests**

    Define a test-only `canonicalWriteFixture` with separate literal fields:

    ```go
    type canonicalWriteFixture struct {
        Batch   SessionBatchWrite
        Pricing []bunmodel.ModelPricing
        Bands   []bunmodel.ModelPricingBand
        Stars   []bunmodel.StarredSession
        Pins    []bunmodel.PinnedMessage
    }
    ```

    `Batch` contains the session, messages, tool calls/results, usage, secrets,
    and identity data already supported by `SessionBatchWrite`; the other fields
    are applied through their canonical write APIs in the same target setup.
    Apply the fixture to a temporary SQLite archive, PostgreSQL target, and
    DuckDB mirror, then read each with the common store and assert identical
    durable rows. PostgreSQL cases include a foreign owner, an excluded session,
    local target rename/trash state, and a legacy alias; assert a later push
    neither takes ownership nor overwrites target curation. Add failure
    injection before commit and assert no partial rows or advanced watermark
    remain. Do not widen the production `SessionBatchWrite` contract for test
    convenience.

- [ ] **Step 2: Verify RED**

    Run:

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'TestCanonicalBunWrite|TestWriteSessionBatchAtomic' -count=1
    make test-postgres
    ```

    Expected: canonical write helpers are absent and existing target-specific
    writers do not satisfy the shared failure-injection test on any target.
    Leave the PostgreSQL container running for the GREEN rerun because this
    agent started it.

- [ ] **Step 3: Implement shared Bun row writes**

    Convert parser-facing writes to canonical rows once. Use Bun transactions,
    chunked model inserts, `ON CONFLICT` clauses supported by all targets, and
    whole-session dependent-row replacement. Where generated IDs differ, insert
    source-assigned IDs for mirrored rows and use `RETURNING` only on writable
    archive/curation paths.

- [ ] **Step 4: Rewire PostgreSQL push**

    Keep target selection, push windows, fingerprints, advisory locking, vector
    generation, and watermarks in `internal/postgres`. Replace its per-table SQL
    with the shared row write helpers. Advance fingerprints/watermarks only
    after the Bun transaction commits.

- [ ] **Step 5: Rewire DuckDB rebuild and incremental push**

    Keep rebuild/probe/swap, per-session fingerprint gating, metadata cursors, and
    local-only target validation in `internal/duckdb`. Replace common table
    inserts/replacements with shared Bun helpers. Add a transaction-aware
    metadata writer in `internal/duckdb/schema.go`. Per-session fingerprints may
    publish with their completed session batch; operation-level cutoffs,
    revisions, scope, and the fresh mirror generation publish only after every
    batch in the complete push succeeds, in the same final DuckDB transaction. A
    failed later batch therefore cannot advance a cutoff past unprocessed
    sessions.

- [ ] **Step 6: Verify local and PostgreSQL write paths**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test(CanonicalBunWrite|WriteSessionBatchAtomic|Push|Rebuild|SyncFastpath)' -count=1
    make test-postgres
    go fmt ./...
    go vet ./...
    ```

    Expected: all targets expose the same durable row data, failed batches remain
    atomic, and incremental push skip/watermark tests pass.

- [ ] **Step 7: Commit shared replication writes**

    ```bash
    git add internal/db internal/postgres internal/duckdb
    git commit -m "refactor(storage): unify Bun replication writes"
    ```

    Stop the PostgreSQL test container if this agent started it.

______________________________________________________________________

### Task 11: Remove legacy SQL paths and finish the single-store cutover

**Files:**

- Delete: `internal/db/query_dialect.go`
- Delete: `internal/db/query_dialect_test.go`
- Delete or reduce to capability/operational code:
  `internal/postgres/sessions.go`
- Delete or reduce to capability/operational code:
  `internal/postgres/messages.go`
- Delete or reduce to capability/operational code:
  `internal/postgres/analytics.go`
- Delete or reduce to capability/operational code: `internal/postgres/usage.go`
- Delete or reduce to capability/operational code: `internal/duckdb/store.go`
- Delete or reduce to capability/operational code: `internal/duckdb/messages.go`
- Delete or reduce to capability/operational code:
  `internal/duckdb/analytics_usage.go`
- Delete: `internal/duckdb/stubs.go`
- Modify: `internal/backendcontract/contract.go`
- Modify: `internal/db/store.go`
- Modify: `cmd/agentsview/pg.go`
- Modify: `cmd/agentsview/duckdb.go`
- Modify: `cmd/agentsview/pg_vector_search.go`
- Modify: `internal/backendbench/store_bench_test.go`
- Modify: `docs/agents/storage.md`

**Interfaces:**

- Consumes: the complete shared `BunStore` and backend capability adapters.

- Produces exactly one implementation of every method in `db.Store`.

- Produces concrete SQLite/PostgreSQL/DuckDB wrappers that expose only
  lifecycle, sync, operational metadata, and FTS/vector additions.

- Removes the old placeholder/dialect builder and all legacy Store receivers.

- Centralizes the remaining live SQLite identity, mapping, and session-snapshot
  writes behind application-owned Bun transactions. Those helpers update
  publication revisions and change journals atomically, after which the 11
  canonical identity triggers are removed. Migration-only raw seams remain
  named and documented.

- [ ] **Step 1: Compile the complete backend contract and inventory shadows**

    Run focused source inventories as development checks, not tests:

    ```bash
    go doc -all go.kenn.io/agentsview/internal/db.Store
    rg -n '^func \([^)]*\*(DB|Store)\) [A-Z][A-Za-z0-9_]*\(' \
      internal/db internal/postgres internal/duckdb --glob '*.go' --glob '!**/*_test.go'
    rg -n 'QueryDialect|NewQueryBuilder|BuildSessionFilterSQL' \
      internal/db internal/postgres internal/duckdb --glob '*.go'
    ```

    Cross-check every method printed for `db.Store` against the complete exported
    receiver inventory. Record every remaining concrete receiver as lifecycle,
    sync/operational metadata, or an allowed FTS/vector capability; there is no
    sampled method-name regex, and `GetAnalytics*` families cannot evade the
    audit. Do not add a source-grep regression test.

- [ ] **Step 2: Remove shadowing methods and the legacy dialect builder**

    Delete old receivers and their backend-only scanners after their replacement
    contract passes. Split mixed files so retained push/search/lifecycle code
    has one responsibility. Remove `QueryDialect`, placeholder builders, and
    query tests that only protected the deleted implementation.

    Finish routing SQLite identity and worktree-mapping mutations through the
    canonical helpers. Move revision and change-journal maintenance into those
    same transactions, remove the canonical identity triggers, and make stamped
    archive opens validation-only. This is the final cutover, not a temporary
    dual write or compatibility layer.

- [ ] **Step 3: Audit direct `database/sql` execution**

    Search all three storage packages:

    ```bash
    rg -n '(QueryContext|QueryRowContext|ExecContext|BeginTx|\.Query\(|\.QueryRow\(|\.Exec\()' \
      internal/db internal/postgres internal/duckdb --glob '*.go' --glob '!**/*_test.go'
    ```

    Convert remaining application/schema/transaction calls to Bun. Retain direct
    calls only in driver open/configuration, dedicated connection setup
    (`PRAGMA`, `ATTACH`, `USE`, advisory connector state), pool drain/swap, and
    unavoidable driver probes. Add a short ownership comment at each retained
    direct-execution seam.

- [ ] **Step 4: Simplify command wiring and backend benchmarks**

    Construct the same shared store from each command path, wire search
    capabilities through adapters, and update backend benchmarks to measure the
    common Store methods without type assertions to backend-specific query
    implementations.

- [ ] **Step 5: Update durable storage guidance**

    Add the canonical Bun ownership rule and allowed adapter seams to
    `docs/agents/storage.md`. Preserve all existing archive/mirror safety rules;
    do not copy dependency or command catalogues into the guide.

- [ ] **Step 6: Verify the cutover and commit**

    ```bash
    CGO_ENABLED=1 go test -tags fts5 ./internal/db/... ./internal/postgres/... \
      ./internal/duckdb/... ./internal/backendcontract/... -count=1
    go fmt ./...
    go vet ./...
    git add internal cmd/agentsview docs/agents/storage.md
    git commit -m "refactor(storage): complete shared Bun cutover"
    ```

    Expected: compile-time backend contracts pass with one common method owner,
    and focused searches show only documented direct-driver seams.

______________________________________________________________________

### Task 12: Run full parity, performance, and workflow verification

**Files:**

- Modify only if behavior evidence requires it: `internal/storetest/contract.go`
- Modify only if behavior evidence requires it:
  `internal/backendbench/store_bench_test.go`
- Modify only if user-visible behavior changed unexpectedly: `README.md`
- Modify: `docs/superpowers/plans/2026-08-02-unified-bun-storage.md`

**Interfaces:**

- Consumes: the finished shared store, canonical schema, migrations, dialect,
  adapters, and existing build/test targets.

- Produces verified parity across SQLite, PostgreSQL, local DuckDB, and Quack;
  benchmark evidence; completed Kata issues; and a clean committed worktree.

- [ ] **Step 1: Run focused race/lifecycle verification**

    ```bash
    CGO_ENABLED=1 go test -race -tags fts5 ./internal/db ./internal/duckdb \
      -run 'Test.*(Reopen|Replacement|Close|Quack|StoreContract)' -count=1
    ```

    Expected: no races, stale handles, leaked replacement aliases, or contract
    failures.

- [ ] **Step 2: Run the full Go suite**

    ```bash
    make test
    go vet ./...
    ```

    Expected: all packages pass with the required FTS5/CGO configuration and vet
    reports no findings.

- [ ] **Step 3: Run PostgreSQL integration verification**

    ```bash
    make test-postgres
    ```

    Expected: schema migration, push, search, analytics, usage, curation, and
    store contracts pass against the dedicated container. Stop it with
    `make postgres-down` because this agent started it.

- [ ] **Step 4: Run DuckDB and Quack workflow verification**

    Run the repository's DuckDB-tagged Go tests and focused browser workflow:

    ```bash
    CGO_ENABLED=1 go test -tags 'fts5,duckdbtest' ./internal/duckdb/... \
      ./cmd/agentsview/... -count=1
    make e2e-duckdb
    ```

    Expected: local mirror serve, Quack query transport, Bun-generated quoted
    predicates and JSON/timestamp scans, stale-attachment retry, credential
    redaction, data mode, session list, and mirror replacement workflows pass.

- [ ] **Step 5: Run backend performance gates**

    First capture the candidate's backend comparison and hot-path samples:

    ```bash
    backend_flags="-bench . -run '^$' -benchmem -count 6 -benchtime 20x"
    make bench-backends BENCH_BACKENDS_FLAGS="$backend_flags" \
      | tee /tmp/agentsview-bun-backends-new.txt
    make bench-gate | tee /tmp/agentsview-bun-gate-new.txt
    ```

    Then create an isolated source snapshot of the exact merge base, run the same
    fixed-count gate configuration there, and compare with the repository's real
    threshold command:

    ```bash
    baseline_dir=$(mktemp -d)
    merge_base=$(git merge-base HEAD origin/main)
    git archive "$merge_base" | tar -x -C "$baseline_dir"
    eval "$(make bench-gate-config)"
    (
      cd "$baseline_dir"
      make bench-backends BENCH_BACKENDS_FLAGS="$backend_flags"
    ) | tee /tmp/agentsview-bun-backends-old.txt
    (
      cd "$baseline_dir"
      make bench-gate \
        BENCH_GATE_COUNT="$BENCH_GATE_COUNT" \
        BENCH_GATE_TIME="$BENCH_GATE_TIME"
    ) | tee /tmp/agentsview-bun-gate-old.txt
    go run ./cmd/benchgate \
      -old /tmp/agentsview-bun-gate-old.txt \
      -new /tmp/agentsview-bun-gate-new.txt
    go run ./cmd/benchgate \
      -old /tmp/agentsview-bun-backends-old.txt \
      -new /tmp/agentsview-bun-backends-new.txt
    ```

    Expected: both comparisons report
    `benchgate: no regressions beyond   thresholds`; backend-store performance
    is enforced rather than recorded for manual review. Contract
    query-count/cardinality limits also pass. If a benchmark regresses, profile
    only these isolated fixtures and fix query count/index usage before
    proceeding. The snapshot directory was created by this agent and may be
    removed after the comparison.

- [ ] **Step 6: Re-run deletion/direct-access audits**

    ```bash
    go doc -all go.kenn.io/agentsview/internal/db.Store
    rg -n '^func \([^)]*\*(DB|Store)\) [A-Z][A-Za-z0-9_]*\(' \
      internal/db internal/postgres internal/duckdb --glob '*.go' --glob '!**/*_test.go'
    rg -n 'QueryDialect|NewQueryBuilder|BuildSessionFilterSQL' \
      internal/db internal/postgres internal/duckdb --glob '*.go'
    rg -n '(QueryContext|QueryRowContext|ExecContext|BeginTx|\.Query\(|\.QueryRow\(|\.Exec\()' \
      internal/db internal/postgres internal/duckdb --glob '*.go' --glob '!**/*_test.go'
    git diff --check
    git status --short
    ```

    Expected: one shared owner for common methods, no legacy query dialect, only
    documented low-level direct-driver seams, no whitespace errors, and no
    unrelated worktree changes.

- [ ] **Step 7: Commit any verification-driven corrections**

    If verification changed tracked files, make one focused correction commit
    after re-running the affected checks. Inspect `git status --short`, pass
    each relevant path literally to `git add`, review `git diff --cached`, and
    commit with subject `fix(storage): finalize Bun parity`. Do not use a glob
    or repository-wide add.

    If no tracked files changed, do not create an empty commit. Mark each Kata
    child complete immediately after its own evidence and commit exist; close
    the parent only after all children are complete.

______________________________________________________________________
