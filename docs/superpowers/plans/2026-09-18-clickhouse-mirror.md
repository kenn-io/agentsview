# ClickHouse Mirror Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Push the SQLite archive into ClickHouse and serve the read-only HTTP
API and web UI from it with the PostgreSQL operator story.

**Architecture:** A new `internal/clickhouse` package holds a push-only `Sync`
(DuckDB write model: fingerprint on the session row, cursor in the mirror's
`sync_metadata`, whole-session replace ordered insert, version-bounded delete,
session row last) and a read-only `Store` implementing `db.Store`. The shared
`internal/db` filter builder gains a ClickHouse dialect. Config, CLI, daemon
push route, watcher, and service install mirror the `pg` command tree.

**Tech Stack:** Go 1.27, `github.com/ClickHouse/clickhouse-go/v2` v2.48.0
through `database/sql`, `ReplacingMergeTree`, testcontainers-go (already a
dependency), ClickHouse server image `clickhouse/clickhouse-server:25.8`.

**Spec:** `docs/superpowers/specs/2026-09-18-clickhouse-mirror-design.md`

## Global Constraints

- SQLite is the archive. No task deletes, drops, truncates, or recreates it.
- Every ClickHouse table is `ReplacingMergeTree(push_version)`; every connection
  sets `final = 1`.
- Push order per batch: dependents insert, dependents
  `DELETE ... WHERE session_id IN (...) AND push_version < v`, session rows.
- Existing dialects render byte-identical SQL after the dialect hooks land.
- Read the routed guides before editing: `docs/agents/storage.md`,
  `docs/agents/testing.md`, `docs/agents/build.md`.
- Tests use testify (`require` for setup, `assert` for independent checks),
  `dbtest.OpenTestDB`, `t.TempDir()`. Integration tests carry
  `//go:build chtest`.
- `go fmt ./...` and `go vet ./...` before each commit. Run
  `go test ./internal/db ./internal/config ./cmd/agentsview -run <focused>`
  for unit work and `make test-clickhouse` for integration work.
- Keep private hostnames, identities, and absolute user paths out of code,
  tests, docs, and commits.
- Commit after every task with a conventional subject; no attribution lines.

______________________________________________________________________

## File map

| Path                                                                                                                                                                  | Responsibility                                                                                             |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| `internal/db/query_dialect.go`                                                                                                                                        | `ClickHouseQueryDialect()`, hooks `recursiveUnion`, `starredPredicate`, `orphanPredicate`                  |
| `internal/db/query_dialect_test.go`                                                                                                                                   | Rendering assertions for the new dialect and unchanged existing dialects                                   |
| `internal/clickhouse/connect.go`                                                                                                                                      | `Target`, `Open`, `OpenForAdmin`, `CheckTransportSecurity`, `RedactDSN`, `TargetFingerprint`               |
| `internal/clickhouse/schema.go`                                                                                                                                       | `SchemaVersion`, table specs, `EnsureSchema`, `CheckSchemaCompat`, `CheckDataVersionCompat`, metadata keys |
| `internal/clickhouse/sync.go`                                                                                                                                         | `Sync`, `SyncOptions`, `New`, `PushResult`, `PushProgress`, `Push`, `PushWithOptions`, `Status`, `Close`   |
| `internal/clickhouse/push.go`                                                                                                                                         | Candidate selection, fingerprints, batch writer, deletions, metadata writes                                |
| `internal/clickhouse/push_fingerprint.go`                                                                                                                             | `sessionFingerprintFields`, `sessionFingerprints`                                                          |
| `internal/clickhouse/store.go`                                                                                                                                        | `Store`, `NewStore`, `NewStoreFromDB`, query wrappers, cursors, capability flags, stubs                    |
| `internal/clickhouse/sessions.go`                                                                                                                                     | Session column list, scanning, list/sidebar/get/find/children/trash                                        |
| `internal/clickhouse/messages.go`                                                                                                                                     | Messages, windows, tool call attachment, resume counts, activity, timing                                   |
| `internal/clickhouse/search.go`                                                                                                                                       | `Search`, `SearchSession`, `SearchContent` (substring, regex in Go)                                        |
| `internal/clickhouse/secrets.go`                                                                                                                                      | Secret finding list and source                                                                             |
| `internal/clickhouse/metadata.go`                                                                                                                                     | Stats, projects, agents, machines, branches, machine labels and aliases, starred IDs, pinned lists         |
| `internal/clickhouse/analytics.go`                                                                                                                                    | 13 analytics methods and trends                                                                            |
| `internal/clickhouse/usage.go`                                                                                                                                        | Usage methods and pricing resolver                                                                         |
| `internal/clickhouse/pricing_push.go`                                                                                                                                 | Pricing and cursor usage event sync                                                                        |
| `internal/clickhouse/activityreport.go`                                                                                                                               | Activity report plus artifact, probe, token extensions; recent edits; unit range                           |
| `internal/clickhouse/project_identity.go`                                                                                                                             | Identity observations, snapshots, worktree mappings push and reads; inventory, rules, candidates           |
| `internal/clickhouse/*_chtest_test.go`                                                                                                                                | Integration tests (`//go:build chtest`)                                                                    |
| `internal/clickhouse/chtest/chtest.go`                                                                                                                                | Test helper: container or `TEST_CLICKHOUSE_URL`, fresh database per test                                   |
| `internal/backendcontract/contract.go`                                                                                                                                | Add `*clickhouse.Store` assertion                                                                          |
| `internal/config/config.go`                                                                                                                                           | Generic named-target parsing, `ClickHouseConfig`, env, resolve, `LoadClickHouseServePFlags`                |
| `cmd/agentsview/clickhouse.go`                                                                                                                                        | `ClickHousePushConfig`, target selection, push, status, serve                                              |
| `cmd/agentsview/clickhouse_watch.go`                                                                                                                                  | Watch runner and pusher                                                                                    |
| `cmd/agentsview/cli.go`                                                                                                                                               | `newClickHouseCommand` tree registration                                                                   |
| `cmd/agentsview/archive_write_backend.go`                                                                                                                             | `ClickHousePush`, `ClickHousePushWatch` on both backends                                                   |
| `cmd/agentsview/pg_service*.go`                                                                                                                                       | Service kind parameter (label, args, log name)                                                             |
| `internal/server/huma_routes_push.go`                                                                                                                                 | `/api/v1/push/clickhouse` route                                                                            |
| `internal/apiclient/*`, `frontend/src/lib/api/*`                                                                                                                      | Regenerated clients                                                                                        |
| `Makefile`, `.github/workflows/ci.yml`, `docker-compose.test.yml`                                                                                                     | `test-clickhouse`, CI service container                                                                    |
| `docs/clickhouse-sync.md`, `docs/zensical.toml`, `docs/changelog.md`, `docs/agents/storage.md`, `docs/configuration.md`, `docs/commands.md`, `AGENTS.md`, `README.md` | Operator and maintainer docs                                                                               |

