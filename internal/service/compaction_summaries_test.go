package service_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// compactionSpy can return rows from a daemon replaced after the version check.
type compactionSpy struct {
	service.SessionService
	detail  *service.SessionDetail
	limit   int
	reads   []string
	checked bool
}

func (s *compactionSpy) CheckCompactionSummaries(context.Context) error {
	s.checked = true
	return nil
}

func (s *compactionSpy) Get(context.Context, string) (*service.SessionDetail, error) {
	return s.detail, nil
}

func (s *compactionSpy) List(_ context.Context, f service.ListFilter) (*service.SessionList, error) {
	s.limit = f.Limit
	return &service.SessionList{Sessions: []db.Session{{ID: "event", Agent: "roocode"}, {ID: "normal", Agent: "claude"}}, NextCursor: "next"}, nil
}

func (s *compactionSpy) Messages(_ context.Context, id string, f service.MessageFilter) (*service.MessageList, error) {
	if !s.checked || !f.CompactBoundaryOnly {
		return nil, errors.New("expected a successful preflight and boundary filter")
	}
	// The replacement ignores the boundary filter after preflight succeeds.
	s.reads = append(s.reads, id)
	return &service.MessageList{Messages: []db.Message{
		{Content: "injected prompt", IsSystem: true},
		{Content: "ordinary message"},
		{Content: "  ", IsSystem: true, IsCompactBoundary: true},
	}}, nil
}

func TestCompactionSummariesExcludedSessionDoesNotReadMessages(t *testing.T) {
	roo := &service.SessionDetail{}
	roo.Agent = "roocode"
	kilo := &service.SessionDetail{}
	kilo.Agent = "kilo-legacy"
	for _, detail := range []*service.SessionDetail{roo, kilo} {
		svc := &compactionSpy{detail: detail}
		out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: "session"})
		require.NoError(t, err)
		assert.Empty(t, out.Summaries)
		assert.Empty(t, svc.reads)
	}
}

func TestCompactionSummariesMissingSession(t *testing.T) {
	svc := service.NewDirectBackend(dbtest.OpenTestDB(t), nil)
	_, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: "missing"})
	require.ErrorIs(t, err, service.ErrSessionNotFound)
}

func TestCompactionSummariesFilteredPage(t *testing.T) {
	svc := &compactionSpy{}
	out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	assert.Equal(t, 20, svc.limit)
	assert.Empty(t, out.Summaries)
	assert.Equal(t, "next", out.NextCursor)
	assert.Equal(t, []string{"normal"}, svc.reads)
}

func TestCompactionSummariesValidation(t *testing.T) {
	for _, tc := range []struct {
		req  service.CompactionSummariesRequest
		want string
	}{
		{service.CompactionSummariesRequest{}, "provide exactly one"},
		{service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: -1}, "limit must be 0 or more"},
		{service.CompactionSummariesRequest{SessionID: "s", ActiveSince: "2024-06-01T00:00:00Z"}, "provide exactly one"},
		{service.CompactionSummariesRequest{SessionID: "s", Cursor: "next"}, "cursor requires active_since"},
	} {
		_, err := service.CompactionSummaries(t.Context(), &compactionSpy{}, tc.req)
		require.ErrorContains(t, err, tc.want)
	}
}

func TestCompactionSummariesMultipleMessagePages(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	svc := service.NewDirectBackend(d, nil)
	dbtest.SeedSession(t, d, "many", "project")
	messages := make([]db.Message, db.MaxMessageLimit+1)
	for i := range messages {
		messages[i] = db.Message{SessionID: "many", Ordinal: i, Role: "assistant", IsSystem: true, IsCompactBoundary: true, Content: "summary"}
	}
	require.NoError(t, d.InsertMessages(t.Context(), messages))
	out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: "many"})
	require.NoError(t, err)
	require.Len(t, out.Summaries, db.MaxMessageLimit+1)
	assert.Equal(t, db.MaxMessageLimit, out.Summaries[len(out.Summaries)-1].Ordinal)
}

func TestCompactionSummariesLegacyEvener(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for _, row := range []struct{ id, ended string }{{"evener:a", "2024-06-18T00:00:00Z"}, {"evener:b", "2024-06-17T00:00:00Z"}, {"evener:c", "2024-06-16T00:00:00Z"}} {
		dbtest.SeedSession(t, d, row.id, "project", func(s *db.Session) { s.Agent = "evener"; s.EndedAt = &row.ended })
	}
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "evener:a", Role: "system", IsSystem: true, IsCompactBoundary: true, SourceType: "entry", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
		{SessionID: "evener:b", Role: "system", IsSystem: true, IsCompactBoundary: true, SourceType: "entry", SourceSubtype: "CHECKPOINT", Content: "Retained summary"},
		{SessionID: "evener:c", Ordinal: 0, Role: "system", IsSystem: true, IsCompactBoundary: true, SourceType: "evener_literal_compaction", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
		{SessionID: "evener:c", Ordinal: 1, Role: "system", IsSystem: true, IsCompactBoundary: true, SourceType: "evener_empty_compaction", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
	}))
	svc := service.NewDirectBackend(d, nil)
	for id, want := range map[string][]string{"evener:a": nil, "evener:b": {"Retained summary"}, "evener:c": {"[SUMMARY]"}} {
		out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: id})
		require.NoError(t, err)
		var contents []string
		for _, summary := range out.Summaries {
			contents = append(contents, summary.Content)
		}
		assert.Equal(t, want, contents, id)
	}
	active, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	assert.Contains(t, active.Ambiguous, service.CompactionAmbiguity{SessionID: "evener:a", Ordinal: 0})
	assert.Len(t, active.Summaries, 2)
}
