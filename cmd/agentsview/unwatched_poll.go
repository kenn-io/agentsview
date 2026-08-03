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
	binding          agentsync.BoundedCoverageBinding
	checkpoint       parser.OpenCodeCoverageCheckpoint
	nativeAdmitted   bool
	admissionPending bool
	pollOwned        bool
	dbFile           os.FileInfo
	generation       uint64
	pendingWake      bool
	running          bool
	retry            bool
	auditPending     bool
	auditBoundary    parser.OpenCodeCoverageCheckpoint
	mode             boundedBindingMode
	wake             boundedWake
}

func sameBoundedFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return a == b
	}
	return os.SameFile(a, b)
}

func (s *boundedCoverageState) admit(binding agentsync.BoundedCoverageBinding, file os.FileInfo, checkpoint parser.OpenCodeCoverageCheckpoint, generation uint64, native bool) {
	s.binding = binding
	s.dbFile = file
	s.checkpoint = checkpoint
	s.generation = generation
	s.mode = boundedModeAdmitted
	if native {
		s.mode = boundedModeNative
	} else {
		s.mode = boundedModePolling
	}
	s.wake = boundedWakePending
	s.pendingWake = true
	s.nativeAdmitted = native
	s.pollOwned = !native
	s.admissionPending = false
}

