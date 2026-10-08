package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) GetProjectInventory(ctx context.Context, filter db.ProjectDateFilter) (db.ProjectInventory, error) {
	return s.catalog().GetProjectInventory(ctx, filter)
}

func (s *Store) projectInventoryAggregate(
	ctx context.Context,
	filter db.ProjectDateFilter,
) (map[string]db.ProjectInventoryAgg, error) {
	where, args := db.BuildSessionBaseFilterSQL(filter.SessionFilter(), db.PostgresQueryDialect())
	rows, err := s.pg.QueryContext(ctx, `
		SELECT project,
		       COUNT(*),
		       COUNT(DISTINCT machine),
		       COUNT(DISTINCT agent),
		       COUNT(DISTINCT CASE WHEN cwd IS NOT NULL AND cwd != ''
		             THEN replace(cwd, '\', '/') END),
		       MIN(started_at),
		       MAX(COALESCE(ended_at, started_at))
		FROM sessions
		WHERE `+where+`
		GROUP BY project
		ORDER BY project`, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregating pg project inventory: %w", err)
	}
	defer rows.Close()

	out := map[string]db.ProjectInventoryAgg{}
	for rows.Next() {
		var project string
		var agg db.ProjectInventoryAgg
		var first, last sql.NullTime
		if err := rows.Scan(
			&project, &agg.Sessions, &agg.Machines, &agg.Agents,
			&agg.DistinctCwds, &first, &last,
		); err != nil {
			return nil, fmt.Errorf("scanning pg project inventory row: %w", err)
		}
		if first.Valid {
			t := first.Time
			agg.First = &t
		}
		if last.Valid {
			t := last.Time
			agg.Last = &t
		}
		out[project] = agg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating pg project inventory rows: %w", err)
	}
	return out, nil
}
