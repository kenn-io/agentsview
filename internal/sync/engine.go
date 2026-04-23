package sync

import (
	"context"
	"encoding/json"
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
	"github.com/wesm/agentsview/internal/signals"
	"github.com/wesm/agentsview/internal/timeutil"
)

const (
	batchSize  = 100
	maxWorkers = 8
)

var errSessionPreserved = errors.New("session preserved")

// Emitter is notified after a sync pass writes data. Implementations
// must be thread-safe; Emit is called from whatever goroutine runs
// the sync pass (e.g., the file watcher, a periodic timer, or a
// handler goroutine triggered by POST /api/v1/sync).
//
// Emit must not block. A slow implementation can delay the sync
// pipeline; see server.Broadcaster for the production implementation,
// which drops events on full per-subscriber buffers.
type Emitter interface {
	Emit(scope string)
}

// EngineConfig holds the configuration needed by the sync
// engine, replacing per-agent positional parameters.
type EngineConfig struct {
	AgentDirs               map[parser.AgentType][]string
	Machine                 string
	BlockedResultCategories []string
	// IDPrefix is prepended to all session IDs. Used by
	// remote sync to namespace IDs by host (e.g. "host~").
	IDPrefix string
	// PathRewriter transforms file paths before storage.
	// Used by remote sync to replace temp paths with
	// "host:/remote/path" references.
	PathRewriter func(string) string
	// Ephemeral disables sync-state persistence (timestamps
	// and skip cache) so remote sync does not interfere with
	// local sync watermarks or pollute the skipped_files table
	// with temp-dir paths.
	Ephemeral bool
	// Emitter, when non-nil, is called once after each sync pass
	// that wrote data. Safe to leave nil (e.g., in PG serve mode
	// where the engine is not run).
	Emitter Emitter
}

// Engine orchestrates session file discovery and sync.
type Engine struct {
	db                      *db.DB
	openCodeArchiveStore    db.Store
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
	// idPrefix and pathRewriter support remote sync:
	// prefix all session IDs to avoid collisions, rewrite
	// temp paths to "host:/remote/path" form.
	ephemeral    bool
	idPrefix     string
	pathRewriter func(string) string
	emitter      Emitter
}

// codexExecMigrationKey is the pg_sync_state flag that
// records whether the one-time cleanup of legacy codex_exec
// skip cache entries has already run on this database.
const codexExecMigrationKey = "codex_exec_legacy_migration_v1"

// NewEngine creates a sync engine. It pre-populates the
// in-memory skip cache from the database so that files
// skipped in a prior run are not re-parsed on startup, and
// migrates legacy codex_exec skip entries on first run under
// the new bulk-sync behavior.
func NewEngine(
	database *db.DB, cfg EngineConfig,
) *Engine {
	skipCache := make(map[string]int64)
	if !cfg.Ephemeral {
		if loaded, err := database.LoadSkippedFiles(); err == nil {
			skipCache = loaded
		} else {
			log.Printf("loading skip cache: %v", err)
		}
		migrateLegacyCodexExecSkips(database, skipCache)
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
		ephemeral:               cfg.Ephemeral,
		idPrefix:                cfg.IDPrefix,
		pathRewriter:            cfg.PathRewriter,
		emitter:                 cfg.Emitter,
	}
}