______________________________________________________________________

### Task 1: ClickHouse query dialect

**Files:**

- Modify: `internal/db/query_dialect.go`
- Test: `internal/db/query_dialect_test.go`

**Interfaces:**

- Produces: `func ClickHouseQueryDialect() QueryDialect`; unexported hooks
  `recursiveUnion string`, `starredPredicate func(idExpr string) string`,
  `orphanPredicate func(sessionAlias, parentAlias string) string` on
  `QueryDialect`.

- [ ] Add the three hook fields. `buildSessionFilterWithBuilder` uses
  `b.dialect.recursiveUnionSQL()` (returns `"UNION"` when empty) in place of
  the literal `" UNION "`. `sessionFilterPredicates` calls
  `b.dialect.starredPredicateSQL(q("id"))`, which defaults to the current
  `EXISTS (SELECT 1 FROM starred_sessions ss WHERE ss.session_id = <id>)`.
  `BuildCanonicalRootWhere` calls
  `dialect.orphanPredicateSQL(sessionAlias, "parent")`, which defaults to
  `SidebarOrphanPredicate`.

- [ ] Add `ClickHouseQueryDialect()`:

```go
func ClickHouseQueryDialect() QueryDialect {
	return QueryDialect{
		name:             "clickhouse",
		placeholderStyle: placeholderQuestion,
		trueLiteral:      "true",
		falseLiteral:     "false",
		dateStartExpr: func(q func(string) string) string {
			return "COALESCE(" + q("started_at") + ", " + q("created_at") + ")"
		},
		dateEndExpr: func(q func(string) string) string {
			return "COALESCE(" + q("ended_at") + ", " + q("last_message_at") +
				", " + q("started_at") + ", " + q("created_at") + ")"
		},
		dateParam:          clickhouseTimestampParam,
		activityParam:      clickhouseTimestampParam,
		cursorActivityExpr: "COALESCE(ended_at, started_at, created_at)",
		cursorParam:        clickhouseTimestampParam,
		castCursor:         clickhouseCastCursor,
		terminationExpr:    "COALESCE(ended_at, started_at, created_at)",
		terminationKind:    timestampCast,
		caseInsensitiveLike: "ILIKE",
		regexPredicate: func(col, ph string) string {
			return "match(" + col + ", concat('(?i)', " + ph + "))"
		},
		sidebarChildRelationships:   []string{"subagent", "fork"},
		canonicalChildRelationships: []string{"subagent", "fork", "continuation"},
		nullsLast:                   true,
		recursiveUnion:              "UNION ALL",
		starredPredicate: func(id string) string {
			return id + " IN (SELECT session_id FROM starred_sessions)"
		},
		orphanPredicate: func(sessionAlias, _ string) string {
			return sessionAlias + ".parent_session_id NOT IN (SELECT id FROM sessions)"
		},
	}
}

func clickhouseTimestampParam(ph string) string {
	return "parseDateTime64BestEffort(" + ph + ", 6, 'UTC')"
}
```

