# Session-Name Cascade Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Simplify the session-name feature by dropping the "Use session names" toggle and `name_source` from all API/frontend surfaces, while keeping it as a backend-only write-time guard so user renames survive re-sync.

**Architecture:** `name_source` stays in SQLite and PG schemas and the upsert CASE (write-time only). It is hidden from JSON responses (tag changed to `json:"-"`) and removed from sidebar/PG read queries. The frontend receives a single resolved `display_name` and falls back to `first_message`. A shared `ParsedSessionNameFields` helper in `internal/db/` becomes the single place that couples display name and name_source for all three parse→DB converters. The toggle UI and `visibleSessionName` helper are deleted; every display site uses `session.display_name` directly.

**Tech Stack:** Go 1.22+, SQLite3/fts5 (CGO), PostgreSQL (optional), Svelte 5, TypeScript, Vitest, testify

---

## File Map

| File | Action | What changes |
|---|---|---|
| `internal/db/parsedsession.go` | **Create** | `ParsedSessionNameFields` helper |
| `internal/db/parsedsession_test.go` | **Create** | Tests for helper |
| `internal/sync/engine.go` | Modify | `toDBSession` uses helper |
| `internal/server/upload.go` | Modify | `sessionBatchWriteFromParsed` uses helper |
| `internal/server/upload_test.go` | Modify | Add DisplayName regression test |
| `internal/importer/importer.go` | Modify | Remove manual guard, use helper |
| `internal/db/sessions.go` | Modify | `NameSource` JSON tag→`"-"` on `Session` and `SidebarSessionIndexRow`; remove from sidebar query+scan |
| `internal/postgres/sessions.go` | Modify | Remove `name_source` from `pgSessionCols` and `scanPGSession` |
| `frontend/src/lib/stores/ui.svelte.ts` | Modify | Remove `showSessionNames`, constant, methods, `$effect` |
| `frontend/src/lib/utils/sessionName.ts` | **Delete** | |
| `frontend/src/lib/utils/sessionName.test.ts` | **Delete** | |
| `frontend/src/lib/components/settings/AppearanceSettings.svelte` | Modify | Remove "Use session names" row |
| `frontend/src/lib/stores/sessions.svelte.ts` | Modify | Remove `name_source` from type and `sidebarRowToSession` |
| `frontend/src/lib/components/sidebar/SessionList.svelte` | Modify | Simplify `needsVisibleHydration`, drop import |
| `frontend/src/lib/components/sidebar/SessionItem.svelte` | Modify | Replace `visibleSessionName` with `session.display_name`, drop import |
| `frontend/src/lib/components/layout/SessionBreadcrumb.svelte` | Modify | Same |
| `frontend/src/lib/components/modals/ConfirmDeleteModal.svelte` | Modify | Same |
| `frontend/src/lib/components/trash/TrashPage.svelte` | Modify | Same |
| `frontend/src/lib/components/pinned/PinnedPage.svelte` | Modify | Same |
| `frontend/src/lib/stores/sessions.test.ts` | Modify | Remove `name_source` tests |
| `frontend/src/lib/stores/ui.test.ts` | Modify | Remove `showSessionNames` tests |

---

### Task 1: Create `ParsedSessionNameFields` helper

**Files:**
- Create: `internal/db/parsedsession.go`
- Create: `internal/db/parsedsession_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/db/parsedsession_test.go`:

```go
package db_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestParsedSessionNameFields(t *testing.T) {
	t.Run("no name extracted returns nil nil", func(t *testing.T) {
		name, src := db.ParsedSessionNameFields(parser.ParsedSession{})
		require.Nil(t, name)
		require.Nil(t, src)
	})
	t.Run("empty DisplayName returns nil nil", func(t *testing.T) {
		name, src := db.ParsedSessionNameFields(parser.ParsedSession{DisplayName: ""})
		require.Nil(t, name)
		require.Nil(t, src)
	})
	t.Run("name extracted sets agent source", func(t *testing.T) {
		name, src := db.ParsedSessionNameFields(parser.ParsedSession{DisplayName: "My Session"})
		require.NotNil(t, name)
		require.Equal(t, "My Session", *name)
		require.NotNil(t, src)
		require.Equal(t, "agent", *src)
	})
}
```

