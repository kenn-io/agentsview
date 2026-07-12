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
	root string,
) (SyncStats, error) {
	var stats SyncStats
	if err := validateTargetSetPaths(targets); err != nil {
		return stats, err
	}
	if len(targets.Dirs) == 0 {
		return stats, nil
	}
	layout, config, err := newImportInputs(
		im.Host, im.BlockedResultCategories, targets, root,
	)
	if err != nil {
		return stats, err
	}

	engine := syncpkg.NewEngine(im.DB, config)
	defer engine.Close()

	if !im.Full {
		if err := loadImportSkipCache(im.DB, im.Host, engine, layout); err != nil {
			return stats, err
		}
	}

	engineStats := engine.SyncAll(ctx, hostProgress(im.Host, im.Progress))
	if err := saveEngineSkipCache(im.DB, engine, layout.paths); err != nil {
		return stats, err
	}
	stats.SessionsSynced = engineStats.Synced
	stats.SessionsTotal = engineStats.TotalSessions
	stats.Skipped = engineStats.Skipped
	stats.Failed = engineStats.Failed
	return stats, nil
}

// importLayout maps stable remote paths to one prepared source root. Keeping
// this mapping independent from Importer lets prepared HTTP imports and future
// rebuild contributors share the exact engine inputs and cache translation.
type importLayout struct {
	engineDirs map[parser.AgentType][]string
	paths      remotePathMap
}

type remotePathMap struct {
	host       string
	root       string
	remoteDirs []string
	localDirs  []string
}

func newImportLayout(targets TargetSet, root string) (importLayout, error) {
	layout := importLayout{
		engineDirs: make(map[parser.AgentType][]string),
	}
	layout.paths.root = root
	for agentType, agentDirList := range targets.Dirs {
		for _, remoteDir := range agentDirList {
			local, err := safeRemappedRemotePath(root, remoteDir)
			if err != nil {
				return importLayout{}, err
			}
			layout.engineDirs[agentType] = append(layout.engineDirs[agentType], local)
			layout.paths.remoteDirs = append(layout.paths.remoteDirs, remoteDir)
			layout.paths.localDirs = append(layout.paths.localDirs, local)
		}
	}
	return layout, nil
}

func newImportInputs(
	host string,
	blockedResultCategories []string,
	targets TargetSet,
	root string,
) (importLayout, syncpkg.EngineConfig, error) {
	layout, err := newImportLayout(targets, root)
	if err != nil {
		return importLayout{}, syncpkg.EngineConfig{}, err
	}
	layout.paths.host = host
	return layout, importEngineConfig(
		host, blockedResultCategories, layout,
	), nil
}

func (p remotePathMap) pathRewriter() func(string) string {
	return func(localPath string) string {
		remotePath, ok := tempPathToRemotePath(
			localPath, p.remoteDirs, p.localDirs,
		)
		if !ok {
			remotePath = RemapToRemotePath(p.root, "", localPath)
		}
		return p.host + ":" + remotePath
	}
}

func importEngineConfig(
	host string,
	blockedResultCategories []string,
	layout importLayout,
) syncpkg.EngineConfig {
	return syncpkg.EngineConfig{
		AgentDirs:               layout.engineDirs,
		Machine:                 host,
		IDPrefix:                host + "~",
		PathRewriter:            layout.paths.pathRewriter(),
		Ephemeral:               true,
		BlockedResultCategories: blockedResultCategories,
	}
}

func loadImportSkipCache(
	database *db.DB,
	host string,
	engine *syncpkg.Engine,
	layout importLayout,
) error {
	remoteCache, err := database.LoadRemoteSkippedFiles(host)
	if err != nil {
		return fmt.Errorf("load skip cache: %w", err)
	}
	remoteCache = migrateVisualStudioCopilotRemoteSkips(database, host, remoteCache)
	engine.InjectSkipCache(translateRemoteCacheToTemp(
		remoteCache, layout.paths.remoteDirs, layout.paths.localDirs,
	))
	return nil
}

func hostProgress(host string, progress syncpkg.ProgressFunc) syncpkg.ProgressFunc {
	if progress == nil {
		return nil
	}
	return func(p syncpkg.Progress) {
		progress(transformHostProgress(host, p))
	}
}

func transformHostProgress(host string, p syncpkg.Progress) syncpkg.Progress {
	switch {
	case p.Phase == syncpkg.PhaseDiscovering:
		p.Detail = fmt.Sprintf("Discovering sessions from %s", host)
	case p.Phase == syncpkg.PhaseSyncing && p.SessionsTotal > 0:
		p.Detail = fmt.Sprintf("Processing sessions from %s", host)
	case p.Phase == syncpkg.PhaseDone && p.SessionsTotal > 0:
		p.Detail = fmt.Sprintf("Processing sessions from %s", host)
	}
	return p
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

func saveEngineSkipCache(
	database *db.DB,
	engine *syncpkg.Engine,
	paths remotePathMap,
) error {
	snapshot := engine.SnapshotSkipCache()
	remoteCache := make(map[string]int64, len(snapshot))
	for localPath, mtime := range snapshot {
		remotePath, ok := tempPathToRemotePath(
			localPath, paths.remoteDirs, paths.localDirs,
		)
		if ok {
			remoteCache[remotePath] = mtime
		}
	}
	if err := database.ReplaceRemoteSkippedFiles(paths.host, remoteCache); err != nil {
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
