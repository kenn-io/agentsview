// Package rawwatch captures provider watcher events and repairs missed events
// with bounded periodic audits, without invoking normalized parsing.
package rawwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcapture"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
)

// AuditResult summarizes bounded work for one provider pass.
type AuditResult struct {
	Visited    int
	Captured   int
	Unchanged  int
	Tombstoned int
	Degraded   int
	Complete   bool
}

// Auditor rotates capture work and repairs sources missed by watcher events.
type Auditor struct {
	store         *rawcheckpoint.Store
	capturer      *rawcapture.Capturer
	maxWork       int
	scans         map[parser.AgentType]*auditDiscoveryScan
	nextKnownRoot map[parser.AgentType]int
	knownAfter    map[string]string
}

// NewAuditor constructs a bounded auditor. maxWork must be positive.
func NewAuditor(
	store *rawcheckpoint.Store,
	capturer *rawcapture.Capturer,
	maxWork int,
) *Auditor {
	if maxWork <= 0 {
		maxWork = 1
	}
	return &Auditor{
		store: store, capturer: capturer, maxWork: maxWork,
		scans:         make(map[parser.AgentType]*auditDiscoveryScan),
		nextKnownRoot: make(map[parser.AgentType]int),
		knownAfter:    make(map[string]string),
	}
}

// AuditProvider reconciles at most maxWork source captures or tombstones.
func (a *Auditor) AuditProvider(
	ctx context.Context,
	provider parser.Provider,
) (AuditResult, error) {
	return a.auditProviderBounded(ctx, provider)
}

// AuditProviderFull reconciles every currently discovered or absent source.
// It is reserved for explicit full-sync markers; periodic audits stay bounded.
func (a *Auditor) AuditProviderFull(
	ctx context.Context,
	provider parser.Provider,
) (AuditResult, error) {
	a.stopDiscoveryScan(provider.Definition().Type)
	return a.auditProviderFull(ctx, provider)
}

type auditDiscoveryEvent struct {
	source   parser.SourceRef
	complete bool
	err      error
	terminal bool
	progress bool
	resume   chan struct{}
}

type auditDiscoveryScan struct {
	events       <-chan auditDiscoveryEvent
	cancel       context.CancelFunc
	resume       chan struct{}
	terminal     *auditDiscoveryEvent
	known        []rawcheckpoint.SourceCheckpoint
	presentKnown map[string]bool
	knownRoot    *parser.WatchRoot
}

func (a *Auditor) auditProviderBounded(
	ctx context.Context,
	provider parser.Provider,
) (AuditResult, error) {
	result := AuditResult{}
	providerType := provider.Definition().Type
	traversalRemaining := a.maxWork
	scan, err := a.discoveryScan(ctx, provider, &traversalRemaining)
	if err != nil {
		return result, err
	}
	if scan.events == nil {
		return result, nil
	}
	if scan.resume != nil {
		close(scan.resume)
		scan.resume = nil
		traversalRemaining--
		if traversalRemaining == 0 {
			return result, nil
		}
	}

	remaining := a.maxWork
	seenPhysical := make(map[string]struct{}, a.maxWork)
	if scan.terminal != nil {
		event := *scan.terminal
		scan.terminal = nil
		return a.finishDiscoveryScan(
			ctx, providerType, scan, event, result, remaining, &traversalRemaining,
		)
	}
	for {
		if traversalRemaining == 0 {
			return result, nil
		}
		var event auditDiscoveryEvent
		var ok bool
		select {
		case <-ctx.Done():
			a.stopDiscoveryScan(providerType)
			return result, ctx.Err()
		case event, ok = <-scan.events:
		}
		if !ok {
			a.stopDiscoveryScan(providerType)
			return result, nil
		}
		if event.terminal {
			return a.finishDiscoveryScan(
				ctx, providerType, scan, event, result, remaining, &traversalRemaining,
			)
		}
		if event.progress {
			traversalRemaining--
			if traversalRemaining == 0 {
				scan.resume = event.resume
				return result, nil
			}
			close(event.resume)
			continue
		}
		identity, supported, err := a.rawCaptureSourceIdentity(ctx, provider, event.source)
		if err != nil {
			a.stopDiscoveryScan(providerType)
			return result, err
		}
		if !supported {
			continue
		}
		key := auditSourceIdentityKey(identity)
		if _, tracked := scan.presentKnown[key]; tracked {
			scan.presentKnown[key] = true
		}
		physicalKey := rawWatchSourceDedupKey(event.source)
		if _, duplicate := seenPhysical[physicalKey]; duplicate {
			continue
		}
		seenPhysical[physicalKey] = struct{}{}
		capture, err := a.capturer.Capture(ctx, provider, event.source)
		if err != nil {
			a.stopDiscoveryScan(providerType)
			return result, err
		}
		remaining--
		result.Visited++
		switch capture.Status {
		case rawcapture.StatusCaptured:
			result.Captured++
		case rawcapture.StatusUnchanged:
			result.Unchanged++
		case rawcapture.StatusDegraded:
			result.Degraded++
		}
		if remaining == 0 {
			return result, nil
		}
	}
}

