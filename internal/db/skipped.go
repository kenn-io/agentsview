package db

import "fmt"

// LoadSkippedFiles returns all persisted skip cache entries
// as a map from file_path to file_mtime.
func (db *DB) LoadSkippedFiles() (map[string]int64, error) {
	rows, err := db.getReader().Query(
		"SELECT file_path, file_mtime FROM skipped_files",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"loading skipped files: %w", err,
		)
	}
	defer rows.Close()

	result := make(map[string]int64)
	for rows.Next() {
		var path string
		var mtime int64
		if err := rows.Scan(&path, &mtime); err != nil {
			return nil, fmt.Errorf(
				"scanning skipped file: %w", err,
			)
		}
		result[path] = mtime
	}
	return result, rows.Err()
}

// ReplaceSkippedFiles replaces all skip cache entries in a
// single transaction. This is called after each sync cycle
// to persist the in-memory skip cache.
func (db *DB) ReplaceSkippedFiles(
	entries map[string]int64,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		"DELETE FROM skipped_files",
	); err != nil {
		return fmt.Errorf("clearing skipped files: %w", err)
	}

	stmt, err := tx.Prepare(
		"INSERT INTO skipped_files" +
			" (file_path, file_mtime) VALUES (?, ?)",
	)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for path, mtime := range entries {
		if _, err := stmt.Exec(path, mtime); err != nil {
			return fmt.Errorf(
				"inserting skipped file %s: %w",
				path, err,
			)
		}
	}

	return tx.Commit()
}

// DeleteSkippedFile removes a single skip cache entry.
func (db *DB) DeleteSkippedFile(path string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		"DELETE FROM skipped_files WHERE file_path = ?",
		path,
	)
	return err
}

// DeleteSkippedFileAndPrefix removes one exact cache key and every variant
// beginning with prefix. Sync uses this for hash-qualified source keys so a
// tombstone cannot leave a durable sibling that survives process restart.
func (db *DB) DeleteSkippedFileAndPrefix(path, prefix string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	_, err := db.getWriter().Exec(
		`DELETE FROM skipped_files
		 WHERE file_path = ? OR substr(file_path, 1, length(?)) = ?`,
		path, prefix, prefix,
	)
	return err
}
