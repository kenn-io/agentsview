package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

var ErrArchiveOnly = errors.New("archive-only database: source sync and publication are disabled; use archive import or archive reparse")

// EnableArchiveOnly permanently marks a stopped or staged archive before its
// first startup. There is deliberately no runtime or configuration override.
func (db *DB) EnableArchiveOnly(ctx context.Context) error {
	_, err := db.getWriter().Exec(ctx, `INSERT INTO archive_metadata (key, value)
 VALUES ('archive_only', '1') ON CONFLICT(key) DO UPDATE SET value = '1'`)
	return err
}

func (db *DB) IsArchiveOnly(ctx context.Context) (bool, error) {
	var enabled bool
	err := db.getReader().QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM archive_metadata WHERE key = 'archive_only' AND value = '1')`).Scan(&enabled)
	return enabled, err
}

func (db *DB) RequireSourceSync(ctx context.Context) error {
	enabled, err := db.IsArchiveOnly(ctx)
	if err != nil {
		return fmt.Errorf("reading archive mode: %w", err)
	}
	if enabled {
		return ErrArchiveOnly
	}
	return nil
}

// ArchiveOnlyAt checks custody before startup can launch a source worker. It
// does not create or migrate the database, including on a first installation.
func ArchiveOnlyAt(ctx context.Context, path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	conn, err := sql.Open(sqliteArchiveDriverName, makeDSN(path, true))
	if err != nil {
		return false, err
	}
	defer conn.Close()
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type='table' AND name='archive_metadata')`).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	var enabled bool
	err = conn.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM archive_metadata WHERE key='archive_only' AND value='1')`).Scan(&enabled)
	return enabled, err
}
