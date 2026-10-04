package server

import (
	"context"

	"github.com/danielgtaylor/huma/v2"

	"go.kenn.io/agentsview/internal/kata"
)

// WithKataConn shares a Kata connection with the composition root so the
// server and future filer see one cached probe.
func WithKataConn(c *kata.Conn) Option {
	return func(s *Server) {
		if c != nil {
			s.kata = c
		}
	}
}

func (s *Server) kataConn() *kata.Conn { return s.kata }

func (s *Server) registerKataRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Kata")
	s.get(group, "/kata/status", "Get Kata connection status", s.humaGetKataStatus)
}

func (s *Server) humaGetKataStatus(ctx context.Context, _ *emptyInput) (*jsonOutput[kata.Status], error) {
	return &jsonOutput[kata.Status]{Body: s.kataConn().Status(ctx, true)}, nil
}
