package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// WatchBatchSyncer is the engine surface needed to classify and apply one
// public watcher batch. Engine implements it; narrow recorders can exercise
// planning without opening an archive.
type WatchBatchSyncer interface {
	SyncPathsContext(context.Context, []string) error
	HasActiveSessionSourceBelow(ctx context.Context, agent, path string) (bool, error)
	ReconciliationRootsForAgent(agent string) []string
	ReconcileWatchRoots(context.Context, []string, bool) error
	ReconcileWatchRootsAfterLostEvents(context.Context, []string, bool) error
}

type ownedWatchBatchSyncer interface {
	SyncWatchBatchThenRun(
		context.Context, WatchBatch, *WatchRecoveryScope, func() error,
	) (SyncStats, error)
}

// ValidateWatchBatch rejects malformed or ambiguous public watcher scope
// before any archive work begins.
func ValidateWatchBatch(batch WatchBatch, recovery *WatchRecoveryScope) error {
	if len(batch.Paths) == 0 && len(batch.Renames) == 0 &&
		len(batch.ReconcileRoots) == 0 && !batch.FullSync {
		return errors.New("watch batch contains no work")
	}
	for _, path := range batch.Paths {
		if strings.TrimSpace(path) == "" {
			return errors.New("watch batch contains a blank path")
		}
	}
	for _, root := range batch.ReconcileRoots {
		if strings.TrimSpace(root) == "" {
			return errors.New("watch batch contains a blank reconciliation root")
		}
	}
	for _, rename := range batch.Renames {
		if strings.TrimSpace(rename.Path) == "" {
			return errors.New("watch batch contains a rename with a blank path")
		}
		if rename.ItemType > ItemIsDir {
			return fmt.Errorf("watch batch contains invalid rename item type %d", rename.ItemType)
		}
	}
	if batch.FullSync && (len(batch.Paths) > 0 || len(batch.Renames) > 0 ||
		len(batch.ReconcileRoots) > 0) {
		return errors.New("full watch batch cannot retain fine-grained work")
	}
	if (batch.FullSync || len(batch.Renames) > 0) && recovery == nil {
		return errors.New("authoritative watch batch requires recovery scope")
	}
	if recovery == nil {
		return nil
	}
	for _, root := range append(
		append([]string(nil), recovery.AvailableRoots...),
		recovery.DeferredRoots...,
	) {
		if strings.TrimSpace(root) == "" {
			return errors.New("watch recovery contains a blank root")
		}
	}
	for _, available := range recovery.AvailableRoots {
		for _, deferred := range recovery.DeferredRoots {
			if samePathOrDescendant(available, deferred) ||
				samePathOrDescendant(deferred, available) {
				return fmt.Errorf(
					"watch recovery available and deferred roots overlap: %q and %q",
					available, deferred,
				)
			}
		}
	}
	return nil
}

type watchBatchPlan struct {
	paths          []string
	reconcileRoots []string
	full           bool
	lostEvents     bool
}

type watchBatchApplyError struct {
	cause     error
	retry     WatchBatch
	deferOnly bool
}

func (e *watchBatchApplyError) Error() string                      { return e.cause.Error() }
func (e *watchBatchApplyError) Unwrap() error                      { return e.cause }
func (e *watchBatchApplyError) ReconciliationRetryDeferOnly() bool { return e.deferOnly }

func (e *watchBatchApplyError) WatchRetryBatch() WatchBatch {
	retry := e.retry
	retry.Paths = append([]string(nil), retry.Paths...)
	retry.ReconcileRoots = append([]string(nil), retry.ReconcileRoots...)
	return retry
}

func watchBatchDeferOnlyError(err error) bool {
	if err == nil {
		return false
	}
	pending := []error{err}
	matched := false
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if next == nil {
			continue
		}
		if _, isApply := next.(interface{ WatchRetryBatch() WatchBatch }); isApply {
			deferOnly, ok := next.(interface{ ReconciliationRetryDeferOnly() bool })
			if !ok || !deferOnly.ReconciliationRetryDeferOnly() {
				return false
			}
			matched = true
			continue
		}
		if joined, ok := next.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			pending = append(pending, children...)
			continue
		}
		if _, hasPaths := next.(interface{ ReconciliationRetryPaths() []string }); hasPaths {
			deferOnly, ok := next.(interface{ ReconciliationRetryDeferOnly() bool })
			if !ok || !deferOnly.ReconciliationRetryDeferOnly() {
				return false
			}
			matched = true
			continue
		}
		if wrapped, ok := next.(interface{ Unwrap() error }); ok {
			cause := wrapped.Unwrap()
			if cause == nil {
				return false
			}
			pending = append(pending, cause)
			continue
		}
		return false
	}
	return matched
}