- [ ] **Step 2: Run test — expect compile failure (function not yet defined)**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/... -run TestParsedSessionNameFields -v
```

Expected: compile error `db.ParsedSessionNameFields undefined`

- [ ] **Step 3: Implement the helper**

Create `internal/db/parsedsession.go`:

```go
package db

import "go.kenn.io/agentsview/internal/parser"

// ParsedSessionNameFields returns the display_name and name_source values
// to store when upserting a parsed session. Both are nil when the parser
// did not extract a name. This is the single place that couples the two
// fields — callers must not set them independently.
func ParsedSessionNameFields(sess parser.ParsedSession) (displayName *string, nameSource *string) {
	if sess.DisplayName == "" {
		return nil, nil
	}
	name := sess.DisplayName
	src := "agent"
	return &name, &src
}
```

- [ ] **Step 4: Run test — expect PASS**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/db/... -run TestParsedSessionNameFields -v
```

Expected: `PASS`

- [ ] **Step 5: Commit**

```bash
git add internal/db/parsedsession.go internal/db/parsedsession_test.go
git commit -m "feat(db): add ParsedSessionNameFields helper to couple display_name+name_source"
```

---

### Task 2: Fix `toDBSession` in engine.go to use the helper

**Files:**
- Modify: `internal/sync/engine.go` (around line 5118)

- [ ] **Step 1: Replace inline name-setting with helper call**

In `internal/sync/engine.go`, find `toDBSession` (around line 5080). Replace:

```go
	if pw.sess.DisplayName != "" {
		s.DisplayName = &pw.sess.DisplayName
		s.NameSource = strPtr("agent")
	}
```

with:

```go
	s.DisplayName, s.NameSource = db.ParsedSessionNameFields(pw.sess)
```

- [ ] **Step 2: Run existing DB tests to verify no regression**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/... -v 2>&1 | tail -20
```

Expected: all tests pass

- [ ] **Step 3: Commit**

```bash
git add internal/sync/engine.go
git commit -m "refactor(sync): use ParsedSessionNameFields in toDBSession"
```

---

### Task 3: Fix `upload.go` converter (roborev finding)

**Files:**
- Modify: `internal/server/upload.go` (around line 228)
- Modify or create: test file for upload path

- [ ] **Step 1: Write a failing regression test**

Find or create `internal/server/upload_test.go`. Add a test that verifies a Claude session with a `/rename` directive has its `DisplayName` persisted after upload. Look for an existing test that calls `sessionBatchWriteFromParsed` directly or tests the upload handler. If no such test exists, add a unit test for `sessionBatchWriteFromParsed`:

```go
func TestSessionBatchWriteFromParsedPreservesDisplayName(t *testing.T) {
	sess := parser.ParsedSession{
		ID:          "test-session",
		DisplayName: "My Renamed Session",
	}
	result := sessionBatchWriteFromParsed(sess, nil)
	require.NotNil(t, result.Session.DisplayName,
		"DisplayName must be persisted on upload")
	require.Equal(t, "My Renamed Session", *result.Session.DisplayName)
	require.NotNil(t, result.Session.NameSource)
	require.Equal(t, "agent", *result.Session.NameSource)
}
```

Note: `sessionBatchWriteFromParsed` is package-private. If the test must be in the same package, use `package server` (not `package server_test`) or expose it. Check whether existing upload tests use `package server` or `package server_test`. Match the existing pattern.

- [ ] **Step 2: Run test — expect FAIL**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server/... -run TestSessionBatchWriteFromParsedPreservesDisplayName -v
```

Expected: FAIL — `DisplayName` is nil because the current code doesn't set it.

- [ ] **Step 3: Fix `sessionBatchWriteFromParsed`**

In `internal/server/upload.go`, the `sessionBatchWriteFromParsed` function currently does not set `DisplayName`. Add the call after the existing `FirstMessage` block (around line 253):

Current code (around line 248–260):
```go
	if sess.FirstMessage != "" {
		dbSess.FirstMessage = &sess.FirstMessage
	}
	if !sess.StartedAt.IsZero() {
		dbSess.StartedAt = timeutil.Ptr(sess.StartedAt)
	}
```

