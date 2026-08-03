package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

var errUnwatchedPollStopped = errors.New("unwatched poll coordinator stopped")

type unwatchedPollSyncer interface {
	// ReconcileProviderRootsGrouped runs one bounded pass per provider group
	// while sharing a single archive-sized epilogue across the batch; the
	// coordinator issues exactly one grouped call per poll pass.
	ReconcileProviderRootsGrouped(context.Context, []agentsync.ProviderRootsGroup) error
}

type unwatchedPollAdd struct {
	obligation pollingObligation
	remove     bool
	done       chan struct{}
}

// pollingScope identifies one configured provider root within a polling obligation.
type pollingScope struct {
	Agent parser.AgentType
	Root  string
}

type pollingObligation struct {
	Key    string
	Scopes []pollingScope
	// Probe mirrors sync.PollingObligation.Probe: the physical watcher path
	// whose availability gates this obligation's reconciliation scopes. When
	// it is missing, the scopes are deferred rather than reconciled
	// authoritatively — a nested physical root (Gemini's <root>/tmp) can
	// vanish while its configured scope <root> still exists, and reconciling
	// the scope then would tombstone every session under the missing
	// subtree. Empty means the Scopes' roots themselves are probed.
	Probe string
}

type boundedBindingMode uint8

const (
	boundedModeAdmitted boundedBindingMode = iota
	boundedModeNative
	boundedModePolling
	boundedModeAudit
	boundedModeRetired
)

type boundedWake uint8

const (
	boundedWakeNone boundedWake = iota
	boundedWakePending
)

type boundedCoverageState struct {
	lease          *agentsync.BoundedCoverageLease
	binding        agentsync.BoundedCoverageBinding
	checkpoint     parser.OpenCodeCoverageCheckpoint
	nativeAdmitted bool
	pollOwned      bool
	dbFile         os.FileInfo
	generation     uint64
	pendingWake    bool
	running        bool
	retry          bool
	auditPending   bool
	auditBoundary  parser.OpenCodeCoverageCheckpoint
	mode           boundedBindingMode
	wake           boundedWake
	frozen         bool
}

func sameBoundedFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return os.SameFile(a, b)
}

type sharedUnwatchedPollCoordinator struct {
	ctx               context.Context
	workerCtx         context.Context
	workerCancel      context.CancelFunc
	engine            unwatchedPollSyncer
	coverage          agentsync.BoundedCoverageResolver
	coverageMu        sync.Mutex
	coverageState     map[string]*boundedCoverageState
	coverageEpoch     map[string]uint64
	requestAudit      func(context.Context, agentsync.BoundedCoverageBinding, string) error
	requestLeaseAudit func(context.Context, *agentsync.BoundedCoverageLease, string) error
	ticks             <-chan time.Time
	stopTicker        func()
	doWork            func(func())
	// onRootsOwned is a test observer invoked after installation and before ack.
	onRootsOwned func([]string)
	// onBoundedCoveragePage is a test observer for bounded work counters.
	onBoundedCoveragePage func(parser.OpenCodeFeedResult)
	// onBoundedCoverageApply is a test observer for committed source writes.
	onBoundedCoverageApply func(agentsync.SyncStats)
	now                    func() time.Time
	after                  func(time.Duration) <-chan time.Time
	add                    chan unwatchedPollAdd
	// pollWake coalesces ticks and explicit wakes while the serialized worker runs.
	pollWake            chan struct{}
	pollDone            chan struct{}
	coveragePassDone    chan struct{}
	coveragePassRunning bool
	pollMu              sync.Mutex
	// pollObligations is the latest complete snapshot owned by the
	// coordinator loop; each entry keeps its probe so availability is
	// evaluated per obligation at poll time.
	pollObligations []pollingObligation
	ownedBindings   map[string]map[string]struct{}
	// lastCompletion is the wall-clock time the most recent pass completed.
	// Zero means no prior pass; a zero value skips the cooldown on the first wake.
	lastCompletion time.Time
	stop           chan struct{}
	done           chan struct{}
	stopOnce       sync.Once
}

func newUnwatchedPollCoordinator(
	ctx context.Context,
	engine unwatchedPollSyncer,
	idleTracker *server.IdleTracker,
) *sharedUnwatchedPollCoordinator {
	ticker := time.NewTicker(unwatchedPollInterval)
	return newUnwatchedPollCoordinatorWithTicks(
		ctx, engine, ticker.C, ticker.Stop, idleTracker.Do, nil, time.Now, time.After,
	)
}

