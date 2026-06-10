# Design Note: Session-Name Cascade (No-Toggle Simplification)

**Branch**: `feat/session-display-name`  
**PR**: #601 (DRAFT)  
**Date**: 2026-06-08  
**Status**: Evaluation only — no code changes

---

## Problem Statement

PR #601 introduced `name_source` ('user' | 'agent' | NULL) to discriminate
`display_name`, plus a "Use session names" Appearance toggle (default off) to
hide agent-provided names. Each roborev review round found another surface —
sidebar, breadcrumb, delete modal, trash, pinned list, search/command-palette,
upload converter, importer, PG schema probe, PG push fingerprint — that needed
to learn the gate. The PR became a churn treadmill.

Maintainer guidance: **drop the toggle**. A coalesce-based cascade is the
expected implementation. The author agrees. This note evaluates what that means
in practice, with focus on the crux: how to enforce "user rename beats agent
name" without threading a discriminator to the frontend.

---

## The Crux

Three requirements pull in different directions:

1. **User rename wins over agent name** — always, including after re-sync or
   PG push.
2. **Live agent `/rename` updates flow through** — if the agent adds or changes
   its `/rename` directive and the user has NOT set a custom name, the new
   agent name should appear after the next sync.
3. **No discriminator in the frontend** — the toggle-gating that caused the
   treadmill came from passing `name_source` to the frontend so it could decide
   whether to show a name. The new design should give the frontend a single
   resolved name, not raw + discriminator.

Requirements 1 and 2 together mean a pure "last-one-wins" or "first-write-wins"
approach doesn't work: some marker must distinguish "user renamed this" from
"parser extracted this." The question is where that marker lives and what form
it takes.

---

## Options Evaluated

### (a) Backend-only discriminator, write-time only

Keep a discriminator column (`name_source` or rename to `user_renamed BOOLEAN`)
in SQLite/PG, but **never expose it in API responses or frontend types**. The
upsert CASE uses it to decide whether to overwrite `display_name`. The frontend
receives only `display_name` (resolved), falling back to `first_message`.

**Clobber-safety**: Same CASE expression as today (`WHEN name_source = 'user'
THEN keep`). Correct.

**Live agent updates**: When parser emits a new `/rename`, `toDBSession` sends
`display_name = "agent name", name_source = "agent"`. Upsert CASE: no user
rename → takes agent value. Correct.

**Re-sync (CopySessionMetadataFrom)**: Copies only `name_source = 'user'` rows
(as today). Agent names are re-populated by re-parse.

**Import**: `importer.go` currently hand-rolls a "preserve existing name if
different" guard instead of using `name_source`. With option (a) it simplifies:
always pass `name_source = "agent"` when `DisplayName != ""`, let the upsert
CASE protect user renames automatically.

**Upload**: `sessionBatchWriteFromParsed` currently drops `DisplayName`
entirely (roborev finding). Fix: populate `DisplayName` and `NameSource =
"agent"` from `ParsedSession`, same as `toDBSession`.

**PG parity**: `name_source` stays in the PG DDL, fingerprint, and push path
(all already present on the branch). The compat probe keeps probing it. No
rework needed.

**Frontend threading**: None. `name_source` is stripped from
`sessionBaseCols`/`sessionFullCols` JSON, from sidebar index rows, from search
results. The frontend `Session` and `SidebarSessionIndexRow` types lose
`name_source`. Every display site just reads `display_name`, falling back to
`first_message`.

**`visibleSessionName` helper**: Replaced by a one-liner: `session.display_name
?? normalizePreview(session.first_message) ?? session.project`.

**Hydration logic** (`needsVisibleHydration` in SessionList): Simplifies to
`!session.display_name && !session.first_message` — no toggle arithmetic.

**Roborev finding class**: Eliminated. There is no frontend `name_source` to
miss threading to. Any new display surface just reads `display_name`.

---

### (b) Two fields: user name in `display_name`, agent name in `agent_name`

`display_name` becomes user-only. A new `agent_name TEXT` column holds
parser-extracted names. Reads use `COALESCE(display_name, agent_name)`.

**Clobber-safety**: Structural — the two paths never touch each other's column.

**Live agent updates**: Sync writes to `agent_name`; `display_name` (user) is
untouched. Correct.

**Re-sync / import / upload**: All write to `agent_name`. `display_name` is
never overwritten by parsers.

**Migration complexity**: Existing `display_name` rows have mixed provenance.
Need to split: rows with `name_source = 'user'` → keep in `display_name`;
rows with `name_source = 'agent'` or NULL → move to `agent_name`, clear
`display_name`. This requires reading and transforming existing data during
migration, not just adding a column.

**Schema surface**: Every query that currently reads `display_name` as the name
to show must become `COALESCE(display_name, agent_name)`. That's
`sessionBaseCols`, `sessionFullCols`, sidebar index, search COALESCE, PG
sidebar query, PG session scan, PG push fingerprint, PG DDL. More touches than
option (a), not fewer.

**PG DDL**: New `agent_name` column in coreDDL, compat probe, fingerprint,
push, read paths.

