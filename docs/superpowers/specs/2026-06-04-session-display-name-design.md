# Session Names From Agent Sessions — Design

**Status:** Approved (brainstorming complete; ready for implementation plan)

**Goal:** Persist agent-provided session names into the database and show them
in the sidebar. This covers Claude Code's `/rename` command (currently ignored)
and the native names that seven other parsers already extract but that are
silently dropped before reaching the database. A manual in-app rename always
wins, and agent-provided names are shown only when the user opts in via an
Appearance toggle.

**Tech stack:** Go (SQLite + optional PostgreSQL), Svelte 5 / TypeScript.

## Background

The `display_name` mechanism is roughly 80% built but not wired end to end:

- The `sessions.display_name` column, the manual rename UI, `db.RenameSession`,
  and the sidebar's `displayLabel` precedence all work (added for the manual
  rename feature in #95).
- Seven parsers populate `ParsedSession.DisplayName` from their format's native
  name field (`chatgpt`, `claude_ai`, `forge`, `hermes`, `kiro`, `piebald`,
  `cortex`).
- **The gap:** `toDBSession` (`internal/sync/engine.go`), the only converter in
  the live disk-sync pipeline, never copies `DisplayName` into the database row.
  So no parser-derived name survives a normal sync; only the importer path
  persists it. The upsert's `ON CONFLICT DO UPDATE` also deliberately omits
  `display_name`, which protects manual renames but also blocks backfill.
- Claude Code (`claude.go`) does not extract a name at all. Its `/rename` lives
  inline in the transcript as a `system` / `local_command` record:
  `<command-name>/rename</command-name> ... <command-args>NAME</command-args>`,
  followed by `<local-command-stdout>Session renamed to: NAME</...>`.

Terminology: the user-facing and plumbing term is **session name** (`/rename`,
`display_name`, `RenameSession`). Some parsers' internal field is called
`title`; that is an implementation detail and is not used in user-facing text.

## Out of scope

- **OpenCode** keeps routing its LLM-generated name through `first_message`
  (`opencode.go`). That name therefore stays visible even when the Appearance
  toggle is off; migrating it to `display_name` would make it disappear by
  default — a regression — so we leave it untouched.
- **Codex, Gemini, Copilot** have no session-name concept in their formats; no
  extraction is added.
- No change to the manual rename UX itself (input, context menu) beyond the
  ownership bookkeeping described below.

## Ownership model

A single column, `name_source`, is the source of truth for both ownership and
the frontend display gate.

`name_source TEXT` values:

- `'user'` — set by an in-app rename. Pins the name: agent renames never
  overwrite it.
- `'agent'` — set by the sync pipeline from a parsed name (native name or Claude
  `/rename`).
- `NULL` — unset; no custom name.

Rules ("user pins, clear un-pins"):

- A manual rename sets `display_name` and `name_source='user'`.
- Clearing the manual name (`RenameSession(id, nil)`) sets `display_name=NULL`
  and `name_source=NULL`, reverting the session to agent ownership so the latest
  agent name flows back in on the next sync.
- The sync pipeline writes a parsed name and `name_source='agent'` **only when
  the row is not `'user'`-owned**.

## Design

### 1. Data model

- Add `name_source TEXT` to the `sessions` table (SQLite and PostgreSQL) via a
  non-destructive `ALTER TABLE` migration. The database is a persistent archive;
  no drop/recreate.
- Add `name_source` to the sidebar session-index row (SQLite and PostgreSQL,
  keeping parity) so the client can apply the display gate.
- Bump `dataVersion` (`internal/db/db.go`, currently 31 → 32). The parser logic
  changes, so existing sessions must be re-parsed to backfill `display_name` /
  `name_source`.

### 2. Parser layer

- **`claude.go`:** in the main scan loop in `ParseClaudeSession`, detect the
  `/rename` envelope on `system` / `local_command` records and capture the
  argument, reusing the existing `xmlCmdNameRe` / `xmlCmdArgsRe`. Add a
  `displayName` field to `claudeSessionMeta`, applied in `applyTo`. **The last
  `/rename` in the file wins.** A `/rename` with an empty argument clears the
  captured name (so an agent-owned name reverts to the first-message preview; a
  user-owned name is unaffected because the parser only ever sets
  `name_source='agent'`).
- The other seven parsers already set `ParsedSession.DisplayName`; no parser
  changes needed there.

### 3. Sync and persistence

- **`toDBSession`** (the core gap): map `pw.sess.DisplayName` → `s.DisplayName`,
  and set `name_source='agent'` when a parsed name is present.
- **Upsert** (`upsertSessionSQL`): on conflict, write the parsed name and source
  only when the existing row is not user-owned, e.g.:
  - `display_name = CASE WHEN sessions.name_source = 'user' THEN sessions.display_name ELSE excluded.display_name END`
  - `name_source  = CASE WHEN sessions.name_source = 'user' THEN sessions.name_source ELSE excluded.name_source END`
  - This lets agent renames update an agent-owned row on every sync while a
    manual rename pins permanently.
- **`RenameSession`:** set `name_source='user'` on rename; set
  `name_source=NULL` when clearing.
- **Full-resync swap:** `CopySessionMetadataFrom` must copy `name_source`
  alongside `display_name` so the user/agent distinction survives a rebuild.
