# Session Termination Status Implementation Plan

> **Status: superseded.** This plan reflects the original task breakdown
> at the start of the build-out. The shipped feature evolved during
> implementation (added `awaiting_user`, replaced the detail-page banner
> with `StatusDot`, added active/stale/unclean time tiers, added Codex
> classification, made incremental sync clear stale status). For the
> current contract, see the "as-implemented addendum" at the top of
> `docs/superpowers/specs/2026-04-28-session-termination-status-design.md`
> and the actual code. Tasks below remain useful as a layered build
> sequence reference but should not be followed verbatim.
>
> ### As-shipped checklist (for follow-up work and verification)
>
> Use this checklist instead of the original task list when extending
> or auditing the feature:
>
> - **Parser taxonomy**: `clean`, `awaiting_user`, `tool_call_pending`,
>   `truncated`, `NULL`. See `internal/parser/termination.go`.
>   Claude classifier checks `stop_reason` (`end_turn` →
>   `awaiting_user`) and orphan tool calls scoped to messages
>   strictly *after* the last assistant message. Codex classifier
>   reads task lifecycle events (`task_started` / `task_complete` /
>   `turn_aborted`).
> - **SQLite persistence**: `sessions.termination_status` column +
>   `idx_sessions_termination_status`. `UpsertSession` writes the
>   parser value; `UpdateSessionIncremental` clears to `NULL` on
>   every incremental write. Migration covered by
>   `TestMigration_TerminationStatusColumn`.
> - **PostgreSQL parity**: same column + index, same upsert/clear
>   semantics, mirrored predicate helpers (`pgTerminationPred`).
>   Push round-trip covered in `internal/postgres/push_pgtest_test.go`.
> - **Filter contract**: comma-separated `?termination=` accepts
>   `clean`, `awaiting_user`, `active`, `stale`, `unclean`, or
>   `all`/empty. `active`/`stale`/`unclean` combine recency with the
>   parser flag (see `buildTerminationPredSQLite` in
>   `internal/db/sessions.go`). Unknown values are silently ignored
>   (consistent with `outcome` and `health_grade` filters).
> - **Frontend status tiers**: `getSessionStatus` in
>   `frontend/src/lib/stores/sessions.svelte.ts` derives one of
>   `working` / `waiting` / `idle` / `stale` / `unclean` / `quiet`
>   from termination_status + recency. `StatusDot` renders the dot
>   or champagne speech-bubble. Sidebar sort: working → waiting →
>   idle → stale → quiet → unclean.
> - **UI surfaces**: sidebar `SessionItem`, Top Sessions analytics
>   table, multi-select Active/Stale/Unclean pills in the sidebar
>   filter, ActiveFilters chip in the analytics page. No detail-page
>   banner (deliberately removed).
> - **Regression tests**: `TestClassify` (parser, including
>   prior-matching-result-then-final-orphan), the migration test
>   above, `TestIncrementalUpdateClearsTerminationStatus`,
>   `buildSessionGroups` status-tier sort test, and the
>   `frontend/e2e/termination.spec.ts` Playwright spec.
> - **Edge case to keep covered**: `awaiting_user` should render
>   as `waiting` only inside the 10m active window. Once the
>   session ages past 10m it should render as `quiet` (not stuck
>   on the waiting bubble forever). Add coverage if the
>   precedence rules change.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Detect and surface sessions that ended without a clean
shutdown signal, so users can find them via filter, see them flagged
on the detail page, and rank them in analytics.

**Architecture:** A new `termination_status` enum column on `sessions`
gets populated at parse time by a Claude-Code-specific classifier. A
`dataVersion` bump triggers a one-shot full resync that re-classifies
every session. The filter is wired through SessionFilter →
buildSessionFilter → HTTP handler → both frontend filter stores; the
status surfaces as a chip, a detail-page callout, and a column in the
Top Sessions table.

**Tech Stack:** Go 1.x (sqlite3 + pgx), Svelte 5, TypeScript, Vite,
Playwright. Build flag: `CGO_ENABLED=1 -tags fts5`. PG integration
tests use `-tags pgtest` and `make test-postgres`.

**Spec:** `docs/superpowers/specs/2026-04-28-session-termination-status-design.md`

---

## Phase 1 — Backend foundation

### Task 1: Classifier (`internal/parser/termination.go`)

**Files:**
- Create: `internal/parser/termination.go`
- Create: `internal/parser/termination_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/parser/termination_test.go`:

```go
package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		messages  []ParsedMessage
		truncated bool
		want      TerminationStatus
	}{
		{
			name:     "empty messages, not truncated",
			messages: nil,
			want:     "",
		},
		{
			name:      "empty messages, truncated wins",
			messages:  nil,
			truncated: true,
			want:      TerminationTruncated,
		},
		{
			name: "clean session: assistant text only, no tool calls",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "hello"},
				{Role: RoleAssistant, Content: "hi"},
			},
			want: TerminationClean,
		},
		{
			name: "clean session: tool call resolved by tool result",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "read file"},
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1", ToolName: "Read"},
				}},
				{Role: RoleUser, ToolResults: []ParsedToolResult{
					{ToolUseID: "toolu_1"},
				}},
				{Role: RoleAssistant, Content: "done"},
			},
			want: TerminationClean,
		},
		{
			name: "tool_call_pending: last assistant has unmatched tool_use",
			messages: []ParsedMessage{
				{Role: RoleUser, Content: "read file"},
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1", ToolName: "Read"},
				}},
			},
			want: TerminationToolCallPending,
		},
		{
			name: "tool_call_pending: prior turns matched, last has unmatched",
			messages: []ParsedMessage{
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1"},
				}},
				{Role: RoleUser, ToolResults: []ParsedToolResult{
					{ToolUseID: "toolu_1"},
				}},
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_2"},
				}},
			},
			want: TerminationToolCallPending,
		},
		{
			name: "truncated overrides tool_call_pending",
			messages: []ParsedMessage{
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: "toolu_1"},
				}},
			},
			truncated: true,
			want:      TerminationTruncated,
		},
		{
			name: "ignores empty ToolUseID",
			messages: []ParsedMessage{
				{Role: RoleAssistant, ToolCalls: []ParsedToolCall{
					{ToolUseID: ""},
				}},
			},
			want: TerminationClean,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.messages, tc.truncated)
			assert.Equal(t, tc.want, got)
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/parser/ -run TestClassify -v`

Expected: FAIL with "undefined: Classify" or similar.

- [ ] **Step 3: Implement the classifier**

Create `internal/parser/termination.go`:

```go
package parser

// TerminationStatus describes how a parsed session appears to have
// ended. The empty string means "unknown" — caller should leave the
// stored column NULL.
type TerminationStatus string

const (
	TerminationClean           TerminationStatus = "clean"
	TerminationToolCallPending TerminationStatus = "tool_call_pending"
	TerminationTruncated       TerminationStatus = "truncated"
)

// Classify returns a status given a parsed message slice and a
// sentinel from the file scanner. Returns "" (unknown) when no
// classification can be made — for example, an empty message slice
// from an unparseable file. Truncation takes precedence over
// tool_call_pending: if the file was cut off mid-write, that's the
// stronger signal about what went wrong.
func Classify(messages []ParsedMessage, fileTruncated bool) TerminationStatus {
	if fileTruncated {
		return TerminationTruncated
	}
	if len(messages) == 0 {
		return ""
	}
	if hasOrphanedToolCall(messages) {
		return TerminationToolCallPending
	}
	return TerminationClean
}

// hasOrphanedToolCall reports whether the last assistant message has
// any tool_use blocks that lack a matching tool_result anywhere in
// the message slice.
func hasOrphanedToolCall(messages []ParsedMessage) bool {
	lastAssistantIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleAssistant {
			lastAssistantIdx = i
			break
		}
	}
	if lastAssistantIdx == -1 {
		return false
	}
	last := messages[lastAssistantIdx]
	if len(last.ToolCalls) == 0 {
		return false
	}

	resolved := make(map[string]bool)
	for _, m := range messages {
		for _, tr := range m.ToolResults {
			if tr.ToolUseID != "" {
				resolved[tr.ToolUseID] = true
			}
		}
	}

	for _, tc := range last.ToolCalls {
		if tc.ToolUseID != "" && !resolved[tc.ToolUseID] {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/parser/ -run TestClassify -v`

Expected: PASS — all 8 subtests.

- [ ] **Step 5: Commit**

```bash
git add internal/parser/termination.go internal/parser/termination_test.go
git commit -m "feat(parser): add termination status classifier"
```

---

### Task 2: Parser integration (Claude tracks truncation, calls Classify)

**Files:**
- Modify: `internal/parser/types.go` (add field to `ParsedSession`)
- Modify: `internal/parser/claude.go` (track truncation, call Classify per ParseResult)
- Modify: `internal/parser/claude_parser_test.go` (add fixture tests)
- Create: `internal/parser/testdata/claude/tool_call_pending.jsonl`
- Create: `internal/parser/testdata/claude/truncated.jsonl`

- [ ] **Step 1: Add `TerminationStatus` field to `ParsedSession`**

In `internal/parser/types.go`, locate the `ParsedSession` struct (around line 402) and add `TerminationStatus` near the other top-level metadata fields (after `MessageCount`/`UserMessageCount` is fine):

