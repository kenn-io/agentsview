package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

const planUsageColumns = db.SessionPlanUsageColumns

// replacePlanUsage replaces a session's session_plan_usage row in PG with
// the archive's row, or removes it when the archive has none. It reports
// whether the PG table changed.
func (s *Sync) replacePlanUsage(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM session_plan_usage WHERE session_id = $1`, sessionID,
	)
	if err != nil {
		return false, fmt.Errorf("deleting pg plan usage for %s: %w", sessionID, err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting deleted pg plan usage for %s: %w", sessionID, err)
	}
	p, err := s.local.GetSessionPlanUsage(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("reading local plan usage for %s: %w", sessionID, err)
	}
	if p == nil {
		return deleted > 0, nil
	}
	return true, insertPlanUsage(ctx, tx, sessionID, p)
}

func insertPlanUsage(
	ctx context.Context, tx *sql.Tx, sessionID string, p *db.SessionPlanUsage,
) error {
	values, err := db.SessionPlanUsageValues(p)
	if err != nil {
		return fmt.Errorf("writing pg plan usage for %s: %w", sessionID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_plan_usage (session_id, `+planUsageColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`,
		append([]any{sessionID}, values...)...,
	); err != nil {
		return fmt.Errorf("writing pg plan usage for %s: %w", sessionID, err)
	}
	return nil
}

// GetSessionPlanUsage returns one session's plan usage row, or nil when
// the session has none.
func (s *Store) GetSessionPlanUsage(
	ctx context.Context, sessionID string,
) (*db.SessionPlanUsage, error) {
	rows, err := s.pg.QueryContext(ctx,
		`SELECT `+planUsageColumns+` FROM session_plan_usage WHERE session_id = $1`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying pg plan usage for %s: %w", sessionID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	p, err := db.ScanSessionPlanUsage(rows.Scan)
	if err != nil {
		return nil, fmt.Errorf("scanning pg plan usage for %s: %w", sessionID, err)
	}
	return p, rows.Err()
}

// GetPlanUsageSessionCounts counts the sessions a usage report leaves out
// as plan-only and the sessions it undercounts as partly captured, with
// the same rules as the SQLite query (db.GetPlanUsageSessionCounts).
func (s *Store) GetPlanUsageSessionCounts(
	ctx context.Context, f db.UsageFilter,
) (db.PlanUsageSessionCounts, error) {
	pb := &paramBuilder{}
	b := pgUsageBoundsForFilter(pb, f)

	messageWhere := appendPGUsageBranchFilterClauses(pgUsageMatchingMessageEligibility+`
	AND NOT EXISTS (SELECT 1 FROM usage_events ue WHERE ue.session_id = s.id)`,
		pb, f, "m.model")
	messageWhere = appendPGUsageColumnBounds(
		messageWhere, "COALESCE(m.timestamp, s.started_at)", b)
	query := `
SELECT '` + db.PlanUsageCountKindPlanOnly + `', s.id, m.model,
	COALESCE(m.timestamp, s.started_at)
FROM session_plan_usage p
JOIN sessions s ON s.id = p.session_id
JOIN messages m ON m.session_id = s.id
WHERE ` + messageWhere

	if len(db.CSVFilterValues(f.Model)) == 0 {
		sessionWhere := appendPGUsageSessionFilterClauses(pgUsageSessionEligibility+`
	AND NOT EXISTS (SELECT 1 FROM usage_events ue WHERE ue.session_id = s.id)
	AND NOT EXISTS (
		SELECT 1 FROM messages m
		WHERE m.session_id = s.id AND `+pgUsageMatchingMessageSourceEligibility+`
	)`, pb, f)
		sessionWhere = appendPGUsageColumnBounds(sessionWhere, "s.started_at", b)
		query += `
UNION ALL
SELECT '` + db.PlanUsageCountKindPlanOnly + `', s.id, '', s.started_at
FROM session_plan_usage p
JOIN sessions s ON s.id = p.session_id
WHERE ` + sessionWhere
	}

	eventWhere := appendPGUsageBranchFilterClauses(pgUsageEventEligibility+`
	AND p.uncaptured_turns > 0`, pb, f, "ue.model")
	eventWhere = appendPGUsageColumnBounds(
		eventWhere, "COALESCE(ue.occurred_at, s.started_at)", b)
	query += `
UNION ALL
SELECT '` + db.PlanUsageCountKindPartlyCaptured + `', s.id, ue.model,
	COALESCE(ue.occurred_at, s.started_at)
FROM session_plan_usage p
JOIN sessions s ON s.id = p.session_id
JOIN usage_events ue ON ue.session_id = s.id
WHERE ` + eventWhere

	rows, err := s.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return db.PlanUsageSessionCounts{}, fmt.Errorf("querying pg plan usage session counts: %w", err)
	}
	defer rows.Close()
	tally := db.NewPlanUsageCountTally(f)
	loc := f.Location()
	for rows.Next() {
		var kind, sessionID, model string
		var ts sql.NullTime
		if err := rows.Scan(&kind, &sessionID, &model, &ts); err != nil {
			return db.PlanUsageSessionCounts{}, fmt.Errorf("scanning pg plan usage session count: %w", err)
		}
		if err := tally.Add(kind, sessionID, model, usageDate(ts, loc)); err != nil {
			return db.PlanUsageSessionCounts{}, err
		}
	}
	if err := rows.Err(); err != nil {
		return db.PlanUsageSessionCounts{}, fmt.Errorf("iterating pg plan usage session counts: %w", err)
	}
	return tally.Counts(), nil
}
