//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func cleanPGSchema(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	if err != nil {
		t.Fatalf("connecting to pg: %v", err)
	}
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
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()

	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("first EnsureSchema: %v", err)
	}

	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}

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
	if err != nil {
		t.Fatalf("connecting to pg: %v", err)
	}
	defer pg.Close()

	ctx := context.Background()

	// Simulate a 0.16.x schema: create the schema and core
	// tables but omit tool_result_events.
	if _, err := pg.ExecContext(ctx,
		"CREATE SCHEMA IF NOT EXISTS agentsview",
	); err != nil {
		t.Fatalf("creating schema: %v", err)
	}
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
	if _, err := pg.ExecContext(ctx, legacyDDL); err != nil {
		t.Fatalf("creating legacy tables: %v", err)
	}

	// Verify tool_result_events does not exist yet.
	if err := CheckSchemaCompat(ctx, pg); err == nil {
		t.Fatal("expected CheckSchemaCompat to fail on legacy schema")
	}

	// Run EnsureSchema — should create the missing table.
	if err := EnsureSchema(ctx, pg, "agentsview"); err != nil {
		t.Fatalf("EnsureSchema on legacy schema: %v", err)
	}

	// Now the compat check should pass.
	if err := CheckSchemaCompat(ctx, pg); err != nil {
		t.Fatalf(
			"CheckSchemaCompat after migration: %v", err,
		)
	}
}

func TestPushSingleSession(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

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
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := local.InsertMessages([]db.Message{
		{
			SessionID: "sess-001",
			Ordinal:   0,
			Role:      "user",
			Content:   firstMsg,
		},
	}); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	result, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if result.SessionsPushed != 1 {
		t.Errorf(
			"sessions pushed = %d, want 1",
			result.SessionsPushed,
		)
	}
	if result.MessagesPushed != 1 {
		t.Errorf(
			"messages pushed = %d, want 1",
			result.MessagesPushed,
		)
	}

	var pgProject, pgMachine string
	err = ps.pg.QueryRowContext(ctx,
		"SELECT project, machine FROM sessions WHERE id = $1",
		"sess-001",
	).Scan(&pgProject, &pgMachine)
	if err != nil {
		t.Fatalf("querying pg session: %v", err)
	}
	if pgProject != "test-project" {
		t.Errorf(
			"pg project = %q, want %q",
			pgProject, "test-project",
		)
	}
	if pgMachine != "test-machine" {
		t.Errorf(
			"pg machine = %q, want %q",
			pgMachine, "test-machine",
		)
	}

	var pgMsgContent string
	err = ps.pg.QueryRowContext(ctx,
		"SELECT content FROM messages WHERE session_id = $1 AND ordinal = 0",
		"sess-001",
	).Scan(&pgMsgContent)
	if err != nil {
		t.Fatalf("querying pg message: %v", err)
	}
	if pgMsgContent != firstMsg {
		t.Errorf(
			"pg message content = %q, want %q",
			pgMsgContent, firstMsg,
		)
	}
}

func TestPushIdempotent(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:           "sess-002",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 0,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	result1, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if result1.SessionsPushed != 1 {
		t.Errorf(
			"first push sessions = %d, want 1",
			result1.SessionsPushed,
		)
	}

	result2, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if result2.SessionsPushed != 0 {
		t.Errorf(
			"second push sessions = %d, want 0",
			result2.SessionsPushed,
		)
	}
}

func TestPushWithToolCalls(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:           "sess-tc-001",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 1,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := local.InsertMessages([]db.Message{
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
	}); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	result, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if result.MessagesPushed != 1 {
		t.Errorf(
			"messages pushed = %d, want 1",
			result.MessagesPushed,
		)
	}

	var toolName string
	var resultLen int
	err = ps.pg.QueryRowContext(ctx,
		"SELECT tool_name, result_content_length FROM tool_calls WHERE session_id = $1",
		"sess-tc-001",
	).Scan(&toolName, &resultLen)
	if err != nil {
		t.Fatalf("querying pg tool_call: %v", err)
	}
	if toolName != "Read" {
		t.Errorf(
			"tool_name = %q, want %q", toolName, "Read",
		)
	}
	if resultLen != 42 {
		t.Errorf(
			"result_content_length = %d, want 42",
			resultLen,
		)
	}
}

