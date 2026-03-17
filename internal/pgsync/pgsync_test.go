//go:build pgtest

package pgsync

import (
	"context"
	"database/sql"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

func testPGURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_PG_URL")
	if url == "" {
		t.Skip("TEST_PG_URL not set; skipping PG tests")
	}
	return url
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(
		t.TempDir() + "/test.db",
	)
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
	_, _ = pg.Exec("DROP SCHEMA IF EXISTS agentsview CASCADE")
}

func TestEnsureSchemaIdempotent(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()

	// First call creates schema.
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("first EnsureSchema: %v", err)
	}

	// Second call should be idempotent.
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
}

func TestPushSingleSession(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// Insert a session and message locally.
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

	// Push to PG.
	result, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if result.SessionsPushed != 1 {
		t.Errorf("sessions pushed = %d, want 1", result.SessionsPushed)
	}
	if result.MessagesPushed != 1 {
		t.Errorf("messages pushed = %d, want 1", result.MessagesPushed)
	}

	// Verify in PG.
	var pgProject, pgMachine string
	err = ps.pg.QueryRowContext(ctx,
		"SELECT project, machine FROM agentsview.sessions WHERE id = $1",
		"sess-001",
	).Scan(&pgProject, &pgMachine)
	if err != nil {
		t.Fatalf("querying pg session: %v", err)
	}
	if pgProject != "test-project" {
		t.Errorf("pg project = %q, want %q", pgProject, "test-project")
	}
	if pgMachine != "test-machine" {
		t.Errorf("pg machine = %q, want %q", pgMachine, "test-machine")
	}

	// Verify messages in PG (no machine column).
	var pgMsgContent string
	err = ps.pg.QueryRowContext(ctx,
		"SELECT content FROM agentsview.messages WHERE session_id = $1 AND ordinal = 0",
		"sess-001",
	).Scan(&pgMsgContent)
	if err != nil {
		t.Fatalf("querying pg message: %v", err)
	}
	if pgMsgContent != firstMsg {
		t.Errorf("pg message content = %q, want %q", pgMsgContent, firstMsg)
	}
}

func TestPushIdempotent(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
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

	// Push twice.
	result1, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if result1.SessionsPushed != 1 {
		t.Errorf("first push sessions = %d, want 1", result1.SessionsPushed)
	}

	// Second push should find nothing new.
	result2, err := ps.Push(ctx, false)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if result2.SessionsPushed != 0 {
		t.Errorf("second push sessions = %d, want 0", result2.SessionsPushed)
	}
}

func TestPushWithToolCalls(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
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
					SubagentSessionID:   "",
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
		t.Errorf("messages pushed = %d, want 1", result.MessagesPushed)
	}

	// Verify tool call in PG.
	var toolName string
	var resultLen int
	err = ps.pg.QueryRowContext(ctx,
		"SELECT tool_name, result_content_length FROM agentsview.tool_calls WHERE session_id = $1",
		"sess-tc-001",
	).Scan(&toolName, &resultLen)
	if err != nil {
		t.Fatalf("querying pg tool_call: %v", err)
	}
	if toolName != "Read" {
		t.Errorf("tool_name = %q, want %q", toolName, "Read")
	}
	if resultLen != 42 {
		t.Errorf("result_content_length = %d, want 42", resultLen)
	}
}

func TestStatus(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
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
		t.Errorf("machine = %q, want %q", status.Machine, "test-machine")
	}
	if status.PGSessions != 0 {
		t.Errorf("pg sessions = %d, want 0", status.PGSessions)
	}
}

func TestStatusMissingSchema(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
	}
	defer ps.Close()

	// Status without EnsureSchema should return zeros, not an error.
	ctx := context.Background()
	status, err := ps.Status(ctx)
	if err != nil {
		t.Fatalf("status on missing schema: %v", err)
	}
	if status.PGSessions != 0 {
		t.Errorf("pg sessions = %d, want 0", status.PGSessions)
	}
	if status.PGMessages != 0 {
		t.Errorf("pg messages = %d, want 0", status.PGMessages)
	}
	if status.Machine != "test-machine" {
		t.Errorf("machine = %q, want %q", status.Machine, "test-machine")
	}
}

