# Session Name Two-Field Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the single `display_name + name_source` discriminator design with two separate columns — `session_name` (parser/importer-owned) and `display_name` (user-owned) — resolving them via `COALESCE` in all read queries so the frontend receives one pre-resolved value and `name_source` disappears entirely.

**Architecture:** `session_name` is freely overwritten by every re-parse. `display_name` is touched only by `RenameSession`. All read queries return `COALESCE(display_name, session_name) AS display_name`. The `name_source` column, its CASE expression, `BackfillNameSource`, `ParsedSessionNameFields`, and the rename-clear re-parse machinery are all deleted.

**Tech Stack:** Go 1.22+, SQLite3/fts5 (CGO), PostgreSQL (optional), testify

---

## File Map

| File | Action | What changes |
|---|---|---|
| `internal/parser/types.go` | Modify | `ParsedSession.DisplayName` → `SessionName` |
| `internal/parser/claude.go` | Modify | Set `sess.SessionName` instead of `sess.DisplayName` |
| `internal/parser/claude_ai.go` | Modify | Same |
| `internal/parser/chatgpt.go` | Modify | Same |
| `internal/parser/cortex.go` | Modify | Same |
| `internal/parser/forge.go` | Modify | Same |
| `internal/parser/hermes.go` | Modify | Same |
| `internal/parser/kiro_ide.go` | Modify | Same |
| `internal/parser/piebald.go` | Modify | Same |
| `internal/db/schema.sql` | Modify | `session_name TEXT`, remove `name_source TEXT` |
| `internal/db/sessions.go` | Modify | `SessionName` field; COALESCE in `sessionBaseCols`; both cols in `sessionFullCols`; upsert SQL; scans; `RenameSession`; delete `ResetFileStateByPath` |
| `internal/db/parsedsession.go` | Modify | Replace `ParsedSessionNameFields` with `ParsedSessionName` |
| `internal/db/parsedsession_test.go` | Modify | Update tests for new helper |
| `internal/db/db.go` | Modify | `migrateColumns` entry; delete `BackfillNameSource` and its call |
| `internal/db/orphaned.go` | Modify | Remove `hasNameSource` branch; simplify `CopySessionMetadataFrom` |
| `internal/db/db_test.go` | Modify | Delete `name_source` tests; add `session_name` round-trip test |
| `internal/db/sessions_test.go` | Modify | Rewrite `TestGetSessionPopulatesNameSourceInternally` |
| `internal/sync/engine.go` | Modify | `toDBSession` uses `ParsedSessionName` → `s.SessionName` |
| `internal/server/upload.go` | Modify | `sessionBatchWriteFromParsed` sets `SessionName` |
| `internal/server/huma_routes_sessions.go` | Modify | `humaRenameSession` — remove re-parse block; `RenameSession` drop `name_source` |
| `internal/importer/importer.go` | Modify | Both converters set `SessionName` |
| `internal/postgres/schema.go` | Modify | `coreDDL`: `session_name`; `AddMissingColumns`: `session_name` entry |
| `internal/postgres/push.go` | Modify | `pushSession` INSERT; `sessionPushFingerprint` |
| `internal/postgres/sessions.go` | Modify | `pgSessionCols` COALESCE; `scanPGSession` |
| `internal/postgres/name_source_pgtest_test.go` | Modify | Rewrite for `session_name` |
| `internal/postgres/sync_test.go` | Modify | Update any remaining `name_source` references |

---

### Task 1: Rename `ParsedSession.DisplayName` → `SessionName` in the parser

**Files:**
- Modify: `internal/parser/types.go:531`
- Modify: `internal/parser/claude.go`, `claude_ai.go`, `chatgpt.go`, `cortex.go`, `forge.go`, `hermes.go`, `kiro_ide.go`, `piebald.go`
- Modify: `internal/db/parsedsession.go`

**Important:** `AgentDef.DisplayName` (types.go line 48) is the agent's human-readable label ("Claude Code", "Codex", etc.) and must NOT be renamed. Only `ParsedSession.DisplayName` changes.

- [ ] **Step 1: Rename the field in `ParsedSession`**

In `internal/parser/types.go`, change line 531:
```go
// Before:
DisplayName      string
// After:
SessionName      string
```

- [ ] **Step 2: Update each parser that sets the field**

In `internal/parser/claude.go` (around line 692):
```go
// Before:
sess.DisplayName = m.displayName
// After:
sess.SessionName = m.displayName
```

In `internal/parser/claude_ai.go` (around line 165):
```go
// Before:
DisplayName: conv.Name,
// After:
SessionName: conv.Name,
```

In `internal/parser/chatgpt.go` (around line 195):
```go
// Before:
DisplayName: conv.Title,
// After:
SessionName: conv.Title,
```

In `internal/parser/cortex.go` (around line 437):
```go
// Before:
DisplayName: displayName,
// After:
SessionName: displayName,
```