```go
type ParsedSession struct {
	ID               string
	Project          string
	Machine          string
	Agent            AgentType
	ParentSessionID  string
	RelationshipType RelationshipType
	Cwd              string
	FirstMessage     string
	DisplayName      string
	StartedAt        time.Time
	EndedAt          time.Time
	MessageCount     int
	UserMessageCount int
	File             FileInfo

	// TerminationStatus describes how the session appears to have
	// ended. Empty string = unknown (parser did not classify, or
	// agent format does not yet support classification).
	TerminationStatus TerminationStatus

	TotalOutputTokens    int
	PeakContextTokens    int
	HasTotalOutputTokens bool
	HasPeakContextTokens bool

	// aggregateTokenPresenceKnown marks session aggregate token
	// coverage as parser-owned and authoritative.
	aggregateTokenPresenceKnown bool
}
```

- [ ] **Step 2: Build to verify field compiles**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go build -tags fts5 ./...`

Expected: success, no compile errors.

- [ ] **Step 3: Create the `tool_call_pending` fixture**

Create `internal/parser/testdata/claude/tool_call_pending.jsonl`:

```jsonl
{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"Read auth.ts"},"cwd":"/Users/alice/code/my-app"}
{"type":"assistant","timestamp":"2024-01-01T10:00:05Z","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Reading the file..."},{"type":"tool_use","id":"toolu_orphan","name":"Read","input":{"file_path":"src/auth.ts"}}],"usage":{"input_tokens":100,"output_tokens":50}}}
```

(Note: the assistant emits a tool_use but no user turn follows with a tool_result. The session was killed mid-action.)

- [ ] **Step 4: Create the `truncated` fixture**

Create `internal/parser/testdata/claude/truncated.jsonl`. Take a copy of the valid_session content but truncate the LAST line in the middle of a JSON value:

```jsonl
{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"hello"},"cwd":"/Users/alice/code/my-app"}
{"type":"assistant","timestamp":"2024-01-01T10:00:05Z","message":{"model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"Hi there"}],"usage":{"input_tokens":10,"output_tokens":5}}}
{"type":"user","timestamp":"2024-01-01T10:01:00Z","message":{"content":"more
```

(The file ends mid-string with no closing quote, no closing brace, no newline.)

- [ ] **Step 5: Write failing test for classification on existing valid fixture**

Add to `internal/parser/claude_parser_test.go` (just before the final closing of the file or grouped with other parser tests):

```go
func TestParseClaudeSession_TerminationStatus(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		content := loadFixture(t, "claude/valid_session.jsonl")
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, TerminationClean, sess.TerminationStatus)
	})

	t.Run("tool_call_pending", func(t *testing.T) {
		content := loadFixture(t, "claude/tool_call_pending.jsonl")
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, TerminationToolCallPending, sess.TerminationStatus)
	})

	t.Run("truncated", func(t *testing.T) {
		content := loadFixture(t, "claude/truncated.jsonl")
		sess, _ := runClaudeParserTest(t, "test.jsonl", content)
		assert.Equal(t, TerminationTruncated, sess.TerminationStatus)
	})
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/parser/ -run TestParseClaudeSession_TerminationStatus -v`

Expected: FAIL — `sess.TerminationStatus` is empty string for all three because the parser doesn't populate it yet.

- [ ] **Step 7: Wire the classifier into `ParseClaudeSession`**

In `internal/parser/claude.go`, modify the parse loop to track truncation. Locate the line-reading loop (around line 78–86 where `lr := newLineReader(f, maxLineSize)` lives). Add a `lastLineFailed` tracker:

```go
lr := newLineReader(f, maxLineSize)
lastLineFailed := false
for {
	line, ok := lr.next()
	if !ok {
		break
	}
	if !gjson.Valid(line) {
		lastLineFailed = true
		continue
	}
	lastLineFailed = false

	// ... existing processing logic continues ...
}
```

Then locate the function's return statement that builds and returns `[]ParseResult`. Before returning, classify each result. Find where `ParseResult` slice is built (likely near the end of `ParseClaudeSession`). Add this loop right before returning:

```go
for i := range results {
	results[i].Session.TerminationStatus = Classify(
		results[i].Messages, lastLineFailed,
	)
}
```

(All forks from a single file share the same `lastLineFailed` value — a truncated tail affects all forks of that file.)

- [ ] **Step 8: Run the test to verify it passes**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/parser/ -run TestParseClaudeSession_TerminationStatus -v`

Expected: PASS — all three subtests.

- [ ] **Step 9: Run the full parser test suite to confirm no regressions**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/parser/...`

Expected: PASS — all parser tests.

- [ ] **Step 10: Commit**

```bash
git add internal/parser/types.go internal/parser/claude.go internal/parser/claude_parser_test.go internal/parser/testdata/claude/tool_call_pending.jsonl internal/parser/testdata/claude/truncated.jsonl
git commit -m "feat(parser): classify Claude sessions by termination status"
```

---

## Phase 2 — SQLite persistence

### Task 3: SQLite schema migration + dataVersion bump

**Files:**
- Modify: `internal/db/schema.sql` (add column for fresh DBs)
- Modify: `internal/db/db.go` (migration entry, index, dataVersion bump)

- [ ] **Step 1: Add column to fresh-DB schema**

In `internal/db/schema.sql`, locate the `CREATE TABLE IF NOT EXISTS sessions` definition (lines 2–27). Add `termination_status TEXT` as the LAST column (matching the position ALTER TABLE produces on migrated DBs, so fresh and migrated DBs have identical column order):

```sql
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    first_message TEXT,
    display_name TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    user_message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    file_hash   TEXT,
    local_modified_at TEXT,
    parent_session_id TEXT,
    relationship_type TEXT NOT NULL DEFAULT '',
    total_output_tokens INTEGER NOT NULL DEFAULT 0,
    peak_context_tokens INTEGER NOT NULL DEFAULT 0,
    has_total_output_tokens INTEGER NOT NULL DEFAULT 0,
    has_peak_context_tokens INTEGER NOT NULL DEFAULT 0,
    is_automated INTEGER NOT NULL DEFAULT 0,
    deleted_at  TEXT,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    termination_status TEXT
);
```

(Do NOT add the index here — putting `CREATE INDEX … ON sessions(termination_status)` in `schema.sql` would fail on existing databases that haven't yet had the column added by `migrateColumns`. The index goes in `migrateColumns` instead, after the ALTER TABLE.)

- [ ] **Step 2: Add migration entry and index in `migrateColumns`**

In `internal/db/db.go`, locate the `migrations` slice inside `migrateColumns()` (around line 275). Append a new entry at the end of the slice (after the `is_automated` entry):

```go
{
    "sessions", "termination_status",
    "ALTER TABLE sessions ADD COLUMN termination_status TEXT",
},
```

Then locate the post-loop code where `remote_skipped_files` is created (around line 380). Just before that block, add the index creation:

```go
if _, err := w.Exec(
    `CREATE INDEX IF NOT EXISTS idx_sessions_termination_status
     ON sessions(termination_status)`,
); err != nil {
    return fmt.Errorf(
        "creating idx_sessions_termination_status: %w", err,
    )
}
```

- [ ] **Step 3: Bump `dataVersion` from 11 to 12**

In `internal/db/db.go`, locate the `dataVersion` constant (line 34). Update the comment and the value:

```go
// dataVersion tracks parser changes that require a full
// re-sync. Increment this when parsing logic changes in ways
// that affect stored data (e.g. new fields extracted, content
// formatting changes). Old databases with a lower user_version
// trigger a non-destructive re-sync (mtime reset + skip cache
// clear) so existing session data is preserved.
//
// Bumped to 12: added termination_status to sessions; existing
// rows need re-parsing so the Claude classifier can populate
// the new column.
const dataVersion = 12
```

- [ ] **Step 4: Write a test confirming a fresh DB has the column**

Add to `internal/db/db_test.go` (or wherever migration tests live; if uncertain, group at the end of `db_test.go`):

```go
func TestSessionsHasTerminationStatusColumn(t *testing.T) {
	d := testDB(t)

	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('sessions')
		 WHERE name = 'termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "probing termination_status column")

	if count != 1 {
		t.Fatalf(
			"expected 1 termination_status column, got %d", count,
		)
	}
}

func TestSessionsTerminationStatusIndex(t *testing.T) {
	d := testDB(t)

	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'index' AND name = 'idx_sessions_termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "probing idx_sessions_termination_status")

	if count != 1 {
		t.Fatalf(
			"expected idx_sessions_termination_status to exist, got count=%d",
			count,
		)
	}
}
```

- [ ] **Step 5: Run tests**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run "TestSessionsHasTerminationStatusColumn|TestSessionsTerminationStatusIndex" -v`

Expected: PASS — both tests.

- [ ] **Step 6: Commit**

```bash
git add internal/db/schema.sql internal/db/db.go internal/db/db_test.go
git commit -m "feat(db): add termination_status column and index"
```

---

### Task 4: SQLite Session struct + read paths + write path

**Files:**
- Modify: `internal/db/sessions.go` (struct, three column-list constants, four scan paths, UpsertSession)
- Modify: `internal/db/sessions_test.go` (round-trip test)

