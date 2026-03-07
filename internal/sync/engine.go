package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	gosync "sync"
	"time"

	"github.com/wesm/agentsview/internal/db"
	"github.com/wesm/agentsview/internal/parser"
	"github.com/wesm/agentsview/internal/timeutil"
)

const (
	batchSize  = 100
	maxWorkers = 8
)

// EngineConfig holds the configuration needed by the sync
// engine, replacing per-agent positional parameters.
type EngineConfig struct {
	AgentDirs               map[parser.AgentType][]string
	Machine                 string
	BlockedResultCategories []string
}

// Engine orchestrates session file discovery and sync.
type Engine struct {
	db                      *db.DB
	agentDirs               map[parser.AgentType][]string
	machine                 string
	blockedResultCategories map[string]bool
	syncMu                  gosync.Mutex // serializes all sync operations
	mu                      gosync.RWMutex
	lastSync                time.Time
	lastSyncStats           SyncStats
	// skipCache tracks paths that should be skipped on
	// subsequent syncs, keyed by path with the file mtime
	// at time of caching. Covers parse errors and
	// non-interactive sessions (nil result). The file is
	// retried when its mtime changes.
	skipMu    gosync.RWMutex
	skipCache map[string]int64
}

// NewEngine creates a sync engine. It pre-populates the
// in-memory skip cache from the database so that files
// skipped in a prior run are not re-parsed on startup.
func NewEngine(
	database *db.DB, cfg EngineConfig,
) *Engine {
	skipCache := make(map[string]int64)
	if loaded, err := database.LoadSkippedFiles(); err == nil {
		skipCache = loaded
	} else {
		log.Printf("loading skip cache: %v", err)
	}

	dirs := make(map[parser.AgentType][]string, len(cfg.AgentDirs))
	for k, v := range cfg.AgentDirs {
		dirs[k] = append([]string(nil), v...)
	}

	return &Engine{
		db:                      database,
		agentDirs:               dirs,
		machine:                 cfg.Machine,
		blockedResultCategories: blockedCategorySet(cfg.BlockedResultCategories),
		skipCache:               skipCache,
	}
}

// blockedCategorySet converts a slice of category names into a
// set for O(1) lookup. Returns nil when the slice is empty.
// Entries are trimmed and title-cased to match parser categories.
func blockedCategorySet(cats []string) map[string]bool {
	if len(cats) == 0 {
		return nil
	}
	m := make(map[string]bool, len(cats))
	for _, c := range cats {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		c = strings.ToUpper(c[:1]) + strings.ToLower(c[1:])
		m[c] = true
	}
	return m
}

// LastSync returns the time of the last completed sync.
func (e *Engine) LastSync() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastSync
}

// LastSyncStats returns statistics from the last sync.
func (e *Engine) LastSyncStats() SyncStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.lastSyncStats
}

type syncJob struct {
	processResult
	path string
}

// SyncPaths syncs only the specified changed file paths
// instead of discovering and hashing all session files.
// Paths that don't match known session file patterns are
// silently ignored.
func (e *Engine) SyncPaths(paths []string) {
	files := e.classifyPaths(paths)
	if len(files) == 0 {
		return
	}

	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	results := e.startWorkers(files)
	stats := e.collectAndBatch(
		results, len(files), nil,
	)
	e.persistSkipCache()

	e.mu.Lock()
	e.lastSync = time.Now()
	e.lastSyncStats = stats
	e.mu.Unlock()

	if stats.Synced > 0 {
		log.Printf(
			"sync: %d file(s) updated", stats.Synced,
		)
	}
}

// classifyPaths maps changed file system paths to
// parser.DiscoveredFile structs, filtering out paths that don't
// match known session file patterns.
func (e *Engine) classifyPaths(
	paths []string,
) []parser.DiscoveredFile {
	geminiProjectsByDir := make(map[string]map[string]string)
	var files []parser.DiscoveredFile
	for _, p := range paths {
		if df, ok := e.classifyOnePath(
			p, geminiProjectsByDir,
		); ok {
			files = append(files, df)
		}
	}
	return files
}

// isUnder checks whether path is strictly inside dir after
// cleaning both paths. Returns the relative path on success.
func isUnder(dir, path string) (string, bool) {
	dir = filepath.Clean(dir)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", false
	}
	sep := string(filepath.Separator)
	if rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+sep) {
		return "", false
	}
	return rel, true
}

// findContainingDir returns the first dir from dirs that is a
// parent of path, or "" if none match.
func findContainingDir(dirs []string, path string) string {
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if _, ok := isUnder(d, path); ok {
			return d
		}
	}
	return ""
}