func newUnwatchedPollCoordinatorWithTicks(
	ctx context.Context,
	engine unwatchedPollSyncer,
	ticks <-chan time.Time,
	stopTicker func(),
	doWork func(func()),
	onRootsOwned func([]string),
	now func() time.Time,
	after func(time.Duration) <-chan time.Time,
) *sharedUnwatchedPollCoordinator {
	workerCtx, workerCancel := context.WithCancel(ctx)
	coordinator := &sharedUnwatchedPollCoordinator{
		ctx:           ctx,
		workerCtx:     workerCtx,
		workerCancel:  workerCancel,
		engine:        engine,
		ticks:         ticks,
		stopTicker:    stopTicker,
		doWork:        doWork,
		now:           now,
		after:         after,
		add:           make(chan unwatchedPollAdd),
		pollWake:      make(chan struct{}, 1),
		pollDone:      make(chan struct{}),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		onRootsOwned:  onRootsOwned,
		coverageState: make(map[string]*boundedCoverageState),
		ownedBindings: make(map[string]map[string]struct{}),
	}
	if resolver, ok := engine.(agentsync.BoundedCoverageResolver); ok {
		coordinator.coverage = resolver
	}
	go coordinator.run()
	return coordinator
}

func (c *sharedUnwatchedPollCoordinator) SetBoundedCoverageAuditRequester(
	request func(context.Context, agentsync.BoundedCoverageBinding, string) error,
) {
	c.coverageMu.Lock()
	c.requestAudit = request
	c.coverageMu.Unlock()
}

func (c *sharedUnwatchedPollCoordinator) SetBoundedCoverageLeaseAuditRequester(
	request func(context.Context, *agentsync.BoundedCoverageLease, string) error,
) {
	c.coverageMu.Lock()
	c.requestLeaseAudit = request
	c.coverageMu.Unlock()
}

func (c *sharedUnwatchedPollCoordinator) DisableBoundedCoverage() {
	c.coverageMu.Lock()
	c.coverage = nil
	c.coverageState = make(map[string]*boundedCoverageState)
	c.coverageMu.Unlock()
}

func (c *sharedUnwatchedPollCoordinator) WakeBoundedCoverage(
	bindings []agentsync.BoundedCoverageBinding,
) {
	if len(bindings) == 0 {
		return
	}
	c.coverageMu.Lock()
	for _, binding := range bindings {
		c.invalidateBoundedCoverageReplacementsLocked(binding)
		state := c.coverageState[binding.Key]
		current, statErr := os.Stat(binding.DBPath)
		if state != nil && ((statErr == nil && state.dbFile != nil && !sameBoundedFile(state.dbFile, current)) ||
			(statErr != nil && errors.Is(statErr, os.ErrNotExist))) {
			state.mode = boundedModeRetired
			delete(c.coverageState, binding.Key)
			state = nil
		}
		if state == nil {
			state = &boundedCoverageState{binding: binding}
			state.generation = c.nextCoverageGenerationLocked(boundedCoverageGenerationKey(binding))
			c.coverageState[binding.Key] = state
		}
		state.binding = binding
		if state.dbFile == nil {
			state.dbFile, _ = os.Stat(binding.DBPath)
		}
		state.mode = boundedModeNative
		state.nativeAdmitted = true
		state.wake = boundedWakePending
		state.pendingWake = true
	}
	c.coverageMu.Unlock()
	c.requestPoll()
}

// AdmitBoundedCoverage installs a complete physical lease before the caller
// diverts ordinary ownership. New leases start at row zero; an existing lease
// keeps its committed checkpoint and generation.
func (c *sharedUnwatchedPollCoordinator) AdmitBoundedCoverage(
	ctx context.Context, bindings []agentsync.BoundedCoverageBinding, native bool,
) ([]agentsync.BoundedCoverageBinding, error) {
	return c.admitBoundedCoverage(ctx, bindings, native)
}

