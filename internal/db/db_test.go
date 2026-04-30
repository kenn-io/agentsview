package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// filterWith returns a SessionFilter with Limit defaulted to 100.
func filterWith(fn func(*SessionFilter)) SessionFilter {
	f := SessionFilter{Limit: 100}
	fn(&f)
	return f
}

// sessionSet inserts 3 sessions with sequential dates and
// increasing message counts (5, 15, 25).
func sessionSet(t *testing.T, d *DB) {
	t.Helper()
	for i, mc := range []int{5, 15, 25} {
		day := fmt.Sprintf("2024-06-0%dT10:00:00Z", i+1)
		end := fmt.Sprintf("2024-06-0%dT11:00:00Z", i+1)
		insertSession(t, d, fmt.Sprintf("s%d", i+1),
			"proj", func(s *Session) {
				s.StartedAt = new(day)
				s.EndedAt = new(end)
				s.MessageCount = mc
			})
	}
}

// requireCount lists sessions with filter and asserts the count.
func requireCount(
	t *testing.T, d *DB, f SessionFilter, want int,
) {
	t.Helper()
	page, err := d.ListSessions(
		context.Background(), f,
	)
	requireNoError(t, err, "ListSessions")
	if got := len(page.Sessions); got != want {
		t.Errorf("got %d sessions, want %d", got, want)
	}
}

// requireSessions lists sessions with filter and asserts the exact IDs returned.
func requireSessions(
	t *testing.T, d *DB, f SessionFilter, wantIDs []string,
) {
	t.Helper()
	page, err := d.ListSessions(
		context.Background(), f,
	)
	requireNoError(t, err, "ListSessions")

	gotIDs := collectIDs(page.Sessions)
	wantSorted := make([]string, len(wantIDs))
	copy(wantSorted, wantIDs)
	slices.Sort(wantSorted)

	gotSorted := make([]string, len(gotIDs))
	copy(gotSorted, gotIDs)
	slices.Sort(gotSorted)

	if diff := cmp.Diff(wantSorted, gotSorted); diff != "" {
		t.Errorf("sessions mismatch (-want +got):\n%s", diff)
	}
}

// requireNoError fails the test if err is not nil.
func requireNoError(t *testing.T, err error, msg string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

// requireErrContains fails if err is nil or doesn't contain
// substr.
func requireErrContains(
	t *testing.T, err error, substr string,
) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), substr) {
		t.Errorf("error %q does not contain %q",
			err.Error(), substr)
	}
}

const (
	defaultMachine = "local"
	defaultAgent   = "claude"

	// Timestamp constants for test data.
	tsZero    = "2024-01-01T00:00:00Z"
	tsZeroS1  = "2024-01-01T00:00:01Z"
	tsZeroS2  = "2024-01-01T00:00:02Z"
	tsHour1   = "2024-01-01T01:00:00Z"
	tsMidYear = "2024-06-01T10:00:00Z"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := Open(path)
	requireNoError(t, err, "opening test db")
	t.Cleanup(func() { d.Close() })
	return d
}

// Ptr returns a pointer to v.
//
//go:fix inline
func Ptr[T any](v T) *T { return new(v) }

// insertSession creates and upserts a session with sensible
// defaults. Override any field via the opts functions.
func insertSession(
	t *testing.T, d *DB, id, project string,
	opts ...func(*Session),
) {
	t.Helper()
	s := Session{
		ID:           id,
		Project:      project,
		Machine:      defaultMachine,
		Agent:        defaultAgent,
		MessageCount: 1,
	}
	for _, opt := range opts {
		opt(&s)
	}
	if err := d.UpsertSession(s); err != nil {
		t.Fatalf("insertSession %s: %v", id, err)
	}
}

// updateSignals is a helper that updates session signal columns
// and fails the test on error.
func updateSignals(
	t *testing.T, d *DB, id string, u SessionSignalUpdate,
) {
	t.Helper()
	if err := d.UpdateSessionSignals(id, u); err != nil {
		t.Fatalf("updateSignals %s: %v", id, err)
	}
}

// insertMessages is a helper that inserts messages and fails
// the test on error.
func insertMessages(t *testing.T, d *DB, msgs ...Message) {
	t.Helper()
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("insertMessages: %v", err)
	}
}

// userMsg creates a user message with the given content.
func userMsg(sid string, ordinal int, content string) Message {
	return Message{
		SessionID:     sid,
		Ordinal:       ordinal,
		Role:          "user",
		Content:       content,
		ContentLength: len(content),
		Timestamp:     tsZero,
	}
}

// asstMsg creates an assistant message with the given content.
func asstMsg(sid string, ordinal int, content string) Message {
	return Message{
		SessionID:     sid,
		Ordinal:       ordinal,
		Role:          "assistant",
		Content:       content,
		ContentLength: len(content),
		Timestamp:     tsZero,
	}
}

// userMsgAt creates a user message with the given content and
// timestamp.
func userMsgAt(
	sid string, ordinal int, content, ts string,
) Message {
	m := userMsg(sid, ordinal, content)
	m.Timestamp = ts
	return m
}

// asstMsgAt creates an assistant message with the given content
// and timestamp.
func asstMsgAt(
	sid string, ordinal int, content, ts string,
) Message {
	m := asstMsg(sid, ordinal, content)
	m.Timestamp = ts
	return m
}

type msgBuilder struct {
	id   string
	ord  int
	msgs []Message
}

func (b *msgBuilder) user(content string) {
	b.msgs = append(b.msgs, userMsg(b.id, b.ord, content))
	b.ord++
}

func (b *msgBuilder) asst(content string) {
	b.msgs = append(b.msgs, asstMsg(b.id, b.ord, content))
	b.ord++
}

// canceledCtx returns an already-canceled context.
func canceledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// requireCanceledErr asserts that err is context.Canceled.
func requireCanceledErr(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error from canceled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

// requireFTS skips the test if FTS is not available.
func requireFTS(t *testing.T, d *DB) {
	t.Helper()
	if !d.HasFTS() {
		t.Skip("no FTS support")
	}
}

// requireSessionExists asserts that a session exists and returns it.
func requireSessionExists(t *testing.T, d *DB, id string) *Session {
	t.Helper()
	s, err := d.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession %q: %v", id, err)
	}
	if s == nil {
		t.Fatalf("session %q should exist", id)
	}
	return s
}

// requireSessionGone asserts that a session does not exist.
func requireSessionGone(t *testing.T, d *DB, id string) {
	t.Helper()
	s, err := d.GetSession(context.Background(), id)
	if err != nil {
		t.Fatalf("GetSession %q: %v", id, err)
	}
	if s != nil {
		t.Fatalf("session %q should be gone", id)
	}
}

func TestOpenCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "test.db")
	d, err := Open(path)
	requireNoError(t, err, "Open")
	defer d.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not created: %v", err)
	}
}

func TestOpenDataVersionBump_PreservesData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Create a valid DB (sets user_version = dataVersion).
	d, err := Open(path)
	requireNoError(t, err, "initial open")

	err = d.UpsertSession(Session{
		ID:           "s1",
		Project:      "proj",
		Machine:      "local",
		Agent:        "codex",
		MessageCount: 1,
		FileMtime:    new(int64(12345)),
	})
	requireNoError(t, err, "insert session")
	insertMessages(t, d,
		userMsg("s1", 0, "hello"),
		asstMsg("s1", 1, "world"),
	)

	// Add a skipped file entry.
	err = d.ReplaceSkippedFiles(map[string]int64{
		"/tmp/skip.jsonl": 99999,
	})
	requireNoError(t, err, "add skipped file")
	d.Close()

	// Set user_version to 0 to simulate stale data version.
	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")
	_, err = conn.Exec("PRAGMA user_version = 0")
	requireNoError(t, err, "reset version")
	conn.Close()

	// Re-open: should detect stale version but preserve data.
	d2, err := Open(path)
	requireNoError(t, err, "reopen")
	defer d2.Close()

	// NeedsResync should be true.
	if !d2.NeedsResync() {
		t.Fatal("expected NeedsResync()=true after version bump")
	}

	// Session and messages must still exist.
	page, err := d2.ListSessions(
		context.Background(),
		SessionFilter{Limit: 100},
	)
	requireNoError(t, err, "list sessions")
	if len(page.Sessions) != 1 {
		t.Fatalf("expected 1 session preserved, got %d",
			len(page.Sessions))
	}

	msgs, err := d2.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "get messages")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages preserved, got %d",
			len(msgs))
	}

	// user_version must stay stale — it is only bumped
	// after a successful ResyncAll, not at Open() time.
	var ver int
	err = d2.getReader().QueryRow(
		"PRAGMA user_version",
	).Scan(&ver)
	requireNoError(t, err, "read version")
	if ver != 0 {
		t.Fatalf("expected user_version=0 (stale), got %d",
			ver)
	}
}

func TestOpenDataVersionBump_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Create a DB and downgrade its version.
	d, err := Open(path)
	requireNoError(t, err, "initial open")
	insertSession(t, d, "s1", "proj")
	d.Close()

	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")
	_, err = conn.Exec("PRAGMA user_version = 0")
	requireNoError(t, err, "reset version")
	conn.Close()

	// First reopen: detects stale, does NOT bump version.
	d2, err := Open(path)
	requireNoError(t, err, "reopen 1")
	if !d2.NeedsResync() {
		t.Fatal("first reopen: expected NeedsResync=true")
	}
	d2.Close() // simulate process exit without resync

	// Second reopen: must still detect stale because the
	// version was not bumped.
	d3, err := Open(path)
	requireNoError(t, err, "reopen 2")
	defer d3.Close()
	if !d3.NeedsResync() {
		t.Fatal("second reopen: expected NeedsResync=true")
	}
}

