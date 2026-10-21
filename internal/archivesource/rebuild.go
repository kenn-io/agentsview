package archivesource

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/cenkalti/backoff/v7"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/docbank/docbank"
)

// RebuildContributors prepares sources without retaining repository leases or
// SQLite connections. The coordinator owns the replacement and all writes.
func RebuildContributors(ctx context.Context, cfg config.Config) (syncer.RebuildOptions, error) {
	var options syncer.RebuildOptions
	for _, source := range cfg.ArchiveSources {
		if _, err := os.Stat(source.Path()); errors.Is(err, fs.ErrNotExist) {
			log.Printf("archive source %q unavailable; retaining indexed history", source.Name)
			continue
		} else if err != nil {
			return options, err
		}
		index, err := OpenIndex(ctx, source, filepath.Join(cfg.DataDir, "archive-sources"), nil)
		if archiveBusy(err) {
			log.Printf("archive source %q busy; retaining indexed history", source.Name)
			continue
		}
		if err != nil {
			return options, err
		}
		prefixes, err := index.idPrefixes(ctx)
		var sources []db.ArchiveSourceProvenance
		if err == nil {
			sources, err = index.rebuildSources(ctx)
		}
		err = errors.Join(err, index.Close())
		if err != nil {
			return options, err
		}
		options.Contributors = append(options.Contributors, syncer.RebuildContributor{
			Name: source.Name, IDPrefixes: prefixes, Config: syncer.EngineConfig{Ephemeral: true},
			ArchiveSources: sources,
			Run: func(ctx context.Context, engine *syncer.Engine, database *db.DB, progress syncer.ProgressFunc) (syncer.SyncStats, error) {
				return rebuildAttachment(ctx, cfg, source, index.Info, engine, database, progress)
			},
		})
	}
	return options, nil
}

func (i *Index) rebuildSources(ctx context.Context) ([]db.ArchiveSourceProvenance, error) {
	rows, err := i.database.QueryContext(ctx, `SELECT r.device_id,s.root_id,s.source_key,s.manifest_id
		FROM sources s JOIN roots r ON r.id=s.root_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := []db.ArchiveSourceProvenance{}
	for rows.Next() {
		p := db.ArchiveSourceProvenance{Source: db.ArchiveSourceIdentity{ArchiveID: i.ArchiveID}}
		if err := rows.Scan(&p.Source.DeviceID, &p.Source.RootID, &p.Source.SourceKey, &p.ManifestID); err != nil {
			return nil, err
		}
		sources = append(sources, p)
	}
	return sources, rows.Err()
}

func (i *Index) idPrefixes(ctx context.Context) ([]string, error) {
	rows, err := i.database.QueryContext(ctx, `SELECT device_id FROM roots GROUP BY device_id ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prefixes []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		prefixes = append(prefixes, id+"~")
	}
	return prefixes, rows.Err()
}

func rebuildAttachment(ctx context.Context, cfg config.Config, source config.ArchiveSource, expected docbank.BackupReaderInfo, engine *syncer.Engine, database *db.DB, progress syncer.ProgressFunc) (total syncer.SyncStats, retErr error) {
	index, err := waitForRepository(ctx, source.Name, 0, progress, func() (*Index, error) {
		return OpenIndex(ctx, source, filepath.Join(cfg.DataDir, "archive-sources"), indexProgress(progress))
	})
	if err != nil {
		return total, err
	}
	defer func() { retErr = errors.Join(retErr, index.Close()) }()
	if index.Info.RepositoryID != expected.RepositoryID || index.Info.SnapshotID != expected.SnapshotID {
		return total, fmt.Errorf("%w: archive changed before rebuild", db.ErrArchiveSourceConflict)
	}
	if err := index.register(ctx, database, cfg); err != nil {
		return total, err
	}
	scratch := filepath.Join(cfg.DataDir, "archive-sources", "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return total, err
	}
	after := ""
	completed := 0
	for {
		sources, err := index.Sources(ctx, after, 128)
		if err != nil {
			return total, err
		}
		if len(sources) == 0 {
			return total, nil
		}
		for _, item := range sources {
			target, err := completionTarget(item, cfg)
			if err != nil {
				return total, err
			}
			stats, err := waitForRepository(ctx, source.Name, completed, progress, func() (syncer.SyncStats, error) {
				return index.syncSource(ctx, source.Repository, item, cfg, engine, scratch, target, progress)
			})
			total.Merge(stats)
			if err != nil {
				return total, err
			}
			completed++
			after = item.ManifestID
		}
	}
}

// Only writer contention can become available without changing the snapshot.
func waitForRepository[T any](ctx context.Context, name string, completed int, progress syncer.ProgressFunc, work func() (T, error)) (T, error) {
	started := time.Now()
	policy := backoff.NewExponentialBackOff()
	policy.InitialInterval = 250 * time.Millisecond
	policy.MaxInterval = 5 * time.Second
	return backoff.Retry(ctx, func() (T, error) {
		value, err := work()
		if err != nil && !archiveBusy(err) {
			return value, backoff.Permanent(err)
		}
		return value, err
	}, backoff.WithBackOff(policy), backoff.WithMaxTries(0), backoff.WithMaxElapsedTime(0), backoff.WithNotify(func(_ error, _ time.Duration) {
		if progress != nil {
			progress(syncer.Progress{Phase: syncer.PhaseSyncing, Resync: true, Detail: fmt.Sprintf("Waiting for archive repository %q: %d sources complete, %s elapsed", name, completed, time.Since(started).Round(time.Second))})
		}
	}))
}

func archiveBusy(err error) bool {
	return errors.Is(err, docbank.ErrBackupRepositoryLocked) || errors.Is(err, errDirectoryBusy)
}
