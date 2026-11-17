package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
)

// scanHostedUnits reads the physical session, including trashed sessions, in
// the same order and scope as the local embedding scan.
func scanHostedUnits(ctx context.Context, pg *sql.DB, sessionID string, includeAutomated bool, maxRunRunes int, fn func(db.EmbeddableUnit) error) error {
	query := `SELECT m.session_id, m.role, m.source_uuid, m.ordinal, m.content,
       m.is_sidechain, s.relationship_type, s.parent_session_id, s.ended_at::text
FROM messages m JOIN sessions s ON s.id=m.session_id
WHERE m.session_id=$1 AND m.role IN ('user','assistant') AND NOT m.is_system AND ` + db.PostgresSystemPrefixSQL("m.content", "m.role")
	if !includeAutomated {
		query += ` AND NOT s.is_automated`
	}
	query += ` ORDER BY m.session_id, m.ordinal`
	rows, err := pg.QueryContext(ctx, query, sessionID)
	if err != nil {
		return fmt.Errorf("scanning hosted embedding units: %w", err)
	}
	defer rows.Close()
	_, err = db.ReduceEmbeddableUnitRows(rows, maxRunRunes, fn)
	return err
}