- [ ] **Step 1: Add `TerminationStatus` to the `Session` struct**

In `internal/db/sessions.go`, locate the `Session` struct (line 87). Insert `TerminationStatus *string` after `DeletedAt` (matching the existing nullable-text pattern):

```go
type Session struct {
	ID                   string  `json:"id"`
	Project              string  `json:"project"`
	Machine              string  `json:"machine"`
	Agent                string  `json:"agent"`
	FirstMessage         *string `json:"first_message"`
	DisplayName          *string `json:"display_name,omitempty"`
	StartedAt            *string `json:"started_at"`
	EndedAt              *string `json:"ended_at"`
	MessageCount         int     `json:"message_count"`
	UserMessageCount     int     `json:"user_message_count"`
	ParentSessionID      *string `json:"parent_session_id,omitempty"`
	RelationshipType     string  `json:"relationship_type,omitempty"`
	TotalOutputTokens    int     `json:"total_output_tokens"`
	PeakContextTokens    int     `json:"peak_context_tokens"`
	HasTotalOutputTokens bool    `json:"has_total_output_tokens"`
	HasPeakContextTokens bool    `json:"has_peak_context_tokens"`
	IsAutomated          bool    `json:"is_automated"`
	DeletedAt            *string `json:"deleted_at,omitempty"`
	TerminationStatus    *string `json:"termination_status,omitempty"`
	FilePath             *string `json:"file_path,omitempty"`
	FileSize             *int64  `json:"file_size,omitempty"`
	FileMtime            *int64  `json:"file_mtime,omitempty"`
	FileHash             *string `json:"file_hash,omitempty"`
	LocalModifiedAt      *string `json:"local_modified_at,omitempty"`
	CreatedAt            string  `json:"created_at"`
}
```

- [ ] **Step 2: Add `termination_status` to all three column-list constants**

In `internal/db/sessions.go` (lines 26–55), update each constant to include `termination_status` immediately after `deleted_at`:

```go
const sessionBaseCols = `id, project, machine, agent,
	first_message, display_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	has_total_output_tokens, has_peak_context_tokens,
	is_automated,
	deleted_at, termination_status, created_at`

const sessionPruneCols = `id, project, machine, agent,
	first_message, display_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	has_total_output_tokens, has_peak_context_tokens,
	is_automated,
	deleted_at, termination_status, file_path, file_size, created_at`

const sessionFullCols = `id, project, machine, agent,
	first_message, display_name, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	has_total_output_tokens, has_peak_context_tokens,
	is_automated,
	deleted_at, termination_status, file_path, file_size, file_mtime,
	file_hash, local_modified_at, created_at`
```

- [ ] **Step 3: Update `scanSessionRow` (uses `sessionBaseCols`)**

In `internal/db/sessions.go`, locate `scanSessionRow` (line 71). Add `&s.TerminationStatus` after `&s.DeletedAt`:

```go
func scanSessionRow(rs rowScanner) (Session, error) {
	var s Session
	err := rs.Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent,
		&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
		&s.MessageCount, &s.UserMessageCount,
		&s.ParentSessionID, &s.RelationshipType,
		&s.TotalOutputTokens, &s.PeakContextTokens,
		&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
		&s.IsAutomated,
		&s.DeletedAt, &s.TerminationStatus, &s.CreatedAt,
	)
	return s, err
}
```

- [ ] **Step 4: Update `GetSessionFull` scan (uses `sessionFullCols`)**

In `internal/db/sessions.go`, locate `GetSessionFull` (around line 485). Add `&s.TerminationStatus` after `&s.DeletedAt`:

```go
err := row.Scan(
	&s.ID, &s.Project, &s.Machine, &s.Agent,
	&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
	&s.MessageCount, &s.UserMessageCount,
	&s.ParentSessionID, &s.RelationshipType,
	&s.TotalOutputTokens, &s.PeakContextTokens,
	&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
	&s.IsAutomated,
	&s.DeletedAt, &s.TerminationStatus, &s.FilePath, &s.FileSize,
	&s.FileMtime, &s.FileHash, &s.LocalModifiedAt, &s.CreatedAt,
)
```

- [ ] **Step 5: Update `ListSessionsModifiedBetween` scan (uses `sessionFullCols`)**

In `internal/db/sessions.go`, locate `ListSessionsModifiedBetween` (around line 1332). The scan call inside the row loop has the same 24 columns as `GetSessionFull` — apply the same change: add `&s.TerminationStatus` after `&s.DeletedAt`:

```go
err := rows.Scan(
	&s.ID, &s.Project, &s.Machine, &s.Agent,
	&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
	&s.MessageCount, &s.UserMessageCount,
	&s.ParentSessionID, &s.RelationshipType,
	&s.TotalOutputTokens, &s.PeakContextTokens,
	&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
	&s.IsAutomated,
	&s.DeletedAt, &s.TerminationStatus, &s.FilePath, &s.FileSize,
	&s.FileMtime, &s.FileHash, &s.LocalModifiedAt, &s.CreatedAt,
)
```

- [ ] **Step 6: Update `FindPruneCandidates` scan (uses `sessionPruneCols`)**

In `internal/db/sessions.go`, locate `FindPruneCandidates` (around line 1094). The scan call has 21 columns. Add `&s.TerminationStatus` after `&s.DeletedAt`:

```go
err := rows.Scan(
	&s.ID, &s.Project, &s.Machine, &s.Agent,
	&s.FirstMessage, &s.DisplayName, &s.StartedAt, &s.EndedAt,
	&s.MessageCount, &s.UserMessageCount,
	&s.ParentSessionID, &s.RelationshipType,
	&s.TotalOutputTokens, &s.PeakContextTokens,
	&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
	&s.IsAutomated,
	&s.DeletedAt, &s.TerminationStatus, &s.FilePath, &s.FileSize, &s.CreatedAt,
)
```

- [ ] **Step 7: Update `UpsertSession` write path**

In `internal/db/sessions.go`, locate `UpsertSession` (around line 541). Update the INSERT column list, the `VALUES` placeholders, and the `ON CONFLICT DO UPDATE SET` clauses to include `termination_status`. Also add `s.TerminationStatus` to the args list. Insert after `is_automated`:

```go
_, err := db.getWriter().Exec(`
	INSERT INTO sessions (
		id, project, machine, agent, first_message, display_name,
		started_at, ended_at, message_count,
		user_message_count, parent_session_id,
		relationship_type,
		total_output_tokens, peak_context_tokens,
		has_total_output_tokens, has_peak_context_tokens,
		is_automated,
		termination_status,
		file_path, file_size, file_mtime, file_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO UPDATE SET
		project = excluded.project,
		machine = excluded.machine,
		agent = excluded.agent,
		first_message = excluded.first_message,
		started_at = excluded.started_at,
		ended_at = excluded.ended_at,
		message_count = excluded.message_count,
		user_message_count = excluded.user_message_count,
		parent_session_id = excluded.parent_session_id,
		relationship_type = excluded.relationship_type,
		total_output_tokens = excluded.total_output_tokens,
		peak_context_tokens = excluded.peak_context_tokens,
		has_total_output_tokens = excluded.has_total_output_tokens,
		has_peak_context_tokens = excluded.has_peak_context_tokens,
		is_automated = excluded.is_automated,
		termination_status = excluded.termination_status,
		file_path = excluded.file_path,
		file_size = excluded.file_size,
		file_mtime = excluded.file_mtime,
		file_hash = excluded.file_hash`,
	s.ID, s.Project, s.Machine, s.Agent, s.FirstMessage, s.DisplayName,
	s.StartedAt, s.EndedAt, s.MessageCount,
	s.UserMessageCount, s.ParentSessionID,
	s.RelationshipType,
	s.TotalOutputTokens, s.PeakContextTokens,
	s.HasTotalOutputTokens, s.HasPeakContextTokens,
	isAutomated,
	s.TerminationStatus,
	s.FilePath, s.FileSize, s.FileMtime, s.FileHash)
```

(Note the change from 21 placeholders to 22.)

- [ ] **Step 8: Build to confirm everything compiles**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go build -tags fts5 ./...`

Expected: success.

- [ ] **Step 9: Write round-trip test**

Add to `internal/db/sessions_test.go` (add `"context"` to the imports if not already present):

```go
func TestUpsertSessionTerminationStatus(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	clean := "clean"
	pending := "tool_call_pending"

	tests := []struct {
		name string
		val  *string
	}{
		{name: "null", val: nil},
		{name: "clean", val: &clean},
		{name: "tool_call_pending", val: &pending},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := "session_" + tc.name
			s := Session{
				ID:                id,
				Project:           "p",
				Machine:           "local",
				Agent:             "claude",
				MessageCount:      1,
				UserMessageCount:  1,
				TerminationStatus: tc.val,
			}
			if err := d.UpsertSession(s); err != nil {
				t.Fatalf("upsert: %v", err)
			}

			got, err := d.GetSession(ctx, id)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if got == nil {
				t.Fatal("session not found")
			}

			if (got.TerminationStatus == nil) != (tc.val == nil) {
				t.Fatalf(
					"nil mismatch: got=%v want=%v",
					got.TerminationStatus, tc.val,
				)
			}
			if got.TerminationStatus != nil && *got.TerminationStatus != *tc.val {
				t.Fatalf(
					"value mismatch: got=%q want=%q",
					*got.TerminationStatus, *tc.val,
				)
			}
		})
	}
}
```

- [ ] **Step 10: Run round-trip test**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestUpsertSessionTerminationStatus -v`

Expected: PASS — all three subtests.

- [ ] **Step 11: Run full DB test suite to confirm no regressions**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/db/...`

Expected: PASS (full suite).

- [ ] **Step 12: Commit**

```bash
git add internal/db/sessions.go internal/db/sessions_test.go
git commit -m "feat(db): plumb termination_status through Session struct and queries"
```

---

### Task 5: SessionFilter + buildSessionFilter + HTTP handler

**Files:**
- Modify: `internal/db/sessions.go` (`SessionFilter`, `buildSessionFilter`)
- Modify: `internal/db/sessions_test.go` (filter test)
- Modify: `internal/server/sessions.go` (URL param parsing)

- [ ] **Step 1: Write the failing filter test**

Add to `internal/db/sessions_test.go`:

```go
func TestListSessionsTerminationFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	clean := "clean"
	pending := "tool_call_pending"
	truncated := "truncated"

	insertWithTerm := func(id string, val *string) {
		s := Session{
			ID:                id,
			Project:           "p",
			Machine:           "local",
			Agent:             "claude",
			MessageCount:      1,
			UserMessageCount:  2,
			TerminationStatus: val,
		}
		if err := d.UpsertSession(s); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	insertWithTerm("clean1", &clean)
	insertWithTerm("clean2", &clean)
	insertWithTerm("pending1", &pending)
	insertWithTerm("trunc1", &truncated)
	insertWithTerm("null1", nil)

	collect := func(f SessionFilter) []string {
		page, err := d.ListSessions(ctx, f)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		ids := make([]string, len(page.Sessions))
		for i, s := range page.Sessions {
			ids[i] = s.ID
		}
		return ids
	}

	tests := []struct {
		name        string
		termination string
		wantIDs     []string
	}{
		{name: "all (default)", termination: "", wantIDs: []string{"clean1", "clean2", "pending1", "trunc1", "null1"}},
		{name: "clean", termination: "clean", wantIDs: []string{"clean1", "clean2"}},
		{name: "unclean", termination: "unclean", wantIDs: []string{"pending1", "trunc1"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := collect(SessionFilter{Termination: tc.termination})
			assertStringSetsEqual(t, got, tc.wantIDs)
		})
	}
}

