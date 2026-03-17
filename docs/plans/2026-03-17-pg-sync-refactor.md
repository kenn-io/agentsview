# PG Sync Refactor Implementation Plan

> **For agentic workers:** REQUIRED: Use
> superpowers:subagent-driven-development (if subagents available) or
> superpowers:executing-plans to implement this plan. Steps use checkbox
> (`- [ ]`) syntax for tracking.

**Goal:** Refactor the PostgreSQL sync feature to consolidate packages,
fix CLI ergonomics, migrate config from JSON to TOML, and use native
PG timestamps.

**Architecture:** Three PG packages (`pgdb`, `pgsync`, `pgutil`) merge
into `internal/postgres/`. Config migrates from JSON to TOML with
auto-migration. CLI gains a `pg` command group replacing overloaded
flags. PG schema uses `TIMESTAMPTZ` natively and schema name is
configurable via `search_path`.

**Tech Stack:** Go, PostgreSQL (pgx driver), BurntSushi/toml, SQLite

**Spec:** `docs/specs/2026-03-17-pg-sync-refactor-design.md`

---

## File Structure

### New files (internal/postgres/)

| File | Responsibility |
|------|---------------|
| `connect.go` | SSL checks, DSN helpers, open pool with `search_path` |
| `schema.go` | DDL with `TIMESTAMPTZ`, `EnsureSchema`, `CheckSchemaCompat` |
| `sync.go` | `Sync` struct (push coordinator), constructor, `Status` |
| `push.go` | Push logic, fingerprinting, boundary state, message dedup |
| `store.go` | `Store` struct (read-only `db.Store` impl), stubs |
| `sessions.go` | Session list/detail/child queries for read side |
| `messages.go` | Message queries, ILIKE search for read side |
| `analytics.go` | Analytics queries using native PG time functions |
| `time.go` | SQLite text ↔ `time.Time` conversion helpers |

### New files (cmd/)

| File | Responsibility |
|------|---------------|
| `cmd/agentsview/pg.go` | `pg` command group: `push`, `status`, `serve` |

### Modified files

| File | Changes |
|------|---------|
| `internal/config/config.go` | TOML read/write, JSON migration, `PGConfig` struct |
| `internal/config/config_test.go` | Update for TOML format |
| `internal/config/persistence_test.go` | Update for TOML format |
| `cmd/agentsview/main.go` | Route `pg` subcommand, remove PG from `runServe` |
| `cmd/agentsview/sync.go` | Remove `--pg`/`--pg-status` flags |
| `cmd/agentsview/sync_test.go` | Remove PG flag tests |
| `go.mod` / `go.sum` | Add `BurntSushi/toml` |

### Deleted files

| Directory | Reason |
|-----------|--------|
| `internal/pgdb/` (all files) | Merged into `internal/postgres/` |
| `internal/pgsync/` (all files) | Merged into `internal/postgres/` |
| `internal/pgutil/` (all files) | Merged into `internal/postgres/` |

---

## Task 1: Add TOML dependency

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Add BurntSushi/toml**

```bash
go get github.com/BurntSushi/toml@latest
```

- [ ] **Step 2: Verify**

```bash
grep BurntSushi go.mod
```

