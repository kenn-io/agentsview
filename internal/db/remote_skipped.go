package db

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// LoadRemoteSkippedFiles returns persisted skip cache entries
// for the given remote host as a map from path to file_mtime.
func (db *DB) LoadRemoteSkippedFiles(ctx context.Context,
	host string,
) (map[string]int64, error) {
	rows, err := db.getReader().Query(ctx,
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

// LoadRemoteSkippedFilesForScopes loads only cache families attached to the
// given exact paths or rooted below the given directory paths. Prefix range
// probes use the (host, path) primary key; the Go-side check removes lexical
// neighbors admitted by the range.
func (db *DB) LoadRemoteSkippedFilesForScopes(
	ctx context.Context,
	host string,
	exactPaths []string,
	rootPaths []string,
) (map[string]int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	exact := make(map[string]struct{}, len(exactPaths))
	roots := make(map[string]struct{}, len(rootPaths))
	for _, path := range exactPaths {
		if path != "" {
			exact[path] = struct{}{}
		}
	}
	for _, path := range rootPaths {
		if path != "" {
			roots[path] = struct{}{}
		}
	}
	prefixes := make([]string, 0, len(exact)+len(roots))
	for path := range exact {
		prefixes = append(prefixes, path)
	}
	for path := range roots {
		prefixes = append(prefixes, path)
	}
	sort.Strings(prefixes)
	prefixes = slices.Compact(prefixes)

	result := make(map[string]int64)
	for _, prefix := range prefixes {
		if err := func() error {
			rows, err := db.getReader().QueryContext(
				ctx,
				`SELECT path, file_mtime FROM remote_skipped_files
			 WHERE host = ? AND path >= ? AND path < ?`,
				host, prefix, prefix+"\U0010ffff",
			)
			if err != nil {
				return fmt.Errorf(
					"loading scoped remote skipped files for %s: %w", host, err,
				)
			}
			defer rows.Close()
			for rows.Next() {
				var path string
				var mtime int64
				if err := rows.Scan(&path, &mtime); err != nil {
					_ = rows.Close()
					return fmt.Errorf(
						"scanning scoped remote skipped file: %w", err,
					)
				}
				base := remoteSkippedFileBasePath(path)
				_, exactMatch := exact[base]
				if !exactMatch && !remoteSkippedFileWithinAnyRoot(base, roots) {
					continue
				}
				result[path] = mtime
			}
			if err := rows.Close(); err != nil {
				return fmt.Errorf(
					"closing scoped remote skipped file rows: %w", err,
				)
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf(
					"iterating scoped remote skipped files: %w", err,
				)
			}

			return nil
		}(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func remoteSkippedFileBasePath(path string) string {
	base, _, _ := strings.Cut(path, "?")
	return base
}

func remoteSkippedFileWithinAnyRoot(path string, roots map[string]struct{}) bool {
	path = strings.ReplaceAll(path, `\`, "/")
	for root := range roots {
		root = strings.TrimRight(strings.ReplaceAll(root, `\`, "/"), "/")
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// ClearRemoteSkippedFiles removes all skip cache entries for the given host.
// Entries for other hosts are not affected.
func (db *DB) ClearRemoteSkippedFiles(ctx context.Context, host string) error {
	if err := db.requireWritable(); err != nil {
		return err
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	if _, err := db.getWriter().Exec(ctx,
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
func (db *DB) ReplaceRemoteSkippedFiles(ctx context.Context,
	host string, entries map[string]int64,
) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		"DELETE FROM remote_skipped_files WHERE host = ?",
		host,
	); err != nil {
		return fmt.Errorf(
			"clearing remote skipped files for %s: %w",
			host, err,
		)
	}

	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO remote_skipped_files"+
			" (host, path, file_mtime) VALUES (?, ?, ?)",
	)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for path, mtime := range entries {
		if _, err := stmt.ExecContext(ctx, host, path, mtime); err != nil {
			return fmt.Errorf(
				"inserting remote skipped file %s: %w",
				path, err,
			)
		}
	}

	return tx.Commit()
}

// ApplyRemoteSkippedFileChanges deletes and upserts selected host cache rows
// in one transaction. Unmentioned paths and other hosts are left untouched.
func (db *DB) ApplyRemoteSkippedFileChanges(ctx context.Context,
	host string,
	deletes []string,
	upserts map[string]int64,
) error {
	if err := db.requireWritable(); err != nil {
		return err
	}
	if len(deletes) == 0 && len(upserts) == 0 {
		return nil
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.getWriter().Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin remote skip cache update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	deleteStmt, err := tx.PrepareContext(ctx,
		"DELETE FROM remote_skipped_files WHERE host = ? AND path = ?",
	)
	if err != nil {
		return fmt.Errorf("prepare remote skip cache delete: %w", err)
	}
	defer deleteStmt.Close()
	for _, path := range deletes {
		if _, err := deleteStmt.ExecContext(ctx, host, path); err != nil {
			return fmt.Errorf("deleting remote skipped file %s: %w", path, err)
		}
	}

	upsertStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO remote_skipped_files (host, path, file_mtime)
		 VALUES (?, ?, ?)
		 ON CONFLICT(host, path) DO UPDATE SET file_mtime = excluded.file_mtime`,
	)
	if err != nil {
		return fmt.Errorf("prepare remote skip cache upsert: %w", err)
	}
	defer upsertStmt.Close()
	paths := make([]string, 0, len(upserts))
	for path := range upserts {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if _, err := upsertStmt.ExecContext(ctx, host, path, upserts[path]); err != nil {
			return fmt.Errorf("upserting remote skipped file %s: %w", path, err)
		}
	}
	return tx.Commit()
}
