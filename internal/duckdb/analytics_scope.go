package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) resolveAnalyticsMessageScope(ctx context.Context, ids []string, f db.AnalyticsFilter, includeContent bool) (db.MessageScope, error) {
	return s.analytics().ResolveMessageScope(ctx, ids, f, includeContent)
}
