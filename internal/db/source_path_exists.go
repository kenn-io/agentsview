package db

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

const hasActiveSessionSourceBelowQuery = `
	SELECT 1
	FROM sessions
	WHERE agent = ?
	  AND file_path >= ?
	  AND file_path < ?
	  AND file_path IS NOT NULL
	  AND deleted_at IS NULL
	LIMIT 1`

// HasActiveSessionSourceBelow reports whether the local archive contains an
// active source beneath path for agent. This watcher lookup intentionally
// remains on the writable SQLite DB rather than the server Store contract:
// filesystem events and their source-path reconciliation are local-only state,
// so PostgreSQL/Cockroach read backends cannot observe or service the query.
func (db *DB) HasActiveSessionSourceBelow(agent, path string) (bool, error) {
	lower, upper := activeSessionSourceBounds(path)
	var one int
	err := db.getReader().QueryRow(
		hasActiveSessionSourceBelowQuery, agent, lower, upper,
	).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking active %s source below %q: %w", agent, path, err)
	}
	return true, nil
}

func activeSessionSourceBounds(path string) (lower, upper string) {
	separator := byte(filepath.Separator)
	lower = filepath.Clean(path)
	if !strings.HasSuffix(lower, string(filepath.Separator)) {
		lower += string(filepath.Separator)
	}
	upperBytes := []byte(lower)
	upperBytes[len(upperBytes)-1] = separator + 1
	return lower, string(upperBytes)
}
