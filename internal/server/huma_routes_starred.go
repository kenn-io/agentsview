package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

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
	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()

	// Prior state decides rollback: StarSession reports success for an
	// already-starred session too, and that star must survive a failed
	// metadata append.
	wasStarred := false
	if s.metadata != nil {
		var err error
		wasStarred, err = s.sessionStarred(ctx, in.ID)
		if err != nil {
			return nil, internalError("star session prior state", err)
		}
	}
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
		var publishedErr *artifact.MetadataPublishedError
		if errors.As(err, &publishedErr) {
			return nil, internalError("star session metadata event", err)
		}
		if !wasStarred {
			if _, removeErr := s.db.UnstarSession(in.ID); removeErr != nil {
				return nil, internalError(
					"star session metadata event",
					errors.Join(err, fmt.Errorf("remove star after metadata failure: %w", removeErr)),
				)
			}
		}
		return nil, internalError("star session metadata event", err)
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

// sessionStarred reports whether the session is currently starred.
func (s *Server) sessionStarred(ctx context.Context, id string) (bool, error) {
	ids, err := s.db.ListStarredSessionIDs(ctx)
	if err != nil {
		return false, err
	}
	if slices.Contains(ids, id) {
		return true, nil
	}
	return false, nil
}

func (s *Server) humaUnstarSession(
	ctx context.Context,
	in *idPathInput,
) (*noContentOutput, error) {
	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()

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
	s.lockSessionLifecycle()
	defer s.sessionLifecycleMu.Unlock()
	ctx, releaseArtifactStore, err := s.acquireMetadataArtifactLease(ctx)
	if err != nil {
		return nil, internalError("bulk star artifact store", err)
	}
	defer releaseArtifactStore()

	starred, err := s.db.BulkStarSessions(in.Body.SessionIDs)
	if err != nil {
		if handled := handleHumaReadOnly(err); handled != nil {
			return nil, handled
		}
		return nil, internalError("bulk star", err)
	}
	newlyStarred := make(map[string]struct{}, len(starred))
	// Emit one star event per session actually starred so localStorage star
	// migration converges through artifact sync, matching single-session star.
	for i, id := range starred {
		newlyStarred[id] = struct{}{}
		if err := s.appendMetadataEvent(ctx, artifact.MetadataEventInput{
			SessionID: id,
			Op:        artifact.MetadataOpStar,
		}); err != nil {
			// Stars whose events are already in the ledger stay; the
			// rest were created by this request without a ledger event
			// to sync them, so they are removed. A published error
			// means the failed event itself is durably recorded, so
			// its star stays too.
			rollback := starred[i:]
			var publishedErr *artifact.MetadataPublishedError
			if errors.As(err, &publishedErr) {
				rollback = starred[i+1:]
			}
			return nil, internalError("bulk star metadata event",
				s.rollbackBulkStar(rollback, err))
		}
	}
	starredIDs, err := s.db.ListStarredSessionIDs(ctx)
	if err != nil {
		return nil, internalError("bulk star metadata repair", err)
	}
	starredNow := make(map[string]struct{}, len(starredIDs))
	for _, id := range starredIDs {
		starredNow[id] = struct{}{}
	}
	seenRetry := map[string]struct{}{}
	for _, id := range in.Body.SessionIDs {
		if _, ok := newlyStarred[id]; ok {
			continue
		}
		if _, ok := starredNow[id]; !ok {
			continue
		}
		if _, ok := seenRetry[id]; ok {
			continue
		}
		seenRetry[id] = struct{}{}
		if err := s.ensureLocalMetadataEvent(ctx, artifact.MetadataEventInput{
			SessionID: id,
			Op:        artifact.MetadataOpStar,
		}, "starred", artifact.MetadataOpStar); err != nil {
			return nil, internalError("bulk star metadata repair", err)
		}
	}
	return &noContentOutput{Status: http.StatusNoContent}, nil
}

// rollbackBulkStar removes stars this request created but never
// recorded in the ledger, so local state does not run ahead of the
// artifact log. The returned error joins the append failure with any
// rollback failures.
func (s *Server) rollbackBulkStar(ids []string, cause error) error {
	errs := []error{cause}
	for _, id := range ids {
		if _, err := s.db.UnstarSession(id); err != nil {
			errs = append(errs,
				fmt.Errorf("remove star %s after metadata failure: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
