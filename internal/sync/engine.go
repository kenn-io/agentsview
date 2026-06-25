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
	"slices"
	"strings"
	gosync "sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/timeutil"
)

const (
	batchSize  = 100
	maxWorkers = 8
)

type syncWriteMode int

const (
	syncWriteDefault syncWriteMode = iota
	syncWriteBulk
)

var errSessionPreserved = errors.New("session preserved")

func isIntentionalSessionSkip(err error) bool {
	return errors.Is(err, db.ErrSessionExcluded) ||
		errors.Is(err, db.ErrSessionTrashed)
}

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
	// ProviderFactories and ProviderMigrationModes let stacked provider
	// migration branches opt concrete providers into side-effect-free
	// caller-level shadow observation before provider writes become
	// authoritative. Nil uses the parser package registry/manifest.
	ProviderFactories      []parser.ProviderFactory
	ProviderMigrationModes map[parser.AgentType]parser.ProviderMigrationMode
	// ProviderShadowRecorder receives serialized shadow observations.
	// Nil logs only provider errors or mismatches.
	ProviderShadowRecorder func(ProviderShadowComparison)
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
	currentProgress         *Progress
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
	ephemeral              bool
	idPrefix               string
	pathRewriter           func(string) string
	emitter                Emitter
	providerFactories      map[parser.AgentType]parser.ProviderFactory
	providerMigrationModes map[parser.AgentType]parser.ProviderMigrationMode
	providerShadowMu       gosync.Mutex
	providerShadowRecorder func(ProviderShadowComparison)

	// forceParse disables every stored-state skip (skip cache,
	// size/mtime/data_version checks, incremental JSONL deltas) so
	// parse-diff fully re-parses every discovered file. Normal sync
	// never sets it; behavior must be identical when false.
	forceParse bool

	// phaseStats accumulates per-phase wall-clock time inside the bulk
	// write path. Exposed via PhaseStats() so a CLI driver can log the
	// totals after a sync pass completes.
	phaseStats PhaseStats
}

// PhaseStats returns the engine's phase counter. The values reflect only
// the most recent sync pass; callers should read after SyncAll/ResyncAll
// returns.
func (e *Engine) PhaseStats() *PhaseStats { return &e.phaseStats }

// refuseWriteInForceParse guards the public sync entrypoints against an
// engine created by NewDiffEngine, whose forceParse mode exists purely
// for report-only re-parsing. Such an engine is also Ephemeral, so a
// write would persist nothing useful, but it would still rewrite or
// re-derive archive rows -- exactly what the report-only contract
// promises not to do. Rather than widen the read-only surface into a
// separate interface (which would change NewDiffEngine's return type and
// break ParseDiff callers), the write entrypoints refuse and log when
// forceParse is set. A real sync engine never sets forceParse, so this
// is a no-op for every production caller.
//
// It returns true when the caller must abort. op names the refused
// entrypoint for the log line.
func (e *Engine) refuseWriteInForceParse(op string) bool {
	if !e.forceParse {
		return false
	}
	log.Printf(
		"sync: refusing %s on a report-only (parse-diff) engine; "+
			"forceParse engines never write", op,
	)
	return true
}

// codexExecMigrationKey is the pg_sync_state flag that
// records whether the one-time cleanup of legacy codex_exec
// skip cache entries has already run on this database.
const codexExecMigrationKey = "codex_exec_legacy_migration_v1"

// visualStudioCopilotSkipMigrationKey is the pg_sync_state flag
// that records whether the one-time cleanup of Visual Studio
// Copilot skip cache entries has already run on this database.
// Older builds cached trace read/scan errors keyed by an
// unchanged mtime, which would otherwise suppress retries after
// upgrading to the non-cacheable read-error behavior.
const visualStudioCopilotSkipMigrationKey = "visualstudio_copilot_skip_migration_v1"

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
		migrateVisualStudioCopilotSkips(database, skipCache)
	}

	dirs := make(map[parser.AgentType][]string, len(cfg.AgentDirs))
	for k, v := range cfg.AgentDirs {
		dirs[k] = append([]string(nil), v...)
	}
	providerFactories := parser.ProviderFactories()
	if cfg.ProviderFactories != nil {
		providerFactories = cfg.ProviderFactories
	}
	providerModes := parser.ProviderMigrationModes()
	if cfg.ProviderMigrationModes != nil {
		maps.Copy(providerModes, cfg.ProviderMigrationModes)
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
		providerFactories:       providerFactoryMap(providerFactories),
		providerMigrationModes:  providerModes,
		providerShadowRecorder:  cfg.ProviderShadowRecorder,
	}
}

func providerFactoryMap(
	factories []parser.ProviderFactory,
) map[parser.AgentType]parser.ProviderFactory {
	out := make(map[parser.AgentType]parser.ProviderFactory, len(factories))
	for _, factory := range factories {
		def := factory.Definition()
		out[def.Type] = factory
	}
	return out
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

// migrateVisualStudioCopilotSkips removes skip cache entries for
// Visual Studio Copilot trace files. Older builds cached trace
// read/scan errors keyed by mtime, so an unchanged unreadable
// file would be skipped on later syncs instead of retried. The
// scrub clears both physical trace paths and
// <traceFile>#<conversationID> virtual paths; successfully synced
// conversations are re-cached on the next sync, while read errors
// surface again because they are no longer cacheable.
//
// The scrub runs once per database: a pg_sync_state flag is set
// after the first successful pass. It mirrors
// migrateLegacyCodexExecSkips: the cleaned snapshot is persisted
// through the atomic ReplaceSkippedFiles before the in-memory map
// and done flag are updated, so a partial failure is retried on
// the next startup rather than being falsely marked complete.
func migrateVisualStudioCopilotSkips(
	database *db.DB, skipCache map[string]int64,
) {
	done, err := database.GetSyncState(visualStudioCopilotSkipMigrationKey)
	if err != nil {
		log.Printf("visual studio copilot skip migration: %v", err)
		return
	}
	if done != "" {
		return
	}

	cleaned := make(map[string]int64, len(skipCache))
	var stale []string
	for path, mtime := range skipCache {
		if IsVisualStudioCopilotSkipPath(path) {
			stale = append(stale, path)
			continue
		}
		cleaned[path] = mtime
	}

	if len(stale) > 0 {
		if err := database.ReplaceSkippedFiles(cleaned); err != nil {
			log.Printf(
				"visual studio copilot skip migration: "+
					"persist cleaned skip cache: %v",
				err,
			)
			return
		}
		for _, p := range stale {
			delete(skipCache, p)
		}
		log.Printf(
			"visual studio copilot skip migration: cleared %d skip entries",
			len(stale),
		)
	}

	if err := database.SetSyncState(
		visualStudioCopilotSkipMigrationKey, "done",
	); err != nil {
		log.Printf(
			"visual studio copilot skip migration: set flag: %v", err,
		)
	}
}

// IsVisualStudioCopilotSkipPath reports whether a skip cache key
// belongs to a Visual Studio Copilot trace: either a physical
// trace file or a <traceFile>#<conversationID> virtual path. It
// is shared with remote sync so both the local and remote skip
// migrations classify paths identically.
func IsVisualStudioCopilotSkipPath(path string) bool {
	if parser.IsVisualStudioCopilotTraceFile(path) {
		return true
	}
	_, _, ok := parser.ParseVisualStudioCopilotVirtualPath(path)
	return ok
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

// CurrentProgress returns the most recent in-flight sync progress.
func (e *Engine) CurrentProgress() (Progress, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.currentProgress == nil {
		return Progress{}, false
	}
	return *e.currentProgress, true
}

func (e *Engine) reportProgress(
	onProgress ProgressFunc, p Progress,
) {
	e.mu.Lock()
	e.currentProgress = &p
	e.mu.Unlock()
	if onProgress != nil {
		onProgress(p)
	}
}

func (e *Engine) clearCurrentProgress() {
	e.mu.Lock()
	e.currentProgress = nil
	e.mu.Unlock()
}

// Machine returns the machine name this engine writes on sessions.
func (e *Engine) Machine() string {
	if e == nil {
		return ""
	}
	return e.machine
}

type syncJob struct {
	processResult
	path string
}

func (j syncJob) skipCacheKey() string {
	return j.processResult.skipCacheKey(j.path)
}

func (r processResult) skipCacheKey(path string) string {
	if r.cacheKey != "" {
		return r.cacheKey
	}
	return path
}

// SyncPaths syncs only the specified changed file paths
// instead of discovering and hashing all session files.
// Paths that don't match known session file patterns are
// silently ignored.
func (e *Engine) SyncPaths(paths []string) {
	if e.refuseWriteInForceParse("SyncPaths") {
		return
	}
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
	defer e.clearCurrentProgress()

	results := e.startWorkers(context.Background(), files)
	stats = e.collectAndBatch(
		context.Background(), results, len(files), len(files), nil,
		syncWriteDefault,
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
	seen := make(map[string]int, len(paths))
	files := make([]parser.DiscoveredFile, 0, len(paths))
	for _, p := range paths {
		// Antigravity sidecar events map to potentially several
		// session sources and must classify even when the event
		// path was deleted, so they bypass classifyOnePath.
		dfs := e.classifyAntigravitySidecarPath(p)
		if len(dfs) == 0 {
			dfs = e.classifyCodexIndexPath(p)
		}
		if len(dfs) == 0 {
			if df, ok := e.classifyOnePath(
				p, geminiProjectsByDir,
			); ok {
				dfs = []parser.DiscoveredFile{df}
			}
		}
		dfs = append(dfs, e.classifyProviderChangedPath(p)...)
		for _, df := range dfs {
			key := string(df.Agent) + "\x00" + df.Path
			if idx, ok := seen[key]; ok {
				files[idx] = mergeChangedPathDiscoveredFile(files[idx], df)
				continue
			}
			seen[key] = len(files)
			files = append(files, df)
		}
	}
	files = e.expandClaudeDuplicateCandidates(files)
	files = dedupeDiscoveredFiles(files)
	return e.dedupeClaudeDiscoveredFiles(files)
}

func mergeChangedPathDiscoveredFile(
	current parser.DiscoveredFile,
	next parser.DiscoveredFile,
) parser.DiscoveredFile {
	current.ForceParse = current.ForceParse || next.ForceParse
	current.ProviderProcess = current.ProviderProcess || next.ProviderProcess
	if current.Project == "" {
		current.Project = next.Project
	}
	if current.ProviderSource == nil && next.ProviderSource != nil {
		current.ProviderSource = next.ProviderSource
	}
	return current
}

func (e *Engine) classifyProviderChangedPath(
	path string,
) []parser.DiscoveredFile {
	ctx := context.Background()
	eventKind := providerChangedPathEventKind(path)
	var files []parser.DiscoveredFile
	seen := map[string]struct{}{}

	agents := make([]parser.AgentType, 0, len(e.providerFactories))
	for agent := range e.providerFactories {
		agents = append(agents, agent)
	}
	slices.SortFunc(agents, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})

	for _, agentType := range agents {
		mode := e.providerMigrationModes[agentType]
		switch mode {
		case parser.ProviderMigrationShadowCompare,
			parser.ProviderMigrationProviderAuthoritative:
		default:
			continue
		}
		roots := e.agentDirs[agentType]
		if len(roots) == 0 {
			continue
		}
		factory, ok := e.providerFactories[agentType]
		if !ok || factory == nil {
			continue
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots:   roots,
			Machine: e.machine,
		})
		def := provider.Definition()
		watchRoots := providerChangedPathWatchRoots(ctx, provider, roots)
		for _, watchRoot := range watchRoots {
			storedSourcePaths, err := e.db.ListStoredSourcePathHints(
				string(def.Type),
				[]string{watchRoot},
			)
			if err != nil {
				log.Printf(
					"%s provider changed-path stored hints: %v",
					def.Type, err,
				)
			}
			sources, err := provider.SourcesForChangedPath(
				ctx,
				parser.ChangedPathRequest{
					Path:              path,
					EventKind:         eventKind,
					WatchRoot:         watchRoot,
					StoredSourcePaths: storedSourcePaths,
				},
			)
			if err != nil {
				if !errors.Is(err, parser.ErrUnsupportedProviderFeature) {
					log.Printf(
						"%s provider changed-path classification: %v",
						def.Type, err,
					)
				}
				continue
			}
			for _, source := range sources {
				sourcePath := providerDiscoveredPath(source)
				if sourcePath == "" {
					continue
				}
				agent := source.Provider
				if agent == "" {
					agent = def.Type
				}
				key := string(agent) + "\x00" + sourcePath
				if _, ok := seen[key]; ok {
					continue
				}
				if eventKind == "remove" &&
					filepath.Clean(sourcePath) == filepath.Clean(path) &&
					!parser.IsRegularFile(sourcePath) &&
					!providerDeletedPhysicalSQLiteSource(agent, sourcePath) {
					continue
				}
				seen[key] = struct{}{}
				sourceCopy := source
				files = append(files, parser.DiscoveredFile{
					Path:            sourcePath,
					Project:         source.ProjectHint,
					Agent:           agent,
					ForceParse:      providerChangedPathForceParse(agent, sourcePath, path, eventKind, mode),
					ProviderSource:  &sourceCopy,
					ProviderProcess: mode == parser.ProviderMigrationProviderAuthoritative,
				})
			}
		}
	}
	return files
}

func providerChangedPathWatchRoots(
	ctx context.Context,
	provider parser.Provider,
	roots []string,
) []string {
	plan, err := provider.WatchPlan(ctx)
	if err == nil && len(plan.Roots) > 0 {
		watchRoots := make([]string, 0, len(plan.Roots))
		seen := make(map[string]struct{}, len(plan.Roots))
		for _, root := range plan.Roots {
			path := filepath.Clean(root.Path)
			if path == "" || path == "." {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			watchRoots = append(watchRoots, path)
		}
		if len(watchRoots) > 0 {
			return watchRoots
		}
	}
	watchRoots := make([]string, 0, len(roots))
	for _, root := range roots {
		root = filepath.Clean(root)
		if root == "" || root == "." {
			continue
		}
		watchRoots = append(watchRoots, root)
	}
	return watchRoots
}

func providerChangedPathForceParse(
	agent parser.AgentType,
	sourcePath string,
	eventPath string,
	eventKind string,
	mode parser.ProviderMigrationMode,
) bool {
	if mode != parser.ProviderMigrationProviderAuthoritative {
		return true
	}
	if filepath.Clean(sourcePath) != filepath.Clean(eventPath) &&
		!providerVirtualSourceBackedByEvent(sourcePath, eventPath) {
		return true
	}
	return eventKind == "remove" &&
		providerDeletedPhysicalSQLiteSource(agent, sourcePath)
}

func providerVirtualSourceBackedByEvent(sourcePath, eventPath string) bool {
	idx := strings.LastIndex(sourcePath, "#")
	if idx < 0 {
		return false
	}
	dbPath := filepath.Clean(sourcePath[:idx])
	eventPath = filepath.Clean(eventPath)
	return eventPath == dbPath ||
		eventPath == dbPath+"-wal" ||
		eventPath == dbPath+"-shm"
}

func providerChangedPathEventKind(path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Lstat(path); err != nil && os.IsNotExist(err) {
		return "remove"
	}
	return "write"
}

func providerDiscoveredPath(source parser.SourceRef) string {
	for _, path := range []string{
		source.DisplayPath,
		source.FingerprintKey,
		source.Key,
	} {
		if path != "" {
			return path
		}
	}
	return ""
}

func providerDeletedPhysicalSQLiteSource(
	agent parser.AgentType, path string,
) bool {
	switch agent {
	case parser.AgentZed:
		return filepath.Base(path) == "threads.db"
	case parser.AgentShelley:
		return filepath.Base(path) == shelleyDBFile
	default:
		return false
	}
}

func dedupeDiscoveredFiles(
	files []parser.DiscoveredFile,
) []parser.DiscoveredFile {
	if len(files) < 2 {
		return files
	}

	bestByKey := make(map[string]parser.DiscoveredFile, len(files))
	for _, file := range files {
		key := discoveredFileKey(file)
		if current, ok := bestByKey[key]; ok {
			if preferDiscoveredFile(file, current) {
				bestByKey[key] = file
			}
			continue
		}
		bestByKey[key] = file
	}

	out := make([]parser.DiscoveredFile, 0, len(bestByKey))
	for _, file := range files {
		key := discoveredFileKey(file)
		chosen, ok := bestByKey[key]
		if !ok || chosen.Path != file.Path || chosen.Agent != file.Agent {
			continue
		}
		out = append(out, file)
		delete(bestByKey, key)
	}
	return out
}

func discoveredFileKey(file parser.DiscoveredFile) string {
	if file.Agent == parser.AgentCodex {
		if id := parser.CodexSessionUUIDFromFilename(filepath.Base(file.Path)); id != "" {
			return string(file.Agent) + "\x00" + id
		}
	}
	return string(file.Agent) + "\x00" + file.Path
}

func preferDiscoveredFile(
	candidate, current parser.DiscoveredFile,
) bool {
	if candidate.Agent == parser.AgentCodex && current.Agent == parser.AgentCodex {
		candLayout := codexLayoutForPath(candidate.Path)
		currLayout := codexLayoutForPath(current.Path)
		if candLayout != currLayout {
			return candLayout == parser.CodexLayoutDated
		}
	}
	return false
}

func (e *Engine) expandClaudeDuplicateCandidates(
	files []parser.DiscoveredFile,
) []parser.DiscoveredFile {
	sessionIDs := make(map[string]struct{})
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		seen[string(file.Agent)+"\x00"+file.Path] = struct{}{}
		if file.Agent != parser.AgentClaude {
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			continue
		}
		sessionIDs[sessionID] = struct{}{}
	}
	if len(sessionIDs) == 0 {
		return files
	}

	out := files
	for _, claudeDir := range e.agentDirs[parser.AgentClaude] {
		for _, candidate := range parser.DiscoverClaudeProjects(claudeDir) {
			sessionID := claudeSessionIDFromPath(candidate.Path)
			if _, ok := sessionIDs[sessionID]; !ok {
				continue
			}
			key := string(candidate.Agent) + "\x00" + candidate.Path
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, candidate)
		}
	}
	return out
}

