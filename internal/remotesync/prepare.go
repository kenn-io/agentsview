package remotesync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncpkg "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/atomicfile"
)

var (
	ErrDuplicateMirrorLock = errors.New("duplicate mirror lock identity")
	ErrPreparedInUse       = errors.New("prepared remote sources are in use")
	ErrPreparedClosed      = errors.New("prepared remote sources are closing or closed")
)

type rebuildCachePersistError struct{ err error }

func (e *rebuildCachePersistError) Error() string { return e.err.Error() }
func (e *rebuildCachePersistError) Unwrap() error { return e.err }

// HostError attributes a remote preparation failure to its configured host.
// The cause remains available to errors.Is and errors.As callers.
type HostError struct {
	Host      string
	Operation string
	Err       error
}

func (e *HostError) Error() string {
	if e.Operation == "" {
		return fmt.Sprintf("remote host %q: %v", e.Host, e.Err)
	}
	return fmt.Sprintf("remote host %q %s: %v", e.Host, e.Operation, e.Err)
}

func (e *HostError) Unwrap() error { return e.Err }

// PreparedRemoteSyncs owns a deterministically prepared group of remote sources.
// Source ownership stays internal so locks and temporary roots can only be
// released as a group.
type PreparedRemoteSyncs struct {
	mu                    sync.Mutex
	sources               []*PreparedRemote
	unavailableIDPrefixes []string
	closing               bool
	borrows               int
}

// PrepareRemoteSyncs prepares up to four remote hosts concurrently. Persistent
// mirror locks are acquired in canonical path order; results retain that order,
// with legacy temporary sources ordered by host/URL. Callbacks are serialized.
// It copies the input so callers retain their original ordering. When both the
// preparation and its unwind fail, the returned set is nonnil and callers must
// retry Close even though err is also nonnil.
func PrepareRemoteSyncs(
	ctx context.Context, syncs []RemoteSync,
) (*PreparedRemoteSyncs, error) {
	prepared, _, err := prepareRemoteSyncsWithUnavailable(ctx, syncs, false, func(
		ctx context.Context, hs RemoteSync,
	) (*PreparedRemote, error) {
		return hs.Prepare(ctx)
	})
	return prepared, err
}

// PrepareAvailableRemoteSyncs prepares every reachable remote host. Unavailable
// hosts are omitted; other failures retain the all-or-nothing cleanup behavior
// of PrepareRemoteSyncs.
func PrepareAvailableRemoteSyncs(
	ctx context.Context, syncs []RemoteSync,
) (*PreparedRemoteSyncs, error) {
	prepared, _, err := prepareRemoteSyncsWithUnavailable(ctx, syncs, true, func(
		ctx context.Context, hs RemoteSync,
	) (*PreparedRemote, error) {
		return hs.Prepare(ctx)
	})
	return prepared, err
}

func prepareRemoteSyncs(
	ctx context.Context,
	syncs []RemoteSync,
	prepare func(context.Context, RemoteSync) (*PreparedRemote, error),
) (*PreparedRemoteSyncs, error) {
	prepared, _, err := prepareRemoteSyncsWithUnavailable(
		ctx, syncs, false, prepare,
	)
	return prepared, err
}