func (a *Auditor) discoveryScan(
	ctx context.Context,
	provider parser.Provider,
	traversalRemaining *int,
) (*auditDiscoveryScan, error) {
	providerType := provider.Definition().Type
	if scan := a.scans[providerType]; scan != nil {
		if scan.events == nil && *traversalRemaining > 0 {
			a.startDiscoveryScan(ctx, provider, scan)
		}
		return scan, nil
	}
	watchPlan, err := provider.WatchPlan(ctx)
	if err != nil {
		return nil, err
	}
	known, knownRoot, err := a.nextKnownSourcePage(
		ctx, providerType, watchPlan.Roots, traversalRemaining,
	)
	if err != nil {
		return nil, err
	}
	presentKnown := make(map[string]bool, len(known))
	for _, source := range known {
		presentKnown[auditSourceIdentityKey(source.Source)] = false
	}

	scan := &auditDiscoveryScan{
		known: known, presentKnown: presentKnown,
		knownRoot: knownRoot,
	}
	a.scans[providerType] = scan
	if *traversalRemaining > 0 {
		a.startDiscoveryScan(ctx, provider, scan)
	}
	return scan, nil
}

func (a *Auditor) startDiscoveryScan(
	ctx context.Context,
	provider parser.Provider,
	scan *auditDiscoveryScan,
) {
	scanCtx, cancel := context.WithCancel(ctx)
	events := make(chan auditDiscoveryEvent)
	scan.events = events
	scan.cancel = cancel
	go func() {
		defer close(events)
		progressCtx := parser.WithRawCaptureDiscoveryProgress(scanCtx, func() error {
			resume := make(chan struct{})
			select {
			case events <- auditDiscoveryEvent{progress: true, resume: resume}:
			case <-scanCtx.Done():
				return scanCtx.Err()
			}
			select {
			case <-resume:
				return nil
			case <-scanCtx.Done():
				return scanCtx.Err()
			}
		})
		complete, discoveryErr := parser.StreamRawCaptureSources(
			progressCtx, provider, func(source parser.SourceRef) error {
				select {
				case events <- auditDiscoveryEvent{source: source}:
				case <-scanCtx.Done():
					return scanCtx.Err()
				}
				return nil
			},
		)
		select {
		case events <- auditDiscoveryEvent{
			complete: complete, err: discoveryErr, terminal: true,
		}:
		case <-scanCtx.Done():
		}
	}()
}

