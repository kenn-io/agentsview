package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// WatchBatchSyncer is the engine surface needed to classify and apply one
// public watcher batch. Engine implements it; narrow recorders can exercise
// planning without opening an archive.
type WatchBatchSyncer interface {
	SyncPathsContext(context.Context, []string) error
	HasActiveSessionSourceBelow(agent, path string) (bool, error)
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
	cause error
	retry WatchBatch
}

func (e *watchBatchApplyError) Error() string { return e.cause.Error() }
func (e *watchBatchApplyError) Unwrap() error { return e.cause }
func (e *watchBatchApplyError) WatchRetryBatch() WatchBatch {
	retry := e.retry
	retry.Paths = append([]string(nil), retry.Paths...)
	retry.ReconcileRoots = append([]string(nil), retry.ReconcileRoots...)
	return retry
}

func watchBatchDeferOnlyError(err error) bool {
	var paths interface{ ReconciliationRetryPaths() []string }
	var deferOnly interface{ ReconciliationRetryDeferOnly() bool }
	return errors.As(err, &paths) && errors.As(err, &deferOnly) &&
		deferOnly.ReconciliationRetryDeferOnly()
}

func watchBatchReconciliationError(
	cause error, paths, roots []string, full, lostEvents bool,
) error {
	var scopedPaths interface{ ReconciliationRetryPaths() []string }
	var retryPaths []string
	hasScopedPaths := errors.As(cause, &scopedPaths)
	if hasScopedPaths {
		retryPaths = watchDeduplicateStrings(scopedPaths.ReconciliationRetryPaths())
		hasScopedPaths = len(retryPaths) > 0
	}
	if !hasScopedPaths {
		retryPaths = watchDeduplicateStrings(paths)
	}
	var scoped interface{ ReconciliationRetryRoots() []string }
	var retryRoots []string
	hasScopedRoots := errors.As(cause, &scoped)
	if hasScopedRoots {
		retryRoots = watchDeduplicateStrings(scoped.ReconciliationRetryRoots())
	}
	if !hasScopedRoots {
		retryRoots = watchDeduplicateStrings(roots)
	}
	var overflow interface{ ReconciliationRetryOverflow() bool }
	overflowed := errors.As(cause, &overflow) && overflow.ReconciliationRetryOverflow()
	if overflowed && len(retryPaths) == 0 && len(retryRoots) == 0 {
		return &watchBatchApplyError{cause: cause, retry: WatchBatch{
			FullSync: true, LostEvents: lostEvents,
		}}
	}
	if len(retryPaths) > 0 || len(retryRoots) > 0 {
		return &watchBatchApplyError{cause: cause, retry: WatchBatch{
			Paths: retryPaths, ReconcileRoots: retryRoots, LostEvents: lostEvents,
		}}
	}
	if full {
		return &watchBatchApplyError{cause: cause, retry: WatchBatch{
			FullSync: true, LostEvents: lostEvents,
		}}
	}
	retry := WatchBatch{FullSync: full, LostEvents: lostEvents}
	if !full && !hasScopedRoots {
		retry.ReconcileRoots = append([]string(nil), roots...)
	}
	return &watchBatchApplyError{cause: cause, retry: retry}
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
	causes := make([]error, 0, len(present))
	for _, phase := range present {
		causes = append(causes, phase)
		var retry interface{ WatchRetryBatch() WatchBatch }
		if !errors.As(phase, &retry) {
			continue
		}
		batch := retry.WatchRetryBatch()
		combined.Paths = append(combined.Paths, batch.Paths...)
		combined.ReconcileRoots = append(combined.ReconcileRoots, batch.ReconcileRoots...)
		combined.FullSync = combined.FullSync || batch.FullSync
		combined.LostEvents = combined.LostEvents || batch.LostEvents
	}
	if combined.FullSync {
		combined.Paths = nil
		combined.ReconcileRoots = nil
	} else {
		combined.Paths = watchDeduplicateStrings(combined.Paths)
		combined.ReconcileRoots = watchDeduplicateStrings(combined.ReconcileRoots)
	}
	return &watchBatchApplyError{cause: errors.Join(causes...), retry: combined}
}

func planWatchBatch(
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
			hasDescendant, err := engine.HasActiveSessionSourceBelow(
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
	plan, err := planWatchBatch(engine, batch, recovery, os.Stat)
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
	e.syncMu.Lock()
	defer func() {
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
	plan, err := planWatchBatch(e, batch, recovery, statPath)
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
		reconcileStats, tombstoned, _, reconcileErr :=
			e.reconcileScopedWatchRootsLocked(
				ctx, "", reconcileRoots, false, plan.lostEvents, nil,
			)
		mergeSyncStats(&stats, reconcileStats)
		changed = changed || reconcileStats.hasSessionChanges() || tombstoned > 0
		if reconcileErr != nil {
			reconcilePhaseErr = watchBatchReconciliationError(
				reconcileErr, nil, reconcileRoots, plan.full, plan.lostEvents,
			)
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
	e.signalSched.flushAllInline()
	e.clearCurrentProgress()
	if work != nil {
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