func (c *sharedUnwatchedPollCoordinator) admitBoundedCoverage(
	ctx context.Context, bindings []agentsync.BoundedCoverageBinding, native bool,
) ([]agentsync.BoundedCoverageBinding, error) {
	if c.coverage == nil {
		return nil, nil
	}
	admitter, ok := c.coverage.(agentsync.BoundedCoverageAdmitter)
	if !ok {
		return nil, errors.New("bounded coverage resolver cannot admit a lease")
	}
	admitted := make([]agentsync.BoundedCoverageBinding, 0, len(bindings))
	for _, binding := range bindings {
		c.coverageMu.Lock()
		passRunning := c.coveragePassRunning
		passDone := c.coveragePassDone
		c.coverageMu.Unlock()
		if passRunning {
			select {
			case <-ctx.Done():
				return admitted, ctx.Err()
			case <-passDone:
			}
			continue
		}
		file, err := os.Stat(binding.DBPath)
		if err != nil {
			return admitted, err
		}
		c.coverageMu.Lock()
		state := c.coverageState[binding.Key]
		needsLease := state == nil || state.dbFile == nil || !sameBoundedFile(state.dbFile, file)
		generation := uint64(0)
		if needsLease {
			generation = c.nextCoverageGenerationLocked(boundedCoverageGenerationKey(binding))
		}
		oldKey := ""
		frozen := make(map[*boundedCoverageState]bool)
		freeze := func(candidate *boundedCoverageState) {
			if _, recorded := frozen[candidate]; !recorded {
				frozen[candidate] = candidate.frozen
			}
			candidate.frozen = true
		}
		restoreFrozen := func() {
			c.coverageMu.Lock()
			for candidate, wasFrozen := range frozen {
				candidate.frozen = wasFrozen
			}
			c.coverageMu.Unlock()
		}
		if needsLease {
			if state != nil {
				freeze(state)
			}
			for key, candidate := range c.coverageState {
				if filepath.Clean(candidate.binding.PhysicalDBPath) == filepath.Clean(binding.PhysicalDBPath) &&
					filepath.Clean(candidate.binding.Scope) != filepath.Clean(binding.Scope) {
					oldKey = key
					freeze(candidate)
					break
				}
			}
		}
		c.coverageMu.Unlock()
		var lease *agentsync.BoundedCoverageLease
		if needsLease {
			binding.Generation = generation
			if leaseResolver, ok := c.coverage.(agentsync.BoundedCoverageLeaseResolver); ok {
				lease, err = leaseResolver.AdmitBoundedCoverageLease(ctx, binding)
			} else {
				checkpoint, admitErr := admitter.InitializeBoundedCoverage(ctx, binding)
				err = admitErr
				if err == nil {
					lease = &agentsync.BoundedCoverageLease{Binding: binding, Provider: binding.Agent,
						PhysicalDBPath: binding.PhysicalDBPath, ExactProviderScope: binding.Scope,
						Generation: binding.Generation, AdmissionCheckpoint: checkpoint, AdmissionRowZero: true}
				}
			}
			if err != nil {
				restoreFrozen()
				return admitted, err
			}
			if leaseResolver, ok := c.coverage.(agentsync.BoundedCoverageLeaseResolver); ok {
				if _, err = leaseResolver.TransitionBoundedCoverageRequest(
					ctx, lease, nil, lease.AdmissionCheckpoint, true,
				); err != nil {
					restoreFrozen()
					return admitted, err
				}
			}
		}
		c.coverageMu.Lock()
		state = c.coverageState[binding.Key]
		if state != nil && state.dbFile != nil && sameBoundedFile(state.dbFile, file) {
			state.nativeAdmitted = state.nativeAdmitted || native
			state.pollOwned = true
			if native {
				state.mode = boundedModeNative
				state.pendingWake = true
				state.wake = boundedWakePending
			} else if !state.nativeAdmitted {
				state.mode = boundedModePolling
			}
			admitted = append(admitted, state.binding)
			c.coverageMu.Unlock()
			continue
		}
		state = &boundedCoverageState{lease: lease, binding: binding, checkpoint: lease.AdmissionCheckpoint,
			dbFile: file, generation: generation, pendingWake: true,
			wake: boundedWakePending, nativeAdmitted: native, pollOwned: true}
		state.mode = boundedModePolling
		if native {
			state.mode = boundedModeNative
		}
		c.coverageState[binding.Key] = state
		if oldKey != "" {
			delete(c.coverageState, oldKey)
		}
		admitted = append(admitted, binding)
		c.coverageMu.Unlock()
	}
	c.requestPoll()
	return admitted, nil
}

func (c *sharedUnwatchedPollCoordinator) invalidateBoundedCoverageReplacementsLocked(
	binding agentsync.BoundedCoverageBinding,
) {
	physical := filepath.Clean(binding.PhysicalDBPath)
	for _, state := range c.coverageState {
		if filepath.Clean(state.binding.PhysicalDBPath) == physical &&
			filepath.Clean(state.binding.Scope) != filepath.Clean(binding.Scope) {
			// Freeze under coverageMu. The write gate is acquired by the
			// replacement handoff after this lock is released.
			state.frozen = true
		}
	}
}

func boundedCoverageGenerationKey(binding agentsync.BoundedCoverageBinding) string {
	return string(binding.Agent) + "\x00" + filepath.Clean(binding.PhysicalDBPath)
}

func (c *sharedUnwatchedPollCoordinator) PrimedBoundedCoverage(
	bindings []agentsync.BoundedCoverageBinding,
) []agentsync.BoundedCoverageBinding {
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	primed := make([]agentsync.BoundedCoverageBinding, 0, len(bindings))
	for _, binding := range bindings {
		if state := c.coverageState[binding.Key]; state != nil &&
			state.nativeAdmitted {
			primed = append(primed, binding)
		}
	}
	return primed
}