Add between them:
```go
	if sess.FirstMessage != "" {
		dbSess.FirstMessage = &sess.FirstMessage
	}
	dbSess.DisplayName, dbSess.NameSource = db.ParsedSessionNameFields(sess)
	if !sess.StartedAt.IsZero() {
		dbSess.StartedAt = timeutil.Ptr(sess.StartedAt)
	}
```

Make sure `db` is imported in `upload.go` — it already is (check with `grep '"go.kenn.io/agentsview/internal/db"' internal/server/upload.go`).

- [ ] **Step 4: Run test — expect PASS**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/server/... -run TestSessionBatchWriteFromParsedPreservesDisplayName -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/server/upload.go internal/server/upload_test.go
git commit -m "fix(server): persist ParsedSession.DisplayName through upload path"
```

---

### Task 4: Fix importer.go — remove manual guard, use helper

**Files:**
- Modify: `internal/importer/importer.go` (lines 173–195)

- [ ] **Step 1: Write a failing test verifying the importer sets NameSource**

In `internal/importer/` find or create a test file. Add:

```go
func TestImportSetsAgentNameSource(t *testing.T) {
	store := newTestStore(t)
	result := parser.ParseResult{
		Session: parser.ParsedSession{
			ID:           "sess1",
			DisplayName:  "Agent Title",
			FirstMessage: "hello",
		},
	}
	_, err := importSession(context.Background(), store, result, &lazyFTS{})
	require.NoError(t, err)

	got, err := store.GetSession(context.Background(), "sess1")
	require.NoError(t, err)
	require.NotNil(t, got.DisplayName)
	require.Equal(t, "Agent Title", *got.DisplayName)
	require.NotNil(t, got.NameSource)
	require.Equal(t, "agent", *got.NameSource)
}
```

Note: look at existing importer tests to learn how `newTestStore` or similar helpers work; match the existing pattern. `importSession` is the `importSession` function in `importer.go`. If it is not exported and the test must be in `package importer`, use `package importer` (not `_test`).

- [ ] **Step 2: Run test — expect FAIL**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/importer/... -run TestImportSetsAgentNameSource -v
```

Expected: FAIL — `NameSource` is nil because the current code doesn't set it.

- [ ] **Step 3: Replace manual guard with helper**

In `internal/importer/importer.go`, replace lines 173–195:

**Before:**
```go
	// Preserve user-renamed display_name on re-import.
	displayName := strPtr(s.DisplayName)
	if !isNew && existing != nil && existing.DisplayName != nil {
		importName := strPtr(s.DisplayName)
		nameChanged := importName == nil ||
			*existing.DisplayName != *importName
		if nameChanged {
			displayName = existing.DisplayName
		}
	}

	sess := db.Session{
		ID:               s.ID,
		Project:          s.Project,
		Machine:          s.Machine,
		Agent:            string(s.Agent),
		FirstMessage:     strPtr(s.FirstMessage),
		DisplayName:      displayName,
		StartedAt:        timeStr(s.StartedAt),
		EndedAt:          timeStr(s.EndedAt),
		MessageCount:     s.MessageCount,
		UserMessageCount: s.UserMessageCount,
	}
```

**After:**
```go
	displayName, nameSource := db.ParsedSessionNameFields(s)

	sess := db.Session{
		ID:               s.ID,
		Project:          s.Project,
		Machine:          s.Machine,
		Agent:            string(s.Agent),
		FirstMessage:     strPtr(s.FirstMessage),
		DisplayName:      displayName,
		NameSource:       nameSource,
		StartedAt:        timeStr(s.StartedAt),
		EndedAt:          timeStr(s.EndedAt),
		MessageCount:     s.MessageCount,
		UserMessageCount: s.UserMessageCount,
	}
```

The upsert CASE (`WHEN name_source = 'user' THEN keep`) now handles user-rename protection automatically — the manual guard was redundant and incorrect. Make sure `db` is imported in `importer.go`.

