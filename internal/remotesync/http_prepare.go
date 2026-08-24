package remotesync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

var (
	ErrDuplicateMirrorLock = errors.New("duplicate mirror lock identity")
	ErrPreparedInUse       = errors.New("prepared HTTP sources are in use")
	ErrPreparedClosed      = errors.New("prepared HTTP sources are closing or closed")
)

type rebuildCachePersistError struct{ err error }

func (e *rebuildCachePersistError) Error() string { return e.err.Error() }
func (e *rebuildCachePersistError) Unwrap() error { return e.err }

// HostError attributes an HTTP preparation failure to its configured host.
// The cause remains available to errors.Is and errors.As callers.
type HostError struct {
	Host      string
	Operation string
	Err       error
}

func (e *HostError) Error() string {
	if e.Operation == "" {
		return fmt.Sprintf("HTTP host %q: %v", e.Host, e.Err)
	}
	return fmt.Sprintf("HTTP host %q %s: %v", e.Host, e.Operation, e.Err)
}

func (e *HostError) Unwrap() error { return e.Err }

// PreparedHTTPSyncs owns a deterministically prepared group of HTTP sources.
// Source ownership stays internal so locks and temporary roots can only be
// released as a group.
type PreparedHTTPSyncs struct {
	mu                    sync.Mutex
	sources               []*PreparedHTTP
	unavailableIDPrefixes []string
	closing               bool
	borrows               int
}

// PrepareHTTPSyncs prepares HTTP hosts in deterministic order, using canonical
// mirror-lock paths for persistent sources and host/URL keys for legacy
// temporary sources. It copies the input so callers retain their original
// ordering. When both the preparation and its unwind fail, the returned set is
// nonnil and callers must retry Close even though err is also nonnil.
func PrepareHTTPSyncs(
	ctx context.Context, syncs []HTTPSync,
) (*PreparedHTTPSyncs, error) {
	prepared, _, err := prepareHTTPSyncsWithUnavailable(ctx, syncs, false, func(
		ctx context.Context, hs HTTPSync,
	) (*PreparedHTTP, error) {
		return hs.Prepare(ctx)
	})
	return prepared, err
}

// PrepareAvailableHTTPSyncs prepares every reachable HTTP host. Unavailable
// hosts are omitted; other failures retain the all-or-nothing cleanup behavior
// of PrepareHTTPSyncs.
func PrepareAvailableHTTPSyncs(
	ctx context.Context, syncs []HTTPSync,
) (*PreparedHTTPSyncs, error) {
	prepared, _, err := prepareHTTPSyncsWithUnavailable(ctx, syncs, true, func(
		ctx context.Context, hs HTTPSync,
	) (*PreparedHTTP, error) {
		return hs.Prepare(ctx)
	})
	return prepared, err
}

func prepareHTTPSyncs(
	ctx context.Context,
	syncs []HTTPSync,
	prepare func(context.Context, HTTPSync) (*PreparedHTTP, error),
) (*PreparedHTTPSyncs, error) {
	prepared, _, err := prepareHTTPSyncsWithUnavailable(
		ctx, syncs, false, prepare,
	)
	return prepared, err
}

func prepareHTTPSyncsWithUnavailable(
	ctx context.Context,
	syncs []HTTPSync,
	skipUnavailable bool,
	prepare func(context.Context, HTTPSync) (*PreparedHTTP, error),
) (*PreparedHTTPSyncs, []*HostError, error) {
	type orderedSync struct {
		sync    HTTPSync
		sortKey string
	}
	ordered := make([]orderedSync, 0, len(syncs))
	seenLocks := make(map[string]HTTPSync, len(syncs))
	for _, hs := range syncs {
		if hs.DataDir == "" {
			// Legacy sources use isolated temporary roots and never acquire a
			// mirror lock. Give them a stable ordering without resolving a
			// nonexistent lock path (which would create cwd artifacts).
			ordered = append(ordered, orderedSync{
				sync: hs, sortKey: "legacy\x00" + hs.Host + "\x00" + hs.URL,
			})
			continue
		}
		lockPath, err := canonicalMirrorLockPath(MirrorDir(hs.DataDir, hs.Host))
		if err != nil {
			return nil, nil, &HostError{
				Host: hs.Host, Operation: "resolve mirror lock", Err: err,
			}
		}
		if previous, exists := seenLocks[lockPath]; exists {
			cause := fmt.Errorf(
				"%w: hosts %q and %q use %q",
				ErrDuplicateMirrorLock, previous.Host, hs.Host, lockPath,
			)
			return nil, nil, &HostError{
				Host: hs.Host, Operation: "resolve mirror lock", Err: cause,
			}
		}
		seenLocks[lockPath] = hs
		ordered = append(ordered, orderedSync{
			sync: hs, sortKey: "mirror\x00" + lockPath,
		})
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].sortKey < ordered[j].sortKey
	})

	prepared := &PreparedHTTPSyncs{
		sources: make([]*PreparedHTTP, 0, len(ordered)),
	}
	var unavailable []*HostError
	for _, input := range ordered {
		hs := input.sync
		if hs.Lifecycle != nil && hs.Lifecycle.PrepareStarted != nil {
			hs.Lifecycle.PrepareStarted()
		}
		source, err := prepare(ctx, hs)
		if hs.Lifecycle != nil && hs.Lifecycle.PrepareFinished != nil {
			hs.Lifecycle.PrepareFinished(err)
		}
		if err != nil {
			primary := &HostError{Host: hs.Host, Operation: "prepare", Err: err}
			if skipUnavailable && source == nil && ctx.Err() == nil &&
				IsHostUnavailable(err) {
				unavailable = append(unavailable, primary)
				prepared.unavailableIDPrefixes = append(
					prepared.unavailableIDPrefixes, rebuildIDPrefix(hs.Host),
				)
				hs.reportProgressDetail(
					"Skipped offline remote host " + hs.Host,
				)
				continue
			}
			if source != nil {
				prepared.sources = append(prepared.sources, source)
			}
			cleanupErr := prepared.Close()
			if cleanupErr != nil {
				return prepared, unavailable, errors.Join(primary, cleanupErr)
			}
			return nil, unavailable, primary
		}
		prepared.sources = append(prepared.sources, source)
	}
	return prepared, unavailable, nil
}