func watchBatchReconciliationError(
	cause error, paths, roots []string, full, lostEvents bool,
) error {
	deferOnly := watchBatchDeferOnlyError(cause)
	// When discovery and reconciliation finished cleanly and only deferred
	// individual sources remain, the lost-events recovery pass is complete.
	// Keeping LostEvents=true on a defer-only retry would re-enter forced
	// overflow recovery and clear the caches that let deferred forks settle.
	retryLostEvents := lostEvents && !deferOnly
	scopedPaths, hasScopedPaths, scopedRoots, hasScopedRoots := reconciliationRetryScope(cause)
	var retryPaths []string
	if hasScopedPaths {
		retryPaths = watchDeduplicateStrings(scopedPaths)
		hasScopedPaths = len(retryPaths) > 0
	}
	if !hasScopedPaths {
		retryPaths = watchDeduplicateStrings(paths)
	}
	var retryRoots []string
	if hasScopedRoots {
		retryRoots = watchDeduplicateStrings(scopedRoots)
	} else {
		retryRoots = watchDeduplicateStrings(roots)
	}
	overflow, hasOverflow := errors.AsType[interface {
		error
		interface{ ReconciliationRetryOverflow() bool }
	}](cause)
	overflowed := hasOverflow && overflow.ReconciliationRetryOverflow()
	if overflowed && len(retryPaths) == 0 && len(retryRoots) == 0 {
		return &watchBatchApplyError{cause: cause, retry: WatchBatch{
			FullSync: true, LostEvents: retryLostEvents,
		}, deferOnly: deferOnly}
	}
	if len(retryPaths) > 0 || len(retryRoots) > 0 {
		return &watchBatchApplyError{cause: cause, retry: WatchBatch{
			Paths: retryPaths, ReconcileRoots: retryRoots, LostEvents: retryLostEvents,
		}, deferOnly: deferOnly}
	}
	if full {
		return &watchBatchApplyError{cause: cause, retry: WatchBatch{
			FullSync: true, LostEvents: retryLostEvents,
		}, deferOnly: deferOnly}
	}
	retry := WatchBatch{FullSync: full, LostEvents: retryLostEvents}
	if !full && !hasScopedRoots {
		retry.ReconcileRoots = append([]string(nil), roots...)
	}
	return &watchBatchApplyError{cause: cause, retry: retry, deferOnly: deferOnly}
}

// reconciliationRetryScope gathers retry paths and roots from every scoped
// reconciliation error in err's tree. A scoped reconciliation can join its own
// error with the one from waking forks, and each carries its own sources.
func reconciliationRetryScope(err error) (
	paths []string, hasPaths bool, roots []string, hasRoots bool,
) {
	pending := []error{err}
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if next == nil {
			continue
		}
		scopedPaths, isPaths := next.(interface{ ReconciliationRetryPaths() []string })
		scopedRoots, isRoots := next.(interface{ ReconciliationRetryRoots() []string })
		if isPaths {
			paths = append(paths, scopedPaths.ReconciliationRetryPaths()...)
			hasPaths = true
		}
		if isRoots {
			roots = append(roots, scopedRoots.ReconciliationRetryRoots()...)
			hasRoots = true
		}
		if isPaths || isRoots {
			continue
		}
		{
			var wrapped interface{ Unwrap() []error }
			var wrapped1 interface{ Unwrap() error }
			switch {
			case errors.As(next, &wrapped):
				pending = append(pending, wrapped.Unwrap()...)
			case errors.As(next, &wrapped1):
				pending = append(pending, wrapped1.Unwrap())
			}
		}
	}
	return paths, hasPaths, roots, hasRoots
}