func TestMigration_ResultContentColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Create a DB with the current schema then drop the
	// result_content column to simulate a pre-migration DB.
	d, err := Open(path)
	requireNoError(t, err, "initial open")
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d,
		userMsg("s1", 0, "hello"),
		Message{
			SessionID:  "s1",
			Ordinal:    1,
			Role:       "assistant",
			Content:    "Let me read that.",
			HasToolUse: true,
			ToolCalls: []ToolCall{{
				SessionID:           "s1",
				ToolName:            "Read",
				Category:            "Read",
				ToolUseID:           "tu1",
				ResultContentLength: 42,
			}},
		},
	)
	d.Close()

	// Remove result_content via raw SQL: recreate tool_calls
	// without the column to simulate a legacy schema.
	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")
	_, err = conn.Exec(`
		CREATE TABLE tool_calls_old AS
			SELECT id, message_id, session_id, tool_name,
			       category, tool_use_id, input_json,
			       skill_name, result_content_length,
			       subagent_session_id
			FROM tool_calls;
		DROP TABLE tool_calls;
		ALTER TABLE tool_calls_old RENAME TO tool_calls;
	`)
	requireNoError(t, err, "drop result_content column")

	// Verify column is gone and tool_calls row exists.
	var count int
	err = conn.QueryRow(
		`SELECT count(*) FROM pragma_table_info('tool_calls')` +
			` WHERE name = 'result_content'`,
	).Scan(&count)
	requireNoError(t, err, "verify column removed")
	if count != 0 {
		t.Fatal("expected result_content column to be absent")
	}
	var tcCount int
	err = conn.QueryRow(
		`SELECT count(*) FROM tool_calls`,
	).Scan(&tcCount)
	requireNoError(t, err, "count tool_calls pre-migration")
	if tcCount != 1 {
		t.Fatalf("expected 1 tool_call row, got %d", tcCount)
	}
	conn.Close()

	// Reopen with Open() — migration should add the column.
	d2, err := Open(path)
	requireNoError(t, err, "reopen after migration")
	defer d2.Close()

	// Verify column exists.
	err = d2.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('tool_calls')` +
			` WHERE name = 'result_content'`,
	).Scan(&count)
	requireNoError(t, err, "verify column added")
	if count != 1 {
		t.Fatal("expected result_content column after migration")
	}

	// Verify tool_calls row preserved with fields intact.
	msgs, err := d2.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "get messages")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d",
			len(msgs[1].ToolCalls))
	}
	tc := msgs[1].ToolCalls[0]
	if tc.ToolName != "Read" {
		t.Errorf("ToolName = %q, want Read", tc.ToolName)
	}
	if tc.ToolUseID != "tu1" {
		t.Errorf("ToolUseID = %q, want tu1", tc.ToolUseID)
	}
	if tc.ResultContentLength != 42 {
		t.Errorf("ResultContentLength = %d, want 42",
			tc.ResultContentLength)
	}
	if tc.ResultContent != "" {
		t.Errorf("ResultContent = %q, want empty (NULL)",
			tc.ResultContent)
	}
}

func TestMigration_ToolResultEventsTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d, err := Open(path)
	requireNoError(t, err, "initial open")
	insertSession(t, d, "s1", "proj")
	d.Close()

	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")
	legacyVersion := dataVersion - 1
	_, err = conn.Exec(fmt.Sprintf(`
		DROP TABLE tool_result_events;
		PRAGMA user_version = %d;
	`, legacyVersion))
	requireNoError(t, err, "drop tool_result_events")

	var count int
	err = conn.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'table' AND name = 'tool_result_events'`,
	).Scan(&count)
	requireNoError(t, err, "verify table removed")
	if count != 0 {
		t.Fatal("expected tool_result_events table to be absent")
	}
	requireNoError(t, conn.Close(), "close legacy db")

	d2, err := Open(path)
	requireNoError(t, err, "reopen after migration")
	defer d2.Close()

	requireSessionExists(t, d2, "s1")
	if !d2.NeedsResync() {
		t.Fatal("expected NeedsResync()=true after data version bump")
	}

	err = d2.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'table' AND name = 'tool_result_events'`,
	).Scan(&count)
	requireNoError(t, err, "verify table exists")
	if count != 1 {
		t.Fatal("expected tool_result_events table after reopen")
	}
}

func TestInsertMessages_PreservesToolResultEvents(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s-events", "proj")

	err := d.InsertMessages([]Message{
		{
			SessionID:  "s-events",
			Ordinal:    0,
			Role:       "assistant",
			Content:    "tool use response",
			HasToolUse: true,
			ToolCalls: []ToolCall{
				{
					SessionID:           "s-events",
					ToolName:            "wait",
					Category:            "Task",
					ToolUseID:           "call_wait",
					ResultContentLength: 9,
					ResultContent:       "latest one",
					ResultEvents: []ToolResultEvent{
						{
							ToolUseID:         "call_wait",
							AgentID:           "agent-1",
							SubagentSessionID: "codex:agent-1",
							Source:            "wait_output",
							Status:            "completed",
							Content:           "first result",
							ContentLength:     len("first result"),
							Timestamp:         "2026-03-27T10:00:00Z",
							EventIndex:        0,
						},
						{
							ToolUseID:         "call_wait",
							AgentID:           "agent-2",
							SubagentSessionID: "codex:agent-2",
							Source:            "subagent_notification",
							Status:            "errored",
							Content:           "second result",
							ContentLength:     len("second result"),
							Timestamp:         "2026-03-27T10:01:00Z",
							EventIndex:        1,
						},
					},
				},
			},
		},
	})
	requireNoError(t, err, "InsertMessages")

	msgs, err := d.GetMessages(context.Background(), "s-events", 0, 100, true)
	requireNoError(t, err, "GetMessages")
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("got %d tool_calls, want 1", len(msgs[0].ToolCalls))
	}
	tc := msgs[0].ToolCalls[0]
	if len(tc.ResultEvents) != 2 {
		t.Fatalf("got %d result events, want 2", len(tc.ResultEvents))
	}
	if tc.ResultEvents[0].AgentID != "agent-1" {
		t.Errorf("result event 0 agent_id = %q, want %q", tc.ResultEvents[0].AgentID, "agent-1")
	}
	if tc.ResultEvents[1].Source != "subagent_notification" {
		t.Errorf("result event 1 source = %q, want %q", tc.ResultEvents[1].Source, "subagent_notification")
	}
}

func TestOpenPreservesDataAtCurrentVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d, err := Open(path)
	requireNoError(t, err, "initial open")
	err = d.UpsertSession(Session{
		ID:           "s1",
		Project:      "proj",
		Machine:      "local",
		Agent:        "codex",
		MessageCount: 1,
	})
	requireNoError(t, err, "insert session")
	d.Close()

	// Re-open without changing user_version: data survives.
	d2, err := Open(path)
	requireNoError(t, err, "reopen")
	defer d2.Close()

	if d2.NeedsResync() {
		t.Fatal("expected NeedsResync()=false at current version")
	}

	page, err := d2.ListSessions(
		context.Background(),
		SessionFilter{Limit: 100},
	)
	requireNoError(t, err, "list sessions")
	if len(page.Sessions) != 1 {
		t.Fatalf("expected 1 session preserved, got %d",
			len(page.Sessions))
	}
}

func TestOpenDoesNotDowngradeUserVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d, err := Open(path)
	requireNoError(t, err, "initial open")

	// Simulate a newer build by setting user_version higher
	// than our dataVersion.
	futureVersion := dataVersion + 10
	_, err = d.getWriter().Exec(
		fmt.Sprintf("PRAGMA user_version = %d", futureVersion),
	)
	requireNoError(t, err, "set future version")
	d.Close()

	// Reopen with current (lower) dataVersion.
	d2, err := Open(path)
	requireNoError(t, err, "reopen")
	defer d2.Close()

	var version int
	err = d2.getWriter().QueryRow(
		"PRAGMA user_version",
	).Scan(&version)
	requireNoError(t, err, "read version")

	if version != futureVersion {
		t.Errorf(
			"user_version = %d, want %d (should not downgrade)",
			version, futureVersion,
		)
	}
	if d2.NeedsResync() {
		t.Error("NeedsResync should be false for higher version")
	}
}

func TestOpenProbeErrorPropagates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping: chmod semantics differ on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("skipping: running as root")
	}

	t.Run("StatPermissionError", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sub, "test.db")

		d, err := Open(path)
		requireNoError(t, err, "setup")
		d.Close()

		// Remove execute on parent dir so os.Stat fails
		// with EACCES, not ENOENT.
		if err := os.Chmod(sub, 0o000); err != nil {
			t.Skipf("cannot remove permissions: %v", err)
		}
		t.Cleanup(func() { os.Chmod(sub, 0o755) })

		_, err = Open(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !errors.Is(err, fs.ErrPermission) {
			t.Errorf("expected permission error, got: %v",
				err)
		}
		if !strings.Contains(err.Error(),
			"checking schema") {
			t.Errorf("expected 'checking schema' wrapper: %v",
				err)
		}
	})

	t.Run("ProbeReadError", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "test.db")

		d, err := Open(path)
		requireNoError(t, err, "setup")
		d.Close()

		// Remove read on the file so os.Stat succeeds
		// but the SQLite probe fails.
		if err := os.Chmod(path, 0o000); err != nil {
			t.Skipf("cannot remove permissions: %v", err)
		}
		t.Cleanup(func() { os.Chmod(path, 0o644) })

		_, err = Open(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(),
			"checking schema") &&
			!strings.Contains(err.Error(),
				"probing schema") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

func TestSessionCRUD(t *testing.T) {
	d := testDB(t)

	s := Session{
		ID:           "test-session-1",
		Project:      "my_project",
		Machine:      defaultMachine,
		Agent:        defaultAgent,
		FirstMessage: new("Hello world"),
		StartedAt:    new(tsZero),
		EndedAt:      new(tsHour1),
		MessageCount: 5,
	}

	if err := d.UpsertSession(s); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got := requireSessionExists(t, d, "test-session-1")
	if got.Project != "my_project" {
		t.Errorf("project = %q, want %q", got.Project, "my_project")
	}
	if got.MessageCount != 5 {
		t.Errorf("message_count = %d, want 5", got.MessageCount)
	}

	// Update
	s.MessageCount = 10
	if err := d.UpsertSession(s); err != nil {
		t.Fatalf("UpsertSession update: %v", err)
	}
	got = requireSessionExists(t, d, "test-session-1")
	if got.MessageCount != 10 {
		t.Errorf("after update: message_count = %d, want 10",
			got.MessageCount)
	}

	// Get nonexistent
	requireSessionGone(t, d, "nonexistent")
}

func TestSessionParentSessionID(t *testing.T) {
	d := testDB(t)

	t.Run("UpsertWithParent", func(t *testing.T) {
		insertSession(t, d, "child-1", "proj", func(s *Session) {
			s.ParentSessionID = new("parent-uuid")
		})

		got := requireSessionExists(t, d, "child-1")
		if got.ParentSessionID == nil ||
			*got.ParentSessionID != "parent-uuid" {
			t.Errorf("parent_session_id = %v, want %q",
				got.ParentSessionID, "parent-uuid")
		}
	})

	t.Run("WithoutParent", func(t *testing.T) {
		insertSession(t, d, "child-2", "proj")

		got := requireSessionExists(t, d, "child-2")
		if got.ParentSessionID != nil {
			t.Errorf("parent_session_id = %v, want nil",
				got.ParentSessionID)
		}
	})

	t.Run("ParentInListSessions", func(t *testing.T) {
		page, err := d.ListSessions(
			context.Background(),
			filterWith(func(f *SessionFilter) {
				f.Project = "proj"
			}),
		)
		requireNoError(t, err, "ListSessions")
		found := false
		for _, s := range page.Sessions {
			if s.ID == "child-1" {
				found = true
				if s.ParentSessionID == nil ||
					*s.ParentSessionID != "parent-uuid" {
					t.Errorf("parent_session_id = %v, want %q",
						s.ParentSessionID, "parent-uuid")
				}
			}
		}
		if !found {
			t.Error("child-1 not found in list")
		}
	})

	t.Run("ParentInGetSessionFull", func(t *testing.T) {
		got, err := d.GetSessionFull(
			context.Background(), "child-1",
		)
		requireNoError(t, err, "GetSessionFull")
		if got == nil {
			t.Fatal("session not found")
			return
		}
		if got.ParentSessionID == nil ||
			*got.ParentSessionID != "parent-uuid" {
			t.Errorf("parent_session_id = %v, want %q",
				got.ParentSessionID, "parent-uuid")
		}
	})
}

func TestGetChildSessions(t *testing.T) {
	d := testDB(t)

	// Insert a parent session.
	insertSession(t, d, "parent-1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.EndedAt = new("2024-06-01T11:00:00Z")
		s.MessageCount = 5
	})

	// Insert child sessions with different relationship types.
	insertSession(t, d, "child-sub", "proj", func(s *Session) {
		s.ParentSessionID = new("parent-1")
		s.RelationshipType = "subagent"
		s.StartedAt = new("2024-06-01T10:05:00Z")
		s.EndedAt = new("2024-06-01T10:10:00Z")
		s.MessageCount = 3
	})
	insertSession(t, d, "child-fork", "proj", func(s *Session) {
		s.ParentSessionID = new("parent-1")
		s.RelationshipType = "fork"
		s.StartedAt = new("2024-06-01T10:20:00Z")
		s.EndedAt = new("2024-06-01T10:30:00Z")
		s.MessageCount = 2
	})
	insertSession(t, d, "child-cont", "proj", func(s *Session) {
		s.ParentSessionID = new("parent-1")
		s.RelationshipType = "continuation"
		s.StartedAt = new("2024-06-01T10:10:00Z")
		s.EndedAt = new("2024-06-01T10:15:00Z")
		s.MessageCount = 4
	})
	insertSession(t, d, "child-deleted", "proj", func(s *Session) {
		s.ParentSessionID = new("parent-1")
		s.RelationshipType = "subagent"
		s.StartedAt = new("2024-06-01T10:07:00Z")
		s.EndedAt = new("2024-06-01T10:08:00Z")
		s.MessageCount = 1
	})
	requireNoError(t, d.SoftDeleteSession("child-deleted"), "SoftDeleteSession")

	// Insert an unrelated session (no parent).
	insertSession(t, d, "unrelated", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 1
	})

	t.Run("ReturnsChildrenOrderedByStartedAt", func(t *testing.T) {
		children, err := d.GetChildSessions(
			context.Background(), "parent-1",
		)
		requireNoError(t, err, "GetChildSessions")
		if len(children) != 3 {
			t.Fatalf("expected 3 visible children, got %d", len(children))
		}
		// Ordered by started_at ascending.
		wantIDs := []string{"child-sub", "child-cont", "child-fork"}
		for i, want := range wantIDs {
			if children[i].ID != want {
				t.Errorf("children[%d].ID = %q, want %q",
					i, children[i].ID, want)
			}
		}
	})

	t.Run("NoChildren", func(t *testing.T) {
		children, err := d.GetChildSessions(
			context.Background(), "unrelated",
		)
		requireNoError(t, err, "GetChildSessions")
		if len(children) != 0 {
			t.Fatalf("expected 0 children, got %d", len(children))
		}
	})

	t.Run("NonexistentParent", func(t *testing.T) {
		children, err := d.GetChildSessions(
			context.Background(), "no-such-parent",
		)
		requireNoError(t, err, "GetChildSessions")
		if len(children) != 0 {
			t.Fatalf("expected 0 children, got %d", len(children))
		}
	})

	t.Run("CanceledContext", func(t *testing.T) {
		_, err := d.GetChildSessions(
			canceledCtx(), "parent-1",
		)
		requireCanceledErr(t, err)
	})
}

func TestListSessions(t *testing.T) {
	d := testDB(t)

	for i := range 5 {
		ea := fmt.Sprintf("2024-01-01T0%d:00:00Z", i)
		insertSession(t, d,
			fmt.Sprintf("session-%c", 'a'+i), "proj",
			func(s *Session) {
				s.EndedAt = new(ea)
				s.MessageCount = i + 1
			},
		)
	}

	requireCount(t, d, SessionFilter{Limit: 10}, 5)

	page, err := d.ListSessions(
		context.Background(), SessionFilter{Limit: 2},
	)
	requireNoError(t, err, "ListSessions limit")
	if len(page.Sessions) != 2 {
		t.Errorf("got %d sessions, want 2", len(page.Sessions))
	}
	if page.NextCursor == "" {
		t.Error("expected next cursor")
	}

	requireCount(t, d, SessionFilter{
		Limit:  10,
		Cursor: page.NextCursor,
	}, 3)
}

func TestListSessionsPaginationNoDuplicates(t *testing.T) {
	d := testDB(t)

	// 5 sessions: 2 share the same ended_at to test
	// tie-breaking at page boundaries.
	times := []string{
		"2024-01-01T01:00:00Z",
		"2024-01-01T02:00:00Z",
		"2024-01-01T02:00:00Z", // same as previous
		"2024-01-01T03:00:00Z",
		"2024-01-01T04:00:00Z",
	}
	for i, ea := range times {
		insertSession(t, d,
			fmt.Sprintf("page-%c", 'a'+i), "proj",
			func(s *Session) { s.EndedAt = new(ea) },
		)
	}

	// Paginate through all sessions 2 at a time.
	seen := make(map[string]bool)
	cursor := ""
	pages := 0
	for {
		page, err := d.ListSessions(
			context.Background(),
			SessionFilter{Limit: 2, Cursor: cursor},
		)
		if err != nil {
			t.Fatalf("ListSessions page %d: %v", pages, err)
		}
		for _, s := range page.Sessions {
			if seen[s.ID] {
				t.Errorf("duplicate session %s on page %d",
					s.ID, pages)
			}
			seen[s.ID] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("saw %d sessions, want 5", len(seen))
	}
}

func TestListSessionsPaginationEmptyTimestamps(t *testing.T) {
	d := testDB(t)

	// Mix of normal, NULL, and empty-string timestamps.
	// Empty-string ended_at/started_at should sort by
	// created_at, same as NULL.
	insertSession(t, d, "s-normal", "proj", func(s *Session) {
		s.EndedAt = new("2024-06-01T12:00:00Z")
		s.StartedAt = new("2024-06-01T10:00:00Z")
	})
	insertSession(t, d, "s-empty-ended", "proj", func(s *Session) {
		s.EndedAt = new("")
		s.StartedAt = new("2024-05-01T10:00:00Z")
	})
	insertSession(t, d, "s-both-empty", "proj", func(s *Session) {
		s.EndedAt = new("")
		s.StartedAt = new("")
	})
	insertSession(t, d, "s-null-ts", "proj")

	// Paginate 1 at a time to exercise cursor encoding.
	seen := make(map[string]bool)
	cursor := ""
	for {
		page, err := d.ListSessions(
			context.Background(),
			SessionFilter{Limit: 1, Cursor: cursor},
		)
		if err != nil {
			t.Fatalf("ListSessions: %v", err)
		}
		for _, s := range page.Sessions {
			if seen[s.ID] {
				t.Errorf("duplicate session %s", s.ID)
			}
			seen[s.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 4 {
		t.Errorf("saw %d sessions, want 4", len(seen))
	}
}

func TestListSessionsProjectFilter(t *testing.T) {
	d := testDB(t)

	for i, proj := range []string{"proj_a", "proj_a", "proj_b"} {
		ea := fmt.Sprintf("2024-01-01T00:00:0%dZ", i)
		insertSession(t, d,
			fmt.Sprintf("%s-%d", proj, i), proj,
			func(s *Session) { s.EndedAt = new(ea) },
		)
	}

	requireCount(t, d, filterWith(func(f *SessionFilter) {
		f.Project = "proj_a"
	}), 2)
}

func TestListSessionsMachineMultiSelect(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s-local", "proj", func(s *Session) {
		s.Machine = "local"
		s.EndedAt = new("2024-01-01T00:00:00Z")
	})
	insertSession(t, d, "s-remote", "proj", func(s *Session) {
		s.Machine = "remote"
		s.EndedAt = new("2024-01-01T00:00:01Z")
	})
	insertSession(t, d, "s-other", "proj", func(s *Session) {
		s.Machine = "other"
		s.EndedAt = new("2024-01-01T00:00:02Z")
	})

	page, err := d.ListSessions(
		context.Background(),
		SessionFilter{
			Machine: "local,other",
			Limit:   10,
		},
	)
	requireNoError(t, err, "ListSessions")
	if page.Total != 2 {
		t.Fatalf("total = %d, want 2", page.Total)
	}

	got := map[string]bool{}
	for _, session := range page.Sessions {
		got[session.Machine] = true
	}
	if !got["local"] {
		t.Fatalf("machines = %v, want local included", got)
	}
	if !got["other"] {
		t.Fatalf("machines = %v, want other included", got)
	}
	if got["remote"] {
		t.Fatalf("machines = %v, want remote excluded", got)
	}
}

func TestMessageCRUD(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.MessageCount = 4
	})

	m1 := userMsg("s1", 0, "Hello")
	m2 := asstMsgAt("s1", 1, "Hi there", tsZeroS1)
	m3 := userMsgAt("s1", 2, "Thanks", tsZeroS2)
	m4 := userMsgAt("s1", 3, "Empty TS", "")

	insertMessages(t, d, m1, m2, m3, m4)

	got, err := d.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4", len(got))
	}
	if got[0].Content != "Hello" {
		t.Errorf("first message = %q", got[0].Content)
	}
	if got[3].Timestamp != "" {
		t.Errorf("expected empty timestamp, got %q", got[3].Timestamp)
	}

	// Paginated
	got, err = d.GetMessages(context.Background(), "s1", 1, 2, true)
	requireNoError(t, err, "GetMessages")
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].Ordinal != 1 {
		t.Errorf("first ordinal = %d, want 1", got[0].Ordinal)
	}

	// Descending
	got, err = d.GetMessages(context.Background(), "s1", 2, 10, false)
	requireNoError(t, err, "GetMessages desc")
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	if got[0].Ordinal != 2 {
		t.Errorf("desc first ordinal = %d, want 2", got[0].Ordinal)
	}
}

func TestReplaceSessionMessages(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")

	insertMessages(t, d, userMsg("s1", 0, "old"))

	if err := d.ReplaceSessionMessages("s1", []Message{
		userMsg("s1", 0, "new1"),
		asstMsg("s1", 1, "new2"),
	}); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	got, _ := d.GetAllMessages(context.Background(), "s1")
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].Content != "new1" {
		t.Errorf("content = %q, want %q", got[0].Content, "new1")
	}
}

// TestReplaceSessionMessagesPreservesPins verifies that pinned
// messages survive a full message replacement (regression test for
// the ON DELETE CASCADE bug: deleting messages used to cascade-delete
// pinned_messages rows).
func TestReplaceSessionMessagesPreservesPins(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "p")
	insertMessages(t, d,
		userMsg("s1", 0, "msg0"),
		asstMsg("s1", 1, "msg1"),
		userMsg("s1", 2, "msg2"),
	)

	msgs, err := d.GetAllMessages(ctx, "s1")
	if err != nil {
		t.Fatalf("GetAllMessages: %v", err)
	}

	// Pin ordinal-0 with a note and ordinal-2 with no note.
	note := "important"
	if _, err := d.PinMessage("s1", msgs[0].ID, &note); err != nil {
		t.Fatalf("PinMessage ord=0: %v", err)
	}
	if _, err := d.PinMessage("s1", msgs[2].ID, nil); err != nil {
		t.Fatalf("PinMessage ord=2: %v", err)
	}

	// Record created_at before replace so we can verify it is preserved.
	prePins, err := d.ListPinnedMessages(ctx, "s1", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages before replace: %v", err)
	}
	pinCreatedAt := make(map[int]string) // ordinal → created_at
	for _, p := range prePins {
		pinCreatedAt[p.Ordinal] = p.CreatedAt
	}

	// Full replace (simulates a resync of an OpenCode or
	// explicitly re-synced session).
	if err := d.ReplaceSessionMessages("s1", []Message{
		userMsg("s1", 0, "msg0-updated"),
		asstMsg("s1", 1, "msg1-updated"),
		userMsg("s1", 2, "msg2-updated"),
	}); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	newMsgs, err := d.GetAllMessages(ctx, "s1")
	if err != nil {
		t.Fatalf("GetAllMessages after replace: %v", err)
	}
	if len(newMsgs) != 3 {
		t.Fatalf("want 3 messages after replace, got %d", len(newMsgs))
	}

	pins, err := d.ListPinnedMessages(ctx, "s1", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pins) != 2 {
		t.Fatalf("want 2 pins after replace, got %d", len(pins))
	}

	byOrdinal := make(map[int]PinnedMessage)
	for _, p := range pins {
		byOrdinal[p.Ordinal] = p
	}

	// Ordinal-0: note preserved, message_id updated, created_at preserved.
	p0, ok := byOrdinal[0]
	if !ok {
		t.Fatal("pin for ordinal 0 missing after replace")
	}
	if p0.MessageID != newMsgs[0].ID {
		t.Errorf("ord=0 pin message_id = %d, want %d",
			p0.MessageID, newMsgs[0].ID)
	}
	if p0.Note == nil || *p0.Note != note {
		t.Errorf("ord=0 pin note = %v, want %q", p0.Note, note)
	}
	if p0.CreatedAt != pinCreatedAt[0] {
		t.Errorf("ord=0 pin created_at = %q, want %q",
			p0.CreatedAt, pinCreatedAt[0])
	}

	// Ordinal-2: nil note preserved, message_id updated.
	p2, ok := byOrdinal[2]
	if !ok {
		t.Fatal("pin for ordinal 2 missing after replace")
	}
	if p2.MessageID != newMsgs[2].ID {
		t.Errorf("ord=2 pin message_id = %d, want %d",
			p2.MessageID, newMsgs[2].ID)
	}
	if p2.Note != nil {
		t.Errorf("ord=2 pin note = %v, want nil", p2.Note)
	}
	if p2.CreatedAt != pinCreatedAt[2] {
		t.Errorf("ord=2 pin created_at = %q, want %q",
			p2.CreatedAt, pinCreatedAt[2])
	}
}

// TestReplaceSessionMessagesDropsPinsForRemovedOrdinals verifies that
// pins whose ordinal no longer exists after a replace are silently
// dropped (the underlying message was removed from the session).
func TestReplaceSessionMessagesDropsPinsForRemovedOrdinals(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "p")
	insertMessages(t, d,
		userMsg("s1", 0, "msg0"),
		asstMsg("s1", 1, "msg1"),
	)

	msgs, err := d.GetAllMessages(ctx, "s1")
	if err != nil {
		t.Fatalf("GetAllMessages: %v", err)
	}
	// Pin both messages.
	for _, m := range msgs {
		if _, err := d.PinMessage("s1", m.ID, nil); err != nil {
			t.Fatalf("PinMessage: %v", err)
		}
	}

	// Replace with only ordinal-0 (ordinal-1 is gone).
	if err := d.ReplaceSessionMessages("s1", []Message{
		userMsg("s1", 0, "msg0-updated"),
	}); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	pins, err := d.ListPinnedMessages(ctx, "s1", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("want 1 pin (ordinal-1 dropped), got %d", len(pins))
	}
	if pins[0].Ordinal != 0 {
		t.Errorf("surviving pin ordinal = %d, want 0", pins[0].Ordinal)
	}
	if pins[0].Note != nil {
		t.Errorf("surviving pin note = %v, want nil", pins[0].Note)
	}
}

// TestReplaceSessionMessagesPinSourceUUIDFollowsRow verifies that a
// pin tracks its message by source_uuid even when the message's
// ordinal shifts on rewrite (e.g. when a new compact-boundary row
// is inserted earlier in the stream). The pin must follow the
// content, not the position.
func TestReplaceSessionMessagesPinSourceUUIDFollowsRow(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "p")
	insertMessages(t, d,
		Message{
			SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "first", Timestamp: tsZero,
			SourceUUID: "uuid-first",
		},
		Message{
			SessionID: "s1", Ordinal: 1, Role: "assistant",
			Content: "answer", Timestamp: tsZero,
			SourceUUID: "uuid-answer",
		},
	)

	msgs, err := d.GetAllMessages(ctx, "s1")
	if err != nil {
		t.Fatalf("GetAllMessages: %v", err)
	}
	note := "important"
	if _, err := d.PinMessage("s1", msgs[1].ID, &note); err != nil {
		t.Fatalf("PinMessage: %v", err)
	}

	// Rewrite: a compact-boundary row is now ordinal 1, pushing
	// "answer" to ordinal 2. The pin should follow uuid-answer
	// to its new ordinal, not stay on ordinal 1 (the boundary).
	if err := d.ReplaceSessionMessages("s1", []Message{
		{
			SessionID: "s1", Ordinal: 0, Role: "user",
			Content: "first", Timestamp: tsZero,
			SourceUUID: "uuid-first",
		},
		{
			SessionID: "s1", Ordinal: 1, Role: "user",
			Content: "[compact]", Timestamp: tsZero,
			SourceUUID:        "uuid-boundary",
			IsCompactBoundary: true,
		},
		{
			SessionID: "s1", Ordinal: 2, Role: "assistant",
			Content: "answer", Timestamp: tsZero,
			SourceUUID: "uuid-answer",
		},
	}); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	pins, err := d.ListPinnedMessages(ctx, "s1", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("want 1 pin, got %d", len(pins))
	}
	if pins[0].Ordinal != 2 {
		t.Errorf(
			"pin ordinal = %d, want 2 (followed source_uuid)",
			pins[0].Ordinal,
		)
	}
	if pins[0].Note == nil || *pins[0].Note != note {
		t.Errorf("pin note = %v, want %q", pins[0].Note, note)
	}
}

// TestReplaceSessionMessagesPinFallsBackToOrdinal verifies that
// when a pin's source_uuid is empty (legacy row from before the
// column existed) the restore falls back to ordinal matching.
func TestReplaceSessionMessagesPinFallsBackToOrdinal(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "p")
	// Messages without source_uuid (legacy).
	insertMessages(t, d,
		userMsg("s1", 0, "msg0"),
		asstMsg("s1", 1, "msg1"),
	)

	msgs, err := d.GetAllMessages(ctx, "s1")
	if err != nil {
		t.Fatalf("GetAllMessages: %v", err)
	}
	if _, err := d.PinMessage("s1", msgs[1].ID, nil); err != nil {
		t.Fatalf("PinMessage: %v", err)
	}

	// Replace with the same ordinals (and still no source_uuid).
	if err := d.ReplaceSessionMessages("s1", []Message{
		userMsg("s1", 0, "msg0-v2"),
		asstMsg("s1", 1, "msg1-v2"),
	}); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	pins, err := d.ListPinnedMessages(ctx, "s1", "")
	if err != nil {
		t.Fatalf("ListPinnedMessages: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("want 1 pin, got %d", len(pins))
	}
	if pins[0].Ordinal != 1 {
		t.Errorf("pin ordinal = %d, want 1", pins[0].Ordinal)
	}
}

func TestGetSessionFilePath(t *testing.T) {
	d := testDB(t)

	fp := "/tmp/sessions/abc.jsonl"
	insertSession(t, d, "zencoder:abc", "p", func(s *Session) {
		s.FilePath = &fp
	})

	got := d.GetSessionFilePath("zencoder:abc")
	if got != fp {
		t.Errorf("GetSessionFilePath = %q, want %q", got, fp)
	}

	// Non-existent session returns empty.
	got = d.GetSessionFilePath("zencoder:nonexistent")
	if got != "" {
		t.Errorf("GetSessionFilePath(missing) = %q, want empty",
			got)
	}
}

func TestLinkSubagentSessionsOverridesContinuation(t *testing.T) {
	d := testDB(t)

	// Parent session with a tool call referencing a child.
	insertSession(t, d, "parent", "p", func(s *Session) {
		s.MessageCount = 1
	})
	// Child session initially classified as continuation
	// (e.g. Zencoder header parentId).
	insertSession(t, d, "child", "p", func(s *Session) {
		s.MessageCount = 1
		parentID := "header-parent"
		s.ParentSessionID = &parentID
		s.RelationshipType = "continuation"
	})

	// Insert a message with a tool call that references the child.
	m := Message{
		SessionID: "parent", Ordinal: 0,
		Role: "assistant", Content: "spawning subagent",
		HasToolUse: true,
		ToolCalls: []ToolCall{{
			ToolName:          "subagent",
			Category:          "Task",
			SubagentSessionID: "child",
		}},
	}
	insertMessages(t, d, m)

	// Link should override continuation -> subagent.
	if err := d.LinkSubagentSessions(); err != nil {
		t.Fatalf("LinkSubagentSessions: %v", err)
	}

	sess, err := d.GetSession(context.Background(), "child")
	requireNoError(t, err, "GetSession")
	if sess.RelationshipType != "subagent" {
		t.Errorf("relationship_type = %q, want 'subagent'",
			sess.RelationshipType)
	}
	if sess.ParentSessionID == nil ||
		*sess.ParentSessionID != "parent" {
		t.Errorf("parent_session_id = %v, want 'parent'",
			sess.ParentSessionID)
	}
}

func TestIsSystemPersisted(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.MessageCount = 2
	})

	m1 := userMsg("s1", 0, "normal user message")
	m2 := userMsg("s1", 1, "system injected notice")
	m2.IsSystem = true

	insertMessages(t, d, m1, m2)

	msgs, err := d.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].IsSystem {
		t.Error("msgs[0].IsSystem = true, want false")
	}
	if !msgs[1].IsSystem {
		t.Error("msgs[1].IsSystem = false, want true")
	}
}

func TestSearchBasic(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.MessageCount = 2
	})

	m1 := userMsg("s1", 0, "Fix the authentication bug")
	m2 := asstMsgAt("s1", 1, "Looking at the auth module",
		tsZeroS1)

	insertMessages(t, d, m1, m2)

	page, err := d.Search(context.Background(), SearchFilter{
		Query: "authentication",
		Limit: 10,
	})
	requireNoError(t, err, "Search")
	if len(page.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(page.Results))
	}
	if page.Results[0].SessionID != "s1" {
		t.Errorf("session_id = %q", page.Results[0].SessionID)
	}
}

func TestSearchExcludesSystemMessages(t *testing.T) {
	d := testDB(t)
	requireFTS(t, d)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.MessageCount = 3
	})

	m1 := userMsg("s1", 0, "unique searchterm here")
	m2 := userMsg("s1", 1, "system unique searchterm notice")
	m2.IsSystem = true
	m3 := asstMsg("s1", 2, "response to user")

	insertMessages(t, d, m1, m2, m3)

	page, err := d.Search(context.Background(), SearchFilter{
		Query: "searchterm",
		Limit: 10,
	})
	requireNoError(t, err, "Search")
	// Only the non-system message should appear
	if len(page.Results) != 1 {
		t.Fatalf("got %d results, want 1 (system msg excluded)",
			len(page.Results))
	}
	if page.Results[0].Ordinal != 0 {
		t.Errorf("ordinal = %d, want 0", page.Results[0].Ordinal)
	}
}

func TestCanceledContext(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.MessageCount = 1
	})
	insertMessages(t, d, userMsg("s1", 0, "searchable content"))

	ctx := canceledCtx()

	tests := []struct {
		name string
		fn   func() error
		skip bool
	}{
		{"Search", func() error {
			_, err := d.Search(ctx, SearchFilter{
				Query: "searchable", Limit: 10,
			})
			return err
		}, !d.HasFTS()},
		{"ListSessions", func() error {
			_, err := d.ListSessions(ctx, SessionFilter{Limit: 10})
			return err
		}, false},
		{"GetMessages", func() error {
			_, err := d.GetMessages(ctx, "s1", 0, 10, true)
			return err
		}, false},
		{"GetStats", func() error {
			_, err := d.GetStats(ctx, false, false)
			return err
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skip {
				t.Skip("no FTS support")
			}
			requireCanceledErr(t, tt.fn())
		})
	}
}

func TestStats(t *testing.T) {
	d := testDB(t)

	// Empty DB returns nil EarliestSession
	stats, err := d.GetStats(context.Background(), false, false)
	requireNoError(t, err, "GetStats empty")
	if stats.EarliestSession != nil {
		t.Errorf(
			"earliest_session = %v, want nil",
			*stats.EarliestSession,
		)
	}

	early := "2024-01-15T09:00:00Z"
	late := "2024-06-01T14:00:00Z"
	insertSession(t, d, "s1", "p1", func(s *Session) {
		s.StartedAt = &late
	})
	insertSession(t, d, "s2", "p2", func(s *Session) {
		s.Machine = "remote"
		s.Agent = "codex"
		s.StartedAt = &early
	})
	insertMessages(t, d,
		userMsg("s1", 0, "hi"),
		userMsg("s2", 0, "bye"),
	)

	stats, err = d.GetStats(context.Background(), false, false)
	requireNoError(t, err, "GetStats")
	if stats.SessionCount != 2 {
		t.Errorf("session_count = %d, want 2", stats.SessionCount)
	}
	if stats.MessageCount != 2 {
		t.Errorf("message_count = %d, want 2", stats.MessageCount)
	}
	if stats.ProjectCount != 2 {
		t.Errorf("project_count = %d, want 2", stats.ProjectCount)
	}
	if stats.MachineCount != 2 {
		t.Errorf("machine_count = %d, want 2", stats.MachineCount)
	}
	if stats.EarliestSession == nil {
		t.Fatal("earliest_session is nil, want non-nil")
	}
	if *stats.EarliestSession != early {
		t.Errorf(
			"earliest_session = %q, want %q",
			*stats.EarliestSession, early,
		)
	}
}

func TestStatsEarliestFallsBackToCreatedAt(t *testing.T) {
	d := testDB(t)

	// Session with NULL started_at — earliest should fall
	// back to created_at instead of being nil.
	insertSession(t, d, "s-null-start", "proj")
	insertMessages(t, d, userMsg("s-null-start", 0, "hi"))

	stats, err := d.GetStats(context.Background(), false, false)
	requireNoError(t, err, "GetStats null started_at")
	if stats.EarliestSession == nil {
		t.Fatal(
			"earliest_session nil when started_at is NULL;" +
				" should fall back to created_at",
		)
	}

	// Session with empty-string started_at — NULLIF should
	// treat it the same as NULL.
	insertSession(t, d, "s-empty-start", "proj", func(s *Session) {
		s.StartedAt = new("")
	})
	insertMessages(t, d, userMsg("s-empty-start", 0, "hey"))

	stats, err = d.GetStats(context.Background(), false, false)
	requireNoError(t, err, "GetStats empty started_at")
	if stats.EarliestSession == nil {
		t.Fatal(
			"earliest_session nil when started_at is '';" +
				" should fall back to created_at",
		)
	}
	if *stats.EarliestSession == "" {
		t.Fatal(
			"earliest_session is empty string;" +
				" NULLIF should have converted '' to NULL",
		)
	}

	// Add a session with an explicit started_at that is
	// older than the auto-generated created_at.
	old := "2020-01-01T00:00:00Z"
	insertSession(t, d, "s-old", "proj", func(s *Session) {
		s.StartedAt = &old
	})
	insertMessages(t, d, userMsg("s-old", 0, "hello"))

	stats, err = d.GetStats(context.Background(), false, false)
	requireNoError(t, err, "GetStats with old session")
	if stats.EarliestSession == nil {
		t.Fatal("earliest_session nil")
	}
	if *stats.EarliestSession != old {
		t.Errorf(
			"earliest_session = %q, want %q",
			*stats.EarliestSession, old,
		)
	}
}

func TestGetProjects(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "alpha")
	insertSession(t, d, "s2", "beta", func(s *Session) {
		s.MessageCount = 2
	})
	insertSession(t, d, "s3", "alpha")

	projects, err := d.GetProjects(context.Background(), false, false)
	requireNoError(t, err, "GetProjects")
	if len(projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(projects))
	}
	if projects[0].Name != "alpha" || projects[0].SessionCount != 2 {
		t.Errorf("alpha: %+v", projects[0])
	}
}

// setupPruneData inserts the standard sessions used by the prune
// candidate filter tests. Each session gets real message rows so
// the user-message subquery in FindPruneCandidates works.
func setupPruneData(t *testing.T, d *DB) {
	t.Helper()
	// s1: 2 user messages
	insertSession(t, d, "s1", "spicytakes", func(s *Session) {
		s.FirstMessage = new("You are a code reviewer")
		s.EndedAt = new("2024-01-15T00:00:00Z")
		s.MessageCount = 2
	})
	b1 := &msgBuilder{id: "s1"}
	b1.user("You are a code reviewer")
	b1.user("Review this")
	insertMessages(t, d, b1.msgs...)
	// s2: 2 user messages
	insertSession(t, d, "s2", "spicytakes", func(s *Session) {
		s.FirstMessage = new("Analyze this blog post")
		s.EndedAt = new("2024-03-01T00:00:00Z")
		s.MessageCount = 2
	})
	b2 := &msgBuilder{id: "s2"}
	b2.user("Analyze this blog post")
	b2.user("More analysis")
	insertMessages(t, d, b2.msgs...)
	// s3: 2 user messages
	insertSession(t, d, "s3", "roborev", func(s *Session) {
		s.FirstMessage = new("You are a code reviewer")
		s.EndedAt = new("2024-03-01T00:00:00Z")
		s.MessageCount = 2
	})
	b3 := &msgBuilder{id: "s3"}
	b3.user("You are a code reviewer")
	b3.user("Check this file")
	insertMessages(t, d, b3.msgs...)
	// s4: 5 user messages + 5 assistant messages = 10 total
	insertSession(t, d, "s4", "spicytakes", func(s *Session) {
		s.FirstMessage = new("Help me refactor")
		s.EndedAt = new("2024-06-01T00:00:00Z")
		s.MessageCount = 10
	})
	b4 := &msgBuilder{id: "s4"}
	b4.user("Help me refactor")
	b4.asst("Sure, here's a plan")
	b4.user("Do step 1")
	b4.asst("Done with step 1")
	b4.user("Do step 2")
	b4.asst("Done with step 2")
	b4.user("Do step 3")
	b4.asst("Done with step 3")
	b4.user("Looks good")
	b4.asst("Thanks")
	insertMessages(t, d, b4.msgs...)
}

func TestFindPruneCandidates(t *testing.T) {
	d := testDB(t)
	setupPruneData(t, d)

	tests := []struct {
		name   string
		filter PruneFilter
		want   []string
	}{
		{
			name:   "ProjectSubstring",
			filter: PruneFilter{Project: "spicy"},
			want:   []string{"s1", "s2", "s4"},
		},
		{
			name:   "MaxMessages",
			filter: PruneFilter{MaxMessages: new(2)},
			want:   []string{"s1", "s2", "s3"},
		},
		{
			name: "BeforeDate",
			filter: PruneFilter{
				Before: "2024-02-01",
			},
			want: []string{"s1"},
		},
		{
			name: "FirstMessagePrefix",
			filter: PruneFilter{
				FirstMessage: "You are a code reviewer",
			},
			want: []string{"s1", "s3"},
		},
		{
			name: "CombinedProjectAndMaxMessages",
			filter: PruneFilter{
				Project: "spicytakes", MaxMessages: new(2),
			},
			want: []string{"s1", "s2"},
		},
		{
			name: "AllFiltersNoMatch",
			filter: PruneFilter{
				Project:      "spicytakes",
				MaxMessages:  new(2),
				Before:       "2024-02-01",
				FirstMessage: "Analyze",
			},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.FindPruneCandidates(tt.filter)
			requireNoError(t, err, "FindPruneCandidates")

			gotIDs := collectIDs(got)
			wantSorted := make([]string, len(tt.want))
			copy(wantSorted, tt.want)
			slices.Sort(wantSorted)

			gotSorted := make([]string, len(gotIDs))
			copy(gotSorted, gotIDs)
			slices.Sort(gotSorted)

			if diff := cmp.Diff(wantSorted, gotSorted); diff != "" {
				t.Errorf("candidates mismatch (-want +got):\n%s", diff)
			}
		})
	}

	// The "before" case also checks the specific ID returned.
	t.Run("BeforeDateReturnsCorrectID", func(t *testing.T) {
		got, err := d.FindPruneCandidates(PruneFilter{
			Before: "2024-02-01",
		})
		requireNoError(t, err, "FindPruneCandidates")
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
		if got[0].ID != "s1" {
			t.Errorf("got ID %q, want s1", got[0].ID)
		}
	})

	// File metadata returned correctly.
	t.Run("ReturnsFileMetadata", func(t *testing.T) {
		fp := "/path/to/file.jsonl"
		insertSession(t, d, "s5", "test", func(s *Session) {
			s.FilePath = new(fp)
			s.FileSize = new(int64(4096))
		})
		got, err := d.FindPruneCandidates(PruneFilter{
			Project: "test",
		})
		requireNoError(t, err, "FindPruneCandidates")
		if len(got) != 1 {
			t.Fatalf("got %d, want 1", len(got))
		}
		if got[0].FilePath == nil || *got[0].FilePath != fp {
			t.Errorf("file_path = %v, want %q", got[0].FilePath, fp)
		}
		if got[0].FileSize == nil || *got[0].FileSize != 4096 {
			t.Errorf("file_size = %v, want 4096", got[0].FileSize)
		}
	})
}

// collectIDs extracts session IDs for error messages.
func collectIDs(sessions []Session) []string {
	ids := make([]string, len(sessions))
	for i, s := range sessions {
		ids[i] = s.ID
	}
	return ids
}

func TestFindPruneCandidatesExcludesParents(t *testing.T) {
	d := testDB(t)

	// Create a parent -> child chain.
	insertSession(t, d, "parent1", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T10:00:00Z")
		s.EndedAt = new("2024-06-01T11:00:00Z")
	})
	insertSession(t, d, "child1", "proj", func(s *Session) {
		s.ParentSessionID = new("parent1")
		s.StartedAt = new("2024-06-01T12:00:00Z")
		s.EndedAt = new("2024-06-01T13:00:00Z")
	})
	// A standalone session with no children.
	insertSession(t, d, "standalone", "proj", func(s *Session) {
		s.StartedAt = new("2024-06-01T14:00:00Z")
		s.EndedAt = new("2024-06-01T15:00:00Z")
	})

	got, err := d.FindPruneCandidates(PruneFilter{
		Project: "proj",
	})
	requireNoError(t, err, "FindPruneCandidates")

	ids := collectIDs(got)

	// Parent should be excluded; child and standalone eligible.
	if len(got) != 2 {
		t.Fatalf("got %d candidates %v, want 2",
			len(got), ids)
	}
	for _, s := range got {
		if s.ID == "parent1" {
			t.Errorf("parent1 should be excluded, "+
				"got candidates: %v", ids)
		}
	}
}

func TestFindPruneCandidatesLikeEscaping(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "e1", "my%project", func(s *Session) {
		s.FirstMessage = new("100% complete")
	})
	insertSession(t, d, "e2", "my_project", func(s *Session) {
		s.FirstMessage = new("100% complete")
	})
	insertSession(t, d, "e3", "myXproject")
	insertSession(t, d, "e4", `my\project`, func(s *Session) {
		s.FirstMessage = new(`path\to\file`)
	})

	tests := []struct {
		name     string
		filter   PruneFilter
		wantN    int
		wantOnly string
	}{
		{
			name: "LiteralPercent",
			filter: PruneFilter{
				Project: "%",
			},
			wantN: 1, wantOnly: "e1",
		},
		{
			name: "LiteralUnderscore",
			filter: PruneFilter{
				Project: "_",
			},
			wantN: 1, wantOnly: "e2",
		},
		{
			name: "PercentInFirstMessage",
			filter: PruneFilter{
				FirstMessage: "100%",
			},
			wantN: 2,
		},
		{
			name: "BackslashInProject",
			filter: PruneFilter{
				Project: `\`,
			},
			wantN: 1, wantOnly: "e4",
		},
		{
			name: "BackslashInFirstMessage",
			filter: PruneFilter{
				FirstMessage: `path\to`,
			},
			wantN: 1, wantOnly: "e4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.FindPruneCandidates(tt.filter)
			requireNoError(t, err, "FindPruneCandidates")
			if len(got) != tt.wantN {
				t.Fatalf("got %v, want %d results",
					collectIDs(got), tt.wantN)
			}
			if tt.wantOnly != "" && got[0].ID != tt.wantOnly {
				t.Errorf("got %v, want [%s]",
					collectIDs(got), tt.wantOnly)
			}
		})
	}
}

func TestFindPruneCandidatesMaxMessagesSentinel(t *testing.T) {
	d := testDB(t)

	// m1: 0 user messages
	insertSession(t, d, "m1", "p", func(s *Session) {
		s.MessageCount = 0
	})
	// m2: 1 user message (default from insertSession)
	insertSession(t, d, "m2", "p")
	insertMessages(t, d, userMsg("m2", 0, "hello"))
	// m3: 3 user messages + 2 assistant = 5 total
	insertSession(t, d, "m3", "p", func(s *Session) {
		s.MessageCount = 5
	})
	insertMessages(t, d,
		userMsg("m3", 0, "msg1"),
		asstMsg("m3", 1, "reply1"),
		userMsg("m3", 2, "msg2"),
		asstMsg("m3", 3, "reply2"),
		userMsg("m3", 4, "msg3"),
	)

	tests := []struct {
		name   string
		filter PruneFilter
		want   int
	}{
		{
			name:   "ZeroMatchesOnlyZero",
			filter: PruneFilter{MaxMessages: new(0)},
			want:   1,
		},
		{
			name: "NilDisablesFilter",
			filter: PruneFilter{
				Project: "p",
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.FindPruneCandidates(tt.filter)
			requireNoError(t, err, "FindPruneCandidates")
			if len(got) != tt.want {
				t.Errorf("got %d, want %d", len(got), tt.want)
			}
		})
	}

	// Additional check: MaxMessages=0 returns m1 specifically.
	got, err := d.FindPruneCandidates(PruneFilter{MaxMessages: new(0)})
	requireNoError(t, err, "FindPruneCandidates MaxMessages=0")
	if len(got) != 1 {
		t.Fatalf("MaxMessages 0: got %d results, want 1", len(got))
	}
	if got[0].ID != "m1" {
		t.Errorf("MaxMessages 0: got %q, want m1", got[0].ID)
	}
}

func TestFindPruneCandidatesIgnoresSystemMessages(t *testing.T) {
	d := testDB(t)

	// Session with 1 real user message and 2 system user
	// messages (Zencoder skill/finish). Only the real one
	// should count toward MaxMessages.
	insertSession(t, d, "zen1", "proj")
	realMsg := userMsg("zen1", 0, "real user msg")
	sysMsg1 := userMsg("zen1", 1, "system init")
	sysMsg1.IsSystem = true
	sysMsg2 := userMsg("zen1", 2, "skill finish")
	sysMsg2.IsSystem = true
	insertMessages(t, d, realMsg, sysMsg1, sysMsg2)

	// MaxMessages=1 should include zen1 (1 real user msg).
	got, err := d.FindPruneCandidates(
		PruneFilter{MaxMessages: new(1)},
	)
	requireNoError(t, err, "FindPruneCandidates")
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].ID != "zen1" {
		t.Errorf("got %q, want zen1", got[0].ID)
	}

	// MaxMessages=0 should NOT include zen1 (it has 1 real
	// user message).
	got, err = d.FindPruneCandidates(
		PruneFilter{MaxMessages: new(0)},
	)
	requireNoError(t, err, "FindPruneCandidates")
	if len(got) != 0 {
		t.Fatalf("expected 0 results, got %d", len(got))
	}
}

func TestDeleteSessions(t *testing.T) {
	d := testDB(t)

	for _, id := range []string{"s1", "s2", "s3"} {
		insertSession(t, d, id, "p")
		insertMessages(t, d, userMsg(id, 0, "msg for "+id))
	}

	stats, _ := d.GetStats(context.Background(), false, false)
	if stats.SessionCount != 3 {
		t.Fatalf("initial sessions = %d, want 3", stats.SessionCount)
	}
	if stats.MessageCount != 3 {
		t.Fatalf("initial messages = %d, want 3", stats.MessageCount)
	}

	deleted, err := d.DeleteSessions([]string{"s1", "s3"})
	requireNoError(t, err, "DeleteSessions")
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}

	requireSessionGone(t, d, "s1")
	requireSessionExists(t, d, "s2")
	requireSessionGone(t, d, "s3")

	msgs, _ := d.GetAllMessages(context.Background(), "s1")
	if len(msgs) != 0 {
		t.Errorf("s1 messages = %d, want 0", len(msgs))
	}
	msgs, _ = d.GetAllMessages(context.Background(), "s2")
	if len(msgs) != 1 {
		t.Errorf("s2 messages = %d, want 1", len(msgs))
	}

	stats, _ = d.GetStats(context.Background(), false, false)
	if stats.SessionCount != 1 {
		t.Errorf("session_count = %d, want 1", stats.SessionCount)
	}
	if stats.MessageCount != 1 {
		t.Errorf("message_count = %d, want 1", stats.MessageCount)
	}

	// Deleted sessions must be excluded.
	if !d.IsSessionExcluded("s1") {
		t.Error("s1 should be excluded after DeleteSessions")
	}
	if !d.IsSessionExcluded("s3") {
		t.Error("s3 should be excluded after DeleteSessions")
	}
	if d.IsSessionExcluded("s2") {
		t.Error("s2 should not be excluded (not deleted)")
	}

	deleted, err = d.DeleteSessions(nil)
	requireNoError(t, err, "DeleteSessions empty")
	if deleted != 0 {
		t.Errorf("deleted empty = %d, want 0", deleted)
	}
}

func TestDeleteSessionNonExistentNoGhostExclusion(t *testing.T) {
	d := testDB(t)

	// Deleting a non-existent ID should not create an exclusion.
	requireNoError(t, d.DeleteSession("bogus"), "DeleteSession bogus")
	if d.IsSessionExcluded("bogus") {
		t.Error("bogus should not be excluded (no row deleted)")
	}
}

func TestDeleteSessionsMixedBatchNoGhostExclusion(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "real", "p")

	deleted, err := d.DeleteSessions([]string{"real", "bogus"})
	requireNoError(t, err, "DeleteSessions mixed")
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if !d.IsSessionExcluded("real") {
		t.Error("real should be excluded after bulk delete")
	}
	if d.IsSessionExcluded("bogus") {
		t.Error("bogus should not be excluded (never existed)")
	}
}

func TestSessionFileInfo(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.FileSize = new(int64(1024))
		s.FileMtime = new(int64(1700000000))
		s.FileHash = new("abc123def456")
	})

	gotSize, gotMtime, ok := d.GetSessionFileInfo("s1")
	if !ok {
		t.Fatal("expected ok")
	}
	if gotSize != 1024 {
		t.Errorf("got size=%d, want 1024", gotSize)
	}
	if gotMtime != 1700000000 {
		t.Errorf("got mtime=%d, want 1700000000", gotMtime)
	}

	_, _, ok = d.GetSessionFileInfo("nonexistent")
	if ok {
		t.Error("expected !ok for nonexistent")
	}
}

func TestGetSessionFull(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	t.Run("AllMetadata", func(t *testing.T) {
		insertSession(t, d, "full-1", "proj", func(s *Session) {
			s.FirstMessage = new("hello")
			s.StartedAt = new(tsZero)
			s.EndedAt = new(tsHour1)
			s.MessageCount = 5
			s.FilePath = new("/tmp/session.jsonl")
			s.FileSize = new(int64(2048))
			s.FileMtime = new(int64(1700000000))
			s.FileHash = new("abc123")
		})

		got, err := d.GetSessionFull(ctx, "full-1")
		requireNoError(t, err, "GetSessionFull")
		if got == nil {
			t.Fatal("expected non-nil session")
			return
		}
		want := &Session{
			ID:                "full-1",
			Project:           "proj",
			MessageCount:      5,
			FilePath:          new("/tmp/session.jsonl"),
			FileSize:          new(int64(2048)),
			FileMtime:         new(int64(1700000000)),
			FileHash:          new("abc123"),
			FirstMessage:      new("hello"),
			StartedAt:         new(tsZero),
			EndedAt:           new(tsHour1),
			Machine:           defaultMachine,
			Agent:             defaultAgent,
			Outcome:           "unknown",
			OutcomeConfidence: "low",
			CreatedAt:         got.CreatedAt,
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("GetSessionFull mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("NullMetadata", func(t *testing.T) {
		insertSession(t, d, "full-2", "proj", func(s *Session) {
			s.MessageCount = 1
		})

		got, err := d.GetSessionFull(ctx, "full-2")
		requireNoError(t, err, "GetSessionFull")
		if got == nil {
			t.Fatal("expected non-nil session")
			return
		}
		want := &Session{
			ID:                "full-2",
			Project:           "proj",
			MessageCount:      1,
			Machine:           defaultMachine,
			Agent:             defaultAgent,
			Outcome:           "unknown",
			OutcomeConfidence: "low",
			CreatedAt:         got.CreatedAt,
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("GetSessionFull mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("NotFound", func(t *testing.T) {
		got, err := d.GetSessionFull(ctx, "nonexistent")
		requireNoError(t, err, "GetSessionFull")
		if got != nil {
			t.Errorf("expected nil session, got %+v", got)
		}
	})
}

func TestCursorEncodeDecode(t *testing.T) {
	d := testDB(t)
	encoded := d.EncodeCursor(tsZero, "session-1")
	cur, err := d.DecodeCursor(encoded)
	requireNoError(t, err, "DecodeCursor")
	if cur.EndedAt != tsZero {
		t.Errorf("EndedAt = %q", cur.EndedAt)
	}
	if cur.ID != "session-1" {
		t.Errorf("ID = %q", cur.ID)
	}

	encodedWithTotal := d.EncodeCursor(
		tsZero,
		"session-1",
		123,
	)
	cur, err = d.DecodeCursor(encodedWithTotal)
	requireNoError(t, err, "DecodeCursor with total")
	if cur.Total != 123 {
		t.Errorf("Total = %d, want 123", cur.Total)
	}
}

func TestCursorTampering(t *testing.T) {
	d := testDB(t)
	// 1. Create a valid signed cursor
	original := d.EncodeCursor(tsZero, "s1", 100)

	parts := strings.Split(original, ".")
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts (payload.sig), got %d", len(parts))
	}

	payload := parts[0]
	sig := parts[1]

	// 2. Decode payload, modify Total, re-encode
	data, err := base64.RawURLEncoding.DecodeString(payload)
	requireNoError(t, err, "DecodeString payload")
	var c SessionCursor
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("Unmarshal payload: %v", err)
	}
	c.Total = 999
	tamperedData, err := json.Marshal(c)
	requireNoError(t, err, "Marshal tampered")
	tamperedPayload := base64.RawURLEncoding.EncodeToString(tamperedData)

	// 3. Construct tampered cursor with original signature
	tamperedCursor := tamperedPayload + "." + sig

	// 4. Decode should fail signature check
	_, err = d.DecodeCursor(tamperedCursor)
	if err == nil {
		t.Fatal("expected error for tampered cursor, got nil")
	}
	if !strings.Contains(err.Error(), "signature mismatch") {
		t.Errorf("expected signature mismatch error, got: %v", err)
	}
}

func TestLegacyCursor(t *testing.T) {
	d := testDB(t)
	// Create a legacy cursor (base64 json only, no signature)
	c := SessionCursor{
		EndedAt: tsZero,
		ID:      "s1",
		Total:   100, // Should be ignored
	}
	data, err := json.Marshal(c)
	requireNoError(t, err, "Marshal legacy")
	legacy := base64.RawURLEncoding.EncodeToString(data)

	// Decode
	got, err := d.DecodeCursor(legacy)
	requireNoError(t, err, "DecodeCursor legacy")

	// Verify ID/EndedAt are preserved
	if got.ID != "s1" {
		t.Errorf("ID = %q, want s1", got.ID)
	}
	// Verify Total is ZEROED out
	if got.Total != 0 {
		t.Errorf("Total = %d, want 0 (untrusted legacy)", got.Total)
	}
}

func TestCursorSecretConcurrency(t *testing.T) {
	d := testDB(t)

	const goroutines = 8
	const iterations = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			for j := range iterations {
				switch id % 3 {
				case 0:
					secret := fmt.Appendf(
						nil, "secret-%d-%d", id, j,
					)
					d.SetCursorSecret(secret)
				case 1:
					d.EncodeCursor(
						tsZero,
						fmt.Sprintf("s-%d-%d", id, j),
						42,
					)
				case 2:
					encoded := d.EncodeCursor(
						tsZero, "s1",
					)
					// Decode may fail if secret rotated
					// between encode and decode; that's OK.
					_, err := d.DecodeCursor(encoded)
					if err != nil &&
						!errors.Is(err, ErrInvalidCursor) {
						t.Errorf(
							"unexpected DecodeCursor error: %v",
							err,
						)
					}
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestSetCursorSecretDefensiveCopy(t *testing.T) {
	d := testDB(t)

	secret := []byte("my-secret-key-for-testing-copy!!")
	d.SetCursorSecret(secret)

	encoded := d.EncodeCursor(tsZero, "s1")

	// Mutate the original slice — should not affect the DB.
	for i := range secret {
		secret[i] = 0
	}

	_, err := d.DecodeCursor(encoded)
	if err != nil {
		t.Fatalf(
			"DecodeCursor failed after caller mutated secret: %v",
			err,
		)
	}
}

func TestDeleteSession(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")
	insertMessages(t, d, userMsg("s1", 0, "test"))

	if err := d.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	requireSessionGone(t, d, "s1")

	msgs, _ := d.GetAllMessages(context.Background(), "s1")
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages after cascade, got %d",
			len(msgs))
	}
}

func TestMigrationRace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "race.db")

	// 1. Create a current schema so concurrent Opens exercise
	// the normal init path (old schemas are now dropped and
	// rebuilt, making concurrent migration less interesting).
	db1, err := Open(path)
	requireNoError(t, err, "setup")
	db1.Close()

	// 2. Run concurrent Open.
	errCh := make(chan error, 2)
	var (
		mu         sync.Mutex
		cond       = sync.NewCond(&mu)
		readyCount = 0
		start      = false
	)

	for range 2 {
		go func() {
			mu.Lock()
			readyCount++
			if readyCount == 2 {
				cond.Broadcast()
			}
			for !start {
				cond.Wait()
			}
			mu.Unlock()

			db, err := Open(path)
			if err != nil {
				errCh <- err
				return
			}
			db.Close()
			errCh <- nil
		}()
	}

	mu.Lock()
	for readyCount < 2 {
		cond.Wait()
	}
	start = true
	cond.Broadcast()
	mu.Unlock()

	var successes int
	for range 2 {
		if err := <-errCh; err != nil {
			msg := err.Error()
			if strings.Contains(msg, "database is locked") ||
				strings.Contains(msg, "database schema is locked") ||
				strings.Contains(msg, "SQLITE_BUSY") ||
				strings.Contains(msg, "SQLITE_LOCKED") {
				t.Logf("concurrent Open lock contention: %v", err)
			} else {
				t.Errorf("unexpected concurrent Open error: %v", err)
			}
		} else {
			successes++
		}
	}
	if successes == 0 {
		t.Fatal("both concurrent Opens failed")
	}

	// 3. Verify schema is intact
	dbCheck, err := Open(path)
	requireNoError(t, err, "re-open")
	defer dbCheck.Close()

	_, err = dbCheck.getWriter().Exec(
		"SELECT parent_session_id FROM sessions LIMIT 1",
	)
	if err != nil {
		t.Errorf("parent_session_id column missing: %v", err)
	}
}

func TestToolCallsInsertedWithMessages(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.MessageCount = 2
	})

	m1 := userMsg("s1", 0, "hello")
	m2 := asstMsg("s1", 1, "[Read: main.go]")
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "s1", ToolName: "Read", Category: "Read"},
		{SessionID: "s1", ToolName: "Grep", Category: "Grep"},
	}

	insertMessages(t, d, m1, m2)

	// Query tool_calls directly
	rows, err := d.Reader().Query(
		`SELECT message_id, session_id, tool_name, category
		 FROM tool_calls WHERE session_id = ?
		 ORDER BY id`, "s1")
	requireNoError(t, err, "query tool_calls")
	defer rows.Close()

	var calls []ToolCall
	for rows.Next() {
		var tc ToolCall
		if err := rows.Scan(
			&tc.MessageID, &tc.SessionID,
			&tc.ToolName, &tc.Category,
		); err != nil {
			t.Fatalf("scan tool_call: %v", err)
		}
		calls = append(calls, tc)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(calls) != 2 {
		t.Fatalf("got %d tool_calls, want 2", len(calls))
	}
	if calls[0].ToolName != "Read" || calls[0].Category != "Read" {
		t.Errorf("calls[0] = %+v", calls[0])
	}
	if calls[1].ToolName != "Grep" || calls[1].Category != "Grep" {
		t.Errorf("calls[1] = %+v", calls[1])
	}
	if calls[0].MessageID == 0 {
		t.Error("message_id should be non-zero")
	}
	if calls[0].SessionID != "s1" {
		t.Errorf("session_id = %q, want s1", calls[0].SessionID)
	}
}

func TestToolCallsCascadeOnSessionDelete(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")

	m := asstMsg("s1", 0, "[Bash]")
	m.HasToolUse = true
	m.ToolCalls = []ToolCall{
		{SessionID: "s1", ToolName: "Bash", Category: "Bash"},
	}
	insertMessages(t, d, m)

	if err := d.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	var count int
	if err := d.Reader().QueryRow(
		"SELECT COUNT(*) FROM tool_calls WHERE session_id = ?",
		"s1",
	).Scan(&count); err != nil {
		t.Fatalf("count tool_calls: %v", err)
	}
	if count != 0 {
		t.Errorf("tool_calls count = %d, want 0", count)
	}
}

func TestReplaceSessionMessagesReplacesToolCalls(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")

	m := asstMsg("s1", 0, "[Read: a.go]")
	m.HasToolUse = true
	m.ToolCalls = []ToolCall{
		{SessionID: "s1", ToolName: "Read", Category: "Read"},
	}
	insertMessages(t, d, m)

	// Replace with different tool calls
	m2 := asstMsg("s1", 0, "[Bash]")
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "s1", ToolName: "Bash", Category: "Bash"},
		{SessionID: "s1", ToolName: "Write", Category: "Write"},
	}
	if err := d.ReplaceSessionMessages("s1", []Message{m2}); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	var names []string
	rows, err := d.Reader().Query(
		`SELECT tool_name FROM tool_calls
		 WHERE session_id = ? ORDER BY id`, "s1")
	requireNoError(t, err, "query")
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(names) != 2 {
		t.Fatalf("got %d tool_calls, want 2", len(names))
	}
	if names[0] != "Bash" {
		t.Errorf("names[0] = %q, want Bash", names[0])
	}
	if names[1] != "Write" {
		t.Errorf("names[1] = %q, want Write", names[1])
	}
}

func TestReplaceSessionMessagesReplacesToolResultEvents(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")

	m1 := asstMsg("s1", 0, "[Wait]")
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{{
		SessionID:           "s1",
		ToolName:            "wait",
		Category:            "Other",
		ToolUseID:           "call_wait",
		ResultContent:       "old result",
		ResultContentLength: len("old result"),
		ResultEvents: []ToolResultEvent{{
			ToolUseID:     "call_wait",
			AgentID:       "agent-1",
			Source:        "wait_output",
			Status:        "completed",
			Content:       "old result",
			ContentLength: len("old result"),
			EventIndex:    0,
		}},
	}}
	insertMessages(t, d, m1)

	m2 := asstMsg("s1", 0, "[Wait]")
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{{
		SessionID:           "s1",
		ToolName:            "wait",
		Category:            "Other",
		ToolUseID:           "call_wait",
		ResultContent:       "new result",
		ResultContentLength: len("new result"),
		ResultEvents: []ToolResultEvent{{
			ToolUseID:     "call_wait",
			AgentID:       "agent-1",
			Source:        "wait_output",
			Status:        "completed",
			Content:       "new result",
			ContentLength: len("new result"),
			EventIndex:    0,
		}},
	}}
	if err := d.ReplaceSessionMessages("s1", []Message{m2}); err != nil {
		t.Fatalf("ReplaceSessionMessages: %v", err)
	}

	msgs, err := d.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(msgs[0].ToolCalls))
	}
	tc := msgs[0].ToolCalls[0]
	if tc.ResultContent != "new result" {
		t.Fatalf("result_content = %q, want %q", tc.ResultContent, "new result")
	}
	if len(tc.ResultEvents) != 1 {
		t.Fatalf("result events len = %d, want 1", len(tc.ResultEvents))
	}
	if tc.ResultEvents[0].Content != "new result" {
		t.Fatalf("event content = %q, want %q", tc.ResultEvents[0].Content, "new result")
	}

	var count int
	err = d.Reader().QueryRow(
		"SELECT COUNT(*) FROM tool_result_events WHERE session_id = ?",
		"s1",
	).Scan(&count)
	requireNoError(t, err, "count tool_result_events")
	if count != 1 {
		t.Fatalf("tool_result_events count = %d, want 1", count)
	}
}

func TestToolCallsNoToolCalls(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")
	insertMessages(t, d, userMsg("s1", 0, "hello"))

	var count int
	if err := d.Reader().QueryRow(
		"SELECT COUNT(*) FROM tool_calls WHERE session_id = ?",
		"s1",
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("tool_calls count = %d, want 0", count)
	}
}

func TestToolCallsMixedSessionsOverlappingOrdinals(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")
	insertSession(t, d, "s2", "p")

	// Both sessions have ordinal 0 with tool calls
	m1 := asstMsg("s1", 0, "[Read]")
	m1.HasToolUse = true
	m1.ToolCalls = []ToolCall{
		{SessionID: "s1", ToolName: "Read", Category: "Read"},
	}
	m2 := asstMsg("s2", 0, "[Bash]")
	m2.HasToolUse = true
	m2.ToolCalls = []ToolCall{
		{SessionID: "s2", ToolName: "Bash", Category: "Bash"},
	}

	insertMessages(t, d, m1, m2)

	// Verify each tool_call.message_id joins to the correct
	// session: Read→s1, Bash→s2.
	rows, err := d.Reader().Query(`
		SELECT tc.tool_name, tc.session_id, m.session_id
		FROM tool_calls tc
		JOIN messages m ON m.id = tc.message_id
		ORDER BY tc.tool_name`)
	requireNoError(t, err, "query")
	defer rows.Close()

	type row struct {
		toolName, tcSession, msgSession string
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(
			&r.toolName, &r.tcSession, &r.msgSession,
		); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d tool_calls, want 2", len(got))
	}
	// Bash should be linked to s2
	if got[0].toolName != "Bash" ||
		got[0].tcSession != "s2" ||
		got[0].msgSession != "s2" {
		t.Errorf("Bash row = %+v", got[0])
	}
	// Read should be linked to s1
	if got[1].toolName != "Read" ||
		got[1].tcSession != "s1" ||
		got[1].msgSession != "s1" {
		t.Errorf("Read row = %+v", got[1])
	}
}

func TestResolveToolCallsPanicsOnLengthMismatch(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic, got none")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "resolveToolCalls") {
			t.Errorf("unexpected panic value: %v", r)
		}
	}()

	msgs := []Message{
		{SessionID: "s1", Ordinal: 0, Role: "user"},
		{SessionID: "s1", Ordinal: 1, Role: "assistant"},
	}
	ids := []int64{1} // length mismatch
	resolveToolCalls(msgs, ids)
}

func TestToolCallNewColumns(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "assistant",
		Content:       "[Read: main.go]",
		ContentLength: 15,
		Timestamp:     tsZero,
		ToolCalls: []ToolCall{{
			SessionID:           "s1",
			ToolName:            "Read",
			Category:            "Read",
			ToolUseID:           "toolu_abc",
			InputJSON:           `{"file_path":"main.go"}`,
			ResultContentLength: 500,
		}},
	})

	var toolUseID, inputJSON sql.NullString
	var resultLen sql.NullInt64
	err := d.Reader().QueryRow(`
        SELECT tool_use_id, input_json, result_content_length
        FROM tool_calls WHERE session_id = 's1'
    `).Scan(&toolUseID, &inputJSON, &resultLen)
	requireNoError(t, err, "query tool_calls")
	if !toolUseID.Valid || toolUseID.String != "toolu_abc" {
		t.Errorf("tool_use_id = %v, want toolu_abc", toolUseID)
	}
	if !inputJSON.Valid || inputJSON.String != `{"file_path":"main.go"}` {
		t.Errorf("input_json = %v", inputJSON)
	}
	if !resultLen.Valid || resultLen.Int64 != 500 {
		t.Errorf("result_content_length = %v, want 500", resultLen)
	}
}

func TestToolCallSkillName(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "assistant",
		Content:       "[Skill: superpowers:brainstorming]",
		ContentLength: 34,
		Timestamp:     tsZero,
		ToolCalls: []ToolCall{{
			SessionID: "s1",
			ToolName:  "Skill",
			Category:  "Tool",
			ToolUseID: "toolu_skill1",
			InputJSON: `{"skill":"superpowers:brainstorming"}`,
			SkillName: "superpowers:brainstorming",
		}},
	})

	var skillName sql.NullString
	err := d.Reader().QueryRow(`
        SELECT skill_name FROM tool_calls WHERE session_id = 's1'
    `).Scan(&skillName)
	requireNoError(t, err, "query")
	if !skillName.Valid || skillName.String != "superpowers:brainstorming" {
		t.Errorf("skill_name = %v, want superpowers:brainstorming", skillName)
	}
}

func TestGetMessagesReturnsToolCalls(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "assistant",
		Content:       "[Skill: superpowers:brainstorming]",
		ContentLength: 34,
		Timestamp:     tsZero,
		HasToolUse:    true,
		ToolCalls: []ToolCall{{
			SessionID:           "s1",
			ToolName:            "Skill",
			Category:            "Tool",
			ToolUseID:           "toolu_s1",
			InputJSON:           `{"skill":"superpowers:brainstorming"}`,
			SkillName:           "superpowers:brainstorming",
			ResultContentLength: 42,
		}},
	})

	msgs, err := d.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "GetMessages")
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("got %d tool_calls, want 1",
			len(msgs[0].ToolCalls))
	}
	tc := msgs[0].ToolCalls[0]
	if tc.ToolName != "Skill" {
		t.Errorf("ToolName = %q", tc.ToolName)
	}
	if tc.SkillName != "superpowers:brainstorming" {
		t.Errorf("SkillName = %q", tc.SkillName)
	}
	if tc.InputJSON != `{"skill":"superpowers:brainstorming"}` {
		t.Errorf("InputJSON = %q", tc.InputJSON)
	}
	if tc.ResultContentLength != 42 {
		t.Errorf("ResultContentLength = %d", tc.ResultContentLength)
	}
}

func TestToolCallResultContent(t *testing.T) {
	database := testDB(t)
	sess := Session{
		ID: "sess-rc", Project: "p", Machine: "m", Agent: "claude",
	}
	if err := database.UpsertSession(sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	msgs := []Message{
		{
			SessionID: "sess-rc",
			Ordinal:   0,
			Role:      "assistant",
			Content:   "ok",
			ToolCalls: []ToolCall{
				{
					SessionID:     "sess-rc",
					ToolName:      "Bash",
					Category:      "Bash",
					ToolUseID:     "tu-rc",
					ResultContent: "[main abc1234] Add feature\n 1 file changed",
				},
			},
		},
	}
	if err := database.InsertMessages(msgs); err != nil {
		t.Fatalf("insert: %v", err)
	}
	retrieved, err := database.GetMessages(
		context.Background(), "sess-rc", 0, 10, true,
	)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(retrieved) != 1 || len(retrieved[0].ToolCalls) != 1 {
		t.Fatalf("expected 1 msg with 1 tool call")
	}
	tc := retrieved[0].ToolCalls[0]
	if tc.ResultContent != "[main abc1234] Add feature\n 1 file changed" {
		t.Errorf("ResultContent = %q", tc.ResultContent)
	}
}

func TestGetAllMessagesReturnsToolCallsAcrossBatches(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	total := attachToolCallBatchSize + 25
	msgs := make([]Message, 0, total)
	for i := range total {
		content := fmt.Sprintf("[Read: file-%d.txt]", i)
		msgs = append(msgs, Message{
			SessionID:     "s1",
			Ordinal:       i,
			Role:          "assistant",
			Content:       content,
			ContentLength: len(content),
			Timestamp:     tsZero,
			HasToolUse:    true,
			ToolCalls: []ToolCall{{
				SessionID: "s1",
				ToolName:  "Read",
				Category:  "Read",
				ToolUseID: fmt.Sprintf("toolu_%d", i),
			}},
		})
	}
	insertMessages(t, d, msgs...)

	got, err := d.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	if len(got) != total {
		t.Fatalf("got %d messages, want %d", len(got), total)
	}

	for i := range total {
		if len(got[i].ToolCalls) != 1 {
			t.Fatalf("msg %d: got %d tool_calls, want 1",
				i, len(got[i].ToolCalls))
		}
		if got[i].ToolCalls[0].ToolUseID != fmt.Sprintf("toolu_%d", i) {
			t.Fatalf("msg %d: tool_use_id = %q, want %q",
				i, got[i].ToolCalls[0].ToolUseID,
				fmt.Sprintf("toolu_%d", i))
		}
	}
}

func TestToolCallSubagentSessionID(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s1",
		Ordinal:       0,
		Role:          "assistant",
		Content:       "[Task: implement feature]",
		ContentLength: 24,
		Timestamp:     tsZero,
		HasToolUse:    true,
		ToolCalls: []ToolCall{{
			SessionID:         "s1",
			ToolName:          "Task",
			Category:          "Tool",
			ToolUseID:         "toolu_task1",
			SubagentSessionID: "agent-abc123",
		}},
	})

	// Verify via raw SQL that the column is stored
	var subagentID sql.NullString
	err := d.Reader().QueryRow(`
		SELECT subagent_session_id
		FROM tool_calls WHERE session_id = 's1'
	`).Scan(&subagentID)
	requireNoError(t, err, "query tool_calls")
	if !subagentID.Valid || subagentID.String != "agent-abc123" {
		t.Errorf("subagent_session_id = %v, want agent-abc123",
			subagentID)
	}

	// Verify via GetMessages that it round-trips
	msgs, err := d.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "GetMessages")
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if len(msgs[0].ToolCalls) != 1 {
		t.Fatalf("got %d tool_calls, want 1",
			len(msgs[0].ToolCalls))
	}
	tc := msgs[0].ToolCalls[0]
	if tc.SubagentSessionID != "agent-abc123" {
		t.Errorf("SubagentSessionID = %q, want %q",
			tc.SubagentSessionID, "agent-abc123")
	}

	// Verify empty SubagentSessionID stores as NULL
	insertSession(t, d, "s2", "proj")
	insertMessages(t, d, Message{
		SessionID:     "s2",
		Ordinal:       0,
		Role:          "assistant",
		Content:       "[Read: main.go]",
		ContentLength: 15,
		Timestamp:     tsZero,
		HasToolUse:    true,
		ToolCalls: []ToolCall{{
			SessionID: "s2",
			ToolName:  "Read",
			Category:  "Read",
			ToolUseID: "toolu_read1",
		}},
	})

	var nullSubagent sql.NullString
	err = d.Reader().QueryRow(`
		SELECT subagent_session_id
		FROM tool_calls WHERE session_id = 's2'
	`).Scan(&nullSubagent)
	requireNoError(t, err, "query tool_calls s2")
	if nullSubagent.Valid {
		t.Errorf("expected NULL subagent_session_id for s2, got %q",
			nullSubagent.String)
	}
}

func TestFTSBackfill(t *testing.T) {
	dCheck := testDB(t)
	requireFTS(t, dCheck)
	dCheck.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "backfill.db")

	// 1. Create DB and drop FTS to simulate "old" DB or broken state
	d1, err := Open(path)
	requireNoError(t, err, "Open 1")
	// Use writer directly to ensure it happens
	w := d1.getWriter()
	if _, err := w.Exec("DROP TABLE IF EXISTS messages_fts"); err != nil {
		t.Fatalf("dropping fts: %v", err)
	}
	// Also drop triggers, otherwise inserts will fail
	for _, tr := range []string{"messages_ai", "messages_ad", "messages_au"} {
		if _, err := w.Exec("DROP TRIGGER IF EXISTS " + tr); err != nil {
			t.Fatalf("dropping trigger %s: %v", tr, err)
		}
	}

	// 2. Insert messages while FTS is missing
	insertSession(t, d1, "s1", "proj")
	insertMessages(t, d1, userMsg("s1", 0, "unique_keyword"))

	if err := d1.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}

	// 3. Re-open. This should detect missing FTS, create it, and backfill.
	d2, err := Open(path)
	requireNoError(t, err, "Open 2")
	defer d2.Close()

	if !d2.HasFTS() {
		t.Fatal("FTS should be available after re-open")
	}

	// 4. Verify search finds the message
	page, err := d2.Search(context.Background(), SearchFilter{
		Query: "unique_keyword",
		Limit: 1,
	})
	requireNoError(t, err, "Search")
	if len(page.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(page.Results))
	}
	if page.Results[0].SessionID != "s1" {
		t.Errorf("result session_id = %q, want s1", page.Results[0].SessionID)
	}
}

func TestPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := Open(path)
	requireNoError(t, err, "Open")
	defer d.Close()

	if got := d.Path(); got != path {
		t.Errorf("Path() = %q, want %q", got, path)
	}
}

func TestReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := Open(path)
	requireNoError(t, err, "Open")
	defer d.Close()

	// Insert data before reopen.
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, userMsg("s1", 0, "hello"))

	if err := d.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// Data should still be accessible after reopen.
	got := requireSessionExists(t, d, "s1")
	if got.Project != "proj" {
		t.Errorf("project = %q, want proj", got.Project)
	}

	msgs, err := d.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	if len(msgs) != 1 || msgs[0].Content != "hello" {
		t.Errorf("messages = %v, want [hello]", msgs)
	}

	// Writes should work after reopen.
	insertSession(t, d, "s2", "proj2")
	requireSessionExists(t, d, "s2")
}

func TestReopenAfterSwap(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, "orig.db")
	tempPath := filepath.Join(dir, "temp.db")

	// Create original DB with data.
	origDB, err := Open(origPath)
	requireNoError(t, err, "Open orig")
	defer origDB.Close()
	insertSession(t, origDB, "old-session", "old-proj")

	// Create temp DB with different data.
	tempDB, err := Open(tempPath)
	requireNoError(t, err, "Open temp")
	insertSession(t, tempDB, "new-session", "new-proj")
	tempDB.Close()

	// Close connections before rename (Windows-safe flow).
	if err := origDB.CloseConnections(); err != nil {
		t.Fatalf("CloseConnections: %v", err)
	}

	// Remove WAL/SHM while connections are closed.
	os.Remove(origPath + "-wal")
	os.Remove(origPath + "-shm")

	// Swap: rename temp over original.
	if err := os.Rename(tempPath, origPath); err != nil {
		t.Fatalf("rename: %v", err)
	}
	os.Remove(tempPath + "-wal")
	os.Remove(tempPath + "-shm")

	// Reopen to pick up the new file.
	if err := origDB.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// Original DB handle should now see the new data.
	requireSessionGone(t, origDB, "old-session")
	got := requireSessionExists(t, origDB, "new-session")
	if got.Project != "new-proj" {
		t.Errorf("project = %q, want new-proj", got.Project)
	}
}

func TestCloseConnections(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// Close connections.
	if err := d.CloseConnections(); err != nil {
		t.Fatalf("CloseConnections: %v", err)
	}

	// Queries should fail after close.
	_, err := d.GetSession(context.Background(), "s1")
	if err == nil {
		t.Error("expected error querying after CloseConnections")
	}

	// Reopen should restore service.
	if err := d.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// Queries should work again.
	s, err := d.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetSession after Reopen: %v", err)
	}
	if s == nil {
		t.Error("session s1 missing after Reopen")
	}
}

func TestCloseRenameReopen(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, "orig.db")
	tempPath := filepath.Join(dir, "temp.db")

	// Create original with old data.
	origDB, err := Open(origPath)
	requireNoError(t, err, "Open orig")
	defer origDB.Close()
	insertSession(t, origDB, "old", "old-proj")

	// Create replacement with new data.
	tempDB, err := Open(tempPath)
	requireNoError(t, err, "Open temp")
	insertSession(t, tempDB, "new", "new-proj")
	tempDB.Close()

	// Simulate the ResyncAll sequence:
	// close -> removeWAL -> rename -> reopen
	if err := origDB.CloseConnections(); err != nil {
		t.Fatalf("CloseConnections: %v", err)
	}
	for _, p := range []string{origPath, tempPath} {
		os.Remove(p + "-wal")
		os.Remove(p + "-shm")
	}
	if err := os.Rename(tempPath, origPath); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := origDB.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// Verify swap succeeded.
	requireSessionGone(t, origDB, "old")
	got := requireSessionExists(t, origDB, "new")
	if got.Project != "new-proj" {
		t.Errorf("project = %q, want new-proj", got.Project)
	}
}

func TestCloseRecoveryOnRenameFail(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, "orig.db")

	origDB, err := Open(origPath)
	requireNoError(t, err, "Open orig")
	defer origDB.Close()
	insertSession(t, origDB, "s1", "proj")

	// Close connections as ResyncAll would.
	if err := origDB.CloseConnections(); err != nil {
		t.Fatalf("CloseConnections: %v", err)
	}

	// Simulate rename failure (temp file doesn't exist).
	nonexistent := filepath.Join(dir, "no-such-file.db")
	renameErr := os.Rename(nonexistent, origPath)
	if renameErr == nil {
		t.Fatal("expected rename to fail")
	}

	// Recovery: reopen original to restore service.
	if err := origDB.Reopen(); err != nil {
		t.Fatalf("recovery Reopen: %v", err)
	}

	// Data should still be accessible.
	s, err := origDB.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetSession after recovery: %v", err)
	}
	if s == nil {
		t.Error("session s1 missing after recovery Reopen")
	}
}

func TestConcurrentReadsWhileReopen(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// Spin up readers that continuously query.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var readErrors atomic.Int64

	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				_, err := d.GetSession(ctx, "s1")
				if err != nil && ctx.Err() == nil {
					readErrors.Add(1)
					return
				}
			}
		})
	}

	// Reopen while readers are active.
	for range 5 {
		if err := d.Reopen(); err != nil {
			t.Fatalf("Reopen: %v", err)
		}
	}

	cancel()
	wg.Wait()

	if n := readErrors.Load(); n > 0 {
		t.Errorf("got %d concurrent read errors", n)
	}
}

func TestRepeatedReopenBoundsRetiredPools(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// Reopen many times; retired pools from earlier rounds
	// should be closed by subsequent reopens, keeping only
	// the most recent pair alive.
	for range 20 {
		if err := d.Reopen(); err != nil {
			t.Fatalf("Reopen: %v", err)
		}
	}

	// After 20 reopens the retired slice should hold at most
	// the last pair (2 entries), not 40.
	d.mu.Lock()
	n := len(d.retired)
	d.mu.Unlock()
	if n > 2 {
		t.Errorf("retired pool count = %d, want <= 2", n)
	}

	// Data should still be readable.
	s, err := d.GetSession(context.Background(), "s1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if s == nil {
		t.Error("session s1 missing after repeated Reopen")
	}
}

func TestCloseAfterCloseConnectionsReopen(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// CloseConnections + Reopen is the normal resync lifecycle.
	if err := d.CloseConnections(); err != nil {
		t.Fatalf("CloseConnections: %v", err)
	}
	if err := d.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// Close should succeed without "database is closed" errors
	// from double-closing the pools that CloseConnections
	// already closed.
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCopyInsightsFrom(t *testing.T) {
	dir := t.TempDir()

	// Source DB with insights.
	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	_, err = srcDB.InsertInsight(Insight{
		Type:     "daily_activity",
		DateFrom: "2025-01-15",
		DateTo:   "2025-01-15",
		Agent:    "claude",
		Content:  "test insight content",
	})
	requireNoError(t, err, "InsertInsight")
	srcDB.Close()

	// Destination DB (empty).
	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	// Copy insights from source.
	if err := dstDB.CopyInsightsFrom(srcPath); err != nil {
		t.Fatalf("CopyInsightsFrom: %v", err)
	}

	// Verify insights were copied.
	insights, err := dstDB.ListInsights(
		context.Background(), InsightFilter{},
	)
	requireNoError(t, err, "ListInsights")
	if len(insights) != 1 {
		t.Fatalf("got %d insights, want 1", len(insights))
	}
	if insights[0].Content != "test insight content" {
		t.Errorf(
			"content = %q, want %q",
			insights[0].Content, "test insight content",
		)
	}
}

func TestCopyOrphanedDataFrom(t *testing.T) {
	dir := t.TempDir()

	// Source (old) DB with two sessions: s1 and s2.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
	})
	insertSession(t, srcDB, "s2", "proj", func(s *Session) {
		s.Agent = "codex"
	})
	insertMessages(t, srcDB,
		userMsg("s1", 0, "hello from s1"),
		asstMsg("s1", 1, "reply from s1"),
		userMsg("s2", 0, "hello from s2"),
	)
	// Insert tool_calls for s1 via raw SQL since
	// insertToolCallsTx is unexported.
	_, err = srcDB.getWriter().Exec(`
		INSERT INTO tool_calls
			(message_id, session_id, tool_name, category)
		SELECT id, session_id, 'Read', 'file'
		FROM messages
		WHERE session_id = 's1' AND ordinal = 1`,
	)
	requireNoError(t, err, "insert tool_call")
	srcDB.Close()

	// Destination (new) DB: only has s1 (re-synced from
	// file). s2 is orphaned (file gone).
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()
	insertSession(t, dstDB, "s1", "proj", func(s *Session) {
		s.Agent = "claude"
	})
	insertMessages(t, dstDB,
		userMsg("s1", 0, "hello from s1"),
		asstMsg("s1", 1, "reply from s1"),
	)

	// Copy orphaned data from source.
	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 1 {
		t.Fatalf("expected 1 orphaned session, got %d", count)
	}

	// s2 should now exist in dst.
	s, err := dstDB.GetSession(
		context.Background(), "s2",
	)
	requireNoError(t, err, "GetSession s2")
	if s == nil {
		t.Fatal("orphaned session s2 not found in dst")
		return
	}
	if s.Agent != "codex" {
		t.Errorf("s2 agent = %q, want %q", s.Agent, "codex")
	}

	// s2 messages should be copied.
	ctx := context.Background()
	msgs, err := dstDB.GetMessages(ctx, "s2", 0, 100, true)
	requireNoError(t, err, "GetMessages s2")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message for s2, got %d",
			len(msgs))
	}
	if msgs[0].Content != "hello from s2" {
		t.Errorf("s2 message content = %q, want %q",
			msgs[0].Content, "hello from s2")
	}

	// s1 should still exist and not be duplicated.
	s1msgs, err := dstDB.GetMessages(ctx, "s1", 0, 100, true)
	requireNoError(t, err, "GetMessages s1")
	if len(s1msgs) != 2 {
		t.Fatalf("expected 2 messages for s1, got %d",
			len(s1msgs))
	}

	// Tool calls for s1 should NOT be copied (s1 exists in
	// dst, so it's not orphaned). Only verify s2's tool_calls
	// aren't present (s2 had no tool_calls on ordinal 0).
	var tcCount int
	err = dstDB.getReader().QueryRow(
		"SELECT count(*) FROM tool_calls " +
			"WHERE session_id = 's2'",
	).Scan(&tcCount)
	requireNoError(t, err, "count s2 tool_calls")
	if tcCount != 0 {
		t.Errorf("expected 0 tool_calls for s2, got %d",
			tcCount)
	}
}

func TestCopyOrphanedDataFrom_NoOrphans(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	srcDB.Close()

	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()
	insertSession(t, dstDB, "s1", "proj")

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 0 {
		t.Fatalf("expected 0 orphaned sessions, got %d",
			count)
	}
}

func TestCopyOrphanedDataFrom_WithToolCalls(t *testing.T) {
	dir := t.TempDir()

	// Source DB with session s1 that has tool_calls.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	insertMessages(t, srcDB,
		userMsg("s1", 0, "hello"),
		asstMsg("s1", 1, "used a tool"),
	)
	_, err = srcDB.getWriter().Exec(`
		INSERT INTO tool_calls
			(message_id, session_id, tool_name, category,
			 tool_use_id)
		SELECT id, session_id, 'Bash', 'command',
			'tu_123'
		FROM messages
		WHERE session_id = 's1' AND ordinal = 1`,
	)
	requireNoError(t, err, "insert tool_call")
	srcDB.Close()

	// Empty destination DB.
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 1 {
		t.Fatalf("expected 1 orphaned, got %d", count)
	}

	// Verify tool_call was copied with correct message_id
	// mapping.
	var toolName, toolUseID string
	var msgID int
	err = dstDB.getReader().QueryRow(`
		SELECT tc.message_id, tc.tool_name, tc.tool_use_id
		FROM tool_calls tc
		WHERE tc.session_id = 's1'`,
	).Scan(&msgID, &toolName, &toolUseID)
	requireNoError(t, err, "query tool_call")
	if toolName != "Bash" {
		t.Errorf("tool_name = %q, want %q", toolName, "Bash")
	}
	if toolUseID != "tu_123" {
		t.Errorf(
			"tool_use_id = %q, want %q",
			toolUseID, "tu_123",
		)
	}

	// Verify the message_id FK is valid.
	var ordinal int
	err = dstDB.getReader().QueryRow(
		"SELECT ordinal FROM messages WHERE id = ?", msgID,
	).Scan(&ordinal)
	requireNoError(t, err, "verify FK")
	if ordinal != 1 {
		t.Errorf("tool_call message ordinal = %d, want 1",
			ordinal)
	}
}

func TestCopyOrphanedDataFrom_WithToolResultEvents(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	insertMessages(t, srcDB,
		userMsg("s1", 0, "hello"),
		asstMsg("s1", 1, "waited on child"),
	)
	_, err = srcDB.getWriter().Exec(`
		INSERT INTO tool_calls
			(message_id, session_id, tool_name, category,
			 tool_use_id, result_content_length, result_content)
		SELECT id, session_id, 'wait', 'Other',
			'call_wait', 23, 'Finished successfully'
		FROM messages
		WHERE session_id = 's1' AND ordinal = 1`,
	)
	requireNoError(t, err, "insert tool_call")
	_, err = srcDB.getWriter().Exec(`
		INSERT INTO tool_result_events
			(session_id, tool_call_message_ordinal, call_index,
			 tool_use_id, agent_id, subagent_session_id,
			 source, status, content, content_length,
			 timestamp, event_index)
		VALUES
			('s1', 1, 0, 'call_wait', 'agent-1', 'codex:agent-1',
			 'wait_output', 'completed', 'Finished successfully',
			 23, '2026-03-27T18:00:00Z', 0)`,
	)
	requireNoError(t, err, "insert tool_result_event")
	srcDB.Close()

	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 1 {
		t.Fatalf("expected 1 orphaned, got %d", count)
	}

	msgs, err := dstDB.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	if len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("tool calls len = %d, want 1", len(msgs[1].ToolCalls))
	}
	tc := msgs[1].ToolCalls[0]
	if tc.ResultContent != "Finished successfully" {
		t.Fatalf("result_content = %q, want %q", tc.ResultContent, "Finished successfully")
	}
	if len(tc.ResultEvents) != 1 {
		t.Fatalf("result events len = %d, want 1", len(tc.ResultEvents))
	}
	if tc.ResultEvents[0].Source != "wait_output" {
		t.Fatalf("event source = %q, want %q", tc.ResultEvents[0].Source, "wait_output")
	}
	if tc.ResultEvents[0].SubagentSessionID != "codex:agent-1" {
		t.Fatalf(
			"subagent_session_id = %q, want %q",
			tc.ResultEvents[0].SubagentSessionID, "codex:agent-1",
		)
	}
}

func TestCopyOrphanedDataFrom_AtomicOnFailure(t *testing.T) {
	dir := t.TempDir()

	// Create source DB with a session and messages.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	insertMessages(t, srcDB, userMsg("s1", 0, "hello"))
	srcDB.Close()

	// Corrupt source: drop the messages table so the
	// message-copy step fails.
	raw, err := sql.Open("sqlite3", srcPath)
	requireNoError(t, err, "raw open")
	_, err = raw.Exec("PRAGMA foreign_keys = OFF")
	requireNoError(t, err, "disable fk")
	_, err = raw.Exec("DROP TABLE messages")
	requireNoError(t, err, "drop messages")
	raw.Close()

	// Empty destination.
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	// CopyOrphanedDataFrom should fail on the message
	// copy step.
	_, err = dstDB.CopyOrphanedDataFrom(srcPath)
	if err == nil {
		t.Fatal("expected error from corrupted source")
	}

	// The session insert must have been rolled back — no
	// partial data in the destination.
	page, err := dstDB.ListSessions(
		context.Background(),
		SessionFilter{Limit: 100},
	)
	requireNoError(t, err, "list sessions")
	if len(page.Sessions) != 0 {
		t.Fatalf(
			"expected 0 sessions after failed copy, got %d",
			len(page.Sessions),
		)
	}
}

func TestCopyOrphanedDataFrom_IsSystem(t *testing.T) {
	dir := t.TempDir()

	// Source DB with a session containing a system message.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	m := userMsg("s1", 0, "system init")
	m.IsSystem = true
	insertMessages(t, srcDB, m, asstMsg("s1", 1, "reply"))
	srcDB.Close()

	// Empty destination.
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 1 {
		t.Fatalf("expected 1 orphaned, got %d", count)
	}

	// Verify is_system was preserved.
	msgs, err := dstDB.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "GetMessages")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if !msgs[0].IsSystem {
		t.Error("ordinal 0: is_system should be true")
	}
	if msgs[1].IsSystem {
		t.Error("ordinal 1: is_system should be false")
	}
}

func TestCopyOrphanedDataFrom_LegacyNoIsSystem(t *testing.T) {
	dir := t.TempDir()

	// Source DB with is_system column removed to simulate
	// a legacy database.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	insertMessages(t, srcDB, userMsg("s1", 0, "hello"))
	srcDB.Close()

	// Drop is_system via raw SQL to simulate legacy schema.
	raw, err := sql.Open("sqlite3", srcPath)
	requireNoError(t, err, "raw open")
	// SQLite doesn't support DROP COLUMN before 3.35;
	// recreate the table without is_system.
	_, err = raw.Exec(`
		CREATE TABLE messages_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			timestamp TEXT NOT NULL DEFAULT '',
			has_thinking INTEGER NOT NULL DEFAULT 0,
			has_tool_use INTEGER NOT NULL DEFAULT 0,
			content_length INTEGER NOT NULL DEFAULT 0
		)`)
	requireNoError(t, err, "create messages_new")
	_, err = raw.Exec(`
		INSERT INTO messages_new
			(id, session_id, ordinal, role, content,
			 timestamp, has_thinking, has_tool_use,
			 content_length)
		SELECT id, session_id, ordinal, role, content,
			timestamp, has_thinking, has_tool_use,
			content_length
		FROM messages`)
	requireNoError(t, err, "copy to messages_new")
	_, err = raw.Exec("DROP TABLE messages")
	requireNoError(t, err, "drop messages")
	_, err = raw.Exec(
		"ALTER TABLE messages_new RENAME TO messages",
	)
	requireNoError(t, err, "rename messages_new")
	raw.Close()

	// Empty destination (has is_system column).
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 1 {
		t.Fatalf("expected 1 orphaned, got %d", count)
	}

	// Message should be copied with is_system defaulting to
	// false.
	msgs, err := dstDB.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "GetMessages")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].IsSystem {
		t.Error("is_system should default to false")
	}
}

func TestCopyOrphanedDataFrom_TokenMetadata(t *testing.T) {
	dir := t.TempDir()

	// Source DB with session-level and message-level token data.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj", func(s *Session) {
		s.TotalOutputTokens = 5000
		s.PeakContextTokens = 120000
		s.HasTotalOutputTokens = true
		s.HasPeakContextTokens = true
	})
	msg := asstMsg("s1", 0, "response")
	msg.Model = "claude-opus-4-20250514"
	msg.TokenUsage = json.RawMessage(
		`{"output_tokens":500}`,
	)
	msg.ContextTokens = 80000
	msg.OutputTokens = 500
	msg.HasContextTokens = true
	msg.HasOutputTokens = true
	insertMessages(t, srcDB, msg)
	srcDB.Close()

	// Empty destination.
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	if count != 1 {
		t.Fatalf("expected 1 orphaned, got %d", count)
	}

	// Session token metadata must survive the copy.
	ctx := context.Background()
	s, err := dstDB.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession s1")
	if s == nil {
		t.Fatal("orphaned session s1 not found")
	}
	if s.TotalOutputTokens != 5000 {
		t.Errorf("TotalOutputTokens = %d, want 5000",
			s.TotalOutputTokens)
	}
	if s.PeakContextTokens != 120000 {
		t.Errorf("PeakContextTokens = %d, want 120000",
			s.PeakContextTokens)
	}
	if !s.HasTotalOutputTokens {
		t.Error("HasTotalOutputTokens should be true")
	}
	if !s.HasPeakContextTokens {
		t.Error("HasPeakContextTokens should be true")
	}

	// Message token metadata must survive the copy.
	msgs, err := dstDB.GetMessages(ctx, "s1", 0, 100, true)
	requireNoError(t, err, "GetMessages s1")
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	m := msgs[0]
	if m.Model != "claude-opus-4-20250514" {
		t.Errorf("Model = %q, want claude-opus-4-20250514",
			m.Model)
	}
	if m.ContextTokens != 80000 {
		t.Errorf("ContextTokens = %d, want 80000",
			m.ContextTokens)
	}
	if m.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500",
			m.OutputTokens)
	}
	if !m.HasContextTokens {
		t.Error("HasContextTokens should be true")
	}
	if !m.HasOutputTokens {
		t.Error("HasOutputTokens should be true")
	}
	if len(m.TokenUsage) == 0 {
		t.Error("TokenUsage should be preserved")
	}
}

func TestGetAgentsExcludesEmptyAgent(t *testing.T) {
	d := testDB(t)

	// Insert sessions with various agent values.
	insertSession(t, d, "s1", "proj",
		func(s *Session) { s.Agent = "claude" })
	insertSession(t, d, "s2", "proj",
		func(s *Session) { s.Agent = "cursor" })
	insertSession(t, d, "s3", "proj",
		func(s *Session) { s.Agent = "" })

	agents, err := d.GetAgents(context.Background(), false, false)
	if err != nil {
		t.Fatalf("GetAgents: %v", err)
	}

	for _, a := range agents {
		if a.Name == "" {
			t.Error("GetAgents returned empty agent name")
		}
	}
	if len(agents) != 2 {
		t.Errorf("got %d agents, want 2", len(agents))
	}
}

func TestGetAgentsEmptyResultSerializesAsArray(t *testing.T) {
	d := testDB(t)

	agents, err := d.GetAgents(context.Background(), false, false)
	if err != nil {
		t.Fatalf("GetAgents: %v", err)
	}
	if agents == nil {
		t.Fatal("GetAgents returned nil, want empty slice")
	}
	if len(agents) != 0 {
		t.Errorf("got %d agents, want 0", len(agents))
	}

	b, err := json.Marshal(agents)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Errorf("JSON = %s, want []", b)
	}
}

func TestStarSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")

	// Star existing session.
	ok, err := d.StarSession("s1")
	if err != nil || !ok {
		t.Fatalf("StarSession: ok=%v err=%v", ok, err)
	}

	// Idempotent re-star — should still return true (session exists).
	ok, err = d.StarSession("s1")
	if err != nil {
		t.Fatalf("re-star: %v", err)
	}
	if !ok {
		t.Error("re-star should return true (session exists, already starred)")
	}
	// This is acceptable — the session is already starred.

	// Listed.
	ids, err := d.ListStarredSessionIDs(ctx)
	if err != nil {
		t.Fatalf("ListStarredSessionIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "s1" {
		t.Errorf("listed = %v, want [s1]", ids)
	}

	// Unstar.
	if err := d.UnstarSession("s1"); err != nil {
		t.Fatalf("UnstarSession: %v", err)
	}
	ids, err = d.ListStarredSessionIDs(ctx)
	if err != nil {
		t.Fatalf("ListStarredSessionIDs after unstar: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("listed after unstar = %v, want []", ids)
	}

	// Star non-existent session returns false (no FK error).
	ok, err = d.StarSession("nonexistent")
	if err != nil {
		t.Fatalf("StarSession nonexistent: %v", err)
	}
	if ok {
		t.Error("StarSession should return false for non-existent session")
	}
}

func TestBulkStarSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")
	insertSession(t, d, "s2", "proj")

	// Bulk star with mix of valid and invalid IDs.
	err := d.BulkStarSessions([]string{"s1", "s2", "nonexistent"})
	if err != nil {
		t.Fatalf("BulkStarSessions: %v", err)
	}

	ids, err := d.ListStarredSessionIDs(ctx)
	if err != nil {
		t.Fatalf("ListStarredSessionIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("listed = %d, want 2", len(ids))
	}
}

func TestRestoreSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "p")

	t.Run("restore non-trashed returns 0", func(t *testing.T) {
		n, err := d.RestoreSession("s1")
		requireNoError(t, err, "RestoreSession")
		if n != 0 {
			t.Errorf("rows affected = %d, want 0", n)
		}
	})

	t.Run("restore non-existent returns 0", func(t *testing.T) {
		n, err := d.RestoreSession("no-such-session")
		requireNoError(t, err, "RestoreSession")
		if n != 0 {
			t.Errorf("rows affected = %d, want 0", n)
		}
	})

	t.Run("restore trashed returns 1", func(t *testing.T) {
		requireNoError(t, d.SoftDeleteSession("s1"), "SoftDeleteSession")

		// Should not appear in filtered list queries.
		f := filterWith(func(f *SessionFilter) {})
		page, err := d.ListSessions(ctx, f)
		requireNoError(t, err, "ListSessions")
		if len(page.Sessions) != 0 {
			t.Fatal("soft-deleted session should not appear in list")
		}

		// Should appear in trash list.
		trashed, err := d.ListTrashedSessions(ctx)
		requireNoError(t, err, "ListTrashedSessions")
		if len(trashed) != 1 {
			t.Fatalf("trash count = %d, want 1", len(trashed))
		}

		n, err := d.RestoreSession("s1")
		requireNoError(t, err, "RestoreSession")
		if n != 1 {
			t.Errorf("rows affected = %d, want 1", n)
		}

		// Should appear in list again.
		page, err = d.ListSessions(ctx, f)
		requireNoError(t, err, "ListSessions")
		if len(page.Sessions) != 1 {
			t.Fatal("restored session should appear in list")
		}
	})
}

func TestDeleteSessionExcludes(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")

	if err := d.DeleteSession("s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Session should be gone.
	requireSessionGone(t, d, "s1")

	// Session should be excluded.
	if !d.IsSessionExcluded("s1") {
		t.Error("session should be excluded after permanent delete")
	}

	// UpsertSession should return ErrSessionExcluded.
	err := d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "m", Agent: "claude",
	})
	if !errors.Is(err, ErrSessionExcluded) {
		t.Fatalf("UpsertSession = %v, want ErrSessionExcluded", err)
	}
	requireSessionGone(t, d, "s1")
}

func TestEmptyTrashExcludes(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")
	insertSession(t, d, "s2", "p")
	insertSession(t, d, "s3", "p")

	requireNoError(t, d.SoftDeleteSession("s1"), "SoftDeleteSession s1")
	requireNoError(t, d.SoftDeleteSession("s2"), "SoftDeleteSession s2")

	n, err := d.EmptyTrash()
	requireNoError(t, err, "EmptyTrash")
	if n != 2 {
		t.Errorf("EmptyTrash deleted = %d, want 2", n)
	}

	// Both should be excluded.
	if !d.IsSessionExcluded("s1") {
		t.Error("s1 should be excluded")
	}
	if !d.IsSessionExcluded("s2") {
		t.Error("s2 should be excluded")
	}

	// s3 should NOT be excluded.
	if d.IsSessionExcluded("s3") {
		t.Error("s3 should not be excluded")
	}

	// Re-upsert should return ErrSessionExcluded.
	err = d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "m", Agent: "claude",
	})
	if !errors.Is(err, ErrSessionExcluded) {
		t.Fatalf("UpsertSession s1 = %v, want ErrSessionExcluded", err)
	}
	requireSessionGone(t, d, "s1")

	// s3 should still be upsertable.
	s, _ := d.GetSession(context.Background(), "s3")
	if s == nil {
		t.Error("s3 should still be visible")
	}
}

func TestCopyExcludedSessionsFrom(t *testing.T) {
	dir := t.TempDir()

	// Source DB with excluded sessions.
	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")

	insertSession(t, srcDB, "s1", "p")
	requireNoError(t, srcDB.DeleteSession("s1"), "DeleteSession")
	if !srcDB.IsSessionExcluded("s1") {
		t.Fatal("s1 should be excluded in src")
	}
	srcDB.Close()

	// Destination DB (empty, simulates fresh resync DB).
	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	// Copy excluded sessions.
	if err := dstDB.CopyExcludedSessionsFrom(srcPath); err != nil {
		t.Fatalf("CopyExcludedSessionsFrom: %v", err)
	}

	// s1 should be excluded in destination.
	if !dstDB.IsSessionExcluded("s1") {
		t.Error("s1 should be excluded in dst after copy")
	}

	// Upserting s1 should be rejected.
	err = dstDB.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "m", Agent: "claude",
	})
	if !errors.Is(err, ErrSessionExcluded) {
		t.Errorf("UpsertSession = %v, want ErrSessionExcluded", err)
	}
}

func TestCopySessionMetadataFrom(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Source DB: session with display_name, deleted_at, and a pin.
	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	insertMessages(t, srcDB, Message{
		SessionID: "s1", Ordinal: 1, Role: "user",
		Content: "hello", ContentLength: 5,
	})
	dn := "my-custom-name"
	requireNoError(t, srcDB.RenameSession("s1", &dn), "Rename")
	requireNoError(t, srcDB.SoftDeleteSession("s1"), "SoftDelete")
	// Pin message ordinal 1.
	pinID, err := srcDB.PinMessage("s1", 1, nil)
	if err != nil || pinID == 0 {
		t.Fatalf("PinMessage in src: id=%d err=%v", pinID, err)
	}
	// Star the session.
	if _, err := srcDB.getWriter().Exec(
		"INSERT INTO starred_sessions (session_id) VALUES (?)", "s1",
	); err != nil {
		t.Fatalf("star session in src: %v", err)
	}
	srcDB.Close()

	// Destination DB: same session re-synced (no user metadata).
	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()
	insertSession(t, dstDB, "s1", "proj")
	insertMessages(t, dstDB, Message{
		SessionID: "s1", Ordinal: 1, Role: "user",
		Content: "hello", ContentLength: 5,
	})

	// Before copy: no metadata, no pins.
	s, err := dstDB.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession before")
	if s.DisplayName != nil {
		t.Errorf("display_name before = %v, want nil", *s.DisplayName)
	}
	if s.DeletedAt != nil {
		t.Errorf("deleted_at before = %v, want nil", *s.DeletedAt)
	}
	pins, err := dstDB.ListPinnedMessages(ctx, "s1", "")
	requireNoError(t, err, "ListPins before")
	if len(pins) != 0 {
		t.Errorf("pins before = %d, want 0", len(pins))
	}
	var starCount int
	requireNoError(t, dstDB.getReader().QueryRow(
		"SELECT count(*) FROM starred_sessions WHERE session_id = ?", "s1",
	).Scan(&starCount), "count stars before")
	if starCount != 0 {
		t.Errorf("stars before = %d, want 0", starCount)
	}

	// Copy metadata.
	if err := dstDB.CopySessionMetadataFrom(srcPath); err != nil {
		t.Fatalf("CopySessionMetadataFrom: %v", err)
	}

	// After copy: metadata, pin, and star should be merged.
	// Use GetSessionFull because deleted_at was copied, so
	// GetSession (which filters deleted_at IS NULL) returns nil.
	sf, err := dstDB.GetSessionFull(ctx, "s1")
	requireNoError(t, err, "GetSessionFull after")
	if sf == nil {
		t.Fatal("session should exist after metadata copy")
	}
	if sf.DisplayName == nil || *sf.DisplayName != dn {
		t.Errorf("display_name = %v, want %q", sf.DisplayName, dn)
	}
	if sf.DeletedAt == nil {
		t.Error("deleted_at should be set after copy")
	}
	pins, err = dstDB.ListPinnedMessages(ctx, "s1", "")
	requireNoError(t, err, "ListPins after")
	if len(pins) != 1 {
		t.Fatalf("pins after = %d, want 1", len(pins))
	}
	if pins[0].Ordinal != 1 {
		t.Errorf("pin ordinal = %d, want 1", pins[0].Ordinal)
	}
	requireNoError(t, dstDB.getReader().QueryRow(
		"SELECT count(*) FROM starred_sessions WHERE session_id = ?", "s1",
	).Scan(&starCount), "count stars after")
	if starCount != 1 {
		t.Errorf("stars after = %d, want 1", starCount)
	}
}

func TestCopySessionMetadataCopiesFromSource(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Source DB: session with display_name and deleted_at set.
	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	name := "my-name"
	requireNoError(t, srcDB.RenameSession("s1", &name), "Rename src")
	requireNoError(t, srcDB.SoftDeleteSession("s1"), "SoftDelete src")
	srcDB.Close()

	// Destination DB: same session, freshly synced (NULL metadata).
	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()
	insertSession(t, dstDB, "s1", "proj")

	requireNoError(t, dstDB.CopySessionMetadataFrom(srcPath), "CopySessionMetadataFrom")

	sf, err := dstDB.GetSessionFull(ctx, "s1")
	requireNoError(t, err, "GetSessionFull")
	if sf == nil {
		t.Fatal("session should exist")
	}
	if sf.DisplayName == nil || *sf.DisplayName != name {
		t.Errorf("display_name = %v, want %q", sf.DisplayName, name)
	}
	if sf.DeletedAt == nil {
		t.Error("deleted_at should be set from source")
	}
}

func TestCopySessionMetadataPreservesClears(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// Source DB: session was renamed and trashed, then user
	// cleared the name and restored (both columns now NULL).
	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	srcDB.Close()
	// Session has NULL display_name and NULL deleted_at.

	// Destination DB: freshly synced — also NULL.
	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()
	insertSession(t, dstDB, "s1", "proj")

	requireNoError(t, dstDB.CopySessionMetadataFrom(srcPath), "CopySessionMetadataFrom")

	sf, err := dstDB.GetSessionFull(ctx, "s1")
	requireNoError(t, err, "GetSessionFull")
	if sf == nil {
		t.Fatal("session should exist")
	}
	if sf.DisplayName != nil {
		t.Errorf(
			"display_name = %v, want nil (clear preserved)",
			sf.DisplayName,
		)
	}
	if sf.DeletedAt != nil {
		t.Errorf(
			"deleted_at = %v, want nil (restore preserved)",
			sf.DeletedAt,
		)
	}
}

func TestPinMessageIdempotent(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, userMsg("s1", 1, "hello"))

	// First pin should succeed.
	id1, err := d.PinMessage("s1", 1, nil)
	if err != nil || id1 == 0 {
		t.Fatalf("first PinMessage: id=%d err=%v", id1, err)
	}

	// Idempotent re-pin with same note must not return 0.
	id2, err := d.PinMessage("s1", 1, nil)
	if err != nil {
		t.Fatalf("idempotent PinMessage err: %v", err)
	}
	if id2 == 0 {
		t.Fatal("idempotent PinMessage returned id=0; should return existing id")
	}
	if id2 != id1 {
		t.Errorf("idempotent PinMessage id=%d, want %d", id2, id1)
	}

	// Re-pin with different note should succeed and return same id.
	note := "important"
	id2b, err := d.PinMessage("s1", 1, &note)
	if err != nil {
		t.Fatalf("re-pin with note err: %v", err)
	}
	if id2b != id1 {
		t.Errorf("re-pin with note id=%d, want %d", id2b, id1)
	}

	// Pin with wrong session should return 0.
	id3, err := d.PinMessage("nonexistent", 1, nil)
	if err != nil {
		t.Fatalf("wrong-session PinMessage err: %v", err)
	}
	if id3 != 0 {
		t.Errorf("wrong-session PinMessage id=%d, want 0", id3)
	}
}

func TestDeleteSessionIfTrashed(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// Delete a non-trashed session should return 0.
	n, err := d.DeleteSessionIfTrashed("s1")
	if err != nil {
		t.Fatalf("DeleteSessionIfTrashed non-trashed: %v", err)
	}
	if n != 0 {
		t.Errorf("non-trashed: rows=%d, want 0", n)
	}

	// Soft-delete, then permanent delete should succeed.
	requireNoError(t, d.SoftDeleteSession("s1"), "soft delete")
	n, err = d.DeleteSessionIfTrashed("s1")
	if err != nil {
		t.Fatalf("DeleteSessionIfTrashed trashed: %v", err)
	}
	if n != 1 {
		t.Errorf("trashed: rows=%d, want 1", n)
	}

	// Session should be gone.
	ctx := context.Background()
	s, err := d.GetSessionFull(ctx, "s1")
	if err != nil {
		t.Fatalf("GetSessionFull after delete: %v", err)
	}
	if s != nil {
		t.Error("session should be nil after permanent delete")
	}

	// Session should be excluded.
	if !d.IsSessionExcluded("s1") {
		t.Error("session should be in excluded_sessions")
	}

	// Non-existent session should return 0.
	n, err = d.DeleteSessionIfTrashed("nonexistent")
	if err != nil {
		t.Fatalf("DeleteSessionIfTrashed nonexistent: %v", err)
	}
	if n != 0 {
		t.Errorf("nonexistent: rows=%d, want 0", n)
	}
}

func TestMetadataQueriesExcludeTrashed(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj-a", func(s *Session) {
		s.Agent = "claude"
		s.Machine = "laptop"
	})
	insertSession(t, d, "s2", "proj-b", func(s *Session) {
		s.Agent = "codex"
		s.Machine = "desktop"
	})

	// Before trashing: both projects, agents, machines visible.
	projects, err := d.GetProjects(ctx, false, false)
	requireNoError(t, err, "GetProjects before trash")
	if len(projects) != 2 {
		t.Fatalf("projects before trash: got %d, want 2", len(projects))
	}

	agents, err := d.GetAgents(ctx, false, false)
	requireNoError(t, err, "GetAgents before trash")
	if len(agents) != 2 {
		t.Fatalf("agents before trash: got %d, want 2", len(agents))
	}

	machines, err := d.GetMachines(ctx, false, false)
	requireNoError(t, err, "GetMachines before trash")
	if len(machines) != 2 {
		t.Fatalf("machines before trash: got %d, want 2", len(machines))
	}

	// Soft-delete s2: its project/agent/machine should disappear.
	requireNoError(t, d.SoftDeleteSession("s2"), "soft delete s2")

	projects, err = d.GetProjects(ctx, false, false)
	requireNoError(t, err, "GetProjects after trash")
	if len(projects) != 1 {
		t.Errorf("projects after trash: got %d, want 1", len(projects))
	}
	if projects[0].Name != "proj-a" {
		t.Errorf("project name: got %q, want %q", projects[0].Name, "proj-a")
	}

	agents, err = d.GetAgents(ctx, false, false)
	requireNoError(t, err, "GetAgents after trash")
	if len(agents) != 1 {
		t.Errorf("agents after trash: got %d, want 1", len(agents))
	}
	if agents[0].Name != "claude" {
		t.Errorf("agent name: got %q, want %q", agents[0].Name, "claude")
	}

	machines, err = d.GetMachines(ctx, false, false)
	requireNoError(t, err, "GetMachines after trash")
	if len(machines) != 1 {
		t.Errorf("machines after trash: got %d, want 1", len(machines))
	}
	if machines[0] != "laptop" {
		t.Errorf("machine: got %q, want %q", machines[0], "laptop")
	}
}

func TestGetSessionExcludesTrashed(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj")

	// Before trashing: GetSession returns the session.
	s, err := d.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession before trash")
	if s == nil {
		t.Fatal("session should exist before trash")
	}

	// After trashing: GetSession returns nil.
	requireNoError(t, d.SoftDeleteSession("s1"), "soft delete")
	s, err = d.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession after trash")
	if s != nil {
		t.Error("GetSession should return nil for trashed session")
	}

	// GetSessionFull still returns it.
	sf, err := d.GetSessionFull(ctx, "s1")
	requireNoError(t, err, "GetSessionFull after trash")
	if sf == nil {
		t.Error("GetSessionFull should still return trashed session")
	}
}

func TestOpenMigratesColumnsWithoutDrop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Create a database with the pre-branch schema: sessions
	// table lacks display_name and deleted_at columns.
	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	requireNoError(t, err, "opening legacy db")
	conn.SetMaxOpenConns(1)

	oldSchema := `
CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    project     TEXT NOT NULL,
    machine     TEXT NOT NULL DEFAULT 'local',
    agent       TEXT NOT NULL DEFAULT 'claude',
    first_message TEXT,
    started_at  TEXT,
    ended_at    TEXT,
    message_count INTEGER NOT NULL DEFAULT 0,
    user_message_count INTEGER NOT NULL DEFAULT 0,
    file_path   TEXT,
    file_size   INTEGER,
    file_mtime  INTEGER,
    file_hash   TEXT,
    parent_session_id TEXT,
    relationship_type TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS messages (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL
        REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TEXT,
    has_thinking   INTEGER NOT NULL DEFAULT 0,
    has_tool_use   INTEGER NOT NULL DEFAULT 0,
    content_length INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, ordinal)
);
CREATE TABLE IF NOT EXISTS stats (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO stats (key, value)
    VALUES ('session_count', 0);
INSERT OR IGNORE INTO stats (key, value)
    VALUES ('message_count', 0);
CREATE TABLE IF NOT EXISTS tool_calls (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL
        REFERENCES messages(id) ON DELETE CASCADE,
    session_id TEXT NOT NULL
        REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name  TEXT NOT NULL,
    category   TEXT NOT NULL,
    tool_use_id TEXT,
    input_json  TEXT,
    skill_name  TEXT,
    result_content_length INTEGER,
    subagent_session_id TEXT
);
CREATE TABLE IF NOT EXISTS insights (
    id          INTEGER PRIMARY KEY,
    type        TEXT NOT NULL,
    date_from   TEXT NOT NULL,
    date_to     TEXT NOT NULL,
    project     TEXT,
    agent       TEXT NOT NULL,
    model       TEXT,
    prompt      TEXT,
    content     TEXT NOT NULL,
    created_at  TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);`

	_, err = conn.Exec(oldSchema)
	requireNoError(t, err, "creating legacy schema")

	// Stamp data version to current so we don't trigger resync.
	_, err = conn.Exec(
		fmt.Sprintf("PRAGMA user_version = %d", dataVersion),
	)
	requireNoError(t, err, "setting user_version")

	// Insert a session that must survive migration.
	_, err = conn.Exec(
		`INSERT INTO sessions (id, project, machine, agent,
			message_count)
		VALUES ('keep-me', 'myproj', 'local', 'claude', 3)`,
	)
	requireNoError(t, err, "inserting legacy session")
	requireNoError(t, conn.Close(), "closing legacy db")

	// Open via the normal path — should migrate, not drop.
	d, err := Open(path)
	requireNoError(t, err, "Open with legacy schema")
	defer d.Close()

	// Session data must survive.
	ctx := context.Background()
	s, err := d.GetSession(ctx, "keep-me")
	requireNoError(t, err, "GetSession after migration")
	if s == nil {
		t.Fatal("session lost during migration")
	}
	if s.Project != "myproj" {
		t.Errorf("project = %q, want myproj", s.Project)
	}
	if s.MessageCount != 3 {
		t.Errorf("message_count = %d, want 3", s.MessageCount)
	}

	// New columns must exist and be usable.
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET display_name = 'test' WHERE id = 'keep-me'",
	)
	requireNoError(t, err, "writing display_name")
	_, err = d.getWriter().Exec(
		"UPDATE sessions SET deleted_at = '2024-01-01' WHERE id = 'keep-me'",
	)
	requireNoError(t, err, "writing deleted_at")
}

func TestOpenBackfillsLegacyTokenCoverageFlags(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy-token-flags.db")

	conn, err := sql.Open("sqlite3", makeDSN(path, false))
	requireNoError(t, err, "opening legacy db")
	conn.SetMaxOpenConns(1)

	legacySchema := `
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
    deleted_at  TEXT,
    created_at  TEXT NOT NULL
        DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE TABLE IF NOT EXISTS messages (
    id             INTEGER PRIMARY KEY,
    session_id     TEXT NOT NULL
        REFERENCES sessions(id) ON DELETE CASCADE,
    ordinal        INTEGER NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TEXT,
    has_thinking   INTEGER NOT NULL DEFAULT 0,
    has_tool_use   INTEGER NOT NULL DEFAULT 0,
    content_length INTEGER NOT NULL DEFAULT 0,
    is_system      INTEGER NOT NULL DEFAULT 0,
    model          TEXT NOT NULL DEFAULT '',
    token_usage    TEXT NOT NULL DEFAULT '',
    context_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens  INTEGER NOT NULL DEFAULT 0,
    UNIQUE(session_id, ordinal)
);
CREATE TABLE IF NOT EXISTS insights (
    id          INTEGER PRIMARY KEY,
    type        TEXT NOT NULL DEFAULT '',
    date_from   TEXT NOT NULL,
    date_to     TEXT NOT NULL DEFAULT '',
    project     TEXT,
    agent       TEXT NOT NULL DEFAULT '',
    model       TEXT,
    prompt      TEXT,
    content     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS tool_calls (
    id                  INTEGER PRIMARY KEY,
    message_id          INTEGER NOT NULL,
    session_id          TEXT NOT NULL,
    tool_name           TEXT NOT NULL DEFAULT '',
    category            TEXT NOT NULL DEFAULT '',
    tool_use_id         TEXT,
    input_json          TEXT,
    skill_name          TEXT,
    result_content_length INTEGER,
    result_content      TEXT,
    subagent_session_id TEXT
);`

	_, err = conn.Exec(legacySchema)
	requireNoError(t, err, "creating legacy schema")
	_, err = conn.Exec(
		fmt.Sprintf("PRAGMA user_version = %d", dataVersion),
	)
	requireNoError(t, err, "setting user_version")

	_, err = conn.Exec(
		`INSERT INTO sessions (
			id, project, machine, agent, message_count,
			total_output_tokens, peak_context_tokens
		) VALUES
			('legacy-nonzero', 'proj', 'local', 'claude', 0, 200, 600),
			('legacy-zero', 'proj', 'local', 'claude', 1, 0, 0)`,
	)
	requireNoError(t, err, "inserting legacy sessions")
	_, err = conn.Exec(
		`INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			content_length, model, token_usage,
			context_tokens, output_tokens
		) VALUES
			('legacy-zero', 0, 'assistant', 'hi',
			 '2024-01-01T00:00:00Z', 2,
			 'claude-sonnet-4-20250514',
			 '{"input_tokens":0,"output_tokens":0}', 0, 0)`,
	)
	requireNoError(t, err, "inserting legacy message")
	requireNoError(t, conn.Close(), "closing legacy db")

	d, err := Open(path)
	requireNoError(t, err, "Open with legacy token schema")
	defer d.Close()

	ctx := context.Background()
	nonzero, err := d.GetSession(ctx, "legacy-nonzero")
	requireNoError(t, err, "GetSession legacy-nonzero")
	if nonzero == nil {
		t.Fatal("legacy-nonzero missing")
	}
	if !nonzero.HasTotalOutputTokens {
		t.Error("legacy-nonzero HasTotalOutputTokens = false, want true")
	}
	if !nonzero.HasPeakContextTokens {
		t.Error("legacy-nonzero HasPeakContextTokens = false, want true")
	}

	zero, err := d.GetSession(ctx, "legacy-zero")
	requireNoError(t, err, "GetSession legacy-zero")
	if zero == nil {
		t.Fatal("legacy-zero missing")
	}
	if !zero.HasTotalOutputTokens {
		t.Error("legacy-zero HasTotalOutputTokens = false, want true")
	}
	if !zero.HasPeakContextTokens {
		t.Error("legacy-zero HasPeakContextTokens = false, want true")
	}

	msgs, err := d.GetMessages(ctx, "legacy-zero", 0, 10, true)
	requireNoError(t, err, "GetMessages legacy-zero")
	if len(msgs) != 1 {
		t.Fatalf("legacy-zero messages = %d, want 1", len(msgs))
	}
	if !msgs[0].HasContextTokens {
		t.Error("legacy-zero message HasContextTokens = false, want true")
	}
	if !msgs[0].HasOutputTokens {
		t.Error("legacy-zero message HasOutputTokens = false, want true")
	}
}

func TestOpenRepairsLegacyCurrentSchemaTokenCoverageOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "current-token-flags.db")

	d, err := Open(path)
	requireNoError(t, err, "Open initial")
	_, err = d.getWriter().Exec(
		`INSERT INTO sessions (
			id, project, machine, agent, message_count,
			total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"current", "proj", "local", "claude", 1,
		0, 0, false, false,
	)
	requireNoError(t, err, "insert session")
	_, err = d.getWriter().Exec(
		`INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"current", 0, "assistant", "hello",
		tsZero, `{"input_tokens":0,"output_tokens":0}`, 0, 0,
		false, false,
	)
	requireNoError(t, err, "insert message")
	_, err = d.getWriter().Exec(
		`DELETE FROM stats WHERE key = ?`,
		tokenCoverageRepairStatsKey,
	)
	requireNoError(t, err, "clear token coverage repair marker")
	requireNoError(t, d.Close(), "Close initial")

	d, err = Open(path)
	requireNoError(
		t, err,
		"Open should repair legacy current-schema token coverage once",
	)

	ctx := context.Background()
	sess, err := d.GetSession(ctx, "current")
	requireNoError(t, err, "GetSession current")
	if sess == nil {
		t.Fatal("current session missing")
	}
	if !sess.HasTotalOutputTokens {
		t.Error("HasTotalOutputTokens = false, want true")
	}
	if !sess.HasPeakContextTokens {
		t.Error("HasPeakContextTokens = false, want true")
	}

	msgs, err := d.GetMessages(ctx, "current", 0, 10, true)
	requireNoError(t, err, "GetMessages current")
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	if !msgs[0].HasContextTokens {
		t.Error("HasContextTokens = false, want true")
	}
	if !msgs[0].HasOutputTokens {
		t.Error("HasOutputTokens = false, want true")
	}
	_, err = d.getWriter().Exec(
		`UPDATE sessions
		 SET has_total_output_tokens = 0,
		     has_peak_context_tokens = 0
		 WHERE id = ?`,
		"current",
	)
	requireNoError(t, err, "reset session flags")
	_, err = d.getWriter().Exec(
		`UPDATE messages
		 SET has_context_tokens = 0,
		     has_output_tokens = 0
		 WHERE session_id = ?`,
		"current",
	)
	requireNoError(t, err, "reset message flags")
	requireNoError(t, d.Close(), "Close repaired db")

	d, err = Open(path)
	requireNoError(
		t, err,
		"Open should skip token coverage repair after marker is stored",
	)
	defer d.Close()

	sess, err = d.GetSession(ctx, "current")
	requireNoError(t, err, "GetSession current after marker")
	if sess == nil {
		t.Fatal("current session missing after marker")
	}
	if sess.HasTotalOutputTokens {
		t.Error("HasTotalOutputTokens = true after marker, want false")
	}
	if sess.HasPeakContextTokens {
		t.Error("HasPeakContextTokens = true after marker, want false")
	}

	msgs, err = d.GetMessages(ctx, "current", 0, 10, true)
	requireNoError(t, err, "GetMessages current after marker")
	if len(msgs) != 1 {
		t.Fatalf("messages len after marker = %d, want 1", len(msgs))
	}
	if msgs[0].HasContextTokens {
		t.Error("HasContextTokens = true after marker, want false")
	}
	if msgs[0].HasOutputTokens {
		t.Error("HasOutputTokens = true after marker, want false")
	}
}

func TestBackfillMessageTokenCoverageSkipsRowsWithoutTokenSignals(
	t *testing.T,
) {
	d := testDB(t)

	_, err := d.getWriter().Exec(
		`INSERT INTO sessions (
			id, project, machine, agent, message_count
		) VALUES (?, ?, ?, ?, ?)`,
		"no-signal", "proj", "local", "claude", 1,
	)
	requireNoError(t, err, "insert session")
	_, err = d.getWriter().Exec(
		`INSERT INTO messages (
			session_id, ordinal, role, content, timestamp,
			token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"no-signal", 0, "assistant", "hello", tsZero, "", 0, 0,
		false, false,
	)
	requireNoError(t, err, "insert message")

	candidates, err := d.messageTokenCoverageBackfillCandidatesLocked(
		d.getWriter(),
	)
	requireNoError(t, err, "messageTokenCoverageBackfillCandidatesLocked")
	if len(candidates) != 0 {
		t.Fatalf("candidate count = %d, want 0", len(candidates))
	}
}

func TestOpenBackfillSessionTokenCoverageSkipsMessageScanWithoutCandidates(
	t *testing.T,
) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no-session-backfill-candidates.db")

	d, err := Open(path)
	requireNoError(t, err, "Open")
	defer d.Close()

	_, err = d.getWriter().Exec(
		`INSERT INTO sessions (
			id, project, machine, agent, message_count,
			has_total_output_tokens, has_peak_context_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"done", "proj", "local", "claude", 1, 1, 1,
	)
	requireNoError(t, err, "insert session")

	_, err = d.getWriter().Exec(
		`INSERT INTO messages (
			session_id, ordinal, role, content,
			has_context_tokens, has_output_tokens
		) VALUES (?, ?, ?, ?, ?, ?)`,
		"done", 0, "assistant", "hello", "not-a-bool", "not-a-bool",
	)
	requireNoError(t, err, "insert message")

	updates, err := d.backfillSessionTokenCoverageLocked(
		d.getWriter(),
	)
	requireNoError(t, err, "backfillSessionTokenCoverageLocked")
	if updates != 0 {
		t.Fatalf("updates = %d, want 0", updates)
	}
}

func TestGetSessionForIncremental(t *testing.T) {
	d := testDB(t)

	s := Session{
		ID:                   "codex:inc-test",
		Project:              "my-project",
		Machine:              "test",
		Agent:                "codex",
		FirstMessage:         new("hello world"),
		StartedAt:            new("2024-01-15T10:00:00Z"),
		EndedAt:              new("2024-01-15T10:30:00Z"),
		MessageCount:         5,
		UserMessageCount:     2,
		TotalOutputTokens:    500,
		PeakContextTokens:    1500,
		HasTotalOutputTokens: true,
		HasPeakContextTokens: true,
		FilePath:             new("/tmp/sessions/test.jsonl"),
		FileSize:             new(int64(4096)),
		FileMtime:            new(int64(999)),
	}
	requireNoError(t, d.UpsertSession(s), "upsert")

	t.Run("found", func(t *testing.T) {
		info, ok := d.GetSessionForIncremental(
			"/tmp/sessions/test.jsonl",
		)
		if !ok {
			t.Fatal("expected to find session")
		}
		if info.ID != "codex:inc-test" {
			t.Errorf("ID = %q, want codex:inc-test", info.ID)
		}
		if info.FileSize != 4096 {
			t.Errorf("FileSize = %d, want 4096", info.FileSize)
		}
		if info.MsgCount != 5 {
			t.Errorf("MsgCount = %d, want 5", info.MsgCount)
		}
		if info.UserMsgCount != 2 {
			t.Errorf("UserMsgCount = %d, want 2",
				info.UserMsgCount)
		}
		if info.TotalOutputTokens != 500 {
			t.Errorf("TotalOutputTokens = %d, want 500",
				info.TotalOutputTokens)
		}
		if info.PeakContextTokens != 1500 {
			t.Errorf("PeakContextTokens = %d, want 1500",
				info.PeakContextTokens)
		}
		if !info.HasTotalOutputTokens {
			t.Error("HasTotalOutputTokens = false, want true")
		}
		if !info.HasPeakContextTokens {
			t.Error("HasPeakContextTokens = false, want true")
		}
	})

	t.Run("not_found", func(t *testing.T) {
		_, ok := d.GetSessionForIncremental("/no/such/file")
		if ok {
			t.Error("expected not found")
		}
	})

	t.Run("multi_session_bails_out", func(t *testing.T) {
		// Two sessions sharing the same file_path (Claude
		// DAG fork) should prevent incremental parsing.
		path := "/tmp/sessions/forked.jsonl"
		for _, id := range []string{"fork-main", "fork-1"} {
			requireNoError(t, d.UpsertSession(Session{
				ID:       id,
				Agent:    "claude",
				FilePath: new(path),
				FileSize: new(int64(8192)),
			}), "upsert "+id)
		}
		_, ok := d.GetSessionForIncremental(path)
		if ok {
			t.Error(
				"expected false for multi-session file",
			)
		}
	})

	t.Run("legacy_false_flags_repaired", func(t *testing.T) {
		path := "/tmp/sessions/legacy-flags.jsonl"
		_, err := d.getWriter().Exec(
			`INSERT INTO sessions (
				id, project, machine, agent,
				message_count, user_message_count,
				file_path, file_size, file_mtime,
				total_output_tokens, peak_context_tokens,
				has_total_output_tokens, has_peak_context_tokens
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
			"legacy-flags", "proj", "local", "claude",
			2, 1, path, 1024, 100, 400, 900,
		)
		requireNoError(t, err, "insert legacy false flags")

		info, ok := d.GetSessionForIncremental(path)
		if !ok {
			t.Fatal("expected legacy session for incremental")
		}
		if !info.HasTotalOutputTokens {
			t.Error("HasTotalOutputTokens = false, want true")
		}
		if !info.HasPeakContextTokens {
			t.Error("HasPeakContextTokens = false, want true")
		}

		err = d.UpdateSessionIncremental(
			info.ID, nil, info.MsgCount+1, info.UserMsgCount,
			info.FileSize+256, 200,
			info.TotalOutputTokens+50, info.PeakContextTokens,
			info.HasTotalOutputTokens, info.HasPeakContextTokens,
		)
		requireNoError(t, err, "UpdateSessionIncremental legacy")

		got, err := d.GetSessionFull(context.Background(), info.ID)
		requireNoError(t, err, "GetSessionFull legacy")
		if got == nil {
			t.Fatal("legacy session missing after incremental")
		}
		if !got.HasTotalOutputTokens {
			t.Error("stored HasTotalOutputTokens = false, want true")
		}
		if !got.HasPeakContextTokens {
			t.Error("stored HasPeakContextTokens = false, want true")
		}
	})
}