// BorrowRebuildOptions borrows rebuild descriptions and unavailable host
// namespaces while the prepared set retains cleanup ownership. Callers must
// invoke the idempotent release function after the rebuild stops using the
// options. Close refuses to release any source while a borrow remains active.
func (p *PreparedHTTPSyncs) BorrowRebuildOptions() (
	options syncpkg.RebuildOptions,
	release func(),
	err error,
) {
	if p == nil {
		return syncpkg.RebuildOptions{}, nil, ErrPreparedClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return syncpkg.RebuildOptions{}, nil, ErrPreparedClosed
	}
	options.Contributors = make(
		[]syncpkg.RebuildContributor, 0, len(p.sources),
	)
	options.UnavailableContributorIDPrefixes = slices.Clone(
		p.unavailableIDPrefixes,
	)
	for _, source := range p.sources {
		if source == nil {
			continue
		}
		contributor, err := source.RebuildContributor()
		if err != nil {
			return syncpkg.RebuildOptions{}, nil, &HostError{
				Host: source.sync.Host, Operation: "build rebuild contributor", Err: err,
			}
		}
		options.Contributors = append(options.Contributors, contributor)
	}
	p.borrows++
	var once sync.Once
	release = func() {
		once.Do(func() {
			p.mu.Lock()
			p.borrows--
			p.mu.Unlock()
		})
	}
	return options, release, nil
}

// Close releases prepared sources in reverse order. Successfully closed
// sources are forgotten; failed sources remain owned so cleanup can be retried.
func (p *PreparedHTTPSyncs) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closing = true
	if p.borrows > 0 {
		return ErrPreparedInUse
	}
	var cleanupErr error
	for i := range slices.Backward(p.sources) {
		source := p.sources[i]
		if source == nil {
			continue
		}
		if err := source.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, &HostError{
				Host: source.sync.Host, Operation: "cleanup prepared source", Err: err,
			})
			continue
		}
		p.sources[i] = nil
	}
	return cleanupErr
}

// Commit finalizes each successfully rebuilt persistent source. It is
// idempotent; a failure leaves every uncommitted source owned for retry.
func (p *PreparedHTTPSyncs) Commit() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, source := range p.sources {
		if source == nil {
			continue
		}
		if err := source.Commit(); err != nil {
			return &HostError{Host: source.sync.Host, Operation: "commit rebuild", Err: err}
		}
	}
	return nil
}

// PreparedHTTP is a downloaded remote source ready for active import. A
// persistent mirror remains locked until Close so another process cannot mutate
// files while the importer reads them. Legacy sources own their temporary root.
type PreparedHTTP struct {
	sync                      HTTPSync
	targets                   TargetSet
	root                      string
	lock                      *MirrorLockHandle
	cleanupRoot               bool
	closing                   bool
	closed                    bool
	removeRoot                func(string) error
	releaseLock               func(*MirrorLockHandle) error
	legacy                    bool
	mirrorImport              *preparedMirrorImport
	replaceJournal            func(string, MirrorChangeJournal) error
	retireJournal             func(string) error
	replaceRemoteSkippedFiles func(string, map[string]int64) error
	applyRemoteSkippedChanges func(string, []string, map[string]int64) error
	commitReady               bool
	committed                 bool
}

