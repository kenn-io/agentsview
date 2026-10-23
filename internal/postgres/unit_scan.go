package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

// ScanSessionUnits streams one session's embedding units in ordinal order.
// Filters and run boundaries match SQLite's ScanEmbeddableUnitsForSession.
// It never scans the whole corpus. maxRunRunes includes joined separators.
func (s *Store) ScanSessionUnits(
	ctx context.Context, sessionID string, includeAutomated bool, maxRunRunes int,
	fn func(db.EmbeddableUnit) error,
) error {
	if sessionID == "" {
		return errors.New("session ID is required")
	}
	predicates := []string{
		"m.session_id = $1", "m.role IN ('user', 'assistant')",
		"m.is_system = false", "s.deleted_at IS NULL",
		db.PostgresSystemPrefixSQL("m.content", "m.role"),
	}
	if !includeAutomated {
		predicates = append(predicates, db.PostgresQueryDialect().AutomatedScopePredicate("human", "s.is_automated"))
	}
	rows, err := s.DB().QueryContext(ctx, `
  SELECT m.session_id, m.ordinal, m.role, m.content, m.is_system,
         m.source_uuid, m.is_sidechain, `+db.SubordinateSessionSQL("s")+`
  FROM messages m JOIN sessions s ON s.id = m.session_id
  WHERE `+strings.Join(predicates, " AND ")+`
  ORDER BY m.ordinal`, sessionID)
	if err != nil {
		return fmt.Errorf("scanning session units: %w", err)
	}
	defer rows.Close()
	reducer := db.UnitReducer{MaxRunRunes: maxRunRunes}
	for rows.Next() {
		var row db.UnitRow
		if err := rows.Scan(&row.SessionID, &row.Ordinal, &row.Role, &row.Content, &row.IsSystem,
			&row.SourceUUID, &row.IsSidechain, &row.Subordinate); err != nil {
			return fmt.Errorf("scanning session unit row: %w", err)
		}
		if err := reducer.Push(row, fn); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating session units: %w", err)
	}
	return reducer.Flush(fn)
}
