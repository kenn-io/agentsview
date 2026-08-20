package server

import (
	"context"
	"net/http"
)

func (s *Server) registerStarredRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Starred")

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

func (s *Server) humaStarSession(
	_ context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	ok, err := s.db.StarSession(in.ID)
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

func (s *Server) humaUnstarSession(
	_ context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	if err := s.db.UnstarSession(in.ID); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("unstar session", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaBulkStar(
	_ context.Context,
	in *bulkStarInput,
) (*noContentOutput, error) {
	if len(in.Body.SessionIDs) == 0 {
		return &noContentOutput{Status: http.StatusNoContent}, nil
	}
	if err := s.db.BulkStarSessions(in.Body.SessionIDs); err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("bulk star", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}