func TestNewRejectsMachineLocal(t *testing.T) {
	pgURL := testPGURL(t)
	local := testDB(t)
	_, err := New(pgURL, local, "local", time.Hour, true)
	if err == nil {
		t.Fatal("expected error for machine=local")
	}
}

func TestNewRejectsEmptyMachine(t *testing.T) {
	pgURL := testPGURL(t)
	local := testDB(t)
	_, err := New(pgURL, local, "", time.Hour, true)
	if err == nil {
		t.Fatal("expected error for empty machine")
	}
}

func TestNewRejectsEmptyURL(t *testing.T) {
	local := testDB(t)
	_, err := New("", local, "test", time.Hour, true)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestPushUpdatedAtFormat(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
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

	var updatedAt string
	err = ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM agentsview.sessions WHERE id = $1",
		"sess-ts-001",
	).Scan(&updatedAt)
	if err != nil {
		t.Fatalf("querying updated_at: %v", err)
	}

	// Should match ISO-8601 microsecond format.
	pattern := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`)
	if !pattern.MatchString(updatedAt) {
		t.Errorf("updated_at = %q, want ISO-8601 microsecond format", updatedAt)
	}
}

func TestPushBumpsUpdatedAtOnMessageRewrite(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "machine-a", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
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
	if err := local.ReplaceSessionMessages("sess-bump-001", []db.Message{msg}); err != nil {
		t.Fatalf("replace messages: %v", err)
	}

	// Initial push.
	if _, err := ps.Push(ctx, false); err != nil {
		t.Fatalf("initial push: %v", err)
	}

	var updatedAt1 string
	if err := ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM agentsview.sessions WHERE id = $1",
		"sess-bump-001",
	).Scan(&updatedAt1); err != nil {
		t.Fatalf("querying updated_at: %v", err)
	}

	// Sleep briefly so the timestamp changes.
	time.Sleep(50 * time.Millisecond)

	// Full push (forces message rewrite even though content unchanged).
	result, err := ps.Push(ctx, true)
	if err != nil {
		t.Fatalf("full push: %v", err)
	}
	if result.MessagesPushed == 0 {
		t.Fatal("expected messages to be pushed on full push")
	}

	var updatedAt2 string
	if err := ps.pg.QueryRowContext(ctx,
		"SELECT updated_at FROM agentsview.sessions WHERE id = $1",
		"sess-bump-001",
	).Scan(&updatedAt2); err != nil {
		t.Fatalf("querying updated_at after full push: %v", err)
	}

	if updatedAt2 <= updatedAt1 {
		t.Errorf("updated_at not bumped: before=%q, after=%q",
			updatedAt1, updatedAt2)
	}
}

func TestPushFullBypassesHeuristic(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
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
		{SessionID: "sess-full-001", Ordinal: 0, Role: "user", Content: "test"},
	}); err != nil {
		t.Fatalf("insert messages: %v", err)
	}

	// First push.
	if _, err := ps.Push(ctx, false); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// Full push should re-push messages even though nothing changed.
	// We need to reset the watermark to force the session into scope.
	if err := local.SetSyncState("last_push_at", ""); err != nil {
		t.Fatalf("resetting watermark: %v", err)
	}

	result, err := ps.Push(ctx, true)
	if err != nil {
		t.Fatalf("full push: %v", err)
	}
	if result.SessionsPushed != 1 {
		t.Errorf("full push sessions = %d, want 1", result.SessionsPushed)
	}
	if result.MessagesPushed != 1 {
		t.Errorf("full push messages = %d, want 1", result.MessagesPushed)
	}
}

func TestPushSimplePK(t *testing.T) {
	pgURL := testPGURL(t)
	cleanPGSchema(t, pgURL)
	t.Cleanup(func() { cleanPGSchema(t, pgURL) })

	local := testDB(t)
	ps, err := New(pgURL, local, "test-machine", time.Hour, true)
	if err != nil {
		t.Fatalf("creating pgsync: %v", err)
	}
	defer ps.Close()

	ctx := context.Background()
	if err := ps.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	// Verify sessions PK is just (id), not (id, machine).
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
		t.Errorf("sessions PK = %q, want PRIMARY KEY (id)", constraintDef)
	}

	// Verify messages PK is (session_id, ordinal).
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
		t.Errorf("messages PK = %q, want PRIMARY KEY (session_id, ordinal)", constraintDef)
	}
}