**Assessment**: Structurally cleaner (invariant is enforced by field
separation, not a CASE flag) but the migration and query-surface overhead is
larger than option (a). The gain is conceptual purity; the cost is real.

---

### (c) User renames in a side table, agent name in `display_name`

A `session_renames (session_id, display_name, created_at)` overlay table,
analogous to `starred_sessions` / `pinned_messages`. `sessions.display_name`
holds only parser-extracted names. Reads COALESCE the side table on top.

**Clobber-safety**: Perfect — parsers never touch the side table.

**Live agent updates**: `sessions.display_name` updated by sync. Side table
stable.

**PG push**: Two tables to synchronize, each with its own change-detection
logic. The push path is already complex; a second user-data overlay adds
non-trivial lift.

**Migration**: Existing `name_source = 'user'` rows must be moved out of
`sessions.display_name` into the new side table. More invasive than (a).

**Assessment**: Most structurally correct, most complex to implement and push.
Overkill for the use case. The `starred_sessions` analogy is apt only in
structure; user renames are not nearly as high-volume or independent.

---

### (d) Write-time precedence: only fill `display_name` when unset

Never overwrite a non-NULL `display_name`. Once set (by user or parser), it
stays until explicitly cleared.

**Clobber-safety**: Partial — a user rename can never be overwritten, but
neither can a stale agent name. Once an agent sets a name, a user who wants to
clear back to preview must rename-then-clear, and subsequent agent renames
still won't flow through.

**Live agent updates**: Broken. After the first agent `/rename`, subsequent
renames do not update (because `display_name IS NOT NULL`). This fails
requirement 2.

**Assessment**: Does not satisfy the requirements. Ruled out.

---

## Recommendation: Option (a)

Keep `name_source` as a **backend-only write-time discriminator**; strip it
from all API responses and frontend types.

**Rationale**:

- The current UPSERT CASE is correct and already tested. No logic change needed
  there.
- `BackfillNameSource` (v34 migration) already handles pre-feature rows.
- The PG DDL, fingerprint, and push path already carry `name_source` (v34
  branch commits). No rework needed for PG.
- All frontend changes reduce to: delete `name_source` from every DTO/type,
  delete `visibleSessionName`, delete `showSessionNames`, simplify
  `needsVisibleHydration`. Net frontend change is deletion, not addition.
- Option (b)'s split-field approach is cleaner in theory but requires migrating
  mixed-content `display_name` rows and touching every COALESCE query site —
  more code changes for the same user-visible outcome.

The one mental model shift: `name_source` looks like it wants to reach the
frontend (a discriminator column on a shared table), but in the new design it
is purely an internal write-guard, like a row-level lock hint. A short comment
in `upsertSessionSQL` explaining this prevents future readers from adding it
back to API responses.

---

## REVERT List

Everything introduced solely to support the toggle/gate:

**DB (SQLite)**
- Remove `name_source` from `sessionBaseCols` and `sessionFullCols`
  (`internal/db/sessions.go`)
- Remove `NameSource` from the `Session` and `SidebarSessionIndexRow` struct
  JSON tags (keep the Go field, just remove it from JSON output)
- Keep `name_source` in `upsertSessionSQL` CASE and in `RenameSession` (write
  paths)
- Keep `BackfillNameSource` and `migrateColumns` for `name_source` (v34
  migration)

**PG**
- Remove `name_source` from `pgSessionCols` and `scanPGSession`
  (`internal/postgres/sessions.go`) — the column stays in DDL and push, just
  not returned to callers
- Keep `name_source` in `coreDDL`, `CheckSchemaCompat`, `sessionPushFingerprint`,
  and the push `INSERT ... ON CONFLICT` (write paths must still carry it)

**Frontend**
- Delete `showSessionNames` state and toggle from `ui.svelte.ts`
- Delete the Appearance toggle UI component/setting
- Delete `frontend/src/lib/utils/sessionName.ts` (`visibleSessionName` +
  `agentNamesVisible`)
- Delete `name_source` field from `SessionRow`/`SidebarRow` TypeScript types
  in `sessions.svelte.ts`
- Delete `name_source` from sidebar index TS type in any shared types file
- Simplify `needsVisibleHydration` in `SessionList.svelte`: `return
  !session.display_name` (no toggle check)
- Replace every `visibleSessionName(session)` call with
  `session.display_name ?? normalizePreview(session.first_message) ??
  session.project` inline or via a simple `resolvedSessionName(session)`
  that has no toggle logic
- Delete related tests: `showSessionNames` toggle tests in `ui.test.ts`;
  `name_source` threading tests in `sessions.test.ts`; all
  `sessionName.test.ts`

---

## KEEP List

**Parser extraction** — keep everything that puts names INTO `ParsedSession`:
- `internal/parser/claude.go`: `extractRenameName`, `/rename` extraction in
  `ParseClaudeSession` + `ParseClaudeSessionFrom` fallback
- All 7 other parsers that set `ParsedSession.DisplayName`: chatgpt, claude_ai,
  cortex, forge, hermes, kiro_ide, piebald

