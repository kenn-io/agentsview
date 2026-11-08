package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// HostScopedSessionIDs returns the IDs of live sessions that store the raw
// session ID either bare (a local session) or behind exactly one remote host
// prefix ("host~<raw>"). Remote sync uses it to find which host archived a
// parent session. IDs with nested host prefixes do not match. A raw ID that
// already carries a host prefix, or an empty one, matches nothing.
//
// The suffix comparison uses SUBSTR rather than LIKE so SQL wildcard
// characters in session IDs compare literally. It scans the id index, so
// callers should try a primary-key lookup first.
func (db *DB) HostScopedSessionIDs(
	ctx context.Context, raw string,
) ([]string, error) {
	if raw == "" || strings.Contains(raw, "~") {
		return nil, nil
	}
	rows, err := db.getReader().QueryContext(ctx,
		`SELECT id FROM sessions
		 WHERE (id = ?1 OR SUBSTR(id, -(LENGTH(?1) + 1)) = '~' || ?1)
		   AND deleted_at IS NULL
		 ORDER BY id`,
		raw,
	)
	if err != nil {
		return nil, fmt.Errorf("finding host-scoped sessions for %q: %w", raw, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning session id: %w", err)
		}
		if host := strings.TrimSuffix(id, "~"+raw); id != raw && strings.Contains(host, "~") {
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LiveSessionExists reports whether a session row with id exists and is not
// soft-deleted.
func (db *DB) LiveSessionExists(ctx context.Context, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var one int
	err := db.getReader().QueryRowContext(ctx,
		`SELECT 1 FROM sessions WHERE id = ? AND deleted_at IS NULL`, id,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking session %s: %w", id, err)
	}
	return true, nil
}

// SessionParentIDs returns the stored effective and parser parent IDs of a
// session. Missing rows and NULL columns return empty strings.
func (db *DB) SessionParentIDs(
	ctx context.Context, id string,
) (parent, parserParent string, err error) {
	if id == "" {
		return "", "", nil
	}
	var p, pp sql.NullString
	err = db.getReader().QueryRowContext(ctx,
		`SELECT parent_session_id, parser_parent_session_id
		 FROM sessions WHERE id = ?`, id,
	).Scan(&p, &pp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("reading parents of session %s: %w", id, err)
	}
	return p.String, pp.String, nil
}
