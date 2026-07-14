# Preserve Legacy Archives Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Open and migrate every historically supported SQLite archive without
deleting archived rows, while preserving legacy insight dates and requesting a
restart-safe full resync.

**Architecture:** Use one legacy column-migration catalog for both schema
detection and transactional pre-initialization repair. Repair existing tables
before `schema.sql` creates indexes, migrate the historical insight date in the
same transaction, and leave modern additive migrations in their current
post-initialization path.

**Tech Stack:** Go, `database/sql`, `mattn/go-sqlite3`, SQLite schema metadata,
and `testify` assertions.

## Global Constraints

- Never delete, truncate, recreate, or replace the persistent SQLite archive.
- Preserve orphaned sessions and all other rows when source transcripts no
  longer exist.
- Keep the repair transactional and persist the pending-resync state across
  restart.
- Use representative historical table shapes and observable database behavior in
  tests; do not inspect implementation source text.
- Keep PostgreSQL and CockroachDB unchanged because this path is SQLite-only.
- Run `go fmt ./...` and `go vet ./...` before committing Go changes.
- Do not bypass repository Git hooks.

______________________________________________________________________

### Task 1: Protect representative legacy archives with a failing regression

**Files:**

- Create: `internal/db/legacy_schema_test.go`
- Modify: `internal/db/db_test.go:703`

**Interfaces:**

- Consumes: `Open(path string) (*DB, error)`, `DB.NeedsResync() bool`,
  `DB.GetMessages`, `requireSessionExists`, and `dataVersion` from
  `internal/db`.

- Produces: `TestOpenLegacySchemasPreservesArchiveAndRequestsResync`, which
  protects archive rows, complete legacy column repair, insight date
  migration, current index creation, and restart-safe resync state.

- [ ] **Step 1: Replace the synthetic dropped-column test with historical
  fixtures**

Delete `TestOpenLegacyIndexedColumn_PreservesArchiveAndRequestsResync` from
`internal/db/db_test.go`. Create `internal/db/legacy_schema_test.go` with the
following fixture shapes and behavior test:

```go
package db

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const legacyMessagesAndToolCallsSchema = `
CREATE TABLE messages (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TEXT,
    has_thinking   INTEGER NOT NULL DEFAULT 0,
    has_tool_use   INTEGER NOT NULL DEFAULT 0,
    content_length INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, ordinal)
);
CREATE TABLE tool_calls (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name  TEXT NOT NULL,
    category   TEXT NOT NULL
);`

const preParentLegacySchema = `
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    first_message TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    file_hash   TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);` + legacyMessagesAndToolCallsSchema

const singleDateLegacySchema = `
CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    first_message TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    file_hash   TEXT,
    parent_session_id TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);` + legacyMessagesAndToolCallsSchema + `
CREATE TABLE insights (
    id          INTEGER PRIMARY KEY,
    type        TEXT NOT NULL,
    date        TEXT NOT NULL,
    project     TEXT,
    agent       TEXT NOT NULL,
    model       TEXT,
    prompt      TEXT,
    content     TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);`

const legacyArchiveRows = `
INSERT INTO sessions (
    id, project, machine, agent, first_message, message_count,
    file_path, file_size, file_mtime, file_hash
) VALUES (
    'legacy-session', 'project-a', 'local', 'claude',
    'archived prompt', 1, '/archive/session.jsonl', 128, 42, 'legacy-hash'
);
INSERT INTO messages (
    id, session_id, ordinal, role, content, has_tool_use, content_length
) VALUES (
    1, 'legacy-session', 0, 'assistant', 'archived answer', 1, 15
);
INSERT INTO tool_calls (
    id, message_id, session_id, tool_name, category
) VALUES (
    1, 1, 'legacy-session', 'Read', 'Read'
);`

func TestOpenLegacySchemasPreservesArchiveAndRequestsResync(t *testing.T) {
	tests := []struct {
		name            string
		schema          string
		wantInsightDate string
	}{
		{
			name:   "pre-parent-link archive",
			schema: preParentLegacySchema,
		},
		{
			name:            "single-date insight and base tool calls",
			schema:          singleDateLegacySchema,
			wantInsightDate: "2026-02-23",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.db")
			conn, err := sql.Open("sqlite3", makeDSN(path, false))
			require.NoError(t, err)
			conn.SetMaxOpenConns(1)

			_, err = conn.Exec(tc.schema)
			require.NoError(t, err)
			_, err = conn.Exec(legacyArchiveRows)
			require.NoError(t, err)
			if tc.wantInsightDate != "" {
				_, err = conn.Exec(`
					INSERT INTO insights (
						id, type, date, project, agent, model, prompt, content
					) VALUES (
						1, 'daily', '2026-02-23', 'project-a', 'claude',
						'model-a', 'summarize', 'archived insight'
					)`)
				require.NoError(t, err)
			}
			_, err = conn.Exec(fmt.Sprintf(
				"PRAGMA user_version = %d", dataVersion,
			))
			require.NoError(t, err)
			require.NoError(t, conn.Close())

			d, err := Open(path)
			require.NoError(t, err)
			require.True(t, d.NeedsResync())

			session := requireSessionExists(t, d, "legacy-session")
			assert.Equal(t, "project-a", session.Project)
			require.NotNil(t, session.FirstMessage)
			assert.Equal(t, "archived prompt", *session.FirstMessage)

			messages, err := d.GetMessages(
				context.Background(), "legacy-session", 0, 10, true,
			)
			require.NoError(t, err)
			require.Len(t, messages, 1)
			require.Len(t, messages[0].ToolCalls, 1)
			assert.Equal(t, "Read", messages[0].ToolCalls[0].ToolName)

			if tc.wantInsightDate != "" {
				var dateFrom, dateTo string
				err = d.getReader().QueryRow(`
					SELECT date_from, date_to FROM insights WHERE id = 1
				`).Scan(&dateFrom, &dateTo)
				require.NoError(t, err)
				assert.Equal(t, tc.wantInsightDate, dateFrom)
				assert.Equal(t, tc.wantInsightDate, dateTo)
			}

			requireLegacyRepairIndexes(t, d)
			require.NoError(t, d.Close())

			reopened, err := Open(path)
			require.NoError(t, err)
			defer reopened.Close()
			require.True(t, reopened.NeedsResync())
		})
	}
}

func requireLegacyRepairIndexes(t *testing.T, d *DB) {
	t.Helper()
	for _, name := range []string{
		"idx_sessions_parent",
		"idx_sessions_user_message_count",
		"idx_tool_calls_skill",
		"idx_tool_calls_subagent",
		"idx_insights_lookup",
	} {
		var count int
		err := d.getReader().QueryRow(`
			SELECT count(*) FROM sqlite_master
			WHERE type = 'index' AND name = ?
		`, name).Scan(&count)
		require.NoError(t, err)
		assert.Equal(t, 1, count, "index %s", name)
	}
}
```

