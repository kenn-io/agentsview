package rawarchive

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/kit/atomicfile"
)

type CopyOptions struct {
	DataDir, Destination string
	Settings             RecoverySettings
	CustomModelPricing   map[string]config.CustomModelRate
}

// Copy creates a detached copy while the caller holds the source writer lock.
func Copy(ctx context.Context, options CopyOptions) (retErr error) {
	if err := options.Settings.validate(); err != nil {
		return err
	}
	source, err := canonicalRecoverySource(options.DataDir)
	if err != nil {
		return err
	}
	destination, err := recoveryDestination(options.Destination)
	if err != nil {
		return err
	}
	if err := rejectPathsOverlap(source, destination, "copy destination must be outside the archive data directory"); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("copy requires a new destination")
	}
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".archive-copy-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(stage)) }()
	path := filepath.Join(source, "sessions.db")
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := rawcapture.SnapshotSQLite(ctx, path, filepath.Join(stage, "sessions.db"), info); err != nil {
		return err
	}
	database, err := db.OpenIsolatedWithArchiveContent(ctx, filepath.Join(stage, "sessions.db"), options.Settings.ArchiveContent)
	if err != nil {
		return err
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
		return err
	}
	assets := filepath.Join(source, "assets")
	if info, err := os.Lstat(assets); err == nil {
		if !info.IsDir() {
			return errors.New("assets must be a directory")
		}
		if err := os.CopyFS(filepath.Join(stage, "assets"), os.DirFS(assets)); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	caches := db.UsageCacheFiles(path, databaseID)
	for i := len(caches) - 1; i >= 0; i-- {
		cache := caches[i]
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
			return err
		}
		break
	}
	if err := writeRecoveryConfig(stage, options.Settings); err != nil {
		return err
	}
	if len(options.CustomModelPricing) > 0 {
		prices := make(map[string]map[string]any, len(options.CustomModelPricing))
		for model, rate := range options.CustomModelPricing {
			if rate.PriceAs != "" {
				prices[model] = map[string]any{"price_as": rate.PriceAs}
			} else {
				prices[model] = map[string]any{
					"input_microdollars_per_mtok":             rate.InputMicrodollarsPerMTok,
					"output_microdollars_per_mtok":            rate.OutputMicrodollarsPerMTok,
					"cache_creation_microdollars_per_mtok":    rate.CacheCreationMicrodollarsPerMTok,
					"cache_creation_1h_microdollars_per_mtok": rate.CacheCreation1hMicrodollarsPerMTok,
					"cache_read_microdollars_per_mtok":        rate.CacheReadMicrodollarsPerMTok,
				}
			}
		}
		cfg := config.Config{DataDir: stage}
		if err := cfg.SaveSettings(map[string]any{"custom_model_pricing": prices}); err != nil {
			return err
		}
	}
	database, err = db.OpenIsolatedWithArchiveContent(ctx, filepath.Join(stage, "sessions.db"), options.Settings.ArchiveContent)
	if err != nil {
		return err
	}
	err = database.VerifyAssets(ctx, filepath.Join(stage, "assets"))
	if err = errors.Join(err, database.Close()); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return atomicfile.RenameNoReplace(stage, destination)
}
