package ssh

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

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
func (rs *RemoteSync) migrateVisualStudioCopilotRemoteSkips(
	remoteCache map[string]int64,
) map[string]int64 {
	key := visualStudioCopilotRemoteSkipMigrationKey(rs.Host)
	done, err := rs.DB.GetSyncState(key)
	if err != nil {
		log.Printf(
			"visual studio copilot remote skip migration (%s): %v",
			rs.Host, err,
		)
		return remoteCache
	}
	if done != "" {
		return remoteCache
	}

	cleaned := make(map[string]int64, len(remoteCache))
	stale := 0
	for path, mtime := range remoteCache {
		if sync.IsVisualStudioCopilotSkipPath(path) {
			stale++
			continue
		}
		cleaned[path] = mtime
	}

	if stale > 0 {
		if err := rs.DB.ReplaceRemoteSkippedFiles(
			rs.Host, cleaned,
		); err != nil {
			log.Printf(
				"visual studio copilot remote skip migration (%s): "+
					"persist cleaned skip cache: %v",
				rs.Host, err,
			)
			return remoteCache
		}
		log.Printf(
			"visual studio copilot remote skip migration (%s): "+
				"cleared %d skip entries",
			rs.Host, stale,
		)
	}

	if err := rs.DB.SetSyncState(key, "done"); err != nil {
		log.Printf(
			"visual studio copilot remote skip migration (%s): "+
				"set flag: %v",
			rs.Host, err,
		)
	}
	return cleaned
}

// SyncStats summarizes the outcome of a remote sync run.
type SyncStats struct {
	SessionsSynced int
	SessionsTotal  int
	Skipped        int
	Failed         int
}

// RemoteSync orchestrates pulling session data from a remote
// host over SSH, parsing it, and writing it to the local DB.
type RemoteSync struct {
	Host                    string
	User                    string
	Port                    int
	Full                    bool
	DB                      *db.DB
	SSHOpts                 []string // extra args passed to ssh (e.g. -i keyfile)
	BlockedResultCategories []string
}

// Run executes the full remote sync flow: resolve dirs,
// download via tar, then delegate to sync.Engine for
// discovery, parsing, and writing.
func (rs *RemoteSync) Run(
	ctx context.Context,
) (SyncStats, error) {
	var stats SyncStats

	fmt.Printf(
		"Resolving agent directories on %s...\n", rs.Host,
	)
	dirs, extraFiles, err := resolveDirs(
		ctx, rs.Host, rs.User, rs.Port, rs.SSHOpts,
	)
	if err != nil {
		return stats, fmt.Errorf(
			"resolve dirs on %s: %w", rs.Host, err,
		)
	}
	if len(dirs) == 0 {
		fmt.Printf("No agent directories found on %s\n", rs.Host)
		return stats, nil
	}

	fmt.Printf(
		"Downloading session data from %s (%d agents)...\n",
		rs.Host, len(dirs),
	)
	tmpDir, err := downloadAndExtract(
		ctx, rs.Host, rs.User, rs.Port, rs.SSHOpts, dirs, extraFiles,
	)
	if err != nil {
		return stats, fmt.Errorf(
			"download from %s: %w", rs.Host, err,
		)
	}
	defer os.RemoveAll(tmpDir)
	fmt.Printf("Download complete.\n")

	// Build engine AgentDirs pointing at temp dir equivalents
	// and track remote<->temp dir mappings for path
	// translation.
	engineDirs := make(map[parser.AgentType][]string)
	remoteDirs := make([]string, 0)
	tempDirs := make([]string, 0)
	for agentType, agentDirList := range dirs {
		for _, remoteDir := range agentDirList {
			local := remappedDir(tmpDir, remoteDir)
			engineDirs[agentType] = append(
				engineDirs[agentType], local,
			)
			remoteDirs = append(remoteDirs, remoteDir)
			tempDirs = append(tempDirs, local)
		}
	}

	// Path rewriter: temp path -> "host:/remote/path"
	rewriter := func(tempPath string) string {
		remotePath := remapToRemotePath(
			tmpDir, "", tempPath,
		)
		return rs.Host + ":" + remotePath
	}

	engine := sync.NewEngine(rs.DB, sync.EngineConfig{
		AgentDirs:               engineDirs,
		Machine:                 rs.Host,
		IDPrefix:                rs.Host + "~",
		PathRewriter:            rewriter,
		Ephemeral:               true,
		BlockedResultCategories: rs.BlockedResultCategories,
	})

	// Load remote skip cache and translate paths from
	// remote form to temp-dir form so the engine's skip
	// logic can match them.
	if !rs.Full {
		remoteCache, loadErr := rs.DB.LoadRemoteSkippedFiles(
			rs.Host,
		)
		if loadErr != nil {
			return stats, fmt.Errorf(
				"load skip cache: %w", loadErr,
			)
		}
		remoteCache = rs.migrateVisualStudioCopilotRemoteSkips(
			remoteCache,
		)
		translated := make(
			map[string]int64, len(remoteCache),
		)
		for remotePath, mtime := range remoteCache {
			for i, rd := range remoteDirs {
				if after, ok := strings.CutPrefix(remotePath, rd); ok {
					rel := filepath.FromSlash(after)
					translated[tempDirs[i]+rel] = mtime
					break
				}
			}
		}
		engine.InjectSkipCache(translated)
	}

	t0 := time.Now()
	lastPrint := t0
	var lastProgress sync.Progress
	progress := func(p sync.Progress) {
		lastProgress = p
		now := time.Now()
		if now.Sub(lastPrint) < 500*time.Millisecond {
			return
		}
		lastPrint = now
		elapsed := now.Sub(t0).Truncate(time.Millisecond)
		fmt.Printf(
			"\r  %d/%d sessions (%s)...",
			p.SessionsDone, p.SessionsTotal, elapsed,
		)
	}
	fmt.Printf("Processing sessions...\n")
	engineStats := engine.SyncAll(ctx, progress)
	if lastProgress.SessionsTotal > 0 {
		elapsed := time.Since(t0).Truncate(time.Millisecond)
		fmt.Printf(
			"\r  %d/%d sessions (%s)   \n",
			lastProgress.SessionsDone,
			lastProgress.SessionsTotal, elapsed,
		)
	}

	// Snapshot skip cache and translate temp paths back to
	// remote paths for persistence.
	snapshot := engine.SnapshotSkipCache()
	remoteCache := make(map[string]int64, len(snapshot))
	for tempPath, mtime := range snapshot {
		for i, td := range tempDirs {
			if after, ok := strings.CutPrefix(tempPath, td); ok {
				rel := filepath.ToSlash(after)
				remoteCache[remoteDirs[i]+rel] = mtime
				break
			}
		}
	}
	if err := rs.DB.ReplaceRemoteSkippedFiles(
		rs.Host, remoteCache,
	); err != nil {
		return stats, fmt.Errorf(
			"save skip cache: %w", err,
		)
	}

	stats.SessionsSynced = engineStats.Synced
	stats.SessionsTotal = engineStats.TotalSessions
	stats.Skipped = engineStats.Skipped
	stats.Failed = engineStats.Failed

	fmt.Printf(
		"Synced %d sessions from %s",
		stats.SessionsSynced, rs.Host,
	)
	if stats.Skipped > 0 {
		fmt.Printf(" (%d unchanged)", stats.Skipped)
	}
	if stats.Failed > 0 {
		fmt.Printf(" (%d failed)", stats.Failed)
	}
	fmt.Println()
	return stats, nil
}
