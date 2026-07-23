package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// UnstarSession removes a session's star and reports whether a row was removed.
func (db *DB) UnstarSession(sessionID string) (bool, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	res, err := db.getWriter().Exec(
		"DELETE FROM starred_sessions WHERE session_id = ?",
		sessionID,
	)
	if err != nil {
		return false, fmt.Errorf("unstarring session %s: %w", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking unstar result for %s: %w", sessionID, err)
	}
	return n > 0, nil
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

// ListStarredSessionIDsForScope returns starred session IDs restricted to
// the given project scope (see BuildSessionFilterSQL's project/exclude
// semantics), sorted for deterministic output. Cost is bounded by the
// number of starred rows (one join lookup each), not archive size: callers
// needing a scope-filtered curation fingerprint should use this instead of
// filtering ListStarredSessionIDs's unscoped result against a
// separately-loaded session list.
func (db *DB) ListStarredSessionIDsForScope(
	ctx context.Context, projects, excludeProjects []string,
) ([]string, error) {
	where, args := curationScopeWhere("s", projects, excludeProjects)
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT ss.session_id FROM starred_sessions ss
		 JOIN sessions s ON s.id = ss.session_id`+where+
			` ORDER BY ss.session_id`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("listing scoped starred sessions: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning scoped starred session: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// curationScopeWhere builds a " WHERE ..." clause (with a leading space, ""
// when unfiltered) restricting alias.project to projects/excludeProjects.
// Shared by ListStarredSessionIDsForScope and ListPinnedSessionIDsForScope,
// which both join a curation table to sessions under the same alias
// convention to compute a project-scoped curation fingerprint.
func curationScopeWhere(alias string, projects, excludeProjects []string) (string, []any) {
	var args []any
	var where []string
	if len(projects) > 0 {
		placeholders := make([]string, len(projects))
		for i, p := range projects {
			placeholders[i] = "?"
			args = append(args, p)
		}
		where = append(where, alias+".project IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(excludeProjects) > 0 {
		placeholders := make([]string, len(excludeProjects))
		for i, p := range excludeProjects {
			placeholders[i] = "?"
			args = append(args, p)
		}
		where = append(where, alias+".project NOT IN ("+strings.Join(placeholders, ", ")+")")
	}
	if len(where) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

// BulkStarSessions stars multiple sessions in a single transaction.
// Used for migrating localStorage stars to the database.
func (db *DB) BulkStarSessions(sessionIDs []string) ([]string, error) {
	if err := db.requireWritable(); err != nil {
		return nil, err
	}
	if len(sessionIDs) == 0 {
		return nil, nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return nil, fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Check existence separately from the insert so stale IDs (sessions pruned
	// or deleted from disk) are silently skipped instead of aborting the
	// migration transaction, and so the caller learns which sessions were
	// actually starred and need a converging metadata event.
	exists, err := tx.Prepare(`SELECT 1 FROM sessions WHERE id = ?`)
	if err != nil {
		return nil, fmt.Errorf("preparing existence statement: %w", err)
	}
	defer exists.Close()
	insert, err := tx.Prepare(
		`INSERT OR IGNORE INTO starred_sessions (session_id) VALUES (?)`)
	if err != nil {
		return nil, fmt.Errorf("preparing statement: %w", err)
	}
	defer insert.Close()

	starred := make([]string, 0, len(sessionIDs))
	for _, id := range sessionIDs {
		var one int
		switch err := exists.QueryRow(id).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("checking session %s: %w", id, err)
		}
		res, err := insert.Exec(id)
		if err != nil {
			return nil, fmt.Errorf("starring session %s: %w", id, err)
		}
		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("checking star insert result for %s: %w", id, err)
		}
		if rowsAffected > 0 {
			starred = append(starred, id)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing star transaction: %w", err)
	}
	return starred, nil
}
