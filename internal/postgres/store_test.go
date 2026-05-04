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

func ensureAnalyticsTokenStoreSchema(
	t *testing.T, pgURL string,
) {
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
		INSERT INTO sessions (
			id, machine, project, agent, first_message,
			started_at, ended_at, message_count,
			user_message_count, total_output_tokens,
			has_total_output_tokens
		) VALUES
			('pg-token-001', 'test-machine', 'proj-a', 'claude',
			 'largest token session',
			 '2026-03-12T10:00:00Z'::timestamptz,
			 '2026-03-12T10:30:00Z'::timestamptz,
			 12, 6, 900, TRUE),
			('pg-token-002', 'test-machine', 'proj-a', 'codex',
			 'second token session',
			 '2026-03-12T12:00:00Z'::timestamptz,
			 '2026-03-12T12:15:00Z'::timestamptz,
			 8, 4, 600, TRUE),
			('pg-token-003', 'test-machine', 'proj-b', 'claude',
			 'third token session',
			 '2026-03-13T09:00:00Z'::timestamptz,
			 '2026-03-13T09:10:00Z'::timestamptz,
			 5, 3, 300, TRUE),
			('pg-token-missing', 'test-machine', 'proj-c', 'claude',
			 'missing token coverage',
			 '2026-03-13T11:00:00Z'::timestamptz,
			 '2026-03-13T11:20:00Z'::timestamptz,
			 9, 5, 0, FALSE)
	`)
	if err != nil {
		t.Fatalf("inserting analytics token sessions: %v", err)
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

func TestStoreListSessions_MachineMultiSelect(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	_, err = store.DB().Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			('store-test-002', 'machine-b',
			 'test-project', 'codex',
			 'hello machine b',
			 '2026-03-12T11:00:00Z'::timestamptz,
			 '2026-03-12T11:30:00Z'::timestamptz,
			 2, 1),
			('store-test-003', 'machine-c',
			 'test-project', 'gemini',
			 'hello machine c',
			 '2026-03-12T12:00:00Z'::timestamptz,
			 '2026-03-12T12:30:00Z'::timestamptz,
			 2, 1)
	`)
	if err != nil {
		t.Fatalf("inserting extra sessions: %v", err)
	}

	ctx := context.Background()
	page, err := store.ListSessions(
		ctx,
		db.SessionFilter{
			Machine: "test-machine,machine-c",
			Limit:   10,
		},
	)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if page.Total != 2 {
		t.Fatalf("total = %d, want 2", page.Total)
	}
	got := []string{
		page.Sessions[0].Machine,
		page.Sessions[1].Machine,
	}
	if got[0] != "test-machine" && got[1] != "test-machine" {
		t.Fatalf("machines = %v, want test-machine included", got)
	}
	if got[0] != "machine-c" && got[1] != "machine-c" {
		t.Fatalf("machines = %v, want machine-c included", got)
	}
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
	stats, err := store.GetStats(ctx, false, false)
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

func seedActivitySession(
	t *testing.T, store *Store, sid string, msgs []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	},
) {
	t.Helper()
	pg := store.DB()

	// PG doesn't allow multi-statement prepared queries,
	// so run each statement separately.
	if _, err := pg.Exec(
		`DELETE FROM messages WHERE session_id = $1`, sid,
	); err != nil {
		t.Fatalf("deleting messages: %v", err)
	}
	if _, err := pg.Exec(
		`DELETE FROM sessions WHERE id = $1`, sid,
	); err != nil {
		t.Fatalf("deleting session: %v", err)
	}
	if _, err := pg.Exec(`
		INSERT INTO sessions
			(id, machine, project, agent, first_message,
			 started_at, ended_at, message_count,
			 user_message_count)
		VALUES
			($1, 'test-machine', 'test-project',
			 'claude', 'activity test',
			 '2026-03-26T10:00:00Z'::timestamptz,
			 '2026-03-26T11:00:00Z'::timestamptz,
			 $2, 0)
	`, sid, len(msgs)); err != nil {
		t.Fatalf("inserting session: %v", err)
	}

	for _, m := range msgs {
		var tsVal interface{} = nil
		if m.ts != "" {
			tsVal = m.ts
		}
		if _, err := pg.Exec(`
			INSERT INTO messages
				(session_id, ordinal, role, content,
				 timestamp, content_length, is_system)
			VALUES ($1, $2, $3, $4,
				$5::timestamptz, $6, $7)
		`, sid, m.ordinal, m.role, m.content,
			tsVal, len(m.content), m.system); err != nil {
			t.Fatalf("inserting message ord=%d: %v",
				m.ordinal, err)
		}
	}
}