func TestPushWithToolResultEvents(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	sess := db.Session{
		ID:           "sess-events-001",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "codex",
		MessageCount: 1,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := local.InsertMessages([]db.Message{
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
	}); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	if _, err := ps.Push(ctx, false); err != nil {
		t.Fatalf("push: %v", err)
	}

	var count int
	err = ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_result_events WHERE session_id = $1",
		"sess-events-001",
	).Scan(&count)
	if err != nil {
		t.Fatalf("querying pg tool_result_events: %v", err)
	}
	if count != 1 {
		t.Fatalf("pg tool_result_events = %d, want 1", count)
	}
}

func TestStatus(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	status, err := ps.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Machine != "test-machine" {
		t.Errorf(
			"machine = %q, want %q",
			status.Machine, "test-machine",
		)
	}
	if status.PGSessions != 0 {
		t.Errorf(
			"pg sessions = %d, want 0",
			status.PGSessions,
		)
	}
}

func TestStatusMissingSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	status, err := ps.Status(ctx)
	if err != nil {
		t.Fatalf("status on missing schema: %v", err)
	}
	if status.PGSessions != 0 {
		t.Errorf(
			"pg sessions = %d, want 0",
			status.PGSessions,
		)
	}
	if status.PGMessages != 0 {
		t.Errorf(
			"pg messages = %d, want 0",
			status.PGMessages,
		)
	}
	if status.Machine != "test-machine" {
		t.Errorf(
			"machine = %q, want %q",
			status.Machine, "test-machine",
		)
	}
}

func TestNewRejectsMachineLocal(t *testing.T) {
	pgURL := testPGURL(t)
	local := testDB(t)
	_, err := New(
		pgURL, "agentsview", local, "local", true,
	)
	if err == nil {
		t.Fatal("expected error for machine=local")
	}
}

func TestNewRejectsEmptyMachine(t *testing.T) {
	pgURL := testPGURL(t)
	local := testDB(t)
	_, err := New(
		pgURL, "agentsview", local, "", true,
	)
	if err == nil {
		t.Fatal("expected error for empty machine")
	}
}

