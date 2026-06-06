# Session Names From Agent Sessions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist agent-provided session names (Claude Code's `/rename` plus the
native names seven other parsers already extract) into the database and show
them in the sidebar, without clobbering a user's in-app rename, behind an opt-in
"Session names" Appearance toggle.

**Architecture:** Add a `name_source` column (`'user'` | `'agent'` | NULL) that
drives both ownership and the frontend display gate. The parser captures names;
`toDBSession` carries them; a CASE-based upsert lets agent names update while a
manual rename pins permanently; a frontend toggle decides whether agent names
are shown. Existing data is backfilled via a `dataVersion` bump (full resync).

**Tech Stack:** Go (SQLite + PostgreSQL, `-tags fts5`, CGO), Svelte 5 /
TypeScript, Vitest.

Spec: `docs/superpowers/specs/2026-06-04-session-display-name-design.md`

______________________________________________________________________

## Conventions for every task

- Go tests: `CGO_ENABLED=1 go test -tags fts5 ./<pkg>/...`. Use testify
  (`require`/`assert`). DB tests use the `testDB(t)` helper.
- Frontend tests: from `frontend/`, `npm test` (Vitest).
- After Go changes: `go fmt ./...` and `go vet -tags fts5 ./...` before commit.
- Commit after each task with a conventional-commit message.

______________________________________________________________________

## Task 1: Add `name_source` column, migration backfill, and dataVersion bump

**Files:**

- Modify: `internal/db/schema.sql:8` (sessions CREATE TABLE)

- Modify: `internal/db/db.go:107` (dataVersion), `internal/db/db.go:429-446`
  (migrateColumns)

- Test: `internal/db/db_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/db/db_test.go`:

```go
func TestNameSourceMigrationBackfillsUserRenames(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// A pre-feature row: a manual rename stored display_name with no
	// name_source. Insert via a session, then set display_name directly.
	requireNoError(t, d.UpsertSession(db.Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
	}), "upsert s1")
	requireNoError(t, d.RenameSession("s1", strPtr("Manual Name")), "rename")

	// Simulate a legacy row by clearing name_source back to NULL.
	_, err := d.Writer().Exec(
		"UPDATE sessions SET name_source = NULL WHERE id = 's1'")
	requireNoError(t, err, "null out name_source")

	// Re-run the column migration; it must backfill name_source='user'
	// for rows that already have a display_name.
	requireNoError(t, d.BackfillNameSource(), "backfill")

	var src *string
	requireNoError(t, d.Writer().QueryRow(
		"SELECT name_source FROM sessions WHERE id = 's1'").Scan(&src),
		"select name_source")
	require.NotNil(t, src, "name_source should be backfilled")
	assert.Equal(t, "user", *src)
}
```

This test references two helpers we will add: `d.Writer()` may already exist as
an unexported accessor; if not, use the existing test pattern for raw SQL. If
`testDB` exposes no raw writer, replace the two raw `Exec`/`QueryRow` blocks
with the project's existing raw-access helper (grep `func.*testDB` and nearby
tests for the pattern, e.g. `d.getWriter()` is unexported — tests in this
package call it directly since they are in `package db`). Use `d.getWriter()` if
the test file is `package db`.

Adjust the test to the in-package form:

```go
	_, err := d.getWriter().Exec(
		"UPDATE sessions SET name_source = NULL WHERE id = 's1'")
```

```go
	requireNoError(t, d.getWriter().QueryRow(
		"SELECT name_source FROM sessions WHERE id = 's1'").Scan(&src),
		"select name_source")
```

- [ ] **Step 2: Run the test to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestNameSourceMigration -v`
Expected: FAIL — `name_source` column does not exist / `BackfillNameSource`
undefined.

- [ ] **Step 3: Add the column to the canonical schema**

In `internal/db/schema.sql`, after line 8 (`display_name TEXT,`):

```sql
    display_name TEXT,
    name_source TEXT,
```

- [ ] **Step 4: Add the migration + backfill and bump dataVersion**

In `internal/db/db.go`, change `const dataVersion = 31` (line 107) to:

```go
// (32: name_source column; agent-provided session names persisted
// via toDBSession and a CASE-based upsert.)
const dataVersion = 32
```

In `migrateColumns` (the `migrations` slice starting at line 438), add a new
entry immediately after the `display_name` entry (lines 439-442):

```go
		{
			"sessions", "name_source",
			"ALTER TABLE sessions ADD COLUMN name_source TEXT",
		},
```

Then, after the migration loop applies columns (just before `migrateColumns`
returns nil), call the backfill. First add the backfill method to the same file:

```go
// BackfillNameSource stamps name_source='user' on rows that have a
// display_name but no name_source. Pre-feature databases stored only
// manual renames in display_name, so every such row is user-owned.
// Idempotent: rows already marked are skipped by the WHERE clause.
func (db *DB) BackfillNameSource() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`UPDATE sessions SET name_source = 'user'
		 WHERE display_name IS NOT NULL AND name_source IS NULL`)
	if err != nil {
		return fmt.Errorf("backfilling name_source: %w", err)
	}
	return nil
}
```

In `migrateColumns`, after the migration loop (before its final `return nil`),
release the lock-held section appropriately: `migrateColumns` already holds
`db.mu`, and `BackfillNameSource` also locks. Do NOT call `BackfillNameSource`
from inside `migrateColumns`. Instead, call it from `Open` right after
`migrateColumns()` succeeds. Grep for the existing `migrateColumns()` call site
in `Open` and add:

```go
	if err := db.migrateColumns(); err != nil {
		return nil, err
	}
	if err := db.BackfillNameSource(); err != nil {
		return nil, err
	}