func codexLayoutForPath(path string) parser.CodexLayout {
	path = filepath.Clean(path)
	name := filepath.Base(path)
	if parser.CodexSessionUUIDFromFilename(name) == "" {
		return parser.CodexLayoutUnknown
	}
	day := filepath.Base(filepath.Dir(path))
	month := filepath.Base(filepath.Dir(filepath.Dir(path)))
	year := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	if parser.IsDigits(day) && parser.IsDigits(month) && parser.IsDigits(year) {
		return parser.CodexLayoutDated
	}
	return parser.CodexLayoutArchivedFlat
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

// classifyContainerPath runs the container- and SQLite-style classifiers that
// resolve a path whether or not it currently exists on disk (OpenCode-format
// stores, Kiro, Zed, Shelley, and Vibe). Split out of classifyOnePath to keep
// that function within NilAway's per-function CFG-block limit.
func (e *Engine) classifyContainerPath(
	path string, pathExists bool,
) (parser.DiscoveredFile, bool) {
	if df, ok := e.classifyOpenCodeFormatPath(
		parser.AgentOpenCode, path, pathExists,
	); ok {
		return df, true
	}
	if df, ok := e.classifyOpenCodeFormatPath(
		parser.AgentKilo, path, pathExists,
	); ok {
		return df, true
	}
	if df, ok := e.classifyOpenCodeFormatPath(
		parser.AgentMiMoCode, path, pathExists,
	); ok {
		return df, true
	}
	if df, ok := e.classifyKiroSQLitePath(path); ok {
		return df, true
	}
	if df, ok := e.classifyZedSQLitePath(path); ok {
		return df, true
	}
	if df, ok := e.classifyShelleySQLitePath(path); ok {
		return df, true
	}
	if df, ok := e.classifyVibePath(path); ok {
		return df, true
	}
	return parser.DiscoveredFile{}, false
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

	if df, ok := e.classifyContainerPath(path, pathExists); ok {
		return df, true
	}
	// Reasonix sidecar delete events arrive after .jsonl.meta no longer
	// exists; classify them against the sibling transcript before the
	// generic missing-path guard.
	if strings.HasSuffix(path, ".jsonl.meta") {
		if df, ok := e.classifyReasonixPath(path); ok {
			return df, true
		}
	}
	if !pathExists {
		return parser.DiscoveredFile{}, false
	}
	if df, ok := e.classifyReasonixPath(path); ok {
		return df, true
	}

	// Claude: <claudeDir>/<project>/<session>.jsonl
	//     or: <claudeDir>/<project>/<session>/subagents/**/agent-<id>.jsonl
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

			// Subagent: project/session/subagents/**/agent-*.jsonl
			if len(parts) >= 4 && parts[2] == "subagents" {
				stem := strings.TrimSuffix(
					parts[len(parts)-1], ".jsonl",
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

	// Cowork: <coworkDir>/<orgId>/<workspaceId>/local_<uuid>/.claude/
	//   projects/<enc>/<cliSessionId>.jsonl (transcript), or the sibling
	//   local_<uuid>.json metadata file (resolves to its transcript).
	for _, coworkDir := range e.agentDirs[parser.AgentCowork] {
		if coworkDir == "" {
			continue
		}
		if transcript, ok := parser.ClassifyCoworkPath(
			coworkDir, path,
		); ok {
			return parser.DiscoveredFile{
				Path:  transcript,
				Agent: parser.AgentCowork,
			}, true
		}
	}

	// Codex: either <codexDir>/<year>/<month>/<day>/<file>.jsonl
	// or <codexDir>/<file>.jsonl for archived sessions.
	for _, codexDir := range e.agentDirs[parser.AgentCodex] {
		if codexDir == "" {
			continue
		}
		if _, _, ok := parser.CodexSessionPathInfo(codexDir, path); ok {
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
				if parts[1] == "events.jsonl" {
					return parser.DiscoveredFile{
						Path:  path,
						Agent: parser.AgentCopilot,
					}, true
				}
				// workspace.yaml changes should trigger a re-parse
				// of the sibling events.jsonl.
				if parts[1] == "workspace.yaml" {
					eventsPath := filepath.Join(
						stateDir, parts[0], "events.jsonl",
					)
					if _, err := os.Stat(eventsPath); err == nil {
						return parser.DiscoveredFile{
							Path:  eventsPath,
							Agent: parser.AgentCopilot,
						}, true
					}
				}
				continue
			default:
				continue
			}
		}
	}

	// Gemini: <geminiDir>/tmp/<dir>/chats/session-*.json(.l)
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
				(!strings.HasSuffix(name, ".json") &&
					!strings.HasSuffix(name, ".jsonl")) {
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

	// Kimi: <kimiDir>/<project>/<session>/wire.jsonl              (legacy)
	//    or <kimiDir>/<project>/<session>/agents/<agent>/wire.jsonl (.kimi-code)
	// Components that cannot round-trip through the ':'-delimited
	// session ID (per IsValidSessionID) are left unclassified so they
	// are never imported in a non-resyncable state.
	for _, kimiDir := range e.agentDirs[parser.AgentKimi] {
		if kimiDir == "" {
			continue
		}
		if rel, ok := isUnder(kimiDir, path); ok {
			parts := strings.Split(rel, sep)
			switch {
			case len(parts) == 3 && parts[2] == "wire.jsonl" &&
				parser.IsValidSessionID(parts[0]) &&
				parser.IsValidSessionID(parts[1]):
				return parser.DiscoveredFile{
					Path:    path,
					Project: parser.DecodeKimiProjectDir(parts[0]),
					Agent:   parser.AgentKimi,
				}, true
			case len(parts) == 5 && parts[2] == "agents" &&
				parts[4] == "wire.jsonl" &&
				parser.IsValidSessionID(parts[0]) &&
				parser.IsValidSessionID(parts[1]) &&
				parser.IsValidSessionID(parts[3]):
				return parser.DiscoveredFile{
					Path:    path,
					Project: parser.DecodeKimiProjectDir(parts[0]),
					Agent:   parser.AgentKimi,
				}, true
			}
		}
	}

	// QwenPaw: <qwenpawDir>/<workspace>/sessions/<name>.json
	//       or <qwenpawDir>/<workspace>/sessions/<subdir>/<name>.json
	for _, qwenpawDir := range e.agentDirs[parser.AgentQwenPaw] {
		if qwenpawDir == "" {
			continue
		}
		if rel, ok := isUnder(qwenpawDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) < 3 || parts[1] != "sessions" {
				continue
			}
			if !parser.IsValidQwenPawIDPart(parts[0]) {
				continue
			}
			var stem string
			switch {
			case len(parts) == 3:
				stem = parts[2]
			case len(parts) == 4 && !strings.HasPrefix(parts[2], "."):
				if !parser.IsValidQwenPawIDPart(parts[2]) {
					continue
				}
				stem = parts[3]
			default:
				continue
			}
			sessionID, ok := strings.CutSuffix(stem, ".json")
			if !ok || !parser.IsValidQwenPawIDPart(sessionID) {
				continue
			}
			return parser.DiscoveredFile{
				Path:    path,
				Project: parts[0],
				Agent:   parser.AgentQwenPaw,
			}, true
		}
	}

	// WorkBuddy: <workbuddyDir>/<project>/<session>.jsonl
	//     or: <workbuddyDir>/<project>/<session>/subagents/*.jsonl
	for _, workBuddyDir := range e.agentDirs[parser.AgentWorkBuddy] {
		if workBuddyDir == "" {
			continue
		}
		if rel, ok := isUnder(workBuddyDir, path); ok {
			if !strings.HasSuffix(path, ".jsonl") {
				continue
			}
			parts := strings.Split(rel, sep)
			if len(parts) == 2 {
				stem := strings.TrimSuffix(parts[1], ".jsonl")
				if !parser.IsValidSessionID(stem) {
					continue
				}
				return parser.DiscoveredFile{
					Path:    path,
					Project: parts[0],
					Agent:   parser.AgentWorkBuddy,
				}, true
			}
			if len(parts) == 4 && parts[2] == "subagents" {
				return parser.DiscoveredFile{
					Path:    path,
					Project: parts[0],
					Agent:   parser.AgentWorkBuddy,
				}, true
			}
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

	// DeepSeek TUI / CodeWhale: <sessionsDir>/<session>.json
	for _, dsDir := range e.agentDirs[parser.AgentDeepSeekTUI] {
		if dsDir == "" {
			continue
		}
		if rel, ok := isUnder(dsDir, path); ok {
			if strings.Count(rel, sep) != 0 {
				continue
			}
			name := filepath.Base(rel)
			if name == "latest.json" || name == "offline_queue.json" {
				continue
			}
			sessionID, ok := strings.CutSuffix(name, ".json")
			if !ok || !parser.IsValidSessionID(sessionID) {
				continue
			}
			return parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentDeepSeekTUI,
			}, true
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

	// Visual Studio Copilot: <traces>/*_VSGitHubCopilot_traces.jsonl
	if df, ok := e.classifyVisualStudioCopilotPath(path, sep); ok {
		return df, true
	}

	// Pi/OMP: <sessionsDir>/<encoded-cwd>/<session>.jsonl
	for _, agent := range []parser.AgentType{parser.AgentPi, parser.AgentOMP} {
		for _, piDir := range e.agentDirs[agent] {
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
					Agent: agent,
					// Project left empty; parser derives from header cwd.
				}, true
			}
		}
	}

	// Qwen: <qwenProjectsDir>/<encoded-project>/chats/<session>.jsonl
	for _, qwenDir := range e.agentDirs[parser.AgentQwen] {
		if qwenDir == "" {
			continue
		}
		if rel, ok := isUnder(qwenDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 3 || parts[1] != "chats" {
				continue
			}
			sessionID, ok := strings.CutSuffix(parts[2], ".jsonl")
			if !ok || !parser.IsValidSessionID(sessionID) {
				continue
			}
			return parser.DiscoveredFile{
				Path:    path,
				Project: parser.GetProjectName(parts[0]),
				Agent:   parser.AgentQwen,
			}, true
		}
	}

	// gptme: <logsDir>/<session-dir>/conversation.jsonl
	for _, gptmeDir := range e.agentDirs[parser.AgentGptme] {
		if gptmeDir == "" {
			continue
		}
		if rel, ok := isUnder(gptmeDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 2 || parts[1] != "conversation.jsonl" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentGptme,
			}, true
		}
	}

	if df, ok := e.classifyAiderPath(path); ok {
		return df, true
	}

	// Command Code: <projectsDir>/<slugified-cwd>/<session>.jsonl
	for _, commandCodeDir := range e.agentDirs[parser.AgentCommandCode] {
		if commandCodeDir == "" {
			continue
		}
		if rel, ok := isUnder(commandCodeDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 2 {
				continue
			}
			if sessionID, ok := strings.CutSuffix(parts[1], ".meta.json"); ok {
				if !parser.IsValidSessionID(sessionID) {
					continue
				}
				jsonlPath := filepath.Join(commandCodeDir, parts[0], sessionID+".jsonl")
				if _, err := os.Stat(jsonlPath); err != nil {
					continue
				}
				return parser.DiscoveredFile{
					Path:    jsonlPath,
					Project: parser.NormalizeName(parts[0]),
					Agent:   parser.AgentCommandCode,
				}, true
			}
			if strings.HasSuffix(parts[1], ".checkpoints.jsonl") ||
				strings.HasSuffix(parts[1], ".prompts.jsonl") {
				continue
			}
			sessionID, ok := strings.CutSuffix(parts[1], ".jsonl")
			if !ok || !parser.IsValidSessionID(sessionID) {
				continue
			}
			return parser.DiscoveredFile{
				Path:    path,
				Project: parser.NormalizeName(parts[0]),
				Agent:   parser.AgentCommandCode,
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

	// Kiro CLI legacy: <kiroDir>/<uuid>.jsonl
	for _, kiroDir := range e.agentDirs[parser.AgentKiro] {
		if kiroDir == "" {
			continue
		}
		if rel, ok := isUnder(kiroDir, path); ok {
			if strings.Count(rel, sep) == 0 &&
				strings.HasSuffix(rel, ".jsonl") {
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentKiro,
				}, true
			}
		}
	}

	// QClaw: <qclawDir>/<agentId>/sessions/<sessionId>.jsonl
	//     or: <qclawDir>/<agentId>/sessions/<sessionId>.jsonl.<archiveSuffix>
	for _, qcDir := range e.agentDirs[parser.AgentQClaw] {
		if qcDir == "" {
			continue
		}
		if rel, ok := isUnder(qcDir, path); ok {
			parts := strings.Split(rel, sep)
			// Expect: <agentId>/sessions/<file>
			if len(parts) != 3 || parts[1] != "sessions" {
				continue
			}
			if !parser.IsValidSessionID(parts[0]) {
				continue
			}
			if !parser.IsQClawSessionFile(parts[2]) {
				continue
			}
			if !strings.HasSuffix(parts[2], ".jsonl") {
				sid := parser.QClawSessionID(parts[2])
				active := filepath.Join(
					qcDir, parts[0], "sessions",
					sid+".jsonl",
				)
				if _, err := os.Stat(active); err == nil {
					continue
				}
				best := parser.FindQClawSourceFile(
					qcDir, parts[0]+":"+sid,
				)
				if best != path {
					continue
				}
			}
			return parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentQClaw,
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

	// Antigravity IDE: <root>/conversations/<uuid>.db (+ -wal, -shm).
	// annotations/<uuid>.pbtxt and brain/<uuid>/* sidecar events are
	// handled in classifyPaths via classifyAntigravitySidecarPath,
	// which runs without the path-existence requirement above.
	for _, agDir := range e.agentDirs[parser.AgentAntigravity] {
		if agDir == "" {
			continue
		}
		rel, ok := isUnder(agDir, path)
		if !ok {
			continue
		}
		parts := strings.Split(rel, sep)
		if len(parts) != 2 || parts[0] != "conversations" {
			continue
		}
		name := strings.TrimSuffix(parts[1], "-wal")
		name = strings.TrimSuffix(name, "-shm")
		if !strings.HasSuffix(name, ".db") {
			continue
		}
		id := strings.TrimSuffix(name, ".db")
		if !parser.IsValidSessionID(id) {
			continue
		}
		return parser.DiscoveredFile{
			Path:  filepath.Join(agDir, "conversations", id+".db"),
			Agent: parser.AgentAntigravity,
		}, true
	}

	// Antigravity CLI: <root>/conversations/<uuid>.db or
	// <root>/conversations|implicit/<uuid>.pb (+ trajectory.json sidecars)
	for _, agDir := range e.agentDirs[parser.AgentAntigravityCLI] {
		if agDir == "" {
			continue
		}
		if rel, ok := isUnder(agDir, path); ok {
			parts := strings.Split(rel, sep)
			if len(parts) != 2 ||
				(parts[0] != "conversations" &&
					parts[0] != "implicit") {
				continue
			}
			name := parts[1]
			var sourcePath string
			var id string
			if strings.HasSuffix(name, ".pb") {
				sourcePath = path
				id = strings.TrimSuffix(name, ".pb")
			} else if strings.HasSuffix(name, ".db") ||
				strings.HasSuffix(name, ".db-wal") ||
				strings.HasSuffix(name, ".db-shm") {
				name = strings.TrimSuffix(name, "-wal")
				name = strings.TrimSuffix(name, "-shm")
				sourcePath = filepath.Join(agDir, parts[0], name)
				id = strings.TrimSuffix(name, ".db")
			} else if strings.HasSuffix(name, ".trajectory.json") {
				sourcePath = strings.TrimSuffix(path, ".trajectory.json") + ".pb"
				id = strings.TrimSuffix(name, ".trajectory.json")
			} else {
				continue
			}
			if !parser.IsValidSessionID(id) {
				continue
			}
			if parts[0] == "conversations" &&
				strings.HasSuffix(sourcePath, ".pb") {
				dbPath := filepath.Join(agDir, parts[0], id+".db")
				if _, err := os.Stat(dbPath); err == nil {
					sourcePath = dbPath
				}
			}
			if _, err := os.Stat(sourcePath); err != nil {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sourcePath,
				Agent: parser.AgentAntigravityCLI,
			}, true
		}
	}

	return parser.DiscoveredFile{}, false
}

// classifyVisualStudioCopilotPath matches a top-level Visual Studio Copilot
// trace file (<traces>/*_VSGitHubCopilot_traces.jsonl) under a configured
// trace directory. Trace files live directly in the directory, so nested
// paths are rejected. Split out of classifyOnePath to keep that function
// within NilAway's per-function size limit.
func (e *Engine) classifyVisualStudioCopilotPath(
	path, sep string,
) (parser.DiscoveredFile, bool) {
	if !parser.IsVisualStudioCopilotTraceFile(path) {
		return parser.DiscoveredFile{}, false
	}
	for _, vsDir := range e.agentDirs[parser.AgentVSCopilot] {
		if vsDir == "" {
			continue
		}
		rel, ok := isUnder(vsDir, path)
		if !ok {
			continue
		}
		if strings.Contains(rel, sep) {
			continue
		}
		return parser.DiscoveredFile{
			Path:    path,
			Project: "visualstudio",
			Agent:   parser.AgentVSCopilot,
		}, true
	}
	return parser.DiscoveredFile{}, false
}

// classifyAiderPath handles Aider's rootless chat-history layout:
//
//	<aiderRoot>/.../.aider.chat.history.md
//
// extracted from classifyOnePath to stay within nilaway CFG limits.
func (e *Engine) classifyAiderPath(
	path string,
) (parser.DiscoveredFile, bool) {
	if filepath.Base(path) != parser.AiderHistoryFileName() {
		return parser.DiscoveredFile{}, false
	}
	for _, aiderDir := range e.agentDirs[parser.AgentAider] {
		if aiderDir == "" {
			continue
		}
		if _, ok := isUnder(aiderDir, path); ok {
			return parser.DiscoveredFile{
				Path:  path,
				Agent: parser.AgentAider,
			}, true
		}
	}
	return parser.DiscoveredFile{}, false
}

// classifyVibePath handles Vibe's session directory layout:
//
//	<vibeDir>/session_<timestamp>_<uuid>/messages.jsonl
//	<vibeDir>/session_<timestamp>_<uuid>/meta.json
//
// meta.json changes route back to messages.jsonl because title, model,
// timestamps, and usage stats are sourced from the sidecar metadata file.
func (e *Engine) classifyVibePath(
	path string,
) (parser.DiscoveredFile, bool) {
	sep := string(filepath.Separator)
	for _, vibeDir := range e.agentDirs[parser.AgentVibe] {
		if vibeDir == "" {
			continue
		}
		rel, ok := isUnder(vibeDir, path)
		if !ok {
			continue
		}
		parts := strings.Split(rel, sep)
		if len(parts) != 2 || !strings.HasPrefix(parts[0], "session_") {
			continue
		}
		switch parts[1] {
		case "messages.jsonl":
			if _, err := os.Stat(path); err != nil {
				continue
			}
			return parser.DiscoveredFile{
				Path:    path,
				Project: parts[0],
				Agent:   parser.AgentVibe,
			}, true
		case "meta.json":
			messagesPath := filepath.Join(
				vibeDir, parts[0], "messages.jsonl",
			)
			if _, err := os.Stat(messagesPath); err == nil {
				return parser.DiscoveredFile{
					Path:    messagesPath,
					Project: parts[0],
					Agent:   parser.AgentVibe,
				}, true
			}
		}
	}
	return parser.DiscoveredFile{}, false
}

// classifyAntigravitySidecarPath maps Antigravity sidecar events --
// IDE annotations/<id>.pbtxt plus IDE and CLI brain/<id>/* artifacts
// -- to every session source file that renders them. A CLI storage
// UUID can hold both a conversation and an implicit session, so one
// brain event can affect two sources. The sidecar path itself may no
// longer exist (deletes must reparse the session too), so only the
// mapped source files are required to exist.
func (e *Engine) classifyAntigravitySidecarPath(
	path string,
) []parser.DiscoveredFile {
	if df, ok := e.classifyAntigravityIDESidecar(path); ok {
		return []parser.DiscoveredFile{df}
	}
	return e.classifyAntigravityCLIBrainPath(path)
}

func (e *Engine) classifyAntigravityIDESidecar(
	path string,
) (parser.DiscoveredFile, bool) {
	sep := string(filepath.Separator)
	for _, agDir := range e.agentDirs[parser.AgentAntigravity] {
		if agDir == "" {
			continue
		}
		rel, ok := isUnder(agDir, path)
		if !ok {
			continue
		}
		parts := strings.Split(rel, sep)
		var id string
		switch {
		case len(parts) == 2 && parts[0] == "annotations" &&
			strings.HasSuffix(parts[1], ".pbtxt"):
			id = strings.TrimSuffix(parts[1], ".pbtxt")
		case len(parts) == 3 && parts[0] == "brain":
			id = parts[1]
		default:
			continue
		}
		if !parser.IsValidSessionID(id) {
			continue
		}
		dbPath := filepath.Join(agDir, "conversations", id+".db")
		if _, err := os.Stat(dbPath); err != nil {
			continue
		}
		return parser.DiscoveredFile{
			Path:  dbPath,
			Agent: parser.AgentAntigravity,
		}, true
	}
	return parser.DiscoveredFile{}, false
}

func (e *Engine) classifyAntigravityCLIBrainPath(
	path string,
) []parser.DiscoveredFile {
	sep := string(filepath.Separator)
	for _, agDir := range e.agentDirs[parser.AgentAntigravityCLI] {
		if agDir == "" {
			continue
		}
		rel, ok := isUnder(agDir, path)
		if !ok {
			continue
		}
		parts := strings.Split(rel, sep)
		if len(parts) != 3 || parts[0] != "brain" {
			continue
		}
		id := parts[1]
		if !parser.IsValidSessionID(id) {
			continue
		}
		var out []parser.DiscoveredFile
		// Conversation session: prefer the SQLite source when both
		// old and new files exist, matching discovery.
		for _, src := range []string{
			filepath.Join(agDir, "conversations", id+".db"),
			filepath.Join(agDir, "conversations", id+".pb"),
		} {
			if _, err := os.Stat(src); err == nil {
				out = append(out, parser.DiscoveredFile{
					Path:  src,
					Agent: parser.AgentAntigravityCLI,
				})
				break
			}
		}
		// The implicit session is distinct from the conversation
		// session and renders the same brain artifacts.
		implicit := filepath.Join(agDir, "implicit", id+".pb")
		if _, err := os.Stat(implicit); err == nil {
			out = append(out, parser.DiscoveredFile{
				Path:  implicit,
				Agent: parser.AgentAntigravityCLI,
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// classifyOpenCodeFormatPath classifies a path under an OpenCode-format
// root (OpenCode, its Kilo fork, or MiMoCode), which share an on-disk
// layout and differ only in the SQLite filename, the storage/<subdir>
// holding session JSON, and the agent label. MiMoCode stores sessions
// under storage/session_diff; the session subdir is taken from the
// resolved source rather than assumed.
//
//	<dir>/storage/<sessionSubdir>/<project>/<session>.json
//	<dir>/storage/message/<session>/<message>.json
//	<dir>/storage/part/<message>/<part>.json
func (e *Engine) classifyOpenCodeFormatPath(
	agent parser.AgentType, path string, pathExists bool,
) (parser.DiscoveredFile, bool) {
	sep := string(filepath.Separator)
	dbName := openCodeFormatDBName(agent)

	for _, dir := range e.agentDirs[agent] {
		if dir == "" {
			continue
		}
		rel, ok := isUnder(dir, path)
		if !ok {
			continue
		}
		base := filepath.Base(rel)
		if rel == dbName || strings.HasPrefix(base, dbName+"-") {
			dbPath := filepath.Join(dir, dbName)
			if info, err := os.Stat(dbPath); err == nil &&
				!info.IsDir() {
				return parser.DiscoveredFile{
					Path:  dbPath,
					Agent: agent,
				}, true
			}
			continue
		}
		src := resolveOpenCodeFormatSource(agent, dir)
		if src.Mode != parser.OpenCodeSourceStorage {
			continue
		}
		sessionSubdir := filepath.Base(src.SessionRoot)
		parts := strings.Split(rel, sep)
		switch {
		case pathExists &&
			len(parts) == 4 &&
			parts[0] == "storage" &&
			parts[1] == sessionSubdir &&
			strings.HasSuffix(parts[3], ".json"):
			return parser.DiscoveredFile{
				Path:  path,
				Agent: agent,
			}, true
		case len(parts) == 4 &&
			parts[0] == "storage" &&
			parts[1] == "message" &&
			strings.HasSuffix(parts[3], ".json"):
			sessionPath := findOpenCodeFormatSourceFile(
				agent, dir, parts[2],
			)
			if sessionPath == "" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sessionPath,
				Agent: agent,
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
						dir, parts[2],
					)
			}
			if sessionID == "" {
				continue
			}
			sessionPath := findOpenCodeFormatSourceFile(
				agent, dir, sessionID,
			)
			if sessionPath == "" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sessionPath,
				Agent: agent,
			}, true
		case !pathExists &&
			len(parts) == 3 &&
			parts[0] == "storage" &&
			parts[1] == "message":
			sessionPath := findOpenCodeFormatSourceFile(
				agent, dir, parts[2],
			)
			if sessionPath == "" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sessionPath,
				Agent: agent,
			}, true
		case !pathExists &&
			len(parts) == 3 &&
			parts[0] == "storage" &&
			parts[1] == "part":
			sessionID := findOpenCodeStorageSessionIDByMessageID(
				dir, parts[2],
			)
			if sessionID == "" {
				continue
			}
			sessionPath := findOpenCodeFormatSourceFile(
				agent, dir, sessionID,
			)
			if sessionPath == "" {
				continue
			}
			return parser.DiscoveredFile{
				Path:  sessionPath,
				Agent: agent,
			}, true
		}
	}
	return parser.DiscoveredFile{}, false
}

func (e *Engine) classifyKiroSQLitePath(
	path string,
) (parser.DiscoveredFile, bool) {
	if dbPath, _, ok := parser.ParseKiroSQLiteVirtualPath(path); ok {
		for _, kiroDir := range e.agentDirs[parser.AgentKiro] {
			if _, under := isUnder(kiroDir, dbPath); under {
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentKiro,
				}, true
			}
		}
	}
	for _, kiroDir := range e.agentDirs[parser.AgentKiro] {
		if kiroDir == "" {
			continue
		}
		rel, ok := isUnder(kiroDir, path)
		if !ok {
			continue
		}
		base := filepath.Base(rel)
		if rel != "data.sqlite3" &&
			!strings.HasPrefix(base, "data.sqlite3-") {
			continue
		}
		if dbPath := parser.FindKiroSQLiteDBPath(kiroDir); dbPath != "" {
			return parser.DiscoveredFile{
				Path:  dbPath,
				Agent: parser.AgentKiro,
			}, true
		}
	}
	return parser.DiscoveredFile{}, false
}

func (e *Engine) classifyZedSQLitePath(
	path string,
) (parser.DiscoveredFile, bool) {
	// Virtual path: threads.db#<sessionID>
	if dbPath, _, ok := parser.ParseZedSQLiteVirtualPath(path); ok {
		for _, zedDir := range e.agentDirs[parser.AgentZed] {
			if _, under := isUnder(zedDir, dbPath); under {
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentZed,
				}, true
			}
		}
	}
	// Real path: threads/threads.db or WAL/SHM siblings.
	// Handled here (before the !pathExists guard) so that delete
	// and rename events on threads.db-wal / threads.db-shm are
	// not dropped when the sibling no longer exists on disk.
	zedDBRel := filepath.Join("threads", "threads.db")
	for _, zedDir := range e.agentDirs[parser.AgentZed] {
		if zedDir == "" {
			continue
		}
		rel, ok := isUnder(zedDir, path)
		if !ok {
			continue
		}
		base := filepath.Base(rel)
		if rel != zedDBRel && !strings.HasPrefix(base, "threads.db-") {
			continue
		}
		dbPath := filepath.Join(zedDir, zedDBRel)
		if fi, err := os.Stat(dbPath); err == nil && !fi.IsDir() {
			return parser.DiscoveredFile{
				Path:  dbPath,
				Agent: parser.AgentZed,
			}, true
		}
	}
	return parser.DiscoveredFile{}, false
}

const shelleyDBFile = "shelley.db"

// classifyShelleySQLitePath classifies a Shelley source path. Shelley
// stores every conversation in a single shelley.db under its config
// directory, so paths are either a virtual conversation path
// (shelley.db#<id>) or the real DB file and its WAL/SHM siblings.
func (e *Engine) classifyShelleySQLitePath(
	path string,
) (parser.DiscoveredFile, bool) {
	// Virtual path: shelley.db#<conversationID>
	if dbPath, _, ok := parser.ParseShelleyVirtualPath(path); ok {
		for _, dir := range e.agentDirs[parser.AgentShelley] {
			if _, under := isUnder(dir, dbPath); under {
				return parser.DiscoveredFile{
					Path:  path,
					Agent: parser.AgentShelley,
				}, true
			}
		}
	}
	// Real path: shelley.db or its WAL/SHM siblings. Handled here
	// (before the !pathExists guard) so that delete and rename events
	// on shelley.db-wal / shelley.db-shm are not dropped when the
	// sibling no longer exists on disk.
	for _, dir := range e.agentDirs[parser.AgentShelley] {
		if dir == "" {
			continue
		}
		rel, ok := isUnder(dir, path)
		if !ok {
			continue
		}
		base := filepath.Base(rel)
		if rel != shelleyDBFile && !strings.HasPrefix(base, shelleyDBFile+"-") {
			continue
		}
		dbPath := filepath.Join(dir, shelleyDBFile)
		if fi, err := os.Stat(dbPath); err == nil && !fi.IsDir() {
			return parser.DiscoveredFile{
				Path:  dbPath,
				Agent: parser.AgentShelley,
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
	if e.refuseWriteInForceParse("ResyncAll") {
		return SyncStats{}
	}
	e.syncMu.Lock()
	// Defers LIFO: Unlock runs before emit.
	defer func() {
		if stats.Synced > 0 {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()

	return e.resyncAllLocked(ctx, onProgress)
}

func (e *Engine) resyncAllLocked(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	reportResyncProgress := func(p Progress) {
		p.Resync = true
		if p.Phase == PhaseSyncing && p.Detail == "" {
			p.Detail = "Syncing sessions into rebuilt database"
		}
		e.reportProgress(onProgress, p)
	}
	reportResyncPhase := func(phase Phase, detail, hint string) {
		reportResyncProgress(Progress{
			Phase:  phase,
			Detail: detail,
			Hint:   hint,
		})
	}

	origDB := e.db
	origPath := origDB.Path()
	tempPath := origPath + resyncTempSuffix
	reportResyncPhase(
		PhasePreparingResync,
		"Preparing full resync",
		"",
	)

	// Snapshot old non-OpenCode-format file-backed session count
	// to detect empty-discovery. OpenCode-format agents are
	// excluded entirely because a root may legitimately fall back
	// between storage and SQLite sources across resyncs. Fail closed:
	// if we can't query, assume old DB has file-backed data
	// worth protecting.
	oldFileSessions, err := origDB.FileBackedSessionCount(
		context.Background(),
	)
	if err != nil {
		log.Printf("resync: get old file count: %v", err)
		oldFileSessions = 1
	} else {
		oldFileSessions -= e.countRootOpenCodeFormatSessions(
			origDB, parser.AgentOpenCode,
		)
		oldFileSessions -= e.countRootKiroSQLiteSessions(origDB)
		oldFileSessions -= e.countRootOpenCodeFormatSessions(
			origDB, parser.AgentKilo,
		)
		oldFileSessions -= e.countRootOpenCodeFormatSessions(
			origDB, parser.AgentMiMoCode,
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
	reportResyncPhase(
		PhasePreparingResync,
		"Opening temporary database",
		"",
	)
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
	reportResyncPhase(
		PhasePreparingResync,
		"Copying deletion state into temporary database",
		"",
	)
	if err := newDB.CopyExcludedSessionsFrom(origPath); err != nil {
		log.Printf("resync: pre-sync copy excluded sessions: %v", err)
		// Non-fatal: worst case, deleted sessions reappear.
	}
	trashedCopied := 0
	if n, err := newDB.CopyTrashedDataFrom(origPath); err != nil {
		log.Printf("resync: pre-sync copy trashed sessions: %v", err)
		// Non-fatal: worst case, trashed sessions are reparsed
		// and then re-marked as trashed by metadata copy.
	} else if n > 0 {
		trashedCopied = n
		log.Printf("resync: pre-sync copied %d trashed sessions", n)
	}
	// The temp DB is not swapped into production until the end,
	// so avoid per-row FTS trigger work during the bulk load and
	// rebuild the index once all message rows are final.
	ftsDropped := false
	if newDB.HasFTS() {
		tFTS := time.Now()
		reportResyncPhase(
			PhasePreparingResync,
			"Disabling temporary search index updates",
			"",
		)
		if err := newDB.DropFTS(); err != nil {
			log.Printf("resync: drop temp fts: %v", err)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			stats = SyncStats{
				Aborted: true,
				Warnings: []string{
					"resync failed: drop temp fts: " +
						err.Error(),
				},
			}
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return
		}
		ftsDropped = true
		log.Printf(
			"resync: drop temp fts: %s",
			time.Since(tFTS).Round(time.Millisecond),
		)
	}

	// 3. Point engine at newDB and sync into it.
	e.openCodeArchiveStore = origDB
	e.db = newDB
	stats = e.syncAllLocked(
		ctx, reportResyncProgress, time.Time{}, nil, syncWriteBulk, true,
	)
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
	preservedOnly := stats.Synced == 0 &&
		stats.TotalSessions > 0 &&
		stats.Failed == 0 &&
		(oldFileSessions == 0 || trashedCopied > 0)
	excludedOnly := stats.Synced == 0 &&
		stats.TotalSessions > 0 &&
		stats.Failed == 0 &&
		stats.parserExcludedFiles > 0 &&
		stats.filesOK == stats.parserExcludedFiles
	abortSwap := stats.Aborted ||
		emptyDiscovery ||
		(stats.Synced == 0 &&
			stats.TotalSessions > 0 &&
			!preservedOnly &&
			!excludedOnly) ||
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
	reportResyncPhase(
		PhaseCopyingMetadata,
		"Closing current database before final copy",
		"",
	)
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
	reportResyncPhase(
		PhaseCopyingMetadata,
		"Copying sync metadata",
		"",
	)
	if err := newDB.CopyExcludedSessionsFrom(origPath); err != nil {
		log.Printf("resync: post-sync copy excluded sessions: %v", err)
	}
	if err := newDB.PurgeExcludedSessions(); err != nil {
		log.Printf("resync: purge excluded sessions: %v", err)
	}
	if err := newDB.CopySyncStateFrom(origPath); err != nil {
		log.Printf("resync: copy sync state: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"sync state copy failed, aborting swap: "+err.Error(),
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

	// Copy insights into newDB from the quiesced old DB file.
	tInsights := time.Now()
	reportResyncPhase(
		PhaseCopyingMetadata,
		"Copying cached insights",
		"",
	)
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
	reportResyncPhase(
		PhaseCopyingOrphans,
		"Copying archived sessions",
		"",
	)
	orphaned, err := newDB.CopyOrphanedDataFromExcluding(
		origPath, stats.parserExcludedIDs,
	)
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
		reportResyncPhase(
			PhaseCopyingOrphans,
			"Relinking archived subagent sessions",
			"",
		)
		if err := newDB.LinkSubagentSessions(); err != nil {
			log.Printf("resync: relink subagent sessions: %v", err)
		}
	}

	// Merge user-managed data (display_name, deleted_at,
	// starred_sessions, pinned_messages) from the old DB
	// so renames, soft-deletes, stars, and pins survive.
	reportResyncPhase(
		PhaseCopyingMetadata,
		"Copying user-managed session metadata",
		"",
	)
	if err := newDB.CopySessionMetadataFrom(origPath); err != nil {
		log.Printf("resync: copy session metadata: %v", err)
		// Non-fatal: worst case, renames/soft-deletes are lost.
	}
	if _, err := newDB.ApplyWorktreeProjectMappingsFromSync(
		context.Background(), e.machine,
	); err != nil {
		log.Printf("resync: apply worktree mappings: %v", err)
	}

	// Reclassify is_automated across every row. Orphan-copied
	// rows carry is_automated values computed against the OLD
	// DB's classifier set; the temp DB's at-Open backfill ran on
	// an empty table and stamped the current hash, so without
	// this pass those rows would be permanently stuck with stale
	// flags. Non-fatal: worst case, some sessions keep their
	// pre-resync classification until the next algorithm bump.
	reportResyncPhase(
		PhaseReclassifying,
		"Reclassifying sessions",
		"",
	)
	if err := newDB.ForceBackfillIsAutomated(); err != nil {
		log.Printf("resync: reclassify is_automated: %v", err)
	}

	if ftsDropped {
		tFTS := time.Now()
		reportResyncPhase(
			PhaseRebuildingSearch,
			"Rebuilding search index",
			"Rebuilding the search index may take a while on large archives.",
		)
		if err := newDB.RebuildFTS(); err != nil {
			log.Printf("resync: rebuild fts: %v", err)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings,
				"fts rebuild failed, aborting swap: "+
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
			"resync: rebuild fts: %s",
			time.Since(tFTS).Round(time.Millisecond),
		)
	}

	// 5. Close newDB and swap files, then reopen origDB.
	reportResyncPhase(
		PhaseSwappingDatabase,
		"Swapping rebuilt database into place",
		"",
	)
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
	} else {
		origDB.MarkDataCurrent()
		if err := origDB.CheckpointWALTruncateWithRetry(ctx); err != nil {
			if errors.Is(err, db.ErrWALCheckpointBusy) {
				log.Printf("resync: wal checkpoint busy")
			} else {
				log.Printf("resync: wal checkpoint: %v", err)
			}
		}
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

func (e *Engine) countRootOpenCodeFormatSessions(
	database *db.DB, agent parser.AgentType,
) int {
	if !isOpenCodeFormatStorageAgent(agent) {
		return 0
	}
	var count int
	err := database.Reader().QueryRow(`
		SELECT COUNT(*) FROM sessions
		WHERE agent = ?
		  AND message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL
	`, string(agent)).Scan(&count)
	if err != nil {
		log.Printf("count root %s sessions: %v", agent, err)
	}
	return count
}

func (e *Engine) countRootKiroSQLiteSessions(
	database *db.DB,
) int {
	var count int
	err := database.Reader().QueryRow(`
		SELECT COUNT(*) FROM sessions
		WHERE agent = ?
		  AND file_path LIKE ?
		  AND message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL
	`, string(parser.AgentKiro), "%data.sqlite3#%").Scan(&count)
	if err != nil {
		log.Printf("count root kiro sqlite sessions: %v", err)
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

// SyncThenRun runs the local sync/resync decision and invokes work while
// syncMu is still held. Daemon-owned mirror pushes use this to keep local sync,
// row scanning, and watermark writes serialized against watcher and periodic
// sync passes.
func (e *Engine) SyncThenRun(
	ctx context.Context,
	full bool,
	onProgress ProgressFunc,
	work func(forceFull bool) error,
) (stats SyncStats, err error) {
	if e.refuseWriteInForceParse("SyncThenRun") {
		return SyncStats{}, nil
	}
	e.syncMu.Lock()
	// Defers run LIFO: Unlock runs before emit.
	defer func() {
		if stats.Synced > 0 {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()

	didResync := full || e.db.NeedsResync()
	if didResync {
		stats = e.resyncAllLocked(ctx, onProgress)
		if stats.Aborted && ctx.Err() == nil {
			stats = e.syncAllLocked(
				ctx, onProgress, time.Time{}, nil,
				syncWriteDefault, true,
			)
		}
	} else {
		stats = e.syncAllLocked(
			ctx, onProgress, time.Time{}, nil,
			syncWriteDefault, true,
		)
	}
	if ctx.Err() != nil {
		return stats, ctx.Err()
	}
	if err := work(full || didResync); err != nil {
		return stats, err
	}
	return stats, nil
}

// RunExclusive runs DB-writing work while holding the same mutex used by local
// sync and resync operations. Use this for daemon-owned maintenance operations
// that must serialize with sync but should not force a local sync first.
func (e *Engine) RunExclusive(work func() error) error {
	if e.refuseWriteInForceParse("RunExclusive") {
		return errors.New(
			"RunExclusive refused on report-only parse-diff engine",
		)
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	return work()
}

// SyncAll discovers and syncs all session files from all agents.
func (e *Engine) SyncAll(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	if e.refuseWriteInForceParse("SyncAll") {
		return SyncStats{}
	}
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
	defer e.clearCurrentProgress()
	stats = e.syncAllLocked(
		ctx, onProgress, time.Time{}, nil, syncWriteDefault, true,
	)
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
	if e.refuseWriteInForceParse("SyncAllSince") {
		return SyncStats{}
	}
	e.syncMu.Lock()
	defer func() {
		if stats.Synced > 0 {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	stats = e.syncAllLocked(
		ctx, onProgress, since, nil, syncWriteDefault, true,
	)
	return
}

// SyncRootsSince syncs only configured roots matching the given
// root paths whose mtimes are at or after the given cutoff. Passing
// "all" in roots is equivalent to SyncAllSince.
func (e *Engine) SyncRootsSince(
	ctx context.Context, roots []string, since time.Time,
	onProgress ProgressFunc,
) (stats SyncStats) {
	if e.refuseWriteInForceParse("SyncRootsSince") {
		return SyncStats{}
	}
	e.syncMu.Lock()
	defer func() {
		if stats.Synced > 0 {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	scope := newRootSyncScope(roots)
	stats = e.syncAllLocked(
		ctx, onProgress, since, scope, syncWriteDefault, scope == nil,
	)
	return
}

type rootSyncScope struct {
	roots []string
}

func newRootSyncScope(roots []string) *rootSyncScope {
	if len(roots) == 0 {
		return nil
	}
	scope := &rootSyncScope{}
	for _, root := range roots {
		if root == "" {
			continue
		}
		if root == "all" {
			return nil
		}
		scope.roots = append(scope.roots, cleanRootPath(root))
	}
	if len(scope.roots) == 0 {
		return nil
	}
	return scope
}

func (s *rootSyncScope) includes(dir string) bool {
	if s == nil {
		return true
	}
	if dir == "" {
		return false
	}
	cleaned := cleanRootPath(dir)
	return slices.ContainsFunc(s.roots, func(root string) bool {
		return samePathOrDescendant(cleaned, root)
	})
}

func (s *rootSyncScope) includesAny(dirs []string) bool {
	if s == nil {
		return true
	}
	return slices.ContainsFunc(dirs, s.includes)
}

func cleanRootPath(path string) string {
	cleaned := filepath.Clean(path)
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return cleaned
	}
	return abs
}

func samePathOrDescendant(path, root string) bool {
	if path == root {
		return true
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (e *Engine) syncAllLocked(
	ctx context.Context, onProgress ProgressFunc, since time.Time,
	scope *rootSyncScope, writeMode syncWriteMode, recordSyncState bool,
) SyncStats {
	if ctx.Err() != nil {
		return SyncStats{Aborted: true}
	}

	if recordSyncState {
		e.recordSyncStarted()
	}
	e.phaseStats.Reset()

	t0 := time.Now()

	var all []parser.DiscoveredFile
	counts := make(map[parser.AgentType]int)
	for _, def := range parser.Registry {
		if !def.FileBased || def.DiscoverFunc == nil {
			continue
		}
		for _, d := range e.agentDirs[def.Type] {
			if !scope.includes(d) {
				continue
			}
			found := def.DiscoverFunc(d)
			counts[def.Type] += len(found)
			all = append(all, found...)
		}
	}
	providerFound, providerFailures := e.discoverProviderSources(ctx, scope)
	for _, file := range providerFound {
		counts[file.Agent]++
	}
	all = append(all, providerFound...)

	if !since.IsZero() {
		all = e.dedupeClaudeDiscoveredFiles(all)
		all = e.filterFilesByMtime(all, since)
	}

	all = dedupeDiscoveredFiles(all)
	all = e.dedupeClaudeDiscoveredFiles(all)
	all = e.filterShadowedLegacyKiroFiles(all)

	verbose := onProgress == nil

	if verbose {
		log.Printf(
			"discovered %d files (%d claude, %d codex, %d copilot, %d gemini, %d cursor, %d amp, %d zencoder, %d iflow, %d vscode-copilot, %d visualstudio-copilot, %d pi, %d omp, %d kiro, %d zed, %d vibe) in %s",
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
			counts[parser.AgentVSCopilot],
			counts[parser.AgentPi],
			counts[parser.AgentOMP],
			counts[parser.AgentKiro],
			counts[parser.AgentZed],
			counts[parser.AgentVibe],
			time.Since(t0).Round(time.Millisecond),
		)
	}

	progressTotal := len(all)
	progressTotal += e.countDBBackedSessions(ctx, scope)
	e.reportProgress(onProgress, Progress{
		Phase:         PhaseSyncing,
		SessionsTotal: progressTotal,
	})

	tWorkers := time.Now()
	results := e.startWorkers(ctx, all)
	stats := e.collectAndBatch(
		ctx, results, len(all), progressTotal, onProgress, writeMode,
	)
	for range providerFailures {
		stats.RecordFailed()
	}
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

	dbProgress := Progress{
		Phase:           PhaseSyncing,
		SessionsTotal:   progressTotal,
		SessionsDone:    stats.filesDiscovered,
		MessagesIndexed: stats.messagesIndexed,
	}

	advanceDBProgress := func(total int, pending []pendingWrite) {
		if total == 0 {
			return
		}
		dbProgress.SessionsDone += total
		for _, pw := range pending {
			dbProgress.MessagesIndexed += len(pw.msgs)
		}
		stats.messagesIndexed = dbProgress.MessagesIndexed
		e.reportProgress(onProgress, dbProgress)
	}

	// Sync current Kiro CLI sessions (SQLite-backed).
	tKiro := time.Now()
	var kiroPending []pendingWrite
	if scope.includesAny(e.agentDirs[parser.AgentKiro]) {
		kiroPending = e.syncKiroSQLite(ctx, scope)
	}
	if len(kiroPending) > 0 {
		stats.TotalSessions += len(kiroPending)
		tWrite := time.Now()
		var kiroWritten int
		if writeMode == syncWriteBulk {
			var failedWrites int
			kiroWritten, _, failedWrites = e.writeBatch(
				kiroPending, writeMode, true,
			)
			for range failedWrites {
				stats.RecordFailed()
			}
		} else {
			resolveWorktreeProject := e.loadWorktreeProjectResolver()
			for _, pw := range kiroPending {
				if ctx.Err() != nil {
					break
				}
				switch err := e.writeSessionFullWithResolver(
					pw, resolveWorktreeProject,
				); {
				case err == nil:
					kiroWritten++
				case isIntentionalSessionSkip(err),
					errors.Is(err, errSessionPreserved):
				default:
					stats.RecordFailed()
				}
			}
		}
		stats.RecordSynced(kiroWritten)
		if verbose {
			log.Printf(
				"kiro sqlite write: %d sessions in %s",
				len(kiroPending),
				time.Since(tWrite).Round(time.Millisecond),
			)
		}
	}
	if verbose {
		log.Printf(
			"kiro sqlite sync: %s",
			time.Since(tKiro).Round(time.Millisecond),
		)
	}
	advanceDBProgress(
		e.countDBBackedProgressTotal(parser.AgentKiro, scope),
		kiroPending,
	)

	if ctx.Err() != nil {
		stats.Aborted = true
		return stats
	}

	// Sync OpenCode-format sessions (DB-backed, not file-based).
	// Uses full replace because these messages can change in place
	// (streaming updates, tool result pairing). Kilo is a fork of
	// OpenCode and shares the same SQLite-backed sync.
	if scope.includesAny(e.agentDirs[parser.AgentOpenCode]) {
		if e.syncOpenCodeFormatAgent(
			ctx, parser.AgentOpenCode, "opencode",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.agentDirs[parser.AgentKilo]) {
		if e.syncOpenCodeFormatAgent(
			ctx, parser.AgentKilo, "kilo",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.agentDirs[parser.AgentMiMoCode]) {
		if e.syncOpenCodeFormatAgent(
			ctx, parser.AgentMiMoCode, "mimocode",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}

	// Sync Warp sessions (DB-backed, not file-based).
	tWarp := time.Now()
	var warpPending []pendingWrite
	if scope.includesAny(e.agentDirs[parser.AgentWarp]) {
		warpPending = e.syncWarp(ctx, scope)
	}
	if len(warpPending) > 0 {
		stats.TotalSessions += len(warpPending)
		tWrite := time.Now()
		var warpWritten int
		if writeMode == syncWriteBulk {
			var failedWrites int
			warpWritten, _, failedWrites = e.writeBatch(
				warpPending, writeMode, true,
			)
			for range failedWrites {
				stats.RecordFailed()
			}
		} else {
			resolveWorktreeProject := e.loadWorktreeProjectResolver()
			for _, pw := range warpPending {
				if ctx.Err() != nil {
					break
				}
				switch err := e.writeSessionFullWithResolver(
					pw, resolveWorktreeProject,
				); {
				case err == nil:
					warpWritten++
				case isIntentionalSessionSkip(err),
					errors.Is(err, errSessionPreserved):
					// Intentional skip, not a failure.
				default:
					stats.RecordFailed()
				}
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
	advanceDBProgress(
		e.countDBBackedProgressTotal(parser.AgentWarp, scope),
		warpPending,
	)

	if ctx.Err() != nil {
		stats.Aborted = true
		return stats
	}

	// Sync Forge sessions (DB-backed, not file-based).
	tForge := time.Now()
	var forgePending []pendingWrite
	if scope.includesAny(e.agentDirs[parser.AgentForge]) {
		forgePending = e.syncForge(ctx, scope)
	}
	if len(forgePending) > 0 {
		stats.TotalSessions += len(forgePending)
		tWrite := time.Now()
		var forgeWritten int
		if writeMode == syncWriteBulk {
			var failedWrites int
			forgeWritten, _, failedWrites = e.writeBatch(
				forgePending, writeMode, true,
			)
			for range failedWrites {
				stats.RecordFailed()
			}
		} else {
			resolveWorktreeProject := e.loadWorktreeProjectResolver()
			for _, pw := range forgePending {
				if ctx.Err() != nil {
					break
				}
				switch err := e.writeSessionFullWithResolver(
					pw, resolveWorktreeProject,
				); {
				case err == nil:
					forgeWritten++
				case errors.Is(err, db.ErrSessionExcluded),
					errors.Is(err, errSessionPreserved):
					// Intentional skip, not a failure.
				default:
					stats.RecordFailed()
				}
			}
		}
		stats.RecordSynced(forgeWritten)
		if verbose {
			log.Printf(
				"forge write: %d sessions in %s",
				len(forgePending),
				time.Since(tWrite).Round(time.Millisecond),
			)
		}
	}
	if verbose {
		log.Printf(
			"forge sync: %s",
			time.Since(tForge).Round(time.Millisecond),
		)
	}
	advanceDBProgress(
		e.countDBBackedProgressTotal(parser.AgentForge, scope),
		forgePending,
	)

	if ctx.Err() != nil {
		stats.Aborted = true
		return stats
	}

	// Sync Piebald sessions (DB-backed, not file-based).
	tPiebald := time.Now()
	var piebaldPending []pendingWrite
	if scope.includesAny(e.agentDirs[parser.AgentPiebald]) {
		piebaldPending = e.syncPiebald(ctx, scope)
	}
	if len(piebaldPending) > 0 {
		stats.TotalSessions += len(piebaldPending)
		tWrite := time.Now()
		var piebaldWritten int
		if writeMode == syncWriteBulk {
			var failedWrites int
			piebaldWritten, _, failedWrites = e.writeBatch(
				piebaldPending, writeMode, true,
			)
			for range failedWrites {
				stats.RecordFailed()
			}
		} else {
			for _, pw := range piebaldPending {
				if ctx.Err() != nil {
					break
				}
				switch err := e.writeSessionFull(pw); {
				case err == nil:
					piebaldWritten++
				case errors.Is(err, db.ErrSessionExcluded),
					errors.Is(err, errSessionPreserved):
					// Intentional skip, not a failure.
				default:
					stats.RecordFailed()
				}
			}
		}
		stats.RecordSynced(piebaldWritten)
		if verbose {
			log.Printf(
				"piebald write: %d sessions in %s",
				len(piebaldPending),
				time.Since(tWrite).Round(time.Millisecond),
			)
		}
	}
	if verbose {
		log.Printf(
			"piebald sync: %s",
			time.Since(tPiebald).Round(time.Millisecond),
		)
	}
	advanceDBProgress(
		e.countDBBackedProgressTotal(parser.AgentPiebald, scope),
		piebaldPending,
	)

	if ctx.Err() != nil {
		stats.Aborted = true
		return stats
	}

	// Link subagent child sessions to their parents after all DB-backed
	// agent writes (Warp, Forge, Piebald). LinkSubagentSessions is idempotent — its
	// WHERE filter and partial index make it a cheap no-op when nothing new
	// was written — so no guard is needed.
	if err := e.db.LinkSubagentSessions(); err != nil {
		log.Printf("link subagent sessions: %v", err)
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

	e.reportProgress(onProgress, Progress{
		Phase:           PhaseDone,
		SessionsTotal:   progressTotal,
		SessionsDone:    progressTotal,
		MessagesIndexed: stats.messagesIndexed,
	})

	e.mu.Lock()
	e.lastSync = time.Now()
	e.lastSyncStats = stats
	e.mu.Unlock()

	if recordSyncState && providerFailures == 0 {
		e.recordSyncFinished()
	}
	// Emission happens in SyncAll / SyncAllSince after syncMu is
	// released; syncAllLocked runs under the caller's lock.
	return stats
}

// discoverProviderSources runs full-sync discovery through the provider facade
// for every concrete provider that is authoritative. It is the provider-shape
// counterpart to the legacy AgentDef.DiscoverFunc loop, so a provider can drop
// its DiscoverFunc and still be discovered once it owns live processing. Shadow
// mode remains observational and never appends provider-only work to the live
// sync list.
func (e *Engine) discoverProviderSources(
	ctx context.Context,
	scope *rootSyncScope,
) ([]parser.DiscoveredFile, int) {
	var files []parser.DiscoveredFile
	var failures int

	agents := make([]parser.AgentType, 0, len(e.providerFactories))
	for agent := range e.providerFactories {
		agents = append(agents, agent)
	}
	slices.SortFunc(agents, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})

	for _, agentType := range agents {
		mode := e.providerMigrationModes[agentType]
		if mode != parser.ProviderMigrationProviderAuthoritative {
			continue
		}
		roots := e.agentDirs[agentType]
		if len(roots) == 0 {
			continue
		}
		filteredRoots := make([]string, 0, len(roots))
		for _, root := range roots {
			if scope.includes(root) {
				filteredRoots = append(filteredRoots, root)
			}
		}
		if len(filteredRoots) == 0 {
			continue
		}
		factory, ok := e.providerFactories[agentType]
		if !ok || factory == nil {
			continue
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots:   filteredRoots,
			Machine: e.machine,
		})
		sources, err := provider.Discover(ctx)
		if err != nil {
			log.Printf("%s provider discovery: %v", agentType, err)
			failures++
			continue
		}
		def := provider.Definition()
		for _, source := range sources {
			sourcePath := providerDiscoveredPath(source)
			if sourcePath == "" {
				continue
			}
			agent := source.Provider
			if agent == "" {
				agent = def.Type
			}
			sourceCopy := source
			files = append(files, parser.DiscoveredFile{
				Path:            sourcePath,
				Project:         source.ProjectHint,
				Agent:           agent,
				ProviderSource:  &sourceCopy,
				ProviderProcess: true,
			})
		}
	}
	return files, failures
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
func (e *Engine) filterFilesByMtime(
	files []parser.DiscoveredFile, cutoff time.Time,
) []parser.DiscoveredFile {
	cutoffNs := cutoff.UnixNano()
	out := files[:0]
	codexIndexRefresh := make(map[string][]parser.DiscoveredFile)
	for _, f := range files {
		mtime, err := discoveredFileMtime(f)
		if err != nil {
			out = append(out, f)
			continue
		}
		if mtime >= cutoffNs {
			out = append(out, f)
			continue
		}
		if f.Agent != parser.AgentCodex || !e.codexIndexNeedsRefreshSince(f.Path, cutoffNs) {
			continue
		}
		key := discoveredFileKey(f)
		codexIndexRefresh[key] = append(codexIndexRefresh[key], f)
	}
	if len(codexIndexRefresh) == 0 {
		return out
	}

	included := make(map[string]struct{}, len(out))
	for _, f := range out {
		included[discoveredFileKey(f)] = struct{}{}
	}
	for key, candidates := range codexIndexRefresh {
		if _, ok := included[key]; ok {
			continue
		}
		out = append(out, pickPreferredCodexDiscoveredFile(e.db, candidates))
	}
	return out
}

func discoveredFileMtime(
	file parser.DiscoveredFile,
) (int64, error) {
	if file.Agent == parser.AgentKiro {
		if _, _, ok := parser.ParseKiroSQLiteVirtualPath(file.Path); ok {
			return parser.KiroSQLiteSourceMtime(file.Path)
		}
	}
	if isOpenCodeFormatStorageAgent(file.Agent) {
		if isOpenCodeFormatSQLiteVirtualPath(file.Agent, file.Path) ||
			isOpenCodeFormatStoragePath(file.Agent, file.Path) {
			return openCodeFormatSourceMtime(
				file.Agent, file.Path,
			)
		}
	}
	if file.Agent == parser.AgentZed {
		dbPath := file.Path
		if p, _, ok := parser.ParseZedSQLiteVirtualPath(file.Path); ok {
			dbPath = p
		}
		return zedDBCompositeMtime(dbPath)
	}
	if file.Agent == parser.AgentShelley {
		dbPath := file.Path
		if p, _, ok := parser.ParseShelleyVirtualPath(file.Path); ok {
			dbPath = p
		}
		return shelleyDBCompositeMtime(dbPath)
	}
	if file.Agent == parser.AgentVSCopilot {
		// Sessions are stored under a <traceFile>#<conversationID> virtual
		// path; stat the physical trace so the mtime filter can drop
		// conversations whose trace file is unchanged.
		info, err := os.Stat(parser.ResolveSourceFilePath(file.Path))
		if err != nil {
			return 0, err
		}
		return info.ModTime().UnixNano(), nil
	}
	if file.Agent == parser.AgentAntigravityCLI {
		info, err := parser.AntigravityCLIFileInfo(file.Path)
		if err != nil {
			return 0, err
		}
		return info.ModTime().UnixNano(), nil
	}
	if file.Agent == parser.AgentAntigravity {
		info, err := parser.AntigravityFileInfo(file.Path)
		if err != nil {
			return 0, err
		}
		return info.ModTime().UnixNano(), nil
	}
	if file.Agent == parser.AgentCowork {
		info, err := os.Stat(file.Path)
		if err != nil {
			return 0, err
		}
		return parser.CoworkSessionMtime(
			file.Path, info.ModTime().UnixNano(),
		), nil
	}
	if file.Agent == parser.AgentCommandCode {
		info, err := os.Stat(file.Path)
		if err != nil {
			return 0, err
		}
		return commandCodeEffectiveInfo(file.Path, info).ModTime().UnixNano(), nil
	}
	if file.Agent == parser.AgentVibe {
		info, err := os.Stat(file.Path)
		if err != nil {
			return 0, err
		}
		return vibeEffectiveInfo(file.Path, info).ModTime().UnixNano(), nil
	}
	if file.Agent == parser.AgentReasonix {
		info, err := os.Stat(file.Path)
		if err != nil {
			return 0, err
		}
		return reasonixEffectiveInfo(file.Path, info).ModTime().UnixNano(), nil
	}

	info, err := os.Stat(file.Path)
	if err != nil {
		return 0, err
	}

	if file.Agent == parser.AgentCopilot {
		return copilotEffectiveMtime(file.Path, info), nil
	}

	return info.ModTime().UnixNano(), nil
}

func (e *Engine) dedupeClaudeDiscoveredFiles(
	files []parser.DiscoveredFile,
) []parser.DiscoveredFile {
	bySessionID := make(map[string][]parser.DiscoveredFile)
	for _, file := range files {
		if file.Agent != parser.AgentClaude {
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			continue
		}
		bySessionID[sessionID] = append(bySessionID[sessionID], file)
	}
	if len(bySessionID) == 0 {
		return files
	}

	preferred := make(map[string]parser.DiscoveredFile, len(bySessionID))
	for sessionID, candidates := range bySessionID {
		preferred[sessionID] = e.pickPreferredClaudeDiscoveredFile(
			sessionID, candidates,
		)
	}

	out := files[:0]
	seen := make(map[string]struct{}, len(preferred))
	for _, file := range files {
		if file.Agent != parser.AgentClaude {
			out = append(out, file)
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			out = append(out, file)
			continue
		}
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		out = append(out, preferred[sessionID])
	}
	return out
}

func claudeSessionIDFromPath(path string) string {
	name := filepath.Base(path)
	sessionID, ok := strings.CutSuffix(name, ".jsonl")
	if !ok {
		return ""
	}
	return sessionID
}

func (e *Engine) pickPreferredClaudeDiscoveredFile(
	sessionID string, candidates []parser.DiscoveredFile,
) parser.DiscoveredFile {
	if len(candidates) == 1 {
		return candidates[0]
	}

	fullID := e.idPrefix + sessionID
	storedPath := e.db.GetSessionFilePath(fullID)
	if storedPath != "" {
		for _, candidate := range candidates {
			if e.effectiveSourcePath(candidate.Path) != storedPath {
				continue
			}
			if e.claudeSourceMatchesStored(fullID, candidate.Path) {
				best := candidate
				for _, competing := range candidates {
					if e.effectiveSourcePath(competing.Path) == storedPath ||
						!claudeCandidateHasAppendProgress(competing, candidate) {
						continue
					}
					if preferClaudeDiscoveredFile(competing, best) {
						best = competing
					}
				}
				return best
			}
		}
	}

	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if preferClaudeDiscoveredFile(candidate, best) {
			best = candidate
		}
	}
	return best
}

func (e *Engine) claudeSourceMatchesStored(
	sessionID, path string,
) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(sessionID)
	if !ok {
		return false
	}
	if storedSize != info.Size() ||
		storedMtime != info.ModTime().UnixNano() {
		return false
	}
	return e.db.GetSessionDataVersion(sessionID) >= db.CurrentDataVersion()
}

func (e *Engine) effectiveSourcePath(path string) string {
	if e.pathRewriter != nil {
		return e.pathRewriter(path)
	}
	return path
}

func claudeCandidateHasAppendProgress(
	candidate, current parser.DiscoveredFile,
) bool {
	candidateInfo, candidateErr := os.Stat(candidate.Path)
	currentInfo, currentErr := os.Stat(current.Path)
	if candidateErr != nil || currentErr != nil {
		return false
	}
	return candidateInfo.Size() > currentInfo.Size()
}

func preferClaudeDiscoveredFile(
	candidate, current parser.DiscoveredFile,
) bool {
	candidateInfo, candidateErr := os.Stat(candidate.Path)
	currentInfo, currentErr := os.Stat(current.Path)
	switch {
	case candidateErr == nil && currentErr != nil:
		return true
	case candidateErr != nil && currentErr == nil:
		return false
	case candidateErr == nil && currentErr == nil:
		if candidateInfo.Size() != currentInfo.Size() {
			return candidateInfo.Size() > currentInfo.Size()
		}
		if !candidateInfo.ModTime().Equal(currentInfo.ModTime()) {
			return candidateInfo.ModTime().After(currentInfo.ModTime())
		}
	}
	return candidate.Path < current.Path
}

// zedDBCompositeMtime returns the maximum mtime across the Zed
// threads.db main file and its WAL/SHM siblings. WAL-only updates
// do not touch threads.db itself, so the composite is needed to
// detect all changes.
func zedDBCompositeMtime(dbPath string) (int64, error) {
	var maxMtime int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(dbPath + suffix)
		if err != nil {
			continue
		}
		if t := info.ModTime().UnixNano(); t > maxMtime {
			maxMtime = t
		}
	}
	if maxMtime == 0 {
		return 0, &os.PathError{Op: "stat", Path: dbPath, Err: os.ErrNotExist}
	}
	return maxMtime, nil
}

// shelleyDBCompositeMtime returns the maximum mtime across the Shelley
// shelley.db main file and its WAL/SHM siblings. The DB is WAL-mode and
// churns constantly, so WAL-only updates that do not touch shelley.db
// itself still need to be detected.
func shelleyDBCompositeMtime(dbPath string) (int64, error) {
	var maxMtime int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		info, err := os.Stat(dbPath + suffix)
		if err != nil {
			continue
		}
		if t := info.ModTime().UnixNano(); t > maxMtime {
			maxMtime = t
		}
	}
	if maxMtime == 0 {
		return 0, &os.PathError{Op: "stat", Path: dbPath, Err: os.ErrNotExist}
	}
	return maxMtime, nil
}

// openCodeFormatPendingSessionIDs returns the SQLite session IDs that
// need re-parsing for an OpenCode-format agent. Uses per-session
// time_updated to detect changes and skips IDs shadowed by a canonical
// storage transcript.
func (e *Engine) openCodeFormatPendingSessionIDs(
	agent parser.AgentType, dir string,
) []string {
	dbPath := filepath.Join(dir, openCodeFormatDBName(agent))
	if info, err := os.Stat(dbPath); err != nil || info.IsDir() {
		return nil
	}

	metas, err := listOpenCodeFormatSessionMeta(agent, dbPath)
	if err != nil {
		log.Printf("sync %s: %v", agent, err)
		return nil
	}
	storageIDs := openCodeFormatStorageSessionIDs(agent, dir)
	var changed []string
	for _, m := range metas {
		if _, ok := storageIDs[m.SessionID]; ok {
			continue
		}
		_, storedMtime, ok := e.db.GetFileInfoByPath(m.VirtualPath)
		if ok && storedMtime == m.FileMtime &&
			e.db.GetDataVersionByPath(m.VirtualPath) >= db.CurrentDataVersion() {
			continue
		}
		changed = append(changed, m.SessionID)
	}
	return changed
}

func (e *Engine) countOneOpenCodeFormatSessions(
	agent parser.AgentType, dir string,
) int {
	dbPath := filepath.Join(dir, openCodeFormatDBName(agent))
	if info, err := os.Stat(dbPath); err != nil || info.IsDir() {
		return 0
	}
	metas, err := listOpenCodeFormatSessionMeta(agent, dbPath)
	if err != nil {
		log.Printf("sync %s: %v", agent, err)
		return 0
	}
	storageIDs := openCodeFormatStorageSessionIDs(agent, dir)
	count := 0
	for _, m := range metas {
		if _, ok := storageIDs[m.SessionID]; !ok {
			count++
		}
	}
	return count
}

func (e *Engine) countDBBackedProgressTotal(
	agent parser.AgentType, scope *rootSyncScope,
) int {
	total := 0
	for _, dir := range e.agentDirs[agent] {
		if dir == "" || !scope.includes(dir) {
			continue
		}
		switch agent {
		case parser.AgentKiro:
			total += e.countOneKiroSQLiteSessions(dir)
		case parser.AgentOpenCode, parser.AgentKilo, parser.AgentMiMoCode:
			total += e.countOneOpenCodeFormatSessions(agent, dir)
		case parser.AgentWarp:
			total += e.countOneWarpSessions(dir)
		case parser.AgentForge:
			total += e.countOneForgeSessions(dir)
		case parser.AgentPiebald:
			total += e.countOnePiebaldSessions(dir)
		}
	}
	return total
}

func (e *Engine) countDBBackedSessions(
	ctx context.Context, scope *rootSyncScope,
) int {
	if ctx.Err() != nil {
		return 0
	}
	total := 0
	for _, agent := range []parser.AgentType{
		parser.AgentKiro,
		parser.AgentOpenCode,
		parser.AgentKilo,
		parser.AgentMiMoCode,
		parser.AgentWarp,
		parser.AgentForge,
		parser.AgentPiebald,
	} {
		total += e.countDBBackedProgressTotal(agent, scope)
	}
	return total
}

func (e *Engine) filterShadowedLegacyKiroFiles(
	files []parser.DiscoveredFile,
) []parser.DiscoveredFile {
	if !hasLegacyKiroCandidates(files) {
		return files
	}

	currentIDs := make(map[string]struct{})
	for _, dir := range e.agentDirs[parser.AgentKiro] {
		for id := range parser.KiroSQLiteSessionIDs(dir) {
			currentIDs[id] = struct{}{}
		}
	}
	if len(currentIDs) == 0 {
		return files
	}

	out := files[:0]
	for _, file := range files {
		if file.Agent != parser.AgentKiro ||
			filepath.Base(file.Path) == "data.sqlite3" {
			out = append(out, file)
			continue
		}
		legacyID := parser.KiroSessionIDFromPath(file.Path)
		if _, shadowed := currentIDs[legacyID]; shadowed {
			continue
		}
		out = append(out, file)
	}
	return out
}

func hasLegacyKiroCandidates(files []parser.DiscoveredFile) bool {
	for _, file := range files {
		if file.Agent == parser.AgentKiro &&
			filepath.Base(file.Path) != "data.sqlite3" {
			return true
		}
	}
	return false
}

func (e *Engine) isShadowedLegacyKiroPath(path string) bool {
	if filepath.Base(path) == "data.sqlite3" {
		return false
	}
	legacyID := parser.KiroSessionIDFromPath(path)
	if legacyID == "" {
		return false
	}
	for _, dir := range e.agentDirs[parser.AgentKiro] {
		dbPath := parser.FindKiroSQLiteDBPath(dir)
		if dbPath != "" &&
			parser.KiroSQLiteSessionExists(dbPath, legacyID) {
			return true
		}
	}
	return false
}

func (e *Engine) kiroSQLitePendingSessionIDs(
	metas []parser.KiroSQLiteSessionMeta,
) []string {
	var changed []string
	for _, meta := range metas {
		_, storedMtime, ok := e.db.GetFileInfoByPath(meta.VirtualPath)
		if ok && storedMtime == meta.FileMtime &&
			e.db.GetDataVersionByPath(meta.VirtualPath) >=
				db.CurrentDataVersion() {
			continue
		}
		changed = append(changed, meta.SessionID)
	}
	return changed
}

func (e *Engine) countOneKiroSQLiteSessions(dir string) int {
	dbPath := parser.FindKiroSQLiteDBPath(dir)
	if dbPath == "" {
		return 0
	}
	store, err := parser.OpenKiroSQLiteStore(dbPath)
	if err != nil {
		log.Printf("sync kiro sqlite: %v", err)
		return 0
	}
	defer store.Close()
	metas, err := store.ListSessionMeta()
	if err != nil {
		log.Printf("sync kiro sqlite: %v", err)
		return 0
	}
	return len(metas)
}

func (e *Engine) syncKiroSQLite(
	ctx context.Context, scope *rootSyncScope,
) []pendingWrite {
	var allPending []pendingWrite
	for _, dir := range e.agentDirs[parser.AgentKiro] {
		if ctx.Err() != nil {
			break
		}
		if dir == "" || !scope.includes(dir) {
			continue
		}
		allPending = append(
			allPending, e.syncOneKiroSQLite(ctx, dir)...,
		)
	}
	return allPending
}

func (e *Engine) syncOneKiroSQLite(
	ctx context.Context, dir string,
) []pendingWrite {
	dbPath := parser.FindKiroSQLiteDBPath(dir)
	if dbPath == "" {
		return nil
	}
	store, err := parser.OpenKiroSQLiteStore(dbPath)
	if err != nil {
		log.Printf("sync kiro sqlite: %v", err)
		return nil
	}
	defer store.Close()
	metas, err := store.ListSessionMeta()
	if err != nil {
		log.Printf("sync kiro sqlite: %v", err)
		return nil
	}
	changed := e.kiroSQLitePendingSessionIDs(metas)
	if len(changed) == 0 {
		return nil
	}

	var pending []pendingWrite
	for _, sid := range changed {
		if ctx.Err() != nil {
			break
		}
		sess, msgs, err := store.ParseSession(
			sid, e.machine,
		)
		if err != nil {
			log.Printf("kiro sqlite session %s: %v", sid, err)
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

// syncOpenCodeFormat syncs sessions from an OpenCode-format agent's
// SQLite database (OpenCode or its Kilo fork). Uses per-session
// time_updated to detect changes, so only modified sessions are fully
// parsed. Returns pending writes.
func (e *Engine) syncOpenCodeFormat(
	ctx context.Context, agent parser.AgentType, scope *rootSyncScope,
) []pendingWrite {
	var allPending []pendingWrite
	for _, dir := range e.agentDirs[agent] {
		if ctx.Err() != nil {
			break
		}
		if dir == "" || !scope.includes(dir) {
			continue
		}
		allPending = append(
			allPending, e.syncOneOpenCodeFormat(ctx, agent, dir)...,
		)
	}
	return allPending
}

// syncOneOpenCodeFormat handles a single OpenCode-format directory.
func (e *Engine) syncOneOpenCodeFormat(
	ctx context.Context, agent parser.AgentType, dir string,
) []pendingWrite {
	dbPath := filepath.Join(dir, openCodeFormatDBName(agent))
	changed := e.openCodeFormatPendingSessionIDs(agent, dir)
	if len(changed) == 0 {
		return nil
	}

	var pending []pendingWrite
	for _, sid := range changed {
		if ctx.Err() != nil {
			break
		}
		sess, msgs, err := parseOpenCodeFormatSession(
			agent, dbPath, sid, e.machine,
		)
		if err != nil {
			log.Printf(
				"%s session %s: %v", agent, sid, err,
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

// syncOpenCodeFormatAgent collects, writes, and records pending
// sessions for one OpenCode-format agent. It returns true when the
// context was cancelled so the caller can mark the sync aborted.
func (e *Engine) syncOpenCodeFormatAgent(
	ctx context.Context, agent parser.AgentType, label string,
	writeMode syncWriteMode, verbose bool, scope *rootSyncScope,
	stats *SyncStats,
	advanceDBProgress func(total int, pending []pendingWrite),
) bool {
	start := time.Now()
	pending := e.syncOpenCodeFormat(ctx, agent, scope)
	if len(pending) > 0 {
		stats.TotalSessions += len(pending)
		tWrite := time.Now()
		var written int
		if writeMode == syncWriteBulk {
			var failedWrites int
			written, _, failedWrites = e.writeBatch(
				pending, writeMode, true,
			)
			for range failedWrites {
				stats.RecordFailed()
			}
		} else {
			resolveWorktreeProject := e.loadWorktreeProjectResolver()
			for _, pw := range pending {
				if ctx.Err() != nil {
					break
				}
				switch err := e.writeSessionFullWithResolver(
					pw, resolveWorktreeProject,
				); {
				case err == nil:
					written++
				case isIntentionalSessionSkip(err),
					errors.Is(err, errSessionPreserved):
					// Intentional skip, not a failure.
				default:
					stats.RecordFailed()
				}
			}
		}
		stats.RecordSynced(written)
		if verbose {
			log.Printf(
				"%s write: %d sessions in %s",
				label, len(pending),
				time.Since(tWrite).Round(time.Millisecond),
			)
		}
	}
	if verbose {
		log.Printf(
			"%s sync: %s",
			label, time.Since(start).Round(time.Millisecond),
		)
	}
	advanceDBProgress(e.countDBBackedProgressTotal(agent, scope), pending)
	return ctx.Err() != nil
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
	buffer := max(workers*2, 1)

	jobs := make(chan parser.DiscoveredFile, buffer)
	results := make(chan syncJob, buffer)

	var wg gosync.WaitGroup
	for range workers {
		wg.Go(func() {
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
					processResult: e.processFile(ctx, file),
					path:          file.Path,
				}
			}
		})
	}

	go func() {
		for _, f := range files {
			jobs <- f
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	return results
}

// collectAndBatch drains the results channel, batches
// successful parses, and writes them to the database.
// When ctx is cancelled, it stops processing new results
// and returns partial stats.
func (e *Engine) collectAndBatch(
	ctx context.Context,
	results <-chan syncJob, total int, progressTotal int,
	onProgress ProgressFunc,
	writeMode syncWriteMode,
) SyncStats {
	var stats SyncStats
	stats.TotalSessions = total
	stats.filesDiscovered = total

	if progressTotal == 0 {
		progressTotal = total
	}
	progress := Progress{
		Phase:         PhaseSyncing,
		SessionsTotal: progressTotal,
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
			if r.cacheSkip && r.mtime != 0 && !r.noCacheSkip {
				e.cacheSkip(r.skipCacheKey(), r.mtime)
			}
			log.Printf("sync error: %v", r.err)
			continue
		}
		if r.skip {
			if r.cacheSkip && r.mtime != 0 && !r.noCacheSkip {
				e.cacheSkip(r.skipCacheKey(), r.mtime)
			}
			stats.RecordSkip()
			progress.SessionsDone++
			e.reportProgress(onProgress, progress)
			continue
		}
		excludedSessionIDs := e.applyIDPrefixToSessionIDs(
			r.excludedSessionIDs,
		)
		if len(excludedSessionIDs) > 0 {
			if _, err := e.db.DeleteParserExcludedSessions(
				excludedSessionIDs,
			); err != nil {
				log.Printf("delete parser-excluded sessions: %v", err)
				stats.RecordFailed()
				continue
			}
			stats.parserExcludedIDs = append(
				stats.parserExcludedIDs,
				excludedSessionIDs...,
			)
		}
		if len(r.results) == 0 && r.incremental == nil {
			if len(r.excludedSessionIDs) > 0 {
				stats.filesOK++
				stats.parserExcludedFiles++
			}
			if r.cacheSkip && !r.noCacheSkip {
				e.cacheSkip(r.skipCacheKey(), r.mtime)
			}
			progress.SessionsDone++
			e.reportProgress(onProgress, progress)
			continue
		}
		if r.cacheSkip {
			e.clearSkip(r.skipCacheKey())
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
			stats.messagesIndexed = progress.MessagesIndexed
		} else {
			for _, pr := range r.results {
				pending = append(pending, pendingWrite{
					sess:         pr.Session,
					msgs:         pr.Messages,
					usageEvents:  pr.UsageEvents,
					needsRetry:   r.needsRetryForSession(pr.Session.ID),
					forceReplace: r.forceReplace,
				})
			}
		}

		if len(pending) >= batchSize {
			writtenSessions, writtenMessages, failedWrites :=
				e.writeBatch(pending, writeMode, false)
			stats.RecordSynced(writtenSessions)
			for range failedWrites {
				stats.RecordFailed()
			}
			progress.MessagesIndexed += writtenMessages
			stats.messagesIndexed = progress.MessagesIndexed
			pending = pending[:0]
		}

		progress.SessionsDone++
		e.reportProgress(onProgress, progress)
	}

flush:
	if len(pending) > 0 {
		writtenSessions, writtenMessages, failedWrites :=
			e.writeBatch(pending, writeMode, false)
		stats.RecordSynced(writtenSessions)
		for range failedWrites {
			stats.RecordFailed()
		}
		progress.MessagesIndexed += writtenMessages
		stats.messagesIndexed = progress.MessagesIndexed
	}

	// Link subagent child sessions to their parents via
	// tool_calls.subagent_session_id references. Run once
	// after all batches to avoid repeated full-table scans.
	if err := e.db.LinkSubagentSessions(); err != nil {
		log.Printf("link subagent sessions: %v", err)
	}

	// PhaseDone is emitted by syncAllLocked after DB-backed
	// agents finish, so this stage stays in PhaseSyncing.
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
	fileHash             string
	nextOrdinal          int
	lastEntryUUID        string
	totalOutputTokens    int // absolute (old + new)
	peakContextTokens    int // absolute max(old, new)
	hasTotalOutputTokens bool
	hasPeakContextTokens bool
}

// sessionParseError is a per-session parse failure inside a shared
// SQLite store (OpenCode, Zed, Kiro), where one file path fans out to
// many sessions and a single bad payload must not fail the whole db.
type sessionParseError struct {
	sessionID   string // raw parser-side ID, no engine prefix
	virtualPath string // dbPath#rawID source path
	err         error
}

type processResult struct {
	results            []parser.ParseResult
	excludedSessionIDs []string
	// sessionErrs carries per-session parse failures from the
	// shared-db fan-out loops. Normal sync logs and skips these;
	// parse-diff (forceParse) surfaces them as DiffParseError report
	// entries so --fail-on-change cannot pass over a session the
	// current binary failed to parse.
	sessionErrs []sessionParseError
	skip        bool
	mtime       int64
	err         error
	incremental *incrementalUpdate
	cacheSkip   bool
	// noCacheSkip suppresses skip-cache recording for an errored
	// result even when cacheSkip is set for the agent. Read/scan
	// failures are transient: a permission or readability fix may
	// not change the file mtime, so caching the failure by mtime
	// would silently skip the file on later syncs instead of
	// retrying it.
	noCacheSkip bool
	needsRetry  bool
	// forceReplace requests full message replacement on write,
	// even when the existing rows would otherwise be left in
	// place. Set when a fall-through to full parse is recovering
	// from a cross-sync streaming split: the new merged messages
	// reuse the existing ordinals, so the default append-only
	// writeMessages would silently drop the rewrite.
	forceReplace bool
	cacheKey     string
	// retrySessionIDs carries provider per-result data-version state.
	// Legacy parsers use needsRetry as a source-wide fallback.
	retrySessionIDs map[string]bool
	// suppressPresenceSweep marks an incomplete source result where
	// missing stored sessions are expected rather than parser drift.
	suppressPresenceSweep bool
}

func (r processResult) needsRetryForSession(sessionID string) bool {
	if r.retrySessionIDs != nil {
		return r.retrySessionIDs[sessionID]
	}
	return r.needsRetry
}

func (r processResult) suppressesPresenceSweepForRetry() bool {
	return r.retrySessionIDs == nil && r.needsRetry
}

func (e *Engine) processFile(
	ctx context.Context,
	file parser.DiscoveredFile,
) processResult {
	if res, ok := e.processProviderFile(ctx, file); ok {
		return res
	}

	var info os.FileInfo
	var err error
	switch file.Agent {
	case parser.AgentAntigravityCLI:
		info, err = parser.AntigravityCLIFileInfo(file.Path)
	case parser.AgentAntigravity:
		// WAL-only commits and annotation updates do not touch
		// the main .db, so skip checks need the composite stat.
		info, err = parser.AntigravityFileInfo(file.Path)
	default:
		statPath := file.Path
		if dbPath, _, ok := parser.ParseKiroSQLiteVirtualPath(file.Path); ok {
			statPath = dbPath
		} else if dbPath, _, ok := parser.ParseKiloSQLiteVirtualPath(file.Path); ok {
			statPath = dbPath
		} else if dbPath, _, ok := parser.ParseMiMoCodeSQLiteVirtualPath(file.Path); ok {
			statPath = dbPath
		} else if dbPath, _, ok := parser.ParseZedSQLiteVirtualPath(file.Path); ok {
			statPath = dbPath
		} else if dbPath, _, ok := parser.ParseShelleyVirtualPath(file.Path); ok {
			statPath = dbPath
		} else if tracePath, _, ok := parser.ParseVisualStudioCopilotVirtualPath(file.Path); ok {
			statPath = tracePath
		} else if historyPath, _, ok := parser.ParseAiderVirtualPath(file.Path); ok {
			// aider stores "<historyFile>#<runIdx>"; stat the physical file
			// so SyncSingleSession (live watcher / on-demand re-sync) works.
			statPath = historyPath
		}
		info, err = os.Stat(statPath)
	}
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
	if file.Agent == parser.AgentCowork {
		mtime = parser.CoworkSessionMtime(file.Path, mtime)
	}
	if file.Agent == parser.AgentVibe {
		// Vibe metadata (title, model, usage, canonical ID) lives in the
		// sibling meta.json, so the skip-cache key must move when either file
		// changes. Match vibeEffectiveInfo (max of messages.jsonl and
		// meta.json) so a fixed meta.json retries a cached parse error instead
		// of staying skipped on the unchanged transcript mtime.
		mtime = vibeEffectiveInfo(file.Path, info).ModTime().UnixNano()
	}
	if file.Agent == parser.AgentReasonix {
		mtime = reasonixEffectiveInfo(file.Path, info).ModTime().UnixNano()
	}
	cacheSkip := e.shouldCacheSkip(file)

	// Skip files cached from a previous sync (parse errors
	// or non-interactive sessions) whose mtime is unchanged.
	// Legacy codex_exec entries from pre-bulk-sync builds are
	// scrubbed once at engine construction by
	// migrateLegacyCodexExecSkips, so this check can treat
	// the skip cache as authoritative without per-file
	// re-validation.
	if cacheSkip && !e.forceParse && !file.ForceParse { // parse-diff: ignore the skip cache
		e.skipMu.RLock()
		cachedMtime, cached := e.skipCache[file.Path]
		e.skipMu.RUnlock()
		if cached && cachedMtime == mtime {
			if e.pathNeedsProjectReparse(file.Path) {
				e.clearSkip(file.Path)
			} else {
				res := processResult{
					skip:      true,
					mtime:     mtime,
					cacheSkip: true,
				}
				e.observeProviderShadow(ctx, file, res)
				return res
			}
		}
	}

	var res processResult
	switch file.Agent {
	case parser.AgentClaude:
		res = e.processClaude(ctx, file, info)
	case parser.AgentCowork:
		res = e.processCowork(file, info)
	case parser.AgentCodex:
		res = e.processCodex(file, info)
	case parser.AgentCopilot:
		res = e.processCopilot(file, info)
	case parser.AgentReasonix:
		res = e.processReasonix(file, info)
	case parser.AgentGemini:
		res = e.processGemini(file, info)
	case parser.AgentOpenCode, parser.AgentKilo, parser.AgentMiMoCode:
		res = e.processOpenCodeFormat(file.Agent, file, info)
	case parser.AgentOpenHands:
		res = e.processOpenHands(file, info)
	case parser.AgentCursor:
		res = e.processCursor(file, info)
	case parser.AgentIflow:
		res = e.processIflow(ctx, file, info)
	case parser.AgentAmp:
		res = e.processAmp(file, info)
	case parser.AgentDeepSeekTUI:
		res = e.processDeepSeekTUI(file, info)
	case parser.AgentZencoder:
		res = e.processZencoder(file, info)
	case parser.AgentVSCodeCopilot:
		res = e.processVSCodeCopilot(file, info)
	case parser.AgentVSCopilot:
		res = e.processVisualStudioCopilot(file, info)
	case parser.AgentPi, parser.AgentOMP:
		res = e.processPi(file, info)
	case parser.AgentQwen:
		res = e.processQwen(file, info)
	case parser.AgentCommandCode:
		res = e.processCommandCode(file, info)
	case parser.AgentOpenClaw:
		res = e.processOpenClaw(file, info)
	case parser.AgentQClaw:
		res = e.processQClaw(file, info)
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
	case parser.AgentWorkBuddy:
		res = e.processWorkBuddy(file, info)
	case parser.AgentVibe:
		res = e.processVibe(file, info)
	case parser.AgentPositron:
		res = e.processPositron(file, info)
	case parser.AgentZed:
		res = e.processZed(file, info)
	case parser.AgentShelley:
		res = e.processShelley(file, info)
	case parser.AgentAntigravity:
		res = e.processAntigravity(file, info)
	case parser.AgentAntigravityCLI:
		res = e.processAntigravityCLI(file, info)
	case parser.AgentQwenPaw:
		res = e.processQwenPaw(file, info)
	case parser.AgentGptme:
		res = e.processGptme(file, info)
	case parser.AgentAider:
		res = e.processAider(file, info)
	default:
		res = processResult{
			err: fmt.Errorf(
				"unknown agent type: %s", file.Agent,
			),
		}
	}
	res.cacheSkip = cacheSkip
	res.mtime = mtime
	e.observeProviderShadow(ctx, file, res)
	return res
}

func (e *Engine) pathNeedsProjectReparse(path string) bool {
	if e == nil || e.db == nil {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	project, ok := e.db.GetProjectByPath(lookupPath)
	return ok && parser.NeedsProjectReparse(project)
}

func (e *Engine) processProviderFile(
	ctx context.Context,
	file parser.DiscoveredFile,
) (processResult, bool) {
	mode := e.providerMigrationModes[file.Agent]
	if mode != parser.ProviderMigrationProviderAuthoritative {
		return processResult{}, false
	}
	if file.ProviderSource != nil && !file.ProviderProcess {
		return processResult{}, false
	}

	factory, ok := e.providerFactories[file.Agent]
	if !ok {
		return processResult{
			err: fmt.Errorf("provider not found for agent type: %s", file.Agent),
		}, true
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   e.agentDirs[file.Agent],
		Machine: e.machine,
	})

	source, found, err := e.providerSourceForDiscoveredFile(ctx, provider, file)
	if err != nil {
		return processResult{err: err}, true
	}
	if !found {
		return processResult{
			err: fmt.Errorf(
				"%s provider source not found for %s",
				file.Agent,
				file.Path,
			),
		}, true
	}

	fingerprint, err := provider.Fingerprint(ctx, source)
	if err != nil {
		return processResult{err: err}, true
	}
	cacheKey := providerProcessCacheKey(file, source, fingerprint)
	cacheSkip := e.shouldCacheSkip(file)
	if cacheSkip && !e.forceParse && !file.ForceParse {
		e.skipMu.RLock()
		cachedMtime, cached := e.skipCache[cacheKey]
		e.skipMu.RUnlock()
		if cached && cachedMtime == fingerprint.MTimeNS {
			return processResult{
				skip:      true,
				mtime:     fingerprint.MTimeNS,
				cacheSkip: true,
				cacheKey:  cacheKey,
			}, true
		}
	}

	outcome, err := provider.Parse(ctx, parser.ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
		Machine:     e.machine,
		ForceParse:  e.forceParse || file.ForceParse,
	})
	if err != nil {
		return processResult{
			err:         err,
			mtime:       fingerprint.MTimeNS,
			cacheSkip:   cacheSkip,
			cacheKey:    cacheKey,
			noCacheSkip: true,
		}, true
	}
	if err := validateProviderOutcome(
		provider.Definition(),
		source,
		fingerprint,
		outcome,
	); err != nil {
		return processResult{
			err:         err,
			mtime:       fingerprint.MTimeNS,
			cacheSkip:   cacheSkip,
			cacheKey:    cacheKey,
			noCacheSkip: true,
		}, true
	}
	cleanCache := providerOutcomeAllowsCleanSkipCache(outcome)
	if outcome.SkipReason != parser.SkipNone {
		return processResult{
			skip:        true,
			mtime:       fingerprint.MTimeNS,
			cacheSkip:   cacheSkip,
			cacheKey:    cacheKey,
			noCacheSkip: !cleanCache,
		}, true
	}

	res := processResult{
		results:               parseOutcomeResults(outcome.Results),
		excludedSessionIDs:    append([]string(nil), outcome.ExcludedSessionIDs...),
		mtime:                 fingerprint.MTimeNS,
		cacheSkip:             cacheSkip,
		cacheKey:              cacheKey,
		noCacheSkip:           !cleanCache,
		forceReplace:          outcome.ForceReplace,
		suppressPresenceSweep: !outcome.ResultSetComplete,
	}
	for _, result := range outcome.Results {
		if result.DataVersion == parser.DataVersionNeedsRetry {
			if res.retrySessionIDs == nil {
				res.retrySessionIDs = make(map[string]bool)
			}
			res.retrySessionIDs[result.Result.Session.ID] = true
		}
	}
	if e.forceParse || file.ForceParse {
		for _, sourceErr := range outcome.SourceErrors {
			res.sessionErrs = append(res.sessionErrs, sessionParseError{
				sessionID:   sourceErr.SessionID,
				virtualPath: sourceErr.SourceKey,
				err:         sourceErr.Err,
			})
		}
	}
	return res, true
}

func providerOutcomeAllowsCleanSkipCache(outcome parser.ParseOutcome) bool {
	if !outcome.ResultSetComplete {
		return false
	}
	if len(outcome.SourceErrors) > 0 {
		return false
	}
	for _, result := range outcome.Results {
		if result.DataVersion == parser.DataVersionNeedsRetry {
			return false
		}
	}
	return true
}

func (e *Engine) providerSourceForDiscoveredFile(
	ctx context.Context,
	provider parser.Provider,
	file parser.DiscoveredFile,
) (parser.SourceRef, bool, error) {
	if file.ProviderSource != nil {
		source := *file.ProviderSource
		if source.Provider != file.Agent {
			return parser.SourceRef{}, false, fmt.Errorf(
				"provider source mismatch for %s: %s",
				file.Agent,
				source.Provider,
			)
		}
		return source, true, nil
	}

	return provider.FindSource(ctx, parser.FindSourceRequest{
		StoredFilePath:     file.Path,
		FingerprintKey:     file.Path,
		RequireFreshSource: !e.forceParse && !file.ForceParse,
	})
}

func providerProcessCacheKey(
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) string {
	if key := plannedSkipKey(source, fingerprint); key != "" {
		return key
	}
	return file.Path
}

func (e *Engine) observeProviderShadow(
	ctx context.Context,
	file parser.DiscoveredFile,
	legacy processResult,
) {
	mode := e.providerMigrationModes[file.Agent]
	if mode != parser.ProviderMigrationShadowCompare {
		return
	}
	comparison := ProviderShadowComparison{File: file, Mode: mode}
	if reason := providerShadowNotComparableReason(legacy); reason != "" {
		comparison.NotComparableReason = reason
		e.recordProviderShadowComparison(comparison)
		return
	}
	factory, ok := e.providerFactories[file.Agent]
	if !ok {
		return
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   e.agentDirs[file.Agent],
		Machine: e.machine,
	})
	source, found, err := e.providerSourceForDiscoveredFile(ctx, provider, file)
	comparison.Err = err
	if err == nil && found {
		comparison.Source = source
		comparison.Observation, comparison.Err = ObserveProviderSource(
			ctx,
			provider,
			ProviderObserveRequest{
				Source:     source,
				Machine:    e.machine,
				ForceParse: e.forceParse || file.ForceParse,
			},
		)
		if comparison.Err == nil {
			comparison.Mismatches = compareProviderObservationToProcessResult(
				comparison.Observation,
				legacy,
				file,
			)
		}
	}
	if err == nil && !found {
		comparison.Err = fmt.Errorf(
			"%s provider shadow source not found for %s",
			file.Agent,
			file.Path,
		)
	}
	e.recordProviderShadowComparison(comparison)
}

func providerShadowNotComparableReason(legacy processResult) string {
	switch {
	case legacy.err != nil:
		return "legacy error"
	case legacy.incremental != nil:
		return "legacy incremental"
	case legacy.skip:
		return "legacy skip"
	default:
		return ""
	}
}

func (e *Engine) recordProviderShadowComparison(
	comparison ProviderShadowComparison,
) {
	if e.providerShadowRecorder != nil {
		e.providerShadowMu.Lock()
		defer e.providerShadowMu.Unlock()
		e.providerShadowRecorder(comparison)
		return
	}
	if comparison.NotComparableReason != "" {
		return
	}
	sourceKey := comparison.Source.Key
	if sourceKey == "" {
		sourceKey = comparison.Source.FingerprintKey
	}
	fingerprintKey := comparison.Observation.Fingerprint.Key
	if fingerprintKey == "" {
		fingerprintKey = comparison.Source.FingerprintKey
	}
	if comparison.Err != nil {
		log.Printf(
			"%s provider shadow %s mode=%s source=%q fingerprint=%q: %v",
			comparison.File.Agent,
			comparison.File.Path,
			comparison.Mode,
			sourceKey,
			fingerprintKey,
			comparison.Err,
		)
		return
	}
	if len(comparison.Mismatches) == 0 {
		return
	}
	log.Printf(
		"%s provider shadow %s mode=%s source=%q fingerprint=%q mismatches: %s",
		comparison.File.Agent,
		comparison.File.Path,
		comparison.Mode,
		sourceKey,
		fingerprintKey,
		strings.Join(comparison.Mismatches, "; "),
	)
}

func (e *Engine) shouldCacheSkip(
	file parser.DiscoveredFile,
) bool {
	if file.Agent == parser.AgentKiro {
		if filepath.Base(file.Path) == "data.sqlite3" {
			return false
		}
		if _, _, ok := parser.ParseKiroSQLiteVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentZed {
		if filepath.Base(file.Path) == "threads.db" {
			return false
		}
		if _, _, ok := parser.ParseZedSQLiteVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentShelley {
		if filepath.Base(file.Path) == shelleyDBFile {
			return false
		}
		if _, _, ok := parser.ParseShelleyVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentVSCopilot {
		// Visual Studio Copilot conversations are skipped by a composite
		// fingerprint spanning every sibling trace file (see
		// processVisualStudioCopilot). The generic skip cache keys on the
		// representative file's mtime alone, so a cached entry would bypass that
		// composite check and miss a sibling-only change or removal.
		if parser.IsVisualStudioCopilotTraceFile(file.Path) {
			return false
		}
		if _, _, ok :=
			parser.ParseVisualStudioCopilotVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentAider {
		// A virtual aider path ("<historyFile>#<runIdx>") resolves to one
		// run inside a shared physical file; let processAider own it so the
		// generic per-file mtime cache cannot stand in for the per-run parse.
		// The physical history file itself keeps the generic mtime skip: any
		// write bumps the file mtime and re-parses every run.
		if _, _, ok := parser.ParseAiderVirtualPath(file.Path); ok {
			return false
		}
	}
	if !isOpenCodeFormatStorageAgent(file.Agent) {
		return true
	}
	if filepath.Base(file.Path) == openCodeFormatDBName(file.Agent) {
		return false
	}
	if isOpenCodeFormatSQLiteVirtualPath(file.Agent, file.Path) {
		return false
	}
	for _, dir := range e.agentDirs[file.Agent] {
		if dir == "" {
			continue
		}
		src := resolveOpenCodeFormatSource(file.Agent, dir)
		if src.Mode != parser.OpenCodeSourceStorage {
			continue
		}
		if rel, ok := isUnder(dir, file.Path); ok {
			rel = filepath.ToSlash(rel)
			sessionPrefix := "storage/" +
				filepath.Base(src.SessionRoot) + "/"
			return !strings.HasPrefix(rel, sessionPrefix)
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
	if e.forceParse { // parse-diff: always re-parse
		return false
	}
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
	if e.forceParse { // parse-diff: always re-parse
		return false
	}
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
	ctx context.Context,
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {

	sessionID := strings.TrimSuffix(info.Name(), ".jsonl")

	if e.shouldSkipFile(sessionID, info) {
		sess, _ := e.db.GetSession(
			ctx, e.idPrefix+sessionID,
		)
		if sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			return processResult{skip: true}
		}
	}

	// Try incremental parse for append-only JSONL files
	// that have already been synced. When the incremental path
	// declines but signals forceReplace (e.g. cross-sync split
	// recovery), carry the flag onto the full-parse result so the
	// write path uses ReplaceSessionMessages.
	res, ok := e.tryIncrementalJSONL(
		file, info, parser.AgentClaude,
		parser.ParseClaudeSessionFrom,
	)
	if ok {
		return res
	}
	forceReplace := res.forceReplace

	// Determine project name from cwd if possible
	project := parser.GetProjectName(file.Project)
	cwd, gitBranch := parser.ExtractClaudeProjectHints(
		file.Path,
	)
	if cwd != "" {
		if p := parser.ExtractProjectFromCwdWithBranchContext(
			ctx, cwd, gitBranch,
		); p != "" {
			project = p
		}
	}

	results, excludedIDs, err := parser.ParseClaudeSessionWithExclusions(
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

	return processResult{
		results:            results,
		excludedSessionIDs: excludedIDs,
		forceReplace:       forceReplace,
	}
}

// processCowork parses a Claude Desktop "cowork" (local agent mode)
// session. The transcript is a standard Claude Code JSONL file nested
// inside the cowork session directory, so the work is delegated to the
// Claude parser and rewritten into the cowork namespace by
// parser.ParseCoworkSession. Cowork session IDs are "cowork:"-prefixed, so
// the skip check keys off file_path rather than the bare filename stem.
func (e *Engine) processCowork(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {

	// The session title lives in the sibling metadata file, so a rename
	// changes only that file. Skip on the composite (transcript+metadata)
	// mtime so renames are re-parsed instead of skipped as unchanged.
	compositeMtime := parser.CoworkSessionMtime(
		file.Path, info.ModTime().UnixNano(),
	)
	fi := fakeSnapshotInfo{fSize: info.Size(), fMtime: compositeMtime}
	if e.shouldSkipByPath(file.Path, fi) {
		return processResult{skip: true}
	}

	results, excludedIDs, err := parser.ParseCoworkSession(
		file.Path, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}

	inode, device := getFileIdentity(info)
	hash, hashErr := ComputeFileHash(file.Path)
	for i := range results {
		results[i].Session.File.Inode = inode
		results[i].Session.File.Device = device
		if hashErr == nil {
			results[i].Session.File.Hash = hash
		}
	}

	return processResult{
		results:            results,
		excludedSessionIDs: excludedIDs,
	}
}

// incrementalParseFunc reads new JSONL lines from a file
// starting at the given byte offset with the given starting
// ordinal. Returns parsed messages, the latest timestamp
// (endedAt), bytes consumed (relative to offset), and any
// error. The consumed count covers only complete, valid JSON
// lines so it can be used as a safe resume offset.
type incrementalParseFunc func(
	path string, offset int64, startOrdinal int, lastEntryUUID string,
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
	if e.forceParse { // parse-diff: never produce append deltas
		return processResult{}, false
	}
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

	// Claude-only: if the stored preview is empty despite the
	// session already having user turns, the parser skipped
	// every user message so far (e.g. a session that opens with
	// /clear or /effort). Fall back to a full parse so any real
	// user message appended this sync becomes first_message.
	//
	// Other agents can legitimately have UserMsgCount > 0 with
	// an empty first_message — for example Codex inserts orphan
	// subagent notifications as Role=user messages that bypass
	// firstMessage — so this fall-through is gated on Claude.
	if agent == parser.AgentClaude &&
		inc.FirstMessage == "" && inc.UserMsgCount > 0 {
		return processResult{}, false
	}

	currentSize := info.Size()
	if currentSize <= inc.FileSize {
		return processResult{}, false
	}

	// A prior sync that stored no message rows has no safe append
	// boundary. Rewritten files can grow in place and keep the same
	// identity, which makes a full-file replacement look like an
	// append from the old file_size offset.
	if inc.MsgCount == 0 {
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

	// Persist the same effective file_mtime a full parse would store. For
	// Codex that folds in session_index.jsonl (parser.CodexEffectiveMtime),
	// exactly as ParseCodexSession sets File.Mtime; a full sync of the same
	// file stores that effective value. Keeping the incremental write on the
	// same basis means parse-diff's raced guard -- which reads the freshly
	// parsed effective File.Mtime -- compares against a matching stored
	// file_mtime no matter whether the last write was incremental or full,
	// and shouldSkipCodex's storedMtime==effectiveMtime fast path stays
	// accurate. Plain JSONL agents (Claude/Gemini) keep the raw stat.
	incMtime := info.ModTime().UnixNano()
	if agent == parser.AgentCodex {
		incMtime = parser.CodexEffectiveMtime(file.Path, incMtime)
	}

	newMsgs, endedAt, consumed, err := parseFn(
		file.Path, inc.FileSize, inc.NextOrdinal, inc.LastEntryUUID,
	)
	if err != nil {
		if parser.IsIncrementalFullParseFallback(err) {
			log.Printf(
				"incremental %s %s: %v (explicit full parse fallback)",
				agent, file.Path, err,
			)
			// The fallback fires when appended lines update
			// already-stored rows (toolUseResult.agentId
			// linkage, same-message.id chunk merging). The
			// full parse must replace existing messages —
			// otherwise the append-only write path skips
			// rows whose ordinal ≤ maxOrd and the updates
			// are silently dropped.
			return processResult{forceReplace: true}, false
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
	var incHash string
	if agent == parser.AgentCodex {
		if hash, err := ComputeFileHashPrefix(file.Path, newOffset); err == nil {
			incHash = hash
		}
	}

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
					fileMtime:            incMtime,
					fileHash:             incHash,
					nextOrdinal:          inc.NextOrdinal,
					lastEntryUUID:        inc.LastEntryUUID,
					totalOutputTokens:    inc.TotalOutputTokens,
					peakContextTokens:    inc.PeakContextTokens,
					hasTotalOutputTokens: inc.HasTotalOutputTokens,
					hasPeakContextTokens: inc.HasPeakContextTokens,
				},
			}, true
		}
		return processResult{skip: true}, true
	}

	// Claude cross-sync split detection: when the first appended
	// assistant message shares its provider message id with the
	// last already-stored assistant message for this session, the
	// previous sync stopped mid-stream. The incremental path would
	// store the new chunk as a separate message instead of merging
	// it into the existing one — fall back to a full parse so the
	// chunk merge sees the whole run. forceReplace tells the
	// downstream write path to use ReplaceSessionMessages: the
	// merged tail reuses existing ordinals, so the default
	// append-only writeMessages would silently drop it.
	if agent == parser.AgentClaude {
		first := newMsgs[0]
		if first.Role == parser.RoleAssistant &&
			first.ClaudeMessageID != "" {
			if e.db.LastClaudeMessageID(inc.ID) ==
				first.ClaudeMessageID {
				log.Printf(
					"incremental %s %s: appended chunk shares"+
						" message.id with stored tail, full parse",
					agent, file.Path,
				)
				return processResult{forceReplace: true}, false
			}
		}
	}

	newUserCount := countUserMsgs(newMsgs)
	nextOrdinal := nextParsedOrdinal(inc.NextOrdinal, newMsgs)
	lastEntryUUID := lastParsedSourceUUID(inc.LastEntryUUID, newMsgs)

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
		// Accumulate from per-message values already bounded to the
		// per-message clamp the central pass applies to the stored rows, so
		// a corrupt new message cannot inflate the session aggregates past
		// what the persisted rows justify (parity with the full path, which
		// re-derives message-derived totals from the clamped rows).
		if msgHasOut {
			totalOut += clampedTokens(m.OutputTokens)
			hasTotalOut = true
		}
		if ctx := clampedTokens(m.ContextTokens); msgHasCtx &&
			(!hasPeakCtx || ctx > peakCtx) {
			peakCtx = ctx
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
			fileMtime:            incMtime,
			fileHash:             incHash,
			nextOrdinal:          nextOrdinal,
			lastEntryUUID:        lastEntryUUID,
			totalOutputTokens:    totalOut,
			peakContextTokens:    peakCtx,
			hasTotalOutputTokens: hasTotalOut,
			hasPeakContextTokens: hasPeakCtx,
		},
	}, true
}

func (e *Engine) shouldSkipCodex(
	path string, info os.FileInfo,
) bool {
	if e.forceParse { // parse-diff: always re-parse
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
	if !ok || storedSize != info.Size() {
		return false
	}
	if project, ok := e.db.GetProjectByPath(lookupPath); ok &&
		parser.NeedsProjectReparse(project) {
		return false
	}
	if e.db.GetDataVersionByPath(lookupPath) <
		db.CurrentDataVersion() {
		return false
	}
	fileMtime := info.ModTime().UnixNano()
	effectiveMtime := parser.CodexEffectiveMtime(path, fileMtime)
	if storedMtime == effectiveMtime {
		return true
	}
	return effectiveMtime > storedMtime &&
		fileMtime <= storedMtime &&
		!e.codexIndexSessionNameChanged(path)
}

// codexIndexNeedsRefreshSince reports whether a Codex session whose transcript
// predates the cutoff still needs a refresh because its session_index.jsonl
// title changed at or after the cutoff. It compares the index title to the
// stored session_name directly rather than gating on indexMtime > storedMtime:
// the incremental write folds the index mtime into the stored file_mtime, so a
// title-only rename whose index mtime is <= that stored value would otherwise
// be filtered out and the stale title would never resolve.
func (e *Engine) codexIndexNeedsRefreshSince(
	path string, cutoffNs int64,
) bool {
	indexMtime := parser.CodexEffectiveMtime(path, 0)
	if indexMtime == 0 || indexMtime < cutoffNs {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	if _, _, ok := e.db.GetFileInfoByPath(lookupPath); !ok {
		return false
	}
	return e.codexIndexSessionNameChanged(path)
}

func (e *Engine) codexIndexSessionNameChanged(path string) bool {
	uuid := parser.CodexSessionUUIDFromFilename(filepath.Base(path))
	if uuid == "" {
		return false
	}
	currentName := parser.LookupCodexThreadName(path, uuid)
	stored, err := e.db.GetSessionFull(
		context.Background(), e.idPrefix+"codex:"+uuid,
	)
	if err != nil || stored == nil {
		return true
	}
	storedName := ""
	if stored.SessionName != nil {
		storedName = strings.TrimSpace(*stored.SessionName)
	}
	return currentName != storedName
}

// classifyCodexIndexPath maps a Codex session_index.jsonl change to the
// session files whose stored title no longer matches the index. The live
// watcher sees this file only because its parent directory is watched
// shallowly (see ResolveCodexShallowWatchRoots); without this translation a
// title-only rename would not refresh until the next periodic sync, since the
// session transcript itself is untouched.
func (e *Engine) classifyCodexIndexPath(
	path string,
) []parser.DiscoveredFile {
	if filepath.Base(path) != parser.CodexSessionIndexFilename {
		return nil
	}
	indexDir := filepath.Dir(path)
	var sessionRoots []string
	for _, agDir := range e.agentDirs[parser.AgentCodex] {
		if agDir != "" && filepath.Dir(agDir) == indexDir {
			sessionRoots = append(sessionRoots, agDir)
		}
	}
	if len(sessionRoots) == 0 {
		return nil
	}
	titles := parser.CodexSessionIndexTitles(path)
	if len(titles) == 0 {
		return nil
	}

	var out []parser.DiscoveredFile
	for uuid, title := range titles {
		if !e.codexStoredNameDiffers(uuid, title) {
			continue
		}
		var candidates []parser.DiscoveredFile
		for _, root := range sessionRoots {
			if src := parser.FindCodexSourceFile(root, uuid); src != "" {
				candidates = append(candidates, parser.DiscoveredFile{
					Path:  src,
					Agent: parser.AgentCodex,
				})
			}
		}
		if len(candidates) == 0 {
			continue
		}
		// A UUID can exist in both sessions/ and archived_sessions/.
		// Prefer the path the DB already tracks so a title rename does
		// not reparse a stale duplicate over the stored copy.
		out = append(out, pickPreferredCodexDiscoveredFile(e.db, candidates))
	}
	return out
}

// codexStoredNameDiffers reports whether the stored session_name for a Codex
// session differs from the given index title. Unknown sessions return false:
// a brand-new session is synced through its own transcript event, not the
// index, so the index path only refreshes renames of already-synced sessions.
func (e *Engine) codexStoredNameDiffers(uuid, indexTitle string) bool {
	stored, err := e.db.GetSessionFull(
		context.Background(), e.idPrefix+"codex:"+uuid,
	)
	if err != nil || stored == nil {
		return false
	}
	storedName := ""
	if stored.SessionName != nil {
		storedName = strings.TrimSpace(*stored.SessionName)
	}
	return strings.TrimSpace(indexTitle) != storedName
}

func pickPreferredCodexDiscoveredFile(
	database *db.DB, candidates []parser.DiscoveredFile,
) parser.DiscoveredFile {
	if len(candidates) == 0 {
		return parser.DiscoveredFile{}
	}
	if id := parser.CodexSessionUUIDFromFilename(
		filepath.Base(candidates[0].Path),
	); id != "" {
		storedPath := filepath.Clean(database.GetSessionFilePath("codex:" + id))
		if storedPath != "" {
			for _, candidate := range candidates {
				if filepath.Clean(candidate.Path) == storedPath {
					return candidate
				}
			}
		}
	}
	chosen := candidates[0]
	for _, candidate := range candidates[1:] {
		if preferDiscoveredFile(candidate, chosen) {
			chosen = candidate
		}
	}
	return chosen
}

func (e *Engine) processCodex(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {

	// Fast path: skip by file_path + effective mtime (includes session_index.jsonl).
	if e.shouldSkipCodex(file.Path, info) {
		return processResult{skip: true}
	}

	projectNeedsReparse := e.pathNeedsProjectReparse(file.Path)
	forceReplace := false

	codexParseFn := func(
		path string, offset int64, startOrd int, _ string,
	) ([]parser.ParsedMessage, time.Time, int64, error) {
		return parser.ParseCodexSessionFrom(
			path, offset, startOrd, false,
		)
	}
	if res, ok := e.tryIncrementalJSONL(
		file, info, parser.AgentCodex, codexParseFn,
	); ok {
		if !projectNeedsReparse {
			// Force a full parse whenever the index title differs from the
			// stored session_name. A mtime gate (indexMtime > storedMtime) is
			// not enough here: the incremental write folds the index mtime into
			// the stored file_mtime, so a later rename whose index mtime is <=
			// that stored value slips past the gate. shouldSkipCodex's
			// storedMtime==effectiveMtime fast path would then skip the refresh
			// forever, stranding the stale title. Comparing the name directly
			// closes that window.
			if !e.codexIndexSessionNameChanged(file.Path) {
				return res
			}
			// The index title changed, so a full parse still needs to refresh
			// session metadata. Keep any fallback signal discovered while probing
			// appended bytes so existing rows rewritten by the full parse are not
			// dropped by the append-only write path.
			forceReplace = res.forceReplace
		}
	} else {
		forceReplace = res.forceReplace
	}

	sess, msgs, err := parser.ParseCodexSession(
		file.Path, e.machine, false,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{skip: true}
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
		forceReplace: forceReplace,
	}
}

func (e *Engine) processOpenCodeFormat(
	agent parser.AgentType,
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if dbPath, sessionID, ok := parseOpenCodeFormatSQLiteVirtualPath(
		agent, file.Path,
	); ok {
		sess, msgs, err := parseOpenCodeFormatSession(
			agent, dbPath, sessionID, e.machine,
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
	if filepath.Base(file.Path) == openCodeFormatDBName(agent) {
		metas, err := listOpenCodeFormatSessionMeta(agent, file.Path)
		if err != nil {
			return processResult{err: err}
		}
		storageIDs := openCodeFormatStorageSessionIDs(
			agent, filepath.Dir(file.Path),
		)
		var results []parser.ParseResult
		var sessionErrs []sessionParseError
		for _, meta := range metas {
			if _, ok := storageIDs[meta.SessionID]; ok {
				continue
			}
			_, storedMtime, ok := e.db.GetFileInfoByPath(meta.VirtualPath)
			// parse-diff: !e.forceParse disables the stored-state skip.
			if !e.forceParse && ok && storedMtime == meta.FileMtime &&
				e.db.GetDataVersionByPath(meta.VirtualPath) >=
					db.CurrentDataVersion() {
				continue
			}
			sess, msgs, err := parseOpenCodeFormatSession(
				agent, file.Path, meta.SessionID, e.machine,
			)
			if err != nil {
				if e.forceParse {
					sessionErrs = append(sessionErrs, sessionParseError{
						sessionID:   meta.SessionID,
						virtualPath: meta.VirtualPath,
						err:         err,
					})
				} else {
					log.Printf(
						"%s sqlite watch session %s: %v",
						agent, meta.SessionID, err,
					)
				}
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
		return processResult{
			results:      results,
			sessionErrs:  sessionErrs,
			forceReplace: true,
		}
	}
	if e.shouldSkipOpenCodeFormatByPath(agent, file.Path) {
		return processResult{skip: true}
	}

	sess, msgs, err := parseOpenCodeFormatFile(
		agent, file.Path, e.machine,
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

func (e *Engine) shouldSkipOpenCodeFormatByPath(
	agent parser.AgentType, path string,
) bool {
	if e.forceParse { // parse-diff: always re-parse
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}

	_, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
	if !ok {
		return false
	}

	sourceMtime, err := openCodeFormatSourceMtime(agent, path)
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
	// Use effective mtime = max(events.jsonl, workspace.yaml) so
	// that a new or updated workspace.yaml triggers a re-parse and
	// the stored mtime stays consistent with what we compare against
	// on subsequent syncs (preventing oscillation).
	effectiveMtime := copilotEffectiveMtime(file.Path, info)
	if e.shouldSkipCopilot(file.Path, info, effectiveMtime) {
		return processResult{skip: true}
	}

	sess, msgs, usageEvents, err := parser.ParseCopilotSession(
		file.Path, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	if effectiveMtime > sess.File.Mtime {
		sess.File.Mtime = effectiveMtime
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs, UsageEvents: usageEvents},
		},
	}
}

// copilotEffectiveMtime returns max(events.jsonl mtime,
// workspace.yaml mtime). For flat .jsonl sessions (no
// workspace.yaml sibling) it returns the events.jsonl mtime.
func copilotEffectiveMtime(eventsPath string, info os.FileInfo) int64 {
	m := info.ModTime().UnixNano()
	if filepath.Base(eventsPath) != "events.jsonl" {
		return m
	}
	yamlPath := filepath.Join(
		filepath.Dir(eventsPath), "workspace.yaml",
	)
	if yi, err := os.Stat(yamlPath); err == nil {
		if ym := yi.ModTime().UnixNano(); ym > m {
			m = ym
		}
	}
	return m
}

// classifyReasonixPath handles Reasonix session classification,
// extracted from classifyOnePath to stay within nilaway limits.
func (e *Engine) classifyReasonixPath(
	path string,
) (parser.DiscoveredFile, bool) {
	sep := string(filepath.Separator)
	for _, reasonixDir := range e.agentDirs[parser.AgentReasonix] {
		if reasonixDir == "" {
			continue
		}
		if rel, ok := isUnder(reasonixDir, path); ok {
			// Map .jsonl.meta sidecar events to sibling .jsonl
			if strings.HasSuffix(path, ".jsonl.meta") {
				jsonlPath := strings.TrimSuffix(path, ".meta")
				if _, err := os.Stat(jsonlPath); err != nil {
					continue
				}
				path = jsonlPath
				rel = strings.TrimSuffix(rel, ".meta")
			}
			if !strings.HasSuffix(path, ".jsonl") {
				continue
			}
			parts := strings.Split(rel, sep)

			// Project sessions: projects/{project}/sessions/{id}.jsonl
			// or projects/{project}/sessions/{id}/{id}.jsonl
			if len(parts) == 4 && parts[0] == "projects" &&
				parts[2] == "sessions" &&
				strings.HasSuffix(parts[3], ".jsonl") {
				return parser.DiscoveredFile{
					Path:    path,
					Project: parts[1],
					Agent:   parser.AgentReasonix,
				}, true
			}

			// Project sessions: projects/{project}/sessions/{id}/{id}.jsonl
			if len(parts) == 5 && parts[0] == "projects" &&
				parts[2] == "sessions" {
				base := strings.TrimSuffix(parts[4], ".jsonl")
				if base != "" && parts[3] == base {
					return parser.DiscoveredFile{
						Path:    path,
						Project: parts[1],
						Agent:   parser.AgentReasonix,
					}, true
				}
			}

			// Global or archive sessions
			if len(parts) == 2 {
				if (parts[0] == "sessions" || parts[0] == "archive") &&
					strings.HasSuffix(parts[1], ".jsonl") {
					return parser.DiscoveredFile{
						Path:  path,
						Agent: parser.AgentReasonix,
					}, true
				}
			}

			// Nested global or subagent: sessions/{id}/{id}.jsonl or sessions/subagents/{id}.jsonl
			if len(parts) == 3 {
				base := strings.TrimSuffix(parts[2], ".jsonl")
				if parts[0] == "sessions" &&
					(parts[1] == "subagents" ||
						parts[1] == base) {
					if base != "" {
						return parser.DiscoveredFile{
							Path:  path,
							Agent: parser.AgentReasonix,
						}, true
					}
				}
			}
		}
	}

	return parser.DiscoveredFile{}, false
}

func (e *Engine) processReasonix(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	effectiveInfo := reasonixEffectiveInfo(file.Path, info)
	if e.shouldSkipByPath(file.Path, effectiveInfo) {
		return processResult{skip: true}
	}

	sess, msgs, _, err := parser.ParseReasonixSession(
		file.Path, e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}

	// Use the discovered project only when metadata did not supply a
	// project via workspace_root.
	if file.Project != "" && sess.Project == "" {
		sess.Project = file.Project
	}

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	sess.File.Size = effectiveInfo.Size()
	sess.File.Mtime = effectiveInfo.ModTime().UnixNano()

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
	}
}

func reasonixEffectiveInfo(path string, info os.FileInfo) os.FileInfo {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	metaPath := path + ".meta"
	if metaInfo, err := os.Stat(metaPath); err == nil {
		size += metaInfo.Size()
		if metaMtime := metaInfo.ModTime().UnixNano(); metaMtime > mtime {
			mtime = metaMtime
		}
	}
	return fakeSnapshotInfo{fSize: size, fMtime: mtime}
}

// shouldSkipCopilot is like shouldSkipByPath but uses the
// pre-computed effectiveMtime (max of events.jsonl and
// workspace.yaml) for the mtime comparison, keeping the stored
// value consistent with what we compare against on next sync.
func (e *Engine) shouldSkipCopilot(
	path string, info os.FileInfo, effectiveMtime int64,
) bool {
	if e.forceParse { // parse-diff: always re-parse
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
	if !ok {
		return false
	}
	if storedSize != info.Size() || storedMtime != effectiveMtime {
		return false
	}
	if e.db.GetDataVersionByPath(lookupPath) <
		db.CurrentDataVersion() {
		return false
	}
	return true
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

func (e *Engine) processDeepSeekTUI(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseDeepSeekTUISession(
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
	inode, device := getFileIdentity(info)
	sess.File.Inode = inode
	sess.File.Device = device

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
			{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: sess.UsageEvents,
			},
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

func (e *Engine) processQClaw(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseQClawSession(
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

func (e *Engine) processVisualStudioCopilot(
	file parser.DiscoveredFile, _ os.FileInfo,
) processResult {
	// Resolve the physical trace path first. Discovery emits one
	// <traceFile>#<conversationID> work item per conversation; a watcher event
	// or single-session resync may instead pass a real trace file, which can
	// hold spans for several conversations.
	tracePath := file.Path
	var conversationIDs []string
	if resolved, conversationID, ok :=
		parser.ParseVisualStudioCopilotVirtualPath(file.Path); ok {
		tracePath = resolved
		conversationIDs = []string{conversationID}
	}

	// Skip on a fingerprint spanning every sibling trace file: a
	// conversation's transcript is rebuilt from all of them, so a change to any
	// sibling must defeat the skip even when the representative trace file is
	// unchanged. The primary-file stat alone would let a single-session resync
	// or watch fallback leave a session stale.
	size, mtime, err := parser.VisualStudioCopilotTraceFingerprintStrict(
		tracePath,
	)
	if err != nil {
		return processResult{err: err, noCacheSkip: true}
	}
	if e.shouldSkipByPath(
		file.Path, fakeSnapshotInfo{fSize: size, fMtime: mtime},
	) {
		return processResult{skip: true}
	}

	// A real trace file can hold spans for several conversations, so enumerate
	// them and emit each independently.
	if conversationIDs == nil {
		ids, err := parser.VisualStudioCopilotFileConversationIDs(file.Path)
		if err != nil {
			return processResult{err: err, noCacheSkip: true}
		}
		conversationIDs = ids
	}

	hash, hashErr := ComputeFileHash(tracePath)

	var results []parser.ParseResult
	for _, conversationID := range conversationIDs {
		sess, msgs, err := parser.ParseVisualStudioCopilotConversation(
			tracePath, conversationID, file.Project, e.machine,
		)
		if err != nil {
			return processResult{err: err, noCacheSkip: true}
		}
		if sess == nil {
			continue
		}
		if hashErr == nil {
			sess.File.Hash = hash
		}
		results = append(results, parser.ParseResult{
			Session: *sess, Messages: msgs,
		})
	}

	// forceReplace mirrors the other multi-session-per-source agents
	// (Zed, Kiro): each conversation's messages are fully re-derived from
	// all of its spans on every parse, so existing rows must be replaced
	// rather than appended.
	return processResult{
		results:      results,
		forceReplace: true,
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

func (e *Engine) processQwenPaw(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseQwenPawSession(
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

	// forceReplace: QwenPaw's _atomic_write_json rewrites the entire
	// sessions/<name>.json on every save, and ParseQwenPawSession
	// assigns Ordinal by position in agent.memory.content. If that
	// array is compacted, summarized, or reordered — common in
	// agent-memory frameworks — ordinals shift, and the append-only
	// writeMessages path would silently keep stale rows. Treat every
	// re-parse as a full rewrite, matching OpenCode / Antigravity.
	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs},
		},
		forceReplace: true,
	}
}

func (e *Engine) processZed(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if dbPath, sessionID, ok := parser.ParseZedSQLiteVirtualPath(file.Path); ok {
		result, err := parser.ParseZedThreadDirect(
			dbPath, sessionID, e.machine, info,
		)
		if err != nil {
			return processResult{err: err}
		}
		if result == nil {
			return processResult{}
		}
		if hash, err := ComputeFileHash(dbPath); err == nil {
			result.Session.File.Hash = hash
		}
		return processResult{
			results:      []parser.ParseResult{*result},
			forceReplace: true,
		}
	}
	conn, err := parser.OpenZedDB(file.Path)
	if err != nil {
		return processResult{err: err}
	}
	defer conn.Close()

	metas, err := parser.ListZedThreadMetas(conn, file.Path)
	if err != nil {
		return processResult{err: err}
	}

	hash, _ := ComputeFileHash(file.Path)

	var results []parser.ParseResult
	var sessionErrs []sessionParseError
	for _, meta := range metas {
		_, storedMtime, ok := e.db.GetFileInfoByPath(meta.VirtualPath)
		// parse-diff: !e.forceParse disables the stored-state skip.
		if !e.forceParse && ok && storedMtime == meta.FileMtime &&
			e.db.GetDataVersionByPath(meta.VirtualPath) >=
				db.CurrentDataVersion() {
			continue
		}
		result, err := parser.ParseZedThreadFromDB(
			conn, file.Path, meta.RawID, e.machine, info,
		)
		if err != nil {
			if e.forceParse {
				sessionErrs = append(sessionErrs, sessionParseError{
					sessionID:   meta.RawID,
					virtualPath: meta.VirtualPath,
					err:         err,
				})
			} else {
				log.Printf("zed thread %s: %v", meta.RawID, err)
			}
			continue
		}
		if result == nil {
			continue
		}
		if hash != "" {
			result.Session.File.Hash = hash
		}
		results = append(results, *result)
	}
	return processResult{
		results:      results,
		sessionErrs:  sessionErrs,
		forceReplace: true,
	}
}

func (e *Engine) processShelley(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if dbPath, sessionID, ok := parser.ParseShelleyVirtualPath(file.Path); ok {
		result, err := parser.ParseShelleyConversationDirect(
			dbPath, sessionID, e.machine, info,
		)
		if err != nil {
			return processResult{err: err}
		}
		if result == nil {
			return processResult{}
		}
		// File.Hash is the parser's per-conversation content fingerprint;
		// the whole-db hash would be identical across conversations and is
		// not used for Shelley change detection.
		return processResult{
			results:      []parser.ParseResult{*result},
			forceReplace: true,
		}
	}
	conn, err := parser.OpenShelleyDB(file.Path)
	if err != nil {
		return processResult{err: err}
	}
	defer conn.Close()

	metas, err := parser.ListShelleyConversationMetas(conn, file.Path)
	if err != nil {
		return processResult{err: err}
	}

	var results []parser.ParseResult
	var sessionErrs []sessionParseError
	for _, meta := range metas {
		lookupPath := meta.VirtualPath
		if e.pathRewriter != nil {
			lookupPath = e.pathRewriter(lookupPath)
		}
		_, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
		storedHash, _ := e.db.GetFileHashByPath(lookupPath)
		// parse-diff: !e.forceParse disables the stored-state skip.
		// FileMtime alone has second precision, so the content fingerprint
		// (stored in file_hash) catches same-second appends and in-place
		// rewrites; see shelleyChangeMtime in the parser.
		if !e.forceParse && ok && storedMtime == meta.FileMtime &&
			storedHash == meta.Fingerprint &&
			e.db.GetDataVersionByPath(lookupPath) >=
				db.CurrentDataVersion() {
			continue
		}
		result, err := parser.ParseShelleyConversationFromDB(
			conn, file.Path, meta.RawID, e.machine, info,
		)
		if err != nil {
			if e.forceParse {
				sessionErrs = append(sessionErrs, sessionParseError{
					sessionID:   meta.RawID,
					virtualPath: meta.VirtualPath,
					err:         err,
				})
			} else {
				log.Printf("shelley conversation %s: %v", meta.RawID, err)
			}
			continue
		}
		if result == nil {
			continue
		}
		results = append(results, *result)
	}
	return processResult{
		results:      results,
		sessionErrs:  sessionErrs,
		forceReplace: true,
	}
}

func (e *Engine) processKiro(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if dbPath, sessionID, ok := parser.ParseKiroSQLiteVirtualPath(file.Path); ok {
		sess, msgs, err := parser.ParseKiroSQLiteSession(
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
			forceReplace: true,
		}
	}
	if filepath.Base(file.Path) == "data.sqlite3" {
		store, err := parser.OpenKiroSQLiteStore(file.Path)
		if err != nil {
			return processResult{err: err}
		}
		defer store.Close()
		metas, err := store.ListSessionMeta()
		if err != nil {
			return processResult{err: err}
		}
		var results []parser.ParseResult
		var sessionErrs []sessionParseError
		for _, meta := range metas {
			_, storedMtime, ok := e.db.GetFileInfoByPath(
				meta.VirtualPath,
			)
			// parse-diff: !e.forceParse disables the stored-state skip.
			if !e.forceParse && ok && storedMtime == meta.FileMtime &&
				e.db.GetDataVersionByPath(meta.VirtualPath) >=
					db.CurrentDataVersion() {
				continue
			}
			sess, msgs, err := store.ParseSession(
				meta.SessionID, e.machine,
			)
			if err != nil {
				if e.forceParse {
					sessionErrs = append(sessionErrs, sessionParseError{
						sessionID:   meta.SessionID,
						virtualPath: meta.VirtualPath,
						err:         err,
					})
				} else {
					log.Printf(
						"kiro sqlite watch session %s: %v",
						meta.SessionID, err,
					)
				}
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
		return processResult{
			results:      results,
			sessionErrs:  sessionErrs,
			forceReplace: true,
		}
	}
	if e.isShadowedLegacyKiroPath(file.Path) {
		return processResult{skip: true}
	}
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

	if filepath.Base(file.Path) == "state.db" {
		results, err := parser.ParseHermesArchive(
			file.Path, file.Project, e.machine,
		)
		if err != nil {
			return processResult{err: err}
		}
		return processResult{results: results, forceReplace: true}
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

func (e *Engine) processWorkBuddy(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseWorkBuddySession(
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

func (e *Engine) processVibe(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	// Title/model/usage stats come from the sibling meta.json, so the
	// skip check and stored file info must account for it too, or a
	// meta.json-only update never refreshes those fields.
	effectiveInfo := vibeEffectiveInfo(file.Path, info)
	if e.shouldSkipByPath(file.Path, effectiveInfo) {
		return processResult{skip: true}
	}

	// Pass an empty project so the parser-derived project (from the
	// session's working directory) is kept. file.Project holds the
	// cryptic session directory name, which must not become the project.
	sess, msgs, usageEvents, err := parser.ParseVibeSessionWrapper(
		file.Path, "", e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if sess == nil {
		return processResult{}
	}
	sess.File.Size = effectiveInfo.Size()
	sess.File.Mtime = effectiveInfo.ModTime().UnixNano()

	hash, err := ComputeFileHash(file.Path)
	if err == nil {
		sess.File.Hash = hash
	}

	var excludedIDs []string
	lookupPath := file.Path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(file.Path)
	}
	existingIDs, err := e.db.ListSessionIDsByFilePath(
		lookupPath, string(parser.AgentVibe),
	)
	if err != nil {
		return processResult{err: err}
	}
	currentID := sess.ID
	currentPrefixedID := e.idPrefix + sess.ID
	fallbackID := "vibe:" + filepath.Base(filepath.Dir(file.Path))
	for _, id := range existingIDs {
		if id != currentID && id != currentPrefixedID {
			excludedIDs = append(excludedIDs, id)
		}
	}

	currentFallbackTrashed := sess.ID == fallbackID && e.isSessionTrashed(fallbackID)
	if e.isSessionBlocked(fallbackID) ||
		(sess.ID == fallbackID &&
			e.db.HasTrashedSessionByFilePath(lookupPath, string(parser.AgentVibe))) {
		if !currentFallbackTrashed && !slices.Contains(excludedIDs, sess.ID) {
			excludedIDs = append(excludedIDs, sess.ID)
		}
		return processResult{excludedSessionIDs: excludedIDs}
	}

	// Sessions parsed before meta.json existed (or was parseable) are stored
	// under the directory-name fallback ID. Keep excluding that legacy row even
	// if it predates file_path metadata and did not appear in the path lookup.
	if sess.ID != fallbackID && !slices.Contains(excludedIDs, fallbackID) {
		excludedIDs = append(excludedIDs, fallbackID)
	}

	return processResult{
		results: []parser.ParseResult{
			{Session: *sess, Messages: msgs, UsageEvents: usageEvents},
		},
		excludedSessionIDs: excludedIDs,
	}
}

func (e *Engine) isSessionBlocked(id string) bool {
	if e.idPrefix != "" && !strings.HasPrefix(id, e.idPrefix) {
		prefixed := e.idPrefix + id
		return e.db.IsSessionExcluded(prefixed) || e.db.IsSessionTrashed(prefixed)
	}
	if e.db.IsSessionExcluded(id) || e.db.IsSessionTrashed(id) {
		return true
	}
	return false
}

func (e *Engine) isSessionTrashed(id string) bool {
	if e.idPrefix != "" && !strings.HasPrefix(id, e.idPrefix) {
		return e.db.IsSessionTrashed(e.idPrefix + id)
	}
	return e.db.IsSessionTrashed(id)
}

// vibeEffectiveInfo returns size/mtime for a Vibe session that account
// for the sibling meta.json file: size is the sum of both files, and
// mtime is the larger of the two. Returns info unchanged when meta.json
// is absent or unreadable.
func vibeEffectiveInfo(path string, info os.FileInfo) os.FileInfo {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	metaPath := filepath.Join(filepath.Dir(path), "meta.json")
	if metaInfo, err := os.Stat(metaPath); err == nil {
		size += metaInfo.Size()
		if metaMtime := metaInfo.ModTime().UnixNano(); metaMtime > mtime {
			mtime = metaMtime
		}
	}
	return fakeSnapshotInfo{fSize: size, fMtime: mtime}
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

func (e *Engine) processGptme(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseGptmeSession(
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

// aiderFileUnchanged reports whether a physical aider history file is
// unchanged since the last sync. Aider sessions are stored under virtual
// "<history>#<idx>" paths, so the generic shouldSkipByPath (which looks the
// physical path up in the DB) never matches and would re-parse, re-hash, and
// re-write every run on every full/periodic sync. Mirror the per-virtual-path
// skip the other multi-session agents use (cf. kiroSQLitePendingSessionIDs).
//
// The whole file is skipped only when EVERY expected run row is known
// current: each run meta's virtual path must have a stored row whose size and
// mtime match this file's and whose data version is current. Size is checked
// alongside mtime so a same-mtime append/truncate is not wrongly skipped. If
// any run row is missing (e.g. a previous batch wrote only some runs, or a new run was
// appended whose row does not exist yet) or stale (an older data version, or
// resynced after a data-version bump while siblings were not), the file is
// re-parsed so the remaining sessions are repaired. Skipping on the first
// matching row would strand those runs forever. A run-less or unreadable
// file is treated as changed (never skipped) so it is retried.
func (e *Engine) aiderFileUnchanged(path string, info os.FileInfo) bool {
	metas, err := parser.ListAiderRunMetas(path)
	if err != nil || len(metas) == 0 {
		return false
	}
	mtime := info.ModTime().UnixNano()
	size := info.Size()
	current := db.CurrentDataVersion()
	expected := 0
	for _, m := range metas {
		// Header-only runs produce no session row, so the fan-out never
		// writes one for them; do not expect a stored row.
		if !m.HasMessages {
			continue
		}
		expected++
		lookupPath := m.VirtualPath
		if e.pathRewriter != nil {
			lookupPath = e.pathRewriter(lookupPath)
		}
		storedSize, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
		if !ok || storedSize != size || storedMtime != mtime ||
			e.db.GetDataVersionByPath(lookupPath) < current {
			// This run is missing or stale: do not skip the file, so the
			// fan-out re-parses and repairs every run. The size is compared
			// alongside mtime so a same-mtime append/truncate (which leaves
			// new or removed runs unsynced) is never wrongly skipped.
			return false
		}
	}
	// Skip only when at least one run was expected and all expected run rows
	// are current. A file whose runs all lack turns produces no sessions, so
	// there is nothing to skip-and-strand; re-parse it (cheap, capped read).
	return expected > 0
}

// aiderIdentityPath returns the canonical history-file path used to derive
// stable aider session IDs. During remote SSH sync the file is read from a
// random temp extraction dir, so hashing the on-disk path would re-key the
// run on every sync; rewriting it to its canonical remote path keeps the ID
// stable. Returns "" for local sync (no pathRewriter), which makes the
// parser fall back to the on-disk path -- the original local behavior.
func (e *Engine) aiderIdentityPath(historyPath string) string {
	if e.pathRewriter == nil {
		return ""
	}
	return e.pathRewriter(historyPath)
}

func (e *Engine) processAider(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	// Virtual path "<historyFile>#<runIdx>": parse that one run only. Used
	// when re-syncing a single session by its source path.
	if historyPath, idx, ok := parser.ParseAiderVirtualPath(file.Path); ok {
		sess, msgs, err := parser.ParseAiderRunWithID(
			historyPath, e.aiderIdentityPath(historyPath), idx, e.machine,
		)
		if err != nil {
			return processResult{err: err}
		}
		if sess == nil {
			return processResult{}
		}
		if hash, err := ComputeFileHash(historyPath); err == nil {
			sess.File.Hash = hash
		}
		return processResult{
			results:      []parser.ParseResult{{Session: *sess, Messages: msgs}},
			forceReplace: true,
		}
	}

	// parse-diff: !e.forceParse disables the stored-state skip so a forced
	// reparse re-reads already-synced aider files instead of skipping them.
	if !e.forceParse && e.aiderFileUnchanged(file.Path, info) {
		return processResult{skip: true}
	}

	// Physical history file: fan it out into one session per run. The file
	// is read and split once. The whole file shares one content hash, so
	// any write re-parses every run (acceptable: aider history is
	// append-mostly and a single capped read).
	results, err := parser.ParseAiderRunsWithID(
		file.Path, e.aiderIdentityPath(file.Path), e.machine,
	)
	if err != nil {
		return processResult{err: err}
	}
	if len(results) == 0 {
		return processResult{}
	}
	if hash, err := ComputeFileHash(file.Path); err == nil {
		for i := range results {
			results[i].Session.File.Hash = hash
		}
	}
	return processResult{
		results:      results,
		forceReplace: true,
	}
}

func (e *Engine) processAntigravity(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, usageEvents, err := parser.ParseAntigravitySession(
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
			{Session: *sess, Messages: msgs, UsageEvents: usageEvents},
		},
	}
}

func (e *Engine) processAntigravityCLI(
	file parser.DiscoveredFile, effectiveInfo os.FileInfo,
) processResult {
	// processFile supplies AntigravityCLIFileInfo here, so .db WAL/SHM
	// sidecars and .pb trajectory sidecars participate in skip checks.
	if e.shouldSkipByPath(file.Path, effectiveInfo) {
		return processResult{skip: true}
	}

	sess, msgs, usageEvents, parseStatus, err := parser.ParseAntigravityCLISessionWithStatus(
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
		needsRetry: parseStatus.NeedsRetry,
		results: []parser.ParseResult{
			{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: usageEvents,
			},
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

	var (
		sess *parser.ParsedSession
		msgs []parser.ParsedMessage
		err  error
	)
	if file.Agent == parser.AgentOMP {
		sess, msgs, err = parser.ParseOMPSession(file.Path, file.Project, e.machine)
	} else {
		sess, msgs, err = parser.ParsePiSession(file.Path, file.Project, e.machine)
	}
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

func (e *Engine) processQwen(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	if e.shouldSkipByPath(file.Path, info) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseQwenSession(
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

func (e *Engine) processCommandCode(
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	effectiveInfo := commandCodeEffectiveInfo(file.Path, info)
	if e.shouldSkipByPath(file.Path, effectiveInfo) {
		return processResult{skip: true}
	}

	sess, msgs, err := parser.ParseCommandCodeSession(
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
	sess.File.Size = effectiveInfo.Size()
	sess.File.Mtime = effectiveInfo.ModTime().UnixNano()

	return processResult{
		results: []parser.ParseResult{{
			Session:  *sess,
			Messages: msgs,
		}},
	}
}

func commandCodeEffectiveInfo(path string, info os.FileInfo) os.FileInfo {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
	if metaInfo, err := os.Stat(metaPath); err == nil {
		size += metaInfo.Size()
		if metaMtime := metaInfo.ModTime().UnixNano(); metaMtime > mtime {
			mtime = metaMtime
		}
	}
	return fakeSnapshotInfo{fSize: size, fMtime: mtime}
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
	ctx context.Context,
	file parser.DiscoveredFile, info os.FileInfo,
) processResult {
	// Extract session ID from filename: session-<uuid>.jsonl
	sessionID := "iflow:" + strings.TrimPrefix(strings.TrimSuffix(info.Name(), ".jsonl"), "session-")

	if e.shouldSkipFile(sessionID, info) {
		sess, _ := e.db.GetSession(
			ctx, e.idPrefix+sessionID,
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
		if p := parser.ExtractProjectFromCwdWithBranchContext(
			ctx, cwd, gitBranch,
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
	for _, v := range slices.Backward(calls) {
		if signals.IsFailure(v) {
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
	if e.refuseWriteInForceParse("RecomputeSignals") {
		return errors.New(
			"RecomputeSignals refused on report-only parse-diff engine",
		)
	}
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
	update, findings := computeSignalsAndSecrets(*sess, msgs)
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
	if err := e.db.ReplaceSessionSecretFindings(
		sessionID, findings, update.SecretLeakCount, update.SecretsRulesVersion,
	); err != nil {
		log.Printf("secrets: persist %s: %v", sessionID, err)
		return fmt.Errorf("persisting findings %s: %w", sessionID, err)
	}
	return nil
}

type pendingWrite struct {
	sess         parser.ParsedSession
	msgs         []parser.ParsedMessage
	usageEvents  []parser.ParsedUsageEvent
	needsRetry   bool
	forceReplace bool
}

func dataVersionForWrite(pw pendingWrite) int {
	if !pw.needsRetry {
		return db.CurrentDataVersion()
	}
	// Keep successfully written fallback content visible while
	// forcing the next sync to retry the higher-resolution source.
	v := db.CurrentDataVersion() - 1
	if v < 0 {
		return 0
	}
	return v
}

type worktreeProjectResolver func(
	machine, cwd, currentProject string,
) (string, bool)

func (e *Engine) loadWorktreeProjectResolver() worktreeProjectResolver {
	cache := map[string][]db.WorktreeProjectMapping{}
	failed := map[string]bool{}
	return func(machine, cwd, currentProject string) (string, bool) {
		if machine == "" {
			return currentProject, false
		}
		mappings, ok := cache[machine]
		if !ok {
			if failed[machine] {
				return currentProject, false
			}
			var err error
			mappings, err = e.db.ListActiveWorktreeProjectMappings(
				context.Background(), machine,
			)
			if err != nil {
				log.Printf(
					"load worktree project mappings for machine %s: %v",
					machine, err,
				)
				failed[machine] = true
				return currentProject, false
			}
			cache[machine] = mappings
		}
		if len(mappings) == 0 {
			return currentProject, false
		}
		return db.ResolveWorktreeProjectFromSortedMappings(
			mappings, cwd, currentProject,
		)
	}
}

func (e *Engine) writeBatch(
	batch []pendingWrite,
	writeMode syncWriteMode,
	forceReplace bool,
) (writtenSessions, writtenMessages, failedSessions int) {
	if writeMode == syncWriteBulk {
		return e.writeBatchBulk(batch, forceReplace)
	}

	resolveWorktreeProject := e.loadWorktreeProjectResolver()
	for _, pw := range batch {
		s, msgs, ok := e.prepareSessionWrite(
			pw, resolveWorktreeProject,
		)
		if !ok {
			continue
		}

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

		// UpsertSession first: the session row must exist
		// before messages can be inserted (FK constraint).
		// This is safe because writeBatch runs full parses
		// that always recompute all columns. For
		// incremental updates (writeIncremental), messages
		// are written first since the session already
		// exists.
		if err := e.db.UpsertSession(s); err != nil {
			if isIntentionalSessionSkip(err) {
				if pw.sess.File.Path != "" {
					e.cacheSkip(
						pw.sess.File.Path,
						pw.sess.File.Mtime,
					)
				}
				continue
			}
			log.Printf("upsert session %s: %v", s.ID, err)
			failedSessions++
			continue
		}

		replaceMessages := shouldReplaceFullParseMessages(
			pw, forceReplace, stale,
		)

		update, findings := computeSignalsAndSecrets(s, msgs)

		var werr error
		if replaceMessages {
			werr = e.db.ReplaceSessionContent(s.ID, msgs, update, findings)
		} else {
			werr = e.writeMessages(s.ID, msgs)
		}
		if werr != nil {
			log.Printf(
				"write messages for %s: %v",
				s.ID, werr,
			)
			failedSessions++
			continue
		}
		if err := e.db.ReplaceSessionUsageEvents(
			s.ID, toDBUsageEvents(s.ID, pw.usageEvents),
		); err != nil {
			log.Printf(
				"write usage events for %s: %v",
				s.ID, err,
			)
			failedSessions++
			continue
		}

		// Advance data_version only after the message write
		// succeeded. UpsertSession deliberately does not
		// touch this column so a transient write failure
		// won't leave the session marked at the current
		// parser version with stale messages.
		if err := e.db.SetSessionDataVersion(
			s.ID, dataVersionForWrite(pw),
		); err != nil {
			log.Printf(
				"set data_version for %s: %v", s.ID, err,
			)
		}

		if !replaceMessages {
			if err := e.db.UpdateSessionSignals(s.ID, update); err != nil {
				log.Printf("signals: update %s: %v", s.ID, err)
			}
			if err := e.db.ReplaceSessionSecretFindings(
				s.ID, findings, update.SecretLeakCount,
				update.SecretsRulesVersion); err != nil {
				log.Printf("secrets: persist %s: %v", s.ID, err)
			}
		}
		writtenSessions++
		writtenMessages += len(msgs)
	}
	return writtenSessions, writtenMessages, failedSessions
}

func (e *Engine) prepareSessionWrite(
	pw pendingWrite,
	resolveWorktreeProject worktreeProjectResolver,
) (db.Session, []db.Message, bool) {
	msgs := toDBMessages(pw, e.blockedResultCategories)
	s := toDBSession(pw)
	applySessionMessageDerivedFields(&s, msgs)
	e.applyRemoteRewrites(&s, msgs)
	if s.Cwd != "" && resolveWorktreeProject != nil {
		if mapped, ok := resolveWorktreeProject(
			s.Machine, s.Cwd, s.Project,
		); ok {
			s.Project = mapped
		}
	}

	if e.shouldPreserveOpenCodeFormatArchive(
		pw.sess.Agent, pw.sess.File.Path, s.ID,
		pw.sess.File.Mtime, derefString(s.FileHash), msgs,
	) {
		return db.Session{}, nil, false
	}
	if mergedMsgs, preserve, archived := e.reconcileVisualStudioCopilotArchive(
		pw.sess.Agent, s.ID, pw.sess.File.Size, msgs,
	); preserve {
		return db.Session{}, nil, false
	} else if mergedMsgs != nil {
		parsedMsgs := msgs
		msgs = mergedMsgs
		applyVisualStudioCopilotArchiveSessionFields(
			&s, archived, parsedMsgs, msgs,
		)
		applySessionMessageDerivedFields(&s, msgs)
		applySessionTokenTotalsFromMessages(&s, msgs)
	}
	// Snapshot, before sanitizing, whether the session's token aggregates
	// are derived from the per-message rows or the per-usage-event rows, by
	// matching the stored value against each source's raw sum/max. Aggregates
	// set directly from a session-level usage summary -- agents like
	// Warp/Vibe/Hermes/Zed -- must survive the per-row clamp untouched.
	// Source=="session" usage events mirror those same summary totals, so
	// exclude them from the event-derived detector and re-clamp path.
	msgTotal, msgHasOut, msgPeak, msgHasCtx := messageTokenTotals(msgs)
	evtTotal, evtHasOut, evtPeak, evtHasCtx := usageEventTokenTotals(
		pw.usageEvents, false,
	)
	totalFromMsgs := s.HasTotalOutputTokens == msgHasOut &&
		s.TotalOutputTokens == msgTotal
	totalFromEvts := s.HasTotalOutputTokens == evtHasOut &&
		s.TotalOutputTokens == evtTotal
	peakFromMsgs := s.HasPeakContextTokens == msgHasCtx &&
		s.PeakContextTokens == msgPeak
	peakFromEvts := s.HasPeakContextTokens == evtHasCtx &&
		s.PeakContextTokens == evtPeak

	// Central validation/sanitization pass: every session write flows
	// through here so all agents are covered uniformly. Stats are discarded
	// for now; a later anomaly-counter task will surface them.
	_ = validateAndSanitize(&s, msgs, nil)

	// A per-row token clamp must not leave an inflated value stranded in a
	// row-derived session total while the row that produced it was clamped.
	// Re-derive a matched aggregate from its now-clamped source (messages
	// clamped above; usage events clamped on the fly the same way
	// toDBUsageEvents will store them). Summary-derived aggregates match
	// neither source and are left as-is. The sum is re-summed from clamped
	// rows rather than clamped to the per-row bound, so a legitimately large
	// total over many rows is preserved. Re-deriving is a no-op when nothing
	// was clamped, keeping the pass idempotent. Messages take precedence when
	// both sources match (identical values).
	if totalFromMsgs {
		t, h, _, _ := messageTokenTotals(msgs)
		s.TotalOutputTokens, s.HasTotalOutputTokens = t, h
	} else if totalFromEvts {
		t, h, _, _ := usageEventTokenTotals(pw.usageEvents, true)
		s.TotalOutputTokens, s.HasTotalOutputTokens = t, h
	}
	if peakFromMsgs {
		_, _, p, h := messageTokenTotals(msgs)
		s.PeakContextTokens, s.HasPeakContextTokens = p, h
	} else if peakFromEvts {
		_, _, p, h := usageEventTokenTotals(pw.usageEvents, true)
		s.PeakContextTokens, s.HasPeakContextTokens = p, h
	}
	return s, msgs, true
}

func applySessionMessageDerivedFields(s *db.Session, msgs []db.Message) {
	s.MessageCount, s.UserMessageCount = postFilterCounts(msgs)
	s.IsAutomated = db.IsAutomatedTranscript(
		s.UserMessageCount, msgs, s.FirstMessage,
	)
}

// messageTokenTotals computes the message-derived session token
// aggregates: the sum of per-message output tokens and the peak
// per-message context tokens, each with a presence flag. It is the
// canonical derivation shared by applySessionTokenTotalsFromMessages and
// the post-sanitize reconciliation that re-derives message-derived totals
// from the clamped rows. Absent values return 0 with a false presence.
func messageTokenTotals(
	msgs []db.Message,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool) {
	for _, msg := range msgs {
		if msg.HasOutputTokens {
			hasOut = true
			totalOut += msg.OutputTokens
		}
		if msg.HasContextTokens {
			hasCtx = true
			if msg.ContextTokens > peakCtx {
				peakCtx = msg.ContextTokens
			}
		}
	}
	return totalOut, hasOut, peakCtx, hasCtx
}

func applySessionTokenTotalsFromMessages(s *db.Session, msgs []db.Message) {
	totalOut, hasOut, peakCtx, hasCtx := messageTokenTotals(msgs)
	s.TotalOutputTokens = totalOut
	s.HasTotalOutputTokens = hasOut
	s.PeakContextTokens = peakCtx
	s.HasPeakContextTokens = hasCtx
}

// usageEventTokenTotals computes event-derived session token aggregates through
// parser.UsageEventTokenAggregate -- the same rollup per-turn event parsers use
// to populate stored session totals (positive output summed, peak full context
// = input + cache-creation + cache-read where positive). Session-summary usage
// events mirror parser summary totals rather than per-turn rows, so they are
// excluded from this detector and re-clamp path. When clamp is true each
// included event token field is first bounded to the per-row plausibility cap,
// matching how sanitizeUsageEvent bounds the stored usage_event row.
func usageEventTokenTotals(
	events []parser.ParsedUsageEvent, clamp bool,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool) {
	rolled := make([]parser.ParsedUsageEvent, 0, len(events))
	for _, ev := range events {
		if ev.Source == "session" {
			continue
		}
		rolled = append(rolled, ev)
	}
	if clamp {
		for i, ev := range rolled {
			ev.InputTokens = clampedTokens(ev.InputTokens)
			ev.OutputTokens = clampedTokens(ev.OutputTokens)
			ev.CacheCreationInputTokens = clampedTokens(
				ev.CacheCreationInputTokens,
			)
			ev.CacheReadInputTokens = clampedTokens(ev.CacheReadInputTokens)
			rolled[i] = ev
		}
	}
	return parser.UsageEventTokenAggregate(rolled)
}

func applyVisualStudioCopilotArchiveSessionFields(
	s *db.Session, archived *db.Session,
	parsedMsgs, mergedMsgs []db.Message,
) {
	if archived == nil {
		return
	}
	archiveExtendsBounds := sessionTimeBefore(
		archived.StartedAt, s.StartedAt,
	) || sessionTimeAfter(archived.EndedAt, s.EndedAt)
	if !visualStudioCopilotMergedFirstMessageFromParsed(
		parsedMsgs, mergedMsgs,
	) {
		s.FirstMessage = cloneStringPtr(archived.FirstMessage)
	}
	if archiveExtendsBounds || stringPtrEmpty(s.SessionName) {
		s.SessionName = cloneStringPtr(archived.SessionName)
	}
	s.StartedAt = earlierSessionTime(archived.StartedAt, s.StartedAt)
	s.EndedAt = laterSessionTime(archived.EndedAt, s.EndedAt)
}

func visualStudioCopilotMergedFirstMessageFromParsed(
	parsed, merged []db.Message,
) bool {
	if len(parsed) == 0 || len(merged) == 0 {
		return false
	}
	mergedFirst := merged[0]
	for _, parsedMsg := range parsed {
		if visualStudioCopilotMessagePresenceKey(parsedMsg) !=
			visualStudioCopilotMessagePresenceKey(mergedFirst) {
			continue
		}
		return !visualStudioCopilotMessageLooksIncomplete(
			parsedMsg, mergedFirst,
		) && !visualStudioCopilotMessageHasArchiveUpdate(
			mergedFirst, parsedMsg,
		)
	}
	return false
}

func stringPtrEmpty(v *string) bool {
	return v == nil || strings.TrimSpace(*v) == ""
}

func cloneStringPtr(v *string) *string {
	if v == nil {
		return nil
	}
	clone := *v
	return &clone
}

func sessionTimeBefore(a, b *string) bool {
	return sessionTimeCompares(a, b, func(aTime, bTime time.Time) bool {
		return aTime.Before(bTime)
	})
}

func sessionTimeAfter(a, b *string) bool {
	return sessionTimeCompares(a, b, func(aTime, bTime time.Time) bool {
		return aTime.After(bTime)
	})
}

func sessionTimeCompares(
	a, b *string, compare func(time.Time, time.Time) bool,
) bool {
	if a == nil || b == nil {
		return a != nil && b == nil
	}
	aTime, aErr := time.Parse(time.RFC3339Nano, *a)
	bTime, bErr := time.Parse(time.RFC3339Nano, *b)
	if aErr != nil || bErr != nil {
		return false
	}
	return compare(aTime, bTime)
}

func earlierSessionTime(a, b *string) *string {
	return chooseSessionTime(a, b, func(aTime, bTime time.Time) bool {
		return aTime.Before(bTime)
	})
}

func laterSessionTime(a, b *string) *string {
	return chooseSessionTime(a, b, func(aTime, bTime time.Time) bool {
		return aTime.After(bTime)
	})
}

func chooseSessionTime(
	a, b *string, chooseA func(time.Time, time.Time) bool,
) *string {
	switch {
	case a == nil:
		return cloneStringPtr(b)
	case b == nil:
		return cloneStringPtr(a)
	}
	aTime, aErr := time.Parse(time.RFC3339Nano, *a)
	bTime, bErr := time.Parse(time.RFC3339Nano, *b)
	switch {
	case aErr != nil:
		return cloneStringPtr(b)
	case bErr != nil:
		return cloneStringPtr(a)
	case chooseA(aTime, bTime):
		return cloneStringPtr(a)
	default:
		return cloneStringPtr(b)
	}
}

// reconcileVisualStudioCopilotArchive returns either a preserved-archive skip
// or a merged transcript for an incomplete Visual Studio Copilot reparse. A
// conversation's transcript is rebuilt from every sibling trace file and
// written with full message replacement, so when a sibling is rotated away or
// deleted the reparse can see fewer spans or weaker span metadata and would
// otherwise drop messages and tool results already stored in SQLite. If a
// remaining trace gained richer data or new messages, merge those updates into
// the archived transcript while retaining archived-only messages.
func (e *Engine) reconcileVisualStudioCopilotArchive(
	agent parser.AgentType, sessionID string,
	currentSize int64, currentMsgs []db.Message,
) (merged []db.Message, preserve bool, archived *db.Session) {
	if agent != parser.AgentVSCopilot {
		return nil, false, nil
	}
	stored, err := e.db.GetSessionFull(context.Background(), sessionID)
	if err != nil || stored == nil {
		return nil, false, nil
	}
	storedSize := derefInt64(stored.FileSize)
	storedMsgs, err := e.db.GetAllMessages(context.Background(), sessionID)
	if err != nil || len(storedMsgs) == 0 {
		return nil, false, nil
	}
	decision := visualStudioCopilotArchiveDecision(
		currentMsgs, storedMsgs,
	)
	if decision.preserve {
		log.Printf(
			"preserve %s %s: reparse looks incomplete relative to archived "+
				"transcript (%d stored messages, %d parsed messages, "+
				"composite trace %d->%d bytes)",
			agent, sessionID, len(storedMsgs), len(currentMsgs),
			storedSize, currentSize,
		)
		return storedMsgs, false, stored
	}
	if decision.merged != nil {
		log.Printf(
			"merge %s %s: reparse updated archived messages while "+
				"retaining archived transcript rows (%d stored "+
				"messages, %d parsed messages, composite trace "+
				"%d->%d bytes)",
			agent, sessionID, len(storedMsgs), len(currentMsgs),
			storedSize, currentSize,
		)
		return decision.merged, false, stored
	}
	return nil, false, nil
}

type visualStudioCopilotArchiveReconcile struct {
	preserve bool
	merged   []db.Message
}

func visualStudioCopilotArchiveDecision(
	parsed, stored []db.Message,
) visualStudioCopilotArchiveReconcile {
	if len(stored) == 0 {
		return visualStudioCopilotArchiveReconcile{}
	}
	if parsed == nil {
		return visualStudioCopilotArchiveReconcile{preserve: true}
	}

	storedByKey := make(map[string][]int, len(stored))
	for i, msg := range stored {
		key := visualStudioCopilotMessagePresenceKey(msg)
		storedByKey[key] = append(storedByKey[key], i)
	}
	matchedStored := make([]bool, len(stored))
	updates := make(map[int]db.Message)
	additions := make([]db.Message, 0)
	hasIncomplete := false
	for _, parsedMsg := range parsed {
		key := visualStudioCopilotMessagePresenceKey(parsedMsg)
		candidates := storedByKey[key]
		if len(candidates) == 0 {
			additions = append(additions, parsedMsg)
			continue
		}
		storedIndex := candidates[0]
		storedByKey[key] = candidates[1:]
		matchedStored[storedIndex] = true
		storedMsg := stored[storedIndex]
		incomplete := visualStudioCopilotMessageLooksIncomplete(
			parsedMsg, storedMsg,
		)
		if incomplete {
			hasIncomplete = true
		}
		if !incomplete &&
			visualStudioCopilotMessageHasArchiveUpdate(
				parsedMsg, storedMsg,
			) {
			updates[storedIndex] = parsedMsg
		}
	}
	fallbackMatched := false
	additions, fallbackMatched = visualStudioCopilotResolveArchiveAdditions(
		stored, matchedStored, updates, additions, &hasIncomplete,
	)
	hasArchiveOnly := false
	for _, matched := range matchedStored {
		if !matched {
			hasArchiveOnly = true
			break
		}
	}
	if hasIncomplete || hasArchiveOnly || fallbackMatched {
		if len(updates) > 0 || len(additions) > 0 ||
			(fallbackMatched && !hasIncomplete) {
			return visualStudioCopilotArchiveReconcile{
				merged: visualStudioCopilotMergeArchiveMessages(
					stored, updates, additions,
				),
			}
		}
		return visualStudioCopilotArchiveReconcile{preserve: true}
	}
	return visualStudioCopilotArchiveReconcile{}
}

func visualStudioCopilotResolveArchiveAdditions(
	stored []db.Message,
	matchedStored []bool,
	updates map[int]db.Message,
	additions []db.Message,
	hasIncomplete *bool,
) ([]db.Message, bool) {
	matched := false
	unresolved := additions[:0]
	for _, parsedMsg := range additions {
		storedIndex, ok := visualStudioCopilotArchiveFallbackMatch(
			parsedMsg, stored, matchedStored,
		)
		if !ok {
			unresolved = append(unresolved, parsedMsg)
			continue
		}
		matched = true
		matchedStored[storedIndex] = true
		storedMsg := stored[storedIndex]
		incomplete := visualStudioCopilotMessageLooksIncomplete(
			parsedMsg, storedMsg,
		)
		if incomplete {
			*hasIncomplete = true
			continue
		}
		update := visualStudioCopilotArchiveFallbackUpdate(
			parsedMsg, storedMsg,
		)
		if visualStudioCopilotMessageHasArchiveUpdate(update, storedMsg) {
			updates[storedIndex] = update
		}
	}
	return unresolved, matched
}

func visualStudioCopilotArchiveFallbackMatch(
	parsed db.Message,
	stored []db.Message,
	matchedStored []bool,
) (int, bool) {
	match := -1
	for i, storedMsg := range stored {
		if matchedStored[i] {
			continue
		}
		if !visualStudioCopilotMessagesFallbackMatch(parsed, storedMsg) {
			continue
		}
		if match != -1 {
			return 0, false
		}
		match = i
	}
	if match == -1 {
		return 0, false
	}
	return match, true
}

func visualStudioCopilotMessagesFallbackMatch(
	parsed, stored db.Message,
) bool {
	if parsed.Role != stored.Role {
		return false
	}
	if visualStudioCopilotMessagesShareToolIdentity(parsed, stored) {
		return true
	}
	return visualStudioCopilotMessagesShareContentIdentity(parsed, stored)
}

func visualStudioCopilotMessagesShareToolIdentity(
	parsed, stored db.Message,
) bool {
	if len(parsed.ToolCalls) == 0 || len(stored.ToolCalls) == 0 {
		return false
	}
	parsedIDs := make(map[string]string, len(parsed.ToolCalls))
	for _, call := range parsed.ToolCalls {
		id := strings.TrimSpace(call.ToolUseID)
		if id == "" {
			continue
		}
		parsedIDs[id] = strings.TrimSpace(call.ToolName)
	}
	for _, call := range stored.ToolCalls {
		id := strings.TrimSpace(call.ToolUseID)
		if id == "" {
			continue
		}
		parsedName, ok := parsedIDs[id]
		if !ok {
			continue
		}
		storedName := strings.TrimSpace(call.ToolName)
		if parsedName != "" && storedName != "" &&
			parsedName != storedName {
			continue
		}
		return true
	}
	return false
}

func visualStudioCopilotMessagesShareContentIdentity(
	parsed, stored db.Message,
) bool {
	if len(parsed.ToolCalls) > 0 || len(stored.ToolCalls) > 0 {
		return false
	}
	switch parsed.Role {
	case string(parser.RoleAssistant), string(parser.RoleUser):
	default:
		return false
	}
	return parsed.Content != "" && parsed.Content == stored.Content
}

func visualStudioCopilotArchiveFallbackUpdate(
	parsed, stored db.Message,
) db.Message {
	update := parsed
	// A duplicate span can be flushed later with a different timestamp; keep
	// the archived timestamp as the transcript anchor while taking any richer
	// parsed payload such as tool results or token usage.
	update.Timestamp = stored.Timestamp
	return update
}

func visualStudioCopilotMergeArchiveMessages(
	stored []db.Message, updates map[int]db.Message,
	additions []db.Message,
) []db.Message {
	merged := make([]db.Message, 0, len(stored)+len(additions))
	merged = append(merged, stored...)
	for index, msg := range updates {
		merged[index] = msg
	}
	merged = append(merged, additions...)
	if len(additions) > 0 {
		slices.SortStableFunc(
			merged, compareVisualStudioCopilotMessageOrder,
		)
	}
	for i := range merged {
		merged[i].Ordinal = i
	}
	return merged
}

func compareVisualStudioCopilotMessageOrder(a, b db.Message) int {
	aTime, aOK := visualStudioCopilotMessageTime(a)
	bTime, bOK := visualStudioCopilotMessageTime(b)
	if aOK && bOK {
		switch {
		case aTime.Before(bTime):
			return -1
		case aTime.After(bTime):
			return 1
		default:
			return 0
		}
	}
	if aOK {
		return -1
	}
	if bOK {
		return 1
	}
	return 0
}

func visualStudioCopilotMessageTime(msg db.Message) (time.Time, bool) {
	if msg.Timestamp == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, msg.Timestamp)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func visualStudioCopilotMessagePresenceKey(msg db.Message) string {
	if msg.Timestamp != "" {
		return msg.Role + "\x00time\x00" + msg.Timestamp
	}
	if msg.SourceUUID != "" {
		return msg.Role + "\x00source\x00" + msg.SourceUUID
	}
	return fmt.Sprintf("%s\x00ordinal\x00%d", msg.Role, msg.Ordinal)
}

func visualStudioCopilotMessageLooksIncomplete(
	parsed, stored db.Message,
) bool {
	if parsed.Role != stored.Role {
		return false
	}
	// Stored rows are sanitized and length-adjusted on write; measure the
	// parsed side the same way so a reparse that only stripped control bytes
	// is not judged shorter and allowed to bypass archive preservation.
	p := sanitizedForArchiveCompare(parsed)
	if p.ContentLength < stored.ContentLength {
		return true
	}
	if stored.HasThinking && !p.HasThinking {
		return true
	}
	if stored.HasOutputTokens &&
		(!p.HasOutputTokens ||
			p.OutputTokens < stored.OutputTokens) {
		return true
	}
	if stored.HasContextTokens &&
		(!p.HasContextTokens ||
			p.ContextTokens < stored.ContextTokens) {
		return true
	}
	if len(p.ToolCalls) < len(stored.ToolCalls) {
		return true
	}
	if countToolResultEvents(p.ToolCalls) <
		countToolResultEvents(stored.ToolCalls) {
		return true
	}
	return countToolResultContentLength(p.ToolCalls) <
		countToolResultContentLength(stored.ToolCalls)
}

// sanitizedForArchiveCompare returns a copy of m with the same
// validation/sanitization stored rows receive on write (control runes
// stripped, ContentLength delta-adjusted, tokens clamped), so the VS Copilot
// archive reconcile compares freshly parsed messages against archived rows
// like-for-like. The copy is shallow; sanitizeMessage only rewrites value
// fields, leaving the shared ToolCalls slice untouched.
func sanitizedForArchiveCompare(m db.Message) db.Message {
	_ = sanitizeMessage(&m)
	return m
}

func visualStudioCopilotMessageHasArchiveUpdate(
	parsed, stored db.Message,
) bool {
	if parsed.Role != stored.Role {
		return false
	}
	// Stored rows are sanitized and length-adjusted on write, but the parsed
	// message still carries raw content here. Compare a sanitized copy so a
	// reparse that differs only in stripped control bytes is not treated as
	// an archive update, preserving idempotency.
	p := sanitizedForArchiveCompare(parsed)
	if p.ContentLength > stored.ContentLength {
		return true
	}
	if p.ContentLength == stored.ContentLength &&
		p.Content != stored.Content {
		return true
	}
	if p.HasThinking && (!stored.HasThinking ||
		p.ThinkingText != stored.ThinkingText) {
		return true
	}
	if p.HasOutputTokens &&
		(!stored.HasOutputTokens ||
			p.OutputTokens > stored.OutputTokens) {
		return true
	}
	if p.HasContextTokens &&
		(!stored.HasContextTokens ||
			p.ContextTokens > stored.ContextTokens) {
		return true
	}
	if string(p.TokenUsage) != "" &&
		string(p.TokenUsage) != string(stored.TokenUsage) {
		return true
	}
	return visualStudioCopilotToolCallsHaveArchiveUpdate(
		p.ToolCalls, stored.ToolCalls,
	)
}

func visualStudioCopilotToolCallsHaveArchiveUpdate(
	parsed, stored []db.ToolCall,
) bool {
	if len(parsed) > len(stored) {
		return true
	}
	for i := 0; i < len(parsed) && i < len(stored); i++ {
		if visualStudioCopilotToolCallHasArchiveUpdate(
			parsed[i], stored[i],
		) {
			return true
		}
	}
	return false
}

func visualStudioCopilotToolCallHasArchiveUpdate(
	parsed, stored db.ToolCall,
) bool {
	if parsed.ResultContentLength > stored.ResultContentLength {
		return true
	}
	if parsed.ResultContentLength == stored.ResultContentLength &&
		parsed.ResultContent != "" &&
		parsed.ResultContent != stored.ResultContent {
		return true
	}
	if len(parsed.ResultEvents) > len(stored.ResultEvents) {
		return true
	}
	for i := 0; i < len(parsed.ResultEvents) &&
		i < len(stored.ResultEvents); i++ {
		parsedEvent := parsed.ResultEvents[i]
		storedEvent := stored.ResultEvents[i]
		if parsedEvent.ContentLength > storedEvent.ContentLength {
			return true
		}
		if parsedEvent.ContentLength == storedEvent.ContentLength &&
			parsedEvent.Content != "" &&
			parsedEvent.Content != storedEvent.Content {
			return true
		}
		if parsedEvent.Status != "" &&
			parsedEvent.Status != storedEvent.Status {
			return true
		}
	}
	return false
}

func countToolResultContentLength(calls []db.ToolCall) int {
	total := 0
	for _, call := range calls {
		total += call.ResultContentLength
		for _, event := range call.ResultEvents {
			total += event.ContentLength
		}
	}
	return total
}

type batchSourceFile struct {
	path  string
	mtime int64
}

func (e *Engine) writeBatchBulk(
	batch []pendingWrite, forceReplace bool,
) (writtenSessions, writtenMessages, failedSessions int) {
	writes := make([]db.SessionBatchWrite, 0, len(batch))
	sources := make(map[string]batchSourceFile, len(batch))
	resolveWorktreeProject := e.loadWorktreeProjectResolver()

	for _, pw := range batch {
		tPrep := time.Now()
		s, msgs, ok := e.prepareSessionWrite(
			pw, resolveWorktreeProject,
		)
		e.phaseStats.PrepNanos.Add(int64(time.Since(tPrep)))
		if !ok {
			continue
		}
		replaceMessages := shouldReplaceFullParseMessages(
			pw, forceReplace, false,
		)
		tScan := time.Now()
		update, findings := computeSignalsAndSecrets(s, msgs)
		e.phaseStats.ScanNanos.Add(int64(time.Since(tScan)))
		writes = append(writes, db.SessionBatchWrite{
			Session:         s,
			Messages:        msgs,
			UsageEvents:     toDBUsageEvents(s.ID, pw.usageEvents),
			Signals:         update,
			Findings:        findings,
			DataVersion:     dataVersionForWrite(pw),
			ReplaceMessages: replaceMessages,
		})
		if pw.sess.File.Path != "" {
			sources[s.ID] = batchSourceFile{
				path:  pw.sess.File.Path,
				mtime: pw.sess.File.Mtime,
			}
		}
	}
	if len(writes) == 0 {
		return 0, 0, 0
	}

	tWrite := time.Now()
	result, err := e.db.WriteSessionBatch(writes)
	e.phaseStats.WriteNanos.Add(int64(time.Since(tWrite)))
	e.phaseStats.Batches.Add(1)
	e.phaseStats.WriteBatchSize.Add(int64(len(writes)))
	e.phaseStats.BatchedWrites.Add(int64(result.WrittenSessions))
	if err != nil {
		log.Printf("write session batch: %v", err)
		return 0, 0, len(writes)
	}
	for _, id := range result.ExcludedIDs {
		if source, ok := sources[id]; ok && source.path != "" {
			e.cacheSkip(source.path, source.mtime)
		}
	}
	for _, err := range result.Errors {
		log.Printf("write session batch: %v", err)
	}
	return result.WrittenSessions,
		result.WrittenMessages,
		result.FailedSessions
}

func shouldReplaceFullParseMessages(
	pw pendingWrite, forceReplace, stale bool,
) bool {
	return forceReplace || pw.forceReplace || pw.needsRetry || stale ||
		pw.sess.Agent == parser.AgentCowork ||
		isOpenCodeFormatStorageAgent(pw.sess.Agent) ||
		pw.sess.Agent == parser.AgentVSCopilot ||
		pw.sess.Agent == parser.AgentAntigravity ||
		pw.sess.Agent == parser.AgentAntigravityCLI ||
		pw.sess.Agent == parser.AgentQwenPaw ||
		// Vibe pairs later tool-result carrier records back to an
		// earlier assistant tool call. An incremental append would
		// only add the new ordinals and leave the existing tool call's
		// result_content empty, so force a full replace.
		pw.sess.Agent == parser.AgentVibe ||
		pw.sess.Agent == parser.AgentReasonix
}

// writeIncremental appends new messages and partially updates
// session metadata without overwriting columns that are not
// recomputed during incremental parsing (e.g. parent_session_id,
// relationship_type). Codex refreshes file_hash because parse-diff
// uses it as the transcript fingerprint for raced-skew detection.
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
	// The incremental append path bypasses prepareSessionWrite, so run
	// the central validation/sanitization pass on the new message rows
	// here to keep coverage uniform across write paths.
	_ = validateAndSanitize(nil, dbMsgs, nil)

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
	// Run the appended ended_at through the same timestamp plausibility
	// check the full path applies in sanitizeSession, so an implausible
	// appended timestamp is blanked here instead of persisting via the
	// incremental path while a full sync of the same file would blank it
	// (an incremental-vs-full parity divergence). The session token
	// aggregates (totalOutputTokens/peakContextTokens) are accumulated from
	// per-message values already clamped to the per-message bound (see the
	// clampedTokens calls feeding this update), so a corrupt new message
	// cannot inflate them past what the stored rows justify -- parity with
	// the full path, which re-derives message-derived totals from the
	// clamped rows. The sum itself is not clamped to the per-message bound,
	// since a long session legitimately exceeds it.
	endedAt, _ = blankImplausibleTimestampPtr(endedAt)

	if err := e.db.WriteSessionIncremental(
		inc.sessionID,
		dbMsgs,
		db.IncrementalSessionUpdate{
			EndedAt:              endedAt,
			MsgCount:             msgCount,
			UserMsgCount:         userMsgCount,
			FileSize:             inc.fileSize,
			FileMtime:            inc.fileMtime,
			FileHash:             strPtr(inc.fileHash),
			NextOrdinal:          inc.nextOrdinal,
			LastEntryUUID:        inc.lastEntryUUID,
			TotalOutputTokens:    inc.totalOutputTokens,
			PeakContextTokens:    inc.peakContextTokens,
			HasTotalOutputTokens: inc.hasTotalOutputTokens,
			HasPeakContextTokens: inc.hasPeakContextTokens,
		},
	); err != nil {
		return fmt.Errorf(
			"incremental write %s: %w",
			inc.sessionID, err,
		)
	}

	if err := e.applyWorktreeMappingToSingleSession(
		inc.sessionID,
	); err != nil {
		return err
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
// writeSessionFull returns nil on success, a session skip
// sentinel for intentional skips, or another error for real
// failures.
func (e *Engine) writeSessionFull(pw pendingWrite) error {
	resolveWorktreeProject := e.loadWorktreeProjectResolver()
	return e.writeSessionFullWithResolver(pw, resolveWorktreeProject)
}

func (e *Engine) writeSessionFullWithResolver(
	pw pendingWrite,
	resolveWorktreeProject worktreeProjectResolver,
) error {
	s, msgs, ok := e.prepareSessionWrite(
		pw, resolveWorktreeProject,
	)
	if !ok {
		return errSessionPreserved
	}
	if err := e.db.UpsertSession(s); err != nil {
		if isIntentionalSessionSkip(err) {
			if pw.sess.File.Path != "" {
				e.cacheSkip(pw.sess.File.Path, pw.sess.File.Mtime)
			}
			return err
		}
		log.Printf("upsert session %s: %v", s.ID, err)
		return err
	}
	update, findings := computeSignalsAndSecrets(s, msgs)
	if err := e.db.ReplaceSessionContent(s.ID, msgs, update, findings); err != nil {
		log.Printf(
			"replace messages for %s: %v",
			s.ID, err,
		)
		return err
	}
	if err := e.db.ReplaceSessionUsageEvents(
		s.ID, toDBUsageEvents(s.ID, pw.usageEvents),
	); err != nil {
		log.Printf(
			"replace usage events for %s: %v",
			s.ID, err,
		)
		return err
	}

	// See writeBatch for why data_version is bumped here
	// rather than inside UpsertSession.
	if err := e.db.SetSessionDataVersion(
		s.ID, dataVersionForWrite(pw),
	); err != nil {
		log.Printf(
			"set data_version for %s: %v", s.ID, err,
		)
	}

	return nil
}

func (e *Engine) shouldPreserveOpenCodeFormatArchive(
	agent parser.AgentType, path, sessionID string,
	currentMtime int64,
	currentHash string,
	currentMsgs []db.Message,
) bool {
	if !isOpenCodeFormatStorageAgent(agent) {
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
	storedIsStorageArchive := hasOpenCodeFormatStorageFingerprint(
		agent, storedHash,
	) || isOpenCodeFormatStoragePath(agent, storedPath)
	if isOpenCodeFormatSQLiteVirtualPath(agent, path) &&
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
	if hasOpenCodeFormatStorageFingerprint(agent, storedHash) &&
		hasOpenCodeFormatStorageFingerprint(agent, currentHash) &&
		!parser.OpenCodeStorageFingerprintMissing(
			storedHash, currentHash,
		) {
		return false
	}
	if storedIsStorageArchive &&
		isOpenCodeFormatSQLiteVirtualPath(agent, path) &&
		currentMtime != 0 &&
		storedMtime != 0 &&
		currentMtime <= storedMtime {
		log.Printf(
			"skip %s session %s: sqlite fallback is not newer than preserved storage archive",
			agent, sessionID,
		)
		return true
	}
	if openCodeLegacyArchiveLooksIncomplete(
		currentMsgs, storedMsgs,
	) {
		if hasOpenCodeFormatStorageFingerprint(agent, storedHash) {
			log.Printf(
				"skip %s session %s: storage fingerprint changed but update looks incomplete relative to archive",
				agent, sessionID,
			)
		} else {
			log.Printf(
				"skip %s session %s: storage update looks incomplete relative to legacy archive",
				agent, sessionID,
			)
		}
		return true
	}
	return false
}

func isOpenCodeFormatStorageAgent(agent parser.AgentType) bool {
	return agent == parser.AgentOpenCode ||
		agent == parser.AgentKilo ||
		agent == parser.AgentMiMoCode
}

func openCodeFormatDBName(agent parser.AgentType) string {
	switch agent {
	case parser.AgentOpenCode:
		return "opencode.db"
	case parser.AgentKilo:
		return "kilo.db"
	case parser.AgentMiMoCode:
		return "mimocode.db"
	default:
		return ""
	}
}

func resolveOpenCodeFormatSource(
	agent parser.AgentType, dir string,
) parser.OpenCodeSource {
	switch agent {
	case parser.AgentOpenCode:
		return parser.ResolveOpenCodeSource(dir)
	case parser.AgentKilo:
		return parser.ResolveKiloSource(dir)
	case parser.AgentMiMoCode:
		return parser.ResolveMiMoCodeSource(dir)
	default:
		return parser.OpenCodeSource{}
	}
}

func openCodeFormatSourceMtime(
	agent parser.AgentType, path string,
) (int64, error) {
	switch agent {
	case parser.AgentOpenCode:
		return parser.OpenCodeSourceMtime(path)
	case parser.AgentKilo:
		return parser.KiloSourceMtime(path)
	case parser.AgentMiMoCode:
		return parser.MiMoCodeSourceMtime(path)
	default:
		return 0, fmt.Errorf("unknown OpenCode-format agent: %s", agent)
	}
}

// hasOpenCodeFormatStorageFingerprint reports whether hash is an
// OpenCode storage fingerprint. Kilo reuses OpenCode's storage format
// verbatim, so the same check applies to both agents.
func hasOpenCodeFormatStorageFingerprint(
	agent parser.AgentType, hash string,
) bool {
	return isOpenCodeFormatStorageAgent(agent) &&
		parser.HasOpenCodeStorageFingerprint(hash)
}

func isOpenCodeFormatStoragePath(
	agent parser.AgentType, path string,
) bool {
	return strings.HasSuffix(path, ".json") &&
		!isOpenCodeFormatSQLiteVirtualPath(agent, path)
}

func isOpenCodeFormatSQLiteVirtualPath(
	agent parser.AgentType, path string,
) bool {
	if !isOpenCodeFormatStorageAgent(agent) {
		return false
	}
	_, _, ok := parseOpenCodeFormatSQLiteVirtualPath(agent, path)
	return ok
}

func parseOpenCodeFormatSQLiteVirtualPath(
	agent parser.AgentType, path string,
) (dbPath, sessionID string, ok bool) {
	switch agent {
	case parser.AgentKilo:
		return parser.ParseKiloSQLiteVirtualPath(path)
	case parser.AgentMiMoCode:
		return parser.ParseMiMoCodeSQLiteVirtualPath(path)
	default:
		return parser.ParseOpenCodeSQLiteVirtualPath(path)
	}
}

func listOpenCodeFormatSessionMeta(
	agent parser.AgentType, dbPath string,
) ([]parser.OpenCodeSessionMeta, error) {
	switch agent {
	case parser.AgentKilo:
		return parser.ListKiloSessionMeta(dbPath)
	case parser.AgentMiMoCode:
		return parser.ListMiMoCodeSessionMeta(dbPath)
	default:
		return parser.ListOpenCodeSessionMeta(dbPath)
	}
}

func openCodeFormatStorageSessionIDs(
	agent parser.AgentType, dir string,
) map[string]struct{} {
	switch agent {
	case parser.AgentKilo:
		return parser.KiloStorageSessionIDs(dir)
	case parser.AgentMiMoCode:
		return parser.MiMoCodeStorageSessionIDs(dir)
	default:
		return parser.OpenCodeStorageSessionIDs(dir)
	}
}

func findOpenCodeFormatSourceFile(
	agent parser.AgentType, dir, sessionID string,
) string {
	switch agent {
	case parser.AgentKilo:
		return parser.FindKiloSourceFile(dir, sessionID)
	case parser.AgentMiMoCode:
		return parser.FindMiMoCodeSourceFile(dir, sessionID)
	default:
		return parser.FindOpenCodeSourceFile(dir, sessionID)
	}
}

func parseOpenCodeFormatSession(
	agent parser.AgentType, dbPath, sessionID, machine string,
) (*parser.ParsedSession, []parser.ParsedMessage, error) {
	switch agent {
	case parser.AgentKilo:
		return parser.ParseKiloSession(dbPath, sessionID, machine)
	case parser.AgentMiMoCode:
		return parser.ParseMiMoCodeSession(dbPath, sessionID, machine)
	default:
		return parser.ParseOpenCodeSession(dbPath, sessionID, machine)
	}
}

func parseOpenCodeFormatFile(
	agent parser.AgentType, path, machine string,
) (*parser.ParsedSession, []parser.ParsedMessage, error) {
	switch agent {
	case parser.AgentKilo:
		return parser.ParseKiloFile(path, machine)
	case parser.AgentMiMoCode:
		return parser.ParseMiMoCodeFile(path, machine)
	default:
		return parser.ParseOpenCodeFile(path, machine)
	}
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
	if parsed == nil {
		return len(stored) > 0
	}
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
	if sanitizedMessageContentLength(parsed) <
		sanitizedMessageContentLength(stored) {
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

func sanitizedMessageContentLength(msg db.Message) int {
	sanitized := db.SanitizeUTF8(msg.Content)
	if sanitized != msg.Content {
		return len(sanitized)
	}
	return msg.ContentLength
}

func countToolResultEvents(calls []db.ToolCall) int {
	total := 0
	for _, call := range calls {
		total += len(call.ResultEvents)
	}
	return total
}

func (e *Engine) applyIDPrefixToSessionIDs(ids []string) []string {
	if e.idPrefix == "" || len(ids) == 0 {
		return ids
	}
	prefixed := make([]string, len(ids))
	for i, id := range ids {
		if id == "" || strings.HasPrefix(id, e.idPrefix) {
			prefixed[i] = id
			continue
		}
		prefixed[i] = e.idPrefix + id
	}
	return prefixed
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
		TerminationStatus:    strPtr(string(pw.sess.TerminationStatus)),
		// data_version is intentionally left at the
		// existing column default (0). UpsertSession does
		// not persist this field; the caller bumps it via
		// SetSessionDataVersion only after the message
		// rewrite succeeds.
		FilePath:      strPtr(pw.sess.File.Path),
		FileSize:      int64Ptr(pw.sess.File.Size),
		FileMtime:     int64Ptr(pw.sess.File.Mtime),
		NextOrdinal:   nextParsedOrdinal(0, pw.msgs),
		LastEntryUUID: strPtr(lastParsedSourceUUID("", pw.msgs)),
		FileInode:     int64Ptr(pw.sess.File.Inode),
		FileDevice:    int64Ptr(pw.sess.File.Device),
		FileHash:      strPtr(pw.sess.File.Hash),
	}
	if pw.sess.FirstMessage != "" {
		s.FirstMessage = &pw.sess.FirstMessage
	}
	s.SessionName = db.ParsedSessionName(pw.sess)
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

// toDBUsageEvents converts parser usage events for one session.
// sessionID is the final ID after remote rewrites; parser-stamped
// event session IDs predate the idPrefix and are ignored.
func toDBUsageEvents(
	sessionID string, events []parser.ParsedUsageEvent,
) []db.UsageEvent {
	out := make([]db.UsageEvent, 0, len(events))
	for _, ev := range events {
		out = append(out, db.UsageEvent{
			SessionID:                sessionID,
			MessageOrdinal:           ev.MessageOrdinal,
			Source:                   ev.Source,
			Model:                    ev.Model,
			InputTokens:              ev.InputTokens,
			OutputTokens:             ev.OutputTokens,
			CacheCreationInputTokens: ev.CacheCreationInputTokens,
			CacheReadInputTokens:     ev.CacheReadInputTokens,
			ReasoningTokens:          ev.ReasoningTokens,
			CostUSD:                  ev.CostUSD,
			CostStatus:               ev.CostStatus,
			CostSource:               ev.CostSource,
			OccurredAt:               ev.OccurredAt,
			DedupKey:                 ev.DedupKey,
		})
	}
	// Route usage events through the central validation/sanitization
	// pass so they get the same treatment as messages and sessions at
	// every call site. Stats are discarded for now.
	_ = validateAndSanitize(nil, nil, out)
	return out
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

func nextParsedOrdinal(
	current int, msgs []parser.ParsedMessage,
) int {
	if len(msgs) == 0 {
		return current
	}
	return msgs[len(msgs)-1].Ordinal + 1
}

func lastParsedSourceUUID(
	current string, msgs []parser.ParsedMessage,
) string {
	for _, v := range slices.Backward(msgs) {
		if v.SourceUUID != "" {
			return v.SourceUUID
		}
	}
	return current
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
	if !ok {
		return ""
	}
	rawSessionID := strings.TrimPrefix(rawID, def.IDPrefix)
	if !def.FileBased {
		switch def.Type {
		case parser.AgentWarp:
			for _, d := range e.agentDirs[def.Type] {
				dbPath := parser.FindWarpDBPath(d)
				if dbPath == "" {
					continue
				}
				if _, _, err := parser.ParseWarpSession(dbPath, rawSessionID, e.machine); err == nil {
					return dbPath
				}
			}
		case parser.AgentForge:
			for _, d := range e.agentDirs[def.Type] {
				dbPath := parser.FindForgeDBPath(d)
				if dbPath == "" {
					continue
				}
				if _, _, err := parser.ParseForgeSession(dbPath, rawSessionID, e.machine); err == nil {
					return dbPath
				}
			}
		case parser.AgentPiebald:
			chatID, _, _ := strings.Cut(rawSessionID, "-")
			for _, d := range e.agentDirs[def.Type] {
				dbPath := parser.FindPiebaldDBPath(d)
				if dbPath == "" {
					continue
				}
				results, err := parser.ParsePiebaldSessionResults(dbPath, chatID, e.machine)
				if err == nil && piebaldResultsContain(results, sessionID) {
					return dbPath
				}
			}
		}
		if f := e.findProviderSourceFile(
			context.Background(), def, sessionID, rawSessionID,
		); f != "" {
			return f
		}
		return ""
	}
	if def.Type == parser.AgentKiro {
		for _, dir := range e.agentDirs[parser.AgentKiro] {
			dbPath := parser.FindKiroSQLiteDBPath(dir)
			if dbPath == "" ||
				!parser.KiroSQLiteSessionExists(
					dbPath, rawSessionID,
				) {
				continue
			}
			return parser.KiroSQLiteVirtualPath(
				dbPath, rawSessionID,
			)
		}
	}

	bareID := strings.TrimPrefix(rawID, def.IDPrefix)

	// Prefer stored file_path — it's authoritative and handles
	// cases where the session ID doesn't match the filename.
	// Resolve virtual paths (e.g. Visual Studio Copilot's
	// <traceFile>#<conversationID>) for the existence check, but
	// return the stored path so downstream parsing stays scoped to
	// the requested conversation rather than the whole trace file.
	if fp := e.db.GetSessionFilePath(sessionID); fp != "" {
		if historyPath, idx, ok := parser.ParseAiderVirtualPath(fp); ok {
			// aider's stored "<historyPath>#<idx>" is positional: an
			// inserted or removed earlier run shifts the index onto a
			// different session. Only trust the stored path when run idx
			// still recomputes to the requested raw ID; otherwise fall
			// through to FindSourceFunc, which re-resolves by raw ID.
			if got, ok := parser.AiderRawIDAt(historyPath, idx); ok && got == bareID {
				return fp
			}
		} else if _, err := os.Stat(parser.ResolveSourceFilePath(fp)); err == nil {
			return fp
		}
	}

	if def.FindSourceFunc != nil {
		for _, d := range e.agentDirs[def.Type] {
			if f := def.FindSourceFunc(d, bareID); f != "" {
				return f
			}
		}
	}
	if f := e.findProviderSourceFile(
		context.Background(), def, sessionID, bareID,
	); f != "" {
		return f
	}
	return ""
}

// findProviderSourceFile resolves a single session's source file through the
// provider facade for authoritative concrete providers. It is the
// provider-shape counterpart to AgentDef.FindSourceFunc, so a provider can drop
// its FindSourceFunc hook and stay locatable for diagnostics, export, and
// parse-diff lookups once it owns live processing. Shadow mode remains
// observational and must not satisfy lookups that legacy lookup would miss.
func (e *Engine) findProviderSourceFile(
	ctx context.Context,
	def parser.AgentDef,
	sessionID string,
	rawSessionID string,
) string {
	mode := e.providerMigrationModes[def.Type]
	if mode != parser.ProviderMigrationProviderAuthoritative {
		return ""
	}
	factory, ok := e.providerFactories[def.Type]
	if !ok || factory == nil {
		return ""
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   e.agentDirs[def.Type],
		Machine: e.machine,
	})
	source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
		RawSessionID:  rawSessionID,
		FullSessionID: sessionID,
	})
	if err != nil {
		log.Printf("%s provider source lookup: %v", def.Type, err)
		return ""
	}
	if !found {
		return ""
	}
	return providerDiscoveredPath(source)
}

// SourceMtime returns the current source-backed mtime for a
// session. Most file-based agents map directly to a single source
// file, but OpenCode storage sessions derive their effective mtime
// from the session JSON plus related message/part files.
func (e *Engine) SourceMtime(sessionID string) int64 {
	host, rawID := parser.StripHostPrefix(sessionID)
	if host != "" {
		return 0
	}

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok {
		return 0
	}
	rawSessionID := strings.TrimPrefix(rawID, def.IDPrefix)
	if !def.FileBased {
		switch def.Type {
		case parser.AgentWarp:
			for _, d := range e.agentDirs[def.Type] {
				dbPath := parser.FindWarpDBPath(d)
				if dbPath == "" {
					continue
				}
				metas, err := parser.ListWarpSessionMeta(dbPath)
				if err != nil {
					continue
				}
				for _, meta := range metas {
					if meta.SessionID == rawSessionID {
						return meta.FileMtime
					}
				}
			}
		case parser.AgentForge:
			for _, d := range e.agentDirs[def.Type] {
				dbPath := parser.FindForgeDBPath(d)
				if dbPath == "" {
					continue
				}
				metas, err := parser.ListForgeSessionMeta(dbPath)
				if err != nil {
					continue
				}
				for _, meta := range metas {
					if meta.SessionID == rawSessionID {
						return meta.FileMtime
					}
				}
			}
		case parser.AgentPiebald:
			chatID, _, _ := strings.Cut(rawSessionID, "-")
			for _, d := range e.agentDirs[def.Type] {
				dbPath := parser.FindPiebaldDBPath(d)
				if dbPath == "" {
					continue
				}
				metas, err := parser.ListPiebaldSessionMeta(dbPath)
				if err != nil {
					continue
				}
				var mtime int64
				for _, meta := range metas {
					if meta.SessionID == chatID {
						mtime = meta.FileMtime
						break
					}
				}
				if mtime == 0 {
					continue
				}
				// Base chat IDs are confirmed by meta. Fork IDs
				// need a parse to verify the requested fork exists.
				if chatID == rawSessionID {
					return mtime
				}
				results, err := parser.ParsePiebaldSessionResults(dbPath, chatID, e.machine)
				if err == nil && piebaldResultsContain(results, sessionID) {
					return mtime
				}
			}
		}
		return 0
	}

	path := e.FindSourceFile(sessionID)
	if path == "" {
		return 0
	}

	if isOpenCodeFormatStorageAgent(def.Type) {
		mtime, err := openCodeFormatSourceMtime(def.Type, path)
		if err != nil {
			return 0
		}
		return mtime
	}
	if def.Type == parser.AgentKiro {
		if _, _, ok := parser.ParseKiroSQLiteVirtualPath(path); ok {
			mtime, err := parser.KiroSQLiteSourceMtime(path)
			if err != nil {
				return 0
			}
			return mtime
		}
	}
	if def.Type == parser.AgentZed {
		if _, _, ok := parser.ParseZedSQLiteVirtualPath(path); ok {
			mtime, err := parser.ZedSQLiteSourceMtime(path)
			if err != nil {
				return 0
			}
			return mtime
		}
	}
	if def.Type == parser.AgentShelley {
		if _, _, ok := parser.ParseShelleyVirtualPath(path); ok {
			mtime, err := parser.ShelleySourceMtime(path)
			if err != nil {
				return 0
			}
			return mtime
		}
	}
	if def.Type == parser.AgentAntigravityCLI {
		info, err := parser.AntigravityCLIFileInfo(path)
		if err != nil {
			return 0
		}
		return info.ModTime().UnixNano()
	}
	if def.Type == parser.AgentAntigravity {
		info, err := parser.AntigravityFileInfo(path)
		if err != nil {
			return 0
		}
		return info.ModTime().UnixNano()
	}
	if def.Type == parser.AgentCowork {
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		return parser.CoworkSessionMtime(path, info.ModTime().UnixNano())
	}
	if def.Type == parser.AgentCommandCode {
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		return commandCodeEffectiveInfo(path, info).ModTime().UnixNano()
	}
	if def.Type == parser.AgentVSCopilot {
		// A conversation's transcript is rebuilt from every sibling trace
		// file, so the watcher fallback must compare a composite mtime
		// spanning all of them, not just the representative trace file.
		_, mtime := parser.VisualStudioCopilotTraceFingerprint(
			parser.ResolveSourceFilePath(path),
		)
		return mtime
	}
	if def.Type == parser.AgentVibe {
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		return vibeEffectiveInfo(path, info).ModTime().UnixNano()
	}
	if def.Type == parser.AgentReasonix {
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		return reasonixEffectiveInfo(path, info).ModTime().UnixNano()
	}

	// FindSourceFile may return a virtual path (e.g. Visual Studio
	// Copilot's <traceFile>#<conversationID>); resolve it to the
	// physical source for the stat.
	info, err := os.Stat(parser.ResolveSourceFilePath(path))
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

// SyncSingleSession re-syncs a single session by its ID and
// uses the existing DB project as fallback where applicable.
func (e *Engine) SyncSingleSession(sessionID string) (err error) {
	return e.SyncSingleSessionContext(context.Background(), sessionID)
}

// SyncSingleSessionContext re-syncs a single session by its ID using ctx for
// cancellable git-backed project resolution and database reads on this path.
func (e *Engine) SyncSingleSessionContext(
	ctx context.Context, sessionID string,
) (err error) {
	if e.refuseWriteInForceParse("SyncSingleSession") {
		return fmt.Errorf(
			"cannot sync session %s on a report-only (parse-diff) engine",
			sessionID,
		)
	}
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
		case parser.AgentForge:
			return e.syncSingleForge(sessionID)
		case parser.AgentPiebald:
			return e.syncSinglePiebald(sessionID)
		default:
			err = e.syncSingleOpenCodeFormat(
				sessionID, parser.AgentOpenCode,
			)
			if errors.Is(err, errSessionPreserved) {
				preserved = true
				return nil
			}
			return err
		}
	}

	if def.Type == parser.AgentZed {
		err = e.syncSingleZed(sessionID)
		if errors.Is(err, errSessionPreserved) {
			preserved = true
			return nil
		}
		return err
	}

	path := e.FindSourceFile(sessionID)
	if path == "" {
		return fmt.Errorf(
			"source file not found for %s", sessionID,
		)
	}
	if isOpenCodeFormatStorageAgent(def.Type) &&
		isOpenCodeFormatSQLiteVirtualPath(def.Type, path) {
		err = e.syncSingleOpenCodeFormat(sessionID, def.Type)
		if errors.Is(err, errSessionPreserved) {
			preserved = true
			return nil
		}
		return err
	}
	if def.Type == parser.AgentKiro &&
		isKiroSQLiteVirtualPath(path) {
		err = e.syncSingleKiroSQLite(sessionID)
		if errors.Is(err, errSessionPreserved) {
			preserved = true
			return nil
		}
		return err
	}
	if def.Type == parser.AgentHermes {
		hermesProject := ""
		if sess, _ := e.db.GetSession(ctx, sessionID); sess != nil &&
			sess.Project != "" && !parser.NeedsProjectReparse(sess.Project) {
			hermesProject = sess.Project
		}
		ok, err := e.syncSingleHermesArchive(
			sessionID, path, hermesProject,
		)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}

	agent := def.Type

	// Clear skip cache so explicit re-sync always processes
	// the file, even if it was cached as non-interactive
	// during a bulk SyncAll.
	file := parser.DiscoveredFile{
		Path:       path,
		Agent:      agent,
		ForceParse: true,
	}
	if e.shouldCacheSkip(file) {
		e.clearSkip(path)
	}

	// Reuse processFile for stat and DB-skip logic.
	switch agent {
	case parser.AgentClaude:
		// Try to preserve existing project from DB first
		if sess, _ := e.db.GetSession(ctx, sessionID); sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			file.Project = sess.Project
		} else {
			file.Project = filepath.Base(filepath.Dir(path))
		}
	case parser.AgentVSCopilot:
		// processVisualStudioCopilot persists file.Project into every
		// parsed session, so an empty project here would overwrite the
		// existing "visualstudio" value. Prefer the stored project; fall
		// back to the canonical default discovery assigns.
		if sess, _ := e.db.GetSession(ctx, sessionID); sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			file.Project = sess.Project
		} else {
			file.Project = "visualstudio"
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
		if sess, _ := e.db.GetSession(ctx, sessionID); sess != nil &&
			sess.Project != "" &&
			!parser.NeedsProjectReparse(sess.Project) {
			file.Project = sess.Project
		} else {
			file.Project = filepath.Base(filepath.Dir(path))
		}
	case parser.AgentKimi:
		// path is <kimiDir>/<project>/<session>/wire.jsonl              (legacy)
		//    or <kimiDir>/<project>/<session>/agents/<agent>/wire.jsonl (.kimi-code)
		// In both layouts the project is the first path segment relative
		// to the sessions dir. Deriving two levels up (the old approach)
		// mis-resolves to "agents" under the .kimi-code layout.
		for _, kimiDir := range e.agentDirs[parser.AgentKimi] {
			rel, ok := isUnder(kimiDir, path)
			if !ok {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) > 0 {
				file.Project = parser.DecodeKimiProjectDir(parts[0])
			}
			break
		}
		if file.Project == "" {
			file.Project = "kimi"
		}
	case parser.AgentQwenPaw:
		// path is <qwenpawDir>/<workspace>/sessions/<name>.json or
		//               <qwenpawDir>/<workspace>/sessions/<subdir>/<name>.json
		// Workspace name is the first path segment relative to the
		// QwenPaw root.
		for _, qwenpawDir := range e.agentDirs[parser.AgentQwenPaw] {
			rel, ok := isUnder(qwenpawDir, path)
			if !ok {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) > 0 {
				file.Project = parts[0]
			}
			break
		}
		// Fallback when the stored file_path points outside any
		// currently configured QWENPAW_DIR (e.g. the root was
		// removed, or the session was synced from a custom path).
		// Without this, ParseQwenPawSession would build
		// "qwenpaw::<stem>" and orphan the requested
		// "qwenpaw:<workspace>:<stem>" row. Prefer the DB-stored
		// Project as the authoritative record; parse the workspace
		// from the sessionID prefix as a final fallback that works
		// even when the DB row is missing or stale.
		if file.Project == "" {
			if sess, _ := e.db.GetSession(ctx, sessionID); sess != nil &&
				sess.Project != "" &&
				!parser.NeedsProjectReparse(sess.Project) {
				file.Project = sess.Project
			}
		}
		if file.Project == "" {
			bareID := strings.TrimPrefix(sessionID, def.IDPrefix)
			if workspace, _, ok := strings.Cut(bareID, ":"); ok &&
				workspace != "" {
				file.Project = workspace
			}
		}
	case parser.AgentReasonix:
		if classified, ok := e.classifyReasonixPath(path); ok {
			file.Project = classified.Project
		} else {
			if sess, _ := e.db.GetSession(ctx, sessionID); sess != nil &&
				sess.Project != "" &&
				!parser.NeedsProjectReparse(sess.Project) {
				file.Project = sess.Project
			}
		}
	case parser.AgentQwen:
		// path is <qwenProjectsDir>/<encoded-project>/chats/<session>.jsonl
		file.Project = parser.GetProjectName(
			filepath.Base(filepath.Dir(filepath.Dir(path))),
		)
	case parser.AgentWorkBuddy:
		for _, workBuddyDir := range e.agentDirs[parser.AgentWorkBuddy] {
			rel, ok := isUnder(workBuddyDir, path)
			if !ok {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) == 2 || len(parts) == 4 && parts[2] == "subagents" {
				file.Project = parts[0]
				break
			}
		}
	}

	res := e.processFile(ctx, file)
	if res.err != nil {
		if res.cacheSkip && res.mtime != 0 && !res.noCacheSkip {
			e.cacheSkip(res.skipCacheKey(path), res.mtime)
		}
		return res.err
	}
	if res.skip {
		return nil
	}
	if res.cacheSkip {
		e.clearSkip(res.skipCacheKey(path))
	}

	// Delete parser-excluded sessions before writing the parsed
	// results, mirroring collectAndBatch. Vibe promotes a session
	// from its directory-name fallback ID to the canonical
	// meta.json ID and returns the stale fallback ID here; without
	// this delete a single-session resync would leave both rows in
	// the DB and double-count messages and usage.
	if excluded := e.applyIDPrefixToSessionIDs(
		res.excludedSessionIDs,
	); len(excluded) > 0 {
		if _, err := e.db.DeleteParserExcludedSessions(
			excluded,
		); err != nil {
			return fmt.Errorf(
				"delete parser-excluded sessions: %w", err,
			)
		}
	}

	// Handle incremental updates from processFile (e.g.
	// append-only JSONL that was already synced).
	if res.incremental != nil {
		if err := e.writeIncremental(res.incremental); err != nil {
			return err
		}
		return nil
	}

	if len(res.results) == 0 {
		return nil
	}

	for _, pr := range res.results {
		if err := e.writeSessionFull(
			pendingWrite{
				sess:        pr.Session,
				msgs:        pr.Messages,
				usageEvents: pr.UsageEvents,
				needsRetry:  res.needsRetryForSession(pr.Session.ID),
			},
		); err != nil &&
			!isIntentionalSessionSkip(err) &&
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

func (e *Engine) syncSingleHermesArchive(
	sessionID, path, project string,
) (bool, error) {
	stateDB := ""
	if filepath.Base(path) == "state.db" {
		stateDB = path
	} else if filepath.Base(filepath.Dir(path)) == "sessions" {
		candidate := filepath.Join(
			filepath.Dir(filepath.Dir(path)), "state.db",
		)
		if parser.IsRegularFile(candidate) {
			stateDB = candidate
		}
	}
	if stateDB == "" {
		return false, nil
	}

	results, err := parser.ParseHermesArchive(
		stateDB, project, e.machine,
	)
	if err != nil {
		return true, err
	}
	for _, pr := range results {
		if pr.Session.ID != sessionID {
			continue
		}
		if err := e.writeSessionFull(pendingWrite{
			sess:        pr.Session,
			msgs:        pr.Messages,
			usageEvents: pr.UsageEvents,
		}); err != nil && !isIntentionalSessionSkip(err) &&
			!errors.Is(err, errSessionPreserved) {
			return true, fmt.Errorf(
				"write session %s: %w", pr.Session.ID, err,
			)
		}
		return true, nil
	}
	return true, fmt.Errorf(
		"session %s not found in Hermes archive %s",
		sessionID, stateDB,
	)
}

func (e *Engine) applyWorktreeMappingToSingleSession(
	sessionID string,
) error {
	ctx := context.Background()
	sess, err := e.db.GetSession(ctx, sessionID)
	if err != nil || sess == nil || sess.Cwd == "" {
		return err
	}

	machine := sess.Machine
	if machine == "" {
		machine = e.machine
	}
	_, err = e.db.ApplyWorktreeProjectMappingToSessionFromSync(
		ctx, machine, sess.ID, sess.Cwd, sess.Project,
	)
	if err != nil {
		return fmt.Errorf(
			"apply worktree mapping to session %s: %w",
			sessionID, err,
		)
	}
	return nil
}

// syncSingleOpenCodeFormat re-syncs a single SQLite-backed session for
// an OpenCode-format agent (OpenCode or its Kilo fork).
func (e *Engine) syncSingleOpenCodeFormat(
	sessionID string, agent parser.AgentType,
) error {
	if !isOpenCodeFormatStorageAgent(agent) {
		return fmt.Errorf("unknown OpenCode-format agent: %s", agent)
	}
	rawID := strings.TrimPrefix(sessionID, string(agent)+":")
	dbName := openCodeFormatDBName(agent)

	var lastErr error
	for _, dir := range e.agentDirs[agent] {
		if dir == "" {
			continue
		}
		dbPath := filepath.Join(dir, dbName)
		if info, err := os.Stat(dbPath); err != nil ||
			info.IsDir() {
			continue
		}
		sess, msgs, err := parseOpenCodeFormatSession(
			agent, dbPath, rawID, e.machine,
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
			!isIntentionalSessionSkip(err) &&
			!errors.Is(err, errSessionPreserved) {
			return fmt.Errorf("write session %s: %w",
				sess.ID, err)
		} else if errors.Is(err, errSessionPreserved) {
			return err
		}
		return nil
	}

	if len(e.agentDirs[agent]) == 0 {
		return fmt.Errorf("%s dir not configured", agent)
	}
	if lastErr != nil {
		return fmt.Errorf(
			"%s session %s: %w", agent, sessionID, lastErr,
		)
	}
	return fmt.Errorf("%s session %s not found", agent, sessionID)
}

func (e *Engine) syncSingleKiroSQLite(
	sessionID string,
) error {
	rawID := strings.TrimPrefix(sessionID, "kiro:")

	var lastErr error
	for _, dir := range e.agentDirs[parser.AgentKiro] {
		dbPath := parser.FindKiroSQLiteDBPath(dir)
		if dbPath == "" {
			continue
		}
		store, err := parser.OpenKiroSQLiteStore(dbPath)
		if err != nil {
			lastErr = err
			continue
		}
		sess, msgs, err := store.ParseSession(
			rawID, e.machine,
		)
		if closeErr := store.Close(); closeErr != nil && err == nil {
			lastErr = closeErr
			continue
		}
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
			!isIntentionalSessionSkip(err) &&
			!errors.Is(err, errSessionPreserved) {
			return fmt.Errorf("write session %s: %w",
				sess.ID, err)
		} else if errors.Is(err, errSessionPreserved) {
			return err
		}
		return nil
	}

	if len(e.agentDirs[parser.AgentKiro]) == 0 {
		return fmt.Errorf("kiro dir not configured")
	}
	if lastErr != nil {
		return fmt.Errorf(
			"kiro sqlite session %s: %w", sessionID, lastErr,
		)
	}
	return fmt.Errorf("kiro sqlite session %s not found", sessionID)
}

func (e *Engine) syncSingleZed(sessionID string) error {
	rawID := strings.TrimPrefix(sessionID, "zed:")

	var lastErr error
	for _, dir := range e.agentDirs[parser.AgentZed] {
		dbPath := filepath.Join(dir, parser.ZedThreadsDBRelPath)
		if !parser.IsRegularFile(dbPath) {
			continue
		}
		info, err := os.Stat(dbPath)
		if err != nil {
			lastErr = err
			continue
		}
		result, err := parser.ParseZedThreadDirect(dbPath, rawID, e.machine, info)
		if err != nil {
			lastErr = err
			continue
		}
		if result == nil {
			continue
		}
		if hash, err := ComputeFileHash(dbPath); err == nil {
			result.Session.File.Hash = hash
		}
		pw := pendingWrite{
			sess:         result.Session,
			msgs:         result.Messages,
			usageEvents:  result.UsageEvents,
			forceReplace: true,
		}
		if err := e.writeSessionFull(pw); err != nil &&
			!isIntentionalSessionSkip(err) &&
			!errors.Is(err, errSessionPreserved) {
			return fmt.Errorf("write session %s: %w", result.Session.ID, err)
		} else if errors.Is(err, errSessionPreserved) {
			return err
		}
		return nil
	}

	if len(e.agentDirs[parser.AgentZed]) == 0 {
		return fmt.Errorf("zed dir not configured")
	}
	if lastErr != nil {
		return fmt.Errorf("zed session %s: %w", sessionID, lastErr)
	}
	return fmt.Errorf("zed session %s not found", sessionID)
}

func isKiroSQLiteVirtualPath(path string) bool {
	_, _, ok := parser.ParseKiroSQLiteVirtualPath(path)
	return ok
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

func (e *Engine) warpPendingSessionIDs(dir string) []string {
	dbPath := parser.FindWarpDBPath(dir)
	if dbPath == "" {
		return nil
	}
	metas, err := parser.ListWarpSessionMeta(dbPath)
	if err != nil {
		log.Printf("sync warp: %v", err)
		return nil
	}
	var changed []string
	for _, m := range metas {
		_, storedMtime, ok := e.db.GetFileInfoByPath(m.VirtualPath)
		if ok && storedMtime == m.FileMtime {
			continue
		}
		changed = append(changed, m.SessionID)
	}
	return changed
}

func (e *Engine) countOneWarpSessions(dir string) int {
	dbPath := parser.FindWarpDBPath(dir)
	if dbPath == "" {
		return 0
	}
	metas, err := parser.ListWarpSessionMeta(dbPath)
	if err != nil {
		log.Printf("sync warp: %v", err)
		return 0
	}
	return len(metas)
}

// syncWarp syncs sessions from Warp SQLite databases.
// Uses per-conversation last_modified_at to detect changes,
// so only modified conversations are fully parsed.
func (e *Engine) syncWarp(
	ctx context.Context, scope *rootSyncScope,
) []pendingWrite {
	var allPending []pendingWrite
	for _, dir := range e.agentDirs[parser.AgentWarp] {
		if ctx.Err() != nil {
			break
		}
		if dir == "" || !scope.includes(dir) {
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
	changed := e.warpPendingSessionIDs(dir)
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
		); err != nil && !isIntentionalSessionSkip(err) {
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

func (e *Engine) forgePendingSessionIDs(dir string) []string {
	dbPath := parser.FindForgeDBPath(dir)
	if dbPath == "" {
		return nil
	}
	metas, err := parser.ListForgeSessionMeta(dbPath)
	if err != nil {
		log.Printf("sync forge: %v", err)
		return nil
	}
	var changed []string
	for _, m := range metas {
		_, storedMtime, ok := e.db.GetFileInfoByPath(m.VirtualPath)
		if ok && storedMtime == m.FileMtime &&
			e.db.GetDataVersionByPath(m.VirtualPath) >= db.CurrentDataVersion() {
			continue
		}
		changed = append(changed, m.SessionID)
	}
	return changed
}

func (e *Engine) countOneForgeSessions(dir string) int {
	dbPath := parser.FindForgeDBPath(dir)
	if dbPath == "" {
		return 0
	}
	metas, err := parser.ListForgeSessionMeta(dbPath)
	if err != nil {
		log.Printf("sync forge: %v", err)
		return 0
	}
	return len(metas)
}

// syncForge syncs sessions from Forge SQLite databases.
func (e *Engine) syncForge(
	ctx context.Context, scope *rootSyncScope,
) []pendingWrite {
	var allPending []pendingWrite
	for _, dir := range e.agentDirs[parser.AgentForge] {
		if ctx.Err() != nil {
			break
		}
		if dir == "" || !scope.includes(dir) {
			continue
		}
		allPending = append(allPending, e.syncOneForge(ctx, dir)...)
	}
	return allPending
}

// syncOneForge handles a single Forge directory.
func (e *Engine) syncOneForge(
	ctx context.Context, dir string,
) []pendingWrite {
	dbPath := parser.FindForgeDBPath(dir)
	changed := e.forgePendingSessionIDs(dir)
	if len(changed) == 0 {
		return nil
	}

	var pending []pendingWrite
	for _, cid := range changed {
		if ctx.Err() != nil {
			break
		}
		sess, msgs, err := parser.ParseForgeSession(dbPath, cid, e.machine)
		if err != nil {
			log.Printf("forge conversation %s: %v", cid, err)
			continue
		}
		if sess == nil {
			continue
		}
		pending = append(pending, pendingWrite{sess: *sess, msgs: msgs})
	}
	return pending
}

// syncSingleForge re-syncs a single Forge conversation.
func (e *Engine) syncSingleForge(
	sessionID string,
) error {
	rawID := strings.TrimPrefix(sessionID, "forge:")

	var lastErr error
	for _, dir := range e.agentDirs[parser.AgentForge] {
		if dir == "" {
			continue
		}
		dbPath := parser.FindForgeDBPath(dir)
		if dbPath == "" {
			continue
		}
		sess, msgs, err := parser.ParseForgeSession(dbPath, rawID, e.machine)
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
			return fmt.Errorf("write session %s: %w", sess.ID, err)
		}
		if err := e.db.LinkSubagentSessions(); err != nil {
			log.Printf("link subagent sessions: %v", err)
		}
		return nil
	}

	if len(e.agentDirs[parser.AgentForge]) == 0 {
		return fmt.Errorf("forge dir not configured")
	}
	if lastErr != nil {
		return fmt.Errorf("forge session %s: %w", sessionID, lastErr)
	}
	return fmt.Errorf("forge session %s not found", sessionID)
}

func (e *Engine) piebaldPendingSessionIDs(dir string) []string {
	dbPath := parser.FindPiebaldDBPath(dir)
	if dbPath == "" {
		return nil
	}
	metas, err := parser.ListPiebaldSessionMeta(dbPath)
	if err != nil {
		log.Printf("sync piebald: %v", err)
		return nil
	}
	var changed []string
	for _, m := range metas {
		_, storedMtime, ok := e.db.GetFileInfoByPath(m.VirtualPath)
		if ok && storedMtime == m.FileMtime &&
			e.db.GetDataVersionByPath(m.VirtualPath) >= db.CurrentDataVersion() {
			continue
		}
		changed = append(changed, m.SessionID)
	}
	return changed
}

func (e *Engine) countOnePiebaldSessions(dir string) int {
	dbPath := parser.FindPiebaldDBPath(dir)
	if dbPath == "" {
		return 0
	}
	metas, err := parser.ListPiebaldSessionMeta(dbPath)
	if err != nil {
		log.Printf("sync piebald: %v", err)
		return 0
	}
	return len(metas)
}

// syncPiebald syncs sessions from Piebald SQLite databases.
func (e *Engine) syncPiebald(
	ctx context.Context, scope *rootSyncScope,
) []pendingWrite {
	var allPending []pendingWrite
	for _, dir := range e.agentDirs[parser.AgentPiebald] {
		if ctx.Err() != nil {
			break
		}
		if dir == "" || !scope.includes(dir) {
			continue
		}
		allPending = append(allPending, e.syncOnePiebald(ctx, dir)...)
	}
	return allPending
}

// syncOnePiebald handles a single Piebald data directory.
func (e *Engine) syncOnePiebald(
	ctx context.Context, dir string,
) []pendingWrite {
	dbPath := parser.FindPiebaldDBPath(dir)
	changed := e.piebaldPendingSessionIDs(dir)
	if len(changed) == 0 {
		return nil
	}

	var pending []pendingWrite
	for _, cid := range changed {
		if ctx.Err() != nil {
			break
		}
		results, err := parser.ParsePiebaldSessionResults(dbPath, cid, e.machine)
		if err != nil {
			log.Printf("piebald chat %s: %v", cid, err)
			continue
		}
		for _, result := range results {
			pending = append(pending, pendingWrite{
				sess:        result.Session,
				msgs:        result.Messages,
				usageEvents: result.UsageEvents,
			})
		}
	}
	return pending
}

// syncSinglePiebald re-syncs a single Piebald chat. Fork session IDs of the
// form "piebald:<chat>-<row>" are mapped back to their base chat so the parser
// re-emits the main session and every fork branch together.
func (e *Engine) syncSinglePiebald(
	sessionID string,
) error {
	rawID := strings.TrimPrefix(sessionID, "piebald:")
	chatID, _, _ := strings.Cut(rawID, "-")

	var lastErr error
	for _, dir := range e.agentDirs[parser.AgentPiebald] {
		if dir == "" {
			continue
		}
		dbPath := parser.FindPiebaldDBPath(dir)
		if dbPath == "" {
			continue
		}
		results, err := parser.ParsePiebaldSessionResults(dbPath, chatID, e.machine)
		if err != nil {
			lastErr = err
			continue
		}
		if !piebaldResultsContain(results, sessionID) {
			continue
		}
		for _, result := range results {
			if err := e.writeSessionFull(
				pendingWrite{
					sess:        result.Session,
					msgs:        result.Messages,
					usageEvents: result.UsageEvents,
				},
			); err != nil && !errors.Is(err, db.ErrSessionExcluded) {
				return fmt.Errorf("write session %s: %w", result.Session.ID, err)
			}
		}
		if err := e.db.LinkSubagentSessions(); err != nil {
			log.Printf("link subagent sessions: %v", err)
		}
		return nil
	}

	if len(e.agentDirs[parser.AgentPiebald]) == 0 {
		return fmt.Errorf("piebald dir not configured")
	}
	if lastErr != nil {
		return fmt.Errorf("piebald session %s: %w", sessionID, lastErr)
	}
	return fmt.Errorf("piebald session %s not found", sessionID)
}

// piebaldResultsContain reports whether any parsed result has the given
// session ID. Used to verify a requested fork session was actually
// produced by the parser before treating a base-chat lookup as a hit.
func piebaldResultsContain(results []parser.ParseResult, sessionID string) bool {
	for _, r := range results {
		if r.Session.ID == sessionID {
			return true
		}
	}
	return false
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

const scanProgressInterval = 50

// SecretScanInput parameterises ScanSecrets.
type SecretScanInput struct {
	Backfill bool
	Project  string
	Agent    string
	DateFrom string
	DateTo   string
}

// SecretScanProgress is one progress tick.
type SecretScanProgress struct {
	Scanned int `json:"scanned"`
	Total   int `json:"total"`
}

// SecretScanSummary is the final result of a scan.
type SecretScanSummary struct {
	Scanned int `json:"scanned"`
	// WithSecrets counts sessions with ≥1 definite finding. It does NOT
	// include sessions whose findings are all candidate-tier; the
	// presence of those is implied by CandidateFindings > 0 when
	// DefiniteFindings is 0.
	WithSecrets       int `json:"with_secrets"`
	TotalFindings     int `json:"total_findings"`
	DefiniteFindings  int `json:"definite_findings"`
	CandidateFindings int `json:"candidate_findings"`
}

// ScanSecrets scans candidate sessions and persists their findings, invoking
// progress periodically. Resumable: each scanned session records the current
// rules version, so an interrupted backfill resumes by skipping sessions
// already at that version.
func (e *Engine) ScanSecrets(
	ctx context.Context, in SecretScanInput,
	progress func(SecretScanProgress),
) (SecretScanSummary, error) {
	if e.refuseWriteInForceParse("ScanSecrets") {
		return SecretScanSummary{}, errors.New(
			"ScanSecrets refused on report-only parse-diff engine",
		)
	}
	ver := secrets.RulesVersion()
	ids, err := e.db.SecretScanCandidates(ctx, db.SecretScanCandidateFilter{
		CurrentVersion: ver, OnlyStale: in.Backfill,
		Project: in.Project, Agent: in.Agent,
		DateFrom: in.DateFrom, DateTo: in.DateTo,
	})
	if err != nil {
		return SecretScanSummary{}, err
	}
	var sum SecretScanSummary
	total := len(ids)
	for i, id := range ids {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		nf, leak, ok := e.scanOneSession(ctx, id, ver)
		// A cancellation during the scan must end the run with an error,
		// not a partial success. This covers both a failed scan and a
		// successful final session whose context was canceled mid-scan,
		// since scanOneSession does CPU work and a non-context-aware
		// persist after its context-aware reads.
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if !ok {
			continue
		}
		sum.Scanned++
		sum.TotalFindings += nf
		sum.DefiniteFindings += leak
		sum.CandidateFindings += nf - leak
		if leak > 0 {
			sum.WithSecrets++
		}
		if progress != nil && scanShouldReport(i, total) {
			progress(SecretScanProgress{Scanned: sum.Scanned, Total: total})
		}
	}
	return sum, nil
}

// scanOneSession scans one session and persists its findings at ver. Returns
// the finding count, the definite-leak count, and ok=false when the session
// could not be loaded or persisted (skipped, not fatal to the whole run).
//
// Holds syncMu so the read/compute/write path is atomic against a concurrent
// sync replacing this session's messages: otherwise a sync could write fresh
// findings for new messages and then have this scan overwrite them with
// results from a stale snapshot while marking the session current. The lock is
// taken per session, not for the whole scan, so a long backfill does not stall
// the file watcher and periodic sync.
func (e *Engine) scanOneSession(
	ctx context.Context, id, ver string,
) (int, int, bool) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	sess, err := e.db.GetSessionFull(ctx, id)
	if err != nil || sess == nil {
		return 0, 0, false
	}
	msgs, err := e.db.GetAllMessages(ctx, id)
	if err != nil {
		return 0, 0, false
	}
	findings, leak := scanSecretsFromMessages(*sess, msgs, secrets.Scan)
	if err := e.db.ReplaceSessionSecretFindings(id, findings, leak, ver); err != nil {
		log.Printf("secrets scan: persist %s: %v", id, err)
		return 0, 0, false
	}
	return len(findings), leak, true
}

func scanShouldReport(i, total int) bool {
	return (i+1)%scanProgressInterval == 0 || i+1 == total
}
