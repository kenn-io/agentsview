# Session Name Two-Field Design

**Date:** 2026-06-09
**Branch:** feat/session-display-name
**Status:** Approved for implementation

---

## Problem

The current branch stores both user renames and parser-extracted session names in a single `display_name` column, using a `name_source` discriminator ('user' | 'agent' | NULL) to protect user renames from being overwritten on re-parse. This caused a roborev review treadmill — every new surface needed to learn the discriminator — and the maintainer has requested a cleaner two-field approach where the backend resolves the name before sending it to the frontend.

Additionally, `ParsedSession.DisplayName` was named to match the DB column, which caused the natural but wrong instinct to store it in `display_name`, conflating two distinct concepts.

---

## Design

### Two columns, one resolved output

| DB column | Owned by | Written by |
|---|---|---|
| `session_name TEXT` | Parser / importer | `toDBSession`, `sessionBatchWriteFromParsed`, all importers |
| `display_name TEXT` | User | `RenameSession` only |

All read queries return `COALESCE(display_name, session_name) AS display_name`. The frontend receives one pre-resolved `display_name` field and is unaware of the distinction. The cascade is: user rename → session name → `first_message` preview → project.

### Naming fix

`parser.ParsedSession.DisplayName` is renamed to `parser.ParsedSession.SessionName` throughout. This is a purely internal Go rename (no DB column, JSON key, or file format impact) but it eliminates the conceptual conflation that caused the single-field design in the first place.

---

## Schema

**SQLite `sessions` table:**
- Add: `session_name TEXT`
- Remove: `name_source TEXT`
- Keep: `display_name TEXT` (user override only)

**`migrateColumns`:** adds `session_name TEXT` entry; removes `name_source` entry.

**`dataVersion`:** stays at 34. Re-parse populates `session_name` for existing file-backed sessions. No explicit data backfill needed — upgrading users come from main where `display_name` held only user renames (parsers never set it before this PR).

**`BackfillNameSource`:** deleted entirely.

---

## Read paths

**`sessionBaseCols`** (standard session queries → API responses):
```sql
COALESCE(display_name, session_name) AS display_name, ...
-- name_source removed
```
`scanSessionRow` unchanged — scans into `s.DisplayName` as before.

**`sessionFullCols`** (PG push read path):
```sql
display_name, session_name, ...
-- both columns separately, for push path to read SessionName
```

**`SidebarSessionIndexRow`:** `NameSource` field removed. `DisplayName` stays, now pre-resolved via COALESCE in the sidebar query.

**`db.Session` struct:**
- `DisplayName *string json:"display_name,omitempty"` — resolved value from COALESCE reads
- `SessionName *string json:"-"` — populated from `sessionFullCols` for PG push; never serialized

---

## Write paths

**`UpsertSession`:**
```sql
ON CONFLICT(id) DO UPDATE SET
    session_name = EXCLUDED.session_name,  -- always overwrite
    -- display_name NOT in SET clause       -- only RenameSession touches it
    ...
```
The `name_source` CASE expression is gone. No discriminator needed.

**`RenameSession`:** sets or clears `display_name` only. When `displayName = nil`, `display_name = NULL` and `session_name` already holds the parser name — no re-parse required.

**`humaRenameSession`:** the `ResetFileStateByPath + SyncPaths` block is deleted. Clearing a user rename immediately exposes `session_name` via the COALESCE; no file re-parse is needed.

**Three converters** (`toDBSession`, `sessionBatchWriteFromParsed`, `ImportClaudeAI`/`ImportChatGPT`): all set `Session.SessionName` via `ParsedSessionName(sess)`. Never set `Session.DisplayName`.

**`ParsedSessionNameFields`** → replaced by `ParsedSessionName(sess parser.ParsedSession) *string`: returns `&sess.SessionName` when non-empty, nil otherwise. One return value.

---

## PostgreSQL

**`coreDDL`:** `session_name TEXT` replaces `name_source TEXT`.

**Push INSERT/ON CONFLICT:** `session_name = EXCLUDED.session_name`. No CASE, no `name_source`.

**Push fingerprint:** `session_name` replaces `name_source`.

**`pgSessionCols`** (PG read path): `COALESCE(display_name, session_name) AS display_name`. `scanPGSession` scans into `s.DisplayName` as before. `name_source` removed.

**`CheckSchemaCompat`:** does NOT probe `session_name` — it is a write column, not required for reads. (Same reasoning applied when we removed `name_source` from the probe.)

---

## Cleanup (deleted with no replacement)

**Code:**
- `name_source` column: SQLite schema, `migrateColumns`, `upsertSessionSQL` CASE, PG DDL, push INSERT, push fingerprint, `scanPGSession`
- `NameSource *string` field: `db.Session`, `db.SidebarSessionIndexRow`
- `BackfillNameSource()` and its call in `db.Open`
- `ParsedSessionNameFields()` helper
- `ResetFileStateByPath()` in `internal/db/sessions.go`
- `ResetFileStateByPath + SyncPaths` block in `humaRenameSession`
- `hasNameSource` probe and pre-feature branch in `CopySessionMetadataFrom` (`orphaned.go`)

**Tests:**
- `TestResetFileStateByPathClearsAllSharers`
- `TestBackfillNameSourceSkipsImportedSessions`
- `TestNameSourceMigrationBackfillsUserRenames`
- `name_source_pgtest_test.go` tests — replaced with `session_name` equivalents
- `TestGetSessionPopulatesNameSourceInternally` — rewritten for `session_name`

---

## Invariants

1. Only `RenameSession` writes `display_name`. Parsers and importers never touch it.
2. Only parsers and importers write `session_name`. `RenameSession` never touches it.
3. Every read query resolves `COALESCE(display_name, session_name) AS display_name`. No caller receives raw unresolved fields.
4. The frontend receives one `display_name` field and is unaware of `session_name`.
5. `session_name` in both SQLite and PG is always the most recently parsed value — freely overwritten, never protected.

---

## What does not change

- Frontend code (already cleaned up in simplification)
- `parser.ParsedClaudeSession` extraction logic (only the field name changes)
- `RenameSession` API contract
- `dataVersion = 34` rationale (re-parse needed to populate `session_name`)
- `CopySessionMetadataFrom` overlay logic for `display_name` (user renames still copied across resync)