func TestNewRejectsEmptyURL(t *testing.T) {
	local := testDB(t)
	_, err := New(
		"", "agentsview", local, "test", true,
	)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestPushUpdatedAtFormat(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:        "sess-ts-001",
		Project:   "test-project",
		Machine:   "local",
		Agent:     "claude",
		StartedAt: &started,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	if _, err := ps.Push(ctx, false); err != nil {
		t.Fatalf("push: %v", err)
	}

	var updatedAt time.Time
	err = ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM sessions WHERE id = $1",
		"sess-ts-001",
	).Scan(&updatedAt)
	if err != nil {
		t.Fatalf("querying updated_at: %v", err)
	}

	formatted := updatedAt.UTC().Format(
		"2006-01-02T15:04:05.000000Z",
	)
	pattern := regexp.MustCompile(
		`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`,
	)
	if !pattern.MatchString(formatted) {
		t.Errorf(
			"updated_at = %q, want ISO-8601 "+
				"microsecond format", formatted,
		)
	}
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
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	started := time.Now().UTC().Format(time.RFC3339)
	sess := db.Session{
		ID:           "sess-bump-001",
		Project:      "test",
		Machine:      "local",
		Agent:        "test-agent",
		StartedAt:    &started,
		MessageCount: 1,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	msg := db.Message{
		SessionID:     "sess-bump-001",
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: 5,
	}
	if err := local.ReplaceSessionMessages(
		"sess-bump-001", []db.Message{msg},
	); err != nil {
		t.Fatalf("replace messages: %v", err)
	}

	if _, err := ps.Push(ctx, false); err != nil {
		t.Fatalf("initial push: %v", err)
	}

	var updatedAt1 time.Time
	if err := ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM sessions WHERE id = $1",
		"sess-bump-001",
	).Scan(&updatedAt1); err != nil {
		t.Fatalf("querying updated_at: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	result, err := ps.Push(ctx, true)
	if err != nil {
		t.Fatalf("full push: %v", err)
	}
	if result.MessagesPushed == 0 {
		t.Fatal(
			"expected messages to be pushed on full push",
		)
	}

	var updatedAt2 time.Time
	if err := ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM sessions WHERE id = $1",
		"sess-bump-001",
	).Scan(&updatedAt2); err != nil {
		t.Fatalf(
			"querying updated_at after full push: %v",
			err,
		)
	}

	if !updatedAt2.After(updatedAt1) {
		t.Errorf(
			"updated_at not bumped: before=%v, after=%v",
			updatedAt1, updatedAt2,
		)
	}
}

func TestPushFullBypassesHeuristic(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	started := "2026-03-11T12:00:00Z"
	sess := db.Session{
		ID:           "sess-full-001",
		Project:      "test-project",
		Machine:      "local",
		Agent:        "claude",
		StartedAt:    &started,
		MessageCount: 1,
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := local.InsertMessages([]db.Message{
		{
			SessionID: "sess-full-001",
			Ordinal:   0,
			Role:      "user",
			Content:   "test",
		},
	}); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	if _, err := ps.Push(ctx, false); err != nil {
		t.Fatalf("first push: %v", err)
	}

	if err := local.SetSyncState(
		"last_push_at", "",
	); err != nil {
		t.Fatalf("resetting watermark: %v", err)
	}

	result, err := ps.Push(ctx, true)
	if err != nil {
		t.Fatalf("full push: %v", err)
	}
	if result.SessionsPushed != 1 {
		t.Errorf(
			"full push sessions = %d, want 1",
			result.SessionsPushed,
		)
	}
	if result.MessagesPushed != 1 {
		t.Errorf(
			"full push messages = %d, want 1",
			result.MessagesPushed,
		)
	}
}

func TestPushDetectsSchemaReset(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

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
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if err := local.InsertMessages([]db.Message{{
		SessionID:     "sess-reset-001",
		Ordinal:       0,
		Role:          "user",
		Content:       "hello",
		ContentLength: 5,
	}}); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	r1, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("initial push: %v", err)
	}
	if r1.SessionsPushed != 1 {
		t.Fatalf(
			"initial push sessions = %d, want 1",
			r1.SessionsPushed,
		)
	}

	// Simulate a PG schema reset — don't manually recreate;
	// let Push detect and handle it via the coherence check.
	cleanPGSchema(t, pgURL)

	// An incremental push should detect the mismatch
	// (local watermark set, PG has 0 sessions), recreate
	// the schema, and automatically force a full push.
	r2, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("post-reset push: %v", err)
	}
	if r2.SessionsPushed != 1 {
		t.Errorf(
			"post-reset push sessions = %d, want 1 "+
				"(should auto-detect schema reset)",
			r2.SessionsPushed,
		)
	}
	if r2.MessagesPushed != 1 {
		t.Errorf(
			"post-reset push messages = %d, want 1",
			r2.MessagesPushed,
		)
	}
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
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	ctx := context.Background()

	sess := db.Session{
		ID:        "sess-full-drop",
		Project:   "proj",
		Machine:   "test-machine",
		Agent:     "claude",
		CreatedAt: "2026-03-11T12:00:00.000Z",
	}
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}

	r1, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("initial push: %v", err)
	}
	if r1.SessionsPushed != 1 {
		t.Fatalf(
			"initial push sessions = %d, want 1",
			r1.SessionsPushed,
		)
	}

	// Drop the schema without clearing local state.
	cleanPGSchema(t, pgURL)

	// A full push should recreate the schema even though
	// schemaDone is memoized from the first push.
	r2, err := ps.Push(ctx, true)
	if err != nil {
		t.Fatalf("full push after drop: %v", err)
	}
	if r2.SessionsPushed != 1 {
		t.Errorf(
			"full push sessions = %d, want 1",
			r2.SessionsPushed,
		)
	}
}