`clickhouseCastCursor` maps `kindTimestamp` to `clickhouseTimestampParam`,
`kindInt` to `toInt64(ph)`, `kindReal` to `toFloat64(ph)`, else `ph`.

- [ ] Tests: render `BuildSessionFilterSQL` for SQLite, PostgreSQL, and DuckDB
  with `Starred: true`, `IncludeChildren: true`, and `IncludeOrphans: true`
  and assert the strings equal the pre-change output captured in the test
  (copy the current output into the test before editing). Render the
  ClickHouse dialect and assert `UNION ALL`, the
  `IN (SELECT session_id FROM starred_sessions)` predicate, the
  `NOT IN (SELECT id FROM sessions)` orphan predicate, `last_message_at` in
  the date-end expression, and no `ESCAPE`. Render a two-key cursor predicate
  (`recent`, `messages`) and assert the `parseDateTime64BestEffort` and
  `toInt64` casts.
- [ ] Run `go test ./internal/db -run 'Dialect|SessionFilter|Cursor'`, commit
  `feat(db): add a ClickHouse query dialect`.

______________________________________________________________________

### Task 2: Connection, target security, and schema

**Files:**

- Create: `internal/clickhouse/connect.go`, `internal/clickhouse/schema.go`,
  `internal/clickhouse/connect_test.go`, `internal/clickhouse/schema_test.go`
- Modify: `go.mod`, `go.sum` (driver already added)

**Interfaces:**

- Produces:

```go
type Target struct {
	URL      string // DSN with optional path database
	Database string // overrides the DSN path; default "agentsview"
}
func CheckTransportSecurity(dsn string, allowInsecure bool) error
func RedactDSN(dsn string) string // host:port only
func Open(ctx context.Context, t Target) (*sql.DB, error)          // Auth.Database = t.Database, Settings final=1
func OpenForAdmin(ctx context.Context, t Target) (*sql.DB, error)  // DSN database, used only for CREATE DATABASE
func TargetFingerprint(t Target) string // "v1:" + hex(sha256(host list, user, database))

const SchemaVersion = 1
func EnsureSchema(ctx context.Context, t Target) error
func EnsureSchemaOn(ctx context.Context, conn *sql.DB) error // tables + columns + version row
func CheckSchemaCompat(ctx context.Context, conn *sql.DB) error
func CheckDataVersionCompat(ctx context.Context, conn *sql.DB) error
func IsPermissionError(err error) bool // ClickHouse code 497 ACCESS_DENIED, 495 NOT_ENOUGH_PRIVILEGES
```

- [ ] `CheckTransportSecurity`: `clickhouse.ParseDSN`; loopback hosts pass;
  otherwise require `Protocol == HTTP && scheme https` or native with
  `TLS != nil`. Error text names the redacted host and the fix (`secure=true`
  or `https://`, or `allow_insecure = true`).
- [ ] `Open`: parse DSN, set `opt.Auth.Database = t.Database`, `opt.Settings`
  `final: 1`, `MaxOpenConns 5`, `ConnMaxLifetime 30m`, ping with a 10 s
  timeout, close on failure.
- [ ] Schema: a `tableSpec{name, orderBy string; columns []columnSpec}` list
  drives
  `CREATE TABLE IF NOT EXISTS <name> (<cols>) ENGINE = ReplacingMergeTree(push_version) ORDER BY (<orderBy>)`
  and per-column `ALTER TABLE <name> ADD COLUMN IF NOT EXISTS <col> <type>`.
  Tables and columns follow the DuckDB schema (`internal/duckdb/schema.go`)
  with these changes: `push_version UInt64` everywhere; `sessions` adds
  `last_message_at Nullable(DateTime64(6, 'UTC'))`,
  `agentsview_push_fingerprint String`, `source_archive_id String`;
  `tool_calls` adds `message_ordinal Int64`; timestamps are
  `DateTime64(6, 'UTC')` or `Nullable(...)`; booleans `Bool`; integers
  `Int64`; floats `Float64`; text `String` or `Nullable(String)` for the
  session pointer fields; `token_usage String`;
  `genai_pricing.data_json String`.
- [ ] `sync_metadata` keys: `agentsview_schema_version`,
  `agentsview_source_data_version`, and per-archive keys built by
  `archiveMetadataKey(base, archiveID string) string` returning
  `base + ":" + archiveID` for `agentsview_last_push_cutoff`,
  `agentsview_last_push_at`, `agentsview_last_push_machine`,
  `agentsview_push_scope`, `agentsview_deletion_revision`,
  `agentsview_identity_revision`, `agentsview_mapping_revision`,
  `agentsview_curation_fingerprint`, `agentsview_cursor_usage_max_id`. Machine
  labels and aliases use `db.MachineLabelKeyPrefix` and
  `db.MachineAliasKeyPrefix` like DuckDB.
