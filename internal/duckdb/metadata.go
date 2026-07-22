package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

// ListMetadataConflicts returns no rows for DuckDB read mode because the local
// artifact metadata ledger is not part of the analytical mirror.
func (s *Store) ListMetadataConflicts(
	context.Context,
	[]string,
) ([]db.MetadataConflict, error) {
	return []db.MetadataConflict{}, nil
}

// CountMetadataConflicts returns zero for DuckDB read mode because the local
// artifact metadata ledger is not part of the analytical mirror.
func (s *Store) CountMetadataConflicts(context.Context) (int, error) {
	return 0, nil
}