```

- [ ] **Step 5: Run the test to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestNameSourceMigration -v`
Expected: PASS.

- [ ] **Step 6: go fmt, vet, commit**

```bash
go fmt ./... && go vet -tags fts5 ./internal/db/
git add internal/db/schema.sql internal/db/db.go internal/db/db_test.go
git commit -m "feat(db): add name_source column with migration backfill"
```

______________________________________________________________________

## Task 2: Add `NameSource` to Session + SidebarSessionIndexRow and the SQLite sidebar query

**Files:**

- Modify: `internal/db/sessions.go:152` (Session struct), `:393`
  (SidebarSessionIndexRow), `:731` and `:767` (sidebar query + scan)

- Test: `internal/db/filter_test.go` (alongside the existing display_name
  sidebar test near line 1113)

- [ ] **Step 1: Write the failing test**

In `internal/db/filter_test.go`, add:

```go
func TestSidebarIndexIncludesNameSource(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	requireNoError(t, d.UpsertSession(db.Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
	}), "upsert")
	requireNoError(t, d.RenameSession("s1", strPtr("My Name")), "rename")

	index, err := d.GetSidebarSessionIndex(ctx, db.SessionFilter{})
	requireNoError(t, err, "sidebar index")
	require.Len(t, index.Sessions, 1)
	require.NotNil(t, index.Sessions[0].NameSource, "name_source nil")
	assert.Equal(t, "user", *index.Sessions[0].NameSource)
}
```

- [ ] **Step 2: Run to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestSidebarIndexIncludesNameSource -v`
Expected: FAIL — `NameSource` undefined.

- [ ] **Step 3: Add the struct fields**

In `internal/db/sessions.go`, in the `Session` struct after line 152
(`DisplayName *string ...`):

```go
	DisplayName          *string `json:"display_name,omitempty"`
	NameSource           *string `json:"name_source,omitempty"`
```

In the `SidebarSessionIndexRow` struct after line 393:

```go
	DisplayName       *string `json:"display_name,omitempty"`
	NameSource        *string `json:"name_source,omitempty"`
```

- [ ] **Step 4: Add name_source to the sidebar query and scan**

In `GetSidebarSessionIndex` (lines 723-746), add `name_source,` after
`display_name,` (line 731):

```sql
				display_name,
				name_source,
```

In the scan (lines 760-776), add `&row.NameSource,` after `&row.DisplayName,`
(line 767):

```go
				&row.DisplayName,
				&row.NameSource,
```

- [ ] **Step 5: Run to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestSidebarIndexIncludesNameSource -v`
Expected: PASS.

- [ ] **Step 6: commit**

```bash
go fmt ./... && go vet -tags fts5 ./internal/db/
git add internal/db/sessions.go internal/db/filter_test.go
git commit -m "feat(db): expose name_source on session and sidebar index"
```

______________________________________________________________________

## Task 3: CASE-based upsert (agent names update; user renames pin)

**Files:**

- Modify: `internal/db/sessions.go:881-925` (`upsertSessionSQL`), `:933-947`
  (`upsertSessionArgs`)

- Test: `internal/db/sessions_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/db/sessions_test.go`:

```go
func TestUpsertNameSourceOwnership(t *testing.T) {
	d := testDB(t)

	// Agent name lands on a fresh row.
	requireNoError(t, d.UpsertSession(db.Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: strPtr("agent-one"), NameSource: strPtr("agent"),
	}), "insert agent name")
	got := getSessionRow(t, d, "s1")
	require.NotNil(t, got.DisplayName)
	assert.Equal(t, "agent-one", *got.DisplayName)
	assert.Equal(t, "agent", *got.NameSource)

	// A newer agent name overwrites an agent-owned row.
	requireNoError(t, d.UpsertSession(db.Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: strPtr("agent-two"), NameSource: strPtr("agent"),
	}), "update agent name")
	got = getSessionRow(t, d, "s1")
	assert.Equal(t, "agent-two", *got.DisplayName)

	// A manual rename pins the row.
	requireNoError(t, d.RenameSession("s1", strPtr("user-name")), "rename")

	// A subsequent agent name must NOT overwrite the user name.
	requireNoError(t, d.UpsertSession(db.Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: strPtr("agent-three"), NameSource: strPtr("agent"),
	}), "agent after user")
	got = getSessionRow(t, d, "s1")
	assert.Equal(t, "user-name", *got.DisplayName, "user name must survive")
	assert.Equal(t, "user", *got.NameSource)
}
```

This needs a `getSessionRow` helper and `NameSource` available on the scanned
row. Add a small helper to the test file (reads the two fields directly, so it
does not depend on `scanSessionRow`):

```go
func getSessionRow(t *testing.T, d *db.DB, id string) db.Session {
	t.Helper()
	var s db.Session
	s.ID = id
	requireNoError(t, d.GetWriterForTest().QueryRow(
		"SELECT display_name, name_source FROM sessions WHERE id = ?", id).
		Scan(&s.DisplayName, &s.NameSource), "get session row")
	return s
}
```

