//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	require.NoError(t, err, "opening test db")
	t.Cleanup(func() { d.Close() })
	return d
}

func cleanPGSchema(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()
	_, _ = pg.Exec(
		"DROP SCHEMA IF EXISTS agentsview CASCADE",
	)
}

func TestEnsureSchemaIdempotent(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()

	require.NoError(t, ps.EnsureSchema(ctx), "first EnsureSchema")
	require.NoError(t, ps.EnsureSchema(ctx), "second EnsureSchema")

	var eventIndex int
	err = ps.pg.QueryRowContext(ctx,
		"SELECT event_index FROM tool_result_events LIMIT 0",
	).Scan(&eventIndex)
	if err != nil && err != sql.ErrNoRows {
		t.Fatalf("tool_result_events schema probe: %v", err)
	}
}

func TestEnsureSchemaMigratesLegacySchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	pg, err := Open(pgURL, "agentsview", true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()

	// Simulate a 0.16.x schema: create the schema and core
	// tables but omit tool_result_events.
	_, err = pg.ExecContext(ctx,
		"CREATE SCHEMA IF NOT EXISTS agentsview",
	)
	require.NoError(t, err, "creating schema")
	legacyDDL := `
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
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS messages (
    session_id     TEXT NOT NULL,
    ordinal        INT NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    timestamp      TIMESTAMPTZ,
    has_thinking   BOOLEAN NOT NULL DEFAULT FALSE,
    has_tool_use   BOOLEAN NOT NULL DEFAULT FALSE,
    content_length INT NOT NULL DEFAULT 0,
    is_system      BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (session_id, ordinal),
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS tool_calls (
    id                    BIGSERIAL PRIMARY KEY,
    session_id            TEXT NOT NULL,
    tool_name             TEXT NOT NULL,
    category              TEXT NOT NULL,
    call_index            INT NOT NULL DEFAULT 0,
    tool_use_id           TEXT NOT NULL DEFAULT '',
    input_json            TEXT,
    skill_name            TEXT,
    result_content_length INT,
    result_content        TEXT,
    subagent_session_id   TEXT,
    message_ordinal       INT NOT NULL,
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);`
	_, err = pg.ExecContext(ctx, legacyDDL)
	require.NoError(t, err, "creating legacy tables")

	// Verify tool_result_events does not exist yet.
	require.Error(t, CheckSchemaCompat(ctx, pg),
		"expected CheckSchemaCompat to fail on legacy schema")

	// Run EnsureSchema — should create the missing table.
	require.NoError(t, EnsureSchema(ctx, pg, "agentsview"),
		"EnsureSchema on legacy schema")

	// Now the compat check should pass.
	require.NoError(t, CheckSchemaCompat(ctx, pg),
		"CheckSchemaCompat after migration")
}

// TestCheckSchemaCompatMissingSecretsRulesVersion pins the schema-compat
// probe against a regression where pgSessionCols selects a column the
// compat check doesn't probe for: a PG schema missing only
// sessions.secrets_rules_version must fail CheckSchemaCompat rather
// than passing the probe and 500-ing at runtime on the first session
// query. EnsureSchema brings the column in via migration; this test
// then drops it to simulate a legacy/read-only schema and verifies the
// probe catches the absence.
func TestCheckSchemaCompatMissingSecretsRulesVersion(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	pg, err := Open(pgURL, "agentsview", true)
	require.NoError(t, err, "connecting to pg")
	defer pg.Close()

	ctx := context.Background()
	require.NoError(t, EnsureSchema(ctx, pg, "agentsview"), "EnsureSchema")
	require.NoError(t, CheckSchemaCompat(ctx, pg),
		"precondition: CheckSchemaCompat should pass after EnsureSchema")
	_, err = pg.ExecContext(ctx,
		`ALTER TABLE sessions DROP COLUMN secrets_rules_version`,
	)
	require.NoError(t, err, "dropping secrets_rules_version")
	require.Error(t, CheckSchemaCompat(ctx, pg),
		"CheckSchemaCompat should fail when secrets_rules_version is missing")
}