func (c *sharedUnwatchedPollCoordinator) RetireBoundedCoveragePaths(paths []string) {
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	for key, state := range c.coverageState {
		current, err := os.Stat(state.binding.DBPath)
		if err == nil && (state.dbFile == nil || os.SameFile(state.dbFile, current)) {
			continue
		}
		for _, path := range paths {
			if sameCoverageEventPathForPoll(path, state.binding.DBPath) {
				delete(c.coverageState, key)
				break
			}
		}
	}
}

func (c *sharedUnwatchedPollCoordinator) AddObligation(
	obligation pollingObligation,
) error {
	if obligation.Key == "" {
		return errors.New("polling obligation key is empty")
	}
	return c.updateRoots(obligation, false)
}

func (c *sharedUnwatchedPollCoordinator) RemoveObligation(key string) error {
	return c.updateRoots(pollingObligation{Key: key}, true)
}

func (c *sharedUnwatchedPollCoordinator) updateRoots(
	obligation pollingObligation, remove bool,
) error {
	request := unwatchedPollAdd{
		obligation: pollingObligation{
			Key:    obligation.Key,
			Scopes: append([]pollingScope(nil), obligation.Scopes...),
			Probe:  obligation.Probe,
		},
		remove: remove,
		done:   make(chan struct{}),
	}
	select {
	case <-c.done:
		return errUnwatchedPollStopped
	case c.add <- request:
	}
	<-request.done
	return nil
}

func (c *sharedUnwatchedPollCoordinator) Stop() {
	c.stopOnce.Do(func() {
		c.workerCancel()
		close(c.stop)
	})
	<-c.done
}

func (c *sharedUnwatchedPollCoordinator) run() {
	defer close(c.done)
	defer c.stopTicker()
	go c.runPollWorker()
	defer func() { <-c.pollDone }()
	obligations := make(map[string]pollingObligation)
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.stop:
			return
		case request := <-c.add:
			if request.remove {
				delete(obligations, request.obligation.Key)
				delete(c.ownedBindings, request.obligation.Key)
				c.coverageMu.Lock()
				c.rebuildPollingOwnershipLocked()
				c.coverageMu.Unlock()
			} else {
				obligations[request.obligation.Key] = request.obligation
			}
			if !request.remove && c.coverage != nil {
				roots := make([]agentsync.BoundedCoverageRoot, 0, len(request.obligation.Scopes))
				for _, scope := range request.obligation.Scopes {
					roots = append(roots, agentsync.BoundedCoverageRoot{Agent: scope.Agent, Root: scope.Root})
				}
				bindings, err := c.coverage.BoundedCoverageBindings(c.ctx, roots)
				if err != nil {
					log.Printf("bounded coverage admission: %v", err)
				} else {
					owned := make(map[string]struct{}, len(bindings))
					for _, binding := range bindings {
						owned[binding.Key] = struct{}{}
					}
					if _, err := c.admitBoundedCoverage(c.ctx, bindings, false); err != nil {
						log.Printf("bounded coverage lease admission: %v", err)
					}
					c.ownedBindings[request.obligation.Key] = owned
					c.coverageMu.Lock()
					c.rebuildPollingOwnershipLocked()
					c.coverageMu.Unlock()
				}
			}
			c.setPollObligations(obligations)
			if c.onRootsOwned != nil {
				c.onRootsOwned(unwatchedPollObligationRoots(obligations))
			}
			close(request.done)
		case <-c.ticks:
			if c.coverage != nil {
				if err := c.refreshBoundedCoverage(obligations); err != nil {
					log.Printf("bounded coverage admission: %v", err)
				}
			}
			c.requestPoll()
		}
	}
}

func (c *sharedUnwatchedPollCoordinator) refreshBoundedCoverage(
	obligations map[string]pollingObligation,
) error {
	refreshed := make(map[string]map[string]struct{}, len(obligations))
	allBindings := make([]agentsync.BoundedCoverageBinding, 0)
	for key, obligation := range obligations {
		roots := make([]agentsync.BoundedCoverageRoot, 0, len(obligation.Scopes))
		for _, scope := range obligation.Scopes {
			roots = append(roots, agentsync.BoundedCoverageRoot{Agent: scope.Agent, Root: scope.Root})
		}
		bindings, err := c.coverage.BoundedCoverageBindings(c.ctx, roots)
		if err != nil {
			c.markBoundedCoverageRetry()
			return err
		}
		owned := make(map[string]struct{}, len(bindings))
		for _, binding := range bindings {
			owned[binding.Key] = struct{}{}
			allBindings = append(allBindings, binding)
		}
		refreshed[key] = owned
	}
	if _, err := c.admitBoundedCoverage(c.ctx, allBindings, false); err != nil {
		c.markBoundedCoverageRetry()
		return err
	}
	c.coverageMu.Lock()
	c.ownedBindings = refreshed
	c.rebuildPollingOwnershipLocked()
	c.coverageMu.Unlock()
	return nil
}