In `internal/parser/forge.go` (around line 348):
```go
// Before:
DisplayName: c.title,
// After:
SessionName: c.title,
```

In `internal/parser/hermes.go` (around line 761):
```go
// Before:
sess.DisplayName = ss.title
// After:
sess.SessionName = ss.title
```

In `internal/parser/kiro_ide.go` (around line 387):
```go
// Before:
DisplayName: title,
// After:
SessionName: title,
```

In `internal/parser/piebald.go` (around line 364):
```go
// Before:
DisplayName: c.title,
// After:
SessionName: c.title,
```

- [ ] **Step 3: Update `parsedsession.go`**

In `internal/db/parsedsession.go`, update both the function name and the field reference:
```go
package db

import "go.kenn.io/agentsview/internal/parser"

// ParsedSessionName returns the session_name value to store when upserting
// a parsed session. Returns nil when the parser did not extract a name.
func ParsedSessionName(sess parser.ParsedSession) *string {
	if sess.SessionName == "" {
		return nil
	}
	n := sess.SessionName
	return &n
}
```

- [ ] **Step 4: Verify the compiler catches all references**

```bash
CGO_ENABLED=1 go build -tags fts5 ./internal/parser/... ./internal/db/...
```

Expected: compile errors for any remaining `sess.DisplayName` or `ParsedSessionNameFields` references — fix them. Expected: PASS once all are updated.

- [ ] **Step 5: Run parser tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/parser/... -v 2>&1 | tail -10
```

Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add internal/parser/ internal/db/parsedsession.go
git commit -m "refactor(parser,db): rename ParsedSession.DisplayName→SessionName; simplify ParsedSessionName helper"
```

---

### Task 2: Update `db.Session` struct, schema, and `migrateColumns`

**Files:**
- Modify: `internal/db/schema.sql`
- Modify: `internal/db/sessions.go` (Session struct, SidebarSessionIndexRow)
- Modify: `internal/db/db.go` (`migrateColumns`, delete `BackfillNameSource` and its call)

- [ ] **Step 1: Update `schema.sql`**

In `internal/db/schema.sql`, change the sessions table definition:
```sql
-- Before:
display_name TEXT,
name_source TEXT,
-- After:
display_name TEXT,
session_name TEXT,
```

- [ ] **Step 2: Update `db.Session` struct**

In `internal/db/sessions.go` (around line 152), change:
```go
// Before:
DisplayName          *string `json:"display_name,omitempty"`
NameSource           *string `json:"-"`
// After:
DisplayName          *string `json:"display_name,omitempty"`
SessionName          *string `json:"-"`
```

In `SidebarSessionIndexRow` (around line 394), change:
```go
// Before:
DisplayName       *string `json:"display_name,omitempty"`
NameSource        *string `json:"-"`
// After:
DisplayName       *string `json:"display_name,omitempty"`
// NameSource removed — session_name not needed in sidebar rows
```

- [ ] **Step 3: Update `migrateColumns` in `db.go`**

In `internal/db/db.go`, in the `migrateColumns` function, replace the `name_source` entry:
```go
// Before:
{
    "sessions", "name_source",
    "ALTER TABLE sessions ADD COLUMN name_source TEXT",
},
// After:
{
    "sessions", "session_name",
    "ALTER TABLE sessions ADD COLUMN session_name TEXT",
},
```

- [ ] **Step 4: Delete `BackfillNameSource` and its call**

In `internal/db/db.go`:

Delete the call (around line 335):
```go
// Delete these lines:
if err := d.BackfillNameSource(); err != nil {
    return nil, fmt.Errorf("backfilling name_source: %w", err)
}
```

Delete the entire `BackfillNameSource` function (around line 775–789):
```go
// Delete this entire function:
func (db *DB) BackfillNameSource() error { ... }
```

- [ ] **Step 5: Build to confirm changes compile**

```bash
CGO_ENABLED=1 go build -tags fts5 ./internal/db/...
```

Expected: compile errors only for remaining `NameSource` or `name_source` references — those will be fixed in subsequent tasks.

- [ ] **Step 6: Commit**

```bash
git add internal/db/schema.sql internal/db/sessions.go internal/db/db.go
git commit -m "feat(db): add session_name column; replace name_source in schema and migrateColumns; delete BackfillNameSource"
```

---

### Task 3: Update `upsertSessionSQL`, `upsertSessionArgs`, and the read path column lists

**Files:**
- Modify: `internal/db/sessions.go` (upsert SQL, args, sessionBaseCols, sessionFullCols, all scans)

- [ ] **Step 1: Write a failing test for the COALESCE read behavior**

In `internal/db/sessions_test.go`, add:

```go
func TestSessionNameCOALESCEInGetSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Session with only session_name (no user rename) — GetSession should return it.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Agent Title"), MessageCount: 1,
	}), "upsert with session_name")
	s, err := d.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Agent Title", *s.DisplayName, "COALESCE returns session_name when no user rename")

	// User renames — display_name wins over session_name.
	requireNoError(t, d.RenameSession("s1", Ptr("User Title")), "rename")
	s, err = d.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "User Title", *s.DisplayName, "display_name wins over session_name")

	// Clear rename — session_name visible again.
	requireNoError(t, d.RenameSession("s1", nil), "clear rename")
	s, err = d.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "Agent Title", *s.DisplayName, "session_name restored after clearing rename")
}
```

- [ ] **Step 2: Run the test — expect FAIL**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/... -run TestSessionNameCOALESCEInGetSession -v 2>&1 | tail -10
```

Expected: compile error (SessionName not yet in upsert) or test failure.

- [ ] **Step 3: Update `sessionBaseCols`**

In `internal/db/sessions.go` (around line 31), change:
```go
// Before:
const sessionBaseCols = `id, project, machine, agent,
	first_message, display_name, name_source, started_at, ended_at,
// After:
const sessionBaseCols = `id, project, machine, agent,
	first_message, COALESCE(display_name, session_name) AS display_name, started_at, ended_at,
```

- [ ] **Step 4: Update `scanSessionRow` to remove NameSource scan**

In `internal/db/sessions.go` (around line 120), change:
```go
// Before:
&s.FirstMessage, &s.DisplayName, &s.NameSource, &s.StartedAt, &s.EndedAt,
// After:
&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
```

- [ ] **Step 5: Update `sessionFullCols` to use both raw columns**

In `internal/db/sessions.go` (around line 78), change:
```go
// Before:
const sessionFullCols = `id, project, machine, agent,
	first_message, display_name, name_source, started_at, ended_at,
// After:
const sessionFullCols = `id, project, machine, agent,
	first_message, display_name, session_name, started_at, ended_at,
