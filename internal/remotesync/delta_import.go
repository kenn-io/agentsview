package remotesync

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

type JournalOutcome string

const (
	JournalRetired                 JournalOutcome = "retired"
	JournalAbortedBeforeProcessing JournalOutcome = "retained(aborted-before-processing)"
	JournalProcessingFailures      JournalOutcome = "retained(processing-failures)"
	JournalCachePersistFailed      JournalOutcome = "retained(cache-persist-failed)"
	JournalRetirementFailed        JournalOutcome = "retained(retirement-failed)"
	JournalCancelled               JournalOutcome = "retained(cancelled)"
	JournalPendingSwap             JournalOutcome = "retained(pending-swap)"
)

func (outcome JournalOutcome) Valid() bool {
	switch outcome {
	case JournalRetired, JournalAbortedBeforeProcessing,
		JournalProcessingFailures, JournalCachePersistFailed,
		JournalRetirementFailed, JournalCancelled, JournalPendingSwap:
		return true
	default:
		return false
	}
}

type DeltaImportRequest struct {
	Journal                  MirrorChangeJournal
	FullReason               FullImportReason
	ForceParse               bool
	ForceFullParseAfterCache bool
	ResetAttemptCache        bool
	AttemptCacheDataVersion  int
}

type CachePruneStats struct {
	ExactScopes    int
	ProviderScopes int
	HostWideScope  bool
	Exact          int
	Provider       int
	HostWide       int
}

func (s CachePruneStats) Total() int { return s.Exact + s.Provider + s.HostWide }

type PreparedDeltaImport struct {
	DisarmedJournal MirrorChangeJournal
	Stats           SyncStats

	database            *db.DB
	layout              importLayout
	config              syncpkg.EngineConfig
	plan                syncpkg.ChangedPathPlan
	forceScope          syncpkg.ChangedPathPruneScope
	cache               map[string]int64
	remoteCache         map[string]int64
	full                bool
	forceParse          bool
	forceFullParse      bool
	requiredDataVersion int
	progress            syncpkg.ProgressFunc
	save                func(*db.DB, *syncpkg.Engine, remotePathMap) error
	apply               func(string, []string, map[string]int64) error
}

