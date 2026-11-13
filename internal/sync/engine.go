package sync

import (
	"context"
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
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/lockctx"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/pathutil"
	"go.kenn.io/agentsview/internal/sync/failurecache"
	"go.kenn.io/kit/atomicfile"
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

const (
	finalizingSessionWritesDetail = "Finalizing sync: committing session writes"
	finalizingSourceStateDetail   = "Finalizing sync: saving session source state"
	finalizingFileLinksDetail     = "Finalizing sync: linking file-backed subagent sessions"
	finalizingParentRepairDetail  = "Finalizing sync: repairing subagent relationships"
	finalizingMemoryDetail        = "Finalizing sync: releasing parsed-session memory"
	finalizingDBBackedDetail      = "Finalizing sync: checking database-backed sessions"
	finalizingAllLinksDetail      = "Finalizing sync: linking all subagent sessions"
	finalizingSkipCacheDetail     = "Finalizing sync: saving the skip cache"
)

var errSessionPreserved = errors.New("session preserved")

type (
	reconciliationMetricsContextKey  struct{}
	reconciliationBaselineContextKey struct{}
	deferGlobalLinkContextKey        struct{}
	deferPassEpilogueContextKey      struct{}
)

// passEpilogueDeferred reports whether a grouped caller owns the pass
// epilogue — queued parent repairs, global linking, and skip-cache persistence —
// so a scoped reconciliation must not repeat that work itself.
func passEpilogueDeferred(ctx context.Context) bool {
	deferred, _ := ctx.Value(deferPassEpilogueContextKey{}).(bool)
	return deferred
}

// passEpilogueEligibility records, at the per-pass gate sites, whether a
// pass would have run queued repairs, global linking, and skip-cache persistence.
// Grouped callers consume it instead of inferring eligibility from the
// pass's final error: that error also reflects later tombstoning and spool
// cleanup failures, which never suppressed the per-pass epilogue and must
// not suppress the shared one.
type passEpilogueEligibility struct {
	repair  bool
	link    bool
	persist bool
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
	// ArchiveReparse permits only the verified materialized inputs supplied by
	// rawderive.PrepareLocalSource. It is not exposed in runtime configuration.
	ArchiveReparse bool
	// ArchiveSessionPolicy binds the native parser ID to the rewritten session
	// before any content is published. False preserves source deletion policy.
	// Used only by the local archive's scratch-database reparse.
	ArchiveSessionPolicy func(context.Context, *db.Session, string) (bool, error)
	// ArchiveWriteProvenance commits source ownership with normalized content.
	ArchiveWriteProvenance func(*db.Session, string) (db.ArchiveSessionProvenance, error)
	// ParserExclusionPolicy validates exclusions before any session metadata changes.
	ParserExclusionPolicy func(context.Context, []string) error

	AgentDirs      map[parser.AgentType][]string
	SourceMachines map[parser.AgentType]map[string]string
	// ProviderMetadata carries the resolved metadata directories used to
	// configure provider instances, including temporary remote imports.
	ProviderMetadata map[parser.AgentType]map[string][]string
	// DisabledAgents identifies providers omitted from this local filesystem
	// engine. Remote import engines leave it empty and import every transferred
	// provider.
	DisabledAgents          []parser.AgentType
	Machine                 string
	BlockedResultCategories []string
	// StagedCodexParseMinBytes overrides the full-parse size above which a
	// Codex source streams through the scratch staging path. Zero selects
	// the default (stagedCodexParseMinBytes). Tests lower it so the staged
	// wiring is covered by small fixtures in the default test build.
	StagedCodexParseMinBytes int64
	// CodexStagingDir selects the directory for staged Codex scratch
	// databases. Empty means the system temporary directory.
	CodexStagingDir string
	// ToolResultImages carries the configured retention policy into workers
	// that open the source archive read-only before building a replacement.
	ToolResultImages config.ToolResultImages
	AssetsDir        string
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
	// iCloud Drive, cloud-provider folders, and volumes under /Volumes). It
	// defaults to false so a first sync cannot raise consent prompts the
	// user cannot explain;
	// sessions there keep path-only project identity. Populated from the
	// scan_protected_paths config option. The safe default belongs to the
	// zero value so an engine built without the option never prompts.
	ScanProtectedPaths bool
	// IDPrefix is prepended to all session IDs. Used by
	// remote sync to namespace IDs by host (e.g. "host~").
	IDPrefix string
	// PathRewriter transforms file paths before storage.
	// Used by remote sync and archived sources to replace temporary paths
	// with stable references. Independent of IDPrefix.
	PathRewriter func(string) string
	// StoredPathResolver maps a canonical stored source path back to its
	// physical path under the current mirror. Remote changed-path planning uses
	// it to make persisted source hints usable by mirror-local providers.
	StoredPathResolver func(storedPath string) (physicalPath string, ok bool)
	// CompleteSourceMirror means resolved source paths cover the remote's
	// complete export. A missing resolved file can prove a source is gone.
	// Leave false for partial extracts; a full parse alone proves no coverage.
	CompleteSourceMirror bool
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
	// FrictionEnabled turns on Friction Log review. Sync never reviews a
	// session inline; it marks changed sessions stale, and BackfillFriction
	// and DrainStaleFrictionLocked review them later. Both are no-ops when
	// this is false.
	FrictionEnabled bool
	// DisableFilesystemProjectDiscovery prevents project attribution from
	// touching working directories recorded in imported transcripts. Bounded
	// capture uses lexical metadata instead.
	DisableFilesystemProjectDiscovery bool
	// StableSourceSnapshots reports that configured source files are immutable
	// for this engine. Bounded capture sets it after copying quiescent sources.
	StableSourceSnapshots bool
	// ArchiveContent tightens the database handle's storage policy before
	// the engine writes. The handle is the single authority afterwards; a
	// looser engine value never weakens a policy the handle already holds.
	ArchiveContent config.ArchiveContent
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
	// OnRebuildDeferred schedules retry work after a rebuild or startup pass.
	OnRebuildDeferred func(WatchBatch)
	// ProgressStallAfter controls when CurrentProgress marks an active pass as
	// stalled. Zero uses the production default. Tests and embedders may use a
	// shorter interval without changing the daemon-wide policy.
	ProgressStallAfter time.Duration
	// RebuildSources prepares configured non-filesystem sources inside the rebuild lock.
	RebuildSources func(context.Context) (RebuildOptions, error)
	// ProviderFactories and ProviderMigrationModes select which concrete
	// providers own discovery and parsing for their agents. Nil uses the
	// parser package registry/manifest.
	ProviderFactories      []parser.ProviderFactory
	ProviderMigrationModes map[parser.AgentType]parser.ProviderMigrationMode
}

// Engine orchestrates session file discovery and sync.
type Engine struct {
	sourceSyncErr  error
	rebuildSources func(context.Context) (RebuildOptions, error)

	db *db.DB
	// Rebuilds swap db temporarily; annotation writes must stay on the live archive.
	archive *db.DB
	stat    func(string) (os.FileInfo, error)
	lstat   func(string) (os.FileInfo, error)
	// archiveStore is the database holding previously archived
	// sessions for the preserve guards in prepareSessionWrite.
	// During a resync/rebuild it points at the original DB while
	// e.db points at the fresh one; nil means e.db is the archive.
	archiveStore db.Store
	// archiveRebuildIndex snapshots source ownership in the original archive
	// for one rebuild; nil outside a rebuild, where e.db is queried instead.
	archiveRebuildIndex *archiveRebuildIndex
	// sourceClaims maps a full session id to the file that took it before
	// its row is written, so a second file in the same pass derives its own.
	sourceClaimsMu    gosync.Mutex
	sourceClaims      map[string]string
	deferredSourceCwd *sourceCwdReconciliationBatch
	// deferredNativeTitles holds rebuild title changes for archived
	// sessions that reach the replacement only through the orphan copy.
	// Agents share it because every key carries its own agent prefix.
	deferredNativeTitles map[string]*string
	// Providers have distinct title-store paths, checked only while enabled.
	appliedNativeTitles map[string]appliedNativeTitleStore
	// sourceSet holds the provider set and session roots this engine
	// discovers from. ReconfigureSources replaces it as one snapshot.
	sourceSet atomic.Pointer[engineSources]
	// baseProviderFactories is the unfiltered, unconfigured provider list
	// that each source snapshot is built from.
	baseProviderFactories   []parser.ProviderFactory
	machine                 string
	blockedResultCategories map[string]bool
	// stagedCodexMin is the resolved full-parse size above which Codex
	// sources take the staged streaming path.
	stagedCodexMin int64
	// stagedCodexDir is the scratch directory for staged Codex parses.
	stagedCodexDir   string
	toolResultImages config.ToolResultImages
	cwdFilter        cwdPrefixFilter
	// scanProtectedPaths, homeDir, and goos gate passive probing of macOS
	// TCC-protected locations. homeDir is empty when the home directory
	// cannot be resolved, which disables the gate rather than guessing.
	// goos mirrors runtime.GOOS so the gate is testable off-darwin.
	scanProtectedPaths  bool
	homeDir             string
	goos                string
	subagentLinkPending bool                 // protected by syncMu; retains failed global linking
	trailerScanAttempts map[string]time.Time // protected by syncMu; includes failed scans
	syncMu              engineSyncLock       // serializes all sync operations
	mu                  gosync.RWMutex
	lastSync            time.Time
	lastSyncStats       SyncStats
	currentProgress     *Progress
	progressStallAfter  time.Duration
	// skipCache tracks paths that should be skipped on
	// subsequent syncs, keyed by path with the file mtime
	// at time of caching. Covers parse errors and
	// non-interactive sessions (nil result). The file is
	// retried when its mtime changes. S3 entries also keep an
	// in-memory source fingerprint when one is available.
	skipMu    gosync.RWMutex
	skipCache map[string]int64
	// failures remembers sources whose parse failed and will keep failing
	// until the file changes. It is separate from skipCache: a failure is
	// recorded by a pass that did not complete, and skipCache persists only
	// after a complete one.
	failures failurecache.Cache
	// skipCacheDirty is protected by skipMu and cleared only for a persistence attempt.
	skipCacheDirty   bool
	skipFingerprints map[string]string
	// piebaldFailureMemo suppresses repeated ordinary parse failures per virtual source.
	piebaldFailureMemo map[string]piebaldFailureMemoEntry
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
	// checkpointAudit, when set, bypasses the checkpoint stat-trust gate so
	// the provider's full-source fingerprint verifies content. The periodic
	// archive audit sets it to catch same-stat in-place rewrites that
	// append-trust would otherwise keep stale.
	checkpointAudit atomic.Bool
	// soleArchiveWriter permits a pass-start freshness snapshot while startup
	// holds the archive write lock and no other archive writer can run.
	soleArchiveWriter atomic.Bool
	// idPrefix and pathRewriter support remote sync:
	// prefix all session IDs to avoid collisions, rewrite
	// temp paths to "host:/remote/path" form.
	ephemeral               bool
	discardWritesOnCancel   bool
	disableSignalRecompute  bool
	disableProjectDiscovery bool
	stableSourceSnapshots   bool
	archiveSessionPolicy    func(context.Context, *db.Session, string) (bool, error)
	archiveWriteProvenance  func(*db.Session, string) (db.ArchiveSessionProvenance, error)
	parserExclusionPolicy   func(context.Context, []string) error
	idPrefix                string
	pathRewriter            func(string) string
	storedPathResolver      func(string) (string, bool)
	completeSourceMirror    bool
	emitter                 Emitter
	providerMigrationModes  map[parser.AgentType]parser.ProviderMigrationMode

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
	onRebuildDeferred       func(WatchBatch)
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
	// parseAdmissionObserver is a cardinality-test seam; result admission
	// reports results seen and still held for the current source.
	parseAdmissionObserver func(yielded, retained int)
	// changedPathListedHook is a test seam called after a changed-path
	// listing and before its re-list check; nil in production.
	changedPathListedHook func(path string)
	// incrementalCommittedHook is a test seam called after an incremental
	// session write commits; nil in production.
	incrementalCommittedHook func(sessionID string)
	// groupedReconcileGroupHook is a test seam called with syncMu held at the
	// start of each ReconcileProviderRootsGrouped group; nil in production.
	groupedReconcileGroupHook func(index int)
	// claudeProjectSessionFiles is an observability seam for cardinality tests
	// around duplicate-session discovery.
	claudeProjectSessionFiles func(string) []parser.DiscoveredFile
	parseRetentionOnce        gosync.Once
	parseRetentionBudget      *parseRetentionBudget
	bulkRetentionOnce         gosync.Once
	bulkRetentionBudget       *parseRetentionBudget
	// activeRetention points at the budget the in-flight pass admits parses
	// through. Bulk archive passes (full sync, resync rebuild, remote import)
	// install the byte-weighted bulk budget for their duration; when nil,
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
	signalSched        *signalScheduler
	frictionEnabled    bool
	frictionBackfillMu gosync.Mutex
	// frictionReviewHook replaces friction.Review in tests.
	frictionReviewHook func(friction.SessionInput) []friction.Signal

	// containerMu guards the OpenCode-family shared-SQLite freshness
	// gate (see opencode_container_gate.go). trustedSQLiteContainers
	// maps a container DB path to its state at the end of the last
	// completed pass; digestVerifiedAt records when that container last
	// completed a full composite listing. containerPass is the bookkeeping
	// for the pass currently running (nil outside passes). Persisted sync state
	// restores recent verification after a restart without renewing its age.
	containerMu             gosync.Mutex
	trustedSQLiteContainers map[string]trustedSQLiteContainer
	digestVerifiedAt        map[string]time.Time
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

	// contentChecks and contentCheckBytes count sources whose content a
	// freshness check or provider fingerprint read because their stats alone
	// could not prove them unchanged. Changed-path passes log the per-pass
	// delta, so a user can see when watcher events force those reads.
	contentChecks     atomic.Int64
	contentCheckBytes atomic.Int64

	reconciliationMu           gosync.RWMutex
	lastReconciliation         ReconciliationResult
	reconciliationSpoolFactory func(context.Context, string) (reconciliationSpoolStore, error)
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
	return e.sources().providerStatHashers[agent]
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
func NewEngine(ctx context.Context,
	database *db.DB, cfg EngineConfig,
) *Engine {
	database.SetArchiveContent(cfg.ArchiveContent)
	skipCache := make(map[string]int64)
	if !cfg.Ephemeral {
		if loaded, err := database.LoadSkippedFiles(ctx); err == nil {
			skipCache = loaded
		} else {
			log.Printf("loading skip cache: %v", err)
		}
		migrateLegacyCodexExecSkips(ctx, database, skipCache)
		migrateVisualStudioCopilotSkips(ctx, database, skipCache)
	}
	skipHashKeys, _ := normalizeSourceHashSkipCache(skipCache, nil)

	providerFactories := parser.ProviderFactories()
	if cfg.ProviderFactories != nil {
		providerFactories = append([]parser.ProviderFactory(nil), cfg.ProviderFactories...)
	}
	sourceSyncErr := database.RequireSourceSync(ctx)
	if errors.Is(sourceSyncErr, db.ErrArchiveOnly) && cfg.ArchiveReparse {
		sourceSyncErr = nil
	}
	if sourceSyncErr != nil {
		// No provider may discover default roots, even during a parser rebuild or
		// after source reconfiguration. Disabled providers retain their old rows.
		cfg.DisabledAgents = nil
		for _, factory := range providerFactories {
			cfg.DisabledAgents = append(cfg.DisabledAgents, factory.Definition().Type)
		}
		cfg.AgentDirs = nil
		cfg.SourceMachines = nil
		cfg.ProviderMetadata = nil
		cfg.DisableFilesystemProjectDiscovery = true
	}
	providerModes := parser.ProviderMigrationModes()
	if cfg.ProviderMigrationModes != nil {
		maps.Copy(providerModes, cfg.ProviderMigrationModes)
	}

	stagedCodexDir := cfg.CodexStagingDir
	// Ephemeral engines keep scratch files in the system temporary directory;
	// their archive directory may be a capture bundle with a fixed layout.
	if stagedCodexDir == "" && !cfg.Ephemeral {
		dbPath := database.Path()
		if dbPath != "" && dbPath != ":memory:" {
			stagedCodexDir = filepath.Join(
				filepath.Dir(dbPath), "scratch", "codex",
			)
		}
	}
	if stagedCodexDir != "" {
		if err := prepareCodexStagingDir(stagedCodexDir); err != nil {
			log.Printf("preparing codex staging directory: %v", err)
		}
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
	toolResultImages := database.ToolResultImages()
	if cfg.AssetsDir != "" {
		database.SetAssetsDir(cfg.AssetsDir)
	}
	if cfg.ToolResultImages == config.ToolResultImagesDrop || cfg.ToolResultImages == config.ToolResultImagesOffload {
		toolResultImages = cfg.ToolResultImages
	}
	e := &Engine{
		db:                      database,
		archive:                 database,
		stat:                    os.Stat,
		lstat:                   os.Lstat,
		baseProviderFactories:   providerFactories,
		machine:                 cfg.Machine,
		blockedResultCategories: blockedCategorySet(cfg.BlockedResultCategories),
		stagedCodexMin:          stagedCodexMinBytes(cfg.StagedCodexParseMinBytes),
		stagedCodexDir:          stagedCodexDir,
		toolResultImages:        toolResultImages,
		cwdFilter:               newCwdPrefixFilter(cfg.IncludeCwdPrefixes),
		scanProtectedPaths:      cfg.ScanProtectedPaths,
		homeDir:                 userHomeDirOrEmpty(),
		goos:                    runtime.GOOS,
		skipCache:               skipCache,
		skipCacheDirty:          true,
		skipFingerprints:        make(map[string]string),
		piebaldFailureMemo:      make(map[string]piebaldFailureMemoEntry),
		skipHashKeys:            skipHashKeys,
		s3CodexIndexCache:       make(map[string]s3CodexIndexSnapshot),
		ephemeral:               cfg.Ephemeral,
		discardWritesOnCancel:   cfg.DiscardPendingWritesOnCancel,
		disableSignalRecompute: cfg.DisableSignalRecomputation ||
			database.ArchiveContent().UsageOnly(),
		disableProjectDiscovery: cfg.DisableFilesystemProjectDiscovery,
		frictionEnabled:         cfg.FrictionEnabled,
		stableSourceSnapshots:   cfg.StableSourceSnapshots,
		idPrefix:                cfg.IDPrefix,
		archiveSessionPolicy:    cfg.ArchiveSessionPolicy,
		archiveWriteProvenance:  cfg.ArchiveWriteProvenance,
		rebuildSources:          cfg.RebuildSources,
		parserExclusionPolicy:   cfg.ParserExclusionPolicy,
		pathRewriter:            cfg.PathRewriter,
		storedPathResolver:      cfg.StoredPathResolver,
		completeSourceMirror:    cfg.CompleteSourceMirror,
		emitter:                 cfg.Emitter,
		providerMigrationModes:  providerModes,
		digestVerifiedAt:        make(map[string]time.Time),
		providerWatchRoots:      make(map[parser.AgentType][]parser.WatchRoot),
		projectIdentityCache:    make(map[string]projectIdentityCacheEntry),
		projectIdentityWritten:  make(map[string]struct{}),
		startupMaintenanceReady: make(chan struct{}),
		startupReconciledReady:  make(chan struct{}),
		startupAttemptReady:     make(chan struct{}),
		onStartupReconciled:     cfg.OnStartupReconciled,
		onRebuildDeferred:       cfg.OnRebuildDeferred,
		progressStallAfter:      progressStallAfter,
		reconciliationSpoolFactory: func(ctx context.Context, path string) (reconciliationSpoolStore, error) {
			return newReconciliationSpool(ctx, path)
		},
	}
	e.sourceSyncErr = sourceSyncErr
	e.sourceSet.Store(newEngineSources(providerFactories, SourceConfig{
		AgentDirs:        cfg.AgentDirs,
		SourceMachines:   cfg.SourceMachines,
		ProviderMetadata: cfg.ProviderMetadata,
		DisabledAgents:   cfg.DisabledAgents,
	}))
	if !cfg.Ephemeral {
		if err := e.failures.Load(ctx, database); err != nil {
			log.Printf("%v", err)
		}
	}
	if len(cfg.InitialSkipCache) > 0 {
		e.InjectSkipCache(cfg.InitialSkipCache)
	}
	if !cfg.DeferStartupMaintenance {
		e.ReleaseStartupMaintenance()
	}
	// A competing archive writer can invalidate a snapshot after it was
	// loaded. Keep failed work queued even if no later source change arrives.
	recompute := func(ctx context.Context, sessionID string) {
		if _, err := e.recomputeSignalsFromDB(ctx, sessionID); err != nil {
			log.Printf("signals: recompute %s: %v", sessionID, err)
			e.signalSched.deferRetry(sessionID)
		}
	}
	if e.disableSignalRecompute {
		recompute = func(context.Context, string) {}
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
	if e.disableSignalRecompute {
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
	for root, candidate := range e.sources().sourceMachines[agent] {
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
	configured := make([]string, 0, len(e.sources().sourceMachines[agent]))
	for root := range e.sources().sourceMachines[agent] {
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
				add(e.sources().sourceMachines[agent][cfg])
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
// allocated from the engine's configured factory and retained for the engine's
// lifetime. Metadata directories are immutable and belong to that factory.
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
	if _, ok := e.sources().providerStatHashers[hash.agent]; !ok {
		return
	}
	if !e.providerStatHashMetadataVerified(hash) {
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
func (e *Engine) providerStatHashMetadataVerified(hash pendingProviderStatHash) bool {
	if hash.agent != parser.AgentCodex {
		return true
	}
	if err := e.codexMetadata().Verify(hash.physicalPath); err != nil {
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
func (e *Engine) partitionIntentionalSourceSkips(ctx context.Context,
	ids []string,
) (active []string, skipped map[string]bool) {
	active = make([]string, 0, len(ids))
	for _, id := range ids {
		if e.db.IsSessionExcluded(ctx, id) || e.db.IsSessionTrashed(ctx, id) {
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
func migrateLegacyCodexExecSkips(ctx context.Context,
	database *db.DB, skipCache map[string]int64,
) {
	done, err := database.GetSyncState(ctx, codexExecMigrationKey)
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
		if err := database.ReplaceSkippedFiles(ctx,
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

	if err := database.SetSyncState(ctx,
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
func migrateVisualStudioCopilotSkips(ctx context.Context,
	database *db.DB, skipCache map[string]int64,
) {
	done, err := database.GetSyncState(ctx, visualStudioCopilotSkipMigrationKey)
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
		if err := database.ReplaceSkippedFiles(ctx, cleaned); err != nil {
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

	if err := database.SetSyncState(ctx,
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

func (e *Engine) reportFinalizingProgress(
	onProgress ProgressFunc, writeMode syncWriteMode, detail string,
) {
	if writeMode != syncWriteBulk {
		return
	}
	e.reportProgress(onProgress, Progress{
		Phase: PhaseFinalizing, Detail: detail,
	})
}

func (e *Engine) reportSubagentRepairProgress(onProgress ProgressFunc, done, total int) {
	e.reportProgress(onProgress, Progress{
		Phase: PhaseFinalizing, Detail: finalizingParentRepairDetail,
		SessionsDone: done, SessionsTotal: total,
	})
	if done == total {
		e.reportFinalizingProgress(onProgress, syncWriteBulk,
			"Finalizing sync: saving repaired subagent relationships")
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

// SetSoleArchiveWriter permits a pass-start snapshot while the startup worker
// owns the archive write lock and the daemon has not opened the archive.
func (e *Engine) SetSoleArchiveWriter(enabled bool) { e.soleArchiveWriter.Store(enabled) }

// SetCheckpointAudit enables or disables the checkpoint-bypassing content
// audit. When enabled, checkpointed sources are re-hashed with the provider's
// full-source fingerprint instead of being trusted on stat, so same-stat
// in-place rewrites are detected and repaired. The periodic archive audit
// toggles this around its reconciliation pass.
func (e *Engine) SetCheckpointAudit(enabled bool) {
	if e == nil {
		return
	}
	e.checkpointAudit.Store(enabled)
}

type syncJob struct {
	processResult
	agent          parser.AgentType
	path           string
	containerPath  string
	machine        string
	retentionLease *parseRetentionLease
}

func (j *syncJob) containerResultPath() string {
	if j.containerPath != "" {
		return j.containerPath
	}
	return j.path
}

const (
	reconciliationRetryPathLimit     = 256
	reconciliationRetryPathByteLimit = 64 * 1024
)

func reconciliationRetryPathBytes(paths []string) int {
	bytes := 0
	for _, path := range paths {
		bytes += len(path)
	}
	return bytes
}

func (j *syncJob) releaseRetention() {
	if j == nil {
		return
	}
	j.retentionLease.Release()
	j.retentionLease = nil
}

// releaseAll drops every parse-owned resource a discarded result holds: the
// retention lease bounds the parsed payload memory, and releaseStaged closes
// the scratch staging sink and restores the process GC target it lowered.
// Every collector discard path — including cancellation draining — must call
// this instead of releaseRetention alone, or a staged Codex result leaks its
// scratch database and pins the process GC percent at the staged value.
func (j *syncJob) releaseAll() {
	j.releaseRetention()
	j.releaseStaged()
}

func (j *syncJob) skipCacheKey() string {
	return j.processResult.skipCacheKey(j.path)
}

func (r *processResult) skipCacheKey(path string) string {
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
	// OpenCode virtual paths never exist as filesystem entries. Its changed-
	// path provider already checked that each returned SQLite member exists;
	// do not turn a successful single-member classification into a container-
	// wide absence scan. Missing members return no source and keep that scan.
	resolvedVirtual := make(map[string]struct{})
	for _, file := range prepared.files {
		if file.ProviderSource != nil && isOpenCodeFormatSQLiteVirtualPath(file.Agent, file.Path) {
			resolvedVirtual[filepath.Clean(file.Path)] = struct{}{}
		}
	}
	prepared.missingPaths = slices.DeleteFunc(prepared.missingPaths, func(path string) bool {
		path = filepath.Clean(path)
		// Closing a SQLite writer can remove its WAL/SHM files while the
		// container and every session remain. The pre-discovery capture
		// proves this is a known, existing container. Classify its contents
		// normally, but do not infer source loss from a vanished sidecar.
		for _, suffix := range []string{"-wal", "-shm"} {
			if container, ok := strings.CutSuffix(path, suffix); ok {
				if _, exists := prepared.preContainerStates[container]; exists {
					return true
				}
			}
		}
		_, resolved := resolvedVirtual[path]
		return resolved
	})
	return prepared
}

// syncChangedPathsLocked tracks changed-path preparation and application as
// one owned pass. The caller holds syncMu, so this progress cannot overwrite a
// different pass while waiting to enter the serialized sync path.
func (e *Engine) syncChangedPathsLocked(
	ctx context.Context, paths []string,
) (stats SyncStats, tombstoned int, retErr error) {
	ctx, closeProviderCache, err := e.withProviderFingerprintCache(ctx, paths)
	if err != nil {
		return SyncStats{Aborted: true}, 0, err
	}
	defer func() { retErr = errors.Join(retErr, closeProviderCache()) }()
	e.reportProgress(nil, Progress{
		Phase:  PhaseDiscovering,
		Detail: "Preparing changed session paths",
	})
	ctx = parser.WithProjectRootMemo(ctx)
	ctx = parser.WithCodexParentInventoryBatch(ctx)
	e.invalidateNativeTitleCache(paths)
	summary := e.beginChangedPathPass(paths)
	prepared := e.prepareChangedPathSync(ctx, paths)
	summary.sources = len(prepared.files)
	defer func() { log.Print(summary.finish(e, stats)) }()
	stats, tombstoned, err = e.applyChangedPathSyncLocked(ctx, prepared)
	// Title store events carry no session files, so this also handles them.
	if err == nil && !stats.Aborted {
		var titleStats SyncStats
		e.reconcileNativeTitlesLocked(ctx, &titleStats)
		e.recordNativeTitleUpdates(titleStats)
		stats.TitlesUpdated += titleStats.TitlesUpdated
		stats.Failed += titleStats.Failed
	}
	return stats, tombstoned, err
}

// providerFingerprintCacheRootEligible reports whether at least one of a
// provider's configured roots is eligible to receive the batch fingerprint
// cache. A flat statExists works for most BatchFingerprintCache providers,
// but Hermes and Augure Desktop can be configured with a "sessions"
// directory that has no reason to exist on disk yet (no transcript has ever
// been written) while its sibling state.db, the thing that actually makes
// the cache useful, already does. Use the same resolution discovery does
// for those two agents instead of asking whether the bare root path exists.
func providerFingerprintCacheRootEligible(
	agent parser.AgentType, roots []string,
) bool {
	if agent == parser.AgentHermes || agent == parser.AgentAugureDesktop {
		return slices.ContainsFunc(roots, parser.HermesRootResolvesToStateDB)
	}
	return slices.ContainsFunc(roots, statExists)
}

func (e *Engine) withProviderFingerprintCache(
	ctx context.Context, paths []string,
) (context.Context, func() error, error) {
	sources := e.sources()
	for agent, roots := range sources.agentDirs {
		factory := sources.providerFactories[agent]
		if factory == nil || !factory.Capabilities().Sync.BatchFingerprintCache ||
			!providerFingerprintCacheRootEligible(agent, roots) {
			continue
		}
		if len(paths) == 0 {
			return parser.WithReconciliationCache(ctx)
		}
		provider := factory.NewProvider(parser.ProviderConfig{Roots: roots})
		plan, err := provider.WatchPlan(ctx)
		if err != nil {
			return ctx, nil, err
		}
		for _, path := range paths {
			for _, root := range plan.Roots {
				if changedPathWithinAnyRoot(path, []string{root.Path}) {
					return parser.WithReconciliationCache(ctx)
				}
			}
		}
	}
	return ctx, func() error { return nil }, nil
}

func (e *Engine) applyChangedPathSyncLocked(
	ctx context.Context, prepared preparedChangedPathSync,
) (SyncStats, int, error) {
	if prepared.empty() {
		if prepared.classificationErr == nil {
			return SyncStats{}, 0, nil
		}
		// Unclassified changes may hide member edits, so withdraw authority.
		revokeErr := e.revokeSQLiteContainerVerification(ctx, prepared.preContainerStates)
		return SyncStats{}, 0, errors.Join(prepared.classificationErr, revokeErr)
	}
	e.resetS3CodexIndexCache()
	e.resetSourceClaims()
	e.anomalies.reset()
	// Begin a container pass so an already-trusted, unchanged container
	// still gates its fan-out, but never promote from a changed-path subset.
	if err := e.prepareSQLiteContainerVerification(ctx, prepared.preContainerStates); err != nil {
		return SyncStats{Aborted: true}, 0, err
	}
	e.beginSQLiteContainerPass(prepared.files, prepared.preContainerStates)
	e.reportProgress(nil, Progress{
		Phase:         PhaseSyncing,
		Detail:        "Syncing changed session paths",
		SessionsTotal: len(prepared.files),
	})
	processingCtx := context.WithValue(ctx, deferGlobalLinkContextKey{}, true)
	results := e.startWorkers(processingCtx, prepared.files)
	affectedSessionIDs := make(changedSessionLinks)
	stats := e.collectAndBatchWithOptions(
		processingCtx, results, len(prepared.files), len(prepared.files), nil,
		syncWriteDefault, collectAndBatchOptions{
			observeResult: func(job syncJob) {
				affectedSessionIDs.observe(job, e.idPrefix)
			},
		},
	)
	inventoryErr := e.finishCodexParentInventoryBatch(processingCtx, &stats)
	stats.logRetrySummary("sync")
	linkErr := affectedSessionIDs.link(ctx, e, &stats)
	if linkErr != nil {
		stats.RecordFailed()
	}
	e.anomalies.applyTo(&stats)
	complete := prepared.classificationErr == nil && ctx.Err() == nil &&
		stats.ProcessingComplete()
	tombstoned := 0
	var tombstoneErr error
	if complete && len(prepared.missingPaths) > 0 {
		tombstoned, tombstoneErr = e.tombstoneMissingWatchSourcesLocked(
			ctx, prepared.missingPaths, nil,
		)
	}
	if ctx.Err() == nil {
		e.persistSkipCache(ctx)
	}
	// The pass stays open through tombstoning so a late pass-level failure
	// still reaches finalization. Such failures cannot be attributed to a
	// container, so they poison the whole capture; a clean subset keeps its
	// verification age and, being partial, never promotes.
	if !complete || tombstoneErr != nil {
		e.poisonSQLiteContainerPass()
	}
	e.finishSQLiteContainerPass(ctx, true, false)
	if tombstoneErr != nil {
		return stats, tombstoned, fmt.Errorf(
			"watcher source reconciliation: %w", tombstoneErr,
		)
	}
	e.mu.Lock()
	e.lastSync = time.Now()
	e.lastSyncStats = stats
	e.mu.Unlock()
	if stats.Synced > 0 {
		log.Printf("sync: %d file(s) updated", stats.Synced)
	}
	if err := errors.Join(prepared.classificationErr, inventoryErr, linkErr, ctx.Err()); err != nil {
		if stats.Deferred > 0 {
			return stats, tombstoned, &incompleteReconciliationError{
				deferred:    stats.Deferred,
				paths:       append([]string(nil), stats.deferredRetryPaths...),
				overflow:    stats.deferredRetryOverflow,
				cause:       err,
				retryCauses: stats.retryCauseSample(),
			}
		}
		return stats, tombstoned, err
	}
	if !complete {
		incompleteErr := fmt.Errorf(
			"changed-path sync incomplete: %d source or archive failures",
			stats.Failed,
		)
		if stats.Deferred > 0 {
			incompleteErr = &incompleteReconciliationError{
				deferred:    stats.Deferred,
				paths:       append([]string(nil), stats.deferredRetryPaths...),
				overflow:    stats.deferredRetryOverflow,
				cause:       incompleteErr,
				deferOnly:   stats.Failed == 0 && stats.providerFailures == 0 && !stats.Aborted,
				retryCauses: stats.retryCauseSample(),
			}
		}
		return stats, tombstoned, incompleteErr
	}
	return stats, tombstoned, nil
}

// SyncPathsContext is SyncPaths with caller-controlled cancellation. The
// parsePolicyContext applies engine-level parse policies to ctx. A
// discovery-disabled engine must carry the policy on every context that
// reaches provider parsing -- full syncs, streamed reconciliation, changed
// paths, single-session refreshes, source lookups, and parse diffs -- so a
// working directory recorded in a transcript is never probed on the local
// host regardless of which entry point triggered the parse.
func (e *Engine) parsePolicyContext(ctx context.Context) context.Context {
	if e.disableProjectDiscovery {
		return parser.WithoutFilesystemProjectDiscovery(ctx)
	}
	return ctx
}

// file watcher threads the serve shutdown context through here.
func (e *Engine) SyncPathsContext(ctx context.Context, paths []string) error {
	return e.syncPathsContext(ctx, paths, false)
}

// ReparsePathsContext fully parses only the selected sources, retaining the
// normal publication rules and bounded-memory staging for large Codex files.
func (e *Engine) ReparsePathsContext(ctx context.Context, paths []string) error {
	return e.syncPathsContext(ctx, paths, true)
}

func (e *Engine) syncPathsContext(ctx context.Context, paths []string, force bool) error {
	return e.syncPathsWithLockContext(ctx, paths, force, false)
}

func (e *Engine) syncPathsWithLockContext(ctx context.Context, paths []string, force, try bool) error {
	if e.sourceSyncErr != nil {
		return e.sourceSyncErr
	}
	if e.refuseWriteInForceParse("SyncPaths") {
		return nil
	}
	ctx = e.parsePolicyContext(ctx)
	stats, tombstoned, err := func() (SyncStats, int, error) {
		if err := e.lockSync(try); err != nil {
			return SyncStats{}, 0, err
		}
		defer e.syncMu.Unlock()
		defer e.clearCurrentProgress()
		previous := e.forceFullParse
		e.forceFullParse = previous || force
		defer func() { e.forceFullParse = previous }()
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
		dfs := e.classifyCodexIndexPath(ctx, p)
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
	return e.dedupeClaudeDiscoveredFiles(ctx, files), classificationErr
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

	agents := make([]parser.AgentType, 0, len(e.sources().providerFactories))
	for agent := range e.sources().providerFactories {
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
		roots := e.sources().agentDirs[agentType]
		if len(roots) == 0 {
			continue
		}
		factory, ok := e.sources().providerFactories[agentType]
		if !ok || factory == nil {
			continue
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots:          roots,
			Machine:        e.machine,
			SourceMachines: e.sources().sourceMachines[agentType],
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
		watermarkSingleSnapshot := false
		watermarkContainer := openCodeContainerPathForChangedPathEvent(
			agentType, roots, path,
		)
		if watermarkContainer == "" {
			watermarkContainer, _ = parser.ResolveStoredMemberFreshnessContainer(
				provider, path,
			)
			watermarkSingleSnapshot = watermarkContainer != ""
		}
		var watermarkPreState parser.SQLiteContainerState
		watermarkPreStateOK := false
		if watermarkContainer != "" {
			watermarkPreState, watermarkPreStateOK = statSQLiteContainerState(watermarkContainer)
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
				request.StoredMemberFreshnessPage = e.storedMemberFreshnessPager(watermarkContainer, def.IDPrefix, nil)
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
			if e.changedPathListedHook != nil {
				e.changedPathListedHook(path)
			}
			// A single-snapshot listing needs no re-list; a later commit arrives as its own watcher event.
			if err == nil && request.StoredMemberFreshnessPage != nil && !watermarkSingleSnapshot {
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
				_, incomplete := errors.AsType[parser.DiscoveryIncompleteError](err)
				if !incomplete || ctx.Err() != nil || !provider.Capabilities().Sync.PartialDiscovery {
					continue
				}
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
	// Copilot companion events compare per-session hashes before parsing.
	if agent == parser.AgentCopilot && filepath.Clean(sourcePath) != filepath.Clean(eventPath) {
		return false
	}
	if mode != parser.ProviderMigrationProviderAuthoritative {
		return true
	}
	// Grok requires a matching content fingerprint for all companion files.
	// Ordinary watcher events must not clear its failed-source retry cache.
	if agent == parser.AgentGrok {
		return false
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
		// A Hermes/Augure Desktop state.db event's orphan-transcript
		// fallback surfaces every transcript whose session currently has
		// no row, including one that was never a member and so is
		// unrelated to this event. Its plain transcript path (not a
		// state.db#<id> member) isn't backed by the event, so without this
		// carve-out it would force-reparse and rewrite every
		// always-standalone sibling on every unrelated state.db write.
		// Let the normal freshness gates decide, the same way full sync
		// does for the identical source; keep the force on a remove
		// event so a deletion still re-emits.
		if eventKind != "remove" &&
			(agent == parser.AgentHermes || agent == parser.AgentAugureDesktop) {
			// A "#" in a parent directory name does not make a transcript
			// path a member.
			if _, _, member := parser.ParseVirtualSourcePathForBase(sourcePath, "state.db"); !member {
				return false
			}
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

// providerPersistentSharedContainerSource reports whether source addresses a
// whole shared-database container whose archive rows are all virtual members:
// Omnigent's chat.db or Cursor IDE's state.vscdb. Such containers have no
// stored row under their own physical path, so skip-cache identity carries
// the container hash and parser data version instead of a stored-row check,
// and container parses are cached only after their member writes commit.
func providerPersistentSharedContainerSource(source parser.SourceRef) bool {
	return parser.IsOmnigentContainerSource(source) ||
		parser.IsCursorIDEContainerSource(source)
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
// passAbortCause names why a sync pass stopped: cancellation, or the cause the
// pass recorded when it set Aborted.
func passAbortCause(ctx context.Context, stats SyncStats) string {
	if err := ctx.Err(); err != nil {
		return "canceled: " + err.Error()
	}
	if stats.abortCause != "" {
		return stats.abortCause
	}
	return "the sync stopped without saying why"
}

// keptArchive leads every cause for a rebuild that the swap safety check
// refused, so the reader knows the existing archive is unchanged.
const keptArchive = "kept the existing archive: "

// contributorAbortCause names why an attached archive source stopped a
// rebuild. A contributor that finished with deferred sources, such as a Codex
// fork whose parent rollout is missing, did not complete processing.
func contributorAbortCause(
	ctx context.Context, contributorErr error,
	contributorStats, stats SyncStats, safetyReason string,
) string {
	switch {
	case ctx.Err() != nil:
		return "canceled: " + ctx.Err().Error()
	case contributorErr != nil:
		return contributorErr.Error()
	case safetyReason != "":
		return keptArchive + safetyReason
	case contributorStats.Aborted:
		return passAbortCause(ctx, contributorStats)
	case !contributorStats.ProcessingComplete():
		return contributorStats.IncompleteCause()
	default:
		return passAbortCause(ctx, stats)
	}
}

// resyncSwapAbortReason decides whether a finished resync pass built a
// database that would be worse than the original, so the swap must be
// abandoned, and returns why. It returns "" when the swap may proceed. It
// aborts when:
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
func resyncSwapAbortReason(
	stats SyncStats, oldFileSessions, trashedCopied int,
) string {
	emptyDiscovery := stats.nonContainerDiscovered == 0 &&
		oldFileSessions > 0
	preservedOnly := stats.Synced == 0 &&
		stats.TotalSessions > 0 &&
		stats.Failed == 0 &&
		(oldFileSessions == 0 || trashedCopied > 0)
	// Deferred Codex files and preserved Antigravity CLI fallbacks can be mixed
	// with files intentionally filtered or excluded. Every successful file must
	// still be accounted for, and the only failures allowed are the preserved
	// fallbacks, whose saved rows the orphan copy restores.
	deferredOnly := stats.Synced == 0 && stats.Failed == stats.preservedRetryFailures &&
		stats.filesOK > 0 && stats.TotalSessions == stats.filesOK &&
		len(stats.deferredPreservedIDs) > 0 &&
		len(stats.deferredPreservedIDs)+stats.cwdFilteredFiles+stats.parserExcludedFiles == stats.filesOK
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
	switch {
	case stats.Aborted:
		if stats.abortCause != "" {
			return "the rebuild stopped early: " + stats.abortCause
		}
		return "the rebuild stopped early"
	case emptyDiscovery:
		return fmt.Sprintf(
			"the rebuild found no session files, but the archive has %s from files",
			countNoun(oldFileSessions, "session", "sessions"),
		)
	case stats.Synced == 0 &&
		stats.TotalSessions > 0 &&
		!preservedOnly &&
		!deferredOnly &&
		!excludedOnly &&
		!cwdFilteredOnly:
		reason := fmt.Sprintf(
			"the rebuild found %s but saved none (%d could not be read, %d wait for a retry)",
			countNoun(stats.TotalSessions, "session", "sessions"), stats.Failed, stats.Deferred,
		)
		if sample := stats.retryCauseSample(); sample != "" {
			reason += "; why: " + sample
		}
		return reason
	case stats.Failed > 0 && stats.Failed > stats.filesOK:
		return fmt.Sprintf(
			"%d session files could not be read, more than the %d that could",
			stats.Failed, stats.filesOK,
		)
	}
	return ""
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
	if err := ctx.Err(); err != nil {
		cause := "canceled: " + err.Error()
		stats = SyncStats{Aborted: true, Warnings: []string{"resync aborted: 0 synced, 0 failed: " + cause}, abortCause: cause}
		e.mu.Lock()
		e.lastSyncStats = stats
		e.mu.Unlock()
		return stats, err
	}
	ops = ops.withDefaults()

	// Hold the write barrier for the whole in-process build-and-swap window. The
	// build reads the original while it stays open, and the swap's
	// CloseConnections runs only after the copies, so without the barrier a
	// direct write (star/delete/restore/annotation) that bypasses syncMu could
	// land in the original between the copies and the rename and be discarded
	// by the swap.
	// The barrier rejects such writes with ErrWriterClosed instead. The worker
	// arm holds this barrier at the daemon layer (runWorkerResyncBuild) and calls
	// ResyncBuild directly, so it never reaches this method; this arm covers the
	// in-process fallback, CLI --full, worker startup, and unified rebuilds. Only
	// engage it when this call owns the barrier, so an outer owner is not
	// double-closed or prematurely reopened.
	ownedBarrier := false
	swapStageReached := false
	// Registered first so it runs after the recovery below reopens the writer.
	// A build that was discarded flushed its source failures only into the
	// replacement; this makes them survive a restart. It writes nothing after
	// a successful swap, which already reloaded the cache from the new archive.
	defer func() {
		if ctx.Err() == nil {
			e.flushFailureCache(ctx)
		}
	}()
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

	preBuildLinkPending := e.subagentLinkPending
	installed := false
	defer func() {
		// Linking in the replacement does not repair the live archive until
		// the swap installs it. Keep the live retry on every discarded build.
		if !installed {
			e.subagentLinkPending = preBuildLinkPending
		}
	}()

	stats, err := e.resyncBuildLocked(ctx, onProgress, opts, ops, ownedBarrier)
	if err != nil || stats.Aborted {
		return stats, err
	}
	swapStageReached = true
	var swapErr error
	installed, swapErr = e.swapResyncDatabaseLocked(
		ctx, onProgress, e.ResyncTempPath(), ops, &stats,
	)
	if swapErr != nil {
		if !installed {
			// The original archive is still in place; drop the discarded
			// replacement's skip entries and its change counts. A
			// post-install failure keeps both: the replacement is the live
			// archive there.
			e.skipMu.Lock()
			e.skipCache = preBuildSkipCache
			e.skipCacheDirty = true
			e.skipHashKeys = preBuildSkipHashKeys
			e.skipMu.Unlock()
			stats.Tombstoned = 0
			stats.LinksUpdated = 0
		}
		e.setLastSyncStats(stats)
		return stats, swapErr
	}
	if err := e.ResetCachesAfterSwap(ctx); err != nil {
		log.Printf("resync: reset caches after swap: %v", err)
	}
	e.NotifyRebuildDeferred(stats)
	e.setLastSyncStats(stats)
	return stats, nil
}

// NotifyRebuildDeferred hands installed rebuild retry work to the watcher.
func (e *Engine) NotifyRebuildDeferred(stats SyncStats) {
	if stats.ArchiveRebuilt && stats.Deferred > 0 && e.onRebuildDeferred != nil {
		e.onRebuildDeferred(stats.DeferredRetryBatch())
	}
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
	if err := origDB.CloseConnections(ctx); err != nil {
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

	if err := atomicfile.Replace(tempPath, origPath); err != nil {
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
	ctx context.Context, tempPath string,
) (installed bool, err error) {
	var stats SyncStats
	return e.swapResyncDatabaseLocked(
		ctx, nil, tempPath, productionRebuildOperations, &stats,
	)
}

// ResetCachesAfterSwap re-baselines every parent-side cache keyed to the
// replaced database: it clears the trusted SQLite containers, trusted OpenCode
// storage sessions, and verified sources, then reloads the skip cache from the
// swapped archive so the engine carries warm skip state. skipFingerprints is
// reset to empty, matching engine construction (fingerprints are recomputed on
// the next sync, never persisted). The caller invokes it immediately after a
// successful SwapResyncDatabase.
func (e *Engine) ResetCachesAfterSwap(ctx context.Context) error {
	e.clearTrustedSQLiteContainers()
	e.clearTrustedOpenCodeStorageSessions()
	e.clearVerifiedSources()
	e.clearPiebaldFailureMemo()
	return e.ReloadSkipCache(ctx)
}

// ReloadSkipCache replaces the in-memory skip state with the durable archive
// state. Worker-backed passes call it after reacquiring write ownership because
// the child may have removed cache entries while tombstoning missing sources.
// The caller must serialize this with other engine sync work.
func (e *Engine) ReloadSkipCache(ctx context.Context) error {
	skipCache := make(map[string]int64)
	if !e.ephemeral {
		loaded, err := e.db.LoadSkippedFiles(ctx)
		if err != nil {
			return fmt.Errorf("reloading skip cache after swap: %w", err)
		}
		skipCache = loaded
		if err := e.failures.Load(ctx, e.db); err != nil {
			return fmt.Errorf("reloading failure cache: %w", err)
		}
	}
	skipHashKeys, _ := normalizeSourceHashSkipCache(skipCache, nil)

	e.skipMu.Lock()
	e.skipCache = skipCache
	e.skipCacheDirty = true
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

func countRootSessionsForAgent(ctx context.Context,
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
	err := database.Reader().QueryRow(ctx, `
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

func countIcodemateContainerRootSessions(ctx context.Context,
	database *db.DB, machine, idPrefix string, scoped bool,
) int {
	machinePredicate := ""
	args := []any{string(parser.AgentIcodemate)}
	if scoped {
		machinePredicate = " AND machine = ?"
		args = append(args, machine)
	}
	if idPrefix != "" {
		machinePredicate += " AND substr(id, 1, length(?)) = ?"
		args = append(args, idPrefix, idPrefix)
	}
	var count int
	err := database.Reader().QueryRow(ctx, `
		SELECT COUNT(*) FROM sessions
		WHERE agent = ?
		  AND file_path NOT LIKE '%.jsonl'
		  AND message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL
	`+machinePredicate, args...).Scan(&count)
	if err != nil {
		log.Printf("count root ICodeMate container sessions: %v", err)
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

func (e *Engine) protectedFileSessionCount(ctx context.Context,
	database *db.DB, machine, idPrefix string, scoped bool,
) (int, error) {
	return protectedFileSessionCount(ctx,
		database, machine, idPrefix, scoped, e.sources().preserveAgents,
	)
}

func protectedFileSessionCount(ctx context.Context,
	database *db.DB, machine, idPrefix string, scoped bool,
	preserveAgents []parser.AgentType,
) (int, error) {
	var count int
	var err error
	if scoped {
		if idPrefix != "" {
			count, err = database.FileBackedSessionCountForSource(
				ctx, machine, idPrefix,
			)
		} else {
			count, err = database.FileBackedSessionCountForMachine(
				ctx, machine,
			)
		}
	} else {
		count, err = database.FileBackedSessionCount(ctx)
	}
	if err != nil {
		return 0, err
	}
	for _, agent := range []parser.AgentType{
		parser.AgentOpenCode,
		parser.AgentKilo,
		parser.AgentMiMoCode,
	} {
		count -= countRootSessionsForAgent(ctx,
			database, agent, machine, idPrefix, scoped,
		)
	}
	storageAgents := storageAgentsForDisabledProviders(preserveAgents)
	if slices.Contains(storageAgents, parser.AgentIcodemate) {
		// A disabled ICodeMate provider discovers nothing, so its CLI JSONL
		// rows are preserved rather than rediscovered: subtract every
		// ICodeMate row, not just the self-preserving container layouts.
		count -= countRootSessionsForAgent(ctx,
			database, parser.AgentIcodemate, machine, idPrefix, scoped,
		)
	} else {
		count -= countIcodemateContainerRootSessions(ctx,
			database, machine, idPrefix, scoped,
		)
	}
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
		count -= countRootSessionsForAgent(ctx,
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
func (e *Engine) LastSyncStartedAt(ctx context.Context) time.Time {
	raw, err := e.db.GetSyncState(ctx, syncStateStartedAt)
	if err != nil || raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (e *Engine) lockSyncWithProgress(onProgress ProgressFunc) {
	if e.syncMu.TryLock() {
		return
	}
	// Only notify this caller: the lock owner still owns CurrentProgress.
	if onProgress != nil {
		onProgress(Progress{
			Phase: PhaseSyncing, Detail: "Waiting for the current sync to finish",
		})
	}
	e.syncMu.Lock()
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
	if e.sourceSyncErr != nil {
		return SyncStats{Aborted: true}, e.sourceSyncErr
	}
	if e.refuseWriteInForceParse("SyncThenRun") {
		return SyncStats{}, nil
	}
	e.lockSyncWithProgress(onProgress)
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
	if err == nil && (stats.Deferred == 0 || stats.ArchiveRebuilt) {
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
	if !stats.ProcessingComplete() {
		return stats, nil
	}
	// work typically scans and pushes SQLite rows, so flush any
	// deferred signal recomputes first (inline: syncMu is held) or
	// pushed sessions could carry stale signal/secret fields.
	e.signalSched.flushAllInline(ctx)
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
	if e.sourceSyncErr != nil {
		return SyncStats{Aborted: true}, e.sourceSyncErr
	}
	if e.refuseWriteInForceParse("SyncThenRunWithRebuild") {
		return SyncStats{}, nil
	}
	e.lockSyncWithProgress(onProgress)
	defer e.notifyStartupReconciled()
	defer func() {
		if stats.shouldEmitSync() {
			e.emit("sync")
		}
	}()
	defer e.syncMu.Unlock()
	defer func() {
		if retErr == nil && (stats.Deferred == 0 || stats.ArchiveRebuilt) {
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
	if !stats.ProcessingComplete() {
		return stats, nil
	}
	e.signalSched.flushAllInline(ctx)
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
	if !e.StartupReconciled() && !stats.ArchiveRebuilt && stats.Deferred > 0 &&
		err == nil && stats.Failed == 0 && stats.AuthoritativeDiscoveryComplete() && e.onRebuildDeferred != nil {
		e.onRebuildDeferred(stats.DeferredRetryBatch())
		stats.deferredRetryScheduled = true
		e.ReleaseStartupMaintenance()
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
		} else if stats.Deferred > 0 && !stats.ArchiveRebuilt && !stats.deferredRetryScheduled {
			e.startupReconciledErr = fmt.Errorf(
				"startup processing incomplete: %d deferred, %d failed",
				stats.Deferred, stats.Failed,
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
		stats.AuthoritativeDiscoveryComplete() && (stats.Deferred == 0 || stats.ArchiveRebuilt || stats.deferredRetryScheduled)
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
	e.syncMu.Lock()
	e.RecordStartupReconciledExclusive(stats, err)
	e.syncMu.Unlock()
	e.FinishStartupReconciled(stats)
}

// RecordStartupReconciledExclusive is RecordStartupReconciled's lock-held
// half. Callers inside RunExclusive use it so the startup-reconciliation gate
// closes before the exclusive lock is released; a deferred fallback waiting on
// that lock then observes the completed pass instead of launching a duplicate
// archive-scale worker. FinishStartupReconciled completes the tail that must
// run outside the lock.
func (e *Engine) RecordStartupReconciledExclusive(stats SyncStats, err error) {
	e.RetainSubagentLinkRetryExclusive(stats.LinksPending)
	e.mu.Lock()
	if err == nil && !stats.Aborted {
		e.lastSync = time.Now()
	}
	e.lastSyncStats = stats
	e.mu.Unlock()
	// The worker's err already reports a canceled pass; record it regardless
	// of the daemon's context.
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
func (stats *SyncStats) AuthoritativeDiscoveryComplete() bool {
	return !stats.Aborted && stats.providerFailures == 0
}

// ProcessingComplete reports whether all processed results reached a terminal
// state that may acknowledge the run and retire retry state.
func (stats *SyncStats) ProcessingComplete() bool {
	return !stats.Aborted && stats.Failed == 0 &&
		stats.providerFailures == 0 && stats.Deferred == 0
}

// ProcessingCompleteOrRetrying reports whether a caller can continue after a
// pass whose only unfinished work is deferred sources already queued for
// retry: an installed rebuild or a startup pass that scheduled its retries.
// Deferred sources must still not be acknowledged through ProcessingComplete.
func (stats *SyncStats) ProcessingCompleteOrRetrying() bool {
	return stats.ProcessingComplete() ||
		((stats.ArchiveRebuilt || stats.deferredRetryScheduled) &&
			stats.Deferred > 0 && stats.Failed == 0 && stats.AuthoritativeDiscoveryComplete())
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

// RunExclusiveCtx is RunExclusive for a caller that must stop waiting for the
// lock when ctx ends, such as a push holding the daemon's push turn. It
// returns ctx.Err() without running work if ctx ends before the lock is free.
func (e *Engine) RunExclusiveCtx(ctx context.Context, work func() error) error {
	if e.refuseWriteInForceParse("RunExclusiveCtx") {
		return errors.New(
			"RunExclusiveCtx refused on report-only parse-diff engine",
		)
	}
	if err := lockctx.Lock(ctx, &e.syncMu); err != nil {
		return err
	}
	defer e.syncMu.Unlock()
	return work()
}

// ErrSyncInProgress reports that non-blocking foreground sync coordination
// found another sync or maintenance pass holding the exclusive engine lock.
var ErrSyncInProgress = errors.New("sync already in progress")

func (e *Engine) lockSync(try bool) error {
	if try {
		if !e.syncMu.TryLock() {
			return ErrSyncInProgress
		}
	} else {
		e.syncMu.Lock()
	}
	return nil
}

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
	if err := e.lockSync(true); err != nil {
		return err
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
func (e *Engine) RunExclusiveFlushed(ctx context.Context, work func() error) error {
	if e.refuseWriteInForceParse("RunExclusiveFlushed") {
		return errors.New(
			"RunExclusiveFlushed refused on report-only parse-diff engine",
		)
	}
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.signalSched.flushAllInline(ctx)
	return work()
}

// SettleForPush waits for an in-progress sync to finish and then flushes
// deferred signal recomputes, so a push that starts next copies settled
// sessions. It does not hold the sync lock afterward: later syncs may write
// while the push runs. If ctx ends first, SettleForPush returns ctx.Err() and
// abandons the wait; the lock is released without flushing once acquired.
func (e *Engine) SettleForPush(ctx context.Context) error {
	if e.refuseWriteInForceParse("SettleForPush") {
		return errors.New(
			"SettleForPush refused on report-only parse-diff engine",
		)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var abandoned atomic.Bool
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		e.syncMu.Lock()
		defer e.syncMu.Unlock()
		if !abandoned.Load() {
			e.signalSched.flushAllInline(ctx)
		}
	}()
	select {
	case <-settled:
		return nil
	case <-ctx.Done():
		abandoned.Store(true)
		return ctx.Err()
	}
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

// AssignSessionProject records a one-session project override under the archive
// write lock, waiting for a closed writer to reopen, then publishes the inventory.
func (e *Engine) AssignSessionProject(
	ctx context.Context,
	sessionID string,
	project string,
) (db.SessionProjectAssignment, error) {
	return runSessionAnnotationWrite(ctx, e, func() (db.SessionProjectAssignment, error) {
		return e.archive.AssignSessionProject(ctx, sessionID, project)
	}, func(db.SessionProjectAssignment) bool { return true })
}

// ClearSessionProjectAssignment removes a one-session override under the archive
// write lock, waiting for a closed writer to reopen, then publishes the inventory.
func (e *Engine) ClearSessionProjectAssignment(
	ctx context.Context,
	sessionID string,
) (db.ClearedSessionProjectAssignment, error) {
	return runSessionAnnotationWrite(ctx, e, func() (db.ClearedSessionProjectAssignment, error) {
		return e.archive.ClearSessionProjectAssignment(ctx, sessionID)
	}, func(db.ClearedSessionProjectAssignment) bool { return true })
}

// SetSessionLabels replaces one session's labels under the archive write lock,
// waiting for a closed writer to reopen, then publishes when the labels changed.
func (e *Engine) SetSessionLabels(
	ctx context.Context, sessionID string, labels []string,
) (db.SessionLabels, error) {
	return runSessionAnnotationWrite(ctx, e, func() (db.SessionLabels, error) {
		return e.archive.SetSessionLabels(ctx, sessionID, labels)
	}, sessionLabelsChanged)
}

// UpdateSessionLabels adds and removes labels under the archive write lock,
// waiting for a closed writer to reopen, then publishes when the labels changed.
func (e *Engine) UpdateSessionLabels(
	ctx context.Context, sessionID string, add, remove []string,
) (db.SessionLabels, error) {
	return runSessionAnnotationWrite(ctx, e, func() (db.SessionLabels, error) {
		return e.archive.UpdateSessionLabels(ctx, sessionID, add, remove)
	}, sessionLabelsChanged)
}

// SetSessionExternalParent records a launcher-supplied parent link under the
// archive write lock, waiting for a closed writer to reopen.
func (e *Engine) SetSessionExternalParent(
	ctx context.Context, sessionID, parentID string,
) (db.SessionExternalParent, error) {
	return runSessionAnnotationWrite(ctx, e, func() (db.SessionExternalParent, error) {
		return e.archive.SetSessionExternalParent(ctx, sessionID, parentID)
	}, sessionExternalParentChanged)
}

// ClearSessionExternalParent removes a launcher-supplied parent link under the
// archive write lock, waiting for a closed writer to reopen.
func (e *Engine) ClearSessionExternalParent(
	ctx context.Context, sessionID string,
) (db.SessionExternalParent, error) {
	return runSessionAnnotationWrite(ctx, e, func() (db.SessionExternalParent, error) {
		return e.archive.ClearSessionExternalParent(ctx, sessionID)
	}, sessionExternalParentChanged)
}

// sessionLabelsChanged skips labels stored ahead of sync, which change no
// listed session.
func sessionLabelsChanged(l db.SessionLabels) bool { return l.Changed && l.SessionFound }

func sessionExternalParentChanged(l db.SessionExternalParent) bool { return l.Changed }

// runSessionAnnotationWrite waits for the archive writer with the caller's
// context. The write takes the archive write lock, and a visible change publishes
// the session inventory.
func runSessionAnnotationWrite[T any](
	ctx context.Context, e *Engine, write func() (T, error), changed func(T) bool,
) (T, error) {
	var result T
	if e.refuseWriteInForceParse("runSessionAnnotationWrite") {
		return result, errors.New("runSessionAnnotationWrite refused on report-only parse-diff engine")
	}
	result, err := db.RetryAcrossHandoff(ctx, e.archive, write)
	if err == nil && changed(result) {
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
func (e *Engine) HasActiveSessionSourceBelow(ctx context.Context, agent, path string) (bool, error) {
	found, err := e.db.HasActiveSessionSourceBelow(ctx, agent, path)
	if err != nil || found {
		return found, err
	}
	if agent == string(parser.AgentCodebuff) {
		return e.db.HasActiveSessionSourceBelow(ctx, string(parser.AgentFreebuff), path)
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
// group and shares one pass epilogue — queued repairs, global linking, and
// skip-cache persistence — across the whole batch. A multi-provider poll performs
// global linking once instead of once per provider. Every group is
// attempted even when an earlier one fails; per-group failures are wrapped
// with the provider and joined.
//
// A poll pass can take far longer than one provider group, so between groups
// the pass yields syncMu to any sync already waiting for it. Watcher syncs and
// scheduled reconciliation therefore wait for at most one group, not the whole
// batch. The epilogue still runs under syncMu, and persistSkipCache snapshots
// the in-memory skip cache at that point, so state written by a sync that ran
// between groups is included rather than overwritten. "sessions" emits happen
// after the lock is released, coalesced into one event for the batch.
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
		repairEligible := false
		linkEligible := false
		persistEligible := false
		for i, group := range groups {
			if ctx.Err() != nil {
				break
			}
			if i > 0 {
				// The waiting sync owns CurrentProgress while it runs.
				e.syncMu.yield(e.clearCurrentProgress)
			}
			if e.groupedReconcileGroupHook != nil {
				e.groupedReconcileGroupHook(i)
			}
			stats, tombstoned, eligibility, err := e.reconcileScopedWatchRootsLocked(
				deferredCtx, group.Agent, group.Roots, false, false, nil,
			)
			changed = changed || stats.hasSessionChanges() || tombstoned > 0
			repairEligible = repairEligible || eligibility.repair
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
			linked, err := e.linkSubagentSessions(ctx)
			if err != nil {
				errs = append(errs, fmt.Errorf(
					"link subagent sessions after grouped reconciliation: %w", err,
				))
			} else if linked > 0 {
				// The repair runs after per-group stats are folded into
				// changed. A parent-only update still has to refresh clients.
				changed = true
			}
		}
		if repairEligible {
			repaired, err := e.db.RepairQueuedSubagentParentsContext(ctx, nil)
			if err != nil {
				errs = append(errs, fmt.Errorf(
					"repair queued subagent parents after grouped reconciliation: %w", err,
				))
			} else if repaired > 0 {
				changed = true
			}
		}
		if persistEligible {
			e.persistSkipCache(ctx)
		} else if len(errs) > 0 {
			e.flushFailureCache(ctx)
		}
		var titleStats SyncStats
		e.reconcileNativeTitlesLocked(ctx, &titleStats)
		e.recordNativeTitleUpdates(titleStats)
		changed = changed || titleStats.TitlesUpdated > 0
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
	if e.sourceSyncErr != nil {
		return SyncStats{Aborted: true}, 0, passEpilogueEligibility{}, e.sourceSyncErr
	}
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
		stats.ProcessingComplete()
	if err == nil && !complete {
		err = errors.New("watch root reconciliation aborted")
	}
	e.setLastReconciliationResult(ReconciliationResult{
		Complete:         complete,
		Aborted:          !complete,
		ProviderFailures: stats.providerFailures,
		Metrics:          metrics,
	})
	if !passEpilogueDeferred(ctx) {
		var titleStats SyncStats
		e.reconcileNativeTitlesLocked(ctx, &titleStats)
		e.recordNativeTitleUpdates(titleStats)
		stats.TitlesUpdated += titleStats.TitlesUpdated
		stats.Failed += titleStats.Failed
	}
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
	sources := e.sources()
	if _, enabled := sources.providerFactories[agentType]; !enabled {
		return nil
	}
	return append([]string(nil), sources.agentDirs[agentType]...)
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
	agents := make([]parser.AgentType, 0, len(e.sources().providerFactories))
	for agent := range e.sources().providerFactories {
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
		if len(e.sources().agentDirs[agent]) == 0 {
			continue
		}
		factory := e.sources().providerFactories[agent]
		if factory == nil {
			continue
		}
		requestRoots := roots
		if fullCoverage {
			// A full authoritative request covers each provider's complete
			// configured scope; a partial request lets each provider resolve
			// the same exact caller roots.
			requestRoots = e.sources().agentDirs[agent]
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
			Roots: e.sources().agentDirs[agent], Machine: e.machine,
			SourceMachines: e.sources().sourceMachines[agent],
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
		for _, dirs := range e.sources().agentDirs {
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
// and ReconcileProviderRootsGrouped holds it for each group and for the
// shared epilogue, yielding only between groups.
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
	ctx = e.parsePolicyContext(ctx)
	if force {
		preparation, _ := ctx.Value(recoveryCachePreparationKey{}).(*recoveryCachePreparation)
		if preparation == nil || !preparation.prepared {
			e.clearWatcherOverflowCaches(ctx)
		}
		if preparation != nil {
			preparation.prepared = true
		}
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
	ctx = parser.WithProjectRootMemo(ctx)
	ctx = parser.WithCodexParentInventoryBatch(ctx)
	// Share S3 Codex parent downloads across every reconciliation page.
	ctx, closeS3ParentCache := withS3CodexParentCache(ctx)
	defer closeS3ParentCache()
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

	spool, err := e.reconciliationSpoolFactory(ctx, e.db.Path())
	if err != nil {
		stats.Aborted = true
		return stats, metrics, 0, eligibility, err
	}
	cleaned := false
	defer func() {
		if !cleaned {
			_ = spool.CloseAndRemove(ctx)
		}
	}()

	preContainerStates := e.capturePlannedSQLiteContainerStates(plans, fullCoverage)
	if err := e.prepareSQLiteContainerVerification(ctx, preContainerStates); err != nil {
		stats.Aborted = true
		return stats, metrics, 0, eligibility, err
	}
	e.beginStreamingSQLiteContainerPass(preContainerStates)
	defer func() {
		// Failures here cannot be attributed to one container, so they
		// poison the whole captured set; a clean pass finalizes normally
		// with promotion gated per container.
		if retErr != nil || stats.Aborted || stats.Failed > 0 ||
			stats.providerFailures > 0 {
			e.poisonSQLiteContainerPass()
		}
		e.finishSQLiteContainerPass(ctx, false, fullCoverage)
	}()
	providers, completedScopes, failedRoots,
		failures, discoveryErr, err := e.streamReconciliationCandidates(
		ctx, plans, spool, preContainerStates,
	)
	stats.providerFailures = failures
	if err != nil {
		stats.Aborted = true
		metrics = mergeMetrics(spool.Metrics())
		if cleanupErr := spool.CloseAndRemove(ctx); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
		cleaned = true
		return stats, metrics, 0, eligibility, err
	}
	e.finishStreamingSQLiteContainerDiscovery()
	authoritativeProviders := make(map[parser.AgentType]struct{}, len(completedScopes))
	for _, completed := range completedScopes {
		authoritativeProviders[completed.agent] = struct{}{}
		// Freebuff shares Codebuff's provider and on-disk roots. Mark both
		// agents authoritative after one scan so baseline admission and
		// cache-write promotion include Freebuff rows.
		if completed.agent == parser.AgentCodebuff {
			authoritativeProviders[parser.AgentFreebuff] = struct{}{}
		}
	}
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
		authority := newReconciliationAuthority(authoritativeProviders, providers, completedScopes, spool)
		nonAuthoritativeScopes := baselineTracker.listNonAuthoritativeScopes()
		for i := range nonAuthoritativeScopes {
			scope := &nonAuthoritativeScopes[i]
			scope.Path = authority.physicalPath(scope.Provider, scope.Path)
		}
		if err := spool.AddNonAuthoritativeScopes(ctx, nonAuthoritativeScopes); err != nil {
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
		baselineCandidates, baselineAdmitted, err := authority.eligibleBaselines(
			ctx, page, baselineTracker.list(),
		)
		cacheWrites := baselineTracker.listCacheWrites()
		if err != nil {
			e.rejectSkipCacheWrites(pageCtx, cacheWrites)
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
		exactOwnerships, err := authority.ownerships(ctx, baselineTracker.listExactOwnerships())
		var rejectedExactOwnerships []db.SessionSourceOwnership
		if err == nil {
			rejectedExactOwnerships, err = authority.ownerships(ctx, baselineTracker.listRejectedExactOwnerships())
		}
		if err == nil {
			err = e.baselineReconciliationCandidates(
				ctx, baselineCandidates, baselineAdmitted,
				exactOwnerships, rejectedExactOwnerships,
			)
		}
		if err != nil {
			e.rejectSkipCacheWrites(pageCtx, cacheWrites)
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
			e.clearSkip(pageCtx, write.key)
		}
		e.promoteSkipCacheWrites(eligibleCacheWrites)
		cursor = page[len(page)-1].Cursor()
	}
	retErr = errors.Join(retErr, e.finishCodexParentInventoryBatch(ctx, &stats))
	stats.logRetrySummary("sync")
	// Pending linking depends only on committed archive rows. Unrelated source
	// failures must not suppress it; full reconciliation without pending work
	// still requires a completed pass. Grouped callers consume this eligibility
	// once after all groups, and tombstoning keeps its separate success gate.
	e.subagentLinkPending = e.subagentLinkPending || stats.Synced > 0
	if ctx.Err() == nil {
		eligibility.repair = true
		eligibility.link = e.subagentLinkPending ||
			(fullCoverage && retErr == nil && stats.Failed == 0 && !stats.Aborted)
	}
	if eligibility.link && !passEpilogueDeferred(ctx) {
		// Keep earlier processing errors so failed work still blocks tombstoning
		// and retains its retry roots even when the pending links are repaired.
		linked, err := e.linkSubagentSessions(ctx)
		if err != nil {
			stats.RecordFailed()
			stats.Aborted = true
			retErr = errors.Join(retErr, fmt.Errorf(
				"link subagent sessions after reconciliation: %w", err,
			))
		} else {
			// Record after subagentLinkPending is sampled above. Folding
			// the repair into that sample would keep the next unchanged
			// poll on the global link path.
			stats.RecordLinksUpdated(linked)
		}
	}
	// An empty spool never enters collectAndBatch, so its pending durable
	// repairs must run here too. This touches queued IDs, not the full archive.
	if eligibility.repair && !passEpilogueDeferred(ctx) {
		repaired, err := e.db.RepairQueuedSubagentParentsContext(ctx, nil)
		if err != nil {
			stats.RecordFailed()
			stats.Aborted = true
			retErr = errors.Join(retErr, fmt.Errorf(
				"repair queued subagent parents after reconciliation: %w", err,
			))
		} else {
			stats.RecordLinksUpdated(repaired)
		}
	}
	canTombstoneCompletedScopes :=
		retErr == nil && (failures > 0 || stats.Deferred > 0) &&
			stats.Failed == 0 && !stats.Aborted
	if failures > 0 || stats.Deferred > 0 {
		retryRoots := append([]string(nil), failedRoots...)
		if retErr != nil || stats.deferredRetryOverflow {
			for _, completed := range completedScopes {
				retryRoots = append(retryRoots, completed.roots...)
			}
			slices.Sort(retryRoots)
			retryRoots = slices.Compact(retryRoots)
		}
		incomplete := &incompleteReconciliationError{
			failures:    failures,
			deferred:    stats.Deferred,
			roots:       retryRoots,
			paths:       append([]string(nil), stats.deferredRetryPaths...),
			overflow:    stats.deferredRetryOverflow,
			completed:   completedScopes,
			cause:       discoveryErr,
			deferOnly:   failures == 0 && retErr == nil && discoveryErr == nil && stats.Failed == 0 && !stats.Aborted,
			retryCauses: stats.retryCauseSample(),
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
			e.persistSkipCache(ctx)
		}
		e.mu.Lock()
		e.lastSync = time.Now()
		e.lastSyncStats = stats
		e.mu.Unlock()
	}
	if retErr == nil && ctx.Err() == nil && stats.ProcessingComplete() {
		tombstoned, retErr = e.tombstoneMissingWatchSourceScopesLocked(
			ctx, completedScopes, spool,
		)
	} else if canTombstoneCompletedScopes && ctx.Err() == nil &&
		!stats.Aborted && stats.Failed == 0 {
		incomplete, hasIncomplete := errors.AsType[*incompleteReconciliationError](retErr)
		if hasIncomplete && len(incomplete.completed) > 0 {
			var tombstoneErr error
			tombstoned, tombstoneErr = e.tombstoneCompletedReconciliationScopesLocked(
				ctx, spool, incomplete,
			)
			if tombstoneErr != nil {
				retErr = tombstoneErr
			}
		}
	}
	if ctx.Err() == nil && !passEpilogueDeferred(ctx) {
		e.flushFailureCache(ctx)
	}
	metrics = mergeMetrics(spool.Metrics())
	if cleanupErr := spool.CloseAndRemove(ctx); cleanupErr != nil {
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
		failures:    incomplete.failures,
		deferred:    incomplete.deferred,
		roots:       retryRoots,
		paths:       append([]string(nil), incomplete.paths...),
		overflow:    incomplete.overflow,
		cause:       errors.Join(incomplete, err),
		retryCauses: incomplete.retryCauses,
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

func (authority *reconciliationAuthority) eligibleBaselines(
	ctx context.Context,
	candidates []reconciliationCandidate,
	admitted []machineSessionSource,
) ([]reconciliationCandidate, []machineSessionSource, error) {
	var eligibleCandidates []reconciliationCandidate
	for _, candidate := range candidates {
		eligible, err := authority.allows(ctx, candidate.Provider, candidate.Path)
		if err != nil {
			return nil, nil, err
		}
		if eligible {
			eligibleCandidates = append(eligibleCandidates, candidate)
		}
	}
	var eligibleAdmitted []machineSessionSource
	for _, source := range admitted {
		eligible, err := authority.allows(ctx, parser.AgentType(source.Source.Agent), source.Source.FilePath)
		if err != nil {
			return nil, nil, err
		}
		if eligible {
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
	var incompleteScopes []reconciliationSourceScope
	// Trusted containers stream the bounded watermark listing here for the
	// same reason full discovery lists them that way: every candidate they
	// spool will gate-skip, so the child digest would be archive-sized work
	// nothing reads. The predicate is keyed to this pass's pre-discovery
	// captures; a container that changes mid-stream fails its recapture
	// check and its candidates resolve full fingerprints instead.
	containerListsWatermarkOnly := e.sqliteContainerListsWatermarkOnly(
		preContainerStates,
	)
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
		factory := e.sources().providerFactories[agent]
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
		if agent == parser.AgentKiro {
			traversalRoots = append([]string(nil), e.sources().agentDirs[agent]...)
		}
		provider := factory.NewProvider(parser.ProviderConfig{
			Roots: traversalRoots, Machine: e.machine, PathRewriter: e.pathRewriter,
			SourceMachines:                    e.sources().sourceMachines[agent],
			SQLiteContainerListsWatermarkOnly: containerListsWatermarkOnly,
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
			discoveryRoots := group.roots
			if agent == parser.AgentKiro {
				// Kiro source arbitration spans every configured root even when
				// the proof scope is partial; only the winning candidate inside
				// that proof is admitted to processing below.
				discoveryRoots = e.sources().agentDirs[agent]
			}
			scopeProvider := factory.NewProvider(parser.ProviderConfig{
				Roots: discoveryRoots, Machine: e.machine,
				SourceMachines:                    e.sources().sourceMachines[agent],
				PathRewriter:                      e.pathRewriter,
				SQLiteContainerListsWatermarkOnly: containerListsWatermarkOnly,
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
			rankingRoots := traversalRoots
			if agent == parser.AgentKiro {
				// Kiro precedence uses configured roots for reordered scoped requests.
				rankingRoots = e.sources().agentDirs[agent]
			}
			var kiroWinners map[string]reconciliationCandidate
			if agent == parser.AgentKiro {
				kiroWinners = make(map[string]reconciliationCandidate)
			}
			var spoolErr error
			var scopedDiscovery, emitted bool
			discover := discoverer.DiscoverEach
			if scoped, ok := scopeProvider.(parser.ScopedStreamingDiscoverer); ok {
				discover = func(ctx context.Context, yield func(parser.SourceRef) error) error {
					scopedDiscovery = true
					err := scoped.DiscoverScopesEach(ctx, group.scopes, yield)
					unsupported, missing := errors.AsType[parser.UnsupportedProviderFeatureError](err)
					if !emitted && missing && unsupported.Feature == parser.ProviderFeatureScopedDiscovery {
						scopedDiscovery = false
						return discoverer.DiscoverEach(ctx, yield)
					}
					return err
				}
			}
			err = discover(ctx, func(source parser.SourceRef) error {
				emitted = true
				candidate, ok := e.reconciliationCandidate(ctx,
					provider, source, rankingRoots, watchRoots,
				)
				if !ok {
					return nil
				}
				if agent == parser.AgentKiro {
					current, exists := kiroWinners[candidate.Identity]
					if !exists || reconciliationCandidatePreferred(candidate, current) {
						kiroWinners[candidate.Identity] = candidate
					}
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
				if spoolErr == nil {
					replaced, replacedOK := spool.LastAddReplaced()
					if replacedOK {
						e.unNoteSQLiteContainerDiscovery(parser.DiscoveredFile{
							Agent: replaced.Provider,
							Path:  replaced.Path,
						})
					}
					if spool.LastAddWon() || replacedOK {
						e.noteSQLiteContainerDiscovery(parser.DiscoveredFile{
							Agent:          candidate.Provider,
							Path:           candidate.Path,
							ProviderSource: &source,
						})
					}
				}
				return spoolErr
			})
			if err == nil && agent == parser.AgentKiro {
				for _, identity := range slices.Sorted(maps.Keys(kiroWinners)) {
					candidate := kiroWinners[identity]
					admitted := false
					for _, proofScopes := range groupProofs {
						if db.StoredSourcePathHintScopesContain(candidate.Path, proofScopes) {
							admitted = true
							break
						}
					}
					if !admitted {
						continue
					}
					spoolErr = spool.Add(ctx, candidate)
					if spoolErr != nil {
						break
					}
				}
				if spoolErr != nil {
					return providers, completedScopes,
						failedRoots, failures, discoveryErr, spoolErr
				}
			}
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
				var excluded []reconciliationSourceScope
				var retryRoots []string
				var localised bool
				if scopedDiscovery {
					excluded, retryRoots, localised = localisedReconciliationFailures(agent, group, err)
				}
				if !localised || len(retryRoots) > 0 {
					failures++
					discoveryErr = errors.Join(discoveryErr, fmt.Errorf(
						"%s provider streaming discovery: %w", agent, err,
					))
				}
				if !localised {
					failedRoots = append(failedRoots, groupRetryRoots...)
					continue
				}
				failedRoots = append(failedRoots, retryRoots...)
				incompleteScopes = append(incompleteScopes, excluded...)
			}
			for _, scope := range group.scopes {
				coverage := scope.CoverageIdentities
				if err != nil {
					coverage = nil
				}
				completedScopes = append(completedScopes, reconciliationProviderScope{
					agent: agent,
					roots: append([]string(nil), scope.RetryRoots...),
					proofScopes: append(
						[]parser.StoredSourceHintScope(nil), scope.PhysicalProofScopes...,
					),
					coverageIdentities: append(
						[]string(nil), coverage...,
					),
					requiredCoverageIdentities: append(
						[]string(nil), plan.plan.RequiredCoverageIdentities...,
					),
				})
			}
		}
	}
	if len(incompleteScopes) > 0 {
		if err := spool.AddNonAuthoritativeScopes(ctx, incompleteScopes); err != nil {
			return providers, completedScopes, failedRoots, failures, discoveryErr, err
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
	deferred  int
	roots     []string
	paths     []string
	overflow  bool
	completed []reconciliationProviderScope
	cause     error
	deferOnly bool
	// retryCauses is a bounded sample of why results need retry.
	retryCauses string
}

func (e *incompleteReconciliationError) Error() string {
	if e.failures == 0 {
		msg := fmt.Sprintf(
			"watch root reconciliation deferred: %d provider results need retry",
			e.deferred,
		)
		if e.retryCauses != "" {
			msg += ": " + e.retryCauses
		}
		return msg
	}
	return fmt.Sprintf(
		"watch root reconciliation incomplete: %d provider discoveries failed",
		e.failures,
	)
}

func (e *incompleteReconciliationError) Unwrap() error { return e.cause }

func (e *incompleteReconciliationError) ReconciliationRetryDeferOnly() bool {
	return e.deferOnly
}

func (e *incompleteReconciliationError) ReconciliationRetryRoots() []string {
	return append([]string(nil), e.roots...)
}

func (e *incompleteReconciliationError) ReconciliationRetryPaths() []string {
	return append([]string(nil), e.paths...)
}

func (e *incompleteReconciliationError) ReconciliationRetryOverflow() bool {
	return e.overflow
}

func (e *Engine) reconciliationCandidate(ctx context.Context,
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
	claudeFormat := isClaudeFormatTranscript(agent, path)
	if claudeFormat {
		// This mirrors the duplicate-selection policy documented on
		// claudeFormatTranscriptPreference. Claude ranks size, mtime, then
		// unchanged-committed-copy; ICodeMate ranks committed-or-longer,
		// mtime, then size, because its transcripts are rewritten in place
		// and a larger stale copy must not outrank the committed source.
		transcriptSize, transcriptMtime, transcriptOK := claudeTranscriptFileSourceInfo(
			parser.DiscoveredFile{Path: statPath, Agent: agent},
		)
		if transcriptOK {
			if agent == parser.AgentIcodemate {
				preference2, preference3 = claudeFormatTranscriptPreference(
					agent, transcriptSize, transcriptMtime,
				)
			} else {
				preference1, preference2 = claudeFormatTranscriptPreference(
					agent, transcriptSize, transcriptMtime,
				)
			}
			fullID := applyIDPrefixToID(
				e.idPrefix, claudeFormatArchiveSessionID(agent, identity),
			)
			storedPath := e.db.GetSessionFilePath(ctx, fullID)
			if agent == parser.AgentIcodemate {
				preferredAgainstStored := storedPath != "" &&
					e.effectiveSourcePath(path) == storedPath
				if !preferredAgainstStored && storedPath != "" {
					storedTranscriptSize, _, storedTranscriptOK := claudeTranscriptFileSourceInfo(parser.DiscoveredFile{
						Path: storedPath, Agent: agent,
					})
					preferredAgainstStored = storedTranscriptOK &&
						transcriptSize > storedTranscriptSize
				}
				preference1 = boolPreference(preferredAgainstStored)
			} else {
				sourceSize, sourceMtime, sourceOK := claudeFormatFileSourceInfo(
					parser.DiscoveredFile{Path: statPath, Agent: agent},
				)
				storedSize, storedMtime, stored := e.db.GetSessionFileInfo(ctx, fullID)
				if sourceOK && stored && e.effectiveSourcePath(path) == storedPath &&
					storedSize == sourceSize && storedMtime == sourceMtime &&
					e.db.GetSessionDataVersion(ctx, fullID) >= db.CurrentDataVersion() {
					preference3 = 1
				}
			}
		}
	}
	if isCodexFormatAgent(agent) {
		if codexLayoutForPath(path) == parser.CodexLayoutDated {
			preference1 = 1
		}
		// Match discovery: dated layout, then plain rollout, then path.
		preference2 = boolPreference(!strings.HasSuffix(path, ".jsonl.zst"))
	}
	if isOpenCodeFormatAgent(agent) && !claudeFormat {
		if statPath == path {
			preference1 = 1
		}
	} else if !claudeFormat && !isCodexFormatAgent(agent) && agent != parser.AgentOpenClaw {
		preference1 = configuredRootPreference(statPath, roots)
	}
	if ranker, ok := provider.(parser.ReconciliationSourceRanker); ok {
		rank := ranker.ReconciliationSourceRank(source)
		if agent == parser.AgentKiro {
			preference1 = rank.Class
			preference2 = configuredRootPreferenceForSource(source, statPath, roots)
			preference3 = rank.Recency
		} else {
			preference2 = rank.Class
			preference3 = rank.Recency
		}
	}
	if agent == parser.AgentAntigravityCLI {
		preference2 = boolPreference(strings.HasSuffix(path, ".db"))
	}
	var sourceState parser.ReconciliationSourceState
	if stateProvider, ok := provider.(parser.ReconciliationSourceStateProvider); ok {
		if state, stateOK := stateProvider.ReconciliationSourceState(ctx, source); stateOK {
			sourceState = state
		}
	}
	return reconciliationCandidate{
		Provider: agent, Identity: identity, Path: path,
		StoredPath: canonicalReconciliationSourceIdentity(
			e.effectiveSourcePath(path),
		), MemberIdentity: source.ReconciliationIdentity, WatchRoot: root,
		Machine: e.machineForProviderSource(agent, source, path),
		Project: source.ProjectHint, SourceState: sourceState,
		Preference1: preference1,
		Preference2: preference2, Preference3: preference3,
	}, true
}

func reconciliationCandidatePreferred(
	candidate, current reconciliationCandidate,
) bool {
	if candidate.Preference1 != current.Preference1 {
		return candidate.Preference1 > current.Preference1
	}
	if candidate.Preference2 != current.Preference2 {
		return candidate.Preference2 > current.Preference2
	}
	if candidate.Preference3 != current.Preference3 {
		return candidate.Preference3 > current.Preference3
	}
	return candidate.Path < current.Path
}

func configuredRootPreference(path string, roots []string) int64 {
	for i, configured := range roots {
		if samePathOrDescendant(path, configured) {
			return int64(len(roots) - i)
		}
	}
	return 0
}

func configuredRootPreferenceForSource(
	source parser.SourceRef, path string, roots []string,
) int64 {
	if source.ConfiguredRoot != "" {
		for i, configured := range roots {
			if samePathOrDescendant(configured, source.ConfiguredRoot) &&
				samePathOrDescendant(source.ConfiguredRoot, configured) {
				return int64(len(roots) - i)
			}
		}
	}
	return configuredRootPreference(path, roots)
}

func boolPreference(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func reconciliationSourceIdentity(agent parser.AgentType, source parser.SourceRef) string {
	path := providerDiscoveredPath(source)
	if isClaudeFormatTranscript(agent, path) {
		return claudeSessionIDFromPath(path)
	}
	if isOpenCodeFormatAgent(agent) {
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
// reconciliation identity, parse-diff mtime) so the Codex forks get the same
// handling. Branches that depend on Codex's session_index.jsonl sidecar or its
// S3 archive layout stay keyed to parser.AgentCodex alone: agentsview reads
// neither for the forks.
func isCodexFormatAgent(agent parser.AgentType) bool {
	return parser.IsCodexFormatAgent(agent)
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
	e.refreshReconciliationPageContainerCaptures(page)
	files := make([]parser.DiscoveredFile, 0, len(page))
	for _, candidate := range page {
		forceCandidate := force
		provider := providers[candidate.Provider]
		if provider == nil {
			return nil, fmt.Errorf("rehydrate %s source: provider unavailable", candidate.Provider)
		}
		if resolver, ok := provider.(parser.ReconciliationSourceResolver); ok {
			var source parser.SourceRef
			var found bool
			var err error
			if stateResolver, stateOK := provider.(parser.ReconciliationSourceStateResolver); stateOK &&
				candidate.SourceState.Version != 0 {
				source, found, err = stateResolver.SourceForReconciliationWithState(
					ctx, candidate.Path, candidate.Project, candidate.SourceState,
				)
			} else {
				source, found, err = resolver.SourceForReconciliation(
					ctx, candidate.Path, candidate.Project,
				)
			}
			if err != nil {
				return nil, fmt.Errorf("rehydrate %s source %s: %w", candidate.Provider, candidate.Path, err)
			}
			if found && reconciliationSourceIdentity(candidate.Provider, source) == candidate.Identity {
				if e.applyReconciliationSourceStateIfValid(ctx,
					provider, &source, candidate.SourceState,
					candidate.Provider, candidate.Path,
				) {
					file := parser.DiscoveredFile{
						Path: candidate.Path, Project: source.ProjectHint,
						Agent: candidate.Provider, ForceParse: forceCandidate,
						Machine:        candidate.Machine,
						ProviderSource: &source, ProviderProcess: true,
					}
					membershipFile := file
					membershipFile.Path = providerDiscoveredPath(source)
					if membershipFile.Path == "" {
						membershipFile.Path = candidate.Path
					}
					e.unNoteSQLiteContainerDiscovery(parser.DiscoveredFile{
						Agent: candidate.Provider, Path: candidate.Path,
					})
					e.noteSQLiteContainerDiscovery(membershipFile)
					files = append(files, file)
					continue
				}
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
		_ = e.applyReconciliationSourceStateIfValid(ctx,
			provider, &source, candidate.SourceState,
			candidate.Provider, candidate.Path,
		)
		file := parser.DiscoveredFile{
			Path: candidate.Path, Project: source.ProjectHint,
			Agent: candidate.Provider, ForceParse: forceCandidate,
			// Carry the candidate's stored attribution: recomputing it from the
			// physical path is wrong for providers whose source can sit outside
			// the labeled root it was configured under.
			Machine:        candidate.Machine,
			ProviderSource: &source, ProviderProcess: true,
		}
		membershipFile := file
		membershipFile.Path = providerDiscoveredPath(source)
		if membershipFile.Path == "" {
			membershipFile.Path = candidate.Path
		}
		e.unNoteSQLiteContainerDiscovery(parser.DiscoveredFile{
			Agent: candidate.Provider, Path: candidate.Path,
		})
		e.noteSQLiteContainerDiscovery(membershipFile)
		files = append(files, file)
	}
	return files, nil
}

// refreshReconciliationPageContainerCaptures invalidates SQLite container
// captures that changed after discovery and before a spool page was rehydrated.
// This keeps a full child digest from becoming stale while it is still being
// used to avoid per-member child lookups.
func (e *Engine) refreshReconciliationPageContainerCaptures(
	page []reconciliationCandidate,
) {
	e.containerMu.Lock()
	pass := e.containerPass
	if pass == nil {
		e.containerMu.Unlock()
		return
	}
	expected := make(map[string]parser.SQLiteContainerState)
	for _, candidate := range page {
		dbPath, _, ok := sqliteContainerSourceForFile(parser.DiscoveredFile{
			Agent: candidate.Provider, Path: candidate.Path,
		})
		if !ok || pass.failed[dbPath] {
			continue
		}
		state, captured := pass.captured[dbPath]
		if !captured {
			pass.failed[dbPath] = true
			continue
		}
		expected[dbPath] = state
	}
	e.containerMu.Unlock()

	for dbPath, before := range expected {
		after, ok := statSQLiteContainerState(dbPath)
		if ok && after == before {
			continue
		}
		e.containerMu.Lock()
		if e.containerPass == pass {
			pass.failed[dbPath] = true
		}
		e.containerMu.Unlock()
	}
}

// applyReconciliationSourceStateIfValid treats provider state as an optional
// optimization. Missing, malformed, or stale state falls through to the
// authoritative changed-path source resolution instead of aborting
// reconciliation. The container capture check keys on the RESOLVED source
// representation, not the candidate path: resolution may have promoted a
// virtual SQLite member to its canonical storage shadow, whose parse does
// not depend on the container, and rejecting it here would send it to a
// path-matching fallback that cannot match the promoted path.
func (e *Engine) applyReconciliationSourceStateIfValid(ctx context.Context,
	provider parser.Provider,
	source *parser.SourceRef,
	state parser.ReconciliationSourceState,
	agent parser.AgentType,
	path string,
) bool {
	if source == nil {
		return false
	}
	if state.Version == 0 {
		return true
	}
	resolvedPath := providerDiscoveredPath(*source)
	if resolvedPath == "" {
		resolvedPath = path
	}
	if dbPath, _, ok := sqliteContainerSourceForFile(parser.DiscoveredFile{
		Agent: agent, Path: resolvedPath,
	}); ok {
		e.containerMu.Lock()
		passActive := e.containerPass != nil
		e.containerMu.Unlock()
		if passActive && !e.sqliteContainerPassCaptureStillCurrent(dbPath) {
			return false
		}
	}
	stateProvider, ok := provider.(parser.ReconciliationSourceStateProvider)
	if !ok {
		return false
	}
	return stateProvider.ApplyReconciliationSourceState(ctx, source, state) == nil
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
// The lookup uses the session identity and compares any result with the
// archived source path before treating it as a move.
func (e *Engine) reconciliationMemberRelocated(
	ctx context.Context,
	provider parser.Provider,
	fullSessionID, storedPath string,
) (bool, error) {
	if provider == nil {
		// Without a provider the move cannot be ruled out; report the member
		// as possibly relocated so deletion is withheld.
		return true, nil
	}
	source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{
		FullSessionID: fullSessionID,
	})
	if err != nil {
		return false, fmt.Errorf(
			"resolve possibly relocated member %s: %w", fullSessionID, err,
		)
	}
	if !found {
		return false, nil
	}
	lowerRanked, err := e.storedSourceRanksHigher(
		ctx, provider, storedPath, source,
	)
	if err != nil {
		return false, err
	}
	if lowerRanked {
		return false, nil
	}
	return found, nil
}

func isProviderVirtualMemberPath(
	provider parser.Provider, path string, proofs []parser.StoredSourceHintScope,
) bool {
	container, _, virtual := parser.ParseVirtualSourcePath(path)
	if !virtual {
		return false
	}
	if reconciliationProofCoversContainerMembership(proofs, container) {
		return true
	}
	resolver, ok := provider.(parser.StoredSourceHintScopeProvider)
	if !ok {
		return false
	}
	return reconciliationProofCoversContainerMembership(
		resolver.StoredSourceHintScopes(parser.ChangedPathRequest{Path: container}),
		container,
	)
}

func reconciliationPhysicalSourcePath(
	provider parser.Provider, path string, proofs []parser.StoredSourceHintScope,
) string {
	if isProviderVirtualMemberPath(provider, path, proofs) {
		container, _, _ := parser.ParseVirtualSourcePath(path)
		return container
	}
	return path
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
	ctx context.Context, spool reconciliationSpoolStore,
	provider parser.Provider, scope reconciliationProviderScope,
	ownership db.SessionSourceOwnership,
) (bool, error) {
	if spool == nil {
		return false, nil
	}
	path := reconciliationPhysicalSourcePath(provider, ownership.FilePath, scope.proofScopes)
	return spool.ContainsNonAuthoritativeScope(ctx, scope.agent, path)
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
	if isClaudeFormatAgent(agent) {
		if isClaudeFormatTranscript(agent, storedPath) {
			return claudeSessionIDFromPath(storedPath)
		}
		return ""
	}
	switch {
	case isCodexFormatAgent(agent):
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
	dst.TitlesUpdated += src.TitlesUpdated
	dst.LinksUpdated += src.LinksUpdated
	dst.Aborted = dst.Aborted || src.Aborted
	dst.mergeCauses(src)
	if !dst.deferredRetryOverflow {
		deferred := dst.Deferred
		for _, path := range src.deferredRetryPaths {
			dst.recordDeferred(path)
		}
		dst.Deferred = deferred + src.Deferred
		dst.deferredRetryOverflow = dst.deferredRetryOverflow ||
			src.deferredRetryOverflow
		if dst.deferredRetryOverflow {
			dst.deferredRetryPaths = nil
		}
	} else {
		dst.Deferred += src.Deferred
	}
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
	if e.sourceSyncErr != nil && e.archiveStore == nil {
		return SyncStats{Aborted: true, Warnings: []string{e.sourceSyncErr.Error()}, abortCause: e.sourceSyncErr.Error()}
	}
	if ctx.Err() != nil {
		return SyncStats{Aborted: true}
	}
	e.invalidateNativeTitleCache(nil)
	ctx, closeProviderCache, err := e.withProviderFingerprintCache(ctx, nil)
	if err != nil {
		return SyncStats{Aborted: true, Warnings: []string{err.Error()}, abortCause: err.Error()}
	}
	defer func() {
		if err := closeProviderCache(); err != nil {
			stats.abort(err.Error())
			stats.RecordFailed()
			stats.Warnings = append(stats.Warnings, err.Error())
		}
	}()
	ctx = e.parsePolicyContext(ctx)
	ctx = parser.WithProjectRootMemo(ctx)
	ctx = parser.WithCodexParentInventoryBatch(ctx)

	if recordSyncState {
		e.recordSyncStarted(ctx)
	}
	e.phaseStats.Reset()
	e.resetS3CodexIndexCache()
	e.resetSourceClaims()
	e.anomalies.reset()
	// Fold the per-run anomaly accumulator into the returned stats on
	// every exit path so the CLI sync summary can surface them.
	defer func() { e.anomalies.applyTo(&stats) }()

	// A whole-archive pass (resync rebuild, full/initial sync, remote
	// import) is bulk work: it bounds parsed data by retained bytes and
	// frees that memory once at the end. Cutoff- or root-scoped passes are
	// steady-state daemon churn and keep the default retention budget.
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
	if err := e.prepareSQLiteContainerVerification(ctx, preContainerStates); err != nil {
		return SyncStats{Aborted: true, Warnings: []string{err.Error()}}
	}

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
		all = e.dedupeClaudeDiscoveredFiles(ctx, all)
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
		all = e.filterQuickSyncFiles(ctx, all, since)
	}

	if quickSyncCutoff {
		all = dedupeDiscoveredFilesPreferNewestCodex(all)
	} else {
		all = dedupeDiscoveredFiles(all)
	}
	all = e.dedupeClaudeDiscoveredFiles(ctx, all)
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
		if !isOpenCodeFormatContainerSource(f.Agent, f.Path) {
			nonContainerDiscovered++
		}
	}

	tWorkers := time.Now()
	// Discovery deduplicates sources before their freshness checks and writes.
	// Cross-source parent-inventory demotions run after the workers finish.
	if e.soleArchiveWriter.Load() {
		ctx = e.withSourceFreshnessSnapshot(ctx, all)
	}
	results := e.startWorkers(ctx, all)
	stats = e.collectAndBatch(
		ctx, results, len(all), progressTotal, onProgress, writeMode,
	)
	if err := e.finishCodexParentInventoryBatch(ctx, &stats); err != nil {
		log.Printf("%v", err)
	}
	stats.logRetrySummary("sync")
	stats.providerFailures = providerFailures
	for range providerFailures {
		stats.RecordFailed()
	}
	// Discovery failures cannot be attributed to a provider here, so any
	// failure conservatively poisons every captured verification this
	// pass. Only unscoped passes discovered every root, so only they may
	// drop trusted entries for containers that produced no sources.
	if stats.Aborted || ctx.Err() != nil || providerFailures > 0 {
		e.poisonSQLiteContainerPass()
	}
	e.finishSQLiteContainerPass(ctx, false, scope == nil)
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
		if writeMode != syncWriteBulk {
			e.reportProgress(onProgress, dbProgress)
		}
	}

	if ctx.Err() != nil {
		stats.Aborted = true
		return stats
	}
	e.reportFinalizingProgress(
		onProgress, writeMode, finalizingDBBackedDetail,
	)

	// OpenCode-format sessions (OpenCode and its Kilo and MiMoCode
	// forks) are provider-authoritative: discovery and parsing flow
	// through the provider facade in the file-sync phase above, so no
	// dedicated DB-backed sync pass is needed here.

	// Sync Warp, Forge, Piebald, ZCode, Goose, and Crush sessions. These are
	// provider-authoritative DB-backed providers: a shared SQLite DB hosts every
	// session, so the provider facade enumerates sources and parses only the
	// changed ones.
	if scope.includesAny(e.sources().agentDirs[parser.AgentWarp]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentWarp, "warp",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.sources().agentDirs[parser.AgentForge]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentForge, "forge",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.sources().agentDirs[parser.AgentPiebald]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentPiebald, "piebald",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.sources().agentDirs[parser.AgentZCode]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentZCode, "zcode",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.sources().agentDirs[parser.AgentGoose]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentGoose, "goose",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	if scope.includesAny(e.sources().agentDirs[parser.AgentCrush]) {
		if e.syncProviderDBBackedAgent(
			ctx, parser.AgentCrush, "crush",
			writeMode, verbose, scope, &stats, advanceDBProgress,
		) {
			stats.Aborted = true
			return stats
		}
	}
	// Link subagent child sessions to their parents after all DB-backed
	// agent writes (including provider-authoritative Crush, Forge, Goose,
	// Piebald, and ZCode).
	// LinkSubagentSessions is idempotent — its WHERE filter and partial index
	// make it a cheap no-op when nothing new was written — so no guard is
	// needed.
	e.reportFinalizingProgress(
		onProgress, writeMode, finalizingAllLinksDetail,
	)
	linked, err := e.linkSubagentSessions(ctx)
	if err != nil {
		log.Printf("link subagent sessions: %v", err)
		stats.RecordFailed()
	} else {
		stats.RecordLinksUpdated(linked)
	}
	e.reconcileNativeTitlesLocked(ctx, &stats)

	e.reportFinalizingProgress(
		onProgress, writeMode, finalizingSkipCacheDetail,
	)
	tPersist := time.Now()
	skipCount := e.persistSkipCache(ctx)
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

	if since.IsZero() && scope == nil && e.archiveRebuildIndex == nil {
		stats.RecordLinksUpdated(e.reconcileCommitTrailersLocked(ctx))
	}
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

	if recordSyncState && stats.providerFailures == 0 &&
		stats.ProcessingComplete() {
		e.recordSyncFinished(ctx)
	}
	// Only a full, authoritative pass has visited every affected source.
	if recordSyncState && since.IsZero() && !e.ephemeral &&
		startupReconciliationSucceeded(ctx, stats, nil) {
		if err := e.db.CompleteScopedUpgrade(ctx); err != nil {
			log.Printf("persist scoped upgrade completion: %v", err)
		}
	}
	// Emission happens in SyncAll / SyncAllSince after syncMu is
	// released; syncAllLocked runs under the caller's lock.
	return stats
}

// recordSyncStarted persists the start time of a sync run
// into pg_sync_state. Callers use this to compute mtime
// cutoffs for future quick incremental syncs.
func (e *Engine) recordSyncStarted(ctx context.Context) {
	if e.ephemeral {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.db.SetSyncState(ctx, syncStateStartedAt, ts); err != nil {
		log.Printf("persist sync start time: %v", err)
	}
}

// recordSyncFinished persists the finish time of a completed
// sync run. Only called on successful completion (not on
// cancellation or abort).
func (e *Engine) recordSyncFinished(ctx context.Context) {
	if e.ephemeral {
		return
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	if err := e.db.SetSyncState(ctx, syncStateFinishedAt, ts); err != nil {
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
	staleIdentities := e.staleDataVersionIdentitySet(ctx)
	for _, f := range files {
		if f.ForceParse {
			out = append(out, f)
			continue
		}
		if f.Agent == parser.AgentOpenClaw {
			if _, _, member := parser.ParseVirtualSourcePathForBase(f.Path, "openclaw-agent.sqlite"); member {
				// Edits and deletions need not advance event timestamps.
				// Discovery already computed each member's fingerprint;
				// let normal freshness checks skip unchanged members.
				out = append(out, f)
				continue
			}
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
			sourceCwd = e.sourceCwdDecision(ctx, *f.ProviderSource)
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
		if e.pathInStaleIdentitySet(staleIdentities, f.Agent, f.Path) &&
			e.staleSourceReparseAdmitted(
				sourceCwdParticipating, sourceCwd,
			) {
			// Parser upgrades and cwd reconciliation must bypass the cutoff,
			// including unchanged S3 objects with stale archived output.
			out = append(out, f)
			continue
		}
		if isS3SourcePath(f.Path) && e.s3SourceMetadataChanged(ctx, f) {
			out = append(out, f)
			continue
		}
		if usesCompositeSidecarFreshness(f.Agent, f.Path) {
			rawSessionID := claudeSessionIDFromPath(f.Path)
			sessionID := applyIDPrefixToID(
				e.idPrefix,
				claudeFormatArchiveSessionID(f.Agent, rawSessionID),
			)
			if e.icodemateCLIDeletedCompanionMtime(ctx,
				sessionID, f.Path, mtime,
			) >= cutoffNs {
				out = append(out, f)
				continue
			}
		}
		if f.Agent != parser.AgentCodex {
			continue
		}
		var indexNeedsRefresh bool
		if isS3SourcePath(f.Path) {
			indexNeedsRefresh = e.s3CodexIndexNeedsRefreshSince(
				ctx, f, cutoffNs,
			)
		} else {
			indexNeedsRefresh = e.codexIndexNeedsRefreshSince(ctx,
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
		out = append(out, pickPreferredCodexDiscoveredFile(ctx, e.db, candidates))
	}
	return out
}

// icodemateCLIDeletedCompanionMtime recovers the project-directory deletion
// signal only when the committed source metadata proves that the now-missing
// per-session directory contributed to the last fingerprint. This avoids
// making unrelated sibling-session changes refresh every CLI transcript.
func (e *Engine) icodemateCLIDeletedCompanionMtime(ctx context.Context,
	sessionID, path string, currentMtime int64,
) int64 {
	transcriptInfo, err := os.Stat(path)
	if err != nil {
		return currentMtime
	}
	companionDir := strings.TrimSuffix(path, filepath.Ext(path))
	if _, err := os.Stat(companionDir); err == nil || !errors.Is(err, os.ErrNotExist) {
		return currentMtime
	}
	storedSize, storedMtime, ok := e.db.GetSessionFileInfo(ctx, sessionID)
	if !ok || (storedSize == transcriptInfo.Size() &&
		storedMtime == transcriptInfo.ModTime().UnixNano()) {
		return currentMtime
	}
	projectInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return currentMtime
	}
	return max(currentMtime, projectInfo.ModTime().UnixNano())
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
		return discoveredFileMtime(ctx, file)
	}
	// S3 objects are discovered through the provider facade (so they carry a
	// ProviderSource), but providers read local files and cannot Fingerprint an
	// s3:// URI. Routing them through providerSourceMtime below would error, and
	// filterFilesByMtime keeps any file whose mtime cannot be resolved, defeating
	// the incremental cutoff and reprocessing every old S3 object on each sync.
	// The threaded object metadata (or a HEAD stat) gives the timestamp directly.
	if isS3SourcePath(file.Path) {
		return discoveredFileMtime(ctx, file)
	}
	// Copilot store changes must pass the cutoff even when the transcript is
	// old. Parent timestamps expose deletions; ctime catches restored mtimes.
	// Keep these stat-only signals separate from persisted session mtimes.
	if file.Agent == parser.AgentCopilot {
		mtime, err := discoveredFileMtime(ctx, file)
		if err != nil {
			return 0, err
		}
		root := filepath.Dir(filepath.Dir(file.Path))
		if filepath.Base(file.Path) == "events.jsonl" {
			root = filepath.Dir(root)
		}
		storePath := filepath.Join(root, "session-store.db")
		for _, path := range []string{storePath, storePath + "-wal", root} {
			info, err := os.Stat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return 0, err
			}
			changeTime, _ := fileChangeTime(path, info)
			mtime = max(mtime, info.ModTime().UnixNano(), changeTime)
		}
		return mtime, nil
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
	if file.Agent == parser.AgentCline {
		info, err := os.Stat(file.Path)
		if err != nil {
			return 0, err
		}
		_, mtime := clineEffectiveStat(file.Path, info)
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
	// ICodeMate CLI fingerprints hash transcript and sidecar contents. The
	// incremental cutoff needs only their stat metadata, including sidecar
	// directory mtimes so deletions remain visible without hashing every
	// unchanged transcript during each polling pass.
	if usesCompositeSidecarFreshness(file.Agent, file.Path) {
		return parser.ClaudeLayoutCompositeMtime(file.Path)
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
	return discoveredFileMtime(ctx, file)
}

// providerSourceMtime resolves a provider-sourced file's effective mtime through
// the owning provider's Fingerprint. The boolean reports whether the provider
// runtime produced a usable timestamp; a false result tells the caller to fall
// back to the legacy mtime path.
func (e *Engine) providerFingerprint(ctx context.Context, provider parser.Provider, source parser.SourceRef) (parser.SourceFingerprint, error) {
	if reusable, ok := provider.(parser.StoredFingerprintProvider); ok {
		return reusable.FingerprintWithStored(ctx, source, func(path string) (string, bool) {
			if e.pathRewriter != nil {
				path = e.pathRewriter(path)
			}
			return e.db.GetFileHashByAgentPath(ctx, path, string(provider.Definition().Type))
		})
	}
	return provider.Fingerprint(ctx, source)
}

func (e *Engine) providerSourceMtime(
	ctx context.Context, file parser.DiscoveredFile,
) (int64, bool, error) {
	if file.ProviderSource == nil {
		return 0, false, nil
	}
	factory, ok := e.sources().providerFactories[file.Agent]
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
		Roots:        e.sources().agentDirs[file.Agent],
		Machine:      e.machine,
		PathRewriter: e.pathRewriter,
	})
	fingerprint, err := e.providerFingerprint(ctx, provider, source)
	if err != nil {
		return 0, false, err
	}
	if fingerprint.MTimeNS == 0 {
		return 0, false, nil
	}
	return fingerprint.MTimeNS, true, nil
}

func discoveredFileMtime(ctx context.Context,
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
			return parser.KiroSQLiteSourceMtime(ctx, file.Path)
		}
	}
	if isOpenCodeFormatStorageAgent(file.Agent) {
		if isOpenCodeFormatSQLiteVirtualPath(file.Agent, file.Path) ||
			isOpenCodeFormatStoragePath(file.Agent, file.Path) {
			return openCodeFormatSourceMtime(ctx,
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

func (e *Engine) effectiveSourcePath(path string) string {
	if e.pathRewriter != nil {
		return e.pathRewriter(path)
	}
	return path
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
	roots := make([]string, 0, len(e.sources().agentDirs[agent]))
	for _, dir := range e.sources().agentDirs[agent] {
		if dir == "" || !scope.includes(dir) {
			continue
		}
		roots = append(roots, dir)
	}
	if len(roots) == 0 {
		return 0, 0, nil
	}
	factory, ok := e.sources().providerFactories[agent]
	if !ok || factory == nil {
		return 0, 0, nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:          roots,
		Machine:        e.machine,
		SourceMachines: e.sources().sourceMachines[agent],
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
	piebaldDiscovered := make(map[string]struct{})
	piebaldPruneRoots := roots
	if agent == parser.AgentPiebald {
		piebaldPruneRoots = piebaldAuthoritativeRoots(roots, os.Stat)
	}
	err := discoverer.DiscoverEach(ctx, func(source parser.SourceRef) error {
		discovered++
		if agent == parser.AgentPiebald {
			if key, _, ok := piebaldFailureSourcePaths(source); ok {
				piebaldDiscovered[key] = struct{}{}
			}
		}
		var piebaldFailure piebaldFailureLookup
		piebaldRetry := false
		if agent == parser.AgentPiebald {
			var found bool
			piebaldFailure, found = e.preparePiebaldFailure(ctx, source, e.forceParseBypassesCache(parser.DiscoveredFile{}))
			if found {
				if piebaldFailure.err != nil {
					return nil //nolint:nilerr // Skip the unchanged failure without promoting its source baseline.
				}
				piebaldRetry = piebaldFailure.retry
			}
		}
		fingerprint, err := e.providerFingerprint(ctx, provider, source)
		if err != nil {
			log.Printf("sync %s fingerprint: %v", agent, err)
			sourceFailures++
			return nil
		}
		machine := e.machineForProviderSource(
			agent, source, providerDiscoveredPath(source),
		)
		if !piebaldRetry && e.providerDBBackedSourceFresh(
			ctx, agent, source, fingerprint,
		) {
			return queueBaseline(source)
		}
		file := parser.DiscoveredFile{Path: providerDiscoveredPath(source), Agent: agent}
		failureIdentity, failureIdentityOK := e.sourceFailureCacheIdentity(file, fingerprint)
		if failureIdentityOK {
			if _, cached := e.cachedSourceFailure(ctx, file, failureIdentity); cached {
				return nil
			}
		}
		outcome, err := provider.Parse(ctx, parser.ParseRequest{
			Source:      source,
			Fingerprint: fingerprint,
			Machine:     machine,
			ForceParse:  e.forceParse || e.forceFullParse || piebaldRetry,
		})
		if err != nil {
			if agent == parser.AgentPiebald &&
				!e.forceParse && !e.forceFullParse {
				e.rememberPiebaldParseFailure(
					ctx, source, piebaldFailure, err,
				)
			}
			if ctx.Err() == nil && failureIdentityOK && sourceParseFailureCacheable(err, file.Path) {
				e.failures.Record(providerAgentSkipCacheKey(file.Path, agent), failureIdentity)
			}
			log.Printf("sync %s parse: %v", agent, err)
			sourceFailures++
			return nil
		}
		if agent == parser.AgentPiebald {
			e.clearPiebaldFailure(source)
		}
		e.failures.Clear(providerAgentSkipCacheKey(file.Path, agent))
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
	if agent == parser.AgentPiebald {
		e.prunePiebaldFailures(piebaldPruneRoots, piebaldDiscovered)
	}
	return discovered, sourceFailures, nil
}

// providerDBBackedSourceFresh reports whether a DB-backed provider source is
// already stored at the current data version with an unchanged source mtime
// and, when required by the provider, an unchanged content hash. This is the
// change-detection half of the legacy *PendingSessionIDs helpers.
func (e *Engine) providerDBBackedSourceFresh(ctx context.Context,
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
	_, storedMtime, ok := e.db.GetFileInfoByAgentPath(ctx,
		lookupPath, string(agent),
	)
	if !ok {
		return false
	}
	if storedMtime != fingerprint.MTimeNS {
		return false
	}
	if factory, ok := e.sources().providerFactories[agent]; ok && factory != nil &&
		!e.providerFingerprintHashMatchesDB(ctx,
			agent,
			lookupPath,
			fingerprint,
			factory.Capabilities().Sync.FingerprintHashRequiredForFreshness,
		) {
		return false
	}
	return e.db.GetDataVersionByAgentPath(ctx,
		lookupPath, string(agent),
	) >= db.CurrentDataVersion()
}

// syncProviderDBBackedAgent runs the full-sync phase for a provider-authoritative
// DB-backed agent (Crush, Forge, Goose, Piebald, Warp, ZCode). It mirrors
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
	resolveWorktreeProject := e.loadWorktreeProjectResolverContext(ctx)
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
				batchWritten, batchMessages, failedWrites, cwdFiltered := e.writeBatchOverride(pending, writeMode, true)
				outcome = writeBatchOutcome{
					writtenSessions: batchWritten,
					writtenMessages: batchMessages,
					failedSessions:  failedWrites,
					cwdFiltered:     cwdFiltered,
				}
				complete = failedWrites == 0 && cwdFiltered == 0 &&
					batchWritten == len(pending)
			} else {
				outcome = e.writeBatchWithOutcomeContext(ctx, pending, writeMode, true)
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
					err = e.writeSessionFullWithResolver(ctx, pw, resolveWorktreeProject)
				} else {
					err = e.writeSessionFullWithResolver(ctx, pw, e.loadWorktreeProjectResolverContext(ctx))
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
		// Retain linking if cancellation skips the final full-sync pass.
		e.subagentLinkPending = e.subagentLinkPending || written > 0
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
	// Children of one S3 Codex parent reuse its download while the pool
	// runs; a caller that already scoped a cache to its pass keeps it.
	ctx, closeS3ParentCache := withS3CodexParentCache(ctx)
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
					containerPath:  result.sqliteContainerResultPath,
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
		closeS3ParentCache()
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

// beginBulkRetentionPass installs the byte-weighted bulk retention budget for
// the duration of an archive-scale pass and returns the restore func the caller
// must defer. The budget bounds active and queued parse results; once the
// collector owns a result, a separate byte limit bounds pending database
// writes without coupling parser admission to transaction size. The caller
// holds syncMu, so no other pass can observe the switched budget.
func (e *Engine) beginBulkRetentionPass() func() {
	e.bulkRetentionOnce.Do(func() {
		if e.bulkRetentionBudget == nil {
			e.bulkRetentionBudget = newBulkParseRetentionBudget(
				defaultBulkParseRetentionBytes,
			)
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
	observeResult func(syncJob)
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
	var pendingBytes int64
	var pendingLeases []*parseRetentionLease
	var pendingStaged []*codexStagingSink
	var pendingStagedGC []func()
	var pendingCacheWrites []skipCacheWrite
	var pendingRetentionBytes int64
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
		resolvedCandidates, resolvedAdmitted, err := e.expandSourceBaselinesByStoredAttribution(
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
			e.rejectSkipCacheWrites(baselineCtx, cacheWrites)
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
				if ids, err := e.db.ListSessionIDsByFilePath(ctx, path, string(candidateAgent)); err == nil && len(ids) > 0 {
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
			e.rejectSkipCacheWrites(ctx, []skipCacheWrite{write})
			return
		}
		source, ok := baselineSourceForJob(job)
		if !ok {
			e.rejectSkipCacheWrites(ctx, []skipCacheWrite{write})
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
		func() {
			defer releaseParseRetentionLeases(pendingLeases)
			defer closeCodexStagingSinks(pendingStaged)
			defer releaseStagedGCGuards(pendingStagedGC)
			if ctx.Err() != nil && e.discardWritesOnCancel {
				return
			}
			completionCtx := ctx
			if !e.discardWritesOnCancel {
				completionCtx = context.WithoutCancel(ctx)
			}
			var outcome writeBatchOutcome
			if e.writeBatchOverride != nil {
				writtenSessions, writtenMessages, failedSessions, cwdFiltered := e.writeBatchOverride(pending, writeMode, false)
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
		// The collector reuses these backing arrays for the next batch. Clear
		// pointer-bearing entries before reslicing so the completed batch's
		// parsed messages and source metadata become collectible immediately,
		// rather than remaining live until the next batch overwrites every slot
		// or the whole pass returns.
		clear(pending)
		pending = pending[:0]
		clear(pendingLeases)
		pendingBytes = 0
		pendingLeases = pendingLeases[:0]
		clear(pendingCacheWrites)
		clear(pendingStaged)
		pendingStaged = pendingStaged[:0]
		clear(pendingStagedGC)
		pendingStagedGC = pendingStagedGC[:0]
		pendingCacheWrites = pendingCacheWrites[:0]
		pendingRetentionBytes = 0
	}

	budget := e.retentionBudget()
	archiveRetention := budget.pendingCapacity > 0
	pendingRetentionLimit := budget.pendingCapacity
	defer func() {
		if writeMode == syncWriteBulk && budget.scavengePending.Load() {
			e.reportFinalizingProgress(
				onProgress, writeMode, finalizingMemoryDetail,
			)
		}
		budget.scavengeIfNeeded()
	}()
	for i := range total {
		var r syncJob
		for {
			var pressure <-chan struct{}
			if !archiveRetention && len(pending) > 0 {
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
			r.releaseAll()
			drainResults(results, total-i-1)
			goto flush
		}
		resultRetentionBytes := int64(0)
		if archiveRetention && r.retentionLease != nil {
			resultRetentionBytes = r.retentionLease.retainedBytes
		}

		if r.err != nil {
			// Workers emit ctx.Err() for files skipped
			// after cancellation — treat the same as the
			// ctx.Done() branch above.
			if ctx.Err() != nil {
				stats.Aborted = true
				r.releaseAll()
				drainResults(results, total-i-1)
				goto flush
			}
			stats.RecordFailed()
			if r.sourceCwdChanged && e.deferredSourceCwd == nil {
				stats.RecordCwdUpdated(1)
			}
			e.noteSQLiteContainerResult(r.containerResultPath(), false)
			if r.cacheFailure {
				e.failures.Record(r.failureCacheKey, r.failureIdentity)
			}
			if r.cacheSkip && r.mtime != 0 && !r.noCacheSkip {
				e.cacheSkip(r.skipCacheKey(), r.mtime, r.sourceFingerprint)
			}
			log.Printf("sync error: %v", r.err)
			r.releaseAll()
			continue
		}
		if len(r.excludedSessionIDs) > 0 || len(r.sourceMissingMembers) > 0 {
			e.markRetryUnsafeSkipSource(r.path)
		}
		for range r.providerFailureCount {
			stats.RecordFailed()
		}
		if isCodexFormatAgent(r.agent) && r.deferredCount > 0 && !slices.Contains(stats.Warnings, codexForkUnresolvedWarning) {
			stats.Warnings = append(stats.Warnings, codexForkUnresolvedWarning)
		}
		for range r.deferredCount {
			stats.recordDeferred(r.path)
		}
		for _, cause := range r.retryCauses {
			cause.sessionID = applyIDPrefixToID(e.idPrefix, cause.sessionID)
			stats.recordRetryCause(cause)
		}
		proofWithheld := r.sourceProofWithheld(false)
		if proofWithheld {
			if tracker := reconciliationBaselineTrackerFor(ctx); tracker != nil {
				tracker.addNonAuthoritativeScope(r.agent, r.path)
			}
		}
		if r.skip {
			cwdChanged, err := e.reconcileSkippedSourceCwd(ctx, r)
			if err != nil {
				log.Printf("reconcile skipped source cwd: %v", err)
				stats.RecordFailed()
				e.noteSQLiteContainerResult(r.containerResultPath(), false)
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
			rowlessCached := !proofWithheld && e.cacheClaudeRowlessFreshness(ctx, r)
			if !rowlessCached && r.cacheSkip && r.mtime != 0 &&
				!r.noCacheSkip {
				e.cacheSkip(r.skipCacheKey(), r.mtime)
			}
			stats.RecordSkip()
			e.noteSQLiteContainerResult(r.containerResultPath(), !proofWithheld)
			if !proofWithheld && !r.suppressPresenceSweep {
				admitted, exactOwnerships, err := e.skippedSourceAllowsCwdFilter(ctx, r)
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
			r.releaseAll()
			continue
		}
		completionCtx := ctx
		if !e.discardWritesOnCancel {
			completionCtx = context.WithoutCancel(ctx)
		}
		if err := e.validateSourceSessionOwnership(completionCtx, r.processResult); err != nil {
			log.Printf("validate source session ownership: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.containerResultPath(), false)
			r.releaseAll()
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
		children, err := e.db.SubagentChildSessionIDs(completionCtx, append(
			append([]string{}, excluded...), resultIDs...,
		))
		if err != nil {
			log.Printf("list pre-write subagent children: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.containerResultPath(), false)
			r.releaseAll()
			continue
		}
		// Persist affected IDs before any exclusion or replacement can
		// cascade their only spawn edge away. The queue is cleared only in
		// the same transaction that successfully repairs the hierarchy.
		if err := e.db.QueueSubagentParentCleanupRepairs(completionCtx, children); err != nil {
			log.Printf("queue subagent parent repairs: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.containerResultPath(), false)
			r.releaseAll()
			continue
		}
		atomicDAG := sourceRequiresAtomicDAGCompletion(
			r.agent, len(r.results),
		)
		var sourceCompletionSkipped map[string]bool
		if atomicDAG {
			activeResultIDs, skipped := e.partitionIntentionalSourceSkips(ctx, resultIDs)
			sourceCompletionSkipped = skipped
			staleVersion := max(db.CurrentDataVersion()-1, 0)
			if err := e.db.SetExistingSessionDataVersions(ctx,
				activeResultIDs, staleVersion,
			); err != nil {
				e.clearProviderSourceFreshness(ctx, r.providerStatHash)
				log.Printf("stage DAG source data versions: %v", err)
				stats.RecordFailed()
				e.noteSQLiteContainerResult(r.containerResultPath(), false)
				r.releaseAll()
				continue
			}
		}
		excludedSessionIDs, err := e.deleteParserExcludedSessions(ctx,
			r.processResult, sourceAllowsParserExclusions,
		)
		if err != nil {
			log.Printf("delete parser-excluded sessions: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.containerResultPath(), false)
			r.releaseAll()
			continue
		}
		if len(excludedSessionIDs) > 0 {
			stats.parserExcludedIDs = append(
				stats.parserExcludedIDs, excludedSessionIDs...,
			)
		}
		// Virtual members that vanished from a still-existing shared container
		// are marked source-missing with their exact source ownership, matching
		// the reconciliation audit, instead of being hidden or hard-deleted.
		// The cwd-filter freeze is judged per member against the
		// archived cwd (missingMemberTombstoneAllowed), not source-wide:
		// unchanged survivors are dropped from r.results before this
		// point, so the source-wide gate would freeze an allowed
		// member's deletion whenever everything else was unchanged.
		if len(r.sourceMissingMembers) > 0 {
			tombstoned, deferred, tombstoneErr := e.reconcileSourceMissingMembers(
				ctx, r.sourceMissingMembers,
				baselineExactOwnership, rejectExactOwnership,
			)
			stats.Tombstoned += tombstoned
			if tombstoneErr != nil {
				log.Printf(
					"tombstone source-missing members: %v", tombstoneErr,
				)
				stats.RecordFailed()
				e.noteSQLiteContainerResult(r.containerResultPath(), false)
				r.releaseAll()
				continue
			}
			stats.sourceMissingArchiveMembers = append(
				stats.sourceMissingArchiveMembers, deferred...,
			)
		}
		if len(r.results) == 0 && r.incremental == nil {
			cwdChanged, err := e.reconcileSkippedSourceCwd(ctx, r)
			if err != nil {
				log.Printf("reconcile rowless source cwd: %v", err)
				stats.RecordFailed()
				e.noteSQLiteContainerResult(r.containerResultPath(), false)
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
			e.noteSQLiteContainerResult(r.containerResultPath(), !proofWithheld)
			if !proofWithheld &&
				sourceAllowsParserExclusions {
				baselineProcessedSource(r, true)
			} else if !proofWithheld {
				baselineProcessedSource(r, false)
			}
			progress.SessionsDone++
			e.reportProgress(onProgress, progress)
			r.releaseAll()
			continue
		}
		if r.cacheSkip {
			e.clearSkip(ctx, r.skipCacheKey())
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
		cwdChanged, err := e.reconcileFilteredSourceCwd(ctx, r.results, sourceCwd)
		if err != nil {
			log.Printf("reconcile filtered source cwd: %v", err)
			stats.RecordFailed()
			e.noteSQLiteContainerResult(r.containerResultPath(), false)
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
		presenceProofWithheld := r.sourceProofWithheld(vetoed > 0)
		sourceProofWithheld := r.sourceProofWithheld(false)
		e.noteSQLiteContainerResult(r.containerResultPath(), !presenceProofWithheld)
		if vetoed > 0 && len(allowed) == 0 {
			e.clearProviderSourceFreshness(ctx, r.providerStatHash)
			// Claude can emit a synthetic base result for a replay-only
			// transcript. If the active CWD filter rejects that result while a
			// stale fork is also preserved, record the successful complete parse
			// under a filter-scoped marker. This restores steady-state freshness
			// without letting a later filter configuration inherit the proof.
			if !sourceProofWithheld {
				e.cacheClaudeRowlessFreshness(ctx, r)
			}
			stats.cwdFilteredFiles++
			if !sourceProofWithheld {
				baselineProcessedSource(r, false)
			}
			progress.SessionsDone++
			e.reportProgress(onProgress, progress)
			r.releaseAll()
			continue
		}

		if r.incremental != nil {
			if err := e.writeIncremental(ctx, r.incremental); err != nil {
				log.Printf("%v", err)
				stats.RecordFailed()
				r.releaseAll()
				continue
			}
			stats.RecordSynced(1)
			if !sourceProofWithheld {
				baselineJob := r
				baselineJob.machine = r.incremental.machine
				baselineProcessedSource(baselineJob, true)
			}
			progress.MessagesIndexed += len(
				r.incremental.msgs,
			)
			stats.messagesIndexed = progress.MessagesIndexed
			r.releaseAll()
		} else {
			// An unresolved fork or low-fidelity Antigravity CLI parse cannot
			// authoritatively replace saved history. New sessions may still
			// publish fallback content and remain marked for retry.
			if (isCodexFormatAgent(r.agent) && r.deferredCount > 0) ||
				(r.agent == parser.AgentAntigravityCLI && len(r.retrySessionIDs) > 0) {
				var store db.Store = e.db
				if e.archiveStore != nil {
					store = e.archiveStore
				}
				kept := allowed[:0]
				for _, pr := range allowed {
					if isCodexFormatAgent(r.agent) && pr.Session.CodexFork != nil {
						// The shared writer reconciles explicit replay provenance atomically.
						kept = append(kept, pr)
						continue
					}
					if !r.needsRetryForSession(pr.Session.ID) {
						kept = append(kept, pr)
						continue
					}
					id := applyIDPrefixToID(e.idPrefix, pr.Session.ID)
					stored, err := savedHistory(ctx, store, id)
					if err != nil {
						cause := fmt.Sprintf("read saved history of deferred session %s: %v", id, err)
						log.Printf("sync: %s", cause)
						stats.RecordFailed()
						stats.abort(cause)
						continue
					}
					if stored == nil {
						kept = append(kept, pr)
					} else if e.archiveStore != nil {
						stats.deferredPreservedIDs = append(stats.deferredPreservedIDs, id)
						if r.agent == parser.AgentAntigravityCLI {
							stats.preservedRetryFailures++
						}
					} else if stored.DataVersion >= db.CurrentDataVersion() {
						if err := e.db.SetSessionDataVersion(ctx, id, max(db.CurrentDataVersion()-1, 0)); err != nil {
							cause := fmt.Sprintf("mark deferred session %s for retry: %v", id, err)
							log.Printf("sync: %s", cause)
							stats.RecordFailed()
							stats.abort(cause)
						}
					}
				}
				allowed = kept
				if len(allowed) == 0 {
					r.releaseAll()
					progress.SessionsDone++
					e.reportProgress(onProgress, progress)
					continue
				}
			}
			sourceNeedsRetry := presenceProofWithheld
			if resultRetentionBytes > 0 && len(pending) > 0 &&
				(pendingRetentionBytes >= pendingRetentionLimit ||
					resultRetentionBytes > pendingRetentionLimit-pendingRetentionBytes) {
				flushPending()
			}
			if archiveRetention {
				r.releaseRetention()
			}
			for i, pr := range allowed {
				sessionNeedsRetry := r.providerWideFailureCount > 0 ||
					r.needsRetryForSession(pr.Session.ID)
				pw := pendingWrite{
					sess:                    pr.Session,
					msgs:                    pr.Messages,
					usageEvents:             pr.UsageEvents,
					sourceBytes:             r.sourceBytes,
					checkpoint:              pr.Checkpoint,
					checkpointHashState:     pr.CheckpointHashState,
					checkpointAnchorDigest:  pr.CheckpointAnchorDigest,
					needsRetry:              sessionNeedsRetry || atomicDAG,
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
				if i == 0 && (atomicDAG || r.providerStatHash != nil) {
					// Claude-compatible providers can emit several DAG branches
					// from one transcript.
					// Carry their contiguous write count so the flush can make
					// one source-level completion decision. Other digest-backed
					// providers currently emit one result per source.
					pw.sourceWriteCount = 1
					if atomicDAG {
						pw.sourceWriteCount = len(allowed)
					}
					pw.providerStatHash = r.providerStatHash
					pw.sourceCompletionEligible = !sourceNeedsRetry
					pw.promoteSourceOnComplete = atomicDAG
				}
				if i == 0 {
					pw.staged = r.staged
				}
				pending = append(pending, pw)
				pendingBytes += pw.sourceBytes
				if runtimeMetrics != nil {
					runtimeMetrics.pendingWrites(len(pending))
				}
			}
			if archiveRetention && len(allowed) > 0 {
				pendingRetentionBytes = min(
					pendingRetentionLimit,
					pendingRetentionBytes+resultRetentionBytes,
				)
			} else if r.retentionLease != nil {
				pendingLeases = append(pendingLeases, r.retentionLease)
				r.retentionLease = nil
			}
			if r.staged != nil {
				pendingStaged = append(pendingStaged, r.staged)
				r.staged = nil
			}
			if r.stagedGCRelease != nil {
				pendingStagedGC = append(
					pendingStagedGC, r.stagedGCRelease,
				)
				r.stagedGCRelease = nil
			}
			if r.cacheAfterWrite && !sourceNeedsRetry {
				pendingCacheWrites = append(pendingCacheWrites, skipCacheWrite{
					agent:             r.agent,
					key:               r.skipCacheKey(),
					mtime:             r.mtime,
					sourceFingerprint: r.sourceFingerprint,
				})
			}
			if len(pending) >= batchSize ||
				(archiveRetention &&
					pendingRetentionBytes >= pendingRetentionLimit) ||
				(!archiveRetention && (budget.underPressure() ||
					pendingBytes >= parseBatchBytesLimit)) {
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
	if len(pending) > 0 {
		e.reportFinalizingProgress(
			onProgress, writeMode, finalizingSessionWritesDetail,
		)
	}
	flushPending()
	if ctx.Err() != nil && e.discardWritesOnCancel {
		return stats
	}
	e.reportFinalizingProgress(
		onProgress, writeMode, finalizingSourceStateDetail,
	)
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
		e.reportFinalizingProgress(
			onProgress, writeMode, finalizingFileLinksDetail,
		)
		linked, err := e.linkSubagentSessions(postWriteCtx)
		if err != nil {
			log.Printf("link subagent sessions: %v", err)
			stats.RecordFailed()
		} else {
			stats.RecordLinksUpdated(linked)
		}
	}
	e.reportFinalizingProgress(
		onProgress, writeMode, finalizingParentRepairDetail,
	)
	var repairProgress func(int, int)
	if writeMode == syncWriteBulk {
		repairProgress = func(done, total int) {
			e.reportSubagentRepairProgress(onProgress, done, total)
		}
	}
	repaired, err := e.db.RepairQueuedSubagentParentsContext(postWriteCtx, repairProgress)
	if err != nil {
		log.Printf("repair queued subagent parents: %v", err)
		stats.RecordFailed()
	} else {
		stats.RecordLinksUpdated(repaired)
	}

	// PhaseDone is emitted by syncAllLocked after the DB-backed
	// agents and the remaining pass epilogue finish.
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
		ids, err := e.db.ListSessionIDsByFilePath(ctx, path, agentStr)
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
	sourceWideAllowed, exactOwnerships, err := e.skippedSourceAllowsCwdFilter(ctx, job)
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

func (e *Engine) linkSubagentSessions(ctx context.Context) (int, error) {
	if runtimeMetrics := reconciliationRuntimeMetricsFor(ctx); runtimeMetrics != nil {
		runtimeMetrics.globalLinkPass()
	}
	updated, err := e.db.LinkSubagentSessionsContext(ctx)
	e.subagentLinkPending = err != nil
	if err != nil {
		return 0, err
	}
	return updated, nil
}

// drainResults consumes remaining items from the results
// channel so that worker goroutines can exit and be collected.
func drainResults(results <-chan syncJob, remaining int) {
	for range remaining {
		job := <-results
		job.releaseAll()
	}
}

// applyProviderFilePathPolicies reproduces the DB-aware, file-path-scoped
// session bookkeeping that a provider cannot do on its own (it has no database
// handle). It runs only for single-session-per-file providers whose canonical
// ID can change while the source path is unchanged (e.g. Vibe, whose ID flips
// between the meta.json session_id and the directory-name fallback as meta.json
// appears or is removed). Multi-session sources are skipped, where several
// distinct sessions legitimately share one path; for stable-ID providers it is
// a no-op because the stored ID always matches the freshly parsed one.
// Codebuff uses applyCodebuffClassificationPolicies instead.
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
	idPrefix string,
) {
	// Captured identities share paths; the archive policy validates each ID.
	if e.archiveSessionPolicy != nil {
		return
	}
	if len(res.results) == 0 {
		return
	}
	if provider.Capabilities().Source.MultiSessionSource == parser.CapabilitySupported {
		if agent == parser.AgentCodebuff {
			e.applyCodebuffClassificationPolicies(ctx, filePath, res)
			e.stageProviderStatHash(agent, filePath, res)
		}
		return
	}

	excluded := make(map[string]struct{}, len(res.excludedSessionIDs))
	for _, id := range applyIDPrefixToIDs(idPrefix, res.excludedSessionIDs) {
		excluded[id] = struct{}{}
	}
	// Source-missing ownership takes precedence over parser-exclusion cleanup
	// for Cline: a stored session whose member file vanished from a present
	// session directory must be tombstoned by the complete-result ownership
	// arm, never hard-deleted as a stale-row sibling.
	sourceMissing := make(map[string]struct{})
	if agent == parser.AgentCline {
		for _, member := range res.sourceMissingMembers {
			if id := applyIDPrefixToID(idPrefix, member.sessionID); id != "" {
				sourceMissing[id] = struct{}{}
			}
		}
	}
	addExclusion := func(id string) {
		if id == "" {
			return
		}
		if _, ok := excluded[id]; ok {
			return
		}
		if _, ok := sourceMissing[id]; ok {
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
		originalID := result.Session.ID
		admitted := e.cwdFilter.allows(sourceCwdForFilter(result.Session.Cwd, sourceCwdDecision{
			resolution: res.sourceCwdResolution,
			storedCwd:  res.sourceCwdStored,
			storedOK:   res.sourceCwdStoredOK,
		}))
		currentID, moved, err := e.sourceCollisionID(ctx, provider, lookupPath, &result.Session, admitted, idPrefix)
		if err != nil {
			// Ownership is unknown, so skip the source this pass and retry it.
			res.err = err
			res.noCacheSkip = true
			res.results = kept[:0]
			return
		}
		res.forceReplace = res.forceReplace || moved
		if currentID != originalID && res.retrySessionIDs[originalID] {
			res.retrySessionIDs[currentID] = true
		}
		currentPrefixedID := applyIDPrefixToID(idPrefix, currentID)

		agentsToQuery := []string{string(agent)}
		var existingIDs []string
		primaryErr := make(map[string]error, len(agentsToQuery))
		for _, agentStr := range agentsToQuery {
			ids, err := e.db.ListSessionIDsByFilePath(ctx, lookupPath, agentStr)
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
				ids, retryErr := e.db.ListSessionIDsByFilePath(ctx, lookupPath, agentStr)
				if retryErr != nil {
					continue
				}
				existingIDs = append(existingIDs, ids...)
				delete(primaryErr, agentStr)
			}
		}
		// Bail when any agent's identity lookup remains unsuccessful
		// after retry. Continuing with partial lifecycle information
		// can recreate a trashed identity or leave a stale identity
		// active for the same file.
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
			if e.db.HasTrashedSessionByFilePath(ctx, lookupPath, agentStr) {
				suppress = true
				break
			}
		}
		if !suppress {
			for id := range excluded {
				if id == currentID || id == currentPrefixedID {
					continue
				}
				if e.db.IsSessionExcluded(ctx, id) || e.db.IsSessionTrashed(ctx, id) {
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
				if e.db.IsSessionExcluded(ctx, id) || e.db.IsSessionTrashed(ctx, id) {
					suppress = true
					break
				}
			}
		}
		if suppress {
			// Keep a trashed current ID trashed rather than converting it to a
			// parser deletion; the upsert's trash guard already hides it.
			if (currentPrefixedID == "" || !e.db.IsSessionTrashed(ctx, currentPrefixedID)) &&
				!e.db.IsSessionTrashed(ctx, currentID) {
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
	e.stageProviderStatHash(agent, filePath, res)
}

// stageProviderStatHash stages the per-component provider_freshness digest for
// a source whose parse kept at least one result.
func (e *Engine) stageProviderStatHash(
	agent parser.AgentType, filePath string, res *processResult,
) {
	if len(res.results) > 0 && filePath != "" {
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
			if hasher, ok := e.sources().providerStatHashers[agent]; ok {
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
func (e *Engine) deleteParserExcludedSessions(ctx context.Context,
	res processResult,
	sourceAllowed bool,
) ([]string, error) {
	if !sourceAllowed {
		return nil, nil
	}

	excluded := e.applyIDPrefixToSessionIDs(res.excludedSessionIDs)
	if len(excluded) > 0 {
		if _, err := e.db.DeleteParserExcludedSessions(ctx, excluded); err != nil {
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

func reconciliationMemberIdentity(
	agent parser.AgentType, source parser.SourceRef,
) string {
	if source.ReconciliationIdentity != "" {
		return source.ReconciliationIdentity
	}
	return reconciliationSourceIdentity(agent, source)
}

func reconciliationSourceRankLower(
	candidate, stored parser.ReconciliationSourceRank,
) bool {
	if candidate.Class != stored.Class {
		return candidate.Class < stored.Class
	}
	if candidate.Recency != stored.Recency {
		return candidate.Recency < stored.Recency
	}
	return candidate.Path > stored.Path
}

func (e *Engine) storedSourceRanksHigher(
	ctx context.Context,
	provider parser.Provider,
	storedPath string,
	candidate parser.SourceRef,
) (bool, error) {
	resolver, resolves := provider.(parser.ReconciliationSourceResolver)
	ranker, ranks := provider.(parser.ReconciliationSourceRanker)
	if !resolves || !ranks || storedPath == "" {
		return false, nil
	}
	if e.pathRewriter != nil {
		if e.storedPathResolver == nil {
			return false, nil
		}
		resolvedPath, ok := e.storedPathResolver(storedPath)
		if !ok || resolvedPath == "" {
			return false, nil
		}
		storedPath = resolvedPath
	}
	stored, found, err := resolver.SourceForReconciliation(
		ctx, storedPath, candidate.ProjectHint,
	)
	if err != nil || !found {
		return false, err
	}
	return reconciliationSourceRankLower(
		ranker.ReconciliationSourceRank(candidate),
		ranker.ReconciliationSourceRank(stored),
	), nil
}

func (e *Engine) providerSourceLowerRanked(
	ctx context.Context,
	provider parser.Provider,
	agent parser.AgentType,
	candidate parser.SourceRef,
) (bool, error) {
	if _, ok := provider.(parser.ReconciliationSourceRanker); !ok {
		return false, nil
	}
	identity := reconciliationMemberIdentity(agent, candidate)
	if identity == "" {
		return false, nil
	}
	fullSessionID := applyIDPrefixToID(
		provider.Definition().IDPrefix, identity,
	)
	fullSessionID = applyIDPrefixToID(e.idPrefix, fullSessionID)
	storedPath := e.db.GetSessionFilePath(ctx, fullSessionID)
	return e.storedSourceRanksHigher(ctx, provider, storedPath, candidate)
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

// shouldSkipFile returns true when the file's size and mtime
// match what is already stored in the database (by session ID).
// This relies on mtime changing on any write, which holds for
// append-only session files under normal filesystem behavior.
// S3 callers pass an object fingerprint to guard same-size,
// same-timestamp rewrites on object stores with coarse mtimes.
func (e *Engine) shouldSkipFile(ctx context.Context,
	sessionID string, info os.FileInfo,
) bool {
	return e.shouldSkipFileWithPrefix(ctx, e.idPrefix, sessionID, info)
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
	storedSize, storedMtime, ok := e.db.GetFileInfoByAgentPath(ctx,
		lookupPath, string(agent),
	)
	if !ok {
		return false
	}
	statsMatch := storedSize == fingerprint.Size && storedMtime == fingerprint.MTimeNS
	if !statsMatch && (fingerprint.Hash == "" || !providerFingerprintHashEstablishesFreshness(agent)) {
		return false
	}
	var storedHash string
	if fingerprint.Hash != "" && (!statsMatch || semantics.FingerprintHashRequiredForFreshness) {
		storedHash, _ = e.db.GetFileHashByAgentPath(ctx, lookupPath, string(agent))
	}
	if !providerFingerprintMatchesStored(agent, fingerprint, storedSize, storedMtime, storedHash, semantics.FingerprintHashRequiredForFreshness) {
		return false
	}
	project, _ := e.db.GetProjectByAgentPath(ctx, lookupPath, string(agent))
	stored := db.VirtualContainerMemberFreshness{
		Project:     project,
		DataVersion: e.db.GetDataVersionByAgentPath(ctx, lookupPath, string(agent)),
	}
	fresh := providerSourceMetadataFresh(stored)
	if fresh {
		e.stampProviderStatHashForConfirmedSource(ctx, preParseStatHash)
	}
	return fresh
}

func providerSourceMetadataFresh(stored db.VirtualContainerMemberFreshness) bool {
	return stored.DataVersion >= db.CurrentDataVersion() &&
		!parser.NeedsProjectReparse(stored.Project)
}

func providerFingerprintMatchesStored(agent parser.AgentType, fingerprint parser.SourceFingerprint, size, mtime int64, hash string, required bool) bool {
	if size != fingerprint.Size || mtime != fingerprint.MTimeNS {
		return fingerprint.Hash != "" && providerFingerprintHashEstablishesFreshness(agent) && hash == fingerprint.Hash
	}
	return fingerprint.Hash == "" || !required || hash == fingerprint.Hash
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
	if !e.providerStatHashMetadataVerified(*statHash) {
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

func (e *Engine) providerFingerprintHashMatchesDB(ctx context.Context,
	agent parser.AgentType,
	lookupPath string,
	fingerprint parser.SourceFingerprint,
	required bool,
) bool {
	if fingerprint.Hash == "" || !required {
		return true
	}
	storedHash, ok := e.db.GetFileHashByAgentPath(ctx,
		lookupPath, string(agent),
	)
	return ok && providerFingerprintMatchesStored(agent, fingerprint, fingerprint.Size, fingerprint.MTimeNS, storedHash, required)
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
	return agent == parser.AgentHermes || agent == parser.AgentAugureDesktop
}

// shouldSkipByPath checks file size and mtime against what is
// stored in the database by file_path. Used for codex/gemini
// files where the session ID requires parsing.
func (e *Engine) shouldSkipByPath(ctx context.Context,
	path string, info os.FileInfo,
) bool {
	if e.forceParse || e.forceFullParse {
		return false
	}
	lookupPath := path
	if e.pathRewriter != nil {
		lookupPath = e.pathRewriter(path)
	}
	storedSize, storedMtime, ok := e.db.GetFileInfoByPath(ctx, lookupPath)
	if !ok {
		return false
	}
	if storedSize != info.Size() ||
		storedMtime != info.ModTime().UnixNano() {
		return false
	}
	if e.db.GetDataVersionByPath(ctx, lookupPath) <
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
	// A source-missing primary must read as unavailable here: it cannot
	// vouch for the source (shouldSkipFile ignores it), so only the rowless
	// marker can prove an unchanged source that no longer admits a primary.
	primaryPath := e.db.GetSessionFilePathNotSourceMissing(ctx, fullID)
	if primaryPath == "" {
		fresh, contentVerified := e.claudeSourceFreshWithoutPrimary(
			ctx, lookupPath, statPath, info, "",
		)
		if !fresh {
			return 0, false, false, false
		}
		if e.providerIncrementalIdentityChanged(ctx, lookupPath, info) {
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
	if !e.shouldSkipFile(ctx, sessionID, info) {
		return 0, false, false, false
	}
	// Claude transcripts can fan out into several active DAG sessions. The
	// filename stem identifies only the main row, so its current version cannot
	// hide a stale restored fork that shares the same source path. Forks the CWD
	// filter intentionally preserves are the exception: the filter-scoped
	// cleanup guard below re-evaluates them on every pass.
	_, sourceDataVersion, _, _, sourceFound := e.db.GetSourceRepairStateByAgentPath(ctx,
		lookupPath, string(parser.AgentClaude),
	)
	if !sourceFound ||
		(sourceDataVersion < db.CurrentDataVersion() &&
			!e.claudeSourceStaleRowsArePreservedForks(
				ctx, lookupPath, true,
			)) {
		return 0, false, false, false
	}
	if e.providerIncrementalIdentityChanged(ctx, lookupPath, info) {
		return 0, false, true, false
	}
	contentChanged, contentVerified := e.providerIncrementalContentChanged(ctx,
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

func (e *Engine) providerIncrementalIdentityChanged(ctx context.Context,
	lookupPath string,
	info os.FileInfo,
) bool {
	if e.pathRewriter != nil {
		// Remote imports rewrite per-run temp paths to stable source paths;
		// the temp inode is expected to change between identical downloads.
		return false
	}
	curInode, curDevice := getFileIdentity(lookupPath, info)
	return e.db.FileIdentityChanged(ctx, lookupPath, curInode, curDevice)
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
func (e *Engine) providerIncrementalContentChanged(ctx context.Context,
	fullID, hashPath string,
	info os.FileInfo,
) (changed, verified bool) {
	storedHash, ok := e.db.GetSessionFileHash(ctx, fullID)
	if !ok || storedHash == "" {
		return false, false
	}
	curHash, err := e.contentCheckHash(hashPath, info.Size())
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
func (e *Engine) providerStatFreshnessMtime(
	agent parser.AgentType,
	lookupPath string,
	chatInfo os.FileInfo,
) int64 {
	switch agent {
	case parser.AgentCodebuff, parser.AgentFreebuff:
		return parser.CodebuffCompanionMtime(lookupPath, chatInfo)
	case parser.AgentCodex:
		return e.codexMetadata().EffectiveMtime(
			lookupPath, chatInfo.ModTime().UnixNano(),
		)
	default:
		return chatInfo.ModTime().UnixNano()
	}
}

// providerStatDigestEligible reports whether this engine may stage,
// stamp, or consult a provider_freshness stat digest for the agent. The
// content-hashing JSONL providers (Claude, Codex family, Evener) are
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
	return agent != parser.AgentClaude && agent != parser.AgentEvener && !isCodexFormatAgent(agent)
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
func (e *Engine) providerFreshDigestSourceCurrentInDB(ctx context.Context,
	agent parser.AgentType, lookupPath string,
) bool {
	rowFound := false
	snap := sourceFreshnessSnapshotFrom(ctx)
	for _, owned := range parser.ProviderStoredAgents(agent) {
		project, dataVersion, active, ok := snap.State(string(owned), lookupPath)
		if !ok {
			if _, _, ok := e.db.GetFileInfoByAgentPath(ctx,
				lookupPath, string(owned),
			); !ok {
				continue
			}
			project, dataVersion, _, _, active = e.db.GetSourceRepairStateByAgentPath(ctx,
				lookupPath, string(owned),
			)
		}
		rowFound = true
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
		stored, hasStored := sourceFreshnessSnapshotFrom(ctx).StatHash(string(file.Agent), lookupPath)
		var hashErr error
		if !hasStored {
			stored, hasStored, hashErr = e.db.GetProviderStatHash(ctx, file.Agent, lookupPath)
		}
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
			if !e.providerFreshDigestSourceCurrentInDB(ctx,
				file.Agent, lookupPath,
			) {
				return 0, false
			}
			return e.providerStatFreshnessMtime(file.Agent, lookupPath, info), true
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
		if e.shouldSkipByPath(ctx, path, effectiveInfo) {
			return mtime, true
		}
	// Gemini is deliberately absent here. Its fingerprint hash folds in the
	// session's resolved project name, so a pre-fingerprint skip keyed only on
	// the session file's size and mtime would skip a session whose project
	// metadata changed while the transcript did not, leaving a stale project
	// on scheduled syncs. Gemini relies on the post-fingerprint DB hash check
	// instead (providerFingerprintHashRequiredForFreshness), which catches a
	// resolved-project change even when size and mtime are unchanged.
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
		if e.shouldSkipByPath(ctx, path, effectiveInfo) {
			return mtime, true
		}
	case parser.AgentCline:
		size, mtime := clineEffectiveStat(path, info)
		effectiveInfo := fakeSnapshotInfo{
			fSize:  size,
			fMtime: mtime,
		}
		if e.shouldSkipByPath(ctx, path, effectiveInfo) {
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
		if e.shouldSkipByPath(ctx, path, effectiveInfo) {
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
		if e.shouldSkipByPath(ctx, path, effectiveInfo) {
			return mtime, true
		}
	default:
		// Other providers require fingerprinting before a freshness decision.
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

// discardStaleSQLiteProviderSource removes discovery-carried metadata when
// the container pass no longer holds a live capture: the pass failed its
// recapture, or the container changed after it. The recheck stats the live
// container, so a write landing between the post-discovery recapture and
// this worker cannot leave a stale full-digest source whose pre-change
// child digest matches the archived fingerprint and skips the changed
// session. The provider must resolve the current source before any
// freshness gate can inspect its fingerprint.
func (e *Engine) discardStaleSQLiteProviderSource(file *parser.DiscoveredFile) {
	if file == nil || file.ProviderSource == nil {
		return
	}
	dbPath, _, ok := sqliteContainerSourceForFile(*file)
	if !ok {
		return
	}
	e.containerMu.Lock()
	passActive := e.containerPass != nil
	e.containerMu.Unlock()
	if passActive && !e.sqliteContainerPassCaptureStillCurrent(dbPath) {
		file.ProviderSource = nil
	}
}

// discardFailedSQLiteProviderSource drops carried metadata for a container
// the pass has recorded as failed, under the container lock, without a
// fresh stat. It backs the stat-based recheck above for consumers that run
// after it: the failure may be recorded by any worker at any point in the
// pass, and carried digests must not outlive it.
func (e *Engine) discardFailedSQLiteProviderSource(file *parser.DiscoveredFile) {
	if file == nil || file.ProviderSource == nil {
		return
	}
	dbPath, _, ok := sqliteContainerSourceForFile(*file)
	if !ok {
		return
	}
	e.containerMu.Lock()
	failed := e.containerPass != nil && e.containerPass.failed[dbPath]
	e.containerMu.Unlock()
	if failed {
		file.ProviderSource = nil
	}
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
func (e *Engine) shouldSkipProviderSourceByDB(ctx context.Context,
	file parser.DiscoveredFile,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) bool {
	fresh, _ := e.providerSourceFreshnessByDB(ctx, file, fingerprint, semantics)
	return fresh
}

// providerSourceFreshnessByDB returns the Codex-family DB freshness decision
// and whether all metadata needed to persist local stat trust was verified.
// A transient Codex title-index failure may still skip unchanged transcript
// content, but it cannot promote in-memory trust or stamp a stat digest.
func (e *Engine) providerSourceFreshnessByDB(ctx context.Context,
	file parser.DiscoveredFile,
	fingerprint parser.SourceFingerprint,
	semantics parser.ProviderSyncSemantics,
) (fresh, metadataVerified bool) {
	if !isCodexFormatAgent(file.Agent) {
		return false, false
	}
	return e.codexFingerprintFreshness(ctx,
		file.Agent, file.Path, fingerprint, semantics,
	)
}

// classifyReasonixPath handles Reasonix session classification as a dedicated
// helper to stay within nilaway limits.
func (e *Engine) classifyReasonixPath(
	path string,
) (parser.DiscoveredFile, bool) {
	sep := string(filepath.Separator)
	for _, reasonixDir := range e.sources().agentDirs[parser.AgentReasonix] {
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
func (e *Engine) BackfillProjectIdentitySnapshots(ctx context.Context) (resultErr error) {
	if errors.Is(e.sourceSyncErr, db.ErrArchiveOnly) {
		return e.sourceSyncErr
	}
	if e.refuseWriteInForceParse("BackfillProjectIdentitySnapshots") {
		return errors.New(
			"BackfillProjectIdentitySnapshots refused on report-only parse-diff engine",
		)
	}
	defer func() {
		if resultErr != nil && ctx.Err() == nil {
			resultErr = e.failProjectIdentityBackfill(ctx, resultErr)
		}
	}()
	if e.sourceSyncErr != nil {
		return e.sourceSyncErr
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
			return err
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
			return err
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
	if e.db.ArchiveContent().UsageOnly() {
		return 0, e.db.SettleUsageOnlySignals(ctx, sessionID)
	}
	if e.disableSignalRecompute {
		return 0, nil
	}
	return e.recomputeSignalsFromDBWithHook(ctx, sessionID, nil)
}

// recomputeSignalsFromDBWithHook retries a full recompute when the transcript
// changes after its revision token is captured. beforePublish is a deterministic
// test seam invoked after the snapshot has been computed and before the
// conditional write; production callers pass nil.
func (e *Engine) recomputeSignalsFromDBWithHook(
	ctx context.Context,
	sessionID string,
	beforePublish func(attempt int),
) (int, error) {
	const maxSnapshotAttempts = 3
	for attempt := range maxSnapshotAttempts {
		if err := ctx.Err(); err != nil {
			return 0, err
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
		// Capture every session-row input consumed by the signal compute before
		// loading transcript rows. The conditional transaction below rejects
		// transcript changes and metadata-only races alike.
		expectedInputs, err := db.SignalInputSnapshot(*sess)
		if err != nil {
			return 0, err
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
		projectedSession, msgs := e.db.ProjectSessionForStorage(*sess, msgs)
		update, findings := e.computeSignalsAndSecretsForStorage(projectedSession, msgs)
		heapBytes := recomputeHeapBytes(msgs, findings)
		var state db.SessionSignalState
		if isCodexFormatAgent(parser.AgentType(sess.Agent)) {
			state, err = buildSignalStateFromRows(
				sessionID, msgs, extractToolCallRows(msgs),
				expectedInputs.TranscriptRevision,
			)
			if err != nil {
				return 0, err
			}
		}
		if beforePublish != nil {
			beforePublish(attempt)
		}
		applied, err := e.db.ReplaceSessionSignalsIfInputsMatch(ctx,
			sessionID, expectedInputs, findings, update, state,
		)
		if err != nil {
			return 0, fmt.Errorf(
				"publishing signal snapshot %s: %w", sessionID, err,
			)
		}
		if applied {
			return heapBytes, nil
		}
	}
	return 0, fmt.Errorf(
		"session %s changed during %d signal recompute attempts",
		sessionID, maxSnapshotAttempts,
	)
}

type pendingWrite struct {
	sess        parser.ParsedSession
	msgs        []parser.ParsedMessage
	usageEvents []parser.ParsedUsageEvent
	// sourceBytes is the physical source size carried from the parse result;
	// collectAndBatch uses it to flush batches on estimated bytes as well as
	// session count.
	sourceBytes int64
	// checkpoint is the provider's persisted continuation cursor for a full
	// parse. The flush path persists it as a parser_checkpoints row after the
	// session rows commit, so later appends can resume without rescanning the
	// transcript prefix. Empty for providers without checkpoints.
	checkpoint []byte
	// checkpointHashState/checkpointAnchorDigest carry the single-pass
	// hash state and tail-anchor digest the parser captured while reading
	// the snapshot; persisting them avoids any second source read after a
	// full parse. Empty when the provider did not supply them.
	checkpointHashState    []byte
	checkpointAnchorDigest string
	// staged carries the scratch staging sink when this write came from a
	// streaming Codex full parse; the write path publishes tool-result rows
	// and summaries from it and the batch flush closes it.
	staged       *codexStagingSink
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
				batch[i].sourceIdentityUnverified = !sessionWriteIdentitySupportsStoredAttribution(stored, incoming)
				batch[i].sess.Machine = stored.Machine
				// A rebuild writes into a fresh database, so the upsert cannot
				// preserve a title from the destination row. Carry the archived
				// nullable title into parses with no title signal; an explicitly
				// present blank remains authoritative and bypasses this path.
				if e.archiveStore != nil && parser.PreservesStoredSessionName(batch[i].sess) {
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
			batch[i].sourceIdentityUnverified = !sessionWriteIdentitySupportsStoredAttribution(
				first, e.pendingWriteIdentity(batch[i]),
			)
			batch[i].sess.Machine = first.Machine
		}
	}
	return batch, nil
}

type writeBatchOutcome struct {
	err             error
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

func (e *Engine) rejectSkipCacheWrites(ctx context.Context, writes []skipCacheWrite) {
	for _, write := range writes {
		e.clearSkip(ctx, write.key)
	}
}

// markStaleFailedMemberWrite demotes the stored data version of an omnigent
// session whose write failed. Shared-container members have no per-file mtime
// to invalidate, so without the demotion a partial write (session row updated,
// messages not) would compare as unchanged and never be repaired.
func (e *Engine) markStaleFailedMemberWrite(ctx context.Context, pw pendingWrite) {
	if pw.sess.Agent != parser.AgentOmnigent || pw.sess.ID == "" {
		return
	}
	// Sessions are stored under the remote-sync prefixed ID
	// (applyRemoteRewrites in prepareSessionWrite), so the demotion must
	// target the same row.
	id := applyIDPrefixToID(e.idPrefix, pw.sess.ID)
	staleVersion := max(db.CurrentDataVersion()-1, 0)
	if e.db.GetSessionDataVersion(ctx, id) > staleVersion {
		if err := e.db.SetSessionDataVersion(ctx, id, staleVersion); err != nil {
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
			e.markStaleFailedMemberWrite(ctx, pw)
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
			e.markStaleFailedMemberWrite(ctx, pw)
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
		claudeSources, err := e.claudeSubagentWriteSources(ctx, pw)
		if err != nil {
			outcome.err = err
			outcome.failedSessions++
			return outcome
		}
		if claudeSources != nil || pw.sess.CodexFork != nil {
			// Keep contributing paths and content in the same transaction.
			version := e.db.GetSessionDataVersion(ctx, applyIDPrefixToID(e.idPrefix, pw.sess.ID))
			stale := version > 0 && version < db.CurrentDataVersion()
			written := e.writeBatchBulkWithOutcomeContext(ctx, batch[i:i+1], forceReplace || stale)
			outcome.writtenSessions += written.writtenSessions
			outcome.writtenMessages += written.writtenMessages
			outcome.failedSessions += written.failedSessions
			outcome.cwdFiltered += written.cwdFiltered
			outcome.written[i] = written.written[0]
			outcome.resolved[i] = written.resolved[0]
			if written.err != nil {
				outcome.err = errors.Join(outcome.err, written.err)
			}
			continue
		}
		prepared, verdict, prepErr := e.prepareSessionNormalizedContext(
			ctx, pw, resolveWorktreeProject, true,
		)
		if prepErr != nil {
			outcome.err = prepErr
			outcome.failedSessions++
			return outcome
		}
		if verdict != sessionWriteOK {
			if verdict == sessionWriteCwdFiltered {
				outcome.cwdFiltered++
			}
			continue
		}
		s, msgs := prepared.Session, prepared.Messages
		// Detect stale parser version BEFORE UpsertSession
		// overwrites it. Existing message rows from an
		// older parser lack new metadata columns, and newly
		// emitted compact-boundary messages can shift the
		// ordinal stream — both demand a full rewrite
		// rather than the append-only writeMessages path.
		stale := false
		if existing := e.db.GetSessionDataVersion(ctx, s.ID); existing > 0 &&
			existing < db.CurrentDataVersion() {
			stale = true
		}
		if ctx.Err() != nil {
			return outcome
		}

		// The session row must exist before messages can be inserted (FK
		// constraint), but a row stays source-missing until every
		// dependent write succeeds below. For incremental updates
		// (writeIncremental), messages are written first since the session
		// already exists.
		revivingSourceMissing, err := e.upsertSessionPendingContentForWrite(ctx, pw, s)
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
			e.markStaleFailedMemberWrite(ctx, pw)
			outcome.failedSessions++
			continue
		}
		replaceMessages := shouldReplaceFullParseMessages(
			pw, forceReplace, stale, revivingSourceMissing,
		)

		update := prepared.Signals
		findings := prepared.Findings
		var werr error
		if replaceMessages && pw.staged != nil {
			// The staged sink owns this parse's tool-result rows, and only the
			// staged write publishes them, so it runs even when signal
			// recomputation is disabled.
			werr = e.writeStagedFullParse(ctx, s, msgs, pw, nil)
		} else if replaceMessages && !e.disableSignalRecompute {
			update, werr = e.attachFullSignalState(s, msgs, update, nil)
			if werr != nil {
				log.Printf("compute full signals %s: %v", s.ID, werr)
				outcome.failedSessions++
				continue
			}
			if ctx.Err() != nil {
				return outcome
			}
			if isCodexFormatAgent(pw.sess.Agent) {
				cp, blobs, cpErr := e.buildCodexFullParseCheckpoint(
					pw.sess.File.Path, pw,
				)
				if cpErr != nil {
					log.Printf(
						"checkpoint build %s: %v",
						pw.sess.File.Path, cpErr,
					)
					cp, blobs = nil, nil
				}
				werr = e.db.ReplaceSessionContentWithCheckpointAndToolResultImages(ctx,
					s.ID, msgs, update, findings, cp, blobs,
					e.toolResultImages,
				)
			} else {
				werr = e.db.ReplaceSessionContentWithToolResultImages(ctx,
					s.ID, msgs, update, findings, e.toolResultImages,
				)
			}
		} else if replaceMessages {
			if msgs == nil {
				msgs = []db.Message{}
			}
			werr = e.db.ReplaceSessionMessagesWithToolResultImages(ctx,
				s.ID, msgs, e.toolResultImages,
			)
		} else {
			if !e.disableSignalRecompute && ctx.Err() != nil {
				return outcome
			}
			werr = e.writeMessages(ctx, s.ID, msgs)
		}
		if werr != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf(
				"write messages for %s: %v",
				s.ID, werr,
			)
			e.markStaleFailedMemberWrite(ctx, pw)
			outcome.failedSessions++
			continue
		}

		if ctx.Err() != nil {
			return outcome
		}
		if err := e.db.ReplaceSessionUsageEvents(ctx,
			s.ID, prepared.UsageEvents,
		); err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf(
				"write usage events for %s: %v",
				s.ID, err,
			)
			e.markStaleFailedMemberWrite(ctx, pw)
			outcome.failedSessions++
			continue
		}
		if err := e.db.ReplaceSessionPlanUsage(ctx,
			s.ID, prepared.PlanUsage,
		); err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf(
				"write plan usage for %s: %v",
				s.ID, err,
			)
			e.markStaleFailedMemberWrite(ctx, pw)
			outcome.failedSessions++
			continue
		}
		if err := e.db.ReplaceSessionUsageTotal(ctx,
			s.ID, prepared.SessionTotal,
		); err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf(
				"write session total for %s: %v",
				s.ID, err,
			)
			e.markStaleFailedMemberWrite(ctx, pw)
			outcome.failedSessions++
			continue
		}
		if ctx.Err() != nil {
			return outcome
		}

		// Advance data_version only after the message and usage writes
		// succeeded. The pending upsert deliberately does not touch this
		// column, and source-missing state is cleared only after this
		// succeeds, so an old current version cannot hide a failed rewrite.
		if err := e.db.SetSessionDataVersion(ctx,
			s.ID, dataVersionForWrite(pw),
		); err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf(
				"set data_version for %s: %v", s.ID, err,
			)
			e.markStaleFailedMemberWrite(ctx, pw)
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
			if err := e.db.ReplaceSessionSecretFindings(ctx,
				s.ID, findings, update.SecretLeakCount,
				update.SecretsRulesVersion); err != nil {
				log.Printf("secrets: persist %s: %v", s.ID, err)
			} else if err := e.db.UpdateSessionSignals(ctx, s.ID, update); err != nil {
				log.Printf("signals: update %s: %v", s.ID, err)
			}
		}
		if ctx.Err() != nil {
			return outcome
		}
		if err := e.db.ClearSessionSourceMissing(ctx, s.ID); err != nil {
			if ctx.Err() != nil {
				return outcome
			}
			log.Printf("clear source-missing state for session %s: %v", s.ID, err)
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
	prepared, verdict, _ := e.prepareSessionNormalizedContext(
		context.Background(), pw, resolveWorktreeProject, false,
	)
	return prepared.Session, prepared.Messages, verdict
}

func (e *Engine) projectToolResultImagesForPrepare(
	messages []db.Message,
) ([]db.Message, db.ToolImageStats) {
	if !e.forceParse && e.toolResultImages != config.ToolResultImagesDrop {
		return messages, db.ToolImageStats{}
	}
	return e.db.ProjectToolResultImagesForComparison(
		messages, e.toolResultImages,
	)
}

func (e *Engine) prepareSessionWriteContext(
	ctx context.Context,
	pw pendingWrite,
	resolveWorktreeProject worktreeProjectResolver,
) (db.Session, []db.Message, sessionWriteVerdict, error) {
	prepared, verdict, err := e.prepareSessionNormalizedContext(
		ctx, pw, resolveWorktreeProject, false,
	)
	return prepared.Session, prepared.Messages, verdict, err
}

func (e *Engine) prepareSessionNormalizedContext(
	ctx context.Context,
	pw pendingWrite,
	resolveWorktreeProject worktreeProjectResolver,
	derive bool,
) (preparedSessionWrite, sessionWriteVerdict, error) {
	candidate, err := ingest.PrepareCandidate(ctx, parser.ParseResult{
		Session: pw.sess, Messages: pw.msgs, UsageEvents: pw.usageEvents,
	}, ingest.ContentOptions{
		BlockedResultCategories: e.blockedResultCategories,
		ToolResultImages:        e.toolResultImages,
		ArchiveContent:          e.db.ArchiveContent(),
	})
	if err != nil {
		return preparedSessionWrite{}, sessionWritePreserved, err
	}
	candidate.Messages, _ = e.projectToolResultImagesForPrepare(candidate.Messages)
	s, msgs := candidate.Session, candidate.Messages
	if err := e.applyRemoteRewritesContext(ctx, &s, msgs); err != nil {
		return preparedSessionWrite{}, sessionWritePreserved, err
	}
	if e.archiveSessionPolicy != nil {
		keep, err := e.archiveSessionPolicy(ctx, &s, pw.sess.ID)
		if err != nil || !keep {
			return preparedSessionWrite{}, sessionWritePreserved, err
		}
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
		return preparedSessionWrite{}, sessionWriteCwdFiltered, nil
	}
	if err := ctx.Err(); err != nil {
		return preparedSessionWrite{}, sessionWritePreserved, err
	}

	candidate.Session, candidate.Messages = s, msgs
	if e.localClaudeSubagentSources(pw) != nil {
		missing, err := e.missingClaudeSubagentSource(ctx, s.ID)
		if err != nil || missing {
			return preparedSessionWrite{}, sessionWritePreserved, err
		}
	}
	history, err := e.reconcileProviderHistoryContext(ctx, candidate)
	if err != nil {
		return preparedSessionWrite{}, sessionWritePreserved, err
	}
	if history.Action == ingest.HistoryPreserve {
		return preparedSessionWrite{}, sessionWritePreserved, nil
	}
	candidate = history.Candidate
	candidate, forkState, stagedToolKeys, err := e.reconcileCodexFork(ctx, candidate, pw.staged)
	if err != nil {
		return preparedSessionWrite{}, sessionWritePreserved, err
	}
	candidate.Messages, _ = e.projectToolResultImagesForPrepare(candidate.Messages)
	if err := ctx.Err(); err != nil {
		return preparedSessionWrite{}, sessionWritePreserved, err
	}
	options := ingest.ContentOptions{
		BlockedResultCategories: e.blockedResultCategories,
		ToolResultImages:        e.toolResultImages,
		ArchiveContent:          e.db.ArchiveContent(),
	}
	rows, err := ingest.FinalizeRows(ctx, candidate, options)
	if err != nil {
		return preparedSessionWrite{}, sessionWritePreserved, err
	}
	prepared := preparedSessionWrite{PreparedSession: rows, codexForkState: forkState, stagedToolKeys: stagedToolKeys}
	if forkState != nil {
		// Totals describe the sanitized message and independent event rows that
		// will be committed. Session-summary events are not additive usage.
		if err := ingest.ApplySessionTokenTotalsFromMessagesContext(ctx, &prepared.Session, prepared.Messages); err != nil {
			return preparedSessionWrite{}, sessionWritePreserved, err
		}
		for _, event := range prepared.UsageEvents {
			if event.Source == "session" {
				continue
			}
			prepared.Session.TotalOutputTokens += event.OutputTokens
			prepared.Session.HasTotalOutputTokens = prepared.Session.HasTotalOutputTokens || event.OutputTokens > 0
			contextTokens := event.InputTokens + event.CacheCreationInputTokens + event.CacheReadInputTokens
			prepared.Session.PeakContextTokens = max(prepared.Session.PeakContextTokens, contextTokens)
			prepared.Session.HasPeakContextTokens = prepared.Session.HasPeakContextTokens || contextTokens > 0
		}
		for _, m := range prepared.Messages {
			digest, keyErr := db.CodexForkMessageKey(m)
			if keyErr != nil {
				return preparedSessionWrite{}, sessionWritePreserved, keyErr
			}
			forkState.Messages[m.Ordinal].Digest = digest
		}
	}

	if prepared.PlanUsage != nil {
		prepared.PlanUsage.CountedUnder = applyIDPrefixToID(
			e.idPrefix, prepared.PlanUsage.CountedUnder,
		)
	}
	if candidate.Parsed.Session.Agent == parser.AgentCodexCloud {
		if err := e.keepCodexCloudUsage(ctx, &prepared.PreparedSession); err != nil {
			return preparedSessionWrite{}, sessionWritePreserved, err
		}
	}
	if derive && pw.staged == nil && !e.disableSignalRecompute {
		prepared.Signals, prepared.Findings = e.computeSignalsAndSecretsForStorage(
			prepared.Session, prepared.Messages,
		)
	}
	s = prepared.Session
	e.anomalies.recordSanitize(prepared.Validation)
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
	return prepared, sessionWriteOK, ctx.Err()
}

// hasUnresolvedPersistedToolResult reports whether any parsed Claude or
// iCodeMate tool_result could not resolve a persisted-output side file,
// over the cheap parser-level carriers, before any store read.
func hasUnresolvedPersistedToolResult(messages []parser.ParsedMessage) bool {
	for _, message := range messages {
		for _, toolResult := range message.ToolResults {
			if toolResult.PersistedOutputUnresolved {
				return true
			}
		}
	}
	return false
}

// keepCodexCloudUsage copies onto prepared the token counts and plan usage
// its mirror file lost; see ingest.CarryCodexCloudUsage.
func (e *Engine) keepCodexCloudUsage(
	ctx context.Context, prepared *ingest.PreparedSession,
) error {
	store := e.historyStore()
	stored, err := store.GetSessionFull(ctx, prepared.Session.ID)
	if err != nil || stored == nil {
		return err
	}
	reader, ok := store.(interface {
		GetUsageEvents(context.Context, string) ([]db.UsageEvent, error)
	})
	if !ok {
		return fmt.Errorf("keep token counts of %s: the archive cannot read usage events", prepared.Session.ID)
	}
	events, err := reader.GetUsageEvents(ctx, prepared.Session.ID)
	if err != nil {
		return err
	}
	plan, err := store.GetSessionPlanUsage(ctx, prepared.Session.ID)
	if err != nil {
		return err
	}
	if ingest.CarryCodexCloudUsage(prepared, ingest.CodexCloudPrior{
		Session: *stored, UsageEvents: events, PlanUsage: plan,
		Current: stored.DataVersion >= db.AgentDataVersion(parser.AgentCodexCloud),
	}) {
		log.Printf("codex-cloud: kept saved usage figures of %s that the mirror file no longer holds", prepared.Session.ID)
	}
	return nil
}

func (e *Engine) historyStore() db.Store {
	if e.archiveStore != nil {
		return e.archiveStore
	}
	return e.db
}

func (e *Engine) reconcileProviderHistoryContext(
	ctx context.Context, candidate ingest.Candidate,
) (ingest.HistoryResult, error) {
	agent := candidate.Parsed.Session.Agent
	options := ingest.ContentOptions{
		BlockedResultCategories: e.blockedResultCategories,
		ToolResultImages:        e.toolResultImages,
		ArchiveContent:          e.db.ArchiveContent(),
	}
	var prior *ingest.PriorSession
	if candidate.Parsed.Session.KeepStoredGroupKey {
		store := e.historyStore()
		stored, err := store.GetSessionFull(ctx, candidate.Session.ID)
		if err != nil {
			return ingest.HistoryResult{}, err
		}
		if stored != nil {
			prior = &ingest.PriorSession{Session: *stored}
		}
	}
	if ingest.IsClaudeFormatTranscript(
		agent, candidate.Parsed.Session.File.Path,
	) {
		if (prior == nil || len(prior.Messages) == 0) &&
			hasUnresolvedPersistedToolResult(candidate.Parsed.Messages) {
			store := e.historyStore()
			stored, err := store.GetSessionFull(ctx, candidate.Session.ID)
			if err != nil {
				return ingest.HistoryResult{}, err
			}
			if stored != nil {
				messages, messagesErr := store.GetAllMessages(
					ctx, candidate.Session.ID,
				)
				if messagesErr != nil {
					return ingest.HistoryResult{}, messagesErr
				}
				if len(messages) > 0 {
					prior = &ingest.PriorSession{
						Session: *stored, Messages: messages,
					}
				}
			}
		}
	} else {
		switch agent {
		case parser.AgentOpenClaw:
			path := candidate.Parsed.Session.File.Path
			_, _, sqliteMember := parser.ParseVirtualSourcePathForBase(path, "openclaw-agent.sqlite")
			if sqliteMember || !parser.IsOpenClawSessionFile(filepath.Base(path)) || candidate.Session.FilePath == nil {
				break
			}
			store := e.historyStore()
			stored, err := store.GetSessionFull(ctx, candidate.Session.ID)
			if err != nil {
				return ingest.HistoryResult{}, err
			}
			if stored != nil && stored.FilePath != nil &&
				sameReconciliationSourcePath(*candidate.Session.FilePath, *stored.FilePath) &&
				len(candidate.Messages) < stored.MessageCount {
				// A shortened copy of the same legacy file is incomplete history.
				// Different sources and SQLite members remain authoritative.
				return ingest.HistoryResult{Action: ingest.HistoryPreserve}, nil
			}
		case parser.AgentCodexCloud:
			stored, err := e.historyStore().GetSessionFull(ctx, candidate.Session.ID)
			if err != nil {
				return ingest.HistoryResult{}, err
			}
			if stored == nil {
				break
			}
			if reason := ingest.CodexCloudKeepReason(len(candidate.Messages), ingest.CodexCloudPrior{
				Session: *stored,
				Current: stored.DataVersion >= db.AgentDataVersion(parser.AgentCodexCloud),
			}); reason != "" {
				log.Printf("codex-cloud: kept the saved copy of %s because %s; the saved copy stays until a copy with as many messages replaces it",
					candidate.Session.ID, reason)
				return ingest.HistoryResult{Action: ingest.HistoryPreserve}, nil
			}
		case parser.AgentRooCode, parser.AgentKiloLegacy, parser.AgentCline:
			if len(candidate.Messages) > 0 {
				break
			}
			store := e.historyStore()
			messages, err := store.GetAllMessages(
				ctx, candidate.Session.ID,
			)
			if err == nil && len(messages) > 0 {
				prior = &ingest.PriorSession{
					Session: candidate.Session, Messages: messages,
				}
			}
		case parser.AgentVSCopilot:
			stored, err := e.db.GetSessionFull(
				ctx, candidate.Session.ID,
			)
			if err == nil && stored != nil {
				messages, messagesErr := e.db.GetAllMessages(
					ctx, candidate.Session.ID,
				)
				if messagesErr == nil && len(messages) > 0 {
					prior = &ingest.PriorSession{
						Session: *stored, Messages: messages,
					}
				}
			}
		default:
			if ingest.IsOpenCodeFormatStorageAgent(agent) {
				store := e.historyStore()
				stored, err := store.GetSessionFull(
					ctx, candidate.Session.ID,
				)
				if err == nil && stored != nil {
					messages, messagesErr := store.GetAllMessages(
						ctx, candidate.Session.ID,
					)
					if messagesErr == nil && len(messages) > 0 {
						prior = &ingest.PriorSession{
							Session: *stored, Messages: messages,
						}
					}
				}
			}
		}
	}
	result, err := ingest.ReconcileProviderHistory(
		ctx, candidate, prior, options,
	)
	if err != nil || prior == nil {
		return result, err
	}
	switch result.Action {
	case ingest.HistoryReplace:
		// Replacement needs no history reconciliation notice.
	case ingest.HistoryPreserve:
		log.Printf(
			"preserve %s %s: reparse looks incomplete relative to archived transcript (%d stored messages, %d parsed messages)",
			agent, candidate.Session.ID, len(prior.Messages), len(candidate.Messages),
		)
	case ingest.HistoryMerge:
		log.Printf(
			"merge %s %s: reparse updated archived messages while retaining archived transcript rows (%d stored messages, %d parsed messages)",
			agent, candidate.Session.ID, len(prior.Messages), len(candidate.Messages),
		)
	}
	return result, nil
}

type visualStudioCopilotArchiveReconcile struct {
	preserve bool
	merged   []db.Message
}

func visualStudioCopilotArchiveDecision(
	parsed, stored []db.Message,
) visualStudioCopilotArchiveReconcile {
	merged, preserve := ingest.ReconcileVisualStudioCopilotArchive(
		parsed, stored,
	)
	return visualStudioCopilotArchiveReconcile{
		preserve: preserve, merged: merged,
	}
}

func visualStudioCopilotMessageLooksIncomplete(
	parsed, stored db.Message,
) bool {
	return ingest.VisualStudioCopilotMessageLooksIncomplete(parsed, stored)
}

func visualStudioCopilotMessageHasArchiveUpdate(
	parsed, stored db.Message,
) bool {
	return ingest.VisualStudioCopilotMessageHasArchiveUpdate(parsed, stored)
}

type batchSourceFile struct {
	path        string
	mtime       int64
	fingerprint string
}

// stagedToolCallPositions maps every occurrence-qualified staging key in the
// message model to its final message/call coordinates. Provider call IDs can
// repeat, so raw tool_use_id alone cannot identify a staged event target.
func stagedToolCallPositions(
	msgs []db.Message,
) map[string]db.StagedToolCallPosition {
	positions := make(map[string]db.StagedToolCallPosition)
	callOccurrences := make(map[string]int)
	for _, m := range msgs {
		for callIdx, tc := range m.ToolCalls {
			occurrence := callOccurrences[tc.ToolUseID]
			callOccurrences[tc.ToolUseID] = occurrence + 1
			stageKey := db.StagedToolCallKey(tc.ToolUseID, occurrence)
			positions[stageKey] = db.StagedToolCallPosition{
				ToolUseID: tc.ToolUseID,
				Ordinal:   m.Ordinal,
				CallIndex: callIdx,
			}
		}
	}
	return positions
}

// writeStagedFullParse publishes one staged streaming result. The publish
// transaction resolves every per-call summary once and runs the signals
// closure before commit, so the content-failure-aware signals and
// findings persist atomically with the message, tool-call, and event
// rows. The incremental signal state uses the same captured verdicts and
// commits with the content.
func (e *Engine) writeStagedFullParse(
	ctx context.Context, s db.Session, msgs []db.Message, pw pendingWrite,
	write *db.SessionBatchWrite,
) error {
	if pw.staged != nil {
		if err := pw.staged.PublishToolResultImages(ctx); err != nil {
			return err
		}
	}
	positions := stagedToolCallPositions(msgs)
	if write != nil && write.CodexForkState != nil {
		sourcePositions := make(map[string]db.StagedToolCallPosition, len(write.StagedToolKeys))
		for final, source := range write.StagedToolKeys {
			sourcePositions[source] = positions[final]
		}
		positions = sourcePositions
	}
	var closure db.StagedSignalsFunc
	if !e.disableSignalRecompute {
		closure = func(verdicts map[string]db.StagedToolVerdict) (
			db.SessionSignalUpdate, []db.SecretFinding, error,
		) {
			if write != nil && write.CodexForkState != nil {
				finalVerdicts := make(map[string]db.StagedToolVerdict, len(verdicts))
				for final, source := range write.StagedToolKeys {
					if verdict, ok := verdicts[source]; ok {
						finalVerdicts[final] = verdict
					}
				}
				verdicts = finalVerdicts
			}
			update, findings, err := e.computeFullSignalsAndSecretsForStorage(s, msgs, verdicts)
			if err != nil {
				return db.SessionSignalUpdate{}, nil, err
			}
			if e.db.ArchiveContent().OmitsToolContent() {
				return update, findings, nil
			}
			combined := append(
				append([]db.SecretFinding(nil), findings...),
				pw.staged.Findings(s.ID, positions)...,
			)
			update.SecretLeakCount = definiteFindingCount(combined)
			return update, combined, nil
		}
	}
	cp, blobs, cpErr := e.buildCodexFullParseCheckpoint(
		pw.sess.File.Path, pw,
	)
	if pw.sess.CodexFork != nil {
		cp, blobs = nil, nil
	}
	if cpErr != nil {
		log.Printf("checkpoint build %s: %v", pw.sess.File.Path, cpErr)
		cp, blobs = nil, nil
	}
	var writeErr error
	if write == nil {
		writeErr = e.db.ReplaceSessionContentStagedWithCheckpoint(ctx, s.ID, msgs, pw.staged, e.blockedResultCategories, closure, cp, blobs)
	} else {
		write.Checkpoint, write.CheckpointBlobs = cp, blobs
		writeErr = e.db.WriteSessionBatchStaged(ctx, *write, pw.staged, e.blockedResultCategories, closure)
	}
	if writeErr != nil {
		return writeErr
	}
	e.anomalies.recordSanitize(pw.staged.ValidationStats())

	return nil
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
	sources := make([]batchSourceFile, 0, len(batch))
	pendingByID := make(map[string]pendingWrite, len(batch))
	resolveWorktreeProject := e.loadWorktreeProjectResolverContext(ctx)

	for pendingIndex, pw := range batch {
		if ctx.Err() != nil {
			return outcome
		}
		tPrep := time.Now()
		prepared, verdict, prepErr := e.prepareSessionNormalizedContext(
			ctx, pw, resolveWorktreeProject, true,
		)
		if prepErr != nil {
			outcome.err = prepErr
			outcome.failedSessions++
			return outcome
		}
		e.phaseStats.PrepNanos.Add(int64(time.Since(tPrep)))
		if verdict != sessionWriteOK {
			if verdict == sessionWriteCwdFiltered {
				outcome.cwdFiltered++
			}
			continue
		}
		s, msgs := prepared.Session, prepared.Messages
		provenance, err := e.sessionArchiveProvenance(&s, pw.sess.ID)
		if err != nil {
			outcome.err = err
			outcome.failedSessions++
			return outcome
		}
		if pw.staged != nil && (provenance != nil || prepared.codexForkState != nil) {
			err := e.writeAttachedStaged(ctx, prepared, pw, provenance)
			if err != nil {
				if isIntentionalSessionSkip(err) {
					outcome.resolved[pendingIndex] = true
					continue
				}
				outcome.err = errors.Join(outcome.err, err)
				outcome.failedSessions++
				continue
			}
			outcome.written[pendingIndex], outcome.resolved[pendingIndex] = true, true
			outcome.writtenSessions++
			outcome.writtenMessages += len(msgs)
			continue
		}
		if pw.staged != nil {
			// Staged streaming results bypass the bulk batch: their
			// tool-result rows live in the staging scratch database and
			// must be published through the staged transaction, which
			// also persists the content-failure-aware signals in the
			// same commit. A staged full parse always force-replaces.
			// The bulk batch would normally create the session row, so
			// mirror the standard write path's session upsert and
			// post-write sequence here.
			_, err := e.upsertSessionPendingContentForWrite(ctx, pw, s)
			if err != nil {
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
				e.markStaleFailedMemberWrite(ctx, pw)
				outcome.failedSessions++
				continue
			}
			tWrite := time.Now()
			err = e.writeStagedFullParse(ctx, s, msgs, pw, nil)
			e.phaseStats.WriteNanos.Add(int64(time.Since(tWrite)))
			if err != nil {
				log.Printf(
					"write staged session %s: %v", s.ID, err,
				)
				e.markStaleFailedMemberWrite(ctx, pw)
				outcome.failedSessions++
				continue
			}
			if err := e.db.ReplaceSessionUsageEvents(ctx,
				s.ID, prepared.UsageEvents,
			); err != nil {
				log.Printf(
					"write usage events for %s: %v", s.ID, err,
				)
				e.markStaleFailedMemberWrite(ctx, pw)
				outcome.failedSessions++
				continue
			}
			if err := e.db.ReplaceSessionPlanUsage(ctx,
				s.ID, prepared.PlanUsage,
			); err != nil {
				log.Printf(
					"write plan usage for %s: %v", s.ID, err,
				)
				e.markStaleFailedMemberWrite(ctx, pw)
				outcome.failedSessions++
				continue
			}
			if err := e.db.ReplaceSessionUsageTotal(ctx,
				s.ID, prepared.SessionTotal,
			); err != nil {
				log.Printf(
					"write session total for %s: %v", s.ID, err,
				)
				e.markStaleFailedMemberWrite(ctx, pw)
				outcome.failedSessions++
				continue
			}
			if err := e.db.SetSessionDataVersion(ctx,
				s.ID, dataVersionForWrite(pw),
			); err != nil {
				log.Printf(
					"set data_version for %s: %v", s.ID, err,
				)
				e.markStaleFailedMemberWrite(ctx, pw)
				outcome.failedSessions++
				continue
			}
			if err := e.db.ClearSessionSourceMissing(ctx, s.ID); err != nil {
				log.Printf(
					"clear source-missing state for session %s: %v", s.ID, err,
				)
				outcome.failedSessions++
				continue
			}
			outcome.written[pendingIndex] = true
			outcome.writtenSessions++
			outcome.writtenMessages += len(msgs)
			continue
		}
		replaceMessages := shouldReplaceFullParseMessages(
			pw, forceReplace, false, false,
		)
		update := prepared.Signals
		findings := prepared.Findings
		if !e.disableSignalRecompute {
			tScan := time.Now()
			var signalErr error
			update, signalErr = e.attachFullSignalState(s, msgs, update, nil)
			if signalErr != nil {
				log.Printf("compute full signals %s: %v", s.ID, signalErr)
				outcome.failedSessions++
				continue
			}
			if ctx.Err() != nil {
				return outcome
			}
			e.phaseStats.ScanNanos.Add(int64(time.Since(tScan)))
		}
		snapshotProject := pw.sess.Project
		var checkpoint *db.ParserCheckpoint
		var checkpointBlobs *db.ParserCheckpointBlobs
		if isCodexFormatAgent(pw.sess.Agent) && pw.sess.CodexFork == nil {
			var checkpointErr error
			checkpoint, checkpointBlobs, checkpointErr = e.buildCodexFullParseCheckpoint(pw.sess.File.Path, pw)
			if checkpointErr != nil {
				log.Printf(
					"checkpoint build %s: %v",
					pw.sess.File.Path, checkpointErr,
				)
				checkpoint, checkpointBlobs = nil, nil
			}
		}
		identityObservation, hasIdentityObservation := e.projectIdentityObservationForWrite(pw, s)
		claudeSources, err := e.claudeSubagentWriteSources(ctx, pw)
		if err != nil {
			outcome.err = err
			outcome.failedSessions++
			return outcome
		}
		writes = append(writes, db.SessionBatchWrite{
			CodexForkState:      prepared.codexForkState,
			StagedToolKeys:      prepared.stagedToolKeys,
			ArchiveProvenance:   provenance,
			Session:             s,
			Messages:            msgs,
			UsageEvents:         prepared.UsageEvents,
			PlanUsage:           prepared.PlanUsage,
			SessionTotal:        prepared.SessionTotal,
			ReplaceSessionTotal: true,
			ToolResultImages:    &e.toolResultImages,
			IdentityObservation: identityObservationOrZero(
				identityObservation, hasIdentityObservation,
			),
			IdentitySnapshotProject: &snapshotProject,
			Signals:                 update,
			Findings:                findings,
			SkipSignalUpdates:       e.disableSignalRecompute,
			DataVersion:             dataVersionForWrite(pw),
			ReplaceMessages:         replaceMessages,
			Checkpoint:              checkpoint,
			CheckpointBlobs:         checkpointBlobs,
			ClaudeSubagentSources:   claudeSources,
		})
		pendingIndexes = append(pendingIndexes, pendingIndex)
		pendingByID[s.ID] = pw
		sources = append(sources, batchSourceFile{
			path:        pw.sess.File.Path,
			mtime:       pw.sess.File.Mtime,
			fingerprint: pw.sess.File.Hash,
		})
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
		outcome.err = err
		log.Printf("write session batch: %v", err)
		for _, pw := range pendingByID {
			e.markStaleFailedMemberWrite(ctx, pw)
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
			e.markStaleFailedMemberWrite(ctx, pw)
		}
	}
	// Resolve skips by write index: two sources in one batch can share a
	// session id, and each skipped source must be skip-cached.
	for _, excludedIndex := range result.ExcludedIndexes {
		if excludedIndex < 0 || excludedIndex >= len(pendingIndexes) {
			continue
		}
		pendingIndex := pendingIndexes[excludedIndex]
		outcome.resolved[pendingIndex] = true
		if source := sources[excludedIndex]; source.path != "" {
			e.cacheSkip(
				source.path, source.mtime, source.fingerprint,
			)
		}
	}
	for _, err := range result.Errors {
		outcome.err = errors.Join(outcome.err, err)
		log.Printf("write session batch: %v", err)
	}
	outcome.writtenSessions = result.WrittenSessions
	outcome.writtenMessages = result.WrittenMessages
	outcome.failedSessions += result.FailedSessions
	return outcome
}

func (e *Engine) upsertSessionPendingContentForWrite(ctx context.Context,
	pw pendingWrite,
	s db.Session,
) (bool, error) {
	obs, ok := e.projectIdentityObservationForWrite(pw, s)
	if !ok {
		return e.db.UpsertSessionPendingContent(ctx, s)
	}
	return e.db.UpsertSessionPendingContentWithProjectIdentity(ctx,
		s, obs, pw.sess.Project,
	)
}

func shouldReplaceFullParseMessages(
	pw pendingWrite, forceReplace, stale, revivingSourceMissing bool,
) bool {
	return forceReplace || pw.forceReplace || pw.needsRetry || pw.sess.CodexFork != nil || stale ||
		revivingSourceMissing ||
		isCodexFormatAgent(pw.sess.Agent) ||
		// Kiro full parses rebuild the complete accepted message projection;
		// append semantics would retain rows removed or rewritten by the source.
		pw.sess.Agent == parser.AgentKiro ||
		pw.sess.Agent == parser.AgentCowork ||
		// Copilot execution start/completion records arrive after the
		// assistant tool-call message and attach result events to it. An
		// append would leave the persisted tool call without those events.
		pw.sess.Agent == parser.AgentCopilot ||
		// VS Code-format replies grow at an existing message position.
		pw.sess.Agent == parser.AgentVSCodeCopilot ||
		pw.sess.Agent == parser.AgentPositron ||
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
		pw.sess.Agent == parser.AgentCline ||
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
		pw.sess.Agent == parser.AgentReasonix ||
		// Resuming Command Code v2 rewrites it as v3 and merges tool/user
		// turns. Ordinals shrink, so appending would lose new turns.
		pw.sess.Agent == parser.AgentCommandCode
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
	return e.writeSessionFullWithResolver(context.Background(), pw, resolveWorktreeProject)
}

func (e *Engine) writeSessionFullWithResolver(
	ctx context.Context, pw pendingWrite,
	resolveWorktreeProject worktreeProjectResolver,
) error {
	normalized, err := e.normalizePendingWriteMachines(
		ctx, []pendingWrite{pw},
	)
	if err != nil {
		return err
	}
	preserved, err := e.preserveUnavailableSourceProjects(
		ctx, normalized,
	)
	if err != nil {
		return err
	}
	pw = preserved[0]
	claudeSources, err := e.claudeSubagentWriteSources(ctx, pw)
	if err != nil {
		return err
	}
	if claudeSources != nil || pw.sess.CodexFork != nil {
		outcome := e.writeBatchBulkWithOutcomeContext(ctx, []pendingWrite{pw}, true)
		if outcome.err != nil {
			return outcome.err
		}
		if outcome.failedSessions > 0 {
			return fmt.Errorf("write Claude subagent %s with source provenance", pw.sess.ID)
		}
		if !outcome.written[0] {
			return errSessionPreserved
		}
		return nil
	}
	prepared, verdict, err := e.prepareSessionNormalizedContext(
		ctx, pw, resolveWorktreeProject, true,
	)
	if err != nil {
		return err
	}
	if verdict != sessionWriteOK {
		return errSessionPreserved
	}
	s, msgs := prepared.Session, prepared.Messages
	_, err = e.upsertSessionPendingContentForWrite(ctx, pw, s)
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
	if pw.staged != nil {
		// The staged sink owns this parse's tool-result rows, and only the
		// staged write publishes them.
		if err := e.writeStagedFullParse(ctx, s, msgs, pw, nil); err != nil {
			log.Printf(
				"write staged session %s: %v",
				s.ID, err,
			)
			return err
		}
	} else if e.disableSignalRecompute {
		if msgs == nil {
			msgs = []db.Message{}
		}
		if err := e.db.ReplaceSessionMessagesWithToolResultImages(ctx,
			s.ID, msgs, e.toolResultImages,
		); err != nil {
			log.Printf(
				"replace messages for %s: %v",
				s.ID, err,
			)
			return err
		}
	} else {
		update, signalErr := e.attachFullSignalState(
			s, msgs, prepared.Signals, nil,
		)
		if signalErr != nil {
			return signalErr
		}
		var checkpoint *db.ParserCheckpoint
		var checkpointBlobs *db.ParserCheckpointBlobs
		if isCodexFormatAgent(pw.sess.Agent) {
			var checkpointErr error
			checkpoint, checkpointBlobs, checkpointErr = e.buildCodexFullParseCheckpoint(pw.sess.File.Path, pw)
			if checkpointErr != nil {
				log.Printf(
					"checkpoint build %s: %v",
					pw.sess.File.Path, checkpointErr,
				)
				checkpoint, checkpointBlobs = nil, nil
			}
		}
		if err := e.db.ReplaceSessionContentWithCheckpointAndToolResultImages(ctx,
			s.ID, msgs, update, prepared.Findings, checkpoint, checkpointBlobs,
			e.toolResultImages,
		); err != nil {
			log.Printf(
				"replace messages for %s: %v",
				s.ID, err,
			)
			return err
		}
	}
	if err := e.db.ReplaceSessionUsageEvents(ctx,
		s.ID, prepared.UsageEvents,
	); err != nil {
		log.Printf(
			"replace usage events for %s: %v",
			s.ID, err,
		)
		return err
	}
	if err := e.db.ReplaceSessionPlanUsage(ctx,
		s.ID, prepared.PlanUsage,
	); err != nil {
		log.Printf(
			"replace plan usage for %s: %v",
			s.ID, err,
		)
		return err
	}
	if err := e.db.ReplaceSessionUsageTotal(ctx,
		s.ID, prepared.SessionTotal,
	); err != nil {
		log.Printf(
			"replace session total for %s: %v",
			s.ID, err,
		)
		return err
	}

	// See writeBatch for why data_version is bumped here
	// rather than inside UpsertSession.
	if err := e.db.SetSessionDataVersion(ctx,
		s.ID, dataVersionForWrite(pw),
	); err != nil {
		log.Printf(
			"set data_version for %s: %v", s.ID, err,
		)
		return err
	}
	if err := e.db.ClearSessionSourceMissing(ctx, s.ID); err != nil {
		log.Printf("clear source-missing state for session %s: %v", s.ID, err)
		return err
	}
	return nil
}

// FindSourceFile locates the original source file for a
// session ID. It first checks the stored file_path from the
// database (handles cases where filename differs from session
// ID, e.g. Zencoder header ID vs filename), then falls back
// to agent-specific path reconstruction.
func (e *Engine) FindSourceFile(ctx context.Context, sessionID string) string {
	host, rawID := parser.StripHostPrefix(sessionID)
	if host != "" {
		if fp := e.db.GetSessionFilePath(ctx, sessionID); isS3SourcePath(fp) {
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
		// Crush, Forge, Piebald, Warp, and ZCode are DB-backed providers that
		// own discovery and source lookup through the provider facade. Their
		// virtual <db>#<sessionID> path is resolved by findProviderSourceFile
		// below. Non-provider, non-file-based agents (e.g. remote imports)
		// have no local source file.
		if !e.isProviderAuthoritative(def.Type) {
			return ""
		}
		storedPath := e.db.GetSessionFilePath(ctx, sessionID)
		if f := e.findProviderSourceFile(
			ctx, def, sessionID, rawSessionID, storedPath,
		); f != "" {
			return f
		}
		return ""
	}
	bareID := strings.TrimPrefix(rawID, def.IDPrefix)
	storedPath := e.db.GetSessionFilePath(ctx, sessionID)

	if f := e.findProviderSourceFile(
		ctx, def, sessionID, bareID, storedPath,
	); f != "" {
		return f
	}

	// A legacy Hermes/Augure Desktop row's stored file_path is the bare
	// state.db archive path -- that's what every session got under the
	// old whole-archive write, before per-session paths existed. The
	// provider facade above already checked this specific session
	// against current archive membership and returned not-found
	// authoritatively; falling back to the bare archive path here would
	// let SyncSingleSessionContext reparse and force-replace every
	// session in state.db, the same harm FindSource's own stored-hint
	// check now guards against. Trust the provider's not-found, the way
	// the non-FileBased DB-backed providers above already do
	// unconditionally. A standalone transcript file's stored path still
	// falls through below -- it's a real, individually-owned file, not a
	// shared container.
	if (def.Type == parser.AgentHermes || def.Type == parser.AgentAugureDesktop) &&
		parser.HermesStoredPathIsAggregateArchive(storedPath) {
		return ""
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
	ctx = e.parsePolicyContext(ctx)
	mode := e.providerMigrationModes[def.Type]
	if mode != parser.ProviderMigrationProviderAuthoritative {
		return ""
	}
	factory, ok := e.sources().providerFactories[def.Type]
	if !ok || factory == nil {
		return ""
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:          e.sources().agentDirs[def.Type],
		Machine:        e.machine,
		SourceMachines: e.sources().sourceMachines[def.Type],
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
	ctx = e.parsePolicyContext(ctx)
	factory, ok := e.sources().providerFactories[def.Type]
	if !ok || factory == nil {
		return 0
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots:          e.sources().agentDirs[def.Type],
		Machine:        e.machine,
		SourceMachines: e.sources().sourceMachines[def.Type],
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
	fingerprint, err := e.providerFingerprint(ctx, provider, source)
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
	if def.Type == parser.AgentCursorIDE && fingerprint.Hash != "" {
		// SourceMtime is an equality-only change token (see the Codebuff
		// branch): folding the member content digest in lets the watcher's
		// polling fallback see an edit that leaves lastUpdatedAt untouched.
		h := fnv.New64a()
		_, _ = fmt.Fprintf(h, "%d|%s", fingerprint.MTimeNS, fingerprint.Hash)
		return int64(h.Sum64())
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
	case parser.AgentPositAssistant, parser.AgentQoder:
		// These providers store sidecars whose mtimes the plain path stat misses.
		return true
	case parser.AgentCursorIDE:
		// A "state.vscdb#<composer>" virtual path cannot be stat'ed. The
		// path-shape check already routes it through the member fingerprint
		// because Windsurf's database shares the state.vscdb name; this
		// entry makes cursor-ide's session-watcher token independent of
		// that coincidence.
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
func (e *Engine) SourceMtime(ctx context.Context, sessionID string) int64 {
	host, rawID := parser.StripHostPrefix(sessionID)
	if host != "" {
		if fp := e.db.GetSessionFilePath(ctx, sessionID); isS3SourcePath(fp) {
			agent := parser.AgentType("")
			if sess, err := e.db.GetSession(
				ctx, sessionID,
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
		// Crush, Forge, Piebald, Warp, and ZCode are DB-backed providers:
		// their per-session source mtime comes from the provider fingerprint
		// (which mirrors the legacy List*SessionMeta last-modified value).
		// Non-provider, non-file-based agents have no local source.
		if e.isProviderAuthoritative(def.Type) {
			return e.providerSessionSourceMtime(
				ctx, def, sessionID, rawSessionID, "",
			)
		}
		return 0
	}

	path := e.FindSourceFile(ctx, sessionID)
	if path == "" {
		return 0
	}
	if e.isProviderAuthoritative(def.Type) &&
		(providerSourcePathNeedsFingerprint(path) ||
			providerSourceMtimeNeedsFingerprint(def.Type)) {
		if mtime := e.providerSessionSourceMtime(
			ctx, def, sessionID, rawSessionID, path,
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

	if def.Type == parser.AgentEvener {
		// Session polling must notice metadata and parent changes too.
		// This opaque stat token is compared for equality, not ordering.
		if hasher := e.sources().providerStatHashers[def.Type]; hasher != nil {
			return int64(hasher.ComputeMultiFileStatHash(path))
		}
		return 0
	}
	if usesCompositeSidecarFreshness(def.Type, path) {
		mtime, err := parser.ClaudeLayoutCompositeMtime(path)
		if err != nil {
			return 0
		}
		return mtime
	}
	if isOpenCodeFormatStorageAgent(def.Type) {
		mtime, err := openCodeFormatSourceMtime(ctx, def.Type, path)
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
	if def.Type == parser.AgentCline {
		info, err := os.Stat(path)
		if err != nil {
			return 0
		}
		_, mtime := clineEffectiveStat(path, info)
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
			mtime, err := parser.KiroSQLiteSourceMtime(ctx, path)
			if err != nil {
				return 0
			}
			return mtime
		}
	}
	if def.Type == parser.AgentZed {
		if _, _, ok := parser.ParseVirtualSourcePathForBase(path, "threads.db"); ok {
			mtime, err := parser.ZedSQLiteSourceMtimeContext(ctx, path)
			if err != nil {
				return 0
			}
			return mtime
		}
	}
	if def.Type == parser.AgentShelley {
		if _, _, ok := parser.ParseVirtualSourcePathForBase(path, shelleyDBFile); ok {
			mtime, err := parser.ShelleySourceMtime(ctx, path)
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
	if agent != parser.AgentDevin && agent != parser.AgentHermes &&
		agent != parser.AgentAugureDesktop && agent != parser.AgentFx {
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
	return e.syncSingleSessionContext(ctx, sessionID, false)
}

func (e *Engine) syncSingleSessionContext(ctx context.Context, sessionID string, try bool) (err error) {
	if e.sourceSyncErr != nil {
		return e.sourceSyncErr
	}
	if e.refuseWriteInForceParse("SyncSingleSession") {
		return fmt.Errorf(
			"cannot sync session %s on a report-only (parse-diff) engine",
			sessionID,
		)
	}
	ctx = e.parsePolicyContext(ctx)
	if err := e.lockSync(try); err != nil {
		return err
	}
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
	e.resetSourceClaims()

	if strings.HasPrefix(e.db.GetSessionFilePath(ctx, sessionID), "archive://") {
		return errors.New("reparse attached archive sessions with agentsview source sync --full")
	}
	host, _ := parser.StripHostPrefix(sessionID)
	if host != "" && !isS3SourcePath(e.db.GetSessionFilePath(ctx, sessionID)) {
		return fmt.Errorf(
			"cannot sync remote session %s locally", sessionID,
		)
	}

	def, ok := parser.AgentByPrefix(sessionID)
	if !ok {
		return fmt.Errorf("unknown agent for session %s", sessionID)
	}
	if !def.FileBased {
		// Crush, Forge, Piebald, Warp, and ZCode are DB-backed providers:
		// re-sync routes through FindSourceFile (resolving the virtual
		// <db>#<sessionID> path) plus the provider-aware processFile path
		// below, mirroring the file-based agents. Other non-file-based
		// agents use the OpenCode-format storage path.
		if !e.isProviderAuthoritative(def.Type) {
			return fmt.Errorf(
				"cannot resync non-file-based session %s for agent %s",
				sessionID, def.Type,
			)
		}
	}

	path := e.FindSourceFile(ctx, sessionID)
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
		e.clearSkip(ctx, path)
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
		for _, cursorDir := range e.sources().agentDirs[parser.AgentCursor] {
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
		for _, qwenpawDir := range e.sources().agentDirs[parser.AgentQwenPaw] {
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
		for _, qoderDir := range e.sources().agentDirs[parser.AgentQoder] {
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
	default:
		// Other providers determine their project during parsing.
	}

	if _, _, ok := sqliteContainerSourceForFile(file); ok {
		states := e.captureSQLiteContainerStates([]string{file.Path})
		if err := e.prepareSQLiteContainerVerification(ctx, states); err != nil {
			return err
		}
		e.beginSQLiteContainerPass([]parser.DiscoveredFile{file}, states)
		defer func() {
			e.noteSQLiteContainerResult(file.Path, err == nil && ctx.Err() == nil)
			e.finishSQLiteContainerPass(ctx, true, false)
		}()
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
	defer func() {
		if ctx.Err() == nil {
			e.flushFailureCache(ctx)
		}
	}()
	path := file.Path
	res := e.processFile(ctx, file)
	defer e.retentionBudget().scavengeIfNeeded()
	defer res.retentionLease.Release()
	defer res.releaseStaged()
	if res.err != nil {
		sessionsChanged = res.sourceCwdChanged
		if res.cacheFailure && ctx.Err() == nil {
			e.failures.Record(res.failureCacheKey, res.failureIdentity)
		}
		if res.cacheSkip && res.mtime != 0 && !res.noCacheSkip {
			e.cacheSkip(res.skipCacheKey(path), res.mtime, res.sourceFingerprint)
		}
		return false, sessionsChanged, res.err
	}
	if err := e.validateSourceSessionOwnership(ctx, res); err != nil {
		return false, sessionsChanged, err
	}
	if res.sourceCwdResolution.State != parser.SourceCwdUnspecified {
		changed, reconcileErr := e.reconcileFilteredSourceCwd(ctx,
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
			changed, reconcileErr := e.reconcileSourceCwdAtPath(ctx,
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
		repaired, err := e.db.RepairQueuedSubagentParentsContext(ctx, nil)
		if err != nil {
			return false, sessionsChanged, fmt.Errorf(
				"repair queued subagent parents: %w", err,
			)
		}
		sessionsChanged = sessionsChanged || repaired > 0
		// A previous write may have stored a new spawn edge but failed
		// before its child could be durably queued. The requested session is
		// still a bounded repair seed on the freshness path because its
		// surviving edges identify those children directly.
		linked, err := e.db.LinkSubagentSessionsForSessions(ctx,
			[]string{requestedSessionID},
		)
		if err != nil {
			return false, sessionsChanged, fmt.Errorf(
				"link fresh subagent sessions: %w", err,
			)
		}
		sessionsChanged = sessionsChanged || linked > 0
		return false, sessionsChanged, nil
	}
	if res.cacheSkip {
		e.clearSkip(ctx, res.skipCacheKey(path))
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
	priorChildren, childErr := e.db.SubagentChildSessionIDs(ctx,
		append(append([]string{}, excluded...), resultIDs...),
	)
	if childErr != nil {
		return false, sessionsChanged, fmt.Errorf(
			"list pre-write subagent children: %w", childErr)
	}
	// A prior sync may have removed an edge and then failed before repairing
	// its child. Retry that durable work after this sync's read-only capture
	// but before making any new mutations.
	repaired, err := e.db.RepairQueuedSubagentParentsContext(ctx, nil)
	if err != nil {
		return false, sessionsChanged, fmt.Errorf(
			"repair queued subagent parents: %w", err,
		)
	}
	sessionsChanged = sessionsChanged || repaired > 0
	if err := e.db.QueueSubagentParentCleanupRepairs(ctx, priorChildren); err != nil {
		return false, sessionsChanged, fmt.Errorf(
			"queue subagent parent repairs: %w", err,
		)
	}
	atomicDAG := sourceRequiresAtomicDAGCompletion(
		file.Agent, len(res.results),
	)
	var sourceCompletionSkipped map[string]bool
	if atomicDAG {
		activeResultIDs, skipped := e.partitionIntentionalSourceSkips(ctx, resultIDs)
		sourceCompletionSkipped = skipped
		staleVersion := max(db.CurrentDataVersion()-1, 0)
		if err := e.db.SetExistingSessionDataVersions(ctx,
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
		// Queued repairs must run even when a write failed because ctx was
		// canceled, so detach from cancellation while keeping ctx values.
		repaired, repairErr := e.db.RepairQueuedSubagentParentsContext(
			context.WithoutCancel(ctx), nil,
		)
		if repairErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"repair queued subagent parents: %w", repairErr,
			))
		}
		sessionsChanged = sessionsChanged || repaired > 0
	}()
	queueWrittenChildren := func(spawnerIDs []string) error {
		children, childErr := e.db.SubagentChildSessionIDs(ctx, spawnerIDs)
		if childErr != nil {
			return fmt.Errorf("list post-write subagent children: %w", childErr)
		}
		if err := e.db.QueueSubagentParentRepairs(ctx, children); err != nil {
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
	if _, err := e.deleteParserExcludedSessions(ctx,
		res, sourceAllowsParserExclusions,
	); err != nil {
		return false, sessionsChanged, fmt.Errorf(
			"delete parser-excluded sessions: %w", err,
		)
	}
	// A virtual member gone from a still-existing shared container is marked
	// source-missing with its exact source ownership, mirroring
	// collectAndBatch, so a single-session resync preserves the archive
	// row with recoverable source-missing state. The cwd-filter
	// freeze is judged per member against the archived cwd, matching
	// the batch path.
	if len(res.sourceMissingMembers) > 0 {
		var exactOwnerships []db.SessionSourceOwnership
		var rejectedExactOwnerships []db.SessionSourceOwnership
		tombstoned, _, err := e.reconcileSourceMissingMembers(
			ctx, res.sourceMissingMembers,
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
		if err := e.writeIncremental(ctx, res.incremental); err != nil {
			return false, sessionsChanged, err
		}
		if err := queueWrittenChildren(
			[]string{res.incremental.sessionID},
		); err != nil {
			return false, sessionsChanged, err
		}
		linked, err := e.db.LinkSubagentSessionsForSessions(ctx,
			[]string{res.incremental.sessionID},
		)
		if err != nil {
			return false, sessionsChanged, fmt.Errorf(
				"link incremental subagent sessions: %w", err)
		}
		sessionsChanged = sessionsChanged || linked > 0
		return false, sessionsChanged, nil
	}

	if len(res.results) == 0 {
		return false, sessionsChanged, nil
	}

	sourceNeedsRetry := res.sourceProofWithheld(false)
	resolved := 0
	writtenIDs := make([]string, 0, len(res.results))
	markSourceIncomplete := func() {
		if file.Agent == parser.AgentClaude {
			e.clearProviderSourceFreshness(ctx, res.providerStatHash)
		}
	}
	for _, cause := range res.retryCauses {
		log.Printf("sync: session needs retry: %s", cause)
	}
	for i, pr := range res.results {
		sessionNeedsRetry := res.providerWideFailureCount > 0 ||
			res.needsRetryForSession(pr.Session.ID)
		// Match the batch path: these fallback parses cannot replace saved
		// history until the higher-fidelity source is available.
		if (file.Agent == parser.AgentAntigravityCLI || (isCodexFormatAgent(file.Agent) && pr.Session.CodexFork == nil)) && res.needsRetryForSession(pr.Session.ID) {
			stored, readErr := savedHistory(ctx, e.db, resultIDs[i])
			if readErr != nil {
				return false, sessionsChanged, fmt.Errorf("read deferred session %s: %w", resultIDs[i], readErr)
			}
			if stored != nil {
				if stored.DataVersion >= db.CurrentDataVersion() {
					if err := e.db.SetSessionDataVersion(ctx, resultIDs[i], max(db.CurrentDataVersion()-1, 0)); err != nil {
						return false, sessionsChanged, fmt.Errorf("mark deferred session %s for retry: %w", resultIDs[i], err)
					}
				}
				preserved = true
				continue
			}
		}
		write := pendingWrite{
			sess:                   pr.Session,
			msgs:                   pr.Messages,
			usageEvents:            pr.UsageEvents,
			checkpoint:             pr.Checkpoint,
			checkpointHashState:    pr.CheckpointHashState,
			checkpointAnchorDigest: pr.CheckpointAnchorDigest,
			needsRetry:             sessionNeedsRetry || atomicDAG,
			forceReplace:           res.forceReplace,
			sourceCwdResolution:    res.sourceCwdResolution,
			sourceCwdStored:        res.sourceCwdStored,
			sourceCwdStoredOK:      res.sourceCwdStoredOK,
		}
		if i == 0 {
			write.staged = res.staged
		}
		// The session upsert commits parser-derived parent provenance before
		// the later content, usage, and completion stages. Queue the attempted
		// session itself first so a failure after that upsert still re-resolves
		// its incoming spawn edges in the deferred repair pass.
		if err := e.db.QueueSubagentParentRepairs(ctx,
			[]string{resultIDs[i]},
		); err != nil {
			markSourceIncomplete()
			return false, sessionsChanged, fmt.Errorf(
				"queue attempted session parent repair: %w", err,
			)
		}
		repairQueued = true
		writeErr := e.writeSessionFullWithResolver(ctx, write, e.loadWorktreeProjectResolverContext(ctx))
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
			e.markStaleFailedMemberWrite(ctx, write)
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
	// DAG members stay stale until the source-level decision succeeds.
	sourceComplete := resolved == len(res.results) &&
		!preserved && !sourceNeedsRetry
	if !sourceComplete {
		markSourceIncomplete()
	}
	linked, err := e.db.LinkSubagentSessionsForSessions(ctx, resultIDs)
	if err != nil {
		markSourceIncomplete()
		return false, sessionsChanged, fmt.Errorf(
			"link changed subagent sessions: %w", err,
		)
	}
	sessionsChanged = sessionsChanged || linked > 0
	if sourceComplete && atomicDAG {
		if err := e.db.SetSessionDataVersionsContext(ctx,
			writtenIDs, db.CurrentDataVersion(),
		); err != nil {
			markSourceIncomplete()
			return false, sessionsChanged, fmt.Errorf(
				"complete DAG source data versions: %w", err,
			)
		}
	}
	if sourceComplete && res.providerStatHash != nil {
		e.recordProviderStatHash(ctx, *res.providerStatHash)
	}

	return preserved, sessionsChanged, nil
}

func sourceRequiresAtomicDAGCompletion(
	agent parser.AgentType, resultCount int,
) bool {
	if resultCount <= 1 {
		return false
	}
	return isClaudeFormatAgent(agent)
}

func (e *Engine) applyWorktreeMappingToSingleSession(
	ctx context.Context, sessionID string,
) (string, error) {
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

// kiroSQLiteDBName is the filename of the current-store Kiro SQLite DB.
const kiroSQLiteDBName = "data.sqlite3"

// parseKiroSQLiteVirtualPath splits a virtual Kiro SQLite source path back
// into its database path and raw session ID using the provider-neutral
// virtual-source-path resolver.
func parseKiroSQLiteVirtualPath(path string) (string, string, bool) {
	return parser.ParseVirtualSourcePathForBase(path, kiroSQLiteDBName)
}

// emit fires a refresh event if an emitter is wired. Safe to call
// with a nil emitter.
func (e *Engine) emit(scope string) {
	if e.emitter != nil {
		e.emitter.Emit(scope)
	}
}

func (e *Engine) codexMetadata() parser.CodexMetadata {
	if provider, ok := e.sources().providerStatHashers[parser.AgentCodex].(parser.CodexMetadataProvider); ok {
		return provider.Metadata()
	}
	return parser.CodexMetadata{}
}