func TestPushSingleSession(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := "2026-03-11T12:00:00Z"
	firstMsg := "hello world"
	sess := db.Session{
		ID:           "sess-001",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		FirstMessage: &firstMsg,
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID: "sess-001",
			Ordinal:   0,
			Role:      "user",
			Content:   firstMsg,
		},
	}), "insert messages")

	result, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	assert.Equal(t, 1, result.SessionsPushed)
	assert.Equal(t, 1, result.MessagesPushed)

	var pgProject, pgMachine string
	err = ps.pg.QueryRowContext(ctx,
		"SELECT project, machine FROM sessions WHERE id = $1",
		"sess-001",
	).Scan(&pgProject, &pgMachine)
	require.NoError(t, err, "querying pg session")
	assert.Equal(t, "test-project", pgProject)
	assert.Equal(t, "test-machine", pgMachine)

	var pgMsgContent string
	err = ps.pg.QueryRowContext(ctx,
		"SELECT content FROM messages WHERE session_id = $1 AND ordinal = 0",
		"sess-001",
	).Scan(&pgMsgContent)
	require.NoError(t, err, "querying pg message")
	assert.Equal(t, firstMsg, pgMsgContent)
}

func TestPushIdempotent(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:           "sess-002",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 0,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")

	result1, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "first push")
	assert.Equal(t, 1, result1.SessionsPushed)

	result2, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "second push")
	assert.Equal(t, 0, result2.SessionsPushed)
}

func TestPushWithToolCalls(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:           "sess-tc-001",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID:  "sess-tc-001",
			Ordinal:    0,
			Role:       "assistant",
			Content:    "tool use response",
			HasToolUse: true,
			ToolCalls: []db.ToolCall{
				{
					ToolName:            "Read",
					Category:            "Read",
					ToolUseID:           "toolu_001",
					ResultContentLength: 42,
					ResultContent:       "file content here",
				},
			},
		},
	}), "insert messages")

	result, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	assert.Equal(t, 1, result.MessagesPushed)

	var toolName string
	var resultLen int
	err = ps.pg.QueryRowContext(ctx,
		"SELECT tool_name, result_content_length FROM tool_calls WHERE session_id = $1",
		"sess-tc-001",
	).Scan(&toolName, &resultLen)
	require.NoError(t, err, "querying pg tool_call")
	assert.Equal(t, "Read", toolName)
	assert.Equal(t, 42, resultLen)
}

func TestPushWithToolResultEvents(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	sess := db.Session{
		ID:           "sess-events-001",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "codex",
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID:  "sess-events-001",
			Ordinal:    0,
			Role:       "assistant",
			Content:    "tool use response",
			HasToolUse: true,
			ToolCalls: []db.ToolCall{
				{
					ToolName:  "wait",
					Category:  "Task",
					ToolUseID: "call_wait",
					ResultEvents: []db.ToolResultEvent{
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
					},
				},
			},
		},
	}), "insert messages")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	var count int
	err = ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_result_events WHERE session_id = $1",
		"sess-events-001",
	).Scan(&count)
	require.NoError(t, err, "querying pg tool_result_events")
	assert.Equal(t, 1, count)
}

func TestStatus(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	status, err := ps.Status(ctx)
	require.NoError(t, err, "status")
	assert.Equal(t, "test-machine", status.Machine)
	assert.Equal(t, 0, status.PGSessions)
}

func TestStatusMissingSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	status, err := ps.Status(ctx)
	require.NoError(t, err, "status on missing schema")
	assert.Equal(t, 0, status.PGSessions)
	assert.Equal(t, 0, status.PGMessages)
	assert.Equal(t, "test-machine", status.Machine)
}

func TestNewRejectsMachineLocal(t *testing.T) {
	pgURL := testPGURL(t)
	local := testDB(t)
	_, err := New(
		pgURL, "agentsview", local, "local", true,
		SyncOptions{},
	)
	require.Error(t, err, "expected error for machine=local")
}

func TestNewRejectsEmptyMachine(t *testing.T) {
	pgURL := testPGURL(t)
	local := testDB(t)
	_, err := New(
		pgURL, "agentsview", local, "", true,
		SyncOptions{},
	)
	require.Error(t, err, "expected error for empty machine")
}

func TestNewRejectsEmptyURL(t *testing.T) {
	local := testDB(t)
	_, err := New(
		"", "agentsview", local, "test", true,
		SyncOptions{},
	)
	require.Error(t, err, "expected error for empty URL")
}

func TestPushUpdatedAtFormat(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:        "sess-ts-001",
		Project:   "test-project",
		Machine:   "local",
		Agent:     "claude",
		StartedAt: &started,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")

	var updatedAt time.Time
	err = ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM sessions WHERE id = $1",
		"sess-ts-001",
	).Scan(&updatedAt)
	require.NoError(t, err, "querying updated_at")

	formatted := updatedAt.UTC().Format(
		"2006-01-02T15:04:05.000000Z",
	)
	pattern := regexp.MustCompile(
		`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`,
	)
	assert.True(t, pattern.MatchString(formatted),
		"updated_at = %q, want ISO-8601 microsecond format", formatted)
}

