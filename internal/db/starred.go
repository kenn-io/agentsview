package db

import (
	"context"
	"database/sql"
	"fmt"
)

// StarSession marks a session as starred. Uses INSERT...SELECT
// with an EXISTS check so the operation is atomic and avoids FK
// errors if the session is concurrently deleted.  Returns false
// if the session does not exist (idempotent for already-starred).
func (db *DB) StarSession(sessionID string) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	w := db.getWriter()
	res, err := w.Exec(`
		INSERT OR IGNORE INTO starred_sessions (session_id)
		SELECT ? WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ?)`,
		sessionID, sessionID)
	if err != nil {
		return false, fmt.Errorf("starring session %s: %w", sessionID, err)
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		return true, nil // newly starred
	}
	// Zero rows: either already starred or session doesn't exist.
	var exists int
	err = w.QueryRow(
		"SELECT 1 FROM sessions WHERE id = ?", sessionID,
	).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil // session doesn't exist
	}
	if err != nil {
		return false, fmt.Errorf("checking session %s: %w", sessionID, err)
	}
	return true, nil // already starred
}

// UnstarSession removes a session's star.
func (db *DB) UnstarSession(sessionID string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		"DELETE FROM starred_sessions WHERE session_id = ?",
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("unstarring session %s: %w", sessionID, err)
	}
	return nil
}

// ListStarredSessionIDs returns all starred session IDs.
func (db *DB) ListStarredSessionIDs(
	ctx context.Context,
) ([]string, error) {
	rows, err := db.getReader().QueryContext(ctx,
		"SELECT session_id FROM starred_sessions ORDER BY created_at DESC",
	)
	if err != nil {
		return nil, fmt.Errorf("listing starred sessions: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning starred session: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// BulkStarSessions stars multiple sessions in a single transaction.
// Used for migrating localStorage stars to the database.
func (db *DB) BulkStarSessions(sessionIDs []string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if len(sessionIDs) == 0 {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Use INSERT ... SELECT ... WHERE EXISTS so that stale IDs
	// (sessions pruned or deleted from disk) are silently skipped
	// instead of causing a foreign key violation that aborts the
	// entire migration transaction.
	stmt, err := tx.Prepare(`
		INSERT OR IGNORE INTO starred_sessions (session_id)
		SELECT ? WHERE EXISTS (SELECT 1 FROM sessions WHERE id = ?)`)
	if err != nil {
		return fmt.Errorf("preparing statement: %w", err)
	}
	defer stmt.Close()

	for _, id := range sessionIDs {
		if _, err := stmt.Exec(id, id); err != nil {
			return fmt.Errorf("starring session %s: %w", id, err)
		}
	}

	return tx.Commit()
}
