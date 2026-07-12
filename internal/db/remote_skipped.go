package db

import "fmt"

// LoadRemoteSkippedFiles returns persisted skip cache entries
// for the given remote host as a map from path to file_mtime.
func (db *DB) LoadRemoteSkippedFiles(
	host string,
) (map[string]int64, error) {
	rows, err := db.getReader().Query(
		"SELECT path, file_mtime FROM remote_skipped_files"+
			" WHERE host = ?",
		host,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"loading remote skipped files for %s: %w",
			host, err,
		)
	}
	defer rows.Close()

	result := make(map[string]int64)
	for rows.Next() {
		var path string
		var mtime int64
		if err := rows.Scan(&path, &mtime); err != nil {
			return nil, fmt.Errorf(
				"scanning remote skipped file: %w", err,
			)
		}
		result[path] = mtime
	}
	return result, rows.Err()
}

// ClearRemoteSkippedFiles removes all skip cache entries for the given host.
// Entries for other hosts are not affected.
func (db *DB) ClearRemoteSkippedFiles(host string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if _, err := db.getWriter().Exec(
		"DELETE FROM remote_skipped_files WHERE host = ?",
		host,
	); err != nil {
		return fmt.Errorf(
			"clearing remote skipped files for %s: %w",
			host, err,
		)
	}
	return nil
}

// ReplaceRemoteSkippedFiles replaces all skip cache entries
// for the given host in a single transaction. Entries for
// other hosts are not affected.
func (db *DB) ReplaceRemoteSkippedFiles(
	host string, entries map[string]int64,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin()
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(
		"DELETE FROM remote_skipped_files WHERE host = ?",
		host,
	); err != nil {
		return fmt.Errorf(
			"clearing remote skipped files for %s: %w",
			host, err,
		)
	}

	stmt, err := tx.Prepare(
		"INSERT INTO remote_skipped_files" +
			" (host, path, file_mtime) VALUES (?, ?, ?)",
	)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for path, mtime := range entries {
		if _, err := stmt.Exec(host, path, mtime); err != nil {
			return fmt.Errorf(
				"inserting remote skipped file %s: %w",
				path, err,
			)
		}
	}

	return tx.Commit()
}