func TestPushBumpsUpdatedAtOnMessageRewrite(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"machine-a", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := time.Now().UTC().Format(time.RFC3339)
	sess := db.Session{
		ID:           "sess-bump-001",
		Project:      "test",
		Machine:      "local",
		Agent:        "test-agent",
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	msg := db.Message{
		SessionID:     "sess-bump-001",
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: 5,
	}
	require.NoError(t, local.ReplaceSessionMessages(
		"sess-bump-001", []db.Message{msg},
	), "replace messages")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "initial push")

	var updatedAt1 time.Time
	require.NoError(t, ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM sessions WHERE id = $1",
		"sess-bump-001",
	).Scan(&updatedAt1), "querying updated_at")

	time.Sleep(50 * time.Millisecond)

	result, err := ps.Push(ctx, true, nil)
	require.NoError(t, err, "full push")
	require.NotZero(t, result.MessagesPushed,
		"expected messages to be pushed on full push")

	var updatedAt2 time.Time
	require.NoError(t, ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM sessions WHERE id = $1",
		"sess-bump-001",
	).Scan(&updatedAt2), "querying updated_at after full push")

	assert.True(t, updatedAt2.After(updatedAt1),
		"updated_at not bumped: before=%v, after=%v", updatedAt1, updatedAt2)
}

func TestPushFullBypassesHeuristic(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:           "sess-full-001",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID: "sess-full-001",
			Ordinal:   0,
			Role:      "user",
			Content:   "test",
		},
	}), "insert messages")

	_, err = ps.Push(ctx, false, nil)
	require.NoError(t, err, "first push")

	require.NoError(t, local.SetSyncState(
		"last_push_at", "",
	), "resetting watermark")

	result, err := ps.Push(ctx, true, nil)
	require.NoError(t, err, "full push")
	assert.Equal(t, 1, result.SessionsPushed)
	assert.Equal(t, 1, result.MessagesPushed)
}

func TestPushDetectsSchemaReset(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	// Push a session so the watermark advances.
	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:           "sess-reset-001",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 1,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	require.NoError(t, local.InsertMessages([]db.Message{{
		SessionID:     "sess-reset-001",
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: 5,
	}}), "insert message")

	r1, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "initial push")
	require.Equal(t, 1, r1.SessionsPushed)

	// Simulate a PG schema reset — don't manually recreate;
	// let Push detect and handle it via the coherence check.
	cleanPGSchema(t, pgURL)

	// An incremental push should detect the mismatch
	// (local watermark set, PG has 0 sessions), recreate
	// the schema, and automatically force a full push.
	r2, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "post-reset push")
	assert.Equal(t, 1, r2.SessionsPushed,
		"should auto-detect schema reset")
	assert.Equal(t, 1, r2.MessagesPushed)
}

func TestPushFullAfterSchemaDropRecreatesSchema(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	ctx := context.Background()

	sess := db.Session{
		ID:        "sess-full-drop",
		Project:   "proj",
		Machine:   "test-machine",
		Agent:     "claude",
		CreatedAt: "2026-03-11T12:00:00.000Z",
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")

	r1, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "initial push")
	require.Equal(t, 1, r1.SessionsPushed)

	// Drop the schema without clearing local state.
	cleanPGSchema(t, pgURL)

	// A full push should recreate the schema even though
	// schemaDone is memoized from the first push.
	r2, err := ps.Push(ctx, true, nil)
	require.NoError(t, err, "full push after drop")
	assert.Equal(t, 1, r2.SessionsPushed)
}