// assertStringSetsEqual checks that two slices contain the same
// elements regardless of order.
func assertStringSetsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got=%d want=%d (got=%v want=%v)",
			len(got), len(want), got, want)
	}
	seen := make(map[string]int)
	for _, s := range want {
		seen[s]++
	}
	for _, s := range got {
		seen[s]--
	}
	for s, n := range seen {
		if n != 0 {
			t.Fatalf("set mismatch on %q: leftover=%d (got=%v want=%v)",
				s, n, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestListSessionsTerminationFilter -v`

Expected: compile error — `unknown field Termination in struct literal of type db.SessionFilter`.

- [ ] **Step 3: Add `Termination` field to `SessionFilter`**

In `internal/db/sessions.go`, locate `SessionFilter` (line 192). Append the new field at the end:

```go
type SessionFilter struct {
	Project          string
	ExcludeProject   string
	Machine          string
	Agent            string
	Date             string
	DateFrom         string
	DateTo           string
	ActiveSince      string
	MinMessages      int
	MaxMessages      int
	MinUserMessages  int
	ExcludeOneShot   bool
	ExcludeAutomated bool
	IncludeChildren  bool
	Cursor           string
	Limit            int
	// Termination filters by termination_status:
	//   "" or "all"  → no filter (default)
	//   "clean"      → only sessions with status = 'clean'
	//   "unclean"    → only sessions with status IN
	//                  ('tool_call_pending', 'truncated')
	Termination string
}
```

- [ ] **Step 4: Add the predicate to `buildSessionFilter`**

In `internal/db/sessions.go`, inside `buildSessionFilter` (around line 220), add the termination predicate alongside the other filter predicates. Place it after the `MinUserMessages` block:

```go
if f.MinUserMessages > 0 {
	filterPreds = append(filterPreds, "user_message_count >= ?")
	filterArgs = append(filterArgs, f.MinUserMessages)
}

switch f.Termination {
case "clean":
	filterPreds = append(filterPreds, "termination_status = 'clean'")
case "unclean":
	filterPreds = append(filterPreds,
		"termination_status IN ('tool_call_pending', 'truncated')")
}
// "" and "all" add no predicate.
```

(No bound parameter needed since the values are literal SQL strings, not user input.)

- [ ] **Step 5: Run test to verify it passes**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/db/ -run TestListSessionsTerminationFilter -v`

Expected: PASS — all three subtests.

- [ ] **Step 6: Wire `?termination=` into the HTTP handler**

In `internal/server/sessions.go`, locate `handleListSessions` (around line 62). Add `Termination` to the filter struct alongside the other URL-param-driven fields:

```go
filter := db.SessionFilter{
	Project:          q.Get("project"),
	ExcludeProject:   q.Get("exclude_project"),
	Machine:          q.Get("machine"),
	Agent:            q.Get("agent"),
	Date:             date,
	DateFrom:         dateFrom,
	DateTo:           dateTo,
	ActiveSince:      activeSince,
	MinMessages:      minMsgs,
	MaxMessages:      maxMsgs,
	MinUserMessages:  minUserMsgs,
	ExcludeOneShot:   !includeOneShot,
	ExcludeAutomated: !includeAutomated,
	IncludeChildren:  includeChildren,
	Cursor:           q.Get("cursor"),
	Limit:            limit,
	Termination:      q.Get("termination"),
}
```

- [ ] **Step 7: Run server tests to confirm no regressions**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/server/...`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/db/sessions.go internal/db/sessions_test.go internal/server/sessions.go
git commit -m "feat(api): add ?termination= filter to GET /api/sessions"
```

---

## Phase 3 — Sync layer

### Task 6: `toDBSession` plumbing

**Files:**
- Modify: `internal/sync/engine.go` (`toDBSession`)
- Modify: `internal/sync/engine_test.go` if it exists; otherwise add a small targeted test

- [ ] **Step 1: Write the failing test**

Add to `internal/sync/engine_test.go` (create file if missing — package is `sync`):

```go
package sync

import (
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/parser"
)

func TestToDBSessionTerminationStatus(t *testing.T) {
	tests := []struct {
		name string
		in   parser.TerminationStatus
		want *string
	}{
		{name: "empty maps to nil", in: "", want: nil},
		{name: "clean maps to pointer", in: parser.TerminationClean, want: ptrString("clean")},
		{name: "tool_call_pending maps to pointer", in: parser.TerminationToolCallPending, want: ptrString("tool_call_pending")},
		{name: "truncated maps to pointer", in: parser.TerminationTruncated, want: ptrString("truncated")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pw := pendingWrite{
				sess: parser.ParsedSession{
					ID:                "s1",
					Project:           "p",
					Machine:           "m",
					Agent:             parser.AgentClaude,
					StartedAt:         time.Now(),
					EndedAt:           time.Now(),
					MessageCount:      1,
					UserMessageCount:  1,
					TerminationStatus: tc.in,
				},
			}
			got := toDBSession(pw)

			if (got.TerminationStatus == nil) != (tc.want == nil) {
				t.Fatalf("nil mismatch: got=%v want=%v",
					got.TerminationStatus, tc.want)
			}
			if got.TerminationStatus != nil && *got.TerminationStatus != *tc.want {
				t.Fatalf("value mismatch: got=%q want=%q",
					*got.TerminationStatus, *tc.want)
			}
		})
	}
}

func ptrString(s string) *string { return &s }
```

(If `engine_test.go` already exists in the package, append the test there and reuse helpers; if a `ptrString` helper already exists, drop the redefinition.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/sync/ -run TestToDBSessionTerminationStatus -v`

Expected: FAIL — `TerminationStatus` field is not copied through.

- [ ] **Step 3: Update `toDBSession`**

In `internal/sync/engine.go`, locate `toDBSession` (around line 2733). Add a line setting `TerminationStatus` after the existing `FileHash` assignment, using the existing `strPtr` helper (defined at line 3158, returns nil for empty string):

```go
func toDBSession(pw pendingWrite) db.Session {
	hasTotal, hasPeak := pw.sess.TokenCoverage(pw.msgs)
	s := db.Session{
		ID:                   pw.sess.ID,
		Project:              pw.sess.Project,
		Machine:              pw.sess.Machine,
		Agent:                string(pw.sess.Agent),
		MessageCount:         pw.sess.MessageCount,
		UserMessageCount:     pw.sess.UserMessageCount,
		ParentSessionID:      strPtr(pw.sess.ParentSessionID),
		RelationshipType:     string(pw.sess.RelationshipType),
		TotalOutputTokens:    pw.sess.TotalOutputTokens,
		PeakContextTokens:    pw.sess.PeakContextTokens,
		HasTotalOutputTokens: hasTotal,
		HasPeakContextTokens: hasPeak,
		FilePath:             strPtr(pw.sess.File.Path),
		FileSize:             int64Ptr(pw.sess.File.Size),
		FileMtime:            int64Ptr(pw.sess.File.Mtime),
		FileHash:             strPtr(pw.sess.File.Hash),
		TerminationStatus:    strPtr(string(pw.sess.TerminationStatus)),
	}
	if pw.sess.FirstMessage != "" {
		s.FirstMessage = &pw.sess.FirstMessage
	}
	if !pw.sess.StartedAt.IsZero() {
		s.StartedAt = timeutil.Ptr(pw.sess.StartedAt)
	}
	if !pw.sess.EndedAt.IsZero() {
		s.EndedAt = timeutil.Ptr(pw.sess.EndedAt)
	}
	return s
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/sync/ -run TestToDBSessionTerminationStatus -v`

Expected: PASS — all four subtests.

- [ ] **Step 5: Run full sync tests**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/sync/...`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/sync/engine.go internal/sync/engine_test.go
git commit -m "feat(sync): copy termination_status from parser to db.Session"
```

---

## Phase 4 — PostgreSQL

### Task 7: PG schema (coreDDL + alters + CheckSchemaCompat)

**Files:**
- Modify: `internal/postgres/schema.go` (`coreDDL`, `alters`, `CheckSchemaCompat`)
- Modify: relevant `_pgtest_test.go` for migration coverage if a natural slot exists

- [ ] **Step 1: Add the column to `coreDDL` (fresh databases)**

In `internal/postgres/schema.go`, locate `coreDDL` (line 21). Add `termination_status TEXT` to the `sessions` table definition, after `is_automated` and before `updated_at`:

```go
const coreDDL = `
CREATE TABLE IF NOT EXISTS sync_metadata (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    id                 TEXT PRIMARY KEY,
    machine            TEXT NOT NULL,
    project            TEXT NOT NULL,
    agent              TEXT NOT NULL,
    first_message      TEXT,
    display_name       TEXT,
    created_at         TIMESTAMPTZ,
    started_at         TIMESTAMPTZ,
    ended_at           TIMESTAMPTZ,
    deleted_at         TIMESTAMPTZ,
    message_count      INT NOT NULL DEFAULT 0,
    user_message_count INT NOT NULL DEFAULT 0,
    parent_session_id  TEXT,
    relationship_type  TEXT NOT NULL DEFAULT '',
    total_output_tokens INT NOT NULL DEFAULT 0,
    peak_context_tokens INT NOT NULL DEFAULT 0,
    has_total_output_tokens BOOLEAN NOT NULL DEFAULT FALSE,
    has_peak_context_tokens BOOLEAN NOT NULL DEFAULT FALSE,
    is_automated       BOOLEAN NOT NULL DEFAULT FALSE,
    termination_status TEXT,
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ... rest of coreDDL unchanged ...
```

(Keep the rest of `coreDDL` exactly as it was. Only the `sessions` block changes.)

- [ ] **Step 2: Add to the `alters` slice (existing PG databases)**

In `internal/postgres/schema.go`, locate the `alters` slice inside `EnsureSchema` (around line 156). Append a new entry at the end:

```go
{
	"sessions", "termination_status",
	`ALTER TABLE sessions
	 ADD COLUMN IF NOT EXISTS termination_status TEXT`,
	"adding sessions.termination_status",
},
```

- [ ] **Step 3: Add the index after the alters loop**

In `internal/postgres/schema.go`, after the `for _, a := range alters { ... }` loop and before the `backfillIsAutomatedPG` call, add:

```go
if _, err := db.ExecContext(ctx,
	`CREATE INDEX IF NOT EXISTS idx_sessions_termination_status
	 ON sessions(termination_status)`,
); err != nil {
	return fmt.Errorf(
		"creating idx_sessions_termination_status: %w", err,
	)
}
```

- [ ] **Step 4: Update `CheckSchemaCompat`**

In `internal/postgres/schema.go`, locate `CheckSchemaCompat` (line 785). Add `termination_status` to the existing first sessions probe:

```go
func CheckSchemaCompat(
	ctx context.Context, db *sql.DB,
) error {
	rows, err := db.QueryContext(ctx,
		`SELECT id, created_at, deleted_at, updated_at,
		    termination_status
		 FROM sessions LIMIT 0`)
	if err != nil {
		return fmt.Errorf(
			"sessions table missing required columns: %w",
			err,
		)
	}
	rows.Close()

	// ... rest of function unchanged ...
}
```

- [ ] **Step 5: Build to confirm everything compiles**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go build -tags fts5 ./...`

Expected: success.

- [ ] **Step 6: Run PG migration via integration tests**

Spin up the PG container and run integration tests:

Run: `make test-postgres`

Expected: full PG suite passes. The fresh-schema and ALTER paths both run during test setup; if the column is wrong in either path, the existing tests will fail.

- [ ] **Step 7: Commit**

```bash
git add internal/postgres/schema.go
git commit -m "feat(postgres): add termination_status column, index, and compat probe"
```

---

### Task 8: PG sessions.go reads

**Files:**
- Modify: `internal/postgres/sessions.go` (every `SELECT … FROM sessions` site + scan paths)

- [ ] **Step 1: Identify all sessions SELECT sites**

Run: `nix shell 'nixpkgs#ripgrep' --command rg -n "FROM sessions|SELECT.*sessions" internal/postgres/sessions.go`

Capture the output to a scratch note (you'll touch ~12+ sites). For each, determine whether the SELECT projects full session columns or just specific aggregates. Sites that select only counts, IDs, or specific scalar columns (e.g., `SELECT COUNT(*)`) do NOT need `termination_status` added.

- [ ] **Step 2: Update column-list constants if any exist (mirror the SQLite pattern)**

If `internal/postgres/sessions.go` defines column-list constants like `sessionBaseCols` (search for any `const … sessionCols`), add `termination_status` after `deleted_at` to match SQLite. If no such constants exist (each query inlines its column list), proceed to Step 3.

- [ ] **Step 3: Update every full-session SELECT site**

For each query that returns a full session row (i.e., its scan call assigns into multiple `db.Session` fields), add `termination_status` to the SELECT column list and `&s.TerminationStatus` to the corresponding `Scan` call. The position should match the SQLite ordering: between `deleted_at` and the next column. Apply this systematically — there are ~12+ sites to touch.

For each site, the change pattern is:

```sql
-- Before
SELECT id, project, machine, agent, ...
       deleted_at, ...
FROM sessions ...

-- After
SELECT id, project, machine, agent, ...
       deleted_at, termination_status, ...
FROM sessions ...
```

And in the matching `Scan`:

```go
// Before
err := row.Scan(..., &s.DeletedAt, &s.FilePath, ...)

// After
err := row.Scan(..., &s.DeletedAt, &s.TerminationStatus, &s.FilePath, ...)
```

- [ ] **Step 4: Add termination predicate to `buildPGSessionFilter`**

`buildPGSessionFilter` (line 115) is the PG counterpart to the SQLite `buildSessionFilter`. Without this step, `GET /api/sessions?termination=…` is silently ignored when serving from a PostgreSQL backend. Inside `buildPGSessionFilter`, after the existing predicates, add:

```go
switch f.Termination {
case "clean":
	preds = append(preds, "termination_status = 'clean'")
case "unclean":
	preds = append(preds,
		"termination_status IN ('tool_call_pending', 'truncated')")
}
// "" and "all" add no predicate.
```

(Adapt the local slice/builder name — `preds` here — to whatever the surrounding function uses. Read the function first to match its style.)

- [ ] **Step 5: Build and run unit tests**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go build -tags fts5 ./...`
Then: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/postgres/...`

Expected: success and PASS for non-pgtest tests.

- [ ] **Step 6: Run PG integration tests**

Run: `make test-postgres`

Expected: PASS. If a query site was missed and a Scan now expects more columns than the SELECT provides, the relevant `_pgtest_test.go` will fail loudly with a column-count mismatch.

- [ ] **Step 7: Commit**

```bash
git add internal/postgres/sessions.go
git commit -m "feat(postgres): include termination_status in session reads and filter"
```

---

### Task 9: PG push.go write path

**Files:**
- Modify: `internal/postgres/push.go` (`pushSession`)
- Modify: `internal/postgres/push_pgtest_test.go` (round-trip test)

- [ ] **Step 1: Write a failing push round-trip test**

Add to `internal/postgres/push_pgtest_test.go` (the `pgtest`-tagged push test file):

```go
//go:build pgtest

func TestPushSessionTerminationStatus(t *testing.T) {
	helper := newPushTestHelper(t) // adapt to the existing helper name in this file
	defer helper.Close()

	pending := "tool_call_pending"
	s := db.Session{
		ID:                "term-test-1",
		Project:           "p",
		Machine:           "m",
		Agent:             "claude",
		MessageCount:      1,
		UserMessageCount:  1,
		TerminationStatus: &pending,
		CreatedAt:         "2024-01-01T00:00:00.000Z",
	}

	if err := helper.pushSession(s); err != nil {
		t.Fatalf("push: %v", err)
	}

	var got *string
	if err := helper.pgConn().QueryRow(
		"SELECT termination_status FROM sessions WHERE id = $1",
		s.ID,
	).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}

	if got == nil || *got != "tool_call_pending" {
		t.Fatalf("got %v, want tool_call_pending", got)
	}

	// Update to NULL and verify ON CONFLICT clears it.
	s.TerminationStatus = nil
	if err := helper.pushSession(s); err != nil {
		t.Fatalf("re-push: %v", err)
	}

	if err := helper.pgConn().QueryRow(
		"SELECT termination_status FROM sessions WHERE id = $1",
		s.ID,
	).Scan(&got); err != nil {
		t.Fatalf("read back 2: %v", err)
	}

	if got != nil {
		t.Fatalf("got %v, want NULL", *got)
	}
}
```

(Adapt `newPushTestHelper`, `helper.pushSession`, and `helper.pgConn` to the helper names already in `push_pgtest_test.go`. Read the file first to find the existing convention.)

- [ ] **Step 2: Run test to verify failure**

Run: `make test-postgres` (or the equivalent invocation that runs `pgtest`-tagged tests).

Expected: FAIL — `pushSession` doesn't include `termination_status` yet, so the column stays NULL after the first push.

- [ ] **Step 3: Update `pushSession`**

In `internal/postgres/push.go`, locate `pushSession` (line 680). Update three places:

a) The INSERT column list (line 688) — add `termination_status` after `is_automated`:

```sql
INSERT INTO sessions (
    id, machine, project, agent,
    first_message, display_name,
    created_at, started_at, ended_at, deleted_at,
    message_count, user_message_count,
    total_output_tokens, peak_context_tokens,
    has_total_output_tokens, has_peak_context_tokens,
    is_automated,
    termination_status,
    parent_session_id, relationship_type,
    updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10,
    $11, $12, $13, $14,
    $15, $16, $17, $18, $19, $20, NOW()
)
```

(Note placeholder count goes from 19 to 20.)

b) The ON CONFLICT clause — add `termination_status = EXCLUDED.termination_status`:

```sql
ON CONFLICT (id) DO UPDATE SET
    machine = EXCLUDED.machine,
    project = EXCLUDED.project,
    agent = EXCLUDED.agent,
    first_message = EXCLUDED.first_message,
    display_name = EXCLUDED.display_name,
    created_at = EXCLUDED.created_at,
    started_at = EXCLUDED.started_at,
    ended_at = EXCLUDED.ended_at,
    deleted_at = EXCLUDED.deleted_at,
    message_count = EXCLUDED.message_count,
    user_message_count = EXCLUDED.user_message_count,
    total_output_tokens = EXCLUDED.total_output_tokens,
    peak_context_tokens = EXCLUDED.peak_context_tokens,
    has_total_output_tokens = EXCLUDED.has_total_output_tokens,
    has_peak_context_tokens = EXCLUDED.has_peak_context_tokens,
    is_automated = EXCLUDED.is_automated,
    termination_status = EXCLUDED.termination_status,
    parent_session_id = EXCLUDED.parent_session_id,
    relationship_type = EXCLUDED.relationship_type,
    updated_at = NOW()
```

c) The change-detection WHERE clause — add another DISTINCT-FROM check:

```sql
WHERE sessions.machine IS DISTINCT FROM EXCLUDED.machine
    OR sessions.project IS DISTINCT FROM EXCLUDED.project
    OR sessions.agent IS DISTINCT FROM EXCLUDED.agent
    OR sessions.first_message IS DISTINCT FROM EXCLUDED.first_message
    OR sessions.display_name IS DISTINCT FROM EXCLUDED.display_name
    OR sessions.created_at IS DISTINCT FROM EXCLUDED.created_at
    OR sessions.started_at IS DISTINCT FROM EXCLUDED.started_at
    OR sessions.ended_at IS DISTINCT FROM EXCLUDED.ended_at
    OR sessions.deleted_at IS DISTINCT FROM EXCLUDED.deleted_at
    OR sessions.message_count IS DISTINCT FROM EXCLUDED.message_count
    OR sessions.user_message_count IS DISTINCT FROM EXCLUDED.user_message_count
    OR sessions.total_output_tokens IS DISTINCT FROM EXCLUDED.total_output_tokens
    OR sessions.peak_context_tokens IS DISTINCT FROM EXCLUDED.peak_context_tokens
    OR sessions.has_total_output_tokens IS DISTINCT FROM EXCLUDED.has_total_output_tokens
    OR sessions.has_peak_context_tokens IS DISTINCT FROM EXCLUDED.has_peak_context_tokens
    OR sessions.is_automated IS DISTINCT FROM EXCLUDED.is_automated
    OR sessions.termination_status IS DISTINCT FROM EXCLUDED.termination_status
    OR sessions.parent_session_id IS DISTINCT FROM EXCLUDED.parent_session_id
    OR sessions.relationship_type IS DISTINCT FROM EXCLUDED.relationship_type
```

d) Update the `tx.ExecContext` args to pass `sess.TerminationStatus` in the corresponding position (between `is_automated` and `parent_session_id`):

```go
_, err := tx.ExecContext(ctx, `… SQL above …`,
    sess.ID, sess.Machine, sess.Project, sess.Agent,
    sess.FirstMessage, sess.DisplayName,
    createdAt, sess.StartedAt, sess.EndedAt, sess.DeletedAt,
    sess.MessageCount, sess.UserMessageCount,
    sess.TotalOutputTokens, sess.PeakContextTokens,
    sess.HasTotalOutputTokens, sess.HasPeakContextTokens,
    isAutomated,
    sess.TerminationStatus,
    sess.ParentSessionID, sess.RelationshipType,
)
```

(Read the existing arg order in `push.go` to confirm — adapt to whatever the file currently does.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `make test-postgres`

Expected: PASS, including the new test.

- [ ] **Step 5: Commit**

```bash
git add internal/postgres/push.go internal/postgres/push_pgtest_test.go
git commit -m "feat(postgres): push termination_status through to remote schema"
```

---

### Task 9.5: Analytics filter plumbing

The Sessions analytics tab (Top Sessions table) clicks the unclean count pill to set the same filter shared with the analytics view. Without this task, the analytics chip can be set while charts and Top Sessions ignore it.

**Files:**
- Modify: `internal/db/analytics.go` (`AnalyticsFilter`, `buildAnalyticsWhere`, `TopSession` struct, top-session SELECT/scan)
- Modify: `internal/server/analytics.go` (`parseAnalyticsFilter`)
- Modify: `internal/postgres/analytics.go` (PG analytics query builder, top-session SELECT/scan)
- Modify: `frontend/src/lib/api/types/analytics.ts` (TopSession TS type)
- Modify: relevant `*_test.go` files for round-trip coverage

- [ ] **Step 1: Add `Termination` field to `db.AnalyticsFilter`**

In `internal/db/analytics.go` (struct at line 44), add the new field after the existing filter fields (mirroring the SessionFilter convention from Task 5):

```go
type AnalyticsFilter struct {
	// ... existing fields ...
	Termination string // "", "clean", or "unclean"
}
```

- [ ] **Step 2: Apply the predicate in the SQLite analytics WHERE builder**

In `internal/db/analytics.go`, the WHERE builder is a method on `AnalyticsFilter`: `(f AnalyticsFilter).buildWhere(dateCol)` (called at lines 201, 333, 532, 722, 935). Inside that method, append the same `clean`/`unclean` predicates as `buildSessionFilter`:

```go
switch f.Termination {
case "clean":
	preds = append(preds, "termination_status = 'clean'")
case "unclean":
	preds = append(preds,
		"termination_status IN ('tool_call_pending', 'truncated')")
}
```

(Adapt the local slice/builder variable name to match the surrounding function.)

- [ ] **Step 3: Apply the predicate in PG analytics**

In `internal/postgres/analytics.go`, the standalone function `buildAnalyticsWhere` (line 65) is the PG counterpart. Add the same predicate. Use the local `paramBuilder` convention if present; literal SQL is fine for these enum values since they aren't user input.

- [ ] **Step 4: Read `?termination=` in the analytics HTTP handler**

In `internal/server/analytics.go:49` (`parseAnalyticsFilter`), set `f.Termination = q.Get("termination")` alongside the other URL-param-driven assignments. All analytics endpoints flow through this helper, so they pick it up automatically.

- [ ] **Step 5: Add `TerminationStatus` to `db.TopSession`**

In `internal/db/analytics.go:2040`, add a `TerminationStatus *string \`json:"termination_status,omitempty"\`` field. Update the SQLite top-session SELECT and scan paths to populate it (mirror what Task 4 did for `db.Session`).

- [ ] **Step 6: PG top-session SELECT/scan**

In `internal/postgres/analytics.go`, find the PG top-session query and scan, add `termination_status` to the SELECT and `&t.TerminationStatus` to the Scan in the right position.

- [ ] **Step 7: Add `termination_status` to TS analytics TopSession type**

In `frontend/src/lib/api/types/analytics.ts`, add `termination_status?: string | null;` to the `TopSession` interface (matching the same shape used in core.ts for `Session`).

- [ ] **Step 8: Plumb `termination` into `AnalyticsParams` and the analytics request builders**

The analytics store builds requests via two helpers that pass an `AnalyticsParams`-typed object through to the API client. Without this step, the analytics chip can be set while every `/api/v1/analytics/*` call still omits `termination`.

- In `frontend/src/lib/api/client.ts`, find the `AnalyticsParams` type and add an optional field:

  ```ts
  export interface AnalyticsParams {
    // ... existing fields ...
    termination?: string;
  }
  ```

- In `frontend/src/lib/stores/analytics.svelte.ts`, locate `baseParams()` (line ~249) and `filterParams()` (line ~289). Both are methods on the analytics store class, so use `this.termination` (matching the existing pattern, e.g. `if (this.agent) p.agent = this.agent;` at line 265):

  ```ts
  if (this.termination) p.termination = this.termination;
  ```

- [ ] **Step 9: Round-trip test**

Add a test for `buildAnalyticsWhere` with `Termination = "unclean"` returning rows whose `termination_status` is `tool_call_pending` or `truncated`. Mirror the SessionFilter test pattern from Task 5. If a PG-equivalent test exists, add a parallel case.

Also add a frontend test that asserts `analytics.fetchSummary()` (or similar) results in a `termination=unclean` query param when the store filter is set. Mirror any existing analytics-fetch test in the project.

- [ ] **Step 10: Run tests and commit**

Run: `nix shell 'nixpkgs#go' --command env CGO_ENABLED=1 go test -tags fts5 ./internal/db/ ./internal/server/ ./internal/postgres/ -v` and (if PG integration is in scope) `make test-postgres`. Then `nix shell 'nixpkgs#nodejs' --command sh -c "cd frontend && npm test"`.

Expected: PASS.

```bash
git add internal/db/analytics.go internal/server/analytics.go internal/postgres/analytics.go frontend/src/lib/api/types/analytics.ts frontend/src/lib/api/client.ts frontend/src/lib/stores/analytics.svelte.ts
git commit -m "feat(analytics): wire termination filter through analytics pipeline"
```

---

## Phase 5 — Frontend

### Task 10: TypeScript types + filter stores

**Files:**
- Modify: `frontend/src/lib/api/types/core.ts`
- Modify: `frontend/src/lib/stores/sessions.svelte.ts`
- Modify: `frontend/src/lib/stores/analytics.svelte.ts`
- Modify: `frontend/src/lib/stores/sessions.test.ts`

- [ ] **Step 1: Add `termination_status` to the TS Session type**

In `frontend/src/lib/api/types/core.ts`, locate the `Session` interface (line 9). Add `termination_status?: string | null;` after `deleted_at`:

```ts
export interface Session {
  id: string;
  project: string;
  machine: string;
  agent: string;
  first_message: string | null;
  display_name?: string | null;
  started_at: string | null;
  ended_at: string | null;
  message_count: number;
  user_message_count: number;
  parent_session_id?: string;
  relationship_type?: string;
  deleted_at?: string | null;
  termination_status?: string | null;
  file_path?: string;
  file_size?: number;
  file_mtime?: number;
  total_output_tokens: number;
  peak_context_tokens: number;
  has_total_output_tokens?: boolean;
  has_peak_context_tokens?: boolean;
  is_automated: boolean;
  created_at: string;
}
```

- [ ] **Step 2: Add `termination` to the sessions filter store**

In `frontend/src/lib/stores/sessions.svelte.ts`, find the filter struct and serialization. Look near the existing `agent`, `machine`, `date` fields. Add a `termination` field with default `""`, include it in the URL serialization (`p["termination"] = f.termination` when non-empty), and in the deserialization (`termination: params["termination"] ?? ""`).

The exact structure depends on the file — read it first, then add `termination` everywhere `agent` or `machine` appears as a filter field.

- [ ] **Step 3: Add `termination` to the analytics store**

In `frontend/src/lib/stores/analytics.svelte.ts`, find the equivalent filter state (the `agent` field is at line 66). Add a `termination: string = $state("")` alongside it. If the analytics store already syncs to `sessions.filters` (e.g., `sessions.filters.agent = …` at line 150), add the same sync for `termination`.

- [ ] **Step 4: Write tests for filter serialization**

In `frontend/src/lib/stores/sessions.test.ts`, add or extend a test that verifies setting `termination` to `"unclean"` produces `termination=unclean` in the API URL. Follow the pattern of any existing `agent`/`machine` filter test in that file. If no such pattern exists, write a small test that:

1. Constructs the filter object with `termination: "unclean"`.
2. Calls the URL serialization helper.
3. Asserts the resulting URL contains `termination=unclean`.

- [ ] **Step 5: Run frontend tests**

Run: `nix shell 'nixpkgs#nodejs' --command sh -c "cd frontend && npm test"`

Expected: PASS, including the new termination filter test.

- [ ] **Step 6: Commit**

```bash
git add frontend/src/lib/api/types/core.ts frontend/src/lib/stores/sessions.svelte.ts frontend/src/lib/stores/analytics.svelte.ts frontend/src/lib/stores/sessions.test.ts
git commit -m "feat(frontend): add termination filter to session and analytics stores"
```

---

### Task 11: Active filter chip + control UI

**Files:**
- Modify: `frontend/src/lib/components/analytics/ActiveFilters.svelte` (chip)
- Modify or create: a sidebar filter control surface (depends on existing patterns)

- [ ] **Step 1: Add the chip rendering in `ActiveFilters.svelte`**

In `frontend/src/lib/components/analytics/ActiveFilters.svelte`, find the section that renders chips for `analytics.agent`, `analytics.project`, etc. Add a new chip for `analytics.termination` when it equals `"unclean"` or `"clean"`.

The chip should:
- Use the existing `filter-chip` class (or equivalent — match the agent/project chip styling).
- Display the label (e.g., "Status: Unclean" or "Status: Clean").
- Have a clear button (`×`) that resets the filter via `analytics.termination = ""` (or a `clearTermination()` helper if the store has a similar pattern for other filters).

Update the `filterCount` derivation in the same file to add `+ (analytics.termination !== "" ? 1 : 0)`.

- [ ] **Step 2: Add a control to set the filter**

Find the existing controls that set `analytics.agent` or similar (look in components alongside `ActiveFilters.svelte`, in the analytics page, or in the sidebar-filter UI). Add a status selector that mirrors the existing pattern.

For an analytics tab that already has typeahead/dropdown filters, a simple three-option selector (All / Clean / Unclean) is appropriate. For a sidebar-filter row that uses chips/buttons, follow that pattern.

If no good place to add the picker exists yet, add a small select element near the other filter controls. The exact location is implementer's choice — match what surrounds the agent picker.

- [ ] **Step 3: Frontend smoke test (browser visual check)**

Run the dev server:

```bash
nix shell 'nixpkgs#nodejs' --command sh -c "cd frontend && npm run dev" &
nix shell 'nixpkgs#go' --command sh -c "make dev"
```

Open the app in a browser. Set the status filter to "Unclean" and verify:
- A chip appears in the active-filters row (analytics view).
- The session list updates (you may need to manually mark a session unclean for now — bouncing through the parser populates real values, but for a smoke test pick any session and `UPDATE sessions SET termination_status='tool_call_pending' WHERE id='…'` directly via sqlite3 if needed).
- Clicking the chip's × button resets the filter and the chip disappears.

(This is a manual smoke check; automated coverage comes in Task 14.)

- [ ] **Step 4: Commit**

```bash
git add frontend/src/lib/components/analytics/ActiveFilters.svelte frontend/src/lib/components/<filter-control-file>.svelte
git commit -m "feat(frontend): render termination filter chip and control"
```

---

### Task 12: Session detail page callout banner

**Files:**
- Modify: the parent component that renders `MessageList.svelte` (search for `<MessageList` usage to find it)
- Add or modify: a small banner component

- [ ] **Step 1: Locate the parent component**

Run: `nix shell 'nixpkgs#ripgrep' --command rg -n "<MessageList" frontend/src/`

The parent renders the message list inside the session detail view. Open it.

- [ ] **Step 2: Add the banner above the message list**

Inside the parent component, just above the `<MessageList />` element, add a conditional banner. Assuming the session is exposed as `session` in the component:

```svelte
{#if session?.termination_status === "tool_call_pending"}
  <div class="termination-banner termination-banner--unclean">
    This session ended with a tool call that never received a response.
    The agent process likely terminated before the tool finished.
  </div>
{:else if session?.termination_status === "truncated"}
  <div class="termination-banner termination-banner--unclean">
    The session file ends mid-write. The agent process likely
    terminated abruptly.
  </div>
{/if}

<MessageList ... />
```

Add corresponding CSS in the same component, matching existing warning/info styling in the codebase (look at how `continuation-badge` or other status indicators are styled for color cues — typically amber/warning):

```css
.termination-banner {
  margin: 0 0 0.75rem;
  padding: 0.625rem 0.875rem;
  border-radius: 6px;
  font-size: 0.875rem;
  line-height: 1.4;
}
.termination-banner--unclean {
  background: var(--warning-bg, #fef3c7);
  color: var(--warning-fg, #92400e);
  border: 1px solid var(--warning-border, #fcd34d);
}
```

(Tweak the CSS variable references to match the project's existing tokens — search the codebase for existing warning colors first.)

- [ ] **Step 3: Add a component test**

Add or update a test alongside the parent component (e.g., `<ParentComponent>.test.ts`):

```ts
import { render } from "@testing-library/svelte";
import { describe, it, expect } from "vitest";
import ParentComponent from "./ParentComponent.svelte";

describe("session detail termination banner", () => {
  it("renders banner for tool_call_pending", () => {
    const session = {
      id: "s1",
      // ...minimum required fields...
      termination_status: "tool_call_pending",
    };
    const { getByText } = render(ParentComponent, { session });
    expect(getByText(/tool call that never received a response/i))
      .toBeInTheDocument();
  });

  it("renders banner for truncated", () => {
    const session = { /* ... */, termination_status: "truncated" };
    const { getByText } = render(ParentComponent, { session });
    expect(getByText(/ends mid-write/i)).toBeInTheDocument();
  });

  it("renders no banner for clean", () => {
    const session = { /* ... */, termination_status: "clean" };
    const { queryByText } = render(ParentComponent, { session });
    expect(queryByText(/tool call/i)).not.toBeInTheDocument();
  });

  it("renders no banner for null", () => {
    const session = { /* ... */, termination_status: null };
    const { queryByText } = render(ParentComponent, { session });
    expect(queryByText(/tool call/i)).not.toBeInTheDocument();
  });
});
```

(Adapt the import paths and minimum-required-fields to whatever the parent component needs.)

- [ ] **Step 4: Run frontend tests**

Run: `nix shell 'nixpkgs#nodejs' --command sh -c "cd frontend && npm test"`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add frontend/src/lib/components/<parent-component>.svelte frontend/src/lib/components/<parent-component>.test.ts
git commit -m "feat(frontend): show termination callout on session detail page"
```

---

### Task 13: Top Sessions Status column

The spec's "Sessions analytics tab" choice points at the analytics view, where the count pill click-toggles the analytics filter. The primary target is therefore `analytics/TopSessions.svelte`. The Usage tab also has a Top Sessions table (`usage/TopSessionsTable.svelte`) — apply the same column there too if it has access to `termination_status` from its data source.

**Files:**
- Modify: `frontend/src/lib/components/analytics/TopSessions.svelte` (primary)
- Modify: `frontend/src/lib/components/usage/TopSessionsTable.svelte` (secondary; only if its rows include `termination_status`)

**Prerequisites:** Task 9.5 must be in place. The analytics Top Sessions endpoint needs to return `termination_status` on each row. Without that backend change, this UI step has nothing to render.

- [ ] **Step 1: Read the existing table structure**

Open `frontend/src/lib/components/analytics/TopSessions.svelte`. Locate the column headers and the row template. Then look at `usage/TopSessionsTable.svelte` to confirm its row shape; if its data source is the same backend endpoint, the changes apply identically.

- [ ] **Step 2: Add a Status column**

Add a `<th>` for "Status" in the headers (matching the existing column markup). Add a corresponding `<td>` in each row that renders:

- A small green dot for `clean`.
- An amber warning glyph (e.g., ⚠ in a styled span) for `tool_call_pending` or `truncated`. Use a `title` attribute for the tooltip with the specific reason.
- An em-dash (—) for null/undefined, with `title="Status not determined"`.

Sketch:

```svelte
<th class="status-col">Status</th>

<!-- in each row: -->
<td class="status-col">
  {#if session.termination_status === "clean"}
    <span class="status-dot status-dot--clean" title="Clean exit">●</span>
  {:else if session.termination_status === "tool_call_pending"}
    <span class="status-glyph status-glyph--unclean"
          title="Ended with an unmatched tool call">⚠</span>
  {:else if session.termination_status === "truncated"}
    <span class="status-glyph status-glyph--unclean"
          title="Session file ends mid-write">⚠</span>
  {:else}
    <span class="status-dash" title="Status not determined">—</span>
  {/if}
</td>
```

Add corresponding CSS:

```css
.status-dot--clean { color: var(--ok-fg, #10b981); }
.status-glyph--unclean { color: var(--warning-fg, #f59e0b); }
.status-dash { color: var(--muted-fg, #9ca3af); }
```

- [ ] **Step 3: Make the column sortable (if other columns are sortable)**

If the table component already has a sort mechanism (look for `sortBy` or `sortColumn` reactive state), add `termination_status` as a sortable key. The sort order should be: unclean → clean → null (or whatever ordering the user finds useful — implementer's call). If the table isn't sortable, skip this step.

- [ ] **Step 4: Add the count pill**

Above the table, add a small pill showing the count of unclean rows in the currently-loaded sessions: "N unclean". Compute the count client-side from the rendered sessions array (no extra API call):

```svelte
<script lang="ts">
  const uncleanCount = $derived(
    sessions.filter(s =>
      s.termination_status === "tool_call_pending" ||
      s.termination_status === "truncated"
    ).length
  );
</script>

{#if uncleanCount > 0}
  <button
    class="status-count-pill"
    onclick={() => analytics.termination = "unclean"}
  >
    {uncleanCount} unclean
  </button>
{/if}
```

(Adapt the analytics store reference to match what's in scope.)

- [ ] **Step 5: Component test**

Add to `frontend/src/lib/components/analytics/TopSessions.test.ts` (create if missing):

```ts
import { render } from "@testing-library/svelte";
import { describe, it, expect } from "vitest";
import TopSessions from "./TopSessions.svelte";

describe("TopSessions status column", () => {
  it("renders clean dot, unclean glyph, and dash", () => {
    const sessions = [
      { id: "s1", termination_status: "clean", /* min fields */ },
      { id: "s2", termination_status: "tool_call_pending", /* ... */ },
      { id: "s3", termination_status: "truncated", /* ... */ },
      { id: "s4", termination_status: null, /* ... */ },
    ];
    const { container } = render(TopSessions, { sessions });
    expect(container.querySelector(".status-dot--clean")).toBeTruthy();
    expect(container.querySelectorAll(".status-glyph--unclean")).toHaveLength(2);
    expect(container.querySelector(".status-dash")).toBeTruthy();
  });
});
```

(Mirror the test for `usage/TopSessionsTable.test.ts` if you also added the column there.)

- [ ] **Step 6: Run frontend tests**

Run: `nix shell 'nixpkgs#nodejs' --command sh -c "cd frontend && npm test"`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add frontend/src/lib/components/analytics/TopSessions.svelte frontend/src/lib/components/analytics/TopSessions.test.ts frontend/src/lib/components/usage/TopSessionsTable.svelte frontend/src/lib/components/usage/TopSessionsTable.test.ts 2>/dev/null
git commit -m "feat(frontend): add Status column to Top Sessions table"
```

(Drop the `usage/` paths from `git add` if you didn't modify them.)

---

### Task 14: E2E test

**Files:**
- Create or extend: `frontend/e2e/<termination>.spec.ts`

- [ ] **Step 1: Inspect existing e2e setup**

Run: `nix shell 'nixpkgs#fd' --command fd '.spec.ts' frontend/e2e`

Read one or two existing specs to understand how they prepare fixtures and start the server.

- [ ] **Step 2: Write the e2e spec**

Create `frontend/e2e/termination.spec.ts`:

```ts
import { test, expect } from "@playwright/test";

test.describe("session termination status", () => {
  test("unclean session shows banner on detail page", async ({ page }) => {
    // Setup: server fixture should be configured to load a session
    // with termination_status='tool_call_pending'. Adapt to the
    // existing fixture-setup pattern in scripts/ or other specs.
    await page.goto("/sessions/<fixture-session-id>");
    await expect(
      page.getByText(/tool call that never received a response/i),
    ).toBeVisible();
  });

  test("status filter shows only unclean sessions", async ({ page }) => {
    await page.goto("/");
    // Click the status filter control and select "Unclean"
    // (selectors depend on Task 11 implementation).
    await page.getByRole("button", { name: /status/i }).click();
    await page.getByRole("menuitem", { name: /unclean/i }).click();

    // The chip appears and the session list narrows.
    await expect(page.getByText(/Status: Unclean/i)).toBeVisible();
  });

  test("Top Sessions table shows status column", async ({ page }) => {
    // analytics/TopSessions.svelte renders inside AnalyticsPage,
    // which appears in the right pane when no session is selected
    // (see App.svelte). There is no /analytics route — visit / and
    // do not select a session.
    await page.goto("/");
    await expect(
      page.locator(".status-glyph--unclean").first(),
    ).toBeVisible();
  });
});
```

(Adapt selectors and fixture setup to match the project's existing e2e conventions — read other specs in `frontend/e2e/` first.)

- [ ] **Step 3: Run e2e**

Run: `make e2e`

Expected: the new spec passes alongside the rest.

- [ ] **Step 4: Commit**

```bash
git add frontend/e2e/termination.spec.ts
git commit -m "test(e2e): cover termination status filter, banner, and column"
```

---

## Final verification

- [ ] **Step 1: Run full backend test suite**

Run: `make test`

Expected: PASS.

- [ ] **Step 2: Run lint and vet**

Run: `make lint && make vet`

Expected: clean.

- [ ] **Step 3: Run PG integration tests**

Run: `make test-postgres`

Expected: PASS.

- [ ] **Step 4: Run e2e**

Run: `make e2e`

Expected: PASS.

- [ ] **Step 5: Manual smoke test**

Build and run the app, sync your session corpus, verify:
- The first launch after the upgrade triggers the resync (look for the log line `data version outdated; full resync required`).
- After resync completes, sessions show their classified statuses.
- Filter, banner, and analytics column all behave as designed.

If everything passes, the feature is ready for PR.