type sharedUnwatchedPollCoordinator struct {
	ctx           context.Context
	workerCtx     context.Context
	workerCancel  context.CancelFunc
	engine        unwatchedPollSyncer
	coverage      agentsync.BoundedCoverageResolver
	coverageMu    sync.Mutex
	coverageState map[string]*boundedCoverageState
	coverageEpoch map[string]uint64
	requestAudit  func(context.Context, agentsync.BoundedCoverageBinding, string) error
	ticks         <-chan time.Time
	stopTicker    func()
	doWork        func(func())
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
	pollWake chan struct{}
	pollDone chan struct{}
	pollMu   sync.Mutex
	// pollObligations is the latest complete snapshot owned by the
	// coordinator loop; each entry keeps its probe so availability is
	// evaluated per obligation at poll time.
	pollObligations []pollingObligation
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
			state.generation = c.nextCoverageGenerationLocked(binding.Key)
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

func (c *sharedUnwatchedPollCoordinator) PrimedBoundedCoverage(
	bindings []agentsync.BoundedCoverageBinding,
) []agentsync.BoundedCoverageBinding {
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	primed := make([]agentsync.BoundedCoverageBinding, 0, len(bindings))
	for _, binding := range bindings {
		if state := c.coverageState[binding.Key]; state != nil &&
			state.nativeAdmitted && !state.admissionPending {
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

func (c *sharedUnwatchedPollCoordinator) PrimeBoundedCoverage(
	ctx context.Context, roots []agentsync.BoundedCoverageRoot,
) error {
	if c.coverage == nil {
		return nil
	}
	bindings, err := c.coverage.BoundedCoverageBindings(ctx, roots)
	if err != nil {
		return err
	}
	_, ok := c.coverage.(agentsync.BoundedCoveragePrimer)
	if !ok {
		return nil
	}
	return c.PrimeBoundedCoverageBindings(ctx, bindings)
}

func (c *sharedUnwatchedPollCoordinator) PrimeBoundedCoverageBindings(
	ctx context.Context, bindings []agentsync.BoundedCoverageBinding,
) error {
	primer, ok := c.coverage.(agentsync.BoundedCoveragePrimer)
	if !ok {
		return nil
	}
	for _, binding := range bindings {
		c.coverageMu.Lock()
		state := c.coverageState[binding.Key]
		current, statErr := os.Stat(binding.DBPath)
		if state != nil && ((statErr == nil && state.dbFile != nil && !sameBoundedFile(state.dbFile, current)) ||
			(statErr != nil && errors.Is(statErr, os.ErrNotExist))) {
			state.mode = boundedModeRetired
			delete(c.coverageState, binding.Key)
			state = nil
		}
		initialized := state != nil && state.checkpoint.Initialized
		if initialized && state.admissionPending {
			state.admissionPending = false
			state.nativeAdmitted = true
			state.generation = c.nextCoverageGenerationLocked(binding.Key)
		}
		c.coverageMu.Unlock()
		if initialized {
			continue
		}
		checkpoint, err := primer.PrimeBoundedCoverage(ctx, binding)
		if err != nil {
			return err
		}
		c.coverageMu.Lock()
		state = c.coverageState[binding.Key]
		if state == nil {
			state = &boundedCoverageState{binding: binding}
			c.coverageState[binding.Key] = state
		} else if state.checkpoint.Initialized {
			c.coverageMu.Unlock()
			continue
		}
		state.binding = binding
		state.checkpoint = checkpoint
		state.dbFile, _ = os.Stat(binding.DBPath)
		state.mode = boundedModeNative
		state.wake = boundedWakePending
		state.pendingWake = true
		state.nativeAdmitted = true
		state.generation = c.nextCoverageGenerationLocked(binding.Key)
		c.coverageMu.Unlock()
	}
	return nil
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
	ownedBindings := make(map[string]map[string]struct{})
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.stop:
			return
		case request := <-c.add:
			if request.remove {
				delete(obligations, request.obligation.Key)
				for key := range ownedBindings[request.obligation.Key] {
					c.coverageMu.Lock()
					if state := c.coverageState[key]; state != nil {
						state.pollOwned = false
						if !state.nativeAdmitted {
							delete(c.coverageState, key)
						}
					}
					c.coverageMu.Unlock()
				}
				delete(ownedBindings, request.obligation.Key)
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
					if err := c.PrimeBoundedCoverageBindings(c.ctx, bindings); err != nil {
						log.Printf("bounded coverage prime: %v", err)
					} else {
						c.coverageMu.Lock()
						for _, binding := range bindings {
							if state := c.coverageState[binding.Key]; state != nil {
								state.pollOwned = true
							}
						}
						c.coverageMu.Unlock()
						c.WakeBoundedCoverage(bindings)
					}
					ownedBindings[request.obligation.Key] = owned
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
	roots := make([]agentsync.BoundedCoverageRoot, 0)
	for _, obligation := range obligations {
		for _, scope := range obligation.Scopes {
			roots = append(roots, agentsync.BoundedCoverageRoot{Agent: scope.Agent, Root: scope.Root})
		}
	}
	bindings, err := c.coverage.BoundedCoverageBindings(c.ctx, roots)
	if err != nil {
		return err
	}
	primer, ok := c.coverage.(agentsync.BoundedCoveragePrimer)
	if !ok {
		return errors.New("bounded coverage resolver cannot prime")
	}
	c.coverageMu.Lock()
	initialized := make(map[string]struct{}, len(c.coverageState))
	pending := make(map[string]struct{}, len(c.coverageState))
	for key, state := range c.coverageState {
		current, statErr := os.Stat(state.binding.DBPath)
		if (statErr == nil && state.dbFile != nil && !sameBoundedFile(state.dbFile, current)) ||
			(statErr != nil && errors.Is(statErr, os.ErrNotExist)) {
			// A replacement is a new physical journal. Retire continuity before
			// admitting the replacement, while coverageEpoch keeps generation monotonic.
			state.mode = boundedModeRetired
			delete(c.coverageState, key)
			continue
		}
		if state.checkpoint.Initialized && state.admissionPending {
			pending[key] = struct{}{}
		} else if state.checkpoint.Initialized {
			initialized[key] = struct{}{}
		}
	}
	c.coverageMu.Unlock()
	checkpoints := make(map[string]parser.OpenCodeCoverageCheckpoint)
	for _, binding := range bindings {
		if _, admitted := initialized[binding.Key]; admitted {
			continue
		}
		if _, waiting := pending[binding.Key]; waiting {
			continue
		}
		checkpoint, err := primer.PrimeBoundedCoverage(c.ctx, binding)
		if err != nil {
			return err
		}
		checkpoints[binding.Key] = checkpoint
	}
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	admitted := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if _, ok := initialized[binding.Key]; ok {
			admitted[binding.Key] = struct{}{}
		} else if _, ok := pending[binding.Key]; ok {
			admitted[binding.Key] = struct{}{}
			continue
		} else if checkpoint, ok := checkpoints[binding.Key]; ok {
			admitted[binding.Key] = struct{}{}
			state := c.coverageState[binding.Key]
			if state == nil {
				state = &boundedCoverageState{binding: binding}
				c.coverageState[binding.Key] = state
			}
			state.binding = binding
			state.checkpoint = checkpoint
			state.dbFile, _ = os.Stat(binding.DBPath)
			state.admissionPending = true
			state.mode = boundedModePolling
			state.pollOwned = true
			state.wake = boundedWakePending
			state.pendingWake = true
			state.generation = c.nextCoverageGenerationLocked(binding.Key)
			continue
		}
		admitted[binding.Key] = struct{}{}
		state := c.coverageState[binding.Key]
		if state == nil {
			continue
		}
		state.binding = binding
		if state.dbFile == nil {
			state.dbFile, _ = os.Stat(binding.DBPath)
		}
		state.pollOwned = true
		state.mode = boundedModePolling
		state.wake = boundedWakePending
		state.pendingWake = true
		state.generation = c.nextCoverageGenerationLocked(binding.Key)
	}
	for key := range c.coverageState {
		if _, ok := admitted[key]; !ok {
			state := c.coverageState[key]
			state.pollOwned = false
			if !state.nativeAdmitted {
				delete(c.coverageState, key)
			}
		}
	}
	return nil
}

func (c *sharedUnwatchedPollCoordinator) primeAfterOrdinaryPoll(
	ctx context.Context,
) error {
	if c.coverage == nil {
		return nil
	}
	obligations := c.currentPollObligations()
	roots := make([]agentsync.BoundedCoverageRoot, 0)
	for _, obligation := range obligations {
		for _, scope := range obligation.Scopes {
			roots = append(roots, agentsync.BoundedCoverageRoot{Agent: scope.Agent, Root: scope.Root})
		}
	}
	bindings, err := c.coverage.BoundedCoverageBindings(ctx, roots)
	if err != nil {
		return err
	}
	if err := c.PrimeBoundedCoverageBindings(ctx, bindings); err != nil {
		return err
	}
	c.coverageMu.Lock()
	defer c.coverageMu.Unlock()
	for _, binding := range bindings {
		if state := c.coverageState[binding.Key]; state != nil && state.checkpoint.Initialized {
			state.pollOwned = true
			// Ordinary reconciliation settles polling ownership; it cannot consume
			// a native wake that was admitted concurrently.
			state.mode = boundedModePolling
			state.pollOwned = true
		}
	}
	return nil
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
				} else if err := c.primeAfterOrdinaryPoll(c.workerCtx); err != nil {
					log.Printf("bounded coverage admission after ordinary poll: %v", err)
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
	type workItem struct {
		key           string
		generation    uint64
		binding       agentsync.BoundedCoverageBinding
		checkpoint    parser.OpenCodeCoverageCheckpoint
		auditPending  bool
		auditBoundary parser.OpenCodeCoverageCheckpoint
		retired       bool
	}
	states := make([]workItem, 0, len(c.coverageState))
	for _, state := range c.coverageState {
		if (state.nativeAdmitted || state.pollOwned) && (state.pendingWake || state.auditPending || state.retry) {
			state.running = true
			state.pendingWake = false
			state.generation = c.nextCoverageGenerationLocked(state.binding.Key)
			states = append(states, workItem{key: state.binding.Key,
				generation: state.generation, binding: state.binding,
				checkpoint: state.checkpoint, auditPending: state.auditPending,
				auditBoundary: state.auditBoundary})
		}
	}
	requestAudit := c.requestAudit
	c.coverageMu.Unlock()
	for _, work := range states {
		if work.auditPending {
			if requestAudit == nil {
				c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true })
				continue
			}
			if err := requestAudit(ctx, work.binding, "bounded coverage repair"); err != nil {
				c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true })
				return err
			}
			work.checkpoint = checkpointAfterBoundedAudit(work.auditBoundary)
			work.auditPending = false
		}
		more := false
		for page := 0; page < 32; page++ {
			result, sources, err := c.coverage.DrainBoundedCoverage(ctx, work.binding, work.checkpoint)
			if err == nil && c.onBoundedCoveragePage != nil {
				c.onBoundedCoveragePage(result)
			}
			if err != nil {
				work.auditBoundary = result.Next
				if errors.Is(err, parser.ErrOpenCodeCoverageDatabaseMissing) {
					work.auditPending = true
					if requestAudit != nil {
						if auditErr := requestAudit(ctx, work.binding, err.Error()); auditErr != nil {
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
					s.auditPending = work.auditPending
					s.auditBoundary = work.auditBoundary
				})
				return err
			}
			if result.AuditRequired {
				work.auditBoundary = result.Next
				work.auditPending = true
				if requestAudit == nil {
					c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) {
						s.running = false
						s.auditPending = true
						s.auditBoundary = work.auditBoundary
					})
					continue
				}
				if err := requestAudit(ctx, work.binding, "structural journal evidence"); err != nil {
					c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true; s.auditBoundary = work.auditBoundary })
					return err
				}
				work.checkpoint = checkpointAfterBoundedAudit(work.auditBoundary)
				work.auditPending = false
				continue
			}
			if len(sources) > 0 {
				stats, err := c.coverage.ApplyBoundedCoverageSources(ctx, sources)
				if err != nil {
					c.commitCoverageState(work.key, work.generation, func(s *boundedCoverageState) { s.running = false; s.retry = true })
					return err
				}
				if c.onBoundedCoverageApply != nil {
					c.onBoundedCoverageApply(stats)
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