- [ ] **Step 4: Run test — expect PASS**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/importer/... -run TestImportSetsAgentNameSource -v
```

Expected: PASS

- [ ] **Step 5: Run all tests**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/... 2>&1 | tail -10
```

Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add internal/importer/importer.go internal/importer/
git commit -m "fix(importer): set name_source=agent via ParsedSessionNameFields, remove manual guard"
```

---

### Task 5: Hide `name_source` from API responses (backend)

**Files:**
- Modify: `internal/db/sessions.go` (lines 153, 395, 734, 770–771)
- Modify: `internal/postgres/sessions.go` (lines 33, 139)

This is all deletions/tag changes — no new tests needed (existing tests will catch scan mismatches).

- [ ] **Step 1: Change JSON tags on `Session` and `SidebarSessionIndexRow`**

In `internal/db/sessions.go`:

Line 153 — change:
```go
	NameSource           *string `json:"name_source,omitempty"`
```
to:
```go
	NameSource           *string `json:"-"`
```

Line 395 — change:
```go
	NameSource        *string `json:"name_source,omitempty"`
```
to:
```go
	NameSource        *string `json:"-"`
```

- [ ] **Step 2: Remove `name_source` from the sidebar index query**

In `internal/db/sessions.go`, in `GetSidebarSessionIndex` (around line 725), the query currently selects `name_source` at line 734. Remove it:

**Before:**
```go
			display_name,
			name_source,
			started_at,
```

**After:**
```go
			display_name,
			started_at,
