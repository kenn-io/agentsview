package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/db"
)

func (s *Server) registerCommitRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Commits")
	s.post(group, "/commits/sessions", "Find sessions by commit", s.humaCommitSessions)
}

type commitSessionsInput struct {
	Body struct {
		SHAs []string `json:"shas" minItems:"1" maxItems:"200"`
	}
}

func commitLinkError(err error) error {
	if errors.Is(err, db.ErrInvalidCommitSHA) {
		return apiError(http.StatusBadRequest, err.Error())
	}
	if errors.Is(err, db.ErrCommitLinksUnavailable) {
		return apiError(http.StatusNotImplemented, err.Error())
	}
	return serverError(err)
}

func (s *Server) humaSessionCommits(ctx context.Context, in *idPathInput) (*jsonOutput[*db.SessionCommitList], error) {
	out, err := s.sessions.SessionCommits(ctx, in.ID)
	if err != nil {
		return nil, commitLinkError(err)
	}
	return &jsonOutput[*db.SessionCommitList]{Body: out}, nil
}

func (s *Server) humaCommitSessions(ctx context.Context, in *commitSessionsInput) (*jsonOutput[*db.CommitSessionList], error) {
	out, err := s.sessions.CommitSessions(ctx, in.Body.SHAs)
	if err != nil {
		return nil, commitLinkError(err)
	}
	return &jsonOutput[*db.CommitSessionList]{Body: out}, nil
}