- [ ] `CheckSchemaCompat` reads
  `system.columns WHERE database = currentDatabase()` once and reports missing
  tables or columns by name.
- [ ] Unit tests: table-driven `CheckTransportSecurity` (loopback plain ok,
  remote plain rejected, remote `secure=true` ok, `https` ok, `http` remote
  rejected, allowInsecure passes); DDL rendering contains
  `ReplacingMergeTree(push_version)` and the `ORDER BY` for every table; every
  `db.Session` field mirrored by the session insert has a column (reflection
  test shared with Task 3).
- [ ] Commit `feat(clickhouse): add connection and schema management`.

______________________________________________________________________

### Task 3: Push

**Files:**

- Create: `internal/clickhouse/sync.go`, `internal/clickhouse/push.go`,
  `internal/clickhouse/push_fingerprint.go`,
  `internal/clickhouse/chtest/chtest.go`,
  `internal/clickhouse/push_chtest_test.go`,
  `internal/clickhouse/push_fingerprint_test.go`

**Interfaces:**

- Consumes: Task 2 `Open`, `EnsureSchemaOn`, metadata keys; local `*db.DB`
  methods `GetArchiveID`, `GetDatabaseID`,
  `ListSessionsForMirrorWindow(ctx, cutoff, nil, nil)`, `GetAllMessages`,
  `GetUsageEvents`, `SessionSecretFindings`, `ListPinnedMessages`,
  `ToolCallFingerprint`, `UsageEventFingerprints`,
  `SessionDeletionPublicationRevision`, `LoadSessionDeletionDelta`,
  `ListStarredSessionIDsForScope`, `GetMachineLabels`, `GetMachineAliases`.
- Produces:

```go
type SyncOptions struct{ Projects, ExcludeProjects []string }
type PushOptions struct{ Full bool }
type PushResult struct {
	SessionsPushed, MessagesPushed, SkippedUnchanged, DeletedStale, Errors int
	Duration time.Duration
}
type PushProgress struct{ Phase string; SessionsDone, SessionsTotal, MessagesDone, Errors int }
type SyncStatus struct {
	Machine, LastPushAt, LastPushMachine, Scope string
	SchemaVersion, DataVersion, Sessions, Messages int
}
func New(ctx context.Context, t Target, local *db.DB, machine string, opts SyncOptions) (*Sync, error)
func (s *Sync) EnsureSchema(ctx context.Context) error
func (s *Sync) Push(ctx context.Context, full bool, onProgress func(PushProgress)) (PushResult, error)
func (s *Sync) PushWithOptions(ctx context.Context, opts PushOptions, onProgress func(PushProgress)) (PushResult, error)
func (s *Sync) Status(ctx context.Context) (SyncStatus, error)
func (s *Sync) Close() error
func ReadStatus(ctx context.Context, t Target, machine, archiveID string) (SyncStatus, error)
```

- [ ] `PushWithOptions` flow: `CheckDataVersionCompat` on the mirror; read
  per-archive metadata (cutoff, scope, deletion revision); `full` when
  `opts.Full`, scope string differs, or no cutoff;
  `pushVersion := uint64(time.Now().UnixNano())`;
  `cutoff := time.Now().UTC()`; apply the deletion delta
  `(storedRevision, localRevision]`; sync machine labels and aliases into
  `sync_metadata`; list candidates (`ListSessionsForMirrorWindow` with the
  stored cutoff, or `""` when full), partition by scope in Go, delete
  out-of-scope resident sessions; compute fingerprints; read mirror
  fingerprints in `IN` batches of 500; when `full`, treat every candidate as
  changed; push changed sessions in batches of 100; when `full`, delete this
  archive's mirror sessions absent locally
  (`SELECT id FROM sessions WHERE source_archive_id = ?` minus local IDs);
  refresh curation (stars and pins for resident sessions gated by a fingerprint
  like DuckDB `refreshCurationIfChanged`); write metadata (cutoff, at,
  machine, scope, deletion revision, data version, schema version) only when
  `Errors == 0`.
- [ ] Batch writer
  `pushSessionBatch(ctx, batch []db.Session, fps map[string]string, v uint64)`:
  load messages, usage events, findings, pins per session; one `sql.Tx` per
  table for the inserts (`Begin`, `Prepare`, `Exec` per row, `Commit`); then
  one `DELETE FROM <t> WHERE session_id IN (...) AND push_version < ?` per
  dependent table; then the session rows in one transaction. Compute
  `last_message_at` as the max parsed message timestamp. On any error, retry
  each session alone and count `Errors` per failing session, matching DuckDB
  `pushSessionBatchWith`.
