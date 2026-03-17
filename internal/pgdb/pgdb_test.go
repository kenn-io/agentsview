//go:build pgtest

package pgdb

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
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

// ensureSchema creates the agentsview schema and test data.
func ensureSchema(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := sql.Open("pgx", pgURL)
	if err != nil {
		t.Fatalf("connecting to pg: %v", err)
	}
	defer pg.Close()

	// Drop and recreate schema so the test DDL is always current.
	_, err = pg.Exec(`
		DROP SCHEMA IF EXISTS agentsview CASCADE;
		CREATE SCHEMA agentsview;

		CREATE TABLE agentsview.sessions (
			id TEXT PRIMARY KEY,
			machine TEXT NOT NULL,
			project TEXT NOT NULL,
			agent TEXT NOT NULL,
			first_message TEXT,
			display_name TEXT,
			created_at TEXT NOT NULL DEFAULT '',
			started_at TEXT,
			ended_at TEXT,
			deleted_at TEXT,
			message_count INT NOT NULL DEFAULT 0,
			user_message_count INT NOT NULL DEFAULT 0,
			parent_session_id TEXT,
			relationship_type TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		);

		CREATE TABLE agentsview.messages (
			session_id TEXT NOT NULL,
			ordinal INT NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			timestamp TEXT,
			has_thinking BOOLEAN NOT NULL DEFAULT FALSE,
			has_tool_use BOOLEAN NOT NULL DEFAULT FALSE,
			content_length INT NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, ordinal)
		);

		CREATE TABLE agentsview.tool_calls (
			id BIGSERIAL PRIMARY KEY,
			session_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			category TEXT NOT NULL,
			call_index INT NOT NULL DEFAULT 0,
			tool_use_id TEXT NOT NULL DEFAULT '',
			input_json TEXT,
			skill_name TEXT,
			result_content_length INT,
			result_content TEXT,
			subagent_session_id TEXT,
			message_ordinal INT NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("creating schema: %v", err)
	}

	// Insert test data.
	_, err = pg.Exec(`
		INSERT INTO agentsview.sessions
			(id, machine, project, agent, first_message, started_at, ended_at, message_count, user_message_count)
		VALUES
			('pgdb-test-001', 'test-machine', 'test-project', 'claude-code',
			 'hello world', '2026-03-12T10:00:00Z', '2026-03-12T10:30:00Z', 2, 1)
	`)
	if err != nil {
		t.Fatalf("inserting test session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO agentsview.messages (session_id, ordinal, role, content, timestamp, content_length)
		VALUES
			('pgdb-test-001', 0, 'user', 'hello world', '2026-03-12T10:00:00Z', 11),
			('pgdb-test-001', 1, 'assistant', 'hi there', '2026-03-12T10:00:01Z', 8)
	`)
	if err != nil {
		t.Fatalf("inserting test messages: %v", err)
	}
}

func TestNew(t *testing.T) {
	pgURL := testPGURL(t)
	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	if !store.ReadOnly() {
		t.Error("ReadOnly() = false, want true")
	}
	if !store.HasFTS() {
		t.Error("HasFTS() = false, want true")
	}
}

func TestListSessions(t *testing.T) {
	pgURL := testPGURL(t)
	ensureSchema(t, pgURL)

	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.ListSessions(ctx, db.SessionFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if page.Total == 0 {
		t.Error("expected at least 1 session")
	}
	t.Logf("sessions: %d, total: %d", len(page.Sessions), page.Total)
}

func TestGetSession(t *testing.T) {
	pgURL := testPGURL(t)
	ensureSchema(t, pgURL)

	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetSession(ctx, "pgdb-test-001")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if sess.Project != "test-project" {
		t.Errorf("project = %q, want %q", sess.Project, "test-project")
	}
}

func TestGetMessages(t *testing.T) {
	pgURL := testPGURL(t)
	ensureSchema(t, pgURL)

	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	msgs, err := store.GetMessages(ctx, "pgdb-test-001", 0, 100, true)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want 2", len(msgs))
	}
}

func TestGetStats(t *testing.T) {
	pgURL := testPGURL(t)
	ensureSchema(t, pgURL)

	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	stats, err := store.GetStats(ctx, false)
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.SessionCount == 0 {
		t.Error("expected at least 1 session in stats")
	}
	t.Logf("stats: %+v", stats)
}

func TestSearch(t *testing.T) {
	pgURL := testPGURL(t)
	ensureSchema(t, pgURL)

	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.Search(ctx, db.SearchFilter{
		Query: "hello",
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) == 0 {
		t.Error("expected at least 1 search result")
	}
	t.Logf("search results: %d", len(page.Results))
}

func TestGetMinimap(t *testing.T) {
	pgURL := testPGURL(t)
	ensureSchema(t, pgURL)

	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	entries, err := store.GetMinimap(ctx, "pgdb-test-001")
	if err != nil {
		t.Fatalf("GetMinimap: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2", len(entries))
	}
}

func TestAnalyticsSummary(t *testing.T) {
	pgURL := testPGURL(t)
	ensureSchema(t, pgURL)

	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	summary, err := store.GetAnalyticsSummary(ctx, db.AnalyticsFilter{
		From: "2026-01-01",
		To:   "2026-12-31",
	})
	if err != nil {
		t.Fatalf("GetAnalyticsSummary: %v", err)
	}
	if summary.TotalSessions == 0 {
		t.Error("expected at least 1 session in summary")
	}
	t.Logf("summary: %+v", summary)
}

func TestWriteMethodsReturnReadOnly(t *testing.T) {
	pgURL := testPGURL(t)
	store, err := New(pgURL, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer store.Close()

	tests := []struct {
		name string
		fn   func() error
	}{
		{"StarSession", func() error { _, err := store.StarSession("x"); return err }},
		{"UnstarSession", func() error { return store.UnstarSession("x") }},
		{"BulkStarSessions", func() error { return store.BulkStarSessions([]string{"x"}) }},
		{"PinMessage", func() error { _, err := store.PinMessage("x", 1, nil); return err }},
		{"UnpinMessage", func() error { return store.UnpinMessage("x", 1) }},
		{"InsertInsight", func() error { _, err := store.InsertInsight(db.Insight{}); return err }},
		{"DeleteInsight", func() error { return store.DeleteInsight(1) }},
		{"RenameSession", func() error { return store.RenameSession("x", nil) }},
		{"SoftDeleteSession", func() error { return store.SoftDeleteSession("x") }},
		{"RestoreSession", func() error { _, err := store.RestoreSession("x"); return err }},
		{"DeleteSessionIfTrashed", func() error { _, err := store.DeleteSessionIfTrashed("x"); return err }},
		{"EmptyTrash", func() error { _, err := store.EmptyTrash(); return err }},
		{"UpsertSession", func() error { return store.UpsertSession(db.Session{}) }},
		{"ReplaceSessionMessages", func() error { return store.ReplaceSessionMessages("x", nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.fn()
			if err != db.ErrReadOnly {
				t.Errorf("got %v, want ErrReadOnly", err)
			}
		})
	}
}
