package db

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// OpenEmptyReadOnly serves an empty catalog without creating a local archive.
// The current schema is initialized in disposable storage removed on Close.
func OpenEmptyReadOnly(ctx context.Context) (*DB, error) {
	dir, err := os.MkdirTemp("", "agentsview-empty-")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "sessions.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	empty, err := OpenFreshIsolatedContext(ctx, path)
	if err == nil {
		err = errors.Join(empty.RebuildBulkImportIndexes(ctx), empty.Close())
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	empty, err = OpenReadOnly(ctx, path)
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	empty.temporaryDir = dir
	return empty, nil
}