```

- [ ] **Step 6: Update `sessionFullCols` scans to use SessionName**

There are two places that scan `sessionFullCols` — find them with:
```bash
grep -n "s\.NameSource\|s\.DisplayName, &s\.NameSource" internal/db/sessions.go
```

For each scan that was `&s.DisplayName, &s.NameSource`, change to `&s.DisplayName, &s.SessionName`.

The key scans are around lines 827 and 2090. Change each:
```go
// Before:
&s.FirstMessage, &s.DisplayName, &s.NameSource, &s.StartedAt, &s.EndedAt,
// After:
&s.FirstMessage, &s.DisplayName, &s.SessionName, &s.StartedAt, &s.EndedAt,
```

- [ ] **Step 7: Update `upsertSessionSQL`**

In `internal/db/sessions.go` (around line 925), replace the entire INSERT column list and ON CONFLICT SET for name-related columns:

```sql
INSERT INTO sessions (
    id, project, machine, agent, first_message, session_name,
    started_at, ended_at, message_count,
    user_message_count, parent_session_id,
    relationship_type,
    total_output_tokens, peak_context_tokens,
    has_total_output_tokens, has_peak_context_tokens,
    is_automated,
    termination_status,
    cwd, git_branch, source_session_id,
    source_version, parser_malformed_lines,
    is_truncated,
    file_path, file_size, file_mtime,
    file_inode, file_device, file_hash
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    project = excluded.project,
    machine = excluded.machine,
    agent = excluded.agent,
    first_message = excluded.first_message,
    session_name = excluded.session_name,
    -- display_name is NOT in the SET clause: only RenameSession touches it.
    started_at = excluded.started_at,
    ...
```

Note: `display_name` is removed from the INSERT column list and the ON CONFLICT SET. It defaults to NULL on INSERT and is preserved on re-parse. The VALUES `?` count drops by one (remove the `display_name` placeholder).

- [ ] **Step 8: Update `upsertSessionArgs`**

In `internal/db/sessions.go` (around line 983), change:
```go
// Before:
return []any{
    s.ID, s.Project, s.Machine, s.Agent, s.FirstMessage, s.DisplayName, s.NameSource,
    ...
// After:
return []any{
    s.ID, s.Project, s.Machine, s.Agent, s.FirstMessage, s.SessionName,
    ...
```

Remove `s.DisplayName` and `s.NameSource`. Add `s.SessionName` in the slot where `s.DisplayName` was. Verify the argument count matches the `?` count in the INSERT.

- [ ] **Step 9: Update the sidebar index query**

In `internal/db/sessions.go`, in `GetSidebarSessionIndex` (around line 725), the query selects `display_name` directly. Change to COALESCE:
```sql
-- Before:
display_name,
-- After:
COALESCE(display_name, session_name) AS display_name,
```

Also remove `NameSource` from `SidebarSessionIndexRow` and its scan if not already done.

- [ ] **Step 10: Run the test — expect PASS**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/... -run TestSessionNameCOALESCEInGetSession -v 2>&1 | tail -10
```

Expected: PASS

- [ ] **Step 11: Run full db test suite**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/... 2>&1 | tail -10
```

Expected: all pass

- [ ] **Step 12: Commit**

```bash
git add internal/db/sessions.go
git commit -m "feat(db): COALESCE(display_name,session_name) in read paths; session_name in upsert; remove name_source"
```

---

### Task 4: Update `RenameSession` and `humaRenameSession`

**Files:**
- Modify: `internal/db/sessions.go` (RenameSession)
- Modify: `internal/server/huma_routes_sessions.go` (delete re-parse block)

- [ ] **Step 1: Update `RenameSession`**

In `internal/db/sessions.go` (around line 1857), remove the `name_source` column update:

```go
// Before:
_, err := db.getWriter().Exec(
    `UPDATE sessions
     SET display_name = ?,
         name_source = ?,
         local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = ? AND deleted_at IS NULL`,
    displayName, source, id,
)

// After:
_, err := db.getWriter().Exec(
    `UPDATE sessions
     SET display_name = ?,
         local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
     WHERE id = ? AND deleted_at IS NULL`,
    displayName, id,
)
```

Also remove the `source` variable declaration (the `var source *string` block above the Exec call).

- [ ] **Step 2: Delete the re-parse block in `humaRenameSession`**

In `internal/server/huma_routes_sessions.go` (around line 536), delete the entire block:
```go
// Delete this entire block:
// When clearing a user rename, immediately re-parse the session file so
// the agent-provided name (if any) is restored in this response rather
// than waiting for the next periodic sync (up to 15 minutes).
// Reset file_size/file_mtime first so shouldSkipFile doesn't short-circuit
// the re-parse (the file itself hasn't changed on disk).
if displayName == nil && s.engine != nil {
    if localDB, ok := s.db.(*db.DB); ok {
        if path := localDB.GetSessionFilePath(in.ID); path != "" {
            // Reset by path so all sessions sharing the file
            // (e.g. Claude fork sessions) are re-parsed too.
            _ = localDB.ResetFileStateByPath(path)
            s.engine.SyncPaths([]string{path})
        }
    }
}
```

With two fields, clearing a user rename immediately exposes `session_name` via the COALESCE — no re-parse is needed.

- [ ] **Step 3: Also remove the `db` import from `huma_routes_sessions.go` if it was only used by the deleted block**

Check: does `huma_routes_sessions.go` still use `db.DB` after removing the block?
```bash
grep -n "db\.DB\|localDB\|db\.Session" internal/server/huma_routes_sessions.go | head -10
```
If `db.DB` is no longer referenced, remove the type assertion reference. The import `go.kenn.io/agentsview/internal/db` is still needed for `db.Session` etc.

- [ ] **Step 4: Delete `ResetFileStateByPath` from `sessions.go`**

In `internal/db/sessions.go`, delete the `ResetFileStateByPath` function entirely:
```go
// Delete this entire function:
// ResetFileStateByPath nulls out file_size and file_mtime for every session
// sharing the given file path...
func (db *DB) ResetFileStateByPath(path string) error { ... }
```

- [ ] **Step 5: Build and test**

```bash
go fmt ./internal/db/... ./internal/server/... && go vet ./internal/db/... ./internal/server/...
CGO_ENABLED=1 go test -tags fts5 ./internal/db/... ./internal/server/... 2>&1 | tail -10
```

Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add internal/db/sessions.go internal/server/huma_routes_sessions.go
git commit -m "refactor(db,server): simplify RenameSession (drop name_source); delete rename-clear re-parse block"
```

---

### Task 5: Update the three parse→DB converters

**Files:**
- Modify: `internal/sync/engine.go` (toDBSession)
- Modify: `internal/server/upload.go` (sessionBatchWriteFromParsed)
- Modify: `internal/importer/importer.go` (upsertConversation, ImportChatGPT)

- [ ] **Step 1: Update `toDBSession` in `engine.go`**

In `internal/sync/engine.go` (around line 5118), change:
```go
// Before:
s.DisplayName, s.NameSource = db.ParsedSessionNameFields(pw.sess)
// After:
s.SessionName = db.ParsedSessionName(pw.sess)
```

- [ ] **Step 2: Update `sessionBatchWriteFromParsed` in `upload.go`**

In `internal/server/upload.go` (around line 254), change:
```go
// Before:
dbSess.DisplayName, dbSess.NameSource = db.ParsedSessionNameFields(sess)
// After:
dbSess.SessionName = db.ParsedSessionName(sess)
```

- [ ] **Step 3: Update `upsertConversation` in `importer.go`**

In `internal/importer/importer.go` (around line 173), change:
```go
// Before:
displayName, nameSource := db.ParsedSessionNameFields(s)

sess := db.Session{
    ...
    DisplayName:      displayName,
    NameSource:       nameSource,
    ...
}
// After:
sess := db.Session{
    ...
    SessionName: db.ParsedSessionName(s),
    ...
}
```

- [ ] **Step 4: Update `ImportChatGPT` in `importer.go`**

In `internal/importer/importer.go` (around line 299), change:
```go
// Before:
displayName, nameSource := db.ParsedSessionNameFields(s)
sess := db.Session{
    ...
    DisplayName:      displayName,
    NameSource:       nameSource,
    ...
}
// After:
sess := db.Session{
    ...
    SessionName: db.ParsedSessionName(s),
    ...
}
```

- [ ] **Step 5: Build and test**

```bash
CGO_ENABLED=1 go build -tags fts5 ./internal/...
CGO_ENABLED=1 go test -tags fts5 ./internal/sync/... ./internal/server/... ./internal/importer/... 2>&1 | tail -10
```

Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add internal/sync/engine.go internal/server/upload.go internal/importer/importer.go
git commit -m "refactor(sync,server,importer): converters set SessionName via ParsedSessionName"
```

---

### Task 6: Clean up `CopySessionMetadataFrom` in `orphaned.go`

**Files:**
- Modify: `internal/db/orphaned.go`

- [ ] **Step 1: Simplify `CopySessionMetadataFrom`**

In `internal/db/orphaned.go` (around line 370), replace the entire display_name/name_source copying block:

```go
// Before:
hasDisplayName := oldDBHasColumn(ctx, tx, "sessions", "display_name")
hasDeletedAt := oldDBHasColumn(ctx, tx, "sessions", "deleted_at")
hasNameSource := oldDBHasColumn(ctx, tx, "sessions", "name_source")

// ... deleted_at copy ...

if hasDisplayName && hasNameSource {
    if _, err := tx.ExecContext(ctx, `
        UPDATE main.sessions
        SET display_name = old_s.display_name,
            name_source  = 'user'
        FROM old_db.sessions old_s
        WHERE main.sessions.id = old_s.id
          AND old_s.name_source = 'user'`); err != nil {
        return fmt.Errorf("copying user display_name: %w", err)
    }
} else if hasDisplayName {
    if _, err := tx.ExecContext(ctx, `
        UPDATE main.sessions
        SET display_name = old_s.display_name,
            name_source  = 'user'
        FROM old_db.sessions old_s
        WHERE main.sessions.id = old_s.id
          AND old_s.display_name IS NOT NULL`); err != nil {
        return fmt.Errorf("copying legacy display_name: %w", err)
    }
}
```

```go
// After:
hasDisplayName := oldDBHasColumn(ctx, tx, "sessions", "display_name")
hasDeletedAt := oldDBHasColumn(ctx, tx, "sessions", "deleted_at")

// ... deleted_at copy (unchanged) ...

// Copy user-set display_name (renames set via RenameSession) from the old DB.
// session_name is repopulated by re-parse; only the user override needs copying.
if hasDisplayName {
    if _, err := tx.ExecContext(ctx, `
        UPDATE main.sessions
        SET display_name = old_s.display_name
        FROM old_db.sessions old_s
        WHERE main.sessions.id = old_s.id
          AND old_s.display_name IS NOT NULL`); err != nil {
        return fmt.Errorf("copying user display_name: %w", err)
    }
}
```

The `hasNameSource` variable and its conditional branches are removed. We copy any non-NULL `display_name` from the old DB — in the two-field design, `display_name` is always user-owned, so any non-NULL value is always a user rename worth preserving.

Also check and remove any other `name_source` references in `orphaned.go`:
```bash
grep -n "name_source\|NameSource" internal/db/orphaned.go
```

- [ ] **Step 2: Run DB tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/... 2>&1 | tail -10
```

Expected: all pass

- [ ] **Step 3: Commit**

```bash
git add internal/db/orphaned.go
git commit -m "refactor(db): simplify CopySessionMetadataFrom — drop name_source branch, display_name is always user-owned"
```

---

### Task 7: Update PostgreSQL paths

**Files:**
- Modify: `internal/postgres/schema.go` (coreDDL, AddMissingColumns)
- Modify: `internal/postgres/push.go` (pushSession INSERT, sessionPushFingerprint)
- Modify: `internal/postgres/sessions.go` (pgSessionCols, scanPGSession)

- [ ] **Step 1: Update `coreDDL` in `schema.go`**

In `internal/postgres/schema.go` (around line 42), change:
```sql
-- Before:
display_name       TEXT,
name_source        TEXT,
-- After:
display_name       TEXT,
session_name       TEXT,
```

- [ ] **Step 2: Update `AddMissingColumns` in `schema.go`**

In `internal/postgres/schema.go` (around line 530), change the `name_source` entry:
```go
// Before:
{
    "sessions", "name_source",
    `name_source TEXT`,
    "adding sessions.name_source",
},
// After:
{
    "sessions", "session_name",
    `session_name TEXT`,
    "adding sessions.session_name",
},
```

- [ ] **Step 3: Update `pushSession` INSERT in `push.go`**

In `internal/postgres/push.go` (around line 767), change `name_source` to `session_name` in both the INSERT column list and the ON CONFLICT SET:

```sql
-- In INSERT column list (around line 769):
-- Before:
first_message, display_name, name_source,
-- After:
first_message, display_name, session_name,
```

```sql
-- In ON CONFLICT SET (around line 811):
-- Before:
display_name = EXCLUDED.display_name,
name_source = EXCLUDED.name_source,
-- After:
session_name = EXCLUDED.session_name,
-- display_name is NOT in the SET clause (only RenameSession touches it)
```

Also update the change-detection clause (around line 857):
```sql
-- Before:
OR sessions.display_name IS DISTINCT FROM EXCLUDED.display_name
OR sessions.name_source IS DISTINCT FROM EXCLUDED.name_source
-- After:
OR sessions.display_name IS DISTINCT FROM EXCLUDED.display_name
OR sessions.session_name IS DISTINCT FROM EXCLUDED.session_name
```

And update the argument binding (around line 903):
```go
// Before:
nilStr(sess.NameSource),
// After:
nilStr(sess.SessionName),
```

Find and update the exact parameter number in the positional `$N` placeholders if needed.

- [ ] **Step 4: Update `sessionPushFingerprint` in `push.go`**

In `internal/postgres/push.go` (around line 649):
```go
// Before:
stringValue(sess.NameSource),
// After:
stringValue(sess.SessionName),
```

- [ ] **Step 5: Update `pgSessionCols` and `scanPGSession` in `sessions.go`**

In `internal/postgres/sessions.go` (around line 33), change:
```go
// Before:
const pgSessionCols = `id, project, machine, agent,
	first_message, display_name, created_at, started_at,
// After:
const pgSessionCols = `id, project, machine, agent,
	first_message, COALESCE(display_name, session_name) AS display_name, created_at, started_at,
```

`scanPGSession` is unchanged — it already scans into `s.DisplayName` directly, and the SQL alias makes it transparent.

Also check for any PG sidebar queries that select `display_name` directly and apply the same COALESCE:
```bash
grep -n "display_name\|name_source" internal/postgres/sessions.go | head -20
```

Update any inline sidebar queries that have `display_name,` without the COALESCE.

- [ ] **Step 6: Build and test**

```bash
go fmt ./internal/postgres/... && go vet ./internal/postgres/...
CGO_ENABLED=1 go test -tags fts5 ./internal/postgres/... 2>&1 | tail -10
```

Expected: all pass (PG integration tests skipped without `pgtest` tag)

- [ ] **Step 7: Commit**

```bash
git add internal/postgres/schema.go internal/postgres/push.go internal/postgres/sessions.go
git commit -m "feat(postgres): session_name replaces name_source in DDL, push, fingerprint, and read COALESCE"
```

---

### Task 8: Delete stale tests and rewrite for `session_name`

**Files:**
- Modify: `internal/db/db_test.go`
- Modify: `internal/db/sessions_test.go`
- Modify: `internal/db/parsedsession_test.go`
- Modify: `internal/postgres/name_source_pgtest_test.go`
- Modify: `internal/postgres/sync_test.go` (if any name_source refs remain)
- Modify: `internal/server/upload_internal_test.go`
- Modify: `internal/importer/importer_test.go`

- [ ] **Step 1: Delete stale tests in `db_test.go`**

Delete these test functions entirely:
- `TestBackfillNameSourceSkipsImportedSessions`
- `TestNameSourceMigrationBackfillsUserRenames`
- `TestResetFileStateByPathClearsAllSharers`

- [ ] **Step 2: Rename/rewrite test in `sessions_test.go`**

`TestGetSessionPopulatesNameSourceInternally` — rewrite to test `session_name` persistence via the full-cols path:

```go
func TestGetSessionFullPopulatesSessionName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertSession(Session{
		ID: "s-sn", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Agent Title"), MessageCount: 1,
	}), "upsert with session_name")

	// GetSessionFull uses sessionFullCols — populates both raw fields.
	s, err := d.GetSessionFull(ctx, "s-sn")
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, s.SessionName, "SessionName should be populated from sessionFullCols")
	assert.Equal(t, "Agent Title", *s.SessionName)
	// display_name is NULL (no user rename), so DisplayName should be NULL too.
	assert.Nil(t, s.DisplayName, "DisplayName should be nil when no user rename (raw field)")
}
```

Also update `TestUpsertNameSourceOwnership` (around line 409) — rename to `TestUpsertSessionNameOwnership` and rewrite to use `SessionName`:

```go
func TestUpsertSessionNameOwnership(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Parser sets session_name; re-parse updates it freely.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("First Agent Name"), MessageCount: 1,
	}), "first upsert")

	// User renames: display_name wins in GetSession (COALESCE).
	requireNoError(t, d.RenameSession("s1", Ptr("User Name")), "rename")
	s, err := d.GetSession(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "User Name", *s.DisplayName, "user rename wins")

	// Re-parse updates session_name; user rename preserved.
	requireNoError(t, d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("New Agent Name"), MessageCount: 2,
	}), "re-parse upsert")
	s, err = d.GetSession(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "User Name", *s.DisplayName, "user rename still wins after re-parse")

	// Clear rename: session_name now visible.
	requireNoError(t, d.RenameSession("s1", nil), "clear rename")
	s, err = d.GetSession(ctx, "s1")
	require.NoError(t, err)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "New Agent Name", *s.DisplayName, "new session_name visible after clear")
}
```

- [ ] **Step 3: Update `parsedsession_test.go`**

Rename `TestParsedSessionNameFields` → `TestParsedSessionName` and update to the new single-return-value signature:

```go
func TestParsedSessionName(t *testing.T) {
	t.Run("no name extracted returns nil", func(t *testing.T) {
		name := db.ParsedSessionName(parser.ParsedSession{})
		require.Nil(t, name)
	})
	t.Run("empty SessionName returns nil", func(t *testing.T) {
		name := db.ParsedSessionName(parser.ParsedSession{SessionName: ""})
		require.Nil(t, name)
	})
	t.Run("non-empty SessionName returns pointer", func(t *testing.T) {
		name := db.ParsedSessionName(parser.ParsedSession{SessionName: "My Session"})
		require.NotNil(t, name)
		assert.Equal(t, "My Session", *name)
	})
}
```

- [ ] **Step 4: Rewrite `name_source_pgtest_test.go`**

Replace the contents of `internal/postgres/name_source_pgtest_test.go` with `session_name` equivalents:

```go
//go:build pgtest

package postgres

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// TestPushSessionNameRoundTrip verifies that session_name is pushed from
// SQLite to PostgreSQL and stored correctly.
func TestPushSessionNameRoundTrip(t *testing.T) {
	pgURL := testPGURL(t)

	const schema = "agentsview_push_sessionname_test"
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	defer pg.Close()

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	defer localDB.Close()

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}

	sessionName := "My renamed session"
	sess := db.Session{
		ID:          "sessionname-test-001",
		Project:     "test-project",
		Machine:     "test-machine",
		Agent:       "claude",
		MessageCount: 1,
		CreatedAt:   "2026-01-01T00:00:00Z",
		SessionName: &sessionName,
	}

	tx, err := pg.BeginTx(ctx, nil)
	require.NoError(t, err, "BeginTx")
	if err := sync.pushSession(ctx, tx, sess); err != nil {
		_ = tx.Rollback()
		t.Fatalf("pushSession: %v", err)
	}
	require.NoError(t, tx.Commit(), "Commit")

	// Verify session_name stored in PG via direct SQL.
	var gotSessionName *string
	require.NoError(t, pg.QueryRow(
		`SELECT session_name FROM sessions WHERE id = $1`,
		sess.ID,
	).Scan(&gotSessionName), "read back session_name")
	require.NotNil(t, gotSessionName, "session_name should not be NULL")
	assert.Equal(t, "My renamed session", *gotSessionName, "session_name round-trip")

	// Verify COALESCE resolves correctly via store read.
	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err, "NewStore")
	defer store.Close()

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Limit: 50})
	require.NoError(t, err, "GetSidebarSessionIndex")

	var found *db.SidebarSessionIndexRow
	for i := range index.Sessions {
		if index.Sessions[i].ID == sess.ID {
			found = &index.Sessions[i]
			break
		}
	}
	require.NotNil(t, found, "session not found in sidebar index")
	require.NotNil(t, found.DisplayName, "COALESCE should return session_name as display_name")
	assert.Equal(t, "My renamed session", *found.DisplayName)
}

// TestPushSessionNameViaPushPath verifies session_name survives the real push
// path (Push -> ListSessionsModifiedBetween read).
func TestPushSessionNameViaPushPath(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local, "machine-sessionname-push", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := time.Now().UTC().Format(time.RFC3339)
	firstMsg := "real push path"
	sessionName := "plan-2b-review"
	require.NoError(t, local.UpsertSession(db.Session{
		ID:          "sn-push-001",
		Project:     "p",
		Machine:     "local",
		Agent:       "claude",
		FirstMessage: &firstMsg,
		SessionName: &sessionName,
		StartedAt:   &started,
		MessageCount: 1,
	}), "upsert session")

	pushResult, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	require.Equal(t, 1, pushResult.SessionsPushed)

	// Verify session_name stored in PG.
	var pushedSessionName *string
	require.NoError(t, ps.DB().QueryRow(
		`SELECT session_name FROM agentsview.sessions WHERE id = $1`,
		"sn-push-001",
	).Scan(&pushedSessionName), "read session_name via direct SQL")
	require.NotNil(t, pushedSessionName)
	assert.Equal(t, "plan-2b-review", *pushedSessionName)

	// Verify COALESCE returns session_name as display_name.
	store, err := NewStore(pgURL, "agentsview", true)
	require.NoError(t, err, "opening store")
	defer store.Close()

	index, err := store.GetSidebarSessionIndex(ctx, db.SessionFilter{Limit: 50})
	require.NoError(t, err)
	require.Len(t, index.Sessions, 1)
	require.NotNil(t, index.Sessions[0].DisplayName)
	assert.Equal(t, "plan-2b-review", *index.Sessions[0].DisplayName)
}
```

- [ ] **Step 5: Update upload and importer tests**

In `internal/server/upload_internal_test.go`, update `TestSessionBatchWriteFromParsedPreservesDisplayName`:
```go
func TestSessionBatchWriteFromParsedPreservesSessionName(t *testing.T) {
	sess := parser.ParsedSession{
		ID:          "test-session",
		SessionName: "My Renamed Session",
	}
	result := sessionBatchWriteFromParsed(sess, nil)
	require.NotNil(t, result.Session.SessionName,
		"SessionName must be persisted on upload")
	require.Equal(t, "My Renamed Session", *result.Session.SessionName)
}
```

In `internal/importer/importer_test.go`, update `TestImportSetsAgentNameSource`:
```go
func TestImportSetsSessionName(t *testing.T) {
	// ... same setup ...
	require.NotNil(t, got.SessionName)
	require.Equal(t, "Agent Title", *got.SessionName)
}
```

- [ ] **Step 6: Run full test suite**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/... 2>&1 | grep -E "FAIL|ok" | tail -20
```

Expected: all pass

- [ ] **Step 7: Commit**

```bash
git add internal/db/ internal/postgres/name_source_pgtest_test.go internal/postgres/sync_test.go internal/server/upload_internal_test.go internal/importer/importer_test.go
git commit -m "test: rewrite name_source tests for session_name; delete stale backfill/reset tests"
```

---

### Task 9: Final verification

- [ ] **Step 1: Check no `name_source` references remain in production code**

```bash
grep -rn "name_source\|NameSource\|BackfillNameSource\|ParsedSessionNameFields\|ResetFileStateByPath" \
  internal/ \
  --include="*.go" | grep -v "_test\.go"
```

Expected: no output.

- [ ] **Step 2: Check `ParsedSession.DisplayName` is fully renamed**

```bash
grep -rn "ParsedSession\|\.DisplayName\b" internal/parser/ --include="*.go" | grep -v "AgentDef\|// "
```

Expected: no references to `.DisplayName` on `ParsedSession` (only `AgentDef.DisplayName` should remain).

- [ ] **Step 3: Go fmt and vet**

```bash
go fmt ./... && go vet ./...
```

Expected: no output

- [ ] **Step 4: Full Go test suite**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/... 2>&1 | tail -20
```

Expected: all pass, no FAIL

- [ ] **Step 5: Frontend build and tests**

```bash
make frontend
cd frontend && npm run test -- --run 2>&1 | tail -5
```

Expected: frontend unchanged, all tests pass

- [ ] **Step 6: Final commit if any stray changes**

```bash
git status
```

If clean: no commit needed. If loose ends: commit them.

---

## Self-Review

**Spec coverage check:**

| Spec requirement | Covered by |
|---|---|
| `ParsedSession.DisplayName → SessionName` | Task 1 |
| `session_name TEXT` column in SQLite schema | Task 2 |
| `NameSource` field removed from `db.Session` | Task 2 |
| `BackfillNameSource` deleted | Task 2 |
| `migrateColumns` updated | Task 2 |
| COALESCE in `sessionBaseCols` | Task 3 |
| Both raw cols in `sessionFullCols` | Task 3 |
| `upsertSessionSQL` CASE removed, `session_name` in SET | Task 3 |
| `display_name` removed from upsert INSERT/SET | Task 3 |
| All scans updated | Task 3 |
| `RenameSession` drops `name_source` | Task 4 |
| `humaRenameSession` re-parse block deleted | Task 4 |
| `ResetFileStateByPath` deleted | Task 4 |
| `toDBSession` uses `ParsedSessionName` | Task 5 |
| `sessionBatchWriteFromParsed` uses `ParsedSessionName` | Task 5 |
| Both importer converters updated | Task 5 |
| `CopySessionMetadataFrom` simplified | Task 6 |
| PG `coreDDL` updated | Task 7 |
| PG `AddMissingColumns` updated | Task 7 |
| PG `pushSession` INSERT/CONFLICT updated | Task 7 |
| PG `sessionPushFingerprint` updated | Task 7 |
| `pgSessionCols` COALESCE | Task 7 |
| Stale tests deleted | Task 8 |
| New `session_name` tests written | Task 8 |
| No `name_source` in production code | Task 9 |
| `AgentDef.DisplayName` NOT renamed | Task 1 (note) |

**Placeholder scan:** None found.

**Type consistency:** `ParsedSessionName` defined in Task 1 returning `*string`, called in Tasks 5 as `db.ParsedSessionName(sess)`. `db.Session.SessionName *string` defined in Task 2, used in Tasks 3–5, 8. Consistent throughout.