Expected: `github.com/BurntSushi/toml vX.Y.Z`

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "build: add BurntSushi/toml dependency"
```

---

## Task 2: Migrate config from JSON to TOML

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `internal/config/persistence_test.go`

This task changes the config file format. It does not change PG field
names or struct names yet — that happens in Task 6.

Note: the `loadFile()` method has a second JSON parsing pass for agent
directory arrays (lines 292-319) that uses `map[string]json.RawMessage`.
TOML decode into `map[string]any` handles nested arrays natively, so
this section needs a rewrite, not a mechanical substitution.

All config write methods (Step 5) must be completed together with the
read changes (Step 3) — if `ensureCursorSecret()` still writes JSON to
`config.toml`, the file will be corrupted on next read.

- [ ] **Step 1: Update configPath() to return config.toml**

In `config.go`, change `configPath()` to return
`filepath.Join(c.DataDir, "config.toml")`.

- [ ] **Step 2: Add jsonConfigPath() for migration**

Add a helper that returns the old JSON path:
```go
func (c Config) jsonConfigPath() string {
    return filepath.Join(c.DataDir, "config.json")
}
```

- [ ] **Step 3: Rewrite loadFile() to parse TOML**

Replace `json.Unmarshal` with `toml.Decode`. Before loading TOML,
call `migrateJSONToTOML()` to handle the one-time migration.

The TOML struct tags on `Config` and all nested structs need to be
added. Keep the `json:` tags as well (they are used for JSON API
responses in other contexts, e.g. `SaveSettings` reads/writes the
config file and the struct is also used for JSON marshaling in
some server responses). Add `toml:` tags alongside `json:` tags.

- [ ] **Step 4: Write migrateJSONToTOML()**

```go
func (c Config) migrateJSONToTOML() error {
    jsonPath := c.jsonConfigPath()
    tomlPath := c.configPath()

    // Only migrate if JSON exists and TOML doesn't.
    if _, err := os.Stat(tomlPath); err == nil {
        return nil
    }
    data, err := os.ReadFile(jsonPath)
    if err != nil {
        if os.IsNotExist(err) {
            return nil
        }
        return err
    }

    // Parse JSON into a generic map, write as TOML.
    var raw map[string]any
    if err := json.Unmarshal(data, &raw); err != nil {
        return fmt.Errorf("reading config.json: %w", err)
    }
    f, err := os.Create(tomlPath)
    if err != nil {
        return err
    }
    defer f.Close()
    if err := toml.NewEncoder(f).Encode(raw); err != nil {
        return fmt.Errorf("writing config.toml: %w", err)
    }
    // Rename old file so it isn't re-read.
    return os.Rename(jsonPath, jsonPath+".bak")
}
```

- [ ] **Step 5: Rewrite config write methods to use TOML**

Update `SaveSettings`, `SaveTerminalConfig`, `ensureCursorSecret`,
`SaveGithubToken`, and `EnsureAuthToken`. Each currently does:
1. Read config.json
2. `json.Unmarshal` into `map[string]any`
3. Patch the map
4. `json.MarshalIndent` and write back

Change to:
1. Read config.toml
2. `toml.Decode` into `map[string]any`
3. Patch the map
4. `toml.NewEncoder(f).Encode` and write back

The pattern is identical, just different marshal/unmarshal calls.

- [ ] **Step 6: Update config_test.go**

All tests that write JSON config fixtures need to write TOML instead.
For example, change:
```go
`{"host": "0.0.0.0", "port": 9090}`
```
to:
```toml
host = "0.0.0.0"
port = 9090
```

Also update assertions that check config file contents.

- [ ] **Step 7: Update persistence_test.go**

Same pattern — switch fixture format from JSON to TOML.

- [ ] **Step 8: Run tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config/... -v
```

Expected: all pass.

- [ ] **Step 9: Commit**

```bash
git add internal/config/ go.mod go.sum
git commit -m "refactor: migrate config from JSON to TOML"
```

---

## Task 3: Create internal/postgres/ package (connect + schema)

**Files:**
- Create: `internal/postgres/connect.go`
- Create: `internal/postgres/connect_test.go`
- Create: `internal/postgres/schema.go`
- Create: `internal/postgres/time.go`
- Create: `internal/postgres/time_test.go`

This task creates the foundation of the consolidated package. Source
material comes from `internal/pgutil/pgutil.go` and
`internal/pgsync/schema.go`.

**Important**: Tasks 3-5 create the new package alongside the old ones.
Do NOT update import paths in `cmd/` or other packages — that happens
in Task 7. The old packages compile independently until deleted in
Task 8.

- [ ] **Step 1: Create connect.go**

Move from `pgutil/pgutil.go`:
- `CheckSSL(dsn string) error`
- `WarnInsecureSSL(dsn string)`
- `RedactDSN(dsn string) string`
- `isLoopback(host string) bool`
- `hasPlaintextPath(cfg *pgconn.Config) bool`

Add new function:
```go
// Open opens a PG connection pool and sets search_path to the
// given schema. Returns *sql.DB ready for use.
func Open(dsn, schema string, allowInsecure bool) (*sql.DB, error)
```

This function:
1. Calls `CheckSSL` (or `WarnInsecureSSL` if `allowInsecure`)
2. Opens `sql.Open("pgx", dsn)`
3. Pings the connection
4. Executes `SET search_path = <schema>` (use
   `pgx.RegisterConnConfig` or a `ConnConfig.AfterConnect` hook
   to set it on every connection in the pool)
5. Returns the pool

- [ ] **Step 2: Create connect_test.go**

Move tests from `pgutil/pgutil_test.go`. Update package name to
`postgres`. Add a test for `Open` that validates `search_path` is
set (unit test with mock or skip if no PG available via build tag).

