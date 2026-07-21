# Design: AGE column in `session search` results

Date: 2026-07-20
Status: approved

## Problem

Rows in `agentsview session search` human output are hard to associate with a
particular session. Each row shows the session ID, match ordinal, project,
location, and a short snippet — none of which map to how a user actually
remembers a session ("the one from Tuesday evening"). The per-match message
timestamp already exists on `db.ContentMatch` (populated in every search mode:
the lexical scanners read it from the message rows, semantic/hybrid get it via
`enrichSemanticHits`) and is already emitted in `--output json`, but neither
human format renders it.

## Decision

Render the match timestamp as an `AGE` column in the table format and as an
age token in the `--context` record format. Presentation-only change in
`cmd/agentsview/session_search.go` (plus a small refactor in
`session_list_render.go`); no DB, service, MCP, or JSON changes.

## Rendering rules

- Relative under a week, matching `session list`: `now` (future/skew), `42s`,
  `7m`, `3h`, `5d`.
- Absolute beyond a week: `Jan 02` when the timestamp's year equals the
  current year, `Jan 2025` for prior years. Search spans the whole persistent
  archive, so the year disambiguates multi-year results; `session list` keeps
  its existing year-less format.
- Em dash (`—`) when the timestamp is empty or unparseable.

## Placement

- **Table format**: new `AGE` column immediately after `MATCH`, before the
  optional `SCORE` column — mirroring session list's ID-then-AGE order. The
  column is data-sized like the other non-snippet columns; the existing
  snippet-budget logic absorbs the width. Non-TTY output includes the column
  untruncated like every other cell.
- **`--context` record format**: the age token on the match line between the
  ordinal/score markers and the project, e.g.
  `abc123  #14 sub score=0.83  3h  agentsview  message`.
- **JSON output**: unchanged (already carries `timestamp` per match).

## Implementation

- Extract the relative buckets (`now`/seconds/minutes/hours/days) from
  `humanizeSessionAge` into a shared core in `session_list_render.go`:
  `humanizeAgeRelative(t, now time.Time) (string, bool)`, returning
  `("", false)` when the age is a week or more. Each caller supplies its own
  absolute branch: `humanizeSessionAge` keeps the year-less `Jan 02`, so
  `session list` output is byte-identical.
- Add a search-side helper in `session_search.go` that parses the match's
  RFC3339/RFC3339Nano timestamp via the existing `parseSessionTime`, returns
  `emDash` on failure, calls the shared core, and formats the beyond-a-week
  case with the year rule above (`Jan 02` this year, `Jan 2025` prior years).
- Capture `time.Now()` once per render call and thread it into the row loop so
  tests can inject a fixed clock (both `printContentMatchesTable` and
  `printContentMatchesHuman` grow a `now time.Time` parameter; the exported
  entry point `printContentSearchResult` captures the clock).

## Testing

Table-driven tests in `cmd/agentsview/session_search_test.go` using testify:

- Table format: `AGE` header present; one case per age bucket (seconds,
  minutes, hours, days, current-year `Jan 02`, prior-year `Jan 2025`, future
  → `now`, missing/unparseable → `—`); column alignment preserved.
- Record format (`--context`): age token appears on the match line in the
  expected position.
- Existing snippet-budget and alignment tests keep passing unmodified.

## Out of scope

- Changing `session list`'s AGE format.
- MCP `search_content` output shaping (JSON already has the timestamp).
- Any DB or service-layer changes.
