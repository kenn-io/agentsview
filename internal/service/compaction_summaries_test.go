package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// compactionSpy stands in for a daemon that ignores the summary filters.
type compactionSpy struct {
	service.SessionService
	detail         *service.SessionDetail
	limit          int
	reads          []string
	includeSummary bool
}

func (s *compactionSpy) Get(context.Context, string) (*service.SessionDetail, error) {
	return s.detail, nil
}

func (s *compactionSpy) List(_ context.Context, f service.ListFilter) (*service.SessionList, error) {
	s.limit = f.Limit
	return &service.SessionList{Sessions: []db.Session{{ID: "event", Agent: "roocode"}, {ID: "normal", Agent: "claude"}}, NextCursor: "next"}, nil
}

func (s *compactionSpy) Messages(_ context.Context, id string, _ service.MessageFilter) (*service.MessageList, error) {
	s.reads = append(s.reads, id)
	messages := []db.Message{
		{Content: "injected prompt", IsSystem: true},
		{Content: "ordinary message"},
		{Content: "[CHECKPOINT]", SourceType: "evener_empty_compaction", SourceSubtype: "CHECKPOINT", IsCompactBoundary: true},
		{Content: "  ", IsCompactBoundary: true},
	}
	if s.includeSummary {
		messages = append(messages, db.Message{Content: "retained summary", IsCompactBoundary: true})
	}
	return &service.MessageList{Messages: messages}, nil
}

func TestCompactionSummariesExcludedSessionDoesNotReadMessages(t *testing.T) {
	for _, detail := range []*service.SessionDetail{nil, {Session: db.Session{Agent: "roocode"}}} {
		svc := &compactionSpy{detail: detail}
		out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: "session"})
		require.NoError(t, err)
		assert.Empty(t, out.Summaries)
		assert.Empty(t, svc.reads)
	}
}

func TestCompactionSummariesUnfilteredPage(t *testing.T) {
	svc := &compactionSpy{}
	out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	assert.Equal(t, 20, svc.limit)
	assert.Empty(t, out.Summaries)
	assert.Equal(t, "next", out.NextCursor)
	assert.Equal(t, []string{"normal"}, svc.reads)
	_, err = service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: -1})
	require.ErrorContains(t, err, "limit must be 0 or more")
}

func TestCompactionSummariesValidationAndRetainedSummary(t *testing.T) {
	for _, tc := range []struct {
		req  service.CompactionSummariesRequest
		want string
	}{
		{service.CompactionSummariesRequest{}, "provide exactly one"},
		{service.CompactionSummariesRequest{SessionID: "s", ActiveSince: "2024-06-01T00:00:00Z"}, "provide exactly one"},
		{service.CompactionSummariesRequest{SessionID: "s", Cursor: "next"}, "cursor requires active_since"},
	} {
		_, err := service.CompactionSummaries(t.Context(), &compactionSpy{}, tc.req)
		require.ErrorContains(t, err, tc.want)
	}
	out, err := service.CompactionSummaries(t.Context(), &compactionSpy{includeSummary: true}, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	require.Len(t, out.Summaries, 1)
	assert.Equal(t, "retained summary", out.Summaries[0].Content)
}

func TestCompactionSummarySessionMembership(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for _, row := range []struct {
		id, agent, content, source string
		boundary                   bool
	}{
		{"good", "claude", "summary", "", true},
		{"roo", "roocode", "Context condensed", "", true},
		{"kilo", "kilo-legacy", "Context condensed", "", true},
		{"empty", "codex", "", "", true},
		{"placeholder", "evener", "[SUMMARY]", "evener_empty_compaction", true},
		{"pending", "opencode", "pending", "", false},
		{"deleted", "claude", "deleted", "", true},
	} {
		dbtest.SeedSession(t, d, row.id, "project", func(s *db.Session) { s.Agent = row.agent; s.EndedAt = new("2024-06-15T00:00:00Z") })
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: row.id, Role: "assistant", Content: row.content, SourceType: row.source, IsCompactBoundary: row.boundary}}))
	}
	require.NoError(t, d.SoftDeleteSession(t.Context(), "deleted"))
	page, err := d.ListSessions(t.Context(), db.SessionFilter{ActiveSince: "2024-06-01T00:00:00Z", HasCompactionSummary: true, IncludeChildren: true})
	require.NoError(t, err)
	require.Len(t, page.Sessions, 1)
	assert.Equal(t, "good", page.Sessions[0].ID)
	out, err := service.CompactionSummaries(t.Context(), service.NewDirectBackend(d, nil), service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	require.Len(t, out.Summaries, 1)
	assert.Equal(t, "good", out.Summaries[0].SessionID)
	assert.Equal(t, "summary", out.Summaries[0].Content)
}

func TestCompactionSummariesWhitespacePage(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	svc := service.NewDirectBackend(d, nil)
	for _, row := range []struct{ id, ended, content string }{
		{"blank", "2024-06-17T00:00:00Z", "  \n"},
		{"summary", "2024-06-16T00:00:00Z", "summary"},
	} {
		dbtest.SeedSession(t, d, row.id, "project", func(s *db.Session) { s.EndedAt = &row.ended })
		require.NoError(t, d.InsertMessages(t.Context(), []db.Message{{SessionID: row.id, Ordinal: 0, Role: "assistant", IsCompactBoundary: true, Content: row.content}}))
	}
	first, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 1})
	require.NoError(t, err)
	assert.Empty(t, first.Summaries)
	require.NotEmpty(t, first.NextCursor)
	second, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 1, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Summaries, 1)
	assert.Equal(t, "summary", second.Summaries[0].Content)
	assert.Empty(t, second.NextCursor)
}

func TestCompactionSummariesMultipleMessagePages(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	svc := service.NewDirectBackend(d, nil)
	dbtest.SeedSession(t, d, "many", "project")
	messages := make([]db.Message, db.MaxMessageLimit+1)
	for i := range messages {
		messages[i] = db.Message{SessionID: "many", Ordinal: i, Role: "assistant", IsCompactBoundary: true, Content: "summary"}
	}
	require.NoError(t, d.InsertMessages(t.Context(), messages))
	out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: "many"})
	require.NoError(t, err)
	require.Len(t, out.Summaries, db.MaxMessageLimit+1)
	assert.Equal(t, db.MaxMessageLimit, out.Summaries[len(out.Summaries)-1].Ordinal)
}