func (c *sharedUnwatchedPollCoordinator) rebuildPollingOwnershipLocked() {
	owned := make(map[string]struct{})
	for _, bindings := range c.ownedBindings {
		for key := range bindings {
			owned[key] = struct{}{}
		}
	}
	for key, state := range c.coverageState {
		_, state.pollOwned = owned[key]
		if !state.pollOwned && !state.nativeAdmitted && !state.running {
			delete(c.coverageState, key)
		}
	}
	for key := range owned {
		if state := c.coverageState[key]; state != nil && !state.nativeAdmitted {
			state.pollOwned = true
			state.mode = boundedModePolling
		}
	}
}

func (c *sharedUnwatchedPollCoordinator) markBoundedCoverageRetry() {
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	for _, state := range c.coverageState {
		if state.lease != nil {
			state.retry = true
			state.pendingWake = true
			state.wake = boundedWakePending
		}
	}
}

func (c *sharedUnwatchedPollCoordinator) setPollObligations(
	obligations map[string]pollingObligation,
) {
	snapshot := make([]pollingObligation, 0, len(obligations))
	for _, obligation := range obligations {
		snapshot = append(snapshot, obligation)
	}
	slices.SortFunc(snapshot, func(a, b pollingObligation) int {
		return strings.Compare(a.Key, b.Key)
	})
	c.pollMu.Lock()
	c.pollObligations = snapshot
	c.pollMu.Unlock()
}

func (c *sharedUnwatchedPollCoordinator) currentPollObligations() []pollingObligation {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()
	return append([]pollingObligation(nil), c.pollObligations...)
}

func (c *sharedUnwatchedPollCoordinator) requestPoll() {
	select {
	case c.pollWake <- struct{}{}:
	default:
	}
}

func (c *sharedUnwatchedPollCoordinator) runPollWorker() {
	defer close(c.pollDone)
	for {
		select {
		case <-c.workerCtx.Done():
			return
		default:
		}
		select {
		case <-c.workerCtx.Done():
			return
		case <-c.pollWake:
			if c.workerCtx.Err() != nil {
				return
			}
			// Cooldown gate: if the previous pass completed less than
			// unwatchedPollInterval ago, wait out the remaining idle time.
			// The gate is here (after consuming the wake) so every path into
			// a pass crosses it. lastCompletion is zero on first construction,
			// which means no prior pass and therefore no cooldown on startup.
			c.pollMu.Lock()
			last := c.lastCompletion
			c.pollMu.Unlock()
			if !last.IsZero() {
				remaining := last.Add(unwatchedPollInterval).Sub(c.now())
				if remaining > 0 {
					select {
					case <-c.workerCtx.Done():
						return
					case <-c.after(remaining):
					}
				}
			}
			groups := availableUnwatchedPollScopes(c.currentPollObligations())
			groups = c.excludeAdmittedCoverageScopes(groups)
			totalRoots := countUniqueRoots(groups)
			if totalRoots == 0 && !c.hasBoundedCoverageWork() {
				continue
			}
			log.Printf("polling %d unwatched root(s)", totalRoots)
			c.doWork(func() {
				if c.workerCtx.Err() != nil {
					return
				}
				if err := c.pollBoundedCoverageOnce(c.workerCtx); err != nil {
					log.Printf("polling bounded coverage: %v", err)
				}
				if err := pollUnwatchedScopesOnce(c.workerCtx, c.engine, groups); err != nil {
					log.Printf("polling unwatched roots: %v", err)
				}
			})
			c.pollMu.Lock()
			c.lastCompletion = c.now()
			c.pollMu.Unlock()
		}
	}
}

func (c *sharedUnwatchedPollCoordinator) excludeAdmittedCoverageScopes(
	groups map[parser.AgentType][]string,
) map[parser.AgentType][]string {
	c.coverageMu.Lock()
	bindings := make([]agentsync.BoundedCoverageBinding, 0, len(c.coverageState))
	for _, state := range c.coverageState {
		if !state.pollOwned {
			continue
		}
		bindings = append(bindings, state.binding)
	}
	c.coverageMu.Unlock()
	if len(bindings) == 0 {
		return groups
	}
	filtered := make(map[parser.AgentType][]string, len(groups))
	for agent, roots := range groups {
		for _, root := range roots {
			covered := false
			for _, binding := range bindings {
				if agent != "" && binding.Agent == agent &&
					filepath.Clean(binding.DBPath) == filepath.Clean(root) {
					covered = true
					break
				}
			}
			if !covered {
				filtered[agent] = append(filtered[agent], root)
			}
		}
	}
	return filtered
}