type preparedMirrorImport struct {
	journalPath string
	journal     MirrorChangeJournal
	mergeStats  JournalMergeStats
	fullReason  FullImportReason
	outcome     JournalOutcome
	reported    bool
	pending     *PreparedDeltaImport
}

// Prepare resolves the remote targets and prepares either the persistent
// manifest mirror or an isolated legacy archive without importing sessions.
// When preparation and automatic cleanup both fail, it returns a nonnil source
// with the error; callers must retry Close on every nonnil result. If cleanup
// succeeds, preparation failures retain the ordinary (nil, err) shape.
func (hs HTTPSync) Prepare(ctx context.Context) (*PreparedHTTP, error) {
	return hs.prepare(ctx, nil)
}

func (hs HTTPSync) prepare(
	ctx context.Context, configure func(*PreparedHTTP),
) (result *PreparedHTTP, err error) {
	client := hs.Client
	if client == nil {
		client = http.DefaultClient
	}
	hs.reportProgressResolvingTargets()
	targets, err := hs.fetchTargets(ctx, client)
	if err != nil {
		return nil, err
	}
	if err := validateTargetSetPaths(targets); err != nil {
		return nil, err
	}
	prepared := &PreparedHTTP{
		sync:                      hs,
		targets:                   targets,
		removeRoot:                os.RemoveAll,
		releaseLock:               func(lock *MirrorLockHandle) error { return lock.Close() },
		replaceJournal:            replaceMirrorChangeJournal,
		retireJournal:             retireMirrorChangeJournal,
		replaceRemoteSkippedFiles: hs.DB.ReplaceRemoteSkippedFiles,
		applyRemoteSkippedChanges: hs.DB.ApplyRemoteSkippedFileChanges,
	}
	if configure != nil {
		configure(prepared)
	}
	defer func() {
		if err != nil {
			if cleanupErr := prepared.Close(); cleanupErr != nil {
				result = prepared
				err = errors.Join(err, cleanupErr)
			}
		}
	}()

	if hs.DataDir == "" {
		prepared.root, err = hs.downloadAndExtract(ctx, client, targets)
		if err != nil {
			return nil, err
		}
		prepared.cleanupRoot = true
		prepared.legacy = true
		return prepared, nil
	}

	mirrorRoot, err := filepath.Abs(MirrorDir(hs.DataDir, hs.Host))
	if err != nil {
		return nil, fmt.Errorf("make persistent mirror root absolute: %w", err)
	}
	mirrorRoot = filepath.Clean(mirrorRoot)
	prepared.lock, err = AcquireMirrorLock(ctx, mirrorRoot)
	if err != nil {
		return nil, err
	}
	dirScoped, fileScoped := targets.SplitFileScoped()
	hs.reportProgressDetail(fmt.Sprintf(
		"Fetching session manifest from %s", hs.Host,
	))
	manifest, supported, err := hs.fetchManifest(ctx, client, dirScoped)
	if err != nil {
		return nil, err
	}
	if !supported {
		hs.reportLegacyFallback()
		prepared.root, err = hs.downloadAndExtract(ctx, client, targets)
		if err != nil {
			return nil, err
		}
		prepared.cleanupRoot = true
		prepared.legacy = true
		return prepared, nil
	}

	prepared.root = mirrorRoot
	prepared.mirrorImport, err = hs.prepareMirror(ctx, client, splitTargets{
		dirScoped:  dirScoped,
		fileScoped: fileScoped,
	}, manifest, mirrorRoot, prepared)
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

// Root returns the prepared source root.
func (p *PreparedHTTP) Root() string { return p.root }

// Targets returns the target set represented by Root.
func (p *PreparedHTTP) Targets() TargetSet { return p.targets }

// ImportActive imports the prepared source into the active database.
func (p *PreparedHTTP) ImportActive(ctx context.Context) (SyncStats, error) {
	if p == nil || p.closing || p.closed {
		return SyncStats{}, fmt.Errorf("prepared HTTP source is closed")
	}
	if p.mirrorImport == nil {
		if p.legacy {
			return p.sync.importRoot(ctx, p.targets, p.root)
		}
		return SyncStats{}, nil
	}
	stats, err := p.mirrorImport.pending.Execute(ctx)
	p.mirrorImport.outcome = stats.JournalOutcome
	if err != nil || !stats.ProcessingComplete() {
		p.reportDeltaImport(stats)
		return stats, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		stats.JournalOutcome = JournalCancelled
		p.mirrorImport.outcome = stats.JournalOutcome
		p.reportDeltaImport(stats)
		return stats, ctxErr
	}
	retireStart := time.Now()
	if err := p.retireJournal(p.mirrorImport.journalPath); err != nil {
		stats.RetirementDuration = time.Since(retireStart)
		stats.JournalOutcome = JournalRetirementFailed
		p.mirrorImport.outcome = stats.JournalOutcome
		p.reportDeltaImport(stats)
		return stats, fmt.Errorf("retire mirror change journal: %w", err)
	}
	stats.RetirementDuration = time.Since(retireStart)
	stats.JournalOutcome = JournalRetired
	p.mirrorImport.outcome = stats.JournalOutcome
	p.reportDeltaImport(stats)
	return stats, nil
}

func (p *PreparedHTTP) reportDeltaImport(stats SyncStats) {
	if p.mirrorImport != nil {
		p.mirrorImport.reported = true
	}
	p.sync.reportProgressDetail(fmt.Sprintf(
		"Synced %d sessions from %s (%d unchanged, %d error-suppressed)",
		stats.SessionsSynced, p.sync.Host, stats.Skipped, stats.ErrorSuppressed,
	))
	if stats.JournalOutcome != JournalRetired {
		p.sync.reportProgressDetail(fmt.Sprintf(
			"Pending import from %s %s", p.sync.Host, stats.JournalOutcome,
		))
	}
	log.Printf(
		"remote sync delta import: host=%s pending_paths=%d pending_new=%d pending_rearmed=%d pending_replayed=%d armed_paths=%d exact_sources=%d fallback_providers=%d fallback_sources=%d files_discovered=%d files_processed=%d prune_exact_scopes=%d prune_provider_scopes=%d prune_host_wide_scope=%t pruned_exact=%d pruned_provider=%d pruned_host_wide=%d sessions_synced=%d sessions_total=%d skipped=%d error_suppressed=%d failed=%d full_reason=%s journal_outcome=%s planning_duration=%s pruning_duration=%s processing_duration=%s cache_persist_duration=%s retirement_duration=%s",
		p.sync.Host, stats.PendingPaths, stats.PendingNew, stats.PendingRearmed,
		stats.PendingReplayed, stats.ArmedPaths, stats.ExactSources,
		stats.FallbackProviders, stats.FallbackSources, stats.FilesDiscovered,
		stats.FilesProcessed, stats.PruneExactScopes, stats.PruneProviderScopes,
		stats.PruneHostWideScope, stats.PrunedExact, stats.PrunedProvider,
		stats.PrunedHostWide, stats.SessionsSynced, stats.SessionsTotal,
		stats.Skipped, stats.ErrorSuppressed, stats.Failed, stats.FullReason,
		stats.JournalOutcome, stats.PlanningDuration.Round(time.Millisecond),
		stats.PruningDuration.Round(time.Millisecond),
		stats.ProcessingDuration.Round(time.Millisecond),
		stats.CachePersistDuration.Round(time.Millisecond),
		stats.RetirementDuration.Round(time.Millisecond),
	)
}

// RebuildContributor converts this prepared source into an atomic rebuild
// contributor. PreparedHTTP retains ownership of its root and mirror lock;
// callers must Close it after the rebuild finishes.
func (p *PreparedHTTP) RebuildContributor() (syncpkg.RebuildContributor, error) {
	if p == nil || p.closing || p.closed {
		return syncpkg.RebuildContributor{}, fmt.Errorf(
			"prepared HTTP source is closed",
		)
	}
	layout, config, err := newImportInputs(
		p.sync.Host, p.sync.BlockedResultCategories, p.targets, p.root,
	)
	if err != nil {
		return syncpkg.RebuildContributor{}, err
	}
	persistSkipCache := func(engine *syncpkg.Engine, database *db.DB) error {
		var err error
		if p.mirrorImport != nil {
			err = p.mirrorImport.pending.persistSkipCache(database, engine)
		} else {
			err = saveEngineSkipCache(database, engine, layout.paths)
		}
		if err != nil {
			return &rebuildCachePersistError{err: err}
		}
		return nil
	}
	persistRetrySafeSkipCache := func(
		engine *syncpkg.Engine, database *db.DB,
	) error {
		var err error
		if p.mirrorImport != nil {
			err = p.mirrorImport.pending.persistRetrySafeSkipCache(database, engine)
		} else {
			remoteCache := remoteRetrySafeEngineSkipCache(engine, layout.paths)
			err = database.ReplaceRemoteSkippedFiles(layout.paths.host, remoteCache)
		}
		if err != nil {
			return &rebuildCachePersistError{err: err}
		}
		return nil
	}
	persistSuccessfulImport := func(
		engine *syncpkg.Engine, database *db.DB,
	) error {
		if err := persistSkipCache(engine, database); err != nil {
			return err
		}
		if p.mirrorImport == nil {
			return nil
		}
		if err := p.mirrorImport.pending.persistImportDataVersion(
			context.Background(), database,
		); err != nil {
			return &rebuildCachePersistError{err: err}
		}
		return nil
	}
	forceParse := p.sync.forceParseRequested()
	forceFullParseAfterCache := p.sync.forceFullParseAfterCacheRequested()
	contributor := syncpkg.RebuildContributor{
		Name:                     p.sync.Host,
		Config:                   config,
		ForceParse:               forceParse,
		ForceFullParseAfterCache: forceFullParseAfterCache,
		Progress: func(progress syncpkg.Progress) syncpkg.Progress {
			return transformHostProgress(p.sync.Host, progress)
		},
		AfterSync:    persistSuccessfulImport,
		AfterFailure: persistRetrySafeSkipCache,
	}
	if p.mirrorImport != nil {
		contributor.ForceParse = p.mirrorImport.pending.forceParse
		contributor.ForceFullParseAfterCache =
			p.mirrorImport.pending.forceFullParse
		contributor.Config.InitialSkipCache = p.mirrorImport.pending.cache
	}
	if p.sync.Lifecycle != nil {
		contributor.Started = p.sync.Lifecycle.RebuildStarted
	}
	contributor.Finished = func(stats syncpkg.SyncStats, err error) {
		p.commitReady = err == nil && stats.ProcessingComplete()
		if p.mirrorImport != nil {
			pendingStats := &p.mirrorImport.pending.Stats
			pendingStats.SessionsSynced = stats.Synced
			pendingStats.SessionsTotal = stats.TotalSessions
			pendingStats.Skipped = stats.Skipped
			pendingStats.Failed = stats.Failed
			if p.commitReady {
				p.mirrorImport.outcome = JournalPendingSwap
				pendingStats.JournalOutcome = JournalPendingSwap
			} else if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				p.mirrorImport.outcome = JournalCancelled
				pendingStats.JournalOutcome = JournalCancelled
			} else {
				if _, ok := errors.AsType[*rebuildCachePersistError](err); ok {
					p.mirrorImport.outcome = JournalCachePersistFailed
					pendingStats.JournalOutcome = JournalCachePersistFailed
				} else if !stats.ProcessingComplete() || err != nil {
					p.mirrorImport.outcome = JournalProcessingFailures
					pendingStats.JournalOutcome = JournalProcessingFailures
				}
			}
		}
		if p.sync.Lifecycle != nil && p.sync.Lifecycle.RebuildFinished != nil {
			p.sync.Lifecycle.RebuildFinished(stats, err)
		}
	}
	return contributor, nil
}