- [ ] **Step 2: Run the regression and record the existing compile failure**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run '^TestOpenLegacySchemasPreservesArchiveAndRequestsResync$' -count=1
```

Expected: build failure at `openAndInit` because `writer` is `*sql.DB` and
`repairLegacySchemaBeforeInit` requires `*writerHandle`.

### Task 2: Repair the complete historical schema transactionally

**Files:**

- Modify: `internal/db/db.go:1524-1604`
- Modify: `internal/db/db.go:2022-2058`
- Modify: `internal/db/db.go:2919-2933`
- Test: `internal/db/legacy_schema_test.go`

**Interfaces:**

- Consumes: `schemaColumnMigration`, `applyColumnMigrations`, `dataVersion`, and
  the guarded `DB.getWriter()` facade.

- Produces: `legacySchemaColumnMigrations() []schemaColumnMigration`, shared by
  `needsSchemaRepair` and `repairLegacySchemaBeforeInit`, plus
  `backfillLegacyInsightDates(*sql.Tx) error`.

- [ ] **Step 1: Fix the writer-handle type mismatch only**

In `openAndInit`, change the repair invocation to use the already initialized
guarded facade:

```go
if schemaRepairNeeded {
	db.mu.Lock()
	err = repairLegacySchemaBeforeInit(db.getWriter())
	db.mu.Unlock()
	if err != nil {
		db.Close()
		return nil, fmt.Errorf(
			"repairing legacy schema before initialization: %w", err,
		)
	}
}
```

- [ ] **Step 2: Re-run the regression to expose the behavioral failure**

Run the focused command from Task 1 again.

Expected: the test builds, then `Open` fails while initializing `schema.sql`
because the real legacy `tool_calls` table still lacks `skill_name` (and the
insights table still lacks `date_to`).

- [ ] **Step 3: Replace sentinel-only detection with a complete shared catalog**

Replace `preInitSchemaColumnMigrations` with:

```go
func legacySchemaColumnMigrations() []schemaColumnMigration {
	return []schemaColumnMigration{
		{
			"sessions", "parent_session_id",
			"ALTER TABLE sessions ADD COLUMN parent_session_id TEXT",
		},
		{
			"insights", "date_from",
			"ALTER TABLE insights ADD COLUMN date_from TEXT NOT NULL DEFAULT ''",
		},
		{
			"insights", "date_to",
			"ALTER TABLE insights ADD COLUMN date_to TEXT NOT NULL DEFAULT ''",
		},
		{
			"tool_calls", "tool_use_id",
			"ALTER TABLE tool_calls ADD COLUMN tool_use_id TEXT",
		},
		{
			"tool_calls", "input_json",
			"ALTER TABLE tool_calls ADD COLUMN input_json TEXT",
		},
		{
			"tool_calls", "skill_name",
			"ALTER TABLE tool_calls ADD COLUMN skill_name TEXT",
		},
		{
			"tool_calls", "result_content_length",
			"ALTER TABLE tool_calls ADD COLUMN result_content_length INTEGER",
		},
		{
			"sessions", "user_message_count",
			"ALTER TABLE sessions ADD COLUMN user_message_count INTEGER NOT NULL DEFAULT 0",
		},
		{
			"sessions", "relationship_type",
			"ALTER TABLE sessions ADD COLUMN relationship_type TEXT NOT NULL DEFAULT ''",
		},
		{
			"tool_calls", "subagent_session_id",
			"ALTER TABLE tool_calls ADD COLUMN subagent_session_id TEXT",
		},
	}
}
```

Change `needsSchemaRepair` to iterate that catalog rather than a separate probe
slice:

```go
func needsSchemaRepair(conn *sql.DB) (bool, error) {
	for _, migration := range legacySchemaColumnMigrations() {
		var count int
		err := conn.QueryRow(fmt.Sprintf(
			"SELECT count(*) FROM pragma_table_info('%s')"+
				" WHERE name = '%s'",
			migration.table, migration.column,
		)).Scan(&count)
		if err != nil {
			return false, fmt.Errorf(
				"probing schema (%s.%s): %w",
				migration.table, migration.column, err,
			)
		}
		if count == 0 {
			return true, nil
		}
	}
	return false, nil
}
```

- [ ] **Step 4: Backfill single-date insights inside the repair transaction**

Add this helper next to `repairLegacySchemaBeforeInit`:

```go
func backfillLegacyInsightDates(tx *sql.Tx) error {
	var legacyDateCount int
	if err := tx.QueryRow(
		`SELECT count(*) FROM pragma_table_info('insights')
		 WHERE name = 'date'`,
	).Scan(&legacyDateCount); err != nil {
		return fmt.Errorf("probing legacy insight date: %w", err)
	}
	if legacyDateCount == 0 {
		return nil
	}
	if _, err := tx.Exec(`
		UPDATE insights
		SET date_from = CASE
				WHEN date_from = '' THEN COALESCE(date, '')
				ELSE date_from
			END,
			date_to = CASE
				WHEN date_to = '' THEN COALESCE(date, '')
				ELSE date_to
			END
		WHERE date_from = '' OR date_to = ''
	`); err != nil {
		return fmt.Errorf("backfilling legacy insight dates: %w", err)
	}
	return nil
}
```

Apply the shared catalog and invoke the helper before reading `user_version`:

```go
if err := applyColumnMigrations(
	legacySchemaColumnMigrations(),
	func(query string, args ...any) rowScanner {
		return tx.QueryRow(query, args...)
	},
	tx.Exec,
); err != nil {
	return err
}
if err := backfillLegacyInsightDates(tx); err != nil {
	return err
}
```

- [ ] **Step 5: Run focused and package-level tests green**

Run:

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db \
  -run '^TestOpenLegacySchemasPreservesArchiveAndRequestsResync$' -count=1
CGO_ENABLED=1 go test -tags fts5 ./internal/db -count=1
```

