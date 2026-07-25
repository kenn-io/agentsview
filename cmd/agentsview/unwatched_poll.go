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
)

var errUnwatchedPollStopped = errors.New("unwatched poll coordinator stopped")

type unwatchedPollSyncer interface {
	ReconcileWatchRoots(context.Context, []string, bool) error
	ReconcileProviderRoots(context.Context, parser.AgentType, []string) error
}

type unwatchedPollAdd struct {
	obligation pollingObligation
	remove     bool
	done       chan struct{}
}

type pollingObligation struct {
	Key   string
	Agent parser.AgentType
	Roots []string
	// Probe mirrors sync.PollingObligation.Probe: the physical watcher path
	// whose availability gates this obligation's reconciliation Roots. When
	// it is missing, the roots are deferred rather than reconciled
	// authoritatively — a nested physical root (Gemini's <root>/tmp) can
	// vanish while its configured scope <root> still exists, and reconciling
	// the scope then would tombstone every session under the missing
	// subtree. Empty means the Roots themselves are probed.
	Probe string
}

type degradedPollingDecision struct {
	Poll    bool
	Tracked bool
}

type degradedPollingResolver interface {
	ShouldReconcile(context.Context, pollingObligation) (
		degradedPollingDecision, error,
	)
	Forget(pollingObligation)
}

type providerDegradedPollingResolver struct {
	mu     sync.Mutex
	states map[string]string
}

func newProviderDegradedPollingResolver() *providerDegradedPollingResolver {
	return &providerDegradedPollingResolver{
		states: make(map[string]string),
	}
}

func (r *providerDegradedPollingResolver) ShouldReconcile(
	ctx context.Context,
	obligation pollingObligation,
) (degradedPollingDecision, error) {
	if err := ctx.Err(); err != nil {
		return degradedPollingDecision{}, err
	}
	if obligation.Agent == "" || obligation.Probe == "" || len(obligation.Roots) == 0 {
		return degradedPollingDecision{Poll: true}, nil
	}
	provider, ok := parser.NewProvider(obligation.Agent, parser.ProviderConfig{
		Roots: append([]string(nil), obligation.Roots...),
	})
	if !ok {
		r.Forget(obligation)
		return degradedPollingDecision{Poll: true}, nil
	}
	probe, err := parser.ResolveDegradedPollingProbe(ctx, provider, obligation.Probe)
	if err != nil {
		if ctx.Err() != nil {
			return degradedPollingDecision{}, ctx.Err()
		}
		r.Forget(obligation)
		return degradedPollingDecision{Poll: true}, nil
	}
	state, err := probe.DegradedPollingState(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return degradedPollingDecision{}, ctx.Err()
		}
		r.Forget(obligation)
		return degradedPollingDecision{Poll: true}, nil
	}
	r.mu.Lock()
	prior, ok := r.states[obligation.Key]
	r.states[obligation.Key] = state
	r.mu.Unlock()
	if ok && prior == state {
		return degradedPollingDecision{Tracked: true}, nil
	}
	return degradedPollingDecision{Poll: true, Tracked: true}, nil
}

func (r *providerDegradedPollingResolver) Forget(obligation pollingObligation) {
	if obligation.Key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.states, obligation.Key)
}

type sharedUnwatchedPollCoordinator struct {
	ctx          context.Context
	workerCtx    context.Context
	workerCancel context.CancelFunc
	engine       unwatchedPollSyncer
	ticks        <-chan time.Time
	stopTicker   func()
	doWork       func(func())
	// onRootsOwned is a test observer invoked after installation and before ack.
	onRootsOwned func([]string)
	degradedPoll degradedPollingResolver
	add          chan unwatchedPollAdd
	// pollWake coalesces ticks and explicit wakes while the serialized worker runs.
	pollWake chan struct{}
	pollDone chan struct{}
	pollMu   sync.Mutex
	// pollObligations is the latest complete snapshot owned by the
	// coordinator loop; each entry keeps its probe so availability is
	// evaluated per obligation at poll time.
	pollObligations []pollingObligation
	stop            chan struct{}
	done            chan struct{}
	stopOnce        sync.Once
}

