# Session Termination Status

> **Status: as-implemented addendum below.** This spec captured the original
> design; the implementation evolved during build-out. Read this section
> first if you want the shipped contract; the rest of the document is
> retained for historical context.
>
> ### Final taxonomy
>
> Persisted enum on `sessions.termination_status`:
>
> - `clean` — agent stopped for a non-orphan, non-truncated reason that
>   isn't an explicit "your turn" signal (e.g. Claude `max_tokens`,
>   `stop_sequence`).
> - `awaiting_user` — agent emitted an explicit "I'm done, your turn"
>   signal: Claude `end_turn` or Codex `task_complete`. Surfaces in the
>   UI as a champagne speech-bubble.
> - `tool_call_pending` — last assistant message has a `tool_use` block
>   without a matching `tool_result` strictly *after* it.
> - `truncated` — file was cut off mid-write (last line is invalid JSON).
> - `NULL` — unknown / not classified (non-Claude/non-Codex agents,
>   sessions in flight after an incremental append, or pre-bump rows
>   awaiting full resync).
>
> ### Derived UI tiers (`StatusDot`)
>
> Combination of recency + persisted status, evaluated in this order
> (first match wins):
>
> | Tier      | Recency       | Parser flag             | Visual                  |
> |-----------|---------------|-------------------------|-------------------------|
> | waiting   | < 10m         | `awaiting_user`         | champagne bubble        |
> | working   | < 1m          | not `awaiting_user`     | green pulse             |
> | idle      | 1m – 10m      | not `awaiting_user`     | dim dot                 |
> | quiet     | ≥ 10m         | `clean`/`NULL`/`awaiting_user` | grey dot         |
> | stale     | 10m – 60m     | `tool_call_pending`/`truncated` | amber dot       |
> | unclean   | ≥ 60m         | `tool_call_pending`/`truncated` | red dot         |
>
> Two precedence rules to note:
>
> - `awaiting_user` always wins over the time-based tier inside the
>   10m active window: a session that's actively writing but has
>   emitted `end_turn`/`task_complete` shows the waiting bubble, not
>   the working pulse.
> - Once an `awaiting_user` session ages past the 10m active window
>   it falls through to `quiet`, not `waiting` — the bubble is meant
>   to surface freshly-blocked sessions, not every long-completed
>   conversation. Same fall-through applies to `clean` and `NULL`.
>
> Recency uses `COALESCE(ended_at, started_at, created_at)`; this is
> approximate for in-flight sessions but is moved forward by every
> incremental append, so an actively-writing session ranks correctly.
>
> ### Filter contract
>
> `?termination=` accepts a comma-separated list of: `clean`,
> `awaiting_user`, `active`, `stale`, `unclean`, or empty/`all`. `active`
> is purely time-based (< 10m); `stale`/`unclean` require both the
> recency window AND a parser flag. Unknown values are silently ignored,
> matching the convention of other comma-separated filters in this
> codebase.
>
> ### Incremental sync
>
> `UpdateSessionIncremental` clears `termination_status` to `NULL` on
> every write (the classifier needs the full message slice; the
> incremental path only sees the new tail). The next full sync (≤ 15m
> later, or sooner via the file watcher) reclassifies. UI falls back to
> the time-based tier in the meantime.
>
> ### UI surfaces
>
> - Sidebar: each session row renders a `StatusDot`; group rows roll up
>   the freshest member's recency. Sort tiers (top→bottom):
>   working → waiting → idle → stale → quiet → unclean.
> - Sidebar filter: three multi-select pills (Active / Stale / Unclean)
>   tinted with their dot color.
> - Top Sessions analytics table: `Status` column renders `StatusDot`.
> - Detail page: no banner. The sidebar `StatusDot` plus the breadcrumb
>   convey the same signal — the original banner was removed during
>   review based on user feedback.

## Problem

Sessions in agentsview have an `ended_at` timestamp set to the last message
time, regardless of how the agent process actually exited. Crashes, kills, and
clean exits all look identical in the UI. Users have no way to surface
sessions that ended badly so they can investigate, retry, or clean up.

## Goal

Detect and display sessions whose source files show evidence of unclean
termination, so users can find them via filter, see them flagged on the detail
page, and rank them in analytics.

## Non-goals

- Crash detection during idle (gaps between turns where the agent died but
  the file shows no anomaly). This would require a runtime heartbeat from
  the agent, which is out of scope for a viewer.
- Cross-agent unification of termination semantics. We classify per agent and
  start with Claude Code; other agents stay `NULL` until their formats are
  examined individually.
- Recovery actions (resume, replay). Pure read-only surfacing.

## Data model

New nullable column on `sessions`. Added via the existing migration paths
(non-destructive ALTER TABLE) on both backends:

- **SQLite**: append an entry to the `migrations` slice in
  `internal/db/db.go` `migrateColumns()`. Also add the column to
  `internal/db/schema.sql` so fresh DBs start with it.
- **PostgreSQL**: append an entry to the `alters` slice in
  `internal/postgres/schema.go` `EnsureSchema()`. Also add it to `coreDDL`.

```sql
ALTER TABLE sessions ADD COLUMN termination_status TEXT;
CREATE INDEX IF NOT EXISTS idx_sessions_termination_status
    ON sessions(termination_status);
```

The index supports the filter query (`WHERE termination_status IN
('tool_call_pending', 'truncated')`) on both the sidebar list and the
analytics tab without scanning the full table.

Values:

| Value                 | Meaning                                                   |
| --------------------- | --------------------------------------------------------- |
| `'clean'`             | Parser examined the file and found no anomaly             |
| `'tool_call_pending'` | Final assistant turn has `tool_use` with no `tool_result` |
| `'truncated'`         | Final JSONL line failed to parse (interrupted write)      |
| `NULL`                | Parser did not classify (legacy row or unsupported agent) |

UI treats `tool_call_pending` and `truncated` as **unclean**. `NULL` is
**not** flagged — we never assert "unclean" without evidence, to avoid false
positives on agents whose formats we have not analyzed.

### Column plumbing (SQLite)

Adding the column requires touching every read site in `internal/db/sessions.go`:

- The three column-list constants: `sessionBaseCols`, `sessionPruneCols`,
  `sessionFullCols`.
- The four scan paths: `scanSessionRow`, `GetSessionFull`,
  `ListSessionsModifiedBetween`, `FindPruneCandidates`.
- The write path: `UpsertSession` INSERT/UPDATE columns and `?` placeholders.
- The Go struct: `Session.TerminationStatus *string` with JSON tag.

### Column plumbing (PostgreSQL)

The new column must be plumbed through these files:

- `internal/postgres/sessions.go` — many `SELECT … FROM sessions` sites
  (12+) plus their scan paths. Also `buildPGSessionFilter` (line 115)
  needs the same `clean`/`unclean` predicate as the SQLite
  `buildSessionFilter` so PG-backed `GET /api/sessions?termination=…`
  honors the filter (otherwise the API silently ignores the param when
  serving from PG).
- `internal/postgres/push.go` — `pushSession` INSERT/ON CONFLICT/WHERE
  clauses (line 688+); each enumerates every column.
- `internal/postgres/schema.go` — `coreDDL` and `alters` slice (already
  covered in the migration section above).

These files are **not** in the list:

- `internal/postgres/messages.go` — its joins on `sessions` (lines 226,
  252) only project display_name/first_message/ended_at/started_at/
  agent/project for search; doesn't need the new column.
- `internal/postgres/store.go` — its only `FROM sessions` query (line
  56) selects only `message_count` and `updated_at` for SSE versioning;
  doesn't need the new column.
- `internal/postgres/analytics.go` — its session queries don't carry
  session columns through (they aggregate or join on `s.id`).

### Analytics filter plumbing

The UI section requires the analytics-tab Status filter to actually
filter analytics responses (the unclean count pill click-toggles the
same filter). Wire it through:

- `db.AnalyticsFilter` (`internal/db/analytics.go:44`) gains a
  `Termination string` field.
- `parseAnalyticsFilter` (`internal/server/analytics.go:49`) reads
  `q.Get("termination")` and populates `f.Termination`. All analytics
  endpoints flow through this single helper, so they pick it up
  automatically.
- The SQLite analytics WHERE builder is the method
  `(AnalyticsFilter).buildWhere(dateCol)` in
  `internal/db/analytics.go` (called at lines 201, 333, 532, 722,
  935). Append the same `clean`/`unclean` predicates inside it as
  `buildSessionFilter` does.
- The PostgreSQL counterpart is the standalone function
  `buildAnalyticsWhere` in `internal/postgres/analytics.go:65`.
  Apply the same predicate. Top Sessions queries flow through this
  builder, so the analytics tab's "Top Sessions" ranking respects
  the filter.
- `db.TopSession` (`internal/db/analytics.go:2040`) gains a
  `TerminationStatus *string` field, and the SQLite/PG top-session
  SELECT/scan paths populate it so the analytics Top Sessions table
  can render the Status column from API data.

Without these changes, the analytics chip can be set while charts and
the Top Sessions ranking still return unfiltered data.

`CheckSchemaCompat` (`postgres/schema.go:785`) is **left strict-for-now**:
it adds `termination_status` to its probe so that `pg serve` against a
too-old remote schema fails loudly rather than silently dropping the
column. Operators upgrade the remote first, then the binary that serves
it.

## Detection logic

A new helper file `internal/parser/termination.go` exposes the status
constants and a classifier that operates on the parser's existing
`[]ParsedMessage` shape (see `internal/parser/types.go`):