Expected: both commands exit 0. The focused test proves row preservation, date
migration, required indexes, and pending resync after restart.

- [ ] **Step 6: Format and vet before committing**

Run:

```bash
go fmt ./...
go vet ./...
```

Expected: both commands exit 0 and `git diff --check` reports no whitespace
errors.

### Task 3: Reproduce the affected CI surfaces and review the final patch

**Files:**

- Verify: `internal/db/db.go`
- Verify: `internal/db/legacy_schema_test.go`
- Verify: `docs/superpowers/specs/2026-07-14-preserve-legacy-archives-design.md`
- Verify: `docs/superpowers/plans/2026-07-14-preserve-legacy-archives.md`

**Interfaces:**

- Consumes: the completed migration and regression test from Tasks 1-2.

- Produces: fresh build, test, lint, benchmark, and review evidence plus one
  focused commit.

- [ ] **Step 1: Run the full Go and lint gates**

Run:

```bash
make test
make vet
make lint-ci
```

Expected: all commands exit 0.

- [ ] **Step 2: Run the other CI jobs that failed only because Go did not
  compile**

Run:

```bash
make bench-gate
CGO_ENABLED=1 go build -tags "fts5,kit_posthog_disabled" \
  -o /tmp/agentsview-pr1143 ./cmd/agentsview
CGO_ENABLED=1 go build -tags "fts5,kit_posthog_disabled" \
  -o /tmp/testfixture-pr1143 ./cmd/testfixture
```

If an isolated PostgreSQL test service is available, also run:

```bash
make test-postgres
```

Expected: the benchmark and both e2e prerequisite builds exit 0. PostgreSQL
integration tests exit 0 when the dedicated test container is available.

- [ ] **Step 3: Review and scrub the complete public diff**

Run:

```bash
git status --short
git diff --stat HEAD
git diff HEAD
git diff --check
```

Verify the diff contains no database deletion path, no unrelated refactor, no
private paths or identities, and no generated artifacts.

- [ ] **Step 4: Commit the focused repair**

Stage only the three Go files and the approved design/plan documents, then
commit with:

```text
fix(db): migrate complete legacy archive schemas

Legacy archives can contain sessions and insights whose source files no longer
exist, so startup must repair their historical table shapes without recreating
the database. Keep schema detection and repair aligned, preserve single-date
insights, and retain a restart-safe resync marker.

VALID (fixed): #1 -- complete historical schema repair and representative
legacy archive coverage.
```

Do not amend the contributor's existing commit and do not bypass Git hooks.