func newUnwatchedPollCoordinator(
	ctx context.Context,
	engine unwatchedPollSyncer,
	idleTracker *server.IdleTracker,
) *sharedUnwatchedPollCoordinator {
	ticker := time.NewTicker(unwatchedPollInterval)
	return newUnwatchedPollCoordinatorWithTicks(
		ctx, engine, ticker.C, ticker.Stop, idleTracker.Do, nil,
		newProviderDegradedPollingResolver(),
	)
}

func newUnwatchedPollCoordinatorWithTicks(
	ctx context.Context,
	engine unwatchedPollSyncer,
	ticks <-chan time.Time,
	stopTicker func(),
	doWork func(func()),
	onRootsOwned func([]string),
	degradedPoll degradedPollingResolver,
) *sharedUnwatchedPollCoordinator {
	workerCtx, workerCancel := context.WithCancel(ctx)
	coordinator := &sharedUnwatchedPollCoordinator{
		ctx:          ctx,
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
		engine:       engine,
		ticks:        ticks,
		stopTicker:   stopTicker,
		doWork:       doWork,
		onRootsOwned: onRootsOwned,
		degradedPoll: degradedPoll,
		add:          make(chan unwatchedPollAdd),
		pollWake:     make(chan struct{}, 1),
		pollDone:     make(chan struct{}),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	go coordinator.run()
	return coordinator
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
			Key:   obligation.Key,
			Agent: obligation.Agent,
			Roots: append([]string(nil), obligation.Roots...),
			Probe: obligation.Probe,
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
			} else {
				obligations[request.obligation.Key] = request.obligation
			}
			snapshot := sortedPollObligations(obligations)
			c.pollMu.Lock()
			c.pollObligations = snapshot
			c.pollMu.Unlock()
			if c.degradedPoll != nil {
				c.degradedPoll.Forget(request.obligation)
			}
			if c.onRootsOwned != nil {
				c.onRootsOwned(unwatchedPollObligationRoots(obligations))
			}
			close(request.done)
		case <-c.ticks:
			c.requestPoll()
		}
	}
}

func sortedPollObligations(
	obligations map[string]pollingObligation,
) []pollingObligation {
	snapshot := make([]pollingObligation, 0, len(obligations))
	for _, obligation := range obligations {
		snapshot = append(snapshot, obligation)
	}
	slices.SortFunc(snapshot, func(a, b pollingObligation) int {
		return strings.Compare(a.Key, b.Key)
	})
	return snapshot
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
			obligations := c.currentPollSnapshot()
			selected, tracked, err := c.preparePollRun(c.workerCtx, obligations)
			if err != nil {
				c.resetTracked(tracked, tracked)
				return
			}
			if len(selected) == 0 {
				continue
			}
			log.Printf("polling %d unwatched root(s)", len(unwatchedPollObligationSliceRoots(selected)))
			c.doWork(func() {
				if c.workerCtx.Err() != nil {
					c.resetTracked(tracked, tracked)
					return
				}
				failed, err := pollUnwatchedRootsOnce(c.workerCtx, c.engine, selected)
				if err != nil {
					c.resetTracked(tracked, failed)
					return
				}
			})
		}
	}
}

// availableUnwatchedPollRoots selects the reconciliation roots whose
// obligations are currently pollable. An obligation with a probe path is gated
// on that physical path: while it is missing, its roots are deferred entirely
// rather than authoritatively reconciled, because the configured scope can
// still exist while the physical subtree holding every session is gone.
//
// A root shared by several obligations is gated on every probe that references
// it, not just one: Gemini's shallow <root> metadata plan and recursive
// <root>/tmp plan both reconcile <root>, and the present shallow plan must not
// make <root> pollable while the subtree holding every session is missing.
//
// Blocking extends beyond exact root matches to every candidate overlapping a
// blocked root in either direction (overlapsDeferredScope): ReconcileWatchRoots
// expands each requested root to the configured dirs above and below it, so a
// pollable ancestor or descendant of a blocked root would reconcile the
// deferred scope as an authoritative empty discovery and tombstone its
// sessions. Provider-owned candidates only honor blockers from the same
// provider plus unscoped blockers, because ReconcileProviderRoots cannot
// expand into another provider's unavailable scope.
func availableUnwatchedPollRoots(obligations []pollingObligation) []string {
	selected, _, err := availableUnwatchedPollObligations(
		context.Background(), obligations, nil,
	)
	if err != nil {
		return nil
	}
	return unwatchedPollObligationSliceRoots(selected)
}