- [ ] **Step 3: Create schema.go**

Move from `pgsync/schema.go` with these changes:
- Remove hardcoded `agentsview.` schema prefix from all DDL — queries
  are now unqualified because `search_path` handles it
- Change all timestamp columns from `TEXT` to `TIMESTAMPTZ`:
  - `sessions.created_at` → `TIMESTAMPTZ`
  - `sessions.started_at` → `TIMESTAMPTZ`
  - `sessions.ended_at` → `TIMESTAMPTZ`
  - `sessions.deleted_at` → `TIMESTAMPTZ`
  - `sessions.updated_at` → `TIMESTAMPTZ NOT NULL DEFAULT NOW()`
  - `messages.timestamp` → `TIMESTAMPTZ`
- Delete `normalizePGUpdatedAt` function
- Delete `normalizeCreatedAt` backfill
- Delete `updated_at_format_version` and
  `tool_calls_call_index_version` tracking
- Delete advisory lock migration machinery
- Keep `EnsureSchema(ctx, db)` and `CheckSchemaCompat(ctx, db)`
- Keep `IsReadOnlyError(err) bool`
- Rename `EnsureSchemaDB` to just use `EnsureSchema` (single entry
  point since we no longer have the `PGSync` wrapper)

- [ ] **Step 4: Create time.go**

Simpler than the original `pgsync/time.go`. Only needs:
```go
// ParseSQLiteTimestamp parses an ISO-8601 text timestamp from
// SQLite into a time.Time. Returns zero time and false for
// empty/null strings.
func ParseSQLiteTimestamp(s string) (time.Time, bool)

// FormatISO8601 formats a time.Time to ISO-8601 UTC string
// for JSON API responses.
func FormatISO8601(t time.Time) string
```

Keep the local sync watermark helpers that manage push boundary
state in SQLite (`normalizeLocalSyncTimestamp`,
`previousLocalSyncTimestamp`).

- [ ] **Step 5: Create time_test.go**

Move relevant tests from `pgsync/time_test.go`. Delete tests for
removed normalization functions. Add tests for
`ParseSQLiteTimestamp` and `FormatISO8601`.

- [ ] **Step 6: Run tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/postgres/... -v
```

Expected: all pass (unit tests only, no PG required).

- [ ] **Step 7: Commit**

```bash
git add internal/postgres/
git commit -m "refactor: create internal/postgres package with connect, schema, time"
```

---

## Task 4: Move push and sync into internal/postgres/

**Files:**
- Create: `internal/postgres/sync.go`
- Create: `internal/postgres/sync_test.go`
- Create: `internal/postgres/push.go`
- Create: `internal/postgres/push_test.go`
- Create: `internal/postgres/integration_test.go`

Source material: `internal/pgsync/pgsync.go`, `push.go`, and test
files.

- [ ] **Step 1: Create sync.go**

Move from `pgsync/pgsync.go` with these changes:
- Rename `PGSync` → `Sync`
- Remove `interval` field and `time.Duration` parameter from
  constructor
- Delete `StartPeriodicSync()` method entirely
- Delete `runSyncCycle()` method
- Update constructor `New()` to call `postgres.Open()` from
  `connect.go` instead of doing its own connection setup
- Keep `Push()`, `Status()`, `EnsureSchema()`, `Close()` methods
- Remove `agentsview.` schema prefix from `Status()` queries

- [ ] **Step 2: Create push.go**

Move from `pgsync/push.go` with these changes:
- Update receiver type from `*PGSync` to `*Sync`
- Remove `agentsview.` schema prefix from all queries (~23
  occurrences)
- Update timestamp handling in `pushSession()`:
  - Parse SQLite text timestamps to `time.Time` using
    `ParseSQLiteTimestamp()` before passing to PG
  - Use `$N` placeholders with `time.Time` values instead of
    string interpolation for timestamps
- Update `pushMessages()`:
  - Parse `message.Timestamp` to `time.Time` before inserting
  - Scan `TIMESTAMPTZ` from PG into `time.Time` for fingerprint
    comparison
- Update `updated_at` handling: use `NOW()` in SQL instead of
  `pgTimestampSQL` text generation function
- Delete `pgTimestampSQL` function (no longer needed)
- Delete `normalizeSyncTimestamps` function
- Delete `normalizeLocalSyncStateTimestamps` function

- [ ] **Step 3: Create push_test.go**

Move from `pgsync/push_test.go`. Update:
- Package name to `postgres`
- Import paths
- Any timestamp assertions to use `time.Time` comparisons

- [ ] **Step 4: Create sync_test.go**

Move from `pgsync/pgsync_test.go` and `pgsync_unit_test.go` with:
- Package name to `postgres`
- Remove tests for `StartPeriodicSync`
- Remove `interval` parameter from `New()` calls in tests
- Update import paths
- Remove `agentsview.` schema prefix from test assertions/queries

- [ ] **Step 5: Create integration_test.go**

Move from `pgsync/integration_test.go`. Update:
- Package name to `postgres`
- Import paths
- Schema setup to use unqualified table names after `search_path`

- [ ] **Step 6: Run unit tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/postgres/... -v -run 'Test[^I]'
```