func prepareRemoteSyncsWithUnavailable(
	ctx context.Context,
	syncs []RemoteSync,
	skipUnavailable bool,
	prepare func(context.Context, RemoteSync) (*PreparedRemote, error),
) (*PreparedRemoteSyncs, []*HostError, error) {
	type orderedSync struct {
		sync    RemoteSync
		sortKey string
		advance func()
	}
	ordered := make([]orderedSync, 0, len(syncs))
	seenLocks := make(map[string]RemoteSync, len(syncs))
	for _, hs := range syncs {
		if hs.DataDir == "" {
			// Legacy sources use isolated temporary roots and never acquire a
			// mirror lock. Give them a stable ordering without resolving a
			// nonexistent lock path (which would create cwd artifacts).
			ordered = append(ordered, orderedSync{
				sync: hs, sortKey: legacySortKey(hs),
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

	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var callbacks sync.Mutex
	previous := make(chan struct{})
	close(previous)
	for i := range ordered {
		hs := &ordered[i].sync
		if report := hs.Progress; report != nil {
			hs.Progress = func(progress syncpkg.Progress) {
				callbacks.Lock()
				defer callbacks.Unlock()
				report(progress)
			}
		}
		if hs.DataDir == "" {
			continue
		}
		before, acquired := previous, make(chan struct{})
		previous = acquired
		var once sync.Once
		advance := func() {
			// A host that fails before locking must still wait its turn before
			// admitting the next host, unless the whole group is stopping.
			select {
			case <-before:
			case <-workCtx.Done():
			}
			once.Do(func() { close(acquired) })
		}
		ordered[i].advance = advance
		acquire := hs.acquireMirrorLock
		if acquire == nil {
			acquire = AcquireMirrorLock
		}
		hs.acquireMirrorLock = func(ctx context.Context, root string) (*MirrorLockHandle, error) {
			select {
			case <-before:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			lock, err := acquire(ctx, root)
			if err == nil {
				advance()
			}
			return lock, err
		}
	}

	type preparationResult struct {
		source      *PreparedRemote
		err         *HostError
		unavailable bool
	}
	results := make([]preparationResult, len(ordered))
	var failureMu sync.Mutex
	var primary *HostError
	run := func(i int) {
		input := ordered[i]
		if input.advance != nil {
			defer input.advance()
		}
		if workCtx.Err() != nil {
			return
		}
		hs := input.sync
		if hs.Lifecycle != nil && hs.Lifecycle.PrepareStarted != nil {
			callbacks.Lock()
			hs.Lifecycle.PrepareStarted()
			callbacks.Unlock()
		}
		source, err := prepare(workCtx, hs)
		result := preparationResult{source: source}
		if err != nil {
			result.err = &HostError{Host: hs.Host, Operation: "prepare", Err: err}
			result.unavailable = skipUnavailable && source == nil &&
				workCtx.Err() == nil && IsHostUnavailable(err)
			if !result.unavailable {
				failureMu.Lock()
				if primary == nil {
					primary = result.err
					cancel()
				}
				failureMu.Unlock()
			}
		}
		if hs.Lifecycle != nil && hs.Lifecycle.PrepareFinished != nil {
			callbacks.Lock()
			hs.Lifecycle.PrepareFinished(err)
			callbacks.Unlock()
		}
		results[i] = result
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(4, len(ordered)) {
		workers.Go(func() {
			for i := range jobs {
				run(i)
			}
		})
	}
dispatch:
	for i := range ordered {
		// Dispatch in lock order: later jobs must not occupy every worker
		// while waiting for a predecessor that has not started yet.
		select {
		case jobs <- i:
		case <-workCtx.Done():
			break dispatch
		}
	}
	close(jobs)
	workers.Wait()

	prepared := &PreparedRemoteSyncs{
		sources: make([]*PreparedRemote, 0, len(ordered)),
	}
	var unavailable []*HostError
	var failure error
	if primary != nil {
		failure = primary
	}
	for i, result := range results {
		if result.source != nil {
			prepared.sources = append(prepared.sources, result.source)
		}
		if result.unavailable {
			hs := ordered[i].sync
			unavailable = append(unavailable, result.err)
			prepared.unavailableIDPrefixes = append(
				prepared.unavailableIDPrefixes, rebuildIDPrefix(hs.Host),
			)
			hs.reportProgressDetail("Skipped offline remote host " + hs.Host)
			continue
		}
		if result.err == nil || result.err == primary {
			continue
		}
		if ctx.Err() == nil && errors.Is(result.err, context.Canceled) {
			// Sibling cancellation must not turn the original failure into a
			// user cancellation. Spool cleanup can own resources without a
			// PreparedRemote, so retry and retain those owners independently.
			for _, retrier := range cleanupRetriers(result.err) {
				if cleanupErr := retrier.RetryCleanup(); cleanupErr != nil {
					failure = errors.Join(failure, &retainedCleanupError{
						cause:    &HostError{Host: ordered[i].sync.Host, Operation: "cleanup canceled preparation", Err: cleanupErr},
						retriers: []cleanupRetrier{retrier},
					})
				}
			}
			continue
		}
		failure = errors.Join(failure, result.err)
	}
	if ctx.Err() != nil {
		failure = errors.Join(failure, ctx.Err())
	}
	if failure != nil {
		if cleanupErr := prepared.Close(); cleanupErr != nil {
			return prepared, unavailable, errors.Join(failure, cleanupErr)
		}
		return nil, unavailable, failure
	}
	return prepared, unavailable, nil
}

// BorrowRebuildOptions borrows rebuild descriptions and unavailable host
// namespaces while the prepared set retains cleanup ownership. Callers must
// invoke the idempotent release function after the rebuild stops using the
// options. Close refuses to release any source while a borrow remains active.
func (p *PreparedRemoteSyncs) BorrowRebuildOptions(ctx context.Context) (
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
		contributor, err := source.RebuildContributor(ctx)
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
func (p *PreparedRemoteSyncs) Close() error {
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
func (p *PreparedRemoteSyncs) Commit() error {
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

// PreparedRemote is a downloaded remote source ready for active import. A
// persistent mirror remains locked until Close so another process cannot mutate
// files while the importer reads them. Legacy sources own their temporary root.
type PreparedRemote struct {
	sync                      RemoteSync
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
	replaceRemoteSkippedFiles func(context.Context, string, map[string]int64) error
	applyRemoteSkippedChanges func(context.Context, string, []string, map[string]int64) error
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
func (hs RemoteSync) Prepare(ctx context.Context) (*PreparedRemote, error) {
	return hs.prepare(ctx, nil)
}

func (hs RemoteSync) prepare(
	ctx context.Context, configure func(*PreparedRemote),
) (result *PreparedRemote, err error) {
	if hs.Source == nil {
		return nil, errors.New("remote sync source is required")
	}
	if reporter, ok := hs.Source.(ProgressReporter); ok {
		reporter.ReportProgress(hs.report)
	}
	hs.reportProgressResolvingTargets()
	targetsStarted := time.Now()
	targets, err := hs.Source.Targets(ctx)
	logRemotePhase(hs.Host, "targets", targetsStarted)
	if err != nil {
		return nil, err
	}
	if err := validateTargetSetPaths(targets); err != nil {
		return nil, err
	}
	prepared := &PreparedRemote{
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
		prepared.root, err = hs.downloadAndExtract(ctx, targets)
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
	acquire := hs.acquireMirrorLock
	if acquire == nil {
		acquire = AcquireMirrorLock
	}
	prepared.lock, err = acquire(ctx, mirrorRoot)
	if err != nil {
		return nil, err
	}
	dirScoped, fileScoped := targets.SplitFileScoped()
	hs.reportProgressDetail("Fetching session manifest from " + hs.Host)
	manifestStarted := time.Now()
	manifest, supported, err := hs.Source.Manifest(ctx, dirScoped)
	logRemotePhase(hs.Host, "manifest", manifestStarted)
	if err != nil {
		return nil, err
	}
	if !supported {
		hs.reportLegacyFallback()
		prepared.root, err = hs.downloadAndExtract(ctx, targets)
		if err != nil {
			return nil, err
		}
		prepared.cleanupRoot = true
		prepared.legacy = true
		return prepared, nil
	}

	prepared.root = mirrorRoot
	prepared.mirrorImport, err = hs.prepareMirror(ctx, splitTargets{
		dirScoped:  dirScoped,
		fileScoped: fileScoped,
	}, manifest, mirrorRoot, prepared)
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

// logRemotePhase records how long one preparation phase took for host.
func logRemotePhase(host, phase string, started time.Time) {
	log.Printf("remote sync: host=%s phase=%s duration=%s",
		host, phase, time.Since(started).Round(time.Millisecond))
}

// Root returns the prepared source root.
func (p *PreparedRemote) Root() string { return p.root }

// Targets returns the target set represented by Root.
func (p *PreparedRemote) Targets() TargetSet { return p.targets }

// ImportActive imports the prepared source into the active database.
func (p *PreparedRemote) ImportActive(ctx context.Context) (SyncStats, error) {
	if p == nil || p.closing || p.closed {
		return SyncStats{}, errors.New("prepared remote source is closed")
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
	if err := p.persistAntigravityTitleStates(); err != nil {
		log.Printf("Antigravity title verification: %v", err)
	}
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

func (p *PreparedRemote) reportDeltaImport(stats SyncStats) {
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
// contributor. PreparedRemote retains ownership of its root and mirror lock;
// callers must Close it after the rebuild finishes.
func (p *PreparedRemote) RebuildContributor(
	ctx context.Context,
) (syncpkg.RebuildContributor, error) {
	if p == nil || p.closing || p.closed {
		return syncpkg.RebuildContributor{}, errors.New("prepared remote source is closed")
	}
	layout, config, err := newImportInputs(
		p.sync.Host, p.sync.BlockedResultCategories, p.targets, p.root,
	)
	if err != nil {
		return syncpkg.RebuildContributor{}, err
	}
	config.ArchiveContent = p.sync.DB.ArchiveContent()
	config.CompleteSourceMirror = true
	persistSkipCache := func(engine *syncpkg.Engine, database *db.DB) error {
		var err error
		if p.mirrorImport != nil {
			err = p.mirrorImport.pending.persistSkipCache(ctx, database, engine)
		} else {
			err = saveEngineSkipCache(ctx, database, engine, layout.paths)
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
			err = p.mirrorImport.pending.persistRetrySafeSkipCache(ctx, database, engine)
		} else {
			remoteCache := remoteRetrySafeEngineSkipCache(engine, layout.paths)
			err = database.ReplaceRemoteSkippedFiles(ctx, layout.paths.host, remoteCache)
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
		p.mirrorImport.pending.antigravityTitleStates = engine.SnapshotAntigravityTitleStates()
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
		contributor.ForceFullParseAfterCache = p.mirrorImport.pending.forceFullParse
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
func (p *PreparedRemote) Commit() error {
	if p == nil || p.mirrorImport == nil || p.committed {
		return nil
	}
	if !p.commitReady {
		return errors.New("prepared remote journal is not ready to commit")
	}
	start := time.Now()
	if err := p.persistAntigravityTitleStates(); err != nil {
		log.Printf("Antigravity title verification: %v", err)
	}
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
func (p *PreparedRemote) Close() error {
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
			cleanupErr = fmt.Errorf("remove prepared remote root: %w", err)
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

func (hs RemoteSync) reportProgressResolvingTargets() {
	hs.reportProgressDetail("Resolving agent directories on " + hs.Host)
}

func (hs RemoteSync) reportLegacyFallback() {
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
func (hs RemoteSync) prepareMirror(
	ctx context.Context,
	targets splitTargets,
	manifest Manifest,
	mirrorRoot string,
	prepared *PreparedRemote,
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
	compareStarted := time.Now()
	delta, err := MirrorDiff(mirrorRoot, manifest)
	logRemotePhase(hs.Host, "compare", compareStarted)
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
	// Capture the old index's sessions before replacement or deletion loses
	// their association. Journal transcript paths so replay uses the current
	// provider metadata, including titles from remaining homes.
	indexChanges := append([]string(nil), delta.Deletions...)
	for _, remotePath := range delta.Fetch {
		localPath, err := safeRemappedRemotePath(mirrorRoot, remotePath)
		if err != nil {
			return nil, err
		}
		indexChanges = append(indexChanges, localPath)
	}
	for _, indexPath := range indexChanges {
		if filepath.Base(indexPath) != parser.CodexSessionIndexFilename {
			continue
		}
		parser.EvictCodexSessionIndex(indexPath)
		for uuid := range parser.CodexSessionIndexTitles(indexPath) {
			threadID := hs.Host + "~codex:" + uuid
			// Revert pages (codex:<thread>_<rollout>) carry the thread's title.
			pageIDs, err := hs.DB.CodexRevertPageSessionIDs(ctx, threadID)
			if err != nil {
				return nil, err
			}
			for _, id := range append([]string{threadID}, pageIDs...) {
				storedPath := hs.DB.GetSessionFilePath(ctx, id)
				remotePath, ok := strings.CutPrefix(storedPath, hs.Host+":")
				if !ok {
					continue
				}
				path, err := mirrorRelativeRemoteChangePath(mirrorRoot, remotePath)
				if err != nil {
					return nil, err
				}
				observed = append(observed, path)
			}
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
	scopedProviders := db.ScopedDataVersionProviders(
		importedDataVersion, currentDataVersion,
	)
	if bootstrap || hs.Full || fullReason != "" || journal.FullImport {
		scopedProviders = nil
	}
	dataRebuildPending := importedDataVersion != currentDataVersion ||
		dataRebuildRequested || journal.RequiredDataVersion != 0
	if dataRebuildPending && journal.RequiredDataVersion != currentDataVersion {
		if len(scopedProviders) > 0 ||
			bootstrap && !dataRebuildRequested && !journal.FullImport {
			journal.RequiredDataVersion = currentDataVersion
			journal.DataRebuildCacheVersion = 0
		} else {
			journal = requireMirrorDataVersionImport(journal, currentDataVersion)
		}
	}
	if journal.RequiredDataVersion != 0 && fullReason == "" && len(scopedProviders) == 0 {
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
	hasPending := journal.FullImport || journal.RequiredDataVersion != 0 ||
		len(journal.Entries) > 0 || fullReason != "" || hs.Full
	pendingTitles, err := prepared.antigravityTitlesPending()
	if err != nil {
		return nil, err
	}
	if !mutatesMirror && !hasPending && !pendingTitles {
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
		full := len(delta.Fetch) == delta.Total
		err := hs.downloadIntoMirror(
			ctx, targets.dirScoped, delta.Fetch, full, mirrorRoot,
		)
		if err != nil && !full && isRemoteRefusal(err) {
			err = hs.downloadIntoMirror(
				ctx, targets.dirScoped, delta.Fetch, true, mirrorRoot,
			)
		}
		if err != nil {
			return nil, err
		}
	}
	if refreshFileScoped {
		if err := hs.downloadIntoMirror(
			ctx, targets.fileScoped, nil, true, mirrorRoot,
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
		completeSourceMirror:    true,
		BlockedResultCategories: hs.BlockedResultCategories,
		Progress:                hs.Progress, Targets: prepared.targets, Root: mirrorRoot,
		replaceRemoteSkippedFiles: prepared.replaceRemoteSkippedFiles,
		applyRemoteSkippedChanges: prepared.applyRemoteSkippedChanges,
	}).PreparePending(ctx, DeltaImportRequest{
		Journal: journal, FullReason: fullReason,
		ScopedProviders:          scopedProviders,
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

type antigravityTitleVerification struct {
	Version int                                    `json:"version"`
	States  map[string]parser.SQLiteContainerState `json:"states"`
}

func loadAntigravityTitleVerification(root string) antigravityTitleVerification {
	data, err := os.ReadFile(mirrorAntigravityTitlesPath(root))
	if err != nil {
		return antigravityTitleVerification{}
	}
	var applied antigravityTitleVerification
	if json.Unmarshal(data, &applied) != nil || applied.Version != 1 {
		return antigravityTitleVerification{}
	}
	return applied
}

func (p *PreparedRemote) antigravityTitlesPending() (bool, error) {
	if len(p.targets.Dirs[parser.AgentAntigravity]) == 0 || p.sync.DB.ArchiveContent().UsageOnly() {
		return false, nil
	}
	current := make(map[string]parser.SQLiteContainerState)
	for _, remoteRoot := range p.targets.Dirs[parser.AgentAntigravity] {
		root, err := safeRemappedRemotePath(p.root, remoteRoot)
		if err != nil {
			return false, err
		}
		store := filepath.Join(root, parser.AntigravitySummariesDBName)
		if state, ok := parser.StatSQLiteContainerState(store); ok {
			current[store] = state
		}
	}
	applied := loadAntigravityTitleVerification(p.root)
	if applied.Version != 1 {
		return true, nil
	}
	return !maps.Equal(current, applied.States), nil
}

func (p *PreparedRemote) persistAntigravityTitleStates() error {
	if len(p.targets.Dirs[parser.AgentAntigravity]) == 0 || p.sync.DB.ArchiveContent().UsageOnly() {
		return nil
	}
	if data, err := os.ReadFile(mirrorAntigravityTitlesPath(p.root)); err == nil {
		var applied struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(data, &applied) == nil && applied.Version > 1 {
			return nil
		}
	}
	data, err := json.Marshal(antigravityTitleVerification{Version: 1, States: p.mirrorImport.pending.antigravityTitleStates})
	if err != nil {
		return err
	}
	if err := atomicfile.WriteFile(mirrorAntigravityTitlesPath(p.root), data, atomicfile.WithPrivate()); err != nil {
		return fmt.Errorf("persist applied Antigravity title states: %w", err)
	}
	return nil
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

func legacySortKey(hs RemoteSync) string {
	key := "legacy\x00" + hs.Host
	if source, ok := hs.Source.(HTTPSource); ok {
		key += "\x00" + source.URL
	}
	return key
}
