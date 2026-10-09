package dbtest

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

// PublishingCatalog publishes fixture changes between candidate selection and detail loading.
type PublishingCatalog struct {
	readbase.CatalogBackend
	Publish func()
}

func (s PublishingCatalog) WorktreeCandidateSessions(ctx context.Context, ids []string, filter db.SessionFilter) ([]db.WorktreeCandidateSession, error) {
	s.Publish()
	return s.CatalogBackend.WorktreeCandidateSessions(ctx, ids, filter)
}
