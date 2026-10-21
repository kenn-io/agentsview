package main

import (
	"context"

	"go.kenn.io/agentsview/internal/archivesource"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/sync"
)

// Local workers return write ownership before attached history is read, so the
// daemon's watcher can publish fresh local work between archived sources.
func withAttachedSources(manager *archivesource.Manager, local server.LocalSyncRunner) server.LocalSyncRunner {
	if manager == nil {
		return local
	}
	return func(ctx context.Context, progress func(sync.Progress)) (sync.SyncStats, error) {
		stats, err := local(ctx, progress)
		if err == nil && stats.ProcessingComplete() {
			var archiveStats sync.SyncStats
			archiveStats, err = manager.Drain(ctx, progress)
			stats.Merge(archiveStats)
		}
		return stats, err
	}
}