func TestUpdateSessionIncremental(t *testing.T) {
	d := testDB(t)

	// Insert a session with all fields populated.
	s := Session{
		ID:                   "inc-update",
		Project:              "my-project",
		Machine:              "test",
		Agent:                "codex",
		FirstMessage:         new("hello"),
		StartedAt:            new("2024-01-15T10:00:00Z"),
		MessageCount:         3,
		UserMessageCount:     1,
		ParentSessionID:      new("parent-1"),
		RelationshipType:     "continuation",
		FilePath:             new("/tmp/sessions/update.jsonl"),
		FileSize:             new(int64(1024)),
		FileMtime:            new(int64(100)),
		FileHash:             new("abc123"),
		TotalOutputTokens:    300,
		PeakContextTokens:    1200,
		HasTotalOutputTokens: true,
		HasPeakContextTokens: true,
	}
	requireNoError(t, d.UpsertSession(s), "upsert")

	// Incremental update: bump counts and file metadata.
	ended := "2024-01-15T10:30:00Z"
	err := d.UpdateSessionIncremental(
		"inc-update", &ended, 7, 3, 2048, 200, 500, 1600, true, true,
	)
	requireNoError(t, err, "incremental update")

	// Verify updated fields changed.
	got, err := d.GetSessionFull(
		context.Background(), "inc-update",
	)
	requireNoError(t, err, "get session")
	if got.MessageCount != 7 {
		t.Errorf("MessageCount = %d, want 7",
			got.MessageCount)
	}
	if got.UserMessageCount != 3 {
		t.Errorf("UserMessageCount = %d, want 3",
			got.UserMessageCount)
	}
	if got.EndedAt == nil || *got.EndedAt != ended {
		t.Errorf("EndedAt = %v, want %q", got.EndedAt, ended)
	}
	if got.FileSize == nil || *got.FileSize != 2048 {
		t.Errorf("FileSize = %v, want 2048", got.FileSize)
	}
	if got.TotalOutputTokens != 500 {
		t.Errorf("TotalOutputTokens = %d, want 500",
			got.TotalOutputTokens)
	}
	if got.PeakContextTokens != 1600 {
		t.Errorf("PeakContextTokens = %d, want 1600",
			got.PeakContextTokens)
	}
	if !got.HasTotalOutputTokens {
		t.Error("HasTotalOutputTokens = false, want true")
	}
	if !got.HasPeakContextTokens {
		t.Error("HasPeakContextTokens = false, want true")
	}

	// Verify preserved fields were NOT cleared.
	if got.FirstMessage == nil || *got.FirstMessage != "hello" {
		t.Errorf("FirstMessage cleared: %v", got.FirstMessage)
	}
	if got.Project != "my-project" {
		t.Errorf("Project cleared: %q", got.Project)
	}
	if got.ParentSessionID == nil ||
		*got.ParentSessionID != "parent-1" {
		t.Errorf("ParentSessionID cleared: %v",
			got.ParentSessionID)
	}
	if got.RelationshipType != "continuation" {
		t.Errorf("RelationshipType cleared: %q",
			got.RelationshipType)
	}
	if got.FileHash == nil || *got.FileHash != "abc123" {
		t.Errorf("FileHash cleared: %v", got.FileHash)
	}
}