func (a *Auditor) finishDiscoveryScan(
	ctx context.Context,
	providerType parser.AgentType,
	scan *auditDiscoveryScan,
	event auditDiscoveryEvent,
	result AuditResult,
	remaining int,
	traversalRemaining *int,
) (AuditResult, error) {
	discoveryComplete := event.complete
	if _, ok := errors.AsType[parser.DiscoveryIncompleteError](event.err); ok {
		discoveryComplete = false
	} else if errors.Is(event.err, os.ErrNotExist) {
		discoveryComplete = false
	} else if event.err != nil {
		a.stopDiscoveryScan(providerType)
		return result, event.err
	}
	if discoveryComplete && len(scan.known) != 0 {
		if *traversalRemaining == 0 {
			scan.terminal = &event
			return result, nil
		}
		*traversalRemaining--
		if !rawAuditRootsComplete(ctx, []parser.WatchRoot{*scan.knownRoot}) {
			discoveryComplete = false
		}
	}
	result.Complete = discoveryComplete
	if result.Complete {
		for _, checkpoint := range scan.known {
			if remaining == 0 {
				break
			}
			if scan.presentKnown[auditSourceIdentityKey(checkpoint.Source)] {
				continue
			}
			remaining--
			_, queued, err := a.store.QueueTombstoneIfLatest(
				ctx, checkpoint.Source, checkpoint.CaptureID,
				checkpoint.ObservationRevision,
			)
			if errors.Is(err, rawcheckpoint.ErrOutboxFull) {
				result.Degraded++
				continue
			}
			if err != nil {
				a.stopDiscoveryScan(providerType)
				return result, fmt.Errorf("rawwatch: queue tombstone: %w", err)
			}
			if queued {
				result.Tombstoned++
			}
		}
	}
	a.stopDiscoveryScan(providerType)
	return result, nil
}

func (a *Auditor) stopDiscoveryScan(provider parser.AgentType) {
	if scan := a.scans[provider]; scan != nil {
		if scan.cancel != nil {
			scan.cancel()
		}
		delete(a.scans, provider)
	}
}

func (a *Auditor) rawCaptureSourceIdentity(
	ctx context.Context,
	provider parser.Provider,
	source parser.SourceRef,
) (rawcheckpoint.SourceIdentity, bool, error) {
	plan, supported, err := parser.ResolveRawCapturePlan(ctx, provider, source)
	if err != nil || !supported {
		return rawcheckpoint.SourceIdentity{}, supported, err
	}
	root, err := a.store.ResolveConfiguredRoot(
		ctx, source.Provider, plan.ConfiguredRoot,
	)
	if err != nil {
		return rawcheckpoint.SourceIdentity{}, false, err
	}
	return rawcheckpoint.SourceIdentity{
		Provider: source.Provider, ConfiguredRootID: root.ID,
		SourceKey: plan.SourceKey,
	}, true, nil
}

func (a *Auditor) nextKnownSourcePage(
	ctx context.Context,
	provider parser.AgentType,
	roots []parser.WatchRoot,
	traversalRemaining *int,
) ([]rawcheckpoint.SourceCheckpoint, *parser.WatchRoot, error) {
	if len(roots) == 0 {
		return nil, nil, nil
	}
	start := a.nextKnownRoot[provider] % len(roots)
	for offset := 0; offset < len(roots) && *traversalRemaining > 0; offset++ {
		rootIndex := (start + offset) % len(roots)
		a.nextKnownRoot[provider] = (rootIndex + 1) % len(roots)
		*traversalRemaining--
		if !rawAuditRootsComplete(ctx, []parser.WatchRoot{roots[rootIndex]}) {
			continue
		}
		root, err := a.store.ResolveConfiguredRoot(
			ctx, provider, roots[rootIndex].Path,
		)
		if err != nil {
			return nil, nil, err
		}
		after := a.knownAfter[root.ID]
		sources, err := a.store.ConfiguredRootSourcesPage(
			ctx, root.Provider, root.ID, after, a.maxWork,
		)
		if err != nil {
			return nil, nil, err
		}
		if len(sources) == 0 {
			continue
		}
		a.knownAfter[root.ID] = sources[len(sources)-1].Source.SourceKey
		selectedRoot := roots[rootIndex]
		return sources, &selectedRoot, nil
	}
	return nil, nil, nil
}

func auditSourceIdentityKey(source rawcheckpoint.SourceIdentity) string {
	return string(source.Provider) + "\x00" + source.ConfiguredRootID + "\x00" + source.SourceKey
}