If no `GetWriterForTest` exists, and the test is `package db` (check the file's
package clause — sessions_test.go is `package db`), call `d.getWriter()`
directly:

```go
	requireNoError(t, d.getWriter().QueryRow(
		"SELECT display_name, name_source FROM sessions WHERE id = ?", id).
		Scan(&s.DisplayName, &s.NameSource), "get session row")
```

Task 4 implements `RenameSession`'s name_source write; if running tasks out of
order, the "pin" assertion depends on Task 4. Run Tasks 3 and 4 together if
needed.

- [ ] **Step 2: Run to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestUpsertNameSourceOwnership -v`
Expected: FAIL — agent name not persisted (toDBSession/upsert do not write
display_name yet).

- [ ] **Step 3: Add name_source to INSERT columns/placeholders**

In `upsertSessionSQL` (line 883), add `name_source` after `display_name`:

```sql
		INSERT INTO sessions (
			id, project, machine, agent, first_message, display_name,
			name_source,
			started_at, ended_at, message_count,
```

Add one placeholder to the VALUES list (line 896) — it currently has 30 `?`;
make it 31:

```sql
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
```

- [ ] **Step 4: Add the CASE clauses to ON CONFLICT**

In the `ON CONFLICT(id) DO UPDATE SET` block (after line 897, e.g. right after
`first_message = excluded.first_message,`), add:

```sql
			display_name = CASE WHEN sessions.name_source = 'user'
				THEN sessions.display_name
				ELSE excluded.display_name END,
			name_source = CASE WHEN sessions.name_source = 'user'
				THEN sessions.name_source
				ELSE excluded.name_source END,
```

- [ ] **Step 5: Add the arg**

In `upsertSessionArgs` (line 934-935), add `s.NameSource` after `s.DisplayName`:

```go
		s.ID, s.Project, s.Machine, s.Agent, s.FirstMessage, s.DisplayName,
		s.NameSource,
		s.StartedAt, s.EndedAt, s.MessageCount,
```

- [ ] **Step 6: Run to verify it passes** (with Task 4 applied)

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestUpsertNameSourceOwnership -v`
Expected: PASS.

- [ ] **Step 7: commit**

```bash
go fmt ./... && go vet -tags fts5 ./internal/db/
git add internal/db/sessions.go internal/db/sessions_test.go
git commit -m "feat(db): upsert preserves user renames, updates agent names"
```

______________________________________________________________________

## Task 4: RenameSession sets/clears name_source

**Files:**

- Modify: `internal/db/sessions.go:1806-1817` (`RenameSession`)

- Test: `internal/db/sessions_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestRenameSessionSetsAndClearsNameSource(t *testing.T) {
	d := testDB(t)
	requireNoError(t, d.UpsertSession(db.Session{
		ID: "s1", Project: "p", Machine: "local", Agent: "claude",
	}), "upsert")

	requireNoError(t, d.RenameSession("s1", strPtr("Name")), "rename")
	got := getSessionRow(t, d, "s1")
	require.NotNil(t, got.NameSource)
	assert.Equal(t, "user", *got.NameSource)

	requireNoError(t, d.RenameSession("s1", nil), "clear")
	got = getSessionRow(t, d, "s1")
	assert.Nil(t, got.DisplayName, "display_name cleared")
	assert.Nil(t, got.NameSource, "name_source cleared")
}
```

- [ ] **Step 2: Run to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestRenameSessionSetsAndClearsNameSource -v`
Expected: FAIL — name_source stays NULL after rename.

- [ ] **Step 3: Implement**

Replace the body of `RenameSession` (lines 1806-1817) with:

```go
func (db *DB) RenameSession(id string, displayName *string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	var source *string
	if displayName != nil {
		s := "user"
		source = &s
	}
	_, err := db.getWriter().Exec(
		`UPDATE sessions
		 SET display_name = ?,
		     name_source = ?,
		     local_modified_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		 WHERE id = ? AND deleted_at IS NULL`,
		displayName, source, id,
	)
	return err
}
```

- [ ] **Step 4: Run to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestRenameSessionSetsAndClearsNameSource -v`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
go fmt ./... && go vet -tags fts5 ./internal/db/
git add internal/db/sessions.go internal/db/sessions_test.go
git commit -m "feat(db): RenameSession marks name_source user/cleared"
```

______________________________________________________________________

## Task 5: CopySessionMetadataFrom overlays only user-owned names

**Files:**

- Modify: `internal/db/orphaned.go:282-317`
- Test: `internal/db/db_test.go` (near `TestCopySessionMetadataFrom`, line 3975)

Behavior: the fresh DB already holds re-parsed agent names
(`name_source='agent'`). The copy must overlay the old DB's **user** renames
only; agent-owned and cleared rows keep the fresh value. deleted_at is still
copied for all rows.

- [ ] **Step 1: Write the failing test**

```go
func TestCopySessionMetadataPreservesUserNotAgent(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.db")

	// Old DB: one user-renamed session, one agent-named session.
	oldDB, err := db.Open(oldPath)
	requireNoError(t, err, "open old")
	requireNoError(t, oldDB.UpsertSession(db.Session{
		ID: "u", Project: "p", Machine: "local", Agent: "claude",
	}), "upsert u")
	requireNoError(t, oldDB.RenameSession("u", strPtr("User Name")), "rename u")
	requireNoError(t, oldDB.UpsertSession(db.Session{
		ID: "a", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: strPtr("Old Agent"), NameSource: strPtr("agent"),
	}), "upsert a")
	requireNoError(t, oldDB.Close(), "close old")

	// Fresh DB: both re-parsed with new agent names.
	fresh := testDB(t)
	requireNoError(t, fresh.UpsertSession(db.Session{
		ID: "u", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: strPtr("Fresh Agent U"), NameSource: strPtr("agent"),
	}), "fresh u")
	requireNoError(t, fresh.UpsertSession(db.Session{
		ID: "a", Project: "p", Machine: "local", Agent: "claude",
		DisplayName: strPtr("Fresh Agent A"), NameSource: strPtr("agent"),
	}), "fresh a")

	requireNoError(t, fresh.CopySessionMetadataFrom(oldPath), "copy")

	u := getSessionRow(t, fresh, "u")
	assert.Equal(t, "User Name", *u.DisplayName, "user rename overlaid")
	assert.Equal(t, "user", *u.NameSource)

	a := getSessionRow(t, fresh, "a")
	assert.Equal(t, "Fresh Agent A", *a.DisplayName, "agent keeps fresh name")
	assert.Equal(t, "agent", *a.NameSource)
}
```

- [ ] **Step 2: Run to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestCopySessionMetadataPreservesUserNotAgent -v`
Expected: FAIL — current copy overwrites `u` with the old value for all rows and
ignores name_source.

- [ ] **Step 3: Implement**

In `internal/db/orphaned.go`, replace the display_name/deleted_at copy block
(lines 282-317) with one that always copies deleted_at, and overlays
display_name + name_source only for user-owned old rows. Probe for the
name_source column to stay safe against pre-feature source DBs:

```go
	hasDisplayName := oldDBHasColumn(ctx, tx, "sessions", "display_name")
	hasDeletedAt := oldDBHasColumn(ctx, tx, "sessions", "deleted_at")
	hasNameSource := oldDBHasColumn(ctx, tx, "sessions", "name_source")

	if hasDeletedAt {
		if _, err := tx.ExecContext(ctx, `
			UPDATE main.sessions
			SET deleted_at = old_s.deleted_at
			FROM old_db.sessions old_s
			WHERE main.sessions.id = old_s.id`); err != nil {
			return fmt.Errorf("copying deleted_at: %w", err)
		}
	}

	if hasDisplayName && hasNameSource {
		// Overlay user-owned names only; agent-owned and cleared rows
		// keep the freshly re-parsed value.
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
		// Pre-feature source DB: every non-NULL display_name was a
		// manual rename, so treat it as user-owned.
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

- [ ] **Step 4: Run to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run "TestCopySessionMetadata" -v`
Expected: PASS (both the new test and the existing
`TestCopySessionMetadataFrom`).

- [ ] **Step 5: commit**

```bash
go fmt ./... && go vet -tags fts5 ./internal/db/
git add internal/db/orphaned.go internal/db/db_test.go
git commit -m "feat(db): resync metadata copy overlays only user names"
```

______________________________________________________________________

## Task 6: Claude parser extracts /rename into DisplayName

**Files:**

- Modify: `internal/parser/claude.go` (full-parse scan loop ~86-93, 113,
  180-182, 256-263; `claudeSessionMeta` ~619-636; new `extractRenameName`
  helper)

- Test: `internal/parser/claude_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestClaudeRenameSetsDisplayName(t *testing.T) {
	rename := func(name string) string {
		return `{"type":"system","subtype":"local_command","content":` +
			`"<command-name>/rename</command-name>\n<command-args>` +
			name + `</command-args>","timestamp":"2026-06-01T00:00:00Z",` +
			`"sessionId":"s1"}`
	}
	userMsg := `{"type":"user","uuid":"u1","parentUuid":"",` +
		`"message":{"role":"user","content":"hello"},` +
		`"timestamp":"2026-06-01T00:00:01Z","sessionId":"s1","cwd":"/x"}`

	cases := []struct {
		name  string
		lines []string
		want  string
	}{
		{"single", []string{userMsg, rename("plan-7d")}, "plan-7d"},
		{"last wins", []string{userMsg, rename("first"), rename("second")}, "second"},
		{"empty clears", []string{userMsg, rename("first"), rename("")}, ""},
		{"none", []string{userMsg}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeJSONL(t, tc.lines) // existing test helper
			results, err := parser.ParseClaudeSession(path, "proj", "local")
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, tc.want, results[0].Session.DisplayName)
		})
	}
}
```

If no `writeJSONL` helper exists in this package, grep `claude_test.go` for the
established way it writes a temp `.jsonl` (e.g. a `func writeSession` or inline
`os.WriteFile(filepath.Join(t.TempDir(), "s1.jsonl"), ...)`) and use that. The
file's base name (minus `.jsonl`) becomes the session ID, so name it `s1.jsonl`.

- [ ] **Step 2: Run to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/parser/ -run TestClaudeRenameSetsDisplayName -v`
Expected: FAIL — DisplayName empty for all cases.