func TestSyncState_GetSetRoundtrip(t *testing.T) {
	d := testDB(t)

	// Initially empty.
	val, err := d.GetSyncState("last_push_at")
	requireNoError(t, err, "get initial")
	if val != "" {
		t.Fatalf("initial value = %q, want empty", val)
	}

	// Set and read back.
	if err := d.SetSyncState("last_push_at", "2026-03-11T12:00:00.000Z"); err != nil {
		t.Fatalf("set: %v", err)
	}
	val, err = d.GetSyncState("last_push_at")
	requireNoError(t, err, "get after set")
	if val != "2026-03-11T12:00:00.000Z" {
		t.Fatalf("value = %q, want 2026-03-11T12:00:00.000Z", val)
	}

	// Update.
	if err := d.SetSyncState("last_push_at", "2026-03-11T13:00:00.000Z"); err != nil {
		t.Fatalf("update: %v", err)
	}
	val, err = d.GetSyncState("last_push_at")
	requireNoError(t, err, "get after update")
	if val != "2026-03-11T13:00:00.000Z" {
		t.Fatalf("value = %q, want 2026-03-11T13:00:00.000Z", val)
	}
}

func TestListSessionsModifiedBetween(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Insert sessions with different timestamps.
	sessions := []Session{
		{ID: "s1", Project: "p", Machine: "local", Agent: "claude", CreatedAt: "2026-03-10T12:00:00.000Z"},
		{ID: "s2", Project: "p", Machine: "local", Agent: "claude", CreatedAt: "2026-03-11T12:00:00.000Z"},
		{ID: "s3", Project: "p", Machine: "local", Agent: "claude", CreatedAt: "2026-03-12T12:00:00.000Z"},
	}
	for _, s := range sessions {
		if err := d.UpsertSession(s); err != nil {
			t.Fatalf("upsert %s: %v", s.ID, err)
		}
	}

	// Backdate created_at for deterministic test results.
	for _, s := range sessions {
		_, err := d.getWriter().Exec(
			"UPDATE sessions SET created_at = ? WHERE id = ?",
			s.CreatedAt, s.ID,
		)
		if err != nil {
			t.Fatalf("backdate %s: %v", s.ID, err)
		}
	}

	// Query all.
	all, err := d.ListSessionsModifiedBetween(ctx, "", "", nil, nil)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("list all = %d, want 3", len(all))
	}

	// Query with since.
	since, err := d.ListSessionsModifiedBetween(ctx, "2026-03-11T00:00:00Z", "", nil, nil)
	if err != nil {
		t.Fatalf("list since: %v", err)
	}
	if len(since) != 2 {
		t.Fatalf("list since = %d, want 2", len(since))
	}

	// Query with until.
	until, err := d.ListSessionsModifiedBetween(ctx, "", "2026-03-11T12:00:00.000Z", nil, nil)
	if err != nil {
		t.Fatalf("list until: %v", err)
	}
	if len(until) != 2 {
		t.Fatalf("list until = %d, want 2", len(until))
	}

	// Query with both.
	between, err := d.ListSessionsModifiedBetween(ctx, "2026-03-10T12:00:00.000Z", "2026-03-11T12:00:00.000Z", nil, nil)
	if err != nil {
		t.Fatalf("list between: %v", err)
	}
	if len(between) != 1 {
		t.Fatalf("list between = %d, want 1 (s2 only)", len(between))
	}
	if between[0].ID != "s2" {
		t.Errorf("between[0].ID = %q, want s2", between[0].ID)
	}
}

