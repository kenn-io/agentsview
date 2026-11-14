package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

// compactionSpy stands in for a daemon that ignores the summary filters.
type compactionSpy struct {
	service.SessionService
	detail *service.SessionDetail
	limit  int
	reads  []string
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
	return &service.MessageList{Messages: []db.Message{
		{Content: "injected prompt", IsSystem: true},
		{Content: "ordinary message"},
		{Content: "[CHECKPOINT]", SourceType: "evener_empty_compaction", SourceSubtype: "CHECKPOINT", IsCompactBoundary: true},
		{Content: "  ", IsCompactBoundary: true},
	}}, nil
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