- [ ] **Step 3: Add the `extractRenameName` helper**

In `internal/parser/claude.go`, near the other command helpers (after
`extractCommandText`, ~line 1581):

```go
// extractRenameName returns the argument of a Claude Code /rename
// command envelope. The bool is true when content is a /rename
// invocation (including an empty argument, which clears the name) and
// false for any other command or non-command content.
func extractRenameName(content string) (string, bool) {
	m := xmlCmdNameRe.FindStringSubmatch(content)
	if m == nil {
		return "", false
	}
	name := strings.TrimPrefix(strings.TrimSpace(m[1]), "/")
	if name != "rename" {
		return "", false
	}
	args := ""
	if am := xmlCmdArgsRe.FindStringSubmatch(content); am != nil {
		args = strings.TrimSpace(am[1])
	}
	return args, true
}
```

- [ ] **Step 4: Capture it in the full-parse loop**

In `ParseClaudeSession`, add a `displayName` var to the declaration block (after
`gitBranch string` at line 87):

```go
		gitBranch       string
		displayName     string
		displayNameSet  bool
```

Detect the `/rename` system record. After
`entryType := gjson.Get(line, "type").Str` (line 113), before the
queue-operation handling (line 134), add:

```go
		if entryType == "system" {
			if name, ok := extractRenameName(
				gjson.Get(line, "content").Str,
			); ok {
				displayName = name
				displayNameSet = true
			}
			continue
		}
```

