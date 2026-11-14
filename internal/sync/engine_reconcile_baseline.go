package sync

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

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
	if agent == "" || path == "" {
		return
	}
	tracker.nonAuthoritativeScopes[reconciliationSourceScope{
		Provider: agent,
		Path:     path,
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