**Write-time protection** — keep the backend invariants:
- `upsertSessionSQL` CASE (`WHEN name_source = 'user' THEN keep`)
- `RenameSession` setting/clearing `name_source`
- `BackfillNameSource` and `migrateColumns` for `name_source`

**PG write paths** — keep `name_source` in:
- `coreDDL` (column definition)
- `CheckSchemaCompat` (probes the column)
- `sessionPushFingerprint` (change detection)
- Push `INSERT ... ON CONFLICT` (writes the column)

**dataVersion 34** — keep the bump; re-parse is needed to populate agent names
into existing rows.

---

## Converter Consolidation

Roborev flagged that `upload.go` drops `ParsedSession.DisplayName` entirely.
The importer hand-rolls its own "preserve existing name" guard that doesn't set
`NameSource`. `toDBSession` in `engine.go` is the only correct implementation.

Root cause: three independent functions that each need to do the same
`DisplayName + NameSource` population, with no shared path.

**Fix**: extract a `parsedSessionBase` helper in a new file
`internal/db/parsedsession.go` (or in `internal/sync/engine.go` if kept
internal to sync):

```go
// parsedSessionBase builds the core db.Session fields from parsed session
// metadata, excluding file-system fields (callers add those).
// It is the single place that couples DisplayName and NameSource so all
// three converters (sync, upload, import) stay consistent.
func parsedSessionBase(sess parser.ParsedSession, msgs []parser.ParsedMessage) db.Session
```

This function sets `DisplayName` and `NameSource = ptr("agent")` together when
`sess.DisplayName != ""`. All three converters call it and then fill their own
file-path fields.

The importer's existing "preserve existing name" guard is also wrong: it
compares the incoming parsed name against the stored name and keeps the stored
one if they differ. This accidentally preserves a stale user rename only by
coincidence (the user rename and the agent name would differ). With option (a)
in place, the upsert CASE handles this correctly without any converter-level
guard. Remove the guard from `importer.go`; let the CASE do its job.

---

## Migration Plan

The branch already bumped `dataVersion` to 34 and added:
- `migrateColumns` entry for `ALTER TABLE sessions ADD COLUMN name_source TEXT`
- `BackfillNameSource`: stamps `name_source = 'user'` on pre-feature rows that
  have a non-NULL `display_name` (treating all legacy renames as user-sourced)

These remain correct and unchanged under option (a). No additional migration is
needed for the backend.

**What changes in migration**:
- Remove `name_source` from `sessionBaseCols` JSON output means old clients
  that expect `name_source` in the JSON will no longer see it. Since this is a
  local tool with no versioned API contract, that's acceptable.
- PG `name_source` column stays; existing PG deployments already have it if
  they ran the v34 DDL (`AddMissingColumns`). No column drop needed.

If the branch is eventually rebased and the v34 bump consolidated: `dataVersion
= 34` rationale changes to "Claude `/rename` and native conversation titles now
populate `display_name`; re-parse needed to backfill agent names into existing
rows." The migration mechanics (mtime reset + re-parse) are unchanged.

---

## Why Roborev's Finding Class Disappears

Every finding roborev raised was of the form: "surface X reads or displays
`display_name` but does not gate on `name_source`, so agent-named sessions
appear when the toggle is off."

With no toggle, there is no gate. Every surface that reads `display_name` is
correct by construction — if `display_name` is set, it's either a user rename
or an agent name that the user has not overridden, and both should show. A new
display surface added six months from now just reads `display_name`; nothing
else to remember.

The toggle was the source of the treadmill, not `name_source` itself. Removing
the toggle removes the entire class of "forgot to gate" findings. Keeping
`name_source` as a backend write-guard does not reintroduce the class because
the guard is invisible to display code.

---

## Summary

| Criterion | (a) Backend discriminator | (b) Two fields | (c) Side table | (d) Write-once |
|---|---|---|---|---|
| Clobber-safe | Yes (CASE) | Yes (struct) | Yes (struct) | Partial |
| Live updates | Yes | Yes | Yes | No |
| Re-sync | Copies 'user' only | Copies display_name only | Side table stable | N/A |
| Import/upload fix | Set NameSource="agent" | Write to agent_name | Write to sessions | N/A |
| PG rework | None (already done) | New column + paths | New table + push | N/A |
| Frontend threading | None | None | None | None |
| Migration | Keep as-is | Split existing rows | Move rows to table | N/A |
| Roborev class | Eliminated | Eliminated | Eliminated | N/A |
| **Net code delta** | **Mostly deletions** | **More additions** | **Most additions** | **N/A** |

**Recommendation: Option (a).** The toggle removal eliminates roborev's finding
class. `name_source` stays as a silent backend write-guard — strip it from API
responses, keep it in upsert logic and PG write paths. Consolidate the three
parse→DB converters via `parsedSessionBase`. Result: a PR that is net
negative in lines of code, with no frontend discriminator threading, and a
display cascade (`display_name ?? first_message preview ?? project`) that any
new surface can use without ceremony.
