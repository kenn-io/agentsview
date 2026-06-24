package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func reflectedFieldValue(v any, name string) reflect.Value {
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return reflect.Value{}
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() || rv.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return rv.FieldByName(name)
}

func reflectedIntField(v any, name string) int {
	f := reflectedFieldValue(v, name)
	if !f.IsValid() || f.Kind() != reflect.Int {
		return 0
	}
	return int(f.Int())
}

func reflectedStringField(v any, name string) string {
	f := reflectedFieldValue(v, name)
	if !f.IsValid() {
		return ""
	}
	switch f.Kind() {
	case reflect.String:
		return f.String()
	case reflect.Pointer:
		if !f.IsNil() && f.Elem().Kind() == reflect.String {
			return f.Elem().String()
		}
	}
	return ""
}

func callUpdateSessionIncrementalCompat(
	t *testing.T,
	d *DB,
	id string,
	endedAt *string,
	msgCount, userMsgCount int,
	fileSize, fileMtime int64,
	nextOrdinal int,
	lastEntryUUID string,
	totalOutputTokens, peakContextTokens int,
	hasTotalOutputTokens, hasPeakContextTokens bool,
) error {
	t.Helper()

	updateMethod := reflect.ValueOf(d).MethodByName("UpdateSessionIncremental")
	require.True(t, updateMethod.IsValid(), "UpdateSessionIncremental")
	if updateMethod.Type().NumIn() == 2 &&
		updateMethod.Type().In(1).Kind() == reflect.Struct {
		update := reflect.New(updateMethod.Type().In(1)).Elem()
		if f := update.FieldByName("EndedAt"); f.IsValid() {
			if endedAt == nil {
				f.Set(reflect.Zero(f.Type()))
			} else {
				f.Set(reflect.ValueOf(endedAt))
			}
		}
		update.FieldByName("MsgCount").SetInt(int64(msgCount))
		update.FieldByName("UserMsgCount").SetInt(int64(userMsgCount))
		update.FieldByName("FileSize").SetInt(fileSize)
		update.FieldByName("FileMtime").SetInt(fileMtime)
		if f := update.FieldByName("NextOrdinal"); f.IsValid() {
			f.SetInt(int64(nextOrdinal))
		}
		if f := update.FieldByName("LastEntryUUID"); f.IsValid() {
			f.SetString(lastEntryUUID)
		}
		update.FieldByName("TotalOutputTokens").SetInt(int64(totalOutputTokens))
		update.FieldByName("PeakContextTokens").SetInt(int64(peakContextTokens))
		update.FieldByName("HasTotalOutputTokens").SetBool(hasTotalOutputTokens)
		update.FieldByName("HasPeakContextTokens").SetBool(hasPeakContextTokens)
		results := updateMethod.Call([]reflect.Value{
			reflect.ValueOf(id),
			update,
		})
		if results[0].IsNil() {
			return nil
		}
		return results[0].Interface().(error)
	}

	results := updateMethod.Call([]reflect.Value{
		reflect.ValueOf(id),
		reflect.ValueOf(endedAt),
		reflect.ValueOf(msgCount),
		reflect.ValueOf(userMsgCount),
		reflect.ValueOf(fileSize),
		reflect.ValueOf(fileMtime),
		reflect.ValueOf(totalOutputTokens),
		reflect.ValueOf(peakContextTokens),
		reflect.ValueOf(hasTotalOutputTokens),
		reflect.ValueOf(hasPeakContextTokens),
	})
	if results[0].IsNil() {
		return nil
	}
	return results[0].Interface().(error)
}

const blockingCloseDriverName = "agentsview-blocking-close"

var (
	registerBlockingCloseDriverOnce sync.Once
	blockingCloseDriverStates       sync.Map
)

type blockingCloseState struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
}

type blockingCloseDriver struct{}

func (blockingCloseDriver) Open(name string) (driver.Conn, error) {
	state, ok := blockingCloseDriverStates.Load(name)
	if !ok {
		return nil, fmt.Errorf("missing blocking close state for %q", name)
	}
	return &blockingCloseConn{
		state: state.(*blockingCloseState),
	}, nil
}

type blockingCloseConn struct {
	state *blockingCloseState
}

func (c *blockingCloseConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not implemented")
}

func (c *blockingCloseConn) Close() error {
	c.state.startedOnce.Do(func() {
		close(c.state.started)
	})
	<-c.state.release
	return nil
}

func (c *blockingCloseConn) Begin() (driver.Tx, error) {
	return nil, errors.New("begin not implemented")
}

func openBlockingCloseDB(
	t *testing.T,
) (*sql.DB, <-chan struct{}, func()) {
	t.Helper()

	registerBlockingCloseDriverOnce.Do(func() {
		sql.Register(blockingCloseDriverName, blockingCloseDriver{})
	})

	dsn := fmt.Sprintf("%s/%p", t.Name(), t)
	state := &blockingCloseState{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	blockingCloseDriverStates.Store(dsn, state)

	pool, err := sql.Open(blockingCloseDriverName, dsn)
	require.NoError(t, err, "opening blocking close pool")
	pool.SetMaxOpenConns(1)
	require.NoError(t, pool.PingContext(context.Background()),
		"priming blocking close pool")

	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(state.release)
			blockingCloseDriverStates.Delete(dsn)
		})
	}
	t.Cleanup(release)

	return pool, state.started, release
}

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
	require.NoError(t, err, "ListSessions")
	assert.Len(t, page.Sessions, want, "session count")
}

// requireSessions lists sessions with filter and asserts the exact IDs returned.
func requireSessions(
	t *testing.T, d *DB, f SessionFilter, wantIDs []string,
) {
	t.Helper()
	page, err := d.ListSessions(
		context.Background(), f,
	)
	require.NoError(t, err, "ListSessions")

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

// requireNoError fails the test if err is not nil. Wraps testify's
// require.NoError to preserve the legacy helper signature used throughout
// the package.
func requireNoError(t *testing.T, err error, msg string) {
	t.Helper()
	require.NoError(t, err, msg)
}

// requireErrContains fails if err is nil or doesn't contain
// substr.
func requireErrContains(
	t *testing.T, err error, substr string,
) {
	t.Helper()
	require.Error(t, err, "expected error, got nil")
	assert.Contains(t, err.Error(), substr,
		"error %q does not contain %q", err.Error(), substr)
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
	require.NoError(t, err, "opening test db")
	t.Cleanup(func() { d.Close() })
	return d
}

// Ptr returns a pointer to v.
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
	require.NoError(t, d.UpsertSession(s), "insertSession %s", id)
}

// updateSignals is a helper that updates session signal columns
// and fails the test on error.
func updateSignals(
	t *testing.T, d *DB, id string, u SessionSignalUpdate,
) {
	t.Helper()
	require.NoError(t, d.UpdateSessionSignals(id, u),
		"updateSignals %s", id)
}

// insertMessages is a helper that inserts messages and fails
// the test on error.
func insertMessages(t *testing.T, d *DB, msgs ...Message) {
	t.Helper()
	require.NoError(t, d.InsertMessages(msgs), "insertMessages")
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
	require.ErrorIs(t, err, context.Canceled,
		"expected context.Canceled")
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
	require.NoError(t, err, "GetSession %q", id)
	require.NotNil(t, s, "session %q should exist", id)
	return s
}

// requireSessionGone asserts that a session does not exist.
func requireSessionGone(t *testing.T, d *DB, id string) {
	t.Helper()
	s, err := d.GetSession(context.Background(), id)
	require.NoError(t, err, "GetSession %q", id)
	require.Nil(t, s, "session %q should be gone", id)
}

func TestOpenCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "test.db")
	d, err := Open(path)
	requireNoError(t, err, "Open")
	defer d.Close()

	_, err = os.Stat(path)
	require.NoError(t, err, "db file not created")
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
	require.True(t, d2.NeedsResync(),
		"expected NeedsResync()=true after version bump")

	// Session and messages must still exist.
	page, err := d2.ListSessions(
		context.Background(),
		SessionFilter{Limit: 100},
	)
	requireNoError(t, err, "list sessions")
	require.Len(t, page.Sessions, 1, "expected 1 session preserved, got")

	msgs, err := d2.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "get messages")
	require.Len(t, msgs, 2, "expected 2 messages preserved, got")

	// user_version must stay stale — it is only bumped
	// after a successful ResyncAll, not at Open() time.
	var ver int
	err = d2.getReader().QueryRow(
		"PRAGMA user_version",
	).Scan(&ver)
	requireNoError(t, err, "read version")
	require.Equal(t, 0, ver, "expected user_version=0 (stale)")
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
	require.True(t, d2.NeedsResync(),
		"first reopen: expected NeedsResync=true")
	d2.Close() // simulate process exit without resync

	// Second reopen: must still detect stale because the
	// version was not bumped.
	d3, err := Open(path)
	requireNoError(t, err, "reopen 2")
	defer d3.Close()
	require.True(t, d3.NeedsResync(),
		"second reopen: expected NeedsResync=true")
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
	require.Equal(t, 0, count,
		"expected result_content column to be absent")
	var tcCount int
	err = conn.QueryRow(
		`SELECT count(*) FROM tool_calls`,
	).Scan(&tcCount)
	requireNoError(t, err, "count tool_calls pre-migration")
	require.Equal(t, 1, tcCount, "expected 1 tool_call row, got")
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
	require.Equal(t, 1, count,
		"expected result_content column after migration")

	// Verify tool_calls row preserved with fields intact.
	msgs, err := d2.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "get messages")
	require.Len(t, msgs, 2, "expected 2 messages")
	require.Len(t, msgs[1].ToolCalls, 1, "expected 1 tool call, got")
	tc := msgs[1].ToolCalls[0]
	assert.Equal(t, "Read", tc.ToolName, "ToolName")
	assert.Equal(t, "tu1", tc.ToolUseID, "ToolUseID")
	assert.Equal(t, 42, tc.ResultContentLength, "ResultContentLength")
	assert.Equal(t, "", tc.ResultContent, "ResultContent")
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
	require.Equal(t, 0, count,
		"expected tool_result_events table to be absent")
	requireNoError(t, conn.Close(), "close legacy db")

	d2, err := Open(path)
	requireNoError(t, err, "reopen after migration")
	defer d2.Close()

	requireSessionExists(t, d2, "s1")
	require.True(t, d2.NeedsResync(),
		"expected NeedsResync()=true after data version bump")

	err = d2.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'table' AND name = 'tool_result_events'`,
	).Scan(&count)
	requireNoError(t, err, "verify table exists")
	require.Equal(t, 1, count,
		"expected tool_result_events table after reopen")
}

func TestCurrentDataVersionSanitizedMessageShape(t *testing.T) {
	assert.Equal(t, 51, CurrentDataVersion(),
		"Gemini cumulative-to-delta reparse requires a data version bump")
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
	require.Len(t, msgs, 1, "len")
	require.Len(t, msgs[0].ToolCalls, 1, "len")
	tc := msgs[0].ToolCalls[0]
	require.Len(t, tc.ResultEvents, 2, "len")
	assert.Equal(t, "agent-1", tc.ResultEvents[0].AgentID, "result event 0 agent_id")
	assert.Equal(t, "subagent_notification", tc.ResultEvents[1].Source, "result event 1 source")
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

	require.False(t, d2.NeedsResync(),
		"expected NeedsResync()=false at current version")

	page, err := d2.ListSessions(
		context.Background(),
		SessionFilter{Limit: 100},
	)
	requireNoError(t, err, "list sessions")
	require.Len(t, page.Sessions, 1, "expected 1 session preserved, got")
}

func TestOpenRejectsNewerDataVersion(t *testing.T) {
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
	d2, openErr := Open(path)
	require.Nil(t, d2, "newer database must not open")
	require.Error(t, openErr, "newer database must be rejected")

	var version int
	conn, err := sql.Open("sqlite3", path)
	requireNoError(t, err, "raw sqlite open")
	defer conn.Close()
	err = conn.QueryRow(
		"PRAGMA user_version",
	).Scan(&version)
	requireNoError(t, err, "read version")

	assert.Equal(t, futureVersion, version,
		"user_version should not be mutated")
	assert.True(t, IsDataVersionTooNew(openErr),
		"expected too-new data version error")
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
		require.NoError(t, os.Mkdir(sub, 0o755))
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
		require.Error(t, err, "expected error")
		assert.ErrorIs(t, err, fs.ErrPermission,
			"expected permission error")
		assert.Contains(t, err.Error(), "checking database",
			"expected database compatibility wrapper")
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
		require.Error(t, err, "expected error")
		assert.True(t,
			strings.Contains(err.Error(), "checking database") ||
				strings.Contains(err.Error(), "probing data version"),
			"unexpected error: %v", err)
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

	err := d.UpsertSession(s)
	require.NoError(t, err, "UpsertSession")

	got := requireSessionExists(t, d, "test-session-1")
	assert.Equal(t, "my_project", got.Project, "project")
	assert.Equal(t, 5, got.MessageCount, "message_count")

	// Update
	s.MessageCount = 10
	err = d.UpsertSession(s)
	require.NoError(t, err, "UpsertSession update")
	got = requireSessionExists(t, d, "test-session-1")
	assert.Equal(t, 10, got.MessageCount, "after update: message_count")

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
		require.NotNil(t, got.ParentSessionID, "parent_session_id")
		assert.Equal(t, "parent-uuid", *got.ParentSessionID,
			"parent_session_id")
	})

	t.Run("WithoutParent", func(t *testing.T) {
		insertSession(t, d, "child-2", "proj")

		got := requireSessionExists(t, d, "child-2")
		assert.Nil(t, got.ParentSessionID, "parent_session_id")
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
				if assert.NotNil(t, s.ParentSessionID,
					"parent_session_id want %q", "parent-uuid") {
					assert.Equal(t, "parent-uuid",
						*s.ParentSessionID, "parent_session_id")
				}
			}
		}
		assert.True(t, found, "child-1 not found in list")
	})

	t.Run("ParentInGetSessionFull", func(t *testing.T) {
		got, err := d.GetSessionFull(
			context.Background(), "child-1",
		)
		requireNoError(t, err, "GetSessionFull")
		require.NotNil(t, got, "session not found")
		require.NotNil(t, got.ParentSessionID,
			"parent_session_id want %q", "parent-uuid")
		assert.Equal(t, "parent-uuid", *got.ParentSessionID,
			"parent_session_id")
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
		require.Len(t, children, 3, "expected 3 visible children")
		// Ordered by started_at ascending.
		wantIDs := []string{"child-sub", "child-cont", "child-fork"}
		for i, want := range wantIDs {
			assert.Equal(t, want, children[i].ID,
				"children[%d].ID", i)
		}
	})

	t.Run("NoChildren", func(t *testing.T) {
		children, err := d.GetChildSessions(
			context.Background(), "unrelated",
		)
		requireNoError(t, err, "GetChildSessions")
		require.Len(t, children, 0, "expected 0 children")
	})

	t.Run("NonexistentParent", func(t *testing.T) {
		children, err := d.GetChildSessions(
			context.Background(), "no-such-parent",
		)
		requireNoError(t, err, "GetChildSessions")
		require.Len(t, children, 0, "expected 0 children")
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
	assert.Len(t, page.Sessions, 2, "len")
	assert.NotEmpty(t, page.NextCursor, "expected next cursor")

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
		require.NoError(t, err, "ListSessions page %d", pages)
		for _, s := range page.Sessions {
			assert.False(t, seen[s.ID],
				"duplicate session %s on page %d",
				s.ID, pages)
			seen[s.ID] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assert.Len(t, seen, 5, "saw")
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
		require.NoError(t, err, "ListSessions")
		for _, s := range page.Sessions {
			assert.False(t, seen[s.ID], "duplicate session %s", s.ID)
			seen[s.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	assert.Len(t, seen, 4, "saw")
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
	require.Equal(t, 2, page.Total, "total")

	got := map[string]bool{}
	for _, session := range page.Sessions {
		got[session.Machine] = true
	}
	require.True(t, got["local"],
		"machines = %v, want local included", got)
	require.True(t, got["other"],
		"machines = %v, want other included", got)
	require.False(t, got["remote"],
		"machines = %v, want remote excluded", got)
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
	require.Len(t, got, 4, "len")
	assert.Equal(t, "Hello", got[0].Content, "first message")
	assert.Empty(t, got[3].Timestamp, "expected empty timestamp")

	// Paginated
	got, err = d.GetMessages(context.Background(), "s1", 1, 2, true)
	requireNoError(t, err, "GetMessages")
	require.Len(t, got, 2, "len")
	assert.Equal(t, 1, got[0].Ordinal, "first ordinal")

	// Descending
	got, err = d.GetMessages(context.Background(), "s1", 2, 10, false)
	requireNoError(t, err, "GetMessages desc")
	require.Len(t, got, 3, "len")
	assert.Equal(t, 2, got[0].Ordinal, "desc first ordinal")
}

func TestReplaceSessionMessages(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")

	insertMessages(t, d, userMsg("s1", 0, "old"))

	require.NoError(t, d.ReplaceSessionMessages("s1", []Message{
		userMsg("s1", 0, "new1"),
		asstMsg("s1", 1, "new2"),
	}), "ReplaceSessionMessages")

	got, _ := d.GetAllMessages(context.Background(), "s1")
	require.Len(t, got, 2, "len")
	assert.Equal(t, "new1", got[0].Content, "content")
}

func TestInsertMessagesClassifiesAutomationFromUserTranscript(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	title := "Generated review title"
	insertSession(t, d, "insert-review-title", "p", func(s *Session) {
		s.FirstMessage = &title
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	require.NoError(t, d.InsertMessages([]Message{
		userMsg("insert-review-title", 0,
			"You are a code reviewer. Review the code changes shown below."),
		asstMsg("insert-review-title", 1, "Review complete."),
	}), "InsertMessages")

	got, err := d.GetSession(ctx, "insert-review-title")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, got, "insert-review-title session")
	assert.True(t, got.IsAutomated,
		"automation should be classified from the first inserted user message")
}

func TestUpsertSessionPreservesTranscriptAutomation(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	title := "Generated review title"
	insertSession(t, d, "upsert-review-title", "p", func(s *Session) {
		s.FirstMessage = &title
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	require.NoError(t, d.InsertMessages([]Message{
		userMsg("upsert-review-title", 0,
			"You are a code reviewer. Review the code changes shown below."),
		asstMsg("upsert-review-title", 1, "Review complete."),
	}), "InsertMessages")

	require.NoError(t, d.UpsertSession(Session{
		ID:               "upsert-review-title",
		Project:          "p",
		Machine:          defaultMachine,
		Agent:            defaultAgent,
		FirstMessage:     &title,
		MessageCount:     2,
		UserMessageCount: 1,
		IsAutomated:      true,
	}), "UpsertSession")

	got, err := d.GetSession(ctx, "upsert-review-title")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, got, "upsert-review-title session")
	assert.True(t, got.IsAutomated,
		"upsert should persist the transcript-derived automation flag")
}

func TestReplaceSessionMessagesClassifiesAutomationFromUserTranscript(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	title := "Generated review title"
	insertSession(t, d, "review-title", "p", func(s *Session) {
		s.FirstMessage = &title
		s.MessageCount = 2
		s.UserMessageCount = 1
	})

	require.NoError(t, d.ReplaceSessionMessages("review-title", []Message{
		userMsg("review-title", 0,
			"You are a code reviewer. Review the code changes shown below."),
		asstMsg("review-title", 1, "Review complete."),
	}), "ReplaceSessionMessages")

	got, err := d.GetSession(ctx, "review-title")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, got, "review-title session")
	assert.True(t, got.IsAutomated,
		"automation should be classified from the first stored user message")

	page, err := d.ListSessions(ctx, SessionFilter{
		ExcludeAutomated: true,
		Limit:            10,
	})
	require.NoError(t, err, "ListSessions")
	assert.Empty(t, page.Sessions,
		"automated transcript should be excluded even when first_message is a title")
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
	require.NoError(t, err, "GetAllMessages")

	// Pin ordinal-0 with a note and ordinal-2 with no note.
	note := "important"
	_, err = d.PinMessage("s1", msgs[0].ID, &note)
	require.NoError(t, err, "PinMessage ord=0")
	_, err = d.PinMessage("s1", msgs[2].ID, nil)
	require.NoError(t, err, "PinMessage ord=2")

	// Record created_at before replace so we can verify it is preserved.
	prePins, err := d.ListPinnedMessages(ctx, "s1", "")
	require.NoError(t, err, "ListPinnedMessages before replace")
	pinCreatedAt := make(map[int]string) // ordinal → created_at
	for _, p := range prePins {
		pinCreatedAt[p.Ordinal] = p.CreatedAt
	}

	// Full replace (simulates a resync of an OpenCode or
	// explicitly re-synced session).
	require.NoError(t, d.ReplaceSessionMessages("s1", []Message{
		userMsg("s1", 0, "msg0-updated"),
		asstMsg("s1", 1, "msg1-updated"),
		userMsg("s1", 2, "msg2-updated"),
	}), "ReplaceSessionMessages")

	newMsgs, err := d.GetAllMessages(ctx, "s1")
	require.NoError(t, err, "GetAllMessages after replace")
	require.Len(t, newMsgs, 3, "want 3 messages after replace")

	pins, err := d.ListPinnedMessages(ctx, "s1", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 2, "want 2 pins after replace")

	byOrdinal := make(map[int]PinnedMessage)
	for _, p := range pins {
		byOrdinal[p.Ordinal] = p
	}

	// Ordinal-0: note preserved, message_id updated, created_at preserved.
	p0, ok := byOrdinal[0]
	require.True(t, ok, "pin for ordinal 0 missing after replace")
	assert.Equal(t, newMsgs[0].ID, p0.MessageID, "ord=0 pin message_id")
	if assert.NotNil(t, p0.Note, "ord=0 pin note want %q", note) {
		assert.Equal(t, note, *p0.Note, "ord=0 pin note")
	}
	assert.Equal(t, pinCreatedAt[0], p0.CreatedAt, "ord=0 pin created_at")

	// Ordinal-2: nil note preserved, message_id updated.
	p2, ok := byOrdinal[2]
	require.True(t, ok, "pin for ordinal 2 missing after replace")
	assert.Equal(t, newMsgs[2].ID, p2.MessageID, "ord=2 pin message_id")
	assert.Nil(t, p2.Note, "ord=2 pin note")
	assert.Equal(t, pinCreatedAt[2], p2.CreatedAt, "ord=2 pin created_at")
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
	require.NoError(t, err, "GetAllMessages")
	// Pin both messages.
	for _, m := range msgs {
		_, err := d.PinMessage("s1", m.ID, nil)
		require.NoError(t, err, "PinMessage")
	}

	// Replace with only ordinal-0 (ordinal-1 is gone).
	require.NoError(t, d.ReplaceSessionMessages("s1", []Message{
		userMsg("s1", 0, "msg0-updated"),
	}), "ReplaceSessionMessages")

	pins, err := d.ListPinnedMessages(ctx, "s1", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1, "want 1 pin (ordinal-1 dropped)")
	assert.Equal(t, 0, pins[0].Ordinal, "surviving pin ordinal")
	assert.Nil(t, pins[0].Note, "surviving pin note")
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
	require.NoError(t, err, "GetAllMessages")
	note := "important"
	_, err = d.PinMessage("s1", msgs[1].ID, &note)
	require.NoError(t, err, "PinMessage")

	// Rewrite: a compact-boundary row is now ordinal 1, pushing
	// "answer" to ordinal 2. The pin should follow uuid-answer
	// to its new ordinal, not stay on ordinal 1 (the boundary).
	require.NoError(t, d.ReplaceSessionMessages("s1", []Message{
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
	}), "ReplaceSessionMessages")

	pins, err := d.ListPinnedMessages(ctx, "s1", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1, "want 1 pin")
	assert.Equal(t, 2, pins[0].Ordinal,
		"pin ordinal want 2 (followed source_uuid)")
	if assert.NotNil(t, pins[0].Note, "pin note want %q", note) {
		assert.Equal(t, note, *pins[0].Note, "pin note")
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
	require.NoError(t, err, "GetAllMessages")
	_, err = d.PinMessage("s1", msgs[1].ID, nil)
	require.NoError(t, err, "PinMessage")

	// Replace with the same ordinals (and still no source_uuid).
	require.NoError(t, d.ReplaceSessionMessages("s1", []Message{
		userMsg("s1", 0, "msg0-v2"),
		asstMsg("s1", 1, "msg1-v2"),
	}), "ReplaceSessionMessages")

	pins, err := d.ListPinnedMessages(ctx, "s1", "")
	require.NoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1, "want 1 pin")
	assert.Equal(t, 1, pins[0].Ordinal, "pin ordinal")
}

func TestGetSessionFilePath(t *testing.T) {
	d := testDB(t)

	fp := "/tmp/sessions/abc.jsonl"
	insertSession(t, d, "zencoder:abc", "p", func(s *Session) {
		s.FilePath = &fp
	})

	got := d.GetSessionFilePath("zencoder:abc")
	assert.Equal(t, fp, got, "GetSessionFilePath")

	// Non-existent session returns empty.
	got = d.GetSessionFilePath("zencoder:nonexistent")
	assert.Equal(t, "", got, "GetSessionFilePath(missing)")
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
	err := d.LinkSubagentSessions()
	require.NoError(t, err, "LinkSubagentSessions")

	sess, err := d.GetSession(context.Background(), "child")
	requireNoError(t, err, "GetSession")
	assert.Equal(t, "subagent", sess.RelationshipType, "relationship_type")
	if assert.NotNil(t, sess.ParentSessionID,
		"parent_session_id want 'parent'") {
		assert.Equal(t, "parent", *sess.ParentSessionID,
			"parent_session_id")
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
	require.Len(t, msgs, 2, "len")
	assert.False(t, msgs[0].IsSystem,
		"msgs[0].IsSystem want false")
	assert.True(t, msgs[1].IsSystem,
		"msgs[1].IsSystem want true")
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
	require.Len(t, page.Results, 1, "len")
	assert.Equal(t, "s1", page.Results[0].SessionID, "session_id")
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
	require.Len(t, page.Results, 1, "got")
	assert.Equal(t, 0, page.Results[0].Ordinal, "ordinal")
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
	assert.Nil(t, stats.EarliestSession, "earliest_session")

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
	assert.Equal(t, 2, stats.SessionCount, "session_count")
	assert.Equal(t, 2, stats.MessageCount, "message_count")
	assert.Equal(t, 2, stats.ProjectCount, "project_count")
	assert.Equal(t, 2, stats.MachineCount, "machine_count")
	require.NotNil(t, stats.EarliestSession,
		"earliest_session is nil, want non-nil")
	assert.Equal(t, early, *stats.EarliestSession,
		"earliest_session")
}

func TestStatsEarliestFallsBackToCreatedAt(t *testing.T) {
	d := testDB(t)

	// Session with NULL started_at — earliest should fall
	// back to created_at instead of being nil.
	insertSession(t, d, "s-null-start", "proj")
	insertMessages(t, d, userMsg("s-null-start", 0, "hi"))

	stats, err := d.GetStats(context.Background(), false, false)
	requireNoError(t, err, "GetStats null started_at")
	require.NotNil(t, stats.EarliestSession,
		"earliest_session nil when started_at is NULL; "+
			"should fall back to created_at")

	// Session with empty-string started_at — NULLIF should
	// treat it the same as NULL.
	insertSession(t, d, "s-empty-start", "proj", func(s *Session) {
		s.StartedAt = new("")
	})
	insertMessages(t, d, userMsg("s-empty-start", 0, "hey"))

	stats, err = d.GetStats(context.Background(), false, false)
	requireNoError(t, err, "GetStats empty started_at")
	require.NotNil(t, stats.EarliestSession,
		"earliest_session nil when started_at is ''; "+
			"should fall back to created_at")
	require.NotEmpty(t, *stats.EarliestSession,
		"earliest_session is empty string; "+
			"NULLIF should have converted '' to NULL")

	// Add a session with an explicit started_at that is
	// older than the auto-generated created_at.
	old := "2020-01-01T00:00:00Z"
	insertSession(t, d, "s-old", "proj", func(s *Session) {
		s.StartedAt = &old
	})
	insertMessages(t, d, userMsg("s-old", 0, "hello"))

	stats, err = d.GetStats(context.Background(), false, false)
	requireNoError(t, err, "GetStats with old session")
	require.NotNil(t, stats.EarliestSession, "earliest_session nil")
	assert.Equal(t, old, *stats.EarliestSession, "earliest_session")
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
	require.Len(t, projects, 2, "len")
	assert.Equal(t, "alpha", projects[0].Name,
		"alpha: %+v", projects[0])
	assert.Equal(t, 2, projects[0].SessionCount,
		"alpha: %+v", projects[0])
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
		require.Len(t, got, 1, "len")
		assert.Equal(t, "s1", got[0].ID, "got ID")
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
		require.Len(t, got, 1, "len")
		require.NotNil(t, got[0].FilePath, "file_path")
		assert.Equal(t, fp, *got[0].FilePath, "file_path")
		require.NotNil(t, got[0].FileSize, "file_size")
		assert.Equal(t, int64(4096), *got[0].FileSize, "file_size")
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
	require.Len(t, got, 2, "got candidates %v", ids)
	for _, s := range got {
		assert.NotEqual(t, "parent1", s.ID,
			"parent1 should be excluded, got candidates: %v", ids)
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
			require.Len(t, got, tt.wantN,
				"got %v", collectIDs(got))
			if tt.wantOnly != "" {
				assert.Equal(t, tt.wantOnly, got[0].ID,
					"got %v", collectIDs(got))
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
			assert.Len(t, got, tt.want, "got")
		})
	}

	// Additional check: MaxMessages=0 returns m1 specifically.
	got, err := d.FindPruneCandidates(PruneFilter{MaxMessages: new(0)})
	requireNoError(t, err, "FindPruneCandidates MaxMessages=0")
	require.Len(t, got, 1, "MaxMessages 0:")
	assert.Equal(t, "m1", got[0].ID, "MaxMessages 0")
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
	require.Len(t, got, 1, "expected 1 result")
	assert.Equal(t, "zen1", got[0].ID, "got")

	// MaxMessages=0 should NOT include zen1 (it has 1 real
	// user message).
	got, err = d.FindPruneCandidates(
		PruneFilter{MaxMessages: new(0)},
	)
	requireNoError(t, err, "FindPruneCandidates")
	require.Len(t, got, 0, "expected 0 results")
}

func TestDeleteSessions(t *testing.T) {
	d := testDB(t)

	for _, id := range []string{"s1", "s2", "s3"} {
		insertSession(t, d, id, "p")
		insertMessages(t, d, userMsg(id, 0, "msg for "+id))
	}

	stats, _ := d.GetStats(context.Background(), false, false)
	require.Equal(t, 3, stats.SessionCount, "initial sessions")
	require.Equal(t, 3, stats.MessageCount, "initial messages")

	deleted, err := d.DeleteSessions([]string{"s1", "s3"})
	requireNoError(t, err, "DeleteSessions")
	assert.Equal(t, 2, deleted, "deleted")

	requireSessionGone(t, d, "s1")
	requireSessionExists(t, d, "s2")
	requireSessionGone(t, d, "s3")

	msgs, _ := d.GetAllMessages(context.Background(), "s1")
	assert.Equal(t, 0, len(msgs), "s1 messages")
	msgs, _ = d.GetAllMessages(context.Background(), "s2")
	assert.Equal(t, 1, len(msgs), "s2 messages")

	stats, _ = d.GetStats(context.Background(), false, false)
	assert.Equal(t, 1, stats.SessionCount, "session_count")
	assert.Equal(t, 1, stats.MessageCount, "message_count")

	// Deleted sessions must be excluded.
	assert.True(t, d.IsSessionExcluded("s1"),
		"s1 should be excluded after DeleteSessions")
	assert.True(t, d.IsSessionExcluded("s3"),
		"s3 should be excluded after DeleteSessions")
	assert.False(t, d.IsSessionExcluded("s2"),
		"s2 should not be excluded (not deleted)")

	deleted, err = d.DeleteSessions(nil)
	requireNoError(t, err, "DeleteSessions empty")
	assert.Equal(t, 0, deleted, "deleted empty")
}

func TestDeleteSessionNonExistentNoGhostExclusion(t *testing.T) {
	d := testDB(t)

	// Deleting a non-existent ID should not create an exclusion.
	requireNoError(t, d.DeleteSession("bogus"), "DeleteSession bogus")
	assert.False(t, d.IsSessionExcluded("bogus"),
		"bogus should not be excluded (no row deleted)")
}

func TestDeleteSessionsMixedBatchNoGhostExclusion(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "real", "p")

	deleted, err := d.DeleteSessions([]string{"real", "bogus"})
	requireNoError(t, err, "DeleteSessions mixed")
	assert.Equal(t, 1, deleted, "deleted")
	assert.True(t, d.IsSessionExcluded("real"),
		"real should be excluded after bulk delete")
	assert.False(t, d.IsSessionExcluded("bogus"),
		"bogus should not be excluded (never existed)")
}

func TestSessionFileInfo(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p", func(s *Session) {
		s.FileSize = new(int64(1024))
		s.FileMtime = new(int64(1700000000))
		s.FileHash = new("abc123def456")
	})

	gotSize, gotMtime, ok := d.GetSessionFileInfo("s1")
	require.True(t, ok, "expected ok")
	assert.Equal(t, int64(1024), gotSize, "got size=")
	assert.Equal(t, int64(1700000000), gotMtime, "got mtime=")

	_, _, ok = d.GetSessionFileInfo("nonexistent")
	assert.False(t, ok, "expected !ok for nonexistent")
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
		require.NotNil(t, got, "expected non-nil session")
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
		require.NotNil(t, got, "expected non-nil session")
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
		assert.Nil(t, got, "expected nil session")
	})
}

func TestCursorEncodeDecode(t *testing.T) {
	d := testDB(t)
	encoded := d.EncodeCursor(SessionCursor{EndedAt: tsZero, ID: "session-1"})
	cur, err := d.DecodeCursor(encoded)
	requireNoError(t, err, "DecodeCursor")
	assert.Equal(t, tsZero, cur.EndedAt, "EndedAt")
	assert.Equal(t, "session-1", cur.ID, "ID")

	encodedWithTotal := d.EncodeCursor(SessionCursor{
		EndedAt: tsZero,
		ID:      "session-1",
		Total:   123,
	})
	cur, err = d.DecodeCursor(encodedWithTotal)
	requireNoError(t, err, "DecodeCursor with total")
	assert.Equal(t, 123, cur.Total, "Total")
}

func TestCursorTampering(t *testing.T) {
	d := testDB(t)
	// 1. Create a valid signed cursor
	original := d.EncodeCursor(SessionCursor{EndedAt: tsZero, ID: "s1", Total: 100})

	parts := strings.Split(original, ".")
	require.Len(t, parts, 2, "expected 2 parts (payload.sig)")

	payload := parts[0]
	sig := parts[1]

	// 2. Decode payload, modify Total, re-encode
	data, err := base64.RawURLEncoding.DecodeString(payload)
	requireNoError(t, err, "DecodeString payload")
	var c SessionCursor
	err = json.Unmarshal(data, &c)
	require.NoError(t, err, "Unmarshal payload")
	c.Total = 999
	tamperedData, err := json.Marshal(c)
	requireNoError(t, err, "Marshal tampered")
	tamperedPayload := base64.RawURLEncoding.EncodeToString(tamperedData)

	// 3. Construct tampered cursor with original signature
	tamperedCursor := tamperedPayload + "." + sig

	// 4. Decode should fail signature check
	_, err = d.DecodeCursor(tamperedCursor)
	require.Error(t, err, "expected error for tampered cursor, got nil")
	assert.Contains(t, err.Error(), "signature mismatch",
		"expected signature mismatch error")
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
	assert.Equal(t, "s1", got.ID, "ID")
	// Verify Total is ZEROED out
	assert.Equal(t, 0, got.Total, "Total")
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
					d.EncodeCursor(SessionCursor{
						EndedAt: tsZero,
						ID:      fmt.Sprintf("s-%d-%d", id, j),
						Total:   42,
					})
				case 2:
					encoded := d.EncodeCursor(SessionCursor{
						EndedAt: tsZero, ID: "s1",
					})
					// Decode may fail if secret rotated
					// between encode and decode; that's OK.
					_, err := d.DecodeCursor(encoded)
					if err != nil {
						assert.ErrorIs(t, err, ErrInvalidCursor,
							"unexpected DecodeCursor error")
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

	encoded := d.EncodeCursor(SessionCursor{EndedAt: tsZero, ID: "s1"})

	// Mutate the original slice — should not affect the DB.
	for i := range secret {
		secret[i] = 0
	}

	_, err := d.DecodeCursor(encoded)
	require.NoError(t, err,
		"DecodeCursor failed after caller mutated secret")
}

func TestDeleteSession(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")
	insertMessages(t, d, userMsg("s1", 0, "test"))

	err := d.DeleteSession("s1")
	require.NoError(t, err, "DeleteSession")

	requireSessionGone(t, d, "s1")

	msgs, _ := d.GetAllMessages(context.Background(), "s1")
	assert.Len(t, msgs, 0, "expected 0 messages after cascade, got")
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
			isLockErr := strings.Contains(msg, "database is locked") ||
				strings.Contains(msg, "database schema is locked") ||
				strings.Contains(msg, "SQLITE_BUSY") ||
				strings.Contains(msg, "SQLITE_LOCKED")
			if isLockErr {
				t.Logf("concurrent Open lock contention: %v", err)
			} else {
				assert.Fail(t,
					"unexpected concurrent Open error",
					err.Error())
			}
		} else {
			successes++
		}
	}
	require.NotEqual(t, 0, successes, "both concurrent Opens failed")

	// 3. Verify schema is intact
	dbCheck, err := Open(path)
	requireNoError(t, err, "re-open")
	defer dbCheck.Close()

	_, err = dbCheck.getWriter().Exec(
		"SELECT parent_session_id FROM sessions LIMIT 1",
	)
	assert.NoError(t, err, "parent_session_id column missing")
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
		require.NoError(t, rows.Scan(
			&tc.MessageID, &tc.SessionID,
			&tc.ToolName, &tc.Category,
		), "scan tool_call")
		calls = append(calls, tc)
	}
	err = rows.Err()
	require.NoError(t, err, "rows.Err")

	require.Len(t, calls, 2, "len")
	assert.Equal(t, "Read", calls[0].ToolName,
		"calls[0]: %+v", calls[0])
	assert.Equal(t, "Read", calls[0].Category,
		"calls[0]: %+v", calls[0])
	assert.Equal(t, "Grep", calls[1].ToolName,
		"calls[1]: %+v", calls[1])
	assert.Equal(t, "Grep", calls[1].Category,
		"calls[1]: %+v", calls[1])
	assert.NotEqual(t, int64(0), calls[0].MessageID,
		"message_id should be non-zero")
	assert.Equal(t, "s1", calls[0].SessionID, "session_id")
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

	err := d.DeleteSession("s1")
	require.NoError(t, err, "DeleteSession")

	var count int
	require.NoError(t, d.Reader().QueryRow(
		"SELECT COUNT(*) FROM tool_calls WHERE session_id = ?",
		"s1",
	).Scan(&count), "count tool_calls")
	assert.Equal(t, 0, count, "tool_calls count")
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
	err := d.ReplaceSessionMessages("s1", []Message{m2})
	require.NoError(t, err, "ReplaceSessionMessages")

	var names []string
	rows, err := d.Reader().Query(
		`SELECT tool_name FROM tool_calls
		 WHERE session_id = ? ORDER BY id`, "s1")
	requireNoError(t, err, "query")
	defer rows.Close()
	for rows.Next() {
		var name string
		err := rows.Scan(&name)
		require.NoError(t, err, "scan")
		names = append(names, name)
	}
	err = rows.Err()
	require.NoError(t, err, "rows.Err")

	require.Len(t, names, 2, "len")
	assert.Equal(t, "Bash", names[0], "names[0]")
	assert.Equal(t, "Write", names[1], "names[1]")
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
	err := d.ReplaceSessionMessages("s1", []Message{m2})
	require.NoError(t, err, "ReplaceSessionMessages")

	msgs, err := d.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	require.Len(t, msgs, 1, "messages len =")
	require.Len(t, msgs[0].ToolCalls, 1, "tool calls len =")
	tc := msgs[0].ToolCalls[0]
	require.Equal(t, "new result", tc.ResultContent, "result_content")
	require.Len(t, tc.ResultEvents, 1, "result events len =")
	require.Equal(t, "new result", tc.ResultEvents[0].Content, "event content")

	var count int
	err = d.Reader().QueryRow(
		"SELECT COUNT(*) FROM tool_result_events WHERE session_id = ?",
		"s1",
	).Scan(&count)
	requireNoError(t, err, "count tool_result_events")
	require.Equal(t, 1, count, "tool_result_events count")
}

func TestToolCallsNoToolCalls(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")
	insertMessages(t, d, userMsg("s1", 0, "hello"))

	var count int
	require.NoError(t, d.Reader().QueryRow(
		"SELECT COUNT(*) FROM tool_calls WHERE session_id = ?",
		"s1",
	).Scan(&count), "count")
	assert.Equal(t, 0, count, "tool_calls count")
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
		require.NoError(t, rows.Scan(
			&r.toolName, &r.tcSession, &r.msgSession,
		), "scan")
		got = append(got, r)
	}
	err = rows.Err()
	require.NoError(t, err, "rows.Err")

	require.Len(t, got, 2, "len")
	// Bash should be linked to s2
	assert.Equal(t, "Bash", got[0].toolName, "Bash toolName")
	assert.Equal(t, "s2", got[0].tcSession, "Bash tcSession")
	assert.Equal(t, "s2", got[0].msgSession, "Bash msgSession")
	// Read should be linked to s1
	assert.Equal(t, "Read", got[1].toolName, "Read toolName")
	assert.Equal(t, "s1", got[1].tcSession, "Read tcSession")
	assert.Equal(t, "s1", got[1].msgSession, "Read msgSession")
}

func TestResolveToolCallsPanicsOnLengthMismatch(t *testing.T) {
	defer func() {
		r := recover()
		require.NotNil(t, r, "expected panic, got none")
		msg, ok := r.(string)
		assert.True(t, ok && strings.Contains(msg, "resolveToolCalls"),
			"unexpected panic value: %v", r)
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
	require.True(t, toolUseID.Valid, "tool_use_id valid")
	assert.Equal(t, "toolu_abc", toolUseID.String, "tool_use_id")
	require.True(t, inputJSON.Valid, "input_json valid")
	assert.Equal(t, `{"file_path":"main.go"}`, inputJSON.String, "input_json")
	require.True(t, resultLen.Valid, "result_content_length valid")
	assert.Equal(t, int64(500), resultLen.Int64, "result_content_length")
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
	require.True(t, skillName.Valid, "skill_name valid")
	assert.Equal(t, "superpowers:brainstorming", skillName.String, "skill_name")
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
	require.Len(t, msgs, 1, "len")
	require.Len(t, msgs[0].ToolCalls, 1, "got")
	tc := msgs[0].ToolCalls[0]
	assert.Equal(t, "Skill", tc.ToolName, "ToolName")
	assert.Equal(t, "superpowers:brainstorming", tc.SkillName, "SkillName")
	assert.Equal(t, `{"skill":"superpowers:brainstorming"}`,
		tc.InputJSON, "InputJSON")
	assert.Equal(t, 42, tc.ResultContentLength, "ResultContentLength =")
}

func TestToolCallResultContent(t *testing.T) {
	database := testDB(t)
	sess := Session{
		ID: "sess-rc", Project: "p", Machine: "m", Agent: "claude",
	}
	err := database.UpsertSession(sess)
	require.NoError(t, err, "upsert")
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
	err = database.InsertMessages(msgs)
	require.NoError(t, err, "insert")
	retrieved, err := database.GetMessages(
		context.Background(), "sess-rc", 0, 10, true,
	)
	require.NoError(t, err, "get")
	require.Len(t, retrieved, 1, "expected 1 msg")
	require.Len(t, retrieved[0].ToolCalls, 1, "expected 1 tool call")
	tc := retrieved[0].ToolCalls[0]
	assert.Equal(t, "[main abc1234] Add feature\n 1 file changed",
		tc.ResultContent, "ResultContent")
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
	require.Len(t, got, total, "got")

	for i := range total {
		require.Len(t, got[i].ToolCalls, 1,
			"msg %d: tool_calls", i)
		require.Equal(t, fmt.Sprintf("toolu_%d", i),
			got[i].ToolCalls[0].ToolUseID,
			"msg %d: tool_use_id", i)
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
	require.True(t, subagentID.Valid, "subagent_session_id valid")
	assert.Equal(t, "agent-abc123", subagentID.String,
		"subagent_session_id")

	// Verify via GetMessages that it round-trips
	msgs, err := d.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "GetMessages")
	require.Len(t, msgs, 1, "len")
	require.Len(t, msgs[0].ToolCalls, 1, "got")
	tc := msgs[0].ToolCalls[0]
	assert.Equal(t, "agent-abc123", tc.SubagentSessionID, "SubagentSessionID")

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
	assert.False(t, nullSubagent.Valid,
		"expected NULL subagent_session_id for s2, got %q",
		nullSubagent.String)
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
	_, err = w.Exec("DROP TABLE IF EXISTS messages_fts")
	require.NoError(t, err, "dropping fts")
	// Also drop triggers, otherwise inserts will fail
	for _, tr := range []string{"messages_ai", "messages_ad", "messages_au"} {
		_, err := w.Exec("DROP TRIGGER IF EXISTS " + tr)
		require.NoError(t, err, "dropping trigger %s", tr)
	}

	// 2. Insert messages while FTS is missing
	insertSession(t, d1, "s1", "proj")
	insertMessages(t, d1, userMsg("s1", 0, "unique_keyword"))

	err = d1.Close()
	require.NoError(t, err, "Close 1")

	// 3. Re-open. This should detect missing FTS, create it, and backfill.
	d2, err := Open(path)
	requireNoError(t, err, "Open 2")
	defer d2.Close()

	require.True(t, d2.HasFTS(), "FTS should be available after re-open")

	// 4. Verify search finds the message
	page, err := d2.Search(context.Background(), SearchFilter{
		Query: "unique_keyword",
		Limit: 1,
	})
	requireNoError(t, err, "Search")
	require.Len(t, page.Results, 1, "len")
	assert.Equal(t, "s1", page.Results[0].SessionID, "result session_id")
}

func TestPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := Open(path)
	requireNoError(t, err, "Open")
	defer d.Close()

	assert.Equal(t, path, d.Path(), "Path()")
}

func TestOpenConfiguresWALJournalSizeLimit(t *testing.T) {
	d := testDB(t)

	var got int64
	err := d.getWriter().QueryRow("PRAGMA journal_size_limit").Scan(&got)
	requireNoError(t, err, "PRAGMA journal_size_limit")
	assert.Equal(t, int64(walJournalSizeLimitBytes), got)
}

func TestCheckpointWALTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := Open(path)
	requireNoError(t, err, "Open")
	defer d.Close()

	_, err = d.getWriter().Exec(`PRAGMA wal_autocheckpoint=0`)
	requireNoError(t, err, "disable wal autocheckpoint")

	for i := range 100 {
		insertSession(t, d, fmt.Sprintf("wal-session-%03d", i), "proj")
	}
	require.FileExists(t, path+"-wal")

	err = d.CheckpointWALTruncateWithRetry(context.Background())
	requireNoError(t, err, "CheckpointWALTruncateWithRetry")

	if info, err := os.Stat(path + "-wal"); err == nil {
		assert.LessOrEqual(t, info.Size(), int64(walJournalSizeLimitBytes))
	} else {
		require.True(t, os.IsNotExist(err), "stat wal: %v", err)
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

	err = d.Reopen()
	require.NoError(t, err, "Reopen")

	// Data should still be accessible after reopen.
	got := requireSessionExists(t, d, "s1")
	assert.Equal(t, "proj", got.Project, "project")

	msgs, err := d.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	require.Len(t, msgs, 1, "messages = %v, want [hello]", msgs)
	assert.Equal(t, "hello", msgs[0].Content,
		"messages = %v, want [hello]", msgs)

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
	err = origDB.CloseConnections()
	require.NoError(t, err, "CloseConnections")

	// Remove WAL/SHM while connections are closed.
	os.Remove(origPath + "-wal")
	os.Remove(origPath + "-shm")

	// Swap: rename temp over original.
	err = os.Rename(tempPath, origPath)
	require.NoError(t, err, "rename")
	os.Remove(tempPath + "-wal")
	os.Remove(tempPath + "-shm")

	// Reopen to pick up the new file.
	err = origDB.Reopen()
	require.NoError(t, err, "Reopen")

	// Original DB handle should now see the new data.
	requireSessionGone(t, origDB, "old-session")
	got := requireSessionExists(t, origDB, "new-session")
	assert.Equal(t, "new-proj", got.Project, "project")
}

func TestCloseConnections(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// Close connections.
	err := d.CloseConnections()
	require.NoError(t, err, "CloseConnections")

	// Queries should fail after close.
	_, err = d.GetSession(context.Background(), "s1")
	assert.Error(t, err, "expected error querying after CloseConnections")

	// Reopen should restore service.
	err = d.Reopen()
	require.NoError(t, err, "Reopen")

	// Queries should work again.
	s, err := d.GetSession(context.Background(), "s1")
	require.NoError(t, err, "GetSession after Reopen")
	assert.NotNil(t, s, "session s1 missing after Reopen")
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
	err = origDB.CloseConnections()
	require.NoError(t, err, "CloseConnections")
	for _, p := range []string{origPath, tempPath} {
		os.Remove(p + "-wal")
		os.Remove(p + "-shm")
	}
	err = os.Rename(tempPath, origPath)
	require.NoError(t, err, "rename")
	err = origDB.Reopen()
	require.NoError(t, err, "Reopen")

	// Verify swap succeeded.
	requireSessionGone(t, origDB, "old")
	got := requireSessionExists(t, origDB, "new")
	assert.Equal(t, "new-proj", got.Project, "project")
}

func TestCloseRecoveryOnRenameFail(t *testing.T) {
	dir := t.TempDir()
	origPath := filepath.Join(dir, "orig.db")

	origDB, err := Open(origPath)
	requireNoError(t, err, "Open orig")
	defer origDB.Close()
	insertSession(t, origDB, "s1", "proj")

	// Close connections as ResyncAll would.
	err = origDB.CloseConnections()
	require.NoError(t, err, "CloseConnections")

	// Simulate rename failure (temp file doesn't exist).
	nonexistent := filepath.Join(dir, "no-such-file.db")
	renameErr := os.Rename(nonexistent, origPath)
	require.Error(t, renameErr, "expected rename to fail")

	// Recovery: reopen original to restore service.
	err = origDB.Reopen()
	require.NoError(t, err, "recovery Reopen")

	// Data should still be accessible.
	s, err := origDB.GetSession(context.Background(), "s1")
	require.NoError(t, err, "GetSession after recovery")
	assert.NotNil(t, s, "session s1 missing after recovery Reopen")
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
		err := d.Reopen()
		require.NoError(t, err, "Reopen")
	}

	cancel()
	wg.Wait()

	assert.Equal(t, int64(0), readErrors.Load(),
		"got %d concurrent read errors", readErrors.Load())
}

func TestExportedReaderAcquiredBeforeReopenStaysUsable(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	reader := d.Reader()
	for range 2 {
		err := d.Reopen()
		require.NoError(t, err, "Reopen")
	}

	var id string
	err := reader.QueryRow(
		"SELECT id FROM sessions WHERE id = ?", "s1",
	).Scan(&id)
	require.NoError(t, err, "query with pre-reopen reader handle")
	require.Equal(t, "s1", id, "id")
}

func TestReopenDoesNotBlockNewReadsWhileClosingRetiredPool(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	blockingPool, closeStarted, releaseClose := openBlockingCloseDB(t)
	d.connMu.Lock()
	d.retired = append(d.retired, blockingPool)
	d.connMu.Unlock()

	reopenDone := make(chan error, 1)
	go func() {
		reopenDone <- d.Reopen()
	}()

	select {
	case <-closeStarted:
	case err := <-reopenDone:
		require.Failf(t, "Reopen finished early",
			"Reopen finished before blocking close: %v", err)
	case <-time.After(2 * time.Second):
		require.Fail(t, "Reopen did not start closing retired pool")
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := d.GetSession(context.Background(), "s1")
		readDone <- err
	}()

	select {
	case err := <-readDone:
		require.NoError(t, err, "new read while closing retired pool")
	case <-time.After(200 * time.Millisecond):
		require.Fail(t,
			"new read blocked while Reopen closed a retired pool")
	}

	releaseClose()
	err := <-reopenDone
	require.NoError(t, err, "Reopen")
}

func TestRepeatedReopenBoundsRetiredPools(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// Reopen many times; retired pools from earlier rounds
	// should be closed by subsequent reopens, keeping only
	// the most recent pair alive.
	for range 20 {
		err := d.Reopen()
		require.NoError(t, err, "Reopen")
	}

	// After 20 reopens the retired slice should hold at most
	// the last pair (2 entries), not 40.
	d.mu.Lock()
	n := len(d.retired)
	d.mu.Unlock()
	assert.LessOrEqual(t, n, 2,
		"retired pool count = %d, want <= 2", n)

	// Data should still be readable.
	s, err := d.GetSession(context.Background(), "s1")
	require.NoError(t, err, "GetSession")
	assert.NotNil(t, s, "session s1 missing after repeated Reopen")
}

func TestCloseAfterCloseConnectionsReopen(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// CloseConnections + Reopen is the normal resync lifecycle.
	err := d.CloseConnections()
	require.NoError(t, err, "CloseConnections")
	err = d.Reopen()
	require.NoError(t, err, "Reopen")

	// Close should succeed without "database is closed" errors
	// from double-closing the pools that CloseConnections
	// already closed.
	err = d.Close()
	require.NoError(t, err, "Close")
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
	err = dstDB.CopyInsightsFrom(srcPath)
	require.NoError(t, err, "CopyInsightsFrom")

	// Verify insights were copied.
	insights, err := dstDB.ListInsights(
		context.Background(), InsightFilter{},
	)
	requireNoError(t, err, "ListInsights")
	require.Len(t, insights, 1, "len")
	assert.Equal(t, "test insight content", insights[0].Content, "content")
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
	require.Equal(t, 1, count, "expected 1 orphaned session, got")

	// s2 should now exist in dst.
	s, err := dstDB.GetSession(
		context.Background(), "s2",
	)
	requireNoError(t, err, "GetSession s2")
	require.NotNil(t, s, "orphaned session s2 not found in dst")
	assert.Equal(t, "codex", s.Agent, "s2 agent")

	// s2 messages should be copied.
	ctx := context.Background()
	msgs, err := dstDB.GetMessages(ctx, "s2", 0, 100, true)
	requireNoError(t, err, "GetMessages s2")
	require.Len(t, msgs, 1, "expected 1 message for s2, got")
	assert.Equal(t, "hello from s2", msgs[0].Content, "s2 message content")

	// s1 should still exist and not be duplicated.
	s1msgs, err := dstDB.GetMessages(ctx, "s1", 0, 100, true)
	requireNoError(t, err, "GetMessages s1")
	require.Len(t, s1msgs, 2, "expected 2 messages for s1, got")

	// Tool calls for s1 should NOT be copied (s1 exists in
	// dst, so it's not orphaned). Only verify s2's tool_calls
	// aren't present (s2 had no tool_calls on ordinal 0).
	var tcCount int
	err = dstDB.getReader().QueryRow(
		"SELECT count(*) FROM tool_calls " +
			"WHERE session_id = 's2'",
	).Scan(&tcCount)
	requireNoError(t, err, "count s2 tool_calls")
	assert.Equal(t, 0, tcCount,
		"expected 0 tool_calls for s2, got %d", tcCount)
}

// TestCopyOrphanedDataFrom_SkipsStaleCodexForkRows covers the
// dataVersion 40 upgrade path (#643): a pre-fix DB stored a forked
// Codex rollout under the replayed parent's id with double-counted
// totals. After the fresh sync reparses the same file under the
// fork's own id, the stale parent-ID row must not be resurrected as
// an orphan — but genuine Codex orphans (file gone) and SQLite-backed
// agents that share a file_path across sessions must still be copied.
func TestCopyOrphanedDataFrom_SkipsStaleCodexForkRows(t *testing.T) {
	dir := t.TempDir()
	forkFile := filepath.Join(dir, "fork.jsonl")
	goneFile := filepath.Join(dir, "gone.jsonl")
	sharedDB := filepath.Join(dir, "chats.db")

	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	require.NoError(t, err, "Open src")
	// Stale pre-fix row: the fork file stored under the parent's id.
	insertSession(t, srcDB, "codex:parent-1", "proj", func(s *Session) {
		s.Agent = "codex"
		s.FilePath = &forkFile
	})
	// Genuine Codex orphan: its file no longer exists.
	insertSession(t, srcDB, "codex:gone-1", "proj", func(s *Session) {
		s.Agent = "codex"
		s.FilePath = &goneFile
	})
	// SQLite-backed agent: many sessions share one file_path. An id
	// missing from the fresh parse is an evicted chat, not a stale
	// duplicate, and must survive as an orphan.
	insertSession(t, srcDB, "piebald:old-chat", "proj", func(s *Session) {
		s.Agent = "piebald"
		s.FilePath = &sharedDB
	})
	srcDB.Close()

	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()
	// The fork file reparsed under the fork's own id.
	insertSession(t, dstDB, "codex:fork-1", "proj", func(s *Session) {
		s.Agent = "codex"
		s.FilePath = &forkFile
	})
	insertSession(t, dstDB, "piebald:new-chat", "proj", func(s *Session) {
		s.Agent = "piebald"
		s.FilePath = &sharedDB
	})

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	require.NoError(t, err, "CopyOrphanedDataFrom")
	assert.Equal(t, 2, count, "gone-1 and old-chat are the only orphans")

	ctx := context.Background()
	stale, err := dstDB.GetSession(ctx, "codex:parent-1")
	require.NoError(t, err, "GetSession codex:parent-1")
	assert.Nil(t, stale,
		"stale parent-ID row for a reparsed fork file must not be copied")

	gone, err := dstDB.GetSession(ctx, "codex:gone-1")
	require.NoError(t, err, "GetSession codex:gone-1")
	assert.NotNil(t, gone, "genuine codex orphan must be copied")

	evicted, err := dstDB.GetSession(ctx, "piebald:old-chat")
	require.NoError(t, err, "GetSession piebald:old-chat")
	assert.NotNil(t, evicted,
		"evicted chat sharing a file_path must be copied")
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
	require.Equal(t, 0, count,
		"expected 0 orphaned sessions, got %d", count)
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
	require.Equal(t, 1, count, "expected 1 orphaned, got")

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
	assert.Equal(t, "Bash", toolName, "tool_name")
	assert.Equal(t, "tu_123", toolUseID, "tool_use_id")

	// Verify the message_id FK is valid.
	var ordinal int
	err = dstDB.getReader().QueryRow(
		"SELECT ordinal FROM messages WHERE id = ?", msgID,
	).Scan(&ordinal)
	requireNoError(t, err, "verify FK")
	assert.Equal(t, 1, ordinal, "tool_call message ordinal")
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
	require.Equal(t, 1, count, "expected 1 orphaned, got")

	msgs, err := dstDB.GetAllMessages(context.Background(), "s1")
	requireNoError(t, err, "GetAllMessages")
	require.Len(t, msgs, 2, "messages len =")
	require.Len(t, msgs[1].ToolCalls, 1, "tool calls len =")
	tc := msgs[1].ToolCalls[0]
	require.Equal(t, "Finished successfully", tc.ResultContent, "result_content")
	require.Len(t, tc.ResultEvents, 1, "result events len =")
	require.Equal(t, "wait_output", tc.ResultEvents[0].Source, "event source")
	require.Equal(t, "codex:agent-1",
		tc.ResultEvents[0].SubagentSessionID, "subagent_session_id")
}

func TestCopyTrashedDataFromPreservesPins(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj")
	insertMessages(t, srcDB,
		userMsg("s1", 0, "keep this pinned"),
		asstMsg("s1", 1, "reply"),
	)
	srcMsgs, err := srcDB.GetAllMessages(ctx, "s1")
	requireNoError(t, err, "GetAllMessages src")
	note := "important"
	pinID, err := srcDB.PinMessage("s1", srcMsgs[0].ID, &note)
	require.NoError(t, err, "PinMessage src: id=%d", pinID)
	require.NotZero(t, pinID, "PinMessage src returned id=0")
	requireNoError(t, srcDB.SoftDeleteSession("s1"), "SoftDelete src")
	srcDB.Close()

	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyTrashedDataFrom(srcPath)
	requireNoError(t, err, "CopyTrashedDataFrom")
	require.Equal(t, 1, count, "copied trashed sessions")

	pins, err := dstDB.ListPinnedMessages(ctx, "s1", "")
	requireNoError(t, err, "ListPinnedMessages")
	require.Len(t, pins, 1, "pins copied =")
	require.Equal(t, 0, pins[0].Ordinal, "pin ordinal")
	require.NotNil(t, pins[0].Note, "pin note nil")
	require.Equal(t, note, *pins[0].Note, "pin note")

	var messageContent string
	requireNoError(t, dstDB.getReader().QueryRow(
		"SELECT content FROM messages WHERE id = ?",
		pins[0].MessageID,
	).Scan(&messageContent), "query pinned message")
	require.Equal(t, "keep this pinned", messageContent, "pinned content")
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
	require.Error(t, err, "expected error from corrupted source")

	// The session insert must have been rolled back — no
	// partial data in the destination.
	page, err := dstDB.ListSessions(
		context.Background(),
		SessionFilter{Limit: 100},
	)
	requireNoError(t, err, "list sessions")
	require.Empty(t, page.Sessions,
		"expected 0 sessions after failed copy, got %d",
		len(page.Sessions))
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
	require.Equal(t, 1, count, "expected 1 orphaned, got")

	// Verify is_system was preserved.
	msgs, err := dstDB.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "GetMessages")
	require.Len(t, msgs, 2, "expected 2 messages")
	assert.True(t, msgs[0].IsSystem, "ordinal 0: is_system should be true")
	assert.False(t, msgs[1].IsSystem, "ordinal 1: is_system should be false")
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
	require.Equal(t, 1, count, "expected 1 orphaned, got")

	// Message should be copied with is_system defaulting to
	// false.
	msgs, err := dstDB.GetMessages(
		context.Background(), "s1", 0, 100, true,
	)
	requireNoError(t, err, "GetMessages")
	require.Len(t, msgs, 1, "expected 1 message")
	assert.False(t, msgs[0].IsSystem, "is_system should default to false")
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
	require.Equal(t, 1, count, "expected 1 orphaned, got")

	// Session token metadata must survive the copy.
	ctx := context.Background()
	s, err := dstDB.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession s1")
	require.NotNil(t, s, "orphaned session s1 not found")
	assert.Equal(t, 5000, s.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 120000, s.PeakContextTokens, "PeakContextTokens")
	assert.True(t, s.HasTotalOutputTokens, "HasTotalOutputTokens should be true")
	assert.True(t, s.HasPeakContextTokens, "HasPeakContextTokens should be true")

	// Message token metadata must survive the copy.
	msgs, err := dstDB.GetMessages(ctx, "s1", 0, 100, true)
	requireNoError(t, err, "GetMessages s1")
	require.Len(t, msgs, 1, "expected 1 message")
	m := msgs[0]
	assert.Equal(t, "claude-opus-4-20250514", m.Model, "Model")
	assert.Equal(t, 80000, m.ContextTokens, "ContextTokens")
	assert.Equal(t, 500, m.OutputTokens, "OutputTokens")
	assert.True(t, m.HasContextTokens, "HasContextTokens should be true")
	assert.True(t, m.HasOutputTokens, "HasOutputTokens should be true")
	assert.NotEmpty(t, m.TokenUsage, "TokenUsage should be preserved")
}

func TestCopyOrphanedDataFrom_SessionName(t *testing.T) {
	dir := t.TempDir()

	// Source DB: one session with an agent-provided session_name.
	srcPath := filepath.Join(dir, "old.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	insertSession(t, srcDB, "s1", "proj", func(s *Session) {
		s.SessionName = Ptr("Agent Generated Name")
	})
	insertMessages(t, srcDB, userMsg("s1", 0, "hello"))
	srcDB.Close()

	// Empty destination.
	dstPath := filepath.Join(dir, "new.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	count, err := dstDB.CopyOrphanedDataFrom(srcPath)
	requireNoError(t, err, "CopyOrphanedDataFrom")
	require.Equal(t, 1, count, "expected 1 orphaned session")

	// session_name must survive the orphan copy (visible via COALESCE).
	ctx := context.Background()
	s, err := dstDB.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession s1")
	require.NotNil(t, s, "orphaned session s1 not found in dst")
	require.NotNil(t, s.DisplayName, "DisplayName should be populated via COALESCE")
	assert.Equal(t, "Agent Generated Name", *s.DisplayName, "DisplayName via COALESCE(display_name, session_name)")
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
	require.NoError(t, err, "GetAgents")

	for _, a := range agents {
		assert.NotEmpty(t, a.Name, "GetAgents returned empty agent name")
	}
	assert.Len(t, agents, 2, "len")
}

func TestGetAgentsEmptyResultSerializesAsArray(t *testing.T) {
	d := testDB(t)

	agents, err := d.GetAgents(context.Background(), false, false)
	require.NoError(t, err, "GetAgents")
	require.NotNil(t, agents, "GetAgents returned nil, want empty slice")
	assert.Empty(t, agents, "len")

	b, err := json.Marshal(agents)
	require.NoError(t, err, "json.Marshal")
	assert.Equal(t, "[]", string(b), "JSON")
}

func TestStarSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")

	// Star existing session.
	ok, err := d.StarSession("s1")
	require.NoError(t, err, "StarSession")
	require.True(t, ok, "StarSession should succeed for existing session")

	// Idempotent re-star — should still return true (session exists).
	ok, err = d.StarSession("s1")
	require.NoError(t, err, "re-star")
	assert.True(t, ok, "re-star should return true (session exists, already starred)")
	// This is acceptable — the session is already starred.

	// Listed.
	ids, err := d.ListStarredSessionIDs(ctx)
	require.NoError(t, err, "ListStarredSessionIDs")
	assert.Equal(t, []string{"s1"}, ids, "listed = %v, want [s1]", ids)

	// Unstar.
	err = d.UnstarSession("s1")
	require.NoError(t, err, "UnstarSession")
	ids, err = d.ListStarredSessionIDs(ctx)
	require.NoError(t, err, "ListStarredSessionIDs after unstar")
	assert.Empty(t, ids, "listed after unstar = %v, want []", ids)

	// Star non-existent session returns false (no FK error).
	ok, err = d.StarSession("nonexistent")
	require.NoError(t, err, "StarSession nonexistent")
	assert.False(t, ok, "StarSession should return false for non-existent session")
}

func TestBulkStarSessions(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()
	insertSession(t, d, "s1", "proj")
	insertSession(t, d, "s2", "proj")

	// Bulk star with mix of valid and invalid IDs.
	err := d.BulkStarSessions([]string{"s1", "s2", "nonexistent"})
	require.NoError(t, err, "BulkStarSessions")

	ids, err := d.ListStarredSessionIDs(ctx)
	require.NoError(t, err, "ListStarredSessionIDs")
	assert.Equal(t, 2, len(ids), "listed")
}

func TestRestoreSession(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "p")

	t.Run("restore non-trashed returns 0", func(t *testing.T) {
		n, err := d.RestoreSession("s1")
		requireNoError(t, err, "RestoreSession")
		assert.Equal(t, int64(0), n, "rows affected")
	})

	t.Run("restore non-existent returns 0", func(t *testing.T) {
		n, err := d.RestoreSession("no-such-session")
		requireNoError(t, err, "RestoreSession")
		assert.Equal(t, int64(0), n, "rows affected")
	})

	t.Run("restore trashed returns 1", func(t *testing.T) {
		requireNoError(t, d.SoftDeleteSession("s1"), "SoftDeleteSession")

		// Should not appear in filtered list queries.
		f := filterWith(func(f *SessionFilter) {})
		page, err := d.ListSessions(ctx, f)
		requireNoError(t, err, "ListSessions")
		require.Empty(t, page.Sessions,
			"soft-deleted session should not appear in list")

		// Should appear in trash list.
		trashed, err := d.ListTrashedSessions(ctx)
		requireNoError(t, err, "ListTrashedSessions")
		require.Len(t, trashed, 1, "trash count =")

		n, err := d.RestoreSession("s1")
		requireNoError(t, err, "RestoreSession")
		assert.Equal(t, int64(1), n, "rows affected")

		// Should appear in list again.
		page, err = d.ListSessions(ctx, f)
		requireNoError(t, err, "ListSessions")
		require.Len(t, page.Sessions, 1,
			"restored session should appear in list")
	})
}

func TestDeleteSessionExcludes(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")

	err := d.DeleteSession("s1")
	require.NoError(t, err, "DeleteSession")

	// Session should be gone.
	requireSessionGone(t, d, "s1")

	// Session should be excluded.
	assert.True(t, d.IsSessionExcluded("s1"),
		"session should be excluded after permanent delete")

	// UpsertSession should return ErrSessionExcluded.
	err = d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "m", Agent: "claude",
	})
	require.ErrorIs(t, err, ErrSessionExcluded,
		"UpsertSession = %v, want ErrSessionExcluded", err)
	requireSessionGone(t, d, "s1")
}

func TestVibeCanonicalDeleteExcludesFallbackAlias(t *testing.T) {
	cases := []struct {
		name       string
		id         string
		filePath   string
		fallbackID string
		delete     func(*testing.T, *DB, string) error
	}{
		{
			name:       "single delete",
			id:         "vibe:abc123def-0000-0000-0000-000000000000",
			filePath:   "/tmp/vibe/session_20260616_083518_abc123/messages.jsonl",
			fallbackID: "vibe:session_20260616_083518_abc123",
			delete: func(t *testing.T, d *DB, id string) error {
				return d.DeleteSession(id)
			},
		},
		{
			name:       "remote single delete",
			id:         "host~vibe:abc123def-0000-0000-0000-000000000000",
			filePath:   "host:/remote/vibe/session_20260616_083518_abc123/messages.jsonl",
			fallbackID: "host~vibe:session_20260616_083518_abc123",
			delete: func(t *testing.T, d *DB, id string) error {
				return d.DeleteSession(id)
			},
		},
		{
			name:       "delete if trashed",
			id:         "vibe:abc123def-0000-0000-0000-000000000001",
			filePath:   "/tmp/vibe/session_20260616_083518_def456/messages.jsonl",
			fallbackID: "vibe:session_20260616_083518_def456",
			delete: func(t *testing.T, d *DB, id string) error {
				requireNoError(t, d.SoftDeleteSession(id), "SoftDeleteSession")
				n, err := d.DeleteSessionIfTrashed(id)
				require.Equal(t, int64(1), n, "DeleteSessionIfTrashed rows")
				return err
			},
		},
		{
			name:       "batch delete",
			id:         "vibe:abc123def-0000-0000-0000-000000000002",
			filePath:   "/tmp/vibe/session_20260616_083518_ghi789/messages.jsonl",
			fallbackID: "vibe:session_20260616_083518_ghi789",
			delete: func(t *testing.T, d *DB, id string) error {
				n, err := d.DeleteSessions([]string{id})
				require.Equal(t, 1, n, "DeleteSessions rows")
				return err
			},
		},
		{
			name:       "empty trash",
			id:         "vibe:abc123def-0000-0000-0000-000000000003",
			filePath:   "/tmp/vibe/session_20260616_083518_jkl012/messages.jsonl",
			fallbackID: "vibe:session_20260616_083518_jkl012",
			delete: func(t *testing.T, d *DB, id string) error {
				requireNoError(t, d.SoftDeleteSession(id), "SoftDeleteSession")
				n, err := d.EmptyTrash()
				require.Equal(t, 1, n, "EmptyTrash rows")
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			insertSession(t, d, tc.id, "p", func(s *Session) {
				s.Agent = "vibe"
				s.FilePath = &tc.filePath
			})

			requireNoError(t, tc.delete(t, d, tc.id), "delete")

			assert.True(t, d.IsSessionExcluded(tc.id),
				"canonical ID should be excluded")
			assert.True(t, d.IsSessionExcluded(tc.fallbackID),
				"fallback alias should be excluded")
			requireSessionGone(t, d, tc.id)
		})
	}
}

func TestUpsertSessionTrashedReturnsErrSessionTrashed(t *testing.T) {
	d := testDB(t)

	insertSession(t, d, "s1", "p")
	requireNoError(t, d.SoftDeleteSession("s1"), "SoftDeleteSession")

	err := d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "m", Agent: "claude",
	})
	require.ErrorIs(t, err, ErrSessionTrashed,
		"UpsertSession = %v, want ErrSessionTrashed", err)
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
	assert.Equal(t, 2, n, "EmptyTrash deleted")

	// Both should be excluded.
	assert.True(t, d.IsSessionExcluded("s1"), "s1 should be excluded")
	assert.True(t, d.IsSessionExcluded("s2"), "s2 should be excluded")

	// s3 should NOT be excluded.
	assert.False(t, d.IsSessionExcluded("s3"),
		"s3 should not be excluded")

	// Re-upsert should return ErrSessionExcluded.
	err = d.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "m", Agent: "claude",
	})
	require.ErrorIs(t, err, ErrSessionExcluded,
		"UpsertSession s1 = %v, want ErrSessionExcluded", err)
	requireSessionGone(t, d, "s1")

	// s3 should still be upsertable.
	s, _ := d.GetSession(context.Background(), "s3")
	assert.NotNil(t, s, "s3 should still be visible")
}

func TestCopyExcludedSessionsFrom(t *testing.T) {
	dir := t.TempDir()

	// Source DB with excluded sessions.
	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")

	insertSession(t, srcDB, "s1", "p")
	requireNoError(t, srcDB.DeleteSession("s1"), "DeleteSession")
	require.True(t, srcDB.IsSessionExcluded("s1"),
		"s1 should be excluded in src")
	srcDB.Close()

	// Destination DB (empty, simulates fresh resync DB).
	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()

	// Copy excluded sessions.
	err = dstDB.CopyExcludedSessionsFrom(srcPath)
	require.NoError(t, err, "CopyExcludedSessionsFrom")

	// s1 should be excluded in destination.
	assert.True(t, dstDB.IsSessionExcluded("s1"),
		"s1 should be excluded in dst after copy")

	// Upserting s1 should be rejected.
	err = dstDB.UpsertSession(Session{
		ID: "s1", Project: "p", Machine: "m", Agent: "claude",
	})
	assert.ErrorIs(t, err, ErrSessionExcluded,
		"UpsertSession = %v, want ErrSessionExcluded", err)
}

func TestCopySyncStateFrom_NoSourceTable(t *testing.T) {
	dir := t.TempDir()

	// Source DB with no tables (legacy DB shape missing pg_sync_state).
	srcPath := filepath.Join(dir, "src.db")
	srcConn, err := sql.Open("sqlite3", srcPath)
	require.NoError(t, err, "open src")
	require.NoError(t, srcConn.Close(), "close src")

	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()

	// Seed a marker that should be preserved when source has none.
	require.NoError(t, dstDB.SetSyncState(
		"pg_push_marker_id", "marker-123",
	), "seed destination sync state")

	// Copy should be a no-op for legacy source and return nil.
	err = dstDB.CopySyncStateFrom(srcPath)
	require.NoError(t, err, "CopySyncStateFrom")

	got, err := dstDB.GetSyncState("pg_push_marker_id")
	require.NoError(t, err, "GetSyncState")
	assert.Equal(t, "marker-123", got)
}

func TestCopySyncStateFrom_OnlyCopiesDurablePGKeys(t *testing.T) {
	dir := t.TempDir()

	srcPath := filepath.Join(dir, "src.db")
	srcDB, err := Open(srcPath)
	require.NoError(t, err, "Open src")
	require.NoError(t, srcDB.SetSyncState("pg_push_marker_id", "marker-123"),
		"seed source marker")
	require.NoError(t, srcDB.SetSyncState("last_sync_started_at", "old-start"),
		"seed source started")
	require.NoError(t, srcDB.SetSyncState("last_sync_finished_at", "old-finish"),
		"seed source finished")
	require.NoError(t, srcDB.Close(), "Close src")

	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()
	require.NoError(t, dstDB.SetSyncState("last_sync_started_at", "new-start"),
		"seed destination started")
	require.NoError(t, dstDB.SetSyncState("last_sync_finished_at", "new-finish"),
		"seed destination finished")

	err = dstDB.CopySyncStateFrom(srcPath)
	require.NoError(t, err, "CopySyncStateFrom")

	gotMarker, err := dstDB.GetSyncState("pg_push_marker_id")
	require.NoError(t, err, "GetSyncState pg_push_marker_id")
	assert.Equal(t, "marker-123", gotMarker)

	gotStarted, err := dstDB.GetSyncState("last_sync_started_at")
	require.NoError(t, err, "GetSyncState last_sync_started_at")
	assert.Equal(t, "new-start", gotStarted)

	gotFinished, err := dstDB.GetSyncState("last_sync_finished_at")
	require.NoError(t, err, "GetSyncState last_sync_finished_at")
	assert.Equal(t, "new-finish", gotFinished)
}

func TestCopySyncStateFrom_PropagatesErrors(t *testing.T) {
	dir := t.TempDir()

	// Source is not a valid SQLite database, so probing state fails.
	srcPath := filepath.Join(dir, "src.db")
	require.NoError(t, os.WriteFile(srcPath, []byte("not sqlite"), 0o600),
		"write invalid source")

	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	require.NoError(t, err, "Open dst")
	defer dstDB.Close()
	require.NoError(t, dstDB.SetSyncState("pg_push_marker_id", "safe"),
		"seed destination sync state")

	err = dstDB.CopySyncStateFrom(srcPath)
	require.Error(t, err, "CopySyncStateFrom")
	require.ErrorContains(t, err, "attaching source db")

	got, err := dstDB.GetSyncState("pg_push_marker_id")
	require.NoError(t, err, "GetSyncState")
	assert.Equal(t, "safe", got)
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
	require.NoError(t, err, "PinMessage in src: id=%d", pinID)
	require.NotZero(t, pinID, "PinMessage in src returned id=0")
	// Star the session.
	_, err = srcDB.getWriter().Exec(
		"INSERT INTO starred_sessions (session_id) VALUES (?)", "s1",
	)
	require.NoError(t, err, "star session in src")
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
	assert.Nil(t, s.DisplayName, "display_name before")
	assert.Nil(t, s.DeletedAt, "deleted_at before")
	pins, err := dstDB.ListPinnedMessages(ctx, "s1", "")
	requireNoError(t, err, "ListPins before")
	assert.Equal(t, 0, len(pins), "pins before")
	var starCount int
	requireNoError(t, dstDB.getReader().QueryRow(
		"SELECT count(*) FROM starred_sessions WHERE session_id = ?", "s1",
	).Scan(&starCount), "count stars before")
	assert.Equal(t, 0, starCount, "stars before")

	// Copy metadata.
	err = dstDB.CopySessionMetadataFrom(srcPath)
	require.NoError(t, err, "CopySessionMetadataFrom")

	// After copy: metadata, pin, and star should be merged.
	// Use GetSessionFull because deleted_at was copied, so
	// GetSession (which filters deleted_at IS NULL) returns nil.
	sf, err := dstDB.GetSessionFull(ctx, "s1")
	requireNoError(t, err, "GetSessionFull after")
	require.NotNil(t, sf, "session should exist after metadata copy")
	require.NotNil(t, sf.DisplayName, "display_name nil")
	assert.Equal(t, dn, *sf.DisplayName, "display_name")
	assert.NotNil(t, sf.DeletedAt, "deleted_at should be set after copy")
	pins, err = dstDB.ListPinnedMessages(ctx, "s1", "")
	requireNoError(t, err, "ListPins after")
	require.Len(t, pins, 1, "pins after =")
	assert.Equal(t, 1, pins[0].Ordinal, "pin ordinal")
	requireNoError(t, dstDB.getReader().QueryRow(
		"SELECT count(*) FROM starred_sessions WHERE session_id = ?", "s1",
	).Scan(&starCount), "count stars after")
	assert.Equal(t, 1, starCount, "stars after")
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
	require.NotNil(t, sf, "session should exist")
	require.NotNil(t, sf.DisplayName, "display_name nil")
	assert.Equal(t, name, *sf.DisplayName, "display_name")
	assert.NotNil(t, sf.DeletedAt, "deleted_at should be set from source")
}

func TestCopySessionMetadataPreservesWorktreeProjectMappings(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	srcPath := filepath.Join(dir, "src.db")
	srcPrefix := filepath.Join(dir, "src.worktrees")
	dstPrefix := filepath.Join(dir, "dst.worktrees")
	srcDB, err := Open(srcPath)
	requireNoError(t, err, "Open src")
	_, err = srcDB.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: srcPrefix, Project: "src-repo", Enabled: true,
	})
	requireNoError(t, err, "CreateWorktreeProjectMapping src")
	_, err = srcDB.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: dstPrefix, Project: "src-conflict", Enabled: true,
	})
	requireNoError(t, err, "CreateWorktreeProjectMapping conflict")
	srcDB.Close()

	dstPath := filepath.Join(dir, "dst.db")
	dstDB, err := Open(dstPath)
	requireNoError(t, err, "Open dst")
	defer dstDB.Close()
	_, err = dstDB.CreateWorktreeProjectMapping(ctx, WorktreeProjectMapping{
		Machine: "laptop", PathPrefix: dstPrefix, Project: "dst-repo", Enabled: true,
	})
	requireNoError(t, err, "CreateWorktreeProjectMapping dst")

	requireNoError(t, dstDB.CopySessionMetadataFrom(srcPath), "CopySessionMetadataFrom")
	requireNoError(t, dstDB.CopySessionMetadataFrom(srcPath), "CopySessionMetadataFrom again")

	got, err := dstDB.ListWorktreeProjectMappings(ctx, "laptop")
	requireNoError(t, err, "ListWorktreeProjectMappings")
	require.Len(t, got, 2, "mapping count = %d, want 2: %+v", len(got), got)
	projects := map[string]string{}
	for _, m := range got {
		projects[m.PathPrefix] = m.Project
	}
	require.Equal(t, "src_repo", projects[srcPrefix], "source mapping project")
	require.Equal(t, "src_conflict", projects[dstPrefix], "destination mapping project")
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
	require.NotNil(t, sf, "session should exist")
	assert.Nil(t, sf.DisplayName,
		"display_name = %v, want nil (clear preserved)",
		sf.DisplayName)
	assert.Nil(t, sf.DeletedAt,
		"deleted_at = %v, want nil (restore preserved)",
		sf.DeletedAt)
}

func TestPinMessageIdempotent(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, userMsg("s1", 1, "hello"))

	// First pin should succeed.
	id1, err := d.PinMessage("s1", 1, nil)
	require.NoError(t, err, "first PinMessage: id=%d", id1)
	require.NotZero(t, id1, "first PinMessage returned id=0")

	// Idempotent re-pin with same note must not return 0.
	id2, err := d.PinMessage("s1", 1, nil)
	require.NoError(t, err, "idempotent PinMessage err")
	require.NotZero(t, id2,
		"idempotent PinMessage returned id=0; should return existing id")
	assert.Equal(t, id1, id2,
		"idempotent PinMessage id=%d, want %d", id2, id1)

	// Re-pin with different note should succeed and return same id.
	note := "important"
	id2b, err := d.PinMessage("s1", 1, &note)
	require.NoError(t, err, "re-pin with note err")
	assert.Equal(t, id1, id2b,
		"re-pin with note id=%d, want %d", id2b, id1)

	// Pin with wrong session should return 0.
	id3, err := d.PinMessage("nonexistent", 1, nil)
	require.NoError(t, err, "wrong-session PinMessage err")
	assert.Equal(t, int64(0), id3, "wrong-session PinMessage id=")
}

func TestDeleteSessionIfTrashed(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")

	// Delete a non-trashed session should return 0.
	n, err := d.DeleteSessionIfTrashed("s1")
	require.NoError(t, err, "DeleteSessionIfTrashed non-trashed")
	assert.Equal(t, int64(0), n, "non-trashed: rows=")

	// Soft-delete, then permanent delete should succeed.
	requireNoError(t, d.SoftDeleteSession("s1"), "soft delete")
	n, err = d.DeleteSessionIfTrashed("s1")
	require.NoError(t, err, "DeleteSessionIfTrashed trashed")
	assert.Equal(t, int64(1), n, "trashed: rows=")

	// Session should be gone.
	ctx := context.Background()
	s, err := d.GetSessionFull(ctx, "s1")
	require.NoError(t, err, "GetSessionFull after delete")
	assert.Nil(t, s, "session should be nil after permanent delete")

	// Session should be excluded.
	assert.True(t, d.IsSessionExcluded("s1"),
		"session should be in excluded_sessions")

	// Non-existent session should return 0.
	n, err = d.DeleteSessionIfTrashed("nonexistent")
	require.NoError(t, err, "DeleteSessionIfTrashed nonexistent")
	assert.Equal(t, int64(0), n, "nonexistent: rows=")
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
	require.Len(t, projects, 2, "projects before trash:")

	agents, err := d.GetAgents(ctx, false, false)
	requireNoError(t, err, "GetAgents before trash")
	require.Len(t, agents, 2, "agents before trash:")

	machines, err := d.GetMachines(ctx, false, false)
	requireNoError(t, err, "GetMachines before trash")
	require.Len(t, machines, 2, "machines before trash:")

	// Soft-delete s2: its project/agent/machine should disappear.
	requireNoError(t, d.SoftDeleteSession("s2"), "soft delete s2")

	projects, err = d.GetProjects(ctx, false, false)
	requireNoError(t, err, "GetProjects after trash")
	require.Len(t, projects, 1, "projects after trash:")
	assert.Equal(t, "proj-a", projects[0].Name, "project name")

	agents, err = d.GetAgents(ctx, false, false)
	requireNoError(t, err, "GetAgents after trash")
	require.Len(t, agents, 1, "agents after trash:")
	assert.Equal(t, "claude", agents[0].Name, "agent name")

	machines, err = d.GetMachines(ctx, false, false)
	requireNoError(t, err, "GetMachines after trash")
	require.Len(t, machines, 1, "machines after trash:")
	assert.Equal(t, "laptop", machines[0], "machine")
}

func TestGetSessionExcludesTrashed(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	insertSession(t, d, "s1", "proj")

	// Before trashing: GetSession returns the session.
	s, err := d.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession before trash")
	require.NotNil(t, s, "session should exist before trash")

	// After trashing: GetSession returns nil.
	requireNoError(t, d.SoftDeleteSession("s1"), "soft delete")
	s, err = d.GetSession(ctx, "s1")
	requireNoError(t, err, "GetSession after trash")
	assert.Nil(t, s, "GetSession should return nil for trashed session")

	// GetSessionFull still returns it.
	sf, err := d.GetSessionFull(ctx, "s1")
	requireNoError(t, err, "GetSessionFull after trash")
	assert.NotNil(t, sf,
		"GetSessionFull should still return trashed session")
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
	require.NotNil(t, s, "session lost during migration")
	assert.Equal(t, "myproj", s.Project, "project")
	assert.Equal(t, 3, s.MessageCount, "message_count")

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
	require.NotNil(t, nonzero, "legacy-nonzero missing")
	assert.True(t, nonzero.HasTotalOutputTokens, "legacy-nonzero HasTotalOutputTokens = false, want true")
	assert.True(t, nonzero.HasPeakContextTokens, "legacy-nonzero HasPeakContextTokens = false, want true")

	zero, err := d.GetSession(ctx, "legacy-zero")
	requireNoError(t, err, "GetSession legacy-zero")
	require.NotNil(t, zero, "legacy-zero missing")
	assert.True(t, zero.HasTotalOutputTokens, "legacy-zero HasTotalOutputTokens = false, want true")
	assert.True(t, zero.HasPeakContextTokens, "legacy-zero HasPeakContextTokens = false, want true")

	msgs, err := d.GetMessages(ctx, "legacy-zero", 0, 10, true)
	requireNoError(t, err, "GetMessages legacy-zero")
	require.Len(t, msgs, 1, "legacy-zero messages =")
	assert.True(t, msgs[0].HasContextTokens,
		"legacy-zero message HasContextTokens = false, want true")
	assert.True(t, msgs[0].HasOutputTokens,
		"legacy-zero message HasOutputTokens = false, want true")
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
	require.NotNil(t, sess, "current session missing")
	assert.True(t, sess.HasTotalOutputTokens, "HasTotalOutputTokens = false, want true")
	assert.True(t, sess.HasPeakContextTokens, "HasPeakContextTokens = false, want true")

	msgs, err := d.GetMessages(ctx, "current", 0, 10, true)
	requireNoError(t, err, "GetMessages current")
	require.Len(t, msgs, 1, "messages len =")
	assert.True(t, msgs[0].HasContextTokens,
		"HasContextTokens = false, want true")
	assert.True(t, msgs[0].HasOutputTokens,
		"HasOutputTokens = false, want true")
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
	require.NotNil(t, sess, "current session missing after marker")
	assert.False(t, sess.HasTotalOutputTokens, "HasTotalOutputTokens = true after marker, want false")
	assert.False(t, sess.HasPeakContextTokens, "HasPeakContextTokens = true after marker, want false")

	msgs, err = d.GetMessages(ctx, "current", 0, 10, true)
	requireNoError(t, err, "GetMessages current after marker")
	require.Len(t, msgs, 1, "messages len after marker =")
	assert.False(t, msgs[0].HasContextTokens,
		"HasContextTokens = true after marker, want false")
	assert.False(t, msgs[0].HasOutputTokens,
		"HasOutputTokens = true after marker, want false")
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
	require.Len(t, candidates, 0, "candidate count =")
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
	require.Equal(t, 0, updates, "updates")
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
		require.True(t, ok, "expected to find session")
		assert.Equal(t, "codex:inc-test", info.ID, "ID")
		assert.Equal(t, int64(4096), info.FileSize, "FileSize")
		assert.Equal(t, 0, reflectedIntField(info, "NextOrdinal"), "NextOrdinal")
		assert.Equal(t, "", reflectedStringField(info, "LastEntryUUID"), "LastEntryUUID")
		assert.Equal(t, 5, info.MsgCount, "MsgCount")
		assert.Equal(t, 2, info.UserMsgCount, "UserMsgCount")
		assert.Equal(t, 500, info.TotalOutputTokens, "TotalOutputTokens")
		assert.Equal(t, 1500, info.PeakContextTokens, "PeakContextTokens")
		assert.True(t, info.HasTotalOutputTokens, "HasTotalOutputTokens = false, want true")
		assert.True(t, info.HasPeakContextTokens, "HasPeakContextTokens = false, want true")
	})

	t.Run("not_found", func(t *testing.T) {
		_, ok := d.GetSessionForIncremental("/no/such/file")
		assert.False(t, ok, "expected not found")
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
		assert.False(t, ok,
			"expected false for multi-session file")
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
		require.True(t, ok, "expected legacy session for incremental")
		assert.True(t, info.HasTotalOutputTokens, "HasTotalOutputTokens = false, want true")
		assert.True(t, info.HasPeakContextTokens, "HasPeakContextTokens = false, want true")

		err = callUpdateSessionIncrementalCompat(
			t,
			d,
			info.ID,
			nil,
			info.MsgCount+1,
			info.UserMsgCount,
			info.FileSize+256,
			200,
			3,
			"entry-3",
			info.TotalOutputTokens+50,
			info.PeakContextTokens,
			info.HasTotalOutputTokens,
			info.HasPeakContextTokens,
		)
		requireNoError(t, err, "UpdateSessionIncremental legacy")

		got, err := d.GetSessionFull(context.Background(), info.ID)
		requireNoError(t, err, "GetSessionFull legacy")
		require.NotNil(t, got, "legacy session missing after incremental")
		assert.Equal(t, 3, reflectedIntField(got, "NextOrdinal"), "NextOrdinal")
		assert.Equal(t, "entry-3", reflectedStringField(got, "LastEntryUUID"), "LastEntryUUID")
		assert.True(t, got.HasTotalOutputTokens, "stored HasTotalOutputTokens = false, want true")
		assert.True(t, got.HasPeakContextTokens, "stored HasPeakContextTokens = false, want true")
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
	err := callUpdateSessionIncrementalCompat(
		t,
		d,
		"inc-update",
		&ended,
		7,
		3,
		2048,
		200,
		9,
		"uuid-9",
		500,
		1600,
		true,
		true,
	)
	requireNoError(t, err, "incremental update")

	// Verify updated fields changed.
	got, err := d.GetSessionFull(
		context.Background(), "inc-update",
	)
	requireNoError(t, err, "get session")
	assert.Equal(t, 7, got.MessageCount, "MessageCount")
	assert.Equal(t, 3, got.UserMessageCount, "UserMessageCount")
	require.NotNil(t, got.EndedAt, "EndedAt nil")
	assert.Equal(t, ended, *got.EndedAt, "EndedAt")
	require.NotNil(t, got.FileSize, "FileSize nil")
	assert.Equal(t, int64(2048), *got.FileSize, "FileSize")
	assert.Equal(t, 9, reflectedIntField(got, "NextOrdinal"), "NextOrdinal")
	assert.Equal(t, "uuid-9", reflectedStringField(got, "LastEntryUUID"), "LastEntryUUID")
	assert.Equal(t, 500, got.TotalOutputTokens, "TotalOutputTokens")
	assert.Equal(t, 1600, got.PeakContextTokens, "PeakContextTokens")
	assert.True(t, got.HasTotalOutputTokens, "HasTotalOutputTokens = false, want true")
	assert.True(t, got.HasPeakContextTokens, "HasPeakContextTokens = false, want true")

	// Verify preserved fields were NOT cleared.
	require.NotNil(t, got.FirstMessage, "FirstMessage cleared")
	assert.Equal(t, "hello", *got.FirstMessage, "FirstMessage")
	assert.Equal(t, "my-project", got.Project, "Project cleared")
	require.NotNil(t, got.ParentSessionID,
		"ParentSessionID cleared")
	assert.Equal(t, "parent-1", *got.ParentSessionID, "ParentSessionID")
	assert.Equal(t, "continuation", got.RelationshipType,
		"RelationshipType cleared")
	require.NotNil(t, got.FileHash, "FileHash cleared")
	assert.Equal(t, "abc123", *got.FileHash, "FileHash")
}

func TestIncrementalWriteAtomicityRollsBackMessages(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "atomic-target", "proj")

	_, err := d.getWriter().Exec(`
		CREATE TRIGGER sessions_incremental_atomicity_abort
		BEFORE UPDATE ON sessions
		WHEN NEW.id = 'atomic-target'
		BEGIN
			SELECT RAISE(FAIL, 'atomicity proof trigger');
		END;
	`)
	require.NoError(t, err, "create trigger")

	msgsToWrite := []Message{asstMsg("atomic-target", 0, "should rollback")}
	writeMethod := reflect.ValueOf(d).MethodByName("WriteSessionIncremental")
	if writeMethod.IsValid() {
		update := reflect.New(writeMethod.Type().In(2)).Elem()
		update.FieldByName("MsgCount").SetInt(1)
		update.FieldByName("UserMsgCount").SetInt(0)
		update.FieldByName("FileSize").SetInt(128)
		update.FieldByName("FileMtime").SetInt(10)
		if f := update.FieldByName("NextOrdinal"); f.IsValid() {
			f.SetInt(1)
		}
		results := writeMethod.Call([]reflect.Value{
			reflect.ValueOf("atomic-target"),
			reflect.ValueOf(msgsToWrite),
			update,
		})
		if !results[0].IsNil() {
			err = results[0].Interface().(error)
		} else {
			err = nil
		}
	} else {
		err = d.InsertMessages(msgsToWrite)
		require.NoError(t, err, "InsertMessages before non-atomic update")

		updateMethod := reflect.ValueOf(d).MethodByName("UpdateSessionIncremental")
		require.True(t, updateMethod.IsValid(), "UpdateSessionIncremental")
		results := updateMethod.Call([]reflect.Value{
			reflect.ValueOf("atomic-target"),
			reflect.Zero(updateMethod.Type().In(1)),
			reflect.ValueOf(1),
			reflect.ValueOf(0),
			reflect.ValueOf(int64(128)),
			reflect.ValueOf(int64(10)),
			reflect.ValueOf(0),
			reflect.ValueOf(0),
			reflect.ValueOf(false),
			reflect.ValueOf(false),
		})
		if !results[0].IsNil() {
			err = results[0].Interface().(error)
		} else {
			err = nil
		}
	}
	require.Error(t, err, "expected session update trigger to fail")

	msgs, getErr := d.GetMessages(
		context.Background(), "atomic-target", 0, 10, true,
	)
	require.NoError(t, getErr, "GetMessages")
	assert.Empty(t, msgs, "message rows should roll back with session metadata failure")
}

func TestSyncState_GetSetRoundtrip(t *testing.T) {
	d := testDB(t)

	// Initially empty.
	val, err := d.GetSyncState("last_push_at")
	requireNoError(t, err, "get initial")
	require.Equal(t, "", val, "initial value")

	// Set and read back.
	err = d.SetSyncState("last_push_at", "2026-03-11T12:00:00.000Z")
	require.NoError(t, err, "set")
	val, err = d.GetSyncState("last_push_at")
	requireNoError(t, err, "get after set")
	require.Equal(t, "2026-03-11T12:00:00.000Z", val, "value")

	// Update.
	err = d.SetSyncState("last_push_at", "2026-03-11T13:00:00.000Z")
	require.NoError(t, err, "update")
	val, err = d.GetSyncState("last_push_at")
	requireNoError(t, err, "get after update")
	require.Equal(t, "2026-03-11T13:00:00.000Z", val, "value")
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
		require.NoError(t, d.UpsertSession(s),
			"upsert %s", s.ID)
	}

	// Backdate created_at for deterministic test results.
	for _, s := range sessions {
		_, err := d.getWriter().Exec(
			"UPDATE sessions SET created_at = ? WHERE id = ?",
			s.CreatedAt, s.ID,
		)
		require.NoError(t, err, "backdate %s", s.ID)
	}

	// Query all.
	all, err := d.ListSessionsModifiedBetween(ctx, "", "", nil, nil)
	require.NoError(t, err, "list all")
	require.Len(t, all, 3, "list all =")

	// Query with since.
	since, err := d.ListSessionsModifiedBetween(ctx, "2026-03-11T00:00:00Z", "", nil, nil)
	require.NoError(t, err, "list since")
	require.Len(t, since, 2, "list since =")

	// Query with until.
	until, err := d.ListSessionsModifiedBetween(ctx, "", "2026-03-11T12:00:00.000Z", nil, nil)
	require.NoError(t, err, "list until")
	require.Len(t, until, 2, "list until =")

	// Query with both.
	between, err := d.ListSessionsModifiedBetween(ctx, "2026-03-10T12:00:00.000Z", "2026-03-11T12:00:00.000Z", nil, nil)
	require.NoError(t, err, "list between")
	require.Len(t, between, 1, "list between =")
	assert.Equal(t, "s2", between[0].ID, "between[0].ID")
}

func TestMessageContentFingerprint(t *testing.T) {
	d := testDB(t)
	sess := Session{ID: "fp-sess", Project: "p", Machine: "local", Agent: "claude"}
	err := d.UpsertSession(sess)
	require.NoError(t, err, "upsert")
	require.NoError(t, d.InsertMessages([]Message{
		{SessionID: "fp-sess", Ordinal: 0, Role: "user", Content: "hello", ContentLength: 5},
		{SessionID: "fp-sess", Ordinal: 1, Role: "assistant", Content: "hi there!", ContentLength: 9},
	}), "insert")

	sum, max, min, err := d.MessageContentFingerprint("fp-sess")
	require.NoError(t, err, "fingerprint")
	assert.Equal(t, int64(14), sum, "sum")
	assert.Equal(t, int64(9), max, "max")
	assert.Equal(t, int64(5), min, "min")
}

func TestSystemMessageFingerprint(t *testing.T) {
	d := testDB(t)
	sess := Session{ID: "sys-fp", Project: "p", Machine: "local", Agent: "claude"}
	err := d.UpsertSession(sess)
	require.NoError(t, err, "upsert")
	// System ordinals: 0 and 2 → "0,2".
	require.NoError(t, d.InsertMessages([]Message{
		{SessionID: "sys-fp", Ordinal: 0, Role: "user", Content: "sys", ContentLength: 3, IsSystem: true},
		{SessionID: "sys-fp", Ordinal: 1, Role: "assistant", Content: "hi", ContentLength: 2},
		{SessionID: "sys-fp", Ordinal: 2, Role: "user", Content: "sys2", ContentLength: 4, IsSystem: true},
	}), "insert")

	fp, err := d.SystemMessageFingerprint("sys-fp")
	require.NoError(t, err, "SystemMessageFingerprint")
	assert.Equal(t, "0,2", fp, "fingerprint")

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
		require.NoError(t, d.UpsertSession(s), "upsert %s", tc.id)
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
		require.NoError(t, d.InsertMessages(msgs),
			"insert %s", tc.id)
		got, err := d.SystemMessageFingerprint(tc.id)
		require.NoError(t, err, "SystemMessageFingerprint %s", tc.id)
		assert.Equal(t, tc.want, got, "%s", tc.id)
	}
}

func TestToolCallCountAndFingerprint(t *testing.T) {
	d := testDB(t)
	sess := Session{ID: "tc-sess", Project: "p", Machine: "local", Agent: "claude"}
	err := d.UpsertSession(sess)
	require.NoError(t, err, "upsert")
	require.NoError(t, d.InsertMessages([]Message{
		{
			SessionID: "tc-sess", Ordinal: 0, Role: "assistant", Content: "tool",
			ToolCalls: []ToolCall{
				{ToolName: "Read", Category: "Read", ResultContentLength: 100},
				{ToolName: "Write", Category: "Write", ResultContentLength: 50},
			},
		},
	}), "insert")

	count, err := d.ToolCallCount("tc-sess")
	require.NoError(t, err, "count")
	assert.Equal(t, 2, count, "count")

	sum, err := d.ToolCallContentFingerprint("tc-sess")
	require.NoError(t, err, "fingerprint")
	assert.Equal(t, int64(150), sum, "sum")
}

func TestToolCallFingerprintIncludesStableFields(t *testing.T) {
	d := testDB(t)
	for _, id := range []string{"tc-old", "tc-new"} {
		err := d.UpsertSession(Session{
			ID: id, Project: "p", Machine: "local", Agent: "cursor",
		})
		require.NoError(t, err, "upsert %s", id)
	}
	require.NoError(t, d.InsertMessages([]Message{
		{
			SessionID: "tc-old", Ordinal: 0, Role: "assistant", Content: "tool",
			ToolCalls: []ToolCall{
				{
					ToolName:            "ApplyPatch",
					Category:            "ApplyPatch",
					ToolUseID:           "toolu_patch",
					ResultContentLength: 12,
				},
			},
		},
		{
			SessionID: "tc-new", Ordinal: 0, Role: "assistant", Content: "tool",
			ToolCalls: []ToolCall{
				{
					ToolName:            "ApplyPatch",
					Category:            "Edit",
					ToolUseID:           "toolu_patch",
					InputJSON:           `{"patch":"@@\n-old\n+new"}`,
					ResultContentLength: 12,
				},
			},
		},
	}), "insert")

	oldFP, err := d.ToolCallFingerprint("tc-old")
	require.NoError(t, err, "old fingerprint")
	newFP, err := d.ToolCallFingerprint("tc-new")
	require.NoError(t, err, "new fingerprint")

	assert.NotEqual(t, oldFP, newFP)
	assert.Contains(t, newFP, "Edit")
	assert.Contains(t, newFP, `{"patch":"@@\n-old\n+new"}`)
}

func TestToolCallFingerprintHandlesEmptyToolUseID(t *testing.T) {
	d := testDB(t)
	err := d.UpsertSession(Session{
		ID: "tc-empty-id", Project: "p", Machine: "local", Agent: "cursor",
	})
	require.NoError(t, err, "upsert")
	require.NoError(t, d.InsertMessages([]Message{
		{
			SessionID: "tc-empty-id", Ordinal: 0, Role: "assistant",
			Content: "tool",
			ToolCalls: []ToolCall{
				{
					ToolName:  "ApplyPatch",
					Category:  "Edit",
					InputJSON: `{"patch":"@@\n-old\n+new"}`,
				},
			},
		},
	}), "insert")

	fp, err := d.ToolCallFingerprint("tc-empty-id")
	require.NoError(t, err, "fingerprint")

	assert.Contains(t, fp, "ApplyPatch")
	assert.Contains(t, fp, "Edit")
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
		require.NoError(t, d.UpsertSession(s),
			"upsert %s", s.ID)
	}
	for _, s := range sessions {
		_, err := d.getWriter().Exec(
			"UPDATE sessions SET created_at = ? WHERE id = ?",
			s.CreatedAt, s.ID,
		)
		require.NoError(t, err, "backdate %s", s.ID)
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
			require.NoError(t, err, "ListSessionsModifiedBetween")
			var gotIDs []string
			for _, s := range got {
				gotIDs = append(gotIDs, s.ID)
			}
			require.Len(t, gotIDs, len(tt.wantIDs),
				"got %v, want %v", gotIDs, tt.wantIDs)
			for i, id := range tt.wantIDs {
				assert.Equal(t, id, gotIDs[i],
					"got[%d] = %q, want %q", i, gotIDs[i], id)
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

	require.Equal(t, 1, count,
		"expected 1 termination_status column, got %d", count)
}

func TestSessionsTerminationStatusIndex(t *testing.T) {
	d := testDB(t)

	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'index' AND name = 'idx_sessions_termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "probing idx_sessions_termination_status")

	require.Equal(t, 1, count,
		"expected idx_sessions_termination_status to exist, got count=%d",
		count)
}

func TestMessagesUsageCoveringIndex(t *testing.T) {
	d := testDB(t)

	var count int
	err := d.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'index' AND name = 'idx_messages_usage_covering'`,
	).Scan(&count)
	requireNoError(t, err, "probing idx_messages_usage_covering")

	require.Equal(t, 1, count,
		"expected idx_messages_usage_covering to exist, got count=%d",
		count)

	err = d.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type = 'index' AND name = 'idx_messages_usage_timestamp'`,
	).Scan(&count)
	requireNoError(t, err, "probing legacy idx_messages_usage_timestamp")
	require.Equal(t, 0, count,
		"expected idx_messages_usage_timestamp to be dropped")
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
	require.Equal(t, 0, count,
		"expected termination_status column to be absent")
	err = conn.QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_sessions_termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "verify index removed")
	require.Equal(t, 0, count,
		"expected termination_status index to be absent")
	var sessCount int
	err = conn.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessCount)
	requireNoError(t, err, "count sessions pre-migration")
	require.Equal(t, 1, sessCount, "expected 1 session row, got")

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
	require.Equal(t, 1, count,
		"expected termination_status column after migration")
	err = d2.getReader().QueryRow(
		`SELECT count(*) FROM sqlite_master
		 WHERE type='index' AND name='idx_sessions_termination_status'`,
	).Scan(&count)
	requireNoError(t, err, "verify index added")
	require.Equal(t, 1, count,
		"expected termination_status index after migration")

	sessions, err := d2.ListSessions(context.Background(), SessionFilter{})
	requireNoError(t, err, "list sessions")
	require.Len(t, sessions.Sessions, 1,
		"expected 1 session 's1' after migration, got %v",
		sessions.Sessions)
	require.Equal(t, "s1", sessions.Sessions[0].ID,
		"expected session id 's1' after migration, got %v",
		sessions.Sessions)
	if sessions.Sessions[0].TerminationStatus != nil {
		assert.Failf(t, "termination_status not NULL",
			"expected NULL termination_status after migration, got %q",
			*sessions.Sessions[0].TerminationStatus)
	}
}

// TestDisplayNameNotOverwrittenByUpsert verifies that display_name (the user
// override column) is never touched by UpsertSession. Only RenameSession
// may write it. This replaces the old BackfillNameSource tests which tested
// a migration helper that no longer exists.
func TestDisplayNameNotOverwrittenByUpsert(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	fp := "/home/user/.claude/sessions/s1.jsonl"
	// Fresh session with a session_name from the parser.
	requireNoError(t, d.UpsertSession(Session{
		ID:          "s1",
		Project:     "p",
		Machine:     "local",
		Agent:       "claude",
		SessionName: Ptr("Agent Name"),
		FilePath:    &fp,
	}), "initial upsert")

	// User renames it.
	requireNoError(t, d.RenameSession("s1", Ptr("Manual Name")), "RenameSession")

	// Re-parse with a new agent name: display_name must NOT be overwritten.
	requireNoError(t, d.UpsertSession(Session{
		ID:          "s1",
		Project:     "p",
		Machine:     "local",
		Agent:       "claude",
		SessionName: Ptr("New Agent Name"),
		FilePath:    &fp,
	}), "re-upsert with new agent name")

	s, err := d.GetSession(ctx, "s1")
	require.NoError(t, err, "GetSession")
	require.NotNil(t, s, "session not found")
	require.NotNil(t, s.DisplayName, "display_name must not be nil")
	assert.Equal(t, "Manual Name", *s.DisplayName,
		"user's display_name must survive re-parse")
}

func TestCopySessionMetadataPreservesUserNotAgent(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.db")

	// Old DB: one user-renamed session, one agent-named session.
	oldDB, err := Open(oldPath)
	requireNoError(t, err, "open old")
	requireNoError(t, oldDB.UpsertSession(Session{
		ID: "u", Project: "p", Machine: "local", Agent: "claude",
	}), "upsert u")
	requireNoError(t, oldDB.RenameSession("u", Ptr("User Name")), "rename u")
	requireNoError(t, oldDB.UpsertSession(Session{
		ID: "a", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Old Agent"),
	}), "upsert a")
	requireNoError(t, oldDB.Close(), "close old")

	// Fresh DB: both re-parsed with new agent names.
	fresh := testDB(t)
	requireNoError(t, fresh.UpsertSession(Session{
		ID: "u", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Fresh Agent U"),
	}), "fresh u")
	requireNoError(t, fresh.UpsertSession(Session{
		ID: "a", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Fresh Agent A"),
	}), "fresh a")

	requireNoError(t, fresh.CopySessionMetadataFrom(oldPath), "copy")

	u := getSessionRow(t, fresh, "u")
	require.NotNil(t, u.DisplayName, "user rename overlaid")
	assert.Equal(t, "User Name", *u.DisplayName, "user rename overlaid")

	a := getSessionRow(t, fresh, "a")
	assert.Nil(t, a.DisplayName, "agent session has no user display_name")
	require.NotNil(t, a.SessionName, "fresh session_name preserved")
	assert.Equal(t, "Fresh Agent A", *a.SessionName, "agent keeps fresh session_name")
}

// A name the user cleared in the old DB (display_name NULL) must NOT be
// re-applied on resync; the fresh re-parsed session_name wins.
func TestCopySessionMetadataClearedNameYieldsToAgent(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.db")

	oldDB, err := Open(oldPath)
	requireNoError(t, err, "open old")
	requireNoError(t, oldDB.UpsertSession(Session{
		ID: "c", Project: "p", Machine: "local", Agent: "claude",
	}), "upsert c")
	requireNoError(t, oldDB.RenameSession("c", Ptr("Once Named")), "rename")
	requireNoError(t, oldDB.RenameSession("c", nil), "clear")
	requireNoError(t, oldDB.Close(), "close old")

	fresh := testDB(t)
	requireNoError(t, fresh.UpsertSession(Session{
		ID: "c", Project: "p", Machine: "local", Agent: "claude",
		SessionName: Ptr("Fresh Agent C"),
	}), "fresh c")

	requireNoError(t, fresh.CopySessionMetadataFrom(oldPath), "copy")

	c := getSessionRow(t, fresh, "c")
	assert.Nil(t, c.DisplayName,
		"cleared old display_name must not be re-applied")
	require.NotNil(t, c.SessionName, "fresh session_name preserved")
	assert.Equal(t, "Fresh Agent C", *c.SessionName,
		"cleared old name must yield to fresh session_name")
}

func TestUpsertSessionPersistsSessionName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Simulate what a converter should produce: SessionName set, DisplayName nil.
	require.NoError(t, d.UpsertSession(Session{
		ID:           "conv-test-1",
		Project:      "p",
		Machine:      "local",
		Agent:        "claude",
		SessionName:  Ptr("My /rename Title"),
		MessageCount: 1,
	}), "upsert")

	s, err := d.GetSession(ctx, "conv-test-1")
	require.NoError(t, err)
	require.NotNil(t, s)
	require.NotNil(t, s.DisplayName,
		"COALESCE should expose session_name as display_name")
	assert.Equal(t, "My /rename Title", *s.DisplayName)
}

func TestUpsertWithDisplayNameInsteadOfSessionNameDropsName(t *testing.T) {
	d := testDB(t)
	ctx := context.Background()

	// Bug: converter sets DisplayName instead of SessionName.
	// The upsert only writes session_name from SessionName field,
	// so setting DisplayName is silently dropped.
	require.NoError(t, d.UpsertSession(Session{
		ID:           "conv-test-2",
		Project:      "p",
		Machine:      "local",
		Agent:        "claude",
		DisplayName:  Ptr("Wrongly set in DisplayName"),
		MessageCount: 1,
	}), "upsert")

	s, err := d.GetSession(ctx, "conv-test-2")
	require.NoError(t, err)
	require.NotNil(t, s)
	// display_name was passed in but upsert never writes it — should be nil.
	assert.Nil(t, s.DisplayName,
		"upsert must not write display_name; only RenameSession should")
}