func withinOrEqualForPoll(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func sameCoverageEventPathForPoll(path, dbPath string) bool {
	cleanPath := filepath.Clean(path)
	cleanDB := filepath.Clean(dbPath)
	return cleanPath == cleanDB || cleanPath == cleanDB+"-wal" || cleanPath == cleanDB+"-shm"
}

func (c *sharedUnwatchedPollCoordinator) nextCoverageGenerationLocked(
	key string,
) uint64 {
	if c.coverageEpoch == nil {
		c.coverageEpoch = make(map[string]uint64)
	}
	c.coverageEpoch[key]++
	return c.coverageEpoch[key]
}

func (c *sharedUnwatchedPollCoordinator) hasBoundedCoverageWork() bool {
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	for _, state := range c.coverageState {
		if state.pendingWake || state.auditPending || state.retry {
			return true
		}
	}
	return false
}

func (c *sharedUnwatchedPollCoordinator) commitCoverageState(
	key string, generation uint64, update func(*boundedCoverageState),
) bool {
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	state := c.coverageState[key]
	if state == nil || state.generation != generation {
		return false
	}
	update(state)
	return true
}

func (c *sharedUnwatchedPollCoordinator) retireCoverageState(
	key string, generation uint64,
) bool {
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	state := c.coverageState[key]
	if state == nil || state.generation != generation {
		return false
	}
	delete(c.coverageState, key)
	return true
}

func checkpointAfterBoundedAudit(
	boundary parser.OpenCodeCoverageCheckpoint,
) parser.OpenCodeCoverageCheckpoint {
	if boundary.HighWaterRowID > 0 && boundary.HighWaterEventID != "" {
		known := false
		for _, anchor := range boundary.Anchors {
			if anchor.RowID == boundary.HighWaterRowID {
				known = true
				break
			}
		}
		if !known {
			boundary.Anchors = append(boundary.Anchors, parser.OpenCodeJournalAnchor{
				RowID: boundary.HighWaterRowID, EventID: boundary.HighWaterEventID,
				AggregateID: boundary.HighWaterAggregateID,
			})
		}
	}
	boundary.AuditLatched = false
	boundary.HighWaterKnown = false
	boundary.HighWaterRowID = 0
	boundary.HighWaterEventID = ""
	boundary.HighWaterAggregateID = ""
	boundary.ReadyIDs = nil
	return boundary
}

func (c *sharedUnwatchedPollCoordinator) pollBoundedCoverageOnce(ctx context.Context) error {
	c.coverageMu.Lock()
	if c.coveragePassRunning {
		c.coverageMu.Unlock()
		return nil
	}
	c.coveragePassRunning = true
	passDone := make(chan struct{})
	c.coveragePassDone = passDone
	defer func() {
		c.coverageMu.Lock()
		c.coveragePassRunning = false
		close(passDone)
		c.coveragePassDone = nil
		c.coverageMu.Unlock()
	}()
	type workItem struct {
		key           string
		generation    uint64
		lease         *agentsync.BoundedCoverageLease
		binding       agentsync.BoundedCoverageBinding
		checkpoint    parser.OpenCodeCoverageCheckpoint
		dbFile        os.FileInfo
		auditPending  bool
		auditBoundary parser.OpenCodeCoverageCheckpoint
		retired       bool
	}
	states := make([]workItem, 0, len(c.coverageState))
	for _, state := range c.coverageState {
		if !state.frozen && (state.nativeAdmitted || state.pollOwned) && (state.pendingWake || state.auditPending || state.retry) {
			state.running = true
			state.pendingWake = false
			binding := state.binding
			binding.Generation = state.generation
			states = append(states, workItem{key: state.binding.Key, lease: state.lease,
				generation: state.generation, binding: binding,
				checkpoint: state.checkpoint, dbFile: state.dbFile, auditPending: state.auditPending,
				auditBoundary: state.auditBoundary})
		}
	}
	requestAudit := c.requestAudit
	requestLeaseAudit := c.requestLeaseAudit
	c.coverageMu.Unlock()
	for _, work := range states {
		leaseResolver, hasLeaseResolver := c.coverage.(agentsync.BoundedCoverageLeaseResolver)
		if work.auditPending {
			if requestLeaseAudit != nil && work.lease != nil {
				if err := requestLeaseAudit(ctx, work.lease, "bounded coverage repair"); err != nil {
					c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true })
					return err
				}
			} else if requestAudit == nil {
				c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true })
				continue
			} else if err := requestAudit(ctx, work.binding, "bounded coverage repair"); err != nil {
				c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true })
				return err
			}
			work.checkpoint = checkpointAfterBoundedAudit(work.auditBoundary)
			work.auditPending = false
		}
		more := false
		for page := 0; page < 32; page++ {
			var result parser.OpenCodeFeedResult
			var sources []parser.SourceRef
			var err error
			if hasLeaseResolver && work.lease != nil {
				result, sources, err = leaseResolver.DrainBoundedCoverageLease(ctx, work.lease, work.checkpoint)
			} else {
				result, sources, err = c.coverage.DrainBoundedCoverage(ctx, work.binding, work.checkpoint)
			}
			if err == nil && c.onBoundedCoveragePage != nil {
				c.onBoundedCoveragePage(result)
			}
			if err != nil {
				work.auditBoundary = result.Next
				if errors.Is(err, parser.ErrOpenCodeCoverageDatabaseMissing) {
					work.auditPending = true
					if requestLeaseAudit != nil && work.lease != nil || requestAudit != nil {
						var auditErr error
						if requestLeaseAudit != nil && work.lease != nil {
							auditErr = requestLeaseAudit(ctx, work.lease, err.Error())
						} else {
							auditErr = requestAudit(ctx, work.binding, err.Error())
						}
						if auditErr != nil {
							c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true })
							return auditErr
						}
						work.checkpoint = checkpointAfterBoundedAudit(work.auditBoundary)
						work.auditPending = false
						if errors.Is(err, parser.ErrOpenCodeCoverageDatabaseMissing) {
							c.retireCoverageState(work.key, work.generation)
							work.retired = true
							break
						}
						continue
					}
				}
				if errors.Is(err, agentsync.ErrBoundedCoverageUnresolved) {
					// An unresolved identity is a retryable source-resolution failure,
					// not structural evidence. Keep the checkpoint and wake intact.
					c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) {
						s.running = false
						s.retry = true
						s.pendingWake = true
						s.wake = boundedWakePending
					})
					continue
				}
				c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) {
					s.running = false
					s.retry = true
					s.pendingWake = true
					s.wake = boundedWakePending
					s.auditPending = work.auditPending
					s.auditBoundary = work.auditBoundary
				})
				return err
			}
			if result.AuditRequired {
				work.auditBoundary = result.Next
				work.auditPending = true
				if requestLeaseAudit == nil && requestAudit == nil {
					c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) {
						s.running = false
						s.auditPending = true
						s.auditBoundary = work.auditBoundary
					})
					continue
				}
				var auditErr error
				if requestLeaseAudit != nil && work.lease != nil {
					auditErr = requestLeaseAudit(ctx, work.lease, "structural journal evidence")
				} else if requestAudit != nil {
					auditErr = requestAudit(ctx, work.binding, "structural journal evidence")
				}
				if auditErr != nil {
					c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true; s.auditBoundary = work.auditBoundary })
					return auditErr
				}
				work.checkpoint = checkpointAfterBoundedAudit(work.auditBoundary)
				work.auditPending = false
				continue
			}
			if len(sources) > 0 {
				var stats agentsync.SyncStats
				nextCheckpoint := result.Next
				if !result.More {
					nextCheckpoint.PendingIDs = append([]string(nil), result.PendingIDs...)
				}
				if hasLeaseResolver && work.lease != nil {
					transition, transitionErr := leaseResolver.TransitionBoundedCoverageRequest(
						ctx, work.lease, sources, nextCheckpoint, false,
					)
					stats, err = transition.Stats, transitionErr
					if err == nil {
						work.checkpoint = transition.Checkpoint
					}
				} else {
					stats, err = c.coverage.ApplyBoundedCoverageSources(ctx, sources)
				}
				if err != nil {
					c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true })
					return err
				}
				if c.onBoundedCoverageApply != nil {
					c.onBoundedCoverageApply(stats)
				}
				if !hasLeaseResolver || work.lease == nil {
					work.checkpoint = nextCheckpoint
				}
			}
			work.checkpoint = result.Next
			if !result.More {
				work.checkpoint.PendingIDs = append([]string(nil), result.PendingIDs...)
			}
			if !result.More {
				break
			}
			more = true
		}
		if work.retired {
			continue
		}
		currentFile, statErr := os.Stat(work.binding.DBPath)
		if statErr != nil || !sameBoundedFile(work.dbFile, currentFile) {
			c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) {
				s.running = false
				s.retry = true
				s.pendingWake = true
				s.wake = boundedWakePending
			})
			continue
		}
		if more {
			c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.pendingWake = true })
			c.requestPoll()
		}
		c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) {
			s.checkpoint = work.checkpoint
			s.auditBoundary = work.auditBoundary
			s.auditPending = work.auditPending
			s.retry = false
			s.running = false
		})
	}
	return nil
}

