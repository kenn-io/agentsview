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