Expected: unit tests pass (skip integration tests without PG).

- [ ] **Step 7: Commit**

```bash
git add internal/postgres/
git commit -m "refactor: move push/sync into internal/postgres"
```

---

## Task 5: Move read-only store into internal/postgres/

**Files:**
- Create: `internal/postgres/store.go`
- Create: `internal/postgres/store_test.go`
- Create: `internal/postgres/sessions.go`
- Create: `internal/postgres/sessions_test.go`
- Create: `internal/postgres/messages.go`
- Create: `internal/postgres/messages_test.go`
- Create: `internal/postgres/analytics.go`
- Create: `internal/postgres/analytics_test.go`

Source material: `internal/pgdb/` files.

- [ ] **Step 1: Create store.go**

Move from `pgdb/pgdb.go` with these changes:
- Rename `PGDB` → `Store`
- Rename constructor `New()` → `NewStore(url, schema string, allowInsecure bool)`
- Call `postgres.Open()` from `connect.go` for connection setup
- Keep all read-only stub methods that return `db.ErrReadOnly`
- Keep `ReadOnly() bool` returning `true`
- Keep cursor signing logic

- [ ] **Step 2: Create sessions.go**

Move from `pgdb/sessions.go` with these changes:
- Update receiver from `*PGDB` to `*Store`
- Remove `agentsview.` schema prefix from all queries (~14
  occurrences)
- Update timestamp column scanning:
  - Scan `TIMESTAMPTZ` columns into `*time.Time`
  - Format to `*string` (ISO-8601) when populating `db.Session`
    structs for API compatibility
- Update `GetSessionVersion()`:
  - Scan `updated_at` as `time.Time`
  - Format to string before hashing

- [ ] **Step 3: Create messages.go**

Move from `pgdb/messages.go` with these changes:
- Update receiver from `*PGDB` to `*Store`
- Remove `agentsview.` schema prefix (~6 occurrences)
- Scan `TIMESTAMPTZ` message timestamps into `time.Time`, format to
  string for API

- [ ] **Step 4: Create analytics.go**

Move from `pgdb/analytics.go` with these changes:
- Update receiver from `*PGDB` to `*Store`
- Remove `agentsview.` schema prefix (~18 occurrences)
- Rewrite timestamp queries to use native PG functions:
  - `SUBSTR(started_at, 1, 10)` → `DATE(started_at)`
  - `SUBSTR(timestamp, 1, 10)` → `DATE(timestamp)`
  - String-based hour extraction → `EXTRACT(HOUR FROM timestamp)`
  - String-based day-of-week → `EXTRACT(DOW FROM timestamp)`
  - `LEFT(started_at, N)` date comparisons → proper
    `started_at >= $1::timestamptz` comparisons
- Rewrite duration calculation:
  - Currently parses text timestamps in Go for duration ranking
  - Can use `EXTRACT(EPOCH FROM ended_at - started_at)` in SQL

- [ ] **Step 5: Create test files**

Move from `pgdb/*_test.go` files (`analytics_test.go`,
`messages_test.go`, `pgdb_test.go`, `pgdb_unit_test.go`). There is
no existing `sessions_test.go` in pgdb — session query tests live
inside `pgdb_test.go`. Update:
- Package name to `postgres`
- Import paths
- Struct names (`PGDB` → `Store`)
- Schema setup in test helpers (use `search_path` instead of
  schema-qualified DDL)

