package server

import (
	"context"

	"go.kenn.io/agentsview/internal/sessionwatch"
)

// sessionMonitor returns a channel that ticks whenever the
// session's DB state changes. Thin adapter around
// sessionwatch.Watcher, which contains the polling logic shared
// with the CLI `session watch` command.
func (s *Server) sessionMonitor(
	ctx context.Context, sessionID string,
) <-chan struct{} {
	return sessionwatch.New(s.db, s.engine).Events(ctx, sessionID)
}

func (s *Server) sessionTreeMonitor(ctx context.Context, sessionID string) <-chan struct{} {
	return sessionwatch.New(s.db, s.engine).TreeEvents(ctx, sessionID)
}
