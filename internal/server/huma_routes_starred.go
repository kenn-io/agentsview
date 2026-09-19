package server

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
)

func (s *Server) registerStarredRoutes() {
	group := huma.NewGroup(s.api, "/api/v1")
	configureRouteGroup(group, "Starred")

	s.get(group, "/starred", "List starred sessions", s.humaListStarred)
	s.put(group, "/sessions/{id}/star", "Star session", s.humaStarSession)
	s.deleteRoute(group, "/sessions/{id}/star", "Unstar session", s.humaUnstarSession)
	s.post(group, "/starred/bulk", "Bulk star sessions", s.humaBulkStar)
}

type bulkStarInput struct {
	Body struct {
		SessionIDs []string `json:"session_ids" required:"true" doc:"Session IDs to star"`
	}
}

type starredResponse struct {
	SessionIDs []string `json:"session_ids"`
}

func (s *Server) humaListStarred(
	ctx context.Context,
	_ *emptyInput,
) (*jsonOutput[starredResponse], error) {
	ids, err := s.db.ListStarredSessionIDs(ctx)
	if err != nil {
		return nil, internalError("list starred", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return &jsonOutput[starredResponse]{Body: starredResponse{SessionIDs: ids}}, nil
}

func (s *Server) humaStarSession(ctx context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	ok, err := s.db.StarSession(ctx, in.ID)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("star session", err)
	}
	if !ok {
		return nil, apiError(http.StatusNotFound, "session not found")
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaUnstarSession(ctx context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	if err := s.db.UnstarSession(ctx, in.ID); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("unstar session", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaBulkStar(ctx context.Context,
	in *bulkStarInput,
) (*noContentOutput, error) {
	if len(in.Body.SessionIDs) == 0 {
		return &noContentOutput{Status: http.StatusNoContent}, nil
	}
	if err := s.db.BulkStarSessions(ctx, in.Body.SessionIDs); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("bulk star", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}