func TestPushBatchesMultipleSessions(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	// Create 75 sessions to exercise two batches (50 + 25).
	const totalSessions = 75
	for i := range totalSessions {
		id := fmt.Sprintf("batch-sess-%03d", i)
		started := "2026-03-11T12:00:00Z"
		sess := db.Session{
			ID:           id,
			Project:      "batch-project",
			Machine:      "local",
			Agent:        "claude",
			StartedAt:    &started,
			MessageCount: 2,
		}
		require.NoError(t, local.UpsertSession(sess),
			"upsert session %d", i)
		require.NoError(t, local.InsertMessages([]db.Message{
			{
				SessionID:     id,
				Ordinal:       0,
				Role:          "user",
				Content:       fmt.Sprintf("msg %d", i),
				ContentLength: 5,
			},
			{
				SessionID:     id,
				Ordinal:       1,
				Role:          "assistant",
				Content:       fmt.Sprintf("reply %d", i),
				ContentLength: 7,
			},
		}), "insert messages %d", i)
	}

	result, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	assert.Equal(t, totalSessions, result.SessionsPushed)
	assert.Equal(t, totalSessions*2, result.MessagesPushed)
	assert.Equal(t, 0, result.Errors)

	// Verify PG state.
	var pgSessions, pgMessages int
	require.NoError(t, ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions",
	).Scan(&pgSessions), "counting pg sessions")
	require.NoError(t, ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages",
	).Scan(&pgMessages), "counting pg messages")
	assert.Equal(t, totalSessions, pgSessions)
	assert.Equal(t, totalSessions*2, pgMessages)
}

func TestPushBulkInsertManyMessages(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	// Create a session with 250 messages to exercise
	// multi-row VALUES batching (100 per batch).
	const msgCount = 250
	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:           "bulk-msg-sess",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: msgCount,
	}
	require.NoError(t, local.UpsertSession(sess), "upsert session")
	msgs := make([]db.Message, msgCount)
	for i := range msgs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs[i] = db.Message{
			SessionID:     "bulk-msg-sess",
			Ordinal:       i,
			Role:          role,
			Content:       fmt.Sprintf("message %d", i),
			ContentLength: len(fmt.Sprintf("message %d", i)),
		}
		// Add a tool call on every 10th assistant message.
		if role == "assistant" && i%10 == 1 {
			msgs[i].HasToolUse = true
			msgs[i].ToolCalls = []db.ToolCall{{
				ToolName:            "Read",
				Category:            "Read",
				ToolUseID:           fmt.Sprintf("toolu_%d", i),
				ResultContentLength: 10,
				ResultContent:       "some result",
			}}
		}
	}
	require.NoError(t, local.InsertMessages(msgs), "insert messages")

	result, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	assert.Equal(t, 1, result.SessionsPushed)
	assert.Equal(t, msgCount, result.MessagesPushed)

	// Verify all messages landed in PG.
	var pgMsgCount int
	require.NoError(t, ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE session_id = $1",
		"bulk-msg-sess",
	).Scan(&pgMsgCount), "counting pg messages")
	assert.Equal(t, msgCount, pgMsgCount)

	// Verify tool calls landed.
	var pgTCCount int
	require.NoError(t, ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_calls WHERE session_id = $1",
		"bulk-msg-sess",
	).Scan(&pgTCCount), "counting pg tool_calls")
	// Every 10th assistant message (ordinals 1, 11, 21, ...).
	expectedTC := 0
	for i := range msgCount {
		if i%2 == 1 && i%10 == 1 {
			expectedTC++
		}
	}
	assert.Equal(t, expectedTC, pgTCCount)
}

func TestPushSimplePK(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	ctx := context.Background()
	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	var constraintDef string
	err = ps.pg.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_namespace n ON n.oid = c.connamespace
		WHERE n.nspname = 'agentsview'
		  AND c.conrelid = 'agentsview.sessions'::regclass
		  AND c.contype = 'p'
	`).Scan(&constraintDef)
	require.NoError(t, err, "querying sessions PK")
	assert.Equal(t, "PRIMARY KEY (id)", constraintDef)

	err = ps.pg.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_namespace n ON n.oid = c.connamespace
		WHERE n.nspname = 'agentsview'
		  AND c.conrelid = 'agentsview.messages'::regclass
		  AND c.contype = 'p'
	`).Scan(&constraintDef)
	require.NoError(t, err, "querying messages PK")
	assert.Equal(t, "PRIMARY KEY (session_id, ordinal)", constraintDef)
}

