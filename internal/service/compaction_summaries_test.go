package service_test

import (
	"context"
	"encoding/json"
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
	detail         *service.SessionDetail
	limit          int
	reads          []string
	includeSummary bool
	checked        bool
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
	messages := []db.Message{
		{Content: "injected prompt", IsSystem: true},
		{Content: "ordinary message"},
		{Content: "  ", IsCompactBoundary: true},
	}
	if s.includeSummary {
		messages = append(messages, db.Message{Content: "retained summary", IsCompactBoundary: true})
	}
	return &service.MessageList{Messages: messages}, nil
}

func TestCompactionSummariesExcludedSessionDoesNotReadMessages(t *testing.T) {
	roo := &service.SessionDetail{}
	roo.Agent = "roocode"
	kilo := &service.SessionDetail{}
	kilo.Agent = "kilo-legacy"
	for _, detail := range []*service.SessionDetail{nil, roo, kilo} {
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
	assert.True(t, svc.checked)
	assert.Equal(t, 20, svc.limit)
	assert.Empty(t, out.Summaries)
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

func TestCompactionSummariesLegacyEvener(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	for _, row := range []struct{ id, ended string }{{"a", "2024-06-18T00:00:00Z"}, {"b", "2024-06-17T00:00:00Z"}, {"c", "2024-06-16T00:00:00Z"}} {
		dbtest.SeedSession(t, d, row.id, "project", func(s *db.Session) { s.Agent = "evener"; s.EndedAt = &row.ended })
	}
	require.NoError(t, d.InsertMessages(t.Context(), []db.Message{
		{SessionID: "a", Role: "system", IsCompactBoundary: true, SourceType: "entry", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
		{SessionID: "b", Role: "system", IsCompactBoundary: true, SourceType: "entry", SourceSubtype: "CHECKPOINT", Content: "Retained summary"},
		{SessionID: "c", Ordinal: 0, Role: "system", IsCompactBoundary: true, SourceType: "evener_literal_compaction", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
		{SessionID: "c", Ordinal: 1, Role: "system", IsCompactBoundary: true, SourceType: "evener_empty_compaction", SourceSubtype: "SUMMARY", Content: "[SUMMARY]"},
	}))
	svc := service.NewDirectBackend(d, nil)
	for id, want := range map[string][]string{"a": nil, "b": {"Retained summary"}, "c": {"[SUMMARY]"}, "missing": nil} {
		out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: id})
		require.NoError(t, err)
		var contents []string
		for _, summary := range out.Summaries {
			contents = append(contents, summary.Content)
		}
		assert.Equal(t, want, contents, id)
		if id == "a" {
			raw, err := json.Marshal(out)
			require.NoError(t, err)
			assert.Contains(t, string(raw), `"ambiguous":[{"session_id":"a","ordinal":0}]`)
		}
	}
	active, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z"})
	require.NoError(t, err)
	assert.Contains(t, active.Ambiguous, service.CompactionAmbiguity{SessionID: "a", Ordinal: 0})
	assert.Len(t, active.Summaries, 2)
}
