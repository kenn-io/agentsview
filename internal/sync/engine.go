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
	// ProviderFactories and ProviderMigrationModes select which concrete
	// providers own discovery and parsing for their agents. Nil uses the
	// parser package registry/manifest.
	ProviderFactories      []parser.ProviderFactory
	ProviderMigrationModes map[parser.AgentType]parser.ProviderMigrationMode
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
	// retried when its mtime changes. S3 entries also keep an
	// in-memory source fingerprint when one is available.
	skipMu            gosync.RWMutex
	skipCache         map[string]int64
	skipFingerprints  map[string]string
	s3CodexIndexMu    gosync.Mutex
	s3CodexIndexCache map[string]s3CodexIndexSnapshot
	// idPrefix and pathRewriter support remote sync:
	// prefix all session IDs to avoid collisions, rewrite
	// temp paths to "host:/remote/path" form.
	ephemeral              bool
	idPrefix               string
	pathRewriter           func(string) string
	emitter                Emitter
	providerFactories      map[parser.AgentType]parser.ProviderFactory
	providerMigrationModes map[parser.AgentType]parser.ProviderMigrationMode

	// forceParse disables every stored-state skip (skip cache,
	// size/mtime/data_version checks, incremental JSONL deltas) so
	// parse-diff fully re-parses every discovered file. Normal sync
	// never sets it; behavior must be identical when false.
	forceParse bool

	// phaseStats accumulates per-phase wall-clock time inside the bulk
	// write path. Exposed via PhaseStats() so a CLI driver can log the
	// totals after a sync pass completes.
	phaseStats PhaseStats

	// anomalies accumulates per-run parser/sanitizer anomaly signals
	// recorded at the write seam (prepareSessionWrite, writeIncremental,
	// toDBUsageEvents). Reset at the start of each sync run and folded
	// into the returned SyncStats before the run completes.
	anomalies anomalyAccumulator
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
		skipFingerprints:        make(map[string]string),
		s3CodexIndexCache:       make(map[string]s3CodexIndexSnapshot),
		ephemeral:               cfg.Ephemeral,
		idPrefix:                cfg.IDPrefix,
		pathRewriter:            cfg.PathRewriter,
		emitter:                 cfg.Emitter,
		providerFactories:       providerFactoryMap(providerFactories),
		providerMigrationModes:  providerModes,
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
	_, _, ok := parser.SplitVisualStudioCopilotVirtualPath(path)
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
	e.resetS3CodexIndexCache()

	e.anomalies.reset()
	results := e.startWorkers(context.Background(), files)
	stats = e.collectAndBatch(
		context.Background(), results, len(files), len(files), nil,
		syncWriteDefault,
	)
	e.anomalies.applyTo(&stats)
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
	seen := make(map[string]int, len(paths))
	files := make([]parser.DiscoveredFile, 0, len(paths))
	for _, p := range paths {
		// Codex resolved-index events map to potentially several session
		// sources and must classify even when the event path was deleted, so
		// they are handled by classifyCodexIndexPath. All other changed paths,
		// including Antigravity's sidecar fan-out (annotations, brain,
		// history.jsonl), are owned by each provider-authoritative
		// SourcesForChangedPath via classifyProviderChangedPath.
		dfs := e.classifyCodexIndexPath(p)
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
		case parser.ProviderMigrationProviderAuthoritative:
		default:
			continue
		}
		// Codex index (session_index.jsonl) events are owned by the engine's
		// DB-aware classifyCodexIndexPath, which fans out only to sessions whose
		// stored title changed and resolves a UUID's live/archived duplicate to
		// the path the DB already tracks. The provider's broad index fan-out
		// would re-add every sibling and prefer the live-over-archived layout,
		// resurrecting a stale duplicate over the stored copy, so suppress it
		// here and let the engine method classify the index event.
		if agentType == parser.AgentCodex &&
			filepath.Base(path) == parser.CodexSessionIndexFilename {
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
	if processFileUsesProvider(agent) {
		return eventKind == "remove" &&
			providerDeletedPhysicalSQLiteSource(agent, sourcePath)
	}
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
	// A virtual source path (e.g. a SQLite per-session path
	// "<db>#<sessionID>") is never a real file. Resolve it to its physical
	// container so an existence check reflects whether the backing store is
	// present rather than always reporting the synthetic path as removed.
	statPath := path
	if container, _, ok := parser.ParseVirtualSourcePath(path); ok {
		statPath = container
	}
	if _, err := os.Lstat(statPath); err != nil && os.IsNotExist(err) {
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
	return dedupeDiscoveredFilesByPreference(files, preferDiscoveredFile)
}

func dedupeDiscoveredFilesPreferNewestCodex(
	files []parser.DiscoveredFile,
) []parser.DiscoveredFile {
	return dedupeDiscoveredFilesByPreference(files, preferNewestCodexDiscoveredFile)
}

func dedupeDiscoveredFilesByPreference(
	files []parser.DiscoveredFile,
	prefer func(candidate, current parser.DiscoveredFile) bool,
) []parser.DiscoveredFile {
	if len(files) < 2 {
		return files
	}

	bestByKey := make(map[string]parser.DiscoveredFile, len(files))
	for _, file := range files {
		key := discoveredFileKey(file)
		if current, ok := bestByKey[key]; ok {
			if prefer(file, current) {
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
			return string(file.Agent) + "\x00" +
				discoveredFileIDPrefix(file) + "\x00" + id
		}
	}
	return string(file.Agent) + "\x00" + file.Path
}

func discoveredFileIDPrefix(file parser.DiscoveredFile) string {
	if isS3SourcePath(file.Path) {
		return s3SessionIDPrefix(file.Machine)
	}
	return ""
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

func preferNewestCodexDiscoveredFile(
	candidate, current parser.DiscoveredFile,
) bool {
	if candidate.Agent == parser.AgentCodex && current.Agent == parser.AgentCodex {
		candMTime, candOK := discoveredFileMTime(candidate.Path)
		currMTime, currOK := discoveredFileMTime(current.Path)
		if candOK && currOK && candMTime != currMTime {
			return candMTime > currMTime
		}
	}
	return preferDiscoveredFile(candidate, current)
}

func discoveredFileMTime(path string) (int64, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return info.ModTime().UnixNano(), true
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
		for _, candidate := range parser.ClaudeProjectSessionFiles(claudeDir) {
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

// shelleyDBFile is the shared Shelley conversation database basename. Zed and
// Shelley are provider-authoritative, so their changed-path classification and
// parse run through the provider facade; this constant remains for the
// provider-neutral physical-DB deletion and skip-cache checks in the engine.
const shelleyDBFile = "shelley.db"

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
		oldFileSessions -= e.countRootOpenCodeFormatSessions(
			origDB, parser.AgentKilo,
		)
		oldFileSessions -= e.countRootOpenCodeFormatSessions(
			origDB, parser.AgentMiMoCode,
		)
		oldFileSessions -= e.countRootOpenCodeFormatSessions(
			origDB, parser.AgentIcodemate,
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

	// 3. Point engine at newDB and sync into it. Report discovery as its
	// own phase first: syncAllLocked walks every source before emitting
	// its first syncing event, and on a large archive that walk takes
	// minutes. Without this marker the progress printer credits that
	// silent time to the preceding (instant) "Disabling ..." phase.
	e.openCodeArchiveStore = origDB
	e.db = newDB
	reportResyncPhase(
		PhaseDiscovering,
		"Discovering sessions",
		"",
	)
	stats = e.syncAllLocked(
		ctx, reportResyncProgress, time.Time{}, nil, syncWriteBulk, true,
	)
	e.db = origDB // restore immediately
	e.openCodeArchiveStore = nil
	e.phaseStats.Log("resync")

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
	// OpenCode-format storage is a self-preserving container store that
	// now flows through file discovery, so it is excluded here just as it
	// is subtracted from oldFileSessions above. Otherwise its discovery
	// would mask the disappearance of plain file-backed sessions whose
	// directories went empty.
	emptyDiscovery := stats.nonContainerDiscovered == 0 &&
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
) (stats SyncStats) {
	if ctx.Err() != nil {
		return SyncStats{Aborted: true}
	}

	if recordSyncState {
		e.recordSyncStarted()
	}
	e.phaseStats.Reset()
	e.resetS3CodexIndexCache()
	e.anomalies.reset()
	// Fold the per-run anomaly accumulator into the returned stats on
	// every exit path so the CLI sync summary can surface them.
	defer func() { e.anomalies.applyTo(&stats) }()

	t0 := time.Now()

	// Report discovery as its own phase before the walk. syncAllLocked
	// visits every source before emitting any syncing progress, and on a
	// large archive that walk takes minutes; without this marker a
	// daemon-driven `agentsview sync` shows no terminal feedback until the
	// walk and DB-backed count both finish. The resync path emits the same
	// marker, so its progress printer dedupes on the matching Detail.
	e.reportProgress(onProgress, Progress{
		Phase:  PhaseDiscovering,
		Detail: "Discovering sessions",
	})

	var all []parser.DiscoveredFile
	counts := make(map[parser.AgentType]int)
	providerFound, providerFailures := e.discoverProviderSources(ctx, scope)
	for _, file := range providerFound {
		counts[file.Agent]++
	}
	all = append(all, providerFound...)

	quickSyncCutoff := !since.IsZero()
	if quickSyncCutoff {
		all = e.dedupeClaudeDiscoveredFiles(all)
		// A Codex UUID can exist as both a live dated transcript and a flat
		// archived copy. The provider's discovery deduplicates them to the
		// preferred (live) layout, but the mtime cutoff filter runs before the
		// engine's own dedup, so a changed archived copy that is newer than the
		// cutoff would be lost behind an older live copy that the cutoff drops.
		// Re-expand to every on-disk duplicate before filtering so the cutoff
		// sees each copy's real mtime; the quick-sync dedupe below then keeps
		// the newest surviving duplicate before falling back to normal layout
		// preference.
		all = e.expandCodexProviderDuplicates(all, scope)
		all = e.filterFilesByMtime(ctx, all, since)
	}

	if quickSyncCutoff {
		all = dedupeDiscoveredFilesPreferNewestCodex(all)
	} else {
		all = dedupeDiscoveredFiles(all)
	}
	all = e.dedupeClaudeDiscoveredFiles(all)
	all = e.filterShadowedLegacyKiroFiles(all)

	verbose := onProgress == nil

	// Always log discovery timing: this is the only window into the
	// otherwise-silent provider walk, which dominates resync wall-clock
	// on large archives. Suppressing it behind verbose hid that cost on
	// the daemon resync and interactive sync paths (both pass onProgress).
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

	progressTotal := len(all)
	tDBCount := time.Now()
	dbBackedCount := e.countDBBackedSessions(ctx, scope)
	progressTotal += dbBackedCount
	log.Printf(
		"counted %d db-backed sessions in %s",
		dbBackedCount, time.Since(tDBCount).Round(time.Millisecond),
	)
	e.reportProgress(onProgress, Progress{
		Phase:         PhaseSyncing,
		SessionsTotal: progressTotal,
	})

	nonContainerDiscovered := 0
	for _, f := range all {
		if !isOpenCodeFormatStorageAgent(f.Agent) {
			nonContainerDiscovered++
		}
	}

	tWorkers := time.Now()
	results := e.startWorkers(ctx, all)
	stats = e.collectAndBatch(
		ctx, results, len(all), progressTotal, onProgress, writeMode,
	)
	for range providerFailures {
		stats.RecordFailed()
	}
	stats.nonContainerDiscovered = nonContainerDiscovered
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

	if ctx.Err() != nil {
		stats.Aborted = true
		return stats
	}

	// OpenCode-format sessions (OpenCode and its Kilo and MiMoCode
	// forks) are provider-authoritative: discovery and parsing flow
	// through the provider facade in the file-sync phase above, so no
	// dedicated DB-backed sync pass is needed here.

	// Sync Warp, Forge, and Piebald sessions. These are provider-authoritative
	// DB-backed providers: a shared SQLite DB hosts every session, so the
	// provider facade enumerates sources and parses only the changed ones.
	if scope.includesAny(e.agentDirs[parser.AgentWarp]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentWarp, "warp",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.agentDirs[parser.AgentForge]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentForge, "forge",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.agentDirs[parser.AgentPiebald]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentPiebald, "piebald",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}

	// Link subagent child sessions to their parents after all DB-backed
	// agent writes (including provider-authoritative Forge and Piebald).
	// LinkSubagentSessions is idempotent — its WHERE filter and partial index
	// make it a cheap no-op when nothing new was written — so no guard is
	// needed.
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

	// Store the anomaly-folded stats so LastSyncStats (UI) matches the
	// value returned to the CLI summary. The deferred applyTo only reads
	// the accumulator, so folding a separate copy here does not
	// double-count.
	persisted := stats
	e.anomalies.applyTo(&persisted)
	e.mu.Lock()
	e.lastSync = time.Now()
	e.lastSyncStats = persisted
	e.mu.Unlock()

	if recordSyncState && providerFailures == 0 {
		e.recordSyncFinished()
	}
	// Emission happens in SyncAll / SyncAllSince after syncMu is
	// released; syncAllLocked runs under the caller's lock.
	return stats
}

// slowProviderDiscoveryThreshold is the per-provider discovery duration above
// which discovery timing is logged. Most providers finish in well under a
// millisecond; a provider over this bound is doing real per-source work worth
// surfacing.
const slowProviderDiscoveryThreshold = 100 * time.Millisecond

// discoverProviderSources runs full-sync discovery through the provider facade
// for every concrete provider that is authoritative. It is the sole on-disk
// discovery path: every file-based agent owns discovery through its provider.
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
		tDiscover := time.Now()
		sources, err := provider.Discover(ctx)
		// Log only providers whose discovery is slow enough to matter, so a
		// single pathological provider (e.g. a per-source map rebuild) stands
		// out instead of hiding inside the aggregate discovery timing.
		if d := time.Since(tDiscover); d >= slowProviderDiscoveryThreshold {
			log.Printf(
				"discovery: %s returned %d sources in %s",
				agentType, len(sources), d.Round(time.Millisecond),
			)
		}
		if err != nil {
			log.Printf("%s provider discovery: %v", agentType, err)
			failures++
			continue
		}
		currentSources := providerSourcePathSet(sources)
		forceParseSources := map[string]struct{}{}
		if agentType == parser.AgentVSCopilot {
			missingSources, forceSources :=
				e.visualStudioCopilotMissingVS2026PollSources(
					ctx, provider, filteredRoots, currentSources,
				)
			sources = append(sources, missingSources...)
			maps.Copy(forceParseSources, forceSources)
		}
		forceParseSource := func(sourcePath string) bool {
			_, ok := forceParseSources[filepath.Clean(sourcePath)]
			return ok
		}
		// Forge, Piebald, and Warp are DB-backed providers: a shared SQLite
		// DB hosts every session. Full-sync change detection and counting
		// run through their dedicated provider-driven DB sync phase
		// (syncProviderDBBacked), not the per-source discovery list, so a
		// full sync re-counts only changed sessions exactly as the legacy
		// path did. The watcher path still classifies their changes through
		// classifyProviderChangedPath.
		if processFileUsesProvider(agentType) {
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
			discovered := parser.DiscoveredFile{
				Path:            sourcePath,
				Project:         source.ProjectHint,
				Agent:           agent,
				ProviderSource:  &sourceCopy,
				ProviderProcess: true,
			}
			if forceParseSource(sourcePath) {
				discovered.ForceParse = true
			}
			// S3-aware source sets carry the durable object metadata in the
			// Opaque payload. Thread it into the DiscoveredFile so the S3 sync
			// path (object fetch, fingerprinting, machine-ID namespacing) and the
			// freshness/dedup/mtime-cutoff logic see the same source identity the
			// legacy s3:// discovery emitted directly. Providers read local files,
			// so clear ProviderProcess for s3:// objects: processProviderFile must
			// decline them so they route through processS3Session rather than the
			// provider Fingerprint/Parse path, which cannot read a remote object.
			if s3, ok := source.Opaque.(parser.S3DiscoveredSource); ok {
				discovered.Machine = s3.Machine
				discovered.SourceSize = s3.Size
				discovered.SourceMtime = s3.MtimeNS
				discovered.SourceFingerprint = s3.Fingerprint
				discovered.ProviderProcess = false
				if discovered.Project == "" {
					discovered.Project = s3.Project
				}
			}
			files = append(files, discovered)
		}
	}
	return files, failures
}

func providerSourcePathSet(sources []parser.SourceRef) map[string]struct{} {
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		path := providerDiscoveredPath(source)
		if path == "" {
			continue
		}
		seen[filepath.Clean(path)] = struct{}{}
	}
	return seen
}

func (e *Engine) visualStudioCopilotMissingVS2026PollSources(
	ctx context.Context,
	provider parser.Provider,
	roots []string,
	currentSources map[string]struct{},
) ([]parser.SourceRef, map[string]struct{}) {
	watchRoots := providerChangedPathWatchRoots(ctx, provider, roots)
	var out []parser.SourceRef
	seenHints := make(map[string]struct{})
	forceParseSources := make(map[string]struct{})
	for _, watchRoot := range watchRoots {
		hints, err := e.db.ListStoredSourcePathHints(
			string(parser.AgentVSCopilot), []string{watchRoot},
		)
		if err != nil {
			log.Printf(
				"%s provider poll stored hints: %v",
				parser.AgentVSCopilot, err,
			)
			continue
		}
		for _, hint := range hints {
			hint = filepath.Clean(hint)
			if _, seen := seenHints[hint]; seen {
				continue
			}
			seenHints[hint] = struct{}{}
			container, conversationID, ok :=
				parser.SplitVisualStudioCopilotVirtualPath(hint)
			if !ok ||
				!parser.IsVisualStudioCopilotVS2026SessionPath(container) {
				continue
			}
			if _, ok := currentSources[hint]; ok {
				continue
			}
			if current, ok := e.visualStudioCopilotCurrentPollSource(
				ctx, provider, conversationID,
			); ok {
				sourcePath := providerDiscoveredPath(current)
				if sourcePath == "" {
					continue
				}
				path := filepath.Clean(sourcePath)
				forceParseSources[path] = struct{}{}
				if _, exists := currentSources[path]; !exists {
					currentSources[path] = struct{}{}
					out = append(out, current)
				}
				continue
			}
			if !visualStudioCopilotVS2026PollCanTombstone(
				roots, container,
			) {
				continue
			}
			tombstones, err := provider.SourcesForChangedPath(
				ctx,
				parser.ChangedPathRequest{
					Path:              hint,
					EventKind:         "remove",
					WatchRoot:         watchRoot,
					StoredSourcePaths: []string{hint},
				},
			)
			if err != nil {
				log.Printf(
					"%s provider poll tombstone: %v",
					parser.AgentVSCopilot, err,
				)
				continue
			}
			for _, tombstone := range tombstones {
				sourcePath := providerDiscoveredPath(tombstone)
				if sourcePath == "" {
					continue
				}
				path := filepath.Clean(sourcePath)
				if _, exists := currentSources[path]; exists {
					continue
				}
				currentSources[path] = struct{}{}
				forceParseSources[path] = struct{}{}
				out = append(out, tombstone)
			}
		}
	}
	return out, forceParseSources
}

func visualStudioCopilotVS2026PollCanTombstone(
	roots []string,
	container string,
) bool {
	if container == "" {
		return false
	}
	container = filepath.Clean(container)
	if parser.IsRegularFile(container) {
		return false
	}
	if !reachableDir(filepath.Dir(container)) {
		return false
	}
	return slices.ContainsFunc(roots, func(root string) bool {
		root = filepath.Clean(root)
		return samePathOrDescendant(container, root) && reachableDir(root)
	})
}

func reachableDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info != nil && info.IsDir()
}

func (e *Engine) visualStudioCopilotCurrentPollSource(
	ctx context.Context,
	provider parser.Provider,
	conversationID string,
) (parser.SourceRef, bool) {
	current, ok, err := provider.FindSource(
		ctx,
		parser.FindSourceRequest{
			RawSessionID:       conversationID,
			RequireFreshSource: true,
		},
	)
	if err != nil {
		log.Printf(
			"%s provider poll source lookup: %v",
			parser.AgentVSCopilot, err,
		)
		return parser.SourceRef{}, false
	}
	return current, ok
}

// expandCodexProviderDuplicates re-adds the on-disk duplicate paths of each
// discovered Codex source. The provider deduplicates a UUID's live and archived
// copies to the preferred layout at discovery time; this restores the dropped
// duplicates (scoped to the configured roots) so an mtime cutoff filter can
// judge each copy on its own mtime, matching the legacy discover-then-filter
// order. Non-Codex files and Codex files without a UUID-shaped name pass through
// unchanged. Duplicates are keyed by path so nothing is added twice.
func (e *Engine) expandCodexProviderDuplicates(
	files []parser.DiscoveredFile, scope *rootSyncScope,
) []parser.DiscoveredFile {
	pather := e.codexUUIDPathLister(scope)
	if pather == nil {
		return files
	}
	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		seen[string(f.Agent)+"\x00"+filepath.Clean(f.Path)] = struct{}{}
	}
	out := files
	for _, f := range files {
		if f.Agent != parser.AgentCodex {
			continue
		}
		uuid := parser.CodexSessionUUIDFromFilename(filepath.Base(f.Path))
		if uuid == "" {
			continue
		}
		for _, dup := range pather(uuid) {
			key := string(parser.AgentCodex) + "\x00" + filepath.Clean(dup)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, parser.DiscoveredFile{
				Path:            dup,
				Agent:           parser.AgentCodex,
				ProviderProcess: true,
				ProviderSource:  e.codexPinnedProviderSource(dup),
			})
		}
	}
	return out
}