func TestPushFilteredByProject(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)

	// Seed three sessions across three projects.
	for _, s := range []db.Session{
		{
			ID: "s-alpha", Project: "alpha",
			Machine: "local", Agent: "claude",
			MessageCount: 1,
		},
		{
			ID: "s-beta", Project: "beta",
			Machine: "local", Agent: "claude",
			MessageCount: 1,
		},
		{
			ID: "s-gamma", Project: "gamma",
			Machine: "local", Agent: "claude",
			MessageCount: 1,
		},
	} {
		require.NoError(t, local.UpsertSession(s), "upsert %s", s.ID)
		require.NoError(t, local.InsertMessages([]db.Message{
			{
				SessionID: s.ID, Ordinal: 0,
				Role: "user", Content: "msg " + s.ID,
			},
		}), "insert msg %s", s.ID)
	}

	ctx := context.Background()

	// Step 1: push with project filter = ["alpha"].
	filtered, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{Projects: []string{"alpha"}},
	)
	require.NoError(t, err, "creating filtered sync")
	defer filtered.Close()

	require.NoError(t, filtered.EnsureSchema(ctx), "ensure schema")
	r1, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err, "filtered push")
	require.Equal(t, 1, r1.SessionsPushed)

	// Verify only alpha is in PG.
	pgSessionCount := func(project string) int {
		t.Helper()
		var n int
		err := filtered.pg.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sessions "+
				"WHERE project = $1",
			project,
		).Scan(&n)
		require.NoError(t, err, "count %s", project)
		return n
	}
	assert.Equal(t, 1, pgSessionCount("alpha"))
	assert.Equal(t, 0, pgSessionCount("beta"))
	assert.Equal(t, 0, pgSessionCount("gamma"))

	// Step 2: push unfiltered — beta and gamma should arrive.
	unfiltered, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{},
	)
	require.NoError(t, err, "creating unfiltered sync")
	defer unfiltered.Close()

	r2, err := unfiltered.Push(ctx, false, nil)
	require.NoError(t, err, "unfiltered push")
	require.GreaterOrEqual(t, r2.SessionsPushed, 2)

	// Verify all three projects are in PG.
	for _, p := range []string{"alpha", "beta", "gamma"} {
		assert.Equal(t, 1, pgSessionCount(p), "project %s", p)
	}

	// Step 3: second filtered push is a no-op (fingerprints
	// match).
	r3, err := filtered.Push(ctx, false, nil)
	require.NoError(t, err, "second filtered push")
	assert.Equal(t, 0, r3.SessionsPushed)
}

func TestPushExcludeProject(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)

	for _, s := range []db.Session{
		{
			ID: "s-a", Project: "alpha",
			Machine: "local", Agent: "claude",
			MessageCount: 1,
		},
		{
			ID: "s-b", Project: "beta",
			Machine: "local", Agent: "claude",
			MessageCount: 1,
		},
	} {
		require.NoError(t, local.UpsertSession(s), "upsert %s", s.ID)
		require.NoError(t, local.InsertMessages([]db.Message{
			{
				SessionID: s.ID, Ordinal: 0,
				Role: "user", Content: "msg",
			},
		}), "insert msg %s", s.ID)
	}

	ctx := context.Background()

	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{ExcludeProjects: []string{"beta"}},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")
	r, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "push")
	require.Equal(t, 1, r.SessionsPushed)

	var pgProject string
	err = ps.pg.QueryRowContext(ctx,
		"SELECT project FROM sessions LIMIT 1",
	).Scan(&pgProject)
	require.NoError(t, err, "query pg")
	assert.Equal(t, "alpha", pgProject)
}

func TestPushFilteredFullIsIncremental(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)

	require.NoError(t, local.UpsertSession(db.Session{
		ID: "s1", Project: "alpha",
		Machine: "local", Agent: "claude",
		MessageCount: 1,
	}), "upsert")
	require.NoError(t, local.InsertMessages([]db.Message{
		{
			SessionID: "s1", Ordinal: 0,
			Role: "user", Content: "hello",
		},
	}), "insert msg")

	ctx := context.Background()
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
		SyncOptions{Projects: []string{"alpha"}},
	)
	require.NoError(t, err, "creating sync")
	defer ps.Close()

	require.NoError(t, ps.EnsureSchema(ctx), "ensure schema")

	// First push with --full.
	r1, err := ps.Push(ctx, true, nil)
	require.NoError(t, err, "first push")
	require.Equal(t, 1, r1.SessionsPushed)

	// Filtered --full must not advance the global watermark.
	wm, err := local.GetSyncState("last_push_at")
	require.NoError(t, err, "reading watermark")
	assert.Empty(t, wm, "watermark after filtered --full")

	// Boundary fingerprints must have been written.
	bs, err := local.GetSyncState(
		"last_push_boundary_state",
	)
	require.NoError(t, err, "reading boundary state")
	require.NotEmpty(t, bs, "boundary state empty after filtered --full")

	// Second push (not --full) should be a no-op because
	// fingerprints were persisted after the filtered --full.
	r2, err := ps.Push(ctx, false, nil)
	require.NoError(t, err, "second push")
	assert.Equal(t, 0, r2.SessionsPushed)
}