- [ ] Test seam: `type pushHooks struct{ beforeSessionRows func() error }` field
  on `Sync`, nil in production, used by the injected-failure test.
- [ ] Fingerprint: `sessionFingerprints` mirrors DuckDB
  (`JSON of SessionFields, Messages, Usage, ToolCalls, SecretFindings, Pins`,
  SHA-256 hex); `sessionFingerprintFields` lists every mirrored session
  scalar. The reflection test asserts every column name in the session insert
  appears in a `fingerprintColumns` list and vice versa.
- [ ] `chtest.Open(t) (Target, *sql.DB)`: `TEST_CLICKHOUSE_URL` when set,
  otherwise start `clickhouse/clickhouse-server:25.8` with
  `testcontainers.GenericContainer` (env `CLICKHOUSE_SKIP_USER_SETUP=1`, wait
  for HTTP `/ping` on 8123), one container per test binary via `sync.Once`,
  terminate in `TestMain`; each test gets a fresh database
  `agentsview_test_<random>` dropped in `t.Cleanup`.
- [ ] Integration tests (`//go:build chtest`):
    - seed two sessions with messages, a tool call with a result event, a usage
      event, a secret finding, a star and a pin through
      `local.WriteSessionBatchAtomic`; push; assert row counts per table and the
      stored fingerprint per session.
    - push again with no change: `SessionsPushed == 0`, `SkippedUnchanged == 2`.
    - append a message locally (bump `local_modified_at`), push: the session's
      message count in ClickHouse equals the local count and no row has
      `push_version` below the latest push for that session.
    - hard-delete a session locally (`DeleteSessionIfTrashed` after
      `SoftDeleteSession`), push: its rows are gone from every table.
    - injected failure before session rows: push returns `Errors == 1`, cutoff
      unchanged; a second push without the hook repairs it.
    - project-scoped push excludes the other project; moving a session's project
      out of scope removes its rows on the next push.
- [ ] Commit `feat(clickhouse): push the SQLite archive into ClickHouse`.

______________________________________________________________________

### Task 4: Store core and serve-path test

**Files:**

- Create: `internal/clickhouse/store.go`, `sessions.go`, `messages.go`,
  `search.go`, `secrets.go`, `metadata.go`, `stubs.go`,
  `store_chtest_test.go`, `serve_chtest_test.go`
- Modify: `internal/backendcontract/contract.go`

**Interfaces:**

- Produces:

```go
func NewStore(ctx context.Context, t Target) (*Store, error)
func NewStoreFromDB(conn *sql.DB) *Store
func (s *Store) DB() *sql.DB
func (s *Store) Close() error
func (s *Store) SetCustomPricing(map[string]config.CustomModelRate)
```

- [ ] `Store` holds `conn *sql.DB`, `cursorMu`/`cursorSecret`, `customPricing`.
  `queryContext`/`queryRowContext` wrap `conn`. Cursor encode/decode copy the
  DuckDB HMAC implementation.
- [ ] Port `internal/duckdb/store.go` sessions section, `messages.go`,
  `secrets.go`, `curation.go` read side, `machine_labels.go`, and the metadata
  reads (`GetStats`, `GetProjects`, `GetActiveProjectLabels`, `GetAgents`,
  `GetMachines`, `GetBranches`) to ClickHouse SQL. Differences to apply: no
  `FINAL` in text (connection setting); `strpos` becomes `position`; `ILIKE`
  escapes without `ESCAPE`; `COUNT(*)` scans into `int`; timestamps scan into
  `*time.Time` and format with `formatTime`; the session column list includes
  `last_message_at` only where a query needs it. Tool calls join on
  `message_ordinal`, not `message_id`.
- [ ] `Search`, `SearchSession`, `SearchContent`: substring via `ILIKE` across
  messages, tool input, tool result content, tool result events; regex via a
  literal-prefix `ILIKE` prefilter and Go `regexp`, copying the PostgreSQL
  `searchContentRegexPG` shape; `fts` mode maps to substring over messages;
  `semantic`/`hybrid` validate then return `db.ErrSemanticUnavailable`.
- [ ] `stubs.go`: writes return `db.ErrReadOnly`; insights reads return empty;
  recall methods return `db.ErrReadOnly`. Analytics, usage, activity, recent
  edits, inventory, rules, candidates, identity methods are added in Tasks 7
  to 10; until then they return `errNotImplemented` (package error
  `clickhouse: <method> is not implemented yet`) so the compile-time contract
  holds and the gap is loud.