// codexUUIDPathLister returns a function that lists every on-disk Codex
// transcript path for a UUID under the in-scope roots, or nil when the Codex
// provider is unavailable. It scopes a single provider to the in-scope roots so
// the returned paths cover both the live dated and flat archived copies of a
// duplicated UUID, including duplicates that share one root.
func (e *Engine) codexUUIDPathLister(
	scope *rootSyncScope,
) func(string) []string {
	factory, ok := e.providerFactories[parser.AgentCodex]
	if !ok || factory == nil {
		return nil
	}
	roots := make([]string, 0, len(e.agentDirs[parser.AgentCodex]))
	for _, root := range e.agentDirs[parser.AgentCodex] {
		if root == "" || !scope.includes(root) {
			continue
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   roots,
		Machine: e.machine,
	})
	lister, ok := provider.(interface {
		AllSourcePathsForUUID(string) []string
	})
	if !ok {
		return nil
	}
	return lister.AllSourcePathsForUUID
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
	ctx context.Context,
	files []parser.DiscoveredFile,
	cutoff time.Time,
) []parser.DiscoveredFile {
	cutoffNs := cutoff.UnixNano()
	out := files[:0]
	codexIndexRefresh := make(map[string][]parser.DiscoveredFile)
	for _, f := range files {
		if f.ForceParse {
			out = append(out, f)
			continue
		}
		mtime, err := e.discoveredFileEffectiveMtime(ctx, f)
		if err != nil {
			out = append(out, f)
			continue
		}
		if mtime >= cutoffNs {
			out = append(out, f)
			continue
		}
		if isS3SourcePath(f.Path) && e.s3SourceMetadataChanged(f) {
			out = append(out, f)
			continue
		}
		if f.Agent != parser.AgentCodex {
			continue
		}
		indexNeedsRefresh := false
		if isS3SourcePath(f.Path) {
			indexNeedsRefresh = e.s3CodexIndexNeedsRefreshSince(
				f, cutoffNs,
			)
		} else {
			indexNeedsRefresh = e.codexIndexNeedsRefreshSince(
				f.Path, cutoffNs,
			)
		}
		if !indexNeedsRefresh {
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

// discoveredFileEffectiveMtime returns the freshness timestamp used to filter a
// discovered file against an incremental-sync cutoff. For provider-sourced
// files it consults the provider's Fingerprint so composite/sibling-file
// freshness (for example a Positron session whose workspace.json changed while
// the chat transcript did not) is honored without a per-agent legacy helper.
// Files without a provider source fall back to the legacy mtime computation.
func (e *Engine) discoveredFileEffectiveMtime(
	ctx context.Context, file parser.DiscoveredFile,
) (int64, error) {
	// Codex is excluded from the provider-Fingerprint path on purpose. Its
	// Fingerprint folds the shared session_index.jsonl mtime into every
	// session's freshness (see CodexEffectiveMtime). That shared signal is
	// correct for the skip cache but wrong for the incremental-sync cutoff:
	// when the index changes, both the live and archived copies of a UUID
	// would look fresh, defeating the per-copy mtime discrimination that
	// expandCodexProviderDuplicates relies on to preserve a changed archived
	// duplicate. Index refreshes are handled separately by the codexIndexRefresh
	// pass in filterFilesByMtime, so codex uses its raw per-file mtime here.
	if file.Agent == parser.AgentCodex {
		return discoveredFileMtime(file)
	}
	// S3 objects are discovered through the provider facade (so they carry a
	// ProviderSource), but providers read local files and cannot Fingerprint an
	// s3:// URI. Routing them through providerSourceMtime below would error, and
	// filterFilesByMtime keeps any file whose mtime cannot be resolved, defeating
	// the incremental cutoff and reprocessing every old S3 object on each sync.
	// The threaded object metadata (or a HEAD stat) gives the timestamp directly.
	if isS3SourcePath(file.Path) {
		return discoveredFileMtime(file)
	}
	// Provider-authoritative sources resolve freshness through the provider
	// Fingerprint so composite provider-owned source state participates in
	// incremental-sync cutoff checks.
	if file.ProviderSource != nil && file.ProviderProcess {
		if mtime, ok, err := e.providerSourceMtime(ctx, file); err != nil {
			return 0, err
		} else if ok {
			return mtime, nil
		}
	}
	return discoveredFileMtime(file)
}

// providerSourceMtime resolves a provider-sourced file's effective mtime through
// the owning provider's Fingerprint. The boolean reports whether the provider
// runtime produced a usable timestamp; a false result tells the caller to fall
// back to the legacy mtime path.
func (e *Engine) providerSourceMtime(
	ctx context.Context, file parser.DiscoveredFile,
) (int64, bool, error) {
	if file.ProviderSource == nil {
		return 0, false, nil
	}
	factory, ok := e.providerFactories[file.Agent]
	if !ok || factory == nil {
		return 0, false, nil
	}
	source := *file.ProviderSource
	if source.Provider != "" && source.Provider != file.Agent {
		return 0, false, fmt.Errorf(
			"provider source mismatch for %s: %s",
			file.Agent,
			source.Provider,
		)
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   e.agentDirs[file.Agent],
		Machine: e.machine,
	})
	fingerprint, err := provider.Fingerprint(ctx, source)
	if err != nil {
		return 0, false, err
	}
	if fingerprint.MTimeNS == 0 {
		return 0, false, nil
	}
	return fingerprint.MTimeNS, true, nil
}

func discoveredFileMtime(
	file parser.DiscoveredFile,
) (int64, error) {
	if strings.HasPrefix(file.Path, "s3://") {
		if file.SourceMtime != 0 {
			return file.SourceMtime, nil
		}
		stat := statS3Object
		switch file.Agent {
		case parser.AgentClaude:
			stat = statClaudeS3Session
		case parser.AgentCodex:
			stat = statCodexS3Session
		}
		obj, err := stat(file.Path)
		if err != nil {
			return 0, err
		}
		return obj.LastModified.UnixNano(), nil
	}
	if file.Agent == parser.AgentKiro {
		if _, _, ok := parseKiroSQLiteVirtualPath(file.Path); ok {
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
		if p, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, "threads.db"); ok {
			dbPath = p
		}
		return zedDBCompositeMtime(dbPath)
	}
	if file.Agent == parser.AgentShelley {
		dbPath := file.Path
		if p, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, shelleyDBFile); ok {
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
	byKey := make(map[string][]parser.DiscoveredFile)
	sessionIDByKey := make(map[string]string)
	for _, file := range files {
		if file.Agent != parser.AgentClaude {
			continue
		}
		sessionID := claudeSessionIDFromPath(file.Path)
		if sessionID == "" {
			continue
		}
		key := claudeDiscoveredFileKey(file, sessionID)
		byKey[key] = append(byKey[key], file)
		sessionIDByKey[key] = sessionID
	}
	if len(byKey) == 0 {
		return files
	}

	preferred := make(map[string]parser.DiscoveredFile, len(byKey))
	for key, candidates := range byKey {
		preferred[key] = e.pickPreferredClaudeDiscoveredFile(
			sessionIDByKey[key], candidates,
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
		key := claudeDiscoveredFileKey(file, sessionID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, preferred[key])
	}
	return out
}

func claudeDiscoveredFileKey(
	file parser.DiscoveredFile, sessionID string,
) string {
	return discoveredFileIDPrefix(file) + "\x00" + sessionID
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

	idPrefix := e.idPrefix
	if isS3SourcePath(candidates[0].Path) {
		idPrefix = s3SessionIDPrefix(candidates[0].Machine)
	}
	fullID := applyIDPrefixToID(idPrefix, sessionID)
	storedPath := e.db.GetSessionFilePath(fullID)
	if storedPath != "" {
		for _, candidate := range candidates {
			if e.effectiveSourcePath(candidate.Path) != storedPath {
				continue
			}
			if e.claudeSourceMatchesStored(fullID, candidate) {
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
	sessionID string, file parser.DiscoveredFile,
) bool {
	size, mtime, ok := claudeDiscoveredFileSourceInfo(file)
	if !ok {
		return false
	}
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(sessionID)
	if !ok {
		return false
	}
	if storedSize != size || storedMtime != mtime {
		return false
	}
	if file.SourceFingerprint != "" {
		storedHash, ok := e.db.GetSessionFileHash(sessionID)
		if !ok || storedHash != file.SourceFingerprint {
			return false
		}
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
	candidateSize, _, candidateOK := claudeDiscoveredFileSourceInfo(candidate)
	currentSize, _, currentOK := claudeDiscoveredFileSourceInfo(current)
	if !candidateOK || !currentOK {
		return false
	}
	return candidateSize > currentSize
}

func preferClaudeDiscoveredFile(
	candidate, current parser.DiscoveredFile,
) bool {
	candidateSize, candidateMtime, candidateOK := claudeDiscoveredFileSourceInfo(candidate)
	currentSize, currentMtime, currentOK := claudeDiscoveredFileSourceInfo(current)
	switch {
	case candidateOK && !currentOK:
		return true
	case !candidateOK && currentOK:
		return false
	case candidateOK && currentOK:
		if candidateSize != currentSize {
			return candidateSize > currentSize
		}
		if candidateMtime != currentMtime {
			return candidateMtime > currentMtime
		}
	}
	return candidate.Path < current.Path
}

func claudeDiscoveredFileSourceInfo(
	file parser.DiscoveredFile,
) (size, mtime int64, ok bool) {
	if isS3SourcePath(file.Path) {
		if file.SourceMtime != 0 {
			return file.SourceSize, file.SourceMtime, true
		}
		obj, err := statClaudeS3Session(file.Path)
		if err != nil {
			return 0, 0, false
		}
		return obj.Size, obj.LastModified.UnixNano(), true
	}
	info, err := os.Stat(file.Path)
	if err != nil {
		return 0, 0, false
	}
	return info.Size(), info.ModTime().UnixNano(), true
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

func (e *Engine) countDBBackedProgressTotal(
	agent parser.AgentType, scope *rootSyncScope,
) int {
	if processFileUsesProvider(agent) {
		return e.countProviderDBBackedSessions(
			context.Background(), agent, scope,
		)
	}
	return 0
}

// countProviderDBBackedSessions counts every session a DB-backed provider
// (Forge, Piebald, Warp) currently exposes, via provider discovery. It feeds
// the progress total so the DB-backed family contributes the same fixed count
// the legacy countOne*Sessions helpers did.
func (e *Engine) countProviderDBBackedSessions(
	ctx context.Context, agent parser.AgentType, scope *rootSyncScope,
) int {
	roots := make([]string, 0, len(e.agentDirs[agent]))
	for _, dir := range e.agentDirs[agent] {
		if dir == "" || !scope.includes(dir) {
			continue
		}
		roots = append(roots, dir)
	}
	if len(roots) == 0 {
		return 0
	}
	factory, ok := e.providerFactories[agent]
	if !ok || factory == nil {
		return 0
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   roots,
		Machine: e.machine,
	})
	sources, err := provider.Discover(ctx)
	if err != nil {
		log.Printf("%s provider session count: %v", agent, err)
		return 0
	}
	return len(sources)
}

func (e *Engine) countDBBackedSessions(
	ctx context.Context, scope *rootSyncScope,
) int {
	if ctx.Err() != nil {
		return 0
	}
	total := 0
	for _, agent := range []parser.AgentType{
		parser.AgentWarp,
		parser.AgentForge,
		parser.AgentPiebald,
	} {
		total += e.countDBBackedProgressTotal(agent, scope)
	}
	return total
}

// syncProviderDBBacked enumerates a DB-backed provider's sources, parses only
// the changed ones through the provider facade, and returns their pending
// writes. Change detection compares the provider fingerprint mtime against the
// stored source mtime and requires the stored data version to be current,
// reproducing the legacy *PendingSessionIDs behavior.
func (e *Engine) syncProviderDBBacked(
	ctx context.Context, agent parser.AgentType, scope *rootSyncScope,
) []pendingWrite {
	roots := make([]string, 0, len(e.agentDirs[agent]))
	for _, dir := range e.agentDirs[agent] {
		if dir == "" || !scope.includes(dir) {
			continue
		}
		roots = append(roots, dir)
	}
	if len(roots) == 0 {
		return nil
	}
	factory, ok := e.providerFactories[agent]
	if !ok || factory == nil {
		return nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   roots,
		Machine: e.machine,
	})
	sources, err := provider.Discover(ctx)
	if err != nil {
		log.Printf("sync %s: %v", agent, err)
		return nil
	}

	var pending []pendingWrite
	for _, source := range sources {
		if ctx.Err() != nil {
			break
		}
		fingerprint, err := provider.Fingerprint(ctx, source)
		if err != nil {
			log.Printf("sync %s fingerprint: %v", agent, err)
			continue
		}
		if e.providerDBBackedSourceFresh(source, fingerprint) {
			continue
		}
		outcome, err := provider.Parse(ctx, parser.ParseRequest{
			Source:      source,
			Fingerprint: fingerprint,
			Machine:     e.machine,
		})
		if err != nil {
			log.Printf("sync %s parse: %v", agent, err)
			continue
		}
		for _, result := range outcome.Results {
			pending = append(pending, pendingWrite{
				sess:        result.Result.Session,
				msgs:        result.Result.Messages,
				usageEvents: result.Result.UsageEvents,
			})
		}
	}
	return pending
}

// providerDBBackedSourceFresh reports whether a DB-backed provider source is
// already stored at the current data version with an unchanged source mtime, so
// it can be skipped during a full sync. This is the change-detection half of
// the legacy *PendingSessionIDs helpers.
func (e *Engine) providerDBBackedSourceFresh(
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) bool {
	if e.forceParse {
		return false
	}
	if fingerprint.MTimeNS == 0 {
		return false
	}
	lookupPath := ""
	for _, candidate := range []string{
		fingerprint.Key,
		source.FingerprintKey,
		source.DisplayPath,
		source.Key,
	} {
		if candidate != "" {
			lookupPath = candidate
			break
		}
	}
	if lookupPath == "" {
		return false
	}
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(lookupPath)
	}
	_, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
	if !ok {
		return false
	}
	if storedMtime != fingerprint.MTimeNS {
		return false
	}
	return e.db.GetDataVersionByPath(lookupPath) >= db.CurrentDataVersion()
}

// syncProviderDBBackedAgent runs the full-sync phase for a provider-authoritative
// DB-backed agent (Forge, Piebald, Warp). It mirrors syncOpenCodeFormatAgent:
// only changed sessions are parsed (so the second sync of unchanged data is a
// no-op), and the per-session write semantics match the legacy DB sync.
func (e *Engine) syncProviderDBBackedAgent(
	ctx context.Context, agent parser.AgentType, label string,
	writeMode syncWriteMode, verbose bool, scope *rootSyncScope,
	stats *SyncStats,
	advanceDBProgress func(total int, pending []pendingWrite),
) bool {
	start := time.Now()
	pending := e.syncProviderDBBacked(ctx, agent, scope)
	useWorktreeResolver := agent != parser.AgentPiebald
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
				var err error
				if useWorktreeResolver {
					err = e.writeSessionFullWithResolver(
						pw, resolveWorktreeProject,
					)
				} else {
					err = e.writeSessionFull(pw)
				}
				switch {
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
				e.cacheSkip(r.skipCacheKey(), r.mtime, r.sourceFingerprint)
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
				e.cacheSkip(r.skipCacheKey(), r.mtime, r.sourceFingerprint)
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
			// A Kiro SQLite store is discovered as one container source
			// but fans out into one session per row, so `total` counted it
			// as a single file. Add the extra sessions it produced to keep
			// TotalSessions a session count, matching the per-session tally
			// the legacy syncKiroSQLite phase reported. A zero-session
			// container short-circuits at the empty-result branch above and
			// stays counted as one discovered source, consistent with how
			// every other zero-session file is tallied.
			if len(r.results) > 1 &&
				filepath.Base(r.path) == kiroSQLiteDBName {
				stats.TotalSessions += len(r.results) - 1
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
	// sourceFingerprint carries S3 object fingerprints into
	// skip-cache writes so same-mtime object rewrites do not stay
	// hidden behind a cached parse failure or non-interactive result.
	sourceFingerprint string
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

	// Every registered agent is provider-authoritative, so processProviderFile
	// owns all local-file processing. The only sources that fall through are
	// s3:// Claude/Codex objects, which bypass the provider (its source sets
	// read local files) and use the legacy S3 sync path. Anything else is an
	// unrecognized agent type.
	if !strings.HasPrefix(file.Path, "s3://") {
		return processResult{
			err: fmt.Errorf("unknown agent type: %s", file.Agent),
		}
	}

	if file.SourceMtime == 0 {
		obj, err := statS3SourceObject(file)
		if err != nil {
			return processResult{
				err: fmt.Errorf("stat %s: %w", file.Path, err),
			}
		}
		file.SourceSize = obj.Size
		file.SourceMtime = obj.LastModified.UnixNano()
		file.SourceFingerprint = obj.Fingerprint
	}
	info, err := s3SourceFileInfo(file)
	if err != nil {
		return processResult{
			err: fmt.Errorf("stat %s: %w", file.Path, err),
		}
	}

	// Capture mtime once from the initial stat so all downstream cache
	// operations use a consistent value.
	mtime := info.ModTime().UnixNano()
	cacheSkip := e.shouldCacheSkip(file)
	sourceFingerprint := s3SourceFingerprint(file)

	// Skip files cached from a previous sync whose mtime and source
	// fingerprint are unchanged.
	if cacheSkip && !e.forceParse && !file.ForceParse { // parse-diff: ignore the skip cache
		if e.shouldUseCachedSkip(file, mtime, sourceFingerprint) {
			if e.pathNeedsCachedSkipBypass(file.Path) {
				e.clearSkip(file.Path)
			} else {
				return processResult{
					skip:      true,
					mtime:     mtime,
					cacheSkip: true,
				}
			}
		}
	}

	var res processResult
	switch file.Agent {
	case parser.AgentClaude, parser.AgentCodex:
		res = e.processS3Session(ctx, file, info)
	default:
		res = processResult{
			err: fmt.Errorf("unsupported s3 agent type: %s", file.Agent),
		}
	}
	res.cacheSkip = cacheSkip
	res.mtime = mtime
	res.sourceFingerprint = sourceFingerprint
	return res
}

func (e *Engine) shouldUseCachedSkip(
	file parser.DiscoveredFile, mtime int64, sourceFingerprint string,
) bool {
	e.skipMu.RLock()
	cachedMtime, cached := e.skipCache[file.Path]
	cachedFingerprint := ""
	if e.skipFingerprints != nil {
		cachedFingerprint = e.skipFingerprints[file.Path]
	}
	e.skipMu.RUnlock()
	if !cached || cachedMtime != mtime {
		return false
	}
	if isS3SourcePath(file.Path) && sourceFingerprint != "" {
		return cachedFingerprint == sourceFingerprint
	}
	return true
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

func (e *Engine) pathNeedsCachedSkipBypass(path string) bool {
	return e.pathNeedsProjectReparse(path) ||
		e.pathNeedsDataVersionReparse(path)
}

func (e *Engine) pathNeedsDataVersionReparse(path string) bool {
	if e == nil || e.db == nil {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	if _, _, ok := e.db.GetFileInfoByPath(lookupPath); !ok {
		return false
	}
	return e.db.GetDataVersionByPath(lookupPath) < db.CurrentDataVersion()
}

func (e *Engine) processProviderFile(
	ctx context.Context,
	file parser.DiscoveredFile,
) (processResult, bool) {
	mode := e.providerMigrationModes[file.Agent]
	usesProvider := processFileUsesProvider(file.Agent)
	if mode != parser.ProviderMigrationProviderAuthoritative && !usesProvider {
		return processResult{}, false
	}
	// S3 sources are not provider-owned: the provider source sets read local
	// files, so s3:// paths use the legacy S3 sync path (processS3Session),
	// which handles object fetch, fingerprinting, and per-agent skip logic.
	if strings.HasPrefix(file.Path, "s3://") {
		return processResult{}, false
	}
	if file.ProviderSource != nil && !file.ProviderProcess && !usesProvider {
		return processResult{}, false
	}

	factory, ok := e.providerFactories[file.Agent]
	if !ok {
		return processResult{
			err: fmt.Errorf("provider not found for agent type: %s", file.Agent),
		}, true
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:        e.agentDirs[file.Agent],
		Machine:      e.machine,
		PathRewriter: e.pathRewriter,
	})

	source, found, err := e.providerSourceForDiscoveredFile(ctx, provider, file)
	if err != nil {
		return processResult{err: err}, true
	}
	if !found {
		// A forced parse on a deleted shared SQLite database (Zed, Shelley)
		// resolves to no source because the physical file is gone. Mirror the
		// legacy deleted-source handling: complete the source as an empty
		// force-replace so the engine retires every session that lived in the
		// removed database instead of failing the sync.
		if file.ForceParse &&
			providerDeletedPhysicalSQLiteSource(file.Agent, file.Path) {
			return processResult{forceReplace: true}, true
		}
		return processResult{
			err: fmt.Errorf(
				"%s provider source not found for %s",
				file.Agent,
				file.Path,
			),
		}, true
	}

	// SyncSingleSession resolves a single session by ID and carries the
	// caller-preferred project (typically the DB-preserved value, so a
	// user override is not reverted) on file.Project without an explicit
	// ProviderSource. Provider FindSource re-derives ProjectHint from the
	// path, so honor the caller's project as the hint in that case. Full
	// discovery and changed-path classification always supply
	// file.ProviderSource, whose ProjectHint stays authoritative.
	if file.ProviderSource == nil && file.Project != "" {
		source.ProjectHint = file.Project
	}

	// DB-freshness skip for single-session JSONL providers (Claude):
	// when the stored session's size, mtime, and data version already
	// match the source and its project does not need reparse, skip the
	// parse entirely. This reproduces the legacy process arm's
	// shouldSkipFile gate so an unchanged session is not re-parsed on
	// every full sync.
	if mtime, fresh := e.providerSingleSessionFresh(ctx, provider, source, file); fresh {
		return processResult{
			skip:  true,
			mtime: mtime,
		}, true
	}
	if freshMtime, fresh := e.providerSourceFreshBeforeFingerprint(source, file); fresh {
		return processResult{
			skip:  true,
			mtime: freshMtime,
		}, true
	}

	fingerprint, err := provider.Fingerprint(ctx, source)
	if err != nil {
		if file.ForceParse &&
			providerDeletedPhysicalSQLiteSource(file.Agent, file.Path) &&
			errors.Is(err, os.ErrNotExist) {
			return processResult{
				excludedSessionIDs: e.providerSourceSessionIDsForForceReplace(
					file.Agent,
					source,
				),
				forceReplace: true,
			}, true
		}
		return processResult{err: err}, true
	}
	cacheKey := providerProcessCacheKey(file, source, fingerprint)
	cacheSkip := e.shouldCacheSkip(file)
	if cacheSkip && !e.forceParse && !file.ForceParse {
		e.skipMu.RLock()
		cachedMtime, cached := e.skipCache[cacheKey]
		e.skipMu.RUnlock()
		if cached && cachedMtime == fingerprint.MTimeNS {
			// A cached skip must not hide a session whose stored row needs
			// self-healing (e.g. a parser data-version bump or generated
			// roborev CI worktree project): clear the entry and fall through
			// to a full reparse, mirroring the legacy process arm.
			if e.pathNeedsCachedSkipBypass(file.Path) {
				e.clearSkip(cacheKey)
			} else {
				return processResult{
					skip:      true,
					mtime:     fingerprint.MTimeNS,
					cacheSkip: true,
					cacheKey:  cacheKey,
				}, true
			}
		}
	}
	if cacheSkip && e.shouldSkipProviderSource(file, source, fingerprint) {
		return processResult{
			skip:      true,
			mtime:     fingerprint.MTimeNS,
			cacheSkip: true,
			cacheKey:  cacheKey,
		}, true
	}

	// Append-only incremental parse for already-synced JSONL files.
	// When the incremental path declines but signals forceReplace,
	// carry the flag onto the full parse so the write path replaces
	// stored messages instead of appending on top of stale rows.
	incRes, incOK := e.tryProviderIncrementalAppend(
		ctx, provider, source, file, fingerprint,
	)
	if incOK {
		incRes.mtime = fingerprint.MTimeNS
		incRes.cacheSkip = cacheSkip
		incRes.cacheKey = cacheKey
		return incRes, true
	}
	incForceReplace := incRes.forceReplace

	// DB-stored fingerprint skip. The provider has no database handle, so the
	// engine reproduces the legacy DB-aware skip that single-session JSONL
	// providers relied on: an unchanged source whose stored size and effective
	// mtime already match is not reparsed, even when the in-memory skip cache
	// was cleared (e.g. by SyncSingleSession) or never populated (a fresh
	// engine). For Codex this also folds in the session_index.jsonl sidecar:
	// a shared index mtime bump that did not change this session's title must
	// not trigger a reparse.
	if !e.forceParse && !file.ForceParse &&
		e.shouldSkipProviderSourceByDB(file, fingerprint) {
		return processResult{
			skip:        true,
			mtime:       fingerprint.MTimeNS,
			cacheSkip:   cacheSkip,
			cacheKey:    cacheKey,
			noCacheSkip: true,
		}, true
	}

	// DB-stored-file-info skip: a session whose persisted file_size/file_mtime
	// already match the source fingerprint (and whose data_version is current)
	// is unchanged and need not be reparsed. This reproduces the legacy
	// shouldSkipByPath behavior the per-agent process methods provided before the
	// migration, so a repeat full/periodic sync of an untouched
	// provider-authoritative session (OpenHands, Cursor, Hermes, Vibe, ...)
	// skips instead of rewriting. It only skips on an exact size+mtime match, so
	// a provider whose fingerprint mtime differs from the stored value simply
	// reparses, matching the prior behavior. Claude and Cowork have their own
	// earlier freshness checks; this is the generic fallback for the rest.
	if !e.forceParse && !file.ForceParse &&
		e.providerSourceUnchangedInDB(source, fingerprint) {
		return processResult{
			skip:      true,
			mtime:     fingerprint.MTimeNS,
			cacheSkip: cacheSkip,
			cacheKey:  cacheKey,
		}, true
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
		excludedSessionIDs := append([]string(nil), outcome.ExcludedSessionIDs...)
		if outcome.ForceReplace && outcome.ResultSetComplete {
			excludedSessionIDs = append(
				excludedSessionIDs,
				e.providerSourceSessionIDsForForceReplace(file.Agent, source)...,
			)
		}
		return processResult{
			skip:               !outcome.ForceReplace,
			excludedSessionIDs: excludedSessionIDs,
			mtime:              fingerprint.MTimeNS,
			cacheSkip:          cacheSkip,
			cacheKey:           cacheKey,
			noCacheSkip:        !cleanCache,
			forceReplace:       outcome.ForceReplace,
		}, true
	}

	res := processResult{
		results:               e.dropUnchangedSharedSQLiteResults(file, parseOutcomeResults(outcome.Results)),
		excludedSessionIDs:    append([]string(nil), outcome.ExcludedSessionIDs...),
		mtime:                 fingerprint.MTimeNS,
		cacheSkip:             cacheSkip,
		cacheKey:              cacheKey,
		noCacheSkip:           !cleanCache,
		forceReplace:          outcome.ForceReplace || incForceReplace,
		suppressPresenceSweep: !outcome.ResultSetComplete,
	}
	// Incremental-append providers (Claude) need the stored file
	// identity so a later sync can detect an atomic file replacement
	// (new inode/device) and fall back to a full parse instead of
	// appending on top of stale state. Match the legacy process arm,
	// which stamped inode/device from the source file stat.
	e.stampProviderFileIdentity(provider, source, res.results)
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
	e.applyProviderFilePathPolicies(provider, file.Agent, &res)
	return res, true
}

// dropUnchangedSharedSQLiteResults reproduces the legacy per-session skip the
// folded processZed/processShelley loops and the aiderFileUnchanged check
// performed. Zed and Shelley keep every session in one shared SQLite database,
// and Aider fans every run out of one shared history file, so the provider
// re-parses every session on any change to that shared source. Without a
// per-session filter the engine would rewrite and recount unchanged sessions.
// This drops results whose stored file_mtime (and, for Shelley's
// second-precision timestamps and Aider's whole-file content hash, the
// fingerprint stored in file_hash) and data_version already match, using the
// path rewriter so remote stored paths resolve. Force-parse runs (parse-diff,
// single-session resync) keep every result so they always re-emit.
func (e *Engine) dropUnchangedSharedSQLiteResults(
	file parser.DiscoveredFile,
	results []parser.ParseResult,
) []parser.ParseResult {
	if e.forceParse || file.ForceParse || len(results) == 0 {
		return results
	}
	compareHash := false
	switch file.Agent {
	case parser.AgentShelley:
		compareHash = true
	case parser.AgentAider:
		// Every aider run in a history file shares the file's content hash, so
		// a same-mtime append/truncate is caught by the hash compare.
		compareHash = true
	case parser.AgentZed, parser.AgentKiro:
		// Zed and Kiro fan one container DB out to a session per row and have no
		// per-row content hash, so unchanged rows are detected by mtime plus
		// data version, matching their legacy container sync. Without Kiro here
		// every Kiro row is reparsed and rewritten on every full sync.
	default:
		return results
	}

	kept := results[:0]
	for _, r := range results {
		path := r.Session.File.Path
		if path == "" {
			kept = append(kept, r)
			continue
		}
		lookupPath := path
		if e.pathRewriter != nil {
			lookupPath = e.pathRewriter(path)
		}
		_, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
		if !ok || storedMtime != r.Session.File.Mtime {
			kept = append(kept, r)
			continue
		}
		if compareHash {
			storedHash, _ := e.db.GetFileHashByPath(lookupPath)
			if storedHash != r.Session.File.Hash {
				kept = append(kept, r)
				continue
			}
		}
		if e.db.GetDataVersionByPath(lookupPath) < db.CurrentDataVersion() {
			kept = append(kept, r)
			continue
		}
		// Unchanged: drop so the write batch neither rewrites nor recounts it.
	}
	return kept
}

func (e *Engine) providerSourceSessionIDsForForceReplace(
	agent parser.AgentType,
	source parser.SourceRef,
) []string {
	root := ""
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		if candidate != "" {
			root = candidate
			break
		}
	}
	if root == "" {
		return nil
	}
	if e.pathRewriter != nil {
		root = e.pathRewriter(root)
	}
	sourcePaths, err := e.db.ListStoredSourcePathHints(string(agent), []string{root})
	if err != nil {
		log.Printf("list provider force-replace source hints: %v", err)
		return nil
	}
	seen := make(map[string]struct{})
	var ids []string
	for _, sourcePath := range sourcePaths {
		pathIDs, err := e.db.ListSessionIDsByFilePath(sourcePath, string(agent))
		if err != nil {
			log.Printf("list provider force-replace sessions: %v", err)
			continue
		}
		for _, id := range pathIDs {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

// applyProviderFilePathPolicies reproduces the DB-aware, file-path-scoped
// session bookkeeping that a provider cannot do on its own (it has no database
// handle). It runs only for single-session-per-file providers whose canonical
// ID can change while the source path is unchanged (e.g. Vibe, whose ID flips
// between the meta.json session_id and the directory-name fallback as meta.json
// appears or is removed). Multi-session sources are skipped, where several
// distinct sessions legitimately share one path; for stable-ID providers it is
// a no-op because the stored ID always matches the freshly parsed one.
//
// Two policies are applied per result, keyed by the (path-rewritten) file_path:
//
//  1. Resurrection guard: if the user removed the session occupying this path —
//     a trashed row at the same path, or an alternate identity for the path
//     (the provider's excluded fallback ID, or a stale stored ID) that is now
//     trashed or permanently excluded — the freshly parsed row must not be
//     written under its new ID. The result is dropped and its ID is excluded.
//  2. Stale-row cleanup: any other live stored ID at the same path that the
//     current parse no longer emits is added to the exclusion list so the
//     superseded row is deleted.
func (e *Engine) applyProviderFilePathPolicies(
	provider parser.Provider,
	agent parser.AgentType,
	res *processResult,
) {
	if provider.Capabilities().Source.MultiSessionSource == parser.CapabilitySupported {
		return
	}
	if len(res.results) == 0 {
		return
	}

	excluded := make(map[string]struct{}, len(res.excludedSessionIDs))
	for _, id := range e.applyIDPrefixToSessionIDs(res.excludedSessionIDs) {
		excluded[id] = struct{}{}
	}
	addExclusion := func(id string) {
		if id == "" {
			return
		}
		if _, ok := excluded[id]; ok {
			return
		}
		excluded[id] = struct{}{}
		res.excludedSessionIDs = append(res.excludedSessionIDs, id)
	}

	kept := res.results[:0]
	for _, result := range res.results {
		path := result.Session.File.Path
		if path == "" {
			kept = append(kept, result)
			continue
		}
		lookupPath := path
		if e.pathRewriter != nil {
			lookupPath = e.pathRewriter(path)
		}
		currentID := result.Session.ID
		currentPrefixedID := e.idPrefix + result.Session.ID

		existingIDs, err := e.db.ListSessionIDsByFilePath(lookupPath, string(agent))
		if err != nil {
			log.Printf("list session IDs by file path: %v", err)
			kept = append(kept, result)
			continue
		}

		// Resurrection guard. The path's identity is removed when a trashed row
		// shares it, or when any alternate identity for the path (the
		// provider's excluded fallback IDs or a stale stored ID) is trashed or
		// permanently excluded. In that case the new row must not be written.
		suppress := e.db.HasTrashedSessionByFilePath(lookupPath, string(agent))
		if !suppress {
			for id := range excluded {
				if id == currentID || id == currentPrefixedID {
					continue
				}
				if e.db.IsSessionExcluded(id) || e.db.IsSessionTrashed(id) {
					suppress = true
					break
				}
			}
		}
		if !suppress {
			for _, id := range existingIDs {
				if id == currentID || id == currentPrefixedID {
					continue
				}
				if e.db.IsSessionExcluded(id) || e.db.IsSessionTrashed(id) {
					suppress = true
					break
				}
			}
		}
		if suppress {
			// Keep a trashed current ID trashed rather than converting it to a
			// parser deletion; the upsert's trash guard already hides it.
			if (currentPrefixedID == "" || !e.db.IsSessionTrashed(currentPrefixedID)) &&
				!e.db.IsSessionTrashed(currentID) {
				addExclusion(currentID)
			}
			continue
		}

		// Stale-row cleanup for live siblings the current parse supersedes.
		for _, id := range existingIDs {
			if id == currentID || id == currentPrefixedID {
				continue
			}
			addExclusion(id)
		}
		kept = append(kept, result)
	}
	res.results = kept
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

func processFileUsesProvider(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentForge, parser.AgentPiebald, parser.AgentWarp:
		return true
	default:
		return false
	}
}

func (e *Engine) shouldSkipProviderSource(
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) bool {
	agent := file.Agent
	if agent == "" {
		agent = source.Provider
	}
	if !providerSourceSupportsPersistedFreshness(agent) {
		return false
	}
	if e.forceParse || file.ForceParse {
		return false
	}
	lookupPath := providerSkipLookupPath(file, source, fingerprint)
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(lookupPath)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
	if !ok {
		return false
	}
	if fingerprint.Size != 0 && storedSize != fingerprint.Size {
		return false
	}
	if storedMtime != fingerprint.MTimeNS {
		return false
	}
	return e.db.GetDataVersionByPath(lookupPath) >= db.CurrentDataVersion()
}

func providerSourceSupportsPersistedFreshness(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentForge, parser.AgentWarp:
		return true
	default:
		return false
	}
}

func providerSkipLookupPath(
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) string {
	for _, path := range []string{
		fingerprint.Key,
		source.FingerprintKey,
		source.DisplayPath,
		source.Key,
		file.Path,
	} {
		if path != "" {
			return path
		}
	}
	return file.Path
}

func (e *Engine) shouldCacheSkip(
	file parser.DiscoveredFile,
) bool {
	if file.Agent == parser.AgentKiro {
		if filepath.Base(file.Path) == kiroSQLiteDBName {
			return false
		}
		if _, _, ok := parseKiroSQLiteVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentZed {
		if filepath.Base(file.Path) == "threads.db" {
			return false
		}
		if _, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, "threads.db"); ok {
			return false
		}
	}
	if file.Agent == parser.AgentShelley {
		if filepath.Base(file.Path) == shelleyDBFile {
			return false
		}
		if _, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, shelleyDBFile); ok {
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
			parser.SplitVisualStudioCopilotVirtualPath(file.Path); ok {
			return false
		}
	}
	if file.Agent == parser.AgentAider {
		// Aider fans one physical history file out into per-run virtual
		// sessions. A mtime-only skip can hide same-mtime content changes,
		// missing run rows, or stale per-run data versions before the
		// provider fingerprint and dropUnchangedSharedSQLiteResults hash
		// checks run, so all Aider freshness stays on that provider-aware path.
		return false
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
func (e *Engine) cacheSkip(path string, mtime int64, sourceFingerprint ...string) {
	e.skipMu.Lock()
	e.skipCache[path] = mtime
	fingerprint := ""
	if len(sourceFingerprint) > 0 {
		fingerprint = sourceFingerprint[0]
	}
	if fingerprint != "" {
		if e.skipFingerprints == nil {
			e.skipFingerprints = make(map[string]string)
		}
		e.skipFingerprints[path] = fingerprint
	} else if e.skipFingerprints != nil {
		delete(e.skipFingerprints, path)
	}
	e.skipMu.Unlock()
}

// clearSkip removes a skip-cache entry when a file
// produces a valid session.
func (e *Engine) clearSkip(path string) {
	e.skipMu.Lock()
	delete(e.skipCache, path)
	delete(e.skipFingerprints, path)
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
// S3 callers pass an object fingerprint to guard same-size,
// same-timestamp rewrites on object stores with coarse mtimes.
func (e *Engine) shouldSkipFile(
	sessionID string, info os.FileInfo,
) bool {
	return e.shouldSkipFileWithPrefix(e.idPrefix, sessionID, info)
}

// providerSourceUnchangedInDB reports whether a provider source's persisted
// file metadata already matches its current fingerprint, so a reparse would be
// a no-op. It compares the DB-stored file_size/file_mtime for the source's
// path against the fingerprint and requires a current data_version, mirroring
// shouldSkipByPath for the provider-authoritative runtime. A source with no
// stored row, an empty key, or a non-fingerprint identity (no size, e.g. a
// tombstone) never matches and therefore reparses.
func (e *Engine) providerSourceUnchangedInDB(
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) bool {
	if fingerprint.MTimeNS == 0 && fingerprint.Size == 0 {
		return false
	}
	lookupPath := providerDiscoveredPath(source)
	if lookupPath == "" {
		return false
	}
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(lookupPath)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
	if !ok {
		return false
	}
	if storedSize != fingerprint.Size || storedMtime != fingerprint.MTimeNS {
		return false
	}
	// A stale stored project (e.g. a generated roborev CI worktree name)
	// must defeat the unchanged-source skip so the corrected project is
	// reparsed, mirroring shouldSkipCodexFingerprint and the in-memory
	// skip-cache bypass in processProviderFile.
	if project, ok := e.db.GetProjectByPath(lookupPath); ok &&
		parser.NeedsProjectReparse(project) {
		return false
	}
	return e.db.GetDataVersionByPath(lookupPath) >= db.CurrentDataVersion()
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
	fName  string
	fSize  int64
	fMtime int64
}

func (f fakeSnapshotInfo) Name() string      { return f.fName }
func (f fakeSnapshotInfo) Size() int64       { return f.fSize }
func (f fakeSnapshotInfo) Mode() os.FileMode { return 0 }
func (f fakeSnapshotInfo) ModTime() time.Time {
	return time.Unix(0, f.fMtime)
}
func (f fakeSnapshotInfo) IsDir() bool { return false }
func (f fakeSnapshotInfo) Sys() any    { return nil }

// providerSingleSessionFresh reports whether a single-session JSONL
// provider's source (Claude) maps to a stored session that is already
// up to date: the source size and mtime match what is stored, the row
// is at the current parser data version, and its project does not need
// reparse. It reproduces the legacy Claude process arm's shouldSkipFile
// gate so an unchanged session is skipped instead of re-parsed every
// full sync. Providers without incremental append, multi-session
// sources, or sources that are not a single physical file are never
// considered fresh here and always fall through to the full parse.
func (e *Engine) providerSingleSessionFresh(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
	file parser.DiscoveredFile,
) (int64, bool) {
	// Match the legacy shouldSkipFile gate, which keyed off the
	// engine-wide forceParse (parse-diff) flag only. A per-file
	// ForceParse (set by SyncSingleSession to bypass the error skip
	// cache) must not defeat the DB-freshness skip: an unchanged session
	// is still skipped so a single-session resync does not, for example,
	// reapply a worktree project mapping to a file that has not changed.
	if e.forceParse {
		return 0, false
	}
	// Claude is the single-physical-file provider that takes the
	// append-only incremental path. Its source stem is the session ID,
	// so DB freshness can be checked by that ID even though a DAG fork
	// can later split the file into several sessions.
	if provider.Capabilities().Source.IncrementalAppend !=
		parser.CapabilitySupported {
		return 0, false
	}
	path := providerDiscoveredPath(source)
	if path == "" {
		return 0, false
	}
	sessionID := claudeSessionIDFromPath(path)
	if sessionID == "" {
		return 0, false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	info, err := os.Stat(lookupPath)
	if err != nil {
		info, err = os.Stat(path)
		if err != nil {
			return 0, false
		}
	}
	if !e.shouldSkipFile(sessionID, info) {
		return 0, false
	}
	sess, _ := e.db.GetSession(ctx, e.idPrefix+sessionID)
	return info.ModTime().UnixNano(), sess != nil &&
		sess.Project != "" &&
		!parser.NeedsProjectReparse(sess.Project)
}

func (e *Engine) providerSourceFreshBeforeFingerprint(
	source parser.SourceRef,
	file parser.DiscoveredFile,
) (int64, bool) {
	if e.forceParse || file.ForceParse {
		return 0, false
	}
	path := providerDiscoveredPath(source)
	if path == "" {
		return 0, false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	info, err := os.Stat(lookupPath)
	if err != nil {
		info, err = os.Stat(path)
		if err != nil {
			return 0, false
		}
	}
	switch file.Agent {
	case parser.AgentCowork:
		mtime := parser.CoworkSessionMtime(path, info.ModTime().UnixNano())
		effectiveInfo := fakeSnapshotInfo{
			fSize:  info.Size(),
			fMtime: mtime,
		}
		if e.shouldSkipByPath(path, effectiveInfo) {
			return mtime, true
		}
	// Gemini is deliberately absent here. Its fingerprint is composite (the
	// session file plus projects.json and trustedFolders.json), so a
	// pre-fingerprint skip keyed only on the session file's size and mtime
	// would skip a session whose project metadata changed while the transcript
	// did not, leaving a stale project on scheduled syncs. Gemini relies on the
	// post-fingerprint skip cache instead, whose mtime folds in the composite.
	case parser.AgentCopilot:
		mtime := copilotEffectiveMtime(path, info)
		effectiveInfo := fakeSnapshotInfo{
			fSize:  info.Size(),
			fMtime: mtime,
		}
		if e.shouldSkipByPath(path, effectiveInfo) {
			return mtime, true
		}
	}
	return 0, false
}

// stampProviderFileIdentity copies the source file's inode and device onto
// every parsed result for an incremental-append provider (Claude). The
// legacy process arm stamped this identity from the source stat so the
// incremental path can later detect an atomic file replacement and fall
// back to a full parse. Providers whose source is not a single physical
// file, or that do not support incremental append, are left untouched.
func (e *Engine) stampProviderFileIdentity(
	provider parser.Provider,
	source parser.SourceRef,
	results []parser.ParseResult,
) {
	if provider.Capabilities().Source.IncrementalAppend !=
		parser.CapabilitySupported {
		return
	}
	path := providerDiscoveredPath(source)
	if path == "" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	inode, device := getFileIdentity(info)
	for i := range results {
		results[i].Session.File.Inode = inode
		results[i].Session.File.Device = device
	}
}

// tryProviderIncrementalAppend reproduces the legacy incremental-append
// sync path for a provider-authoritative agent that supports append-only
// incremental parsing (Claude). The provider owns the byte-offset parse
// via ParseIncremental, but the engine still owns the DB-aware
// bookkeeping (session lookup, data-version and identity guards, ordinal
// resume, cross-sync split detection, and cumulative counters), so this
// drives the shared tryIncrementalJSONL with an adapter that calls the
// provider. Returns (result, true) when the incremental path produced a
// terminal result, or (result, false) to fall through to the full
// provider parse (carrying any forceReplace signal).
func (e *Engine) tryProviderIncrementalAppend(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
	file parser.DiscoveredFile,
	fingerprint parser.SourceFingerprint,
) (processResult, bool) {
	// Match the legacy tryIncrementalJSONL gate, which suppressed append
	// deltas only under the engine-wide forceParse (parse-diff) flag. A
	// per-file ForceParse does not disable incremental append.
	if e.forceParse {
		return processResult{}, false
	}
	if provider.Capabilities().Source.IncrementalAppend !=
		parser.CapabilitySupported {
		return processResult{}, false
	}
	path := providerDiscoveredPath(source)
	if path == "" {
		return processResult{}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return processResult{}, false
	}

	parseFn := func(
		_ string, offset int64, startOrdinal int, lastEntryUUID string,
	) ([]parser.ParsedMessage, time.Time, int64, error) {
		outcome, status, perr := provider.ParseIncremental(
			ctx,
			parser.IncrementalRequest{
				Source:        source,
				Fingerprint:   fingerprint,
				SessionID:     e.idPrefix + claudeSessionIDFromPath(path),
				Offset:        offset,
				StartOrdinal:  startOrdinal,
				Machine:       e.machine,
				LastEntryUUID: lastEntryUUID,
			},
		)
		if perr != nil {
			return nil, time.Time{}, 0, perr
		}
		switch status {
		case parser.IncrementalNeedsFullParse:
			if outcome.ForceReplace {
				// Signal the shared helper to fall back to a
				// full parse that replaces stored messages.
				return nil, time.Time{}, 0,
					parser.ErrClaudeIncrementalNeedsFullParse
			}
			// A plain full-parse fallback (e.g. DAG detected):
			// return a non-fallback error so the helper runs a
			// normal full parse without forceReplace.
			return nil, time.Time{}, 0, parser.ErrDAGDetected
		case parser.IncrementalNoNewData:
			return nil, time.Time{}, 0, nil
		default:
			return outcome.Messages, outcome.EndedAt,
				outcome.ConsumedBytes, nil
		}
	}

	return e.tryIncrementalJSONL(file, info, file.Agent, parseFn)
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
			return processResult{forceReplace: true}, false
		}
	}
	if currentSize < inc.FileSize {
		log.Printf(
			"incremental %s %s: file truncated from %d to %d, full parse",
			agent, file.Path, inc.FileSize, currentSize,
		)
		return processResult{forceReplace: true}, false
	}
	if currentSize == inc.FileSize {
		log.Printf(
			"incremental %s %s: file size unchanged at %d but changed since last sync, full parse",
			agent, file.Path, currentSize,
		)
		return processResult{forceReplace: true}, false
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

// shouldSkipProviderSourceByDB reports whether a provider-dispatched source is
// already stored at the parsed fingerprint and can be skipped without a reparse.
// It is the engine-side replacement for the DB-aware skip the legacy
// single-session JSONL processors performed, since a provider has no database
// handle. It is scoped to Codex: Codex's effective mtime folds in the shared
// session_index.jsonl sidecar, so a size-and-effective-mtime match plus a
// per-session title check preserves the legacy "skip when only the global index
// advanced but this session's name did not" semantics. Other providers keep
// their existing in-memory skip-cache behavior unchanged.
func (e *Engine) shouldSkipProviderSourceByDB(
	file parser.DiscoveredFile, fingerprint parser.SourceFingerprint,
) bool {
	if file.Agent != parser.AgentCodex {
		return false
	}
	return e.shouldSkipCodexFingerprint(file.Path, fingerprint)
}

// shouldSkipCodexFingerprint reproduces the legacy shouldSkipCodex decision in
// terms of a provider SourceFingerprint. The fingerprint MTimeNS already folds
// in session_index.jsonl via CodexEffectiveMtime, so:
//   - a stored size mismatch or stale data version forces a reparse;
//   - an exact effective-mtime match skips;
//   - an effective mtime ahead of the stored mtime driven only by the index
//     (the raw transcript mtime is still at or below the stored mtime) skips
//     unless this session's stored title differs from the current index title.
func (e *Engine) shouldSkipCodexFingerprint(
	path string, fingerprint parser.SourceFingerprint,
) bool {
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByPath(lookupPath)
	if !ok || storedSize != fingerprint.Size {
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
	effectiveMtime := fingerprint.MTimeNS
	if storedMtime == effectiveMtime {
		return true
	}
	fileMtime := effectiveMtime
	if info, err := os.Stat(path); err == nil {
		fileMtime = info.ModTime().UnixNano()
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
	return e.codexStoredNameDiffersBySession(
		stored, currentName,
	)
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
			if src := e.codexSourceFileForUUID(root, uuid); src != "" {
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
		chosen := pickPreferredCodexDiscoveredFile(e.db, candidates)
		// Pin the provider source to the chosen path and route it through the
		// provider so processProviderFile parses exactly this copy instead of
		// re-canonicalizing the UUID to the preferred dated layout, which would
		// undo the DB-aware selection above.
		chosen.ProviderProcess = true
		chosen.ProviderSource = e.codexPinnedProviderSource(chosen.Path)
		out = append(out, chosen)
	}
	return out
}

// codexSourceFileForUUID resolves a Codex session UUID to its on-disk
// transcript path under a single sessions root, preferring the live dated
// layout over a flat archived entry. It scopes a Codex provider to that one
// root so the provider's cross-root live-over-archived canonicalization does
// not collapse a per-root duplicate; classifyCodexIndexPath then applies its
// own DB-aware preference across the per-root candidates. Returns "" when the
// provider, source lookup, or path resolution fails.
func (e *Engine) codexSourceFileForUUID(root, uuid string) string {
	factory, ok := e.providerFactories[parser.AgentCodex]
	if !ok || factory == nil {
		return ""
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   []string{root},
		Machine: e.machine,
	})
	source, found, err := provider.FindSource(
		context.Background(),
		parser.FindSourceRequest{RawSessionID: uuid},
	)
	if err != nil || !found {
		return ""
	}
	return providerDiscoveredPath(source)
}

// codexPinnedProviderSource builds a Codex provider SourceRef pinned to the
// exact path, bypassing the provider's live-over-archived canonicalization. It
// is used when the engine's DB-aware or mtime-aware logic has already chosen
// which on-disk copy of a duplicated UUID to parse, so processProviderFile
// parses that copy instead of the provider's preferred dated layout. Returns
// nil when the Codex provider or the path's source shape is unavailable.
func (e *Engine) codexPinnedProviderSource(path string) *parser.SourceRef {
	factory, ok := e.providerFactories[parser.AgentCodex]
	if !ok || factory == nil {
		return nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   e.agentDirs[parser.AgentCodex],
		Machine: e.machine,
	})
	pinner, ok := provider.(interface {
		SourceRefForPath(string) (parser.SourceRef, bool)
	})
	if !ok {
		return nil
	}
	source, ok := pinner.SourceRefForPath(path)
	if !ok {
		return nil
	}
	return &source
}

// codexStoredNameDiffers reports whether the stored session_name for a Codex
// session differs from the given index title. Unknown sessions return false:
// a brand-new session is synced through its own transcript event, not the
// index, so the index path only refreshes renames of already-synced sessions.
func (e *Engine) codexStoredNameDiffers(uuid, indexTitle string) bool {
	return e.codexStoredNameDiffersBySessionID(
		e.idPrefix+"codex:"+uuid, indexTitle, false,
	)
}

func (e *Engine) codexStoredNameDiffersBySessionID(
	sessionID, indexTitle string,
	missingDiffers bool,
) bool {
	stored, err := e.db.GetSessionFull(context.Background(), sessionID)
	if err != nil || stored == nil {
		return missingDiffers
	}
	return e.codexStoredNameDiffersBySession(stored, indexTitle)
}

func (e *Engine) codexStoredNameDiffersBySession(
	stored *db.Session, indexTitle string,
) bool {
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
		sessionID := "codex:" + id
		for _, candidate := range candidates {
			storedPath := database.GetSessionFilePath(applyIDPrefixToID(
				discoveredFileIDPrefix(candidate), sessionID,
			))
			if storedPath == "" {
				continue
			}
			storedPath = filepath.Clean(storedPath)
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

// classifyReasonixPath handles Reasonix session classification as a dedicated
// helper to stay within nilaway limits.
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
						pw.sess.File.Hash,
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
			s.ID, e.usageEventsForWrite(s.ID, pw.usageEvents),
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
	// through here so all agents are covered uniformly. The returned fix
	// counts and the parser malformed-line count are accumulated per
	// agent for the sync summary's anomaly section.
	vs := validateAndSanitize(&s, msgs, nil)
	e.anomalies.recordSanitize(vs)
	e.anomalies.recordMalformedLines(
		s.Agent, pw.sess.File.Path, s.ParserMalformedLines,
	)

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
	path        string
	mtime       int64
	fingerprint string
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
			UsageEvents:     e.usageEventsForWrite(s.ID, pw.usageEvents),
			Signals:         update,
			Findings:        findings,
			DataVersion:     dataVersionForWrite(pw),
			ReplaceMessages: replaceMessages,
		})
		if pw.sess.File.Path != "" {
			sources[s.ID] = batchSourceFile{
				path:        pw.sess.File.Path,
				mtime:       pw.sess.File.Mtime,
				fingerprint: pw.sess.File.Hash,
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
			e.cacheSkip(
				source.path, source.mtime, source.fingerprint,
			)
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
		pw.sess.Agent == parser.AgentCortex ||
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
	// here to keep coverage uniform across write paths. The fix counts
	// feed the sync summary's anomaly section.
	//
	// Deliberately only sanitize fixes are recorded here, not malformed-line
	// counts. A malformed JSONL line appended to an actively-syncing file is
	// skipped by the incremental reader, and incrementalParseFunc carries no
	// malformed-line count, so surfacing it on this path would require
	// threading a new return value through the incremental parser API across
	// every append-only agent. That is intentionally out of scope for this
	// best-effort, only-when-nonzero diagnostic: the value is still parsed and
	// persisted, and the next full sync (the periodic pass, or any
	// parser-version bump that forces a full resync) re-derives the
	// malformed-line count for the file. The incremental path therefore
	// under-reports a brand-new summary signal by at most one full-sync
	// interval; it never loses stored data and is not a regression on any
	// prior behavior (no malformed-line count was surfaced anywhere before
	// this feature). Full malformed-line coverage on the incremental path is a
	// deferred follow-up.
	e.anomalies.recordSanitize(validateAndSanitize(nil, dbMsgs, nil))

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
				e.cacheSkip(
					pw.sess.File.Path,
					pw.sess.File.Mtime,
					pw.sess.File.Hash,
				)
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
		s.ID, e.usageEventsForWrite(s.ID, pw.usageEvents),
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
		agent == parser.AgentIcodemate ||
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
	case parser.AgentIcodemate:
		return "icodemate.db"
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
	case parser.AgentIcodemate:
		return parser.ResolveIcodemateSource(dir)
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
	case parser.AgentIcodemate:
		return parser.IcodemateSourceMtime(path)
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
	_, _, ok := parser.ParseVirtualSourcePathForBase(
		path, openCodeFormatDBName(agent),
	)
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
	return applyIDPrefixToIDs(e.idPrefix, ids)
}

// applyRemoteRewrites prefixes session IDs and rewrites
// file paths for remote sync. No-op when idPrefix is empty.
func (e *Engine) applyRemoteRewrites(
	s *db.Session, msgs []db.Message,
) {
	if e.idPrefix == "" {
		return
	}
	s.ID = applyIDPrefixToID(e.idPrefix, s.ID)
	if s.ParentSessionID != nil && *s.ParentSessionID != "" {
		p := applyIDPrefixToID(e.idPrefix, *s.ParentSessionID)
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
					applyIDPrefixToID(
						e.idPrefix,
						msgs[i].ToolCalls[j].SubagentSessionID,
					)
			}
			for k := range msgs[i].ToolCalls[j].ResultEvents {
				re := &msgs[i].ToolCalls[j].ResultEvents[k]
				if re.SubagentSessionID != "" {
					re.SubagentSessionID =
						applyIDPrefixToID(
							e.idPrefix,
							re.SubagentSessionID,
						)
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
		TranscriptFidelity:   pw.sess.TranscriptFidelity,
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
// event session IDs predate the idPrefix and are ignored. It returns the
// fix counts from the central validation/sanitization pass so write paths
// can surface them in the sync summary; diagnostic callers may discard.
func toDBUsageEvents(
	sessionID string, events []parser.ParsedUsageEvent,
) ([]db.UsageEvent, validationStats) {
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
	// every call site.
	return out, validateAndSanitize(nil, nil, out)
}

// usageEventsForWrite converts usage events for a session about to be
// written and records the central-validation fix counts in the per-run
// anomaly accumulator for the sync summary.
func (e *Engine) usageEventsForWrite(
	sessionID string, events []parser.ParsedUsageEvent,
) []db.UsageEvent {
	out, vs := toDBUsageEvents(sessionID, events)
	e.anomalies.recordSanitize(vs)
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
		if fp := e.db.GetSessionFilePath(sessionID); isS3SourcePath(fp) {
			return fp
		}
		// Remote sessions have no local source file.
		return ""
	}

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok {
		return ""
	}
	rawSessionID := strings.TrimPrefix(rawID, def.IDPrefix)
	if !def.FileBased {
		// Forge, Piebald, and Warp are DB-backed providers that own
		// discovery and source lookup through the provider facade. Their
		// virtual <db>#<sessionID> path is resolved by findProviderSourceFile
		// below. Non-provider, non-file-based agents (e.g. remote imports)
		// have no local source file.
		if !e.isProviderAuthoritative(def.Type) {
			return ""
		}
		storedPath := e.db.GetSessionFilePath(sessionID)
		if f := e.findProviderSourceFile(
			context.Background(), def, sessionID, rawSessionID, storedPath,
		); f != "" {
			return f
		}
		return ""
	}
	if def.Type == parser.AgentKiro {
		for _, dir := range e.agentDirs[parser.AgentKiro] {
			dbPath := kiroSQLiteDBPath(dir)
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
	storedPath := e.db.GetSessionFilePath(sessionID)

	if f := e.findProviderSourceFile(
		context.Background(), def, sessionID, bareID, storedPath,
	); f != "" {
		return f
	}

	// Prefer stored file_path — it's authoritative and handles
	// cases where the session ID doesn't match the filename.
	// Resolve virtual paths (e.g. Visual Studio Copilot's
	// <traceFile>#<conversationID>) for the existence check, but
	// return the stored path so downstream parsing stays scoped to
	// the requested conversation rather than the whole trace file.
	if fp := storedPath; fp != "" {
		// s3:// sources have no local file to stat; the path is itself
		// the authoritative source and processFile fetches it directly.
		if strings.HasPrefix(fp, "s3://") {
			return fp
		}
		if historyPath, idx, ok := parser.ParseAiderVirtualPath(fp); ok {
			// aider's stored "<historyPath>#<idx>" is positional: an
			// inserted or removed earlier run shifts the index onto a
			// different session. Only trust the stored path when run idx
			// still recomputes to the requested raw ID; otherwise fall
			// through. The provider facade, tried first above, owns raw-ID
			// re-resolution.
			if got, ok := parser.AiderRawIDAt(historyPath, idx); ok && got == bareID {
				return fp
			}
		} else if _, err := os.Stat(parser.ResolveSourceFilePath(fp)); err == nil {
			return fp
		}
	}

	return ""
}

// isProviderAuthoritative reports whether the agent's runtime sync is owned by
// the provider facade rather than a legacy engine dispatch path.
func (e *Engine) isProviderAuthoritative(agent parser.AgentType) bool {
	return e.providerMigrationModes[agent] ==
		parser.ProviderMigrationProviderAuthoritative
}

// findProviderSourceFile resolves a single session's source file through the
// provider facade for authoritative concrete providers. It is the sole
// source-lookup path, keeping sessions locatable for diagnostics, export, and
// parse-diff lookups.
func (e *Engine) findProviderSourceFile(
	ctx context.Context,
	def parser.AgentDef,
	sessionID string,
	rawSessionID string,
	storedPath string,
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
		Roots:        e.agentDirs[def.Type],
		Machine:      e.machine,
		PathRewriter: e.pathRewriter,
	})
	source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
		RawSessionID:       rawSessionID,
		FullSessionID:      sessionID,
		StoredFilePath:     storedPath,
		FingerprintKey:     storedPath,
		RequireFreshSource: true,
		PreferStoredSource: true,
	})
	if err != nil {
		log.Printf("%s provider source lookup: %v", def.Type, err)
		return ""
	}
	if !found {
		return ""
	}
	// A fork session ID (Piebald piebald:<chat>-<row>) resolves to its base
	// chat source. Confirm the requested fork is actually produced before
	// treating the chat source as a hit, mirroring the legacy parse-verify.
	if providerSessionIsFork(def, sessionID, rawSessionID) {
		outcome, err := provider.Parse(ctx, parser.ParseRequest{
			Source:  source,
			Machine: e.machine,
		})
		if err != nil || !providerOutcomeContainsSession(outcome, sessionID) {
			return ""
		}
	}
	return providerDiscoveredPath(source)
}

// providerSourceMtime resolves the source-backed mtime for a DB-backed
// provider session through the provider facade. The provider fingerprint
// mirrors the legacy List*SessionMeta last-modified value. Piebald fork IDs
// (piebald:<chat>-<row>) resolve to their base chat source, so a fork is
// confirmed by parsing the chat and checking the requested session ID is
// actually produced before returning the chat mtime.
func (e *Engine) providerDBSourceMtime(
	ctx context.Context,
	def parser.AgentDef,
	sessionID string,
	rawSessionID string,
) int64 {
	factory, ok := e.providerFactories[def.Type]
	if !ok || factory == nil {
		return 0
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
		log.Printf("%s provider source mtime lookup: %v", def.Type, err)
		return 0
	}
	if !found {
		return 0
	}
	fingerprint, err := provider.Fingerprint(ctx, source)
	if err != nil {
		log.Printf("%s provider source mtime fingerprint: %v", def.Type, err)
		return 0
	}
	if fingerprint.MTimeNS == 0 {
		return 0
	}
	// A fork session ID resolves to its base chat source. Confirm the
	// requested fork exists before treating the chat mtime as authoritative.
	if providerSessionIsFork(def, sessionID, rawSessionID) {
		outcome, err := provider.Parse(ctx, parser.ParseRequest{
			Source:  source,
			Machine: e.machine,
		})
		if err != nil || !providerOutcomeContainsSession(outcome, sessionID) {
			return 0
		}
	}
	return fingerprint.MTimeNS
}

// providerSessionIsFork reports whether the session ID addresses a fork child
// whose base differs from the resolved source session. Only Piebald uses the
// "<chat>-<row>" fork-ID shape among the DB-backed providers.
func providerSessionIsFork(
	def parser.AgentDef,
	sessionID string,
	rawSessionID string,
) bool {
	if def.Type != parser.AgentPiebald {
		return false
	}
	chatID, _, _ := strings.Cut(rawSessionID, "-")
	return chatID != rawSessionID
}

// providerOutcomeContainsSession reports whether a parse outcome produced the
// given full session ID.
func providerOutcomeContainsSession(
	outcome parser.ParseOutcome,
	sessionID string,
) bool {
	for _, result := range outcome.Results {
		if result.Result.Session.ID == sessionID {
			return true
		}
	}
	return false
}

// SourceMtime returns the current source-backed mtime for a
// session. Most file-based agents map directly to a single source
// file, but OpenCode storage sessions derive their effective mtime
// from the session JSON plus related message/part files.
func (e *Engine) SourceMtime(sessionID string) int64 {
	host, rawID := parser.StripHostPrefix(sessionID)
	if host != "" {
		if fp := e.db.GetSessionFilePath(sessionID); isS3SourcePath(fp) {
			stat := statS3Object
			if def, ok := parser.AgentByPrefix(sessionID); ok &&
				def.Type == parser.AgentClaude {
				stat = statClaudeS3Session
			} else if ok && def.Type == parser.AgentCodex {
				stat = statCodexS3Session
			}
			if sess, err := e.db.GetSession(
				context.Background(), sessionID,
			); err == nil && sess != nil {
				switch sess.Agent {
				case string(parser.AgentClaude):
					stat = statClaudeS3Session
				case string(parser.AgentCodex):
					stat = statCodexS3Session
				}
			}
			obj, err := stat(fp)
			if err != nil {
				return 0
			}
			return obj.LastModified.UnixNano()
		}
		return 0
	}

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok {
		return 0
	}
	rawSessionID := strings.TrimPrefix(rawID, def.IDPrefix)
	if !def.FileBased {
		// Forge, Piebald, and Warp are DB-backed providers: their
		// per-session source mtime comes from the provider fingerprint
		// (which mirrors the legacy List*SessionMeta last-modified value).
		// Non-provider, non-file-based agents have no local source.
		if e.isProviderAuthoritative(def.Type) {
			return e.providerDBSourceMtime(
				context.Background(), def, sessionID, rawSessionID,
			)
		}
		return 0
	}

	path := e.FindSourceFile(sessionID)
	if path == "" {
		return 0
	}
	if isS3SourcePath(path) {
		stat := statS3Object
		switch def.Type {
		case parser.AgentClaude:
			stat = statClaudeS3Session
		case parser.AgentCodex:
			stat = statCodexS3Session
		}
		obj, err := stat(path)
		if err != nil {
			return 0
		}
		return obj.LastModified.UnixNano()
	}

	if isOpenCodeFormatStorageAgent(def.Type) {
		mtime, err := openCodeFormatSourceMtime(def.Type, path)
		if err != nil {
			return 0
		}
		return mtime
	}
	if def.Type == parser.AgentKiro {
		if _, _, ok := parseKiroSQLiteVirtualPath(path); ok {
			mtime, err := parser.KiroSQLiteSourceMtime(path)
			if err != nil {
				return 0
			}
			return mtime
		}
	}
	if def.Type == parser.AgentZed {
		if _, _, ok := parser.ParseVirtualSourcePathForBase(path, "threads.db"); ok {
			mtime, err := parser.ZedSQLiteSourceMtime(path)
			if err != nil {
				return 0
			}
			return mtime
		}
	}
	if def.Type == parser.AgentShelley {
		if _, _, ok := parser.ParseVirtualSourcePathForBase(path, shelleyDBFile); ok {
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
	e.resetS3CodexIndexCache()

	host, _ := parser.StripHostPrefix(sessionID)
	if host != "" && !isS3SourcePath(e.db.GetSessionFilePath(sessionID)) {
		return fmt.Errorf(
			"cannot sync remote session %s locally", sessionID,
		)
	}

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok {
		return fmt.Errorf("unknown agent for session %s", sessionID)
	}
	if !def.FileBased {
		// Forge, Piebald, and Warp are DB-backed providers: re-sync routes
		// through FindSourceFile (resolving the virtual <db>#<sessionID>
		// path) plus the provider-aware processFile path below, mirroring
		// the file-based agents. Other non-file-based agents use the
		// OpenCode-format storage path.
		if !e.isProviderAuthoritative(def.Type) {
			return fmt.Errorf(
				"cannot resync non-file-based session %s for agent %s",
				sessionID, def.Type,
			)
		}
	}

	path := e.FindSourceFile(sessionID)
	if path == "" {
		return fmt.Errorf(
			"source file not found for %s", sessionID,
		)
	}
	// OpenCode-format agents (OpenCode, Kilo, MiMoCode) are
	// provider-authoritative: their SQLite virtual paths and storage
	// sessions resync through the generic processFile path below, which
	// routes to the provider facade.

	agent := def.Type

	// Clear skip cache so explicit re-sync always processes
	// the file, even if it was cached as non-interactive
	// during a bulk SyncAll.
	file := parser.DiscoveredFile{
		Path:       path,
		Agent:      agent,
		ForceParse: true,
	}
	e.hydrateS3DiscoveredFile(ctx, sessionID, &file)
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
	}

	res := e.processFile(ctx, file)
	if res.err != nil {
		if res.cacheSkip && res.mtime != 0 && !res.noCacheSkip {
			e.cacheSkip(res.skipCacheKey(path), res.mtime, res.sourceFingerprint)
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

// filterShadowedLegacyKiroFiles drops discovered legacy Kiro JSONL sources
// whose logical session ID already exists in a current-store SQLite database
// under any configured Kiro root. The Kiro provider performs the same
// shadowing during its own Discover, but only across the roots it is
// configured with; a scoped sync (e.g. SyncRootsSince over a single root)
// configures the provider with that scope only, so the engine reapplies the
// cross-root shadow here using every configured Kiro root. This keeps a legacy
// file from being imported when its session lives in the SQLite store of a
// different, out-of-scope root.
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
			filepath.Base(file.Path) == kiroSQLiteDBName {
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
			filepath.Base(file.Path) != kiroSQLiteDBName {
			return true
		}
	}
	return false
}

// kiroSQLiteDBName is the filename of the current-store Kiro SQLite DB.
const kiroSQLiteDBName = "data.sqlite3"

// kiroSQLiteDBPath returns the current-store Kiro SQLite DB path when the
// configured root contains one, or "" otherwise.
func kiroSQLiteDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, kiroSQLiteDBName)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return ""
	}
	return path
}

// parseKiroSQLiteVirtualPath splits a virtual Kiro SQLite source path back
// into its database path and raw session ID using the provider-neutral
// virtual-source-path resolver.
func parseKiroSQLiteVirtualPath(path string) (string, string, bool) {
	return parser.ParseVirtualSourcePathForBase(path, kiroSQLiteDBName)
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
		filePath := tc.FilePath
		if filePath == "" {
			filePath = parser.ResolveFilePathFromJSON(tc.InputJSON)
		}
		calls[i] = db.ToolCall{
			SessionID:         sessionID,
			ToolName:          tc.ToolName,
			Category:          tc.Category,
			ToolUseID:         tc.ToolUseID,
			InputJSON:         tc.InputJSON,
			FilePath:          filePath,
			CallIndex:         i,
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