func TestMessageContentFingerprint(t *testing.T) {
	d := testDB(t)
	sess := Session{ID: "fp-sess", Project: "p", Machine: "local", Agent: "claude"}
	if err := d.UpsertSession(sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.InsertMessages([]Message{
		{SessionID: "fp-sess", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "fp-sess", Ordinal: 1, Role: "assistant", Content: "hi there!", ContentLength: 9},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	sum, max, min, err := d.MessageContentFingerprint("fp-sess")
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if sum != 14 {
		t.Errorf("sum = %d, want 14", sum)
	}
	if max != 9 {
		t.Errorf("max = %d, want 9", max)
	}
	if min != 5 {
		t.Errorf("min = %d, want 5", min)
	}
}

func TestSystemMessageFingerprint(t *testing.T) {
	d := testDB(t)
	sess := Session{ID: "sys-fp", Project: "p", Machine: "local", Agent: "claude"}
	if err := d.UpsertSession(sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// System ordinals: 0 and 2 → "0,2".
	if err := d.InsertMessages([]Message{
		{SessionID: "sys-fp", Ordinal: 0, Role: "user", Content: "sys", ContentLength: 3, IsSystem: true},
		{SessionID: "sys-fp", Ordinal: 1, Role: "assistant", Content: "hi", ContentLength: 2},
		{SessionID: "sys-fp", Ordinal: 2, Role: "user", Content: "sys2", ContentLength: 4, IsSystem: true},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	fp, err := d.SystemMessageFingerprint("sys-fp")
	if err != nil {
		t.Fatalf("SystemMessageFingerprint: %v", err)
	}
	if fp != "0,2" {
		t.Errorf("fingerprint = %q, want %q", fp, "0,2")
	}

	// Regression: {0,3} and {1,2} both produce sum=3 and sum-of-squares differs,
	// but {0,4,5} and {1,2,6} (sum=9, sumSq=41) collide under the two-component
	// scheme. The string fingerprint is exact.
	for _, tc := range []struct {
		id       string
		ordinals []int // which ordinals are system
		want     string
	}{
		{"fp-03", []int{0, 3}, "0,3"},
		{"fp-12", []int{1, 2}, "1,2"},
		{"fp-045", []int{0, 4, 5}, "0,4,5"},
		{"fp-126", []int{1, 2, 6}, "1,2,6"},
	} {
		s := Session{ID: tc.id, Project: "p", Machine: "local", Agent: "claude"}
		if err := d.UpsertSession(s); err != nil {
			t.Fatalf("upsert %s: %v", tc.id, err)
		}
		maxOrd := 0
		for _, o := range tc.ordinals {
			if o > maxOrd {
				maxOrd = o
			}
		}
		msgs := make([]Message, maxOrd+1)
		systemSet := make(map[int]bool)
		for _, o := range tc.ordinals {
			systemSet[o] = true
		}
		for i := range maxOrd + 1 {
			msgs[i] = Message{
				SessionID: tc.id, Ordinal: i, Role: "user",
				Content: "x", ContentLength: 1,
				IsSystem: systemSet[i],
			}
		}
		if err := d.InsertMessages(msgs); err != nil {
			t.Fatalf("insert %s: %v", tc.id, err)
		}
		got, err := d.SystemMessageFingerprint(tc.id)
		if err != nil {
			t.Fatalf("SystemMessageFingerprint %s: %v", tc.id, err)
		}
		if got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestToolCallCountAndFingerprint(t *testing.T) {
	d := testDB(t)
	sess := Session{ID: "tc-sess", Project: "p", Machine: "local", Agent: "claude"}
	if err := d.UpsertSession(sess); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := d.InsertMessages([]Message{
		{
			SessionID: "tc-sess", Ordinal: 0, Role: "assistant", Content: "tool",
			ToolCalls: []ToolCall{
				{ToolName: "Read", Category: "Read", ResultContentLength: 100},
				{ToolName: "Write", Category: "Write", ResultContentLength: 50},
			},
		},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	count, err := d.ToolCallCount("tc-sess")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}

	sum, err := d.ToolCallContentFingerprint("tc-sess")
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if sum != 150 {
		t.Errorf("sum = %d, want 150", sum)
	}
}

func TestListSessionsModifiedBetween_ProjectFilter(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	sessions := []Session{
		{ID: "s1", Project: "alpha", Machine: "local", Agent: "claude", CreatedAt: "2026-03-10T12:00:00.000Z"},
		{ID: "s2", Project: "beta", Machine: "local", Agent: "claude", CreatedAt: "2026-03-10T12:00:00.000Z"},
		{ID: "s3", Project: "gamma", Machine: "local", Agent: "claude", CreatedAt: "2026-03-10T12:00:00.000Z"},
	}
	for _, s := range sessions {
		if err := d.UpsertSession(s); err != nil {
			t.Fatalf("upsert %s: %v", s.ID, err)
		}
	}
	for _, s := range sessions {
		_, err := d.getWriter().Exec(
			"UPDATE sessions SET created_at = ? WHERE id = ?",
			s.CreatedAt, s.ID,
		)
		if err != nil {
			t.Fatalf("backdate %s: %v", s.ID, err)
		}
	}

	tests := []struct {
		name            string
		projects        []string
		excludeProjects []string
		wantIDs         []string
	}{
		{
			name:    "no filter returns all",
			wantIDs: []string{"s1", "s2", "s3"},
		},
		{
			name:     "include alpha only",
			projects: []string{"alpha"},
			wantIDs:  []string{"s1"},
		},
		{
			name:     "include alpha and gamma",
			projects: []string{"alpha", "gamma"},
			wantIDs:  []string{"s1", "s3"},
		},
		{
			name:            "exclude beta",
			excludeProjects: []string{"beta"},
			wantIDs:         []string{"s1", "s3"},
		},
		{
			name:     "include nonexistent project",
			projects: []string{"nope"},
			wantIDs:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.ListSessionsModifiedBetween(
				ctx, "", "", tt.projects, tt.excludeProjects,
			)
			if err != nil {
				t.Fatalf("ListSessionsModifiedBetween: %v", err)
			}
			var gotIDs []string
			for _, s := range got {
				gotIDs = append(gotIDs, s.ID)
			}
			if len(gotIDs) != len(tt.wantIDs) {
				t.Fatalf("got %v, want %v", gotIDs, tt.wantIDs)
			}
			for i, id := range tt.wantIDs {
				if gotIDs[i] != id {
					t.Errorf("got[%d] = %q, want %q", i, gotIDs[i], id)
				}
			}
		})
	}
}

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

// TestMigration_TerminationStatusColumn simulates upgrading from a
// pre-termination_status schema. Drops the column and its index from
// a freshly-opened DB, reopens, and verifies the migration restores
// both without losing existing session data.
func TestMigration_TerminationStatusColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	d, err := Open(path)
	requireNoError(t, err, "initial open")
	insertSession(t, d, "s1", "proj")
	d.Close()

	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw open")

	// SQLite supports DROP COLUMN as of 3.35; the in-tree driver is
	// recent enough. Drop the index first since SQLite blocks
	// dropping a column referenced by an index.
	_, err = conn.Exec(`DROP INDEX IF EXISTS idx_sessions_termination_status`)
	requireNoError(t, err, "drop termination_status index")
	_, err = conn.Exec(`ALTER TABLE sessions DROP COLUMN termination_status`)
	requireNoError(t, err, "drop termination_status column")

	// Verify column and index are gone, and the row survived.
	var count int
	err = conn.QueryRow(
		`SELECT count(*) FROM pragma_table_info('sessions')
		 WHERE name = 'termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "verify column removed")
	if count != 0 {
		t.Fatal("expected termination_status column to be absent")
	}
	err = conn.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_sessions_termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "verify index removed")
	if count != 0 {
		t.Fatal("expected termination_status index to be absent")
	}
	var sessCount int
	err = conn.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessCount)
	requireNoError(t, err, "count sessions pre-migration")
	if sessCount != 1 {
		t.Fatalf("expected 1 session row, got %d", sessCount)
	}

	// Force the migration path: bump user_version down so Open()
	// re-runs the ADD COLUMN / CREATE INDEX steps.
	_, err = conn.Exec(`PRAGMA user_version = 0`)
	requireNoError(t, err, "reset user_version")
	conn.Close()

	d2, err := Open(path)
	requireNoError(t, err, "reopen after migration")
	defer d2.Close()

	// Column and index restored, row preserved.
	err = d2.getReader().QueryRow(
		`SELECT count(*) FROM pragma_table_info('sessions')
		 WHERE name = 'termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "verify column added")
	if count != 1 {
		t.Fatal("expected termination_status column after migration")
	}
	err = d2.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_sessions_termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "verify index added")
	if count != 1 {
		t.Fatal("expected termination_status index after migration")
	}

	sessions, err := d2.ListSessions(context.Background(), SessionFilter{})
	requireNoError(t, err, "list sessions")
	if len(sessions.Sessions) != 1 || sessions.Sessions[0].ID != "s1" {
		t.Fatalf("expected 1 session 's1' after migration, got %v",
			sessions.Sessions)
	}
	if sessions.Sessions[0].TerminationStatus != nil {
		t.Errorf("expected NULL termination_status after migration, got %q",
			*sessions.Sessions[0].TerminationStatus)
	}
}