func unwatchedPollObligationRoots(obligations map[string]pollingObligation) []string {
	owned := make(map[string]struct{})
	for _, obligation := range obligations {
		for _, root := range obligation.Roots {
			if root != "" {
				owned[root] = struct{}{}
			}
		}
	}
	return unwatchedPollRoots(owned)
}

func unwatchedPollObligationSliceRoots(obligations []pollingObligation) []string {
	owned := make(map[string]struct{})
	for _, obligation := range obligations {
		for _, root := range obligation.Roots {
			if root != "" {
				owned[root] = struct{}{}
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

func pollUnwatchedRootsOnce(
	ctx context.Context, engine unwatchedPollSyncer, obligations []pollingObligation,
) ([]pollingObligation, error) {
	if len(obligations) == 0 {
		return nil, nil
	}
	watchRoots := make(map[string]struct{})
	generic := make([]pollingObligation, 0)
	byAgent := make(map[parser.AgentType]map[string]struct{})
	byAgentObligations := make(map[parser.AgentType][]pollingObligation)
	for _, obligation := range obligations {
		target := watchRoots
		if obligation.Agent != "" {
			if byAgent[obligation.Agent] == nil {
				byAgent[obligation.Agent] = make(map[string]struct{})
			}
			target = byAgent[obligation.Agent]
			byAgentObligations[obligation.Agent] = append(
				byAgentObligations[obligation.Agent], obligation,
			)
		} else {
			generic = append(generic, obligation)
		}
		for _, root := range obligation.Roots {
			if root != "" {
				target[root] = struct{}{}
			}
		}
	}
	var pollErr error
	failed := make([]pollingObligation, 0)
	if roots := unwatchedPollRoots(watchRoots); len(roots) > 0 {
		if err := engine.ReconcileWatchRoots(ctx, roots, false); err != nil {
			log.Printf("polling unwatched roots: %v", err)
			pollErr = errors.Join(pollErr, err)
			failed = append(failed, generic...)
		}
	}
	agents := make([]parser.AgentType, 0, len(byAgent))
	for agent := range byAgent {
		agents = append(agents, agent)
	}
	slices.Sort(agents)
	for _, agent := range agents {
		roots := unwatchedPollRoots(byAgent[agent])
		if len(roots) == 0 {
			continue
		}
		if err := engine.ReconcileProviderRoots(ctx, agent, roots); err != nil {
			log.Printf("polling unwatched %s roots: %v", agent, err)
			pollErr = errors.Join(pollErr, err)
			failed = append(failed, byAgentObligations[agent]...)
		}
	}
	return failed, pollErr
}

func (c *sharedUnwatchedPollCoordinator) currentPollSnapshot() []pollingObligation {
	c.pollMu.Lock()
	defer c.pollMu.Unlock()
	return append([]pollingObligation(nil), c.pollObligations...)
}

func (c *sharedUnwatchedPollCoordinator) preparePollRun(
	ctx context.Context,
	obligations []pollingObligation,
) ([]pollingObligation, []pollingObligation, error) {
	return availableUnwatchedPollObligations(
		ctx, obligations, c.degradedPoll,
	)
}

func (c *sharedUnwatchedPollCoordinator) resetTracked(
	tracked, failed []pollingObligation,
) {
	if c.degradedPoll == nil || len(tracked) == 0 || len(failed) == 0 {
		return
	}
	failedByKey := make(map[string]struct{}, len(failed))
	for _, obligation := range failed {
		failedByKey[obligation.Key] = struct{}{}
	}
	for _, obligation := range tracked {
		if _, ok := failedByKey[obligation.Key]; ok {
			c.degradedPoll.Forget(obligation)
		}
	}
}

func availableUnwatchedPollObligations(
	ctx context.Context,
	obligations []pollingObligation,
	degradedPoll degradedPollingResolver,
) ([]pollingObligation, []pollingObligation, error) {
	blockedGlobal := make(map[string]struct{})
	blockedByAgent := make(map[parser.AgentType]map[string]struct{})
	trackedByKey := make(map[string]pollingObligation)
	selected := make([]pollingObligation, 0, len(obligations))
	for _, obligation := range obligations {
		probeMissing := false
		if obligation.Probe != "" {
			if _, err := os.Stat(obligation.Probe); err != nil {
				probeMissing = true
			}
		}
		if probeMissing {
			target := blockedGlobal
			if obligation.Agent != "" {
				if blockedByAgent[obligation.Agent] == nil {
					blockedByAgent[obligation.Agent] = make(map[string]struct{})
				}
				target = blockedByAgent[obligation.Agent]
			}
			for _, root := range obligation.Roots {
				if root != "" {
					target[filepath.Clean(root)] = struct{}{}
				}
			}
			continue
		}
		if degradedPoll != nil && obligation.Agent != "" {
			decision, err := degradedPoll.ShouldReconcile(ctx, obligation)
			if err != nil {
				return nil, nil, err
			}
			if !decision.Poll {
				continue
			}
			if decision.Tracked {
				trackedByKey[obligation.Key] = obligation
			}
		}
		roots := make([]string, 0, len(obligation.Roots))
		for _, root := range obligation.Roots {
			if root == "" {
				continue
			}
			if _, err := os.Stat(root); err == nil {
				roots = append(roots, root)
			}
		}
		if len(roots) == 0 {
			if tracked, ok := trackedByKey[obligation.Key]; ok && degradedPoll != nil {
				degradedPoll.Forget(tracked)
				delete(trackedByKey, obligation.Key)
			}
			continue
		}
		selected = append(selected, pollingObligation{
			Key:   obligation.Key,
			Agent: obligation.Agent,
			Roots: roots,
			Probe: obligation.Probe,
		})
	}
	filtered := make([]pollingObligation, 0, len(selected))
	for _, obligation := range selected {
		roots := make([]string, 0, len(obligation.Roots))
		for _, root := range obligation.Roots {
			if !rootBlockedForObligation(
				filepath.Clean(root), obligation.Agent, blockedGlobal, blockedByAgent,
			) {
				roots = append(roots, root)
			}
		}
		if len(roots) == 0 {
			if tracked, ok := trackedByKey[obligation.Key]; ok && degradedPoll != nil {
				degradedPoll.Forget(tracked)
				delete(trackedByKey, obligation.Key)
			}
			continue
		}
		if len(roots) != len(obligation.Roots) {
			if tracked, ok := trackedByKey[obligation.Key]; ok && degradedPoll != nil {
				degradedPoll.Forget(tracked)
				delete(trackedByKey, obligation.Key)
			}
		}
		obligation.Roots = roots
		filtered = append(filtered, obligation)
	}
	tracked := make([]pollingObligation, 0, len(filtered))
	for _, obligation := range filtered {
		if trackedObligation, ok := trackedByKey[obligation.Key]; ok {
			tracked = append(tracked, trackedObligation)
		}
	}
	return filtered, tracked, nil
}

func rootBlockedForObligation(
	root string,
	agent parser.AgentType,
	blockedGlobal map[string]struct{},
	blockedByAgent map[parser.AgentType]map[string]struct{},
) bool {
	if overlapsDeferredScope(root, blockedGlobal) {
		return true
	}
	if agent == "" {
		for _, blocked := range blockedByAgent {
			if overlapsDeferredScope(root, blocked) {
				return true
			}
		}
		return false
	}
	return overlapsDeferredScope(root, blockedByAgent[agent])
}