func (im Importer) PreparePending(
	ctx context.Context,
	request DeltaImportRequest,
) (*PreparedDeltaImport, error) {
	// FullReason can be restored from a retained journal. It preserves the
	// pending full-import scope and its observable cause, but it must not turn
	// an ordinary replay into another hard force-parse attempt. Only the current
	// invocation's explicit Full request bypasses durable failure skips. The
	// journal's separate full-parse intent still bypasses freshness and
	// incremental append for sources without a post-invalidation skip entry.
	forceParse := request.ForceParse
	forceFullParse := request.Journal.ForceFullParseAll ||
		request.ForceFullParseAfterCache || journalForcesFullParse(request.Journal)
	stats := SyncStats{
		FullReason:     request.FullReason,
		JournalOutcome: JournalAbortedBeforeProcessing,
		PendingPaths:   len(request.Journal.Entries),
	}
	for _, entry := range request.Journal.Entries {
		if entry.InvalidateCache {
			stats.ArmedPaths++
		}
	}
	if request.Journal.FullImportReason != "" {
		stats.FullReason = request.Journal.FullImportReason
	}
	if stats.FullReason != "" && !stats.FullReason.Valid() {
		return nil, fmt.Errorf("invalid full import reason %q", stats.FullReason)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateMirrorChangeJournal(request.Journal); err != nil {
		return nil, err
	}
	if err := validateTargetSetPaths(im.Targets); err != nil {
		return nil, err
	}
	layout, config, err := newImportInputs(
		im.Host, im.BlockedResultCategories, im.Targets, im.Root,
	)
	if err != nil {
		return nil, err
	}

	physicalPaths := make([]string, 0, len(request.Journal.Entries))
	for _, entry := range request.Journal.Entries {
		path, pathErr := safeLocalArchivePath(
			im.Root, filepath.FromSlash(entry.Path),
		)
		if pathErr != nil {
			return nil, fmt.Errorf("resolve pending mirror path: %w", pathErr)
		}
		physicalPaths = append(physicalPaths, path)
	}
	planningStart := time.Now()
	planningEngine := syncpkg.NewEngine(im.DB, config)
	plan, err := planningEngine.PlanChangedPathsContext(ctx, physicalPaths)
	planningEngine.Close()
	stats.PlanningDuration = time.Since(planningStart)
	if err != nil {
		return nil, err
	}
	stats.ExactSources = len(plan.Files)
	stats.FallbackProviders = len(plan.FallbackProviders)
	forceScope := plan.PruneScope(forceFullParseJournalPhysicalPaths(
		layout, request.Journal,
	))

	full := request.Journal.FullImport || request.FullReason != "" || im.Full
	ensureVisualStudioCopilotRemoteSkipMigration(im.DB, im.Host)
	remoteCache, err := loadPlannedRemoteSkipCache(
		ctx, im.DB, im.Host, layout, plan, request.Journal, full,
	)
	if err != nil {
		return nil, fmt.Errorf("load skip cache: %w", err)
	}
	pruningStart := time.Now()
	pruned, pruneStats := pruneRemoteSkipCache(
		remoteCache, layout, plan, request.Journal,
	)
	if request.ResetAttemptCache {
		pruned = make(map[string]int64)
		pruneStats = CachePruneStats{
			HostWideScope: true,
			HostWide:      len(remoteCache),
		}
	}
	stats.PruningDuration = time.Since(pruningStart)
	stats.PrunedExact = pruneStats.Exact
	stats.PrunedProvider = pruneStats.Provider
	stats.PrunedHostWide = pruneStats.HostWide
	stats.PruneExactScopes = pruneStats.ExactScopes
	stats.PruneProviderScopes = pruneStats.ProviderScopes
	stats.PruneHostWideScope = pruneStats.HostWideScope
	replace := im.replaceRemoteSkippedFiles
	if replace == nil {
		replace = im.DB.ReplaceRemoteSkippedFiles
	}
	apply := im.applyRemoteSkippedChanges
	if apply == nil {
		apply = im.DB.ApplyRemoteSkippedFileChanges
	}
	cachePersistStart := time.Now()
	deleted := removedRemoteSkipCacheKeys(remoteCache, pruned)
	var persistErr error
	if len(deleted) > 0 {
		if request.Journal.InvalidateAll || request.ResetAttemptCache {
			persistErr = replace(im.Host, pruned)
		} else {
			persistErr = apply(im.Host, deleted, nil)
		}
	}
	if persistErr != nil {
		return nil, fmt.Errorf("persist pruned remote skip cache: %w", persistErr)
	}
	stats.CachePersistDuration = time.Since(cachePersistStart)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	preparedCache := translateRemoteCacheToTemp(
		pruned, layout.paths.remoteDirs, layout.paths.localDirs,
	)
	if forceParse {
		preparedCache = nil
	}
	disarmedJournal := disarmMirrorChanges(request.Journal)
	if request.AttemptCacheDataVersion > 0 {
		disarmedJournal.DataRebuildCacheVersion = request.AttemptCacheDataVersion
	}
	return &PreparedDeltaImport{
		DisarmedJournal: disarmedJournal,
		Stats:           stats,
		database:        im.DB, layout: layout, config: config, plan: plan,
		cache:               preparedCache,
		remoteCache:         maps.Clone(pruned),
		forceScope:          forceScope,
		full:                full,
		forceParse:          forceParse,
		forceFullParse:      forceFullParse,
		requiredDataVersion: request.Journal.RequiredDataVersion,
		progress:            im.Progress, save: im.saveSkipCache, apply: apply,
	}, nil
}

func journalForcesFullParse(journal MirrorChangeJournal) bool {
	for _, entry := range journal.Entries {
		if entry.ForceFullParse {
			return true
		}
	}
	return false
}

func shouldPersistImportDataVersion(stats syncpkg.SyncStats) bool {
	return stats.ProcessingComplete()
}

func deltaImportOutcome(stats syncpkg.SyncStats) JournalOutcome {
	if shouldPersistImportDataVersion(stats) {
		return JournalRetired
	}
	return JournalProcessingFailures
}

func (pending *PreparedDeltaImport) Execute(
	ctx context.Context,
) (SyncStats, error) {
	if pending == nil {
		return SyncStats{}, fmt.Errorf("execute nil pending delta import")
	}
	stats := pending.Stats
	engine := syncpkg.NewEngine(pending.database, pending.config)
	defer engine.Close()
	engine.InjectSkipCache(pending.cache)
	processingStart := time.Now()
	var engineStats syncpkg.SyncStats
	var changedResult syncpkg.ChangedPathSyncResult
	var processErr error
	if pending.full {
		progress := hostProgress(pending.layout.paths.host, pending.progress)
		if pending.forceParse {
			engineStats = engine.SyncAllForceParse(ctx, progress)
		} else if pending.forceFullParse {
			engineStats = engine.SyncAllForceParseAfterCache(ctx, progress)
		} else {
			engineStats = engine.SyncAll(ctx, progress)
		}
	} else {
		changedResult, processErr = engine.SyncChangedPathPlanWithOptionsContext(
			ctx, pending.plan, syncpkg.ChangedPathSyncOptions{
				ForceFullParse: pending.forceScope,
			}, hostProgress(pending.layout.paths.host, pending.progress),
		)
		engineStats = changedResult.Stats
		stats.FilesDiscovered = changedResult.FilesDiscovered
		stats.FilesProcessed = changedResult.FilesProcessed
		stats.FallbackSources = changedResult.FallbackSources
		stats.ErrorSuppressed = countDisarmedCachedSuppressions(
			pending.plan, pending.DisarmedJournal, changedResult,
		)
	}
	stats.ProcessingDuration = time.Since(processingStart)
	stats.SessionsSynced = engineStats.Synced
	stats.SessionsTotal = engineStats.TotalSessions
	stats.Skipped = engineStats.Skipped
	stats.Failed = engineStats.Failed
	stats.Deferred = engineStats.Deferred
	stats.incomplete = !engineStats.ProcessingComplete()

	cachePersistStart := time.Now()
	if err := pending.persistSkipCache(pending.database, engine); err != nil {
		stats.CachePersistDuration += time.Since(cachePersistStart)
		stats.JournalOutcome = JournalCachePersistFailed
		pending.Stats = stats
		return stats, err
	}
	stats.CachePersistDuration += time.Since(cachePersistStart)
	switch {
	case ctx.Err() != nil:
		stats.JournalOutcome = JournalCancelled
		processErr = ctx.Err()
	case processErr != nil:
		stats.JournalOutcome = JournalProcessingFailures
	case deltaImportOutcome(engineStats) != JournalRetired:
		stats.JournalOutcome = deltaImportOutcome(engineStats)
		if processErr == nil {
			processErr = fmt.Errorf(
				"remote import processing incomplete: aborted=%t failed=%d deferred=%d",
				engineStats.Aborted, engineStats.Failed, engineStats.Deferred,
			)
		}
	default:
		if err := ctx.Err(); err != nil {
			stats.JournalOutcome = JournalCancelled
			processErr = err
			break
		}
		versionPersistStart := time.Now()
		if err := pending.persistImportDataVersion(
			context.WithoutCancel(ctx), pending.database,
		); err != nil {
			stats.CachePersistDuration += time.Since(versionPersistStart)
			stats.JournalOutcome = JournalCachePersistFailed
			pending.Stats = stats
			return stats, err
		}
		stats.CachePersistDuration += time.Since(versionPersistStart)
		if err := ctx.Err(); err != nil {
			stats.JournalOutcome = JournalCancelled
			processErr = err
			break
		}
		stats.JournalOutcome = JournalRetired
	}
	pending.Stats = stats
	return stats, processErr
}

func (pending *PreparedDeltaImport) persistImportDataVersion(
	ctx context.Context, database *db.DB,
) error {
	if pending.requiredDataVersion == 0 {
		return nil
	}
	if err := database.SetRemoteImportDataVersion(
		ctx, pending.layout.paths.host, pending.requiredDataVersion,
	); err != nil {
		return fmt.Errorf("persist remote import data version: %w", err)
	}
	return nil
}

func (pending *PreparedDeltaImport) persistSkipCache(
	database *db.DB,
	engine *syncpkg.Engine,
) error {
	if pending.save != nil {
		return pending.save(database, engine, pending.layout.paths)
	}
	if pending.full {
		return saveEngineSkipCache(database, engine, pending.layout.paths)
	}
	current := remoteEngineSkipCache(engine, pending.layout.paths)
	deletes, upserts := diffRemoteSkipCache(pending.remoteCache, current)
	if database != pending.database {
		merged, err := pending.database.LoadRemoteSkippedFiles(
			pending.layout.paths.host,
		)
		if err != nil {
			return fmt.Errorf("load active skip cache for rebuild: %w", err)
		}
		for _, key := range deletes {
			delete(merged, key)
		}
		maps.Copy(merged, upserts)
		if err := database.ReplaceRemoteSkippedFiles(
			pending.layout.paths.host, merged,
		); err != nil {
			return fmt.Errorf("save rebuilt skip cache: %w", err)
		}
		return nil
	}
	if err := pending.apply(
		pending.layout.paths.host, deletes, upserts,
	); err != nil {
		return fmt.Errorf("save scoped skip cache: %w", err)
	}
	return nil
}

func (pending *PreparedDeltaImport) persistRetrySafeSkipCache(
	database *db.DB,
	engine *syncpkg.Engine,
) error {
	remoteCache := remoteRetrySafeEngineSkipCache(engine, pending.layout.paths)
	if err := database.ReplaceRemoteSkippedFiles(
		pending.layout.paths.host, remoteCache,
	); err != nil {
		return fmt.Errorf("save retry-safe skip cache: %w", err)
	}
	return nil
}

type plannedRemoteSkipCacheScope struct {
	exact    map[string]map[parser.AgentType]struct{}
	fallback map[parser.AgentType][]string
}

func loadPlannedRemoteSkipCache(
	ctx context.Context,
	database *db.DB,
	host string,
	layout importLayout,
	plan syncpkg.ChangedPathPlan,
	journal MirrorChangeJournal,
	full bool,
) (map[string]int64, error) {
	if full {
		return database.LoadRemoteSkippedFiles(host)
	}
	scope := plannedRemoteSkipScope(layout, plan, journal)
	exactPaths := make([]string, 0, len(scope.exact))
	for path := range scope.exact {
		exactPaths = append(exactPaths, path)
	}
	var rootPaths []string
	for _, roots := range scope.fallback {
		rootPaths = append(rootPaths, roots...)
	}
	candidates, err := database.LoadRemoteSkippedFilesForScopes(
		ctx, host, exactPaths, rootPaths,
	)
	if err != nil {
		return nil, err
	}
	cache := make(map[string]int64, len(candidates))
	for key, mtime := range candidates {
		path, agent := remoteCacheFamily(key)
		if agents := scope.exact[path]; agents != nil {
			if agent == "" {
				cache[key] = mtime
				continue
			}
			if _, ok := agents[agent]; ok {
				cache[key] = mtime
				continue
			}
		}
		for fallbackAgent, roots := range scope.fallback {
			if agent != "" && agent != fallbackAgent {
				continue
			}
			if remotePathWithinAnyRoot(path, roots) {
				cache[key] = mtime
				break
			}
		}
	}
	return cache, nil
}

func plannedRemoteSkipScope(
	layout importLayout,
	plan syncpkg.ChangedPathPlan,
	journal MirrorChangeJournal,
) plannedRemoteSkipCacheScope {
	allPending := make(map[string]struct{}, len(journal.Entries))
	for _, entry := range journal.Entries {
		physical, err := safeLocalArchivePath(
			layout.paths.root, filepath.FromSlash(entry.Path),
		)
		if err == nil {
			allPending[physical] = struct{}{}
		}
	}
	attributed := plan.PruneScope(allPending)
	files := append(slices.Clone(plan.Files), attributed.Files...)
	providers := append(slices.Clone(plan.FallbackProviders), attributed.FallbackProviders...)
	scope := plannedRemoteSkipCacheScope{
		exact:    make(map[string]map[parser.AgentType]struct{}),
		fallback: make(map[parser.AgentType][]string),
	}
	for _, file := range files {
		remotePath, ok := tempPathToRemotePath(
			file.Path, layout.paths.remoteDirs, layout.paths.localDirs,
		)
		if !ok {
			continue
		}
		agents := scope.exact[remotePath]
		if agents == nil {
			agents = make(map[parser.AgentType]struct{})
			scope.exact[remotePath] = agents
		}
		agents[file.Agent] = struct{}{}
	}
	for _, agent := range providers {
		for _, localRoot := range layout.engineDirs[agent] {
			remoteRoot, ok := tempPathToRemotePath(
				localRoot, layout.paths.remoteDirs, layout.paths.localDirs,
			)
			if !ok || slices.Contains(scope.fallback[agent], remoteRoot) {
				continue
			}
			scope.fallback[agent] = append(scope.fallback[agent], remoteRoot)
		}
	}
	return scope
}

func removedRemoteSkipCacheKeys(
	before map[string]int64,
	after map[string]int64,
) []string {
	removed := make([]string, 0, len(before))
	for key := range before {
		if _, ok := after[key]; !ok {
			removed = append(removed, key)
		}
	}
	slices.Sort(removed)
	return removed
}

func diffRemoteSkipCache(
	before map[string]int64,
	after map[string]int64,
) ([]string, map[string]int64) {
	deletes := removedRemoteSkipCacheKeys(before, after)
	upserts := make(map[string]int64)
	for key, mtime := range after {
		if previous, ok := before[key]; !ok || previous != mtime {
			upserts[key] = mtime
		}
	}
	return deletes, upserts
}

func pruneRemoteSkipCache(
	cache map[string]int64,
	layout importLayout,
	plan syncpkg.ChangedPathPlan,
	journal MirrorChangeJournal,
) (map[string]int64, CachePruneStats) {
	stats := CachePruneStats{}
	if journal.InvalidateAll {
		stats.HostWideScope = true
		stats.HostWide = len(cache)
		return map[string]int64{}, stats
	}
	pruned := make(map[string]int64, len(cache))
	maps.Copy(pruned, cache)
	armed := cacheInvalidationJournalPhysicalPaths(layout, journal)
	scope := plan.PruneScope(armed)
	stats.ExactScopes = len(scope.Files)
	stats.ProviderScopes = len(scope.FallbackProviders)
	exact := make(map[string]map[parser.AgentType]struct{}, len(scope.Files))
	for _, file := range scope.Files {
		remotePath, ok := tempPathToRemotePath(
			file.Path, layout.paths.remoteDirs, layout.paths.localDirs,
		)
		if !ok {
			continue
		}
		agents := exact[remotePath]
		if agents == nil {
			agents = make(map[parser.AgentType]struct{})
			exact[remotePath] = agents
		}
		agents[file.Agent] = struct{}{}
	}
	fallback := make(map[parser.AgentType][]string, len(scope.FallbackProviders))
	for _, agent := range scope.FallbackProviders {
		for _, localRoot := range layout.engineDirs[agent] {
			if remoteRoot, ok := tempPathToRemotePath(
				localRoot, layout.paths.remoteDirs, layout.paths.localDirs,
			); ok {
				fallback[agent] = append(fallback[agent], remoteRoot)
			}
		}
	}
	for key := range pruned {
		path, agent := remoteCacheFamily(key)
		if agents := exact[path]; agents != nil {
			_, sameAgent := agents[agent]
			if agent == "" || sameAgent {
				delete(pruned, key)
				stats.Exact++
				continue
			}
		}
		for fallbackAgent, roots := range fallback {
			if agent != "" && agent != fallbackAgent {
				continue
			}
			if remotePathWithinAnyRoot(path, roots) {
				delete(pruned, key)
				stats.Provider++
				break
			}
		}
	}
	return pruned, stats
}

func cacheInvalidationJournalPhysicalPaths(
	layout importLayout,
	journal MirrorChangeJournal,
) map[string]struct{} {
	return journalPhysicalPaths(layout, journal, func(entry MirrorChangeEntry) bool {
		return entry.InvalidateCache
	})
}

func forceFullParseJournalPhysicalPaths(
	layout importLayout,
	journal MirrorChangeJournal,
) map[string]struct{} {
	return journalPhysicalPaths(layout, journal, func(entry MirrorChangeEntry) bool {
		return entry.ForceFullParse
	})
}

func journalPhysicalPaths(
	layout importLayout,
	journal MirrorChangeJournal,
	include func(MirrorChangeEntry) bool,
) map[string]struct{} {
	armed := make(map[string]struct{})
	for _, entry := range journal.Entries {
		if !include(entry) {
			continue
		}
		physical, err := safeLocalArchivePath(
			layout.paths.root, filepath.FromSlash(entry.Path),
		)
		if err == nil {
			armed[physical] = struct{}{}
		}
	}
	return armed
}

func remoteCacheFamily(key string) (string, parser.AgentType) {
	base, _, _ := strings.Cut(key, "?source_hash=")
	path, suffix := syncpkg.SplitProviderSkipCachePath(base)
	if suffix == "" {
		return path, ""
	}
	agent := strings.TrimPrefix(suffix, "?agent=")
	if before, _, ok := strings.Cut(agent, "?"); ok {
		agent = before
	}
	return path, parser.AgentType(agent)
}

func remotePathWithinAnyRoot(path string, roots []string) bool {
	return slices.ContainsFunc(roots, func(root string) bool {
		_, ok := remoteArchiveRel(root, path)
		return ok
	})
}

func countDisarmedCachedSuppressions(
	plan syncpkg.ChangedPathPlan,
	journal MirrorChangeJournal,
	result syncpkg.ChangedPathSyncResult,
) int {
	if journal.InvalidateAll {
		return 0
	}
	for _, entry := range journal.Entries {
		if entry.InvalidateCache {
			return 0
		}
	}
	armed := make(map[string]struct{})
	return plan.CountCachedSuppressedInputs(
		armed, result.CachedSourceKeys, result.CachedFallbackProviders,
	)
}