```

- [ ] **Step 3: Remove `NameSource` from the sidebar scan**

In `internal/db/sessions.go`, the scan block starting at line 763 includes `&row.NameSource` at line 771. Remove it:

**Before:**
```go
		if err := rows.Scan(
			&row.ID,
			&row.ParentSessionID,
			&row.RelationshipType,
			&row.Project,
			&row.Machine,
			&row.Agent,
			&row.DisplayName,
			&row.NameSource,
			&row.StartedAt,
```

**After:**
```go
		if err := rows.Scan(
			&row.ID,
			&row.ParentSessionID,
			&row.RelationshipType,
			&row.Project,
			&row.Machine,
			&row.Agent,
			&row.DisplayName,
			&row.StartedAt,
```

- [ ] **Step 4: Remove `name_source` from `pgSessionCols` and `scanPGSession`**

In `internal/postgres/sessions.go`:

Line 33 — change:
```go
const pgSessionCols = `id, project, machine, agent,
	first_message, display_name, name_source, created_at, started_at,
```

to:
```go
const pgSessionCols = `id, project, machine, agent,
	first_message, display_name, created_at, started_at,
```

In `scanPGSession` (around line 137–139), remove `&s.NameSource` from the scan. The scan currently starts with:
```go
	err := rs.Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent,
		&s.FirstMessage, &s.DisplayName, &s.NameSource,
		&createdAt, &startedAt, &endedAt,
```

Change to:
```go
	err := rs.Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent,
		&s.FirstMessage, &s.DisplayName,
		&createdAt, &startedAt, &endedAt,
```

- [ ] **Step 5: Also check for PG sidebar queries that inline `name_source`**

Search for any PG sidebar queries that list `name_source` explicitly (not via `pgSessionCols`):

```bash
grep -n "name_source" internal/postgres/sessions.go
```

If lines around 601–602 show `display_name, name_source,` in an inline sidebar query, remove `name_source` from that query and its corresponding scan.

- [ ] **Step 6: Run Go tests + fmt/vet**

```bash
go fmt ./...
go vet ./...
CGO_ENABLED=1 go test -tags fts5 ./internal/... 2>&1 | tail -20
```

Expected: all pass. Scan errors would indicate a column count mismatch — fix the scan to match the query.

- [ ] **Step 7: Commit**

```bash
git add internal/db/sessions.go internal/postgres/sessions.go
git commit -m "refactor(db,pg): hide name_source from API responses; keep as backend write guard"
```

---

### Task 6: Remove toggle from UI store and settings

**Files:**
- Modify: `frontend/src/lib/stores/ui.svelte.ts`
- Modify: `frontend/src/lib/components/settings/AppearanceSettings.svelte`

- [ ] **Step 1: Remove `showSessionNames` from `ui.svelte.ts`**

In `frontend/src/lib/stores/ui.svelte.ts`:

1. Remove the constant (around line 41):
```ts
const SHOW_SESSION_NAMES_KEY = "agentsview-show-session-names";
```

2. Remove the state field (around line 186–188):
```ts
  showSessionNames: boolean = $state(
    readStoredBool(SHOW_SESSION_NAMES_KEY, false),
  );
```

3. Remove the `$effect` block that persists it (around lines 294–303):
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

4. Remove the two methods (around lines 447–453):
```ts
  setShowSessionNames(enabled: boolean) {
    this.showSessionNames = enabled;
  }

  toggleShowSessionNames() {
    this.setShowSessionNames(!this.showSessionNames);
  }
```

- [ ] **Step 2: Remove "Use session names" row from AppearanceSettings.svelte**

In `frontend/src/lib/components/settings/AppearanceSettings.svelte`, remove lines 36–44:
```svelte
  <div class="setting-row">
    <span class="setting-label">Use session names</span>
    <button
      class="setting-toggle"
      onclick={() => ui.toggleShowSessionNames()}
    >
      {ui.showSessionNames ? "On" : "Off"}
    </button>
  </div>
```

- [ ] **Step 3: Build frontend to catch type errors**

```bash
cd frontend && npm run check 2>&1 | head -30
```

Expected: errors about `toggleShowSessionNames` and `showSessionNames` being used elsewhere — the next tasks will fix those.

- [ ] **Step 4: Commit** (after all type errors resolved in subsequent tasks)

Hold this commit until Task 7 is done.

---

### Task 7: Delete `sessionName.ts` and replace all call sites

**Files:**
- Delete: `frontend/src/lib/utils/sessionName.ts`
- Delete: `frontend/src/lib/utils/sessionName.test.ts`
- Modify: `frontend/src/lib/stores/sessions.svelte.ts`
- Modify: `frontend/src/lib/components/sidebar/SessionList.svelte`
- Modify: `frontend/src/lib/components/sidebar/SessionItem.svelte`
- Modify: `frontend/src/lib/components/layout/SessionBreadcrumb.svelte`
- Modify: `frontend/src/lib/components/modals/ConfirmDeleteModal.svelte`
- Modify: `frontend/src/lib/components/trash/TrashPage.svelte`
- Modify: `frontend/src/lib/components/pinned/PinnedPage.svelte`

- [ ] **Step 1: Delete sessionName files**

```bash
rm frontend/src/lib/utils/sessionName.ts
rm frontend/src/lib/utils/sessionName.test.ts
```

- [ ] **Step 2: Remove `name_source` from frontend session types**

In `frontend/src/lib/stores/sessions.svelte.ts`:

At line 26, remove:
```ts
  name_source?: string | null;
```

At line 1058, in `sidebarRowToSession`, remove:
```ts
    name_source: row.name_source ?? null,
```

At line 1083, in the merge block, remove:
```ts
    name_source: skinny.name_source,
```

- [ ] **Step 3: Update `SessionList.svelte`**

Remove the `visibleSessionName` import (line 6):
```ts
  import { visibleSessionName } from "../../utils/sessionName.js";
```

Replace `needsVisibleHydration` (lines 202–210):

**Before:**
```ts
  function needsVisibleHydration(item: DisplayItem): boolean {
    const session = sessionForItem(item);
    if (!session?.is_index_only) return false;
    // A row needs its first_message hydrated unless its display_name
    // will actually be rendered. Agent-provided names are hidden when
    // the "session names" preference is off (see SessionItem's
    // displayLabel), so those rows still need the preview fallback.
    return !visibleSessionName(session);
  }
```

**After:**
```ts
  function needsVisibleHydration(item: DisplayItem): boolean {
    const session = sessionForItem(item);
    if (!session?.is_index_only) return false;
    return !session.display_name;
  }
```

- [ ] **Step 4: Update `SessionItem.svelte`**

Remove the `visibleSessionName` import (line 8):
```ts
  import { visibleSessionName } from "../../utils/sessionName.js";
```

Replace the `displayLabel` derived value (around line 108–134). Change the first two lines from:

```ts
    const name = visibleSessionName(session);
    if (name) {
```

to:

```ts
    const name = session.display_name ?? null;
    if (name) {
```

Update `startRename` (around line 191):

**Before:**
```ts
    renameValue =
      visibleSessionName(session)
      ?? normalizeMessagePreview(session.first_message);
```

**After:**
```ts
    renameValue =
      session.display_name
      ?? normalizeMessagePreview(session.first_message)
      ?? "";
```

- [ ] **Step 5: Update `SessionBreadcrumb.svelte`**

Remove the `visibleSessionName` import (around line 29).

Update `startRename` (around line 177):

**Before:**
```ts
    renameValue =
      visibleSessionName(session)
      ?? normalizeMessagePreview(session.first_message);
```

**After:**
```ts
    renameValue =
      session.display_name
      ?? normalizeMessagePreview(session.first_message)
      ?? "";
```

Update the breadcrumb display (around line 428):

**Before:**
```svelte
      {(session ? visibleSessionName(session) : null) ?? session?.project ?? ""}
```

**After:**
```svelte
      {session?.display_name ?? session?.project ?? ""}
```

Also check around line 177 for any other `visibleSessionName` calls — grep to be sure:
```bash
grep -n "visibleSessionName" frontend/src/lib/components/layout/SessionBreadcrumb.svelte
```

- [ ] **Step 6: Update `ConfirmDeleteModal.svelte`**

Remove the `visibleSessionName` import (line 7).

Replace `sessionName` derived value (lines 12–23):

**Before:**
```ts
  let sessionName = $derived.by(() => {
    const s = sessions.activeSession;
    if (!s) return "this session";
    const raw =
      visibleSessionName(s)
      ?? (
        normalizeMessagePreview(s.first_message)
        || s.project
        || "this session"
      );
    return truncate(raw, 60);
  });
```

**After:**
```ts
  let sessionName = $derived.by(() => {
    const s = sessions.activeSession;
    if (!s) return "this session";
    const raw =
      s.display_name
      ?? normalizeMessagePreview(s.first_message)
      ?? s.project
      ?? "this session";
    return truncate(raw, 60);
  });
```

- [ ] **Step 7: Update `TrashPage.svelte`**

Remove the `visibleSessionName` import.

Update `displayName` function (line 68–71):

**Before:**
```ts
  function displayName(s: Session): string {
    const raw = visibleSessionName(s) ?? normalizeMessagePreview(s.first_message);
    return raw ? truncate(raw, 70) : s.project;
  }
```

**After:**
```ts
  function displayName(s: Session): string {
    const raw = s.display_name ?? normalizeMessagePreview(s.first_message);
    return raw ? truncate(raw, 70) : s.project;
  }
```

- [ ] **Step 8: Update `PinnedPage.svelte`**

Remove the `visibleSessionName` import.

Update the `sessionMeta` function (around lines 55–67):

**Before:**
```ts
          name:
            visibleSessionName(s)
            ?? (normalizeMessagePreview(s.first_message) || s.project),
```

**After:**
```ts
          name:
            s.display_name
            ?? normalizeMessagePreview(s.first_message)
            ?? s.project,
```

- [ ] **Step 9: Build check**

```bash
cd frontend && npm run check 2>&1 | head -30
```

Expected: no TypeScript errors. If any `visibleSessionName` or `showSessionNames` references remain, find and fix them:
```bash
grep -rn "visibleSessionName\|showSessionNames\|toggleShowSessionNames\|name_source" frontend/src/ --include="*.svelte" --include="*.ts" | grep -v "test\|spec"
```

Expected: no output.

- [ ] **Step 10: Commit Task 6 + 7 together**

```bash
git add frontend/src/
git commit -m "refactor(frontend): drop name_source toggle; display_name cascade replaces visibleSessionName"
```

---

### Task 8: Clean up frontend tests

**Files:**
- Modify: `frontend/src/lib/stores/sessions.test.ts` (lines 373–409)
- Modify: `frontend/src/lib/stores/ui.test.ts` (showSessionNames tests, around lines 494–574)

- [ ] **Step 1: Remove `name_source` tests from `sessions.test.ts`**

Delete the two `it` blocks (lines 373–409):

```ts
    it("preserves name_source from the sidebar index row", async () => {
      // ... delete this whole block
    });

    it("name_source survives session hydration merge", async () => {
      // ... delete this whole block
    });
```

Also remove `name_source?: string | null` from the local test type (around line 78) if it exists.

Also update `makeSkinnyRow` and `makeSession` helpers if they reference `name_source` — remove those fields.

- [ ] **Step 2: Remove `showSessionNames` tests from `ui.test.ts`**

Find the `describe("showSessionNames", ...)` block (around line 494) and delete the entire block (~80 lines). The block includes tests for: default off, setting, toggling, and localStorage persistence.

- [ ] **Step 3: Run frontend tests**

```bash
cd frontend && npm run test -- --run 2>&1 | tail -20
```

Expected: all pass. If there are remaining failures about `name_source` or `showSessionNames`, find and fix them.

- [ ] **Step 4: Commit**

```bash
git add frontend/src/
git commit -m "test(frontend): remove name_source and showSessionNames tests (feature removed)"
```

---

### Task 9: Final verification and build

- [ ] **Step 1: Go fmt and vet**

```bash
go fmt ./...
go vet ./...
```

Expected: no output (no issues)

- [ ] **Step 2: Full Go test suite**

```bash
CGO_ENABLED=1 go test -tags fts5 ./internal/... 2>&1 | tail -20
```

Expected: all pass

- [ ] **Step 3: Frontend build**

```bash
make frontend
```

Expected: successful build, no TypeScript errors

- [ ] **Step 4: Frontend test suite**

```bash
cd frontend && npm run test -- --run 2>&1 | tail -20
```

Expected: all pass

- [ ] **Step 5: Confirm no stray references remain**

```bash
grep -rn "visibleSessionName\|showSessionNames\|toggleShowSessionNames" frontend/src/ --include="*.svelte" --include="*.ts"
grep -rn "name_source" frontend/src/ --include="*.svelte" --include="*.ts" | grep -v "test"
```

Expected: no output from either command.

- [ ] **Step 6: Final commit if any loose ends**

```bash
git add -p   # review any remaining unstaged changes
git commit -m "chore: final cleanup for session-name cascade simplification"
```

---

## Self-Review

**Spec coverage check:**

| Spec requirement | Covered by |
|---|---|
| Drop toggle (`showSessionNames`) | Task 6, 7, 8 |
| Drop `visibleSessionName` helper | Task 7 |
| Drop `name_source` from frontend types | Task 5 (sessions.svelte.ts), Task 7 (all components) |
| Drop `name_source` from API responses (SQLite) | Task 5 (JSON tag change) |
| Drop `name_source` from sidebar query/scan | Task 5 |
| Drop `name_source` from PG read path | Task 5 (pgSessionCols, scanPGSession) |
| Keep `name_source` in upsert CASE | Not changed (already correct) |
| Keep `name_source` in `RenameSession` | Not changed (already correct) |
| Keep `name_source` in PG write paths | Not changed (already correct) |
| Keep `name_source` in `CheckSchemaCompat` | Not changed (already correct) |
| Keep `name_source` in push fingerprint | Not changed (already correct) |
| Fix upload.go dropping DisplayName | Task 3 |
| Fix importer manual guard → CASE | Task 4 |
| Shared `parsedSessionBase` helper | Task 1 |
| `toDBSession` uses helper | Task 2 |
| `needsVisibleHydration` simplified | Task 7 |
| Frontend cascade: `display_name ?? first_message ?? project` | Task 7 (all 5 components) |
| `startRename` pre-fills with `display_name` directly | Task 7 (SessionItem, Breadcrumb) |
| Delete `sessionName.test.ts` | Task 7 |
| Delete `name_source` tests in sessions.test.ts | Task 8 |
| Delete `showSessionNames` tests in ui.test.ts | Task 8 |
| `dataVersion = 34` kept | Not changed |
| `BackfillNameSource` kept | Not changed |

**Placeholder scan:** No TBD/TODO/similar in any task. All code blocks are complete.

**Type consistency check:** `ParsedSessionNameFields` is defined in Task 1 as returning `(*string, *string)` and called as `s.DisplayName, s.NameSource = db.ParsedSessionNameFields(...)` in Tasks 2–4. The `Session.NameSource` field is `*string` throughout. Consistent.
