package sync

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"maps"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pathutil"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/signals"
	"go.kenn.io/agentsview/internal/timeutil"
)

const (
	batchSize               = 100
	maxWorkers              = 8
	projectIdentityCacheTTL = time.Minute
)

type syncWriteMode int

const (
	syncWriteDefault syncWriteMode = iota
	syncWriteBulk
)

var errSessionPreserved = errors.New("session preserved")

type reconciliationMetricsContextKey struct{}
type reconciliationBaselineContextKey struct{}
type deferGlobalLinkContextKey struct{}
type deferPassEpilogueContextKey struct{}

// passEpilogueDeferred reports whether a grouped caller owns the pass
// epilogue — global subagent linking and skip-cache persistence — so a
// scoped reconciliation must not repeat that archive-sized work itself.
func passEpilogueDeferred(ctx context.Context) bool {
	deferred, _ := ctx.Value(deferPassEpilogueContextKey{}).(bool)
	return deferred
}

// passEpilogueEligibility records, at the per-pass gate sites, whether a
// pass would have run global subagent linking and skip-cache persistence.
// Grouped callers consume it instead of inferring eligibility from the
// pass's final error: that error also reflects later tombstoning and spool
// cleanup failures, which never suppressed the per-pass epilogue and must
// not suppress the shared one.
type passEpilogueEligibility struct {
	link    bool
	persist bool
}

type reconciliationBaselineTracker struct {
	sources                 map[machineSessionSource]struct{}
	rejectedSources         map[machineSessionSource]struct{}
	exactOwnerships         map[db.SessionSourceOwnership]struct{}
	rejectedExactOwnerships map[db.SessionSourceOwnership]struct{}
	cacheWrites             map[string]skipCacheWrite
	nonAuthoritativeScopes  map[reconciliationSourceScope]struct{}
}

type machineSessionSource struct {
	// Machine is an exact stored ownership key. Empty remains valid for
	// sessions written before machine attribution was introduced.
	Machine string
	Source  db.SessionSourcePath
}

func matchingBaselineOwnerships(
	sources []machineSessionSource,
	ownerships map[db.SessionSourceOwnership]struct{},
) []db.SessionSourceOwnership {
	if len(sources) == 0 || len(ownerships) == 0 {
		return nil
	}
	sourceSet := make(map[machineSessionSource]struct{}, len(sources))
	for _, source := range sources {
		sourceSet[source] = struct{}{}
	}
	matched := make([]db.SessionSourceOwnership, 0, len(ownerships))
	for ownership := range ownerships {
		source := machineSessionSource{
			Machine: ownership.Machine,
			Source: db.SessionSourcePath{
				Agent: ownership.Agent, FilePath: ownership.FilePath,
			},
		}
		if _, ok := sourceSet[source]; ok {
			matched = append(matched, ownership)
		}
	}
	return matched
}

func consumeBaselineOwnerships(
	ownerships map[db.SessionSourceOwnership]struct{},
	consumed []db.SessionSourceOwnership,
) {
	for _, ownership := range consumed {
		delete(ownerships, ownership)
	}
}

func newReconciliationBaselineTracker() *reconciliationBaselineTracker {
	return &reconciliationBaselineTracker{
		sources:                make(map[machineSessionSource]struct{}, reconciliationPageSize),
		cacheWrites:            make(map[string]skipCacheWrite),
		nonAuthoritativeScopes: make(map[reconciliationSourceScope]struct{}),
	}
}

func (tracker *reconciliationBaselineTracker) stageCacheWrites(
	writes []skipCacheWrite,
) {
	for _, write := range writes {
		tracker.stageCacheWrite(write)
	}
}

func (tracker *reconciliationBaselineTracker) stageCacheWrite(
	write skipCacheWrite,
) {
	if write.key == "" {
		return
	}
	tracker.cacheWrites[write.key] = write
}

func (tracker *reconciliationBaselineTracker) listCacheWrites() []skipCacheWrite {
	writes := make([]skipCacheWrite, 0, len(tracker.cacheWrites))
	for _, write := range tracker.cacheWrites {
		writes = append(writes, write)
	}
	return writes
}

func reconciliationBaselineTrackerFor(
	ctx context.Context,
) *reconciliationBaselineTracker {
	tracker, _ := ctx.Value(reconciliationBaselineContextKey{}).(*reconciliationBaselineTracker)
	return tracker
}

func (tracker *reconciliationBaselineTracker) add(
	source machineSessionSource,
) {
	if source.Source.Agent == "" || source.Source.FilePath == "" {
		return
	}
	if _, rejected := tracker.rejectedSources[source]; rejected {
		return
	}
	tracker.sources[source] = struct{}{}
}

func (tracker *reconciliationBaselineTracker) list() []machineSessionSource {
	sources := make([]machineSessionSource, 0, len(tracker.sources))
	for source := range tracker.sources {
		sources = append(sources, source)
	}
	return sources
}

func (tracker *reconciliationBaselineTracker) reject(
	source machineSessionSource,
) {
	if source.Source.Agent == "" || source.Source.FilePath == "" {
		return
	}
	if tracker.rejectedSources == nil {
		tracker.rejectedSources = make(map[machineSessionSource]struct{})
	}
	delete(tracker.sources, source)
	tracker.rejectedSources[source] = struct{}{}
}

func (tracker *reconciliationBaselineTracker) listRejected() []machineSessionSource {
	sources := make([]machineSessionSource, 0, len(tracker.rejectedSources))
	for source := range tracker.rejectedSources {
		sources = append(sources, source)
	}
	return sources
}

func (tracker *reconciliationBaselineTracker) addNonAuthoritativeScope(
	agent parser.AgentType, path string,
) {
	path = validatedProviderSourceStatPath(path)
	if agent == "" || path == "" {
		return
	}
	tracker.nonAuthoritativeScopes[reconciliationSourceScope{
		Provider: agent,
		Path:     canonicalReconciliationSourceIdentity(path),
	}] = struct{}{}
}

func (tracker *reconciliationBaselineTracker) listNonAuthoritativeScopes() []reconciliationSourceScope {
	scopes := make([]reconciliationSourceScope, 0, len(tracker.nonAuthoritativeScopes))
	for scope := range tracker.nonAuthoritativeScopes {
		scopes = append(scopes, scope)
	}
	return scopes
}

func (tracker *reconciliationBaselineTracker) addExactOwnership(
	ownership db.SessionSourceOwnership,
) {
	if ownership.ID == "" || ownership.Agent == "" || ownership.FilePath == "" {
		return
	}
	if tracker.exactOwnerships == nil {
		tracker.exactOwnerships = make(map[db.SessionSourceOwnership]struct{})
	}
	tracker.exactOwnerships[ownership] = struct{}{}
}

func (tracker *reconciliationBaselineTracker) listExactOwnerships() []db.SessionSourceOwnership {
	ownerships := make(
		[]db.SessionSourceOwnership, 0, len(tracker.exactOwnerships),
	)
	for ownership := range tracker.exactOwnerships {
		ownerships = append(ownerships, ownership)
	}
	return ownerships
}

func (tracker *reconciliationBaselineTracker) rejectExactOwnership(
	ownership db.SessionSourceOwnership,
) {
	if ownership.ID == "" || ownership.Agent == "" || ownership.FilePath == "" {
		return
	}
	if tracker.rejectedExactOwnerships == nil {
		tracker.rejectedExactOwnerships = make(
			map[db.SessionSourceOwnership]struct{},
		)
	}
	tracker.rejectedExactOwnerships[ownership] = struct{}{}
}

func (tracker *reconciliationBaselineTracker) listRejectedExactOwnerships() []db.SessionSourceOwnership {
	ownerships := make(
		[]db.SessionSourceOwnership, 0, len(tracker.rejectedExactOwnerships),
	)
	for ownership := range tracker.rejectedExactOwnerships {
		ownerships = append(ownerships, ownership)
	}
	return ownerships
}

func (e *Engine) revokeRejectedReconciliationBaselines(
	ctx context.Context,
	sources []machineSessionSource,
	ownerships []db.SessionSourceOwnership,
) error {
	cleanupCtx := context.WithoutCancel(ctx)
	rejectedSources := make([]db.SessionSourcePath, 0, len(sources))
	for _, source := range sources {
		rejectedSources = append(rejectedSources, source.Source)
	}
	var sourceErr error
	if err := e.db.RemoveSessionSourceBaselines(
		cleanupCtx, rejectedSources,
	); err != nil {
		sourceErr = fmt.Errorf("revoke rejected source baselines: %w", err)
	}
	var ownershipErr error
	if err := e.db.RemoveSessionSourceOwnershipBaselines(
		cleanupCtx, ownerships,
	); err != nil {
		ownershipErr = fmt.Errorf(
			"revoke rejected source baseline exceptions: %w", err,
		)
	}
	return errors.Join(sourceErr, ownershipErr)
}

type reconciliationRuntimeMetrics struct {
	mu                       gosync.Mutex
	maxProvider              int
	maxRehydrated            int
	workerResults            int
	maxWorkerResults         int
	maxPendingWrites         int
	globalLinkPasses         int
	providerRetainedBytes    int64
	maxProviderRetainedBytes int64
	sharedContainerScans     int
	openCodeSQLiteParses     int
	codexReplacementBuilds   int
}

func reconciliationRuntimeMetricsFor(ctx context.Context) *reconciliationRuntimeMetrics {
	metrics, _ := ctx.Value(reconciliationMetricsContextKey{}).(*reconciliationRuntimeMetrics)
	return metrics
}

func (metrics *reconciliationRuntimeMetrics) providerBuffered(buffered int) {
	metrics.mu.Lock()
	metrics.maxProvider = max(metrics.maxProvider, buffered)
	metrics.mu.Unlock()
}

func (metrics *reconciliationRuntimeMetrics) rehydrated(count int) {
	metrics.mu.Lock()
	metrics.maxRehydrated = max(metrics.maxRehydrated, count)
	metrics.mu.Unlock()
}

func (metrics *reconciliationRuntimeMetrics) workerQueued(delta int) {
	metrics.mu.Lock()
	metrics.workerResults += delta
	metrics.maxWorkerResults = max(metrics.maxWorkerResults, metrics.workerResults)
	metrics.mu.Unlock()
}

func (metrics *reconciliationRuntimeMetrics) pendingWrites(count int) {
	metrics.mu.Lock()
	metrics.maxPendingWrites = max(metrics.maxPendingWrites, count)
	metrics.mu.Unlock()
}

func (metrics *reconciliationRuntimeMetrics) globalLinkPass() {
	metrics.mu.Lock()
	metrics.globalLinkPasses++
	metrics.mu.Unlock()
}

func (metrics *reconciliationRuntimeMetrics) providerRetained(delta int64) {
	metrics.mu.Lock()
	metrics.providerRetainedBytes += delta
	metrics.maxProviderRetainedBytes = max(
		metrics.maxProviderRetainedBytes,
		metrics.providerRetainedBytes,
	)
	metrics.mu.Unlock()
}

func (metrics *reconciliationRuntimeMetrics) sharedContainerScan() {
	metrics.mu.Lock()
	metrics.sharedContainerScans++
	metrics.mu.Unlock()
}

func (metrics *reconciliationRuntimeMetrics) openCodeSQLiteParse() {
	metrics.mu.Lock()
	metrics.openCodeSQLiteParses++
	metrics.mu.Unlock()
}

// codexReplacementIndexBuild counts one per-pass Codex replacement index
// build. Unlike the other recorders it tolerates a nil receiver because it
// is reached from tombstone passes that run outside streamed reconciliation,
// where no runtime metrics are in the context.
func (metrics *reconciliationRuntimeMetrics) codexReplacementIndexBuild() {
	if metrics == nil {
		return
	}
	metrics.mu.Lock()
	metrics.codexReplacementBuilds++
	metrics.mu.Unlock()
}

func (metrics *reconciliationRuntimeMetrics) snapshot(
	spool ReconciliationMetrics,
) ReconciliationMetrics {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	spool.MaxProviderBuffered = metrics.maxProvider
	spool.MaxRehydratedSources = metrics.maxRehydrated
	spool.MaxWorkerResults = metrics.maxWorkerResults
	spool.MaxPendingWrites = metrics.maxPendingWrites
	spool.GlobalLinkPasses = metrics.globalLinkPasses
	spool.MaxProviderRetainedBytes = metrics.maxProviderRetainedBytes
	spool.SharedContainerScans = metrics.sharedContainerScans
	spool.OpenCodeSQLiteParses = metrics.openCodeSQLiteParses
	spool.CodexReplacementIndexBuilds = metrics.codexReplacementBuilds
	return spool
}

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
	AgentDirs      map[parser.AgentType][]string
	SourceMachines map[parser.AgentType]map[string]string
	// DisabledAgents identifies providers omitted from this local filesystem
	// engine. Remote import engines leave it empty and import every transferred
	// provider.
	DisabledAgents          []parser.AgentType
	Machine                 string
	BlockedResultCategories []string
	// IncludeCwdPrefixes, when non-empty, restricts ingestion to
	// sessions whose working directory equals one of the prefixes
	// or lives underneath one. Sessions without a recorded cwd are
	// skipped while the filter is active. Populated from the
	// sync_include_cwd_prefixes config option for local sync;
	// remote sync leaves it empty because the prefixes describe
	// local paths.
	IncludeCwdPrefixes []string
	// ScanProtectedPaths lets local Git discovery read working directories
	// inside macOS TCC-protected locations (Documents, Downloads, Desktop,
	// iCloud Drive, and cloud-provider folders). It defaults to false so a
	// first sync cannot raise consent prompts the user cannot explain;
	// sessions there keep path-only project identity. Populated from the
	// scan_protected_paths config option. The safe default belongs to the
	// zero value so an engine built without the option never prompts.
	ScanProtectedPaths bool
	// IDPrefix is prepended to all session IDs. Used by
	// remote sync to namespace IDs by host (e.g. "host~").
	IDPrefix string
	// PathRewriter transforms file paths before storage.
	// Used by remote sync to replace temp paths with
	// "host:/remote/path" references.
	PathRewriter func(string) string
	// StoredPathResolver maps a canonical stored source path back to its
	// physical path under the current mirror. Remote changed-path planning uses
	// it to make persisted source hints usable by mirror-local providers.
	StoredPathResolver func(storedPath string) (physicalPath string, ok bool)
	// InitialSkipCache seeds an ephemeral engine with caller-owned translated
	// skip state. Rebuild contributors use it because their engine is created
	// inside the atomic replacement workflow.
	InitialSkipCache map[string]int64
	// Ephemeral disables sync-state persistence (timestamps
	// and skip cache) so remote sync does not interfere with
	// local sync watermarks or pollute the skipped_files table
	// with temp-dir paths.
	Ephemeral bool
	// DiscardPendingWritesOnCancel makes scoped callers treat cancellation as
	// a hard archive-write boundary. Isolated capture databases use it because
	// retries rebuild scratch state; live archives leave it false so cancellation
	// can still revoke already-staged deletion proof before returning.
	DiscardPendingWritesOnCancel bool
	// DisableSignalRecomputation skips quality and secret signal work. It is
	// reserved for bounded parsers whose result does not consume those fields.
	DisableSignalRecomputation bool
	// DisableFilesystemProjectDiscovery prevents project attribution from
	// touching working directories recorded in imported transcripts. Bounded
	// capture uses lexical metadata instead.
	DisableFilesystemProjectDiscovery bool
	// StableSourceSnapshots reports that configured source files are immutable
	// for this engine. Bounded capture sets it after copying quiescent sources.
	StableSourceSnapshots bool
	// Emitter, when non-nil, is called once after each sync pass
	// that wrote data. Safe to leave nil (e.g., in PG serve mode
	// where the engine is not run).
	Emitter Emitter
	// DeferStartupMaintenance keeps startup backfills blocked until the
	// foreground sync that launched the daemon has completed. Maintenance
	// still takes syncMu after it is released so later syncs and resyncs
	// cannot overlap its database access.
	DeferStartupMaintenance bool
	// OnStartupReconciled runs once, after syncMu is released, when a full
	// startup discovery completed authoritatively. Failed, cancelled, and
	// aborted attempts leave it eligible for a later successful owner.
	OnStartupReconciled func(SyncStats, error)
	// ProgressStallAfter controls when CurrentProgress marks an active pass as
	// stalled. Zero uses the production default. Tests and embedders may use a
	// shorter interval without changing the daemon-wide policy.
	ProgressStallAfter time.Duration
	// ProviderFactories and ProviderMigrationModes select which concrete
	// providers own discovery and parsing for their agents. Nil uses the
	// parser package registry/manifest.
	ProviderFactories      []parser.ProviderFactory
	ProviderMigrationModes map[parser.AgentType]parser.ProviderMigrationMode
}

// Engine orchestrates session file discovery and sync.
type Engine struct {
	db    *db.DB
	stat  func(string) (os.FileInfo, error)
	lstat func(string) (os.FileInfo, error)
	// archiveStore is the database holding previously archived
	// sessions for the preserve guards in prepareSessionWrite.
	// During a resync/rebuild it points at the original DB while
	// e.db points at the fresh one; nil means e.db is the archive.
	archiveStore db.Store
	// archiveStaleClaudeForks snapshots the original archive's stale Claude
	// fork rows for one rebuild; nil outside a rebuild, where e.db is queried
	// per source path instead.
	archiveStaleClaudeForks *archiveStaleClaudeForkIndex
	deferredSourceCwd       *sourceCwdReconciliationBatch
	agentDirs               map[parser.AgentType][]string
	sourceMachines          map[parser.AgentType]map[string]string
	preserveAgents          []parser.AgentType
	machine                 string
	blockedResultCategories map[string]bool
	cwdFilter               cwdPrefixFilter
	// scanProtectedPaths, homeDir, and goos gate passive probing of macOS
	// TCC-protected locations. homeDir is empty when the home directory
	// cannot be resolved, which disables the gate rather than guessing.
	// goos mirrors runtime.GOOS so the gate is testable off-darwin.
	scanProtectedPaths bool
	homeDir            string
	goos               string
	syncMu             gosync.Mutex // serializes all sync operations
	mu                 gosync.RWMutex
	lastSync           time.Time
	lastSyncStats      SyncStats
	currentProgress    *Progress
	progressStallAfter time.Duration
	// skipCache tracks paths that should be skipped on
	// subsequent syncs, keyed by path with the file mtime
	// at time of caching. Covers parse errors and
	// non-interactive sessions (nil result). The file is
	// retried when its mtime changes. S3 entries also keep an
	// in-memory source fingerprint when one is available.
	skipMu           gosync.RWMutex
	skipCache        map[string]int64
	skipFingerprints map[string]string
	// retryUnsafeSkipPaths records sources whose successful processing changed
	// exclusion or source-missing state in the current database. A rebuild that
	// later fails discards those database changes, so its skip entries cannot be
	// carried into the next attempt.
	retryUnsafeSkipPaths map[string]struct{}
	// skipHashKeys maps a source base path to its one current
	// ?source_hash= cache key. It is built once when the cache loads so a
	// watcher mutation never scans unrelated archive entries.
	skipHashKeys      map[string]string
	s3CodexIndexMu    gosync.Mutex
	s3CodexIndexCache map[string]s3CodexIndexSnapshot
	// idPrefix and pathRewriter support remote sync:
	// prefix all session IDs to avoid collisions, rewrite
	// temp paths to "host:/remote/path" form.
	ephemeral               bool
	discardWritesOnCancel   bool
	disableSignalRecompute  bool
	disableProjectDiscovery bool
	stableSourceSnapshots   bool
	idPrefix                string
	pathRewriter            func(string) string
	storedPathResolver      func(string) (string, bool)
	emitter                 Emitter
	providerFactories       map[parser.AgentType]parser.ProviderFactory
	providerMigrationModes  map[parser.AgentType]parser.ProviderMigrationMode
	// providerStatHashers caches the optional MultiFileStatHasher
	// implementations keyed by AgentType. Populated at engine
	// construction by type-asserting each constructed provider; nil
	// entries indicate the provider does not implement
	// MultiFileStatHasher (single-file agents and providers without a
	// multi-file layout take the existing stat-only composite path).
	providerStatHashers map[parser.AgentType]parser.MultiFileStatHasher

	providerWatchRootsMu    gosync.Mutex
	providerWatchRoots      map[parser.AgentType][]parser.WatchRoot
	projectIdentityMu       gosync.Mutex
	projectIdentityCache    map[string]projectIdentityCacheEntry
	projectIdentityWritten  map[string]struct{}
	startupMaintenanceOnce  gosync.Once
	startupMaintenanceReady chan struct{}
	startupReconciledOnce   gosync.Once
	startupReconciledReady  chan struct{}
	startupAttemptOnce      gosync.Once
	startupAttemptReady     chan struct{}
	startupReconciledStats  SyncStats
	startupReconciledErr    error
	startupCallbackOnce     gosync.Once
	onStartupReconciled     func(SyncStats, error)
	// writeBatchOverride is a test seam for exercising reconciliation archive
	// write failures after discovery and parse have succeeded.
	writeBatchOverride func([]pendingWrite, syncWriteMode, bool) (int, int, int, int)
	// stagedProviderStatHashes is a test observability seam that
	// counts every successful staging of pr.providerStatHash in
	// applyProviderFilePathPolicies. Tests assert against a
	// post-sync snapshot via ResetStagedProviderStatHashes /
	// StagedProviderStatHashes to verify the per-component digest
	// staging block actually ran, distinct from proving the parse
	// path ran at all (which is the counting wrapper's job). Without
	// this counter a regression that drops the staging call while
	// keeping provider.Fingerprint intact would still satisfy any
	// hasStored==false assertion because flushPending's nil check
	// is a no-op on absent digests. atomic.Int64 is a zero-value
	// type so the field does not need explicit initialization.
	stagedProviderStatHashes atomic.Int64
	// sourceAttributionLookupOverride observes or replaces bounded source
	// attribution queries in sync tests.
	sourceAttributionLookupOverride func(
		context.Context, []db.SessionSourcePath,
	) ([]db.SessionSourceAttribution, error)
	// workerCountOverride is a test seam for exercising the production worker
	// floor and cap independently of the host CPU count.
	workerCountOverride int
	// claudeProjectSessionFiles is an observability seam for cardinality tests
	// around duplicate-session discovery.
	claudeProjectSessionFiles func(string) []parser.DiscoveredFile
	parseRetentionOnce        gosync.Once
	parseRetentionBudget      *parseRetentionBudget
	bulkRetentionOnce         gosync.Once
	bulkRetentionBudget       *parseRetentionBudget
	// activeRetention points at the budget the in-flight pass admits parses
	// through. Bulk archive passes (full sync, resync rebuild, remote import)
	// install the unthrottled bulk budget for their duration; when nil,
	// incremental paths (watcher, scoped/periodic syncs, reconciliation
	// pages, single-session syncs) use the bounded default budget.
	activeRetention atomic.Pointer[parseRetentionBudget]

	// forceParse disables every stored-state skip (skip cache,
	// size/mtime/data_version checks, incremental JSONL deltas) so
	// parse-diff fully re-parses every discovered file. Normal sync
	// never sets it; behavior must be identical when false.
	forceParse bool
	// forceFullParse keeps explicit full-pass intent engine-wide. Full discovery
	// also marks files ForceParse, but provider-specific paths must still bypass
	// every freshness gate if that per-file bit is not carried forward.
	forceFullParse bool
	// forceFullParseAllowsCache retains the complete-parse requirement while
	// allowing a durable error-skip entry to prove that a source was already
	// attempted. Remote journal replay uses this after cache invalidation was
	// durably consumed.
	forceFullParseAllowsCache bool

	// phaseStats accumulates per-phase wall-clock time inside the bulk
	// write path. Exposed via PhaseStats() so a CLI driver can log the
	// totals after a sync pass completes.
	phaseStats PhaseStats

	// anomalies accumulates per-run parser/sanitizer anomaly signals
	// recorded at the write seam (prepareSessionWrite, writeIncremental,
	// toDBUsageEvents). Reset at the start of each sync run and folded
	// into the returned SyncStats before the run completes.
	anomalies anomalyAccumulator

	// signalSched debounces the O(session history) signal/secret
	// recompute triggered by incremental writes, so streaming
	// sessions don't rescan their whole history on every appended
	// line. Close flushes and stops it.
	signalSched *signalScheduler

	// containerMu guards the OpenCode-family shared-SQLite freshness
	// gate (see opencode_container_gate.go). trustedSQLiteContainers
	// maps a container DB path to its state and verified session-ID
	// set at the end of the last pass that verified every one of its
	// discovered sessions; containerPass is the bookkeeping for the
	// pass currently running (nil outside passes). Both are in-memory
	// only: a restart re-verifies once.
	containerMu             gosync.Mutex
	trustedSQLiteContainers map[string]trustedSQLiteContainer
	containerPass           *sqliteContainerPass

	// storageTrustMu guards the per-session freshness gate for
	// OpenCode-family file-backed storage sessions (see
	// opencode_storage_gate.go). trustedStorageSessions maps a session
	// JSON path to the stat signature captured before the last parse
	// whose outcome the archive absorbed (results dropped as already
	// stored, or confirmed written). storageTrustGens counts each
	// session's invalidations and storageTrustEpoch counts full clears,
	// so a promotion whose pre-parse snapshot predates an invalidation
	// is discarded instead of resurrecting the invalidated trust. All
	// in-memory only: a restart re-verifies once.
	storageTrustMu         gosync.Mutex
	trustedStorageSessions map[string]string
	storageTrustGens       map[string]uint64
	storageTrustEpoch      uint64

	// verifiedSourceMu guards the local source stat/ctime trust gate (see
	// verified_source_gate.go). Each path has one compact record containing
	// its trusted signature, invalidation generation, and last full pass seen.
	// The epoch vetoes promotions captured before a global clear. State is
	// memory-only, so process startup always deep-verifies sources once.
	verifiedSourceMu         gosync.Mutex
	verifiedSources          map[verifiedSourceKey]verifiedSourceRecord
	verifiedSourceEpoch      uint64
	verifiedSourcePass       uint64
	verifiedSourceActivePass uint64

	reconciliationMu           gosync.RWMutex
	lastReconciliation         ReconciliationResult
	reconciliationSpoolFactory func(string) (reconciliationSpoolStore, error)
}

// forceParseRequested centralizes the parse modes that require complete source
// processing and preserve every parsed result. The separate cache predicate
// lets remote replay suppress sources whose post-invalidation attempt is
// already durable.
func (e *Engine) forceParseRequested(file parser.DiscoveredFile) bool {
	return e.forceParse || e.forceFullParse || file.ForceParse ||
		file.ForceFullParse
}

func (e *Engine) forceParseBypassesCache(file parser.DiscoveredFile) bool {
	return e.forceParse || file.ForceParse ||
		(e.forceFullParse && !e.forceFullParseAllowsCache)
}

// ReconciliationResult is the structured acknowledgement for the most recent
// watcher-forced reconciliation attempt.
type ReconciliationResult struct {
	Complete         bool
	Aborted          bool
	ProviderFailures int
	Metrics          ReconciliationMetrics
}

// LastReconciliationResult returns a snapshot of the latest watcher-forced
// reconciliation acknowledgement.
func (e *Engine) LastReconciliationResult() ReconciliationResult {
	e.reconciliationMu.RLock()
	defer e.reconciliationMu.RUnlock()
	return e.lastReconciliation
}

func (e *Engine) setLastReconciliationResult(result ReconciliationResult) {
	e.reconciliationMu.Lock()
	e.lastReconciliation = result
	e.reconciliationMu.Unlock()
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

// ProviderStatHasher returns the cached MultiFileStatHasher for the
// given agent type, or nil when the agent does not declare a
// multi-file on-disk layout. Tests use this to confirm the
// per-component freshness gate is wired only for agents whose
// Source.MultiFileStatHash capability is supported, and absent
// otherwise so a 0==0 digest match does not short-circuit the
// legacy size/mtime freshness path for unrelated agents.
func (e *Engine) ProviderStatHasher(agent parser.AgentType) parser.MultiFileStatHasher {
	if e == nil {
		return nil
	}
	return e.providerStatHashers[agent]
}

// StagedProviderStatHashes returns the cumulative number of per-source
// process results whose applyProviderFilePathPolicies step attached a
// non-nil res.providerStatHash. Tests reset the counter via
// ResetStagedProviderStatHashes, run a sync, and assert on the
// post-sync value to verify the per-component digest staging block
// actually ran. The counter is cumulative across the engine's
// lifetime; tests reset it explicitly rather than relying on a
// per-pass reset so production code does not pay for an unnecessary
// zero on every SyncAll entry.
func (e *Engine) StagedProviderStatHashes() int64 {
	if e == nil {
		return 0
	}
	return e.stagedProviderStatHashes.Load()
}

// ResetStagedProviderStatHashes zeroes the test observability counter
// so a single sync pass's staging events can be read as a clean
// snapshot via StagedProviderStatHashes.
func (e *Engine) ResetStagedProviderStatHashes() {
	if e == nil {
		return
	}
	e.stagedProviderStatHashes.Store(0)
}

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
	skipHashKeys, _ := normalizeSourceHashSkipCache(skipCache, nil)

	dirs := make(map[parser.AgentType][]string, len(cfg.AgentDirs))
	for k, v := range cfg.AgentDirs {
		dirs[k] = append([]string(nil), v...)
	}
	sourceMachines := make(map[parser.AgentType]map[string]string, len(cfg.SourceMachines))
	for agent, roots := range cfg.SourceMachines {
		sourceMachines[agent] = maps.Clone(roots)
	}
	providerFactories := parser.ProviderFactories()
	if cfg.ProviderFactories != nil {
		providerFactories = cfg.ProviderFactories
	}
	disabledAgents := append([]parser.AgentType(nil), cfg.DisabledAgents...)
	providerFactories = slices.DeleteFunc(
		providerFactories,
		func(factory parser.ProviderFactory) bool {
			return slices.Contains(disabledAgents, factory.Definition().Type)
		},
	)
	providerModes := parser.ProviderMigrationModes()
	if cfg.ProviderMigrationModes != nil {
		maps.Copy(providerModes, cfg.ProviderMigrationModes)
	}

	if cfg.ScanProtectedPaths {
		// Parsers extract project names by probing recorded cwds for git
		// roots; that guard is package-level because extraction runs deep
		// inside per-format code with no engine to consult. The opt-in only
		// ever enables it: engines built without the option (remote sync,
		// the identity backfill) must not revoke the user's process-wide
		// choice.
		parser.SetAllowProtectedPathProbes(true)
	}
	progressStallAfter := cfg.ProgressStallAfter
	if progressStallAfter <= 0 {
		progressStallAfter = defaultProgressStallAfter
	}
	e := &Engine{
		db:                      database,
		stat:                    os.Stat,
		lstat:                   os.Lstat,
		agentDirs:               dirs,
		sourceMachines:          sourceMachines,
		preserveAgents:          disabledAgents,
		machine:                 cfg.Machine,
		blockedResultCategories: blockedCategorySet(cfg.BlockedResultCategories),
		cwdFilter:               newCwdPrefixFilter(cfg.IncludeCwdPrefixes),
		scanProtectedPaths:      cfg.ScanProtectedPaths,
		homeDir:                 userHomeDirOrEmpty(),
		goos:                    runtime.GOOS,
		skipCache:               skipCache,
		skipFingerprints:        make(map[string]string),
		skipHashKeys:            skipHashKeys,
		s3CodexIndexCache:       make(map[string]s3CodexIndexSnapshot),
		ephemeral:               cfg.Ephemeral,
		discardWritesOnCancel:   cfg.DiscardPendingWritesOnCancel,
		disableSignalRecompute:  cfg.DisableSignalRecomputation,
		disableProjectDiscovery: cfg.DisableFilesystemProjectDiscovery,
		stableSourceSnapshots:   cfg.StableSourceSnapshots,
		idPrefix:                cfg.IDPrefix,
		pathRewriter:            cfg.PathRewriter,
		storedPathResolver:      cfg.StoredPathResolver,
		emitter:                 cfg.Emitter,
		providerFactories:       providerFactoryMap(providerFactories),
		providerMigrationModes:  providerModes,
		providerStatHashers: buildProviderStatHashers(
			providerFactoryMap(providerFactories)),
		providerWatchRoots:      make(map[parser.AgentType][]parser.WatchRoot),
		projectIdentityCache:    make(map[string]projectIdentityCacheEntry),
		projectIdentityWritten:  make(map[string]struct{}),
		startupMaintenanceReady: make(chan struct{}),
		startupReconciledReady:  make(chan struct{}),
		startupAttemptReady:     make(chan struct{}),
		onStartupReconciled:     cfg.OnStartupReconciled,
		progressStallAfter:      progressStallAfter,
		reconciliationSpoolFactory: func(path string) (reconciliationSpoolStore, error) {
			return newReconciliationSpool(path)
		},
		claudeProjectSessionFiles: parser.ClaudeProjectSessionFiles,
	}
	if len(cfg.InitialSkipCache) > 0 {
		e.InjectSkipCache(cfg.InitialSkipCache)
	}
	if !cfg.DeferStartupMaintenance {
		e.ReleaseStartupMaintenance()
	}
	// Errors are logged inside recomputeSignalsFromDB and are
	// non-fatal: the next write or flush retries.
	recompute := func(sessionID string) {
		_, _ = e.recomputeSignalsFromDB(
			context.Background(), sessionID,
		)
	}
	if cfg.DisableSignalRecomputation {
		recompute = func(string) {}
	}
	e.signalSched = newSignalScheduler(
		signalRecomputeInterval, signalRecomputeQuiet,
		// Inline runs happen from markDirty inside writeIncremental,
		// whose callers already hold syncMu.
		recompute,
		// Timer and flush passes happen outside any sync operation,
		// so they take syncMu around the whole claim-and-recompute
		// pass: otherwise a delayed recompute could read an older
		// message snapshot and overwrite signals just written by a
		// concurrent sync, or claim a session and block while a
		// locked pre-push flush finds nothing left to recompute.
		func(flush func()) {
			e.syncMu.Lock()
			defer e.syncMu.Unlock()
			flush()
		},
	)
	if cfg.DisableSignalRecomputation {
		e.signalSched.stop()
	}
	return e
}

func (e *Engine) machineForPath(agent parser.AgentType, path string) string {
	if machine, ok := e.configuredMachineForPath(agent, path); ok {
		return machine
	}
	return e.machine
}

func (e *Engine) configuredMachineForPath(
	agent parser.AgentType, path string,
) (string, bool) {
	path, err := pathutil.LocalComparisonKey(path)
	if err != nil {
		return "", false
	}
	bestRoot := ""
	machine := ""
	for root, candidate := range e.sourceMachines[agent] {
		cleanRoot, err := pathutil.LocalComparisonKey(root)
		if err != nil {
			continue
		}
		if !pathWithinRoot(path, cleanRoot) || len(cleanRoot) <= len(bestRoot) {
			continue
		}
		bestRoot = cleanRoot
		machine = candidate
	}
	return machine, bestRoot != ""
}

func (e *Engine) machineForFile(file parser.DiscoveredFile) string {
	if file.Machine != "" {
		return file.Machine
	}
	return e.machineForPath(file.Agent, file.Path)
}

func (e *Engine) machineForProviderSource(
	agent parser.AgentType, source parser.SourceRef, fallbackPath string,
) string {
	if source.ConfiguredRoot != "" {
		return e.machineForPath(agent, source.ConfiguredRoot)
	}
	return e.machineForPath(agent, fallbackPath)
}

// reconciliationOwnershipMachines returns the distinct machines whose stored
// ownership rows can fall inside roots. A row keeps the machine it was admitted
// under, so a root that has since been relabeled still needs its older machine
// queried or its deletions would never be discovered.
func (e *Engine) reconciliationOwnershipMachines(
	agent parser.AgentType, roots []string, archiveMachines []string,
) []string {
	seen := make(map[string]bool, len(roots)+len(archiveMachines)+1)
	machines := make([]string, 0, len(roots)+len(archiveMachines)+1)
	add := func(machine string) {
		if seen[machine] {
			return
		}
		seen[machine] = true
		machines = append(machines, machine)
	}
	configured := make([]string, 0, len(e.sourceMachines[agent]))
	for root := range e.sourceMachines[agent] {
		configured = append(configured, root)
	}
	// Map iteration is unordered; sort so the query sequence is deterministic.
	slices.Sort(configured)
	for _, root := range roots {
		add(e.machineForPath(agent, root))
		clean, err := pathutil.LocalComparisonKey(root)
		if err != nil {
			continue
		}
		// A reconciliation scope is a watched directory, which may sit above
		// (or below) the labeled root itself — a container file such as
		// Hermes's state.db is configured directly but watched via its parent.
		for _, cfg := range configured {
			cleanCfg, err := pathutil.LocalComparisonKey(cfg)
			if err != nil {
				continue
			}
			if pathWithinRoot(cleanCfg, clean) || pathWithinRoot(clean, cleanCfg) {
				add(e.sourceMachines[agent][cfg])
			}
		}
	}
	// Unlabeled roots, and rows admitted before a label existed, are stored
	// under the local machine, so it always stays in the query set.
	add(e.machine)
	// A label the user has since edited away is still stored on the rows it
	// admitted, and is no longer derivable from configuration. The archive's
	// own machine list is small and closes that gap; each entry still queries
	// through the (machine, agent, file_path) ownership index.
	for _, machine := range archiveMachines {
		add(machine)
	}
	return machines
}

func pathWithinRoot(path, root string) bool {
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Close flushes any pending debounced signal recomputes and stops
// the scheduler. Call once when the engine's owner shuts down;
// safe to call repeatedly.
func (e *Engine) Close() {
	e.signalSched.stop()
}

// FlushSignals immediately recomputes signals for sessions with a
// pending debounced recompute, leaving the scheduler running. Push
// paths that read SQLite rows outside a sync operation call it
// first so pushed sessions carry current signal fields. Callers
// must not hold syncMu; work running inside SyncThenRun is flushed
// by the engine instead.
func (e *Engine) FlushSignals() {
	e.signalSched.flushAll()
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

// buildProviderStatHashers constructs probe providers for each factory
// and caches the MultiFileStatHasher implementations. Because every
// SourceSet-wrapped provider unconditionally satisfies the
// MultiFileStatHasher interface (a forwarding method on the wrapper
// delegates to the inner SourceSet when present and returns 0
// otherwise), the gate here MUST be the explicit
// Source.MultiFileStatHash capability declaration: an agent whose
// inner SourceSet does not own a multi-file layout would otherwise
// be registered with a 0-returning stub, and a warm pass would
// short-circuit its parse on a 0==0 digest match between
// ComputeMultiFileStatHash(path) and the previously-stored 0. The
// probe construction must be side-effect-free: a fresh provider is
// allocated per agent with an empty ProviderConfig (no roots, no
// machine), and a probe that passes both gates is retained for the
// engine's lifetime as the cached hasher. Retaining a config-less
// instance is safe only because ComputeMultiFileStatHash is stateless
// — a pure function of its chatPath argument and the filesystem — so
// implementations must not depend on provider construction state such
// as Roots or Machine.
//
// Freebuff sessions intentionally route through this single
// AgentCodebuff entry: AgentFreebuff is a recognized AgentType string
// for ID-prefix display, but the on-disk Codebuff provider is the
// sole registration in parser.Registry for both paid Codebuff and
// free Freebuff transcripts (see types_test.go TestFreebuffNotRegistered).
// parser.AgentFreebuff therefore never appears as a Registry key
// and never lands here as a separate hasher entry; Freebuff
// sessions are parsed by the AgentCodebuff probe and surface in
// storage with agent=AgentCodebuff, which is the key this map
// actually uses. Any future change that adds AgentFreebuff as a
// distinct Registry entry must carry Source.MultiFileStatHash=
// CapabilitySupported along with it -- otherwise Freebuff sessions
// will silently fall back to the legacy size/max-mtime composite
// in providerSourceFreshBeforeFingerprint and the per-component
// digest gate will under-cover them.
func buildProviderStatHashers(
	factories map[parser.AgentType]parser.ProviderFactory,
) map[parser.AgentType]parser.MultiFileStatHasher {
	out := make(map[parser.AgentType]parser.MultiFileStatHasher, len(factories))
	for agent, factory := range factories {
		probe := factory.NewProvider(parser.ProviderConfig{
			Machine: "",
		})
		if probe.Capabilities().Source.MultiFileStatHash != parser.CapabilitySupported {
			continue
		}
		if h, ok := probe.(parser.MultiFileStatHasher); ok {
			out[agent] = h
		}
	}
	return out
}

// pendingProviderStatHash is the staged freshness digest attached to a
// per-source processResult. The engine stages it during processFile
// (before any sessions-table write) and only persists it after the
// matching source's write batch commits successfully, so a CWD-filtered
// or failed write cannot mark an absent or stale session as fresh.
//
// physicalPath is the on-disk chat path the hasher stats (the path that
// actually exists locally, even when pathRewriter rewrites the stored
// file_path to a logical "host:/remote/path" for remote imports).
// targetKey is the cache key the engine uses for both the digest lookup
// and persistence, so remote-synced sessions hash their materialized
// file but store the digest under their canonical logical key.
//
// digest is the per-component stat snapshot the hasher computed at
// staging time. Capturing it here closes a TOCTOU window between
// provider.Fingerprint (which stats and parses a stable file state)
// and the later flushPending write: if the file changes between the
// two calls, a re-compute at flushPending time would store the new
// state under the old parse, falsely pinning the cache against the
// next warm pass. Persisting the pre-parse digest under the same
// session row guarantees provider_freshness reflects the file state
// the write actually committed.
type pendingProviderStatHash struct {
	agent        parser.AgentType
	physicalPath string
	targetKey    string
	digest       uint64
}

// recordProviderStatHash persists the pre-computed per-component stat
// digest for a source to provider_freshness. The digest is staged in
// applyProviderFilePathPolicies at the same wall-clock moment the file
// is stat-ed for the parse, so what is persisted always matches the
// snapshot the parse saw. Providers without a MultiFileStatHasher
// implementation carry a zero digest and are silently skipped here:
// their freshness is owned by the engine's existing skip-cache path,
// and the staging site does not populate res.providerStatHash for
// non-multi-file agents. The cache key (targetKey) and the hashed path
// (physicalPath) are kept distinct so a remote import whose
// pathRewriter rewrites the stored file_path to a logical
// "host:/remote/path" still hashes the materialized local file.
func (e *Engine) recordProviderStatHash(
	ctx context.Context,
	hash pendingProviderStatHash,
) {
	if hash.digest == 0 {
		return
	}
	if _, ok := e.providerStatHashers[hash.agent]; !ok {
		return
	}
	if !providerStatHashMetadataVerified(hash) {
		return
	}
	if err := e.db.UpsertProviderStatHash(
		ctx, hash.agent, hash.targetKey, hash.digest,
	); err != nil {
		log.Printf(
			"provider_freshness write for %s/%s: %v",
			hash.agent, hash.targetKey, err,
		)
	}
}

// providerStatHashMetadataVerified prevents Codex freshness from outrunning
// its title sidecar. A missing session_index.jsonl is normal and verified;
// read and scan failures are transient and must leave the previous digest in
// place so a later pass retries the title check.
func providerStatHashMetadataVerified(hash pendingProviderStatHash) bool {
	if hash.agent != parser.AgentCodex {
		return true
	}
	if err := parser.VerifyCodexSessionIndex(hash.physicalPath); err != nil {
		log.Printf(
			"verify Codex title index before freshness write for %s: %v",
			hash.physicalPath, err,
		)
		return false
	}
	return true
}

// clearProviderSourceFreshness removes a digest that no longer represents a
// completely committed provider source. Claude DAG members are written stale
// up front, so correctness does not depend on cleanup succeeding after a
// partial write.
func (e *Engine) clearProviderSourceFreshness(
	ctx context.Context,
	statHash *pendingProviderStatHash,
) {
	if statHash != nil {
		if err := e.db.DeleteProviderStatHash(
			ctx, statHash.agent, statHash.targetKey,
		); err != nil {
			log.Printf(
				"delete incomplete provider freshness for %s/%s: %v",
				statHash.agent, statHash.targetKey, err,
			)
		}
	}
}

// partitionIntentionalSourceSkips separates active source members from rows
// that user policy permanently excludes or keeps in trash. Those policy skips
// already resolve the member for source freshness and must not make active
// Claude DAG branches stale.
func (e *Engine) partitionIntentionalSourceSkips(
	ids []string,
) (active []string, skipped map[string]bool) {
	active = make([]string, 0, len(ids))
	for _, id := range ids {
		if e.db.IsSessionExcluded(id) || e.db.IsSessionTrashed(id) {
			if skipped == nil {
				skipped = make(map[string]bool)
			}
			skipped[id] = true
			continue
		}
		active = append(active, id)
	}
	return active, skipped
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
	p := *e.currentProgress
	if !p.UpdatedAt.IsZero() && e.progressStallAfter > 0 &&
		time.Since(p.UpdatedAt) >= e.progressStallAfter {
		p.Stalled = true
	}
	return p, true
}

// UpdateProgress records progress produced by work running outside the engine,
// such as the isolated sync worker process.
func (e *Engine) UpdateProgress(p Progress) {
	e.reportProgress(nil, p)
}

// FinishProgress clears progress produced by work running outside the engine.
func (e *Engine) FinishProgress() {
	e.clearCurrentProgress()
}

func (e *Engine) reportProgress(
	onProgress ProgressFunc, p Progress,
) {
	now := time.Now()
	e.mu.Lock()
	tracked := p
	if tracked.StartedAt.IsZero() {
		if e.currentProgress != nil && !e.currentProgress.StartedAt.IsZero() {
			tracked.StartedAt = e.currentProgress.StartedAt
		} else {
			tracked.StartedAt = now
		}
	}
	tracked.UpdatedAt = now
	tracked.Stalled = false
	e.currentProgress = &tracked
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
	agent          parser.AgentType
	path           string
	machine        string
	retentionLease *parseRetentionLease
}

func (j *syncJob) releaseRetention() {
	if j == nil {
		return
	}
	j.retentionLease.Release()
	j.retentionLease = nil
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
	_ = e.SyncPathsContext(context.Background(), paths)
}

// SyncPathsContext is SyncPaths with caller-controlled cancellation. The
// file watcher threads the serve shutdown context through here: its stop
// path waits for the in-flight onChange callback, so a watcher-driven sync
// that ignored SIGTERM would hold shutdown until a service manager's kill
// timeout instead of aborting between files like every other sync path.
type preparedChangedPathSync struct {
	files              []parser.DiscoveredFile
	missingPaths       []string
	preContainerStates map[string]parser.SQLiteContainerState
	classificationErr  error
}

func (p preparedChangedPathSync) empty() bool {
	return len(p.files) == 0 && len(p.missingPaths) == 0
}

func (e *Engine) prepareChangedPathSync(
	ctx context.Context, paths []string,
) preparedChangedPathSync {
	prepared := preparedChangedPathSync{
		missingPaths: make([]string, 0, len(paths)),
	}
	for _, path := range paths {
		if _, err := e.lstatSource(path); os.IsNotExist(err) {
			prepared.missingPaths = append(prepared.missingPaths, path)
		}
	}
	// Capture container states before classifyPaths lists any session rows,
	// matching the capture-before-discovery ordering of full syncs.
	prepared.preContainerStates = e.captureSQLiteContainerStates(paths)
	prepared.files, prepared.classificationErr = e.classifyPaths(ctx, paths)
	prepared.missingPaths = omitMissingPersistentContainerPaths(
		prepared.missingPaths, prepared.files,
	)
	return prepared
}

// syncChangedPathsLocked tracks changed-path preparation and application as
// one owned pass. The caller holds syncMu, so this progress cannot overwrite a
// different pass while waiting to enter the serialized sync path.
func (e *Engine) syncChangedPathsLocked(
	ctx context.Context, paths []string,
) (SyncStats, int, error) {
	e.reportProgress(nil, Progress{
		Phase:  PhaseDiscovering,
		Detail: "Preparing changed session paths",
	})
	return e.applyChangedPathSyncLocked(
		ctx, e.prepareChangedPathSync(ctx, paths),
	)
}

func (e *Engine) applyChangedPathSyncLocked(
	ctx context.Context, prepared preparedChangedPathSync,
) (SyncStats, int, error) {
	if prepared.empty() {
		return SyncStats{}, 0, prepared.classificationErr
	}
	e.resetS3CodexIndexCache()
	e.anomalies.reset()
	// Begin a container pass so an already-trusted, unchanged container
	// still gates its fan-out, but never promote from a changed-path subset.
	e.beginSQLiteContainerPass(prepared.files, prepared.preContainerStates)
	e.reportProgress(nil, Progress{
		Phase:         PhaseSyncing,
		Detail:        "Syncing changed session paths",
		SessionsTotal: len(prepared.files),
	})
	results := e.startWorkers(ctx, prepared.files)
	stats := e.collectAndBatch(
		ctx, results, len(prepared.files), len(prepared.files), nil,
		syncWriteDefault,
	)
	e.finishSQLiteContainerPass(true, false)
	e.anomalies.applyTo(&stats)
	e.persistSkipCache()
	complete := prepared.classificationErr == nil && ctx.Err() == nil &&
		!stats.Aborted && stats.Failed == 0 && stats.providerFailures == 0
	tombstoned := 0
	if complete && len(prepared.missingPaths) > 0 {
		var err error
		tombstoned, err = e.tombstoneMissingWatchSourcesLocked(
			ctx, prepared.missingPaths, nil,
		)
		if err != nil {
			return stats, tombstoned, fmt.Errorf("watcher source tombstone: %w", err)
		}
	}
	e.mu.Lock()
	e.lastSync = time.Now()
	e.lastSyncStats = stats
	e.mu.Unlock()
	if stats.Synced > 0 {
		log.Printf("sync: %d file(s) updated", stats.Synced)
	}
	if err := errors.Join(prepared.classificationErr, ctx.Err()); err != nil {
		return stats, tombstoned, err
	}
	if !complete {
		return stats, tombstoned, fmt.Errorf(
			"changed-path sync incomplete: %d source or archive failures",
			stats.Failed,
		)
	}
	return stats, tombstoned, nil
}

// SyncPathsContext is SyncPaths with caller-controlled cancellation. The
// file watcher threads the serve shutdown context through here.
func (e *Engine) SyncPathsContext(ctx context.Context, paths []string) error {
	if e.refuseWriteInForceParse("SyncPaths") {
		return nil
	}
	if e.disableProjectDiscovery {
		ctx = parser.WithoutFilesystemProjectDiscovery(ctx)
	}
	stats, tombstoned, err := func() (SyncStats, int, error) {
		e.syncMu.Lock()
		defer e.syncMu.Unlock()
		defer e.clearCurrentProgress()
		return e.syncChangedPathsLocked(ctx, paths)
	}()
	if stats.hasSessionChanges() || tombstoned > 0 {
		e.emit("sessions")
	}
	return err
}

// omitMissingPersistentContainerPaths drops missing changed paths that are
// backed by an omnigent chat.db container: the database is a persistent
// archive, so a vanished container (or one of its WAL/SHM siblings) must not
// tombstone the stored virtual members.
func omitMissingPersistentContainerPaths(
	missingPaths []string, files []parser.DiscoveredFile,
) []string {
	return slices.DeleteFunc(missingPaths, func(missingPath string) bool {
		for _, file := range files {
			if file.ProviderSource == nil ||
				!parser.IsOmnigentContainerSource(*file.ProviderSource) {
				continue
			}
			container := providerDiscoveredPath(*file.ProviderSource)
			if filepath.Clean(missingPath) == filepath.Clean(container) ||
				providerVirtualSourceBackedByEvent(container, missingPath) {
				return true
			}
		}
		return false
	})
}

// classifyPaths maps changed file system paths to
// parser.DiscoveredFile structs, filtering out paths that don't
// match known session file patterns.
func (e *Engine) classifyPaths(
	ctx context.Context,
	paths []string,
) ([]parser.DiscoveredFile, error) {
	seen := make(map[string]int, len(paths))
	files := make([]parser.DiscoveredFile, 0, len(paths))
	var classificationErr error
	for _, p := range paths {
		// Codex resolved-index events map to potentially several session
		// sources and must classify even when the event path was deleted, so
		// they are handled by classifyCodexIndexPath. All other changed paths,
		// including Antigravity's sidecar fan-out (annotations, brain,
		// history.jsonl), are owned by each provider-authoritative
		// SourcesForChangedPath via classifyProviderChangedPath.
		dfs := e.classifyCodexIndexPath(p)
		providerFiles, err := e.classifyProviderChangedPath(ctx, p)
		if err != nil {
			classificationErr = errors.Join(
				classificationErr,
				fmt.Errorf("classify changed path %q: %w", p, err),
			)
		}
		dfs = append(dfs, providerFiles...)
		for _, df := range dfs {
			e.invalidateVerifiedDiscoveredSource(df)
			key := string(df.Agent) + "\x00" + df.Path
			if idx, ok := seen[key]; ok {
				files[idx] = mergeChangedPathDiscoveredFile(files[idx], df)
				continue
			}
			seen[key] = len(files)
			files = append(files, df)
		}
	}
	files, err := e.expandClaudeDuplicateCandidates(ctx, files)
	if err != nil {
		classificationErr = errors.Join(classificationErr, err)
	}
	files = dedupeDiscoveredFiles(files)
	return e.dedupeClaudeDiscoveredFiles(files), classificationErr
}

func mergeChangedPathDiscoveredFile(
	current parser.DiscoveredFile,
	next parser.DiscoveredFile,
) parser.DiscoveredFile {
	current.ForceParse = current.ForceParse || next.ForceParse
	current.ForceFullParse = current.ForceFullParse || next.ForceFullParse
	current.ProviderProcess = current.ProviderProcess || next.ProviderProcess
	if current.Project == "" {
		current.Project = next.Project
	}
	if current.Machine == "" {
		current.Machine = next.Machine
	}
	if current.ProviderSource == nil && next.ProviderSource != nil {
		current.ProviderSource = next.ProviderSource
	}
	return current
}

func (e *Engine) classifyProviderChangedPath(
	ctx context.Context,
	path string,
) ([]parser.DiscoveredFile, error) {
	eventKind := providerChangedPathEventKind(path)
	var files []parser.DiscoveredFile
	var classificationErr error
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
			Roots:          roots,
			Machine:        e.machine,
			SourceMachines: e.sourceMachines[agentType],
			PathRewriter:   e.pathRewriter,
		})
		def := provider.Definition()
		watchRoots, err := e.providerChangedPathWatchRoots(
			ctx, agentType, provider, roots,
		)
		if err != nil {
			classificationErr = errors.Join(
				classificationErr,
				fmt.Errorf(
					"%s provider changed-path watch roots for %q: %w",
					def.Type, path, err,
				),
			)
			continue
		}
		// Every SourcesForChangedPath implementation resolves the
		// changed path within the provider's configured roots or plan
		// watch roots (stored-path hints are scoped to the affected
		// container before the query), so an agent whose roots cannot
		// contain the path never claims it. Skip it before the
		// provider-owned stored-hint DB queries, which otherwise run for
		// every registered agent on every watcher event.
		if !changedPathWithinAnyRoot(path, roots) &&
			!changedPathWithinAnyRoot(path, watchRoots) {
			continue
		}
		// Capture the shared container's state before any watermark-only
		// listing below: the provider-side freshness merge may trust the
		// caller's stored authority only while the container provably has
		// not changed across the listing window, and the pass-level capture
		// guard does not exist yet at classification time.
		watermarkContainer := openCodeContainerPathForChangedPathEvent(
			agentType, roots, path,
		)
		var watermarkPreState parser.SQLiteContainerState
		watermarkPreStateOK := false
		if watermarkContainer != "" {
			watermarkPreState, watermarkPreStateOK =
				statSQLiteContainerState(watermarkContainer)
		}
		for _, watchRoot := range watchRoots {
			request := parser.ChangedPathRequest{
				Path:      path,
				EventKind: eventKind,
				WatchRoot: watchRoot,
				// Shared-container providers merge the bounded session-row
				// listing against the paged stored freshness below and emit
				// only members whose watermark advanced, instead of a
				// whole-container child digest scan.
				AllowWatermarkOnlySources: true,
			}
			if watermarkContainer != "" && watermarkPreStateOK &&
				!e.forceParse && e.pathRewriter == nil {
				request.StoredMemberFreshnessPage =
					e.storedMemberFreshnessPager(watermarkContainer)
			}
			if provider.Capabilities().Source.StoredSourceHints == parser.CapabilitySupported {
				if resolver, ok := provider.(parser.StoredSourceHintScopeProvider); ok {
					scopes := storedSourceDBHintScopes(
						resolver.StoredSourceHintScopes(request),
					)
					if len(scopes) > 0 {
						request.StoredSourcePaths, err = e.db.ListStoredSourcePathHintsContext(
							ctx, string(def.Type),
							scopes,
						)
						if err != nil {
							classificationErr = errors.Join(
								classificationErr,
								fmt.Errorf(
									"%s provider changed-path stored hints for %q: %w",
									def.Type, path, err,
								),
							)
							continue
						}
					}
				}
			}
			sources, err := provider.SourcesForChangedPath(
				ctx,
				request,
			)
			if err == nil && request.StoredMemberFreshnessPage != nil {
				if post, ok := statSQLiteContainerState(watermarkContainer); !ok ||
					post != watermarkPreState {
					// The container changed while the merged listing ran: a
					// commit inside that window can advance a session past
					// its listed watermark, so the merge may have dropped a
					// changed member. Re-list without stored authority and
					// let the per-file gates decide.
					request.StoredMemberFreshnessPage = nil
					sources, err = provider.SourcesForChangedPath(ctx, request)
				}
			}
			if err != nil {
				if !errors.Is(err, parser.ErrUnsupportedProviderFeature) {
					classificationErr = errors.Join(
						classificationErr,
						fmt.Errorf(
							"%s provider changed-path classification for %q: %w",
							def.Type, path, err,
						),
					)
				}
				continue
			}
			if def.Type == parser.AgentOmnigent {
				sources, err = e.expandOmnigentInheritedMetadataSources(
					ctx, provider, sources,
				)
				if err != nil {
					classificationErr = errors.Join(
						classificationErr,
						fmt.Errorf(
							"%s provider dependent-source classification for %q: %w",
							def.Type, path, err,
						),
					)
					continue
				}
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
					!parser.IsOmnigentContainerSource(source) &&
					!providerDeletedPhysicalSQLiteSource(agent, sourcePath) &&
					!providerVirtualSourceContainerExists(sourcePath) {
					continue
				}
				seen[key] = struct{}{}
				sourceCopy := source
				discovered := parser.DiscoveredFile{
					Path:            sourcePath,
					Project:         source.ProjectHint,
					Agent:           agent,
					ForceParse:      providerChangedPathForceParse(agent, sourcePath, path, eventKind, mode),
					ProviderSource:  &sourceCopy,
					ProviderProcess: mode == parser.ProviderMigrationProviderAuthoritative,
				}
				if !isS3SourcePath(sourcePath) {
					discovered.Machine = e.machineForProviderSource(
						agent, source, sourcePath,
					)
				}
				// A watcher event names a concrete change even when the
				// session's stat signature cannot see it (a same-size,
				// same-mtime child rewrite), so the storage gate must
				// re-verify this session by content on the next pass.
				if sessionPath := e.openCodeStorageSessionPath(discovered); sessionPath != "" {
					e.invalidateOpenCodeStorageSession(sessionPath)
				}
				files = append(files, discovered)
			}
		}
	}
	return files, classificationErr
}

// expandOmnigentInheritedMetadataSources adds the still-archived descendant
// (subagent) sources of each changed omnigent member so their inherited cwd
// and branch metadata refresh alongside the changed root.
func (e *Engine) expandOmnigentInheritedMetadataSources(
	ctx context.Context,
	provider parser.Provider,
	sources []parser.SourceRef,
) ([]parser.SourceRef, error) {
	reconciler, exact := provider.(parser.ReconciliationSourceResolver)
	if !exact || len(sources) == 0 {
		return sources, nil
	}
	agent := provider.Definition().Type
	seenSources := make(map[string]struct{}, len(sources))
	seenParents := make(map[string]struct{}, len(sources))
	parentIDs := make([]string, 0, len(sources))
	configuredMachines := make(map[string]string, len(sources))
	for _, source := range sources {
		sourcePath := providerDiscoveredPath(source)
		if sourcePath != "" {
			seenSources[sourcePath] = struct{}{}
		}
		rawID, found := parser.OmnigentMemberSessionID(source)
		if !found {
			continue
		}
		parentID := applyIDPrefixToID(e.idPrefix, rawID)
		if _, exists := seenParents[parentID]; exists {
			continue
		}
		seenParents[parentID] = struct{}{}
		parentIDs = append(parentIDs, parentID)
		configuredMachines[parentID] = e.machineForProviderSource(
			agent, source, sourcePath,
		)
	}
	if len(parentIDs) == 0 {
		return sources, nil
	}
	storedMachines, err := e.db.ListSessionMachinesByID(ctx, parentIDs)
	if err != nil {
		return nil, fmt.Errorf("list omnigent parent session machines: %w", err)
	}
	// A descendant is stored under the machine its parent was admitted under,
	// which is not necessarily the local one, so group the lookup by each
	// parent's stored attribution instead of assuming e.machine.
	parentsByMachine := make(map[string][]string, 1)
	machineOrder := make([]string, 0, 1)
	for _, parentID := range parentIDs {
		machine := configuredMachines[parentID]
		if stored, exists := storedMachines[parentID]; exists {
			machine = stored
		}
		if _, exists := parentsByMachine[machine]; !exists {
			machineOrder = append(machineOrder, machine)
		}
		parentsByMachine[machine] = append(parentsByMachine[machine], parentID)
	}
	var paths []string
	for _, machine := range machineOrder {
		machinePaths, err := e.db.ListActiveDescendantSessionSourcePaths(
			ctx, machine, string(agent), parentsByMachine[machine],
		)
		if err != nil {
			return nil, err
		}
		paths = append(paths, machinePaths...)
	}
	for _, storedPath := range paths {
		path := storedPath
		if e.storedPathResolver != nil {
			resolved, ok := e.storedPathResolver(storedPath)
			if !ok {
				return nil, errors.New(
					"resolve omnigent stored descendant source path",
				)
			}
			path = resolved
		}
		if _, exists := seenSources[path]; exists {
			continue
		}
		source, found, err := reconciler.SourceForReconciliation(ctx, path, "")
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		sourcePath := providerDiscoveredPath(source)
		if sourcePath == "" {
			continue
		}
		seenSources[sourcePath] = struct{}{}
		sources = append(sources, source)
	}
	return sources, nil
}

func storedSourceDBHintScopes(
	scopes []parser.StoredSourceHintScope,
) []db.StoredSourcePathHintScope {
	out := make([]db.StoredSourcePathHintScope, 0, len(scopes))
	for _, scope := range scopes {
		out = append(out, db.StoredSourcePathHintScope{
			Path: scope.Path, IncludeVirtualMembers: scope.IncludeVirtualMembers,
		})
	}
	return out
}

func (e *Engine) providerChangedPathWatchRoots(
	ctx context.Context,
	agent parser.AgentType,
	provider parser.Provider,
	roots []string,
) ([]string, error) {
	e.providerWatchRootsMu.Lock()
	defer e.providerWatchRootsMu.Unlock()
	if cached, ok := e.providerWatchRoots[agent]; ok {
		return watchRootPaths(cached), nil
	}

	resolved, err := parser.ResolveWatchRoots(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("resolve %s provider watch roots: %w", agent, err)
	}
	resolved = normalizedProviderWatchRoots(resolved)
	if len(resolved) == 0 {
		resolved = make([]parser.WatchRoot, 0, len(roots))
		for _, root := range roots {
			resolved = append(resolved, parser.WatchRoot{Path: root})
		}
		resolved = normalizedProviderWatchRoots(resolved)
	}
	if e.providerWatchRoots == nil {
		e.providerWatchRoots = make(map[parser.AgentType][]parser.WatchRoot)
	}
	e.providerWatchRoots[agent] = resolved
	return watchRootPaths(resolved), nil
}

func normalizedProviderWatchRoots(
	roots []parser.WatchRoot,
) []parser.WatchRoot {
	normalized := make([]parser.WatchRoot, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		path := filepath.Clean(root.Path)
		if path == "" || path == "." {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		root.Path = path
		root.IncludeGlobs = nil
		root.ExcludeGlobs = nil
		normalized = append(normalized, root)
	}
	return normalized
}

func watchRootPaths(roots []parser.WatchRoot) []string {
	paths := make([]string, 0, len(roots))
	for _, root := range roots {
		paths = append(paths, root.Path)
	}
	return paths
}

func providerChangedPathForceParse(
	agent parser.AgentType,
	sourcePath string,
	eventPath string,
	eventKind string,
	mode parser.ProviderMigrationMode,
) bool {
	if processFileUsesProvider(agent) {
		// Goose needs no force here despite its one-second timestamps: it
		// declares FingerprintHashRequiredForFreshness, so same-mtime edits
		// are caught by the content-hash gate, and a cold-tracker fan-out
		// must not rewrite every unchanged archived session.
		return eventKind == "remove" &&
			providerDeletedPhysicalSQLiteSource(agent, sourcePath)
	}
	if mode != parser.ProviderMigrationProviderAuthoritative {
		return true
	}
	// Codebuff changed-path events must always force a fingerprint
	// comparison. The composite stat-only freshness gate may skip
	// same-size, same-mtime rewrites, so a concrete changed-path
	// signal (including direct chat-messages.json events) must always
	// trigger the full fingerprint path.
	if agent == parser.AgentCodebuff {
		return true
	}
	if filepath.Clean(sourcePath) != filepath.Clean(eventPath) &&
		!providerVirtualSourceBackedByEvent(sourcePath, eventPath) {
		// OpenCode-family storage sessions resolve message/part events
		// to their session JSON, whose fingerprint and stat signature
		// span those same child files, and the classifier invalidates
		// the session's storage-gate trust for every event. The normal
		// freshness path therefore re-verifies by content, while a
		// forced parse would bypass dropUnchangedSharedSQLiteResults
		// and rewrite the whole session on every streamed append.
		// Remove events keep the force so a deleted child still
		// re-emits through the deletion path.
		if eventKind != "remove" &&
			isOpenCodeFormatStorageAgent(agent) &&
			isOpenCodeFormatStoragePath(agent, sourcePath) {
			return false
		}
		// Gemini project-metadata events fan out to every session under
		// the root. Each session's fingerprint hashes its resolved
		// project metadata, so the hash-aware freshness check re-parses
		// only sessions whose resolved project changed; a forced parse
		// would rewrite the whole archive on every metadata edit. Remove
		// events keep the force so deletion still re-emits.
		if eventKind != "remove" &&
			agent == parser.AgentGemini &&
			parser.IsGeminiProjectMetadataFile(eventPath) {
			return false
		}
		return true
	}
	return eventKind == "remove" &&
		providerDeletedPhysicalSQLiteSource(agent, sourcePath)
}

func providerVirtualSourceBackedByEvent(sourcePath, eventPath string) bool {
	sourcePath = filepath.Clean(sourcePath)
	dbPath := sourcePath
	if idx := strings.LastIndex(sourcePath, "#"); idx >= 0 {
		dbPath = filepath.Clean(sourcePath[:idx])
	}
	eventPath = filepath.Clean(eventPath)
	// The workspace.json branch is keyed on the VS Code style state store
	// basename, which Windsurf and Trae both use, so it covers every
	// provider whose container is a "state.vscdb" sibling of the workspace
	// label file rather than Windsurf alone.
	return eventPath == dbPath ||
		eventPath == dbPath+"-wal" ||
		eventPath == dbPath+"-shm" ||
		(filepath.Base(dbPath) == parser.WindsurfStateDBName &&
			eventPath == filepath.Join(filepath.Dir(dbPath), "workspace.json"))
}

func providerChangedPathEventKind(path string) string {
	if path == "" {
		return ""
	}
	if _, err := os.Lstat(path); err == nil {
		return "write"
	} else if !os.IsNotExist(err) {
		return "write"
	}
	return "remove"
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

func providerVirtualSourceContainerExists(path string) bool {
	container := validatedProviderSourceStatPath(path)
	return container != path && parser.IsRegularFile(container)
}

func providerDeletedPhysicalSQLiteSource(
	agent parser.AgentType, path string,
) bool {
	switch agent {
	case parser.AgentZed:
		return filepath.Base(path) == "threads.db"
	case parser.AgentZCode:
		return filepath.Base(path) == parser.ZCodeDBName
	case parser.AgentGoose:
		return filepath.Base(path) == parser.GooseDBName
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
	if isCodexFormatAgent(file.Agent) {
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
	if candidate.Agent == current.Agent && isCodexFormatAgent(candidate.Agent) {
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
	if candidate.Agent == current.Agent && isCodexFormatAgent(candidate.Agent) {
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
	ctx context.Context,
	files []parser.DiscoveredFile,
) ([]parser.DiscoveredFile, error) {
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
		return files, nil
	}

	listSessionFiles := e.claudeProjectSessionFiles
	if listSessionFiles == nil {
		listSessionFiles = parser.ClaudeProjectSessionFiles
	}
	out := files
	for _, claudeDir := range e.agentDirs[parser.AgentClaude] {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for _, candidate := range listSessionFiles(claudeDir) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
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
	return out, nil
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

// closeWriterForResyncBarrier indirects db.CloseWriter so tests can inject a
// barrier-establishment failure.
var closeWriterForResyncBarrier = func(d *db.DB) error { return d.CloseWriter() }

// ResyncAll builds a fresh database from scratch, syncs all
// sessions into it, copies insights from the old DB, then
// atomically swaps the files and reopens the original DB
// handle. This avoids the per-row trigger overhead of bulk
// deleting hundreds of thousands of messages in place.
// shouldAbortResyncSwap decides whether a finished resync pass built a
// database that would be worse than the original, so the swap must be
// abandoned:
//   - sync was cancelled (partial rebuild)
//   - nothing synced at all (empty discovery, or all skipped)
//     when old DB had data
//   - more files failed than succeeded (permission errors,
//     disk issues)
//
// OpenCode-only rebuilds are allowed to finish with 0 freshly synced
// sessions when every storage parse was intentionally preserved
// against the archive; orphan copy restores those rows immediately
// after the sync pass. A few permanent parse failures are tolerated
// since those files were broken in the old DB too.
// OpenCode-format storage is a self-preserving container store that
// flows through file discovery, so it is excluded from the discovery
// check just as it is subtracted from oldFileSessions by the caller.
// Otherwise its discovery would mask the disappearance of plain
// file-backed sessions whose directories went empty.
func shouldAbortResyncSwap(
	stats SyncStats, oldFileSessions, trashedCopied int,
) bool {
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
	// A zero-write run is intentional when the sync_include_cwd_prefixes
	// allow-list vetoed sessions AND every OK file is accounted for as
	// either fully filtered or parser-excluded: the swap proceeds and
	// the orphan copy restores the archived rows, because the filter
	// gates ingestion only. Requiring the full accounting keeps the
	// guard armed for mixed runs where other files produced nothing for
	// an unexplained reason.
	cwdFilteredOnly := stats.Synced == 0 &&
		stats.TotalSessions > 0 &&
		stats.Failed == 0 &&
		stats.cwdFilteredSessions > 0 &&
		stats.filesOK == stats.cwdFilteredFiles+stats.parserExcludedFiles
	return stats.Aborted ||
		emptyDiscovery ||
		(stats.Synced == 0 &&
			stats.TotalSessions > 0 &&
			!preservedOnly &&
			!excludedOnly &&
			!cwdFilteredOnly) ||
		(stats.Failed > 0 && stats.Failed > stats.filesOK)
}

func (e *Engine) ResyncAll(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	if e.refuseWriteInForceParse("ResyncAll") {
		return SyncStats{}
	}
	e.syncMu.Lock()
	defer e.notifyStartupReconciled()
	// Defers LIFO: Unlock runs before emit.
	defer func() {
		if stats.shouldEmitSync() {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	defer func() { e.recordStartupReconciled(ctx, stats, nil) }()

	return e.resyncAllLocked(ctx, onProgress)
}

func (e *Engine) resyncAllLocked(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	stats, _ = e.resyncAllWithOptionsLocked(
		ctx, onProgress, RebuildOptions{}, productionRebuildOperations,
	)
	// Preserve the legacy result shape; phase diagnostics are part of the
	// options entrypoint's observable contract only.
	stats.RebuildPhases = nil
	e.mu.Lock()
	e.lastSyncStats = stats
	e.mu.Unlock()
	return stats
}

// ResyncAllWithOptions atomically rebuilds the archive from the local sources
// plus each configured contributor.
func (e *Engine) ResyncAllWithOptions(
	ctx context.Context, onProgress ProgressFunc, opts RebuildOptions,
) (stats SyncStats, err error) {
	return e.resyncAllWithOptionsAndOperations(
		ctx, onProgress, opts, productionRebuildOperations,
	)
}

func (e *Engine) resyncAllWithOptionsAndOperations(
	ctx context.Context, onProgress ProgressFunc, opts RebuildOptions,
	ops rebuildOperations,
) (stats SyncStats, err error) {
	if e.refuseWriteInForceParse("ResyncAllWithOptions") {
		return SyncStats{}, nil
	}
	e.syncMu.Lock()
	defer e.notifyStartupReconciled()
	defer func() {
		if stats.shouldEmitSync() {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	defer func() { e.recordStartupReconciled(ctx, stats, err) }()
	opts.includePhaseDiagnostics = true
	return e.resyncAllWithOptionsLocked(
		ctx, onProgress, opts, ops,
	)
}

// resyncAllWithOptionsLocked rebuilds the archive in place: it builds a fresh
// replacement at the temp path, swaps it into the live file, then re-baselines
// the caches that referenced the old database. The caller holds syncMu. The
// extracted methods (ResyncBuild, SwapResyncDatabase, ResetCachesAfterSwap) let
// the daemon run the heavy build in a worker process behind a write barrier and
// perform only the swap tail itself.
func (e *Engine) resyncAllWithOptionsLocked(
	ctx context.Context, onProgress ProgressFunc, opts RebuildOptions,
	ops rebuildOperations,
) (stats SyncStats, retErr error) {
	ops = ops.withDefaults()

	// Hold the write barrier for the whole in-process build-and-swap window. The
	// build reads the original while it stays open, and the swap's
	// CloseConnections runs only after the copies, so without the barrier a
	// direct write (star/delete/restore) that bypasses syncMu could land in the
	// original between the copies and the rename and be discarded by the swap.
	// The barrier rejects such writes with ErrWriterClosed instead. The worker
	// arm holds this barrier at the daemon layer (runWorkerResyncBuild) and calls
	// ResyncBuild directly, so it never reaches this method; this arm covers the
	// in-process fallback, CLI --full, worker startup, and unified rebuilds. Only
	// engage it when this call owns the barrier, so an outer owner is not
	// double-closed or prematurely reopened.
	ownedBarrier := false
	swapStageReached := false
	defer func() {
		// The successful swap's Reopen already restored the writer and cleared
		// the barrier; recover here only when we still own a closed writer
		// (barrier-close failure, build abort, or a swap whose own recovery
		// failed). A failure before the swap stage never touched the reader
		// pool, so ReopenWriter alone restores service; a post-CloseConnections
		// failure may have closed the reader pool too, so only a full Reopen
		// restores reads.
		if !ownedBarrier || !e.db.WriterClosed() {
			return
		}
		if swapStageReached {
			if rerr := e.db.Reopen(); rerr != nil {
				log.Printf("resync: recovery reopen after barrier: %v", rerr)
				retErr = errors.Join(
					retErr, fmt.Errorf("recovery reopen: %w", rerr),
				)
			}
			return
		}
		if rerr := e.db.ReopenWriter(); rerr != nil {
			log.Printf("resync: reopen writer after barrier: %v", rerr)
			retErr = errors.Join(
				retErr, fmt.Errorf("reopen writer after barrier: %w", rerr),
			)
		}
	}()
	if !e.db.WriterClosed() {
		// Own the barrier before attempting the close: CloseWriter's failure
		// posture still leaves the writer closed (undrained pool retained
		// inside db), and the deferred recovery above must reopen it so the
		// daemon does not fail every write until restart. Reopening is safe —
		// this in-process arm never releases write ownership.
		ownedBarrier = true
		if cerr := closeWriterForResyncBarrier(e.db); cerr != nil {
			// Building on a half-established barrier would swap while the
			// retained connection could still write to the original. Abort
			// before building and let the deferred reopen restore service.
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings, fmt.Sprintf(
				"resync aborted: close writer for barrier: %v", cerr,
			))
			e.setLastSyncStats(stats)
			return stats, fmt.Errorf(
				"resync: close writer for barrier: %w", cerr,
			)
		}
	}

	// Snapshot the pre-build skip state. A successful build leaves the
	// in-memory skip cache holding the replacement's fresh entries (persisted
	// into the replacement, not the original), so a swap failure that discards
	// the replacement must restore this snapshot: replacement-only entries
	// would otherwise suppress syncing sources whose data never reached the
	// still-live original archive.
	e.skipMu.Lock()
	preBuildSkipCache := e.skipCache
	preBuildSkipHashKeys := e.skipHashKeys
	e.skipMu.Unlock()

	stats, err := e.resyncBuildLocked(ctx, onProgress, opts, ops, ownedBarrier)
	if err != nil || stats.Aborted {
		return stats, err
	}
	swapStageReached = true
	installed, swapErr := e.swapResyncDatabaseLocked(
		ctx, onProgress, e.ResyncTempPath(), ops, &stats,
	)
	if swapErr != nil {
		if !installed {
			// The original archive is still in place; drop the discarded
			// replacement's skip entries and its tombstone count. A
			// post-install failure keeps both: the replacement is the live
			// archive there.
			e.skipMu.Lock()
			e.skipCache = preBuildSkipCache
			e.skipHashKeys = preBuildSkipHashKeys
			e.skipMu.Unlock()
			stats.Tombstoned = 0
		}
		e.setLastSyncStats(stats)
		return stats, swapErr
	}
	if err := e.ResetCachesAfterSwap(); err != nil {
		log.Printf("resync: reset caches after swap: %v", err)
	}
	e.setLastSyncStats(stats)
	return stats, nil
}

// resyncBuildLocked builds a complete replacement archive at the temp path from
// the current sources plus any contributors, copies preserved state (orphans,
// insights, recall, user metadata) from the original, persists the fresh skip
// cache into the replacement, and closes it. It reads the original but never
// closes or swaps it; the caller performs the swap. The caller holds syncMu and
// guarantees the original receives no writes for the duration.
func (e *Engine) resyncBuildLocked(
	ctx context.Context, onProgress ProgressFunc, opts RebuildOptions,
	ops rebuildOperations, restoreActiveWriterOnAbort bool,
) (stats SyncStats, retErr error) {
	// Rebuild tombstones are committed only inside the replacement, and every
	// aborted or failed build discards it. Hold them here and publish the
	// count only on the successful return, so no failure branch stores or
	// returns removals that never reached the archive.
	pendingTombstoned := 0
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

	// Resync rebuilds the archive from scratch, so every shared-SQLite
	// container and storage session must be re-verified against the
	// fresh database.
	e.clearTrustedSQLiteContainers()
	e.clearTrustedOpenCodeStorageSessions()
	e.clearVerifiedSources()

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
	oldFileSessions, err := e.protectedFileSessionCount(origDB, "", "", false)
	if err != nil {
		log.Printf("resync: get old file count: %v", err)
		oldFileSessions = 1
	}
	localOldFileSessions := oldFileSessions
	rebuildOldFileSessions := oldFileSessions
	contributorOldFileSessions := make([]int, len(opts.Contributors))
	if len(opts.Contributors) > 0 ||
		len(opts.UnavailableContributorIDPrefixes) > 0 {
		contributorPrefixes := slices.Clone(
			opts.UnavailableContributorIDPrefixes,
		)
		contributorPrefixes = slices.Grow(
			contributorPrefixes, len(opts.Contributors),
		)
		for _, contributor := range opts.Contributors {
			if contributor.Config.IDPrefix != "" {
				contributorPrefixes = append(
					contributorPrefixes, contributor.Config.IDPrefix,
				)
			}
		}
		excludedAgents := []string{
			string(parser.AgentOpenCode),
			string(parser.AgentKilo),
			string(parser.AgentMiMoCode),
			string(parser.AgentIcodemate),
		}
		for _, agent := range storageAgentsForDisabledProviders(e.preserveAgents) {
			excludedAgents = append(excludedAgents, string(agent))
		}
		localOldFileSessions, err = origDB.FileBackedSessionCountForRebuildOwner(
			context.Background(), e.machine, contributorPrefixes, excludedAgents,
		)
		if err != nil {
			log.Printf("resync: get old local rebuild file count: %v", err)
			localOldFileSessions = 1
		}
		for i, contributor := range opts.Contributors {
			count, countErr := protectedFileSessionCount(
				origDB, contributor.Config.Machine,
				contributor.Config.IDPrefix,
				contributor.Config.Machine != "",
				contributor.Config.DisabledAgents,
			)
			if countErr != nil {
				log.Printf(
					"resync: get old contributor %q file count: %v",
					contributor.Name, countErr,
				)
				count = 1
			}
			contributorOldFileSessions[i] = count
		}
		rebuildOldFileSessions = localOldFileSessions
		for _, count := range contributorOldFileSessions {
			rebuildOldFileSessions += count
		}
	}

	// Clean up stale temp DB from a prior crash.
	removeTempDB(tempPath)

	// 1. Snapshot and clear in-memory skip cache. The
	// snapshot is restored on early failure so behavior
	// matches the persisted DB until the next restart.
	e.skipMu.Lock()
	savedSkipCache := e.skipCache
	savedSkipHashKeys := e.skipHashKeys
	e.skipCache = make(map[string]int64)
	e.skipHashKeys = make(map[string]string)
	e.skipMu.Unlock()

	restoreSkipCache := func() {
		e.skipMu.Lock()
		e.skipCache = savedSkipCache
		e.skipHashKeys = savedSkipHashKeys
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
		return stats, err
	}
	if err := newDB.CopyArchiveIdentityFrom(origPath); err != nil {
		log.Printf("resync: preserve archive identity: %v", err)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats = SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: preserve archive identity: " + err.Error(),
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
			return stats, err
		}
		ftsDropped = true
		log.Printf(
			"resync: drop temp fts: %s",
			time.Since(tFTS).Round(time.Millisecond),
		)
	}

	// Same trade as FTS: the usage and activity message indexes are
	// pure derived state, so skip their per-row maintenance during the
	// bulk load and build each B-tree once before the swap. Read-only
	// opens require these indexes, so the rebuild must succeed before
	// the replacement is installed.
	if err := newDB.DropUsageMessageIndexes(); err != nil {
		log.Printf("resync: drop temp usage indexes: %v", err)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats = SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: drop temp usage indexes: " +
					err.Error(),
			},
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}

	// 3. Point engine at newDB and sync into it. Report discovery as its
	// own phase first: syncAllLocked walks every source before emitting
	// its first syncing event, and on a large archive that walk takes
	// minutes. Without this marker the progress printer credits that
	// silent time to the preceding (instant) "Disabling ..." phase.
	// The archive is write-barriered for the whole rebuild, so one snapshot of
	// its stale Claude fork rows serves every parsed file. Querying the archive
	// per file would open cold reader connections while workers are busy,
	// which SQLite's busy handler turns into sleeps on every open.
	archiveStaleForks, err := loadArchiveStaleClaudeForkIndex(origDB)
	if err != nil {
		log.Printf("resync: snapshot stale claude forks: %v", err)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		stats = SyncStats{
			Aborted: true,
			Warnings: []string{
				"resync failed: snapshot stale claude forks: " + err.Error(),
			},
		}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	e.archiveStore = origDB
	e.archiveStaleClaudeForks = archiveStaleForks
	deferredSourceCwd := newSourceCwdReconciliationBatch()
	e.deferredSourceCwd = deferredSourceCwd
	defer func() { e.deferredSourceCwd = nil }()
	e.db = newDB
	reportResyncPhase(
		PhaseDiscovering,
		"Discovering sessions",
		"",
	)
	stats = e.syncAllLocked(
		ctx, reportResyncProgress, time.Time{}, nil, syncWriteBulk, true, false,
	)
	e.db = origDB // restore immediately
	e.archiveStore = nil
	e.archiveStaleClaudeForks = nil
	pendingTombstoned += stats.Tombstoned
	stats.Tombstoned = 0
	e.phaseStats.Log("resync")
	if opts.includePhaseDiagnostics {
		stats.RebuildPhases = append(stats.RebuildPhases,
			phaseSnapshot("local", &e.phaseStats))
	}
	localStats := stats
	if stats.Aborted || ctx.Err() != nil {
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
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		return stats, nil
	}

	for contributorIndex, contributor := range opts.Contributors {
		if contributor.Started != nil {
			contributor.Started()
		}
		contributorEngine := NewEngine(newDB, contributor.Config)
		contributorEngine.archiveStore = origDB
		contributorEngine.archiveStaleClaudeForks = archiveStaleForks
		contributorEngine.deferredSourceCwd = deferredSourceCwd
		contributorEngine.forceFullParse = contributor.ForceParse ||
			contributor.ForceFullParseAfterCache
		contributorEngine.forceFullParseAllowsCache =
			contributor.ForceFullParseAfterCache && !contributor.ForceParse
		contributorProgress := func(p Progress) {
			if contributor.Progress != nil {
				p = contributor.Progress(p)
			}
			p.SessionsDone += stats.TotalSessions
			p.SessionsTotal += stats.TotalSessions
			p.MessagesIndexed += stats.messagesIndexed
			reportResyncProgress(p)
		}
		contributorStats := contributorEngine.syncAllLocked(
			ctx, contributorProgress, time.Time{}, nil,
			syncWriteBulk, true, contributor.ForceParse,
		)
		contributorEngine.phaseStats.Log("resync contributor " + contributor.Name)
		phase := phaseSnapshot(contributor.Name, &contributorEngine.phaseStats)
		contributorSafetyAbort := shouldAbortResyncSwap(
			contributorStats,
			contributorOldFileSessions[contributorIndex],
			0,
		)
		if contributorSafetyAbort {
			contributorStats.Aborted = true
		}
		mergeSyncStats(&stats, contributorStats)
		pendingTombstoned += stats.Tombstoned
		stats.Tombstoned = 0
		if opts.includePhaseDiagnostics {
			stats.RebuildPhases = append(stats.RebuildPhases, phase)
		}
		runAfterFailure := func() error {
			if restoreActiveWriterOnAbort && origDB.WriterClosed() {
				if err := origDB.ReopenWriter(); err != nil {
					return fmt.Errorf(
						"reopen active writer for contributor failure: %w", err,
					)
				}
			}
			if contributor.AfterFailure != nil {
				return contributor.AfterFailure(contributorEngine, origDB)
			}
			return nil
		}
		if contributorStats.Aborted || stats.Aborted || ctx.Err() != nil ||
			contributorSafetyAbort {
			failureHookErr := runAfterFailure()
			contributorEngine.Close()
			if contributor.Finished != nil {
				contributor.Finished(
					contributorStats, errors.Join(ctx.Err(), failureHookErr),
				)
			}
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings, fmt.Sprintf(
				"resync aborted: contributor %q did not complete",
				contributor.Name,
			))
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			if failureHookErr != nil {
				return stats, &RebuildContributorError{
					Contributor: contributor.Name,
					Err:         failureHookErr,
				}
			}
			return stats, nil
		}
		if contributor.AfterSync != nil {
			if err := contributor.AfterSync(contributorEngine, newDB); err != nil {
				failureErr := errors.Join(err, runAfterFailure())
				contributorEngine.Close()
				if contributor.Finished != nil {
					contributor.Finished(contributorStats, failureErr)
				}
				newDB.Close()
				removeTempDB(tempPath)
				restoreSkipCache()
				stats.Aborted = true
				stats.Warnings = append(stats.Warnings, fmt.Sprintf(
					"resync contributor %q failed: %v",
					contributor.Name, err,
				))
				e.mu.Lock()
				e.lastSyncStats = stats
				e.mu.Unlock()
				return stats, &RebuildContributorError{
					Contributor: contributor.Name,
					Err:         failureErr,
				}
			}
		}
		contributorEngine.Close()
		if contributor.Finished != nil {
			contributor.Finished(contributorStats, nil)
		}
	}

	localSafetyAbort := false
	if len(opts.Contributors) > 0 ||
		len(opts.UnavailableContributorIDPrefixes) > 0 {
		// Evaluate local safety after contributors so a contributor's own
		// cancellation or failure remains the reported abort reason. Trash
		// copied from the old archive cannot make an empty local pass safe.
		localSafetyAbort = shouldAbortResyncSwap(
			localStats, localOldFileSessions, 0,
		)
	}
	abortSwap := localSafetyAbort ||
		shouldAbortResyncSwap(stats, rebuildOldFileSessions, trashedCopied)
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
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		return stats, nil
	}

	// Copy preserved state from the original into the replacement. The caller
	// guarantees the original is quiesced: the in-process swap owns the final
	// CloseConnections, and the worker path runs behind the daemon's write
	// barrier. These ATTACH reads therefore see a consistent committed snapshot
	// without the build closing the original here.
	//
	// Re-copy excluded session IDs to catch permanent deletes recorded during
	// the sync window, and purge any sessions synced into newDB before the
	// exclusion was recorded.
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
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
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
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	log.Printf(
		"resync: copy insights: %s",
		time.Since(tInsights).Round(time.Millisecond),
	)

	// Copy model pricing so usage costs survive the swap. The
	// startup seed only runs once per daemon lifetime, so a
	// resync triggered through the sync API would otherwise
	// leave the rebuilt DB with an empty pricing table and
	// every usage cost reading $0.00 until the next restart.
	// Non-fatal: a failed copy degrades cost display but does
	// not justify aborting the resync, and the next daemon
	// startup re-seeds pricing.
	if err := newDB.CopyModelPricingFrom(origPath); err != nil {
		log.Printf("resync: copy model pricing: %v", err)
		stats.Warnings = append(stats.Warnings,
			"model pricing copy failed; usage costs show as $0.00 "+
				"until the next daemon restart re-seeds pricing: "+
				err.Error(),
		)
	}

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
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	stats.OrphanedCopied = orphaned
	deferredCwdUpdated, err := e.applyDeferredSourceCwd(
		newDB, deferredSourceCwd,
	)
	if err != nil {
		log.Printf("resync: apply deferred source cwd: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"deferred source cwd reconciliation failed, aborting swap: "+
				err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	stats.RecordCwdUpdated(deferredCwdUpdated)
	e.deferredSourceCwd = nil

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

	// CopySyncStateFrom runs after the fresh archive's normal linking pass so
	// pending hierarchy work from the original must be consumed explicitly.
	// Wait until orphan restoration is complete so every queued session and
	// copied spawn edge is present. A failed repair leaves hierarchy state
	// uncertain and must abort before the replacement can be installed.
	if err := newDB.RepairQueuedSubagentParents(); err != nil {
		log.Printf("resync: repair copied subagent parents: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"hierarchy repair failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}

	// Copy recall entries and their evidence from the quiesced old DB.
	// The fresh DB is built from source files, which never contain
	// recall entries, so without this every accepted entry is lost on
	// resync. Runs after the orphan copy so referenced sessions exist.
	// Failure aborts the swap to avoid destroying the recall archive.
	if err := newDB.CopyRecallEntriesFrom(origPath); err != nil {
		log.Printf("resync: copy recall entries: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"recall copy failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}

	// Merge user-managed data and trustworthy immutable project-identity
	// snapshots from the old DB. Snapshot copy happens after parsing because the
	// destination rows reference freshly parsed sessions. Pre-source-snapshot
	// archives retain the fresh parse results instead. Failure must abort the
	// swap: a fresh database without valid snapshots could no longer export
	// stable identity after a source working directory disappears.
	reportResyncPhase(
		PhaseCopyingMetadata,
		"Copying user-managed session metadata",
		"",
	)
	if err := newDB.CopySessionMetadataFrom(origPath); err != nil {
		log.Printf("resync: copy session metadata: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"session metadata copy failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	if _, err := newDB.RestoreSessionProjectsFromIdentitySnapshots(
		context.Background(),
	); err != nil {
		log.Printf("resync: restore session project identity: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"session project identity restore failed, aborting swap: "+err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	mappingMachines, err := ops.listActiveWorktreeMappingMachines(ctx, newDB)
	if err != nil {
		warning := "worktree mapping machine discovery failed, aborting swap: " +
			err.Error()
		log.Printf("resync: %s", warning)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings, warning)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, fmt.Errorf(
			"discovering active worktree mapping machines: %w", err,
		)
	}
	for _, machine := range mappingMachines {
		if _, applyErr := ops.applyWorktreeMappings(
			ctx, newDB, machine,
		); applyErr != nil {
			warning := fmt.Sprintf(
				"worktree mapping apply failed for machine %q, aborting swap: %v",
				machine, applyErr,
			)
			log.Printf("resync: %s", warning)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings, warning)
			newDB.Close()
			removeTempDB(tempPath)
			restoreSkipCache()
			e.mu.Lock()
			e.lastSyncStats = stats
			e.mu.Unlock()
			return stats, fmt.Errorf(
				"applying worktree mappings for machine %q: %w",
				machine, applyErr,
			)
		}
	}

	// Metadata restoration deliberately copies user-owned deletion state from
	// the original archive. Reconcile archive-only Claude members afterwards so
	// an active legacy fork cannot overwrite the source_missing tombstone that
	// this rebuild just established.
	deferredTombstoned, err := e.reconcileCopiedSourceMissingMembers(
		ctx, newDB, origPath, stats.sourceMissingArchiveMembers,
	)
	if err != nil {
		log.Printf("resync: tombstone copied source-missing sessions: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"copied source-missing reconciliation failed, aborting swap: "+
				err.Error(),
		)
		newDB.Close()
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	pendingTombstoned += deferredTombstoned

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
		if err := ops.rebuildFTS(newDB); err != nil {
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
			return stats, err
		}
		log.Printf(
			"resync: rebuild fts: %s",
			time.Since(tFTS).Round(time.Millisecond),
		)
	}

	tUsageIndexes := time.Now()
	reportResyncPhase(
		PhaseRebuildingSearch,
		"Rebuilding usage indexes",
		"",
	)
	if err := ops.rebuildUsageIndexes(newDB); err != nil {
		log.Printf("resync: rebuild usage indexes: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"usage index rebuild failed, aborting swap: "+
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
		return stats, err
	}
	log.Printf(
		"resync: rebuild usage indexes: %s",
		time.Since(tUsageIndexes).Round(time.Millisecond),
	)

	// Persist the fresh skip state into the replacement so the post-swap engine
	// loads warm state: this engine after an in-process swap, or the daemon
	// after a worker build. Then close the replacement so its file is complete
	// on disk for the caller's rename. A failed close means a connection still
	// holds the replacement and committed rows may sit uncheckpointed in its
	// WAL; the swap renames only the main file and deletes that WAL, so
	// proceeding would install an archive missing those rows. Abort instead.
	e.persistSkipCacheInto(newDB)
	if err := newDB.Close(); err != nil {
		log.Printf("resync: close replacement db: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"replacement close failed, aborting swap: "+err.Error(),
		)
		removeTempDB(tempPath)
		restoreSkipCache()
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	stats.Tombstoned = pendingTombstoned
	return stats, nil
}

// swapResyncDatabaseLocked installs a built replacement archive at tempPath over
// the original: it closes the original's connections (quiescing and
// checkpointing it), removes the WAL sidecars, renames the replacement into
// place, reopens the active handle, marks data current, and checkpoints. Every
// failure appends a warning to stats and marks it aborted; recoverable failures
// reopen the original so it keeps serving. The installed result reports whether
// the rename replaced the original, so callers can tell pre-install failures
// (original still live) from post-install ones. The caller holds syncMu.
func (e *Engine) swapResyncDatabaseLocked(
	ctx context.Context, onProgress ProgressFunc, tempPath string,
	ops rebuildOperations, stats *SyncStats,
) (installed bool, err error) {
	ops = ops.withDefaults()
	origDB := e.db
	origPath := origDB.Path()
	reportResyncPhase := func(phase Phase, detail string) {
		e.reportProgress(onProgress, Progress{
			Phase: phase, Detail: detail, Resync: true,
		})
	}

	// Close the original to quiesce writes and checkpoint its WAL before the
	// rename. The worker path already holds the daemon's write barrier; the
	// in-process path relies on this close for the same guarantee.
	reportResyncPhase(PhaseCopyingMetadata, "Closing current database before swap")
	if err := origDB.CloseConnections(); err != nil {
		log.Printf("resync: close orig db: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"close before swap failed: "+err.Error(),
		)
		removeTempDB(tempPath)
		// Connections may be partially closed; reopen to restore service.
		if rerr := origDB.Reopen(); rerr != nil {
			log.Printf("resync: recovery reopen: %v", rerr)
			return false, errors.Join(err, fmt.Errorf("recovery reopen: %w", rerr))
		}
		return false, err
	}

	reportResyncPhase(PhaseSwappingDatabase, "Swapping rebuilt database into place")
	// CloseConnections checkpointed the original (including the
	// barrier-closed-writer case), so its remaining sidecars are stale files
	// whose removal cannot discard data, and a failed rename below reopens a
	// complete original.
	removeWAL(origPath)

	if err := os.Rename(tempPath, origPath); err != nil {
		log.Printf("resync: rename temp db: %v", err)
		stats.Aborted = true
		stats.Warnings = append(stats.Warnings,
			"resync swap failed: "+err.Error(),
		)
		removeTempDB(tempPath)
		// Restore service even on rename failure.
		if rerr := origDB.Reopen(); rerr != nil {
			log.Printf("resync: recovery reopen: %v", rerr)
			return false, errors.Join(err, fmt.Errorf("recovery reopen: %w", rerr))
		}
		return false, err
	}
	removeWAL(tempPath)

	if err := ops.reopen(origDB); err != nil {
		log.Printf("resync: reopen db: %v", err)
		// The replacement is already renamed into place; a failed reopen
		// leaves both pools closed and every read failing until restart, so
		// retry before surfacing the failure.
		if rerr := ops.reopen(origDB); rerr != nil {
			log.Printf("resync: recovery reopen: %v", rerr)
			stats.Aborted = true
			stats.Warnings = append(stats.Warnings,
				"resync swap completed but reopening active database failed: "+
					err.Error(),
			)
			return true, errors.Join(err, fmt.Errorf("recovery reopen: %w", rerr))
		}
		log.Printf("resync: reopen recovered on retry")
		stats.Warnings = append(stats.Warnings,
			"resync reopen required a retry: "+err.Error(),
		)
	}
	origDB.MarkDataCurrent()
	if err := origDB.CheckpointWALTruncateWithRetry(ctx); err != nil {
		if errors.Is(err, db.ErrWALCheckpointBusy) {
			log.Printf("resync: wal checkpoint busy")
		} else {
			log.Printf("resync: wal checkpoint: %v", err)
		}
	}
	stats.ArchiveRebuilt = true
	return true, nil
}

// ResyncTempPath returns the path where a resync build stages its replacement
// archive: the active database path with the resync temp suffix appended.
func (e *Engine) ResyncTempPath() string {
	return e.db.Path() + resyncTempSuffix
}

// ResyncBuild builds a complete replacement archive at ResyncTempPath (including
// orphan and metadata copy phases and skip-state persistence) using read-only
// access to the original archive. The caller must guarantee the original
// receives no writes while it runs; the daemon holds the write barrier and the
// worker opens the original read-only. It does not swap or reopen anything.
func (e *Engine) ResyncBuild(
	ctx context.Context, onProgress ProgressFunc,
) (string, SyncStats, error) {
	if e.refuseWriteInForceParse("ResyncBuild") {
		return "", SyncStats{}, nil
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	stats, err := e.resyncBuildLocked(
		ctx, onProgress, RebuildOptions{}, productionRebuildOperations, false,
	)
	return e.ResyncTempPath(), stats, err
}

// SwapResyncDatabase installs a replacement archive built at tempPath: it closes
// connections, removes WAL sidecars, renames, reopens, marks data current, and
// checkpoints. It is the daemon's swap tail after a worker build. The caller
// serializes it against sync (the daemon holds the write barrier). The
// installed result tells a failing caller whether the rename replaced the
// original, so pre-install failures can discard the build's tombstone counts
// while post-install failures keep them.
func (e *Engine) SwapResyncDatabase(
	tempPath string,
) (installed bool, err error) {
	var stats SyncStats
	return e.swapResyncDatabaseLocked(
		context.Background(), nil, tempPath, productionRebuildOperations, &stats,
	)
}

// ResetCachesAfterSwap re-baselines every parent-side cache keyed to the
// replaced database: it clears the trusted SQLite containers, trusted OpenCode
// storage sessions, and verified sources, then reloads the skip cache from the
// swapped archive so the engine carries warm skip state. skipFingerprints is
// reset to empty, matching engine construction (fingerprints are recomputed on
// the next sync, never persisted). The caller invokes it immediately after a
// successful SwapResyncDatabase.
func (e *Engine) ResetCachesAfterSwap() error {
	e.clearTrustedSQLiteContainers()
	e.clearTrustedOpenCodeStorageSessions()
	e.clearVerifiedSources()
	return e.ReloadSkipCache()
}

// ReloadSkipCache replaces the in-memory skip state with the durable archive
// state. Worker-backed passes call it after reacquiring write ownership because
// the child may have removed cache entries while tombstoning missing sources.
// The caller must serialize this with other engine sync work.
func (e *Engine) ReloadSkipCache() error {
	skipCache := make(map[string]int64)
	if !e.ephemeral {
		loaded, err := e.db.LoadSkippedFiles()
		if err != nil {
			return fmt.Errorf("reloading skip cache after swap: %w", err)
		}
		skipCache = loaded
	}
	skipHashKeys, _ := normalizeSourceHashSkipCache(skipCache, nil)

	e.skipMu.Lock()
	e.skipCache = skipCache
	e.skipHashKeys = skipHashKeys
	e.skipFingerprints = make(map[string]string)
	e.skipMu.Unlock()
	return nil
}

// setLastSyncStats records the most recent sync/resync outcome under e.mu.
func (e *Engine) setLastSyncStats(stats SyncStats) {
	e.mu.Lock()
	e.lastSyncStats = stats
	e.mu.Unlock()
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

func countRootSessionsForAgent(
	database *db.DB, agent parser.AgentType, machine, idPrefix string, scoped bool,
) int {
	machinePredicate := ""
	args := []any{string(agent)}
	if scoped {
		machinePredicate = " AND machine = ?"
		args = append(args, machine)
	}
	if idPrefix != "" {
		machinePredicate += " AND substr(id, 1, length(?)) = ?"
		args = append(args, idPrefix, idPrefix)
	}
	var count int
	err := database.Reader().QueryRow(`
		SELECT COUNT(*) FROM sessions
		WHERE agent = ?
		  AND message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL
	`+machinePredicate, args...).Scan(&count)
	if err != nil {
		log.Printf("count root %s sessions: %v", agent, err)
	}
	return count
}

func storageAgentsForDisabledProviders(
	disabledProviders []parser.AgentType,
) []parser.AgentType {
	agents := append([]parser.AgentType(nil), disabledProviders...)
	// Codebuff and Freebuff share one provider but persist distinct agent labels.
	if slices.Contains(disabledProviders, parser.AgentCodebuff) &&
		!slices.Contains(agents, parser.AgentFreebuff) {
		agents = append(agents, parser.AgentFreebuff)
	}
	return agents
}

func (e *Engine) protectedFileSessionCount(
	database *db.DB, machine, idPrefix string, scoped bool,
) (int, error) {
	return protectedFileSessionCount(
		database, machine, idPrefix, scoped, e.preserveAgents,
	)
}

func protectedFileSessionCount(
	database *db.DB, machine, idPrefix string, scoped bool,
	preserveAgents []parser.AgentType,
) (int, error) {
	var count int
	var err error
	if scoped {
		if idPrefix != "" {
			count, err = database.FileBackedSessionCountForSource(
				context.Background(), machine, idPrefix,
			)
		} else {
			count, err = database.FileBackedSessionCountForMachine(
				context.Background(), machine,
			)
		}
	} else {
		count, err = database.FileBackedSessionCount(context.Background())
	}
	if err != nil {
		return 0, err
	}
	for _, agent := range []parser.AgentType{
		parser.AgentOpenCode,
		parser.AgentKilo,
		parser.AgentMiMoCode,
		parser.AgentIcodemate,
	} {
		count -= countRootSessionsForAgent(
			database, agent, machine, idPrefix, scoped,
		)
	}
	storageAgents := storageAgentsForDisabledProviders(preserveAgents)
	seenPreserved := make(map[parser.AgentType]struct{}, len(storageAgents))
	for _, agent := range storageAgents {
		if _, seen := seenPreserved[agent]; seen {
			continue
		}
		seenPreserved[agent] = struct{}{}
		providerAgent := agent
		if agent == parser.AgentFreebuff {
			providerAgent = parser.AgentCodebuff
		}
		def, ok := parser.AgentByType(providerAgent)
		if !ok || (!def.FileBased && agent != parser.AgentDevin) ||
			isOpenCodeFormatStorageAgent(agent) {
			continue
		}
		count -= countRootSessionsForAgent(
			database, agent, machine, idPrefix, scoped,
		)
	}
	if count < 0 {
		count = 0
	}
	return count, nil
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
	defer e.notifyStartupReconciled()
	// Defers run LIFO: Unlock runs before emit.
	defer func() {
		if stats.shouldEmitSync() {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	defer func() { e.recordStartupReconciled(ctx, stats, err) }()
	stats, err = e.syncThenRunLocked(ctx, full, onProgress, work)
	if err == nil {
		// Release while syncMu is still held. A timed fallback that was
		// already waiting on the mutex can then recheck the gate before it
		// performs any duplicate startup work.
		e.ReleaseStartupMaintenance()
	}
	return stats, err
}

func (e *Engine) syncThenRunLocked(
	ctx context.Context,
	full bool,
	onProgress ProgressFunc,
	work func(forceFull bool) error,
) (stats SyncStats, err error) {
	didResync := full || e.db.NeedsResync()
	if didResync {
		stats = e.resyncAllLocked(ctx, onProgress)
		if stats.Aborted && ctx.Err() == nil {
			stats = e.syncAllLocked(
				ctx, onProgress, time.Time{}, nil,
				syncWriteDefault, true, false,
			)
		}
	} else {
		stats = e.syncAllLocked(
			ctx, onProgress, time.Time{}, nil,
			syncWriteDefault, true, false,
		)
	}
	if ctx.Err() != nil {
		return stats, ctx.Err()
	}
	// work typically scans and pushes SQLite rows, so flush any
	// deferred signal recomputes first (inline: syncMu is held) or
	// pushed sessions could carry stale signal/secret fields.
	e.signalSched.flushAllInline()
	e.clearCurrentProgress()
	if err := work(full || didResync); err != nil {
		return stats, err
	}
	return stats, nil
}

// RebuildCleanup owns resources prepared for a multi-source rebuild. Close
// may be retried when it fails so callers can retain mirror locks and temporary
// roots instead of silently losing cleanup ownership.
type RebuildCleanup interface {
	Close() error
}

// RebuildCommitter finalizes durable source state only after the replacement
// archive has been installed successfully.
type RebuildCommitter interface {
	Commit() error
}

type rebuildCleanupError struct {
	owner RebuildCleanup
	err   error
}

func (e *rebuildCleanupError) Error() string { return e.err.Error() }
func (e *rebuildCleanupError) Unwrap() error { return e.err }
func (e *rebuildCleanupError) RetryCleanup() error {
	return e.owner.Close()
}

// SyncThenRunWithRebuild coordinates local sync, optional contributor
// preparation, an atomic multi-source rebuild, and post-rebuild work under the
// engine's exclusive sync lock. Preparation only runs when a rebuild is
// required. rebuildDone runs after a rebuild attempt completes and before
// post-rebuild work begins. Work never runs after a failed or aborted rebuild.
func (e *Engine) SyncThenRunWithRebuild(
	ctx context.Context,
	full bool,
	onProgress ProgressFunc,
	prepare func() (RebuildOptions, RebuildCleanup, error),
	rebuildDone func(SyncStats, error),
	work func(forceFull, rebuilt bool) error,
) (stats SyncStats, retErr error) {
	if e.refuseWriteInForceParse("SyncThenRunWithRebuild") {
		return SyncStats{}, nil
	}
	e.syncMu.Lock()
	defer e.notifyStartupReconciled()
	defer func() {
		if stats.shouldEmitSync() {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()
	defer func() {
		if retErr == nil && !stats.Aborted {
			// Match SyncThenRun: successful foreground coordination unblocks
			// startup backfills while syncMu is still held.
			e.ReleaseStartupMaintenance()
		}
	}()
	defer e.clearCurrentProgress()
	defer func() { e.recordStartupReconciled(ctx, stats, retErr) }()

	didResync := full || e.db.NeedsResync()
	if didResync {
		opts, cleanup, err := prepare()
		if cleanup != nil {
			defer func() {
				if err := cleanup.Close(); err != nil {
					retErr = errors.Join(retErr, &rebuildCleanupError{
						owner: cleanup,
						err:   err,
					})
				}
			}()
		}
		if err != nil {
			if rebuildDone != nil {
				rebuildDone(SyncStats{}, err)
			}
			return SyncStats{}, err
		}
		opts.includePhaseDiagnostics = true
		stats, err = e.resyncAllWithOptionsLocked(
			ctx, onProgress, opts, productionRebuildOperations,
		)
		if err != nil {
			if rebuildDone != nil {
				rebuildDone(stats, err)
			}
			return stats, err
		}
		if stats.Aborted {
			if rebuildDone != nil {
				rebuildDone(stats, nil)
			}
			return stats, nil
		}
		if err := ctx.Err(); err != nil {
			if rebuildDone != nil {
				rebuildDone(stats, err)
			}
			return stats, err
		}
		if committer, ok := cleanup.(RebuildCommitter); ok {
			if err := committer.Commit(); err != nil {
				if rebuildDone != nil {
					rebuildDone(stats, err)
				}
				return stats, err
			}
		}
		if rebuildDone != nil {
			rebuildDone(stats, nil)
		}
	} else {
		stats = e.syncAllLocked(
			ctx, onProgress, time.Time{}, nil,
			syncWriteDefault, true, false,
		)
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	e.signalSched.flushAllInline()
	e.clearCurrentProgress()
	if err := work(full || didResync, didResync); err != nil {
		return stats, err
	}
	return stats, nil
}

// ReleaseStartupMaintenance allows startup backfills to begin. It is
// idempotent so the foreground sync and its bounded fallback can coordinate
// completion without assigning separate gate ownership.
func (e *Engine) ReleaseStartupMaintenance() {
	if e.startupMaintenanceReady == nil {
		return
	}
	e.startupMaintenanceOnce.Do(func() {
		close(e.startupMaintenanceReady)
	})
}

func (e *Engine) notifyStartupReconciled() {
	if e.onStartupReconciled == nil || e.startupReconciledReady == nil {
		return
	}
	select {
	case <-e.startupAttemptReady:
	default:
		return
	}
	e.startupCallbackOnce.Do(func() {
		e.onStartupReconciled(e.startupReconciledStats, e.startupReconciledErr)
	})
}

func (e *Engine) recordStartupReconciled(
	ctx context.Context, stats SyncStats, err error,
) {
	if ctx.Err() != nil {
		return
	}
	e.startupAttemptOnce.Do(func() {
		e.startupReconciledStats = stats
		if err != nil {
			e.startupReconciledErr = err
		} else if !stats.AuthoritativeDiscoveryComplete() {
			e.startupReconciledErr = fmt.Errorf(
				"startup discovery incomplete: %d provider failures",
				stats.providerFailures,
			)
		}
		if e.startupAttemptReady != nil {
			close(e.startupAttemptReady)
		}
	})
	if !startupReconciliationSucceeded(ctx, stats, err) {
		return
	}
	e.startupReconciledOnce.Do(func() {
		if e.startupReconciledReady != nil {
			close(e.startupReconciledReady)
		}
	})
}

func startupReconciliationSucceeded(
	ctx context.Context, stats SyncStats, err error,
) bool {
	return err == nil && ctx.Err() == nil &&
		stats.AuthoritativeDiscoveryComplete()
}

// RecordStartupReconciled acknowledges a startup pass performed out of process
// by a sync worker. The daemon calls it after opening the archive, starting the
// watcher in collecting mode, and running the bounded gap reconciliation, so it
// reproduces the in-process path's completion semantics: it records last-sync
// bookkeeping and the attempt (unblocking the OnStartupReconciled gate),
// releases startup maintenance, fires the OnStartupReconciled callback that
// transitions the watcher out of collecting mode, and emits the "sync" event
// when the pass changed data.
//
// stats carries the worker's discovery outcome (Aborted false means discovery
// was authoritative); err is the gap reconciliation error, nil when the gap
// pass completed. The reconciliation steps are sync.Once-guarded, so calling
// this after — or concurrently with — an in-process fallback that already
// acknowledged startup is a no-op rather than a double-release; the last-sync
// bookkeeping and emit re-run per pass by design.
func (e *Engine) RecordStartupReconciled(stats SyncStats, err error) {
	e.RecordStartupReconciledExclusive(stats, err)
	e.FinishStartupReconciled(stats)
}

// RecordStartupReconciledExclusive is RecordStartupReconciled's lock-held
// half. Callers inside RunExclusive use it so the startup-reconciliation gate
// closes before the exclusive lock is released; a deferred fallback waiting on
// that lock then observes the completed pass instead of launching a duplicate
// archive-scale worker. FinishStartupReconciled completes the tail that must
// run outside the lock.
func (e *Engine) RecordStartupReconciledExclusive(stats SyncStats, err error) {
	e.mu.Lock()
	if err == nil && !stats.Aborted {
		e.lastSync = time.Now()
	}
	e.lastSyncStats = stats
	e.mu.Unlock()
	e.recordStartupReconciled(context.Background(), stats, err)
	e.ReleaseStartupMaintenance()
}

// FinishStartupReconciled fires the completion tail that the in-process defers
// run after releasing syncMu: the "sync" emit for a pass that changed data,
// then the OnStartupReconciled callback. Emitting under the exclusive lock
// could let an Emitter widen the critical section or deadlock by re-entering
// sync code, so this must be called after RunExclusive returns.
func (e *Engine) FinishStartupReconciled(stats SyncStats) {
	if stats.shouldEmitSync() {
		e.emit("sync")
	}
	e.notifyStartupReconciled()
}

// StartupReconciled reports whether a startup pass has already completed
// authoritatively: the startupReconciledReady gate closes only on a successful
// reconciliation, matching RunStartupSyncFallback's own re-run gate. The
// deferred fallback consults it to skip a redundant worker pass when a
// foreground request already drove startup reconciliation. A failed or aborted
// attempt does not close the gate, so retries still proceed.
func (e *Engine) StartupReconciled() bool {
	if e.startupReconciledReady == nil {
		return true
	}
	select {
	case <-e.startupReconciledReady:
		return true
	default:
		return false
	}
}

// AuthoritativeDiscoveryComplete reports whether a full discovery pass
// completed without cancellation, safety abort, or provider listing failure.
// Individual parser warnings do not make discovery incomplete.
func (stats SyncStats) AuthoritativeDiscoveryComplete() bool {
	return !stats.Aborted && stats.providerFailures == 0
}

// RunStartupMaintenance waits for the daemon-launching foreground sync, then
// runs work under the same mutex used by sync and resync database swaps.
func (e *Engine) RunStartupMaintenance(
	ctx context.Context, work func() error,
) error {
	if e.startupMaintenanceReady != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.startupMaintenanceReady:
		}
	}
	return e.RunExclusive(work)
}

// RunStartupSyncFallback performs the local sync that a daemon-launching
// client was expected to request. The caller invokes it after a bounded grace
// period when that request may have been abandoned.
func (e *Engine) RunStartupSyncFallback(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats, ran bool, err error) {
	if e.refuseWriteInForceParse("RunStartupSyncFallback") {
		return SyncStats{}, false, nil
	}
	e.syncMu.Lock()
	defer e.notifyStartupReconciled()
	// Defers run LIFO: Unlock runs before emit.
	defer func() {
		if stats.shouldEmitSync() {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	defer func() { e.recordStartupReconciled(ctx, stats, err) }()

	if e.startupReconciledReady != nil {
		select {
		case <-e.startupReconciledReady:
			return SyncStats{}, false, nil
		default:
		}
	}
	stats, err = e.syncThenRunLocked(
		ctx, false, onProgress, func(bool) error { return nil },
	)
	// Once the fallback has attempted the skipped startup sync, maintenance
	// must be allowed to proceed even if that attempt was interrupted.
	e.ReleaseStartupMaintenance()
	return stats, true, err
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

// ErrSyncInProgress reports that non-blocking foreground sync coordination
// found another sync or maintenance pass holding the exclusive engine lock.
var ErrSyncInProgress = errors.New("sync already in progress")

// TryRunExclusive is the non-blocking foreground counterpart to RunExclusive.
// Background work keeps using RunExclusive so scheduled obligations are not
// lost; user-triggered sync requests use this method to fail promptly instead
// of waiting behind a filesystem operation that may be stalled.
func (e *Engine) TryRunExclusive(work func() error) error {
	if e.refuseWriteInForceParse("TryRunExclusive") {
		return errors.New(
			"TryRunExclusive refused on report-only parse-diff engine",
		)
	}
	if !e.syncMu.TryLock() {
		return ErrSyncInProgress
	}
	defer e.syncMu.Unlock()
	return work()
}

// RunExclusiveFlushed runs work while holding the exclusive sync lock, after
// flushing deferred signal recomputes inline. It is the push half of
// SyncThenRun for daemon push routes whose sync or resync already ran through
// the worker-backed pass: work scans and pushes SQLite rows, so pending
// recomputes must land first or pushed sessions could carry stale
// signal/secret fields.
func (e *Engine) RunExclusiveFlushed(work func() error) error {
	if e.refuseWriteInForceParse("RunExclusiveFlushed") {
		return errors.New(
			"RunExclusiveFlushed refused on report-only parse-diff engine",
		)
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.signalSched.flushAllInline()
	return work()
}

// ApplyWorktreeReclassification serializes the mapping rule, historical
// session rewrites, and identity publication with watcher and sync writes.
func (e *Engine) ApplyWorktreeReclassification(
	ctx context.Context,
	draft db.WorktreeReclassificationDraft,
	acceptedToken string,
	existingMappingID *int64,
) (db.WorktreeProjectMapping, db.WorktreeReclassificationPreview, error) {
	var mapping db.WorktreeProjectMapping
	var preview db.WorktreeReclassificationPreview
	err := e.RunExclusive(func() error {
		var err error
		mapping, preview, err = e.db.ApplyWorktreeReclassification(
			ctx, draft, acceptedToken, existingMappingID,
		)
		return err
	})
	if err == nil && preview.UpdatedSessions > 0 {
		e.emit("sessions")
	}
	return mapping, preview, err
}

// ApplyWorktreeProjectMappings serializes historical session rewrites and
// identity publication with watcher and sync writes.
func (e *Engine) ApplyWorktreeProjectMappings(
	ctx context.Context,
	machine string,
) (db.ApplyWorktreeProjectMappingsResult, error) {
	var result db.ApplyWorktreeProjectMappingsResult
	err := e.RunExclusive(func() error {
		var err error
		result, err = e.db.ApplyWorktreeProjectMappings(ctx, machine)
		return err
	})
	if err == nil && result.UpdatedSessions > 0 {
		e.emit("sessions")
	}
	return result, err
}

// SyncAll discovers and syncs all session files from all agents.
func (e *Engine) SyncAll(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	return e.syncAll(ctx, onProgress, false, false)
}

// SyncAllForceParse discovers all session files and forces each source through
// parsing, bypassing both the skip cache and database freshness checks.
func (e *Engine) SyncAllForceParse(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	return e.syncAll(ctx, onProgress, true, false)
}

// SyncAllForceParseAfterCache requires a complete parse for each source that
// does not have a durable skip-cache entry. It is the replay form of a remote
// full import: sources not reached by an interrupted attempt still parse in
// full, while deterministic failures already recorded by that attempt remain
// suppressed.
func (e *Engine) SyncAllForceParseAfterCache(
	ctx context.Context, onProgress ProgressFunc,
) (stats SyncStats) {
	return e.syncAll(ctx, onProgress, true, true)
}

func (e *Engine) syncAll(
	ctx context.Context,
	onProgress ProgressFunc,
	forceFullParse bool,
	allowCachedFailures bool,
) (stats SyncStats) {
	if e.refuseWriteInForceParse("SyncAll") {
		return SyncStats{}
	}
	e.syncMu.Lock()
	previousForceFullParse := e.forceFullParse
	previousForceFullParseAllowsCache := e.forceFullParseAllowsCache
	e.forceFullParse = forceFullParse
	e.forceFullParseAllowsCache = allowCachedFailures
	defer e.notifyStartupReconciled()
	// Defers run LIFO: Unlock runs before the emit closure so
	// Emitter implementations cannot widen the syncMu critical
	// section or deadlock by re-entering sync code.
	defer func() {
		if stats.hasSessionChanges() {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	defer func() {
		e.forceFullParse = previousForceFullParse
		e.forceFullParseAllowsCache = previousForceFullParseAllowsCache
	}()
	defer e.clearCurrentProgress()
	defer func() { e.recordStartupReconciled(ctx, stats, ctx.Err()) }()
	stats = e.syncAllLocked(
		ctx, onProgress, time.Time{}, nil, syncWriteDefault, true,
		forceFullParse && !allowCachedFailures,
	)
	return
}

// HasActiveSessionSourceBelow checks if a path contains active sessions.
// Freebuff shares the Codebuff provider, so when checking Codebuff,
// also check Freebuff to catch rename events for Freebuff-only directories.
func (e *Engine) HasActiveSessionSourceBelow(agent, path string) (bool, error) {
	found, err := e.db.HasActiveSessionSourceBelow(agent, path)
	if err != nil || found {
		return found, err
	}
	if agent == string(parser.AgentCodebuff) {
		return e.db.HasActiveSessionSourceBelow(string(parser.AgentFreebuff), path)
	}
	return false, nil
}

// ReconcileWatchRoots runs authoritative discovery for a bounded set of watch
// roots, or every configured source after an overflow or unscoped recovery.
// A failed or incomplete discovery is returned to the watcher so its marker is
// retained and retried rather than acknowledged prematurely.
func (e *Engine) ReconcileWatchRoots(
	ctx context.Context, roots []string, full bool,
) error {
	return e.reconcileWatchRoots(ctx, roots, full, false)
}

// ReconcileWatchRootsWithStats is ReconcileWatchRoots plus the pass outcome and
// progress callback: the archive-audit worker uses it so progress and synced or
// tombstoned counts reach the daemon through the worker protocol.
func (e *Engine) ReconcileWatchRootsWithStats(
	ctx context.Context, roots []string, full bool, onProgress ProgressFunc,
) (SyncStats, int, error) {
	return e.reconcileScopedWatchRoots(
		ctx, "", roots, full, false, onProgress,
	)
}

func (e *Engine) reconcileWatchRoots(
	ctx context.Context, roots []string, full, force bool,
) error {
	_, _, err := e.reconcileScopedWatchRoots(
		ctx, "", roots, full, force, nil,
	)
	return err
}

// ReconcileProviderRoots runs the bounded scheduled pass for one provider. It
// resolves scopes against that provider's topology only, so a shallow-watched
// agent never enumerates or tombstones another agent's sessions under an
// overlapping root. Providers whose scheduled discovery is non-authoritative
// leave deletion proof to the archive audit.
func (e *Engine) ReconcileProviderRoots(
	ctx context.Context, agent parser.AgentType, roots []string,
) error {
	if agent == "" {
		return e.reconcileWatchRoots(ctx, roots, false, false)
	}
	_, _, err := e.reconcileScopedWatchRoots(
		ctx, agent, roots, false, false, nil,
	)
	return err
}

// ProviderRootsGroup pairs one provider with the roots of its bounded
// scheduled pass. An empty Agent runs the unscoped local reconciliation path.
type ProviderRootsGroup struct {
	Agent parser.AgentType
	Roots []string
}

// ReconcileProviderRootsGrouped runs the bounded scheduled pass for every
// group and shares one pass epilogue — global subagent linking and skip-cache
// persistence — across the whole batch, so a multi-provider poll performs
// that archive-sized work once instead of once per provider. Every group is
// attempted even when an earlier one fails; per-group failures are wrapped
// with the provider and joined.
//
// syncMu is held across every group and the epilogue: releasing it between a
// group and the deferred persistSkipCache would let a concurrent pass update
// and persist newer skip state that the epilogue's snapshot then overwrites,
// resurrecting removed entries after a restart. "sessions" emits happen after
// the lock is released, coalesced into one event for the batch.
//
// Epilogue eligibility is recorded at each pass's own gate sites — after
// page writes commit, before tombstoning and spool cleanup — so a later
// tombstoning or cleanup failure in one group cannot leave successfully
// synced sessions unlinked or the skip cache unpersisted, matching the
// per-pass ordering exactly.
func (e *Engine) ReconcileProviderRootsGrouped(
	ctx context.Context, groups []ProviderRootsGroup,
) error {
	deferredCtx := context.WithValue(ctx, deferPassEpilogueContextKey{}, true)
	var errs []error
	changed := false
	func() {
		e.syncMu.Lock()
		defer e.syncMu.Unlock()
		defer e.clearCurrentProgress()
		linkEligible := false
		persistEligible := false
		for _, group := range groups {
			if ctx.Err() != nil {
				break
			}
			stats, tombstoned, eligibility, err := e.reconcileScopedWatchRootsLocked(
				deferredCtx, group.Agent, group.Roots, false, false, nil,
			)
			changed = changed || stats.hasSessionChanges() || tombstoned > 0
			linkEligible = linkEligible || eligibility.link
			persistEligible = persistEligible || eligibility.persist
			if err == nil {
				continue
			}
			agent := string(group.Agent)
			if agent == "" {
				agent = "unscoped"
			}
			errs = append(errs, fmt.Errorf("reconcile %s roots: %w", agent, err))
		}
		// A canceled batch skips the epilogue entirely: linking and skip-cache
		// persistence are not context-aware, and shutdown must not block on
		// archive-sized database work. In-memory skip promotions survive and
		// persist on the next clean pass; the returned error keeps the batch
		// from being mistaken for a completed one.
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf(
				"grouped reconciliation epilogue skipped: %w", err,
			))
			return
		}
		if linkEligible {
			if err := e.linkSubagentSessions(ctx); err != nil {
				errs = append(errs, fmt.Errorf(
					"link subagent sessions after grouped reconciliation: %w", err,
				))
			}
		}
		if persistEligible {
			e.persistSkipCache()
		}
	}()
	if changed {
		e.emit("sessions")
	}
	return errors.Join(errs...)
}

func (e *Engine) reconcileScopedWatchRoots(
	ctx context.Context, agent parser.AgentType, roots []string, full, force bool,
	onProgress ProgressFunc,
) (SyncStats, int, error) {
	stats, tombstoned, _, err := func() (SyncStats, int, passEpilogueEligibility, error) {
		e.syncMu.Lock()
		defer e.syncMu.Unlock()
		defer e.clearCurrentProgress()
		return e.reconcileScopedWatchRootsLocked(
			ctx, agent, roots, full, force, onProgress,
		)
	}()
	// Emit outside syncMu so an Emitter implementation cannot widen the
	// critical section or deadlock by re-entering sync code (see SyncAll).
	if stats.hasSessionChanges() || tombstoned > 0 {
		e.emit("sessions")
	}
	return stats, tombstoned, err
}

// reconcileScopedWatchRootsLocked is the scoped pass body. The caller holds
// syncMu and is responsible for emitting "sessions" when the returned stats
// or tombstone count report changes. The returned eligibility reflects the
// per-pass epilogue gates so a deferring caller can run the shared epilogue
// exactly when the pass itself would have.
func (e *Engine) reconcileScopedWatchRootsLocked(
	ctx context.Context, agent parser.AgentType, roots []string, full, force bool,
	onProgress ProgressFunc,
) (SyncStats, int, passEpilogueEligibility, error) {
	e.reportProgress(onProgress, Progress{
		Phase:  PhaseDiscovering,
		Detail: "Reconciling watched session roots",
	})
	fullCoverage := full || (agent == "" && len(roots) == 0)
	plans, excludedRemoteRoots := e.resolveReconciliationPlans(
		ctx, agent, roots, full, fullCoverage,
	)
	if !fullCoverage && !reconciliationPlansNeedPass(plans) {
		// No provider resolved any scope for the request: every root was
		// blank, remote, or unrelated to every configured topology. Complete
		// as a bounded no-op before any spool allocation, preserving the
		// remote-root accounting.
		e.setLastReconciliationResult(ReconciliationResult{
			Complete: true,
			Metrics:  ReconciliationMetrics{ExcludedRemoteRoots: excludedRemoteRoots},
		})
		return SyncStats{}, 0, passEpilogueEligibility{}, nil
	}
	stats, metrics, tombstoned, eligibility, err := e.reconcileWatchRootsStreamedLocked(
		ctx, plans, fullCoverage, force, onProgress,
	)
	metrics.ExcludedRemoteRoots = excludedRemoteRoots
	complete := err == nil && ctx.Err() == nil && !stats.Aborted &&
		stats.Failed == 0 && stats.providerFailures == 0
	if err == nil && !complete {
		err = errors.New("watch root reconciliation aborted")
	}
	e.setLastReconciliationResult(ReconciliationResult{
		Complete:         complete,
		Aborted:          !complete,
		ProviderFailures: stats.providerFailures,
		Metrics:          metrics,
	})
	return stats, tombstoned, eligibility, err
}

// ReconcileWatchRootsAfterLostEvents is the watcher-overflow entrypoint. It is
// separate from ordinary full-scope reconciliation so directory renames do not
// force unchanged sources through their parse paths.
func (e *Engine) ReconcileWatchRootsAfterLostEvents(
	ctx context.Context, roots []string, full bool,
) error {
	return e.reconcileWatchRoots(ctx, roots, full, true)
}

// ReconciliationRootsForAgent returns every configured root for one provider.
// Directory rename events use the complete provider scope because FSEvents may
// report only one endpoint of a move between that provider's roots.
func (e *Engine) ReconciliationRootsForAgent(agent string) []string {
	agentType := parser.AgentType(agent)
	if _, enabled := e.providerFactories[agentType]; !enabled {
		return nil
	}
	return append([]string(nil), e.agentDirs[agentType]...)
}

// providerReconciliationPlan pairs one authoritative provider with the scope
// plan it resolved for the caller's request. A resolution failure is carried
// rather than returned so sibling providers still run, and the failed
// provider reports the caller's own roots for retry.
type providerReconciliationPlan struct {
	agent        parser.AgentType
	plan         parser.ReconciliationScopePlan
	requestRoots []string
	err          error
}

// resolveReconciliationPlans asks each in-scope authoritative provider to map
// the request onto its own topology. Providers own traversal, proof,
// coverage, and retry authority; the engine only selects which providers to
// ask and preserves the historical remote-root accounting.
func (e *Engine) resolveReconciliationPlans(
	ctx context.Context,
	agentFilter parser.AgentType,
	roots []string,
	full, fullCoverage bool,
) ([]providerReconciliationPlan, int) {
	agents := make([]parser.AgentType, 0, len(e.providerFactories))
	for agent := range e.providerFactories {
		agents = append(agents, agent)
	}
	slices.SortFunc(agents, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})
	var plans []providerReconciliationPlan
	for _, agent := range agents {
		if e.providerMigrationModes[agent] != parser.ProviderMigrationProviderAuthoritative {
			continue
		}
		if agentFilter != "" && agent != agentFilter {
			continue
		}
		if len(e.agentDirs[agent]) == 0 {
			continue
		}
		factory := e.providerFactories[agent]
		if factory == nil {
			continue
		}
		requestRoots := roots
		if fullCoverage {
			// A full authoritative request covers each provider's complete
			// configured scope; a partial request lets each provider resolve
			// the same exact caller roots.
			requestRoots = e.agentDirs[agent]
		}
		filtered := make([]string, 0, len(requestRoots))
		for _, root := range requestRoots {
			if strings.TrimSpace(root) == "" || isRemoteReconciliationRoot(root) {
				continue
			}
			filtered = append(filtered, root)
		}
		if len(filtered) == 0 {
			continue
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots: e.agentDirs[agent], Machine: e.machine,
			SourceMachines: e.sourceMachines[agent],
			PathRewriter:   e.pathRewriter,
		})
		plan, err := provider.ResolveReconciliationScopes(
			ctx, parser.ReconciliationScopeRequest{Roots: filtered},
		)
		plans = append(plans, providerReconciliationPlan{
			agent: agent, plan: plan, requestRoots: filtered, err: err,
		})
	}
	return plans, e.excludedRemoteReconciliationRoots(agentFilter, roots, full)
}

// excludedRemoteReconciliationRoots preserves the historical ExcludedRemoteRoots
// semantics: agent-scoped requests count every remote occurrence, unscoped
// partial requests count unique remote roots, and a full recovery counts
// unique remote configured roots.
func (e *Engine) excludedRemoteReconciliationRoots(
	agentFilter parser.AgentType, roots []string, full bool,
) int {
	if agentFilter != "" {
		remote := 0
		for _, root := range roots {
			if isRemoteReconciliationRoot(root) {
				remote++
			}
		}
		return remote
	}
	requested := roots
	if full {
		requested = nil
		for _, dirs := range e.agentDirs {
			requested = append(requested, dirs...)
		}
	}
	remote := make(map[string]struct{})
	for _, root := range requested {
		if isRemoteReconciliationRoot(root) {
			remote[root] = struct{}{}
		}
	}
	return len(remote)
}

// reconciliationPlansNeedPass reports whether any provider resolved a scope
// or failed to resolve; either requires the streamed pass to run so work is
// performed or the failure is accounted with retry roots.
func reconciliationPlansNeedPass(plans []providerReconciliationPlan) bool {
	for _, plan := range plans {
		if plan.err != nil || len(plan.plan.Scopes) > 0 {
			return true
		}
	}
	return false
}

// reconcileWatchRootsStreamedLocked runs one streamed reconciliation pass.
// The caller must hold syncMu: reconcileScopedWatchRoots takes it per pass,
// and ReconcileProviderRootsGrouped holds it across every group plus the
// shared epilogue so no other pass can interleave with a pending epilogue.
// It accepts resolved plans rather than raw roots so no caller can bypass
// provider scope resolution with a string slice.
func (e *Engine) reconcileWatchRootsStreamedLocked(
	ctx context.Context, plans []providerReconciliationPlan,
	fullCoverage, force bool, onProgress ProgressFunc,
) (
	stats SyncStats, metrics ReconciliationMetrics, tombstoned int,
	eligibility passEpilogueEligibility, retErr error,
) {
	if err := ctx.Err(); err != nil {
		return SyncStats{Aborted: true}, metrics, 0, eligibility, err
	}
	if force {
		e.clearWatcherOverflowCaches()
	}
	e.phaseStats.Reset()
	e.anomalies.reset()
	defer func() { e.anomalies.applyTo(&stats) }()
	runtimeMetrics := &reconciliationRuntimeMetrics{}
	ctx = context.WithValue(ctx, reconciliationMetricsContextKey{}, runtimeMetrics)
	ctx = parser.WithStreamingDiscoveryBufferObserver(ctx, runtimeMetrics.providerBuffered)
	ctx = parser.WithStreamingRetainedBytesObserver(ctx, runtimeMetrics.providerRetained)
	ctx = parser.WithSharedContainerScanObserver(ctx, runtimeMetrics.sharedContainerScan)
	ctx = context.WithValue(ctx, deferGlobalLinkContextKey{}, true)
	var closeProviderCache func() error
	ctx, closeProviderCache, err := parser.WithReconciliationCache(ctx)
	if err != nil {
		stats.Aborted = true
		return stats, metrics, 0, eligibility, err
	}
	defer func() {
		if cleanupErr := closeProviderCache(); cleanupErr != nil {
			stats.Aborted = true
			stats.providerFailures++
			retErr = errors.Join(retErr, cleanupErr)
		}
	}()
	mergeMetrics := func(spoolMetrics ReconciliationMetrics) ReconciliationMetrics {
		return runtimeMetrics.snapshot(spoolMetrics)
	}

	spool, err := e.reconciliationSpoolFactory(e.db.Path())
	if err != nil {
		stats.Aborted = true
		return stats, metrics, 0, eligibility, err
	}
	cleaned := false
	defer func() {
		if !cleaned {
			_ = spool.CloseAndRemove()
		}
	}()

	preContainerStates := e.capturePlannedSQLiteContainerStates(plans, fullCoverage)
	providers, completedScopes, failedRoots,
		failures, discoveryErr, err := e.streamReconciliationCandidates(
		ctx, plans, spool, preContainerStates,
	)
	stats.providerFailures = failures
	if err != nil {
		stats.Aborted = true
		metrics = mergeMetrics(spool.Metrics())
		if cleanupErr := spool.CloseAndRemove(); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		cleaned = true
		return stats, metrics, 0, eligibility, err
	}
	authoritativeProviders := make(map[parser.AgentType]struct{}, len(completedScopes))
	for _, completed := range completedScopes {
		authoritativeProviders[completed.agent] = struct{}{}
		// Freebuff shares the Codebuff provider. When Codebuff
		// completes, also mark Freebuff as authoritative so
		// baseline admission (eligibleReconciliationBaselines) and
		// cache-write promotion (the write.agent lookup below)
		// see Freebuff rows even though no Freebuff provider exists
		// in parser.Registry: a single Codebuff scan covers both
		// codebuff and freebuff files at the same on-disk roots.
		if completed.agent == parser.AgentCodebuff {
			authoritativeProviders[parser.AgentFreebuff] = struct{}{}
		}
	}
	e.beginStreamingSQLiteContainerPass(preContainerStates)
	e.finishStreamingSQLiteContainerDiscovery()
	defer func() {
		e.finishSQLiteContainerPass(
			retErr != nil || stats.Aborted || stats.Failed > 0 || stats.providerFailures > 0,
			fullCoverage,
		)
	}()

	var verifiedPass uint64
	if fullCoverage && e.pathRewriter == nil {
		verifiedPass = e.beginVerifiedSourcePass()
		defer func() {
			e.finishVerifiedSourcePass(
				verifiedPass,
				retErr == nil && !stats.Aborted && stats.providerFailures == 0,
			)
		}()
	}
	var cursor reconciliationCursor
	for {
		if err := ctx.Err(); err != nil {
			stats.Aborted = true
			retErr = err
			break
		}
		page, err := spool.Page(ctx, cursor, reconciliationPageSize)
		if err != nil {
			stats.Aborted = true
			retErr = err
			break
		}
		if len(page) == 0 {
			break
		}
		for _, candidate := range page {
			e.noteSQLiteContainerDiscovery(parser.DiscoveredFile{
				Agent: candidate.Provider,
				Path:  candidate.Path,
			})
		}
		files, err := e.rehydrateReconciliationPage(
			ctx, page, providers, force,
		)
		if err != nil {
			stats.Aborted = ctx.Err() != nil
			stats.providerFailures++
			retErr = err
			break
		}
		runtimeMetrics.rehydrated(len(files))
		if verifiedPass != 0 {
			e.markVerifiedDiscoveredSources(files)
		}
		baselineTracker := newReconciliationBaselineTracker()
		pageCtx := context.WithValue(
			ctx, reconciliationBaselineContextKey{}, baselineTracker,
		)
		pageStats := e.collectAndBatch(
			pageCtx, e.startWorkers(pageCtx, files), len(files), len(files), onProgress,
			syncWriteDefault,
		)
		mergeReconciliationSyncStats(&stats, pageStats)
		if pageStats.Aborted || pageStats.Failed > 0 {
			cleanupErr := e.revokeRejectedReconciliationBaselines(
				ctx,
				baselineTracker.listRejected(),
				baselineTracker.listRejectedExactOwnerships(),
			)
			stats.Aborted = true
			retErr = errors.Join(
				fmt.Errorf(
					"watch root reconciliation failed processing page: %d failures",
					pageStats.Failed,
				),
				cleanupErr,
			)
			break
		}
		if err := spool.AddNonAuthoritativeScopes(
			ctx, baselineTracker.listNonAuthoritativeScopes(),
		); err != nil {
			cleanupErr := e.revokeRejectedReconciliationBaselines(
				ctx, baselineTracker.listRejected(),
				baselineTracker.listRejectedExactOwnerships(),
			)
			stats.RecordFailed()
			stats.Aborted = true
			retErr = errors.Join(
				fmt.Errorf(
					"persist non-authoritative reconciliation scopes: %w", err,
				),
				cleanupErr,
			)
			break
		}
		baselineCandidates, baselineAdmitted, err := eligibleReconciliationBaselines(
			ctx,
			page, baselineTracker.list(), authoritativeProviders,
			spool,
		)
		cacheWrites := baselineTracker.listCacheWrites()
		if err != nil {
			e.rejectSkipCacheWrites(cacheWrites)
			cleanupErr := e.revokeRejectedReconciliationBaselines(
				ctx, baselineTracker.listRejected(),
				baselineTracker.listRejectedExactOwnerships(),
			)
			stats.RecordFailed()
			stats.Aborted = true
			retErr = errors.Join(
				fmt.Errorf(
					"query non-authoritative reconciliation scopes: %w", err,
				),
				cleanupErr,
			)
			break
		}
		exactOwnerships := baselineTracker.listExactOwnerships()
		exactOwnerships = slices.DeleteFunc(
			exactOwnerships,
			func(ownership db.SessionSourceOwnership) bool {
				agent := parser.AgentType(ownership.Agent)
				_, eligible := authoritativeProviders[agent]
				return !eligible
			},
		)
		rejectedExactOwnerships := baselineTracker.listRejectedExactOwnerships()
		rejectedExactOwnerships = slices.DeleteFunc(
			rejectedExactOwnerships,
			func(ownership db.SessionSourceOwnership) bool {
				agent := parser.AgentType(ownership.Agent)
				_, eligible := authoritativeProviders[agent]
				return !eligible
			},
		)
		if err := e.baselineReconciliationCandidates(
			ctx, baselineCandidates, baselineAdmitted,
			exactOwnerships, rejectedExactOwnerships,
		); err != nil {
			e.rejectSkipCacheWrites(cacheWrites)
			cleanupErr := e.revokeRejectedReconciliationBaselines(
				ctx, baselineTracker.listRejected(), rejectedExactOwnerships,
			)
			stats.RecordFailed()
			stats.Aborted = true
			retErr = errors.Join(err, cleanupErr)
			break
		}
		eligibleCacheWrites := cacheWrites[:0]
		for _, write := range cacheWrites {
			if _, eligible := authoritativeProviders[write.agent]; eligible {
				eligibleCacheWrites = append(eligibleCacheWrites, write)
				continue
			}
			e.clearSkip(write.key)
		}
		e.promoteSkipCacheWrites(eligibleCacheWrites)
		cursor = page[len(page)-1].Cursor()
	}
	// Page writes committed cleanly when the paging loop finished without an
	// error or a failed write; provider discovery failures are layered on
	// below and must not suppress work that only depends on committed writes.
	// A grouped caller (passEpilogueDeferred) runs linking once after every
	// group instead, consuming the eligibility recorded here; tombstoning
	// below then proceeds without the linking gate, which is safe because
	// linking is idempotent and retried on the caller's next pass.
	if retErr == nil && stats.Failed == 0 && !stats.Aborted {
		eligibility.link = true
	}
	if eligibility.link && !passEpilogueDeferred(ctx) {
		// Batch-level linking was deferred to this global pass, so run it
		// whenever the committed page writes succeeded — including partial
		// provider failures. Sessions from healthy providers are already in
		// the archive, and a permanently failing unrelated provider must not
		// leave their subagent relationships missing indefinitely. Linking
		// runs before the incomplete-reconciliation error is built: a
		// linking failure blocks the completed scopes' tombstoning below, so
		// those scopes must join the retry roots rather than staying stale.
		if err := e.linkSubagentSessions(ctx); err != nil {
			stats.RecordFailed()
			stats.Aborted = true
			retErr = fmt.Errorf(
				"link subagent sessions after reconciliation: %w", err,
			)
		}
	}
	canTombstoneCompletedScopes := retErr == nil && failures > 0
	if failures > 0 {
		retryRoots := append([]string(nil), failedRoots...)
		if retErr != nil {
			for _, completed := range completedScopes {
				retryRoots = append(retryRoots, completed.roots...)
			}
			slices.Sort(retryRoots)
			retryRoots = slices.Compact(retryRoots)
		}
		incomplete := &incompleteReconciliationError{
			failures:  failures,
			roots:     retryRoots,
			completed: completedScopes,
			cause:     discoveryErr,
		}
		if retErr == nil {
			retErr = incomplete
		} else {
			retErr = errors.Join(incomplete, retErr)
		}
	}
	if retErr == nil && stats.Failed > 0 {
		stats.Aborted = true
		retErr = fmt.Errorf(
			"watch root reconciliation failed: %d source or archive failures",
			stats.Failed,
		)
	}
	if retErr == nil {
		eligibility.persist = true
		if !passEpilogueDeferred(ctx) {
			e.persistSkipCache()
		}
		e.mu.Lock()
		e.lastSync = time.Now()
		e.lastSyncStats = stats
		e.mu.Unlock()
	}
	if retErr == nil && ctx.Err() == nil && !stats.Aborted &&
		stats.Failed == 0 && stats.providerFailures == 0 {
		tombstoned, retErr = e.tombstoneMissingWatchSourceScopesLocked(
			ctx, completedScopes, spool,
		)
	} else if canTombstoneCompletedScopes && ctx.Err() == nil &&
		!stats.Aborted && stats.Failed == 0 {
		var incomplete *incompleteReconciliationError
		if errors.As(retErr, &incomplete) && len(incomplete.completed) > 0 {
			var tombstoneErr error
			tombstoned, tombstoneErr =
				e.tombstoneCompletedReconciliationScopesLocked(
					ctx, spool, incomplete,
				)
			if tombstoneErr != nil {
				retErr = tombstoneErr
			}
		}
	}
	metrics = mergeMetrics(spool.Metrics())
	if cleanupErr := spool.CloseAndRemove(); cleanupErr != nil {
		stats.Aborted = true
		retErr = errors.Join(retErr, cleanupErr)
	}
	cleaned = true
	tombstoned += stats.Tombstoned
	return stats, metrics, tombstoned, eligibility, retErr
}

func (e *Engine) tombstoneCompletedReconciliationScopesLocked(
	ctx context.Context,
	spool reconciliationSpoolStore,
	incomplete *incompleteReconciliationError,
) (deleted int, retErr error) {
	deleted, err := e.tombstoneMissingWatchSourceScopesLocked(
		ctx, incomplete.completed, spool,
	)
	if err == nil {
		return deleted, nil
	}
	retryRoots := append([]string(nil), incomplete.roots...)
	for _, completed := range incomplete.completed {
		retryRoots = append(retryRoots, completed.roots...)
	}
	slices.Sort(retryRoots)
	retryRoots = slices.Compact(retryRoots)
	return deleted, &incompleteReconciliationError{
		failures: incomplete.failures,
		roots:    retryRoots,
		cause:    errors.Join(incomplete, err),
	}
}

func (e *Engine) baselineReconciliationCandidates(
	ctx context.Context,
	candidates []reconciliationCandidate,
	admitted []machineSessionSource,
	exactAdmitted []db.SessionSourceOwnership,
	exactRejected []db.SessionSourceOwnership,
) error {
	sources := make([]machineSessionSource, 0, len(candidates))
	for _, candidate := range candidates {
		path := e.effectiveSourcePath(candidate.Path)
		sources = append(sources, machineSessionSource{
			Machine: candidate.Machine,
			Source: db.SessionSourcePath{
				Agent:    string(candidate.Provider),
				FilePath: path,
			},
		})
		// Freebuff shares the Codebuff provider but sessions are stored
		// with agent=AgentFreebuff. Replace baselines for both agent
		// keys so stale Freebuff baselines are cleared when the source
		// is rejected by CWD filter or other admission checks.
		if candidate.Provider == parser.AgentCodebuff {
			sources = append(sources, machineSessionSource{
				Machine: candidate.Machine,
				Source: db.SessionSourcePath{
					Agent:    string(parser.AgentFreebuff),
					FilePath: path,
				},
			})
		}
	}
	var err error
	sources, admitted, err = e.expandSourceBaselinesByStoredAttribution(
		ctx, sources, admitted,
	)
	if err != nil {
		return fmt.Errorf("load reconciliation source attributions: %w", err)
	}
	if err := e.replaceActiveSessionSourceBaselinesWithExceptionsByMachine(
		ctx, sources, admitted, exactAdmitted, exactRejected,
	); err != nil {
		return fmt.Errorf("reconcile source baseline page: %w", err)
	}
	return nil
}

func eligibleReconciliationBaselines(
	ctx context.Context,
	candidates []reconciliationCandidate,
	admitted []machineSessionSource,
	eligibleProviders map[parser.AgentType]struct{},
	spool reconciliationSpoolStore,
) ([]reconciliationCandidate, []machineSessionSource, error) {
	eligibleCandidates := make([]reconciliationCandidate, 0, len(candidates))
	providersWithScopes := make(map[parser.AgentType]bool)
	queriedProviders := make(map[parser.AgentType]struct{})
	for _, candidate := range candidates {
		if _, eligible := eligibleProviders[candidate.Provider]; !eligible {
			continue
		}
		if spool != nil {
			if _, queried := queriedProviders[candidate.Provider]; !queried {
				hasScopes, err := spool.HasNonAuthoritativeScopes(ctx, candidate.Provider)
				if err != nil {
					return nil, nil, err
				}
				providersWithScopes[candidate.Provider] = hasScopes
				queriedProviders[candidate.Provider] = struct{}{}
			}
			if providersWithScopes[candidate.Provider] {
				physicalPath := validatedProviderSourceStatPath(candidate.Path)
				canonicalPath := canonicalReconciliationSourceIdentity(physicalPath)
				withheld, err := spool.ContainsNonAuthoritativeScope(
					ctx, candidate.Provider, canonicalPath,
				)
				if err != nil {
					return nil, nil, err
				}
				if withheld {
					continue
				}
			}
		}
		eligibleCandidates = append(eligibleCandidates, candidate)
	}
	eligibleAdmitted := make([]machineSessionSource, 0, len(admitted))
	for _, source := range admitted {
		if _, eligible := eligibleProviders[parser.AgentType(source.Source.Agent)]; eligible {
			eligibleAdmitted = append(eligibleAdmitted, source)
		}
	}
	return eligibleCandidates, eligibleAdmitted, nil
}

func (e *Engine) replaceActiveSessionSourceBaselinesByMachine(
	ctx context.Context,
	candidates []machineSessionSource,
	admitted []machineSessionSource,
) error {
	return e.replaceActiveSessionSourceBaselinesWithExceptionsByMachine(
		ctx, candidates, admitted, nil, nil,
	)
}

func (e *Engine) replaceActiveSessionSourceBaselinesWithExceptionsByMachine(
	ctx context.Context,
	candidates []machineSessionSource,
	admitted []machineSessionSource,
	exactAdmitted []db.SessionSourceOwnership,
	exactRejected []db.SessionSourceOwnership,
) error {
	candidatesByMachine := make(map[string]map[db.SessionSourcePath]struct{})
	admittedByMachine := make(map[string]map[db.SessionSourcePath]struct{})
	var exactAdmittedByMachine map[string][]db.SessionSourceOwnership
	var exactRejectedByMachine map[string][]db.SessionSourceOwnership
	add := func(
		byMachine map[string]map[db.SessionSourcePath]struct{},
		source machineSessionSource,
	) {
		if source.Source.Agent == "" || source.Source.FilePath == "" {
			return
		}
		if byMachine[source.Machine] == nil {
			byMachine[source.Machine] = make(map[db.SessionSourcePath]struct{})
		}
		byMachine[source.Machine][source.Source] = struct{}{}
	}
	for _, source := range candidates {
		add(candidatesByMachine, source)
	}
	for _, source := range admitted {
		add(admittedByMachine, source)
		// An admitted source may carry immutable attribution that differs from
		// the currently configured candidate. Make it a candidate under its own
		// machine so replacement visits that machine as well.
		add(candidatesByMachine, source)
	}
	for _, ownership := range exactAdmitted {
		if exactAdmittedByMachine == nil {
			exactAdmittedByMachine = make(
				map[string][]db.SessionSourceOwnership,
			)
		}
		exactAdmittedByMachine[ownership.Machine] = append(
			exactAdmittedByMachine[ownership.Machine], ownership,
		)
		add(candidatesByMachine, machineSessionSource{
			Machine: ownership.Machine,
			Source: db.SessionSourcePath{
				Agent: ownership.Agent, FilePath: ownership.FilePath,
			},
		})
	}
	for _, ownership := range exactRejected {
		if exactRejectedByMachine == nil {
			exactRejectedByMachine = make(
				map[string][]db.SessionSourceOwnership,
			)
		}
		exactRejectedByMachine[ownership.Machine] = append(
			exactRejectedByMachine[ownership.Machine], ownership,
		)
		add(candidatesByMachine, machineSessionSource{
			Machine: ownership.Machine,
			Source: db.SessionSourcePath{
				Agent: ownership.Agent, FilePath: ownership.FilePath,
			},
		})
	}
	machines := make([]string, 0, len(candidatesByMachine))
	for machine := range candidatesByMachine {
		machines = append(machines, machine)
	}
	slices.Sort(machines)
	for _, machine := range machines {
		sources := make([]db.SessionSourcePath, 0, len(candidatesByMachine[machine]))
		for source := range candidatesByMachine[machine] {
			sources = append(sources, source)
		}
		admittedSources := make(
			[]db.SessionSourcePath, 0, len(admittedByMachine[machine]),
		)
		for source := range admittedByMachine[machine] {
			admittedSources = append(admittedSources, source)
		}
		if err := e.db.ReplaceActiveSessionSourceBaselinesWithExceptions(
			ctx, machine, sources, admittedSources,
			exactAdmittedByMachine[machine], exactRejectedByMachine[machine],
		); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) replaceSessionSourceBaselineExceptionsByMachine(
	ctx context.Context,
	exactAdmitted []db.SessionSourceOwnership,
	exactRejected []db.SessionSourceOwnership,
) error {
	type exceptionSet struct {
		admitted []db.SessionSourceOwnership
		rejected []db.SessionSourceOwnership
	}
	byMachine := make(map[string]exceptionSet)
	add := func(ownership db.SessionSourceOwnership, admitted bool) {
		if ownership.Agent == "" || ownership.FilePath == "" ||
			ownership.ID == "" {
			return
		}
		set := byMachine[ownership.Machine]
		if admitted {
			set.admitted = append(set.admitted, ownership)
			byMachine[ownership.Machine] = set
			return
		}
		set.rejected = append(set.rejected, ownership)
		byMachine[ownership.Machine] = set
	}
	for _, ownership := range exactAdmitted {
		add(ownership, true)
	}
	for _, ownership := range exactRejected {
		add(ownership, false)
	}
	machines := make([]string, 0, len(byMachine))
	for machine := range byMachine {
		machines = append(machines, machine)
	}
	slices.Sort(machines)
	for _, machine := range machines {
		set := byMachine[machine]
		if err := e.db.ReplaceActiveSessionSourceBaselinesWithExceptions(
			ctx, machine, nil, nil, set.admitted, set.rejected,
		); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) streamReconciliationCandidates(
	ctx context.Context,
	plans []providerReconciliationPlan,
	spool reconciliationSpoolStore,
	preContainerStates map[string]parser.SQLiteContainerState,
) (
	map[parser.AgentType]parser.Provider,
	[]reconciliationProviderScope,
	[]string,
	int,
	error,
	error,
) {
	providers := make(map[parser.AgentType]parser.Provider)
	var completedScopes []reconciliationProviderScope
	var failedRoots []string
	var failures int
	var discoveryErr error
	// Trusted containers stream the bounded watermark listing here for the
	// same reason full discovery lists them that way: every candidate they
	// spool will gate-skip, so the child digest would be archive-sized work
	// nothing reads. The predicate is keyed to this pass's pre-discovery
	// captures; a container that changes mid-stream fails its recapture
	// check and its candidates resolve full fingerprints instead.
	containerTrusted := e.sqliteContainerTrustedForDiscovery(preContainerStates)
	for _, plan := range plans {
		agent := plan.agent
		if plan.err != nil {
			failures++
			failedRoots = append(failedRoots, plan.requestRoots...)
			log.Printf("%s provider reconciliation scopes: %v", agent, plan.err)
			discoveryErr = errors.Join(discoveryErr, fmt.Errorf(
				"%s provider reconciliation scopes: %w", agent, plan.err,
			))
			continue
		}
		if len(plan.plan.Scopes) == 0 {
			continue
		}
		factory := e.providerFactories[agent]
		if factory == nil {
			continue
		}
		// traversalRoots is the ordered union across the plan's scopes: the
		// rehydration provider and candidate preference ranking see the same
		// root list one whole-provider discovery would have used.
		var traversalRoots []string
		var planRetryRoots []string
		for _, scope := range plan.plan.Scopes {
			for _, root := range scope.TraversalRoots {
				if !slices.Contains(traversalRoots, root) {
					traversalRoots = append(traversalRoots, root)
				}
			}
			planRetryRoots = append(planRetryRoots, scope.RetryRoots...)
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots: traversalRoots, Machine: e.machine, PathRewriter: e.pathRewriter,
			SourceMachines:                     e.sourceMachines[agent],
			SQLiteContainerUnchangedSinceTrust: containerTrusted,
		})
		providers[agent] = provider
		if provider.Capabilities().Source.StreamingDiscovery != parser.CapabilitySupported {
			failures++
			failedRoots = append(failedRoots, planRetryRoots...)
			log.Printf("%s provider discovery: streaming discovery unsupported", agent)
			continue
		}
		watchRoots, err := parser.ResolveWatchRoots(ctx, provider)
		if err != nil {
			failures++
			failedRoots = append(failedRoots, planRetryRoots...)
			discoveryErr = errors.Join(discoveryErr, fmt.Errorf(
				"%s provider watch roots: %w", agent, err,
			))
			continue
		}
		for _, group := range reconciliationTraversalGroups(plan.plan.Scopes) {
			var groupRetryRoots []string
			for _, scope := range group.scopes {
				groupRetryRoots = append(groupRetryRoots, scope.RetryRoots...)
			}
			scopeProvider := factory.NewProvider(parser.ProviderConfig{
				Roots: group.roots, Machine: e.machine,
				SourceMachines:                     e.sourceMachines[agent],
				PathRewriter:                       e.pathRewriter,
				SQLiteContainerUnchangedSinceTrust: containerTrusted,
			})
			discoverer, ok := scopeProvider.(parser.StreamingDiscoverer)
			if !ok {
				failures++
				failedRoots = append(failedRoots, groupRetryRoots...)
				continue
			}
			groupProofs := make([][]db.StoredSourcePathHintScope, len(group.scopes))
			for i, scope := range group.scopes {
				groupProofs[i] = storedSourceDBHintScopes(scope.PhysicalProofScopes)
			}
			var spoolErr error
			err = discoverer.DiscoverEach(ctx, func(source parser.SourceRef) error {
				candidate, ok := e.reconciliationCandidate(
					provider, source, traversalRoots, watchRoots,
				)
				if !ok {
					return nil
				}
				admitted := false
				for _, proofScopes := range groupProofs {
					if db.StoredSourcePathHintScopesContain(candidate.Path, proofScopes) {
						admitted = true
						break
					}
				}
				if !admitted {
					if reconciliationPathWithinTraversal(
						candidate.Path, group.roots,
					) {
						// An unrequested sibling inside the traversal gateway:
						// dropping before the spool is baseline-safe because
						// ReplaceActiveSessionSourceBaselines diffs only
						// candidates present in the page.
						return nil
					}
					// Outside both traversal and every proof is a provider
					// contract violation; fail the group closed so its
					// sessions are preserved and its own retry roots are
					// returned.
					return fmt.Errorf(
						"source %s outside traversal and proof scope",
						candidate.Path,
					)
				}
				spoolErr = spool.Add(ctx, candidate)
				return spoolErr
			})
			if err != nil {
				if spoolErr != nil {
					return providers, completedScopes,
						failedRoots, failures, discoveryErr, spoolErr
				}
				if ctx.Err() != nil {
					return providers, completedScopes,
						failedRoots, failures, discoveryErr, ctx.Err()
				}
				log.Printf("%s provider streaming discovery: %v", agent, err)
				failures++
				failedRoots = append(failedRoots, groupRetryRoots...)
				discoveryErr = errors.Join(discoveryErr, fmt.Errorf(
					"%s provider streaming discovery: %w", agent, err,
				))
				continue
			}
			for _, scope := range group.scopes {
				completedScopes = append(completedScopes, reconciliationProviderScope{
					agent: agent,
					roots: append([]string(nil), scope.RetryRoots...),
					proofScopes: append(
						[]parser.StoredSourceHintScope(nil), scope.PhysicalProofScopes...,
					),
					coverageIdentities: append(
						[]string(nil), scope.CoverageIdentities...,
					),
					requiredCoverageIdentities: append(
						[]string(nil), plan.plan.RequiredCoverageIdentities...,
					),
				})
			}
		}
	}
	slices.Sort(failedRoots)
	failedRoots = slices.Compact(failedRoots)
	return providers, completedScopes,
		failedRoots, failures, discoveryErr, nil
}

// reconciliationTraversalGroup is one shared discovery walk: every scope in
// the group declares the identical traversal-root set, so a single stream
// serves them all, with each scope keeping its own proof and coverage
// authority.
type reconciliationTraversalGroup struct {
	roots  []string
	scopes []parser.ReconciliationScope
}

// reconciliationTraversalGroups clusters one plan's scopes by their exact
// traversal-root sets. A request naming N descendants under one configured
// gateway resolves N scopes that all traverse that gateway; without grouping
// the pass would walk the gateway N times to admit each proof, and the walk
// is the archive-scale part. A group failure fails every member scope, which
// matches the shared traversal: the same walk would have failed each of them
// individually.
func reconciliationTraversalGroups(
	scopes []parser.ReconciliationScope,
) []reconciliationTraversalGroup {
	var groups []reconciliationTraversalGroup
	for _, scope := range scopes {
		matched := false
		for i := range groups {
			if slices.Equal(groups[i].roots, scope.TraversalRoots) {
				groups[i].scopes = append(groups[i].scopes, scope)
				matched = true
				break
			}
		}
		if !matched {
			groups = append(groups, reconciliationTraversalGroup{
				roots:  scope.TraversalRoots,
				scopes: []parser.ReconciliationScope{scope},
			})
		}
	}
	return groups
}

// reconciliationPathWithinTraversal reports whether a discovered candidate
// lies inside one of the scope's traversal gateways, resolving provider
// virtual member syntax to the physical container first.
func reconciliationPathWithinTraversal(path string, traversalRoots []string) bool {
	cleaned := cleanRootPath(validatedProviderSourceStatPath(path))
	for _, root := range traversalRoots {
		if samePathOrDescendant(cleaned, cleanRootPath(root)) {
			return true
		}
	}
	return false
}

// reconciliationProviderScope is the completed-scope record: the caller's own
// retry roots plus the provider-issued proof, coverage, and required-coverage
// authorities that tombstoning consumes.
type reconciliationProviderScope struct {
	agent                      parser.AgentType
	roots                      []string
	proofScopes                []parser.StoredSourceHintScope
	coverageIdentities         []string
	requiredCoverageIdentities []string
}

type incompleteReconciliationError struct {
	failures  int
	roots     []string
	completed []reconciliationProviderScope
	cause     error
}

func (e *incompleteReconciliationError) Error() string {
	return fmt.Sprintf(
		"watch root reconciliation incomplete: %d provider discoveries failed",
		e.failures,
	)
}

func (e *incompleteReconciliationError) Unwrap() error { return e.cause }

func (e *incompleteReconciliationError) ReconciliationRetryRoots() []string {
	return append([]string(nil), e.roots...)
}

func (e *Engine) reconciliationCandidate(
	provider parser.Provider, source parser.SourceRef, roots []string,
	watchRoots []parser.WatchRoot,
) (reconciliationCandidate, bool) {
	path := providerDiscoveredPath(source)
	if path == "" || isRemoteReconciliationRoot(path) {
		return reconciliationCandidate{}, false
	}
	agent := source.Provider
	if agent == "" {
		agent = provider.Definition().Type
	}
	identity := reconciliationSourceIdentity(agent, source)
	if identity == "" {
		return reconciliationCandidate{}, false
	}
	root := reconciliationWatchRoot(path, watchRoots, roots)
	preference1, preference2, preference3 := int64(0), int64(0), int64(0)
	statPath := validatedProviderSourceStatPath(path)
	if agent == parser.AgentClaude {
		if info, err := os.Stat(statPath); err == nil {
			preference1 = info.Size()
			preference2 = info.ModTime().UnixNano()
			fullID := applyIDPrefixToID(e.idPrefix, identity)
			storedPath := e.db.GetSessionFilePath(fullID)
			storedSize, storedMtime, stored := e.db.GetSessionFileInfo(fullID)
			if stored && e.effectiveSourcePath(path) == storedPath &&
				storedSize == info.Size() && storedMtime == info.ModTime().UnixNano() &&
				e.db.GetSessionDataVersion(fullID) >= db.CurrentDataVersion() {
				preference3 = 1
			}
		}
	}
	if isCodexFormatAgent(agent) && codexLayoutForPath(path) == parser.CodexLayoutDated {
		preference1 = 1
	}
	if isOpenCodeFormatAgent(agent) {
		if statPath == path {
			preference1 = 1
		}
	} else if agent != parser.AgentClaude && !isCodexFormatAgent(agent) {
		for i, configured := range roots {
			if samePathOrDescendant(statPath, configured) {
				preference1 = int64(len(roots) - i)
				break
			}
		}
	}
	if ranker, ok := provider.(parser.ReconciliationSourceRanker); ok {
		rank := ranker.ReconciliationSourceRank(source)
		preference2 = rank.Class
		preference3 = rank.Recency
	}
	if agent == parser.AgentAntigravityCLI {
		preference2 = boolPreference(strings.HasSuffix(path, ".db"))
	}
	return reconciliationCandidate{
		Provider: agent, Identity: identity, Path: path,
		StoredPath: canonicalReconciliationSourceIdentity(
			e.effectiveSourcePath(path),
		), MemberIdentity: source.ReconciliationIdentity, WatchRoot: root,
		Machine: e.machineForProviderSource(agent, source, path),
		Project: source.ProjectHint, Preference1: preference1,
		Preference2: preference2, Preference3: preference3,
	}, true
}

func boolPreference(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func reconciliationSourceIdentity(agent parser.AgentType, source parser.SourceRef) string {
	if agent == parser.AgentClaude {
		return claudeSessionIDFromPath(providerDiscoveredPath(source))
	}
	if isOpenCodeFormatAgent(agent) {
		path := providerDiscoveredPath(source)
		if statPath := validatedProviderSourceStatPath(path); statPath != path {
			_, sessionID, _ := parser.ParseVirtualSourcePath(path)
			return sessionID
		}
		if strings.EqualFold(filepath.Ext(path), ".json") {
			return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		}
	}
	for _, candidate := range []string{source.Key, source.FingerprintKey, source.DisplayPath} {
		if candidate != "" {
			return canonicalReconciliationSourceIdentity(candidate)
		}
	}
	return ""
}

func isOpenCodeFormatAgent(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentOpenCode, parser.AgentKilo, parser.AgentMiMoCode, parser.AgentIcodemate:
		return true
	default:
		return false
	}
}

// isCodexFormatAgent reports whether an agent stores sessions in the Codex
// rollout-JSONL layout: UUID-bearing filenames, a dated year/month/day tree
// with an optional flat archive, and JSONL-tail incremental appends. It gates
// the format-shaped branches (duplicate resolution, layout preference,
// reconciliation identity, parse-diff mtime) so the Codex fork TraeX gets the
// same handling. Branches that depend on Codex's session_index.jsonl sidecar
// or its S3 archive layout stay keyed to parser.AgentCodex alone: TraeX writes
// no index file and has no S3 path convention.
func isCodexFormatAgent(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentCodex, parser.AgentTraeX:
		return true
	default:
		return false
	}
}

func reconciliationWatchRoot(
	path string, watchRoots []parser.WatchRoot, configuredRoots []string,
) string {
	statPath := validatedProviderSourceStatPath(path)
	best := ""
	for _, root := range watchRoots {
		if samePathOrDescendant(statPath, root.Path) && len(root.Path) > len(best) {
			best = root.Path
		}
	}
	if best != "" {
		return best
	}
	for _, root := range configuredRoots {
		if samePathOrDescendant(statPath, root) && len(root) > len(best) {
			best = root
		}
	}
	return best
}

func (e *Engine) rehydrateReconciliationPage(
	ctx context.Context, page []reconciliationCandidate,
	providers map[parser.AgentType]parser.Provider,
	force bool,
) ([]parser.DiscoveredFile, error) {
	files := make([]parser.DiscoveredFile, 0, len(page))
	for _, candidate := range page {
		forceCandidate := force
		provider := providers[candidate.Provider]
		if provider == nil {
			return nil, fmt.Errorf("rehydrate %s source: provider unavailable", candidate.Provider)
		}
		if resolver, ok := provider.(parser.ReconciliationSourceResolver); ok {
			source, found, err := resolver.SourceForReconciliation(
				ctx, candidate.Path, candidate.Project,
			)
			if err != nil {
				return nil, fmt.Errorf("rehydrate %s source %s: %w", candidate.Provider, candidate.Path, err)
			}
			if found && reconciliationSourceIdentity(candidate.Provider, source) == candidate.Identity {
				files = append(files, parser.DiscoveredFile{
					Path: candidate.Path, Project: source.ProjectHint,
					Agent: candidate.Provider, ForceParse: forceCandidate,
					Machine:        candidate.Machine,
					ProviderSource: &source, ProviderProcess: true,
				})
				continue
			}
		}
		sources, err := provider.SourcesForChangedPath(ctx, parser.ChangedPathRequest{
			Path: candidate.Path, EventKind: "write", WatchRoot: candidate.WatchRoot,
		})
		if err != nil {
			return nil, fmt.Errorf("rehydrate %s source %s: %w", candidate.Provider, candidate.Path, err)
		}
		var matched *parser.SourceRef
		for i := range sources {
			if reconciliationSourceIdentity(candidate.Provider, sources[i]) == candidate.Identity &&
				sameReconciliationSourcePath(providerDiscoveredPath(sources[i]), candidate.Path) {
				matched = &sources[i]
				break
			}
		}
		if matched == nil {
			return nil, fmt.Errorf("rehydrate %s source %s: canonical source not found", candidate.Provider, candidate.Path)
		}
		source := *matched
		files = append(files, parser.DiscoveredFile{
			Path: candidate.Path, Project: source.ProjectHint,
			Agent: candidate.Provider, ForceParse: forceCandidate,
			// Carry the candidate's stored attribution: recomputing it from the
			// physical path is wrong for providers whose source can sit outside
			// the labeled root it was configured under.
			Machine:        candidate.Machine,
			ProviderSource: &source, ProviderProcess: true,
		})
	}
	return files, nil
}

func canonicalReconciliationSourceIdentity(value string) string {
	if _, err := os.Lstat(value); err == nil || !os.IsNotExist(err) {
		if looksLikeReconciliationPath(value) {
			return canonicalReconciliationPath(value)
		}
		return value
	}
	container, member, virtual := parser.ParseVirtualSourcePath(value)
	if virtual {
		return canonicalReconciliationPath(container) + "#" + member
	}
	if looksLikeReconciliationPath(value) {
		return canonicalReconciliationPath(value)
	}
	return value
}

// validatedProviderSourceStatPath resolves synthetic member syntax only after
// an exact physical-path check failed. Callers use it exclusively for paths
// emitted or reconstructed by a provider, which supplies the validation that a
// provider-neutral '#' split cannot establish on its own.
func validatedProviderSourceStatPath(value string) string {
	if _, err := os.Lstat(value); err == nil || !os.IsNotExist(err) {
		return value
	}
	if container, _, virtual := parser.ParseVirtualSourcePath(value); virtual {
		return container
	}
	return value
}

func looksLikeReconciliationPath(value string) bool {
	return filepath.IsAbs(value) || strings.ContainsAny(value, `/\`) || isWindowsPath(value)
}

func isWindowsPath(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') ||
		(value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' &&
		(value[2] == '\\' || value[2] == '/') || strings.HasPrefix(value, `\\`)
}

func canonicalReconciliationPath(value string) string {
	if isWindowsPath(value) {
		return strings.ToLower(pathpkg.Clean(strings.ReplaceAll(value, `\`, "/")))
	}
	return filepath.Clean(value)
}

func sameReconciliationSourcePath(left, right string) bool {
	return canonicalReconciliationSourceIdentity(left) ==
		canonicalReconciliationSourceIdentity(right)
}

// reconciliationMemberRelocated reports whether the provider still resolves
// the session anywhere in its full configured scope. A scoped pass proves a
// member gone from its own container, but the same logical member may have
// moved to another configured root the pass never streamed; a deletion
// claimed here would outlive the move until that root happens to sync. The
// lookup passes only the session identity — a stored-path probe could
// resolve the stale spelling without verifying the row exists.
func reconciliationMemberRelocated(
	ctx context.Context, provider parser.Provider, fullSessionID string,
) (bool, error) {
	if provider == nil {
		// Without a provider the move cannot be ruled out; report the member
		// as possibly relocated so deletion is withheld.
		return true, nil
	}
	_, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
		FullSessionID: fullSessionID,
	})
	if err != nil {
		return false, fmt.Errorf(
			"resolve possibly relocated member %s: %w", fullSessionID, err,
		)
	}
	return found, nil
}

// reconciliationProofCoversContainerMembership reports whether one completed
// scope's proof claims the whole virtual membership of the container at
// physicalPath. Such a scope streamed and admitted exactly that container's
// members, so a member row absent from the spool is provably gone from the
// container even though the pass covered no configured root in full.
func reconciliationProofCoversContainerMembership(
	proofs []parser.StoredSourceHintScope, physicalPath string,
) bool {
	for _, proof := range proofs {
		if proof.IncludeVirtualMembers &&
			sameReconciliationSourcePath(proof.Path, physicalPath) {
			return true
		}
	}
	return false
}

func reconciliationOwnershipWithinNonAuthoritativeContainer(
	ctx context.Context,
	spool reconciliationSpoolStore,
	agent parser.AgentType,
	ownership db.SessionSourceOwnership,
) (bool, error) {
	if spool == nil {
		return false, nil
	}
	physicalPath := validatedProviderSourceStatPath(ownership.FilePath)
	canonicalPath := canonicalReconciliationSourceIdentity(physicalPath)
	return spool.ContainsNonAuthoritativeScope(ctx, agent, canonicalPath)
}

// aggregateOwnedMemberGone reports whether an ownership row whose stored
// FilePath is a still-present multi-member container has lost its member.
// Container discovery records the container path itself for every member,
// so a removed member never trips the missing-path stat; compare the row
// against the streamed pass's membership instead. The check requires a
// spool and member authority — full provider-root coverage, or a completed
// scope whose proof spans the row's whole container membership; absence
// from any narrower stream proves nothing — and providers opt in via
// parser.ReconciliationAggregateMemberResolver.
func aggregateOwnedMemberGone(
	ctx context.Context,
	spool reconciliationSpoolStore,
	provider parser.Provider,
	agent parser.AgentType,
	ownership db.SessionSourceOwnership,
	memberAuthority bool,
) (bool, error) {
	if spool == nil || provider == nil || !memberAuthority {
		return false, nil
	}
	resolver, ok := provider.(parser.ReconciliationAggregateMemberResolver)
	if !ok {
		return false, nil
	}
	memberPaths := resolver.ReconciliationAggregateMemberPaths(
		ownership.FilePath, ownership.ID,
	)
	if len(memberPaths) == 0 {
		return false, nil
	}
	for _, path := range memberPaths {
		present, err := spool.ContainsSource(
			ctx, agent, canonicalReconciliationSourceIdentity(path),
		)
		if err != nil {
			return false, fmt.Errorf(
				"lookup %s aggregate member %s: %w",
				agent, ownership.FilePath, err,
			)
		}
		if present {
			return false, nil
		}
	}
	return true, nil
}

// reconciliationReplacementIdentity is the discovery identity a surviving
// same-session duplicate of the missing stored path would carry in the
// reconciliation index. Empty for agents whose stored paths cannot be
// replaced by a differently-located copy.
func reconciliationReplacementIdentity(
	agent parser.AgentType, storedPath string,
) string {
	switch agent {
	case parser.AgentClaude:
		return claudeSessionIDFromPath(storedPath)
	case parser.AgentCodex, parser.AgentTraeX:
		uuid := parser.CodexSessionUUIDFromFilename(filepath.Base(storedPath))
		if uuid == "" {
			return ""
		}
		return parser.CodexSourceKey(agent, uuid)
	default:
		return ""
	}
}

func mergeReconciliationSyncStats(dst *SyncStats, src SyncStats) {
	dst.TotalSessions += src.TotalSessions
	dst.Synced += src.Synced
	dst.Skipped += src.Skipped
	dst.Failed += src.Failed
	dst.filesOK += src.filesOK
	dst.filesDiscovered += src.filesDiscovered
	dst.nonContainerDiscovered += src.nonContainerDiscovered
	dst.messagesIndexed += src.messagesIndexed
	dst.parserExcludedFiles += src.parserExcludedFiles
	dst.parserExcludedIDs = append(dst.parserExcludedIDs, src.parserExcludedIDs...)
	dst.Tombstoned += src.Tombstoned
	dst.sourceMissingArchiveMembers = append(
		dst.sourceMissingArchiveMembers, src.sourceMissingArchiveMembers...,
	)
	dst.cwdFilteredSessions += src.cwdFilteredSessions
	dst.cwdFilteredFiles += src.cwdFilteredFiles
	dst.CwdUpdated += src.CwdUpdated
	dst.Aborted = dst.Aborted || src.Aborted
}

// tombstoneMissingWatchSourcesLocked adapts a raw changed-path root list to
// the typed scope authority by resolving it through each provider's own
// topology, exactly as a reconciliation pass would.
func (e *Engine) tombstoneMissingWatchSourcesLocked(
	ctx context.Context,
	roots []string,
	spool reconciliationSpoolStore,
) (deleted int, retErr error) {
	plans, _ := e.resolveReconciliationPlans(ctx, "", roots, false, false)
	var scopes []reconciliationProviderScope
	for _, plan := range plans {
		if plan.err != nil {
			return 0, fmt.Errorf(
				"%s provider reconciliation scopes: %w", plan.agent, plan.err,
			)
		}
		for _, scope := range plan.plan.Scopes {
			scopes = append(scopes, reconciliationProviderScope{
				agent:                      plan.agent,
				roots:                      scope.RetryRoots,
				proofScopes:                scope.PhysicalProofScopes,
				coverageIdentities:         scope.CoverageIdentities,
				requiredCoverageIdentities: plan.plan.RequiredCoverageIdentities,
			})
		}
	}
	return e.tombstoneMissingWatchSourceScopesLocked(ctx, scopes, spool)
}

// reconciliationCoverageComplete reports whether the scopes completed for one
// provider cover every coverage identity its full configured scope requires.
// This is the provider-issued replacement for recomputing root geometry: a
// remote or unresolved configured root keeps its required identity uncovered,
// so full-coverage deletion authority stays unreachable.
func reconciliationCoverageComplete(scopes []reconciliationProviderScope) bool {
	covered := make(map[string]struct{})
	var required []string
	for _, scope := range scopes {
		if len(scope.requiredCoverageIdentities) > 0 {
			required = scope.requiredCoverageIdentities
		}
		for _, identity := range scope.coverageIdentities {
			covered[identity] = struct{}{}
		}
	}
	if len(required) == 0 {
		return false
	}
	for _, identity := range required {
		if _, ok := covered[identity]; !ok {
			return false
		}
	}
	return true
}

// tombstoneMissingWatchSourceScopesLocked pages ownership rows inside each
// completed scope's provider-issued physical proof and tombstones rows whose
// sources are provably gone.
func (e *Engine) tombstoneMissingWatchSourceScopesLocked(
	ctx context.Context,
	scopes []reconciliationProviderScope,
	spool reconciliationSpoolStore,
) (deleted int, retErr error) {
	if e.pathRewriter != nil {
		// Remote imports rewrite extraction paths to canonical stored paths in
		// one direction only. Without a stored-to-local inverse, stat and
		// provider lookup cannot authoritatively prove source loss.
		return 0, nil
	}
	var agents []parser.AgentType
	scopesByAgent := make(map[parser.AgentType][]reconciliationProviderScope)
	for _, scope := range scopes {
		if _, ok := scopesByAgent[scope.agent]; !ok {
			agents = append(agents, scope.agent)
		}
		scopesByAgent[scope.agent] = append(scopesByAgent[scope.agent], scope)
	}
	// Read once per pass, not per scope: the list is bounded by how many
	// machines the archive holds, which is independent of its session count.
	archiveMachines, err := e.db.GetMachines(ctx, false, false)
	if err != nil {
		return 0, fmt.Errorf("list archive machines for watch reconciliation: %w", err)
	}
	for _, agent := range agents {
		agentScopes := scopesByAgent[agent]
		var provider parser.Provider
		var replacementIndex reconciliationSpoolStore
		ownsReplacementIndex := false
		hasNonAuthoritativeScopes := false
		if spool != nil {
			hasNonAuthoritativeScopes, err = spool.HasNonAuthoritativeScopes(ctx, agent)
			if err != nil {
				return deleted, fmt.Errorf(
					"query %s non-authoritative reconciliation scopes: %w", agent, err,
				)
			}
		}
		allProviderRootsCovered := reconciliationCoverageComplete(agentScopes)
		if factory := e.providerFactories[agent]; factory != nil {
			provider = factory.NewProvider(parser.ProviderConfig{
				Roots: e.agentDirs[agent], Machine: e.machine,
				SourceMachines: e.sourceMachines[agent],
				PathRewriter:   e.pathRewriter,
			})
		}
		for _, scope := range agentScopes {
			ownershipScopes := storedSourceDBHintScopes(scope.proofScopes)
			if len(ownershipScopes) == 0 {
				continue
			}
			// A scope proves absence for its own roots, but a stored row carries
			// the machine it was admitted under. Query every machine those roots
			// can hold so a relabeled root still reconciles its older rows.
			ownershipMachines := e.reconciliationOwnershipMachines(
				agent, scope.roots, archiveMachines,
			)
			if len(ownershipMachines) == 0 {
				continue
			}
			// Freebuff shares the Codebuff provider. When processing
			// Codebuff, also query for Freebuff sessions so they can
			// be tombstoned.
			agentStrs := []string{string(agent)}
			if agent == parser.AgentCodebuff {
				agentStrs = append(agentStrs, string(parser.AgentFreebuff))
			}
			for _, agentStr := range agentStrs {
				queryIndex := 0
				var cursor db.SessionSourceCursor
				for queryIndex < len(ownershipMachines) {
					ownershipMachine := ownershipMachines[queryIndex]
					if err := ctx.Err(); err != nil {
						return deleted, err
					}
					page, err := e.db.ListActiveSessionSourceOwnershipScopesPage(
						ctx, ownershipMachine, agentStr,
						ownershipScopes, cursor,
					)
					if err != nil {
						return deleted, fmt.Errorf(
							"list %s watch source ownership: %w", agent, err,
						)
					}
					if len(page) == 0 {
						queryIndex++
						cursor = db.SessionSourceCursor{}
						continue
					}
					missingByPath := make(map[string]bool, len(page))
					for _, ownership := range page {
						if hasNonAuthoritativeScopes {
							withheld, err := reconciliationOwnershipWithinNonAuthoritativeContainer(
								ctx, spool, agent, ownership,
							)
							if err != nil {
								return deleted, fmt.Errorf(
									"query %s non-authoritative source %q: %w",
									agent, ownership.FilePath, err,
								)
							}
							if withheld {
								continue
							}
						}
						if !db.StoredSourcePathHintScopesContain(
							ownership.FilePath, ownershipScopes,
						) {
							// The SQL LIKE prefilter is ASCII-case-insensitive
							// while this predicate is platform-exact; retain any
							// row the pass holds no proof over and leave the
							// keyset cursor untouched.
							continue
						}
						statPath := ownership.FilePath
						persistentMemberContainerExists := false
						missing, ok := missingByPath[statPath]
						if !ok {
							_, statErr := e.lstatSource(statPath)
							missing = os.IsNotExist(statErr)
							missingByPath[statPath] = missing
						}
						if !missing {
							// An aggregate row stores its container path, so
							// full-root coverage OR a scope proof spanning that
							// container's whole membership grants member-absence
							// authority: either way the completed stream
							// enumerated every member the row could resolve to.
							gone, checkErr := aggregateOwnedMemberGone(
								ctx, spool, provider, agent, ownership,
								allProviderRootsCovered ||
									reconciliationProofCoversContainerMembership(
										scope.proofScopes, ownership.FilePath,
									),
							)
							if checkErr != nil {
								return deleted, checkErr
							}
							if !gone {
								continue
							}
							if !allProviderRootsCovered {
								// Membership authority came from the scope's own
								// container proof; a same-ID copy under another
								// configured root would make this a move, not a
								// deletion.
								relocated, err := reconciliationMemberRelocated(
									ctx, provider, ownership.ID,
								)
								if err != nil {
									return deleted, err
								}
								if relocated {
									continue
								}
							}
							// The container still exists but the streamed pass no
							// longer yields this member; tombstone directly — the
							// guards below all assume a vanished stored path.
							changed, err := e.tombstoneSessionSourceOwnership(
								ctx, ownership.Machine, ownership.Agent,
								ownership.ID, ownership.FilePath,
							)
							if err != nil {
								return deleted, fmt.Errorf(
									"tombstone %s session %s after watch reconciliation: %w",
									agent, ownership.ID, err,
								)
							}
							if changed {
								deleted++
							}
							continue
						}
						// A vanished tracked copy with a surviving same-identity
						// duplicate is a replacement, not a deletion; the next sync
						// re-points the session at the survivor. Claude keys
						// replacements by the session ID in the filename; a Codex
						// UUID can exist as both a live dated copy and a flat
						// archived copy sharing one discovery identity. Both resolve
						// through a bounded per-source index lookup: the streamed
						// pass's spool when it covers every configured root, else a
						// lazily built disk-backed index (at most one per pass).
						replacementIdentity := reconciliationReplacementIdentity(
							agent, ownership.FilePath,
						)
						if replacementIdentity != "" && provider != nil {
							if replacementIndex == nil {
								if spool != nil {
									if !allProviderRootsCovered {
										// A scoped pass cannot prove that a same-identity
										// replacement does not exist under another configured root.
										continue
									}
									replacementIndex = spool
								} else {
									// Deliberate bypass of the pass's narrowed proof:
									// the index must span the provider's full
									// configured scope so a replacement beyond the
									// narrowed pass stays resolvable.
									replacementIndex, err = e.buildReconciliationReplacementIndex(
										ctx, provider, e.agentDirs[agent],
									)
									if err != nil {
										return deleted, fmt.Errorf(
											"index %s reconciliation replacements: %w", agent, err,
										)
									}
									if agent == parser.AgentCodex {
										reconciliationRuntimeMetricsFor(ctx).
											codexReplacementIndexBuild()
									}
									ownsReplacementIndex = true
									defer func() {
										if ownsReplacementIndex {
											retErr = errors.Join(
												retErr, replacementIndex.CloseAndRemove(),
											)
										}
									}()
								}
							}
							replacement, found, lookupErr := replacementIndex.Candidate(
								ctx, agent, replacementIdentity,
							)
							if lookupErr != nil {
								return deleted, fmt.Errorf(
									"lookup %s reconciliation replacement: %w", agent, lookupErr,
								)
							}
							if found && !sameReconciliationSourcePath(
								replacement.Path, ownership.FilePath,
							) {
								continue
							}
						}
						persistentArchive := provider != nil && provider.Capabilities().Source.PersistentArchive == parser.CapabilitySupported
						if persistentArchive {
							resolver, ok := provider.(parser.PersistentArchiveSourceResolver)
							if ok {
								physicalPath, valid := resolver.PersistentArchiveSource(
									statPath, ownership.ID,
								)
								if valid {
									if !allProviderRootsCovered &&
										!reconciliationProofCoversContainerMembership(
											scope.proofScopes, physicalPath,
										) {
										// A scoped pass cannot prove that this member does not
										// exist in a persistent container under another root —
										// unless the completed scope's proof is this container's
										// whole virtual membership, in which case the admitted
										// stream enumerated exactly this container and spool
										// absence is authoritative for rows bound to it.
										continue
									}
									if _, statErr := e.lstatSource(physicalPath); statErr != nil {
										// A vanished or unreadable persistent container cannot
										// authoritatively prove that an archived member was deleted.
										continue
									}
									if spool == nil {
										continue
									}
									present, lookupErr := spool.ContainsSource(
										ctx, agent,
										canonicalReconciliationSourceIdentity(ownership.FilePath),
									)
									if lookupErr != nil {
										return deleted, fmt.Errorf(
											"lookup %s persistent member %s: %w",
											agent, ownership.FilePath, lookupErr,
										)
									}
									if !present {
										// Container-granular discovery spools the
										// still-present container itself rather than
										// each member. A discovered container accounts
										// for its members: deleting a member changes
										// the container's bytes, so the
										// fingerprint-gated complete-result parse
										// already tombstoned it.
										present, lookupErr = spool.ContainsSource(
											ctx, agent,
											canonicalReconciliationSourceIdentity(physicalPath),
										)
										if lookupErr != nil {
											return deleted, fmt.Errorf(
												"lookup %s persistent container %s: %w",
												agent, physicalPath, lookupErr,
											)
										}
									}
									if present {
										continue
									}
									persistentMemberContainerExists = true
								}
							}
						} else if resolver, ok := provider.(parser.ReconciliationSourceResolver); ok {
							source, found, err := resolver.SourceForReconciliation(
								ctx, statPath, "",
							)
							if err != nil {
								return deleted, fmt.Errorf(
									"validate %s source %s during watch reconciliation: %w",
									agent, statPath, err,
								)
							}
							if found {
								container, _, virtual := parser.ParseVirtualSourcePath(
									providerDiscoveredPath(source),
								)
								if virtual && !allProviderRootsCovered &&
									!reconciliationProofCoversContainerMembership(
										scope.proofScopes, container,
									) {
									// A scoped pass cannot prove that the same logical
									// member did not move to another configured root —
									// unless the completed scope's proof spans this
									// container's whole virtual membership; the
									// relocation guard before the tombstone below
									// then rules out a copy under another root.
									continue
								}
								if spool == nil || !virtual {
									continue
								}
								identity := ""
								identityResolver, resolvesIdentity := provider.(parser.ReconciliationMemberIdentityResolver)
								if resolvesIdentity {
									identity = identityResolver.ReconciliationMemberIdentity(
										ownership.ID,
									)
								}
								present, lookupErr := spool.ContainsSourceIdentity(
									ctx, agent,
									canonicalReconciliationSourceIdentity(ownership.FilePath),
									identity,
								)
								if lookupErr != nil {
									return deleted, fmt.Errorf(
										"lookup %s virtual member %s: %w",
										agent, ownership.FilePath, lookupErr,
									)
								}
								if present {
									continue
								}
							}
						}
						if !persistentMemberContainerExists {
							missing, ok = missingByPath[statPath]
							if !ok {
								_, statErr := e.lstatSource(statPath)
								missing = os.IsNotExist(statErr)
								missingByPath[statPath] = missing
							}
							if !missing {
								continue
							}
						}
						if !allProviderRootsCovered {
							// A scoped pass never streamed the other configured
							// roots, so before tombstoning a virtual member —
							// whose home container may itself be gone — ask the
							// provider across its full configured scope whether
							// the session still resolves anywhere: a same-ID copy
							// under another root is a move, not a deletion.
							if _, _, virtual := parser.ParseVirtualSourcePath(
								ownership.FilePath,
							); virtual {
								relocated, err := reconciliationMemberRelocated(
									ctx, provider, ownership.ID,
								)
								if err != nil {
									return deleted, err
								}
								if relocated {
									continue
								}
							}
						}
						changed, err := e.tombstoneSessionSourceOwnership(
							ctx, ownership.Machine, ownership.Agent,
							ownership.ID, ownership.FilePath,
						)
						if err != nil {
							return deleted, fmt.Errorf(
								"tombstone %s session %s after watch reconciliation: %w",
								agent, ownership.ID, err,
							)
						}
						if changed {
							deleted++
						}
					}
					cursor = page[len(page)-1].Cursor()
					if len(page) < db.WatchReconcileSourcePageSize {
						queryIndex++
						cursor = db.SessionSourceCursor{}
					}
				}
			}
		}
	}
	return deleted, nil
}

// canonicalProviderStatHashAgent maps an AgentType key to the canonical
// agent used by the provider_freshness side-table. The side-table is
// only ever written by recordProviderStatHash via the
// pendingProviderStatHash.agent field, which is set at the staging site
// from the discovered source's AgentType — always AgentCodebuff in the
// current registry because the Codebuff provider is the sole provider
// registered with MultiFileStatHash (Freebuff sessions surface with
// agent=AgentCodebuff per parser.AgentLabel routing, see
// buildProviderStatHashers). The storage-layer ownership rows, however,
// can carry agent=AgentFreebuff (the watcher/reconcile path may label a
// Freebuff-on-disk session with the AgentFreebuff literal, while the
// side-table provenance still rooted at the Codebuff probe). Reading
// the side-table at any site that takes ownership.Agent must therefore
// normalize Freebuff to Codebuff; without this, a tombstone on a
// Freebuff-tagged ownership row would silently miss the side-table row
// that the cold sync stamped under the Codebuff key, and a future
// byte-identical restore of the directory would falsely match the
// stale digest.
func canonicalProviderStatHashAgent(agent string) parser.AgentType {
	a := parser.AgentType(agent)
	if a == parser.AgentFreebuff {
		return parser.AgentCodebuff
	}
	return a
}

func (e *Engine) tombstoneSessionSourceOwnership(
	ctx context.Context, machine, agent, id, filePath string,
) (bool, error) {
	// Clear durable freshness state first. If this fails, leave the session
	// active so a later reconciliation can retry the cache invalidation and
	// tombstone as one recoverable operation.
	if _, err := e.clearSkipPersistent(filePath); err != nil {
		return false, fmt.Errorf("clear source skip cache: %w", err)
	}
	if _, err := e.clearSkipPersistent(providerAgentSkipCacheKey(
		filePath, parser.AgentType(agent),
	)); err != nil {
		return false, fmt.Errorf("clear agent source skip cache: %w", err)
	}
	// Also drop the per-component provider_freshness row under the same
	// (agent, filePath) key. Freebuff sessions surface in storage with
	// agent=AgentFreebuff but the provider_freshness side-table is only
	// ever stamped under the canonical AgentCodebuff key (the sole
	// MultiFileStatHasher provider); canonicalProviderStatHashAgent
	// remaps Freebuff to Codebuff here so the delete matches the row the
	// cold-sync staging site wrote. If this row is left intact and the
	// same physical directory is later restored byte-for-byte, the stale
	// digest would short-circuit providerSourceFreshBeforeFingerprint on
	// the next warm pass and silently skip the source, preventing the
	// tombstoned row from being revived by reconciliation.
	if err := e.db.DeleteProviderStatHash(
		ctx, canonicalProviderStatHashAgent(agent), filePath,
	); err != nil {
		return false, fmt.Errorf("clear provider_freshness: %w", err)
	}
	changed, err := e.db.SoftDeleteSessionSourceOwnership(
		ctx, machine, agent, id, filePath,
	)
	if err != nil || !changed {
		return changed, err
	}
	// The skip family was removed before the database transition. Drop the
	// remaining source trust so a byte-identical return is reverified and can
	// revive the tombstoned row.
	e.invalidateVerifiedSource(parser.AgentType(agent), filePath)
	return true, nil
}

// reconcileSourceMissingMembers applies the per-member CWD decision shared by
// batch and single-session writes. A member without deletion proof cannot be
// tombstoned yet, so baseline records only that exact admitted ownership; the
// caller persists those records at the correct point in its write ordering.
func (e *Engine) reconcileSourceMissingMembers(
	ctx context.Context,
	agent parser.AgentType,
	members []sourceMissingMember,
	baseline func(db.SessionSourceOwnership),
	reject func(db.SessionSourceOwnership),
) (int, []sourceMissingMember, error) {
	admitted := make([]bool, len(members))
	for i, member := range members {
		allowed, err := e.missingMemberTombstoneAllowed(
			ctx, member.sessionID,
		)
		if err != nil {
			return 0, nil, err
		}
		if allowed {
			admitted[i] = true
			continue
		}
		if reject != nil {
			reject(db.SessionSourceOwnership{
				Machine:  member.machine,
				Agent:    string(agent),
				ID:       member.sessionID,
				FilePath: member.filePath,
			})
		}
	}

	tombstoned := 0
	var deferred []sourceMissingMember
	for i, member := range members {
		if !admitted[i] {
			continue
		}
		ownership := db.SessionSourceOwnership{
			Machine:  member.machine,
			Agent:    string(agent),
			ID:       member.sessionID,
			FilePath: member.filePath,
		}
		if e.archiveStore != nil {
			replacement, err := e.db.GetSession(ctx, member.sessionID)
			if err != nil {
				return tombstoned, deferred, fmt.Errorf(
					"read replacement source-missing member %s: %w",
					member.sessionID, err,
				)
			}
			if replacement == nil {
				deferred = append(deferred, member)
				continue
			}
		}
		changed, err := e.tombstoneSessionSourceOwnership(
			ctx, member.machine, string(agent),
			member.sessionID, member.filePath,
		)
		if err != nil {
			return tombstoned, deferred, err
		}
		if changed {
			tombstoned++
			continue
		}
		if baseline != nil {
			baseline(ownership)
		}
	}
	return tombstoned, deferred, nil
}

// reconcileCopiedSourceMissingMembers completes rebuild-time reconciliation
// after orphan copy has materialized archive-only rows in the replacement.
// Exact baselines copied from the archive authorize an immediate guarded
// tombstone.
// Baseline-free legacy rows remain active for this pass but gain exact proof,
// preserving the normal two-pass upgrade safety rule.
func (e *Engine) reconcileCopiedSourceMissingMembers(
	ctx context.Context,
	target *db.DB,
	archivePath string,
	members []sourceMissingMember,
) (int, error) {
	tombstoned := 0
	ownerships := make([]db.SessionSourceOwnership, 0, len(members))
	for _, member := range members {
		ownerships = append(ownerships, db.SessionSourceOwnership{
			ID:       member.sessionID,
			Machine:  member.machine,
			Agent:    string(parser.AgentClaude),
			FilePath: member.filePath,
		})
	}
	if err := target.CopySessionSourceOwnershipBaselinesFrom(
		ctx, archivePath, ownerships,
	); err != nil {
		return 0, fmt.Errorf(
			"copy admitted source-missing baselines: %w", err,
		)
	}
	var needsBaseline []db.SessionSourceOwnership
	for i, member := range members {
		e.clearSkipInMemory(member.filePath)
		e.clearSkipInMemory(providerAgentSkipCacheKey(
			member.filePath, parser.AgentClaude,
		))
		if err := target.DeleteProviderStatHash(
			ctx, parser.AgentClaude, member.filePath,
		); err != nil {
			return tombstoned, fmt.Errorf(
				"clear copied source freshness for %s: %w",
				member.sessionID, err,
			)
		}
		changed, err := target.SoftDeleteSessionSourceOwnership(
			ctx, member.machine, string(parser.AgentClaude),
			member.sessionID, member.filePath,
		)
		if err != nil {
			return tombstoned, fmt.Errorf(
				"tombstone copied source-missing member %s: %w",
				member.sessionID, err,
			)
		}
		if changed {
			tombstoned++
			e.invalidateVerifiedSource(parser.AgentClaude, member.filePath)
			continue
		}
		needsBaseline = append(needsBaseline, ownerships[i])
	}
	if err := target.BaselineActiveSessionSourceOwnerships(
		ctx, needsBaseline,
	); err != nil {
		return tombstoned, fmt.Errorf(
			"baseline copied source-missing members: %w", err,
		)
	}
	return tombstoned, nil
}

func (e *Engine) buildReconciliationReplacementIndex(
	ctx context.Context, provider parser.Provider, configuredRoots []string,
) (result reconciliationSpoolStore, retErr error) {
	discoverer, ok := provider.(parser.StreamingDiscoverer)
	if !ok || provider.Capabilities().Source.StreamingDiscovery != parser.CapabilitySupported {
		return nil, errors.New("provider does not support streaming discovery")
	}
	watchRoots, err := parser.ResolveWatchRoots(ctx, provider)
	if err != nil {
		return nil, err
	}
	spool, err := e.reconciliationSpoolFactory(e.db.Path())
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			retErr = errors.Join(retErr, spool.CloseAndRemove())
		}
	}()
	err = discoverer.DiscoverEach(ctx, func(source parser.SourceRef) error {
		candidate, admitted := e.reconciliationCandidate(
			provider, source, configuredRoots, watchRoots,
		)
		if !admitted {
			return nil
		}
		return spool.Add(ctx, candidate)
	})
	if err != nil {
		return nil, err
	}
	keep = true
	return spool, nil
}

func (e *Engine) lstatSource(path string) (os.FileInfo, error) {
	if e.lstat != nil {
		return e.lstat(path)
	}
	return os.Lstat(path)
}

func isRemoteReconciliationRoot(root string) bool {
	return strings.HasPrefix(strings.ToLower(root), "s3://")
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
		if stats.hasSessionChanges() {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	stats = e.syncAllLocked(
		ctx, onProgress, since, nil, syncWriteDefault, true, false,
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
		if stats.hasSessionChanges() {
			e.emit("sessions")
		}
	}()
	defer e.syncMu.Unlock()
	defer e.clearCurrentProgress()
	scope := newRootSyncScope(roots)
	stats = e.syncAllLocked(
		ctx, onProgress, since, scope, syncWriteDefault, scope == nil, false,
	)
	return
}

type rootSyncScope struct {
	roots []string
	// agent, when non-empty, restricts discovery to a single provider so a
	// scoped reconciliation cannot drag other providers into the pass through
	// ancestor/descendant root overlap. Zero value matches every agent.
	agent parser.AgentType
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

// matchesAgent reports whether the given provider is in scope. An unscoped
// scope (nil) or one without an agent filter matches every provider; an
// agent-filtered scope matches only that provider.
func (s *rootSyncScope) matchesAgent(agent parser.AgentType) bool {
	if s == nil || s.agent == "" {
		return true
	}
	return s.agent == agent
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
	forceDiscoveredFiles bool,
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

	// A whole-archive pass (resync rebuild, full/initial sync, remote
	// import) is bulk work: it parses at full parallelism and frees its
	// retained memory once at the end. Cutoff- or root-scoped passes are
	// steady-state daemon churn and keep the bounded retention budget.
	if writeMode == syncWriteBulk || (since.IsZero() && scope == nil) {
		defer e.beginBulkRetentionPass()()
	}

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

	// Container states must be captured BEFORE discovery lists any session
	// rows, so a promoted state can never be newer than the discovered
	// session set (see captureSQLiteContainerStates).
	preContainerStates := e.captureSQLiteContainerStates(nil)

	var all []parser.DiscoveredFile
	counts := make(map[parser.AgentType]int)
	providerFound, providerFailures := e.discoverProviderSources(
		ctx, scope, preContainerStates,
	)
	for _, file := range providerFound {
		counts[file.Agent]++
	}
	all = append(all, providerFound...)

	verifiedPass := uint64(0)
	verifiedPassFinished := false
	if scope == nil && e.pathRewriter == nil {
		verifiedPass = e.beginVerifiedSourcePass()
		e.markVerifiedDiscoveredSources(providerFound)
		defer func() {
			if !verifiedPassFinished {
				e.finishVerifiedSourcePass(verifiedPass, false)
			}
		}()
	}

	// Begin gate bookkeeping from the pre-filter discovery set: promotion
	// needs a completion for every discovered session, so a cutoff-filtered
	// pass must stay unpromotable (see opencode_container_gate.go).
	e.beginSQLiteContainerPass(providerFound, preContainerStates)

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
	if forceDiscoveredFiles {
		for i := range all {
			all[i].ForceParse = true
		}
	}

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
	stats.providerFailures = providerFailures
	for range providerFailures {
		stats.RecordFailed()
	}
	// Discovery failures cannot be attributed to a provider here, so any
	// failure conservatively blocks every container promotion this pass.
	// Only unscoped passes discovered every root, so only they may drop
	// trusted entries for containers that produced no sources.
	e.finishSQLiteContainerPass(
		stats.Aborted || ctx.Err() != nil || providerFailures > 0,
		scope == nil,
	)
	if verifiedPass != 0 {
		e.finishVerifiedSourcePass(
			verifiedPass,
			!stats.Aborted && ctx.Err() == nil && providerFailures == 0,
		)
		verifiedPassFinished = true
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

	advanceDBProgress := func(total, indexedMessages int) {
		if total == 0 {
			return
		}
		progressTotal += total
		dbProgress.SessionsTotal = progressTotal
		dbProgress.SessionsDone += total
		dbProgress.MessagesIndexed += indexedMessages
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

	// Sync Warp, Forge, Piebald, ZCode, and Goose sessions. These are
	// provider-authoritative DB-backed providers: a shared SQLite DB hosts every
	// session, so the provider facade enumerates sources and parses only the
	// changed ones.
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
	if scope.includesAny(e.agentDirs[parser.AgentZCode]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentZCode, "zcode",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.agentDirs[parser.AgentGoose]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentGoose, "goose",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	// Link subagent child sessions to their parents after all DB-backed
	// agent writes (including provider-authoritative Forge, Goose, Piebald,
	// and ZCode).
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

	if recordSyncState && stats.providerFailures == 0 {
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
	preContainerStates map[string]parser.SQLiteContainerState,
) ([]parser.DiscoveredFile, int) {
	var files []parser.DiscoveredFile
	var failures int
	containerTrusted := e.sqliteContainerTrustedForDiscovery(preContainerStates)

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
		if !scope.matchesAgent(agentType) {
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
			Roots:                              filteredRoots,
			Machine:                            e.machine,
			SourceMachines:                     e.sourceMachines[agentType],
			PathRewriter:                       e.pathRewriter,
			SQLiteContainerUnchangedSinceTrust: containerTrusted,
		})
		// Shared-database providers are streamed source-by-source by their
		// dedicated sync phase. Calling Discover here would build an archive-sized
		// slice only to discard it, then traverse the same database again.
		if processFileUsesProvider(agentType) {
			continue
		}
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
		forceParseSource := func(string) bool { return false }
		if agentType == parser.AgentVSCopilot {
			// Only Visual Studio Copilot recovery consumes the discovered
			// path set; building it for every provider would allocate an
			// archive-sized map per full discovery.
			currentSources := providerSourcePathSet(sources)
			missingSources, forceParseSources :=
				e.visualStudioCopilotMissingVS2026PollSources(
					ctx, provider, filteredRoots, currentSources,
				)
			sources = append(sources, missingSources...)
			forceParseSource = func(sourcePath string) bool {
				_, ok := forceParseSources[filepath.Clean(sourcePath)]
				return ok
			}
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
			if !isS3SourcePath(sourcePath) {
				discovered.Machine = e.machineForProviderSource(
					agent, source, sourcePath,
				)
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
	var out []parser.SourceRef
	seenHints := make(map[string]struct{})
	forceParseSources := make(map[string]struct{})
	watchRoots, err := e.providerChangedPathWatchRoots(
		ctx, parser.AgentVSCopilot, provider, roots,
	)
	if err != nil {
		log.Printf("%s provider poll watch roots: %v", parser.AgentVSCopilot, err)
		return out, forceParseSources
	}
	for _, watchRoot := range watchRoots {
		hints, err := e.db.ListStoredSourcePathHints(
			string(parser.AgentVSCopilot), []db.StoredSourcePathHintScope{{Path: watchRoot}},
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
// discovered Codex-format source. The provider deduplicates a UUID's live and
// archived copies to the preferred layout at discovery time; this restores the
// dropped duplicates (scoped to the configured roots) so an mtime cutoff filter
// can judge each copy on its own mtime, matching the legacy discover-then-filter
// order. Files of other agents, and Codex-format files without a UUID-shaped
// name, pass through unchanged. Duplicates are keyed by path so nothing is added
// twice. Each agent is expanded against its own roots and re-added under its own
// identity: a fork's UUID must not be resolved through the Codex provider.
func (e *Engine) expandCodexProviderDuplicates(
	files []parser.DiscoveredFile, scope *rootSyncScope,
) []parser.DiscoveredFile {
	pathers := make(map[parser.AgentType]func(string) []string)
	seen := make(map[string]struct{}, len(files))
	for _, f := range files {
		seen[string(f.Agent)+"\x00"+filepath.Clean(f.Path)] = struct{}{}
	}
	out := files
	for _, f := range files {
		if !isCodexFormatAgent(f.Agent) {
			continue
		}
		pather, resolved := pathers[f.Agent]
		if !resolved {
			pather = e.codexUUIDPathLister(f.Agent, scope)
			pathers[f.Agent] = pather
		}
		if pather == nil {
			continue
		}
		uuid := parser.CodexSessionUUIDFromFilename(filepath.Base(f.Path))
		if uuid == "" {
			continue
		}
		for _, dup := range pather(uuid) {
			key := string(f.Agent) + "\x00" + filepath.Clean(dup)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, parser.DiscoveredFile{
				Path:            dup,
				Agent:           f.Agent,
				Machine:         e.machineForPath(f.Agent, dup),
				ProviderProcess: true,
				ProviderSource:  e.codexPinnedProviderSource(f.Agent, dup),
			})
		}
	}
	return out
}

// codexUUIDPathLister returns a function that lists every on-disk transcript
// path of the given Codex-format agent for a UUID under the in-scope roots, or
// nil when that provider is unavailable. It scopes a single provider to the
// in-scope roots so the returned paths cover both the live dated and flat
// archived copies of a duplicated UUID, including duplicates that share one
// root.
func (e *Engine) codexUUIDPathLister(
	agent parser.AgentType, scope *rootSyncScope,
) func(string) []string {
	factory, ok := e.providerFactories[agent]
	if !ok || factory == nil {
		return nil
	}
	roots := make([]string, 0, len(e.agentDirs[agent]))
	for _, root := range e.agentDirs[agent] {
		if root == "" || !scope.includes(root) {
			continue
		}
		roots = append(roots, root)
	}
	if len(roots) == 0 {
		return nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:          roots,
		Machine:        e.machine,
		SourceMachines: e.sourceMachines[agent],
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
	staleIdentities := e.staleDataVersionIdentitySet()
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
		// The bypass probes below cost a DB read per file, so they run only
		// for files the cutoff would otherwise drop.
		var sourceCwd sourceCwdDecision
		sourceCwdParticipating := false
		if f.ProviderSource != nil {
			sourceCwd = e.sourceCwdDecision(*f.ProviderSource)
			sourceCwdParticipating = sourceCwdParticipates(
				sourceCwd.resolution,
			)
			if sourceCwd.forceParse {
				// A parser-declared workspace change is parse-affecting even
				// when the transcript's own mtime predates the cutoff.
				out = append(out, f)
				continue
			}
		}
		if !isS3SourcePath(f.Path) &&
			e.pathInStaleIdentitySet(staleIdentities, f.Agent, f.Path) &&
			e.staleSourceReparseAdmitted(
				sourceCwdParticipating, sourceCwd,
			) {
			// Cwd-only reconciliation invalidates rows that must bypass mtime cutoff.
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

// codebuffCompositeMtime returns a composite freshness timestamp for a
// Codebuff/Freebuff session directory by stat'ing the primary file
// (chat-messages.json), every companion file declared in
// CodebuffCompanionFilenames (run-state.json, chat-meta.json), and the
// session directory itself. Each stat contributes max(mtime, ctime), so
// a companion-only change or a same-size rewrite with preserved mtime
// still advances the composite. Shared by discoveredFileEffectiveMtime
// (incremental cutoff) and discoveredFileMtime (parse-diff ordering) so
// the two freshness signals never diverge.
func codebuffCompositeMtime(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	mtime := info.ModTime().UnixNano()
	// Include ctime so same-size rewrites with preserved mtime are
	// detected. On platforms without ctime (e.g. Plan 9), fileChangeTime
	// returns (0, false), leaving the mtime-only composite as-is.
	if ct, _ := fileChangeTime(path, info); ct > mtime {
		mtime = ct
	}
	dir := filepath.Dir(path)
	for _, name := range parser.CodebuffCompanionFilenames {
		companion := filepath.Join(dir, name)
		if ci, err := os.Stat(companion); err == nil {
			ts := ci.ModTime().UnixNano()
			if cct, _ := fileChangeTime(companion, ci); cct > ts {
				ts = cct
			}
			if ts > mtime {
				mtime = ts
			}
		}
	}
	// Also consider the session directory mtime as a local cutoff
	// signal to detect companion-file deletions. Deleting a file
	// changes the directory's mtime even though surviving files'
	// mtimes are unchanged.
	if dirInfo, err := os.Stat(dir); err == nil {
		ts := dirInfo.ModTime().UnixNano()
		if dct, _ := fileChangeTime(dir, dirInfo); dct > ts {
			ts = dct
		}
		if ts > mtime {
			mtime = ts
		}
	}
	return mtime, nil
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
	// Codex-format forks take the same branch: their fingerprint carries no
	// index component, so the raw mtime is the same value at lower cost.
	if isCodexFormatAgent(file.Agent) {
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
	// RooCode is excluded from the provider-Fingerprint path for cost, not
	// correctness: its Fingerprint content-hashes history_item.json plus
	// ui_messages.json, so consulting it here would read every task's full
	// transcript on each incremental sync, scaling cutoff filtering with
	// the archive instead of the changed batch. The stat-only composite
	// carries the same cutoff signal — the max mtime of both files — so a
	// sibling-only transcript append still looks fresh. Sources that pass
	// the cutoff go on to the full fingerprint as usual.
	if file.Agent == parser.AgentRooCode {
		info, err := os.Stat(file.Path)
		if err != nil {
			return 0, err
		}
		_, mtime := roocodeEffectiveStat(file.Path, info)
		return mtime, nil
	}
	// Kilo Legacy is excluded from the provider-Fingerprint path for
	// cost, not correctness: its Fingerprint content-hashes all three
	// session files, so consulting it here would read every task's full
	// transcript on each incremental sync, scaling cutoff filtering with
	// the archive instead of the changed batch. The stat-only composite
	// carries the same cutoff signal — the max mtime of all three files —
	// so a sibling-only transcript append still looks fresh. Sources that
	// pass the cutoff go on to the full fingerprint as usual.
	if file.Agent == parser.AgentKiloLegacy {
		info, err := os.Stat(file.Path)
		if err != nil {
			return 0, err
		}
		_, mtime := kiloLegacyEffectiveStat(file.Path, info)
		// Also consider the task directory mtime as a local cutoff
		// signal to detect companion-file deletions. Deleting a file
		// changes the directory's mtime even though surviving files'
		// mtimes are unchanged. This is a local-only signal that does
		// not affect the persisted fingerprint.
		dir := filepath.Dir(file.Path)
		if dirInfo, err := os.Stat(dir); err == nil {
			if ts := dirInfo.ModTime().UnixNano(); ts > mtime {
				mtime = ts
			}
		}
		return mtime, nil
	}
	// Codebuff is excluded from the provider-Fingerprint path for
	// cost: its Fingerprint content-hashes chat-messages.json plus
	// run-state.json and chat-meta.json, so consulting it here would
	// read every session's full transcript on each incremental sync,
	// scaling cutoff filtering with the archive instead of the changed
	// batch. The stat-only composite carries the same cutoff signal:
	// max(mtime, ctime) per file plus the session directory, so a
	// companion-only change still looks fresh and a same-size rewrite
	// with preserved mtime advances the cutoff via ctime. Sources that
	// pass the cutoff go on to the full fingerprint as usual.
	// Codebuff/Freebuff: stat-only composite avoids the provider
	// Fingerprint path (which content-hashes the transcript) to keep
	// cutoff filtering bounded by the changed batch. See the helper's
	// doc comment for what the composite covers.
	if file.Agent == parser.AgentCodebuff || file.Agent == parser.AgentFreebuff {
		return codebuffCompositeMtime(file.Path)
	}
	// Watermark-only shared-container sources carry their session-row
	// watermark from discovery. Consulting the provider Fingerprint instead
	// would resolve the full composite with one indexed child lookup per
	// session, scaling cutoff filtering with the container instead of the
	// changed batch — and these sources are only listed for containers that
	// provably have not changed since their last verified pass, where the
	// carried watermark and the composite are equally stale. That proof is
	// the pass's container capture: a container that changed between the
	// listing and the recapture check may have advanced a session past its
	// carried watermark, so the stale value cannot decide the cutoff — fall
	// through and resolve the live composite instead.
	if file.ProviderSource != nil {
		if wm, ok := parser.SourceWatermarkOnlyMTimeNS(*file.ProviderSource); ok {
			if dbPath, _, ok := sqliteContainerSourceForFile(file); ok && e.sqliteContainerPassCaptureValid(dbPath) {
				return wm, nil
			}
		}
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
		Roots:        e.agentDirs[file.Agent],
		Machine:      e.machine,
		PathRewriter: e.pathRewriter,
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
		obj, err := statS3SourceObject(file)
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

	if file.Agent == parser.AgentCodebuff || file.Agent == parser.AgentFreebuff {
		return codebuffCompositeMtime(file.Path)
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
		obj, err := statS3SourceObject(parser.DiscoveredFile{
			Agent: parser.AgentClaude,
			Path:  file.Path,
		})
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

func (e *Engine) listActiveSessionSourceAttributions(
	ctx context.Context,
	sources []db.SessionSourcePath,
) ([]db.SessionSourceAttribution, error) {
	if e.sourceAttributionLookupOverride != nil {
		return e.sourceAttributionLookupOverride(ctx, sources)
	}
	return e.db.ListActiveSessionSourceAttributions(ctx, sources)
}

// expandSourceBaselinesByStoredAttribution applies one source-level admission
// decision to every immutable machine currently represented at that source.
// This is for no-write outcomes, where there is no normalized pending session
// to carry the stored machine through baseline replacement.
func (e *Engine) expandSourceBaselinesByStoredAttribution(
	ctx context.Context,
	candidates []machineSessionSource,
	admitted []machineSessionSource,
) ([]machineSessionSource, []machineSessionSource, error) {
	requestedSet := make(map[db.SessionSourcePath]struct{}, len(candidates))
	requested := make([]db.SessionSourcePath, 0, len(candidates))
	candidateSet := make(map[machineSessionSource]struct{}, len(candidates))
	for _, candidate := range candidates {
		candidateSet[candidate] = struct{}{}
		if candidate.Source.Agent == "" || candidate.Source.FilePath == "" {
			continue
		}
		if _, exists := requestedSet[candidate.Source]; exists {
			continue
		}
		requestedSet[candidate.Source] = struct{}{}
		requested = append(requested, candidate.Source)
	}
	if len(requested) == 0 {
		return candidates, admitted, nil
	}

	admittedSources := make(map[db.SessionSourcePath]struct{}, len(admitted))
	admittedSet := make(map[machineSessionSource]struct{}, len(admitted))
	for _, source := range admitted {
		admittedSources[source.Source] = struct{}{}
		admittedSet[source] = struct{}{}
	}
	attributions, err := e.listActiveSessionSourceAttributions(ctx, requested)
	if err != nil {
		return candidates, admitted, err
	}
	for _, attribution := range attributions {
		source := machineSessionSource{
			Machine: attribution.Machine,
			Source: db.SessionSourcePath{
				Agent: attribution.Agent, FilePath: attribution.FilePath,
			},
		}
		if _, requested := requestedSet[source.Source]; !requested {
			continue
		}
		if _, exists := candidateSet[source]; !exists {
			candidateSet[source] = struct{}{}
			candidates = append(candidates, source)
		}
		if _, sourceAdmitted := admittedSources[source.Source]; !sourceAdmitted {
			continue
		}
		if _, exists := admittedSet[source]; !exists {
			admittedSet[source] = struct{}{}
			admitted = append(admitted, source)
		}
	}
	return candidates, admitted, nil
}

// syncProviderDBBacked enumerates a DB-backed provider's sources, parses only
// the changed ones through the provider facade, and flushes each source before
// parsing the next. Change detection compares the provider fingerprint mtime
// against the stored source mtime and requires the stored data version to be
// current, reproducing the legacy *PendingSessionIDs behavior.
func (e *Engine) syncProviderDBBacked(
	ctx context.Context, agent parser.AgentType, scope *rootSyncScope,
	flush func([]pendingWrite) bool,
) (int, int, error) {
	roots := make([]string, 0, len(e.agentDirs[agent]))
	for _, dir := range e.agentDirs[agent] {
		if dir == "" || !scope.includes(dir) {
			continue
		}
		roots = append(roots, dir)
	}
	if len(roots) == 0 {
		return 0, 0, nil
	}
	factory, ok := e.providerFactories[agent]
	if !ok || factory == nil {
		return 0, 0, nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:          roots,
		Machine:        e.machine,
		SourceMachines: e.sourceMachines[agent],
	})
	discoverer, ok := provider.(parser.StreamingDiscoverer)
	if !ok || provider.Capabilities().Source.StreamingDiscovery != parser.CapabilitySupported {
		return 0, 0, fmt.Errorf("sync %s: provider lacks streaming discovery", agent)
	}
	baselines := make([]machineSessionSource, 0, reconciliationPageSize)
	flushBaselines := func() error {
		if len(baselines) == 0 {
			return nil
		}
		sources := make([]db.SessionSourcePath, 0, len(baselines))
		for _, candidate := range baselines {
			sources = append(sources, candidate.Source)
		}
		attributions, err := e.listActiveSessionSourceAttributions(ctx, sources)
		if err != nil {
			return fmt.Errorf(
				"load %s streaming source attributions: %w", agent, err,
			)
		}
		admitted := make([]machineSessionSource, 0, len(attributions))
		for _, attribution := range attributions {
			admitted = append(admitted, machineSessionSource{
				Machine: attribution.Machine,
				Source: db.SessionSourcePath{
					Agent: attribution.Agent, FilePath: attribution.FilePath,
				},
			})
		}
		if err := e.replaceActiveSessionSourceBaselinesByMachine(
			ctx, baselines, admitted,
		); err != nil {
			return fmt.Errorf("baseline %s streaming sources: %w", agent, err)
		}
		baselines = baselines[:0]
		return nil
	}
	queueBaseline := func(source parser.SourceRef) error {
		path := providerDiscoveredPath(source)
		if path == "" {
			return nil
		}
		storedPath := e.effectiveSourcePath(path)
		machine := e.machineForProviderSource(agent, source, path)
		baselines = append(baselines, machineSessionSource{
			Machine: machine,
			Source: db.SessionSourcePath{
				Agent: string(agent), FilePath: storedPath,
			},
		})
		if len(baselines) == reconciliationPageSize {
			return flushBaselines()
		}
		return nil
	}

	discovered, sourceFailures := 0, 0
	err := discoverer.DiscoverEach(ctx, func(source parser.SourceRef) error {
		discovered++
		fingerprint, err := provider.Fingerprint(ctx, source)
		if err != nil {
			log.Printf("sync %s fingerprint: %v", agent, err)
			sourceFailures++
			return nil
		}
		machine := e.machineForProviderSource(
			agent, source, providerDiscoveredPath(source),
		)
		if e.providerDBBackedSourceFresh(agent, source, fingerprint) {
			return queueBaseline(source)
		}
		outcome, err := provider.Parse(ctx, parser.ParseRequest{
			Source:      source,
			Fingerprint: fingerprint,
			Machine:     machine,
			ForceParse:  e.forceParse || e.forceFullParse,
		})
		if err != nil {
			log.Printf("sync %s parse: %v", agent, err)
			sourceFailures++
			return nil
		}
		complete := providerOutcomeAllowsCleanSkipCache(outcome)
		if !complete {
			sourceFailures++
		}
		pending := make([]pendingWrite, 0, len(outcome.Results))
		for _, result := range outcome.Results {
			pending = append(pending, pendingWrite{
				sess:        result.Result.Session,
				msgs:        result.Result.Messages,
				usageEvents: result.Result.UsageEvents,
				needsRetry:  !complete,
			})
		}
		if len(pending) > 0 && !flush(pending) {
			complete = false
		}
		if complete {
			if err := queueBaseline(source); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if err != nil {
		log.Printf("sync %s: %v", agent, err)
		return discovered, sourceFailures, err
	}
	if err := flushBaselines(); err != nil {
		log.Printf("sync %s: %v", agent, err)
		return discovered, sourceFailures, err
	}
	return discovered, sourceFailures, nil
}

// providerDBBackedSourceFresh reports whether a DB-backed provider source is
// already stored at the current data version with an unchanged source mtime
// and, when required by the provider, an unchanged content hash. This is the
// change-detection half of the legacy *PendingSessionIDs helpers.
func (e *Engine) providerDBBackedSourceFresh(
	agent parser.AgentType,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
) bool {
	if e.forceParse || e.forceFullParse {
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
	_, storedMtime, ok := e.db.GetFileInfoByAgentPath(
		lookupPath, string(agent),
	)
	if !ok {
		return false
	}
	if storedMtime != fingerprint.MTimeNS {
		return false
	}
	if factory, ok := e.providerFactories[agent]; ok && factory != nil &&
		!e.providerFingerprintHashMatchesDB(
			agent,
			lookupPath,
			fingerprint,
			factory.Capabilities().Sync.FingerprintHashRequiredForFreshness,
		) {
		return false
	}
	return e.db.GetDataVersionByAgentPath(
		lookupPath, string(agent),
	) >= db.CurrentDataVersion()
}

// syncProviderDBBackedAgent runs the full-sync phase for a provider-authoritative
// DB-backed agent (Forge, Goose, Piebald, Warp, ZCode). It mirrors
// syncOpenCodeFormatAgent:
// only changed sessions are parsed (so the second sync of unchanged data is a
// no-op), and the per-session write semantics match the legacy DB sync.
func (e *Engine) syncProviderDBBackedAgent(
	ctx context.Context, agent parser.AgentType, label string,
	writeMode syncWriteMode, verbose bool, scope *rootSyncScope,
	stats *SyncStats,
	advanceDBProgress func(total, indexedMessages int),
) bool {
	start := time.Now()
	useWorktreeResolver := agent != parser.AgentPiebald
	resolveWorktreeProject := e.loadWorktreeProjectResolver()
	var pendingCount, indexedMessages, written int
	var writeDuration time.Duration
	flush := func(pending []pendingWrite) bool {
		complete := true
		pendingCount += len(pending)
		for _, pw := range pending {
			indexedMessages += len(pw.msgs)
		}
		tWrite := time.Now()
		if writeMode == syncWriteBulk {
			var outcome writeBatchOutcome
			if e.writeBatchOverride != nil {
				batchWritten, batchMessages, failedWrites, cwdFiltered :=
					e.writeBatchOverride(pending, writeMode, true)
				outcome = writeBatchOutcome{
					writtenSessions: batchWritten,
					writtenMessages: batchMessages,
					failedSessions:  failedWrites,
					cwdFiltered:     cwdFiltered,
				}
				complete = failedWrites == 0 && cwdFiltered == 0 &&
					batchWritten == len(pending)
			} else {
				outcome = e.writeBatchWithOutcome(pending, writeMode, true)
				for _, wasWritten := range outcome.written {
					if !wasWritten {
						complete = false
						break
					}
				}
			}
			written += outcome.writtenSessions
			for range outcome.failedSessions {
				stats.RecordFailed()
			}
			stats.cwdFilteredSessions += outcome.cwdFiltered
		} else {
			for _, pw := range pending {
				if ctx.Err() != nil {
					complete = false
					break
				}
				var err error
				if useWorktreeResolver {
					err = e.writeSessionFullWithResolver(pw, resolveWorktreeProject)
				} else {
					err = e.writeSessionFull(pw)
				}
				switch {
				case err == nil:
					written++
				case isIntentionalSessionSkip(err), errors.Is(err, errSessionPreserved):
					// Intentional skip, not a failure.
					complete = false
				default:
					complete = false
					stats.RecordFailed()
				}
			}
		}
		writeDuration += time.Since(tWrite)
		return complete
	}
	discovered, sourceFailures, discoveryErr := e.syncProviderDBBacked(
		ctx, agent, scope, flush,
	)
	stats.providerFailures += sourceFailures
	for range sourceFailures {
		stats.RecordFailed()
	}
	if discoveryErr != nil {
		stats.providerFailures++
		stats.RecordFailed()
	}
	if pendingCount > 0 {
		stats.TotalSessions += pendingCount
		stats.RecordSynced(written)
		if verbose {
			log.Printf(
				"%s write: %d sessions in %s",
				label, pendingCount,
				writeDuration.Round(time.Millisecond),
			)
		}
	}
	if verbose {
		log.Printf(
			"%s sync: %s",
			label, time.Since(start).Round(time.Millisecond),
		)
	}
	advanceDBProgress(discovered, indexedMessages)
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
	// Cap fan-out and channel buffers by the batch: a single-path
	// watcher sync needs one worker and one result slot, not a
	// CPU-scaled pool with page-sized buffers of grown syncJob values.
	workers := min(e.workerCount(), max(len(files), 1))
	buffer := min(max(workers*2, 1), max(len(files), 1))

	jobs := make(chan parser.DiscoveredFile, buffer)
	results := make(chan syncJob, buffer)
	runtimeMetrics := reconciliationRuntimeMetricsFor(ctx)
	emitResult := func(result syncJob) {
		if runtimeMetrics != nil {
			runtimeMetrics.workerQueued(1)
		}
		results <- result
	}

	var wg gosync.WaitGroup
	for range workers {
		wg.Go(func() {
			for file := range jobs {
				if ctx.Err() != nil {
					emitResult(syncJob{
						err:     ctx.Err(),
						agent:   file.Agent,
						path:    file.Path,
						machine: e.machineForFile(file),
					})
					continue
				}
				result := e.processFile(ctx, file)
				emitResult(syncJob{
					processResult:  result,
					agent:          file.Agent,
					path:           file.Path,
					machine:        e.machineForFile(file),
					retentionLease: result.retentionLease,
				})
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

func (e *Engine) retentionBudget() *parseRetentionBudget {
	if active := e.activeRetention.Load(); active != nil {
		return active
	}
	e.parseRetentionOnce.Do(func() {
		if e.parseRetentionBudget == nil {
			e.parseRetentionBudget = newParseRetentionBudget(defaultParseRetentionBytes)
		}
	})
	return e.parseRetentionBudget
}

// beginBulkRetentionPass installs the unthrottled bulk retention budget for
// the duration of an archive-scale pass and returns the restore func the
// caller must defer. Bulk passes (full sync, resync rebuild, remote import
// processing) run at full worker parallelism; the memory they retain is
// returned to the OS by the end-of-pass scavenge instead of being bounded
// per source. The caller holds syncMu, so no other pass can observe the
// switched budget.
func (e *Engine) beginBulkRetentionPass() func() {
	e.bulkRetentionOnce.Do(func() {
		if e.bulkRetentionBudget == nil {
			e.bulkRetentionBudget = newBulkParseRetentionBudget()
		}
	})
	e.activeRetention.Store(e.bulkRetentionBudget)
	return func() { e.activeRetention.Store(nil) }
}

func (e *Engine) workerCount() int {
	workers := runtime.NumCPU()
	if e.workerCountOverride > 0 {
		workers = e.workerCountOverride
	}
	return min(max(workers, 2), maxWorkers)
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
	return e.collectAndBatchWithOptions(
		ctx, results, total, progressTotal, onProgress, writeMode,
		collectAndBatchOptions{},
	)
}

type collectAndBatchOptions struct {
	preserveMissingSources bool
	observeResult          func(syncJob)
}

func (e *Engine) collectAndBatchWithOptions(
	ctx context.Context,
	results <-chan syncJob, total int, progressTotal int,
	onProgress ProgressFunc,
	writeMode syncWriteMode,
	options collectAndBatchOptions,
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
	var pendingLeases []*parseRetentionLease
	var pendingCacheWrites []skipCacheWrite
	baselineCacheWrites := make(
		map[machineSessionSource]map[string]skipCacheWrite,
	)
	// Size baseline bookkeeping by the batch, capped at one flush page:
	// a single-path watcher sync must not pay for page-sized structures
	// (a 256-entry map of struct keys dominates the per-append B/op).
	// Appends and map inserts still grow past the hint when providers
	// fan one changed file out to multiple sessions.
	baselineHint := min(total, reconciliationPageSize)
	baselineCandidates := make([]machineSessionSource, 0, baselineHint)
	baselineAdmitted := make([]machineSessionSource, 0, baselineHint)
	baselineAdmission := make(map[machineSessionSource]bool, baselineHint)
	var exactBaselineOwnerships map[db.SessionSourceOwnership]struct{}
	var rejectedBaselineOwnerships map[db.SessionSourceOwnership]struct{}
	runtimeMetrics := reconciliationRuntimeMetricsFor(ctx)
	baselineSourceForJob := func(job syncJob) (machineSessionSource, bool) {
		source := machineSessionSource{
			Machine: job.machine,
			Source: db.SessionSourcePath{
				Agent: string(job.agent), FilePath: e.effectiveSourcePath(job.path),
			},
		}
		return source,
			source.Source.Agent != "" && source.Source.FilePath != ""
	}
	flushBaselineSources := func() {
		if len(baselineCandidates) == 0 {
			return
		}
		baselineAdmitted = baselineAdmitted[:0]
		for _, source := range baselineCandidates {
			if baselineAdmission[source] {
				baselineAdmitted = append(baselineAdmitted, source)
			}
		}
		// ReplaceActiveSessionSourceBaselines only reads baselineAdmitted
		// synchronously within this call (db.baselineActiveSessionSourcePathsTx
		// iterates it while binding statement params); it does not retain the
		// slice, so reusing the same backing array across flushes is safe.
		var cacheWrites []skipCacheWrite
		for _, source := range baselineCandidates {
			for _, write := range baselineCacheWrites[source] {
				cacheWrites = append(cacheWrites, write)
			}
			delete(baselineCacheWrites, source)
		}
		baselineCtx := ctx
		if ctx.Err() != nil &&
			(len(exactBaselineOwnerships) > 0 ||
				len(rejectedBaselineOwnerships) > 0) {
			baselineCtx = context.WithoutCancel(ctx)
		}
		resolvedCandidates, resolvedAdmitted, err :=
			e.expandSourceBaselinesByStoredAttribution(
				baselineCtx, baselineCandidates, baselineAdmitted,
			)
		exactOwnerships := matchingBaselineOwnerships(
			resolvedCandidates, exactBaselineOwnerships,
		)
		rejectedOwnerships := matchingBaselineOwnerships(
			resolvedCandidates, rejectedBaselineOwnerships,
		)
		if err == nil {
			err = e.replaceActiveSessionSourceBaselinesWithExceptionsByMachine(
				baselineCtx, resolvedCandidates, resolvedAdmitted,
				exactOwnerships, rejectedOwnerships,
			)
		}
		if err != nil {
			log.Printf("replace successful non-write source baselines: %v", err)
			stats.RecordFailed()
			e.poisonSQLiteContainerPass()
			e.rejectSkipCacheWrites(cacheWrites)
		} else {
			consumeBaselineOwnerships(
				exactBaselineOwnerships, exactOwnerships,
			)
			consumeBaselineOwnerships(
				rejectedBaselineOwnerships, rejectedOwnerships,
			)
			e.promoteSkipCacheWrites(cacheWrites)
		}
		baselineCandidates = baselineCandidates[:0]
		clear(baselineAdmission)
	}
	baselineProcessedSource := func(job syncJob, admitted bool) {
		// Use the parsed session's agent type for the baseline row,
		// not the provider's agent type. Freebuff sessions are
		// stored under agent=AgentFreebuff but discovered under
		// Codebuff, so the baseline row must key on the resolved
		// agent. Skip-cache staging (stageNoWriteCache) still uses
		// job.agent because the skip cache is per-file, not
		// per-session-agent.
		agent := job.agent
		if len(job.results) > 0 {
			if sessAgent := job.results[0].Session.Agent; sessAgent != "" {
				agent = sessAgent
			}
		} else if job.agent == parser.AgentCodebuff {
			// For skipped sources with no results, query the DB for
			// the actual agent type. Freebuff sessions are stored as
			// AgentFreebuff but discovered under Codebuff.
			path := e.effectiveSourcePath(job.path)
			for _, candidateAgent := range []parser.AgentType{
				parser.AgentCodebuff, parser.AgentFreebuff,
			} {
				if ids, err := e.db.ListSessionIDsByFilePath(path, string(candidateAgent)); err == nil && len(ids) > 0 {
					agent = candidateAgent
					break
				}
			}
		}
		source := machineSessionSource{
			Machine: job.machine,
			Source: db.SessionSourcePath{
				Agent: string(agent), FilePath: e.effectiveSourcePath(job.path),
			},
		}
		if source.Source.Agent == "" || source.Source.FilePath == "" {
			return
		}
		if tracker := reconciliationBaselineTrackerFor(ctx); tracker != nil {
			if admitted {
				tracker.add(source)
			} else {
				tracker.reject(source)
			}
			return
		}
		if runtimeMetrics != nil {
			return
		}
		if previous, duplicate := baselineAdmission[source]; duplicate {
			baselineAdmission[source] = previous && admitted
			return
		}
		baselineAdmission[source] = admitted
		baselineCandidates = append(baselineCandidates, source)
		if len(baselineCandidates) == reconciliationPageSize {
			flushBaselineSources()
		}
	}
	baselineExactOwnership := func(ownership db.SessionSourceOwnership) {
		if tracker := reconciliationBaselineTrackerFor(ctx); tracker != nil {
			tracker.addExactOwnership(ownership)
			return
		}
		if runtimeMetrics != nil || ownership.ID == "" ||
			ownership.Agent == "" || ownership.FilePath == "" {
			return
		}
		if exactBaselineOwnerships == nil {
			exactBaselineOwnerships = make(
				map[db.SessionSourceOwnership]struct{},
			)
		}
		exactBaselineOwnerships[ownership] = struct{}{}
	}
	rejectExactOwnership := func(ownership db.SessionSourceOwnership) {
		if tracker := reconciliationBaselineTrackerFor(ctx); tracker != nil {
			tracker.rejectExactOwnership(ownership)
			return
		}
		if runtimeMetrics != nil || ownership.ID == "" ||
			ownership.Agent == "" || ownership.FilePath == "" {
			return
		}
		if rejectedBaselineOwnerships == nil {
			rejectedBaselineOwnerships = make(
				map[db.SessionSourceOwnership]struct{},
			)
		}
		rejectedBaselineOwnerships[ownership] = struct{}{}
	}
	stageNoWriteCache := func(job syncJob, write skipCacheWrite) {
		if write.key == "" {
			return
		}
		if tracker := reconciliationBaselineTrackerFor(ctx); tracker != nil {
			tracker.stageCacheWrite(write)
			return
		}
		if runtimeMetrics != nil {
			e.rejectSkipCacheWrites([]skipCacheWrite{write})
			return
		}
		source, ok := baselineSourceForJob(job)
		if !ok {
			e.rejectSkipCacheWrites([]skipCacheWrite{write})
			return
		}
		writes := baselineCacheWrites[source]
		if writes == nil {
			writes = make(map[string]skipCacheWrite)
			baselineCacheWrites[source] = writes
		}
		writes[write.key] = write
	}
	flushPending := func() {
		if len(pending) == 0 {
			return
		}
		if ctx.Err() != nil && e.discardWritesOnCancel {
			releaseParseRetentionLeases(pendingLeases)
			pending = pending[:0]
			pendingLeases = pendingLeases[:0]
			pendingCacheWrites = pendingCacheWrites[:0]
			return
		}
		func() {
			defer releaseParseRetentionLeases(pendingLeases)
			completionCtx := ctx
			if !e.discardWritesOnCancel {
				completionCtx = context.WithoutCancel(ctx)
			}
			var outcome writeBatchOutcome
			if e.writeBatchOverride != nil {
				writtenSessions, writtenMessages, failedSessions, cwdFiltered :=
					e.writeBatchOverride(pending, writeMode, false)
				outcome = writeBatchOutcome{
					writtenSessions: writtenSessions,
					writtenMessages: writtenMessages,
					failedSessions:  failedSessions,
					cwdFiltered:     cwdFiltered,
					written:         make([]bool, len(pending)),
					resolved:        make([]bool, len(pending)),
				}
				if failedSessions == 0 && cwdFiltered == 0 &&
					writtenSessions == len(pending) {
					for i := range outcome.written {
						outcome.written[i] = true
						outcome.resolved[i] = true
					}
				}
			} else {
				outcome = e.writeBatchWithOutcomeContext(
					completionCtx, pending, writeMode, false,
				)
			}
			if ctx.Err() != nil && e.discardWritesOnCancel {
				return
			}
			// Claude can emit several session rows from one DAG transcript.
			// Those rows are initially written below the current data version,
			// then promoted together only after every active member succeeds.
			// User-excluded and trashed members are already resolved by policy;
			// a crash, veto, or failed active member still leaves the source stale.
			for i, pw := range pending {
				if pw.sourceWriteCount == 0 {
					continue
				}
				end := i + pw.sourceWriteCount
				sourceComplete := pw.sourceCompletionEligible &&
					end <= len(pending) && end <= len(outcome.written)
				for j := i; j < min(end, len(pending)); j++ {
					memberResolved := pending[j].sourceCompletionSkipped
					if j < len(outcome.written) && outcome.written[j] {
						memberResolved = true
					}
					if j < len(outcome.resolved) && outcome.resolved[j] {
						memberResolved = true
					}
					if !memberResolved {
						sourceComplete = false
					}
				}
				if sourceComplete && pw.promoteSourceOnComplete {
					ids := make([]string, 0, pw.sourceWriteCount)
					for j := i; j < end; j++ {
						if !outcome.written[j] {
							continue
						}
						ids = append(ids, applyIDPrefixToID(
							e.idPrefix, pending[j].sess.ID,
						))
					}
					if err := e.db.SetSessionDataVersionsContext(
						completionCtx, ids, db.CurrentDataVersion(),
					); err != nil {
						log.Printf(
							"complete provider source data versions: %v", err,
						)
						sourceComplete = false
						outcome.failedSessions++
						for j := i; j < end; j++ {
							outcome.written[j] = false
						}
					}
				}
				if !sourceComplete {
					e.clearProviderSourceFreshness(ctx, pw.providerStatHash)
					continue
				}
				if pw.providerStatHash != nil {
					e.recordProviderStatHash(ctx, *pw.providerStatHash)
				}
			}
			baselineErr := e.baselinePendingWriteSources(
				ctx, pending, outcome.written,
				exactBaselineOwnerships, rejectedBaselineOwnerships,
			)
			if baselineErr != nil {
				log.Printf("baseline parsed session sources: %v", baselineErr)
				outcome.failedSessions++
			}
			if outcome.failedSessions == 0 && outcome.cwdFiltered == 0 {
				if tracker := reconciliationBaselineTrackerFor(ctx); tracker != nil {
					tracker.stageCacheWrites(pendingCacheWrites)
				} else {
					e.promoteSkipCacheWrites(pendingCacheWrites)
				}
			}
			stats.RecordSynced(outcome.writtenSessions)
			for range outcome.failedSessions {
				stats.RecordFailed()
			}
			if outcome.failedSessions > 0 {
				e.poisonSQLiteContainerPass()
			}
			e.promoteOpenCodeStorageTrustAfterWrite(
				pending, outcome.writtenSessions, outcome.failedSessions,
				outcome.cwdFiltered,
			)
			stats.cwdFilteredSessions += outcome.cwdFiltered
			progress.MessagesIndexed += outcome.writtenMessages
			stats.messagesIndexed = progress.MessagesIndexed
		}()
		pending = pending[:0]
		pendingLeases = pendingLeases[:0]
		pendingCacheWrites = pendingCacheWrites[:0]
	}

	budget := e.retentionBudget()
	defer budget.scavengeIfNeeded()
	for i := range total {
		var r syncJob
		for {
			var pressure <-chan struct{}
			if len(pending) > 0 {
				pressure = budget.pressureSignal()
			}
			select {
			case <-ctx.Done():
				stats.Aborted = true
				drainResults(results, total-i)
				goto flush
			case <-pressure:
				flushPending()
				continue
			case r = <-results:
				if runtimeMetrics != nil {
					runtimeMetrics.workerQueued(-1)
				}
			}
			break
		}
		if options.observeResult != nil {
			options.observeResult(r)
		}
		if ctx.Err() != nil && e.discardWritesOnCancel {
			stats.Aborted = true
			r.releaseRetention()
			drainResults(results, total-i-1)
			goto flush
		}

		if r.err != nil {
			// Workers emit ctx.Err() for files skipped
			// after cancellation — treat the same as the
			// ctx.Done() branch above.
			if ctx.Err() != nil {
				stats.Aborted = true
				r.releaseRetention()
				drainResults(results, total-i-1)
				goto flush
			}
			stats.RecordFailed()
			if r.sourceCwdChanged && e.deferredSourceCwd == nil {
				stats.RecordCwdUpdated(1)
			}
			e.noteSQLiteContainerResult(r.path, false)
			if r.cacheSkip && r.mtime != 0 && !r.noCacheSkip {
				e.cacheSkip(r.skipCacheKey(), r.mtime, r.sourceFingerprint)
			}
			log.Printf("sync error: %v", r.err)
			r.releaseRetention()
			continue
		}
		if len(r.excludedSessionIDs) > 0 || len(r.sourceMissingMembers) > 0 {
			e.markRetryUnsafeSkipSource(r.path)
		}
		for range r.providerFailureCount {
			stats.RecordFailed()
		}
		if r.skip {
			cwdChanged, err := e.reconcileSkippedSourceCwd(r)
			if err != nil {
				log.Printf("reconcile skipped source cwd: %v", err)
				stats.RecordFailed()
				e.noteSQLiteContainerResult(r.path, false)
				r.releaseRetention()
				continue
			}
			if cwdChanged && e.deferredSourceCwd == nil {
				stats.RecordCwdUpdated(1)
			}
			if r.suppressPresenceSweep {
				if tracker := reconciliationBaselineTrackerFor(ctx); tracker != nil {
					tracker.addNonAuthoritativeScope(r.agent, r.path)
				}
			}
			rowlessCached := e.cacheClaudeRowlessFreshness(ctx, r)
			if !rowlessCached && r.cacheSkip && r.mtime != 0 &&
				!r.noCacheSkip {
				e.cacheSkip(r.skipCacheKey(), r.mtime)
			}
			stats.RecordSkip()
			e.noteSQLiteContainerResult(r.path, true)
			if r.providerFailureCount == 0 && !r.suppressPresenceSweep {
				admitted, exactOwnerships, err :=
					e.skippedSourceAllowsCwdFilter(ctx, r)
				if err != nil {
					log.Printf("check skipped source cwd admission: %v", err)
					stats.RecordFailed()
					e.poisonSQLiteContainerPass()
				} else {
					baselineProcessedSource(r, admitted)
					for _, ownership := range exactOwnerships {
						baselineExactOwnership(ownership)
					}
				}
			}
			progress.SessionsDone++
			e.reportProgress(onProgress, progress)
			r.releaseRetention()
			continue
		}
		sourceAllowsParserExclusions := e.sourceAllowsParserExclusions(
			r.processResult,
		)
		// Capture children before exclusions or full replacements cascade the
		// current spawn edges away. The global linker below can resolve only
		// children still named by an edge, so carry former children into its
		// scoped dangling-parent cleanup explicitly.
		excluded := e.applyIDPrefixToSessionIDs(r.excludedSessionIDs)
		if !sourceAllowsParserExclusions {
			excluded = nil
		}
		resultIDs := make([]string, 0, len(r.results))
		for _, result := range r.results {
			resultIDs = append(resultIDs, result.Session.ID)
		}
		resultIDs = e.applyIDPrefixToSessionIDs(resultIDs)
		children, err := e.db.SubagentChildSessionIDs(append(
			append([]string{}, excluded...), resultIDs...,
		))
		if err != nil {
			log.Printf("list pre-write subagent children: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.path, false)
			r.releaseRetention()
			continue
		}
		// Persist affected IDs before any exclusion or replacement can
		// cascade their only spawn edge away. The queue is cleared only in
		// the same transaction that successfully repairs the hierarchy.
		if err := e.db.QueueSubagentParentCleanupRepairs(children); err != nil {
			log.Printf("queue subagent parent repairs: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.path, false)
			r.releaseRetention()
			continue
		}
		claudeDAG := r.agent == parser.AgentClaude && len(r.results) > 1
		var sourceCompletionSkipped map[string]bool
		if claudeDAG {
			activeResultIDs, skipped :=
				e.partitionIntentionalSourceSkips(resultIDs)
			sourceCompletionSkipped = skipped
			staleVersion := max(db.CurrentDataVersion()-1, 0)
			if err := e.db.SetExistingSessionDataVersions(
				activeResultIDs, staleVersion,
			); err != nil {
				e.clearProviderSourceFreshness(ctx, r.providerStatHash)
				log.Printf("stage Claude source data versions: %v", err)
				stats.RecordFailed()
				e.noteSQLiteContainerResult(r.path, false)
				r.releaseRetention()
				continue
			}
		}
		excludedSessionIDs, err := e.deleteParserExcludedSessions(
			r.processResult, sourceAllowsParserExclusions,
		)
		if err != nil {
			log.Printf("delete parser-excluded sessions: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.path, false)
			r.releaseRetention()
			continue
		}
		if len(excludedSessionIDs) > 0 {
			stats.parserExcludedIDs = append(
				stats.parserExcludedIDs, excludedSessionIDs...,
			)
		}
		// Virtual members that vanished from a still-existing shared
		// container are tombstoned with their exact source ownership,
		// matching the reconciliation audit, instead of hard-deleted.
		// The cwd-filter freeze is judged per member against the
		// archived cwd (missingMemberTombstoneAllowed), not source-wide:
		// unchanged survivors are dropped from r.results before this
		// point, so the source-wide gate would freeze an allowed
		// member's deletion whenever everything else was unchanged.
		if len(r.sourceMissingMembers) > 0 && !options.preserveMissingSources {
			tombstoned, deferred, tombstoneErr := e.reconcileSourceMissingMembers(
				ctx, r.agent, r.sourceMissingMembers,
				baselineExactOwnership, rejectExactOwnership,
			)
			stats.Tombstoned += tombstoned
			if tombstoneErr != nil {
				log.Printf(
					"tombstone source-missing members: %v", tombstoneErr,
				)
				stats.RecordFailed()
				e.noteSQLiteContainerResult(r.path, false)
				r.releaseRetention()
				continue
			}
			stats.sourceMissingArchiveMembers = append(
				stats.sourceMissingArchiveMembers, deferred...,
			)
		}
		if len(r.results) == 0 && r.incremental == nil {
			cwdChanged, err := e.reconcileSkippedSourceCwd(r)
			if err != nil {
				log.Printf("reconcile rowless source cwd: %v", err)
				stats.RecordFailed()
				e.noteSQLiteContainerResult(r.path, false)
				r.releaseRetention()
				continue
			}
			if cwdChanged && e.deferredSourceCwd == nil {
				stats.RecordCwdUpdated(1)
			}
			if len(r.excludedSessionIDs) > 0 ||
				len(r.sourceMissingMembers) > 0 {
				stats.filesOK++
				stats.parserExcludedFiles++
			}
			if !e.cacheClaudeRowlessFreshness(ctx, r) &&
				r.cacheSkip && !r.noCacheSkip {
				if r.agent == parser.AgentOmnigent {
					// An omnigent rowless container entry can vouch for the
					// source with no stored rows at all (see the
					// whole-container clause in providerSkipCacheEntryFreshInDB),
					// so it must wait for the ownership baseline to commit;
					// a failed baseline rejects it. Every other provider keeps
					// the immediate rowless cache write.
					stageNoWriteCache(r, skipCacheWrite{
						agent:             r.agent,
						key:               r.skipCacheKey(),
						mtime:             r.mtime,
						sourceFingerprint: r.sourceFingerprint,
					})
				} else {
					e.cacheSkip(
						r.skipCacheKey(), r.mtime, r.sourceFingerprint,
					)
				}
			}
			e.noteSQLiteContainerResult(r.path, true)
			if r.providerFailureCount == 0 &&
				sourceAllowsParserExclusions {
				baselineProcessedSource(r, true)
			} else if r.providerFailureCount == 0 {
				baselineProcessedSource(r, false)
			}
			progress.SessionsDone++
			e.reportProgress(onProgress, progress)
			r.releaseRetention()
			continue
		}
		if r.cacheSkip {
			e.clearSkip(r.skipCacheKey())
		}
		stats.filesOK++

		// Drop sessions outside the cwd allow-list before batching so
		// the sync stats can tell an intentionally filtered file apart
		// from one whose sessions vanished for an unexplained reason.
		// The prepareSessionWrite veto stays as the write-seam backstop.
		// Filtered files are deliberately not skip-cached: a later
		// allow-list change must be able to pick them up again.
		sourceCwd := sourceCwdDecision{
			resolution: r.sourceCwdResolution,
			storedCwd:  r.sourceCwdStored,
			storedOK:   r.sourceCwdStoredOK,
		}
		allowed, vetoed := e.splitResultsByCwdFilter(r.results, sourceCwd)
		stats.cwdFilteredSessions += vetoed
		cwdChanged, err := e.reconcileFilteredSourceCwd(r.results, sourceCwd)
		if err != nil {
			log.Printf("reconcile filtered source cwd: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.path, false)
			r.releaseRetention()
			continue
		}
		if cwdChanged && e.deferredSourceCwd == nil {
			stats.RecordCwdUpdated(1)
		}
		// A cwd-vetoed session parsed fine but was deliberately not
		// persisted, and sessions parsed at DataVersionNeedsRetry are
		// deferred work — neither is verified state, so their container
		// must stay untrusted. The vetoed case matters because the gate
		// must never hide a filtered session from a future allow-list
		// change; such containers simply keep the pre-gate re-verify
		// behavior.
		e.noteSQLiteContainerResult(
			r.path, vetoed == 0 && len(r.retrySessionIDs) == 0,
		)
		if vetoed > 0 && len(allowed) == 0 {
			e.clearProviderSourceFreshness(ctx, r.providerStatHash)
			// Claude can emit a synthetic base result for a replay-only
			// transcript. If the active CWD filter rejects that result while a
			// stale fork is also preserved, record the successful complete parse
			// under a filter-scoped marker. This restores steady-state freshness
			// without letting a later filter configuration inherit the proof.
			e.cacheClaudeRowlessFreshness(ctx, r)
			stats.cwdFilteredFiles++
			if r.providerFailureCount == 0 {
				baselineProcessedSource(r, false)
			}
			progress.SessionsDone++
			e.reportProgress(onProgress, progress)
			r.releaseRetention()
			continue
		}

		if r.incremental != nil {
			if err := e.writeIncremental(r.incremental); err != nil {
				log.Printf("%v", err)
				stats.RecordFailed()
				r.releaseRetention()
				continue
			}
			stats.RecordSynced(1)
			if r.providerFailureCount == 0 {
				baselineJob := r
				baselineJob.machine = r.incremental.machine
				baselineProcessedSource(baselineJob, true)
			}
			progress.MessagesIndexed += len(
				r.incremental.msgs,
			)
			stats.messagesIndexed = progress.MessagesIndexed
			r.releaseRetention()
		} else {
			sourceNeedsRetry := vetoed > 0 ||
				r.providerFailureCount > 0 || len(r.retrySessionIDs) > 0
			for i, pr := range allowed {
				sessionNeedsRetry := r.providerWideFailureCount > 0 ||
					r.needsRetryForSession(pr.Session.ID)
				pw := pendingWrite{
					sess:                    pr.Session,
					msgs:                    pr.Messages,
					usageEvents:             pr.UsageEvents,
					needsRetry:              sessionNeedsRetry || claudeDAG,
					forceReplace:            r.forceReplace,
					baselineEligible:        !sourceNeedsRetry,
					storageTrustPath:        r.storageTrustPath,
					storageTrustState:       r.storageTrustState,
					storageTrustSnap:        r.storageTrustSnap,
					sourceCwdResolution:     r.sourceCwdResolution,
					sourceCwdStored:         r.sourceCwdStored,
					sourceCwdStoredOK:       r.sourceCwdStoredOK,
					sourceCompletionSkipped: sourceCompletionSkipped[applyIDPrefixToID(e.idPrefix, pr.Session.ID)],
				}
				if i == 0 &&
					(r.agent == parser.AgentClaude || r.providerStatHash != nil) {
					// Claude can emit several DAG branches from one transcript.
					// Carry their contiguous write count so the flush can make
					// one source-level completion decision. Other digest-backed
					// providers currently emit one result per source.
					pw.sourceWriteCount = 1
					if r.agent == parser.AgentClaude {
						pw.sourceWriteCount = len(allowed)
					}
					pw.providerStatHash = r.providerStatHash
					pw.sourceCompletionEligible = !sourceNeedsRetry
					pw.promoteSourceOnComplete = claudeDAG
				}
				pending = append(pending, pw)
				if runtimeMetrics != nil {
					runtimeMetrics.pendingWrites(len(pending))
				}
			}
			if r.retentionLease != nil {
				pendingLeases = append(pendingLeases, r.retentionLease)
				r.retentionLease = nil
			}
			if r.cacheAfterWrite && !sourceNeedsRetry {
				pendingCacheWrites = append(pendingCacheWrites, skipCacheWrite{
					agent:             r.agent,
					key:               r.skipCacheKey(),
					mtime:             r.mtime,
					sourceFingerprint: r.sourceFingerprint,
				})
			}
			if len(pending) >= batchSize || budget.underPressure() {
				flushPending()
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

		progress.SessionsDone++
		e.reportProgress(onProgress, progress)
	}

flush:
	flushPending()
	if ctx.Err() != nil && e.discardWritesOnCancel {
		return stats
	}
	flushBaselineSources()
	if ctx.Err() != nil && e.discardWritesOnCancel {
		return stats
	}
	if len(exactBaselineOwnerships) > 0 ||
		len(rejectedBaselineOwnerships) > 0 {
		exactOwnerships := make(
			[]db.SessionSourceOwnership, 0, len(exactBaselineOwnerships),
		)
		for ownership := range exactBaselineOwnerships {
			exactOwnerships = append(exactOwnerships, ownership)
		}
		rejectedOwnerships := make(
			[]db.SessionSourceOwnership, 0, len(rejectedBaselineOwnerships),
		)
		for ownership := range rejectedBaselineOwnerships {
			rejectedOwnerships = append(rejectedOwnerships, ownership)
		}
		baselineCtx := ctx
		if ctx.Err() != nil {
			baselineCtx = context.WithoutCancel(ctx)
		}
		if err := e.replaceSessionSourceBaselineExceptionsByMachine(
			baselineCtx, exactOwnerships, rejectedOwnerships,
		); err != nil {
			log.Printf("replace exact source baseline exceptions: %v", err)
			stats.RecordFailed()
			e.poisonSQLiteContainerPass()
		}
	}
	postWriteCtx := ctx
	if !e.discardWritesOnCancel {
		postWriteCtx = context.WithoutCancel(ctx)
	}

	// Link subagent child sessions to their parents via
	// tool_calls.subagent_session_id references. Run once
	// after all batches to avoid repeated full-table scans.
	if deferred, _ := ctx.Value(deferGlobalLinkContextKey{}).(bool); !deferred {
		if err := e.linkSubagentSessions(postWriteCtx); err != nil {
			log.Printf("link subagent sessions: %v", err)
			stats.RecordFailed()
		}
	}
	if err := e.db.RepairQueuedSubagentParentsContext(postWriteCtx); err != nil {
		log.Printf("repair queued subagent parents: %v", err)
		stats.RecordFailed()
	}

	// PhaseDone is emitted by syncAllLocked after DB-backed
	// agents finish, so this stage stays in PhaseSyncing.
	return stats
}

func (e *Engine) baselinePendingWriteSources(
	ctx context.Context, pending []pendingWrite, written []bool,
	exactBaselineOwnerships map[db.SessionSourceOwnership]struct{},
	rejectedBaselineOwnerships map[db.SessionSourceOwnership]struct{},
) error {
	eligible := make(
		map[machineSessionSource]bool,
		min(len(pending), reconciliationPageSize),
	)
	for i, write := range pending {
		path := e.effectiveSourcePath(write.sess.File.Path)
		source := machineSessionSource{
			Machine: write.sess.Machine,
			Source: db.SessionSourcePath{
				Agent: string(write.sess.Agent), FilePath: path,
			},
		}
		if source.Source.Agent == "" || source.Source.FilePath == "" {
			continue
		}
		if _, seen := eligible[source]; !seen {
			eligible[source] = true
		}
		if !write.baselineEligible || i >= len(written) || !written[i] {
			eligible[source] = false
		}
	}

	candidates := make([]machineSessionSource, 0, len(eligible))
	admitted := make([]machineSessionSource, 0, len(eligible))
	tracker := reconciliationBaselineTrackerFor(ctx)
	for source, ok := range eligible {
		candidates = append(candidates, source)
		if tracker != nil {
			if ok {
				tracker.add(source)
			} else {
				tracker.reject(source)
			}
			continue
		}
		if ok {
			admitted = append(admitted, source)
		}
	}
	if tracker != nil || len(candidates) == 0 {
		return nil
	}

	exactOwnerships := matchingBaselineOwnerships(
		candidates, exactBaselineOwnerships,
	)
	rejectedOwnerships := matchingBaselineOwnerships(
		candidates, rejectedBaselineOwnerships,
	)
	baselineCtx := ctx
	if ctx.Err() != nil &&
		(len(exactOwnerships) > 0 || len(rejectedOwnerships) > 0) {
		baselineCtx = context.WithoutCancel(ctx)
	}
	if err := e.replaceActiveSessionSourceBaselinesWithExceptionsByMachine(
		baselineCtx, candidates, admitted,
		exactOwnerships, rejectedOwnerships,
	); err != nil {
		return fmt.Errorf("replace parsed source baseline batch: %w", err)
	}
	consumeBaselineOwnerships(exactBaselineOwnerships, exactOwnerships)
	consumeBaselineOwnerships(rejectedBaselineOwnerships, rejectedOwnerships)
	return nil
}

// skippedSourceAllowsCwdFilter determines whether an unchanged source may
// retain source-wide deletion proof after a configuration restart. A freshness
// skip has no parser output to run through sourceAllowsParserExclusions, so use
// the active archived rows for that exact source as the admission evidence. A
// mixed-CWD source rejects broad proof and returns only its admitted exact
// ownerships so filtered siblings remain protected.
func (e *Engine) skippedSourceAllowsCwdFilter(
	ctx context.Context, job syncJob,
) (bool, []db.SessionSourceOwnership, error) {
	if e.cwdFilter.empty() {
		return true, nil, nil
	}
	path := e.effectiveSourcePath(job.path)
	// Freebuff shares the Codebuff provider. Query both agent types
	// so CWD filtering works for sources containing only Freebuff sessions.
	agentsToQuery := []string{string(job.agent)}
	if job.agent == parser.AgentCodebuff {
		agentsToQuery = append(agentsToQuery, string(parser.AgentFreebuff))
	}
	var admittedOwnerships []db.SessionSourceOwnership
	sourceWideAllowed := true
	for _, agentStr := range agentsToQuery {
		ids, err := e.db.ListSessionIDsByFilePath(path, agentStr)
		if err != nil {
			return false, nil, err
		}
		for _, id := range ids {
			session, err := e.db.GetSession(ctx, id)
			if err != nil {
				return false, nil, err
			}
			if session == nil {
				continue
			}
			if !e.cwdFilter.allows(session.Cwd) {
				sourceWideAllowed = false
				continue
			}
			admittedOwnerships = append(
				admittedOwnerships,
				db.SessionSourceOwnership{
					ID:       session.ID,
					Machine:  session.Machine,
					Agent:    session.Agent,
					FilePath: path,
				},
			)
		}
	}
	if sourceWideAllowed {
		return true, nil, nil
	}
	return false, admittedOwnerships, nil
}

// reconcileSkippedSingleSessionSourceBaselines refreshes deletion proof for a
// fresh single-session source before its early return. A CWD filter may have
// narrowed since the source last parsed, so broad proof must be replaced with
// exact proof for only the currently admitted archived rows.
func (e *Engine) reconcileSkippedSingleSessionSourceBaselines(
	ctx context.Context, file parser.DiscoveredFile,
) error {
	job := syncJob{
		agent:   file.Agent,
		path:    file.Path,
		machine: file.Machine,
	}
	sourceWideAllowed, exactOwnerships, err :=
		e.skippedSourceAllowsCwdFilter(ctx, job)
	if err != nil {
		return err
	}

	path := e.effectiveSourcePath(file.Path)
	agents := []parser.AgentType{file.Agent}
	if file.Agent == parser.AgentCodebuff {
		agents = append(agents, parser.AgentFreebuff)
	}
	candidates := make([]machineSessionSource, 0, len(agents))
	admitted := make([]machineSessionSource, 0, len(agents))
	for _, agent := range agents {
		source := machineSessionSource{
			Machine: file.Machine,
			Source: db.SessionSourcePath{
				Agent: string(agent), FilePath: path,
			},
		}
		candidates = append(candidates, source)
		if sourceWideAllowed {
			admitted = append(admitted, source)
		}
	}
	candidates, admitted, err = e.expandSourceBaselinesByStoredAttribution(
		ctx, candidates, admitted,
	)
	if err != nil {
		return err
	}
	if err := e.replaceActiveSessionSourceBaselinesByMachine(
		ctx, candidates, admitted,
	); err != nil {
		return err
	}
	if err := e.db.BaselineActiveSessionSourceOwnerships(
		ctx, exactOwnerships,
	); err != nil {
		return err
	}
	return nil
}

func (e *Engine) linkSubagentSessions(ctx context.Context) error {
	if runtimeMetrics := reconciliationRuntimeMetricsFor(ctx); runtimeMetrics != nil {
		runtimeMetrics.globalLinkPass()
	}
	return e.db.LinkSubagentSessionsContext(ctx)
}

// drainResults consumes remaining items from the results
// channel so that worker goroutines can exit and be collected.
func drainResults(results <-chan syncJob, remaining int) {
	for range remaining {
		job := <-results
		job.releaseRetention()
	}
}

// incrementalUpdate holds the delta produced by an
// incremental JSONL parse, used to partially update the
// session row without overwriting unrelated columns.
type incrementalUpdate struct {
	sessionID            string
	project              string
	sourceProject        string
	machine              string
	cwd                  string
	msgs                 []parser.ParsedMessage
	links                []parser.ClaudeSubagentLink
	endedAt              time.Time
	terminationStatus    *string
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
	providerStatHash     *pendingProviderStatHash
}

// sessionParseError is a per-session parse failure inside a shared
// SQLite store (OpenCode, Zed, Kiro), where one file path fans out to
// many sessions and a single bad payload must not fail the whole db.
type sessionParseError struct {
	sessionID   string // raw parser-side ID, no engine prefix
	virtualPath string // dbPath#rawID source path
	err         error
}

// sourceMissingMember identifies one stored session whose virtual member
// source vanished from a still-existing shared container. The write seam
// tombstones its exact source ownership (deletion_cause = source_missing,
// revivable) instead of hard-deleting it as a parser exclusion.
type sourceMissingMember struct {
	sessionID string
	filePath  string
	machine   string
}

type processResult struct {
	results            []parser.ParseResult
	excludedSessionIDs []string
	// sourceMissingMembers carries stored sessions whose virtual member
	// source no longer exists inside a still-present shared container
	// (e.g. a Windsurf conversation deleted from state.vscdb). They must
	// be tombstoned, never routed through DeleteParserExcludedSessions.
	sourceMissingMembers []sourceMissingMember
	// sessionErrs carries per-session parse failures from the
	// shared-db fan-out loops. Normal sync logs and skips these;
	// parse-diff (forceParse) surfaces them as DiffParseError report
	// entries so --fail-on-change cannot pass over a session the
	// current binary failed to parse.
	sessionErrs      []sessionParseError
	skip             bool
	mtime            int64
	err              error
	sourceCwdChanged bool
	incremental      *incrementalUpdate
	// providerStatHash stages the per-component freshness digest that
	// applyProviderFilePathPolicies computed but did not yet write. The
	// collector persists it after the matching session row commits
	// successfully, so a downstream write failure (or a CWD-filter veto)
	// never marks an absent or stale session as fresh. nil when the
	// source is not a multi-file hasher agent or its parse produced no
	// kept results.
	providerStatHash *pendingProviderStatHash
	cacheSkip        bool
	// claudeRowlessFreshnessKey is staged by a successful complete Claude
	// parse. The collector promotes it only when the parse leaves no admitted
	// session row and a CWD-rejected stale fork still needs filter-scoped
	// freshness, after source-missing reconciliation and CWD filtering succeed.
	claudeRowlessFreshnessKey string
	// cacheAfterWrite records a successful, complete rowless container parse
	// after its member writes commit. Unlike ordinary provider results, these
	// containers have no physical-path session row that can make the next full
	// sync fresh through the archive database.
	cacheAfterWrite bool
	// sourceFingerprint carries S3 object fingerprints into
	// skip-cache writes so same-mtime object rewrites do not stay
	// hidden behind a cached parse failure or non-interactive result.
	sourceFingerprint string
	// noCacheSkip suppresses skip-cache recording even when cacheSkip is set
	// for the agent. Read/scan failures and incomplete append boundaries are
	// transient: a readability fix or completed record may retain the same file
	// mtime, so caching either result would silently skip later work instead of
	// retrying it.
	noCacheSkip bool
	needsRetry  bool
	// forceReplace requests full message replacement on write,
	// even when the existing rows would otherwise be left in
	// place. Set when a fall-through to full parse is recovering
	// from stale stored rows, such as an atomic file replacement
	// or cross-sync streaming split. In those cases the parsed
	// messages can reuse existing ordinals, so the default
	// append-only writeMessages would silently drop the rewrite.
	forceReplace bool
	cacheKey     string
	// retrySessionIDs carries provider per-result data-version state.
	// Legacy parsers use needsRetry as a source-wide fallback.
	retrySessionIDs map[string]bool
	// suppressPresenceSweep marks a source result that must not authorize
	// presence or tombstone reconciliation, including clean unsupported skips.
	suppressPresenceSweep bool
	// providerFailureCount carries non-fatal partial outcome failures through
	// valid partial writes. Reconciliation may persist the valid results, but
	// must not acknowledge the pass as complete.
	providerFailureCount int
	// providerWideFailureCount is the subset of provider failures that applies
	// to every result in the source. Per-result retry state stays in
	// retrySessionIDs so one member cannot demote otherwise valid siblings.
	providerWideFailureCount int
	// cachedSkip distinguishes a direct mtime skip-cache hit from other skip
	// decisions such as DB freshness or container trust. Remote journal replay
	// uses only this signal for error-suppression telemetry.
	cachedSkip bool
	// storageTrustPath/State/Snap carry an OpenCode-family storage
	// session's pre-parse stat signature and invalidation snapshot to
	// the write path, which promotes it once the session's batch is
	// confirmed fully written (see opencode_storage_gate.go). Empty for
	// everything else.
	storageTrustPath  string
	storageTrustState string
	storageTrustSnap  storageTrustSnapshot
	// retentionLease bounds the memory retained by this result's parsed
	// data. It is acquired at the parse seams (provider parse, incremental
	// parse, legacy/S3 parse) and only ever set on a result carrying parsed
	// data; skip results carry none. The worker loop copies it onto
	// syncJob.retentionLease, which is released exactly once via
	// releaseRetention or the pendingLeases flush.
	retentionLease *parseRetentionLease
	// sourceCwdResolution carries parser-owned source authority to the generic
	// write seam. It is deliberately independent of transcript fingerprints.
	sourceCwdResolution parser.SourceCwdResolution
	sourceCwdStored     string
	sourceCwdStoredOK   bool
	sourceCwdPath       string
	sourceCwdAgent      parser.AgentType
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
	// s3:// objects for providers that declare S3Discovery, which bypass the
	// provider (its source sets read local files) and use the dedicated S3
	// sync path. Anything else is an unrecognized agent type.
	if !strings.HasPrefix(file.Path, "s3://") {
		return processResult{
			err: fmt.Errorf("unknown agent type: %s", file.Agent),
		}
	}

	if !parser.AgentSupportsS3Discovery(file.Agent) {
		return processResult{
			err: fmt.Errorf("unsupported s3 agent type: %s", file.Agent),
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
	if cacheSkip && !e.forceParseBypassesCache(file) {
		if e.shouldUseCachedSkip(file, mtime, sourceFingerprint) {
			if e.pathNeedsCachedSkipBypass(file.Agent, file.Path) {
				e.clearSkip(file.Path)
			} else {
				return processResult{
					skip:       true,
					mtime:      mtime,
					cacheSkip:  true,
					cachedSkip: true,
				}
			}
		}
	}

	var res = e.processS3Session(ctx, file, info)
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

func (e *Engine) pathNeedsProjectReparse(
	agent parser.AgentType,
	path string,
) bool {
	if e == nil || e.db == nil {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	project, ok := e.db.GetProjectByAgentPath(lookupPath, string(agent))
	return ok && parser.NeedsProjectReparse(project)
}

func (e *Engine) pathNeedsCachedSkipBypass(
	agent parser.AgentType,
	path string,
) bool {
	return e.pathNeedsProjectReparse(agent, path) ||
		e.pathNeedsDataVersionReparse(agent, path)
}

func (e *Engine) pathNeedsDataVersionReparse(
	agent parser.AgentType,
	path string,
) bool {
	if e == nil || e.db == nil {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	if _, _, ok := e.db.GetFileInfoByAgentPath(
		lookupPath, string(agent),
	); !ok {
		return false
	}
	return e.db.GetDataVersionByAgentPath(
		lookupPath, string(agent),
	) < db.CurrentDataVersion()
}

// staleDataVersionIdentitySet loads every source identity that
// pathNeedsDataVersionReparse would report in one scan, so the mtime-cutoff
// filter does not issue two point queries per discovered file. A load failure
// degrades to no cutoff bypass, matching the per-path form's row-missing
// result.
func (e *Engine) staleDataVersionIdentitySet() map[sourceCwdPathKey]struct{} {
	if e == nil || e.db == nil {
		return nil
	}
	identities, err := e.db.StaleDataVersionAgentPaths(db.CurrentDataVersion())
	if err != nil {
		log.Printf("list stale data-version sources: %v", err)
		return nil
	}
	set := make(map[sourceCwdPathKey]struct{}, len(identities))
	for _, identity := range identities {
		set[sourceCwdPathKey{
			path: identity.FilePath, agent: identity.Agent,
		}] = struct{}{}
	}
	return set
}

// staleSourceReparseAdmitted predicts whether the cwd filter would admit a
// reparse of a stale, cwd-participating source. A vetoed reparse cannot
// rewrite the row, so bypassing the cutoff for it would recur every pass
// without ever resolving the staleness; the row stays stale instead, and the
// bypass re-arms as soon as the filter or the source resolution admits the
// source. Filter admission for participating sources depends only on the
// source resolution and stored Cwd, so the prediction is exact; when the
// resolution disagrees with the stored Cwd the forceParse branch has already
// bypassed the cutoff before this gate runs.
func (e *Engine) staleSourceReparseAdmitted(
	participating bool, decision sourceCwdDecision,
) bool {
	if e.cwdFilter.empty() || !participating {
		return true
	}
	return e.cwdFilter.allows(sourceCwdForFilter("", decision))
}

func (e *Engine) pathInStaleIdentitySet(
	set map[sourceCwdPathKey]struct{},
	agent parser.AgentType,
	path string,
) bool {
	if len(set) == 0 {
		return false
	}
	if e.pathRewriter != nil {
		path = e.pathRewriter(path)
	}
	_, stale := set[sourceCwdPathKey{path: path, agent: string(agent)}]
	return stale
}

func (e *Engine) processProviderFile(
	ctx context.Context,
	file parser.DiscoveredFile,
) (result processResult, used bool) {
	var cwdDecision sourceCwdDecision
	var cwdPath string
	var cwdAgent parser.AgentType
	defer func() {
		if cwdDecision.resolution.State == parser.SourceCwdUnspecified {
			return
		}
		result.sourceCwdResolution = cwdDecision.resolution
		result.sourceCwdStored = cwdDecision.storedCwd
		result.sourceCwdStoredOK = cwdDecision.storedOK
		result.sourceCwdPath = cwdPath
		result.sourceCwdAgent = cwdAgent
	}()
	mode := e.providerMigrationModes[file.Agent]
	usesProvider := processFileUsesProvider(file.Agent)
	if mode != parser.ProviderMigrationProviderAuthoritative && !usesProvider {
		return processResult{}, false
	}
	// S3 sources are not provider-owned: the provider source sets read local
	// files, so s3:// paths use the dedicated S3 sync path (processS3Session),
	// which handles object fetch, fingerprinting, and per-agent skip logic.
	if strings.HasPrefix(file.Path, "s3://") {
		return processResult{}, false
	}
	if file.ProviderSource != nil && !file.ProviderProcess && !usesProvider {
		return processResult{}, false
	}

	// OpenCode-family shared-SQLite gate: when the whole container
	// provably has not changed since the last fully verified pass, none
	// of its sessions can have changed, so skip before paying for the
	// per-session fingerprint (a DB open per source) and parse.
	if e.sqliteContainerSourceFresh(file) {
		return processResult{skip: true}, true
	}

	// OpenCode-family file-backed storage gate: when the session's
	// per-file stat signature matches the last verified pass, its parse
	// inputs are unchanged, so skip before re-reading the whole message
	// and part tree (see opencode_storage_gate.go). The captured state
	// also feeds the post-parse promotion below.
	storageState, storageSnap, storageStateOK :=
		e.openCodeStorageSessionGateState(file)
	if storageStateOK &&
		e.openCodeStorageSessionFresh(file.Path, storageState) {
		return processResult{skip: true}, true
	}

	factory, ok := e.providerFactories[file.Agent]
	if !ok {
		return processResult{
			err: fmt.Errorf("provider not found for agent type: %s", file.Agent),
		}, true
	}
	machine := e.machineForFile(file)
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:                 e.agentDirs[file.Agent],
		Machine:               e.machine,
		StableSourceSnapshots: e.stableSourceSnapshots,
		SourceMachines:        e.sourceMachines[file.Agent],
		PathRewriter:          e.pathRewriter,
	})

	source, found, err := e.providerSourceForDiscoveredFile(ctx, provider, file)
	if err != nil {
		return processResult{err: err}, true
	}
	if !found {
		// A forced parse on a deleted shared SQLite database (Zed, ZCode, Shelley)
		// resolves to no source because the physical file is gone. Mirror the
		// legacy deleted-source handling: complete the source as an empty
		// force-replace so the engine retires every session that lived in the
		// removed database instead of failing the sync.
		if (file.ForceParse || file.ForceFullParse) &&
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
	if source.ConfiguredRoot != "" {
		if sourceMachine, ok := e.configuredMachineForPath(
			file.Agent, source.ConfiguredRoot,
		); ok {
			machine = sourceMachine
		}
	} else if file.Machine == "" {
		machine = e.machineForProviderSource(file.Agent, source, file.Path)
	}
	file.Machine = machine
	providerSemantics := provider.Capabilities().Sync

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
	cwdDecision = e.sourceCwdDecision(source)
	cwdPath = e.sourceCwdLookupPath(source)
	cwdAgent = source.Provider
	if cwdAgent == "" {
		cwdAgent = file.Agent
	}
	forceSourceCwdParse := cwdDecision.forceParse

	verifiedCapture, verifiedMtime, verifiedFresh, verifiedStateOK :=
		e.verifiedProviderSourceState(provider, source, file)
	if !forceSourceCwdParse && verifiedStateOK && verifiedFresh {
		if e.verifiedProviderSourceFreshInDB(
			verifiedCapture.key.agent, source,
			verifiedCapture.signature.size, verifiedMtime,
		) {
			return processResult{
				skip:  true,
				mtime: verifiedMtime,
			}, true
		}
		e.invalidateVerifiedSource(
			verifiedCapture.key.agent, verifiedCapture.key.path,
		)
	}

	// Capture the per-component stat digest from the same pre-parse
	// snapshot that gates freshness, BEFORE fingerprinting or parsing
	// reads the source. The in-memory verified-source gate runs first so
	// a warm engine does not pay for a second stat snapshot it will never
	// consult. Persisting a digest computed after the parse would race a
	// concurrent companion rewrite: the stored digest would describe the
	// new file state while the session row holds the old parse payload,
	// so every later warm sync would short-circuit on the new digest and
	// the stale row would never be refreshed. This single snapshot feeds
	// the warm-match comparison in providerSourceFreshBeforeFingerprint,
	// the confirmed-unchanged-skip stamp, and the write-path staging, so
	// all three always agree.
	//
	// providerStatDigestEligible withholds the capture for
	// content-authority providers under a pathRewriter: materialized
	// remote stats cannot prove a re-download unchanged, so their remote
	// freshness stays content-hash arbitrated.
	var preParseStatHash *pendingProviderStatHash
	if hasher, ok := e.providerStatHashers[file.Agent]; ok &&
		e.providerStatDigestEligible(file.Agent) {
		if physicalPath := providerDiscoveredPath(source); physicalPath != "" {
			targetKey := physicalPath
			if e.pathRewriter != nil {
				targetKey = e.pathRewriter(physicalPath)
			}
			preParseStatHash = &pendingProviderStatHash{
				agent:        file.Agent,
				physicalPath: physicalPath,
				targetKey:    targetKey,
				digest:       hasher.ComputeMultiFileStatHash(physicalPath),
			}
		}
	}

	// Persisted stat-digest skip. This runs before the single-session
	// content guard below on purpose: a matching digest (size, mtime,
	// ctime per component) plus a current stored row proves the source
	// unchanged without opening it, so a fresh process (daemon restart or
	// one-shot CLI sync) skips on stats alone instead of re-hashing the
	// full transcript through providerIncrementalContentChanged. Any real
	// change -- including a same-size same-mtime in-place rewrite --
	// bumps a ctime and breaks the digest, falling through to the
	// content-verified gates.
	if !forceSourceCwdParse {
		if freshMtime, fresh := e.providerSourceFreshBeforeFingerprint(
			ctx, source, file, preParseStatHash,
		); fresh {
			if verifiedStateOK {
				e.promoteVerifiedSource(verifiedCapture)
			}
			return processResult{
				skip:  true,
				mtime: freshMtime,
			}, true
		}
	}

	// DB-freshness skip for single-session JSONL providers (Claude):
	// when the stored session's size, mtime, and data version already
	// match the source and its project does not need reparse, skip the
	// parse entirely. This reproduces the legacy process arm's
	// shouldSkipFile gate so an unchanged session is not re-parsed on
	// every full sync. A content-verified skip confirmed the current
	// bytes against the stored row hash, so it backfills the stat digest
	// for rows that predate the side-table (or whose digest an index or
	// companion touch invalidated); without the stamp those rows would
	// re-hash on every fresh process forever, since a skip never writes.
	sourceForceReplace := false
	if !forceSourceCwdParse {
		if mtime, fresh, forceReplace, contentVerified := e.providerSingleSessionFresh(
			ctx, provider, source, file,
		); fresh {
			if !verifiedStateOK || contentVerified {
				if verifiedStateOK {
					e.promoteVerifiedSource(verifiedCapture)
				}
				if contentVerified {
					e.stampProviderStatHashForConfirmedSource(
						ctx, preParseStatHash,
					)
				}
				return processResult{
					skip:  true,
					mtime: mtime,
				}, true
			}
			// A gate-eligible local source without a comparable stored hash
			// takes the fingerprint path once to earn verified-source trust.
		} else if forceReplace {
			sourceForceReplace = true
		}
	}

	// Watermark-only shared-container sources (changed-path classification)
	// carry just the session-row watermark. When it does not advance past
	// the stored composite watermark, the session and project rows provably
	// did not change, so skip before Fingerprint pays the per-session child
	// lookup; a child-only edit this cannot see is reconciled by the next
	// full-discovery pass, whose digest comparison still catches it.
	if !forceSourceCwdParse {
		if freshMtime, fresh := e.watermarkOnlySQLiteSourceFresh(source, file); fresh {
			return processResult{
				skip:  true,
				mtime: freshMtime,
			}, true
		}
	}

	fingerprint, err := provider.Fingerprint(ctx, source)
	if err != nil {
		if (file.ForceParse || file.ForceFullParse) &&
			providerDeletedPhysicalSQLiteSource(file.Agent, file.Path) &&
			errors.Is(err, os.ErrNotExist) {
			excludedSessionIDs, ownershipErr :=
				e.providerSourceSessionIDsForForceReplace(
					ctx, provider, source,
				)
			if ownershipErr != nil {
				return processResult{
					err:         ownershipErr,
					noCacheSkip: true,
				}, true
			}
			return processResult{
				excludedSessionIDs: excludedSessionIDs,
				forceReplace:       true,
			}, true
		}
		return processResult{err: err}, true
	}
	cacheKey := providerProcessCacheKey(
		file, source, fingerprint, providerSemantics,
	)
	cacheSkip := e.shouldCacheSkip(file)
	if cacheSkip && !forceSourceCwdParse && !e.forceParseBypassesCache(file) {
		e.skipMu.RLock()
		cachedMtime, cached := e.skipCache[cacheKey]
		e.skipMu.RUnlock()
		if cached && cachedMtime == fingerprint.MTimeNS {
			// A cached skip must not hide a session whose stored row needs
			// self-healing (e.g. a parser data-version bump or generated
			// roborev CI worktree project): clear the entry and fall through
			// to a full reparse, mirroring the legacy process arm.
			cacheFresh, cacheRowHashVerified := e.providerSkipCacheEntryFreshInDB(
				file,
				source,
				fingerprint,
				providerSemantics,
			)
			indexChanged, indexVerified := false, true
			if file.Agent == parser.AgentCodex {
				indexChanged, indexVerified =
					e.codexCachedIndexSessionNameState(file.Path)
			}
			if !cacheFresh {
				e.clearSkip(cacheKey)
			} else if e.pathNeedsCachedSkipBypass(file.Agent, file.Path) {
				e.clearSkip(cacheKey)
			} else if indexChanged {
				// The transcript fingerprint can remain byte-for-byte identical
				// while session_index.jsonl changes this session's title. Do not
				// let a pre-existing transcript skip entry hide that metadata
				// refresh; non-Codex providers avoid the index lookup entirely.
				e.clearSkip(cacheKey)
			} else {
				cacheStillFresh := true
				if file.Agent == parser.AgentOmnigent {
					restored, err := parser.RestoreOmnigentCachedSourceState(
						ctx, provider, source,
					)
					if err != nil {
						return processResult{
							err: fmt.Errorf(
								"restore cached %s source state: %w",
								file.Agent, err,
							),
						}, true
					}
					if restored {
						currentFingerprint, err := provider.Fingerprint(
							ctx, source,
						)
						if err != nil {
							return processResult{err: err}, true
						}
						if currentFingerprint != fingerprint {
							e.clearSkip(cacheKey)
							fingerprint = currentFingerprint
							cacheKey = providerProcessCacheKey(
								file,
								source,
								fingerprint,
								providerSemantics,
							)
							cacheStillFresh = false
						}
					}
				}
				if cacheStillFresh {
					if verifiedStateOK && indexVerified &&
						e.shouldSkipProviderSourceByDB(
							file, fingerprint, providerSemantics,
						) {
						e.promoteVerifiedSource(verifiedCapture)
					}
					// The fingerprint just content-hashed the current
					// source and matched the stored row, so this skip may
					// backfill or refresh the stat digest for rows that
					// predate the side-table or whose digest a shared
					// index touch invalidated.
					if cacheRowHashVerified && indexVerified {
						e.stampProviderStatHashForConfirmedSource(
							ctx, preParseStatHash,
						)
					}
					return processResult{
						skip:       true,
						mtime:      fingerprint.MTimeNS,
						cacheSkip:  true,
						cachedSkip: true,
						cacheKey:   cacheKey,
					}, true
				}
				// A commit raced cache validation and tracker restoration.
				// Fall through to parse the now-current container.
			}
		}
	}
	if cacheSkip && !forceSourceCwdParse && e.shouldSkipProviderSource(
		file, source, fingerprint, providerSemantics,
	) {
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
	var incRes processResult
	var incOK bool
	if !forceSourceCwdParse {
		incRes, incOK = e.tryProviderIncrementalAppend(
			ctx, provider, source, file, fingerprint,
		)
	}
	if incOK {
		incRes.mtime = fingerprint.MTimeNS
		incRes.cacheSkip = cacheSkip
		incRes.cacheKey = cacheKey
		if incRes.incremental != nil &&
			incRes.incremental.fileSize == fingerprint.Size {
			incRes.incremental.providerStatHash = preParseStatHash
		}
		return incRes, true
	}
	// An explicit full pass must replace the stored message stream after
	// bypassing incremental append. Otherwise a complete parse can still be
	// written with append semantics and leave stale earlier rows untouched.
	incForceReplace := sourceForceReplace || incRes.forceReplace ||
		e.forceFullParse || file.ForceFullParse

	// DB-stored fingerprint skip. The provider has no database handle, so the
	// engine reproduces the legacy DB-aware skip that single-session JSONL
	// providers relied on: an unchanged source whose stored size and effective
	// mtime already match is not reparsed, even when the in-memory skip cache
	// was cleared (e.g. by SyncSingleSession) or never populated (a fresh
	// engine). For Codex this also folds in the session_index.jsonl sidecar:
	// a shared index mtime bump that did not change this session's title must
	// not trigger a reparse.
	if !forceSourceCwdParse && !incForceReplace && !e.forceParseRequested(file) {
		dbFresh, metadataVerified := e.providerSourceFreshnessByDB(
			file, fingerprint, providerSemantics,
		)
		if dbFresh && verifiedStateOK && metadataVerified {
			e.promoteVerifiedSource(verifiedCapture)
		}
		// The Codex-family fingerprint content-hashed the rollout and
		// shouldSkipCodexFingerprint verified it against the stored row
		// (hash, project, data version, index title), so this skip may
		// backfill or refresh the stat digest: rows that predate the
		// side-table, and rollouts whose digest a shared index touch
		// invalidated without changing their own title, would otherwise
		// re-hash on every fresh process forever.
		if dbFresh {
			if metadataVerified {
				e.stampProviderStatHashForConfirmedSource(
					ctx, preParseStatHash,
				)
			}
			return processResult{
				skip:        true,
				mtime:       fingerprint.MTimeNS,
				cacheSkip:   cacheSkip,
				cacheKey:    cacheKey,
				noCacheSkip: true,
			}, true
		}
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
	if !forceSourceCwdParse && !incForceReplace && !e.forceParseRequested(file) &&
		e.providerSourceUnchangedInDB(
			ctx, source, fingerprint, providerSemantics, preParseStatHash,
		) {
		return processResult{
			skip:      true,
			mtime:     fingerprint.MTimeNS,
			cacheSkip: cacheSkip,
			cacheKey:  cacheKey,
		}, true
	}

	// Provider parse seam: every gate above returns a lease-free skip. From
	// here the provider parses the source, so acquire the retention lease that
	// bounds the parsed payload and attach it to every result carrying that
	// data. A result still classified as a skip below releases it immediately.
	lease, err := e.retentionBudget().acquire(ctx, parseRetentionSourceBytes(file))
	if err != nil {
		return processResult{err: err}, true
	}
	if runtimeMetrics := reconciliationRuntimeMetricsFor(ctx); runtimeMetrics != nil {
		if _, _, ok := sqliteContainerSourceForFile(file); ok {
			runtimeMetrics.openCodeSQLiteParse()
		}
	}
	outcome, err := provider.Parse(ctx, parser.ParseRequest{
		Source:      source,
		Fingerprint: fingerprint,
		Machine:     machine,
		ForceParse:  e.forceParseRequested(file),
	})
	if err != nil {
		if !e.forceParse {
			cwdChanged, reconcileErr := e.reconcileSourceCwdByPath(
				source, cwdDecision,
			)
			if reconcileErr != nil {
				err = errors.Join(err, reconcileErr)
			}
			return processResult{
				err:              err,
				sourceCwdChanged: cwdChanged,
				mtime:            fingerprint.MTimeNS,
				cacheSkip:        cacheSkip,
				cacheKey:         cacheKey,
				noCacheSkip:      true,
				retentionLease:   lease,
			}, true
		}
		return processResult{
			err:            err,
			mtime:          fingerprint.MTimeNS,
			cacheSkip:      cacheSkip,
			cacheKey:       cacheKey,
			noCacheSkip:    true,
			retentionLease: lease,
		}, true
	}
	if err := validateProviderOutcome(
		provider.Definition(),
		source,
		fingerprint,
		outcome,
	); err != nil {
		return processResult{
			err:            err,
			mtime:          fingerprint.MTimeNS,
			cacheSkip:      cacheSkip,
			cacheKey:       cacheKey,
			noCacheSkip:    true,
			retentionLease: lease,
		}, true
	}
	applyProviderFingerprintFileInfo(file.Agent, fingerprint, outcome.Results)
	cleanCache := providerOutcomeAllowsCleanSkipCache(outcome)
	providerWideFailureCount := len(outcome.SourceErrors)
	if !outcome.ResultSetComplete {
		providerWideFailureCount++
	}
	providerFailureCount := providerWideFailureCount
	if outcome.SkipReason != parser.SkipNone {
		if outcome.SkipReason == parser.SkipUnsupportedSource {
			e.anomalies.recordUnsupportedSourceLayout(string(file.Agent), file.Path)
		}
		excludedSessionIDs := append([]string(nil), outcome.ExcludedSessionIDs...)
		var missingMembers []sourceMissingMember
		if outcome.ForceReplace && outcome.ResultSetComplete {
			owned, ownershipErr :=
				e.providerSourceSessionOwnershipsForForceReplace(
					ctx, provider, source,
				)
			if ownershipErr != nil {
				return processResult{
					err:            ownershipErr,
					mtime:          fingerprint.MTimeNS,
					cacheSkip:      cacheSkip,
					cacheKey:       cacheKey,
					noCacheSkip:    true,
					retentionLease: lease,
				}, true
			}
			omnigentContainerExists :=
				parser.IsOmnigentContainerSource(source) &&
					parser.IsRegularFile(providerDiscoveredPath(source))
			traeContainerExists := file.Agent == parser.AgentTrae &&
				parser.IsRegularFile(providerDiscoveredPath(source))
			if e.pathRewriter != nil ||
				(providerVirtualSourceContainerExists(file.Path) ||
					omnigentContainerExists || traeContainerExists) {
				// The provider re-resolved this exact virtual member against a
				// still-present shared container, or authoritatively parsed an
				// empty Omnigent or Trae container. The member was removed from the
				// container, not the container from disk. Carry the stored
				// ownership to the revivable tombstone seam.
				missingMembers = owned
			} else {
				for _, member := range owned {
					excludedSessionIDs = append(excludedSessionIDs, member.sessionID)
				}
			}
		}
		skipRes := processResult{
			skip:                 !outcome.ForceReplace,
			excludedSessionIDs:   excludedSessionIDs,
			sourceMissingMembers: missingMembers,
			mtime:                fingerprint.MTimeNS,
			cacheSkip:            cacheSkip,
			cacheKey:             cacheKey,
			noCacheSkip:          !cleanCache,
			forceReplace:         outcome.ForceReplace,
			suppressPresenceSweep: outcome.SkipReason == parser.SkipUnsupportedSource ||
				!outcome.ResultSetComplete,
			providerFailureCount:     providerFailureCount,
			providerWideFailureCount: providerWideFailureCount,
		}
		if file.Agent == parser.AgentClaude && cleanCache &&
			!e.forceParseRequested(file) {
			skipRes.claudeRowlessFreshnessKey =
				e.claudeRowlessFreshnessCacheKey(file.Path, fingerprint.Hash)
		}
		// A SkipReason outcome without a force-replace carries no parsed data,
		// so it stays a lease-free skip; a force-replace is parse-bearing.
		if skipRes.skip {
			lease.Release()
		} else {
			skipRes.retentionLease = lease
		}
		return skipRes, true
	}
	parsedResults := parseOutcomeResults(outcome.Results)
	parsedCount := len(parsedResults)
	excludedSessionIDs := append([]string(nil), outcome.ExcludedSessionIDs...)
	var missingMembers []sourceMissingMember
	// Parse-diff intentionally reports a removed Trae member through its
	// presence sweep. It never filters unchanged results or writes tombstones,
	// so ownership reconciliation is needed only by real sync engines.
	if ((file.Agent == parser.AgentOmnigent && outcome.ForceReplace) ||
		(file.Agent == parser.AgentTrae && !e.forceParse)) &&
		outcome.ResultSetComplete && len(outcome.SourceErrors) == 0 {
		missingMembers, err =
			e.providerSourceMissingSessionOwnershipsForCompleteResult(
				ctx, provider, source, parsedResults,
			)
		if err != nil {
			return processResult{
				err:            err,
				mtime:          fingerprint.MTimeNS,
				cacheSkip:      cacheSkip,
				cacheKey:       cacheKey,
				noCacheSkip:    true,
				retentionLease: lease,
			}, true
		}
	} else if file.Agent == parser.AgentClaude &&
		outcome.ResultSetComplete && len(outcome.SourceErrors) == 0 {
		missingMembers, err =
			e.claudeSourceMissingSessionOwnershipsForCompleteResult(
				ctx, file.Path, outcome.ExcludedSessionIDs, parsedResults,
			)
		if err != nil {
			return processResult{
				err:            err,
				mtime:          fingerprint.MTimeNS,
				cacheSkip:      cacheSkip,
				cacheKey:       cacheKey,
				noCacheSkip:    true,
				retentionLease: lease,
			}, true
		}
	}
	filteredResults := e.dropUnchangedSharedSQLiteResults(
		file, parsedResults, providerSemantics.UnchangedResults,
	)
	res := processResult{
		results:              filteredResults,
		excludedSessionIDs:   excludedSessionIDs,
		sourceMissingMembers: missingMembers,
		mtime:                fingerprint.MTimeNS,
		cacheSkip:            cacheSkip,
		cacheKey:             cacheKey,
		noCacheSkip:          !cleanCache,
		forceReplace:         outcome.ForceReplace || incForceReplace,
		suppressPresenceSweep: outcome.SkipReason == parser.SkipUnsupportedSource ||
			!outcome.ResultSetComplete,
		providerFailureCount:     providerFailureCount,
		providerWideFailureCount: providerWideFailureCount,
		retentionLease:           lease,
		providerStatHash:         preParseStatHash,
		sourceCwdResolution:      cwdDecision.resolution,
		sourceCwdStored:          cwdDecision.storedCwd,
		sourceCwdStoredOK:        cwdDecision.storedOK,
	}
	if file.Agent == parser.AgentOmnigent && cacheSkip && cleanCache &&
		!e.forceParseRequested(file) &&
		outcome.ResultSetComplete && len(outcome.SourceErrors) == 0 &&
		fingerprint.Hash != "" {
		// A whole-container omnigent parse may only be skip-cached after its
		// member writes commit (cache-after-write); virtual member parses keep
		// the immediate cache path.
		res.cacheAfterWrite = parser.IsOmnigentContainerSource(source)
	}
	// Incremental-append providers (Claude and Codex) need the stored file
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
			res.providerFailureCount++
		}
	}
	if e.forceParseRequested(file) {
		for _, sourceErr := range outcome.SourceErrors {
			res.sessionErrs = append(res.sessionErrs, sessionParseError{
				sessionID:   sourceErr.SessionID,
				virtualPath: sourceErr.SourceKey,
				err:         sourceErr.Err,
			})
		}
	}
	e.applyProviderFilePathPolicies(ctx, provider, file.Agent, file.Path, &res)
	if file.Agent == parser.AgentClaude && cleanCache &&
		!e.forceParseRequested(file) {
		res.claudeRowlessFreshnessKey =
			e.claudeRowlessFreshnessCacheKey(file.Path, fingerprint.Hash)
	}
	if storageStateOK {
		e.stageOpenCodeStorageTrust(
			&res, file.Path, storageState, storageSnap,
			parsedCount, outcome.ResultSetComplete,
		)
	}
	return res, true
}

// dropUnchangedSharedSQLiteResults reproduces the legacy per-session skip the
// folded processZed/processShelley loops and the aiderFileUnchanged check
// performed. Zed, Shelley, and Trae keep every session in one shared SQLite
// database, and Aider fans every run out of one shared history file, so the
// provider re-parses every session on any change to that shared source.
// Without a per-session filter the engine would rewrite and recount unchanged
// sessions. This drops results whose stored file_mtime and, when available, the
// fingerprint stored in file_hash already match, using the path rewriter so
// remote stored paths resolve. Force-parse runs (parse-diff, single-session
// resync) keep every result so they always re-emit.
func (e *Engine) dropUnchangedSharedSQLiteResults(
	file parser.DiscoveredFile,
	results []parser.ParseResult,
	policy parser.UnchangedResultPolicy,
) []parser.ParseResult {
	if e.forceParseRequested(file) || len(results) == 0 {
		return results
	}
	if policy == parser.UnchangedResultNone {
		return results
	}
	compareHash := policy == parser.UnchangedResultMTimeAndHash

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
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
) ([]string, error) {
	members, err := e.providerSourceSessionOwnershipsForForceReplace(
		ctx, provider, source,
	)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.sessionID)
	}
	return ids, nil
}

// providerSourceSessionOwnershipsForForceReplace lists the stored active
// sessions owned by a force-replaced source, paired with the exact stored
// file path each row is tracked under, so callers can either hard-delete
// them as parser exclusions or tombstone their exact source ownership.
func (e *Engine) providerSourceSessionOwnershipsForForceReplace(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
) ([]sourceMissingMember, error) {
	agent := provider.Definition().Type
	root := ""
	for _, candidate := range []string{source.DisplayPath, source.FingerprintKey, source.Key} {
		if candidate != "" {
			root = candidate
			break
		}
	}
	if root == "" {
		return nil, nil
	}
	scopes := []parser.StoredSourceHintScope{{Path: root}}
	if resolver, ok := provider.(parser.StoredSourceHintScopeProvider); ok {
		if resolved := resolver.StoredSourceHintScopes(parser.ChangedPathRequest{Path: root}); len(resolved) > 0 {
			scopes = resolved
		}
	}
	if e.pathRewriter != nil {
		for i := range scopes {
			scopes[i].Path = e.pathRewriter(scopes[i].Path)
		}
	}
	sourcePaths, err := e.db.ListStoredSourcePathHints(
		string(agent), storedSourceDBHintScopes(scopes),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"list provider force-replace source hints: %w", err,
		)
	}
	seen := make(map[string]struct{})
	var members []sourceMissingMember
	var sessionIDs []string
	for _, sourcePath := range sourcePaths {
		pathIDs, err := e.db.ListSessionIDsByFilePath(sourcePath, string(agent))
		if err != nil {
			return nil, fmt.Errorf(
				"list provider force-replace sessions for %s: %w",
				sourcePath, err,
			)
		}
		for _, id := range pathIDs {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			members = append(members, sourceMissingMember{
				sessionID: id,
				filePath:  sourcePath,
				machine: e.machineForProviderSource(
					agent, source, sourcePath,
				),
			})
			sessionIDs = append(sessionIDs, id)
		}
	}
	storedMachines, err := e.db.ListSessionMachinesByID(ctx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf(
			"list provider force-replace session machines: %w", err,
		)
	}
	for i := range members {
		if machine, exists := storedMachines[members[i].sessionID]; exists {
			members[i].machine = machine
		}
	}
	return members, nil
}

func (e *Engine) providerSourceMissingSessionOwnershipsForCompleteResult(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
	results []parser.ParseResult,
) ([]sourceMissingMember, error) {
	emitted := make(map[string]struct{}, len(results))
	for _, result := range results {
		id := applyIDPrefixToID(e.idPrefix, result.Session.ID)
		if id != "" {
			emitted[id] = struct{}{}
		}
	}
	stored, err := e.providerSourceSessionOwnershipsForForceReplace(
		ctx, provider, source,
	)
	if err != nil {
		return nil, err
	}
	missing := make([]sourceMissingMember, 0, len(stored))
	for _, member := range stored {
		if _, present := emitted[member.sessionID]; present {
			continue
		}
		missing = append(missing, member)
	}
	return missing, nil
}

// claudeSourceMissingSessionOwnershipsForCompleteResult lists stored active
// Claude fork sessions written by an older parser version under this
// transcript's exact stored path that a complete full parse no longer derives
// from it. The stale data version and fork relationship together prove the row
// is a legacy DAG branch artifact rather than a current parser presence
// regression. Such rows can never be re-stamped by re-parsing the file, so
// they pin the path's minimum data_version at a stale value and defeat the
// unchanged-source skip on every sweep. Tombstoning them as source-missing
// lets the skip converge; a later parse that re-emits an ID revives the row.
// Unlike the provider-scope variant above, the lookup is bound to the results'
// own file paths: a Claude source scope must never widen to sibling
// transcripts, whose sessions are legitimately absent from this parse.
func (e *Engine) claudeSourceMissingSessionOwnershipsForCompleteResult(
	ctx context.Context,
	sourcePath string,
	excludedSessionIDs []string,
	results []parser.ParseResult,
) ([]sourceMissingMember, error) {
	present := make(map[string]struct{}, len(results))
	paths := make(map[string]struct{}, 1)
	for _, result := range results {
		if id := applyIDPrefixToID(e.idPrefix, result.Session.ID); id != "" {
			present[id] = struct{}{}
		}
		path := result.Session.File.Path
		if path == "" {
			continue
		}
		if e.pathRewriter != nil {
			path = e.pathRewriter(path)
		}
		paths[path] = struct{}{}
	}
	if len(paths) == 0 && sourcePath != "" {
		paths[e.effectiveSourcePath(sourcePath)] = struct{}{}
	}
	// Excluded IDs are owned by the parser-exclusion deletion path.
	for _, id := range e.applyIDPrefixToSessionIDs(excludedSessionIDs) {
		present[id] = struct{}{}
	}
	if index := e.archiveStaleClaudeForks; index != nil {
		return index.missingMembers(paths, present), nil
	}
	var members []sourceMissingMember
	var sessionIDs []string
	for path := range paths {
		storedIDs, err := e.db.ListStaleForkSessionIDsByFilePath(
			path, string(parser.AgentClaude),
		)
		if err != nil {
			return nil, fmt.Errorf(
				"list stale claude fork sessions for %s: %w", path, err,
			)
		}
		for _, id := range storedIDs {
			if _, ok := present[id]; ok {
				continue
			}
			members = append(members, sourceMissingMember{
				sessionID: id,
				filePath:  path,
			})
			sessionIDs = append(sessionIDs, id)
		}
	}
	if len(members) == 0 {
		return nil, nil
	}
	storedMachines, err := e.db.ListSessionMachinesByID(ctx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf(
			"list stored claude session machines: %w", err,
		)
	}
	for i := range members {
		members[i].machine = storedMachines[members[i].sessionID]
	}
	return members, nil
}

// archiveStaleClaudeForkIndex is a rebuild-scoped snapshot of the original
// archive's stale Claude fork rows keyed by stored source path. The archive
// takes no writes while a rebuild reads it, so the snapshot stays exact.
type archiveStaleClaudeForkIndex struct {
	byPath map[string][]db.SessionSourceOwnership
}

func loadArchiveStaleClaudeForkIndex(
	archive *db.DB,
) (*archiveStaleClaudeForkIndex, error) {
	ownerships, err := archive.ListStaleForkSessionOwnerships(
		string(parser.AgentClaude),
	)
	if err != nil {
		return nil, err
	}
	index := &archiveStaleClaudeForkIndex{
		byPath: make(map[string][]db.SessionSourceOwnership),
	}
	for _, ownership := range ownerships {
		index.byPath[ownership.FilePath] = append(
			index.byPath[ownership.FilePath], ownership,
		)
	}
	return index, nil
}

// missingMembers returns the snapshotted stale forks under paths that a
// complete parse did not re-emit, in a stable path-then-ID order.
func (index *archiveStaleClaudeForkIndex) missingMembers(
	paths map[string]struct{},
	present map[string]struct{},
) []sourceMissingMember {
	var members []sourceMissingMember
	for path := range paths {
		for _, ownership := range index.byPath[path] {
			if _, ok := present[ownership.ID]; ok {
				continue
			}
			members = append(members, sourceMissingMember{
				sessionID: ownership.ID,
				filePath:  path,
				machine:   ownership.Machine,
			})
		}
	}
	slices.SortFunc(members, func(a, b sourceMissingMember) int {
		if c := strings.Compare(a.filePath, b.filePath); c != 0 {
			return c
		}
		return strings.Compare(a.sessionID, b.sessionID)
	})
	return members
}

// claudeSourceNeedsStaleForkReconciliation reports whether a freshness skip
// would strand an actionable legacy fork on this exact stored source path.
// Forks rejected by the CWD filter are intentionally preserved and do not
// require another parse. A read error still defeats freshness so cleanup can
// retry instead of silently preserving stale data.
func (e *Engine) claudeSourceNeedsStaleForkReconciliation(
	ctx context.Context, path string,
) bool {
	storedIDs, err := e.db.ListStaleForkSessionIDsByFilePath(
		path, string(parser.AgentClaude),
	)
	if err != nil {
		return true
	}
	for _, id := range storedIDs {
		allowed, err := e.missingMemberTombstoneAllowed(ctx, id)
		if err != nil || allowed {
			return true
		}
	}
	return false
}

// claudeSourceHasOnlyPreservedStaleForks reports whether every active Claude
// row at path is a stale legacy fork that the current CWD filter intentionally
// preserves. Such rows must not keep an otherwise unchanged source permanently
// stale: the filter decision is re-evaluated on every pass, so a later engine
// with a broader filter will resume reconciliation instead of inheriting a
// durable cache exemption. Any lookup error fails closed and reparses.
func (e *Engine) claudeSourceHasOnlyPreservedStaleForks(
	ctx context.Context, path string,
) bool {
	return e.claudeSourceStaleRowsArePreservedForks(ctx, path, false)
}

// claudeSourceStaleRowsArePreservedForks verifies that every stale row under
// path is a legacy fork rejected by the current CWD filter. When
// allowCurrentRows is true, current primary or fork rows may share the source.
func (e *Engine) claudeSourceStaleRowsArePreservedForks(
	ctx context.Context, path string, allowCurrentRows bool,
) bool {
	staleIDs, err := e.db.ListStaleForkSessionIDsByFilePath(
		path, string(parser.AgentClaude),
	)
	if err != nil || len(staleIDs) == 0 {
		return false
	}
	activeIDs, err := e.db.ListSessionIDsByFilePath(
		path, string(parser.AgentClaude),
	)
	if err != nil || len(activeIDs) < len(staleIDs) {
		return false
	}
	stale := make(map[string]struct{}, len(staleIDs))
	for _, id := range staleIDs {
		stale[id] = struct{}{}
	}
	for _, id := range activeIDs {
		if _, ok := stale[id]; !ok {
			if !allowCurrentRows {
				return false
			}
			session, err := e.db.GetSession(ctx, id)
			if err != nil || session == nil ||
				session.DataVersion < db.CurrentDataVersion() {
				return false
			}
			continue
		}
		allowed, err := e.missingMemberTombstoneAllowed(ctx, id)
		if err != nil || allowed {
			return false
		}
	}
	return true
}

// claudeSourceFreshWithoutPrimary verifies an unchanged Claude source through
// exact-path metadata when its canonical primary session is absent and only
// CWD-rejected stale forks remain. The stale forks' old data versions and file
// metadata are ignored only when a current-content-, data-version-, and
// CWD-filter-keyed marker proves the parser completed cleanly without producing
// an admitted primary row.
func (e *Engine) claudeSourceFreshWithoutPrimary(
	ctx context.Context,
	lookupPath string,
	physicalPath string,
	info os.FileInfo,
	sourceFingerprint string,
) (fresh, contentVerified bool) {
	if !e.claudeSourceHasOnlyPreservedStaleForks(ctx, lookupPath) {
		return false, false
	}
	project, ok := e.db.GetProjectByAgentPath(
		lookupPath, string(parser.AgentClaude),
	)
	if !ok || project == "" || parser.NeedsProjectReparse(project) {
		return false, false
	}
	currentHash := sourceFingerprint
	if currentHash == "" {
		var err error
		currentHash, err = computeFileHashPrefix(physicalPath, info.Size())
		if err != nil {
			return false, false
		}
	}
	if !e.claudeRowlessFreshnessMarked(
		lookupPath, physicalPath, currentHash, info.ModTime().UnixNano(),
	) {
		return false, false
	}
	return true, true
}

func (e *Engine) claudeRowlessFreshnessCacheKey(
	path, contentHash string,
) string {
	if path == "" || contentHash == "" {
		return ""
	}
	prefixes := append([]string(nil), e.cwdFilter.prefixes...)
	slices.Sort(prefixes)
	filterHash := sha256.Sum256([]byte(strings.Join(prefixes, "\x00")))
	return providerAgentSkipCacheKey(path, parser.AgentClaude) +
		sourceHashSkipMarker + contentHash +
		"&data_version=" + strconv.Itoa(db.CurrentDataVersion()) +
		"&cwd_filter=" + fmt.Sprintf("%x", filterHash)
}

func (e *Engine) cacheClaudeRowlessFreshness(
	ctx context.Context, job syncJob,
) bool {
	if job.claudeRowlessFreshnessKey == "" || job.mtime == 0 ||
		job.noCacheSkip {
		return false
	}
	if !e.claudeSourceHasOnlyPreservedStaleForks(
		ctx, e.effectiveSourcePath(job.path),
	) {
		return false
	}
	e.cacheSkip(
		job.claudeRowlessFreshnessKey, job.mtime, job.sourceFingerprint,
	)
	return true
}

func (e *Engine) claudeRowlessFreshnessMarked(
	lookupPath, physicalPath, contentHash string,
	mtime int64,
) bool {
	paths := []string{lookupPath}
	if physicalPath != lookupPath {
		paths = append(paths, physicalPath)
	}
	e.skipMu.RLock()
	defer e.skipMu.RUnlock()
	for _, path := range paths {
		key := e.claudeRowlessFreshnessCacheKey(path, contentHash)
		if key != "" && e.skipCache[key] == mtime {
			return true
		}
	}
	return false
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
	ctx context.Context,
	provider parser.Provider,
	agent parser.AgentType,
	filePath string,
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

		// Freebuff shares the Codebuff provider. Query both agent types
		// so stale rows and resurrection guards work for both.
		agentsToQuery := []string{string(agent)}
		if agent == parser.AgentCodebuff {
			agentsToQuery = append(agentsToQuery, string(parser.AgentFreebuff))
		}
		var existingIDs []string
		primaryErr := make(map[string]error, len(agentsToQuery))
		for _, agentStr := range agentsToQuery {
			ids, err := e.db.ListSessionIDsByFilePath(lookupPath, agentStr)
			if err != nil {
				primaryErr[agentStr] = err
				continue
			}
			existingIDs = append(existingIDs, ids...)
		}
		// One-shot retry: any agent that errored on the first call gets
		// one more chance. Successful retries absorb their IDs into
		// existingIDs and clear the per-agent error so a transient
		// primary-agent failure does not propagate as a full-failure.
		if len(primaryErr) > 0 {
			for agentStr := range primaryErr {
				ids, retryErr := e.db.ListSessionIDsByFilePath(lookupPath, agentStr)
				if retryErr != nil {
					continue
				}
				existingIDs = append(existingIDs, ids...)
				delete(primaryErr, agentStr)
			}
		}
		// Bail when any agent's identity lookup remains unsuccessful
		// after retry. Continuing with partial lifecycle information
		// can recreate a trashed identity or leave both Codebuff and
		// Freebuff classifications active for the same file.
		if len(primaryErr) > 0 {
			var failedAgents []string
			var failedErrs []error
			for _, agentStr := range agentsToQuery {
				if err, ok := primaryErr[agentStr]; ok {
					failedAgents = append(failedAgents, agentStr)
					failedErrs = append(failedErrs, err)
				}
			}
			sort.Strings(failedAgents)
			res.err = fmt.Errorf(
				"list session IDs by file path %q for agents %v: %w",
				lookupPath, failedAgents, errors.Join(failedErrs...),
			)
			res.noCacheSkip = true
			kept = kept[:0]
			res.results = kept
			// Per-event work must drop the digest write too: an error
			// path must not stamp a row that suppresses real drift on the
			// next warm sync.
			return
		}

		// Resurrection guard. The path's identity is removed when a trashed row
		// shares it, or when any alternate identity for the path (the
		// provider's excluded fallback IDs or a stale stored ID) is trashed or
		// permanently excluded. In that case the new row must not be written.
		suppress := false
		for _, agentStr := range agentsToQuery {
			if e.db.HasTrashedSessionByFilePath(lookupPath, agentStr) {
				suppress = true
				break
			}
		}
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
	if len(kept) > 0 && filePath != "" {
		// Per-event work gates the digest stage by what actually got kept
		// (an empty kept must not stamp a row that would suppress real
		// drift on the next warm sync). The digest itself is the pre-parse
		// snapshot processProviderFile captured before fingerprinting or
		// parsing (preParseStatHash), so it cannot describe a file state
		// the parse never read; the fallback below recomputes from the
		// physical on-disk chat path only for paths that bypassed
		// processProviderFile's capture. The cache key uses the
		// pathRewriter's "host:/remote/path" form so remote-synced
		// sources keep hashing a real local file but read back under
		// the canonical logical key.
		//
		// The digest is persisted only after the matching source's
		// sessions-table write commits successfully. The per-row persist
		// gate in flushPending and the single-session writeSessionFull
		// loop is what guarantees provider_freshness only sees digests
		// whose matching session row actually committed; a CWD-filter
		// veto, a failed upsert, or a parser-skipped session all bypass
		// the persist call and keep the side-table clean.
		// Fallback staging for results that bypassed processProviderFile's
		// pre-parse capture (res.providerStatHash == nil), applying the
		// same digest-eligibility rule so a path-rewritten import of a
		// content-authority provider never re-stages what the pre-parse
		// site deliberately withheld.
		if res.providerStatHash == nil &&
			e.providerStatDigestEligible(agent) {
			if hasher, ok := e.providerStatHashers[agent]; ok {
				targetKey := filePath
				if e.pathRewriter != nil {
					targetKey = e.pathRewriter(filePath)
				}
				if targetKey != "" {
					res.providerStatHash = &pendingProviderStatHash{
						agent:        agent,
						physicalPath: filePath,
						targetKey:    targetKey,
						digest:       hasher.ComputeMultiFileStatHash(filePath),
					}
				}
			}
		}
		// Test observability: this counter lets tests distinguish a
		// regression that drops just the staging block from one that
		// drops the entire freshness gate. See stagedProviderStatHashes
		// on Engine and the per-row suppress test in
		// codebuff_integration_test.go.
		if res.providerStatHash != nil {
			e.stagedProviderStatHashes.Add(1)
		}
	}
}

// deleteParserExcludedSessions deletes rows the current parser deliberately
// excludes, without recording a permanent user deletion. The returned IDs are
// safe to exclude from resync's orphan copy.
func (e *Engine) deleteParserExcludedSessions(
	res processResult,
	sourceAllowed bool,
) ([]string, error) {
	if !sourceAllowed {
		return nil, nil
	}

	excluded := e.applyIDPrefixToSessionIDs(res.excludedSessionIDs)
	if len(excluded) > 0 {
		if _, err := e.db.DeleteParserExcludedSessions(excluded); err != nil {
			return nil, err
		}
	}
	return excluded, nil
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
		RequireFreshSource: !e.forceParseRequested(file),
	})
}

type sourceCwdPathKey struct {
	path  string
	agent string
}

type sourceCwdSessionKey struct {
	id    string
	path  string
	agent string
}

type sourceCwdReconciliationBatch struct {
	mu       gosync.Mutex
	sessions map[sourceCwdSessionKey]string
	paths    map[sourceCwdPathKey]string
}

func newSourceCwdReconciliationBatch() *sourceCwdReconciliationBatch {
	return &sourceCwdReconciliationBatch{
		sessions: make(map[sourceCwdSessionKey]string),
		paths:    make(map[sourceCwdPathKey]string),
	}
}

type sourceCwdDecision struct {
	resolution parser.SourceCwdResolution
	storedCwd  string
	storedOK   bool
	forceParse bool
}

type sourceCwdReader interface {
	GetCwdByAgentPath(path, agent string) (string, bool)
}

// sourceCwdDecision compares provider-owned Cwd authority with the archive row
// before any generic freshness gate can discard the source.
func (e *Engine) sourceCwdDecision(
	source parser.SourceRef,
) sourceCwdDecision {
	resolution := source.CwdResolution
	decision := sourceCwdDecision{resolution: resolution}
	if resolution.State == parser.SourceCwdUnspecified || e.db == nil {
		return decision
	}
	path := e.sourceCwdLookupPath(source)
	reader := sourceCwdReader(e.db)
	if e.archiveStore != nil {
		if archived, ok := e.archiveStore.(sourceCwdReader); ok {
			reader = archived
		}
	}
	decision.storedCwd, decision.storedOK = reader.GetCwdByAgentPath(
		path, string(source.Provider),
	)
	switch resolution.State {
	case parser.SourceCwdResolved:
		decision.forceParse = decision.storedOK &&
			decision.storedCwd != resolution.Path
	case parser.SourceCwdNone, parser.SourceCwdAmbiguous,
		parser.SourceCwdRemote:
		decision.forceParse = decision.storedOK && decision.storedCwd != ""
	}
	return decision
}

func (e *Engine) sourceCwdLookupPath(source parser.SourceRef) string {
	path := providerDiscoveredPath(source)
	if path == "" {
		path = source.FingerprintKey
	}
	if e.pathRewriter != nil {
		path = e.pathRewriter(path)
	}
	return path
}

func sourceCwdParticipates(resolution parser.SourceCwdResolution) bool {
	return resolution.State != parser.SourceCwdUnspecified
}

func sourceCwdForFilter(parsed string, decision sourceCwdDecision) string {
	switch decision.resolution.State {
	case parser.SourceCwdResolved:
		return decision.resolution.Path
	case parser.SourceCwdNone, parser.SourceCwdAmbiguous,
		parser.SourceCwdRemote:
		return ""
	case parser.SourceCwdUnavailable:
		if decision.storedOK && decision.storedCwd != "" {
			return decision.storedCwd
		}
	}
	return parsed
}

func (e *Engine) reconcileFilteredSourceCwd(
	results []parser.ParseResult, decision sourceCwdDecision,
) (bool, error) {
	if e.cwdFilter.empty() || !sourceCwdParticipates(decision.resolution) {
		return false, nil
	}
	target := sourceCwdForFilter("", decision)
	changed := false
	for _, result := range results {
		if e.cwdFilter.allows(sourceCwdForFilter(
			result.Session.Cwd, decision,
		)) {
			continue
		}
		id := applyIDPrefixToID(e.idPrefix, result.Session.ID)
		agent := string(result.Session.Agent)
		path := e.effectiveSourcePath(result.Session.File.Path)
		if id == "" || agent == "" || path == "" {
			continue
		}
		key := sourceCwdSessionKey{id: id, path: path, agent: agent}
		if e.deferredSourceCwd != nil {
			e.deferredSourceCwd.mu.Lock()
			e.deferredSourceCwd.sessions[key] = target
			e.deferredSourceCwd.mu.Unlock()
		} else {
			updated, err := e.db.UpdateSessionCwdByIdentity(
				id, path, agent, target,
			)
			if err != nil {
				return changed, err
			}
			changed = changed || updated
		}
	}
	return changed, nil
}

func (e *Engine) reconcileSourceCwdByPath(
	source parser.SourceRef, decision sourceCwdDecision,
) (bool, error) {
	changed, err := e.reconcileSourceCwdAtPath(
		e.sourceCwdLookupPath(source), source.Provider, decision,
	)
	return changed, err
}

func (e *Engine) reconcileSourceCwdAtPath(
	path string, agent parser.AgentType, decision sourceCwdDecision,
) (bool, error) {
	if !sourceCwdParticipates(decision.resolution) ||
		path == "" || e.db == nil {
		return false, nil
	}
	target := sourceCwdForFilter("", decision)
	// The decision already captured the stored Cwd for this source under the
	// same locked pass, so a second per-source point query is redundant.
	if !decision.storedOK || decision.storedCwd == target {
		return false, nil
	}
	if e.deferredSourceCwd != nil {
		e.deferredSourceCwd.mu.Lock()
		e.deferredSourceCwd.paths[sourceCwdPathKey{
			path: path, agent: string(agent),
		}] = target
		e.deferredSourceCwd.mu.Unlock()
	} else {
		updated, err := e.db.UpdateCwdByAgentPathCount(
			path, string(agent), target,
		)
		if err != nil {
			return false, err
		}
		return updated > 0, nil
	}
	return true, nil
}

func (e *Engine) reconcileSkippedSourceCwd(r syncJob) (bool, error) {
	if r.sourceCwdResolution.State == parser.SourceCwdUnspecified {
		return false, nil
	}
	return e.reconcileSourceCwdAtPath(
		r.sourceCwdPath,
		r.sourceCwdAgent,
		sourceCwdDecision{
			resolution: r.sourceCwdResolution,
			storedCwd:  r.sourceCwdStored,
			storedOK:   r.sourceCwdStoredOK,
		},
	)
}

func (e *Engine) applyDeferredSourceCwd(
	target *db.DB, batch *sourceCwdReconciliationBatch,
) (int, error) {
	if batch == nil {
		return 0, nil
	}
	batch.mu.Lock()
	defer batch.mu.Unlock()
	updated := 0
	for key, cwd := range batch.sessions {
		rowUpdated, err := target.UpdateSessionCwdByIdentity(
			key.id, key.path, key.agent, cwd,
		)
		if err != nil {
			return updated, err
		}
		if rowUpdated {
			updated++
		}
	}
	for key, cwd := range batch.paths {
		count, err := target.UpdateCwdByAgentPathCount(
			key.path, key.agent, cwd,
		)
		if err != nil {
			return updated, err
		}
		updated += count
	}
	return updated, nil
}

func applySourceCwdResolution(
	s *db.Session, resolution parser.SourceCwdResolution,
	storedCwd string, storedOK bool,
) {
	switch resolution.State {
	case parser.SourceCwdResolved:
		s.Cwd = resolution.Path
	case parser.SourceCwdNone, parser.SourceCwdAmbiguous,
		parser.SourceCwdRemote:
		s.Cwd = ""
	case parser.SourceCwdUnavailable:
		if storedOK && storedCwd != "" {
			s.Cwd = storedCwd
		}
	}
}

func providerProcessCacheKey(
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	providerSemantics parser.ProviderSyncSemantics,
) string {
	key := plannedSkipKey(source, fingerprint)
	if key == "" {
		key = file.Path
	}
	agent := file.Agent
	if agent == "" {
		agent = source.Provider
	}
	key = providerAgentSkipCacheKey(key, agent)
	key = providerProcessCacheKeyWithHash(
		key, fingerprint, providerSemantics,
	)
	// A whole-container omnigent cache identity includes the parser data
	// version: the container has no stored row of its own, so a restart must
	// not accept an entry recorded by an older parser version.
	if parser.IsOmnigentContainerSource(source) {
		separator := "?"
		if strings.Contains(key, "?") {
			separator = "&"
		}
		key += separator + "data_version=" + strconv.Itoa(db.CurrentDataVersion())
	}
	return key
}

const providerAgentSkipMarker = "?agent="

// providerAgentSkipCacheKey prevents providers with overlapping roots or a
// shared on-disk format from inheriting another agent's cached source state.
// The path stays first so remote-cache path translation remains valid.
func providerAgentSkipCacheKey(key string, agent parser.AgentType) string {
	if key == "" || agent == "" {
		return key
	}
	return key + providerAgentSkipMarker + string(agent)
}

// SplitProviderSkipCachePath separates the filesystem path from the provider
// qualifier carried by a skip-cache identity. Remote import translates only
// the path and reattaches the suffix after the path mapping succeeds.
func SplitProviderSkipCachePath(key string) (path, suffix string) {
	path, qualified, ok := strings.Cut(key, providerAgentSkipMarker)
	if !ok {
		return key, ""
	}
	return path, providerAgentSkipMarker + qualified
}

// legacyProviderSkipCacheKey removes the agent qualifier from a provider cache
// key while retaining any hash or data-version suffix. Successful processing
// clears this predecessor alongside the scoped key so upgrades do not retain
// dead path-only cache entries.
func legacyProviderSkipCacheKey(key string) string {
	base, qualified, ok := strings.Cut(key, providerAgentSkipMarker)
	if !ok {
		return ""
	}
	_, suffix, hasSuffix := strings.Cut(qualified, "?")
	if !hasSuffix {
		return base
	}
	return base + "?" + suffix
}

func providerProcessCacheKeyWithHash(
	key string,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) string {
	if key == "" {
		return ""
	}
	if fingerprint.Hash == "" || !semantics.FingerprintHashInCacheKey {
		return key
	}
	return key + "?source_hash=" + fingerprint.Hash
}

// providerSkipCacheEntryFreshInDB reports whether a skip-cache hit is
// still consistent with the archive. rowHashVerified additionally reports
// that the current fingerprint's content hash was compared against a
// stored session row and matched -- the only cache outcome strong enough
// to stamp a provider_freshness digest, since the other fresh returns
// (hash-free semantics, whole-container identity, no stored row yet)
// never consult a row.
func (e *Engine) providerSkipCacheEntryFreshInDB(
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	providerSemantics parser.ProviderSyncSemantics,
) (fresh, rowHashVerified bool) {
	agent := file.Agent
	if agent == "" {
		agent = source.Provider
	}
	if fingerprint.Hash == "" ||
		!providerSemantics.FingerprintHashRequiredForFreshness {
		return true, false
	}
	if parser.IsOmnigentContainerSource(source) {
		// A whole-container omnigent source has only virtual member rows in
		// the archive, so its entry is fresh without ever finding a row for
		// the physical container path: the cache identity already carries the
		// container hash and parser data version. This is distinct from
		// ProviderSyncSemantics.SkipCacheFreshWithoutStoredRow below, which
		// trusts an entry only while NO row exists yet and resumes stored-row
		// hash validation once the provider persists one.
		return true, false
	}
	lookupPath := providerSkipLookupPath(file, source, fingerprint)
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(lookupPath)
	}
	if providerSemantics.SkipCacheFreshWithoutStoredRow {
		storedIDs, err := e.db.ListSessionIDsByFilePath(
			lookupPath, string(agent),
		)
		if err == nil && len(storedIDs) == 0 {
			// A cached parse failure or intentionally ignored source has no
			// persisted row or hash to compare. Retry suppression is therefore
			// mtime/source-signal based until a row exists: a same-mtime rewrite
			// cannot be distinguished in this no-row state. Hash validation applies
			// once a session has actually been stored.
			return true, false
		}
	}
	matched := e.providerFingerprintHashMatchesDB(
		agent, lookupPath, fingerprint,
		providerSemantics.FingerprintHashRequiredForFreshness,
	)
	return matched, matched
}

func processFileUsesProvider(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentForge, parser.AgentGoose, parser.AgentPiebald,
		parser.AgentWarp, parser.AgentZCode:
		return true
	default:
		return false
	}
}

func (e *Engine) shouldSkipProviderSource(
	file parser.DiscoveredFile,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) bool {
	agent := file.Agent
	if agent == "" {
		agent = source.Provider
	}
	if !providerSourceSupportsPersistedFreshness(agent) {
		return false
	}
	if e.forceParseRequested(file) {
		return false
	}
	lookupPath := providerSkipLookupPath(file, source, fingerprint)
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(lookupPath)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByAgentPath(
		lookupPath, string(agent),
	)
	if !ok {
		return false
	}
	if fingerprint.Size != 0 && storedSize != fingerprint.Size {
		return false
	}
	if storedMtime != fingerprint.MTimeNS {
		return false
	}
	if !e.providerFingerprintHashMatchesDB(
		agent, lookupPath,
		fingerprint,
		semantics.FingerprintHashRequiredForFreshness,
	) {
		return false
	}
	return e.db.GetDataVersionByAgentPath(
		lookupPath, string(agent),
	) >= db.CurrentDataVersion()
}

func providerSourceSupportsPersistedFreshness(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentForge, parser.AgentGoose, parser.AgentWarp, parser.AgentZCode:
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
	if file.Agent == parser.AgentZCode {
		if filepath.Base(file.Path) == parser.ZCodeDBName {
			return false
		}
		if _, _, ok := parser.ParseVirtualSourcePathForBase(file.Path, parser.ZCodeDBName); ok {
			return false
		}
	}
	if file.Agent == parser.AgentGoose {
		if filepath.Base(file.Path) == parser.GooseDBName {
			return false
		}
		if _, _, ok := parser.ParseVirtualSourcePathForBase(
			file.Path, parser.GooseDBName,
		); ok {
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
	if file.Agent == parser.AgentTrae {
		if filepath.Base(file.Path) == "state.vscdb" {
			return false
		}
		if _, _, ok := parser.SplitTraeVirtualPath(file.Path); ok {
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

const sourceHashSkipMarker = "?source_hash="

// normalizeSourceHashSkipCache performs the one archive-sized pass needed to
// repair legacy duplicate source-hash entries and build the watcher-time
// sibling index. A family with multiple hashed keys is ambiguous: same-mtime
// rewrites are why the hash exists, so no stored key can safely be called
// current. Drop that family so the source reparses once and establishes a
// trustworthy key.
func normalizeSourceHashSkipCache(
	cache map[string]int64, fingerprints map[string]string,
) (map[string]string, map[string]struct{}) {
	index := make(map[string]string)
	counts := make(map[string]int)
	ambiguous := make(map[string]struct{})
	for path := range cache {
		base, _, hashed := strings.Cut(path, sourceHashSkipMarker)
		if !hashed {
			continue
		}
		counts[base]++
		if counts[base] == 1 {
			index[base] = path
		}
	}
	for path := range cache {
		base, _, hashed := strings.Cut(path, sourceHashSkipMarker)
		if hashed && counts[base] > 1 {
			delete(cache, path)
			ambiguous[base] = struct{}{}
			if fingerprints != nil {
				delete(fingerprints, path)
			}
		}
	}
	for base := range index {
		delete(cache, base)
		if fingerprints != nil {
			delete(fingerprints, base)
		}
		if counts[base] > 1 {
			delete(index, base)
		}
	}
	return index, ambiguous
}

// cacheSkip records a file so it won't be retried until its mtime changes.
// The returned work count measures sibling-index probes and is used by the
// cardinality regression to keep watcher-time work independent of cache size.
func (e *Engine) cacheSkip(
	path string, mtime int64, sourceFingerprint ...string,
) int {
	e.skipMu.Lock()
	work := e.removeSkipHashSiblingsLocked(path)
	e.skipCache[path] = mtime
	if base, _, hashed := strings.Cut(path, sourceHashSkipMarker); hashed {
		e.skipHashKeys[base] = path
	}
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
	return work
}

// clearSkip removes a skip-cache entry when a file produces a valid session.
// Its work count has the same cardinality-regression role as cacheSkip's.
func (e *Engine) clearSkip(path string) int {
	work, err := e.clearSkipPersistent(path)
	if err != nil {
		log.Printf("clearing persisted skip cache for %s: %v", path, err)
	}
	return work
}

func (e *Engine) clearSkipInMemory(path string) int {
	e.skipMu.Lock()
	defer e.skipMu.Unlock()
	work := e.removeSkipHashSiblingsLocked(path)
	delete(e.skipCache, path)
	delete(e.skipFingerprints, path)
	legacyPath := legacyProviderSkipCacheKey(path)
	if legacyPath != "" {
		work += e.removeSkipHashSiblingsLocked(legacyPath)
		delete(e.skipCache, legacyPath)
		delete(e.skipFingerprints, legacyPath)
	}
	return work
}

func (e *Engine) clearSkipPersistent(path string) (int, error) {
	work := e.clearSkipInMemory(path)
	legacyPath := legacyProviderSkipCacheKey(path)
	if e.ephemeral {
		return work, nil
	}
	base, _, _ := strings.Cut(path, sourceHashSkipMarker)
	err := e.db.DeleteSkippedFileAndPrefix(
		base, base+sourceHashSkipMarker,
	)
	if legacyPath != "" {
		legacyBase, _, _ := strings.Cut(legacyPath, sourceHashSkipMarker)
		err = errors.Join(err, e.db.DeleteSkippedFileAndPrefix(
			legacyBase, legacyBase+sourceHashSkipMarker,
		))
	}
	return work, err
}

func (e *Engine) removeSkipHashSiblingsLocked(path string) int {
	if e.skipHashKeys == nil {
		e.skipHashKeys, _ = normalizeSourceHashSkipCache(
			e.skipCache, e.skipFingerprints,
		)
	}
	base, _, hasHash := strings.Cut(path, sourceHashSkipMarker)
	if !hasHash {
		if sibling, ok := e.skipHashKeys[path]; ok {
			delete(e.skipCache, sibling)
			delete(e.skipFingerprints, sibling)
			delete(e.skipHashKeys, path)
		}
		return 1
	}
	delete(e.skipCache, base)
	delete(e.skipFingerprints, base)
	if sibling, ok := e.skipHashKeys[base]; ok {
		delete(e.skipCache, sibling)
		delete(e.skipFingerprints, sibling)
		delete(e.skipHashKeys, base)
	}
	return 2
}

// clearWatcherOverflowCaches invalidates every freshness shortcut whose
// correctness can depend on receiving a concrete changed path. The following
// forced discovery pass rebuilds these caches from parsed source state.
func (e *Engine) clearWatcherOverflowCaches() {
	e.skipMu.Lock()
	e.skipCache = make(map[string]int64)
	e.skipFingerprints = make(map[string]string)
	e.skipHashKeys = make(map[string]string)
	e.skipMu.Unlock()
	if !e.ephemeral {
		if err := e.db.ReplaceSkippedFiles(map[string]int64{}); err != nil {
			log.Printf("clearing skipped files after watcher overflow: %v", err)
		}
	}
	e.clearTrustedOpenCodeStorageSessions()
	e.clearTrustedSQLiteContainers()
	e.clearVerifiedSources()
	parser.EvictAllCodexSessionIndexes()
}

// InjectSkipCache merges entries into the in-memory skip
// cache. Used by remote sync to pre-populate with
// translated paths.
func (e *Engine) InjectSkipCache(entries map[string]int64) {
	e.skipMu.Lock()
	defer e.skipMu.Unlock()
	if e.skipHashKeys == nil {
		e.skipHashKeys, _ = normalizeSourceHashSkipCache(
			e.skipCache, e.skipFingerprints,
		)
	}
	incoming := make(map[string]int64, len(entries))
	maps.Copy(incoming, entries)
	_, ambiguous := normalizeSourceHashSkipCache(incoming, nil)
	for base := range ambiguous {
		e.removeSkipHashSiblingsLocked(base + sourceHashSkipMarker)
	}
	for path, mtime := range incoming {
		e.removeSkipHashSiblingsLocked(path)
		e.skipCache[path] = mtime
		if base, _, hashed := strings.Cut(path, sourceHashSkipMarker); hashed {
			e.skipHashKeys[base] = path
		}
	}
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

// SnapshotRetrySafeSkipCache returns cache state that can survive a failed
// replacement-database rebuild. Entries for sources that changed exclusion or
// source-missing state are omitted because those changes exist only in the
// replacement database that will be discarded.
func (e *Engine) SnapshotRetrySafeSkipCache() map[string]int64 {
	e.skipMu.RLock()
	defer e.skipMu.RUnlock()
	out := make(map[string]int64, len(e.skipCache))
	for key, mtime := range e.skipCache {
		path, _ := SplitProviderSkipCachePath(key)
		if _, unsafe := e.retryUnsafeSkipPaths[path]; unsafe {
			continue
		}
		out[key] = mtime
	}
	return out
}

func (e *Engine) markRetryUnsafeSkipSource(path string) {
	if path == "" {
		return
	}
	e.skipMu.Lock()
	if e.retryUnsafeSkipPaths == nil {
		e.retryUnsafeSkipPaths = make(map[string]struct{})
	}
	e.retryUnsafeSkipPaths[path] = struct{}{}
	e.skipMu.Unlock()
}

// persistSkipCache writes the in-memory skip cache to the
// database so skipped files survive process restarts.
// Returns the number of entries persisted.
func (e *Engine) persistSkipCache() int {
	return e.persistSkipCacheInto(e.db)
}

// persistSkipCacheInto writes the current skip cache into target. A resync build
// persists into the replacement database (not the active one) so the post-swap
// engine reloads warm skip state.
func (e *Engine) persistSkipCacheInto(target *db.DB) int {
	if e.ephemeral {
		return 0
	}
	e.skipMu.RLock()
	snapshot := make(map[string]int64, len(e.skipCache))
	maps.Copy(snapshot, e.skipCache)
	e.skipMu.RUnlock()

	if err := target.ReplaceSkippedFiles(snapshot); err != nil {
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
// shouldSkipByPath for the provider-authoritative runtime. For providers whose
// fingerprint stat is a shared container's (see
// providerFingerprintHashEstablishesFreshness), a matching stored content hash
// establishes freshness even when the stat moved. A source with no stored row,
// an empty key, or a non-fingerprint identity (no size, e.g. a tombstone)
// never matches and therefore reparses.
func (e *Engine) providerSourceUnchangedInDB(
	ctx context.Context,
	source parser.SourceRef,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
	preParseStatHash *pendingProviderStatHash,
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
	agent := source.Provider
	storedSize, storedMtime, ok := e.db.GetFileInfoByAgentPath(
		lookupPath, string(agent),
	)
	if !ok {
		return false
	}
	if storedSize != fingerprint.Size || storedMtime != fingerprint.MTimeNS {
		if !e.providerSourceHashFreshDespiteStat(
			source.Provider, lookupPath, fingerprint,
		) {
			return false
		}
	} else if !e.providerFingerprintHashMatchesDB(
		agent, lookupPath,
		fingerprint,
		semantics.FingerprintHashRequiredForFreshness,
	) {
		return false
	}
	// A stale stored project (e.g. a generated roborev CI worktree name)
	// must defeat the unchanged-source skip so the corrected project is
	// reparsed, mirroring shouldSkipCodexFingerprint and the in-memory
	// skip-cache bypass in processProviderFile.
	if project, ok := e.db.GetProjectByAgentPath(
		lookupPath, string(agent),
	); ok &&
		parser.NeedsProjectReparse(project) {
		return false
	}
	// Only a source confirmed unchanged against an existing current session
	// row may earn a provider_freshness side-table stamp. This is the
	// DB-confirmed skip site the cold-start branch of
	// providerSourceFreshBeforeFingerprint closes its loop through: a
	// content-unchanged source with no stored digest flows fingerprint →
	// this skip → stamp, without ever persisting a digest before an outcome
	// the engine can trust.
	fresh := e.db.GetDataVersionByAgentPath(
		lookupPath, string(agent),
	) >= db.CurrentDataVersion()
	if fresh {
		e.stampProviderStatHashForConfirmedSource(ctx, preParseStatHash)
	}
	return fresh
}

// stampProviderStatHashForConfirmedSource persists the pre-parse
// per-component stat digest for a source whose current content was just
// verified against an existing current session row: the Claude
// content-verified single-session skip, the hash-validated cache skip,
// the Codex-family DB-fingerprint skip, and providerSourceUnchangedInDB.
// These are the only pre-write moments a digest may safely be written: a
// transient failure between an eager stamp and the fingerprint/parse/write
// that follows would leave a matching digest that permanently suppresses
// every later retry. The digest is the
// pre-parse snapshot captured in processProviderFile, so it describes
// exactly the file state the fingerprint verified, and the DB key uses the
// pathRewriter's logical key, mirroring the write path. Providers without
// a MultiFileStatHasher carry no digest and are skipped here; their
// freshness is owned by the engine's existing stat and skip-cache paths.
func (e *Engine) stampProviderStatHashForConfirmedSource(
	ctx context.Context,
	statHash *pendingProviderStatHash,
) {
	if statHash == nil || statHash.digest == 0 {
		return
	}
	if !providerStatHashMetadataVerified(*statHash) {
		return
	}
	if err := e.db.UpsertProviderStatHash(
		ctx, statHash.agent, statHash.targetKey, statHash.digest,
	); err != nil {
		log.Printf(
			"provider_freshness write for %s/%s: %v",
			statHash.agent, statHash.targetKey, err,
		)
	}
}

func (e *Engine) providerFingerprintHashMatchesDB(
	agent parser.AgentType,
	lookupPath string,
	fingerprint parser.SourceFingerprint,
	required bool,
) bool {
	if fingerprint.Hash == "" || !required {
		return true
	}
	storedHash, ok := e.db.GetFileHashByAgentPath(
		lookupPath, string(agent),
	)
	return ok && storedHash == fingerprint.Hash
}

// providerFingerprintHashEstablishesFreshness reports whether a matching
// stored fingerprint hash alone proves a source unchanged when its stored
// size/mtime no longer match. Hermes state members inherit their shared
// container's stat (state.db size and mtime), so any single-member change
// invalidates every member's stat identity; the per-member hash covers the
// member's full parse input (session row, messages, selected transcript), so
// a matching stored hash bounds re-parse and rewrite work to the changed
// members instead of the whole archive. Providers whose fingerprint stat is
// per-source stay stat-gated: a stat mismatch there means real change.
func providerFingerprintHashEstablishesFreshness(agent parser.AgentType) bool {
	return agent == parser.AgentHermes
}

// providerSourceHashFreshDespiteStat is the stat-mismatch arm of
// providerSourceUnchangedInDB. Unlike providerFingerprintHashMatchesDB, an
// absent hash can never establish freshness here: the stat already disagrees,
// so only a positive content match may skip. GetFileHashByAgentPath excludes
// recoverable source-missing tombstones, so a returning member still revives
// through a full parse.
func (e *Engine) providerSourceHashFreshDespiteStat(
	agent parser.AgentType,
	lookupPath string,
	fingerprint parser.SourceFingerprint,
) bool {
	if fingerprint.Hash == "" || !providerFingerprintHashEstablishesFreshness(agent) {
		return false
	}
	storedHash, ok := e.db.GetFileHashByAgentPath(
		lookupPath, string(agent),
	)
	return ok && storedHash == fingerprint.Hash
}

// shouldSkipByPath checks file size and mtime against what is
// stored in the database by file_path. Used for codex/gemini
// files where the session ID requires parsing.
func (e *Engine) shouldSkipByPath(
	path string, info os.FileInfo,
) bool {
	if e.forceParse || e.forceFullParse {
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
// up to date: the source size and mtime match what is stored, every active row
// for the source is at the current parser data version, and the main row's
// project does not need reparse. It reproduces the legacy Claude process arm's
// shouldSkipFile gate so an unchanged session is skipped instead of re-parsed
// every full sync. Providers without incremental append, multi-session sources,
// or sources that are not a single physical file are never considered fresh
// here and always fall through to the full parse.
func (e *Engine) providerSingleSessionFresh(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
	file parser.DiscoveredFile,
) (mtime int64, fresh bool, forceReplace bool, contentVerified bool) {
	// Match the legacy shouldSkipFile gate, which keyed off the
	// engine-wide forceParse (parse-diff) flag only. A per-file
	// ForceParse (set by SyncSingleSession to bypass the error skip
	// cache) must not defeat the DB-freshness skip: an unchanged session
	// is still skipped so a single-session resync does not, for example,
	// reapply a worktree project mapping to a file that has not changed.
	if e.forceParse || e.forceFullParse || file.ForceFullParse {
		return 0, false, false, false
	}
	// Claude is the single-physical-file provider that takes the
	// append-only incremental path. Its source stem is the session ID,
	// so DB freshness can be checked by that ID even though a DAG fork
	// can later split the file into several sessions.
	if provider.Definition().Type != parser.AgentClaude {
		return 0, false, false, false
	}
	if provider.Capabilities().Source.IncrementalAppend !=
		parser.CapabilitySupported {
		return 0, false, false, false
	}
	path := providerDiscoveredPath(source)
	if path == "" {
		return 0, false, false, false
	}
	sessionID := claudeSessionIDFromPath(path)
	if sessionID == "" {
		return 0, false, false, false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	fullID := applyIDPrefixToID(e.idPrefix, sessionID)
	// statPath is the on-disk file the stat came from: lookupPath when it
	// resolves (local sync, where lookupPath == path), otherwise the physical
	// source path (remote sync rewrites path to a non-local logical key). The
	// content guard below hashes statPath, so it must be the openable file.
	statPath := lookupPath
	info, err := os.Stat(lookupPath)
	if err != nil {
		statPath = path
		info, err = os.Stat(path)
		if err != nil {
			return 0, false, false, false
		}
	}
	// A source-missing primary tombstone must read as absent here: it cannot
	// vouch for the source (shouldSkipFile ignores it), so only the rowless
	// marker can prove an unchanged source that no longer admits a primary.
	primaryPath := e.db.GetSessionFilePathNotSourceMissing(fullID)
	if primaryPath == "" {
		fresh, contentVerified := e.claudeSourceFreshWithoutPrimary(
			ctx, lookupPath, statPath, info, "",
		)
		if !fresh {
			return 0, false, false, false
		}
		if e.providerIncrementalIdentityChanged(lookupPath, info) {
			return 0, false, true, false
		}
		return info.ModTime().UnixNano(), true, false, contentVerified
	}
	if primaryPath != lookupPath {
		// A directory rename can preserve size, mtime, inode, and content.
		// Freshness by session ID alone would keep the vanished source path
		// forever instead of parsing the discovered destination and updating
		// source ownership.
		return 0, false, false, false
	}
	if !e.shouldSkipFile(sessionID, info) {
		return 0, false, false, false
	}
	// Claude transcripts can fan out into several active DAG sessions. The
	// filename stem identifies only the main row, so its current version cannot
	// hide a stale restored fork that shares the same source path. Forks the CWD
	// filter intentionally preserves are the exception: the filter-scoped
	// cleanup guard below re-evaluates them on every pass.
	_, sourceDataVersion, _, _, sourceFound :=
		e.db.GetSourceRepairStateByAgentPath(
			lookupPath, string(parser.AgentClaude),
		)
	if !sourceFound ||
		(sourceDataVersion < db.CurrentDataVersion() &&
			!e.claudeSourceStaleRowsArePreservedForks(
				ctx, lookupPath, true,
			)) {
		return 0, false, false, false
	}
	if e.providerIncrementalIdentityChanged(lookupPath, info) {
		return 0, false, true, false
	}
	contentChanged, contentVerified := e.providerIncrementalContentChanged(
		fullID, statPath, info,
	)
	if contentChanged {
		return 0, false, true, contentVerified
	}
	if sourceDataVersion < db.CurrentDataVersion() &&
		e.claudeSourceNeedsStaleForkReconciliation(ctx, lookupPath) {
		// A baseline-free upgrade parse can refresh the primary row before it
		// has authority to tombstone an omitted legacy fork. Keep parsing this
		// exact source until a later baseline-backed pass finishes cleanup.
		return 0, false, false, false
	}
	sess, _ := e.db.GetSession(ctx, fullID)
	return info.ModTime().UnixNano(), sess != nil &&
		sess.Project != "" &&
		!parser.NeedsProjectReparse(sess.Project), false, contentVerified
}

func (e *Engine) providerIncrementalIdentityChanged(
	lookupPath string,
	info os.FileInfo,
) bool {
	if e.pathRewriter != nil {
		// Remote imports rewrite per-run temp paths to stable source paths;
		// the temp inode is expected to change between identical downloads.
		return false
	}
	curInode, curDevice := getFileIdentity(lookupPath, info)
	return e.db.FileIdentityChanged(lookupPath, curInode, curDevice)
}

// providerIncrementalContentChanged reports whether a single-session JSONL
// source whose size, mtime, and file identity already match the stored row
// nonetheless holds different bytes than were last parsed. It is the last
// guard against a same-size, same-mtime, same-inode in-place rewrite: two
// fast writes landing in one filesystem mtime granule (or a coarse-mtime
// filesystem) leave every stat signal identical, so only the content hash
// distinguishes a genuine rewrite from an unchanged file.
//
// hashPath is the physical file the stat came from -- the local path for
// local sources, the materialized download for remote (path-rewritten)
// sources. The stored file_hash is computed over those same materialized
// bytes on both the full-parse (hashJSONLSourceFile) and incremental
// (ComputeFileHashPrefix) paths, so the on-disk prefix hash is directly
// comparable regardless of the logical key the row is stored under. That is
// also why this is the correct freshness signal for remote sync: every
// re-download gets a fresh inode, so the inode net is disabled to avoid a
// false-positive re-parse, but identical content still hashes equal here while
// a genuine rewrite does not. shouldSkipFile has already confirmed the stored
// file_size equals the current size, so the prefix hash covers the stored byte
// range. Rows without a stored hash (legacy or non-fingerprinted) report an
// unverified match. Gate-eligible local sources then fall through to the
// fingerprint path once, while sources that cannot use the local-stat gate
// preserve the legacy size/mtime/identity freshness behavior.
func (e *Engine) providerIncrementalContentChanged(
	fullID, hashPath string,
	info os.FileInfo,
) (changed, verified bool) {
	storedHash, ok := e.db.GetSessionFileHash(fullID)
	if !ok || storedHash == "" {
		return false, false
	}
	curHash, err := computeFileHashPrefix(hashPath, info.Size())
	if err != nil {
		return false, false
	}
	return curHash != storedHash, true
}

// providerStatFreshnessMtime derives the cache mtime key from the same
// rule the provider's cold-write fingerprint uses. Codebuff/Freebuff
// delegate to parser.CodebuffCompanionMtime, which is the single
// source of truth for the max(chat, run-state, chat-meta) derivation;
// Codex folds session_index.jsonl exactly as its fingerprint stamps
// file_mtime (parser.CodexEffectiveMtime), so a cold→warm cycle does
// not drift; other MultiFileStatHasher agents fall through to
// chat-only since they have no sibling companions to fold in.
func providerStatFreshnessMtime(
	agent parser.AgentType,
	lookupPath string,
	chatInfo os.FileInfo,
) int64 {
	switch agent {
	case parser.AgentCodebuff, parser.AgentFreebuff:
		return parser.CodebuffCompanionMtime(lookupPath, chatInfo)
	case parser.AgentCodex:
		return parser.CodexEffectiveMtime(
			lookupPath, chatInfo.ModTime().UnixNano(),
		)
	default:
		return chatInfo.ModTime().UnixNano()
	}
}

// providerStatDigestEligible reports whether this engine may stage,
// stamp, or consult a provider_freshness stat digest for the agent. The
// content-hashing single-file JSONL providers (Claude, Codex family) are
// ineligible under a pathRewriter: a remote import materializes a fresh
// physical file whose mtime is copied from the remote and whose ctime is
// the import clock, so a same-stat different-content re-download can
// collide with a stored digest inside one coarse filesystem timestamp
// tick and skip a real rewrite. Their remote freshness stays
// content-hash arbitrated, matching the pathRewriter carve-outs in
// providerIncrementalIdentityChanged. Codebuff stays eligible
// everywhere: its remote flow deliberately stamps the per-component
// composite under the logical key (see
// TestSyncCodebuffProviderStatHashRemoteStoresUnderLogicalKey).
func (e *Engine) providerStatDigestEligible(agent parser.AgentType) bool {
	if e.pathRewriter == nil {
		return true
	}
	return agent != parser.AgentClaude && !isCodexFormatAgent(agent)
}

// providerFreshnessAgents returns the stored-agent labels a provider's
// sessions may carry for digest-currency checks. Codebuff relabels
// free-tier sessions to Freebuff while discovery and the
// provider_freshness side-table stay keyed on Codebuff, so both labels
// are this provider's own rows; every other hasher stores under its
// discovery label.
func providerFreshnessAgents(agent parser.AgentType) []parser.AgentType {
	if agent == parser.AgentCodebuff {
		return []parser.AgentType{parser.AgentCodebuff, parser.AgentFreebuff}
	}
	return []parser.AgentType{agent}
}

// providerFreshDigestSourceCurrentInDB reports whether the active stored
// sessions for a digest-matched source are still current. User-trashed rows
// are resolved until restoration invalidates the digest and marks the row
// stale, so they do not participate in the repair check. A
// matching provider_freshness digest proves only that the file stat is
// unchanged; these checks are what allow the digest short-circuit to skip
// safely, mirroring the tail guards of providerSourceUnchangedInDB. Every
// lookup is scoped to the provider's own stored-agent labels: Codex and
// TraeX can index the same rollout path, and a path-only lookup would let
// this agent's skip borrow the other agent's newer row and hide a repair
// (e.g. a stale generated project) on its own row.
func (e *Engine) providerFreshDigestSourceCurrentInDB(
	agent parser.AgentType, lookupPath string,
) bool {
	rowFound := false
	for _, owned := range providerFreshnessAgents(agent) {
		if _, _, ok := e.db.GetFileInfoByAgentPath(
			lookupPath, string(owned),
		); !ok {
			continue
		}
		rowFound = true
		project, dataVersion, _, _, active :=
			e.db.GetSourceRepairStateByAgentPath(
				lookupPath, string(owned),
			)
		if !active {
			continue
		}
		if parser.NeedsProjectReparse(project) {
			return false
		}
		if dataVersion < db.CurrentDataVersion() {
			return false
		}
	}
	return rowFound
}

func (e *Engine) providerSourceFreshBeforeFingerprint(
	ctx context.Context,
	source parser.SourceRef,
	file parser.DiscoveredFile,
	preParseStatHash *pendingProviderStatHash,
) (int64, bool) {
	if e.forceParseRequested(file) {
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
	// Per-component digest pre-check for multi-file providers
	// (Codebuff/Freebuff). When the provider implements
	// MultiFileStatHasher and the side-table holds a digest that
	// matches the current stat snapshot, the source is fresh and we
	// short-circuit provider.Fingerprint. The hash is computed from
	// the physical on-disk chat path while the cache lookup uses the
	// pathRewriter's logical key, mirroring the write path; without
	// that split a remote-synced source whose stored file_path is
	// "host:/remote/path" would hash zero-stat tuples for every
	// companion and miss real content drift on the materialized
	// download. A stored digest that does not match the current
	// stat snapshot forces provider.Fingerprint instead of falling
	// through to the size/mtime composite below, because that
	// composite can miss same-size sibling rewrites whose mtime
	// stays below the existing max (or offsetting size deltas that
	// cancel out in sum-of-sizes). The side-table is populated
	// only after an outcome the engine can trust: a successful
	// write's flushPending, a single-session writeSessionFull
	// commit, or a confirmed-unchanged skip in processProviderFile
	// (stampProviderStatHashForConfirmedSource at the Claude
	// content-verified skip, the hash-validated cache skip, the
	// Codex-family DB-fingerprint skip, and
	// providerSourceUnchangedInDB — each runs only after the
	// current source content was verified against an existing
	// current session row). Nothing here ever persists
	// the digest before fingerprinting, parsing, or session
	// writing succeeds — a transient failure must not leave a
	// matching digest that suppresses every later retry.
	if preParseStatHash != nil {
		// Zero is the hasher's unverified sentinel. Do not consult the
		// side-table: even a corrupt or legacy zero row must not turn an
		// unavailable change-time into trusted freshness.
		if preParseStatHash.digest == 0 {
			return 0, false
		}
		stored, hasStored, hashErr :=
			e.db.GetProviderStatHash(ctx, file.Agent, lookupPath)
		switch {
		case hashErr != nil:
			// A read error must be handled before !hasStored: the DB
			// layer reports failures as (0, false, err), so a naive
			// !hasStored-first ordering would swallow the error into
			// the cold-start arm and could persist a digest that never
			// should have been written. On read error force a real
			// re-verification rather than falling through to the lossy
			// size/mtime composite: a persistent read error that
			// started this turn should not silently skip a stale
			// source indefinitely.
			log.Printf(
				"provider_freshness read for %s/%s: %v",
				file.Agent, lookupPath, hashErr)
			return 0, false
		case !hasStored:
			// Cold-start (no side-table row yet) or post-tombstone
			// (provider_freshness was cleared): force a real
			// fingerprint so a content-unchanged source still flows
			// through provider.Fingerprint → engine skip → no write →
			// no flushPending → no recordProviderStatHash => the
			// side-table row would never get re-populated on its own
			// and !hasStored would persist forever. Closing that loop
			// must NOT use a synchronous pre-parse digest write (the
			// old cold-stamp): a transient failure after the stamp
			// would leave a matching digest that suppresses every
			// later retry. Instead the loop is closed at the
			// confirmed-unchanged skips in processProviderFile
			// (Claude content-verified, hash-validated cache,
			// Codex-family DB-fingerprint, providerSourceUnchangedInDB),
			// each of which runs only after the current source content
			// was verified against an existing current session row —
			// the only pre-write moments the digest may safely be
			// persisted.
			// A genuinely new or changed source flows through a
			// successful write whose flushPending (or writeSessionFull)
			// persists the digest; CWD-filtered sources stay absent
			// because their session write never commits
			// (TestSyncCodebuffCwdFilteredSourceDoesNotPersistStatHash).
			// The fingerprint call still runs so size/mtime/data-
			// version freshness checks proceed normally; the digest
			// just lives somewhere gated by a confirmed outcome.
			return 0, false
		case stored == preParseStatHash.digest:
			// Cold writes stamp fingerprint.MTimeNS as the max of
			// chat + sibling companions (see codebuffFingerprintSource);
			// the skip cache key/decision must align with that stamp
			// so a cold→warm cycle does not drift. Only Codebuff/Freebuff
			// have sibling companions today; other agents implementing
			// MultiFileStatHasher fall through to chat-only via the
			// helper.
			//
			// A matching digest proves only that the file stat is
			// unchanged; it cannot prove the stored session is still
			// current. Without row-existence, data-version, and
			// project-reparse checks an unchanged Codebuff source would
			// bypass parser migrations and project repairs indefinitely,
			// because this short-circuit never reaches fingerprinting or
			// parsing. Defer to the fingerprint path whenever the stored
			// row is missing, stale, or needs project reclassification.
			if !e.providerFreshDigestSourceCurrentInDB(
				file.Agent, lookupPath,
			) {
				return 0, false
			}
			return providerStatFreshnessMtime(file.Agent, lookupPath, info), true
		default:
			// Stored digest disagrees with current component stats.
			// Forcing provider.Fingerprint instead of falling through
			// to the size/mtime composite is the point of the per
			// component digest: a same-size companion rewrite whose
			// mtime stays below the existing max, an offsetting pair
			// of size changes that cancel out in sum-of-sizes, or a
			// missing companion that another leg replaces must not
			// short-circuit on the legacy composite.
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
	// Gemini is deliberately absent here. Its fingerprint hash folds in the
	// session's resolved project name, so a pre-fingerprint skip keyed only on
	// the session file's size and mtime would skip a session whose project
	// metadata changed while the transcript did not, leaving a stale project
	// on scheduled syncs. Gemini relies on the post-fingerprint DB hash check
	// instead (providerFingerprintHashRequiredForFreshness), which catches a
	// resolved-project change even when size and mtime are unchanged.
	case parser.AgentCopilot:
		mtime := copilotEffectiveMtime(path, info)
		effectiveInfo := fakeSnapshotInfo{
			fSize:  info.Size(),
			fMtime: mtime,
		}
		if e.shouldSkipByPath(path, effectiveInfo) {
			return mtime, true
		}
	case parser.AgentRooCode:
		// RooCode's fingerprint is composite (history_item.json plus
		// ui_messages.json) and content-hashes both files. The
		// stat-only composite below matches the stored Size/Mtime the
		// fingerprint stamps, so unchanged tasks skip without reading
		// transcript bytes, and a sibling-only transcript append
		// still changes the composite and falls through to the full
		// fingerprint.
		size, mtime := roocodeEffectiveStat(path, info)
		effectiveInfo := fakeSnapshotInfo{
			fSize:  size,
			fMtime: mtime,
		}
		if e.shouldSkipByPath(path, effectiveInfo) {
			return mtime, true
		}
	case parser.AgentKiloLegacy:
		// Kilo Legacy's fingerprint is composite (task_metadata.json
		// plus ui_messages.json and api_conversation_history.json).
		// The stat-only composite below matches the stored Size/Mtime
		// the fingerprint stamps, so unchanged tasks skip without
		// reading transcript bytes, and a sibling-only transcript
		// append still changes the composite and falls through to the
		// full fingerprint.
		size, mtime := kiloLegacyEffectiveStat(path, info)
		effectiveInfo := fakeSnapshotInfo{
			fSize:  size,
			fMtime: mtime,
		}
		if e.shouldSkipByPath(path, effectiveInfo) {
			return mtime, true
		}
	case parser.AgentCodebuff:
		// Codebuff's fingerprint is composite (chat-messages.json
		// plus run-state.json and chat-meta.json). The stat-only
		// composite below matches the stored Size/Mtime the fingerprint
		// stamps, so unchanged sessions skip without reading transcript
		// bytes, and a sibling-only change still changes the composite
		// and falls through to the full fingerprint.
		//
		// Note: whenever a MultiFileStatHasher is registered for this
		// agent — always, in the production configuration — the digest
		// block above returns before this switch: its !hasStored arm
		// forces provider.Fingerprint on cold start, and the loop then
		// closes through providerSourceUnchangedInDB →
		// stampProviderStatHashForConfirmedSource (or a successful
		// write's flushPending). This arm is therefore reachable only
		// for a provider that declares Source.MultiFileStatHash=
		// Supported while its constructed instance fails the
		// MultiFileStatHasher assertion in buildProviderStatHashers.
		// It is kept as a defensive fallback for that
		// hasher-failed-to-register case, not as a production path.
		dir := filepath.Dir(path)
		size := info.Size()
		mtime := info.ModTime().UnixNano()
		for _, name := range []string{"run-state.json", "chat-meta.json"} {
			companion := filepath.Join(dir, name)
			if ci, err := os.Stat(companion); err == nil {
				size += ci.Size()
				if ts := ci.ModTime().UnixNano(); ts > mtime {
					mtime = ts
				}
			}
		}
		effectiveInfo := fakeSnapshotInfo{
			fSize:  size,
			fMtime: mtime,
		}
		if e.shouldSkipByPath(path, effectiveInfo) {
			return mtime, true
		}
	}
	return 0, false
}

// stampProviderFileIdentity fills a missing source inode/device on parsed
// results for an incremental-append provider. A provider may have captured an
// authoritative identity from the same descriptor it parsed, so a later path
// stat must not overwrite that snapshot after an atomic replacement. The
// legacy Claude process arm relies on this fallback because Claude does not
// supply descriptor identity itself. Providers whose source is not a single
// physical file, or that do not support incremental append, are left untouched.
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
	inode, device := getFileIdentity(path, info)
	for i := range results {
		if results[i].Session.File.Inode != 0 ||
			results[i].Session.File.Device != 0 {
			continue
		}
		results[i].Session.File.Inode = inode
		results[i].Session.File.Device = device
	}
}

// tryProviderIncrementalAppend reproduces the legacy incremental-append
// sync path for a provider-authoritative agent that supports append-only
// incremental parsing (Claude or Codex). The provider owns the byte-offset parse
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
	// Match the shared tryIncrementalJSONL gate: parse-diff and an explicit
	// full import both require a complete replacement rather than an append.
	// A per-file ForceParse keeps Claude on its incremental path; Codex is the
	// explicit exception below because a single-session refresh must rebuild
	// head-derived metadata.
	if e.forceParse || e.forceFullParse || file.ForceFullParse {
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
	// Codex-format incremental parsing intentionally preserves head-derived
	// metadata. A manual refresh, title change, or stale project needs the
	// authoritative full parse, and forceReplace prevents the later DB skip
	// gates from swallowing that refresh. Only Codex itself has a
	// session_index.jsonl title, so that check stays keyed to it: a fork would
	// pay a DB lookup that can never report a change.
	providerAgent := provider.Definition().Type
	if isCodexFormatAgent(providerAgent) &&
		(file.ForceParse ||
			e.pathNeedsProjectReparse(providerAgent, path) ||
			(providerAgent == parser.AgentCodex &&
				e.codexIndexSessionNameChanged(path))) {
		return processResult{forceReplace: true}, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return processResult{}, false
	}

	parseFn := func(
		_ string, inc *db.IncrementalInfo,
	) ([]parser.ParsedMessage, []parser.ClaudeSubagentLink, time.Time, int64, *string, error) {
		// The Claude parser needs the stored tail's provider message id
		// so its queued-command masking fallback fires only for a real
		// same-message.id continuation; without it, every routine queued
		// command followed by a fresh response would force a full parse.
		var storedLastClaudeMessageID *string
		if provider.Definition().Type == parser.AgentClaude {
			id := e.db.LastClaudeMessageID(inc.ID)
			storedLastClaudeMessageID = &id
		}
		outcome, status, perr := provider.ParseIncremental(
			ctx,
			parser.IncrementalRequest{
				Source:                    source,
				Fingerprint:               fingerprint,
				SessionID:                 inc.ID,
				Offset:                    inc.FileSize,
				StartOrdinal:              inc.NextOrdinal,
				Machine:                   inc.Machine,
				LastEntryUUID:             inc.LastEntryUUID,
				StoredAgentLabel:          inc.AgentLabel,
				StoredEntrypoint:          inc.Entrypoint,
				StoredSessionKind:         inc.SessionKind,
				StoredClaudeLinearParse:   inc.ClaudeLinearParse,
				StoredLastClaudeMessageID: storedLastClaudeMessageID,
			},
		)
		if perr != nil {
			return nil, nil, time.Time{}, 0, nil, perr
		}
		switch status {
		case parser.IncrementalNeedsFullParse:
			if outcome.ForceReplace {
				// Signal the shared helper to fall back to a
				// full parse that replaces stored messages.
				return nil, nil, time.Time{}, 0, nil,
					parser.ErrIncrementalNeedsFullParse
			}
			// A plain full-parse fallback without a replace request.
			// The Claude provider always sets ForceReplace on its
			// fallbacks (a DAG fork can drop or re-branch stored
			// rows), so this branch serves providers that only need
			// an append-preserving full parse.
			return nil, nil, time.Time{}, 0, nil, parser.ErrDAGDetected
		case parser.IncrementalNoNewData:
			return nil, nil, time.Time{}, 0, nil, nil
		default:
			var terminationStatus *string
			if outcome.TerminationStatus != nil {
				status := string(*outcome.TerminationStatus)
				terminationStatus = &status
			}
			return outcome.Messages, outcome.SubagentLinks,
				outcome.EndedAt, outcome.ConsumedBytes, terminationStatus, nil
		}
	}

	return e.tryIncrementalJSONL(ctx, file, info, file.Agent, parseFn)
}

// incrementalParseFunc reads new JSONL lines from a file
// starting at the given byte offset with the given starting
// ordinal and persisted session ID. Returns parsed messages, the latest
// timestamp (endedAt), bytes consumed (relative to offset), an optional
// authoritative termination status, and any error. The consumed count covers
// only complete, valid JSON lines so it can be used as a safe resume offset.
type incrementalParseFunc func(
	path string, inc *db.IncrementalInfo,
) ([]parser.ParsedMessage, []parser.ClaudeSubagentLink, time.Time, int64, *string, error)

// tryIncrementalJSONL attempts an incremental parse of an
// append-only JSONL file by reading only bytes appended since
// the last sync. Returns (result, true) on success, or
// (zero, false) to fall through to a full parse. Falls back
// to full parse when the file maps to multiple DB sessions
// (e.g. Claude DAG forks).
func (e *Engine) tryIncrementalJSONL(
	ctx context.Context,
	file parser.DiscoveredFile,
	info os.FileInfo,
	agent parser.AgentType,
	parseFn incrementalParseFunc,
) (processResult, bool) {
	if e.forceParse || e.forceFullParse || file.ForceFullParse {
		// Parse-diff and explicit full imports never produce append deltas.
		return processResult{}, false
	}
	lookupPath := file.Path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(file.Path)
	}
	inc, ok := e.db.GetSessionForIncremental(lookupPath, string(agent))
	if !ok || inc.FileSize <= 0 {
		return processResult{}, false
	}

	// A session archived before the cwd allow-list was configured
	// must not keep growing through the append path, which bypasses
	// the prepareSessionWrite veto. Fall back to the full parse path
	// so the same veto applies; it also re-derives the cwd from the
	// whole file, which covers a stored cwd that predates cwd capture.
	if !e.cwdFilter.allows(inc.Cwd) {
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
	// filesystem does not expose a stable identity.
	if e.pathRewriter == nil && inc.FileInode != 0 && inc.FileDevice != 0 {
		curInode, curDevice := getFileIdentity(file.Path, info)
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
		if e.pathRewriter != nil {
			log.Printf(
				"incremental %s remote source: file truncated from %d to %d, full parse",
				agent, inc.FileSize, currentSize,
			)
		} else {
			log.Printf(
				"incremental %s %s: file truncated from %d to %d, full parse",
				agent, file.Path, inc.FileSize, currentSize,
			)
		}
		return processResult{forceReplace: true}, false
	}
	if currentSize == inc.FileSize {
		if isCodexFormatAgent(agent) {
			// A Codex-format rollout with no new transcript bytes can still
			// reach the fingerprint path. Codex's composite mtime may also
			// change when session_index.jsonl does. Let the later database
			// freshness checks decide whether to skip or full-parse.
			return processResult{}, false
		}
		log.Printf(
			"incremental %s %s: file size unchanged at %d but changed since last sync, full parse",
			agent, file.Path, currentSize,
		)
		return processResult{forceReplace: true}, false
	}

	// Persist the same effective file_mtime a full parse would store. Codex
	// folds in session_index.jsonl (parser.CodexEffectiveMtime),
	// exactly as ParseCodexSession sets File.Mtime; a full sync of the same
	// file stores that effective value. Keeping the incremental write on the
	// same basis means parse-diff's raced guard -- which reads the freshly
	// parsed effective File.Mtime -- compares against a matching stored
	// file_mtime no matter whether the last write was incremental or full,
	// and shouldSkipCodex's storedMtime==effectiveMtime fast path stays
	// accurate. Other JSONL agents, including TraeX, keep the raw stat.
	incMtime := info.ModTime().UnixNano()
	if agent == parser.AgentCodex {
		incMtime = parser.CodexEffectiveMtime(file.Path, incMtime)
	}

	// Incremental parse seam: every gate above returns a lease-free decline.
	// From here parseFn reads and parses the appended bytes, so acquire the
	// retention lease that bounds the parsed payload. It is attached to the
	// incremental results below and released on every decline (fall-through to
	// a full parse re-acquires at the provider parse seam) or skip return.
	lease, leaseErr := e.retentionBudget().acquire(
		ctx, parseRetentionSourceBytes(file),
	)
	if leaseErr != nil {
		return processResult{err: leaseErr}, true
	}

	newMsgs, links, endedAt, consumed, terminationStatus, err := parseFn(
		file.Path, inc,
	)
	if err != nil {
		lease.Release()
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
	// Refresh the stored content fingerprint on the incremental path. Codex
	// needs it for parse-diff's raced-skew detection; Claude needs it so
	// providerSingleSessionFresh can compare the stored hash against the
	// on-disk bytes and catch a same-size, same-mtime, same-inode in-place
	// rewrite that the size/mtime/identity skip signals cannot see.
	if isCodexFormatAgent(agent) || agent == parser.AgentClaude {
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
					project:              inc.Project,
					sourceProject:        inc.SourceProject,
					machine:              inc.Machine,
					cwd:                  inc.Cwd,
					links:                links,
					endedAt:              endedAt,
					terminationStatus:    terminationStatus,
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
				retentionLease: lease,
			}, true
		}
		// A larger source with no complete record consumed is an unfinished
		// append, not evidence that this fingerprint is fully processed. Keep
		// the persisted cursor unchanged and suppress the mtime skip entry so a
		// completed record is retried even when the writer restores the same
		// filesystem timestamp.
		lease.Release()
		return processResult{skip: true, noCacheSkip: true}, true
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
				lease.Release()
				return processResult{forceReplace: true}, false
			}
		}
	}

	// Claude-only: an empty stored preview means no real user prompt
	// has been parsed yet (a session that starts with injected IDE
	// context, a continuation record, /clear, or /effort). When this
	// chunk carries the first real prompt, fall back to a full parse
	// so first_message is re-derived from the whole file. Chunks
	// without one — streamed assistant work after an auto-compact
	// continuation, or more injected IDE context — stay incremental
	// so per-event work is bounded by the appended bytes rather than
	// the transcript size.
	//
	// Other agents can legitimately have an empty first_message
	// alongside real user rows — for example Codex inserts orphan
	// subagent notifications as Role=user messages that bypass
	// firstMessage — so this fall-through is gated on Claude.
	if agent == parser.AgentClaude && inc.FirstMessage == "" &&
		chunkHasRealUserPrompt(newMsgs) {
		log.Printf(
			"incremental %s %s: first real user prompt after "+
				"empty preview, full parse",
			agent, file.Path,
		)
		lease.Release()
		return processResult{}, false
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
			project:              inc.Project,
			sourceProject:        inc.SourceProject,
			machine:              inc.Machine,
			cwd:                  inc.Cwd,
			msgs:                 newMsgs,
			links:                links,
			endedAt:              endedAt,
			terminationStatus:    terminationStatus,
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
		retentionLease: lease,
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
// their existing in-memory skip-cache behavior unchanged. TraeX shares the
// transcript size/hash/mtime gate but never reaches the Codex-only index-title
// branch. Every lookup is agent-scoped so overlapping roots or a root
// reassigned between Codex and TraeX cannot borrow freshness.
func (e *Engine) shouldSkipProviderSourceByDB(
	file parser.DiscoveredFile,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) bool {
	fresh, _ := e.providerSourceFreshnessByDB(file, fingerprint, semantics)
	return fresh
}

// providerSourceFreshnessByDB returns the Codex-family DB freshness decision
// and whether all metadata needed to persist local stat trust was verified.
// A transient Codex title-index failure may still skip unchanged transcript
// content, but it cannot promote in-memory trust or stamp a stat digest.
func (e *Engine) providerSourceFreshnessByDB(
	file parser.DiscoveredFile,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) (fresh, metadataVerified bool) {
	if !isCodexFormatAgent(file.Agent) {
		return false, false
	}
	return e.codexFingerprintFreshness(
		file.Agent, file.Path, fingerprint, semantics,
	)
}

// shouldSkipCodexFingerprint reproduces the legacy shouldSkipCodex decision in
// terms of a provider SourceFingerprint. For Codex, fingerprint MTimeNS folds
// in session_index.jsonl via CodexEffectiveMtime; TraeX uses the transcript
// mtime only. Therefore:
//   - a stored size/hash mismatch or stale data version forces a reparse;
//   - an exact effective-mtime match skips;
//   - an effective mtime ahead of the stored mtime driven only by the index
//     (the raw transcript mtime is still at or below the stored mtime) skips
//     unless this session's stored title differs from the current index title.
//   - an effective mtime below the stored mtime skips when the index is absent,
//     because removing a newer index reveals the unchanged transcript mtime.
func (e *Engine) shouldSkipCodexFingerprint(
	agent parser.AgentType,
	path string,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) bool {
	fresh, _ := e.codexFingerprintFreshness(
		agent, path, fingerprint, semantics,
	)
	return fresh
}

func (e *Engine) codexFingerprintFreshness(
	agent parser.AgentType,
	path string,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) (fresh, metadataVerified bool) {
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByAgentPath(
		lookupPath, string(agent),
	)
	if !ok || storedSize != fingerprint.Size {
		return false, false
	}
	if !e.providerFingerprintHashMatchesDB(
		agent, lookupPath,
		fingerprint,
		semantics.FingerprintHashRequiredForFreshness,
	) {
		return false, false
	}
	if project, ok := e.db.GetProjectByAgentPath(
		lookupPath, string(agent),
	); ok &&
		parser.NeedsProjectReparse(project) {
		return false, false
	}
	if e.db.GetDataVersionByAgentPath(lookupPath, string(agent)) <
		db.CurrentDataVersion() {
		return false, false
	}
	effectiveMtime := fingerprint.MTimeNS
	if agent != parser.AgentCodex {
		return storedMtime == effectiveMtime, true
	}
	statFresh := storedMtime == effectiveMtime
	if effectiveMtime < storedMtime {
		indexPath := parser.CodexSessionIndexPath(path)
		if indexPath != "" {
			if _, err := os.Stat(indexPath); errors.Is(err, os.ErrNotExist) {
				statFresh = true
			}
		}
	}
	if effectiveMtime > storedMtime {
		fileMtime := effectiveMtime
		if info, err := os.Stat(path); err == nil {
			fileMtime = info.ModTime().UnixNano()
		}
		statFresh = fileMtime <= storedMtime
	}
	if !statFresh {
		return false, false
	}
	changed, verified := e.codexIndexSessionNameState(path)
	if !verified {
		return true, false
	}
	return !changed, true
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
	changed, _ := e.codexIndexSessionNameState(path)
	return changed
}

func (e *Engine) codexIndexSessionNameState(
	path string,
) (changed, verified bool) {
	uuid := parser.CodexSessionUUIDFromFilename(filepath.Base(path))
	if uuid == "" {
		return false, false
	}
	currentName, ok, err := parser.ReadCodexThreadNameEntry(path, uuid)
	if err != nil {
		return false, false
	}
	if !ok {
		// No index entry means no rename signal, not a rename to empty.
		// Modern Codex releases stopped writing session_index.jsonl; a
		// stored title compared against the absent index would force a
		// full re-parse of every titled session on every sync, and the
		// rewrite preserves the title, so the loop could never converge.
		return false, true
	}
	storedName, found, err := e.db.GetSessionName(
		context.Background(), e.idPrefix+"codex:"+uuid,
	)
	if err != nil || !found {
		return true, true
	}
	return codexSessionNameDiffers(storedName, currentName), true
}

// codexCachedIndexSessionNameState limits title-based cache invalidation to
// sources that already have stored session state. A cached parse failure has
// no title to refresh and its missing row counts as verified for cache-only
// retry suppression; no digest can be stamped without a stored row hash.
func (e *Engine) codexCachedIndexSessionNameState(
	path string,
) (changed, verified bool) {
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	if _, _, ok := e.db.GetFileInfoByPath(lookupPath); !ok {
		return false, true
	}
	return e.codexIndexSessionNameState(path)
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
	parser.EvictCodexSessionIndex(path)
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
					Path:    src,
					Agent:   parser.AgentCodex,
					Machine: e.machineForPath(parser.AgentCodex, src),
				})
			}
		}
		if len(candidates) == 0 {
			continue
		}
		// A UUID can exist in both sessions/ and archived_sessions/.
		// Prefer the path the DB already tracks so a title rename does
		// not reparse a stale duplicate over the stored copy.
		chosen := e.pickPreferredCodexIndexDiscoveredFile(candidates)
		// Pin the provider source to the chosen path and route it through the
		// provider so processProviderFile parses exactly this copy instead of
		// re-canonicalizing the UUID to the preferred dated layout, which would
		// undo the DB-aware selection above.
		chosen.ProviderProcess = true
		chosen.ProviderSource = e.codexPinnedProviderSource(
			parser.AgentCodex, chosen.Path,
		)
		out = append(out, chosen)
	}
	return out
}

func (e *Engine) pickPreferredCodexIndexDiscoveredFile(
	candidates []parser.DiscoveredFile,
) parser.DiscoveredFile {
	if len(candidates) > 0 {
		uuid := ""
		for _, candidate := range candidates {
			uuid = parser.CodexSessionUUIDFromFilename(filepath.Base(candidate.Path))
			if uuid != "" {
				break
			}
		}
		storedPath := e.db.GetSessionFilePath(e.idPrefix + "codex:" + uuid)
		if uuid != "" && storedPath != "" {
			storedPath = filepath.Clean(storedPath)
			for _, candidate := range candidates {
				if filepath.Clean(e.effectiveSourcePath(candidate.Path)) == storedPath {
					return candidate
				}
			}
		}
	}
	return pickPreferredCodexDiscoveredFile(e.db, candidates)
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

// codexPinnedProviderSource builds a Codex-format provider SourceRef pinned to
// the exact path, bypassing the provider's live-over-archived canonicalization.
// It is used when the engine's DB-aware or mtime-aware logic has already chosen
// which on-disk copy of a duplicated UUID to parse, so processProviderFile
// parses that copy instead of the provider's preferred dated layout. The agent
// selects the provider so a fork's path is pinned under the fork's own roots.
// Returns nil when that provider or the path's source shape is unavailable.
func (e *Engine) codexPinnedProviderSource(
	agent parser.AgentType, path string,
) *parser.SourceRef {
	factory, ok := e.providerFactories[agent]
	if !ok || factory == nil {
		return nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:   e.agentDirs[agent],
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
	storedName, found, err := e.db.GetSessionName(
		context.Background(), sessionID,
	)
	if err != nil || !found {
		return missingDiffers
	}
	return codexSessionNameDiffers(storedName, indexTitle)
}

func codexSessionNameDiffers(storedName, indexTitle string) bool {
	return strings.TrimSpace(indexTitle) != strings.TrimSpace(storedName)
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
// roocodeEffectiveStat returns the composite size and latest mtime of
// a RooCode task's history_item.json and its ui_messages.json sibling
// using stat calls only. The values mirror what
// rooCodeFingerprintSource stamps on stored sessions (summed size,
// max mtime), so a stat-only comparison against the stored row is
// sufficient to detect any change to either file.
func roocodeEffectiveStat(historyPath string, info os.FileInfo) (int64, int64) {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	msgPath := filepath.Join(filepath.Dir(historyPath), "ui_messages.json")
	if msgInfo, err := os.Stat(msgPath); err == nil && !msgInfo.IsDir() {
		size += msgInfo.Size()
		if ts := msgInfo.ModTime().UnixNano(); ts > mtime {
			mtime = ts
		}
	}
	return size, mtime
}

// kiloLegacyEffectiveStat returns the composite size and latest mtime of
// a Kilo Legacy task's task_metadata.json, ui_messages.json, and
// api_conversation_history.json using stat calls only. The values mirror
// what kiloLegacyFingerprintSource stamps on stored sessions (summed
// size, max mtime), so a stat-only comparison against the stored row is
// sufficient to detect any change to any of the three files.
func kiloLegacyEffectiveStat(metadataPath string, info os.FileInfo) (int64, int64) {
	size := info.Size()
	mtime := info.ModTime().UnixNano()
	dir := filepath.Dir(metadataPath)
	for _, name := range []string{
		"ui_messages.json",
		"api_conversation_history.json",
	} {
		sibPath := filepath.Join(dir, name)
		sibInfo, err := os.Stat(sibPath)
		if err != nil || sibInfo.IsDir() {
			continue
		}
		size += sibInfo.Size()
		if ts := sibInfo.ModTime().UnixNano(); ts > mtime {
			mtime = ts
		}
	}
	return size, mtime
}

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
	_, err := e.recomputeSignalsFromDB(ctx, sessionID)
	return err
}

// BackfillSignalComputer returns a signal recompute closure for archive
// backfills that releases transient heap after enough loaded content has
// crossed the threshold.
func (e *Engine) BackfillSignalComputer() func(context.Context, string) error {
	var release recomputeHeapReleaser
	return func(ctx context.Context, sessionID string) error {
		if e.refuseWriteInForceParse("BackfillSignalComputer") {
			return errors.New(
				"BackfillSignalComputer refused on report-only parse-diff engine",
			)
		}
		heapBytes, err := e.recomputeSignalsFromDB(ctx, sessionID)
		if err != nil {
			return err
		}
		release.Account(heapBytes)
		return nil
	}
}

// BackfillProjectIdentitySnapshots reconstructs immutable export evidence from
// stored session metadata. Candidate selection and progress are durable, while
// filesystem and Git discovery happen here so database startup remains cheap.
func (e *Engine) BackfillProjectIdentitySnapshots(ctx context.Context) error {
	if e.refuseWriteInForceParse("BackfillProjectIdentitySnapshots") {
		return errors.New(
			"BackfillProjectIdentitySnapshots refused on report-only parse-diff engine",
		)
	}
	if err := e.db.EnsureProjectIdentityBackfillQueued(ctx); err != nil {
		return err
	}
	status, err := e.db.ProjectIdentityBackfillStatus(ctx)
	if err != nil {
		return err
	}
	if status.State == "not_needed" || status.State == "completed" {
		return nil
	}
	if err := e.db.StartProjectIdentityBackfill(ctx); err != nil {
		return err
	}
	log.Printf("project identity backfill: processing %d sessions", status.TotalItems)

	afterID := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		candidates, err := e.db.ProjectIdentityBackfillCandidatesAfter(ctx, afterID)
		if err != nil {
			return e.failProjectIdentityBackfill(ctx, err)
		}
		if len(candidates) == 0 {
			if err := e.db.CompleteProjectIdentityBackfill(ctx); err != nil {
				return err
			}
			log.Printf("project identity backfill: completed %d sessions",
				status.TotalItems)
			return nil
		}
		observations := make([]export.ProjectIdentityObservation, 0, len(candidates))
		for _, session := range candidates {
			if err := ctx.Err(); err != nil {
				return err
			}
			observations = append(observations,
				e.projectIdentityObservationForBackfill(session))
		}
		if err := e.db.ApplyProjectIdentityBackfillBatch(ctx, observations); err != nil {
			return e.failProjectIdentityBackfill(ctx, err)
		}
		afterID = candidates[len(candidates)-1].ID
	}
}

func (e *Engine) projectIdentityObservationForBackfill(
	session db.Session,
) export.ProjectIdentityObservation {
	obs, ok := e.projectIdentityObservation(session)
	if !ok {
		obs = export.ProjectIdentityObservation{
			SessionID:  session.ID,
			Project:    strings.TrimSpace(session.Project),
			Machine:    strings.TrimSpace(session.Machine),
			RootPath:   strings.TrimSpace(session.Cwd),
			GitBranch:  strings.TrimSpace(session.GitBranch),
			ObservedAt: time.Now().UTC(),
		}
		if obs.GitBranch != "" {
			obs.CheckoutState = export.CheckoutBranch
		}
	}
	return obs
}

func (e *Engine) failProjectIdentityBackfill(
	ctx context.Context, cause error,
) error {
	if err := e.db.FailProjectIdentityBackfill(ctx, cause); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// recomputeSignalsFromDB loads a session's full message history
// and stored metadata, runs the pure in-memory signal compute
// over them, and persists the result. Used when callers don't
// already have the message slice in memory (legacy backfill,
// incremental writes).
func (e *Engine) recomputeSignalsFromDB(
	ctx context.Context, sessionID string,
) (int, error) {
	if e.disableSignalRecompute {
		return 0, nil
	}
	sess, err := e.db.GetSessionFull(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf(
			"loading session %s: %w", sessionID, err,
		)
	}
	if sess == nil {
		return 0, nil
	}
	msgs, err := e.db.GetAllMessages(ctx, sessionID)
	if err != nil {
		log.Printf(
			"signals: load messages %s: %v",
			sessionID, err,
		)
		return 0, fmt.Errorf(
			"loading messages %s: %w", sessionID, err,
		)
	}
	update, findings := computeSignalsAndSecrets(*sess, msgs)
	heapBytes := recomputeHeapBytes(msgs, findings)
	// Findings persist before the signals update: UpdateSessionSignals
	// advances quality_signal_version, which BackfillSignals treats as
	// proof the whole compute persisted. Writing it last keeps a
	// session whose findings write failed below the current version,
	// so the next backfill retries it.
	if err := e.db.ReplaceSessionSecretFindings(
		sessionID, findings, update.SecretLeakCount, update.SecretsRulesVersion,
	); err != nil {
		log.Printf("secrets: persist %s: %v", sessionID, err)
		return 0, fmt.Errorf("persisting findings %s: %w", sessionID, err)
	}
	if err := e.db.UpdateSessionSignals(
		sessionID, update,
	); err != nil {
		log.Printf(
			"signals: update %s: %v", sessionID, err,
		)
		return 0, fmt.Errorf(
			"updating signals %s: %w", sessionID, err,
		)
	}
	return heapBytes, nil
}

type pendingWrite struct {
	sess         parser.ParsedSession
	msgs         []parser.ParsedMessage
	usageEvents  []parser.ParsedUsageEvent
	needsRetry   bool
	forceReplace bool
	// sourceIdentityUnverified marks a copy that shares a native session ID
	// without matching the stored machine, source path, or content hash. The
	// copy still follows native-ID deduplication, but it cannot borrow the
	// stored attribution for local filesystem identity discovery.
	sourceIdentityUnverified bool
	// sourceProjectResolved marks writes whose parser project has already
	// been reconciled with durable identity for an unavailable local cwd.
	sourceProjectResolved bool
	// baselineEligible is set by collectAndBatch only when the complete source
	// outcome is safe to make deletion-eligible after this write succeeds.
	baselineEligible bool
	// providerStatHash is set on the first allowed ParseResult of a
	// source whose processResult staged a per-component freshness digest.
	// sourceWriteCount tells the flush how many contiguous results must commit
	// before it may persist the digest. Claude uses the full DAG result count;
	// other digest-backed providers currently use one.
	providerStatHash         *pendingProviderStatHash
	sourceWriteCount         int
	sourceCompletionEligible bool
	promoteSourceOnComplete  bool
	// sourceCompletionSkipped marks a user-excluded or trashed member that
	// resolves source completeness without requiring a write or promotion.
	sourceCompletionSkipped bool
	// storageTrustPath/State/Snap promote the session's OpenCode
	// storage-gate trust after its batch is confirmed fully written.
	// Empty for everything else.
	storageTrustPath    string
	storageTrustState   string
	storageTrustSnap    storageTrustSnapshot
	sourceCwdResolution parser.SourceCwdResolution
	sourceCwdStored     string
	sourceCwdStoredOK   bool
}

type sessionWriteIdentityReader interface {
	ListSessionWriteIdentitiesByID(
		context.Context, []string,
	) (map[string]db.SessionWriteIdentity, error)
}

func (e *Engine) pendingWriteIdentity(pw pendingWrite) db.SessionWriteIdentity {
	return db.SessionWriteIdentity{
		Machine:  pw.sess.Machine,
		Agent:    string(pw.sess.Agent),
		FilePath: e.effectiveSourcePath(pw.sess.File.Path),
		FileHash: pw.sess.File.Hash,
	}
}

func sessionWriteIdentitySupportsStoredAttribution(
	left, right db.SessionWriteIdentity,
) bool {
	if left.Agent != right.Agent {
		return false
	}
	if left.Machine == right.Machine {
		return true
	}
	if left.FilePath != "" && right.FilePath != "" &&
		sameReconciliationSourcePath(left.FilePath, right.FilePath) {
		return true
	}
	return left.FileHash != "" && right.FileHash != "" &&
		left.FileHash == right.FileHash
}

// normalizePendingWriteMachines resolves immutable attribution before any
// consumer can make a project, worktree, baseline, or persistence decision.
// Existing sessions keep the archive's machine. Native IDs continue to
// deduplicate across copied roots, while unverified copies cannot borrow the
// stored attribution for local filesystem identity discovery.
func (e *Engine) normalizePendingWriteMachines(
	ctx context.Context,
	batch []pendingWrite,
) ([]pendingWrite, error) {
	indexes := make(map[string][]int, len(batch))
	ids := make([]string, 0, len(batch))
	for i := range batch {
		if batch[i].sess.ID == "" {
			continue
		}
		id := applyIDPrefixToID(e.idPrefix, batch[i].sess.ID)
		if _, exists := indexes[id]; !exists {
			ids = append(ids, id)
		}
		indexes[id] = append(indexes[id], i)
	}
	if len(ids) == 0 {
		return batch, nil
	}

	var archive any = e.db
	if e.archiveStore != nil {
		archive = e.archiveStore
	}
	reader, ok := archive.(sessionWriteIdentityReader)
	if !ok {
		return batch, fmt.Errorf(
			"archive %T does not support session write identity lookup", archive,
		)
	}
	identities, err := reader.ListSessionWriteIdentitiesByID(ctx, ids)
	if err != nil {
		return batch, fmt.Errorf("load immutable session write identities: %w", err)
	}
	// A rebuild reads preexisting attribution from archiveStore while writing
	// into e.db. An ID first seen earlier in this rebuild is absent from the old
	// archive, so consult the replacement before treating it as a new ingestion.
	if e.archiveStore != nil {
		unresolved := make([]string, 0, len(ids))
		for _, id := range ids {
			if _, exists := identities[id]; !exists {
				unresolved = append(unresolved, id)
			}
		}
		if len(unresolved) > 0 {
			replacementIdentities, err := e.db.ListSessionWriteIdentitiesByID(
				ctx, unresolved,
			)
			if err != nil {
				return batch, fmt.Errorf(
					"load replacement session write identities: %w", err,
				)
			}
			maps.Copy(identities, replacementIdentities)
		}
	}
	for _, id := range ids {
		matchingIndexes := indexes[id]
		if len(matchingIndexes) == 0 {
			continue
		}
		stored, exists := identities[id]
		if exists {
			for _, i := range matchingIndexes {
				incoming := e.pendingWriteIdentity(batch[i])
				batch[i].sourceIdentityUnverified =
					!sessionWriteIdentitySupportsStoredAttribution(stored, incoming)
				batch[i].sess.Machine = stored.Machine
				// A rebuild writes into a fresh database, so the upsert cannot
				// preserve a title from the destination row. Carry the archived
				// nullable title into index-less Codex parses; an explicitly
				// present blank remains authoritative and bypasses this path.
				if e.archiveStore != nil &&
					batch[i].sess.Agent == parser.AgentCodex &&
					!batch[i].sess.SessionNamePresent {
					batch[i].sess.SessionName = ""
					if stored.SessionName != nil {
						batch[i].sess.SessionName = *stored.SessionName
					}
				}
			}
			continue
		}

		first := e.pendingWriteIdentity(batch[matchingIndexes[0]])
		for _, i := range matchingIndexes {
			batch[i].sourceIdentityUnverified =
				!sessionWriteIdentitySupportsStoredAttribution(
					first, e.pendingWriteIdentity(batch[i]),
				)
			batch[i].sess.Machine = first.Machine
		}
	}
	return batch, nil
}

type writeBatchOutcome struct {
	writtenSessions int
	writtenMessages int
	failedSessions  int
	cwdFiltered     int
	written         []bool
	// resolved includes actual writes plus user-excluded or trashed sessions.
	// It stays separate from written so stats and baselines count real writes.
	resolved []bool
}

type skipCacheWrite struct {
	agent             parser.AgentType
	key               string
	mtime             int64
	sourceFingerprint string
}

func (e *Engine) promoteSkipCacheWrites(writes []skipCacheWrite) {
	for _, write := range writes {
		e.cacheSkip(write.key, write.mtime, write.sourceFingerprint)
	}
}

func (e *Engine) rejectSkipCacheWrites(writes []skipCacheWrite) {
	for _, write := range writes {
		e.clearSkip(write.key)
	}
}

// markStaleFailedMemberWrite demotes the stored data version of an omnigent
// session whose write failed. Shared-container members have no per-file mtime
// to invalidate, so without the demotion a partial write (session row updated,
// messages not) would compare as unchanged and never be repaired.
func (e *Engine) markStaleFailedMemberWrite(pw pendingWrite) {
	if pw.sess.Agent != parser.AgentOmnigent || pw.sess.ID == "" {
		return
	}
	// Sessions are stored under the remote-sync prefixed ID
	// (applyRemoteRewrites in prepareSessionWrite), so the demotion must
	// target the same row.
	id := applyIDPrefixToID(e.idPrefix, pw.sess.ID)
	staleVersion := max(db.CurrentDataVersion()-1, 0)
	if e.db.GetSessionDataVersion(id) > staleVersion {
		if err := e.db.SetSessionDataVersion(id, staleVersion); err != nil {
			log.Printf("mark failed member write stale for %s: %v", id, err)
		}
	}
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
	return e.loadWorktreeProjectResolverContext(context.Background())
}

func (e *Engine) loadWorktreeProjectResolverContext(
	ctx context.Context,
) worktreeProjectResolver {
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
				ctx, machine,
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

// skipSourceProjectProbe reports whether pw's working directory must not be
// stat-ed to decide whether its source project is still available. Remote and
// unverified sessions describe another machine's paths, automounter namespaces
// wake automountd, and macOS TCC-protected locations raise a consent prompt.
// Skipping leaves the stored project untouched, which is what an unreachable
// working directory would produce anyway.
func (e *Engine) skipSourceProjectProbe(pw *pendingWrite) bool {
	if e.disableProjectDiscovery {
		return true
	}
	sess := &pw.sess
	cwd := sourceProjectPreservationCwd(*pw)
	if sess.ID == "" || cwd == "" || pw.sourceIdentityUnverified {
		return true
	}
	if !e.isLocalMachineAttribution(sess.Machine) ||
		!safeLocalAbsolutePath(cwd) {
		return true
	}
	if export.IsAutomountNamespacePath(runtime.GOOS, filepath.Clean(cwd)) {
		return true
	}
	return !e.mayProbeLocalPath(cwd)
}

func sourceProjectPreservationCwd(pw pendingWrite) string {
	if pw.sourceCwdResolution.State == parser.SourceCwdUnavailable &&
		pw.sourceCwdStoredOK && pw.sourceCwdStored != "" {
		return pw.sourceCwdStored
	}
	return pw.sess.Cwd
}

func (e *Engine) preserveUnavailableSourceProjects(
	ctx context.Context,
	batch []pendingWrite,
) ([]pendingWrite, error) {
	indexes := make(map[string][]int)
	ids := make([]string, 0, len(batch))
	for i := range batch {
		if batch[i].sourceProjectResolved {
			continue
		}
		sess := batch[i].sess
		preservationCwd := sourceProjectPreservationCwd(batch[i])
		if e.skipSourceProjectProbe(&batch[i]) {
			batch[i].sourceProjectResolved = true
			continue
		}
		stat := e.stat
		if stat == nil {
			stat = os.Stat
		}
		if batch[i].sourceCwdResolution.State != parser.SourceCwdUnavailable {
			if _, err := stat(preservationCwd); err == nil && sess.Project != "" {
				batch[i].sourceProjectResolved = true
				continue
			}
		}
		storedID := applyIDPrefixToID(e.idPrefix, sess.ID)
		if _, exists := indexes[storedID]; !exists {
			ids = append(ids, storedID)
		}
		indexes[storedID] = append(indexes[storedID], i)
	}
	if len(ids) == 0 {
		return batch, nil
	}

	snapshots, err := e.db.ListSessionProjectIdentitySnapshotsByID(
		ctx, ids,
	)
	if err != nil {
		return batch, fmt.Errorf(
			"load unavailable-cwd project identity snapshots: %w", err,
		)
	}
	for _, matchingIndexes := range indexes {
		for _, i := range matchingIndexes {
			batch[i].sourceProjectResolved = true
		}
	}
	for id, snapshot := range snapshots {
		if snapshot.Project == "" ||
			snapshot.RemoteResolution != export.ProjectResolutionResolved ||
			snapshot.GitRemote == "" {
			continue
		}
		for _, i := range indexes[id] {
			sess := &batch[i].sess
			if !e.sameLocalMachineAttribution(snapshot.Machine, sess.Machine) ||
				!e.snapshotRootContainsCwd(
					snapshot.RootPath, sourceProjectPreservationCwd(batch[i]),
				) {
				continue
			}
			sess.Project = snapshot.Project
		}
	}
	return batch, nil
}

// userHomeDirOrEmpty returns the current user's home directory, or "" when it
// cannot be resolved. An empty result leaves protected-path gating inactive,
// which keeps discovery working on systems with no usable home directory.
func userHomeDirOrEmpty() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// mayProbeLocalPath reports whether passive discovery may touch p on disk.
// Working directories inside macOS TCC-protected locations stay untouched
// unless the user set scan_protected_paths, so importing an archive cannot
// raise consent prompts for Documents, Downloads, or a cloud-provider folder.
// Automounter namespaces stay untouched regardless of the opt-in: consenting
// to consent prompts is not consenting to waking automountd. The check
// follows symlinks, so a path that merely links into either kind of location
// is refused before Stat or EvalSymlinks can enter it.
func (e *Engine) mayProbeLocalPath(p string) bool {
	switch export.ClassifyLocalPathProbe(
		e.goos, e.homeDir, p, e.scanProtectedPaths,
	) {
	case export.LocalPathProbeAutomountNamespace:
		return false
	case export.LocalPathProbeProtectedUserData:
		return e.scanProtectedPaths
	case export.LocalPathProbeSafe:
		return true
	}
	return true
}

// isLocalMachineAttribution recognizes empty and the legacy "local" sentinel
// as this machine without rewriting the immutable stored value.
func (e *Engine) isLocalMachineAttribution(machine string) bool {
	return machine == "" || machine == "local" || machine == e.machine
}

func (e *Engine) sameLocalMachineAttribution(left, right string) bool {
	return left == right ||
		(e.isLocalMachineAttribution(left) && e.isLocalMachineAttribution(right))
}

// snapshotRootContainsCwd reports whether the durable snapshot's root
// contains the session cwd. The cwd was vetted by skipSourceProjectProbe,
// but the snapshot root is stored data that can predate protected-path
// gating and name a guarded folder; resolving its symlinks would walk
// inside it, so a refused root falls back to lexical containment.
func (e *Engine) snapshotRootContainsCwd(root, cwd string) bool {
	if !e.mayProbeLocalPath(root) {
		return lexicalPathContains(root, cwd)
	}
	return pathContains(root, cwd)
}

func pathContains(root, path string) bool {
	root = resolveExistingPathPrefix(root)
	path = resolveExistingPathPrefix(path)
	return lexicalPathContains(root, path)
}

func lexicalPathContains(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." ||
		(rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// evalSymlinks is indirected through a var so tests can assert prefix
// resolution never walks a refused root. Production code always uses
// filepath.EvalSymlinks via this binding.
var evalSymlinks = filepath.EvalSymlinks

func resolveExistingPathPrefix(path string) string {
	cleaned := filepath.Clean(path)
	current := cleaned
	var missingTail []string
	for {
		if resolved, err := evalSymlinks(current); err == nil {
			for _, v := range slices.Backward(missingTail) {
				resolved = filepath.Join(resolved, v)
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		missingTail = append(missingTail, filepath.Base(current))
		current = parent
	}
}

func (e *Engine) writeBatch(
	batch []pendingWrite,
	writeMode syncWriteMode,
	forceReplace bool,
) (writtenSessions, writtenMessages, failedSessions, cwdFiltered int) {
	outcome := e.writeBatchWithOutcome(batch, writeMode, forceReplace)
	return outcome.writtenSessions, outcome.writtenMessages,
		outcome.failedSessions, outcome.cwdFiltered
}

func (e *Engine) writeBatchWithOutcome(
	batch []pendingWrite,
	writeMode syncWriteMode,
	forceReplace bool,
) writeBatchOutcome {
	return e.writeBatchWithOutcomeContext(
		context.Background(), batch, writeMode, forceReplace,
	)
}

func (e *Engine) writeBatchWithOutcomeContext(
	ctx context.Context,
	batch []pendingWrite,
	writeMode syncWriteMode,
	forceReplace bool,
) writeBatchOutcome {
	outcome := writeBatchOutcome{
		written:  make([]bool, len(batch)),
		resolved: make([]bool, len(batch)),
	}
	if ctx.Err() != nil {
		return outcome
	}
	var err error
	batch, err = e.normalizePendingWriteMachines(
		ctx, batch,
	)
	if err != nil {
		if ctx.Err() != nil {
			return outcome
		}
		log.Printf("normalize pending write machines: %v", err)
		outcome := writeBatchOutcome{
			written:  make([]bool, len(batch)),
			resolved: make([]bool, len(batch)),
		}
		for _, pw := range batch {
			e.markStaleFailedMemberWrite(pw)
		}
		outcome.failedSessions = len(batch)
		return outcome
	}
	batch, err = e.preserveUnavailableSourceProjects(
		ctx, batch,
	)
	if err != nil {
		if ctx.Err() != nil {
			return outcome
		}
		log.Printf("preserve unavailable source projects: %v", err)
		outcome := writeBatchOutcome{
			written:  make([]bool, len(batch)),
			resolved: make([]bool, len(batch)),
		}
		for _, pw := range batch {
			e.markStaleFailedMemberWrite(pw)
		}
		outcome.failedSessions = len(batch)
		return outcome
	}
	if ctx.Err() != nil {
		return outcome
	}
	if writeMode == syncWriteBulk || e.discardWritesOnCancel {
		return e.writeBatchBulkWithOutcomeContext(ctx, batch, forceReplace)
	}
	resolveWorktreeProject := e.loadWorktreeProjectResolverContext(ctx)
	for i, pw := range batch {
		if ctx.Err() != nil {
			return outcome
		}
		s, msgs, verdict, prepErr := e.prepareSessionWriteContext(
			ctx, pw, resolveWorktreeProject,
		)
		if prepErr != nil {
			return outcome
		}
		if verdict != sessionWriteOK {
			if verdict == sessionWriteCwdFiltered {
				outcome.cwdFiltered++
			}
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
		if ctx.Err() != nil {
			return outcome
		}

		// The session row must exist before messages can be inserted (FK
		// constraint), but a source-missing row stays tombstoned until every
		// dependent write succeeds below. For incremental updates
		// (writeIncremental), messages are written first since the session
		// already exists.
		revivingSourceMissing, err :=
			e.upsertSessionPendingContentForWrite(pw, s)
		if err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			if isIntentionalSessionSkip(err) {
				outcome.resolved[i] = true
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
			e.markStaleFailedMemberWrite(pw)
			outcome.failedSessions++
			continue
		}
		replaceMessages := shouldReplaceFullParseMessages(
			pw, forceReplace, stale, revivingSourceMissing,
		)

		var update db.SessionSignalUpdate
		var findings []db.SecretFinding
		if !e.disableSignalRecompute {
			update, findings = computeSignalsAndSecrets(s, msgs)
			if ctx.Err() != nil {
				return outcome
			}
		}

		var werr error
		if replaceMessages && !e.disableSignalRecompute {
			werr = e.db.ReplaceSessionContent(s.ID, msgs, update, findings)
		} else if replaceMessages {
			if msgs == nil {
				msgs = []db.Message{}
			}
			werr = e.db.ReplaceSessionMessages(s.ID, msgs)
		} else {
			werr = e.writeMessages(s.ID, msgs)
		}
		if werr != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf(
				"write messages for %s: %v",
				s.ID, werr,
			)
			e.markStaleFailedMemberWrite(pw)
			outcome.failedSessions++
			continue
		}
		if ctx.Err() != nil {
			return outcome
		}
		usageEvents, usageErr := e.usageEventsForWriteContext(
			ctx, s.ID, pw.usageEvents,
		)
		if usageErr != nil {
			return outcome
		}
		if err := e.db.ReplaceSessionUsageEvents(
			s.ID, usageEvents,
		); err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf(
				"write usage events for %s: %v",
				s.ID, err,
			)
			e.markStaleFailedMemberWrite(pw)
			outcome.failedSessions++
			continue
		}
		if ctx.Err() != nil {
			return outcome
		}

		// Advance data_version only after the message and usage writes
		// succeeded. The pending upsert deliberately does not touch this
		// column, and the source-missing tombstone is cleared only after this
		// succeeds, so an old current version cannot hide a failed rewrite.
		if err := e.db.SetSessionDataVersion(
			s.ID, dataVersionForWrite(pw),
		); err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf(
				"set data_version for %s: %v", s.ID, err,
			)
			e.markStaleFailedMemberWrite(pw)
			outcome.failedSessions++
			continue
		}
		if ctx.Err() != nil {
			return outcome
		}

		if !replaceMessages && !e.disableSignalRecompute {
			if ctx.Err() != nil {
				return outcome
			}
			// Same ordering contract as recomputeSignalsFromDB: the
			// version-advancing signals update only runs after findings
			// persisted, so a partial failure leaves the session below
			// the current version for the startup backfill to retry.
			if err := e.db.ReplaceSessionSecretFindings(
				s.ID, findings, update.SecretLeakCount,
				update.SecretsRulesVersion); err != nil {
				log.Printf("secrets: persist %s: %v", s.ID, err)
			} else if err := e.db.UpdateSessionSignals(s.ID, update); err != nil {
				log.Printf("signals: update %s: %v", s.ID, err)
			}
		}
		if ctx.Err() != nil {
			return outcome
		}
		if err := e.db.ReviveSourceMissingSession(s.ID); err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf("revive source-missing session %s: %v", s.ID, err)
			outcome.failedSessions++
			continue
		}
		outcome.writtenSessions++
		outcome.writtenMessages += len(msgs)
		outcome.written[i] = true
		outcome.resolved[i] = true
	}
	return outcome
}

// sessionWriteVerdict says whether prepareSessionWrite produced a
// writable session and, when it did not, why. The cwd-filter veto is
// distinguished from archive-preserve vetoes so sync stats can count
// filtered sessions: a resync where every discovered session is
// filtered must read as intentional, not as an empty rebuild.
type sessionWriteVerdict int

const (
	sessionWriteOK sessionWriteVerdict = iota
	sessionWritePreserved
	sessionWriteCwdFiltered
)

func (e *Engine) prepareSessionWrite(
	pw pendingWrite,
	resolveWorktreeProject worktreeProjectResolver,
) (db.Session, []db.Message, sessionWriteVerdict) {
	s, msgs, verdict, _ := e.prepareSessionWriteContext(
		context.Background(), pw, resolveWorktreeProject,
	)
	return s, msgs, verdict
}

func (e *Engine) prepareSessionWriteContext(
	ctx context.Context,
	pw pendingWrite,
	resolveWorktreeProject worktreeProjectResolver,
) (db.Session, []db.Message, sessionWriteVerdict, error) {
	msgs, err := toDBMessagesContext(ctx, pw, e.blockedResultCategories)
	if err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}
	s, err := toDBSessionContext(ctx, pw)
	if err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}
	if err := applySessionMessageDerivedFieldsContext(
		ctx, &s, msgs, pw.sess.CountsAuthoritative,
	); err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}
	if err := e.applyRemoteRewritesContext(ctx, &s, msgs); err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}
	applySourceCwdResolution(
		&s, pw.sourceCwdResolution, pw.sourceCwdStored, pw.sourceCwdStoredOK,
	)
	if !pw.sourceIdentityUnverified &&
		s.Cwd != "" && resolveWorktreeProject != nil {
		if mapped, ok := resolveWorktreeProject(
			s.Machine, s.Cwd, s.Project,
		); ok {
			s.Project = mapped
		}
	}

	// Veto sessions outside the configured cwd allow-list before any
	// preserve/merge handling so a filtered session is not written by
	// any downstream path.
	if !e.cwdFilter.allows(s.Cwd) {
		return db.Session{}, nil, sessionWriteCwdFiltered, nil
	}
	if err := ctx.Err(); err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}

	if e.shouldPreserveOpenCodeFormatArchive(
		pw.sess.Agent, pw.sess.File.Path, s.ID,
		pw.sess.File.Mtime, derefString(s.FileHash), msgs,
	) {
		return db.Session{}, nil, sessionWritePreserved, nil
	}
	if e.shouldPreserveRooCodeArchive(pw.sess.Agent, s.ID, msgs) {
		return db.Session{}, nil, sessionWritePreserved, nil
	}
	if mergedMsgs, preserve, archived := e.reconcileVisualStudioCopilotArchive(
		pw.sess.Agent, s.ID, pw.sess.File.Size, msgs,
	); preserve {
		return db.Session{}, nil, sessionWritePreserved, nil
	} else if mergedMsgs != nil {
		parsedMsgs := msgs
		msgs = mergedMsgs
		applyVisualStudioCopilotArchiveSessionFields(
			&s, archived, parsedMsgs, msgs,
		)
		if err := applySessionMessageDerivedFieldsContext(
			ctx, &s, msgs, pw.sess.CountsAuthoritative,
		); err != nil {
			return db.Session{}, nil, sessionWritePreserved, err
		}
		if err := applySessionTokenTotalsFromMessagesContext(
			ctx, &s, msgs,
		); err != nil {
			return db.Session{}, nil, sessionWritePreserved, err
		}
	}
	if err := ctx.Err(); err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}
	// Snapshot, before sanitizing, whether the session's token aggregates
	// are derived from the per-message rows or the per-usage-event rows, by
	// matching the stored value against each source's raw sum/max. Aggregates
	// set directly from a session-level usage summary -- agents like
	// Warp/Vibe/Hermes/Zed -- must survive the per-row clamp untouched.
	// Source=="session" usage events mirror those same summary totals, so
	// exclude them from the event-derived detector and re-clamp path.
	msgTotal, msgHasOut, msgPeak, msgHasCtx, err :=
		messageTokenTotalsContext(ctx, msgs)
	if err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}
	evtTotal, evtHasOut, evtPeak, evtHasCtx, err :=
		usageEventTokenTotalsContext(ctx, pw.usageEvents, false)
	if err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}
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
	vs, err := validateAndSanitizeContext(ctx, &s, msgs, nil)
	if err != nil {
		return db.Session{}, nil, sessionWritePreserved, err
	}
	e.anomalies.recordSanitize(vs)
	e.anomalies.recordMalformedLines(
		s.Agent, pw.sess.File.Path, s.ParserMalformedLines,
	)
	// An Antigravity session decoded from an unrecognized (newer) schema
	// carries an "agy-schema:" source_version; count it as an early warning
	// that a new agy build may have broken the heuristic decode. Reuse the
	// single-source-of-truth rule so the agent gate stays in one place.
	if parser.DecodeConfidence(s.Agent, s.SourceVersion) == parser.DecodeConfidenceLow {
		e.anomalies.recordUnknownSchemaSession(s.Agent)
	}
	// An Antigravity session whose gen_metadata table carried rows but decoded
	// into zero usage events warns that a newer agy build may have changed the
	// gen_metadata wire format the token-block heuristic depends on. The flag
	// is set by the parsers from the final usageEvents, so a sidecar-rescued
	// session is not counted here.
	if pw.sess.GenMetadataWithoutUsage {
		e.anomalies.recordGenMetadataWithoutUsageSession(s.Agent)
	}

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
		t, h, _, _, err := messageTokenTotalsContext(ctx, msgs)
		if err != nil {
			return db.Session{}, nil, sessionWritePreserved, err
		}
		s.TotalOutputTokens, s.HasTotalOutputTokens = t, h
	} else if totalFromEvts {
		t, h, _, _, err := usageEventTokenTotalsContext(
			ctx, pw.usageEvents, true,
		)
		if err != nil {
			return db.Session{}, nil, sessionWritePreserved, err
		}
		s.TotalOutputTokens, s.HasTotalOutputTokens = t, h
	}
	if peakFromMsgs {
		_, _, p, h, err := messageTokenTotalsContext(ctx, msgs)
		if err != nil {
			return db.Session{}, nil, sessionWritePreserved, err
		}
		s.PeakContextTokens, s.HasPeakContextTokens = p, h
	} else if peakFromEvts {
		_, _, p, h, err := usageEventTokenTotalsContext(
			ctx, pw.usageEvents, true,
		)
		if err != nil {
			return db.Session{}, nil, sessionWritePreserved, err
		}
		s.PeakContextTokens, s.HasPeakContextTokens = p, h
	}
	return s, msgs, sessionWriteOK, ctx.Err()
}

func applySessionMessageDerivedFieldsContext(
	ctx context.Context,
	s *db.Session,
	msgs []db.Message,
	countsAuthoritative bool,
) error {
	if !countsAuthoritative {
		var err error
		s.MessageCount, s.UserMessageCount, err = postFilterCountsContext(
			ctx, msgs,
		)
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.IsAutomated = db.IsAutomatedSessionMetadata(s.Agent, s.SessionKind) ||
		db.IsAutomatedTranscript(
			s.UserMessageCount, msgs, s.FirstMessage,
		)
	return ctx.Err()
}

// messageTokenTotalsContext computes the message-derived session token
// aggregates: the sum of per-message output tokens and the peak
// per-message context tokens, each with a presence flag. It is the
// canonical derivation shared by session preparation and the post-sanitize
// reconciliation that re-derives message-derived totals from the clamped rows.
// Absent values return 0 with a false presence.
func messageTokenTotalsContext(
	ctx context.Context, msgs []db.Message,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool, err error) {
	for _, msg := range msgs {
		if err = ctx.Err(); err != nil {
			return
		}
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
	err = ctx.Err()
	return
}

func applySessionTokenTotalsFromMessagesContext(
	ctx context.Context, s *db.Session, msgs []db.Message,
) error {
	totalOut, hasOut, peakCtx, hasCtx, err := messageTokenTotalsContext(
		ctx, msgs,
	)
	if err != nil {
		return err
	}
	s.TotalOutputTokens = totalOut
	s.HasTotalOutputTokens = hasOut
	s.PeakContextTokens = peakCtx
	s.HasPeakContextTokens = hasCtx
	return nil
}

// usageEventTokenTotals computes event-derived session token aggregates through
// parser.UsageEventTokenAggregate -- the same rollup per-turn event parsers use
// to populate stored session totals (positive output summed, peak full context
// = input + cache-creation + cache-read where positive). Session-summary usage
// events mirror parser summary totals rather than per-turn rows, so they are
// excluded from this detector and re-clamp path. When clamp is true each
// included event token field is first bounded to the per-row plausibility cap,
// matching how sanitizeUsageEvent bounds the stored usage_event row.
func usageEventTokenTotalsContext(
	ctx context.Context, events []parser.ParsedUsageEvent, clamp bool,
) (totalOut int, hasOut bool, peakCtx int, hasCtx bool, err error) {
	rolled := make([]parser.ParsedUsageEvent, 0, len(events))
	for _, ev := range events {
		if err = ctx.Err(); err != nil {
			return
		}
		if ev.Source == "session" {
			continue
		}
		rolled = append(rolled, ev)
	}
	if clamp {
		for i, ev := range rolled {
			if err = ctx.Err(); err != nil {
				return
			}
			ev.InputTokens = clampedTokens(ev.InputTokens)
			ev.OutputTokens = clampedTokens(ev.OutputTokens)
			ev.CacheCreationInputTokens = clampedTokens(
				ev.CacheCreationInputTokens,
			)
			ev.CacheReadInputTokens = clampedTokens(ev.CacheReadInputTokens)
			rolled[i] = ev
		}
	}
	totalOut, hasOut, peakCtx, hasCtx, err =
		parser.UsageEventTokenAggregateContext(ctx, rolled)
	return
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

type projectIdentityCacheEntry struct {
	rootPath         string
	repositoryPath   string
	gitDir           string
	gitRemoteName    string
	gitRemote        string
	remoteResolution export.ProjectResolution
	remoteCandidates int
	worktreeName     string
	worktreeRootPath string
	worktreeKind     export.WorktreeRelationship
	expiresAt        time.Time
}

type localGitIdentity struct {
	rootPath       string
	repositoryPath string
	gitDir         string
	remotes        map[string]string
	worktreeKind   export.WorktreeRelationship
}

func (e *Engine) writeBatchBulkWithOutcome(
	batch []pendingWrite, forceReplace bool,
) writeBatchOutcome {
	return e.writeBatchBulkWithOutcomeContext(
		context.Background(), batch, forceReplace,
	)
}

func (e *Engine) writeBatchBulkWithOutcomeContext(
	ctx context.Context, batch []pendingWrite, forceReplace bool,
) writeBatchOutcome {
	outcome := writeBatchOutcome{
		written:  make([]bool, len(batch)),
		resolved: make([]bool, len(batch)),
	}
	writes := make([]db.SessionBatchWrite, 0, len(batch))
	pendingIndexes := make([]int, 0, len(batch))
	sources := make(map[string]batchSourceFile, len(batch))
	pendingByID := make(map[string]pendingWrite, len(batch))
	pendingIndexByID := make(map[string]int, len(batch))
	resolveWorktreeProject := e.loadWorktreeProjectResolverContext(ctx)

	for pendingIndex, pw := range batch {
		if ctx.Err() != nil {
			return outcome
		}
		tPrep := time.Now()
		s, msgs, verdict, prepErr := e.prepareSessionWriteContext(
			ctx, pw, resolveWorktreeProject,
		)
		if prepErr != nil {
			return outcome
		}
		e.phaseStats.PrepNanos.Add(int64(time.Since(tPrep)))
		if verdict != sessionWriteOK {
			if verdict == sessionWriteCwdFiltered {
				outcome.cwdFiltered++
			}
			continue
		}
		replaceMessages := shouldReplaceFullParseMessages(
			pw, forceReplace, false, false,
		)
		var update db.SessionSignalUpdate
		var findings []db.SecretFinding
		if !e.disableSignalRecompute {
			tScan := time.Now()
			update, findings = computeSignalsAndSecrets(s, msgs)
			if ctx.Err() != nil {
				return outcome
			}
			e.phaseStats.ScanNanos.Add(int64(time.Since(tScan)))
		}
		snapshotProject := pw.sess.Project
		usageEvents, usageErr := e.usageEventsForWriteContext(
			ctx, s.ID, pw.usageEvents,
		)
		if usageErr != nil {
			return outcome
		}
		writes = append(writes, db.SessionBatchWrite{
			Session:     s,
			Messages:    msgs,
			UsageEvents: usageEvents,
			IdentityObservation: identityObservationOrZero(
				e.projectIdentityObservationForWrite(pw, s),
			),
			IdentitySnapshotProject: &snapshotProject,
			Signals:                 update,
			Findings:                findings,
			SkipSignalUpdates:       e.disableSignalRecompute,
			DataVersion:             dataVersionForWrite(pw),
			ReplaceMessages:         replaceMessages,
		})
		pendingIndexes = append(pendingIndexes, pendingIndex)
		pendingByID[s.ID] = pw
		pendingIndexByID[s.ID] = pendingIndex
		if pw.sess.File.Path != "" {
			sources[s.ID] = batchSourceFile{
				path:        pw.sess.File.Path,
				mtime:       pw.sess.File.Mtime,
				fingerprint: pw.sess.File.Hash,
			}
		}
	}
	if len(writes) == 0 {
		return outcome
	}
	if ctx.Err() != nil {
		return outcome
	}

	tWrite := time.Now()
	result, err := e.db.WriteSessionBatchContext(ctx, writes)
	e.phaseStats.WriteNanos.Add(int64(time.Since(tWrite)))
	e.phaseStats.Batches.Add(1)
	e.phaseStats.WriteBatchSize.Add(int64(len(writes)))
	e.phaseStats.BatchedWrites.Add(int64(result.WrittenSessions))
	if err != nil {
		log.Printf("write session batch: %v", err)
		for _, pw := range pendingByID {
			e.markStaleFailedMemberWrite(pw)
		}
		outcome.failedSessions += len(writes)
		return outcome
	}
	for _, writtenIndex := range result.WrittenIndexes {
		if writtenIndex >= 0 && writtenIndex < len(pendingIndexes) {
			pendingIndex := pendingIndexes[writtenIndex]
			outcome.written[pendingIndex] = true
			outcome.resolved[pendingIndex] = true
		}
	}
	for _, id := range result.FailedIDs {
		if pw, ok := pendingByID[id]; ok {
			e.markStaleFailedMemberWrite(pw)
		}
	}
	for _, id := range result.ExcludedIDs {
		if pendingIndex, ok := pendingIndexByID[id]; ok {
			outcome.resolved[pendingIndex] = true
		}
		if source, ok := sources[id]; ok && source.path != "" {
			e.cacheSkip(
				source.path, source.mtime, source.fingerprint,
			)
		}
	}
	for _, err := range result.Errors {
		log.Printf("write session batch: %v", err)
	}
	outcome.writtenSessions = result.WrittenSessions
	outcome.writtenMessages = result.WrittenMessages
	outcome.failedSessions += result.FailedSessions
	return outcome
}

func identityObservationOrZero(
	obs export.ProjectIdentityObservation,
	ok bool,
) export.ProjectIdentityObservation {
	if !ok {
		return export.ProjectIdentityObservation{}
	}
	return obs
}

func (e *Engine) projectIdentityObservation(
	s db.Session,
) (export.ProjectIdentityObservation, bool) {
	project := strings.TrimSpace(s.Project)
	machine := strings.TrimSpace(s.Machine)
	rootPath := strings.TrimSpace(s.Cwd)
	if project == "" || machine == "" {
		return export.ProjectIdentityObservation{}, false
	}

	cached := e.cachedProjectIdentity(machine, rootPath)
	obs := export.ProjectIdentityObservation{
		SessionID:  s.ID,
		Project:    project,
		Machine:    machine,
		RootPath:   cached.rootPath,
		ObservedAt: time.Now().UTC(),
	}
	obs.GitRemoteName = cached.gitRemoteName
	obs.GitRemote = cached.gitRemote
	obs.RemoteResolution = cached.remoteResolution
	obs.RemoteCandidateCount = cached.remoteCandidates
	obs.WorktreeName = cached.worktreeName
	obs.WorktreeRootPath = cached.worktreeRootPath
	obs.RepositoryPath = cached.repositoryPath
	obs.WorktreeRelationship = cached.worktreeKind
	obs.GitBranch = strings.TrimSpace(s.GitBranch)
	if obs.GitBranch != "" {
		obs.CheckoutState = export.CheckoutBranch
	} else {
		obs.CheckoutState, obs.GitBranch = readGitCheckout(
			cached.gitDir, e.mayProbeLocalPath,
		)
	}
	return obs, true
}

func (e *Engine) projectIdentityObservationForWrite(
	pw pendingWrite,
	s db.Session,
) (export.ProjectIdentityObservation, bool) {
	if pw.sourceIdentityUnverified {
		return export.ProjectIdentityObservation{}, false
	}
	return e.projectIdentityObservation(s)
}

func (e *Engine) cachedProjectIdentity(machine, rootPath string) projectIdentityCacheEntry {
	e.projectIdentityMu.Lock()
	defer e.projectIdentityMu.Unlock()
	if e.projectIdentityCache == nil {
		e.projectIdentityCache = make(map[string]projectIdentityCacheEntry)
	}
	cacheKey := machine + "\x00" + rootPath
	now := time.Now()
	if cached, ok := e.projectIdentityCache[cacheKey]; ok &&
		now.Before(cached.expiresAt) {
		return cached
	}
	identity := projectIdentityCacheEntry{rootPath: rootPath}
	// Only probe the local filesystem for sessions recorded on this
	// machine: another machine's cwd (e.g. /home/... from a synced Linux
	// host) is meaningless here, and on macOS merely stat'ing such paths
	// wakes the /home automounter — with tens of thousands of remote
	// sessions and a one-minute cache TTL that becomes a sustained
	// automountd/opendirectoryd CPU storm.
	// mayProbeLocalPath also guards export.NormalizeRootPath below, which
	// resolves symlinks and would reach into a protected location on its own.
	if !e.disableProjectDiscovery && e.idPrefix == "" && e.pathRewriter == nil &&
		e.isLocalMachineAttribution(machine) && e.mayProbeLocalPath(rootPath) {
		if normalized, ok, err := export.NormalizeRootPath(rootPath); err == nil && ok {
			identity.rootPath = normalized
		}
		if discovered := discoverLocalGitIdentity(
			rootPath, e.mayProbeLocalPath,
		); discovered.rootPath != "" {
			identity.rootPath = discovered.rootPath
			identity.repositoryPath = discovered.repositoryPath
			identity.gitDir = discovered.gitDir
			identity.worktreeRootPath = discovered.rootPath
			identity.worktreeName = filepath.Base(discovered.rootPath)
			identity.worktreeKind = discovered.worktreeKind
			selection := export.ResolveRemoteSelection(discovered.remotes)
			identity.remoteResolution = selection.Resolution
			if selection.Resolution == export.ProjectResolutionUnknown {
				identity.remoteResolution = export.ProjectResolutionResolved
			}
			identity.remoteCandidates = countNormalizedRemoteCandidates(discovered.remotes)
			if selection.Resolution == export.ProjectResolutionResolved {
				identity.gitRemoteName = selection.Name
				identity.gitRemote = selection.Raw
			}
		}
	}
	if identity.worktreeRootPath == "" {
		identity.worktreeName = filepath.Base(identity.rootPath)
		identity.worktreeRootPath = identity.rootPath
		identity.worktreeKind = export.WorktreeUnknown
	}
	identity.expiresAt = now.Add(projectIdentityCacheTTL)
	e.projectIdentityCache[cacheKey] = identity
	return identity
}

func (e *Engine) writeProjectIdentityObservation(
	ctx context.Context, s db.Session,
) error {
	return e.writeProjectIdentityObservationWithSnapshotProject(
		ctx, s, s.Project,
	)
}

func (e *Engine) writeProjectIdentityObservationWithSnapshotProject(
	ctx context.Context,
	s db.Session,
	snapshotProject string,
) error {
	obs, ok := e.projectIdentityObservation(s)
	if !ok {
		return nil
	}
	snapshot := obs
	snapshot.Project = snapshotProject
	fingerprint := projectIdentityObservationFingerprint(obs) + "\x00" +
		projectIdentityObservationFingerprint(snapshot)
	e.projectIdentityMu.Lock()
	if e.projectIdentityWritten == nil {
		e.projectIdentityWritten = make(map[string]struct{})
	}
	if _, ok := e.projectIdentityWritten[fingerprint]; ok {
		e.projectIdentityMu.Unlock()
		return nil
	}
	e.projectIdentityMu.Unlock()

	if err := e.db.UpsertProjectIdentityObservationWithSnapshotProject(
		ctx, obs, snapshotProject,
	); err != nil {
		return err
	}

	e.projectIdentityMu.Lock()
	e.projectIdentityWritten[fingerprint] = struct{}{}
	e.projectIdentityMu.Unlock()
	return nil
}

func (e *Engine) upsertSessionPendingContentWithProjectIdentity(
	s db.Session,
	snapshotProject string,
) (bool, error) {
	obs, ok := e.projectIdentityObservation(s)
	if !ok {
		return e.db.UpsertSessionPendingContent(s)
	}
	return e.db.UpsertSessionPendingContentWithProjectIdentity(
		s, obs, snapshotProject,
	)
}

func (e *Engine) upsertSessionPendingContentForWrite(
	pw pendingWrite,
	s db.Session,
) (bool, error) {
	if pw.sourceIdentityUnverified {
		return e.db.UpsertSessionPendingContent(s)
	}
	return e.upsertSessionPendingContentWithProjectIdentity(
		s, pw.sess.Project,
	)
}

func projectIdentityObservationFingerprint(
	obs export.ProjectIdentityObservation,
) string {
	return strings.Join([]string{
		obs.Project,
		obs.SessionID,
		obs.Machine,
		obs.RootPath,
		obs.GitRemote,
		obs.GitRemoteName,
		obs.WorktreeName,
		obs.WorktreeRootPath,
		obs.RepositoryPath,
		string(obs.WorktreeRelationship),
		string(obs.CheckoutState),
		obs.GitBranch,
		string(obs.RemoteResolution),
		strconv.Itoa(obs.RemoteCandidateCount),
	}, "\x00")
}

func countNormalizedRemoteCandidates(remotes map[string]string) int {
	unique := make(map[string]struct{}, len(remotes))
	for _, raw := range remotes {
		if normalized, ok := export.NormalizeGitRemote(raw); ok {
			unique[normalized] = struct{}{}
		}
	}
	return len(unique)
}

func discoverLocalGitIdentity(
	cwd string, mayProbe func(string) bool,
) localGitIdentity {
	if !safeLocalAbsolutePath(cwd) {
		return localGitIdentity{}
	}
	// Skip macOS automounter namespaces: probing them wakes
	// automountd/opendirectoryd for paths that virtually never exist
	// locally (see export.IsAutomountNamespacePath).
	if export.IsAutomountNamespacePath(runtime.GOOS, filepath.Clean(cwd)) {
		return localGitIdentity{}
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(cwd))
	if err != nil {
		return localGitIdentity{}
	}
	root := findLocalGitRoot(resolved, mayProbe)
	if root == "" {
		return localGitIdentity{}
	}
	gitDir, commonDir, relationship := gitDirectoryContext(root, mayProbe)
	result := localGitIdentity{
		rootPath:       root,
		repositoryPath: repositoryPathForGitContext(root, commonDir, mayProbe),
		gitDir:         gitDir,
		worktreeKind:   relationship,
	}
	// The common directory comes from gitfile contents and can point
	// anywhere, including a protected location the vetted cwd never named,
	// and the config file inside a vetted one can itself be a symlink out;
	// vetting the exact file path covers both.
	if commonDir != "" {
		configPath := filepath.Join(commonDir, "config")
		if mayProbe(configPath) {
			result.remotes = readGitRemotes(configPath)
		}
	}
	return result
}

func safeLocalAbsolutePath(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" || strings.Contains(p, "://") {
		return false
	}
	if looksWindowsDrivePath(p) {
		return runtime.GOOS == "windows" && filepath.IsAbs(p)
	}
	if looksRemotePrefixedPath(p) {
		return false
	}
	return filepath.IsAbs(p)
}

func looksRemotePrefixedPath(p string) bool {
	colon := strings.Index(p, ":")
	if colon <= 0 {
		return false
	}
	prefix := p[:colon]
	return !strings.ContainsAny(prefix, `/\`)
}

func looksWindowsDrivePath(p string) bool {
	if len(p) < 3 || p[1] != ':' {
		return false
	}
	drive := p[0]
	if (drive < 'A' || drive > 'Z') && (drive < 'a' || drive > 'z') {
		return false
	}
	return p[2] == '\\' || p[2] == '/'
}

func findLocalGitRoot(start string, mayProbe func(string) bool) string {
	dir := filepath.Clean(start)
	for {
		if gitEntryMarksRoot(filepath.Join(dir, ".git"), mayProbe) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// gitEntryMarksRoot reports whether gitPath denotes a git directory or
// gitfile, following a symlink only when its target passes mayProbe. A
// refused link still marks a root: it is a repo boundary we must not look
// through, and gitDirectoryContext refuses the reads under it.
func gitEntryMarksRoot(gitPath string, mayProbe func(string) bool) bool {
	info, err := os.Lstat(gitPath)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSymlink != 0 {
		if !mayProbe(gitPath) {
			return true
		}
		followed, err := os.Stat(gitPath)
		if err != nil {
			return false
		}
		return followed.IsDir() || followed.Mode().IsRegular()
	}
	return info.IsDir() || info.Mode().IsRegular()
}

func gitDirectoryContext(
	root string, mayProbe func(string) bool,
) (gitDir, commonDir string, relationship export.WorktreeRelationship) {
	gitPath := filepath.Join(root, ".git")
	// root is vetted, but the .git entry itself can be a symlink into a
	// guarded location; classification follows links, so vetting the exact
	// path keeps both the type probe and the gitfile read (and every later
	// HEAD, config, and commondir read under the returned directories)
	// from traversing into it.
	if !mayProbe(gitPath) {
		return "", "", export.WorktreeUnknown
	}
	if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
		return gitPath, gitPath, export.WorktreeMain
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return "", "", export.WorktreeUnknown
	}
	line := strings.TrimSpace(string(data))
	line = strings.TrimPrefix(line, "gitdir:")
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", export.WorktreeUnknown
	}
	if !filepath.IsAbs(line) {
		line = filepath.Join(root, line)
	}
	// The gitfile target comes from file contents, not from the vetted
	// cwd: a linked worktree in an unguarded directory can point at a
	// main repository inside a protected folder, and reading commondir or
	// HEAD there would raise the consent prompt the cwd gate prevented.
	if !mayProbe(line) {
		return "", "", export.WorktreeUnknown
	}
	commonDir = line
	relationship = export.WorktreeMain
	// The commondir file sits inside the vetted gitdir, but as a symlink
	// it can lead anywhere; vet the exact path (classification follows
	// links) before reading through it.
	commondirPath := filepath.Join(line, "commondir")
	if !mayProbe(commondirPath) {
		return filepath.Clean(line), filepath.Clean(commonDir), relationship
	}
	if data, err := os.ReadFile(commondirPath); err == nil {
		common := strings.TrimSpace(string(data))
		if filepath.IsAbs(common) {
			commonDir = common
		} else {
			commonDir = filepath.Clean(filepath.Join(line, common))
		}
		relationship = export.WorktreeLinked
	}
	return filepath.Clean(line), filepath.Clean(commonDir), relationship
}

func repositoryPathForGitContext(
	root, commonDir string, mayProbe func(string) bool,
) string {
	repositoryPath := commonDir
	if commonDir == "" {
		repositoryPath = root
	} else if filepath.Base(commonDir) == ".git" {
		repositoryPath = filepath.Dir(commonDir)
	}
	// Storing the path string is harmless; resolving its symlinks is a
	// filesystem walk that must respect the protected-path policy.
	if !mayProbe(repositoryPath) {
		return filepath.Clean(repositoryPath)
	}
	if resolved, err := filepath.EvalSymlinks(repositoryPath); err == nil {
		return resolved
	}
	return filepath.Clean(repositoryPath)
}

func readGitCheckout(
	gitDir string, mayProbe func(string) bool,
) (export.CheckoutState, string) {
	if gitDir == "" {
		return export.CheckoutUnknown, ""
	}
	// HEAD sits inside the vetted git directory, but as a symlink it can
	// lead anywhere; vet the exact path before reading through it.
	headPath := filepath.Join(gitDir, "HEAD")
	if !mayProbe(headPath) {
		return export.CheckoutUnknown, ""
	}
	data, err := os.ReadFile(headPath)
	if err != nil {
		return export.CheckoutUnknown, ""
	}
	head := strings.TrimSpace(string(data))
	const branchPrefix = "ref: refs/heads/"
	if after, ok := strings.CutPrefix(head, branchPrefix); ok {
		branch := strings.TrimSpace(after)
		if branch != "" {
			return export.CheckoutBranch, branch
		}
	}
	if head != "" {
		return export.CheckoutDetached, ""
	}
	return export.CheckoutUnknown, ""
}

func readGitRemotes(configPath string) map[string]string {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil
	}
	remotes := map[string]string{}
	var current string
	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			current = remoteNameFromGitConfigSection(trimmed)
			continue
		}
		if current == "" || !strings.HasPrefix(trimmed, "url") {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		remotes[current] = strings.TrimSpace(value)
	}
	return remotes
}

func remoteNameFromGitConfigSection(section string) string {
	section = strings.Trim(section, "[]")
	if !strings.HasPrefix(section, `remote `) {
		return ""
	}
	name := strings.TrimSpace(strings.TrimPrefix(section, `remote `))
	return strings.Trim(name, `"`)
}

func shouldReplaceFullParseMessages(
	pw pendingWrite, forceReplace, stale, revivingSourceMissing bool,
) bool {
	return forceReplace || pw.forceReplace || pw.needsRetry || stale ||
		revivingSourceMissing ||
		pw.sess.Agent == parser.AgentCowork ||
		// Copilot execution start/completion records arrive after the
		// assistant tool-call message and attach result events to it. An
		// append would leave the persisted tool call without those events.
		pw.sess.Agent == parser.AgentCopilot ||
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
		// RooCode pairs later command_output, MCP response, subtask
		// result, and error records back to earlier tool-call
		// messages, and strips embedded read results into them. An
		// append would leave the existing rows' result events stale.
		pw.sess.Agent == parser.AgentRooCode ||
		// Kilo Legacy pairs later command_output, MCP response,
		// and error records back to earlier tool-call messages,
		// similar to RooCode. An incremental append would leave
		// the existing rows' result events stale.
		pw.sess.Agent == parser.AgentKiloLegacy ||
		// Poolside pairs tool_call.result back to earlier
		// tool_call.parsed messages via step_id, appending
		// ResultEvents. An incremental append would leave
		// existing tool calls without their results.
		pw.sess.Agent == parser.AgentPoolside ||
		pw.sess.Agent == parser.AgentReasonix
}

// writeIncremental appends new messages and partially updates
// session metadata without overwriting columns that are not
// recomputed during incremental parsing (e.g. parent_session_id,
// relationship_type). Codex refreshes file_hash because parse-diff
// uses it as the transcript fingerprint for raced-skew detection;
// Claude refreshes it so providerSingleSessionFresh can use the
// stored hash as a content fingerprint against same-size in-place
// rewrites. Other agents pass an empty hash, which COALESCE leaves
// untouched.
func (e *Engine) writeIncremental(
	inc *incrementalUpdate,
) error {
	// The full path vetoes filtered sessions in prepareSessionWrite;
	// this is the equivalent veto at the incremental write seam, so
	// no producer can append to a session outside the cwd allow-list.
	// tryIncrementalJSONL already refuses such sessions — this guard
	// keeps the seam safe for any future producer.
	if !e.cwdFilter.allows(inc.cwd) {
		log.Printf(
			"incremental %s: cwd %q outside the configured "+
				"allow-list, skipping append",
			inc.sessionID, inc.cwd,
		)
		return nil
	}

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

	subagentLinks := make([]db.ToolCallSubagentLink, len(inc.links))
	for i, link := range inc.links {
		toolCall := db.ToolCall{
			ResultContent:       parser.DecodeContent(link.ResultContentRaw),
			ResultContentLength: link.ResultContentLen,
		}
		e.anomalies.recordSanitize(db.SanitizeToolCall(&toolCall))
		subagentLinks[i] = db.ToolCallSubagentLink{
			ToolUseID: link.ToolUseID,
			SubagentSessionID: applyIDPrefixToID(
				e.idPrefix, link.SubagentSessionID,
			),
			ResultContent:    toolCall.ResultContent,
			ResultContentLen: toolCall.ResultContentLength,
			HasResult:        link.HasResult,
		}
	}

	if err := e.db.WriteSessionIncremental(
		inc.sessionID,
		dbMsgs,
		db.IncrementalSessionUpdate{
			EndedAt:                 endedAt,
			TerminationStatus:       inc.terminationStatus,
			MsgCount:                msgCount,
			UserMsgCount:            userMsgCount,
			FileSize:                inc.fileSize,
			FileMtime:               inc.fileMtime,
			FileHash:                strPtr(inc.fileHash),
			NextOrdinal:             inc.nextOrdinal,
			LastEntryUUID:           inc.lastEntryUUID,
			TotalOutputTokens:       inc.totalOutputTokens,
			PeakContextTokens:       inc.peakContextTokens,
			HasTotalOutputTokens:    inc.hasTotalOutputTokens,
			HasPeakContextTokens:    inc.hasPeakContextTokens,
			SubagentLinks:           subagentLinks,
			BlockedResultCategories: e.blockedResultCategories,
		},
	); err != nil {
		return fmt.Errorf(
			"incremental write %s: %w",
			inc.sessionID, err,
		)
	}

	finalProject, err := e.applyWorktreeMappingToSingleSession(
		inc.sessionID,
	)
	if err != nil {
		return err
	}
	identitySession := db.Session{
		ID:      inc.sessionID,
		Project: finalProject,
		Machine: inc.machine,
		Cwd:     inc.cwd,
	}
	if err := e.writeProjectIdentityObservationWithSnapshotProject(
		context.Background(),
		identitySession,
		inc.sourceProject,
	); err != nil {
		log.Printf(
			"incremental project identity observation %s: %v",
			inc.sessionID, err,
		)
	}

	// Signal/secret recompute costs O(session history), so it is
	// debounced per session instead of running on every appended
	// line: the first write after a quiet period recomputes
	// inline, writes during a streaming burst coalesce into one
	// recompute per interval plus a trailing flush. Recompute
	// errors are logged inside recomputeSignalsFromDB and are
	// non-fatal; a later write or flush retries.
	e.signalSched.markDirty(inc.sessionID)
	if inc.providerStatHash != nil {
		e.recordProviderStatHash(
			context.Background(), *inc.providerStatHash,
		)
	}

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
	normalized, err := e.normalizePendingWriteMachines(
		context.Background(), []pendingWrite{pw},
	)
	if err != nil {
		return err
	}
	preserved, err := e.preserveUnavailableSourceProjects(
		context.Background(), normalized,
	)
	if err != nil {
		return err
	}
	pw = preserved[0]
	s, msgs, verdict := e.prepareSessionWrite(
		pw, resolveWorktreeProject,
	)
	if verdict != sessionWriteOK {
		return errSessionPreserved
	}
	_, err = e.upsertSessionPendingContentForWrite(pw, s)
	if err != nil {
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
	var replaceErr error
	if e.disableSignalRecompute {
		if msgs == nil {
			msgs = []db.Message{}
		}
		replaceErr = e.db.ReplaceSessionMessages(s.ID, msgs)
	} else {
		update, findings := computeSignalsAndSecrets(s, msgs)
		replaceErr = e.db.ReplaceSessionContent(s.ID, msgs, update, findings)
	}
	if replaceErr != nil {
		log.Printf(
			"replace messages for %s: %v",
			s.ID, replaceErr,
		)
		return replaceErr
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
		return err
	}
	if err := e.db.ReviveSourceMissingSession(s.ID); err != nil {
		log.Printf("revive source-missing session %s: %v", s.ID, err)
		return err
	}

	return nil
}

// shouldPreserveRooCodeArchive reports whether a zero-message RooCode
// parse must not overwrite an archived transcript. A vanished (or
// torn) ui_messages.json parses as a zero-message session while
// history_item.json keeps the task discoverable, so writing that
// parse would corrupt the session's counts on normal sync and — with
// RooCode on the full-replace path — delete the archived messages
// outright; on a rebuild it would recreate the session empty in the
// fresh DB, which also blocks the orphan-copy pass from restoring it.
// Newly created metadata-only tasks have no archived messages and
// still write normally.
func (e *Engine) shouldPreserveRooCodeArchive(
	agent parser.AgentType, sessionID string, msgs []db.Message,
) bool {
	if (agent != parser.AgentRooCode && agent != parser.AgentKiloLegacy) || len(msgs) > 0 {
		return false
	}
	store := e.archiveStore
	if store == nil {
		store = e.db
	}
	stored, err := store.GetAllMessages(context.Background(), sessionID)
	if err != nil || len(stored) == 0 {
		return false
	}
	log.Printf(
		"skip %s session %s: transcript parsed empty but archive has %d messages",
		agent, sessionID, len(stored),
	)
	return true
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
	store := e.archiveStore
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
	storedHasStorageFingerprint := hasOpenCodeFormatStorageFingerprint(
		agent, storedHash,
	)
	storedIsSQLiteVirtual := isOpenCodeFormatSQLiteVirtualPath(
		agent, storedPath,
	)
	storedIsStorageArchive := isOpenCodeFormatStoragePath(
		agent, storedPath,
	) || (storedPath == "" && storedHasStorageFingerprint)
	if storedIsSQLiteVirtual {
		storedIsStorageArchive = false
	}
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
	if storedHasStorageFingerprint &&
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
	_ = e.applyRemoteRewritesContext(context.Background(), s, msgs)
}

func (e *Engine) applyRemoteRewritesContext(
	ctx context.Context, s *db.Session, msgs []db.Message,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.idPrefix == "" {
		return nil
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
		if err := ctx.Err(); err != nil {
			return err
		}
		msgs[i].SessionID = s.ID
		for j := range msgs[i].ToolCalls {
			if err := ctx.Err(); err != nil {
				return err
			}
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
	return ctx.Err()
}

// toDBSession converts a pendingWrite to a db.Session.
func toDBSession(pw pendingWrite) db.Session {
	s, _ := toDBSessionContext(context.Background(), pw)
	return s
}

func toDBSessionContext(
	ctx context.Context, pw pendingWrite,
) (db.Session, error) {
	hasTotal, hasPeak, err := pw.sess.TokenCoverageContext(ctx, pw.msgs)
	if err != nil {
		return db.Session{}, err
	}
	s := db.Session{
		ID:                   pw.sess.ID,
		Project:              pw.sess.Project,
		Machine:              pw.sess.Machine,
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
		FilePath:          strPtr(pw.sess.File.Path),
		FileSize:          int64Ptr(pw.sess.File.Size),
		FileMtime:         int64Ptr(pw.sess.File.Mtime),
		NextOrdinal:       nextParsedOrdinal(0, pw.msgs),
		LastEntryUUID:     strPtr(lastParsedSourceUUID("", pw.msgs)),
		ClaudeLinearParse: pw.sess.ClaudeLinearParse,
		FileInode:         int64Ptr(pw.sess.File.Inode),
		FileDevice:        int64Ptr(pw.sess.File.Device),
		FileHash:          strPtr(pw.sess.File.Hash),
	}
	db.ApplyParsedSessionIdentity(&s, pw.sess)
	if pw.sess.FirstMessage != "" {
		s.FirstMessage = &pw.sess.FirstMessage
	}
	s.SessionName = db.ParsedSessionName(pw.sess)
	s.PreserveSessionName = pw.sess.Agent == parser.AgentCodex &&
		!pw.sess.SessionNamePresent
	if !pw.sess.StartedAt.IsZero() {
		s.StartedAt = timeutil.Ptr(pw.sess.StartedAt)
	}
	if !pw.sess.EndedAt.IsZero() {
		s.EndedAt = timeutil.Ptr(pw.sess.EndedAt)
	}
	return s, ctx.Err()
}

// toDBMessages converts parsed messages to db.Message rows
// with tool-result pairing and filtering applied.
func toDBMessages(pw pendingWrite, blocked map[string]bool) []db.Message {
	msgs, _ := toDBMessagesContext(context.Background(), pw, blocked)
	return msgs
}

func toDBMessagesContext(
	ctx context.Context, pw pendingWrite, blocked map[string]bool,
) ([]db.Message, error) {
	msgs := make([]db.Message, len(pw.msgs))
	for i, m := range pw.msgs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hasCtx, hasOut := m.TokenPresence()
		toolCalls, err := convertToolCallsContext(ctx, pw.sess.ID, m.ToolCalls)
		if err != nil {
			return nil, err
		}
		toolResults, err := convertToolResultsContext(ctx, m.ToolResults)
		if err != nil {
			return nil, err
		}
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
			PromptSource:      m.PromptSource,
			SourceUUID:        m.SourceUUID,
			SourceParentUUID:  m.SourceParentUUID,
			IsSidechain:       m.IsSidechain,
			IsCompactBoundary: m.IsCompactBoundary,
			ToolCalls:         toolCalls,
			ToolResults:       toolResults,
		}
	}
	return pairAndFilterContext(ctx, msgs, blocked)
}

// toDBUsageEvents converts parser usage events for one session.
// sessionID is the final ID after remote rewrites; parser-stamped
// event session IDs predate the idPrefix and are ignored. It returns the
// fix counts from the central validation/sanitization pass so write paths
// can surface them in the sync summary; diagnostic callers may discard.
func toDBUsageEvents(
	sessionID string, events []parser.ParsedUsageEvent,
) ([]db.UsageEvent, validationStats) {
	out, stats, _ := toDBUsageEventsContext(
		context.Background(), sessionID, events,
	)
	return out, stats
}

func toDBUsageEventsContext(
	ctx context.Context, sessionID string, events []parser.ParsedUsageEvent,
) ([]db.UsageEvent, validationStats, error) {
	out := make([]db.UsageEvent, 0, len(events))
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return nil, validationStats{}, err
		}
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
			Cost:                     ev.Cost,
			CostStatus:               ev.CostStatus,
			CostSource:               ev.CostSource,
			OccurredAt:               ev.OccurredAt,
			DedupKey:                 ev.DedupKey,
		})
	}
	// Route usage events through the central validation/sanitization
	// pass so they get the same treatment as messages and sessions at
	// every call site.
	stats, err := validateAndSanitizeContext(ctx, nil, nil, out)
	return out, stats, err
}

// usageEventsForWrite converts usage events for a session about to be
// written and records the central-validation fix counts in the per-run
// anomaly accumulator for the sync summary.
func (e *Engine) usageEventsForWrite(
	sessionID string, events []parser.ParsedUsageEvent,
) []db.UsageEvent {
	out, _ := e.usageEventsForWriteContext(
		context.Background(), sessionID, events,
	)
	return out
}

func (e *Engine) usageEventsForWriteContext(
	ctx context.Context, sessionID string, events []parser.ParsedUsageEvent,
) ([]db.UsageEvent, error) {
	out, vs, err := toDBUsageEventsContext(ctx, sessionID, events)
	if err != nil {
		return nil, err
	}
	e.anomalies.recordSanitize(vs)
	return out, nil
}

// postFilterCounts returns the total and user message counts
// from a filtered message slice. System-injected messages
// (e.g. Zencoder compaction, continuation notices) are excluded
// from the user count.
func postFilterCounts(msgs []db.Message) (total, user int) {
	total, user, _ = postFilterCountsContext(context.Background(), msgs)
	return
}

func postFilterCountsContext(
	ctx context.Context, msgs []db.Message,
) (total, user int, err error) {
	for _, m := range msgs {
		if err = ctx.Err(); err != nil {
			return
		}
		if m.Role == "user" && !m.IsSystem {
			user++
		}
	}
	return len(msgs), user, ctx.Err()
}

// chunkHasRealUserPrompt reports whether msgs contains a message the
// Claude parser would use as first_message (mirroring the firstMsg
// rule in firstMessageAndUserCount): role user, not system-injected,
// non-empty content, and not a bare slash command. Promoted system
// records (IDE context, continuation, stop-hook), tool-result-only
// rows, and preview-skipped commands like /clear do not qualify —
// a full parse triggered by those would leave first_message empty
// and the next append would full-parse again.
func chunkHasRealUserPrompt(msgs []parser.ParsedMessage) bool {
	for _, m := range msgs {
		if m.Role == parser.RoleUser && !m.IsSystem &&
			m.Content != "" &&
			!parser.IsSkippablePreviewCommand(m.Content) {
			return true
		}
	}
	return false
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
		// Forge, Piebald, Warp, and ZCode are DB-backed providers that own
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
		Roots:          e.agentDirs[def.Type],
		Machine:        e.machine,
		SourceMachines: e.sourceMachines[def.Type],
		PathRewriter:   e.pathRewriter,
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
		machine := e.machineForPath(def.Type, providerDiscoveredPath(source))
		outcome, err := provider.Parse(ctx, parser.ParseRequest{
			Source:  source,
			Machine: machine,
		})
		if err != nil || !providerOutcomeContainsSession(outcome, sessionID) {
			return ""
		}
	}
	return providerDiscoveredPath(source)
}

// providerSessionSourceMtime resolves a session's authoritative source-backed
// mtime through the provider facade. It is used for sessions whose stored
// file_path is provider-owned (for example a virtual <db>#<sessionID> path), so
// SourceMtime stays on the same composite fingerprint basis sync uses for DB
// freshness checks. Piebald fork IDs (piebald:<chat>-<row>) resolve to their
// base chat source, so a fork is confirmed by parsing the chat and checking the
// requested session ID is actually produced before returning the chat mtime.
func (e *Engine) providerSessionSourceMtime(
	ctx context.Context,
	def parser.AgentDef,
	sessionID string,
	rawSessionID string,
	storedPath string,
) int64 {
	factory, ok := e.providerFactories[def.Type]
	if !ok || factory == nil {
		return 0
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:          e.agentDirs[def.Type],
		Machine:        e.machine,
		SourceMachines: e.sourceMachines[def.Type],
		PathRewriter:   e.pathRewriter,
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
		machine := e.machineForPath(def.Type, providerDiscoveredPath(source))
		outcome, err := provider.Parse(ctx, parser.ParseRequest{
			Source:  source,
			Machine: machine,
		})
		if err != nil || !providerOutcomeContainsSession(outcome, sessionID) {
			return 0
		}
	}
	return fingerprint.MTimeNS
}

func providerSourcePathNeedsFingerprint(path string) bool {
	if path == "" {
		return false
	}
	if _, _, ok := parser.SplitWindsurfVirtualPath(path); ok {
		return true
	}
	return parser.ResolveSourceFilePath(path) != path
}

func providerSourceMtimeNeedsFingerprint(agent parser.AgentType) bool {
	switch agent {
	case parser.AgentQoder:
		// Qoder stores a sidecar whose mtime the plain path stat misses.
		return true
	default:
		// RooCode is deliberately absent: its fingerprint content-hashes
		// both session files, and SourceMtime is polled by the session
		// watcher, so it uses the stat-only composite branch instead.
		return false
	}
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
			agent := parser.AgentType("")
			if sess, err := e.db.GetSession(
				context.Background(), sessionID,
			); err == nil && sess != nil {
				agent = parser.AgentType(sess.Agent)
			}
			if agent == "" {
				if def, ok := parser.AgentByPrefix(sessionID); ok {
					agent = def.Type
				}
			}
			if agent == "" {
				return 0
			}
			obj, err := statS3SourceObject(parser.DiscoveredFile{
				Agent: agent,
				Path:  fp,
			})
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
		// Forge, Piebald, Warp, and ZCode are DB-backed providers: their
		// per-session source mtime comes from the provider fingerprint
		// (which mirrors the legacy List*SessionMeta last-modified value).
		// Non-provider, non-file-based agents have no local source.
		if e.isProviderAuthoritative(def.Type) {
			return e.providerSessionSourceMtime(
				context.Background(), def, sessionID, rawSessionID, "",
			)
		}
		return 0
	}

	path := e.FindSourceFile(sessionID)
	if path == "" {
		return 0
	}
	if e.isProviderAuthoritative(def.Type) &&
		(providerSourcePathNeedsFingerprint(path) ||
			providerSourceMtimeNeedsFingerprint(def.Type)) {
		if mtime := e.providerSessionSourceMtime(
			context.Background(), def, sessionID, rawSessionID, path,
		); mtime != 0 {
			return mtime
		}
	}
	if isS3SourcePath(path) {
		obj, err := statS3SourceObject(parser.DiscoveredFile{
			Agent: def.Type,
			Path:  path,
		})
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
	if def.Type == parser.AgentRooCode {
		// Freshness spans history_item.json (the stored path) plus its
		// sibling ui_messages.json. The session watcher polls
		// SourceMtime, so this must stay stat-only — content hashing
		// is reserved for the sync fingerprint.
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		_, mtime := roocodeEffectiveStat(path, info)
		return mtime
	}
	if def.Type == parser.AgentCodebuff {
		// Freshness spans chat-messages.json plus run-state.json and
		// chat-meta.json. Reducing three files to a single max mtime is
		// lossy: a same-size companion-file rewrite whose mtime stays
		// below the existing max, or offsetting size changes that keep
		// the sum unchanged, would both leave SourceMtime stable and
		// stale metadata survive. Hash each component's (size, mtime,
		// ctime) triple plus directory metadata so any per-file change
		// in size, mtime, or ctime is detected by the watcher. The
		// triple matches the format used by ComputeMultiFileStatHash;
		// ctime is the reliable change signal a pure (size, mtime)
		// tuple lacks — a same-size rewrite with preserved mtime still
		// bumps ctime on Unix and change-time on Windows.
		dir := filepath.Dir(path)
		h := fnv.New64a()
		h.Write([]byte{0xCB})
		var buf [24]byte
		writeTuple := func(size, mtime, ctime int64) {
			binary.LittleEndian.PutUint64(buf[:8], uint64(size))
			binary.LittleEndian.PutUint64(buf[8:16], uint64(mtime))
			binary.LittleEndian.PutUint64(buf[16:24], uint64(ctime))
			_, _ = h.Write(buf[:])
		}
		// Primary file.
		ci, err := os.Stat(path)
		if err != nil {
			return 0
		}
		ctime, _ := fileChangeTime(path, ci)
		writeTuple(ci.Size(), ci.ModTime().UnixNano(), ctime)
		// Companion files (run-state.json, chat-meta.json).
		for _, name := range parser.CodebuffCompanionFilenames {
			companion := filepath.Join(dir, name)
			if ci, err := os.Stat(companion); err == nil {
				ctime, _ := fileChangeTime(companion, ci)
				writeTuple(ci.Size(), ci.ModTime().UnixNano(), ctime)
			} else {
				writeTuple(0, 0, 0)
			}
		}
		// Directory metadata so create/rename/delete is folded in.
		if di, err := os.Stat(dir); err == nil {
			ctime, _ := fileChangeTime(dir, di)
			writeTuple(0, di.ModTime().UnixNano(), ctime)
		} else {
			writeTuple(0, 0, 0)
		}
		// Despite SourceMtime's name and int64 type, this value is an
		// opaque change token, not a timestamp: reinterpreting the
		// FNV-1a sum can go negative and is non-monotonic across
		// changes. It is valid only for equality and zero/missing
		// comparison — never for ordering or arithmetic against real
		// mtimes.
		return int64(h.Sum64())
	}
	if def.Type == parser.AgentKiloLegacy {
		// Freshness spans task_metadata.json (the stored path) plus
		// its siblings ui_messages.json and api_conversation_history.json.
		// The session watcher polls SourceMtime, so this must stay
		// stat-only — content hashing is reserved for the sync fingerprint.
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		_, mtime := kiloLegacyEffectiveStat(path, info)
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

func applyProviderFingerprintFileInfo(
	agent parser.AgentType,
	fingerprint parser.SourceFingerprint,
	results []parser.ParseResultOutcome,
) {
	if agent != parser.AgentDevin && agent != parser.AgentHermes {
		return
	}
	for i := range results {
		if fingerprint.Size != 0 {
			results[i].Result.Session.File.Size = fingerprint.Size
		}
		if fingerprint.MTimeNS != 0 {
			results[i].Result.Session.File.Mtime = fingerprint.MTimeNS
		}
		if fingerprint.Hash != "" {
			results[i].Result.Session.File.Hash = fingerprint.Hash
		}
	}
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
	sessionsChanged := false
	// Defers run LIFO: unlock runs first (releasing syncMu), then
	// emit. Keep emission outside the critical section so a future
	// Emitter implementation can't widen the lock's scope.
	defer func() {
		if err == nil && !preserved {
			e.emit("messages")
		}
		if sessionsChanged {
			e.emit("sessions")
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
		// Forge, Piebald, Warp, and ZCode are DB-backed providers: re-sync routes
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
	if file.Machine == "" && !isS3SourcePath(file.Path) {
		file.Machine = e.machineForPath(file.Agent, file.Path)
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
	case parser.AgentQoder:
		for _, qoderDir := range e.agentDirs[parser.AgentQoder] {
			rel, ok := isUnder(qoderDir, path)
			if !ok {
				continue
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) == 2 || len(parts) == 4 && parts[2] == "subagents" {
				file.Project = parser.DecodeQoderProjectDir(parts[0])
				break
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

	preserved, sessionsChanged, err = e.processAndWriteSessionFile(
		ctx, file, sessionID,
	)
	return err
}

// processAndWriteSessionFile runs one discovered session source through
// processFile and commits the result, mirroring collectAndBatch's
// exclusion, tombstone, incremental, and full-write handling for a
// single file. It reports whether the write preserved an existing
// archived session instead of replacing it, so callers can skip their
// change event. The caller must hold syncMu.
func (e *Engine) processAndWriteSessionFile(
	ctx context.Context, file parser.DiscoveredFile, requestedSessionID string,
) (preserved bool, sessionsChanged bool, err error) {
	path := file.Path
	res := e.processFile(ctx, file)
	defer e.retentionBudget().scavengeIfNeeded()
	defer res.retentionLease.Release()
	if res.err != nil {
		sessionsChanged = res.sourceCwdChanged
		if res.cacheSkip && res.mtime != 0 && !res.noCacheSkip {
			e.cacheSkip(res.skipCacheKey(path), res.mtime, res.sourceFingerprint)
		}
		return false, sessionsChanged, res.err
	}
	if res.sourceCwdResolution.State != parser.SourceCwdUnspecified {
		changed, reconcileErr := e.reconcileFilteredSourceCwd(
			res.results,
			sourceCwdDecision{
				resolution: res.sourceCwdResolution,
				storedCwd:  res.sourceCwdStored,
				storedOK:   res.sourceCwdStoredOK,
			},
		)
		if reconcileErr != nil {
			return false, sessionsChanged, reconcileErr
		}
		sessionsChanged = sessionsChanged || changed
	}
	if res.skip {
		if res.sourceCwdResolution.State != parser.SourceCwdUnspecified {
			changed, reconcileErr := e.reconcileSourceCwdAtPath(
				res.sourceCwdPath,
				res.sourceCwdAgent,
				sourceCwdDecision{
					resolution: res.sourceCwdResolution,
					storedCwd:  res.sourceCwdStored,
					storedOK:   res.sourceCwdStoredOK,
				},
			)
			if reconcileErr != nil {
				return false, sessionsChanged, reconcileErr
			}
			sessionsChanged = sessionsChanged || changed
		}
		if err := e.reconcileSkippedSingleSessionSourceBaselines(
			ctx, file,
		); err != nil {
			return false, sessionsChanged, fmt.Errorf(
				"reconcile fresh source baselines: %w", err,
			)
		}
		if err := e.db.RepairQueuedSubagentParents(); err != nil {
			return false, sessionsChanged, fmt.Errorf(
				"repair queued subagent parents: %w", err,
			)
		}
		// A previous write may have stored a new spawn edge but failed
		// before its child could be durably queued. The requested session is
		// still a bounded repair seed on the freshness path because its
		// surviving edges identify those children directly.
		if err := e.db.LinkSubagentSessionsForSessions(
			[]string{requestedSessionID},
		); err != nil {
			return false, sessionsChanged, fmt.Errorf(
				"link fresh subagent sessions: %w", err,
			)
		}
		return false, sessionsChanged, nil
	}
	if res.cacheSkip {
		e.clearSkip(res.skipCacheKey(path))
	}

	sourceAllowsParserExclusions := e.sourceAllowsParserExclusions(res)

	// Capture the children this batch's PRE-write spawn edges reference.
	// The parser-exclusion delete below and the full message replacement
	// in the write loop both cascade tool_calls away, and the scoped
	// linker discovers children only through post-write edges — so a
	// child whose edge is about to be removed must be carried into the
	// linking batch explicitly, or it could never re-resolve to a
	// remaining spawner until the next bulk sync.
	excluded := e.applyIDPrefixToSessionIDs(res.excludedSessionIDs)
	resultIDs := make([]string, 0, len(res.results))
	for _, pr := range res.results {
		resultIDs = append(resultIDs, pr.Session.ID)
	}
	resultIDs = e.applyIDPrefixToSessionIDs(resultIDs)
	priorChildren, childErr := e.db.SubagentChildSessionIDs(
		append(append([]string{}, excluded...), resultIDs...),
	)
	if childErr != nil {
		return false, sessionsChanged, fmt.Errorf(
			"list pre-write subagent children: %w", childErr)
	}
	// A prior sync may have removed an edge and then failed before repairing
	// its child. Retry that durable work after this sync's read-only capture
	// but before making any new mutations.
	if err := e.db.RepairQueuedSubagentParents(); err != nil {
		return false, sessionsChanged, fmt.Errorf(
			"repair queued subagent parents: %w", err,
		)
	}
	if err := e.db.QueueSubagentParentCleanupRepairs(priorChildren); err != nil {
		return false, sessionsChanged, fmt.Errorf(
			"queue subagent parent repairs: %w", err,
		)
	}
	claudeDAG := file.Agent == parser.AgentClaude && len(res.results) > 1
	var sourceCompletionSkipped map[string]bool
	if claudeDAG {
		activeResultIDs, skipped :=
			e.partitionIntentionalSourceSkips(resultIDs)
		sourceCompletionSkipped = skipped
		staleVersion := max(db.CurrentDataVersion()-1, 0)
		if err := e.db.SetExistingSessionDataVersions(
			activeResultIDs, staleVersion,
		); err != nil {
			e.clearProviderSourceFreshness(ctx, res.providerStatHash)
			return false, sessionsChanged, fmt.Errorf(
				"stage Claude source data versions: %w", err,
			)
		}
	}
	// Always attempt queued work after mutations begin, including when a later
	// write or scoped link fails. Post-write capture below expands this flag
	// when the write introduces children that did not exist before it.
	repairQueued := len(priorChildren) > 0
	defer func() {
		if !repairQueued {
			return
		}
		if repairErr := e.db.RepairQueuedSubagentParents(); repairErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"repair queued subagent parents: %w", repairErr,
			))
		}
	}()
	queueWrittenChildren := func(spawnerIDs []string) error {
		children, childErr := e.db.SubagentChildSessionIDs(spawnerIDs)
		if childErr != nil {
			return fmt.Errorf("list post-write subagent children: %w", childErr)
		}
		if err := e.db.QueueSubagentParentRepairs(children); err != nil {
			return fmt.Errorf("queue post-write subagent parent repairs: %w", err)
		}
		if len(children) > 0 {
			repairQueued = true
		}
		return nil
	}

	// Delete parser-excluded sessions before writing the parsed
	// results, mirroring collectAndBatch. Vibe promotes a session
	// from its directory-name fallback ID to the canonical
	// meta.json ID and returns the stale fallback ID here; without
	// this delete a single-session resync would leave both rows in
	// the DB and double-count messages and usage. Like
	// collectAndBatch, exclusions from a source with no session
	// inside the cwd allow-list are frozen so archived rows survive.
	if _, err := e.deleteParserExcludedSessions(
		res, sourceAllowsParserExclusions,
	); err != nil {
		return false, sessionsChanged, fmt.Errorf(
			"delete parser-excluded sessions: %w", err,
		)
	}
	// A virtual member gone from a still-existing shared container is
	// tombstoned with its exact source ownership, mirroring
	// collectAndBatch, so a single-session resync preserves the archive
	// row as a revivable source-missing tombstone. The cwd-filter
	// freeze is judged per member against the archived cwd, matching
	// the batch path.
	if len(res.sourceMissingMembers) > 0 {
		var exactOwnerships []db.SessionSourceOwnership
		var rejectedExactOwnerships []db.SessionSourceOwnership
		tombstoned, _, err := e.reconcileSourceMissingMembers(
			ctx, file.Agent, res.sourceMissingMembers,
			func(ownership db.SessionSourceOwnership) {
				exactOwnerships = append(exactOwnerships, ownership)
			},
			func(ownership db.SessionSourceOwnership) {
				rejectedExactOwnerships = append(
					rejectedExactOwnerships, ownership,
				)
			},
		)
		sessionsChanged = sessionsChanged || tombstoned > 0
		baselineCtx := ctx
		if ctx.Err() != nil {
			baselineCtx = context.WithoutCancel(ctx)
		}
		baselineErr := e.replaceSessionSourceBaselineExceptionsByMachine(
			baselineCtx, exactOwnerships, rejectedExactOwnerships,
		)
		if baselineErr != nil {
			baselineErr = fmt.Errorf(
				"persist source-missing baseline exceptions: %w", baselineErr,
			)
		}
		if err != nil {
			return false, sessionsChanged, errors.Join(
				fmt.Errorf("reconcile source-missing members: %w", err),
				baselineErr,
			)
		}
		if baselineErr != nil {
			return false, sessionsChanged, baselineErr
		}
	}

	// Handle incremental updates from processFile (e.g.
	// append-only JSONL that was already synced).
	if res.incremental != nil {
		if err := e.writeIncremental(res.incremental); err != nil {
			return false, sessionsChanged, err
		}
		if err := queueWrittenChildren(
			[]string{res.incremental.sessionID},
		); err != nil {
			return false, sessionsChanged, err
		}
		if err := e.db.LinkSubagentSessionsForSessions(
			[]string{res.incremental.sessionID},
		); err != nil {
			return false, sessionsChanged, fmt.Errorf(
				"link incremental subagent sessions: %w", err)
		}
		return false, sessionsChanged, nil
	}

	if len(res.results) == 0 {
		return false, sessionsChanged, nil
	}

	sourceNeedsRetry := res.providerFailureCount > 0 ||
		len(res.retrySessionIDs) > 0
	resolved := 0
	writtenIDs := make([]string, 0, len(res.results))
	markSourceIncomplete := func() {
		if file.Agent == parser.AgentClaude {
			e.clearProviderSourceFreshness(ctx, res.providerStatHash)
		}
	}
	for i, pr := range res.results {
		sessionNeedsRetry := res.providerWideFailureCount > 0 ||
			res.needsRetryForSession(pr.Session.ID)
		write := pendingWrite{
			sess:                pr.Session,
			msgs:                pr.Messages,
			usageEvents:         pr.UsageEvents,
			needsRetry:          sessionNeedsRetry || claudeDAG,
			forceReplace:        res.forceReplace,
			sourceCwdResolution: res.sourceCwdResolution,
			sourceCwdStored:     res.sourceCwdStored,
			sourceCwdStoredOK:   res.sourceCwdStoredOK,
		}
		// The session upsert commits parser-derived parent provenance before
		// the later content, usage, and completion stages. Queue the attempted
		// session itself first so a failure after that upsert still re-resolves
		// its incoming spawn edges in the deferred repair pass.
		if err := e.db.QueueSubagentParentRepairs(
			[]string{resultIDs[i]},
		); err != nil {
			markSourceIncomplete()
			return false, sessionsChanged, fmt.Errorf(
				"queue attempted session parent repair: %w", err,
			)
		}
		repairQueued = true
		writeErr := e.writeSessionFull(write)
		memberPolicySkipped := sourceCompletionSkipped[resultIDs[i]]
		// Full-write stages commit independently. Message content (and a new
		// spawn edge) can persist even when a later usage, data-version, or
		// sibling write fails, so discover and queue children after every
		// attempt rather than waiting for the entire result set to finish.
		queueErr := queueWrittenChildren([]string{resultIDs[i]})
		if writeErr == nil {
			resolved++
			writtenIDs = append(writtenIDs, resultIDs[i])
		} else if isIntentionalSessionSkip(writeErr) || memberPolicySkipped {
			resolved++
		}
		if writeErr != nil &&
			!isIntentionalSessionSkip(writeErr) &&
			!memberPolicySkipped &&
			!errors.Is(writeErr, errSessionPreserved) {
			// Mirror the batch write paths: a partial write (session
			// row updated, messages or usage not) must demote the
			// stored data version, or the next container parse would
			// compare the member as unchanged and never repair it.
			e.markStaleFailedMemberWrite(write)
			if queueErr != nil {
				writeErr = errors.Join(writeErr, queueErr)
			}
			markSourceIncomplete()
			return false, sessionsChanged, fmt.Errorf("write session %s: %w",
				pr.Session.ID, writeErr)
		}
		if queueErr != nil {
			markSourceIncomplete()
			return false, sessionsChanged, queueErr
		}
		if !memberPolicySkipped && errors.Is(writeErr, errSessionPreserved) {
			preserved = true
		}
	}
	// A source-level digest is valid only when every active result and its
	// hierarchy links commit without retry state or archive preservation.
	// User-excluded and trashed members are resolved without writes; the other
	// Claude DAG members stay stale until the source-level decision succeeds.
	sourceComplete := resolved == len(res.results) &&
		!preserved && !sourceNeedsRetry
	if !sourceComplete {
		markSourceIncomplete()
	}
	if err := e.db.LinkSubagentSessionsForSessions(resultIDs); err != nil {
		markSourceIncomplete()
		return false, sessionsChanged, fmt.Errorf(
			"link changed subagent sessions: %w", err,
		)
	}
	if sourceComplete && claudeDAG {
		if err := e.db.SetSessionDataVersions(
			writtenIDs, db.CurrentDataVersion(),
		); err != nil {
			markSourceIncomplete()
			return false, sessionsChanged, fmt.Errorf(
				"complete Claude source data versions: %w", err,
			)
		}
	}
	if sourceComplete && res.providerStatHash != nil {
		e.recordProviderStatHash(ctx, *res.providerStatHash)
	}

	return preserved, sessionsChanged, nil
}

func (e *Engine) applyWorktreeMappingToSingleSession(
	sessionID string,
) (string, error) {
	ctx := context.Background()
	sess, err := e.db.GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if sess == nil {
		return "", fmt.Errorf(
			"apply worktree mapping to session %s: session disappeared",
			sessionID,
		)
	}

	machine := sess.Machine
	if machine == "" {
		machine = e.machine
	}
	updated, err := e.db.ApplyWorktreeProjectMappingToSessionFromSync(
		ctx, machine, sess.ID, sess.Cwd, sess.Project,
	)
	if err != nil {
		return "", fmt.Errorf(
			"apply worktree mapping to session %s: %w",
			sessionID, err,
		)
	}
	if !updated {
		return sess.Project, nil
	}
	mapped, err := e.db.GetSession(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf(
			"reload mapped session %s: %w",
			sessionID, err,
		)
	}
	if mapped == nil {
		return "", fmt.Errorf(
			"reload mapped session %s: session disappeared",
			sessionID,
		)
	}
	return mapped.Project, nil
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
	calls, _ := convertToolCallsContext(
		context.Background(), sessionID, parsed,
	)
	return calls
}

func convertToolCallsContext(
	ctx context.Context, sessionID string, parsed []parser.ParsedToolCall,
) ([]db.ToolCall, error) {
	if len(parsed) == 0 {
		return nil, ctx.Err()
	}
	calls := make([]db.ToolCall, len(parsed))
	for i, tc := range parsed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		filePath := tc.FilePath
		if filePath == "" {
			filePath = parser.ResolveFilePathFromJSON(tc.InputJSON)
		}
		resultEvents, err := convertToolResultEventsContext(ctx, tc.ResultEvents)
		if err != nil {
			return nil, err
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
			ResultEvents:      resultEvents,
		}
	}
	return calls, ctx.Err()
}

func convertToolResultEventsContext(
	ctx context.Context, parsed []parser.ParsedToolResultEvent,
) ([]db.ToolResultEvent, error) {
	if len(parsed) == 0 {
		return nil, ctx.Err()
	}
	events := make([]db.ToolResultEvent, len(parsed))
	for i, ev := range parsed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
	return events, ctx.Err()
}

// convertToolResults maps parsed tool results to db.ToolResult
// structs for use in pairing before DB insert.
func convertToolResultsContext(
	ctx context.Context, parsed []parser.ParsedToolResult,
) ([]db.ToolResult, error) {
	if len(parsed) == 0 {
		return nil, ctx.Err()
	}
	results := make([]db.ToolResult, len(parsed))
	for i, tr := range parsed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results[i] = db.ToolResult{
			ToolUseID:     tr.ToolUseID,
			ContentLength: tr.ContentLength,
			ContentRaw:    tr.ContentRaw,
		}
	}
	return results, ctx.Err()
}

// pairAndFilter pairs tool results with their corresponding
// tool calls, then removes user messages that carried only
// tool_result blocks (no displayable text).
func pairAndFilter(msgs []db.Message, blocked map[string]bool) []db.Message {
	filtered, _ := pairAndFilterContext(
		context.Background(), msgs, blocked,
	)
	return filtered
}

func pairAndFilterContext(
	ctx context.Context, msgs []db.Message, blocked map[string]bool,
) ([]db.Message, error) {
	if err := pairToolResultsContext(ctx, msgs, blocked); err != nil {
		return nil, err
	}
	if err := pairToolResultEventSummariesContext(
		ctx, msgs, blocked,
	); err != nil {
		return nil, err
	}
	filtered := msgs[:0]
	for _, m := range msgs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if m.Role == "user" &&
			len(m.ToolResults) > 0 &&
			strings.TrimSpace(m.Content) == "" {
			continue
		}
		filtered = append(filtered, m)
	}
	return filtered, ctx.Err()
}

// pairToolResults matches tool_result content to their
// corresponding tool_calls across message boundaries using
// tool_use_id. Categories in blocked are stored without content.
func pairToolResults(msgs []db.Message, blocked map[string]bool) {
	_ = pairToolResultsContext(context.Background(), msgs, blocked)
}

func pairToolResultsContext(
	ctx context.Context, msgs []db.Message, blocked map[string]bool,
) error {
	idx := make(map[string]*db.ToolCall)
	for i := range msgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		for j := range msgs[i].ToolCalls {
			if err := ctx.Err(); err != nil {
				return err
			}
			tc := &msgs[i].ToolCalls[j]
			if tc.ToolUseID != "" {
				idx[tc.ToolUseID] = tc
			}
		}
	}
	if len(idx) == 0 {
		return ctx.Err()
	}
	for _, m := range msgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, tr := range m.ToolResults {
			if err := ctx.Err(); err != nil {
				return err
			}
			if tc, ok := idx[tr.ToolUseID]; ok {
				tc.ResultContentLength = tr.ContentLength
				if !blocked[tc.Category] {
					tc.ResultContent = parser.DecodeContent(tr.ContentRaw)
				}
			}
		}
	}
	return ctx.Err()
}

func pairToolResultEventSummaries(
	msgs []db.Message, blocked map[string]bool,
) {
	_ = pairToolResultEventSummariesContext(
		context.Background(), msgs, blocked,
	)
}

func pairToolResultEventSummariesContext(
	ctx context.Context, msgs []db.Message, blocked map[string]bool,
) error {
	for i := range msgs {
		if err := ctx.Err(); err != nil {
			return err
		}
		for j := range msgs[i].ToolCalls {
			if err := ctx.Err(); err != nil {
				return err
			}
			tc := &msgs[i].ToolCalls[j]
			if len(tc.ResultEvents) == 0 {
				continue
			}
			summary, err := summarizeToolResultEventsContext(
				ctx, tc.ResultEvents,
			)
			if err != nil {
				return err
			}
			tc.ResultContentLength = len(summary)
			if blocked[tc.Category] {
				tc.ResultContent = ""
				for k := range tc.ResultEvents {
					tc.ResultEvents[k].Content = ""
				}
				continue
			}
			tc.ResultContent = summary
		}
	}
	return ctx.Err()
}

func summarizeToolResultEventsContext(
	ctx context.Context, events []db.ToolResultEvent,
) (string, error) {
	if len(events) == 0 {
		return "", ctx.Err()
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
		if err := ctx.Err(); err != nil {
			return "", err
		}
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
				return summary + "\n\n" + lastAnon, ctx.Err()
			}
			return summary, ctx.Err()
		}
		return lastAnon, ctx.Err()
	}
	parts := make([]string, 0, len(orderedAgents))
	for _, agentID := range orderedAgents {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		parts = append(parts, agentID+":\n"+latestByAgent[agentID].content)
	}
	if !allHaveAgentID && lastAnon != "" {
		parts = append(parts, lastAnon)
	}
	return strings.Join(parts, "\n\n"), ctx.Err()
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
		if ok {
			sum.Scanned++
			sum.TotalFindings += nf
			sum.DefiniteFindings += leak
			sum.CandidateFindings += nf - leak
			if leak > 0 {
				sum.WithSecrets++
			}
		}
		// A cancellation during the scan must end the run with an error,
		// not a partial success. This covers both a failed scan and a
		// successful final session whose context was canceled mid-scan,
		// since scanOneSession does CPU work and a non-context-aware
		// persist after its context-aware reads. That session is already
		// counted above: its findings are committed, and callers use the
		// summary to decide whether session eligibility changed.
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		if !ok {
			continue
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
