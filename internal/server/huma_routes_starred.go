package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"go.kenn.io/agentsview/internal/artifact"
)

func (s *Server) registerStarredRoutes() {
	group := newRouteGroup(s.api, "/api/v1", "Starred")

	get(s, group, "/starred", "List starred sessions", s.humaListStarred)
	put(s, group, "/sessions/{id}/star", "Star session", s.humaStarSession)
	deleteRoute(s, group, "/sessions/{id}/star", "Unstar session", s.humaUnstarSession)
	post(s, group, "/starred/bulk", "Bulk star sessions", s.humaBulkStar)
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
	ctx context.Context,
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
	if err := s.appendMetadataEvent(ctx, artifact.MetadataEventInput{
		SessionID: in.ID,
		Op:        artifact.MetadataOpStar,
	}); err != nil {
		return nil, internalError("star session metadata event", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaUnstarSession(
	ctx context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	removed, err := s.db.UnstarSession(in.ID)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("unstar session", err)
	}
	if !removed {
		if _, err := s.repairLocalMetadataEvent(ctx, artifact.MetadataEventInput{
			SessionID: in.ID,
			Op:        artifact.MetadataOpUnstar,
		}); err != nil {
			return nil, internalError("unstar session metadata repair", err)
		}
		return &noContentOutput{Status: http.StatusNoContent}, nil
	}
	if err := s.appendMetadataEvent(ctx, artifact.MetadataEventInput{
		SessionID: in.ID,
		Op:        artifact.MetadataOpUnstar,
	}); err != nil {
		var publishedErr *artifact.MetadataPublishedError
		if errors.As(err, &publishedErr) {
			return nil, internalError("unstar session metadata event", err)
		}
		if restored, restoreErr := s.db.StarSession(in.ID); restoreErr != nil {
			return nil, internalError(
				"unstar session metadata event",
				errors.Join(err, fmt.Errorf("restore star after metadata failure: %w", restoreErr)),
			)
		} else if !restored {
			return nil, internalError(
				"unstar session metadata event",
				errors.Join(err, fmt.Errorf("restore star after metadata failure: session %q not found", in.ID)),
			)
		}
		return nil, internalError("unstar session metadata event", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

func (s *Server) humaBulkStar(
	ctx context.Context,
	in *bulkStarInput,
) (*noContentOutput, error) {
	if len(in.Body.SessionIDs) == 0 {
		return &noContentOutput{Status: http.StatusNoContent}, nil
	}
	starred, err := s.db.BulkStarSessions(in.Body.SessionIDs)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("bulk star", err)
	}
	// Emit one star event per session actually starred so localStorage star
	// migration converges through artifact sync, matching single-session star.
	for _, id := range starred {
		if err := s.appendMetadataEvent(ctx, artifact.MetadataEventInput{
			SessionID: id,
			Op:        artifact.MetadataOpStar,
		}); err != nil {
			return nil, internalError("bulk star metadata event", err)
		}
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}
