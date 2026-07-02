package remotesync

import (
	"context"
	"fmt"
	"log"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func (im Importer) ImportExtracted(
	ctx context.Context,
	targets TargetSet,
	tmpDir string,
) (SyncStats, error) {
	var stats SyncStats
	if err := validateTargetSetPaths(targets); err != nil {
		return stats, err
	}
	if len(targets.Dirs) == 0 {
		return stats, nil
	}
	engineDirs := make(map[parser.AgentType][]string)
	remoteDirs := make([]string, 0)
	tempDirs := make([]string, 0)
	for agentType, agentDirList := range targets.Dirs {
		for _, remoteDir := range agentDirList {
			local, err := safeRemappedRemotePath(tmpDir, remoteDir)
			if err != nil {
				return stats, err
			}
			engineDirs[agentType] = append(engineDirs[agentType], local)
			remoteDirs = append(remoteDirs, remoteDir)
			tempDirs = append(tempDirs, local)
		}
	}

	rewriter := func(tempPath string) string {
		remotePath, ok := tempPathToRemotePath(tempPath, remoteDirs, tempDirs)
		if !ok {
			remotePath = RemapToRemotePath(tmpDir, "", tempPath)
		}
		return im.Host + ":" + remotePath
	}

	engine := syncpkg.NewEngine(im.DB, syncpkg.EngineConfig{
		AgentDirs:               engineDirs,
		Machine:                 im.Host,
		IDPrefix:                im.Host + "~",
		PathRewriter:            rewriter,
		Ephemeral:               true,
		BlockedResultCategories: im.BlockedResultCategories,
	})
	defer engine.Close()

	if !im.Full {
		remoteCache, err := im.DB.LoadRemoteSkippedFiles(im.Host)
		if err != nil {
			return stats, fmt.Errorf("load skip cache: %w", err)
		}
		remoteCache = migrateVisualStudioCopilotRemoteSkips(
			im.DB, im.Host, remoteCache,
		)
		translated := translateRemoteCacheToTemp(
			remoteCache, remoteDirs, tempDirs,
		)
		engine.InjectSkipCache(translated)
	}

	engineStats := engine.SyncAll(ctx, im.hostProgress)
	if err := im.saveSkipCache(engine, remoteDirs, tempDirs); err != nil {
		return stats, err
	}
	stats.SessionsSynced = engineStats.Synced
	stats.SessionsTotal = engineStats.TotalSessions
	stats.Skipped = engineStats.Skipped
	stats.Failed = engineStats.Failed
	return stats, nil
}

func (im Importer) hostProgress(p syncpkg.Progress) {
	if im.Progress == nil {
		return
	}
	switch {
	case p.Phase == syncpkg.PhaseDiscovering:
		p.Detail = fmt.Sprintf("Discovering sessions from %s", im.Host)
	case p.Phase == syncpkg.PhaseSyncing && p.SessionsTotal > 0:
		p.Detail = fmt.Sprintf("Processing sessions from %s", im.Host)
	case p.Phase == syncpkg.PhaseDone && p.SessionsTotal > 0:
		p.Detail = fmt.Sprintf("Processing sessions from %s", im.Host)
	}
	im.Progress(p)
}

func translateRemoteCacheToTemp(
	remoteCache map[string]int64,
	remoteDirs []string,
	tempDirs []string,
) map[string]int64 {
	translated := make(map[string]int64, len(remoteCache))
	for remotePath, mtime := range remoteCache {
		for i, rd := range remoteDirs {
			if rel, ok := remoteArchiveRel(rd, remotePath); ok {
				local, err := safeLocalArchivePath(tempDirs[i], rel)
				if err != nil {
					break
				}
				translated[local] = mtime
				break
			}
		}
	}
	return translated
}

func (im Importer) saveSkipCache(
	engine *syncpkg.Engine,
	remoteDirs []string,
	tempDirs []string,
) error {
	snapshot := engine.SnapshotSkipCache()
	remoteCache := make(map[string]int64, len(snapshot))
	for tempPath, mtime := range snapshot {
		remotePath, ok := tempPathToRemotePath(tempPath, remoteDirs, tempDirs)
		if ok {
			remoteCache[remotePath] = mtime
		}
	}
	if err := im.DB.ReplaceRemoteSkippedFiles(
		im.Host, remoteCache,
	); err != nil {
		return fmt.Errorf("save skip cache: %w", err)
	}
	return nil
}

// visualStudioCopilotRemoteSkipMigrationKey returns the per-host
// pg_sync_state flag that records whether stale Visual Studio
// Copilot entries have been scrubbed from this host's remote
// skip cache. The flag is per host because each host's
// remote_skipped_files are independent.
func visualStudioCopilotRemoteSkipMigrationKey(host string) string {
	return "visualstudio_copilot_remote_skip_migration_v1:" + host
}

// migrateVisualStudioCopilotRemoteSkips removes stale Visual
// Studio Copilot skip entries from this host's remote skip cache
// and returns the cleaned cache. Older builds cached trace
// read/scan errors keyed by mtime, so an unchanged unreadable
// trace would be skipped forever instead of retried under the
// non-cacheable read-error behavior. The scrub clears both
// physical trace paths and <traceFile>#<conversationID> virtual
// paths once per host: a pg_sync_state flag is set after the
// first pass so conversation skips legitimately re-cached later
// are preserved instead of being filtered on every sync.
//
// It mirrors sync.migrateVisualStudioCopilotSkips and reuses the
// same path classifier: the cleaned cache is persisted before
// the flag is set, so a partial failure is retried on the next
// sync rather than being falsely marked complete. On any error
// it logs and returns the input unchanged so the sync proceeds.
func migrateVisualStudioCopilotRemoteSkips(
	database *db.DB,
	host string,
	remoteCache map[string]int64,
) map[string]int64 {
	key := visualStudioCopilotRemoteSkipMigrationKey(host)
	done, err := database.GetSyncState(key)
	if err != nil {
		log.Printf(
			"visual studio copilot remote skip migration (%s): %v",
			host, err,
		)
		return remoteCache
	}
	if done != "" {
		return remoteCache
	}

	cleaned := make(map[string]int64, len(remoteCache))
	stale := 0
	for path, mtime := range remoteCache {
		if syncpkg.IsVisualStudioCopilotSkipPath(path) {
			stale++
			continue
		}
		cleaned[path] = mtime
	}

	if stale > 0 {
		if err := database.ReplaceRemoteSkippedFiles(
			host, cleaned,
		); err != nil {
			log.Printf(
				"visual studio copilot remote skip migration (%s): "+
					"persist cleaned skip cache: %v",
				host, err,
			)
			return remoteCache
		}
		log.Printf(
			"visual studio copilot remote skip migration (%s): "+
				"cleared %d skip entries",
			host, stale,
		)
	}

	if err := database.SetSyncState(key, "done"); err != nil {
		log.Printf(
			"visual studio copilot remote skip migration (%s): "+
				"set flag: %v",
			host, err,
		)
	}
	return cleaned
}