// availableUnwatchedPollScopes selects the reconciliation scopes whose
// obligations are currently pollable, grouped by agent. An obligation with a
// probe path is gated on that physical path: while it is missing, its scopes
// are deferred entirely rather than authoritatively reconciled.
//
// Blocking is conservative in both directions between the empty agent and named
// agents. The empty agent means "every provider" for deferral (an unscoped
// reconciliation pass walks all providers, including any deferred one) and
// "unscoped" for reconciliation. Therefore:
//   - A root blocked under the empty agent also defers every named-agent
//     candidate for that root.
//   - A root blocked under any named agent also defers the empty-agent
//     candidate for that root.
//
// Within each agent, overlap blocking extends beyond exact root matches
// (overlapsDeferredScope), so a pollable ancestor or descendant of a blocked
// root is also deferred for that agent.
func availableUnwatchedPollScopes(
	obligations []pollingObligation,
) map[parser.AgentType][]string {
	// blocked[agent][cleanRoot] = true when agent's probe is missing.
	blocked := make(map[parser.AgentType]map[string]struct{})
	// candidates[agent][root] = true when the root exists and probe is present.
	candidates := make(map[parser.AgentType]map[string]struct{})

	for _, obligation := range obligations {
		probeMissing := false
		if obligation.Probe != "" {
			if _, err := os.Stat(obligation.Probe); err != nil {
				probeMissing = true
			}
		}
		for _, scope := range obligation.Scopes {
			if scope.Root == "" {
				continue
			}
			agent := scope.Agent
			if probeMissing {
				if blocked[agent] == nil {
					blocked[agent] = make(map[string]struct{})
				}
				blocked[agent][filepath.Clean(scope.Root)] = struct{}{}
				continue
			}
			if _, err := os.Stat(scope.Root); err == nil {
				if candidates[agent] == nil {
					candidates[agent] = make(map[string]struct{})
				}
				candidates[agent][scope.Root] = struct{}{}
			}
		}
	}

	// Pre-build the union of all named-agent blocked roots for the empty-agent
	// cross-direction check below.
	allNamedBlocked := make(map[string]struct{})
	for namedAgent, namedBlocked := range blocked {
		if namedAgent == "" {
			continue
		}
		for root := range namedBlocked {
			allNamedBlocked[root] = struct{}{}
		}
	}
	emptyAgentBlocked := blocked[parser.AgentType("")]

	result := make(map[parser.AgentType][]string)
	for agent, agentCandidates := range candidates {
		agentBlocked := blocked[agent]
		for root := range agentCandidates {
			cleanRoot := filepath.Clean(root)
			if agentBlocked != nil && overlapsDeferredScope(cleanRoot, agentBlocked) {
				continue
			}
			// Cross-agent blocking: an unscoped reconciliation pass walks every
			// provider, so a root deferred under either the empty agent or any
			// named agent must also block the other side.
			if agent != "" && emptyAgentBlocked != nil &&
				overlapsDeferredScope(cleanRoot, emptyAgentBlocked) {
				continue
			}
			if agent == "" && overlapsDeferredScope(cleanRoot, allNamedBlocked) {
				continue
			}
			result[agent] = append(result[agent], root)
		}
		if len(result[agent]) > 0 {
			slices.Sort(result[agent])
		} else {
			delete(result, agent)
		}
	}
	return result
}