func (e *Engine) classifyOnePath(
	path string,
	geminiProjectsByDir map[string]map[string]string,
) (parser.DiscoveredFile, bool) {
	sep := string(filepath.Separator)

	// Claude: <claudeDir>/<project>/<session>.jsonl
	//     or: <claudeDir>/<project>/<session>/subagents/agent-<id>.jsonl
	for _, claudeDir := range e.agentDirs[parser.AgentClaude] {
		if claudeDir == "" {
			continue
		}
		if rel, ok := isUnder(claudeDir, path); ok {
			if !strings.HasSuffix(path, ".jsonl") {
				continue
			}
			parts := strings.Split(rel, sep)

			// Standard session: project/session.jsonl
			if len(parts) == 2 {
				stem := strings.TrimSuffix(
					filepath.Base(path), ".jsonl",
				)
				if strings.HasPrefix(stem, "agent-") {
					continue
				}
				return parser.DiscoveredFile{
					Path:    path,
					Project: parts[0],
					Agent:   parser.AgentClaude,
				}, true
			}

			// Subagent: project/session/subagents/agent-*.jsonl
			if len(parts) == 4 && parts[2] == "subagents" {
				stem := strings.TrimSuffix(
					parts[3], ".jsonl",
				)
				if !strings.HasPrefix(stem, "agent-") {
					continue
				}
				return parser.DiscoveredFile{
					Path:    path,
					Project: parts[0],
					Agent:   parser.AgentClaude,
				}, true
			}
		}
	}

	// Codex: <codexDir>/<year>/<month>/<day>/<file>.jsonl
	for _, codexDir := range e.agentDirs[parser.AgentCodex] {
		if codexDir == "" {
			continue
		}
		if rel, ok := isUnder(codexDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 4 {
				continue
			}
			if !parser.IsDigits(parts[0]) ||
				!parser.IsDigits(parts[1]) ||
				!parser.IsDigits(parts[2]) {
				continue
			}
			if !strings.HasSuffix(parts[3], ".jsonl") {
				continue
			}
			return parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentCodex,
			}, true
		}
	}

	// Copilot: <copilotDir>/session-state/<uuid>.jsonl
	//      or: <copilotDir>/session-state/<uuid>/events.jsonl
	for _, copilotDir := range e.agentDirs[parser.AgentCopilot] {
		if copilotDir == "" {
			continue
		}
		stateDir := filepath.Join(
			copilotDir, "session-state",
		)
		if rel, ok := isUnder(stateDir, path); ok {
			parts := strings.Split(rel, sep)
			switch len(parts) {
			case 1:
				stem, ok := strings.CutSuffix(
					parts[0], ".jsonl",
				)
				if !ok {
					continue
				}
				dirEvents := filepath.Join(
					stateDir, stem, "events.jsonl",
				)
				if _, err := os.Stat(dirEvents); err == nil {
					continue
				}
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentCopilot,
				}, true
			case 2:
				if parts[1] != "events.jsonl" {
					continue
				}
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentCopilot,
				}, true
			default:
				continue
			}
		}
	}

	// Gemini: <geminiDir>/tmp/<dir>/chats/session-*.json
	// <dir> is either a SHA-256 hash (old) or project name (new).
	for _, geminiDir := range e.agentDirs[parser.AgentGemini] {
		if geminiDir == "" {
			continue
		}
		if rel, ok := isUnder(geminiDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 4 ||
				parts[0] != "tmp" ||
				parts[2] != "chats" {
				continue
			}
			name := parts[3]
			if !strings.HasPrefix(name, "session-") ||
				!strings.HasSuffix(name, ".json") {
				continue
			}
			dirName := parts[1]
			if _, ok := geminiProjectsByDir[geminiDir]; !ok {
				geminiProjectsByDir[geminiDir] =
					parser.BuildGeminiProjectMap(geminiDir)
			}
			project := parser.ResolveGeminiProject(
				dirName, geminiProjectsByDir[geminiDir],
			)
			return parser.DiscoveredFile{
				Path:    path,
				Project: project,
				Agent:   parser.AgentGemini,
			}, true
		}
	}

	// Cursor: <cursorDir>/<project>/agent-transcripts/<uuid>.{txt,jsonl}
	for _, cursorDir := range e.agentDirs[parser.AgentCursor] {
		if cursorDir == "" {
			continue
		}
		if rel, ok := isUnder(cursorDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 3 {
				continue
			}
			if parts[1] != "agent-transcripts" {
				continue
			}
			if !parser.IsCursorTranscriptExt(parts[2]) {
				continue
			}
			project := parser.DecodeCursorProjectDir(parts[0])
			if project == "" {
				project = "unknown"
			}
			return parser.DiscoveredFile{
				Path:    path,
				Project: project,
				Agent:   parser.AgentCursor,
			}, true
		}
	}

	// Amp: <ampDir>/T-*.json
	for _, ampDir := range e.agentDirs[parser.AgentAmp] {
		if ampDir == "" {
			continue
		}
		if rel, ok := isUnder(ampDir, path); ok {
			if strings.Count(rel, sep) == 0 &&
				parser.IsAmpThreadFileName(filepath.Base(rel)) {
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentAmp,
				}, true
			}
		}
	}

	// VSCode Copilot: <vscodeUserDir>/workspaceStorage/<hash>/chatSessions/<uuid>.{json,jsonl}
	//            or: <vscodeUserDir>/globalStorage/emptyWindowChatSessions/<uuid>.{json,jsonl}
	for _, vscDir := range e.agentDirs[parser.AgentVSCodeCopilot] {
		if vscDir == "" {
			continue
		}
		if rel, ok := isUnder(vscDir, path); ok {
			parts := strings.Split(rel, sep)
			// workspaceStorage/<hash>/chatSessions/<uuid>.{json,jsonl}
			if len(parts) == 4 &&
				parts[0] == "workspaceStorage" &&
				parts[2] == "chatSessions" &&
				(strings.HasSuffix(parts[3], ".json") ||
					strings.HasSuffix(parts[3], ".jsonl")) {
				if vscodeJSONLSiblingExists(path) {
					continue
				}
				hashDir := filepath.Join(
					vscDir, "workspaceStorage", parts[1],
				)
				project := parser.ReadVSCodeWorkspaceManifest(hashDir)
				if project == "" {
					project = "unknown"
				}
				return parser.DiscoveredFile{
					Path:    path,
					Project: project,
					Agent:   parser.AgentVSCodeCopilot,
				}, true
			}
			// globalStorage/emptyWindowChatSessions/<uuid>.{json,jsonl}
			// globalStorage/transferredChatSessions/<uuid>.{json,jsonl}
			if len(parts) == 3 &&
				parts[0] == "globalStorage" &&
				(parts[1] == "emptyWindowChatSessions" || parts[1] == "transferredChatSessions") &&
				(strings.HasSuffix(parts[2], ".json") ||
					strings.HasSuffix(parts[2], ".jsonl")) {
				if vscodeJSONLSiblingExists(path) {
					continue
				}
				return parser.DiscoveredFile{
					Path:    path,
					Project: "empty-window",
					Agent:   parser.AgentVSCodeCopilot,
				}, true
			}
		}
	}

	// OpenClaw: <openclawDir>/<agentId>/sessions/<sessionId>.jsonl
	//       or: <openclawDir>/<agentId>/sessions/<sessionId>.jsonl.<archiveSuffix>
	for _, ocDir := range e.agentDirs[parser.AgentOpenClaw] {
		if ocDir == "" {
			continue
		}
		if rel, ok := isUnder(ocDir, path); ok {
			parts := strings.Split(rel, sep)
			// Expect: <agentId>/sessions/<file>
			if len(parts) != 3 || parts[1] != "sessions" {
				continue
			}
			if !parser.IsValidSessionID(parts[0]) {
				continue
			}
			if !parser.IsOpenClawSessionFile(parts[2]) {
				continue
			}
			if !strings.HasSuffix(parts[2], ".jsonl") {
				sid := parser.OpenClawSessionID(parts[2])
				active := filepath.Join(
					ocDir, parts[0], "sessions",
					sid+".jsonl",
				)
				if _, err := os.Stat(active); err == nil {
					continue
				}
				best := parser.FindOpenClawSourceFile(
					ocDir, parts[0]+":"+sid,
				)
				if best != path {
					continue
				}
			}
			return parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentOpenClaw,
			}, true
		}
	}

	return parser.DiscoveredFile{}, false
}

