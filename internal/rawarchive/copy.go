package rawarchive

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/kit/atomicfile"
)

type CopyOptions struct {
	DataDir, Destination string
	Settings             RecoverySettings
}

// Copy creates a detached copy while the caller holds the source writer lock.
func Copy(ctx context.Context, options CopyOptions) (report Report, retErr error) {
	if err := options.Settings.validate(); err != nil {
		return report, err
	}
	source, err := canonicalRecoverySource(options.DataDir)
	if err != nil {
		return report, err
	}
	destination, err := recoveryDestination(options.Destination)
	if err != nil {
		return report, err
	}
	if err := rejectPathsOverlap(source, destination, "copy destination must be outside the archive data directory"); err != nil {
		return report, err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return report, err
		}
		return report, errors.New("copy requires a new destination")
	}
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".archive-copy-")
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(stage)) }()
	path := filepath.Join(source, "sessions.db")
	info, err := os.Lstat(path)
	if err != nil {
		return report, err
	}
	if err := rawcapture.SnapshotSQLite(ctx, path, filepath.Join(stage, "sessions.db"), info); err != nil {
		return report, err
	}
	database, err := db.OpenIsolatedWithArchiveContent(ctx, filepath.Join(stage, "sessions.db"), options.Settings.ArchiveContent)
	if err != nil {
		return report, err
	}
	err = database.ClearHostSources(ctx)
	if err == nil {
		err = database.EnableArchiveOnly(ctx)
	}
	var databaseID string
	if err == nil {
		databaseID, err = database.GetDatabaseID(ctx)
	}
	if err = errors.Join(err, database.Close()); err != nil {
		return report, err
	}
	assets := filepath.Join(source, "assets")
	if info, err := os.Lstat(assets); err == nil {
		if !info.IsDir() {
			return report, errors.New("assets must be a directory")
		}
		if err := os.CopyFS(filepath.Join(stage, "assets"), os.DirFS(assets)); err != nil {
			return report, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return report, err
	}
	for _, cache := range db.UsageCacheFiles(path, databaseID) {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		info, err := os.Lstat(cache)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		target := filepath.Join(stage, filepath.Base(cache))
		if err == nil {
			err = rawcapture.SnapshotSQLite(ctx, cache, target, info)
		}
		if err == nil {
			err = db.ClearCopiedUsageRollups(ctx, target)
		}
		if err != nil {
			return report, err
		}
	}
	if err := writeRecoveryConfig(stage, options.Settings); err != nil {
		return report, err
	}
	database, err = db.OpenIsolatedWithArchiveContent(ctx, filepath.Join(stage, "sessions.db"), options.Settings.ArchiveContent)
	if err != nil {
		return report, err
	}
	err = database.VerifyAssets(ctx, filepath.Join(stage, "assets"))
	if err = errors.Join(err, database.Close()); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := atomicfile.RenameNoReplace(stage, destination); err != nil {
		return report, err
	}
	return report, nil
}
