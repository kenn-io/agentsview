package db

import "fmt"

// SourceFailure is the identity a source file had when its parse last failed.
// Missing records a source that did not exist; MTimeNS is zero then.
type SourceFailure struct {
	MTimeNS int64
	Missing bool
}

// LoadSourceFailures returns all persisted source failures keyed by cache key.
func (db *DB) LoadSourceFailures() (map[string]SourceFailure, error) {
	rows, err := db.getReader().Query(
		"SELECT cache_key, file_mtime, missing FROM source_failures",
	)
	if err != nil {
		return nil, fmt.Errorf("loading source failures: %w", err)
	}
	defer rows.Close()

	result := make(map[string]SourceFailure)
	for rows.Next() {
		var key string
		var failure SourceFailure
		if err := rows.Scan(
			&key, &failure.MTimeNS, &failure.Missing,
		); err != nil {
			return nil, fmt.Errorf("scanning source failure: %w", err)
		}
		result[key] = failure
	}
	return result, rows.Err()
}

// ReplaceSourceFailures replaces all persisted source failures in a single
// transaction.
func (db *DB) ReplaceSourceFailures(
	entries map[string]SourceFailure,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM source_failures"); err != nil {
		return fmt.Errorf("clearing source failures: %w", err)
	}

	stmt, err := tx.Prepare(
		"INSERT INTO source_failures" +
			" (cache_key, file_mtime, missing) VALUES (?, ?, ?)",
	)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for key, failure := range entries {
		if _, err := stmt.Exec(
			key, failure.MTimeNS, failure.Missing,
		); err != nil {
			return fmt.Errorf("inserting source failure %s: %w", key, err)
		}
	}

	return tx.Commit()
}
