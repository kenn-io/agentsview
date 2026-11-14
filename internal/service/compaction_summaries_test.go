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
