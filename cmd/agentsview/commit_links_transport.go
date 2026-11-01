package main

import (
	"context"

	"go.kenn.io/agentsview/internal/service"
)

func (s *mcpDaemonService) CommitSessions(ctx context.Context, shas []string) (map[string][]service.CommitSessionLink, error) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	svc, cleanup, err := newCommitLookupService(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return svc.CommitSessions(ctx, shas)
}

func (s *mcpDaemonService) SessionCommits(ctx context.Context, id string) ([]service.CommitSessionLink, error) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	svc, cleanup, err := newCommitLookupService(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return svc.SessionCommits(ctx, id)
}
