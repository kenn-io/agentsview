//go:build pgtest

package postgres

import (
	"context"
	"testing"

	"github.com/wesm/agentsview/internal/db"
)

const testSchema = "agentsview_store_test"

// ensureStoreSchema creates the test schema and seed data.
func ensureStoreSchema(t *testing.T, pgURL string) {
	t.Helper()
	pg, err := Open(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("connecting to pg: %v", err)
	}
	defer pg.Close()

	_, err = pg.Exec(`
		DROP SCHEMA IF EXISTS ` + testSchema + ` CASCADE;
	`)
	if err != nil {
		t.Fatalf("dropping schema: %v", err)
	}

	ctx := context.Background()
	if err := EnsureSchema(ctx, pg, testSchema); err != nil {
		t.Fatalf("creating schema: %v", err)
	}

	_, err = pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('store-test-001', 'test-machine',
			 'test-project', 'claude-code',
			 'hello world',
			 '2026-03-12T10:00:00Z'::timestamptz,
			 '2026-03-12T10:30:00Z'::timestamptz,
			 2, 1)
	`)
	if err != nil {
		t.Fatalf("inserting test session: %v", err)
	}
	_, err = pg.Exec(`
		INSERT INTO messages
			(session_id, ordinal, role, content,
			 timestamp, content_length)
		VALUES
			('store-test-001', 0, 'user',
			 'hello world',
			 '2026-03-12T10:00:00Z'::timestamptz, 11),
			('store-test-001', 1, 'assistant',
			 'hi there',
			 '2026-03-12T10:00:01Z'::timestamptz, 8)
	`)
	if err != nil {
		t.Fatalf("inserting test messages: %v", err)
	}
}

func TestNewStore(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	if !store.ReadOnly() {
		t.Error("ReadOnly() = false, want true")
	}
	if !store.HasFTS() {
		t.Error("HasFTS() = false, want true")
	}
}

func TestStoreListSessions(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	page, err := store.ListSessions(
		ctx, db.SessionFilter{Limit: 10},
	)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if page.Total == 0 {
		t.Error("expected at least 1 session")
	}
	t.Logf("sessions: %d, total: %d",
		len(page.Sessions), page.Total)
}

func TestStoreGetSession(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess, err := store.GetSession(ctx, "store-test-001")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if sess.Project != "test-project" {
		t.Errorf("project = %q, want %q",
			sess.Project, "test-project")
	}
}

func TestStoreGetMessages(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	msgs, err := store.GetMessages(
		ctx, "store-test-001", 0, 100, true,
	)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Errorf("got %d messages, want 2", len(msgs))
	}
}

func TestStoreGetStats(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
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

func TestStoreSearch(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
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

func TestStoreGetMinimap(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	entries, err := store.GetMinimap(
		ctx, "store-test-001",
	)
	if err != nil {
		t.Fatalf("GetMinimap: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d entries, want 2", len(entries))
	}
}

func TestStoreAnalyticsSummary(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	summary, err := store.GetAnalyticsSummary(
		ctx, db.AnalyticsFilter{
			From: "2026-01-01",
			To:   "2026-12-31",
		},
	)
	if err != nil {
		t.Fatalf("GetAnalyticsSummary: %v", err)
	}
	if summary.TotalSessions == 0 {
		t.Error("expected at least 1 session in summary")
	}
	t.Logf("summary: %+v", summary)
}

func TestStoreWriteMethodsReturnReadOnly(t *testing.T) {
	pgURL := testPGURL(t)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	tests := []struct {
		name string
		fn   func() error
	}{
		{"StarSession", func() error {
			_, err := store.StarSession("x")
			return err
		}},
		{"UnstarSession", func() error {
			return store.UnstarSession("x")
		}},
		{"BulkStarSessions", func() error {
			return store.BulkStarSessions([]string{"x"})
		}},
		{"PinMessage", func() error {
			_, err := store.PinMessage("x", 1, nil)
			return err
		}},
		{"UnpinMessage", func() error {
			return store.UnpinMessage("x", 1)
		}},
		{"InsertInsight", func() error {
			_, err := store.InsertInsight(db.Insight{})
			return err
		}},
		{"DeleteInsight", func() error {
			return store.DeleteInsight(1)
		}},
		{"RenameSession", func() error {
			return store.RenameSession("x", nil)
		}},
		{"SoftDeleteSession", func() error {
			return store.SoftDeleteSession("x")
		}},
		{"RestoreSession", func() error {
			_, err := store.RestoreSession("x")
			return err
		}},
		{"DeleteSessionIfTrashed", func() error {
			_, err := store.DeleteSessionIfTrashed("x")
			return err
		}},
		{"EmptyTrash", func() error {
			_, err := store.EmptyTrash()
			return err
		}},
		{"UpsertSession", func() error {
			return store.UpsertSession(db.Session{})
		}},
		{"ReplaceSessionMessages", func() error {
			return store.ReplaceSessionMessages("x", nil)
		}},
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