```go
type TerminationStatus string

const (
    TerminationClean           TerminationStatus = "clean"
    TerminationToolCallPending TerminationStatus = "tool_call_pending"
    TerminationTruncated       TerminationStatus = "truncated"
)

// Classify returns a status given a parsed message slice and a sentinel
// from the file scanner. Returns "" (unknown) if neither evidence is
// available — the caller should leave the session column NULL.
func Classify(messages []ParsedMessage, fileTruncated bool) TerminationStatus
```

`ParsedSession` (in `internal/parser/types.go`) gains a
`TerminationStatus string` field. The Claude parser populates it per
`ParseResult` returned by `ParseClaudeSession` — Claude transcripts can
contain DAG forks that split into multiple sessions, so each fork is
classified independently from its own message tree.

The two checks:

- **Tool-call-pending**: walk the messages of one `ParseResult` from the
  end, find the last assistant message, and inspect its `ToolCalls`. If
  any `ParsedToolCall.ToolUseID` is not referenced by a `ParsedToolResult`
  in a subsequent user message in the same fork, status is
  `tool_call_pending`.
- **Truncated**: the current scanner uses `gjson.Valid(line) → continue`
  (`claude.go:84-86`), silently skipping malformed lines. `lineReader`
  already returns a partial trailing line at EOF without dropping it
  (`linereader.go:88-94`), so no scanner-internal change is needed. The
  parse loop instead tracks whether the *last* non-empty line returned by
  `lineReader.next()` failed `gjson.Valid`. If yes, `fileTruncated = true`.
  Implementation is free to refine with additional signals (e.g.,
  whether the file ends with a newline) if real-world fixtures show
  false positives or negatives — the spec only requires that truncated
  files end up classified as `truncated`.

### Sync-layer plumbing

The conversion `ParsedSession` → `db.Session` happens in
`toDBSession` (`internal/sync/engine.go:2733`). It copies fields
field-by-field and uses helpers like `strPtr()` to convert empty
strings to `nil` for nullable columns. Add a line that sets
`s.TerminationStatus = strPtr(pw.sess.TerminationStatus)`.

`db.Session` gets a `TerminationStatus *string` field (matching the
nullable-text pattern of `DisplayName`, `DeletedAt`, etc.).

Other agents (`codex.go`, `gemini.go`, `cursor.go`, etc.) leave the
field empty in `ParsedSession`. `strPtr("")` returns `nil`, so those
rows stay `NULL` in the DB.

### Incremental sync caveat

Claude session files are appended to over time; the sync engine uses
`UpdateSessionIncremental` for fast-path updates of counts, timestamps,
and token aggregates without re-parsing the whole file. Termination
status is **not** updated incrementally:

- A session classified as `tool_call_pending` after a full parse stays
  flagged in the DB even if a later append adds the matching
  `tool_result`.
- The next full re-parse (size mismatch, periodic 15-minute resync, or
  explicit resync) recomputes and overwrites `termination_status`.

For v1 this is acceptable: stale `tool_call_pending` resolves itself
within ≤15 minutes and the value reflects the file's state at the last
full parse. Implementations that want stricter freshness can extend
`UpdateSessionIncremental` to either pass through a re-classification
or set `termination_status` to NULL on append (forcing the next read
to treat it as unclassified) — both options are non-breaking.

## Backfill

Backfill uses the project's existing `dataVersion` mechanism, not a custom
background task. `internal/db/db.go` line 34 holds an integer
`dataVersion` that gets bumped whenever parser changes affect stored data.
On startup, when the DB's `user_version` is below the binary's
`dataVersion`, `Open()` flags the DB as `dataStale` (db.go:155-158).
The CLI startup (`cmd/agentsview/usage.go:187`) checks
`database.NeedsResync()` and, when true, calls
`engine.ResyncAll(ctx, …)` to re-process every session file through
the parser.

**Implementation:** bump `dataVersion` from 11 to 12 in this PR. Existing
deployments will re-sync once on upgrade, classifying every session file
as it passes through the parser. No custom backfill code is needed.

Properties this gets for free from the existing mechanism:

- Resumable across restarts (`user_version` is only stamped after resync
  completes).
- Sessions whose source files are missing keep their existing data and
  end up with `termination_status = NULL` (the parser never runs on them).
- Concurrent UI: the resync runs through the existing sync engine, which
  is already designed to run alongside HTTP serving.

After the resync finishes once, ongoing sync handles classification
naturally — every re-parse on file change recomputes
`termination_status`.

## API

`db.Session` gains `TerminationStatus *string \`json:"termination_status,omitempty"\``.
The matching TypeScript field in `frontend/src/lib/api/types/core.ts`
adds `termination_status?: string | null;` (matching the `display_name`
pattern).

The session list filter is extended at three layers:

