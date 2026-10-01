package db

import (
	"context"
	"database/sql"
)

// GetClaudeSubagentSources reads the local files contributing archived messages.
func (db *DB) GetClaudeSubagentSources(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := db.getReader().Query(ctx,
		`SELECT file_path FROM claude_subagent_sources WHERE session_id = ? ORDER BY file_path`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func writeClaudeSubagentSourcesTx(ctx context.Context, tx *sql.Tx, sessionID string, paths []string, replace bool) error {
	if paths == nil {
		return nil
	}
	if replace {
		if _, err := tx.ExecContext(ctx, `DELETE FROM claude_subagent_sources WHERE session_id = ?`, sessionID); err != nil {
			return err
		}
		// A complete replacement from one file no longer needs joined-source
		// provenance. Ordinary single-file sessions keep their existing path.
		if len(paths) < 2 {
			return nil
		}
	}
	for _, path := range paths {
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO claude_subagent_sources (session_id, file_path) VALUES (?, ?)`, sessionID, path); err != nil {
			return err
		}
	}
	return nil
}
