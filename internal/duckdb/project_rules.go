package duckdb

import (
	"context"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) ListProjectRules(ctx context.Context, machine string) (db.ProjectRules, error) {
	return s.catalog().ListProjectRules(ctx, machine)
}