Add the field to `claudeSessionMeta` (after `gitBranch string`, line 623):

```go
	gitBranch       string
	displayName     string
```

Set it in `meta` (after `gitBranch: gitBranch,` line 260):

```go
		gitBranch:       gitBranch,
		displayName:     displayName,
```

Apply it in `applyTo` (after `sess.GitBranch = m.gitBranch`, line 633):

```go
	sess.GitBranch = m.gitBranch
	sess.DisplayName = m.displayName
```

(`displayNameSet` is declared for clarity but the empty-string default already
encodes "no rename"; the last assignment wins, so the "empty clears" case yields
`""`. Remove `displayNameSet` if `go vet` flags it as unused — it is not
needed.)

Simplify: drop `displayNameSet` entirely; only `displayName` is used.

- [ ] **Step 5: Run to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/parser/ -run TestClaudeRenameSetsDisplayName -v`
Expected: PASS (all four sub-cases).

- [ ] **Step 6: commit**

```bash
go fmt ./... && go vet -tags fts5 ./internal/parser/
git add internal/parser/claude.go internal/parser/claude_test.go
git commit -m "feat(parser): extract Claude /rename into session name"
```

______________________________________________________________________

## Task 7: Incremental Claude parse falls back to full parse on /rename

**Files:**

- Modify: `internal/parser/claude.go:362-401` (`ParseClaudeSessionFrom`)

- Test: `internal/parser/claude_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestClaudeIncrementalRenameTriggersFullParse(t *testing.T) {
	rename := `{"type":"system","subtype":"local_command","content":` +
		`"<command-name>/rename</command-name>\n<command-args>new` +
		`</command-args>","timestamp":"2026-06-01T00:00:02Z","sessionId":"s1"}`
	path := writeJSONL(t, []string{rename})

	_, _, _, err := parser.ParseClaudeSessionFrom(path, 0, 0)
	require.Error(t, err)
	assert.True(t, parser.IsIncrementalFullParseFallback(err),
		"a /rename in the appended region must force a full reparse")
}
```

- [ ] **Step 2: Run to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/parser/ -run TestClaudeIncrementalRenameTriggersFullParse -v`
Expected: FAIL — no error returned (system record is ignored).

- [ ] **Step 3: Implement**

In `ParseClaudeSessionFrom`, add a `sawRename` flag and detect it in the read
callback. Declare it alongside the other vars (after `latestTS time.Time` at
line 359):

```go
		latestTS time.Time
		sawRename bool
```

Inside the `readJSONLFrom` callback, after
`entryType := gjson.Get(line, "type").Str` (line 369), before the `attachment`
handling:

```go
			if entryType == "system" {
				if _, ok := extractRenameName(
					gjson.Get(line, "content").Str,
				); ok {
					sawRename = true
				}
				return
			}
```

After the `readJSONLFrom` call returns and its error is checked (after line
397), before the `len(entries) == 0` check, add:

```go
	if sawRename {
		return nil, time.Time{}, 0, ErrClaudeIncrementalNeedsFullParse
	}
```

- [ ] **Step 4: Run to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/parser/ -run TestClaudeIncrementalRenameTriggersFullParse -v`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
go fmt ./... && go vet -tags fts5 ./internal/parser/
git add internal/parser/claude.go internal/parser/claude_test.go
git commit -m "feat(parser): force full reparse when /rename is appended"
```

______________________________________________________________________

## Task 8: toDBSession carries DisplayName + name_source

**Files:**

- Modify: `internal/sync/engine.go:4996-5039` (`toDBSession`)

- Test: `internal/sync/engine_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestToDBSessionCarriesNameSource(t *testing.T) {
	// toDBSession is unexported; engine_test.go is package sync.
	pw := pendingWrite{sess: parser.ParsedSession{
		ID: "s1", Project: "p", Agent: parser.AgentClaude,
		DisplayName: "agent-name",
	}}
	s := toDBSession(pw)
	require.NotNil(t, s.DisplayName)
	assert.Equal(t, "agent-name", *s.DisplayName)
	require.NotNil(t, s.NameSource)
	assert.Equal(t, "agent", *s.NameSource)

	// No parsed name => both nil.
	s2 := toDBSession(pendingWrite{sess: parser.ParsedSession{
		ID: "s2", Project: "p", Agent: parser.AgentClaude,
	}})
	assert.Nil(t, s2.DisplayName)
	assert.Nil(t, s2.NameSource)
}
```