- [ ] Add `_ db.Store = (*clickhouse.Store)(nil)` in `backendcontract`.
- [ ] Store contract test (`chtest`): one fixture, subtests asserting absolute
  expectations: `ListSessions` default sort order and cursor paging with
  `Limit: 1`; `GetSidebarSessionIndex` includes the child under its parent;
  `GetSession` hides a soft-deleted session while `GetSessionFull` returns it;
  `FindSessionIDsByPartial`; `GetMessages` ascending and descending, window
  around an anchor, tool call with its result event and restored
  `ResultContent`; `GetResumeModelCounts`; `GetSessionTiming` non-nil;
  `Search` finds the seeded phrase and `SearchContent` reports the tool result
  location; `ListSecretFindings` returns the seeded finding and
  `SecretFindingSource` returns its text; `GetSessionVersion` changes after a
  re-push with a new message; `GetStats` counts; `GetProjects`, `GetAgents`,
  `GetMachines`, `GetBranches`, `GetMachineLabels`; `ListStarredSessionIDs`
  and `ListPinnedMessages`; every write returns `db.ErrReadOnly`.
- [ ] Serve-path test (`chtest`):
  `server.New(config.Config{...}, store, nil, server.WithVersion(server.VersionInfo{ReadOnly: true}))`,
  `httptest` requests to `/api/v1/sessions`, `/api/v1/sessions/{id}`,
  `/api/v1/sessions/{id}/messages`, `/api/v1/search?q=`, and
  `/api/v1/settings` (asserts `read_only: true`); decode JSON and assert the
  seeded IDs and content.
- [ ] Commit
  `feat(clickhouse): serve sessions, messages, and search from ClickHouse`.

______________________________________________________________________

### Task 5: Config

**Files:**

- Modify: `internal/config/config.go`
- Test: `internal/config/config_clickhouse_test.go`

**Interfaces:**

- Produces:

```go
type ClickHouseConfig struct {
	URL             string   `toml:"url" json:"url"`
	Database        string   `toml:"database" json:"database"`
	MachineName     string   `toml:"machine_name" json:"machine_name"`
	AllowInsecure   bool     `toml:"allow_insecure" json:"allow_insecure"`
	Projects        []string `toml:"projects" json:"projects,omitempty"`
	ExcludeProjects []string `toml:"exclude_projects" json:"exclude_projects,omitempty"`
}
type ResolvedClickHouseTarget struct{ Name string; Config ClickHouseConfig; IsDefault bool }
// Config fields
ClickHouse        ClickHouseConfig            `json:"clickhouse,omitempty" toml:"clickhouse"`
DefaultClickHouse string                      `json:"default_clickhouse,omitempty" toml:"default_clickhouse"`
ClickHouseTargets map[string]ClickHouseConfig `json:"-" toml:"-"`
func (c *Config) DefaultClickHouseTargetName() (string, error)
func (c *Config) ClickHouseTargetNames() ([]string, string, error)
func (c *Config) RawClickHouseTarget(name string) (ClickHouseConfig, error)
func (c *Config) ResolveClickHouse() (ClickHouseConfig, error)
func (c *Config) ResolveClickHouseTarget(name string) (ClickHouseConfig, error)
func (c *Config) ResolveClickHouseTargets() ([]ResolvedClickHouseTarget, error)
func LoadClickHouseServePFlags(fs *pflag.FlagSet) (Config, error)
```

- [ ] Extract the PG section parser into
  `parseNamedTargetSection[T any](section string, value any, fieldKeys map[string]struct{}, reserved func(string) bool) (T, map[string]T, error)`
  and the default-name / names / raw lookups into a small generic helper
  `namedTargets[T]` used by both PG and ClickHouse methods. Existing PG tests
  stay green unchanged.
- [ ] Parse `[clickhouse]` in `loadFile` next to `[pg]`; read
  `default_clickhouse`; env `AGENTSVIEW_CLICKHOUSE_URL`, `_DATABASE`,
  `_MACHINE` into `clickHouseEnvOverrides` applied to the default target;
  `Database` defaults to `agentsview`, `MachineName` to `InstallationID`.
- [ ] `LoadClickHouseServePFlags` calls `loadPGServeBase` like the DuckDB
  variant.
- [ ] Tests: legacy block, two named targets with `default_clickhouse`, one
  named target without default, missing default error, reserved name error,
  mixing error, env override only on default, `Database` default.
- [ ] Commit `feat(config): add ClickHouse targets`.

______________________________________________________________________

### Task 6: CLI, daemon route, watcher, service

**Files:**

- Create: `cmd/agentsview/clickhouse.go`, `cmd/agentsview/clickhouse_watch.go`,
  `cmd/agentsview/clickhouse_test.go`
- Modify: `cmd/agentsview/cli.go`, `cmd/agentsview/archive_write_backend.go`,
  `cmd/agentsview/pg_service.go`, `pg_service_manager.go`,
  `pg_service_launchd.go`, `pg_service_systemd.go`,
  `internal/server/huma_routes_push.go`, `internal/apiclient/generate.yaml`