func composeWatchBatchErrors(phases ...error) error {
	var present []error
	for _, phase := range phases {
		if phase != nil {
			present = append(present, phase)
		}
	}
	if len(present) == 0 {
		return nil
	}
	if len(present) == 1 {
		return present[0]
	}
	combined := WatchBatch{}
	var preparationSet bool
	allDeferOnly := true
	causes := make([]error, 0, len(present))
	for _, phase := range present {
		causes = append(causes, phase)
		if !watchBatchDeferOnlyError(phase) {
			allDeferOnly = false
		}
		retry, hasRetry := errors.AsType[interface {
			error
			interface{ WatchRetryBatch() WatchBatch }
		}](phase)
		if !hasRetry {
			continue
		}
		batch := retry.WatchRetryBatch()
		if !preparationSet {
			combined.preparedRecovery = batch.preparedRecovery
			preparationSet = true
		} else if combined.preparedRecovery != batch.preparedRecovery {
			combined.preparedRecovery = nil
		}
		combined.Paths = append(combined.Paths, batch.Paths...)
		combined.ReconcileRoots = append(combined.ReconcileRoots, batch.ReconcileRoots...)
		combined.FullSync = combined.FullSync || batch.FullSync
		combined.LostEvents = combined.LostEvents || batch.LostEvents
	}
	if combined.FullSync {
		combined.preparedRecovery = nil
		combined.Paths = nil
		combined.ReconcileRoots = nil
	} else {
		combined.Paths = watchDeduplicateStrings(combined.Paths)
		combined.ReconcileRoots = watchDeduplicateStrings(combined.ReconcileRoots)
	}
	return &watchBatchApplyError{
		cause:     errors.Join(causes...),
		retry:     combined,
		deferOnly: allDeferOnly,
	}
}

func planWatchBatch(ctx context.Context,
	engine WatchBatchSyncer,
	batch WatchBatch,
	recovery *WatchRecoveryScope,
	statPath func(string) (os.FileInfo, error),
) (watchBatchPlan, error) {
	plan := watchBatchPlan{
		paths:          append([]string(nil), batch.Paths...),
		full:           batch.FullSync,
		reconcileRoots: append([]string(nil), batch.ReconcileRoots...),
		lostEvents:     batch.LostEvents,
	}
	type renameOwner struct {
		path  string
		agent string
	}
	authoritativePaths := make(map[string]struct{})
	authoritativeRenames := make(map[renameOwner]struct{})
	promoteDirectoryRename := func(rename WatchRename) {
		roots := engine.ReconciliationRootsForAgent(rename.Agent)
		if rename.Agent == "" || len(roots) == 0 {
			plan.full = true
			return
		}
		for _, root := range roots {
			if watchRecoveryCoversProviderRoot(recovery, root) {
				plan.reconcileRoots = append(plan.reconcileRoots, root)
			}
		}
	}
	for _, rename := range batch.Renames {
		owner := renameOwner{path: rename.Path, agent: rename.Agent}
		if _, authoritative := authoritativeRenames[owner]; authoritative {
			continue
		}
		switch rename.ItemType {
		case ItemIsFile:
			plan.paths = watchAppendUniqueString(plan.paths, rename.Path)
		case ItemIsDir:
			promoteDirectoryRename(rename)
			authoritativePaths[rename.Path] = struct{}{}
			authoritativeRenames[owner] = struct{}{}
			plan.paths = watchRemoveString(plan.paths, rename.Path)
		case ItemIsUnknown:
			info, err := statPath(rename.Path)
			if err == nil {
				if info.IsDir() {
					promoteDirectoryRename(rename)
					authoritativePaths[rename.Path] = struct{}{}
					authoritativeRenames[owner] = struct{}{}
					plan.paths = watchRemoveString(plan.paths, rename.Path)
				} else {
					plan.paths = watchAppendUniqueString(plan.paths, rename.Path)
				}
				continue
			}
			if !errors.Is(err, os.ErrNotExist) {
				return watchBatchPlan{}, fmt.Errorf(
					"classifying watcher rename %q: %w", rename.Path, err,
				)
			}
			hasDescendant, err := engine.HasActiveSessionSourceBelow(ctx,
				rename.Agent, rename.Path,
			)
			if err != nil {
				return watchBatchPlan{}, err
			}
			if hasDescendant {
				promoteDirectoryRename(rename)
				authoritativePaths[rename.Path] = struct{}{}
				authoritativeRenames[owner] = struct{}{}
				plan.paths = watchRemoveString(plan.paths, rename.Path)
			} else if _, authoritative := authoritativePaths[rename.Path]; !authoritative {
				plan.paths = watchAppendUniqueString(plan.paths, rename.Path)
			}
		}
	}
	plan.reconcileRoots = watchDeduplicateStrings(plan.reconcileRoots)
	return plan, nil
}