func TestStoreGetSessionActivity(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sid := "store-test-activity"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "hello", "2026-03-26T10:00:00Z", false},
		{1, "assistant", "hi", "2026-03-26T10:00:30Z", false},
		{2, "user", "next", "2026-03-26T10:01:30Z", false},
		{3, "assistant", "resp", "2026-03-26T10:02:00Z", false},
		{4, "user", "back", "2026-03-26T10:28:00Z", false},
		{5, "assistant", "wb", "2026-03-26T10:29:00Z", false},
		// System message — excluded from buckets.
		{6, "user", "This session is being continued from a previous conversation.", "2026-03-26T10:29:30Z", true},
	})

	ctx := context.Background()
	resp, err := store.GetSessionActivity(ctx, sid)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}

	if resp.IntervalSeconds != 60 {
		t.Errorf("interval = %d, want 60",
			resp.IntervalSeconds)
	}

	if resp.TotalMessages != 7 {
		t.Errorf("total = %d, want 7",
			resp.TotalMessages)
	}

	if len(resp.Buckets) < 28 {
		t.Errorf("bucket count = %d, want >= 28",
			len(resp.Buckets))
	}

	first := resp.Buckets[0]
	if first.UserCount != 1 || first.AssistantCount != 1 {
		t.Errorf("first bucket: user=%d asst=%d, want 1,1",
			first.UserCount, first.AssistantCount)
	}
	if first.FirstOrdinal == nil || *first.FirstOrdinal != 0 {
		t.Errorf("first bucket first_ordinal: got %v, want 0",
			first.FirstOrdinal)
	}

	mid := resp.Buckets[15]
	if mid.UserCount != 0 || mid.AssistantCount != 0 {
		t.Errorf("mid bucket: user=%d asst=%d, want 0,0",
			mid.UserCount, mid.AssistantCount)
	}
	if mid.FirstOrdinal != nil {
		t.Errorf("mid bucket first_ordinal: got %v, want nil",
			mid.FirstOrdinal)
	}
}

func TestStoreGetSessionActivity_NoMessages(t *testing.T) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sid := "store-test-activity-empty"
	seedActivitySession(t, store, sid, nil)

	resp, err := store.GetSessionActivity(
		context.Background(), sid,
	)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}
	if len(resp.Buckets) != 0 {
		t.Errorf("buckets = %d, want 0",
			len(resp.Buckets))
	}
}

func TestStoreGetSessionActivity_NullTimestamps(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sid := "store-test-activity-nullts"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "hi", "", false},
		{1, "assistant", "hello", "", false},
	})

	resp, err := store.GetSessionActivity(
		context.Background(), sid,
	)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}
	if len(resp.Buckets) != 0 {
		t.Errorf("buckets = %d, want 0",
			len(resp.Buckets))
	}
	if resp.TotalMessages != 2 {
		t.Errorf("total = %d, want 2",
			resp.TotalMessages)
	}
}

func TestStoreGetSessionActivity_SingleMessage(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sid := "store-test-activity-single"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "hi", "2026-03-26T10:00:00Z", false},
	})

	resp, err := store.GetSessionActivity(
		context.Background(), sid,
	)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}
	if len(resp.Buckets) != 1 {
		t.Fatalf("buckets = %d, want 1",
			len(resp.Buckets))
	}
	if resp.Buckets[0].UserCount != 1 {
		t.Errorf("user count = %d, want 1",
			resp.Buckets[0].UserCount)
	}
}

func TestStoreGetSessionActivity_PrefixInjectedExcluded(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sid := "store-test-activity-prefix"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "hello", "2026-03-26T10:00:00Z", false},
		{1, "assistant", "hi", "2026-03-26T10:00:30Z", false},
		// Prefix-detected injected message: is_system=false but
		// content starts with a system prefix.
		{2, "user", "This session is being continued from a previous conversation.", "2026-03-26T10:01:00Z", false},
	})

	ctx := context.Background()
	resp, err := store.GetSessionActivity(ctx, sid)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}

	// The prefix-detected message should be excluded from
	// buckets but still count toward TotalMessages.
	if resp.TotalMessages != 3 {
		t.Errorf("total = %d, want 3",
			resp.TotalMessages)
	}

	// Only ordinals 0 and 1 should appear in buckets.
	totalBucketed := 0
	for _, b := range resp.Buckets {
		totalBucketed += b.UserCount + b.AssistantCount
	}
	if totalBucketed != 2 {
		t.Errorf("bucketed messages = %d, want 2",
			totalBucketed)
	}

	// The excluded message at 10:01:00 must not extend the
	// timestamp range. With only 10:00:00-10:00:30 visible,
	// a single bucket should cover the entire span.
	if len(resp.Buckets) != 1 {
		t.Errorf("bucket count = %d, want 1",
			len(resp.Buckets))
	}
}