// Commit retires a processed persistent journal after an atomic rebuild swap.
func (p *PreparedHTTP) Commit() error {
	if p == nil || p.mirrorImport == nil || p.committed {
		return nil
	}
	if !p.commitReady {
		return fmt.Errorf("prepared HTTP journal is not ready to commit")
	}
	start := time.Now()
	if err := p.retireJournal(p.mirrorImport.journalPath); err != nil {
		p.mirrorImport.pending.Stats.RetirementDuration = time.Since(start)
		p.mirrorImport.outcome = JournalRetirementFailed
		p.mirrorImport.pending.Stats.JournalOutcome = JournalRetirementFailed
		p.reportDeltaImport(p.mirrorImport.pending.Stats)
		return fmt.Errorf("retire mirror change journal after rebuild: %w", err)
	}
	p.committed = true
	p.mirrorImport.outcome = JournalRetired
	p.mirrorImport.pending.Stats.JournalOutcome = JournalRetired
	p.mirrorImport.pending.Stats.RetirementDuration = time.Since(start)
	p.reportDeltaImport(p.mirrorImport.pending.Stats)
	return nil
}

// Close releases the mirror lock and removes an owned legacy root. It is safe
// to call more than once.
func (p *PreparedHTTP) Close() error {
	if p == nil || p.closed {
		return nil
	}
	if p.mirrorImport != nil && !p.mirrorImport.reported &&
		p.mirrorImport.outcome.Valid() && p.mirrorImport.outcome != JournalRetired {
		stats := p.mirrorImport.pending.Stats
		stats.JournalOutcome = p.mirrorImport.outcome
		p.reportDeltaImport(stats)
	}
	p.closing = true
	var cleanupErr, lockErr error
	if p.cleanupRoot && p.root != "" {
		if err := p.removeRoot(p.root); err != nil {
			cleanupErr = fmt.Errorf("remove prepared HTTP root: %w", err)
		} else {
			p.cleanupRoot = false
		}
	}
	if p.lock != nil {
		if err := p.releaseLock(p.lock); err != nil {
			lockErr = err
		} else {
			p.lock = nil
		}
	}
	p.closed = !p.cleanupRoot && p.lock == nil
	return errors.Join(cleanupErr, lockErr)
}