Confirm the `pendingWrite` literal compiles — grep `type pendingWrite` in
`internal/sync/` and include any required non-zero fields (e.g. `msgs`). If
`TokenCoverage` panics on a nil `msgs`, pass `msgs: nil` explicitly (the slice
zero value is fine) — `toDBSession` calls `pw.sess.TokenCoverage(pw.msgs)`.

- [ ] **Step 2: Run to verify it fails**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/sync/ -run TestToDBSessionCarriesNameSource -v`
Expected: FAIL — `NameSource` always nil, `DisplayName` nil.

- [ ] **Step 3: Implement**

In `toDBSession`, after the block that sets `s.FirstMessage` (lines 5030-5032),
add:

```go
	if pw.sess.DisplayName != "" {
		s.DisplayName = &pw.sess.DisplayName
		src := "agent"
		s.NameSource = &src
	}
```

- [ ] **Step 4: Run to verify it passes**

Run:
`CGO_ENABLED=1 go test -tags fts5 ./internal/sync/ -run TestToDBSessionCarriesNameSource -v`
Expected: PASS.

- [ ] **Step 5: commit**

```bash
go fmt ./... && go vet -tags fts5 ./internal/sync/
git add internal/sync/engine.go internal/sync/engine_test.go
git commit -m "feat(sync): persist parser session names via toDBSession"
```

______________________________________________________________________

## Task 9: PostgreSQL parity for name_source

**Files:**

- Modify: `internal/postgres/schema.go:41` (coreDDL), and add an ALTER migration
  for existing PG DBs in the `alters`/`columnMigration` section (~283-529)

- Modify: `internal/postgres/push.go:768` (INSERT cols), `:790` (placeholders),
  `:810` (ON CONFLICT — use the same CASE rule as SQLite), `:855` (WHERE
  DISTINCT), `:899` (args)

- Modify: `internal/postgres/sessions.go:33` (pgSessionCols), `:139`
  (scanPGSession), `:601` (sidebar query), `:636` (sidebar scan)

- Test: `internal/postgres/*_pgtest_test.go` (build tag `pgtest`)

- [ ] **Step 1: Write the failing test**

In an existing `pgtest` file (grep for `//go:build pgtest`), add a round-trip:

```go
func TestPGPushRoundTripsNameSource(t *testing.T) {
	store := newTestPGStore(t) // existing helper; match its real name
	src := "agent"
	requireNoError(t, store.pushOneSession(db.Session{ // use real push entry
		ID: "s1", Project: "p", Machine: "m", Agent: "claude",
		DisplayName: strPtr("agent-name"), NameSource: &src,
	}))
	rows := store.sidebarIndex(t) // existing read helper
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].NameSource)
	assert.Equal(t, "agent", *rows[0].NameSource)
}
```

Match the helper names to what the `pgtest` suite already uses (grep
`internal/postgres` for `pushSession`, `GetSidebarSessionIndex`,
`func newTest`/`func setup` in the test files). The point is: push a session
with `NameSource='agent'`, read it back via the PG sidebar index, assert it
survives.

- [ ] **Step 2: Run to verify it fails**

Run: `make test-postgres` (starts a PG container) or, with a running PG:
`TEST_PG_URL="postgres://...sslmode=disable" CGO_ENABLED=1 go test -tags "fts5,pgtest" ./internal/postgres/ -run TestPGPushRoundTripsNameSource -v`
Expected: FAIL — column/field missing.

- [ ] **Step 3: Schema**

In `internal/postgres/schema.go` coreDDL (line 41), after `display_name TEXT,`:

```go
    display_name       TEXT,
    name_source        TEXT,
```

Add an idempotent ALTER in the migration section (mirror an existing
`ALTER TABLE sessions ADD COLUMN IF NOT EXISTS ...` entry):

```go
    "ALTER TABLE sessions ADD COLUMN IF NOT EXISTS name_source TEXT",
```

- [ ] **Step 4: Push**

In `internal/postgres/push.go`:

- INSERT column list (line 768): add `name_source,` after `display_name,`.
- VALUES placeholders (line 790): insert the matching `$N` and renumber the
  subsequent placeholders (PG uses positional `$1..$N`, so every later index
  shifts by one — update them all in this statement).
- ON CONFLICT (line 810): add the CASE rule mirroring SQLite:
  ```sql
  display_name = CASE WHEN sessions.name_source = 'user'
      THEN sessions.display_name ELSE EXCLUDED.display_name END,
  name_source = CASE WHEN sessions.name_source = 'user'
      THEN sessions.name_source ELSE EXCLUDED.name_source END,
  ```
- WHERE DISTINCT (line 855): add
  `OR sessions.name_source IS DISTINCT FROM EXCLUDED.name_source`.
- Args (line 899): add `nilStr(sess.NameSource),` after
  `nilStr(sess.DisplayName),`.

NOTE: because renumbering positional placeholders is error-prone, after editing,
count the columns and the `$N` tokens and confirm they match, and that the args
slice length equals the highest `$N`.

- [ ] **Step 5: Read**

In `internal/postgres/sessions.go`:

- `pgSessionCols` (line 33): add `name_source,` after `display_name,`.

- `scanPGSession` (line 139): add `&s.NameSource,` after `&s.DisplayName,`.

- sidebar query (line 601): add `name_source,` after `display_name,`.

- sidebar scan (line 636): add `&row.NameSource,` after `&row.DisplayName,`.

- [ ] **Step 6: Run to verify it passes**

Run: `make test-postgres` (or the explicit `TEST_PG_URL` command above).
Expected: PASS.

- [ ] **Step 7: commit**

```bash
go fmt ./... && go vet -tags "fts5,pgtest" ./internal/postgres/
git add internal/postgres/schema.go internal/postgres/push.go internal/postgres/sessions.go internal/postgres/*_test.go
git commit -m "feat(postgres): name_source parity in push and read paths"
```

______________________________________________________________________

## Task 10: Frontend types carry name_source through hydration

**Files:**

- Modify: `frontend/src/lib/api/types/core.ts:71` (`SidebarSessionIndexRow`) and
  the `Session` type (grep for `display_name?:` in `core.ts`)

- Modify: `frontend/src/lib/stores/sessions.svelte.ts:25` (`SessionGroupInput`),
  `:1056` and `:1080` (`sidebarIndexRowToSession`)

- Test: `frontend/src/lib/stores/sessions.test.ts`

- [ ] **Step 1: Write the failing test**

In `sessions.test.ts`, follow the existing `makeSkinnyRow` pattern (grep it) and
assert name_source is preserved into the store session and across hydration:

```ts
it("preserves name_source from the sidebar index row", () => {
  const row = makeSkinnyRow({
    id: "s1", display_name: "agent-name", name_source: "agent",
  });
  const s = sidebarIndexRowToSession(row);
  expect(s.name_source).toBe("agent");
});
```

If `sidebarIndexRowToSession` is not exported, test via the store's public
ingest path used by the other sidebar-index tests in this file.

- [ ] **Step 2: Run to verify it fails**

Run (from `frontend/`): `npm test -- sessions` Expected: FAIL — `name_source`
missing on the type / undefined at runtime.

- [ ] **Step 3: Add the type fields**

In `core.ts` `SidebarSessionIndexRow` (after line 71):

```ts
  display_name?: string | null;
  name_source?: string | null;
```

In `core.ts` `Session` type, after its `display_name?: ...` line:

```ts
  name_source?: string | null;
```

In `sessions.svelte.ts` `SessionGroupInput` (after line 25):

```ts
  display_name?: string | null;
  name_source?: string | null;
```

- [ ] **Step 4: Map it in sidebarIndexRowToSession**

In `sessions.svelte.ts`, in the `skinny` object (after line 1056):

```ts
    display_name: row.display_name ?? null,
    name_source: row.name_source ?? null,
```

In the merge override block (after line 1080, where
`display_name: skinny.display_name` is re-applied so hydration does not lose
it):

```ts
    display_name: skinny.display_name,
    name_source: skinny.name_source,
```

- [ ] **Step 5: Run to verify it passes**

Run (from `frontend/`): `npm test -- sessions` Expected: PASS.

- [ ] **Step 6: commit**

```bash
git add frontend/src/lib/api/types/core.ts frontend/src/lib/stores/sessions.svelte.ts frontend/src/lib/stores/sessions.test.ts
git commit -m "feat(frontend): thread name_source through sidebar index types"
```

______________________________________________________________________

## Task 11: "Session names" preference in the ui store (default off)

**Files:**

- Modify: `frontend/src/lib/stores/ui.svelte.ts` (key constant ~40, state ~181,
  setter ~418, $effect persistence ~279)

- Test: `frontend/src/lib/stores/ui.test.ts` (or `settings.test.ts` — match the
  existing ui-store test file)

- [ ] **Step 1: Write the failing test**

```ts
it("defaults showSessionNames off and toggles + persists", () => {
  localStorage.clear();
  // Fresh instance pattern: match how other ui.test.ts cases construct/reset.
  expect(ui.showSessionNames).toBe(false);
  ui.toggleShowSessionNames();
  expect(ui.showSessionNames).toBe(true);
  expect(localStorage.getItem("agentsview-show-session-names")).toBe("true");
});
```

- [ ] **Step 2: Run to verify it fails**

Run (from `frontend/`): `npm test -- ui` Expected: FAIL — `showSessionNames`
undefined.

- [ ] **Step 3: Implement (mirror the followLatest pattern)**

Key constant (near line 40):

```ts
const SHOW_SESSION_NAMES_KEY = "agentsview-show-session-names";
```

State field (near line 181, reusing `readStoredBool`):

```ts
  showSessionNames: boolean = $state(
    readStoredBool(SHOW_SESSION_NAMES_KEY, false),
  );
```

Setter + toggle (near line 418):

```ts
  setShowSessionNames(enabled: boolean) {
    this.showSessionNames = enabled;
  }

  toggleShowSessionNames() {
    this.setShowSessionNames(!this.showSessionNames);
  }
```

Persistence `$effect` inside the constructor (near line 279, mirroring the
followLatest effect):

```ts
      $effect(() => {
        try {
          localStorage?.setItem(
            SHOW_SESSION_NAMES_KEY,
            String(this.showSessionNames),
          );
        } catch {
          // ignore
        }
      });
```

- [ ] **Step 4: Run to verify it passes**

Run (from `frontend/`): `npm test -- ui` Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add frontend/src/lib/stores/ui.svelte.ts frontend/src/lib/stores/ui.test.ts
git commit -m "feat(frontend): add showSessionNames preference (default off)"
```

______________________________________________________________________

## Task 12: Appearance toggle row

**Files:**

- Modify: `frontend/src/lib/components/settings/AppearanceSettings.svelte`

- [ ] **Step 1: Add the row**

After the Theme `setting-row` (lines 29-34), add:

```svelte
  <div class="setting-row">
    <span class="setting-label">Session names</span>
    <button
      class="setting-toggle"
      onclick={() => ui.toggleShowSessionNames()}
    >
      {ui.showSessionNames ? "On" : "Off"}
    </button>
  </div>
```

- [ ] **Step 2: Verify build/typecheck**

Run (from `frontend/`): `npm run build` (or the project's `check` script if one
exists — grep `package.json` `scripts`). Expected: builds with no type errors.

- [ ] **Step 3: commit**

```bash
git add frontend/src/lib/components/settings/AppearanceSettings.svelte
git commit -m "feat(frontend): add Session names toggle to Appearance settings"
```

______________________________________________________________________

## Task 13: SessionItem.displayLabel honors ownership + toggle

**Files:**

- Modify: `frontend/src/lib/components/sidebar/SessionItem.svelte:1-12` (import
  `ui`), `:103-109` (`displayLabel` gate)

- Test: `frontend/src/lib/components/sidebar/SessionList.test.ts` or a
  `SessionItem` test (match the existing sidebar test file)

- [ ] **Step 1: Write the failing test**

Add a case asserting: a `name_source: 'user'` row shows `display_name`
regardless of the toggle; a `name_source: 'agent'` row shows `display_name` only
when `ui.showSessionNames` is true, otherwise the first-message preview. Model
it on the existing rename/display_name rendering test in the sidebar test file
(grep `display_name` in `frontend/src/lib/components/sidebar/*.test.ts`).

```ts
it("shows agent name only when showSessionNames is on", async () => {
  ui.setShowSessionNames(false);
  // render a session with display_name + name_source 'agent' and a
  // first_message; expect the first-message preview, not the agent name.
  // then ui.setShowSessionNames(true) and expect the agent name.
});

it("always shows a user rename", async () => {
  ui.setShowSessionNames(false);
  // display_name + name_source 'user' => agent name shown regardless.
});
```

- [ ] **Step 2: Run to verify it fails**

Run (from `frontend/`): `npm test -- SessionList` Expected: FAIL — current code
shows `display_name` whenever present, ignoring ownership/toggle.

- [ ] **Step 3: Implement**

Add the `ui` import (extend the existing import from `ui.svelte.js` at lines 2-8
in the script, or add a new import line):

```ts
  import { ui } from "../../stores/ui.svelte.js";
```

Replace the opening of `displayLabel` (lines 104-109):

```ts
  let displayLabel = $derived.by((): { text: string; isShell: boolean } => {
    if (
      session.display_name &&
      (session.name_source === "user" || ui.showSessionNames)
    ) {
      return {
        text: truncate(session.display_name, 50),
        isShell: false,
      };
    }
```

(Leave the rest of the function — teammate extraction, preview, project fallback
— unchanged.)

- [ ] **Step 4: Run to verify it passes**

Run (from `frontend/`): `npm test -- SessionList` Expected: PASS.

- [ ] **Step 5: commit**

```bash
git add frontend/src/lib/components/sidebar/SessionItem.svelte frontend/src/lib/components/sidebar/SessionList.test.ts
git commit -m "feat(frontend): gate agent session names behind toggle"
```

______________________________________________________________________

## Task 14: Final verification

- [ ] **Step 1: Full Go suite**

Run: `make test` Expected: all pass.

- [ ] **Step 2: PG integration**

Run: `make test-postgres` then `make postgres-down` Expected: all pass.

- [ ] **Step 3: Lint + vet**

Run: `make vet && make lint` Expected: clean (golangci-lint + NilAway).

- [ ] **Step 4: Frontend suite + build**

Run (from `frontend/`): `npm test && npm run build` Expected: all pass; build
succeeds.

- [ ] **Step 5: Binary build**

Run: `make build` Expected: builds the binary with embedded frontend.

- [ ] **Step 6: Manual smoke (real data)**

Build/run, open the app. In a Claude session, `/rename` it; confirm the sidebar
updates live once "Session names" is enabled in Appearance settings, and that a
manual in-app rename persists and is not overwritten by a later `/rename`.
Confirm that with the toggle off, agent names are hidden but manual renames
still show.

- [ ] **Step 7: Commit any fixups**

```bash
git add -A && git commit -m "chore: session-name feature verification fixups"
```

## Self-review notes (spec coverage)

- name_source data model + migration + backfill: Task 1.
- Sidebar exposure (SQLite): Task 2; (PostgreSQL): Task 9.
- Upsert ownership (CASE): Task 3; RenameSession: Task 4; resync copy: Task 5.
- Claude /rename extraction: Task 6; live refresh fallback: Task 7.
- toDBSession (all seven extracting parsers + Claude): Task 8.
- Frontend types/hydration: Task 10; preference: Task 11; toggle UI: Task 12;
  displayLabel gate: Task 13.
- Out of scope (opencode/codex/gemini/copilot): unchanged, no task — correct.