- Regenerate: `internal/apiclient/client.gen.go`, frontend generated client

**Interfaces:**

- Produces: `newClickHouseCommand()` with `push [target]`, `status [target]`,
  `serve`, `service ...`;
  `archiveWriteBackend.ClickHousePush(ctx, target clickHouseTargetSelection, cfg ClickHousePushConfig, projects, exclude []string) (clickhouse.PushResult, error)`
  and `ClickHousePushWatch(..., debounce, interval time.Duration) error`;
  server route `POST /api/v1/push/clickhouse` reading
  `daemonPushRequest.ClickHouse *config.ClickHouseConfig`;
  `serviceKind{label, unitName, args, logName}` with `pgServiceKind` and
  `clickHouseServiceKind`.

- [ ] `clickhouse.go`: `ClickHousePushConfig` (same flags as PG minus vectors),
  `clickHouseTargetSelection`, `resolveClickHouseTargetSelections`,
  `resolveClickHousePushProjects`, `runClickHousePush`, `runClickHouseStatus`,
  `loadClickHouseServeConfig`, `prepareClickHouseServe`, `runClickHouseServe`
  (banner `agentsview %s (clickhouse read-only) at %s`, runtime record
  read-only, `serveRuntimeOptions{Mode: "clickhouse-serve"}`).

- [ ] `archive_write_backend.go`: local backend runs
  `runLocalSyncAuthoritative`, pricing refresh, `clickhouse.New`,
  `EnsureSchema`, `PushWithOptions`; daemon backend posts
  `apiclient.DaemonPushRequest{Clickhouse: &apiclient.ConfigClickHouseConfig{...}}`
  to `daemonPushClickHouse`. Watch reuses `newArchivePushLoop`,
  `startArchivePushWatcher`, the unwatched poller, and a `clickHousePusher`
  copied from `duckDBPusher` with
  `mirrorPush func(ctx, full bool) (clickhouse.PushResult, error)`.

- [ ] Server: `humaClickHousePush` copies `humaPGPush` without the vector
  source, calling `clickhouse.New` with `config.ClickHouseConfig` fields.

- [ ] Add `post-api-v1-push-clickhouse` to `internal/apiclient/generate.yaml`;
  run `cd frontend && npm ci && npm run generate:api`; commit generated files.

- [ ] Service: `serviceSpec` gains `Kind serviceKind`; launchd label and systemd
  unit name, `ProgramArguments`/`ExecStart`, and log path come from the kind.
  `buildServiceSpec(appCfg, kind)` resolves the PG or ClickHouse raw target.
  `newPGServiceCommand` and `newClickHouseServiceCommand` share
  `newServiceCommands(kind)`.

- [ ] Tests: target selection table (legacy, named default, `--all`, unknown
  name), project flag exclusivity, service render for both kinds (label, args,
  log path), route request decoding with a ClickHouse config,
  `resolveArchiveWriteBackend` fake asserting `ClickHousePush` is called with
  the resolved projects.

- [ ] Commit
  `feat(cli): add clickhouse push, status, serve, and service commands`.

______________________________________________________________________

### Task 7: Analytics

**Files:**

- Create: `internal/clickhouse/analytics.go`, `analytics_scope.go`,
  `analytics_chtest_test.go`

- [ ] Port `internal/duckdb/analytics_usage.go` analytics section and
  `analytics_scope.go` to ClickHouse SQL: `GetAnalyticsSummary`,
  `GetAnalyticsActivity`, `GetAnalyticsHeatmap`, `GetAnalyticsProjects`,
  `GetAnalyticsHourOfWeek`, `GetAnalyticsSessionShape`, `GetAnalyticsTools`,
  `GetAnalyticsSkills`, `GetAnalyticsVelocity`, `GetAnalyticsTopSessions`,
  `GetAnalyticsSignals`, `GetAnalyticsSignalSessions`, `GetTrendsTerms`. Reuse
  the shared `db.*` builders the DuckDB store calls. ClickHouse substitutions:
  `date_trunc('day', ts, tz)` becomes `toStartOfDay(ts, tz)`, week
  `toStartOfWeek(ts, 1, tz)`, month `toStartOfMonth(ts, tz)`;
  `strftime`/`EXTRACT` become `toDayOfWeek`/`toHour`; `epoch(...)` becomes
  `toUnixTimestamp64Micro`; window functions are supported; `date_diff`
  becomes `dateDiff('second', a, b)`.

- [ ] Remove the corresponding `errNotImplemented` stubs.

- [ ] Test (`chtest`): fixture with known counts; assert summary session and
  message totals, activity buckets for a day granularity, heatmap day count,
  tools list contains the seeded tool, top sessions by messages returns the
  larger session first, signals aggregate contains the seeded signal, trends
  term count for the seeded word.