- [ ] **Step 6: Run unit tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/postgres/... -v
```

Expected: all pass.

- [ ] **Step 7: Commit**

```bash
git add internal/postgres/
git commit -m "refactor: move read-only store into internal/postgres"
```

---

## Task 6: Rename PG config fields and update env vars

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

Now that `internal/postgres/` exists with its new API, update the
config to match.

- [ ] **Step 1: Rename PGSyncConfig to PGConfig**

```go
type PGConfig struct {
    URL           string `toml:"url" json:"url"`
    Schema        string `toml:"schema" json:"schema"`
    MachineName   string `toml:"machine_name" json:"machine_name"`
    AllowInsecure bool   `toml:"allow_insecure" json:"allow_insecure"`
}
```

Remove `Enabled`, `Interval`, `PostgresURL` fields.
Rename `Config.PGSync` field to `Config.PG`.

- [ ] **Step 2: Add Schema field with default**

In `ResolvePG()` (renamed from `ResolvePGSync()`), default `Schema`
to `"agentsview"` if empty.

- [ ] **Step 3: Update loadEnv()**

- Keep `AGENTSVIEW_PG_URL` → `c.PG.URL`
- Add `AGENTSVIEW_PG_SCHEMA` → `c.PG.Schema`
- Keep `AGENTSVIEW_PG_MACHINE` → `c.PG.MachineName`
- Remove `AGENTSVIEW_PG_INTERVAL`
- Remove `AGENTSVIEW_PG_READ`

- [ ] **Step 4: Update loadFile()**

The TOML section key changes from `pg_sync` to `pg`. Since we
changed the struct field name, the TOML decoder handles this
automatically via the struct tag.

- [ ] **Step 5: Remove PGReadURL field from Config**

Delete `PGReadURL` field. The `pg serve` command reads `PG.URL`
directly.

- [ ] **Step 6: Remove RegisterServeFlags pg-read flag**

Delete the `--pg-read` flag from `RegisterServeFlags()`. The pg
serve functionality moves to the `pg serve` subcommand.

- [ ] **Step 7: Update tests**

Update config_test.go for new field names, removed fields, and new
env vars.

- [ ] **Step 8: Run tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config/... -v
```

Expected: all pass.

- [ ] **Step 9: Update cmd/ references to compile**

Update `cmd/agentsview/main.go` and `cmd/agentsview/sync.go` to use
the renamed fields (`cfg.PG` instead of `cfg.PGSync`,
`cfg.ResolvePG()` instead of `cfg.ResolvePGSync()`, etc.) so the
project compiles after this task. This is a mechanical find-and-replace
— the structural CLI changes happen in Task 7.

- [ ] **Step 10: Run tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/config/... -v
CGO_ENABLED=1 go build -tags fts5 ./cmd/agentsview/
```

Expected: all pass, build succeeds.

- [ ] **Step 11: Commit**

```bash
git add internal/config/ cmd/agentsview/
git commit -m "refactor: rename PGSyncConfig to PGConfig, update fields"
```

---

## Task 7: Create pg command group and update CLI

**Files:**
- Create: `cmd/agentsview/pg.go`
- Modify: `cmd/agentsview/main.go`
- Modify: `cmd/agentsview/sync.go`
- Modify: `cmd/agentsview/sync_test.go`

- [ ] **Step 1: Create pg.go**

New file with:
```go
func runPG(args []string) {
    if len(args) == 0 {
        fmt.Fprintln(os.Stderr, "usage: agentsview pg <push|status|serve>")
        os.Exit(1)
    }
    switch args[0] {
    case "push":
        runPGPush(args[1:])
    case "status":
        runPGStatus(args[1:])
    case "serve":
        runPGServe(args[1:])
    default:
        fmt.Fprintf(os.Stderr, "unknown pg command: %s\n", args[0])
        os.Exit(1)
    }
}
```

`runPGPush`: Parse `--full` flag. Load config. Run local sync first
(`runLocalSync`). Then create `postgres.Sync`, call `EnsureSchema`,
call `Push`. Print results. This is extracted from the old
`runPGSync` in sync.go.

`runPGStatus`: Load config. Create `postgres.Sync`, call `Status`.
Print results.

`runPGServe`: Parse `--host`/`--port` flags. Load config. Create
`postgres.NewStore`. Run schema compat check. Create `server.New`
with the store. Start HTTP server. Wait for signal. This is
extracted from `runServePGRead` in main.go. Remove the loopback-only
host restriction (`isLoopbackHost` check) — per the spec, auth is
the deployer's responsibility.

- [ ] **Step 2: Add pg routing to main()**

In `main()`, add:
```go
case "pg":
    runPG(os.Args[2:])
    return
```

- [ ] **Step 3: Remove PG sync from runServe()**

Delete the PG sync startup block in `main.go` — the section starting
with `// Start PG sync if configured.` and `var pgSync *pgsync.PGSync`
through the closing brace of the `if pgCfg := ...` block. Also remove
the `pgSync` variable and its `defer pgSync.Close()`.