// migrateLegacyCodexExecSkips removes skip cache entries
// created by older agentsview builds that excluded Codex exec
// sessions from bulk sync. The scrub runs once per database:
// a `pg_sync_state` flag is set after the first successful
// pass so subsequent process starts do not re-scan files.
// New skip entries for real parse errors on exec files are
// untouched here and honored normally on later syncs.
//
// The cleanup builds a rebuilt snapshot and writes it through
// the atomic ReplaceSkippedFiles, then only mutates the
// in-memory map and records the done flag after the persist
// succeeds. A partial failure leaves both the DB and the
// in-memory cache in their prior state so the migration is
// retried on the next startup rather than being falsely
// marked complete.
func migrateLegacyCodexExecSkips(
	database *db.DB, skipCache map[string]int64,
) {
	done, err := database.GetSyncState(codexExecMigrationKey)
	if err != nil {
		log.Printf("codex exec migration: %v", err)
		return
	}
	if done != "" {
		return
	}

	cleaned := make(map[string]int64, len(skipCache))
	var legacy []string
	for path, mtime := range skipCache {
		if strings.HasSuffix(path, ".jsonl") &&
			parser.IsCodexExecSessionFile(path) {
			legacy = append(legacy, path)
			continue
		}
		cleaned[path] = mtime
	}

	if len(legacy) > 0 {
		if err := database.ReplaceSkippedFiles(
			cleaned,
		); err != nil {
			log.Printf(
				"codex exec migration: persist cleaned skip cache: %v",
				err,
			)
			return
		}
		for _, p := range legacy {
			delete(skipCache, p)
		}
		log.Printf(
			"codex exec legacy migration: cleared %d skip entries",
			len(legacy),
		)
	}

	if err := database.SetSyncState(
		codexExecMigrationKey, "done",
	); err != nil {
		log.Printf(
			"codex exec migration: set flag: %v", err,
		)
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
	// Defers run LIFO: the emit closure (declared first) runs AFTER
	// syncMu.Unlock, so an Emitter implementation cannot widen the
	// critical section or deadlock by re-entering sync code. The
	// stats variable is captured by the closure and populated below.
	var stats SyncStats
	defer func() {
		if stats.Synced > 0 {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()

	results := e.startWorkers(context.Background(), files)
	stats = e.collectAndBatch(
		context.Background(), results, len(files), nil,
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
	seen := make(map[string]struct{}, len(paths))
	var files []parser.DiscoveredFile
	for _, p := range paths {
		if df, ok := e.classifyOnePath(
			p, geminiProjectsByDir,
		); ok {
			key := string(df.Agent) + "\x00" + df.Path
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
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
	pathExists := true
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			pathExists = false
		}
	}

	if df, ok := e.classifyOpenCodePath(
		path, pathExists,
	); ok {
		return df, true
	}
	if !pathExists {
		return parser.DiscoveredFile{}, false
	}

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

	// OpenHands CLI:
	//   <openhandsDir>/<conversation-id>/base_state.json
	//   <openhandsDir>/<conversation-id>/TASKS.json
	//   <openhandsDir>/<conversation-id>/events/*.json
	for _, openHandsDir := range e.agentDirs[parser.AgentOpenHands] {
		if openHandsDir == "" {
			continue
		}
		if rel, ok := isUnder(openHandsDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) < 2 || !parser.IsValidSessionID(parts[0]) {
				continue
			}
			switch {
			case len(parts) == 2 &&
				(parts[1] == "base_state.json" ||
					parts[1] == "TASKS.json"):
			case len(parts) == 3 &&
				parts[1] == "events" &&
				strings.HasSuffix(parts[2], ".json"):
			default:
				continue
			}
			return parser.DiscoveredFile{
				Path: filepath.Join(
					openHandsDir, parts[0],
				),
				Agent: parser.AgentOpenHands,
			}, true
		}
	}

	// Cursor:
	//   <cursorDir>/<project>/agent-transcripts/<uuid>.{txt,jsonl}
	//   <cursorDir>/<project>/agent-transcripts/<uuid>/<uuid>.{txt,jsonl}
	for _, cursorDir := range e.agentDirs[parser.AgentCursor] {
		if cursorDir == "" {
			continue
		}
		if rel, ok := isUnder(cursorDir, path); ok {
			projectDir, ok := parser.ParseCursorTranscriptRelPath(rel)
			if !ok {
				continue
			}
			project := parser.DecodeCursorProjectDir(projectDir)
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

	// iFlow: <iflowDir>/<project>/session-<uuid>.jsonl
	for _, iflowDir := range e.agentDirs[parser.AgentIflow] {
		if iflowDir == "" {
			continue
		}
		if rel, ok := isUnder(iflowDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 2 {
				continue
			}
			if !strings.HasPrefix(parts[1], "session-") || !strings.HasSuffix(parts[1], ".jsonl") {
				continue
			}
			return parser.DiscoveredFile{
				Path:    path,
				Project: parts[0],
				Agent:   parser.AgentIflow,
			}, true
		}
	}

	// Kimi: <kimiDir>/<project-hash>/<session-uuid>/wire.jsonl
	for _, kimiDir := range e.agentDirs[parser.AgentKimi] {
		if kimiDir == "" {
			continue
		}
		if rel, ok := isUnder(kimiDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 3 || parts[2] != "wire.jsonl" {
				continue
			}
			return parser.DiscoveredFile{
				Path:    path,
				Project: parts[0],
				Agent:   parser.AgentKimi,
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

	// Zencoder: <zencoderDir>/<uuid>.jsonl
	for _, zenDir := range e.agentDirs[parser.AgentZencoder] {
		if zenDir == "" {
			continue
		}
		if rel, ok := isUnder(zenDir, path); ok {
			if strings.Count(rel, sep) == 0 &&
				parser.IsZencoderSessionFileName(filepath.Base(rel)) {
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentZencoder,
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

	// Pi: <piDir>/<encoded-cwd>/<session>.jsonl
	for _, piDir := range e.agentDirs[parser.AgentPi] {
		if piDir == "" {
			continue
		}
		if rel, ok := isUnder(piDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 2 {
				continue
			}
			if !strings.HasSuffix(parts[1], ".jsonl") {
				continue
			}
			if !parser.IsPiSessionFile(path) {
				continue
			}
			return parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentPi,
				// Project left empty; parser derives from header cwd.
			}, true
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

	// Cortex: <cortexDir>/<uuid>.json
	//     or: <cortexDir>/<uuid>.history.jsonl → remap to .json
	for _, cortexDir := range e.agentDirs[parser.AgentCortex] {
		if cortexDir == "" {
			continue
		}
		if rel, ok := isUnder(cortexDir, path); ok {
			if strings.Count(rel, sep) != 0 {
				continue
			}
			name := filepath.Base(rel)

			// .history.jsonl companion → remap to .json metadata.
			if stem, ok := strings.CutSuffix(
				name, ".history.jsonl",
			); ok {
				jsonPath := filepath.Join(
					cortexDir, stem+".json",
				)
				if parser.IsCortexSessionFile(stem + ".json") {
					return parser.DiscoveredFile{
						Path:  jsonPath,
						Agent: parser.AgentCortex,
					}, true
				}
				continue
			}

			if parser.IsCortexSessionFile(name) {
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentCortex,
				}, true
			}
		}
	}

	return parser.DiscoveredFile{}, false
}

func (e *Engine) classifyOpenCodePath(
	path string, pathExists bool,
) (parser.DiscoveredFile, bool) {
	sep := string(filepath.Separator)

	// OpenCode storage:
	//   <opencodeDir>/storage/session/<project>/<session>.json
	//   <opencodeDir>/storage/message/<session>/<message>.json
	//   <opencodeDir>/storage/part/<message>/<part>.json
	for _, openCodeDir := range e.agentDirs[parser.AgentOpenCode] {
		if openCodeDir == "" {
			continue
		}
		src := parser.ResolveOpenCodeSource(openCodeDir)
		if src.Mode != parser.OpenCodeSourceStorage {
			if src.Mode != parser.OpenCodeSourceSQLite {
				continue
			}
			rel, ok := isUnder(openCodeDir, path)
			if !ok {
				continue
			}
			base := filepath.Base(rel)
			if rel == "opencode.db" ||
				strings.HasPrefix(base, "opencode.db-") {
				return parser.DiscoveredFile{
					Path:  src.DBPath,
					Agent: parser.AgentOpenCode,
				}, true
			}
			continue
		}
		rel, ok := isUnder(openCodeDir, path)
		if !ok {
			continue
		}
		parts := strings.Split(rel, sep)
		switch {
		case pathExists &&
			len(parts) == 4 &&
			parts[0] == "storage" &&
			parts[1] == "session" &&
			strings.HasSuffix(parts[3], ".json"):
			return parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentOpenCode,
			}, true
		case len(parts) == 4 &&
			parts[0] == "storage" &&
			parts[1] == "message" &&
			strings.HasSuffix(parts[3], ".json"):
			sessionPath := parser.FindOpenCodeSourceFile(
				openCodeDir, parts[2],
			)
			if sessionPath == "" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sessionPath,
				Agent: parser.AgentOpenCode,
			}, true
		case len(parts) == 4 &&
			parts[0] == "storage" &&
			parts[1] == "part" &&
			strings.HasSuffix(parts[3], ".json"):
			sessionID := ""
			if pathExists {
				sessionID = readOpenCodeStorageSessionID(path)
			}
			if sessionID == "" {
				sessionID =
					findOpenCodeStorageSessionIDByMessageID(
						openCodeDir, parts[2],
					)
			}
			if sessionID == "" {
				continue
			}
			sessionPath := parser.FindOpenCodeSourceFile(
				openCodeDir, sessionID,
			)
			if sessionPath == "" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sessionPath,
				Agent: parser.AgentOpenCode,
			}, true
		case !pathExists &&
			len(parts) == 3 &&
			parts[0] == "storage" &&
			parts[1] == "message":
			sessionPath := parser.FindOpenCodeSourceFile(
				openCodeDir, parts[2],
			)
			if sessionPath == "" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sessionPath,
				Agent: parser.AgentOpenCode,
			}, true
		case !pathExists &&
			len(parts) == 3 &&
			parts[0] == "storage" &&
			parts[1] == "part":
			sessionID := findOpenCodeStorageSessionIDByMessageID(
				openCodeDir, parts[2],
			)
			if sessionID == "" {
				continue
			}
			sessionPath := parser.FindOpenCodeSourceFile(
				openCodeDir, sessionID,
			)
			if sessionPath == "" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sessionPath,
				Agent: parser.AgentOpenCode,
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
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	e.syncMu.Lock()
	// Defers LIFO: Unlock runs before emit.
	defer func() {
		if stats.Synced > 0 {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()

	origDB := e.db
	origPath := origDB.Path()
	tempPath := origPath + resyncTempSuffix

	// Snapshot old non-OpenCode file-backed session count to
	// detect empty-discovery. OpenCode is excluded entirely
	// because a root may legitimately fall back between
	// storage and SQLite sources across resyncs. Fail closed:
	// if we can't query, assume old DB has file-backed data
	// worth protecting.
	oldFileSessions, err := origDB.FileBackedSessionCount(
		context.Background(),
	)
	if err != nil {
		log.Printf("resync: get old file count: %v", err)
		oldFileSessions = 1
	} else {
		oldFileSessions -= e.countRootOpenCodeSessions(
			origDB,
		)
		if oldFileSessions < 0 {
			oldFileSessions = 0
		}
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
		stats = SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: " + err.Error(),
			},
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return
	}

	// 2b. Copy excluded session IDs from the old DB so that
	// UpsertSession skips permanently deleted sessions during
	// the sync. This must happen before syncAllLocked.
	if err := newDB.CopyExcludedSessionsFrom(origPath); err != nil {
		log.Printf("resync: pre-sync copy excluded sessions: %v", err)
		// Non-fatal: worst case, deleted sessions reappear.
	}

	// 3. Point engine at newDB and sync into it.
	e.openCodeArchiveStore = origDB
	e.db = newDB
	stats = e.syncAllLocked(ctx, onProgress, time.Time{})
	e.db = origDB // restore immediately
	e.openCodeArchiveStore = nil

	// Abort swap when the fresh DB would be worse than the
	// original:
	// - sync was cancelled (partial rebuild)
	// - nothing synced at all (empty discovery, or all skipped)
	//   when old DB had data
	// - more files failed than succeeded (permission errors,
	//   disk issues)
	// OpenCode-only rebuilds are allowed to finish with 0
	// freshly synced sessions when every storage parse was
	// intentionally preserved against the archive; orphan copy
	// restores those rows immediately after the sync pass.
	// A few permanent parse failures are tolerated since those
	// files were broken in the old DB too.
	emptyDiscovery := stats.filesDiscovered == 0 &&
		stats.filesOK == 0 &&
		oldFileSessions > 0
	openCodeArchiveOnly := stats.Synced == 0 &&
		stats.TotalSessions > 0 &&
		stats.Failed == 0 &&
		oldFileSessions == 0
	abortSwap := stats.Aborted ||
		emptyDiscovery ||
		(stats.Synced == 0 &&
			stats.TotalSessions > 0 &&
			!openCodeArchiveOnly) ||
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

	// Re-link subagent sessions after orphan copy so copied
	// tool_calls.subagent_session_id references are resolved.
	if orphaned > 0 {
		if err := newDB.LinkSubagentSessions(); err != nil {
			log.Printf("resync: relink subagent sessions: %v", err)
		}
	}

	// Merge user-managed data (display_name, deleted_at,
	// starred_sessions, pinned_messages) from the old DB
	// so renames, soft-deletes, stars, and pins survive.
	if err := newDB.CopySessionMetadataFrom(origPath); err != nil {
		log.Printf("resync: copy session metadata: %v", err)
		// Non-fatal: worst case, renames/soft-deletes are lost.
	}

	// Reclassify is_automated across every row. Orphan-copied
	// rows carry is_automated values computed against the OLD
	// DB's classifier set; the temp DB's at-Open backfill ran on
	// an empty table and stamped the current hash, so without
	// this pass those rows would be permanently stuck with stale
	// flags. Non-fatal: worst case, some sessions keep their
	// pre-resync classification until the next algorithm bump.
	if err := newDB.ForceBackfillIsAutomated(); err != nil {
		log.Printf("resync: reclassify is_automated: %v", err)
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

	// Emission happens via the deferred closure above, after
	// syncMu is released.
	return
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

func (e *Engine) countRootOpenCodeSessions(
	database *db.DB,
) int {
	var count int
	err := database.Reader().QueryRow(`
		SELECT COUNT(*) FROM sessions
		WHERE agent = ?
		  AND message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL
	`, string(parser.AgentOpenCode)).Scan(&count)
	if err != nil {
		log.Printf("count root opencode sessions: %v", err)
	}
	return count
}

// Sync state keys persisted in pg_sync_state.
const (
	syncStateStartedAt  = "last_sync_started_at"
	syncStateFinishedAt = "last_sync_finished_at"
)

// LastSyncStartedAt returns the recorded start time of the
// most recent sync. Returns zero time if no sync has run.
// Use this as the mtime cutoff for quick incremental syncs —
// anything modified at or after this time must be re-evaluated.
func (e *Engine) LastSyncStartedAt() time.Time {
	raw, err := e.db.GetSyncState(syncStateStartedAt)
	if err != nil || raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// SyncAll discovers and syncs all session files from all agents.
func (e *Engine) SyncAll(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	e.syncMu.Lock()
	// Defers run LIFO: Unlock runs before the emit closure so
	// Emitter implementations cannot widen the syncMu critical
	// section or deadlock by re-entering sync code.
	defer func() {
		if stats.Synced > 0 {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	stats = e.syncAllLocked(ctx, onProgress, time.Time{})
	return
}

// SyncAllSince syncs only files whose mtime is at or after
// the given cutoff time. Use a zero time to sync everything
// (equivalent to SyncAll). The cutoff is applied after
// discovery; directory traversal still walks all session
// directories. Typical callers pass a small safety margin
// behind the last successful sync start to avoid missing
// files that were being written during a prior sync.
func (e *Engine) SyncAllSince(
	ctx context.Context, since time.Time, onProgress ProgressFunc,
) (stats SyncStats) {
	e.syncMu.Lock()
	defer func() {
		if stats.Synced > 0 {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	stats = e.syncAllLocked(ctx, onProgress, since)
	return
}

func (e *Engine) syncAllLocked(
	ctx context.Context, onProgress ProgressFunc, since time.Time,
) SyncStats {
	if ctx.Err() != nil {
		return SyncStats{Aborted: true}
	}

	e.recordSyncStarted()

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

	if !since.IsZero() {
		all = filterFilesByMtime(all, since)
	}

	verbose := onProgress == nil

	if verbose {
		log.Printf(
			"discovered %d files (%d claude, %d codex, %d copilot, %d gemini, %d cursor, %d amp, %d zencoder, %d iflow, %d vscode-copilot, %d pi) in %s",
			len(all),
			counts[parser.AgentClaude],
			counts[parser.AgentCodex],
			counts[parser.AgentCopilot],
			counts[parser.AgentGemini],
			counts[parser.AgentCursor],
			counts[parser.AgentAmp],
			counts[parser.AgentZencoder],
			counts[parser.AgentIflow],
			counts[parser.AgentVSCodeCopilot],
			counts[parser.AgentPi],
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
	results := e.startWorkers(ctx, all)
	stats := e.collectAndBatch(
		ctx, results, len(all), onProgress,
	)
	if verbose {
		log.Printf(
			"file sync: %d synced, %d skipped in %s",
			stats.Synced, stats.Skipped,
			time.Since(tWorkers).Round(time.Millisecond),
		)
	}

	// If cancelled (either collectAndBatch set Aborted, or
	// context was cancelled after the loop with no file-backed
	// sessions), return partial stats without running further
	// phases or mutating state. Don't update lastSync or
	// lastSyncStats so the UI still reflects the last
	// completed sync.
	if stats.Aborted || ctx.Err() != nil {
		stats.Aborted = true
		return stats
	}

	// Sync OpenCode sessions (DB-backed, not file-based).
	// Uses full replace because OpenCode messages can change
	// in place (streaming updates, tool result pairing).
	tOC := time.Now()
	ocPending := e.syncOpenCode(ctx)
	if len(ocPending) > 0 {
		stats.TotalSessions += len(ocPending)
		tWrite := time.Now()
		var ocWritten int
		for _, pw := range ocPending {
			if ctx.Err() != nil {
				break
			}
			switch err := e.writeSessionFull(pw); {
			case err == nil:
				ocWritten++
			case errors.Is(err, db.ErrSessionExcluded),
				errors.Is(err, errSessionPreserved):
				// Intentional skip, not a failure.
			default:
				stats.RecordFailed()
			}
		}
		stats.RecordSynced(ocWritten)
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

	if ctx.Err() != nil {
		stats.Aborted = true
		return stats
	}

	// Sync Warp sessions (DB-backed, not file-based).
	tWarp := time.Now()
	warpPending := e.syncWarp(ctx)
	if len(warpPending) > 0 {
		stats.TotalSessions += len(warpPending)
		tWrite := time.Now()
		var warpWritten int
		for _, pw := range warpPending {
			if ctx.Err() != nil {
				break
			}
			switch err := e.writeSessionFull(pw); {
			case err == nil:
				warpWritten++
			case errors.Is(err, db.ErrSessionExcluded),
				errors.Is(err, errSessionPreserved):
				// Intentional skip, not a failure.
			default:
				stats.RecordFailed()
			}
		}
		stats.RecordSynced(warpWritten)
		if verbose {
			log.Printf(
				"warp write: %d sessions in %s",
				len(warpPending),
				time.Since(tWrite).Round(time.Millisecond),
			)
		}
	}
	if verbose {
		log.Printf(
			"warp sync: %s",
			time.Since(tWarp).Round(time.Millisecond),
		)
	}

	if ctx.Err() != nil {
		stats.Aborted = true
		return stats
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

	e.recordSyncFinished()
	// Emission happens in SyncAll / SyncAllSince after syncMu is
	// released; syncAllLocked runs under the caller's lock.
	return stats
}

// recordSyncStarted persists the start time of a sync run
// into pg_sync_state. Callers use this to compute mtime
// cutoffs for future quick incremental syncs.
func (e *Engine) recordSyncStarted() {
	if e.ephemeral {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.db.SetSyncState(syncStateStartedAt, ts); err != nil {
		log.Printf("persist sync start time: %v", err)
	}
}

// recordSyncFinished persists the finish time of a completed
// sync run. Only called on successful completion (not on
// cancellation or abort).
func (e *Engine) recordSyncFinished() {
	if e.ephemeral {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.db.SetSyncState(syncStateFinishedAt, ts); err != nil {
		log.Printf("persist sync finish time: %v", err)
	}
}

// filterFilesByMtime returns only files whose mtime is at or
// after the given cutoff. Files that can't be stat'd are kept
// (so errors surface in the worker rather than being silently
// dropped). The cost is one stat per file — acceptable for
// polling use cases where most files will be skipped.
func filterFilesByMtime(
	files []parser.DiscoveredFile, cutoff time.Time,
) []parser.DiscoveredFile {
	cutoffNs := cutoff.UnixNano()
	out := files[:0]
	for _, f := range files {
		mtime, err := discoveredFileMtime(f)
		if err != nil {
			out = append(out, f)
			continue
		}
		if mtime >= cutoffNs {
			out = append(out, f)
		}
	}
	return out
}

func discoveredFileMtime(
	file parser.DiscoveredFile,
) (int64, error) {
	if file.Agent == parser.AgentOpenCode {
		if _, _, ok := parser.ParseOpenCodeSQLiteVirtualPath(file.Path); ok {
			return parser.OpenCodeSourceMtime(file.Path)
		}
	}

	info, err := os.Stat(file.Path)
	if err != nil {
		return 0, err
	}
	return info.ModTime().UnixNano(), nil
}

// syncOpenCode syncs sessions from OpenCode SQLite databases.
// Uses per-session time_updated to detect changes, so only
// modified sessions are fully parsed. Returns pending writes.
func (e *Engine) syncOpenCode(
	ctx context.Context,
) []pendingWrite {
	var allPending []pendingWrite
	for _, dir := range e.agentDirs[parser.AgentOpenCode] {
		if ctx.Err() != nil {
			break
		}
		if dir == "" {
			continue
		}
		allPending = append(
			allPending, e.syncOneOpenCode(ctx, dir)...,
		)
	}
	return allPending
}

// syncOneOpenCode handles a single OpenCode directory.
func (e *Engine) syncOneOpenCode(
	ctx context.Context, dir string,
) []pendingWrite {
	src := parser.ResolveOpenCodeSource(dir)
	if src.Mode != parser.OpenCodeSourceSQLite {
		return nil
	}
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
		if ok && storedMtime == m.FileMtime &&
			e.db.GetDataVersionByPath(m.VirtualPath) >=
				db.CurrentDataVersion() {
			continue
		}
		changed = append(changed, m.SessionID)
	}
	if len(changed) == 0 {
		return nil
	}

	var pending []pendingWrite
	for _, sid := range changed {
		if ctx.Err() != nil {
			break
		}
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
// and returns a channel of results. When ctx is cancelled,
// workers skip remaining jobs with a context error instead
// of parsing files.
func (e *Engine) startWorkers(
	ctx context.Context,
	files []parser.DiscoveredFile,
) <-chan syncJob {
	workers := min(max(runtime.NumCPU(), 2), maxWorkers)

	jobs := make(chan parser.DiscoveredFile, len(files))
	results := make(chan syncJob, len(files))

	for range workers {
		go func() {
			for file := range jobs {
				if ctx.Err() != nil {
					results <- syncJob{
						processResult: processResult{
							err: ctx.Err(),
						},
						path: file.Path,
					}
					continue
				}
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
// When ctx is cancelled, it stops processing new results
// and returns partial stats.
func (e *Engine) collectAndBatch(
	ctx context.Context,
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

	for i := range total {
		var r syncJob
		select {
		case <-ctx.Done():
			stats.Aborted = true
			drainResults(results, total-i)
			goto flush
		case r = <-results:
		}

		if r.err != nil {
			// Workers emit ctx.Err() for files skipped
			// after cancellation — treat the same as the
			// ctx.Done() branch above.
			if ctx.Err() != nil {
				stats.Aborted = true
				drainResults(results, total-i-1)
				goto flush
			}
			stats.RecordFailed()
			if r.cacheSkip && r.mtime != 0 {
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
		if len(r.results) == 0 && r.incremental == nil {
			if r.cacheSkip {
				e.cacheSkip(r.path, r.mtime)
			}
			progress.SessionsDone++
			if onProgress != nil {
				onProgress(progress)
			}
			continue
		}
		if r.cacheSkip {
			e.clearSkip(r.path)
		}
		stats.filesOK++

		if r.incremental != nil {
			if err := e.writeIncremental(r.incremental); err != nil {
				log.Printf("%v", err)
				stats.RecordFailed()
				continue
			}
			stats.RecordSynced(1)
			progress.MessagesIndexed += len(
				r.incremental.msgs,
			)
		} else {
			for _, pr := range r.results {
				pending = append(pending, pendingWrite{
					sess: pr.Session,
					msgs: pr.Messages,
				})
			}
		}

		if len(pending) >= batchSize {
			writtenSessions, writtenMessages :=
				e.writeBatch(pending)
			stats.RecordSynced(writtenSessions)
			progress.MessagesIndexed += writtenMessages
			pending = pending[:0]
		}

		progress.SessionsDone++
		if onProgress != nil {
			onProgress(progress)
		}
	}

flush:
	if len(pending) > 0 {
		writtenSessions, writtenMessages :=
			e.writeBatch(pending)
		stats.RecordSynced(writtenSessions)
		progress.MessagesIndexed += writtenMessages
	}

	// Link subagent child sessions to their parents via
	// tool_calls.subagent_session_id references. Run once
	// after all batches to avoid repeated full-table scans.
	if err := e.db.LinkSubagentSessions(); err != nil {
		log.Printf("link subagent sessions: %v", err)
	}

	progress.Phase = PhaseDone
	if onProgress != nil {
		onProgress(progress)
	}
	return stats
}

// drainResults consumes remaining items from the results
// channel so that worker goroutines can exit and be collected.
func drainResults(results <-chan syncJob, remaining int) {
	for range remaining {
		<-results
	}
}

// incrementalUpdate holds the delta produced by an
// incremental JSONL parse, used to partially update the
// session row without overwriting unrelated columns.
type incrementalUpdate struct {
	sessionID            string
	msgs                 []parser.ParsedMessage
	endedAt              time.Time
	msgCount             int // total (old + new)
	userMsgCount         int // total (old + new)
	fileSize             int64
	fileMtime            int64
	totalOutputTokens    int // absolute (old + new)
	peakContextTokens    int // absolute max(old, new)
	hasTotalOutputTokens bool
	hasPeakContextTokens bool
}

type processResult struct {
	results     []parser.ParseResult
	skip        bool
	mtime       int64
	err         error
	incremental *incrementalUpdate
	cacheSkip   bool
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
	if file.Agent == parser.AgentOpenHands {
		snapshot, err := parser.OpenHandsSnapshot(file.Path)
		if err != nil {
			return processResult{err: err}
		}
		mtime = snapshot.Mtime
	}
	cacheSkip := e.shouldCacheSkip(file)

	// Skip files cached from a previous sync (parse errors
	// or non-interactive sessions) whose mtime is unchanged.
	// Legacy codex_exec entries from pre-bulk-sync builds are
	// scrubbed once at engine construction by
	// migrateLegacyCodexExecSkips, so this check can treat
	// the skip cache as authoritative without per-file
	// re-validation.
	if cacheSkip {
		e.skipMu.RLock()
		cachedMtime, cached := e.skipCache[file.Path]
		e.skipMu.RUnlock()
		if cached && cachedMtime == mtime {
			return processResult{
				skip:      true,
				mtime:     mtime,
				cacheSkip: true,
			}
		}
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
	case parser.AgentOpenCode:
		res = e.processOpenCode(file, info)
	case parser.AgentOpenHands:
		res = e.processOpenHands(file, info)
	case parser.AgentCursor:
		res = e.processCursor(file, info)
	case parser.AgentIflow:
		res = e.processIflow(file, info)
	case parser.AgentAmp:
		res = e.processAmp(file, info)
	case parser.AgentZencoder:
		res = e.processZencoder(file, info)
	case parser.AgentVSCodeCopilot:
		res = e.processVSCodeCopilot(file, info)
	case parser.AgentPi:
		res = e.processPi(file, info)
	case parser.AgentOpenClaw:
		res = e.processOpenClaw(file, info)
	case parser.AgentKimi:
		res = e.processKimi(file, info)
	case parser.AgentKiro:
		res = e.processKiro(file, info)
	case parser.AgentKiroIDE:
		res = e.processKiroIDE(file, info)
	case parser.AgentCortex:
		res = e.processCortex(file, info)
	case parser.AgentHermes:
		res = e.processHermes(file, info)
	case parser.AgentPositron:
		res = e.processPositron(file, info)
	default:
		res = processResult{
			err: fmt.Errorf(
				"unknown agent type: %s", file.Agent,
			),
		}
	}
	res.cacheSkip = cacheSkip
	res.mtime = mtime
	return res
}

func (e *Engine) shouldCacheSkip(
	file parser.DiscoveredFile,
) bool {
	if file.Agent != parser.AgentOpenCode {
		return true
	}
	if filepath.Base(file.Path) == "opencode.db" {
		return false
	}
	if _, _, ok := parser.ParseOpenCodeSQLiteVirtualPath(file.Path); ok {
		return false
	}
	for _, dir := range e.agentDirs[parser.AgentOpenCode] {
		if dir == "" {
			continue
		}
		if parser.ResolveOpenCodeSource(dir).Mode !=
			parser.OpenCodeSourceStorage {
			continue
		}
		if rel, ok := isUnder(dir, file.Path); ok {
			rel = filepath.ToSlash(rel)
			return !strings.HasPrefix(
				rel, "storage/session/",
			)
		}
	}
	return true
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

// InjectSkipCache merges entries into the in-memory skip
// cache. Used by remote sync to pre-populate with
// translated paths.
func (e *Engine) InjectSkipCache(entries map[string]int64) {
	e.skipMu.Lock()
	defer e.skipMu.Unlock()
	maps.Copy(e.skipCache, entries)
}

// SnapshotSkipCache returns a copy of the in-memory skip
// cache.
func (e *Engine) SnapshotSkipCache() map[string]int64 {
	e.skipMu.RLock()
	defer e.skipMu.RUnlock()
	out := make(map[string]int64, len(e.skipCache))
	maps.Copy(out, e.skipCache)
	return out
}

// persistSkipCache writes the in-memory skip cache to the
// database so skipped files survive process restarts.
// Returns the number of entries persisted.
func (e *Engine) persistSkipCache() int {
	if e.ephemeral {
		return 0
	}
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
	fullID := e.idPrefix + sessionID
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(
		fullID,
	)
	if !ok {
		return false
	}
	if storedSize != info.Size() ||
		storedMtime != info.ModTime().UnixNano() {
		return false
	}
	if e.db.GetSessionDataVersion(fullID) <
		db.CurrentDataVersion() {
		return false
	}
	return true
}

// shouldSkipByPath checks file size and mtime against what is
// stored in the database by file_path. Used for codex/gemini
// files where the session ID requires parsing.
func (e *Engine) shouldSkipByPath(
	path string, info os.FileInfo,
) bool {
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByPath(
		lookupPath,
	)
	if !ok {
		return false
	}
	if storedSize != info.Size() ||
		storedMtime != info.ModTime().UnixNano() {
		return false
	}
	if e.db.GetDataVersionByPath(lookupPath) <
		db.CurrentDataVersion() {
		return false
	}
	return true
}

// fakeSnapshotInfo wraps a pre-computed size and mtime
// (nanoseconds) as os.FileInfo so that shouldSkipByPath can
// be reused for OpenHands snapshot-based skip detection.
type fakeSnapshotInfo struct {
	fSize  int64
	fMtime int64
}

func (f fakeSnapshotInfo) Name() string      { return "" }
func (f fakeSnapshotInfo) Size() int64       { return f.fSize }
func (f fakeSnapshotInfo) Mode() os.FileMode { return 0 }
func (f fakeSnapshotInfo) ModTime() time.Time {
	return time.Unix(0, f.fMtime)
}
func (f fakeSnapshotInfo) IsDir() bool { return false }
func (f fakeSnapshotInfo) Sys() any    { return nil }

func (e *Engine) processClaude(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {

	sessionID := strings.TrimSuffix(info.Name(), ".jsonl")

	if e.shouldSkipFile(sessionID, info) {
		sess, _ := e.db.GetSession(
			context.Background(), e.idPrefix+sessionID,
		)
		if sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			return processResult{skip: true}
		}
	}

	// Try incremental parse for append-only JSONL files
	// that have already been synced.
	if res, ok := e.tryIncrementalJSONL(
		file, info, parser.AgentClaude,
		parser.ParseClaudeSessionFrom,
	); ok {
		return res
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

	inode, device := getFileIdentity(info)
	for i := range results {
		results[i].Session.File.Inode = inode
		results[i].Session.File.Device = device
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

// incrementalParseFunc reads new JSONL lines from a file
// starting at the given byte offset with the given starting
// ordinal. Returns parsed messages, the latest timestamp
// (endedAt), bytes consumed (relative to offset), and any
// error. The consumed count covers only complete, valid JSON
// lines so it can be used as a safe resume offset.
type incrementalParseFunc func(
	path string, offset int64, startOrdinal int,
) ([]parser.ParsedMessage, time.Time, int64, error)

// tryIncrementalJSONL attempts an incremental parse of an
// append-only JSONL file by reading only bytes appended since
// the last sync. Returns (result, true) on success, or
// (zero, false) to fall through to a full parse. Falls back
// to full parse when the file maps to multiple DB sessions
// (e.g. Claude DAG forks).
func (e *Engine) tryIncrementalJSONL(
	file parser.DiscoveredFile,
	info os.FileInfo,
	agent parser.AgentType,
	parseFn incrementalParseFunc,
) (processResult, bool) {
	lookupPath := file.Path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(file.Path)
	}
	inc, ok := e.db.GetSessionForIncremental(lookupPath)
	if !ok || inc.FileSize <= 0 {
		return processResult{}, false
	}

	// Existing rows from an older parser lack new metadata
	// columns. Force a full parse so the rewrite picks them
	// up rather than appending new rows on top of stale ones.
	if e.db.GetSessionDataVersion(inc.ID) <
		db.CurrentDataVersion() {
		return processResult{}, false
	}

	currentSize := info.Size()
	if currentSize <= inc.FileSize {
		return processResult{}, false
	}

	// If the file was replaced (different inode/device), fall
	// back to a full parse so we don't append on top of stale
	// state. Only check when both sides have a known identity
	// (non-zero); zeros mean the data is missing or the
	// platform doesn't expose inode/device (Windows).
	if inc.FileInode != 0 && inc.FileDevice != 0 {
		curInode, curDevice := getFileIdentity(info)
		if curInode != 0 && curDevice != 0 &&
			(curInode != inc.FileInode ||
				curDevice != inc.FileDevice) {
			log.Printf(
				"incremental %s %s: file identity changed "+
					"(inode %d→%d, device %d→%d), full parse",
				agent, file.Path,
				inc.FileInode, curInode,
				inc.FileDevice, curDevice,
			)
			return processResult{}, false
		}
	}

	maxOrd := e.db.MaxOrdinal(inc.ID)
	if maxOrd < 0 {
		return processResult{}, false
	}

	newMsgs, endedAt, consumed, err := parseFn(
		file.Path, inc.FileSize, maxOrd+1,
	)
	if err != nil {
		if parser.IsIncrementalFullParseFallback(err) {
			log.Printf(
				"incremental %s %s: %v (explicit full parse fallback)",
				agent, file.Path, err,
			)
			return processResult{}, false
		}
		log.Printf(
			"incremental %s %s: %v (full parse)",
			agent, file.Path, err,
		)
		return processResult{}, false
	}

	// Use the offset through the last valid JSON line, not
	// info.Size(), so partial lines at EOF are retried on
	// the next sync.
	newOffset := inc.FileSize + consumed

	if len(newMsgs) == 0 {
		// No new messages, but advance the offset past
		// non-message lines (progress events, metadata)
		// so they aren't re-read on every sync. Carry
		// endedAt forward so session bounds stay current
		// with non-message timestamps (e.g. progress).
		if consumed > 0 {
			return processResult{
				incremental: &incrementalUpdate{
					sessionID:            inc.ID,
					endedAt:              endedAt,
					msgCount:             inc.MsgCount,
					userMsgCount:         inc.UserMsgCount,
					fileSize:             newOffset,
					fileMtime:            info.ModTime().UnixNano(),
					totalOutputTokens:    inc.TotalOutputTokens,
					peakContextTokens:    inc.PeakContextTokens,
					hasTotalOutputTokens: inc.HasTotalOutputTokens,
					hasPeakContextTokens: inc.HasPeakContextTokens,
				},
			}, true
		}
		return processResult{skip: true}, true
	}

	newUserCount := countUserMsgs(newMsgs)

	log.Printf(
		"incremental %s %s: %d new message(s) "+
			"from offset %d",
		agent, inc.ID, len(newMsgs), inc.FileSize,
	)

	totalOut := inc.TotalOutputTokens
	peakCtx := inc.PeakContextTokens
	hasTotalOut := inc.HasTotalOutputTokens
	hasPeakCtx := inc.HasPeakContextTokens
	for _, m := range newMsgs {
		msgHasCtx, msgHasOut := m.TokenPresence()
		if msgHasOut {
			totalOut += m.OutputTokens
			hasTotalOut = true
		}
		if msgHasCtx && (!hasPeakCtx || m.ContextTokens > peakCtx) {
			peakCtx = m.ContextTokens
			hasPeakCtx = true
		}
	}

	return processResult{
		incremental: &incrementalUpdate{
			sessionID:            inc.ID,
			msgs:                 newMsgs,
			endedAt:              endedAt,
			msgCount:             inc.MsgCount + len(newMsgs),
			userMsgCount:         inc.UserMsgCount + newUserCount,
			fileSize:             newOffset,
			fileMtime:            info.ModTime().UnixNano(),
			totalOutputTokens:    totalOut,
			peakContextTokens:    peakCtx,
			hasTotalOutputTokens: hasTotalOut,
			hasPeakContextTokens: hasPeakCtx,
		},
	}, true
}

func (e *Engine) processCodex(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {

	// Fast path: skip by file_path + mtime before parsing.
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	codexParseFn := func(
		path string, offset int64, startOrd int,
	) ([]parser.ParsedMessage, time.Time, int64, error) {
		return parser.ParseCodexSessionFrom(
			path, offset, startOrd, false,
		)
	}
	if res, ok := e.tryIncrementalJSONL(
		file, info, parser.AgentCodex, codexParseFn,
	); ok {
		return res
	}

	sess, msgs, err := parser.ParseCodexSession(
		file.Path, e.machine, false,
	)
	if err != nil {
		return processResult{err: err}
	}

	sess.File.Inode, sess.File.Device = getFileIdentity(info)

	hash, err := ComputeFileHash(file.Path)
	if err == nil && sess.File.Hash == "" {
		sess.File.Hash = hash
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func (e *Engine) processOpenCode(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if dbPath, sessionID, ok := parser.ParseOpenCodeSQLiteVirtualPath(file.Path); ok {
		sess, msgs, err := parser.ParseOpenCodeSession(
			dbPath, sessionID, e.machine,
		)
		if err != nil {
			return processResult{err: err}
		}
		if sess == nil {
			return processResult{}
		}
		return processResult{
			results: []parser.ParseResult{
				{Session: *sess, Messages: msgs},
			},
		}
	}
	if filepath.Base(file.Path) == "opencode.db" {
		metas, err := parser.ListOpenCodeSessionMeta(file.Path)
		if err != nil {
			return processResult{err: err}
		}
		var results []parser.ParseResult
		for _, meta := range metas {
			_, storedMtime, ok := e.db.GetFileInfoByPath(meta.VirtualPath)
			if ok && storedMtime == meta.FileMtime &&
				e.db.GetDataVersionByPath(meta.VirtualPath) >=
					db.CurrentDataVersion() {
				continue
			}
			sess, msgs, err := parser.ParseOpenCodeSession(
				file.Path, meta.SessionID, e.machine,
			)
			if err != nil {
				log.Printf(
					"opencode sqlite watch session %s: %v",
					meta.SessionID, err,
				)
				continue
			}
			if sess == nil {
				continue
			}
			results = append(results, parser.ParseResult{
				Session:  *sess,
				Messages: msgs,
			})
		}
		return processResult{results: results}
	}
	if e.shouldSkipOpenCodeByPath(file.Path) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseOpenCodeFile(
		file.Path, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil && sess.File.Hash == "" {
		sess.File.Hash = hash
	}

	sess.File.Inode, sess.File.Device = getFileIdentity(info)

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func (e *Engine) shouldSkipOpenCodeByPath(path string) bool {
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}

	_, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
	if !ok {
		return false
	}

	sourceMtime, err := parser.OpenCodeSourceMtime(path)
	if err != nil || sourceMtime == 0 {
		return false
	}
	if storedMtime != sourceMtime {
		return false
	}
	if e.db.GetDataVersionByPath(lookupPath) <
		db.CurrentDataVersion() {
		return false
	}
	return true
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

func (e *Engine) processZencoder(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseZencoderSession(
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

func (e *Engine) processKimi(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseKimiSession(
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

func (e *Engine) processKiro(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseKiroSession(
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

func (e *Engine) processKiroIDE(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseKiroIDESession(
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

func (e *Engine) processCortex(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseCortexSession(
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

func (e *Engine) processHermes(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseHermesSession(
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

func (e *Engine) processPositron(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParsePositronSession(
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

func (e *Engine) processOpenHands(
	file parser.DiscoveredFile, _ os.FileInfo,
) processResult {
	snapshot, err := parser.OpenHandsSnapshot(file.Path)
	if err != nil {
		return processResult{err: err}
	}

	fi := fakeSnapshotInfo{
		fSize: snapshot.Size, fMtime: snapshot.Mtime,
	}
	if e.shouldSkipByPath(file.Path, fi) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseOpenHandsSession(
		file.Path, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
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

// processPi parses a pi session file and returns the result
// for batching. Modeled on processClaude.
func (e *Engine) processPi(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParsePiSession(
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
		results: []parser.ParseResult{{
			Session:  *sess,
			Messages: msgs,
		}},
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

func (e *Engine) processIflow(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	// Extract session ID from filename: session-<uuid>.jsonl
	sessionID := "iflow:" + strings.TrimPrefix(strings.TrimSuffix(info.Name(), ".jsonl"), "session-")

	if e.shouldSkipFile(sessionID, info) {
		sess, _ := e.db.GetSession(
			context.Background(), e.idPrefix+sessionID,
		)
		if sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			return processResult{skip: true}
		}
	}

	// Determine project name from cwd if possible
	project := parser.GetProjectName(file.Project)
	cwd, gitBranch := parser.ExtractIflowProjectHints(
		file.Path,
	)
	if cwd != "" {
		if p := parser.ExtractProjectFromCwdWithBranch(
			cwd, gitBranch,
		); p != "" {
			project = p
		}
	}

	results, err := parser.ParseIflowSession(
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

// computeFinalStreak counts trailing consecutive failures
// from the end of the tool call list.
func computeFinalStreak(calls []signals.ToolCallRow) int {
	streak := 0
	for i := len(calls) - 1; i >= 0; i-- {
		if signals.IsFailure(calls[i]) {
			streak++
		} else {
			break
		}
	}
	return streak
}

// RecomputeSignals recomputes signals for a single session
// from existing DB data. Returns nil on success (including
// when the session no longer exists). Returns an error when
// the recompute could not complete -- BackfillSignals uses
// that signal to keep the one-shot completion marker unset
// so the next startup can retry.
func (e *Engine) RecomputeSignals(
	ctx context.Context, sessionID string,
) error {
	return e.recomputeSignalsFromDB(ctx, sessionID)
}

// recomputeSignalsFromDB loads a session's full message history
// and stored metadata, runs the pure in-memory signal compute
// over them, and persists the result. Used when callers don't
// already have the message slice in memory (legacy backfill,
// incremental writes).
func (e *Engine) recomputeSignalsFromDB(
	ctx context.Context, sessionID string,
) error {
	sess, err := e.db.GetSessionFull(ctx, sessionID)
	if err != nil {
		return fmt.Errorf(
			"loading session %s: %w", sessionID, err,
		)
	}
	if sess == nil {
		return nil
	}
	msgs, err := e.db.GetAllMessages(ctx, sessionID)
	if err != nil {
		log.Printf(
			"signals: load messages %s: %v",
			sessionID, err,
		)
		return fmt.Errorf(
			"loading messages %s: %w", sessionID, err,
		)
	}
	update := computeSignalsFromMessages(*sess, msgs)
	if err := e.db.UpdateSessionSignals(
		sessionID, update,
	); err != nil {
		log.Printf(
			"signals: update %s: %v", sessionID, err,
		)
		return fmt.Errorf(
			"updating signals %s: %w", sessionID, err,
		)
	}
	return nil
}

// isAutomatedFromSession recomputes the is_automated flag using
// the same rule UpsertSession applies. Hoisted out of UpsertSession
// so callers can set the field on their in-memory Session before
// running signal computation, ensuring the same value is used by
// outcome classification and persisted to the row.
func isAutomatedFromSession(s db.Session) bool {
	return s.UserMessageCount <= 1 &&
		s.FirstMessage != nil &&
		db.IsAutomatedSession(*s.FirstMessage)
}

type pendingWrite struct {
	sess parser.ParsedSession
	msgs []parser.ParsedMessage
}

func (e *Engine) writeBatch(
	batch []pendingWrite,
) (writtenSessions, writtenMessages int) {
	for _, pw := range batch {
		msgs := toDBMessages(pw, e.blockedResultCategories)
		s := toDBSession(pw)
		s.MessageCount, s.UserMessageCount =
			postFilterCounts(msgs)
		e.applyRemoteRewrites(&s, msgs)
		s.IsAutomated = isAutomatedFromSession(s)

		// Detect stale parser version BEFORE UpsertSession
		// overwrites it. Existing message rows from an
		// older parser lack new metadata columns, and newly
		// emitted compact-boundary messages can shift the
		// ordinal stream — both demand a full rewrite
		// rather than the append-only writeMessages path.
		stale := false
		if existing := e.db.GetSessionDataVersion(s.ID); existing > 0 &&
			existing < db.CurrentDataVersion() {
			stale = true
		}
		if e.shouldPreserveOpenCodeArchive(
			pw.sess.Agent, pw.sess.File.Path, s.ID,
			pw.sess.File.Mtime, derefString(s.FileHash), msgs,
		) {
			continue
		}

		// UpsertSession first: the session row must exist
		// before messages can be inserted (FK constraint).
		// This is safe because writeBatch runs full parses
		// that always recompute all columns. For
		// incremental updates (writeIncremental), messages
		// are written first since the session already
		// exists.
		if err := e.db.UpsertSession(s); err != nil {
			if errors.Is(err, db.ErrSessionExcluded) {
				if pw.sess.File.Path != "" {
					e.cacheSkip(
						pw.sess.File.Path,
						pw.sess.File.Mtime,
					)
				}
				continue
			}
			log.Printf("upsert session %s: %v", s.ID, err)
			continue
		}

		replaceMessages := stale ||
			pw.sess.Agent == parser.AgentOpenCode

		var werr error
		if replaceMessages {
			werr = e.db.ReplaceSessionMessages(s.ID, msgs)
		} else {
			werr = e.writeMessages(s.ID, msgs)
		}
		if werr != nil {
			log.Printf(
				"write messages for %s: %v",
				s.ID, werr,
			)
			continue
		}

		// Advance data_version only after the message write
		// succeeded. UpsertSession deliberately does not
		// touch this column so a transient write failure
		// won't leave the session marked at the current
		// parser version with stale messages.
		if err := e.db.SetSessionDataVersion(
			s.ID, db.CurrentDataVersion(),
		); err != nil {
			log.Printf(
				"set data_version for %s: %v", s.ID, err,
			)
		}

		update := computeSignalsFromMessages(s, msgs)
		if err := e.db.UpdateSessionSignals(
			s.ID, update,
		); err != nil {
			log.Printf(
				"signals: update %s: %v", s.ID, err,
			)
		}
		writtenSessions++
		writtenMessages += len(msgs)
	}
	return writtenSessions, writtenMessages
}

// writeIncremental appends new messages and partially updates
// session metadata without overwriting columns that are not
// recomputed during incremental parsing (e.g. file_hash,
// parent_session_id, relationship_type).
func (e *Engine) writeIncremental(
	inc *incrementalUpdate,
) error {
	dbMsgs := toDBMessages(
		pendingWrite{
			sess: parser.ParsedSession{ID: inc.sessionID},
			msgs: inc.msgs,
		},
		e.blockedResultCategories,
	)

	// Adjust counts for blocked-category filtering.
	newTotal, newUser := postFilterCounts(dbMsgs)
	filtered := len(inc.msgs) - newTotal
	msgCount := inc.msgCount - filtered
	userFiltered := countUserMsgs(inc.msgs) - newUser
	userMsgCount := inc.userMsgCount - userFiltered

	var endedAt *string
	if !inc.endedAt.IsZero() {
		s := inc.endedAt.Format(time.RFC3339Nano)
		endedAt = &s
	}

	// Write messages first — only advance file_size when
	// the insert succeeds so a failure is retried.
	if err := e.writeMessages(
		inc.sessionID, dbMsgs,
	); err != nil {
		return fmt.Errorf(
			"incremental messages %s: %w",
			inc.sessionID, err,
		)
	}

	if err := e.db.UpdateSessionIncremental(
		inc.sessionID, endedAt,
		msgCount, userMsgCount,
		inc.fileSize, inc.fileMtime,
		inc.totalOutputTokens, inc.peakContextTokens,
		inc.hasTotalOutputTokens, inc.hasPeakContextTokens,
	); err != nil {
		return fmt.Errorf(
			"incremental update %s: %w",
			inc.sessionID, err,
		)
	}

	// Errors here are already logged by recomputeSignalsFromDB
	// and are non-fatal for incremental sync; the next
	// incremental write will retry.
	_ = e.recomputeSignalsFromDB(
		context.Background(), inc.sessionID,
	)

	return nil
}

// writeMessages uses an incremental append when possible.
// Session files are append-only, so if the DB already has
// messages for this session and the new set is larger, we
// only insert the new messages (avoiding expensive FTS5
// delete+reinsert of existing content).
func (e *Engine) writeMessages(
	sessionID string, msgs []db.Message,
) error {
	maxOrd := e.db.MaxOrdinal(sessionID)

	// No existing messages — insert all.
	if maxOrd < 0 {
		if err := e.db.InsertMessages(msgs); err != nil {
			return fmt.Errorf(
				"insert messages for %s: %w",
				sessionID, err,
			)
		}
		return nil
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
		return nil
	}

	if err := e.db.InsertMessages(msgs); err != nil {
		return fmt.Errorf(
			"append messages for %s: %w",
			sessionID, err,
		)
	}
	return nil
}

// writeSessionFull upserts a session and does a full
// delete+reinsert of its messages. Used by explicit
// single-session re-syncs where existing content may have
// changed (not just appended).
// writeSessionFull returns nil on success,
// db.ErrSessionExcluded for intentional skips, or
// another error for real failures.
func (e *Engine) writeSessionFull(pw pendingWrite) error {
	msgs := toDBMessages(pw, e.blockedResultCategories)
	s := toDBSession(pw)
	s.MessageCount, s.UserMessageCount =
		postFilterCounts(msgs)
	e.applyRemoteRewrites(&s, msgs)
	s.IsAutomated = isAutomatedFromSession(s)
	if e.shouldPreserveOpenCodeArchive(
		pw.sess.Agent, pw.sess.File.Path, s.ID,
		pw.sess.File.Mtime, derefString(s.FileHash), msgs,
	) {
		return errSessionPreserved
	}
	if err := e.db.UpsertSession(s); err != nil {
		if errors.Is(err, db.ErrSessionExcluded) {
			if pw.sess.File.Path != "" {
				e.cacheSkip(pw.sess.File.Path, pw.sess.File.Mtime)
			}
			return db.ErrSessionExcluded
		}
		log.Printf("upsert session %s: %v", s.ID, err)
		return err
	}
	if err := e.db.ReplaceSessionMessages(
		s.ID, msgs,
	); err != nil {
		log.Printf(
			"replace messages for %s: %v",
			s.ID, err,
		)
		return err
	}

	// See writeBatch for why data_version is bumped here
	// rather than inside UpsertSession.
	if err := e.db.SetSessionDataVersion(
		s.ID, db.CurrentDataVersion(),
	); err != nil {
		log.Printf(
			"set data_version for %s: %v", s.ID, err,
		)
	}

	update := computeSignalsFromMessages(s, msgs)
	if err := e.db.UpdateSessionSignals(s.ID, update); err != nil {
		log.Printf(
			"signals: update %s: %v", s.ID, err,
		)
	}

	return nil
}

func (e *Engine) shouldPreserveOpenCodeArchive(
	agent parser.AgentType, path, sessionID string,
	currentMtime int64,
	currentHash string,
	currentMsgs []db.Message,
) bool {
	if agent != parser.AgentOpenCode {
		return false
	}
	store := e.openCodeArchiveStore
	if store == nil {
		store = e.db
	}
	stored, err := store.GetSessionFull(
		context.Background(), sessionID,
	)
	if err != nil || stored == nil {
		return false
	}
	storedHash := derefString(stored.FileHash)
	storedPath := derefString(stored.FilePath)
	storedMtime := derefInt64(stored.FileMtime)
	storedIsStorageArchive := parser.HasOpenCodeStorageFingerprint(
		storedHash,
	) || isOpenCodeStoragePath(storedPath)
	if isOpenCodeSQLiteVirtualPath(path) &&
		!storedIsStorageArchive {
		return false
	}
	storedMsgs, err := store.GetAllMessages(
		context.Background(), sessionID,
	)
	if err != nil || len(storedMsgs) == 0 {
		return false
	}
	// A changed storage fingerprint alone is not enough to
	// preserve the archive. OpenCode legitimately rewrites
	// live child files in place, so we only preserve when the
	// newly parsed transcript also looks incomplete relative
	// to what is already archived.
	if parser.HasOpenCodeStorageFingerprint(storedHash) &&
		parser.HasOpenCodeStorageFingerprint(currentHash) &&
		!parser.OpenCodeStorageFingerprintMissing(
			storedHash, currentHash,
		) {
		return false
	}
	if storedIsStorageArchive &&
		isOpenCodeSQLiteVirtualPath(path) &&
		currentMtime != 0 &&
		storedMtime != 0 &&
		currentMtime <= storedMtime {
		log.Printf(
			"skip opencode session %s: sqlite fallback is not newer than preserved storage archive",
			sessionID,
		)
		return true
	}
	if openCodeLegacyArchiveLooksIncomplete(
		currentMsgs, storedMsgs,
	) {
		if parser.HasOpenCodeStorageFingerprint(storedHash) {
			log.Printf(
				"skip opencode session %s: storage fingerprint changed but update looks incomplete relative to archive",
				sessionID,
			)
		} else {
			log.Printf(
				"skip opencode session %s: storage update looks incomplete relative to legacy archive",
				sessionID,
			)
		}
		return true
	}
	return false
}

func isOpenCodeStoragePath(path string) bool {
	return strings.HasSuffix(path, ".json") &&
		!isOpenCodeSQLiteVirtualPath(path)
}

func isOpenCodeSQLiteVirtualPath(path string) bool {
	_, _, ok := parser.ParseOpenCodeSQLiteVirtualPath(path)
	return ok
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func openCodeLegacyArchiveLooksIncomplete(
	parsed, stored []db.Message,
) bool {
	if len(parsed) < len(stored) {
		return true
	}
	for i := range stored {
		if openCodeMessageLooksIncomplete(
			parsed[i], stored[i],
		) {
			return true
		}
	}
	return false
}

func openCodeMessageLooksIncomplete(
	parsed, stored db.Message,
) bool {
	if parsed.Ordinal != stored.Ordinal ||
		parsed.Role != stored.Role {
		return false
	}
	if parsed.ContentLength < stored.ContentLength {
		return true
	}
	if parsed.HasThinking != stored.HasThinking &&
		stored.HasThinking {
		return true
	}
	if stored.HasOutputTokens &&
		(!parsed.HasOutputTokens ||
			parsed.OutputTokens < stored.OutputTokens) {
		return true
	}
	if stored.HasContextTokens &&
		(!parsed.HasContextTokens ||
			parsed.ContextTokens < stored.ContextTokens) {
		return true
	}
	if len(parsed.ToolCalls) < len(stored.ToolCalls) {
		return true
	}
	return countToolResultEvents(parsed.ToolCalls) <
		countToolResultEvents(stored.ToolCalls)
}

func countToolResultEvents(calls []db.ToolCall) int {
	total := 0
	for _, call := range calls {
		total += len(call.ResultEvents)
	}
	return total
}

// applyRemoteRewrites prefixes session IDs and rewrites
// file paths for remote sync. No-op when idPrefix is empty.
func (e *Engine) applyRemoteRewrites(
	s *db.Session, msgs []db.Message,
) {
	if e.idPrefix == "" {
		return
	}
	s.ID = e.idPrefix + s.ID
	if s.ParentSessionID != nil && *s.ParentSessionID != "" {
		p := e.idPrefix + *s.ParentSessionID
		s.ParentSessionID = &p
	}
	if e.pathRewriter != nil && s.FilePath != nil {
		fp := e.pathRewriter(*s.FilePath)
		s.FilePath = &fp
	}
	for i := range msgs {
		msgs[i].SessionID = s.ID
		for j := range msgs[i].ToolCalls {
			msgs[i].ToolCalls[j].SessionID = s.ID
			if msgs[i].ToolCalls[j].SubagentSessionID != "" {
				msgs[i].ToolCalls[j].SubagentSessionID =
					e.idPrefix + msgs[i].ToolCalls[j].SubagentSessionID
			}
			for k := range msgs[i].ToolCalls[j].ResultEvents {
				re := &msgs[i].ToolCalls[j].ResultEvents[k]
				if re.SubagentSessionID != "" {
					re.SubagentSessionID =
						e.idPrefix + re.SubagentSessionID
				}
			}
		}
	}
}

// toDBSession converts a pendingWrite to a db.Session.
func toDBSession(pw pendingWrite) db.Session {
	hasTotal, hasPeak := pw.sess.TokenCoverage(pw.msgs)
	s := db.Session{
		ID:                   pw.sess.ID,
		Project:              pw.sess.Project,
		Machine:              pw.sess.Machine,
		Agent:                string(pw.sess.Agent),
		MessageCount:         pw.sess.MessageCount,
		UserMessageCount:     pw.sess.UserMessageCount,
		ParentSessionID:      strPtr(pw.sess.ParentSessionID),
		RelationshipType:     string(pw.sess.RelationshipType),
		TotalOutputTokens:    pw.sess.TotalOutputTokens,
		PeakContextTokens:    pw.sess.PeakContextTokens,
		HasTotalOutputTokens: hasTotal,
		HasPeakContextTokens: hasPeak,
		Cwd:                  pw.sess.Cwd,
		GitBranch:            pw.sess.GitBranch,
		SourceSessionID:      pw.sess.SourceSessionID,
		SourceVersion:        pw.sess.SourceVersion,
		ParserMalformedLines: pw.sess.MalformedLines,
		IsTruncated:          pw.sess.IsTruncated,
		// data_version is intentionally left at the
		// existing column default (0). UpsertSession does
		// not persist this field; the caller bumps it via
		// SetSessionDataVersion only after the message
		// rewrite succeeds.
		FilePath:   strPtr(pw.sess.File.Path),
		FileSize:   int64Ptr(pw.sess.File.Size),
		FileMtime:  int64Ptr(pw.sess.File.Mtime),
		FileInode:  int64Ptr(pw.sess.File.Inode),
		FileDevice: int64Ptr(pw.sess.File.Device),
		FileHash:   strPtr(pw.sess.File.Hash),
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
		hasCtx, hasOut := m.TokenPresence()
		msgs[i] = db.Message{
			SessionID:         pw.sess.ID,
			Ordinal:           m.Ordinal,
			Role:              string(m.Role),
			Content:           m.Content,
			ThinkingText:      m.ThinkingText,
			Timestamp:         timeutil.Format(m.Timestamp),
			HasThinking:       m.HasThinking,
			HasToolUse:        m.HasToolUse,
			ContentLength:     m.ContentLength,
			IsSystem:          m.IsSystem,
			Model:             m.Model,
			TokenUsage:        m.TokenUsage,
			ContextTokens:     m.ContextTokens,
			OutputTokens:      m.OutputTokens,
			HasContextTokens:  hasCtx,
			HasOutputTokens:   hasOut,
			ClaudeMessageID:   m.ClaudeMessageID,
			ClaudeRequestID:   m.ClaudeRequestID,
			SourceType:        m.SourceType,
			SourceSubtype:     m.SourceSubtype,
			SourceUUID:        m.SourceUUID,
			SourceParentUUID:  m.SourceParentUUID,
			IsSidechain:       m.IsSidechain,
			IsCompactBoundary: m.IsCompactBoundary,
			ToolCalls: convertToolCalls(
				pw.sess.ID, m.ToolCalls,
			),
			ToolResults: convertToolResults(m.ToolResults),
		}
	}
	return pairAndFilter(msgs, blocked)
}

// postFilterCounts returns the total and user message counts
// from a filtered message slice. System-injected messages
// (e.g. Zencoder compaction, continuation notices) are excluded
// from the user count.
func postFilterCounts(msgs []db.Message) (total, user int) {
	for _, m := range msgs {
		if m.Role == "user" && !m.IsSystem {
			user++
		}
	}
	return len(msgs), user
}

// countUserMsgs counts user messages in parsed messages.
func countUserMsgs(msgs []parser.ParsedMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == parser.RoleUser {
			n++
		}
	}
	return n
}

// FindSourceFile locates the original source file for a
// session ID. It first checks the stored file_path from the
// database (handles cases where filename differs from session
// ID, e.g. Zencoder header ID vs filename), then falls back
// to agent-specific path reconstruction.
func (e *Engine) FindSourceFile(sessionID string) string {
	host, rawID := parser.StripHostPrefix(sessionID)
	if host != "" {
		// Remote sessions have no local source file.
		return ""
	}

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok || !def.FileBased || def.FindSourceFunc == nil {
		return ""
	}

	// Prefer stored file_path — it's authoritative and handles
	// cases where the session ID doesn't match the filename.
	if fp := e.db.GetSessionFilePath(sessionID); fp != "" {
		if _, err := os.Stat(fp); err == nil {
			return fp
		}
	}

	bareID := strings.TrimPrefix(rawID, def.IDPrefix)
	for _, d := range e.agentDirs[def.Type] {
		if f := def.FindSourceFunc(d, bareID); f != "" {
			return f
		}
	}
	return ""
}

// SourceMtime returns the current source-backed mtime for a
// session. Most file-based agents map directly to a single source
// file, but OpenCode storage sessions derive their effective mtime
// from the session JSON plus related message/part files.
func (e *Engine) SourceMtime(sessionID string) int64 {
	host, _ := parser.StripHostPrefix(sessionID)
	if host != "" {
		return 0
	}

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok || !def.FileBased {
		return 0
	}

	path := e.FindSourceFile(sessionID)
	if path == "" {
		return 0
	}

	if def.Type == parser.AgentOpenCode {
		mtime, err := parser.OpenCodeSourceMtime(path)
		if err != nil {
			return 0
		}
		return mtime
	}

	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

// SyncSingleSession re-syncs a single session by its ID and
// uses the existing DB project as fallback where applicable.
func (e *Engine) SyncSingleSession(sessionID string) (err error) {
	e.syncMu.Lock()
	preserved := false
	// Defers run LIFO: unlock runs first (releasing syncMu), then
	// emit. Keep emission outside the critical section so a future
	// Emitter implementation can't widen the lock's scope.
	defer func() {
		if err == nil && !preserved {
			e.emit("messages")
		}
	}()
	defer e.syncMu.Unlock()

	host, _ := parser.StripHostPrefix(sessionID)
	if host != "" {
		return fmt.Errorf(
			"cannot sync remote session %s locally", sessionID,
		)
	}

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok {
		return fmt.Errorf("unknown agent for session %s", sessionID)
	}
	if !def.FileBased {
		switch def.Type {
		case parser.AgentWarp:
			return e.syncSingleWarp(sessionID)
		default:
			err = e.syncSingleOpenCode(sessionID)
			if errors.Is(err, errSessionPreserved) {
				preserved = true
				return nil
			}
			return err
		}
	}

	path := e.FindSourceFile(sessionID)
	if path == "" {
		return fmt.Errorf(
			"source file not found for %s", sessionID,
		)
	}
	if def.Type == parser.AgentOpenCode &&
		isOpenCodeSQLiteVirtualPath(path) {
		err = e.syncSingleOpenCode(sessionID)
		if errors.Is(err, errSessionPreserved) {
			preserved = true
			return nil
		}
		return err
	}

	agent := def.Type

	// Clear skip cache so explicit re-sync always processes
	// the file, even if it was cached as non-interactive
	// during a bulk SyncAll.
	file := parser.DiscoveredFile{
		Path:  path,
		Agent: agent,
	}
	if e.shouldCacheSkip(file) {
		e.clearSkip(path)
	}

	// Reuse processFile for stat and DB-skip logic.
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
		// Support both flat and nested transcript layouts.
		for _, cursorDir := range e.agentDirs[parser.AgentCursor] {
			rel, ok := isUnder(cursorDir, path)
			if !ok {
				continue
			}
			projDir, ok := parser.ParseCursorTranscriptRelPath(rel)
			if !ok {
				continue
			}
			file.Project = parser.DecodeCursorProjectDir(projDir)
			break
		}
		if file.Project == "" {
			file.Project = "unknown"
		}
	case parser.AgentIflow:
		// path is <iflowDir>/<project>/session-<uuid>.jsonl
		// Extract project dir name from parent directory
		if sess, _ := e.db.GetSession(context.Background(), sessionID); sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			file.Project = sess.Project
		} else {
			file.Project = filepath.Base(filepath.Dir(path))
		}
	case parser.AgentKimi:
		// path is <kimiDir>/<project-hash>/<session-uuid>/wire.jsonl
		// Derive project from two levels up.
		file.Project = filepath.Base(filepath.Dir(filepath.Dir(path)))
	}

	res := e.processFile(file)
	if res.err != nil {
		if res.cacheSkip && res.mtime != 0 {
			e.cacheSkip(path, res.mtime)
		}
		return res.err
	}
	if res.skip {
		return nil
	}

	// Handle incremental updates from processFile (e.g.
	// append-only JSONL that was already synced).
	if res.incremental != nil {
		return e.writeIncremental(res.incremental)
	}

	if len(res.results) == 0 {
		return nil
	}

	for _, pr := range res.results {
		if err := e.writeSessionFull(
			pendingWrite{sess: pr.Session, msgs: pr.Messages},
		); err != nil &&
			!errors.Is(err, db.ErrSessionExcluded) &&
			!errors.Is(err, errSessionPreserved) {
			return fmt.Errorf("write session %s: %w",
				pr.Session.ID, err)
		} else if errors.Is(err, errSessionPreserved) {
			preserved = true
		}
	}

	// Link subagent child sessions to their parents.
	// Required for Zencoder sessions that reference subagent
	// session IDs in tool_calls.subagent_session_id.
	if err := e.db.LinkSubagentSessions(); err != nil {
		log.Printf("link subagent sessions: %v", err)
	}

	return nil
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
		if parser.ResolveOpenCodeSource(dir).Mode !=
			parser.OpenCodeSourceSQLite {
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
		if err := e.writeSessionFull(
			pendingWrite{sess: *sess, msgs: msgs},
		); err != nil &&
			!errors.Is(err, db.ErrSessionExcluded) &&
			!errors.Is(err, errSessionPreserved) {
			return fmt.Errorf("write session %s: %w",
				sess.ID, err)
		} else if errors.Is(err, errSessionPreserved) {
			return err
		}
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

func readOpenCodeStorageSessionID(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var data struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	return data.SessionID
}

func findOpenCodeStorageSessionIDByMessageID(
	openCodeDir, messageID string,
) string {
	messageRoot := filepath.Join(
		openCodeDir, "storage", "message",
	)
	entries, err := os.ReadDir(messageRoot)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(
			messageRoot, entry.Name(), messageID+".json",
		)
		if info, err := os.Stat(path); err == nil &&
			!info.IsDir() {
			return entry.Name()
		}
	}
	return ""
}

// syncWarp syncs sessions from Warp SQLite databases.
// Uses per-conversation last_modified_at to detect changes,
// so only modified conversations are fully parsed.
func (e *Engine) syncWarp(
	ctx context.Context,
) []pendingWrite {
	var allPending []pendingWrite
	for _, dir := range e.agentDirs[parser.AgentWarp] {
		if ctx.Err() != nil {
			break
		}
		if dir == "" {
			continue
		}
		allPending = append(
			allPending, e.syncOneWarp(ctx, dir)...,
		)
	}
	return allPending
}

// syncOneWarp handles a single Warp directory.
func (e *Engine) syncOneWarp(
	ctx context.Context, dir string,
) []pendingWrite {
	dbPath := parser.FindWarpDBPath(dir)
	if dbPath == "" {
		return nil
	}

	metas, err := parser.ListWarpSessionMeta(dbPath)
	if err != nil {
		log.Printf("sync warp: %v", err)
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
	for _, cid := range changed {
		if ctx.Err() != nil {
			break
		}
		sess, msgs, err := parser.ParseWarpSession(
			dbPath, cid, e.machine,
		)
		if err != nil {
			log.Printf(
				"warp conversation %s: %v", cid, err,
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

// syncSingleWarp re-syncs a single Warp conversation.
func (e *Engine) syncSingleWarp(
	sessionID string,
) error {
	rawID := strings.TrimPrefix(sessionID, "warp:")

	var lastErr error
	for _, dir := range e.agentDirs[parser.AgentWarp] {
		if dir == "" {
			continue
		}
		dbPath := parser.FindWarpDBPath(dir)
		if dbPath == "" {
			continue
		}
		sess, msgs, err := parser.ParseWarpSession(
			dbPath, rawID, e.machine,
		)
		if err != nil {
			lastErr = err
			continue
		}
		if sess == nil {
			continue
		}
		if err := e.writeSessionFull(
			pendingWrite{sess: *sess, msgs: msgs},
		); err != nil && !errors.Is(err, db.ErrSessionExcluded) {
			return fmt.Errorf("write session %s: %w",
				sess.ID, err)
		}
		return nil
	}

	if len(e.agentDirs[parser.AgentWarp]) == 0 {
		return fmt.Errorf("warp dir not configured")
	}
	if lastErr != nil {
		return fmt.Errorf(
			"warp session %s: %w", sessionID, lastErr,
		)
	}
	return fmt.Errorf("warp session %s not found", sessionID)
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
			ResultEvents:      convertToolResultEvents(tc.ResultEvents),
		}
	}
	return calls
}

func convertToolResultEvents(
	parsed []parser.ParsedToolResultEvent,
) []db.ToolResultEvent {
	if len(parsed) == 0 {
		return nil
	}
	events := make([]db.ToolResultEvent, len(parsed))
	for i, ev := range parsed {
		events[i] = db.ToolResultEvent{
			ToolUseID:         ev.ToolUseID,
			AgentID:           ev.AgentID,
			SubagentSessionID: ev.SubagentSessionID,
			Source:            ev.Source,
			Status:            ev.Status,
			Content:           ev.Content,
			ContentLength:     len(ev.Content),
			Timestamp:         timeutil.Format(ev.Timestamp),
			EventIndex:        i,
		}
	}
	return events
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
	pairToolResultEventSummaries(msgs, blocked)
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

func pairToolResultEventSummaries(
	msgs []db.Message, blocked map[string]bool,
) {
	for i := range msgs {
		for j := range msgs[i].ToolCalls {
			tc := &msgs[i].ToolCalls[j]
			if len(tc.ResultEvents) == 0 {
				continue
			}
			summary := summarizeToolResultEvents(tc.ResultEvents)
			tc.ResultContentLength = len(summary)
			if blocked[tc.Category] {
				tc.ResultContent = ""
				tc.ResultEvents = nil
				continue
			}
			tc.ResultContent = summary
		}
	}
}

func summarizeToolResultEvents(
	events []db.ToolResultEvent,
) string {
	if len(events) == 0 {
		return ""
	}
	type agentSummary struct {
		order   int
		content string
	}
	latestByAgent := map[string]agentSummary{}
	orderedAgents := make([]string, 0, len(events))
	lastAnon := ""
	allHaveAgentID := true
	for _, ev := range events {
		if strings.TrimSpace(ev.Content) == "" {
			continue
		}
		agentID := strings.TrimSpace(ev.AgentID)
		if agentID == "" {
			allHaveAgentID = false
			lastAnon = ev.Content
			continue
		}
		if _, ok := latestByAgent[agentID]; !ok {
			latestByAgent[agentID] = agentSummary{
				order:   len(orderedAgents),
				content: ev.Content,
			}
			orderedAgents = append(orderedAgents, agentID)
			continue
		}
		entry := latestByAgent[agentID]
		entry.content = ev.Content
		latestByAgent[agentID] = entry
	}
	if len(latestByAgent) <= 1 {
		if len(latestByAgent) == 1 {
			summary := latestByAgent[orderedAgents[0]].content
			if lastAnon != "" {
				return summary + "\n\n" + lastAnon
			}
			return summary
		}
		return lastAnon
	}
	parts := make([]string, 0, len(orderedAgents))
	for _, agentID := range orderedAgents {
		parts = append(parts, agentID+":\n"+latestByAgent[agentID].content)
	}
	if !allHaveAgentID && lastAnon != "" {
		parts = append(parts, lastAnon)
	}
	return strings.Join(parts, "\n\n")
}

// emit fires a refresh event if an emitter is wired. Safe to call
// with a nil emitter.
func (e *Engine) emit(scope string) {
	if e.emitter != nil {
		e.emitter.Emit(scope)
	}
}