- `db.SessionFilter` (`internal/db/sessions.go:192-209`) gains a new
  field `Termination string` with values `""`, `"clean"`, or
  `"unclean"`.
- `buildSessionFilter` (`internal/db/sessions.go:220+`) appends the
  corresponding predicate when the field is non-empty.
- `handleListSessions` (`internal/server/sessions.go:62+`) reads
  `q.Get("termination")` and populates `filter.Termination` alongside
  the other URL-param-driven fields.

`GET /api/sessions` accepts a new query parameter:

| Param         | Values                    | Default | Effect                   |
| ------------- | ------------------------- | ------- | ------------------------ |
| `termination` | `all`, `clean`, `unclean` | `all`   | See behavior table below |

Behavior:

- `all` (or omitted) — return everything, including `NULL`.
- `clean` — only rows where status is `'clean'`.
- `unclean` — only rows where status is `'tool_call_pending'` or
  `'truncated'`.

`GET /api/sessions/:id` returns the same field on the response payload. No
new endpoints, no version bump.

## UI

### Status filter

The frontend has two filter stores: `sessions.svelte.ts` (sidebar
session list) and `analytics.svelte.ts` (analytics tab, surfaced via
`ActiveFilters.svelte`). Both need a new `termination` field with
values `""`, `"clean"`, or `"unclean"`, alongside the existing `agent`,
`machine`, `date` filters in each. Each store serializes it to the
`termination` query param on its respective API calls.

Active-filter chip: when the filter is set on the analytics store,
render a chip in `ActiveFilters.svelte` matching the existing
`filter-chip` pattern (with a clear button). The sidebar's
filter-display surface gets the same treatment in whatever component
shows active sidebar filters.

The control that *sets* the filter follows whatever pattern the
existing filters use (typeahead, toggle, or dedicated picker —
implementer's call based on existing agent/machine controls in each
context).

### Callout on detail page

When the loaded session's `termination_status` is `tool_call_pending` or
`truncated`, render a warning-styled banner above the message list. Not
dismissible — the banner reflects the session's permanent state.

Copy:

- `tool_call_pending`: "This session ended with a tool call that never
  received a response. The agent process likely terminated before the tool
  finished."
- `truncated`: "The session file ends mid-write. The agent process likely
  terminated abruptly."

When status is `clean` or `NULL`, no banner.

### Sessions analytics tab

Add a "Status" column to the existing sessions table (the Top Sessions /
Sessions tab). Cell content:

- `clean` — small green dot
- unclean (either value) — amber warning glyph; tooltip names the specific
  reason
- `NULL` — em-dash; tooltip "Status not determined"

Column is sortable. A small pill above the table reads "N unclean" and
click-toggles the same unclean filter, providing a triage entry point from
the analytics view.

### Empty / edge cases

- No unclean sessions: count pill shows `0` or hides; no other UI change.
- Filter set to `Unclean` with zero matches: standard empty-list copy.
- User has zero sessions overall: existing empty state unchanged.

## Testing

- `internal/parser/termination_test.go`: unit tests for `Classify` with
  fixtures covering each terminal state plus the unknown/empty case.
- `internal/parser/claude_parser_test.go` (or a new fixture-focused
  test file alongside the existing Claude parser tests): end-to-end
  fixtures of full transcripts for clean, `tool_call_pending`, and
  `truncated` cases.
- `internal/db/sessions_test.go`: column round-trip, filter query
  (`termination=unclean`), and verification that the index on
  `termination_status` is used by the filter query plan.
- PG session tests (`internal/postgres/store_test.go`,
  `integration_test.go`, or wherever fits the existing pattern; the
  `pgtest`-tagged files like `push_pgtest_test.go` are good candidates
  for push-sync propagation tests): same coverage on the PG side, plus
  push-sync propagation of the column.
- Frontend: component tests for the filter chip, detail callout, and
  Sessions tab status column. E2E test that loads a fixture session with
  `tool_call_pending` and verifies the banner renders.

## Migration safety

- Column is added with no default and no NOT NULL constraint, so existing
  rows are valid post-migration without data writes.
- The `dataVersion` bump triggers a one-shot full resync via the existing
  `NeedsResync` → `Engine.ResyncAll` path; if the resync is interrupted,
  the next startup retries because `user_version` is only stamped after
  the resync completes (`db.go:165-170`).
- Rollback (downgrading to a binary without this column) is benign: code
  selecting columns by name simply won't ask for `termination_status`.
  Schema-tightening (e.g., NOT NULL) is not introduced.

## Out of scope (deferred)

- Per-agent classification beyond Claude Code.
- Configurable "stale session" detection based on time since last activity
  (deliberately dropped during brainstorming — too arbitrary, weak signal).
- Recovery / resume flows for unclean sessions.
- Notifications or alerts when an unclean session is detected during sync.