func (a *Auditor) auditProviderFull(
	ctx context.Context,
	provider parser.Provider,
) (AuditResult, error) {
	result := AuditResult{}
	discovery, err := parser.DiscoverRawCaptureSources(ctx, provider)
	if _, ok := errors.AsType[parser.DiscoveryIncompleteError](err); ok {
		discovery.Complete = false
	} else if errors.Is(err, os.ErrNotExist) {
		discovery.Complete = false
	} else if err != nil {
		return result, err
	}
	sources := discovery.Sources
	type discoveredSource struct {
		source parser.SourceRef
		rootID string
	}
	discovered := make([]discoveredSource, 0, len(sources))
	present := make(map[string]map[string]struct{})
	for _, source := range sources {
		plan, supported, err := parser.ResolveRawCapturePlan(ctx, provider, source)
		if err != nil {
			return AuditResult{}, err
		}
		if !supported {
			continue
		}
		root, err := a.store.ResolveConfiguredRoot(ctx, source.Provider, plan.ConfiguredRoot)
		if err != nil {
			return AuditResult{}, err
		}
		if present[root.ID] == nil {
			present[root.ID] = make(map[string]struct{})
		}
		present[root.ID][plan.SourceKey] = struct{}{}
		discovered = append(discovered, discoveredSource{source: source, rootID: root.ID})
	}

	watchPlan, err := provider.WatchPlan(ctx)
	if err != nil {
		return result, err
	}
	discovery.Complete = discovery.Complete && rawAuditRootsComplete(ctx, watchPlan.Roots)
	result.Complete = discovery.Complete
	var absent []rawcheckpoint.SourceIdentity
	reconciledRootIDs := make(map[string]struct{}, len(watchPlan.Roots))
	degradedRootIDs := make(map[string]struct{})
	if discovery.Complete {
		for _, watchRoot := range watchPlan.Roots {
			root, err := a.store.ResolveConfiguredRoot(
				ctx, provider.Definition().Type, watchRoot.Path,
			)
			if err != nil {
				return result, err
			}
			reconciledRootIDs[root.ID] = struct{}{}
			known, err := a.store.ConfiguredRootSources(ctx, root.Provider, root.ID)
			if err != nil {
				return result, err
			}
			for _, source := range known {
				if _, exists := present[root.ID][source.SourceKey]; exists {
					continue
				}
				absent = append(absent, source)
			}
		}
	}
	for _, source := range absent {
		_, queued, err := a.store.QueueTombstone(ctx, source)
		if errors.Is(err, rawcheckpoint.ErrOutboxFull) {
			result.Degraded++
			degradedRootIDs[source.ConfiguredRootID] = struct{}{}
			continue
		}
		if err != nil {
			return result, fmt.Errorf("rawwatch: queue tombstone: %w", err)
		}
		if queued {
			result.Tombstoned++
		}
	}
	for _, item := range discovered {
		capture, err := a.capturer.Capture(ctx, provider, item.source)
		if err != nil {
			return result, err
		}
		result.Visited++
		switch capture.Status {
		case rawcapture.StatusCaptured:
			result.Captured++
		case rawcapture.StatusUnchanged:
			result.Unchanged++
		case rawcapture.StatusDegraded:
			result.Degraded++
			degradedRootIDs[item.rootID] = struct{}{}
		}
	}
	for rootID := range reconciledRootIDs {
		if _, degraded := degradedRootIDs[rootID]; degraded {
			continue
		}
		if err := a.store.CompleteRootReconciliation(ctx, rootID); err != nil {
			return result, fmt.Errorf(
				"rawwatch: complete root reconciliation: %w", err,
			)
		}
	}
	return result, nil
}

func rawAuditRootsComplete(ctx context.Context, roots []parser.WatchRoot) bool {
	for _, root := range roots {
		if ctx.Err() != nil {
			return false
		}
		dir, err := os.Open(root.Path)
		if err != nil {
			return false
		}
		info, statErr := dir.Stat()
		_, readErr := dir.Readdirnames(1)
		closeErr := dir.Close()
		if statErr != nil || !info.IsDir() || closeErr != nil ||
			(readErr != nil && !errors.Is(readErr, io.EOF)) {
			return false
		}
	}
	return true
}
