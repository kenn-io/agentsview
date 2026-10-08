package clickhouse

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func (s *Store) GetProjectInventory(ctx context.Context, filter db.ProjectDateFilter) (db.ProjectInventory, error) {
	return s.catalog().GetProjectInventory(ctx, filter)
}

func (s *Store) projectInventoryAggregate(
	ctx context.Context,
	filter db.ProjectDateFilter,
) (map[string]db.ProjectInventoryAgg, error) {
	where, args := db.BuildSessionBaseFilterSQL(filter.SessionFilter(), db.ClickHouseQueryDialect())
	rows, err := s.queryContext(ctx, `
		SELECT project,
		       toInt64(COUNT(*)),
		       toInt64(COUNT(DISTINCT machine)),
		       toInt64(COUNT(DISTINCT agent)),
		       toInt64(COUNT(DISTINCT CASE WHEN cwd IS NOT NULL AND cwd != ''
		             THEN replaceAll(cwd, char(92), '/') END)),
		       MIN(started_at),
		       MAX(COALESCE(ended_at, started_at))
		FROM sessions
		WHERE `+where+`
		GROUP BY project
		ORDER BY project`, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregating clickhouse project inventory: %w", err)
	}
	defer rows.Close()

	out := map[string]db.ProjectInventoryAgg{}
	for rows.Next() {
		var project string
		var agg db.ProjectInventoryAgg
		var first, last any
		if err := rows.Scan(
			&project, &agg.Sessions, &agg.Machines, &agg.Agents,
			&agg.DistinctCwds, &first, &last,
		); err != nil {
			return nil, fmt.Errorf("scanning clickhouse project inventory row: %w", err)
		}
		agg.First, err = parseClickInventoryTime(first)
		if err != nil {
			return nil, fmt.Errorf("parsing clickhouse project inventory first activity: %w", err)
		}
		agg.Last, err = parseClickInventoryTime(last)
		if err != nil {
			return nil, fmt.Errorf("parsing clickhouse project inventory last activity: %w", err)
		}
		out[project] = agg
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse project inventory rows: %w", err)
	}
	return out, nil
}

func parseClickInventoryTime(v any) (*time.Time, error) {
	formatted := formatDBTime(v)
	if formatted == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, formatted)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