// countUniqueRoots returns the number of unique root paths across all agent groups.
func countUniqueRoots(groups map[parser.AgentType][]string) int {
	unique := make(map[string]struct{})
	for _, roots := range groups {
		for _, root := range roots {
			unique[root] = struct{}{}
		}
	}
	return len(unique)
}

func unwatchedPollObligationRoots(obligations map[string]pollingObligation) []string {
	owned := make(map[string]struct{})
	for _, obligation := range obligations {
		for _, scope := range obligation.Scopes {
			if scope.Root != "" {
				owned[scope.Root] = struct{}{}
			}
		}
	}
	return unwatchedPollRoots(owned)
}

func unwatchedPollRoots(owned map[string]struct{}) []string {
	roots := make([]string, 0, len(owned))
	for root := range owned {
		roots = append(roots, root)
	}
	slices.Sort(roots)
	return roots
}

// pollUnwatchedScopesOnce issues one grouped reconcile call covering every
// agent group, in agent order. The engine attempts every group even when an
// earlier one errors and shares one archive-sized epilogue (subagent linking,
// skip-cache persistence) across the batch, so per-pass database work does not
// multiply with the number of providers holding obligations.
func pollUnwatchedScopesOnce(
	ctx context.Context,
	engine unwatchedPollSyncer,
	groups map[parser.AgentType][]string,
) error {
	if len(groups) == 0 {
		return nil
	}
	agents := make([]parser.AgentType, 0, len(groups))
	for agent := range groups {
		agents = append(agents, agent)
	}
	slices.SortFunc(agents, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})
	grouped := make([]agentsync.ProviderRootsGroup, 0, len(agents))
	for _, agent := range agents {
		grouped = append(grouped, agentsync.ProviderRootsGroup{
			Agent: agent, Roots: groups[agent],
		})
	}
	return engine.ReconcileProviderRootsGrouped(ctx, grouped)
}
