package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

const sessionUsageTotalColumns = db.SessionUsageTotalColumns

// sessionUsageTotalsDDL mirrors SQLite's session_usage_totals: the usage
// total the agent recorded for a whole session. Usage queries never read
// it, because it overlaps the session's usage rows.
const sessionUsageTotalsDDL = `
CREATE TABLE IF NOT EXISTS session_usage_totals (
    session_id TEXT PRIMARY KEY,
    source TEXT NOT NULL,
    subagent_scope TEXT NOT NULL DEFAULT '',
    cost_microdollars BIGINT,
    incomplete BOOLEAN NOT NULL DEFAULT FALSE,
    models_json TEXT NOT NULL DEFAULT '[]',
    FOREIGN KEY (session_id)
        REFERENCES sessions(id) ON DELETE CASCADE
);
`

// replaceSessionUsageTotal replaces a session's session_usage_totals row in
// PG with the archive's row, or removes it when the archive has none. It
// reports whether the PG table changed.
func (s *Sync) replaceSessionUsageTotal(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM session_usage_totals WHERE session_id = $1`, sessionID,
	)
	if err != nil {
		return false, fmt.Errorf("deleting pg session total for %s: %w", sessionID, err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("counting deleted pg session total for %s: %w", sessionID, err)
	}
	t, err := s.local.GetSessionUsageTotal(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("reading local session total for %s: %w", sessionID, err)
	}
	if t == nil {
		return deleted > 0, nil
	}
	return true, insertSessionUsageTotal(ctx, tx, sessionID, t)
}

func insertSessionUsageTotal(
	ctx context.Context, tx *sql.Tx, sessionID string, t *db.SessionUsageTotal,
) error {
	values, err := db.SessionUsageTotalValues(t)
	if err != nil {
		return fmt.Errorf("writing pg session total for %s: %w", sessionID, err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_usage_totals (session_id, `+sessionUsageTotalColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		append([]any{sessionID}, values...)...,
	); err != nil {
		return fmt.Errorf("writing pg session total for %s: %w", sessionID, err)
	}
	return nil
}

// GetSessionUsageTotal returns one session's recorded total, or nil when
// the session has none.
func (s *Store) GetSessionUsageTotal(
	ctx context.Context, sessionID string,
) (*db.SessionUsageTotal, error) {
	rows, err := s.pg.QueryContext(ctx,
		`SELECT `+sessionUsageTotalColumns+` FROM session_usage_totals WHERE session_id = $1`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying pg session total for %s: %w", sessionID, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	t, err := db.ScanSessionUsageTotal(rows.Scan)
	if err != nil {
		return nil, fmt.Errorf("scanning pg session total for %s: %w", sessionID, err)
	}
	return t, rows.Err()
}