func TestPushBatchesMultipleSessions(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

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
		if err := local.UpsertSession(sess); err != nil {
			t.Fatalf("upsert session %d: %v", i, err)
		}
		if err := local.InsertMessages([]db.Message{
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
		}); err != nil {
			t.Fatalf("insert messages %d: %v", i, err)
		}
	}

	result, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if result.SessionsPushed != totalSessions {
		t.Errorf(
			"sessions pushed = %d, want %d",
			result.SessionsPushed, totalSessions,
		)
	}
	if result.MessagesPushed != totalSessions*2 {
		t.Errorf(
			"messages pushed = %d, want %d",
			result.MessagesPushed, totalSessions*2,
		)
	}
	if result.Errors != 0 {
		t.Errorf("errors = %d, want 0", result.Errors)
	}

	// Verify PG state.
	var pgSessions, pgMessages int
	if err := ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions",
	).Scan(&pgSessions); err != nil {
		t.Fatalf("counting pg sessions: %v", err)
	}
	if err := ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages",
	).Scan(&pgMessages); err != nil {
		t.Fatalf("counting pg messages: %v", err)
	}
	if pgSessions != totalSessions {
		t.Errorf(
			"pg sessions = %d, want %d",
			pgSessions, totalSessions,
		)
	}
	if pgMessages != totalSessions*2 {
		t.Errorf(
			"pg messages = %d, want %d",
			pgMessages, totalSessions*2,
		)
	}
}

func TestPushBulkInsertManyMessages(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

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
	if err := local.UpsertSession(sess); err != nil {
		t.Fatalf("upsert session: %v", err)
	}
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
	if err := local.InsertMessages(msgs); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	result, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if result.SessionsPushed != 1 {
		t.Errorf(
			"sessions pushed = %d, want 1",
			result.SessionsPushed,
		)
	}
	if result.MessagesPushed != msgCount {
		t.Errorf(
			"messages pushed = %d, want %d",
			result.MessagesPushed, msgCount,
		)
	}

	// Verify all messages landed in PG.
	var pgMsgCount int
	if err := ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM messages WHERE session_id = $1",
		"bulk-msg-sess",
	).Scan(&pgMsgCount); err != nil {
		t.Fatalf("counting pg messages: %v", err)
	}
	if pgMsgCount != msgCount {
		t.Errorf(
			"pg messages = %d, want %d",
			pgMsgCount, msgCount,
		)
	}

	// Verify tool calls landed.
	var pgTCCount int
	if err := ps.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM tool_calls WHERE session_id = $1",
		"bulk-msg-sess",
	).Scan(&pgTCCount); err != nil {
		t.Fatalf("counting pg tool_calls: %v", err)
	}
	// Every 10th assistant message (ordinals 1, 11, 21, ...).
	expectedTC := 0
	for i := range msgCount {
		if i%2 == 1 && i%10 == 1 {
			expectedTC++
		}
	}
	if pgTCCount != expectedTC {
		t.Errorf(
			"pg tool_calls = %d, want %d",
			pgTCCount, expectedTC,
		)
	}
}

func TestPushSimplePK(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(
		pgURL, "agentsview", local,
		"test-machine", true,
	)
	if err != nil {
		t.Fatalf("creating sync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	var constraintDef string
	err = ps.pg.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_namespace n ON n.oid = c.connamespace
		WHERE n.nspname = 'agentsview'
		  AND c.conrelid = 'agentsview.sessions'::regclass
		  AND c.contype = 'p'
	`).Scan(&constraintDef)
	if err != nil {
		t.Fatalf("querying sessions PK: %v", err)
	}
	if constraintDef != "PRIMARY KEY (id)" {
		t.Errorf(
			"sessions PK = %q, want PRIMARY KEY (id)",
			constraintDef,
		)
	}

	err = ps.pg.QueryRowContext(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_namespace n ON n.oid = c.connamespace
		WHERE n.nspname = 'agentsview'
		  AND c.conrelid = 'agentsview.messages'::regclass
		  AND c.contype = 'p'
	`).Scan(&constraintDef)
	if err != nil {
		t.Fatalf("querying messages PK: %v", err)
	}
	if constraintDef != "PRIMARY KEY (session_id, ordinal)" {
		t.Errorf(
			"messages PK = %q, "+
				"want PRIMARY KEY (session_id, ordinal)",
			constraintDef,
		)
	}
}
