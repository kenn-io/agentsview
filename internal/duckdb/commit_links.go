package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func (s *Store) CommitLinks(ctx context.Context, shas []string, id string) ([]db.CommitLink, error) {
	return storage.ReadPublishedCommitLinks(ctx, s.queryContext, db.DuckDBQueryDialect(), storage.CommitLinksRelation, storage.CurrentCommitLinkPublicationsRelation, shas, id)
}