// ApplyWatchBatch classifies and applies one validated watcher batch.
func ApplyWatchBatch(
	ctx context.Context,
	engine WatchBatchSyncer,
	batch WatchBatch,
	recovery *WatchRecoveryScope,
) error {
	if err := ValidateWatchBatch(batch, recovery); err != nil {
		return err
	}
	if owned, ok := engine.(ownedWatchBatchSyncer); ok {
		_, err := owned.SyncWatchBatchThenRun(ctx, batch, recovery, nil)
		return err
	}
	plan, err := planWatchBatch(ctx, engine, batch, recovery, os.Stat)
	if err != nil {
		return err
	}
	if len(plan.paths) > 0 {
		if err := engine.SyncPathsContext(ctx, plan.paths); err != nil {
			if !watchBatchDeferOnlyError(err) {
				retry := WatchBatch{FullSync: plan.full, LostEvents: plan.lostEvents}
				if !plan.full {
					retry.Paths = append([]string(nil), plan.paths...)
					retry.ReconcileRoots = append(
						[]string(nil), plan.reconcileRoots...,
					)
				}
				return &watchBatchApplyError{cause: err, retry: retry}
			}
			pathErr := watchBatchReconciliationError(
				err, plan.paths, nil, plan.full, plan.lostEvents,
			)
			if plan.full {
				plan.reconcileRoots = watchRecoveryAvailableRoots(recovery)
			}
			var rootErr error
			if len(plan.reconcileRoots) > 0 {
				if plan.lostEvents {
					rootErr = engine.ReconcileWatchRootsAfterLostEvents(ctx, plan.reconcileRoots, false)
				} else {
					rootErr = engine.ReconcileWatchRoots(ctx, plan.reconcileRoots, false)
				}
				if rootErr != nil {
					rootErr = watchBatchReconciliationError(rootErr, nil, plan.reconcileRoots, false, plan.lostEvents)
				}
			}
			return composeWatchBatchErrors(pathErr, rootErr)
		}
	}
	if plan.full {
		reconcileRoots := watchRecoveryAvailableRoots(recovery)
		if len(reconcileRoots) == 0 {
			return nil
		}
		if plan.lostEvents {
			err = engine.ReconcileWatchRootsAfterLostEvents(
				ctx, reconcileRoots, false,
			)
		} else {
			err = engine.ReconcileWatchRoots(ctx, reconcileRoots, false)
		}
		if err != nil {
			return watchBatchReconciliationError(err, nil, nil, true, plan.lostEvents)
		}
		return nil
	}
	if len(plan.reconcileRoots) == 0 {
		return nil
	}
	if plan.lostEvents {
		err = engine.ReconcileWatchRootsAfterLostEvents(
			ctx, plan.reconcileRoots, false,
		)
	} else {
		err = engine.ReconcileWatchRoots(ctx, plan.reconcileRoots, false)
	}
	if err != nil {
		return watchBatchReconciliationError(
			err, nil, plan.reconcileRoots, false, plan.lostEvents,
		)
	}
	return nil
}

