package service_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type compactionReadSpy struct {
	service.SessionService
	detail *service.SessionDetail
	reads  int
}

func (s *compactionReadSpy) Get(context.Context, string) (*service.SessionDetail, error) {
	return s.detail, nil
}
func (s *compactionReadSpy) Messages(context.Context, string, service.MessageFilter) (*service.MessageList, error) {
	s.reads++
	return &service.MessageList{}, nil
}

func TestCompactionSummariesExcludedSessionDoesNotReadMessages(t *testing.T) {
	for _, detail := range []*service.SessionDetail{nil, {Session: db.Session{Agent: "roocode"}}} {
		svc := &compactionReadSpy{detail: detail}
		out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{SessionID: "session"})
		require.NoError(t, err)
		assert.Empty(t, out.Summaries)
		assert.Zero(t, svc.reads)
	}
}

type compactionListSpy struct {
	service.SessionService
	limit int
}

func (s *compactionListSpy) List(_ context.Context, f service.ListFilter) (*service.SessionList, error) {
	s.limit = f.Limit
	return &service.SessionList{}, nil
}

func TestCompactionSummariesDefaultLimit(t *testing.T) {
	svc := &compactionListSpy{}
	_, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 0})
	require.NoError(t, err)
	assert.Equal(t, 20, svc.limit)
}

type compactionUnfilteredStore struct {
	service.SessionService
	reads []string
}

func (s *compactionUnfilteredStore) List(_ context.Context, _ service.ListFilter) (*service.SessionList, error) {
	return &service.SessionList{Sessions: []db.Session{{ID: "event", Agent: "roocode"}, {ID: "normal", Agent: "claude"}}, NextCursor: "next"}, nil
}

func (s *compactionUnfilteredStore) Messages(_ context.Context, id string, _ service.MessageFilter) (*service.MessageList, error) {
	s.reads = append(s.reads, id)
	return &service.MessageList{Messages: []db.Message{
		{Content: "injected prompt", IsSystem: true},
		{Content: "ordinary message"},
		{Content: "[CHECKPOINT]", SourceSubtype: "CHECKPOINT", IsCompactBoundary: true},
		{Content: "  ", IsCompactBoundary: true},
	}}, nil
}

func TestCompactionSummariesUnfilteredPage(t *testing.T) {
	svc := &compactionUnfilteredStore{}
	out, err := service.CompactionSummaries(t.Context(), svc, service.CompactionSummariesRequest{ActiveSince: "2024-06-01T00:00:00Z", Limit: 2})
	require.NoError(t, err)
	assert.Empty(t, out.Summaries)
	assert.Equal(t, "next", out.NextCursor)
	assert.Equal(t, []string{"normal"}, svc.reads)
}
