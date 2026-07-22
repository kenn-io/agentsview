package main

import (
	"context"
	"log"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

// remoteSourceSyncEngine is the scoped-sync surface the periodic remote pass
// needs.
type remoteSourceSyncEngine interface {
	SyncRootsSince(
		ctx context.Context, roots []string, since time.Time,
		onProgress sync.ProgressFunc,
	) sync.SyncStats
}

// remoteSourceSyncRoots selects the configured remote object roots. Filesystem
// watchers cannot observe them, the unwatched poller's os.Stat probes never see
// them as present, and every local reconciliation path (scheduled, audit,
// recovery) excludes remote roots from its scope, so post-startup changes there
// are only discovered by the scheduled remote pass. The selection derives from
// the configured dir's scheme alone — no provider construction or disk probing
// — so it needs no per-provider capability.
func remoteSourceSyncRoots(cfg config.Config) []string {
	var roots []string
	for _, def := range parser.Registry {
		for _, dir := range cfg.ResolveDirs(def.Type) {
			if isRemoteSourceRoot(dir) {
				roots = appendUniqueString(roots, dir)
			}
		}
	}
	slices.Sort(roots)
	return roots
}

// isRemoteSourceRoot matches provider discovery's scheme check exactly:
// Claude and Codex s3 discovery recognize only lowercase "s3://", and startup
// cleans any other spelling as a filesystem path that yields nothing. Selecting
// case-insensitively here would make the periodic pass claim roots the
// providers cannot sync, so selection is lowercase-only to keep periodic
// behavior identical to startup behavior. (The engine's
// isRemoteReconciliationRoot stays case-insensitive on purpose: excluding an
// uppercase root from local reconciliation is a harmless no-op, since no
// sessions can ever exist under it.)
func isRemoteSourceRoot(dir string) bool {
	return strings.HasPrefix(dir, "s3://")
}

// runRemoteSourceSyncPass syncs the configured remote roots so new or changed
// remote sessions keep flowing in after startup. The pass is bounded to those
// roots: discovery lists only the remote prefixes and unchanged objects skip on
// their durable fingerprints before any fetch, so no local archive walk runs.
// A zero cutoff covers the full remote scope, matching the pre-worker
// 15-minute SyncAll behavior for these roots.
func runRemoteSourceSyncPass(
	ctx context.Context, engine remoteSourceSyncEngine, roots []string,
) {
	if len(roots) == 0 {
		return
	}
	stats := engine.SyncRootsSince(ctx, roots, time.Time{}, nil)
	if stats.Failed > 0 || stats.Aborted {
		log.Printf(
			"scheduled remote source sync: %d synced, %d failed, aborted=%v",
			stats.Synced, stats.Failed, stats.Aborted,
		)
	}
}