- [ ] **Step 4: Remove runServePGRead()**

Delete the `runServePGRead()` function from `main.go`. Its logic is
now in `pg.go:runPGServe()`.

Remove the early-return branch in `runServe()` that checks
`cfg.PGReadURL` and calls `runServePGRead`.

- [ ] **Step 5: Clean up sync.go**

Remove `PG` and `PGStatus` fields from `SyncConfig`. Remove `--pg`
and `--pg-status` flag registration from `parseSyncFlags()`. Remove
the `runPGSync()` function. Remove PG handling from `runSync()`.

- [ ] **Step 6: Update sync_test.go**

Remove tests for PG-related sync flags.

- [ ] **Step 7: Update printUsage()**

Replace `sync --pg` / `serve --pg-read` documentation with the new
`pg push`, `pg status`, `pg serve` subcommands.

- [ ] **Step 8: Remove pgsync and pgdb imports from main.go**

Update imports to use `"github.com/wesm/agentsview/internal/postgres"`
instead of the old package paths.

- [ ] **Step 9: Verify build**

```bash
CGO_ENABLED=1 go build -tags fts5 ./cmd/agentsview/
```

Expected: builds successfully.

- [ ] **Step 10: Run all tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./cmd/agentsview/... -v
```

Expected: all pass.

- [ ] **Step 11: Commit**

```bash
git add cmd/agentsview/
git commit -m "refactor: add pg command group, remove PG from sync/serve"
```

---

## Task 8: Delete old PG packages

**Files:**
- Delete: `internal/pgdb/` (all files)
- Delete: `internal/pgsync/` (all files)
- Delete: `internal/pgutil/` (all files)

- [ ] **Step 1: Verify no remaining imports**

```bash
rg 'internal/pgdb|internal/pgsync|internal/pgutil' -g '*.go'
```

Expected: no results.

- [ ] **Step 2: Delete directories**

```bash
git rm -r internal/pgdb/ internal/pgsync/ internal/pgutil/
```

- [ ] **Step 3: Full build and test**

```bash
CGO_ENABLED=1 go build -tags fts5 ./cmd/agentsview/
CGO_ENABLED=1 go test -tags fts5 ./... -count=1
go vet ./...
```

Expected: all pass with no references to deleted packages.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "refactor: delete old pgdb, pgsync, pgutil packages"
```

---

## Task 9: Update documentation

**Files:**
- Modify: `CLAUDE.md`
- Modify: `README.md`

- [ ] **Step 1: Update CLAUDE.md**

Update the project structure table and key files table:
- Replace `internal/pgdb/`, `internal/pgsync/`, `internal/pgutil/`
  references with `internal/postgres/`
- Update CLI documentation to show `pg push`, `pg status`, `pg serve`
- Update env var list (remove `AGENTSVIEW_PG_INTERVAL`,
  `AGENTSVIEW_PG_READ`; add `AGENTSVIEW_PG_SCHEMA`)
- Update config file references from `config.json` to `config.toml`

- [ ] **Step 2: Update README.md**

Update PG sync documentation to reflect new CLI commands and config
format.

- [ ] **Step 3: Commit**

```bash
git add CLAUDE.md README.md
git commit -m "docs: update for pg refactor and TOML config"
```

---

## Task 10: Final verification

- [ ] **Step 1: Full build**

```bash
CGO_ENABLED=1 go build -tags fts5 ./cmd/agentsview/
```

- [ ] **Step 2: Full test suite**

```bash
CGO_ENABLED=1 go test -tags fts5 ./... -count=1
```

- [ ] **Step 3: Vet and format**

```bash
go vet ./...
go fmt ./...
```

- [ ] **Step 4: Smoke test CLI commands**

```bash
./agentsview pg --help 2>&1 || true
./agentsview pg push --help 2>&1 || true
./agentsview pg serve --help 2>&1 || true
./agentsview pg status 2>&1 || true
```

Verify help text is correct and commands parse flags properly.

- [ ] **Step 5: Remove spec and plan files**

```bash
git rm docs/specs/2026-03-17-pg-sync-refactor-design.md
git rm docs/plans/2026-03-17-pg-sync-refactor.md
rmdir docs/specs docs/plans 2>/dev/null || true
```

- [ ] **Step 6: Final commit**

```bash
git add -A
git commit -m "chore: remove internal spec/plan files"
```