func TestStoreGetSessionActivity_FractionalTimestamps(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	sid := "store-test-frac-ts"
	seedActivitySession(t, store, sid, []struct {
		ordinal int
		role    string
		content string
		ts      string
		system  bool
	}{
		{0, "user", "a", "2026-03-26T10:00:00.900Z", false},
		{1, "assistant", "b", "2026-03-26T10:00:59.100Z", false},
		{2, "user", "c", "2026-03-26T10:01:01.000Z", false},
	})

	ctx := context.Background()
	resp, err := store.GetSessionActivity(ctx, sid)
	if err != nil {
		t.Fatalf("GetSessionActivity: %v", err)
	}

	if resp.IntervalSeconds != 60 {
		t.Fatalf(
			"interval = %d, want 60",
			resp.IntervalSeconds,
		)
	}

	if len(resp.Buckets) < 2 {
		t.Fatalf(
			"buckets = %d, want >= 2",
			len(resp.Buckets),
		)
	}

	// First bucket should have both sub-second messages.
	first := resp.Buckets[0]
	if first.UserCount != 1 || first.AssistantCount != 1 {
		t.Errorf(
			"first bucket: user=%d asst=%d, want 1,1",
			first.UserCount, first.AssistantCount,
		)
	}

	// Second bucket should have the third message.
	second := resp.Buckets[1]
	if second.UserCount != 1 {
		t.Errorf(
			"second bucket user=%d, want 1",
			second.UserCount,
		)
	}
}

func TestStoreAnalyticsSummaryOutputTokenCoverage(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureAnalyticsTokenStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	summary, err := store.GetAnalyticsSummary(
		context.Background(),
		db.AnalyticsFilter{
			From: "2026-03-12",
			To:   "2026-03-13",
		},
	)
	if err != nil {
		t.Fatalf("GetAnalyticsSummary: %v", err)
	}

	if summary.TotalOutputTokens != 1800 {
		t.Errorf(
			"TotalOutputTokens = %d, want 1800",
			summary.TotalOutputTokens,
		)
	}
	if summary.TokenReportingSessions != 3 {
		t.Errorf(
			"TokenReportingSessions = %d, want 3",
			summary.TokenReportingSessions,
		)
	}
}

func TestStoreAnalyticsHeatmapOutputTokens(t *testing.T) {
	pgURL := testPGURL(t)
	ensureAnalyticsTokenStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	heatmap, err := store.GetAnalyticsHeatmap(
		context.Background(),
		db.AnalyticsFilter{
			From: "2026-03-12",
			To:   "2026-03-13",
		},
		"output_tokens",
	)
	if err != nil {
		t.Fatalf("GetAnalyticsHeatmap: %v", err)
	}

	if heatmap.Metric != "output_tokens" {
		t.Fatalf(
			"Metric = %q, want %q",
			heatmap.Metric, "output_tokens",
		)
	}
	if len(heatmap.Entries) != 2 {
		t.Fatalf(
			"len(Entries) = %d, want 2",
			len(heatmap.Entries),
		)
	}
	if heatmap.Entries[0].Date != "2026-03-12" ||
		heatmap.Entries[0].Value != 1500 {
		t.Errorf(
			"Entries[0] = %+v, want date 2026-03-12 value 1500",
			heatmap.Entries[0],
		)
	}
	if heatmap.Entries[1].Date != "2026-03-13" ||
		heatmap.Entries[1].Value != 300 {
		t.Errorf(
			"Entries[1] = %+v, want date 2026-03-13 value 300",
			heatmap.Entries[1],
		)
	}
}

func TestStoreAnalyticsTopSessionsOutputTokens(
	t *testing.T,
) {
	pgURL := testPGURL(t)
	ensureAnalyticsTokenStoreSchema(t, pgURL)

	store, err := NewStore(pgURL, testSchema, true)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()

	top, err := store.GetAnalyticsTopSessions(
		context.Background(),
		db.AnalyticsFilter{
			From: "2026-03-12",
			To:   "2026-03-13",
		},
		"output_tokens",
	)
	if err != nil {
		t.Fatalf("GetAnalyticsTopSessions: %v", err)
	}

	if top.Metric != "output_tokens" {
		t.Fatalf(
			"Metric = %q, want %q",
			top.Metric, "output_tokens",
		)
	}
	if len(top.Sessions) != 3 {
		t.Fatalf(
			"len(Sessions) = %d, want 3",
			len(top.Sessions),
		)
	}
	if top.Sessions[0].ID != "pg-token-001" ||
		top.Sessions[0].OutputTokens != 900 {
		t.Errorf(
			"Sessions[0] = %+v, want pg-token-001 with 900 output tokens",
			top.Sessions[0],
		)
	}
	for _, session := range top.Sessions {
		if session.ID == "pg-token-missing" {
			t.Fatalf(
				"session without token coverage was included: %+v",
				session,
			)
		}
	}
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
		{"WriteSessionBatchAtomic", func() error {
			_, err := store.WriteSessionBatchAtomic(nil)
			return err
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