// vscodeJSONLSiblingExists returns true when path is a .json
// file and a .jsonl sibling exists for the same UUID. This
// mirrors the dedup logic in DiscoverVSCodeCopilotSessions.
func vscodeJSONLSiblingExists(path string) bool {
	base, ok := strings.CutSuffix(path, ".json")
	if !ok {
		return false
	}
	_, err := os.Stat(base + ".jsonl")
	return err == nil
}

// resyncTempSuffix is appended to the original DB path to
// form the temp database path during resync.
const resyncTempSuffix = "-resync"

// ResyncAll builds a fresh database from scratch, syncs all
// sessions into it, copies insights from the old DB, then
// atomically swaps the files and reopens the original DB
// handle. This avoids the per-row trigger overhead of bulk
// deleting hundreds of thousands of messages in place.
func (e *Engine) ResyncAll(
	onProgress ProgressFunc,
) SyncStats {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	origDB := e.db
	origPath := origDB.Path()
	tempPath := origPath + resyncTempSuffix

	// Snapshot old file-backed session count to detect
	// empty-discovery. Uses file-backed count (excludes
	// OpenCode) so OpenCode-only datasets don't trigger the
	// guard. Fail closed: if we can't query, assume old DB
	// has file-backed data worth protecting.
	ctx := context.Background()
	oldFileSessions, err := origDB.FileBackedSessionCount(ctx)
	if err != nil {
		log.Printf("resync: get old file count: %v", err)
		oldFileSessions = 1
	}

	// Clean up stale temp DB from a prior crash.
	removeTempDB(tempPath)

	// 1. Snapshot and clear in-memory skip cache. The
	// snapshot is restored on early failure so behavior
	// matches the persisted DB until the next restart.
	e.skipMu.Lock()
	savedSkipCache := e.skipCache
	e.skipCache = make(map[string]int64)
	e.skipMu.Unlock()

	restoreSkipCache := func() {
		e.skipMu.Lock()
		e.skipCache = savedSkipCache
		e.skipMu.Unlock()
	}

	// 2. Open a fresh DB at the temp path.
	newDB, err := db.Open(tempPath)
	if err != nil {
		log.Printf("resync: open temp db: %v", err)
		restoreSkipCache()
		stats := SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: " + err.Error(),
			},
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats
	}

	// 2b. Copy excluded session IDs from the old DB so that
	// UpsertSession skips permanently deleted sessions during
	// the sync. This must happen before syncAllLocked.
	if err := newDB.CopyExcludedSessionsFrom(origPath); err != nil {
		log.Printf("resync: pre-sync copy excluded sessions: %v", err)
		// Non-fatal: worst case, deleted sessions reappear.
	}

	// 3. Point engine at newDB and sync into it.
	e.db = newDB
	stats := e.syncAllLocked(onProgress)
	e.db = origDB // restore immediately

	// Abort swap when the fresh DB would be worse than the
	// original:
	// - nothing synced at all (empty discovery, or all skipped)
	//   when old DB had data
	// - more files failed than succeeded (permission errors,
	//   disk issues)
	// A few permanent parse failures are tolerated since those
	// files were broken in the old DB too.
	emptyDiscovery := stats.filesDiscovered == 0 &&
		stats.filesOK == 0 &&
		oldFileSessions > 0
	abortSwap := emptyDiscovery ||
		(stats.Synced == 0 && stats.TotalSessions > 0) ||
		(stats.Failed > 0 && stats.Failed > stats.filesOK)
	if abortSwap {
		log.Printf(
			"resync: aborting swap, %d synced / %d failed / %d total",
			stats.Synced, stats.Failed, stats.TotalSessions,
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings, fmt.Sprintf(
			"resync aborted: %d synced, %d failed",
			stats.Synced, stats.Failed,
		))

		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats
	}

	// 4. Close origDB connections first to quiesce writes,
	// then copy insights into newDB (which is still open).
	// This ensures no insight writes land in the old DB
	// after the copy.
	if err := origDB.CloseConnections(); err != nil {
		log.Printf("resync: close orig db: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"close before swap failed: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		// Connections may be partially closed; reopen to
		// restore service before returning.
		if rerr := origDB.Reopen(); rerr != nil {
			log.Printf("resync: recovery reopen: %v", rerr)
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats
	}

	// Re-copy excluded session IDs now that origDB is quiesced.
	// This catches any permanent deletes that occurred during
	// the sync window (between the pre-sync copy and now).
	// Also purge any sessions that were synced into newDB
	// before the exclusion was recorded.
	if err := newDB.CopyExcludedSessionsFrom(origPath); err != nil {
		log.Printf("resync: post-sync copy excluded sessions: %v", err)
	}
	if err := newDB.PurgeExcludedSessions(); err != nil {
		log.Printf("resync: purge excluded sessions: %v", err)
	}

	// Copy insights into newDB from the quiesced old DB file.
	tInsights := time.Now()
	if err := newDB.CopyInsightsFrom(origPath); err != nil {
		log.Printf("resync: copy insights: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"insights copy failed, aborting swap: "+
				err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		if rerr := origDB.Reopen(); rerr != nil {
			log.Printf("resync: recovery reopen: %v", rerr)
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats
	}
	log.Printf(
		"resync: copy insights: %s",
		time.Since(tInsights).Round(time.Millisecond),
	)

	// Copy orphaned sessions (source files gone) from the
	// old DB so archived data is preserved. Failure aborts
	// the swap to avoid losing archived sessions.
	orphaned, err := newDB.CopyOrphanedDataFrom(origPath)
	if err != nil {
		log.Printf("resync: copy orphaned sessions: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"orphaned session copy failed, aborting swap: "+
				err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		if rerr := origDB.Reopen(); rerr != nil {
			log.Printf("resync: recovery reopen: %v", rerr)
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats
	}
	stats.OrphanedCopied = orphaned

	// Merge user-managed data (display_name, deleted_at,
	// starred_sessions, pinned_messages) from the old DB
	// so renames, soft-deletes, stars, and pins survive.
	if err := newDB.CopySessionMetadataFrom(origPath); err != nil {
		log.Printf("resync: copy session metadata: %v", err)
		// Non-fatal: worst case, renames/soft-deletes are lost.
	}

	// 5. Close newDB and swap files, then reopen origDB.
	newDB.Close()

	removeWAL(origPath)

	if err := os.Rename(tempPath, origPath); err != nil {
		log.Printf("resync: rename temp db: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"resync swap failed: "+err.Error(),
		)
		removeTempDB(tempPath)
		restoreSkipCache()
		// Restore service even on rename failure.
		if rerr := origDB.Reopen(); rerr != nil {
			log.Printf("resync: recovery reopen: %v", rerr)
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats
	}
	removeWAL(tempPath)

	if err := origDB.Reopen(); err != nil {
		log.Printf("resync: reopen db: %v", err)
		stats.Warnings = append(stats.Warnings,
			"reopen after resync failed: "+err.Error(),
		)
	}

	// 6. Persist skip cache into the new DB.
	e.persistSkipCache()

	e.mu.Lock()
	e.lastSyncStats = stats
	e.mu.Unlock()

	return stats
}

// removeTempDB removes a temp database and its WAL/SHM files.
func removeTempDB(path string) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		os.Remove(path + suffix)
	}
}

// removeWAL removes WAL and SHM files for a database path.
func removeWAL(path string) {
	os.Remove(path + "-wal")
	os.Remove(path + "-shm")
}

// SyncAll discovers and syncs all session files from all agents.
func (e *Engine) SyncAll(onProgress ProgressFunc) SyncStats {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	return e.syncAllLocked(onProgress)
}

func (e *Engine) syncAllLocked(
	onProgress ProgressFunc,
) SyncStats {
	t0 := time.Now()

	var all []parser.DiscoveredFile
	counts := make(map[parser.AgentType]int)
	for _, def := range parser.Registry {
		if !def.FileBased || def.DiscoverFunc == nil {
			continue
		}
		for _, d := range e.agentDirs[def.Type] {
			found := def.DiscoverFunc(d)
			counts[def.Type] += len(found)
			all = append(all, found...)
		}
	}

	verbose := onProgress == nil

	if verbose {
		log.Printf(
			"discovered %d files (%d claude, %d codex, %d copilot, %d gemini, %d cursor, %d amp, %d vscode-copilot) in %s",
			len(all),
			counts[parser.AgentClaude],
			counts[parser.AgentCodex],
			counts[parser.AgentCopilot],
			counts[parser.AgentGemini],
			counts[parser.AgentCursor],
			counts[parser.AgentAmp],
			counts[parser.AgentVSCodeCopilot],
			time.Since(t0).Round(time.Millisecond),
		)
	}

	if onProgress != nil {
		onProgress(Progress{
			Phase:         PhaseSyncing,
			SessionsTotal: len(all),
		})
	}

	tWorkers := time.Now()
	results := e.startWorkers(all)
	stats := e.collectAndBatch(
		results, len(all), onProgress,
	)
	if verbose {
		log.Printf(
			"file sync: %d synced, %d skipped in %s",
			stats.Synced, stats.Skipped,
			time.Since(tWorkers).Round(time.Millisecond),
		)
	}

	// Sync OpenCode sessions (DB-backed, not file-based).
	// Uses full replace because OpenCode messages can change
	// in place (streaming updates, tool result pairing).
	tOC := time.Now()
	ocPending := e.syncOpenCode()
	if len(ocPending) > 0 {
		stats.TotalSessions += len(ocPending)
		stats.RecordSynced(len(ocPending))
		tWrite := time.Now()
		for _, pw := range ocPending {
			e.writeSessionFull(pw)
		}
		if verbose {
			log.Printf(
				"opencode write: %d sessions in %s",
				len(ocPending),
				time.Since(tWrite).Round(time.Millisecond),
			)
		}
	}
	if verbose {
		log.Printf(
			"opencode sync: %s",
			time.Since(tOC).Round(time.Millisecond),
		)
	}

	tPersist := time.Now()
	skipCount := e.persistSkipCache()
	if verbose {
		log.Printf(
			"persist skip cache (%d entries): %s",
			skipCount,
			time.Since(tPersist).Round(time.Millisecond),
		)
	}

	e.mu.Lock()
	e.lastSync = time.Now()
	e.lastSyncStats = stats
	e.mu.Unlock()
	return stats
}

// syncOpenCode syncs sessions from OpenCode SQLite databases.
// Uses per-session time_updated to detect changes, so only
// modified sessions are fully parsed. Returns pending writes.
func (e *Engine) syncOpenCode() []pendingWrite {
	var allPending []pendingWrite
	for _, dir := range e.agentDirs[parser.AgentOpenCode] {
		if dir == "" {
			continue
		}
		allPending = append(allPending, e.syncOneOpenCode(dir)...)
	}
	return allPending
}

// syncOneOpenCode handles a single OpenCode directory.
func (e *Engine) syncOneOpenCode(dir string) []pendingWrite {
	dbPath := filepath.Join(dir, "opencode.db")

	metas, err := parser.ListOpenCodeSessionMeta(dbPath)
	if err != nil {
		log.Printf("sync opencode: %v", err)
		return nil
	}
	if len(metas) == 0 {
		return nil
	}

	var changed []string
	for _, m := range metas {
		_, storedMtime, ok :=
			e.db.GetFileInfoByPath(m.VirtualPath)
		if ok && storedMtime == m.FileMtime {
			continue
		}
		changed = append(changed, m.SessionID)
	}
	if len(changed) == 0 {
		return nil
	}

	var pending []pendingWrite
	for _, sid := range changed {
		sess, msgs, err := parser.ParseOpenCodeSession(
			dbPath, sid, e.machine,
		)
		if err != nil {
			log.Printf(
				"opencode session %s: %v", sid, err,
			)
			continue
		}
		if sess == nil {
			continue
		}
		pending = append(pending, pendingWrite{
			sess: *sess,
			msgs: msgs,
		})
	}

	return pending
}

// startWorkers fans out file processing across a worker pool
// and returns a channel of results.
func (e *Engine) startWorkers(
	files []parser.DiscoveredFile,
) <-chan syncJob {
	workers := min(max(runtime.NumCPU(), 2), maxWorkers)

	jobs := make(chan parser.DiscoveredFile, len(files))
	results := make(chan syncJob, len(files))

	for range workers {
		go func() {
			for file := range jobs {
				results <- syncJob{
					processResult: e.processFile(file),
					path:          file.Path,
				}
			}
		}()
	}

	for _, f := range files {
		jobs <- f
	}
	close(jobs)
	return results
}

// collectAndBatch drains the results channel, batches
// successful parses, and writes them to the database.
func (e *Engine) collectAndBatch(
	results <-chan syncJob, total int,
	onProgress ProgressFunc,
) SyncStats {
	var stats SyncStats
	stats.TotalSessions = total
	stats.filesDiscovered = total

	progress := Progress{
		Phase:         PhaseSyncing,
		SessionsTotal: total,
	}

	var pending []pendingWrite

	for range total {
		r := <-results

		if r.err != nil {
			stats.RecordFailed()
			if r.mtime != 0 {
				e.cacheSkip(r.path, r.mtime)
			}
			log.Printf("sync error: %v", r.err)
			continue
		}
		if r.skip {
			stats.RecordSkip()
			progress.SessionsDone++
			if onProgress != nil {
				onProgress(progress)
			}
			continue
		}
		if len(r.results) == 0 {
			e.cacheSkip(r.path, r.mtime)
			progress.SessionsDone++
			if onProgress != nil {
				onProgress(progress)
			}
			continue
		}
		e.clearSkip(r.path)
		stats.filesOK++

		for _, pr := range r.results {
			pending = append(pending, pendingWrite{
				sess: pr.Session,
				msgs: pr.Messages,
			})
		}

		if len(pending) >= batchSize {
			stats.RecordSynced(len(pending))
			progress.MessagesIndexed += countMessages(pending)
			e.writeBatch(pending)
			pending = pending[:0]
		}

		progress.SessionsDone++
		if onProgress != nil {
			onProgress(progress)
		}
	}

	if len(pending) > 0 {
		stats.RecordSynced(len(pending))
		progress.MessagesIndexed += countMessages(pending)
		e.writeBatch(pending)
	}

	progress.Phase = PhaseDone
	if onProgress != nil {
		onProgress(progress)
	}
	return stats
}

type processResult struct {
	results []parser.ParseResult
	skip    bool
	mtime   int64
	err     error
}

func (e *Engine) processFile(
	file parser.DiscoveredFile,
) processResult {

	info, err := os.Stat(file.Path)
	if err != nil {
		return processResult{
			err: fmt.Errorf("stat %s: %w", file.Path, err),
		}
	}

	// Capture mtime once from the initial stat so all
	// downstream cache operations use a consistent value.
	mtime := info.ModTime().UnixNano()

	// Skip files cached from a previous sync (parse errors
	// or non-interactive sessions) whose mtime is unchanged.
	e.skipMu.RLock()
	cachedMtime, cached := e.skipCache[file.Path]
	e.skipMu.RUnlock()
	if cached && cachedMtime == mtime {
		return processResult{skip: true, mtime: mtime}
	}

	var res processResult
	switch file.Agent {
	case parser.AgentClaude:
		res = e.processClaude(file, info)
	case parser.AgentCodex:
		res = e.processCodex(file, info)
	case parser.AgentCopilot:
		res = e.processCopilot(file, info)
	case parser.AgentGemini:
		res = e.processGemini(file, info)
	case parser.AgentCursor:
		res = e.processCursor(file, info)
	case parser.AgentAmp:
		res = e.processAmp(file, info)
	case parser.AgentVSCodeCopilot:
		res = e.processVSCodeCopilot(file, info)
	case parser.AgentOpenClaw:
		res = e.processOpenClaw(file, info)
	default:
		res = processResult{
			err: fmt.Errorf(
				"unknown agent type: %s", file.Agent,
			),
		}
	}
	res.mtime = mtime
	return res
}

// cacheSkip records a file so it won't be retried until
// its mtime changes.
func (e *Engine) cacheSkip(path string, mtime int64) {
	e.skipMu.Lock()
	e.skipCache[path] = mtime
	e.skipMu.Unlock()
}

// clearSkip removes a skip-cache entry when a file
// produces a valid session.
func (e *Engine) clearSkip(path string) {
	e.skipMu.Lock()
	delete(e.skipCache, path)
	e.skipMu.Unlock()
	_ = e.db.DeleteSkippedFile(path)
}

// persistSkipCache writes the in-memory skip cache to the
// database so skipped files survive process restarts.
// Returns the number of entries persisted.
func (e *Engine) persistSkipCache() int {
	e.skipMu.RLock()
	snapshot := make(map[string]int64, len(e.skipCache))
	maps.Copy(snapshot, e.skipCache)
	e.skipMu.RUnlock()

	if err := e.db.ReplaceSkippedFiles(snapshot); err != nil {
		log.Printf("persisting skip cache: %v", err)
	}
	return len(snapshot)
}

// shouldSkipFile returns true when the file's size and mtime
// match what is already stored in the database (by session ID).
// This relies on mtime changing on any write, which holds for
// append-only session files under normal filesystem behavior.
// The file hash is still computed and stored on successful sync
// for integrity; mtime is purely a skip-check optimization.
func (e *Engine) shouldSkipFile(
	sessionID string, info os.FileInfo,
) bool {
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(
		sessionID,
	)
	if !ok {
		return false
	}
	return storedSize == info.Size() &&
		storedMtime == info.ModTime().UnixNano()
}

// shouldSkipByPath checks file size and mtime against what is
// stored in the database by file_path. Used for codex/gemini
// files where the session ID requires parsing.
func (e *Engine) shouldSkipByPath(
	path string, info os.FileInfo,
) bool {
	storedSize, storedMtime, ok := e.db.GetFileInfoByPath(
		path,
	)
	if !ok {
		return false
	}
	return storedSize == info.Size() &&
		storedMtime == info.ModTime().UnixNano()
}

func (e *Engine) processClaude(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {

	sessionID := strings.TrimSuffix(info.Name(), ".jsonl")

	if e.shouldSkipFile(sessionID, info) {
		sess, _ := e.db.GetSession(
			context.Background(), sessionID,
		)
		if sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			return processResult{skip: true}
		}
	}

	// Determine project name from cwd if possible
	project := parser.GetProjectName(file.Project)
	cwd, gitBranch := parser.ExtractClaudeProjectHints(
		file.Path,
	)
	if cwd != "" {
		if p := parser.ExtractProjectFromCwdWithBranch(
			cwd, gitBranch,
		); p != "" {
			project = p
		}
	}

	results, err := parser.ParseClaudeSession(
		file.Path, project, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		for i := range results {
			results[i].Session.File.Hash = hash
		}
	}

	parser.InferRelationshipTypes(results)

	return processResult{results: results}
}

func (e *Engine) processCodex(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {

	// Fast path: skip by file_path + mtime before parsing.
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseCodexSession(
		file.Path, e.machine, false,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{} // non-interactive
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func (e *Engine) processCopilot(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseCopilotSession(
		file.Path, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func (e *Engine) processGemini(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	// Fast path: skip by file_path + mtime before parsing.
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseGeminiSession(
		file.Path, file.Project, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func (e *Engine) processAmp(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	// Fast path: skip by file_path + mtime before parsing.
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseAmpSession(
		file.Path, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func (e *Engine) processVSCodeCopilot(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseVSCodeCopilotSession(
		file.Path, file.Project, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func (e *Engine) processOpenClaw(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseOpenClawSession(
		file.Path, file.Project, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func (e *Engine) processCursor(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	// Skip .txt if a sibling .jsonl exists — .jsonl is the
	// richer format and takes precedence.
	if stem, ok := strings.CutSuffix(file.Path, ".txt"); ok {
		if parser.IsRegularFile(stem + ".jsonl") {
			return processResult{skip: true}
		}
	}

	sessionID := parser.CursorSessionID(file.Path)

	if e.shouldSkipFile(sessionID, info) {
		return processResult{skip: true}
	}

	// Re-validate containment immediately before parsing to
	// close the TOCTOU window between discovery and read.
	// The parser opens with O_NOFOLLOW (rejecting symlinked
	// final components), and this check catches parent
	// directory swaps.
	if root := findContainingDir(
		e.agentDirs[parser.AgentCursor], file.Path,
	); root != "" {
		if err := validateCursorContainment(
			root, file.Path,
		); err != nil {
			return processResult{
				err: fmt.Errorf(
					"containment check: %w", err,
				),
			}
		}
	}

	sess, msgs, err := parser.ParseCursorSession(
		file.Path, file.Project, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	// Hash is computed inside ParseCursorSession from the
	// already-read data to avoid re-opening the file by path.
	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

// validateCursorContainment re-resolves both root and path
// to verify the file still resides within the cursor projects
// directory. Returns an error if containment fails.
func validateCursorContainment(
	cursorDir, path string,
) error {
	resolvedRoot, err := filepath.EvalSymlinks(cursorDir)
	if err != nil {
		return fmt.Errorf("resolve root: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	sep := string(filepath.Separator)
	if err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+sep) {
		return fmt.Errorf(
			"%s escapes %s", path, cursorDir,
		)
	}
	return nil
}

type pendingWrite struct {
	sess parser.ParsedSession
	msgs []parser.ParsedMessage
}

func (e *Engine) writeBatch(batch []pendingWrite) {
	for _, pw := range batch {
		msgs := toDBMessages(pw, e.blockedResultCategories)
		s := toDBSession(pw)
		s.MessageCount, s.UserMessageCount =
			postFilterCounts(msgs)
		if err := e.db.UpsertSession(s); err != nil {
			if errors.Is(err, db.ErrSessionExcluded) {
				// Cache as skipped so excluded files are not
				// re-parsed on subsequent syncs.
				if pw.sess.File.Path != "" {
					e.cacheSkip(pw.sess.File.Path, pw.sess.File.Mtime)
				}
				continue
			}
			log.Printf("upsert session %s: %v", s.ID, err)
			continue
		}
		e.writeMessages(pw.sess.ID, msgs)
	}
}

// writeMessages uses an incremental append when possible.
// Session files are append-only, so if the DB already has
// messages for this session and the new set is larger, we
// only insert the new messages (avoiding expensive FTS5
// delete+reinsert of existing content).
func (e *Engine) writeMessages(
	sessionID string, msgs []db.Message,
) {
	maxOrd := e.db.MaxOrdinal(sessionID)

	// No existing messages — insert all.
	if maxOrd < 0 {
		if err := e.db.InsertMessages(msgs); err != nil {
			log.Printf(
				"insert messages for %s: %v",
				sessionID, err,
			)
		}
		return
	}

	// Find new messages (ordinal > maxOrd).
	delta := 0
	for i, m := range msgs {
		if m.Ordinal > maxOrd {
			delta = len(msgs) - i
			msgs = msgs[i:]
			break
		}
	}

	if delta == 0 {
		return
	}

	if err := e.db.InsertMessages(msgs); err != nil {
		log.Printf(
			"append messages for %s: %v",
			sessionID, err,
		)
	}
}

// writeSessionFull upserts a session and does a full
// delete+reinsert of its messages. Used by explicit
// single-session re-syncs where existing content may have
// changed (not just appended).
func (e *Engine) writeSessionFull(pw pendingWrite) {
	msgs := toDBMessages(pw, e.blockedResultCategories)
	s := toDBSession(pw)
	s.MessageCount, s.UserMessageCount =
		postFilterCounts(msgs)
	if err := e.db.UpsertSession(s); err != nil {
		if errors.Is(err, db.ErrSessionExcluded) {
			if pw.sess.File.Path != "" {
				e.cacheSkip(pw.sess.File.Path, pw.sess.File.Mtime)
			}
		} else {
			log.Printf("upsert session %s: %v", s.ID, err)
		}
		return
	}
	if err := e.db.ReplaceSessionMessages(
		pw.sess.ID, msgs,
	); err != nil {
		log.Printf(
			"replace messages for %s: %v",
			pw.sess.ID, err,
		)
	}
}

// toDBSession converts a pendingWrite to a db.Session.
func toDBSession(pw pendingWrite) db.Session {
	s := db.Session{
		ID:               pw.sess.ID,
		Project:          pw.sess.Project,
		Machine:          pw.sess.Machine,
		Agent:            string(pw.sess.Agent),
		MessageCount:     pw.sess.MessageCount,
		UserMessageCount: pw.sess.UserMessageCount,
		ParentSessionID:  strPtr(pw.sess.ParentSessionID),
		RelationshipType: string(pw.sess.RelationshipType),
		FilePath:         strPtr(pw.sess.File.Path),
		FileSize:         int64Ptr(pw.sess.File.Size),
		FileMtime:        int64Ptr(pw.sess.File.Mtime),
		FileHash:         strPtr(pw.sess.File.Hash),
	}
	if pw.sess.FirstMessage != "" {
		s.FirstMessage = &pw.sess.FirstMessage
	}
	if !pw.sess.StartedAt.IsZero() {
		s.StartedAt = timeutil.Ptr(pw.sess.StartedAt)
	}
	if !pw.sess.EndedAt.IsZero() {
		s.EndedAt = timeutil.Ptr(pw.sess.EndedAt)
	}
	return s
}

// toDBMessages converts parsed messages to db.Message rows
// with tool-result pairing and filtering applied.
func toDBMessages(pw pendingWrite, blocked map[string]bool) []db.Message {
	msgs := make([]db.Message, len(pw.msgs))
	for i, m := range pw.msgs {
		msgs[i] = db.Message{
			SessionID:     pw.sess.ID,
			Ordinal:       m.Ordinal,
			Role:          string(m.Role),
			Content:       m.Content,
			Timestamp:     timeutil.Format(m.Timestamp),
			HasThinking:   m.HasThinking,
			HasToolUse:    m.HasToolUse,
			ContentLength: m.ContentLength,
			ToolCalls: convertToolCalls(
				pw.sess.ID, m.ToolCalls,
			),
			ToolResults: convertToolResults(m.ToolResults),
		}
	}
	return pairAndFilter(msgs, blocked)
}

// postFilterCounts returns the total and user message counts
// from a filtered message slice.
func postFilterCounts(msgs []db.Message) (total, user int) {
	for _, m := range msgs {
		if m.Role == "user" {
			user++
		}
	}
	return len(msgs), user
}

func countMessages(batch []pendingWrite) int {
	n := 0
	for _, pw := range batch {
		n += len(pw.msgs)
	}
	return n
}

// FindSourceFile locates the original source file for a
// session ID.
func (e *Engine) FindSourceFile(sessionID string) string {
	def, ok := parser.AgentByPrefix(sessionID)
	if !ok || !def.FileBased || def.FindSourceFunc == nil {
		return ""
	}
	rawID := strings.TrimPrefix(sessionID, def.IDPrefix)
	for _, d := range e.agentDirs[def.Type] {
		if f := def.FindSourceFunc(d, rawID); f != "" {
			return f
		}
	}
	return ""
}

// SyncSingleSession re-syncs a single session by its ID.
// Unlike the bulk SyncAll path, this includes exec-originated
// Codex sessions and uses the existing DB project as fallback.
func (e *Engine) SyncSingleSession(sessionID string) error {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok {
		return fmt.Errorf("unknown agent for session %s", sessionID)
	}
	if !def.FileBased {
		return e.syncSingleOpenCode(sessionID)
	}

	path := e.FindSourceFile(sessionID)
	if path == "" {
		return fmt.Errorf(
			"source file not found for %s", sessionID,
		)
	}

	agent := def.Type

	// Clear skip cache so explicit re-sync always processes
	// the file, even if it was cached as non-interactive
	// during a bulk SyncAll.
	e.clearSkip(path)

	// Reuse processFile for stat and DB-skip logic. For
	// Claude this is the full pipeline; for Codex we need
	// includeExec=true so we call the parser directly.
	file := parser.DiscoveredFile{
		Path:  path,
		Agent: agent,
	}
	switch agent {
	case parser.AgentClaude:
		// Try to preserve existing project from DB first
		if sess, _ := e.db.GetSession(context.Background(), sessionID); sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			file.Project = sess.Project
		} else {
			file.Project = filepath.Base(filepath.Dir(path))
		}
	case parser.AgentCursor:
		// path is <cursorDir>/<project>/agent-transcripts/<uuid>.txt
		// Extract project dir name from two levels up
		projDir := filepath.Base(
			filepath.Dir(filepath.Dir(path)),
		)
		file.Project = parser.DecodeCursorProjectDir(projDir)
	}

	res := e.processFile(file)
	if res.err != nil {
		if res.mtime != 0 {
			e.cacheSkip(path, res.mtime)
		}
		return res.err
	}
	if res.skip {
		return nil
	}

	// For Codex, processFile uses includeExec=false which may
	// return empty results for exec-originated sessions. Re-parse
	// with includeExec=true when that happens.
	if len(res.results) == 0 && agent == parser.AgentCodex {
		execRes := e.processCodexIncludeExec(file)
		if execRes.err != nil {
			if res.mtime != 0 {
				e.cacheSkip(path, res.mtime)
			}
			return execRes.err
		}
		if len(execRes.results) == 0 {
			return nil
		}
		res.results = execRes.results
	}

	if len(res.results) == 0 {
		return nil
	}

	for _, pr := range res.results {
		e.writeSessionFull(
			pendingWrite{sess: pr.Session, msgs: pr.Messages},
		)
	}
	return nil
}

// processCodexIncludeExec re-parses a Codex session with
// exec-originated sessions included.
func (e *Engine) processCodexIncludeExec(
	file parser.DiscoveredFile,
) processResult {
	sess, msgs, err := parser.ParseCodexSession(
		file.Path, e.machine, true,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}
	if h, herr := ComputeFileHash(file.Path); herr == nil {
		sess.File.Hash = h
	}
	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

// syncSingleOpenCode re-syncs a single OpenCode session.
func (e *Engine) syncSingleOpenCode(
	sessionID string,
) error {
	rawID := strings.TrimPrefix(sessionID, "opencode:")

	var lastErr error
	for _, dir := range e.agentDirs[parser.AgentOpenCode] {
		if dir == "" {
			continue
		}
		dbPath := filepath.Join(dir, "opencode.db")
		sess, msgs, err := parser.ParseOpenCodeSession(
			dbPath, rawID, e.machine,
		)
		if err != nil {
			lastErr = err
			continue
		}
		if sess == nil {
			continue
		}
		e.writeSessionFull(
			pendingWrite{sess: *sess, msgs: msgs},
		)
		return nil
	}

	if len(e.agentDirs[parser.AgentOpenCode]) == 0 {
		return fmt.Errorf("opencode dir not configured")
	}
	if lastErr != nil {
		return fmt.Errorf(
			"opencode session %s: %w", sessionID, lastErr,
		)
	}
	return fmt.Errorf("opencode session %s not found", sessionID)
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func int64Ptr(n int64) *int64 {
	if n == 0 {
		return nil
	}
	return &n
}

// convertToolCalls maps parsed tool calls to db.ToolCall
// structs. MessageID is resolved later during insert.
func convertToolCalls(
	sessionID string, parsed []parser.ParsedToolCall,
) []db.ToolCall {
	if len(parsed) == 0 {
		return nil
	}
	calls := make([]db.ToolCall, len(parsed))
	for i, tc := range parsed {
		calls[i] = db.ToolCall{
			SessionID:         sessionID,
			ToolName:          tc.ToolName,
			Category:          tc.Category,
			ToolUseID:         tc.ToolUseID,
			InputJSON:         tc.InputJSON,
			SkillName:         tc.SkillName,
			SubagentSessionID: tc.SubagentSessionID,
		}
	}
	return calls
}

// convertToolResults maps parsed tool results to db.ToolResult
// structs for use in pairing before DB insert.
func convertToolResults(
	parsed []parser.ParsedToolResult,
) []db.ToolResult {
	if len(parsed) == 0 {
		return nil
	}
	results := make([]db.ToolResult, len(parsed))
	for i, tr := range parsed {
		results[i] = db.ToolResult{
			ToolUseID:     tr.ToolUseID,
			ContentLength: tr.ContentLength,
			ContentRaw:    tr.ContentRaw,
		}
	}
	return results
}

// pairAndFilter pairs tool results with their corresponding
// tool calls, then removes user messages that carried only
// tool_result blocks (no displayable text).
func pairAndFilter(msgs []db.Message, blocked map[string]bool) []db.Message {
	pairToolResults(msgs, blocked)
	filtered := msgs[:0]
	for _, m := range msgs {
		if m.Role == "user" &&
			len(m.ToolResults) > 0 &&
			strings.TrimSpace(m.Content) == "" {
			continue
		}
		filtered = append(filtered, m)
	}
	return filtered
}

// pairToolResults matches tool_result content to their
// corresponding tool_calls across message boundaries using
// tool_use_id. Categories in blocked are stored without content.
func pairToolResults(msgs []db.Message, blocked map[string]bool) {
	idx := make(map[string]*db.ToolCall)
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			if tc.ToolUseID != "" {
				idx[tc.ToolUseID] = tc
			}
		}
	}
	if len(idx) == 0 {
		return
	}
	for _, m := range msgs {
		for _, tr := range m.ToolResults {
			if tc, ok := idx[tr.ToolUseID]; ok {
				tc.ResultContentLength = tr.ContentLength
				if !blocked[tc.Category] {
					tc.ResultContent = parser.DecodeContent(tr.ContentRaw)
				}
			}
		}
	}
}