// SyncWatchBatchThenRun applies one bounded watcher batch and invokes work
// while holding the same engine lock. A push therefore observes the applied
// archive state without allowing another sync to enter between those steps.
func (e *Engine) SyncWatchBatchThenRun(
	ctx context.Context,
	batch WatchBatch,
	recovery *WatchRecoveryScope,
	work func() error,
) (stats SyncStats, err error) {
	if e.refuseWriteInForceParse("SyncWatchBatchThenRun") {
		return SyncStats{}, nil
	}
	if err := ValidateWatchBatch(batch, recovery); err != nil {
		return SyncStats{}, err
	}
	changed := false
	preparation := &recoveryCachePreparation{prepared: batch.preparedRecovery == e && !batch.FullSync && len(batch.Renames) == 0}
	ctx = context.WithValue(ctx, recoveryCachePreparationKey{}, preparation)
	var titleRetry WatchBatch
	ctx = context.WithValue(ctx, nativeTitleRetryContextKey{}, &titleRetry)
	e.syncMu.Lock()
	defer func() {
		if len(titleRetry.Paths) > 0 {
			err = composeWatchBatchErrors(err, &watchBatchApplyError{cause: errors.New("native title reconciliation failed"), retry: titleRetry})
		}
		e.clearCurrentProgress()
		e.syncMu.Unlock()
		if changed {
			e.emit("sessions")
		}
	}()
	e.reportProgress(nil, Progress{
		Phase:  PhaseDiscovering,
		Detail: "Planning watcher batch",
	})
	statPath := e.stat
	if statPath == nil {
		statPath = os.Stat
	}
	plan, err := planWatchBatch(ctx, e, batch, recovery, statPath)
	if err != nil {
		return SyncStats{}, err
	}
	var pathPhaseErr error

	if len(plan.paths) > 0 {
		pathStats, tombstoned, pathErr := e.syncChangedPathsLocked(ctx, plan.paths)
		mergeSyncStats(&stats, pathStats)
		changed = changed || pathStats.hasSessionChanges() || tombstoned > 0
		if pathErr != nil {
			if !watchBatchDeferOnlyError(pathErr) {
				retry := WatchBatch{FullSync: plan.full, LostEvents: plan.lostEvents}
				if !plan.full {
					retry.Paths = append([]string(nil), plan.paths...)
					retry.ReconcileRoots = append(
						[]string(nil), plan.reconcileRoots...,
					)
				}
				return stats, &watchBatchApplyError{cause: pathErr, retry: retry}
			}
			pathPhaseErr = watchBatchReconciliationError(
				pathErr, plan.paths, nil, plan.full, plan.lostEvents,
			)
		}
	}

	reconcileRoots := plan.reconcileRoots
	if plan.full {
		reconcileRoots = watchRecoveryAvailableRoots(recovery)
	}
	var reconcilePhaseErr error
	if len(reconcileRoots) > 0 {
		reconcileStats, tombstoned, _, reconcileErr := e.reconcileScopedWatchRootsLocked(
			ctx, "", reconcileRoots, false, plan.lostEvents, nil,
		)
		mergeSyncStats(&stats, reconcileStats)
		changed = changed || reconcileStats.hasSessionChanges() || tombstoned > 0
		if reconcileErr != nil {
			reconcilePhaseErr = watchBatchReconciliationError(
				reconcileErr, nil, reconcileRoots, plan.full, plan.lostEvents,
			)
			_, localised := parser.IncompleteDiscoveryScopes(reconcileErr)
			if preparation.prepared && localised && reconcileStats.Failed == 0 &&
				reconcileStats.Deferred == 0 && !reconcileStats.Aborted {
				if retry, ok := errors.AsType[*watchBatchApplyError](reconcilePhaseErr); ok && !retry.retry.FullSync {
					retry.retry.preparedRecovery = e
				}
			}
		}
	}
	if err := composeWatchBatchErrors(pathPhaseErr, reconcilePhaseErr); err != nil {
		return stats, err
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	if stats.Deferred > 0 {
		return stats, nil
	}
	e.clearCurrentProgress()
	if work != nil {
		// work typically reads or pushes SQLite rows, so settle deferred
		// signal recomputes first. Without work, a flush would only undo
		// the debounce: a live watcher batch arrives every few seconds.
		e.signalSched.flushAllInline(ctx)
		if err := work(); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// watchRecoveryAvailableRoots keeps the recovery dereference local to the
// nil check. ValidateWatchBatch already requires a recovery scope whenever a
// plan can become full, but callers and static analyzers need not reproduce
// that cross-function implication.
func watchRecoveryAvailableRoots(recovery *WatchRecoveryScope) []string {
	if recovery == nil {
		return nil
	}
	return recovery.AvailableRoots
}

func watchRecoveryCoversProviderRoot(
	recovery *WatchRecoveryScope, root string,
) bool {
	if recovery == nil {
		return false
	}
	for _, deferred := range recovery.DeferredRoots {
		if samePathOrDescendant(root, deferred) ||
			samePathOrDescendant(deferred, root) {
			return false
		}
	}
	for _, available := range recovery.AvailableRoots {
		if samePathOrDescendant(available, root) {
			return true
		}
	}
	return false
}

func watchRemoveString(values []string, remove string) []string {
	return slices.DeleteFunc(values, func(value string) bool { return value == remove })
}

func watchDeduplicateStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func watchAppendUniqueString(values []string, value string) []string {
	if slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
