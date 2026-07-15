# Preserve legacy SQLite archives during schema repair

## Problem

AgentsView historically treated several missing SQLite columns as a signal to
delete and recreate `sessions.db`. That loses sessions which no longer have a
source transcript on disk, even though the missing columns can be added
non-destructively.

The original pull request changed startup to repair those archives before
initializing the current schema, but its repair catalog added only one sentinel
column from each historical transition. Real archives from those releases also
lack companion columns, so current index creation or later queries still fail.
The legacy insights transition also replaced a single `date` value with
`date_from` and `date_to`; adding empty range columns without copying that value
discards archived insight semantics. Retaining the old `date` column afterward
is also unsafe because it is `NOT NULL` without a default while current insight
inserts omit it. The old lookup index has the same name as the current index, so
`CREATE INDEX IF NOT EXISTS` would also preserve the obsolete index shape. The
original repair call also passed a raw `*sql.DB` where a guarded writer handle
was required, so the branch did not compile.

## Goals

- Never delete, truncate, recreate, or replace a persistent SQLite archive to
  handle these historical schema transitions.
- Repair the complete table shapes introduced by the legacy transitions before
  `schema.sql` creates indexes that reference the new columns.
- Preserve existing sessions, messages, tool calls, and insights.
- Copy a legacy insight's `date` into both `date_from` and `date_to`.
- Leave repaired archives immediately able to accept current insight writes.
- Mark a repaired database for the normal non-destructive full resync, with the
  marker surviving a restart until resync succeeds.
- Keep the repair atomic so a failed startup cannot leave a partially upgraded
  archive.
- Cover real historical schema shapes with behavior-focused regression tests.

## Non-goals

- Do not synthesize parser-derived values during schema repair. The existing
  full-resync path remains responsible for repopulating those fields when
  source transcripts are available and preserving orphaned archive rows when
  they are not.
- Do not reorder or broaden every modern additive column migration.
- Do not change PostgreSQL or CockroachDB schemas. This is a SQLite file-startup
  compatibility path.

## Design

### Complete legacy repair catalog

One catalog will describe every column from the historical transitions that
previously triggered a rebuild:

- `sessions.parent_session_id`
- `sessions.user_message_count`
- `sessions.relationship_type`
- `tool_calls.tool_use_id`
- `tool_calls.input_json`
- `tool_calls.skill_name`
- `tool_calls.result_content_length`
- `tool_calls.subagent_session_id`
- `insights.date_from`
- `insights.date_to`

The schema probe and the pre-initialization repair will use the same catalog so
detection cannot drift from repair. A missing catalog column marks the schema
for repair. Applying the catalog skips a table that does not yet exist; the
normal `schema.sql` initialization creates such tables afterward.

Modern additive migrations remain in their existing post-initialization path.
This limits pre-initialization work to columns required by actual legacy archive
shapes and by schema-time indexes.

### Transactional repair and data migration

When the probe detects a legacy schema, `openAndInit` will invoke the repair
through the database's guarded writer handle. The repair will run in one SQLite
transaction and will:

1. add every missing catalog column to an existing table;
1. detect the historical `insights.date` column;
1. copy non-empty legacy dates into empty `date_from` and `date_to` values;
1. drop the legacy lookup index and obsolete `date` column; and
1. lower a current `PRAGMA user_version` by one so the required resync remains
   observable after a process restart.

Older `user_version` values remain unchanged. A database from a newer binary is
still rejected before any repair begins. Any repair or commit error aborts
startup and rolls back the transaction, leaving the archive available for a
later retry. The embedded SQLite version supports `ALTER TABLE ... DROP COLUMN`;
the repair does not recreate the database or the insights table.

After the repair commits, ordinary schema initialization can create all current
indexes safely, including the current four-column insights lookup index.
Existing column migrations and data backfills then run as they do for current
archives. `Open` exposes `NeedsResync()` for either a stale data version or a
repaired legacy schema and does not stamp the version current until the
established resync workflow succeeds.

### Regression tests

The synthetic test which creates a current database and drops one indexed column
will be replaced with fixtures based on representative historical `schema.sql`
snapshots:

- a pre-parent-link archive, covering preservation when the earliest rebuild
  sentinel is absent; and
- a single-date-insights archive from before structured tool-call metadata,
  user-message counts, and session relationship columns.

The fixtures will insert literal session, message, tool-call, and insight rows
before calling the public `Open` path. Assertions will verify observable
behavior and persisted state:

- the archive rows remain readable after startup;
- all catalog columns and the current indexes that depend on them exist;
- `date_from` and `date_to` both equal the historical insight date;
- a current `InsertInsight` succeeds immediately after repair and its row is
  readable;
- the insights lookup index uses `type`, `date_from`, `date_to`, and `project`;
- `NeedsResync()` is true after repair; and
- closing and reopening the repaired archive still reports the pending resync.

The tests use standalone temporary databases and literal expectations. They do
not inspect source text or derive expected values with the migration logic.

## Validation

Implementation will follow a red-green cycle: add the representative fixture,
observe it fail against the incomplete repair, make the minimal repair changes,
and rerun the focused test. Before handoff, run Go formatting and vetting plus
the relevant unit, lint, integration, and benchmark commands that cover the CI
jobs currently failing. Review the final diff separately for archive safety,
restart behavior, and unrelated changes.