- **PostgreSQL push** (`internal/postgres/push.go`, `schema.go`, `sessions.go`):
  carry `name_source` through the push and read paths so multi-machine shared
  access stays consistent.

### 4. Live refresh

A `/rename` appends lines to the transcript, so the file watcher fires. The
incremental Claude path (`ParseClaudeSessionFrom`) returns only messages, not
session metadata, so it would not refresh `display_name` on its own. Make the
incremental path **bail to a full reparse when it encounters a `/rename` line**,
reusing the existing `ErrClaudeIncrementalNeedsFullParse` /
`IsIncrementalFullParseFallback` mechanism (already used for subagent linkage
and chunk merging). The full reparse updates `display_name`, and the existing
session-update SSE event pushes the new name to the sidebar — so the left pane
changes as the rename happens.

### 5. Frontend

- Expose `name_source` on the sidebar session-index row type
  (`SidebarSessionIndexRow` / client `SkinnyRow`).
- Add an Appearance preference **"Session names"** in
  `AppearanceSettings.svelte` as its own row (alongside Theme and Message
  layout, not in the Block visibility checkbox list). Persist it in the client
  `ui` store (localStorage), matching theme / layout / block-visibility.
  **Default off.**
- Update `SessionItem.svelte` `displayLabel` precedence to:
  1. If `display_name` is set **and** (`name_source === 'user'` **or** the
     "Session names" preference is on) → show `display_name`.
  1. Otherwise → existing behavior (teammate task extraction, then
     `previewMessage(first_message)`, then project fallback).
- Net behavior: manual renames always show; agent-provided names show only when
  the toggle is on; toggling is instant (no resync — it only changes the
  label-selection logic).

## Data flow

```
Claude /rename in transcript ─┐
native name (6 parsers) ──────┼─► ParsedSession.DisplayName
                              │        │
                              │        ▼
                              │   toDBSession ─► db.Session{DisplayName, name_source='agent'}
                              │        │
                              │        ▼
                              │   upsert (CASE: skip when name_source='user')
                              │        │
manual rename ────────────────┼─► RenameSession (name_source='user' / NULL)
                              │        │
                              │        ▼
                              │   sessions table ─► sidebar index row (+name_source)
                              │        │
                              │        ▼ SSE
                              └─► SessionItem.displayLabel (gated by name_source + "Session names" toggle)
```

## Error handling and edge cases

- **Multiple renames:** last `/rename` in the file wins (linear scan, last write
  to `meta.displayName`).
- **Empty `/rename`:** clears the agent-owned name; never affects a user-owned
  name (parser only sets agent source).
- **DAG forks:** a file can yield multiple `ParsedSession`s; `applyTo` sets the
  captured name on each, consistent with how `cwd` / `gitBranch` are applied.
- **Existing sessions:** backfilled by the `dataVersion` bump (re-parse). The
  `CASE` upsert ensures backfill never overwrites a manual rename.
- **Clearing then re-rename in agent:** clearing sets `name_source=NULL`; the
  next sync re-applies the latest agent name. Acceptable per the agreed model.

## Testing

- **Parser (`claude_test.go`):** table-driven — single rename, multiple renames
  (last wins), empty rename clears, no rename. Assert
  `ParsedSession.DisplayName` via testify.
- **DB (`sessions_test.go`):** migration adds the column; upsert preserves
  `name_source='user'` against an incoming agent name; upsert updates an
  agent-owned row; `RenameSession` sets `'user'` and clears to `NULL`.
- **Sync (`engine_test.go`):** `toDBSession` carries `DisplayName` /
  `name_source`; a `/rename` line triggers the full-parse fallback;
  `CopySessionMetadataFrom` preserves `name_source` across resync.
- **PostgreSQL (`pgtest`):** push/read round-trips `name_source`.
- **Frontend:** `displayLabel` honors the toggle and the user/agent distinction
  (`SessionItem` / `SessionList` / store tests); the Appearance toggle persists.

## Files touched (indicative)

| File                                                             | Action | Responsibility                                                                   |
| ---------------------------------------------------------------- | ------ | -------------------------------------------------------------------------------- |
| `internal/db/schema.sql`, `internal/db/db.go`                    | Modify | Add `name_source` column + migration; bump `dataVersion`                         |
| `internal/db/sessions.go`                                        | Modify | Upsert `CASE` rules; `RenameSession` source; index row                           |
| `internal/parser/claude.go`                                      | Modify | Extract `/rename`; `claudeSessionMeta.displayName`                               |
| `internal/sync/engine.go`                                        | Modify | `toDBSession` mapping; incremental `/rename` fallback; `CopySessionMetadataFrom` |
| `internal/postgres/{schema,push,sessions}.go`                    | Modify | `name_source` parity                                                             |
| `frontend/src/lib/stores/ui.svelte.ts`                           | Modify | "Session names" preference (localStorage)                                        |
| `frontend/src/lib/components/settings/AppearanceSettings.svelte` | Modify | Add the toggle row                                                               |
| `frontend/src/lib/components/sidebar/SessionItem.svelte`         | Modify | `displayLabel` gating                                                            |
| `frontend/src/lib/api/client.ts`, sidebar index types            | Modify | Surface `name_source`                                                            |
| Tests across the above                                           | Add    | Parser, DB, sync, PG, frontend                                                   |
