package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/agentsview/internal/service"
)

type commitSessionsInput struct {
	Body struct {
		SHAs []string `json:"shas"`
	}
}
type sessionCommitsInput struct {
	ID string `path:"id"`
}

func (s *Server) registerCommitRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Commits")
	s.post(group, "/commits/sessions", "Find sessions for commits", s.humaCommitSessions)
	s.get(group, "/sessions/{id}/commits", "List a session's commits", s.humaSessionCommits)
}

func (s *Server) humaCommitSessions(ctx context.Context, in *commitSessionsInput) (*jsonOutput[service.CommitSessionsResponse], error) {
	rows, err := s.sessions.CommitSessions(ctx, in.Body.SHAs)
	if err != nil {
		return nil, commitLookupError(err)
	}
	return &jsonOutput[service.CommitSessionsResponse]{Body: service.CommitSessionsResponse{Commits: rows}}, nil
}
func (s *Server) humaSessionCommits(ctx context.Context, in *sessionCommitsInput) (*jsonOutput[service.SessionCommitsResponse], error) {
	rows, err := s.sessions.SessionCommits(ctx, in.ID)
	if err != nil {
		return nil, commitLookupError(err)
	}
	return &jsonOutput[service.SessionCommitsResponse]{Body: service.SessionCommitsResponse{Commits: rows}}, nil
}
func commitLookupError(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidCommitQuery):
		return apiError(http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrCommitLookupTooLarge):
		return apiError(http.StatusUnprocessableEntity, service.ErrCommitLookupTooLarge.Error())
	case errors.Is(err, service.ErrCommitIndexUnavailable):
		return apiError(http.StatusNotImplemented, err.Error())
	default:
		return serverError(err)
	}
}