- [ ] Commit `feat(clickhouse): serve analytics from ClickHouse`.

______________________________________________________________________

### Task 8: Usage and pricing

**Files:**

- Create: `internal/clickhouse/usage.go`, `pricing_push.go`,
  `usage_chtest_test.go`

- [ ] Push side: `syncModelPricing` (model_pricing, model_pricing_bands,
  genai_pricing from the local archive, copying DuckDB `push.go` pricing
  section) and `syncCursorUsageEvents` (high-water mark key, skipped on
  filtered pushes).

- [ ] Read side: port `GetSessionUsage`, `GetDailyUsage`,
  `GetTopSessionsByCost`, `GetUsageSessionCounts`,
  `GetUsageMatchingSessionCount`, and the pricing resolver loader from the
  DuckDB store; `SetCustomPricing` feeds the resolver.

- [ ] Test (`chtest`): seed pricing with `local.UpsertModelPricing`, push,
  compare `GetDailyUsage` totals and `GetSessionUsage` cost with the SQLite
  result for the same fixture and assert the absolute cost the DuckDB test
  pins for the same rates.

- [ ] Commit `feat(clickhouse): serve token usage and cost from ClickHouse`.

______________________________________________________________________

### Task 9: Activity report, recent edits, unit range, parity leg

**Files:**

- Create: `internal/clickhouse/activityreport.go`, `recentedits.go`,
  `unit_range.go`, `activityreport_chtest_test.go`

- Modify: `internal/activity/parity_pgtest_test.go`

- [ ] Port `internal/duckdb/activityreport.go`, `activityreport_probe.go`,
  `activityreport_token.go`, `recentedits.go`, `unit_range.go`.

- [ ] Add `pushParityClickHouse(t, ctx, local) *clickhouse.Store` and a third
  leg to `assertParityForCase` and `assertCandidateParity`, skipping when
  neither `TEST_CLICKHOUSE_URL` nor Docker is available; the file keeps the
  `pgtest` tag and adds `chtest`.

- [ ] Test (`chtest`): activity report for the fixture day returns the seeded
  session count; recent edits lists the seeded edited file.

- [ ] Commit `feat(clickhouse): serve activity reports and recent edits`.

______________________________________________________________________

### Task 10: Project identity, inventory, rules, candidates

**Files:**

- Create: `internal/clickhouse/project_identity.go`, `project_inventory.go`,
  `project_identity_chtest_test.go`

- [ ] Push side: `syncProjectIdentityObservations` and `syncWorktreeMappings`
  with archive-scoped revisions in `sync_metadata`, porting
  `internal/duckdb/project_identity_upsert.go` and `worktree_mappings_push.go`
  (insert new rows, version-bounded delete).

- [ ] Read side: port `project_identity.go`, `project_inventory.go`,
  `project_rules.go`, `worktree_candidates.go`.

- [ ] Test (`chtest`): inventory `TotalProjects == 2`; rules lists the seeded
  machine; candidates for the seeded project; identity map keyed by label.

- [ ] Commit
  `feat(clickhouse): serve project inventory and identity from ClickHouse`.

______________________________________________________________________

### Task 11: Build targets, CI, docs, changelog

**Files:**

- Modify: `Makefile`, `.github/workflows/ci.yml`, `docker-compose.test.yml`,
  `docs/zensical.toml`, `docs/changelog.md`, `docs/agents/storage.md`,
  `docs/configuration.md`, `docs/commands.md`, `AGENTS.md`, `README.md`

- Create: `docs/clickhouse-sync.md`

- [ ] `Makefile`: `clickhouse-up`, `clickhouse-down` (compose service
  `clickhouse` on host port 18123/19000), `test-clickhouse` (sets
  `TEST_CLICKHOUSE_URL=clickhouse://localhost:19000/default`, runs
  `go test -tags "fts5,chtest" ./internal/clickhouse/... ./internal/activity/...`),
  `test-clickhouse-ci`; help text.

- [ ] CI: a `clickhouse` service container on the integration job or a new job
  mirroring it, running `make test-clickhouse-ci` with the service URL.

- [ ] `docs/clickhouse-sync.md`: quick start (config, push, serve), commands
  table, config keys, named targets, transport security, watcher and service,
  differences from PostgreSQL, ClickHouse notes (ReplacingMergeTree, deletes
  applied at merge, `final=1`).

- [ ] Nav in `docs/zensical.toml` after DuckDB; `## Unreleased` entry in
  `docs/changelog.md` under `**New features**`; storage guide section
  "ClickHouse Mirror" (rules: never mutate SQLite, keep push order, version
  column on every table, tests under `chtest`); `AGENTS.md` task route row and
  project map line; configuration and commands docs; README backend mention.

- [ ] `mdformat --wrap 80` on changed Markdown; commit
  `docs(clickhouse): document the ClickHouse mirror and serve`.