func (hs HTTPSync) reportProgressResolvingTargets() {
	hs.reportProgressDetail(fmt.Sprintf("Resolving agent directories on %s", hs.Host))
}

func (hs HTTPSync) reportLegacyFallback() {
	hs.reportProgressDetail(fmt.Sprintf(
		"Remote %s does not support incremental sync; downloading full archive",
		hs.Host,
	))
}

// splitTargets carries a target set alongside its SplitFileScoped partition so
// mirror preparation addresses each half without re-deriving it.
type splitTargets struct {
	dirScoped  TargetSet
	fileScoped TargetSet
}

func mergeAgentPaths(
	base map[parser.AgentType][]string,
	extra map[parser.AgentType][]string,
) map[parser.AgentType][]string {
	merged := make(map[parser.AgentType][]string, len(base)+len(extra))
	for agent, paths := range base {
		merged[agent] = append([]string(nil), paths...)
	}
	for agent, paths := range extra {
		for _, path := range paths {
			if !slices.Contains(merged[agent], path) {
				merged[agent] = append(merged[agent], path)
			}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func addTargetDirs(targets TargetSet, dirs map[parser.AgentType][]string) TargetSet {
	targets.Dirs = mergeAgentPaths(targets.Dirs, dirs)
	return targets
}

func mirrorFileScopedPaths(
	mirrorRoot string, targets TargetSet,
) (map[string]struct{}, error) {
	remotePaths := make([]string, 0)
	for _, paths := range targets.Files {
		remotePaths = append(remotePaths, paths...)
	}
	remotePaths = append(remotePaths, targets.AllExtraFiles()...)
	result := make(map[string]struct{}, len(remotePaths))
	for _, remotePath := range remotePaths {
		path, err := mirrorRelativeRemoteChangePath(mirrorRoot, remotePath)
		if err != nil {
			return nil, err
		}
		result[path] = struct{}{}
	}
	return result, nil
}

func mirrorFileScopedRoots(
	mirrorRoot string, dirs map[parser.AgentType][]string,
) ([]string, error) {
	var roots []string
	for _, remoteDirs := range dirs {
		for _, remoteDir := range remoteDirs {
			root, err := safeRemappedRemotePath(mirrorRoot, remoteDir)
			if err != nil {
				return nil, err
			}
			roots = append(roots, root)
		}
	}
	return roots, nil
}

// prepareMirror applies the manifest delta to the persistent mirror. The
// caller holds the mirror lock from before the manifest fetch until the
// prepared source is closed.
func (hs HTTPSync) prepareMirror(
	ctx context.Context,
	client *http.Client,
	targets splitTargets,
	manifest Manifest,
	mirrorRoot string,
	prepared *PreparedHTTP,
) (result *preparedMirrorImport, retErr error) {
	fullReason := hs.FullReason
	if hs.Full && fullReason == "" {
		fullReason = FullImportExplicit
	}
	defer func() {
		if retErr == nil {
			return
		}
		hs.reportProgressDetail(fmt.Sprintf(
			"Pending import from %s %s",
			hs.Host, JournalAbortedBeforeProcessing,
		))
		log.Printf(
			"remote sync delta import: host=%s full_reason=%s journal_outcome=%s",
			hs.Host, fullReason, JournalAbortedBeforeProcessing,
		)
	}()
	journalPath := mirrorJournalPath(mirrorRoot)
	journal, err := loadMirrorChangeJournal(journalPath)
	if err != nil {
		if !errors.Is(err, ErrMalformedMirrorJournal) {
			return nil, err
		}
		journal = MirrorChangeJournal{
			Version: mirrorJournalVersion, FullImport: true,
			FullImportReason: FullImportJournalRecovery, InvalidateAll: true,
			ForceFullParseAll: true,
		}
		if replaceErr := prepared.replaceJournal(journalPath, journal); replaceErr != nil {
			return nil, fmt.Errorf("persist mirror journal recovery marker: %w", replaceErr)
		}
	}
	_, mirrorStatErr := os.Stat(mirrorRoot)
	bootstrap := errors.Is(mirrorStatErr, os.ErrNotExist)
	delta, err := MirrorDiff(mirrorRoot, manifest)
	if err != nil {
		return nil, err
	}
	hs.reportProgressDetail(fmt.Sprintf(
		"Compared session manifest from %s: %d total, %d changed, %d deleted",
		hs.Host, delta.Total, len(delta.Fetch), len(delta.Deletions),
	))
	pendingFileScopedDirs := journal.FileScopedDirs
	fileScopedDirs := targets.fileScoped.Dirs
	if pendingFileScopedDirs != nil {
		fileScopedDirs = pendingFileScopedDirs
		prepared.targets = addTargetDirs(prepared.targets, pendingFileScopedDirs)
	}
	fileScopedPaths, err := mirrorFileScopedPaths(mirrorRoot, targets.fileScoped)
	if err != nil {
		return nil, err
	}
	fileScopedRoots, err := mirrorFileScopedRoots(mirrorRoot, fileScopedDirs)
	if err != nil {
		return nil, err
	}
	fileScopedChange := func(relativePath string) (bool, error) {
		if _, exact := fileScopedPaths[relativePath]; exact {
			return true, nil
		}
		localPath, pathErr := safeLocalArchivePath(mirrorRoot, relativePath)
		if pathErr != nil {
			return false, pathErr
		}
		for _, root := range fileScopedRoots {
			if within(root, localPath) {
				return true, nil
			}
		}
		return false, nil
	}
	// A disarmed journal represents a mirror snapshot that is already ready to
	// process. Full journals cannot retain path entries, so their host-wide flag
	// selects replay while the saved provider roots protect and classify the
	// sanitized snapshot if the current target response changed.
	deferFileScopedRefresh := journal.FullImport && !journal.InvalidateAll
	if !deferFileScopedRefresh {
		for _, entry := range journal.Entries {
			fileScoped, classifyErr := fileScopedChange(entry.Path)
			if classifyErr != nil {
				return nil, classifyErr
			}
			if fileScoped && !entry.InvalidateCache {
				deferFileScopedRefresh = true
				break
			}
		}
	}
	if deferFileScopedRefresh {
		kept := delta.Deletions[:0]
		for _, localPath := range delta.Deletions {
			path, normalizeErr := mirrorRelativeLocalChangePath(
				mirrorRoot, localPath,
			)
			if normalizeErr != nil {
				return nil, normalizeErr
			}
			fileScoped, classifyErr := fileScopedChange(path)
			if classifyErr != nil {
				return nil, classifyErr
			}
			if fileScoped {
				continue
			}
			kept = append(kept, localPath)
		}
		delta.Deletions = kept
	}
	refreshFileScoped := !targets.fileScoped.IsEmpty() && !deferFileScopedRefresh
	mutatesMirror := len(delta.Deletions) > 0 || len(delta.Fetch) > 0 ||
		refreshFileScoped
	observed := make([]string, 0, len(delta.Fetch)+len(delta.Deletions))
	forceFullParseObserved := make([]string, 0, len(delta.Fetch))
	for _, remotePath := range delta.Fetch {
		path, normalizeErr := mirrorRelativeRemoteChangePath(mirrorRoot, remotePath)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		observed = append(observed, path)
		forceFullParseObserved = append(forceFullParseObserved, path)
	}
	for _, localPath := range delta.Deletions {
		path, normalizeErr := mirrorRelativeLocalChangePath(mirrorRoot, localPath)
		if normalizeErr != nil {
			return nil, normalizeErr
		}
		observed = append(observed, path)
	}
	if refreshFileScoped {
		for path := range fileScopedPaths {
			observed = append(observed, path)
			forceFullParseObserved = append(forceFullParseObserved, path)
		}
	}
	journal, mergeStats, err := mergeMirrorChangesWithForce(
		journal, observed, forceFullParseObserved,
	)
	if err != nil {
		return nil, err
	}
	captureCurrentFileScopedDirs := refreshFileScoped ||
		(deferFileScopedRefresh && pendingFileScopedDirs == nil &&
			len(targets.fileScoped.Dirs) > 0)
	if pendingFileScopedDirs != nil || captureCurrentFileScopedDirs {
		fileScopedSnapshot := mergeAgentPaths(nil, pendingFileScopedDirs)
		if captureCurrentFileScopedDirs {
			fileScopedSnapshot = mergeAgentPaths(
				fileScopedSnapshot, targets.fileScoped.Dirs,
			)
		}
		journal, err = attachFileScopedJournalDirs(journal, fileScopedSnapshot)
		if err != nil {
			return nil, err
		}
	}
	currentDataVersion := db.CurrentDataVersion()
	importedDataVersion, err := hs.DB.RemoteImportDataVersion(ctx, hs.Host)
	if err != nil {
		return nil, err
	}
	dataRebuildRequested := hs.forceFullParseAfterCacheRequested()
	dataRebuildPending := importedDataVersion != currentDataVersion ||
		dataRebuildRequested || journal.RequiredDataVersion != 0
	if dataRebuildPending && journal.RequiredDataVersion != currentDataVersion {
		if bootstrap && !dataRebuildRequested && !journal.FullImport {
			journal.RequiredDataVersion = currentDataVersion
			journal.DataRebuildCacheVersion = 0
		} else {
			journal = requireMirrorDataVersionImport(journal, currentDataVersion)
		}
	}
	if journal.RequiredDataVersion != 0 && fullReason == "" {
		fullReason = FullImportDataRebuild
		if bootstrap {
			fullReason = FullImportBootstrap
		}
	}
	if fullReason == FullImportExplicit {
		if !journal.FullImport {
			journal = MirrorChangeJournal{
				Version:                 mirrorJournalVersion,
				FullImport:              true,
				FullImportReason:        FullImportExplicit,
				RequiredDataVersion:     journal.RequiredDataVersion,
				DataRebuildCacheVersion: journal.DataRebuildCacheVersion,
				FileScopedDirs:          journal.FileScopedDirs,
			}
		}
		journal.InvalidateAll = true
		journal.ForceFullParseAll = true
	}
	if journal.FullImportReason != "" {
		fullReason = journal.FullImportReason
	} else if fullReason == "" && bootstrap {
		fullReason = FullImportBootstrap
	}
	hasPending := journal.FullImport || len(journal.Entries) > 0 ||
		fullReason != "" || hs.Full
	if !mutatesMirror && !hasPending {
		return nil, nil
	}
	if err := prepared.replaceJournal(journalPath, journal); err != nil {
		return nil, fmt.Errorf("persist pending mirror changes: %w", err)
	}
	// Deletions precede extraction so remote file/directory type changes cannot
	// wedge the mirror. File-scoped content is absent from the manifest and is
	// re-populated by its separate full archive below.
	if err := ApplyMirrorDeletions(mirrorRoot, delta.Deletions); err != nil {
		return nil, err
	}
	if err := RemoveMirrorTypeConflicts(mirrorRoot, delta.Fetch); err != nil {
		return nil, err
	}
	if len(delta.Fetch) > 0 {
		full := len(delta.Fetch)*2 >= delta.Total
		err := hs.downloadIntoMirror(
			ctx, client, targets.dirScoped, delta.Fetch, full, mirrorRoot,
		)
		var statusErr *StatusError
		if err != nil && !full && errors.As(err, &statusErr) {
			err = hs.downloadIntoMirror(
				ctx, client, targets.dirScoped, delta.Fetch, true, mirrorRoot,
			)
		}
		if err != nil {
			return nil, err
		}
	}
	if refreshFileScoped {
		if err := hs.downloadIntoMirror(
			ctx, client, targets.fileScoped, nil, true, mirrorRoot,
		); err != nil {
			return nil, err
		}
	}
	hs.reportProgressDetail(fmt.Sprintf(
		"Planning import from %s: %d pending paths", hs.Host, len(journal.Entries),
	))
	dataRebuild := journal.RequiredDataVersion != 0
	attemptCacheDataVersion := 0
	if dataRebuild {
		attemptCacheDataVersion = currentDataVersion
	}
	pending, err := (Importer{
		Host: hs.Host, Full: hs.Full, DB: hs.DB,
		BlockedResultCategories: hs.BlockedResultCategories,
		Progress:                hs.Progress, Targets: prepared.targets, Root: mirrorRoot,
		replaceRemoteSkippedFiles: prepared.replaceRemoteSkippedFiles,
		applyRemoteSkippedChanges: prepared.applyRemoteSkippedChanges,
	}).PreparePending(ctx, DeltaImportRequest{
		Journal: journal, FullReason: fullReason,
		ForceParse:               hs.forceParseRequested(),
		ForceFullParseAfterCache: dataRebuild,
		ResetAttemptCache: dataRebuild &&
			journal.DataRebuildCacheVersion != currentDataVersion,
		AttemptCacheDataVersion: attemptCacheDataVersion,
	})
	if err != nil {
		return nil, err
	}
	pending.Stats.PendingNew = mergeStats.New
	pending.Stats.PendingRearmed = mergeStats.Rearmed
	pending.Stats.PendingReplayed = mergeStats.Replayed
	if pending.Stats.FullReason != "" {
		hs.reportProgressDetail(fmt.Sprintf(
			"Planned full import from %s (%s)", hs.Host, pending.Stats.FullReason,
		))
	} else {
		hs.reportProgressDetail(fmt.Sprintf(
			"Planned import from %s: %d exact sources, %d provider fallback",
			hs.Host, pending.Stats.ExactSources, pending.Stats.FallbackProviders,
		))
	}
	if err := prepared.replaceJournal(journalPath, pending.DisarmedJournal); err != nil {
		return nil, fmt.Errorf("persist disarmed mirror changes: %w", err)
	}
	return &preparedMirrorImport{
		journalPath: journalPath, journal: pending.DisarmedJournal,
		mergeStats: mergeStats, fullReason: fullReason,
		outcome: JournalAbortedBeforeProcessing, pending: pending,
	}, nil
}

func requireMirrorDataVersionImport(
	journal MirrorChangeJournal, version int,
) MirrorChangeJournal {
	return MirrorChangeJournal{
		Version:             mirrorJournalVersion,
		FullImport:          true,
		FullImportReason:    journal.FullImportReason,
		InvalidateAll:       true,
		ForceFullParseAll:   true,
		RequiredDataVersion: version,
		FileScopedDirs:      journal.FileScopedDirs,
	}
}
