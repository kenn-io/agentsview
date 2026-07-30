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
}

type boundedCoverageSyncer interface {
	ReconcileCoverage(context.Context, parser.CoverageTask) (parser.CoverageResult, error)
}

type unwatchedPollAdd struct {
	obligation pollingObligation
	remove     bool
	done       chan struct{}
}

type pollingObligation struct {
	Key   string
	Roots []string
	// Probe mirrors sync.PollingObligation.Probe: the physical watcher path
	// whose availability gates this obligation's reconciliation Roots. When
	// it is missing, the roots are deferred rather than reconciled
	// authoritatively — a nested physical root (Gemini's <root>/tmp) can
	// vanish while its configured scope <root> still exists, and reconciling
	// the scope then would tombstone every session under the missing
	// subtree. Empty means the Roots themselves are probed.
	Probe            string
	NonBlockingProbe bool
	Scopes           []pollingScope
}

type pollingScope struct {
	Agent                 parser.AgentType
	Root                  string
	CoverageKey           string
	AuthoritativeFallback bool
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
	add          chan unwatchedPollAdd
	// pollWake coalesces ticks and explicit wakes while the serialized worker runs.
	pollWake chan struct{}
	pollDone chan struct{}
	pollMu   sync.Mutex
	// pollObligations is the latest complete snapshot owned by the
	// coordinator loop; each entry keeps its probe so availability is
	// evaluated per obligation at poll time.
	pollObligations []pollingObligation
	completionDelay time.Duration
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
	return newUnwatchedPollCoordinatorWithSchedule(
		ctx, engine, ticker.C, ticker.Stop, idleTracker.Do, nil,
		unwatchedPollInterval,
	)
}

func newUnwatchedPollCoordinatorWithTicks(
	ctx context.Context,
	engine unwatchedPollSyncer,
	ticks <-chan time.Time,
	stopTicker func(),
	doWork func(func()),
	onRootsOwned func([]string),
) *sharedUnwatchedPollCoordinator {
	return newUnwatchedPollCoordinatorWithSchedule(
		ctx, engine, ticks, stopTicker, doWork, onRootsOwned, 0,
	)
}

func newUnwatchedPollCoordinatorWithSchedule(
	ctx context.Context,
	engine unwatchedPollSyncer,
	ticks <-chan time.Time,
	stopTicker func(),
	doWork func(func()),
	onRootsOwned func([]string),
	completionDelay time.Duration,
) *sharedUnwatchedPollCoordinator {
	workerCtx, workerCancel := context.WithCancel(ctx)
	coordinator := &sharedUnwatchedPollCoordinator{
		ctx:             ctx,
		workerCtx:       workerCtx,
		workerCancel:    workerCancel,
		engine:          engine,
		ticks:           ticks,
		stopTicker:      stopTicker,
		doWork:          doWork,
		add:             make(chan unwatchedPollAdd),
		pollWake:        make(chan struct{}, 1),
		pollDone:        make(chan struct{}),
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
		onRootsOwned:    onRootsOwned,
		completionDelay: completionDelay,
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
			Key: obligation.Key, Roots: append([]string(nil), obligation.Roots...),
			Probe: obligation.Probe, NonBlockingProbe: obligation.NonBlockingProbe,
			Scopes: append([]pollingScope(nil), obligation.Scopes...),
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
			c.setPollObligations(obligations)
			if c.onRootsOwned != nil {
				c.onRootsOwned(unwatchedPollObligationRoots(obligations))
			}
			close(request.done)
		case <-c.ticks:
			c.requestPoll()
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
			obligations := c.currentPollObligations()
			if len(obligations) == 0 {
				continue
			}
			if allGenericPollingObligations(obligations) {
				roots := availableUnwatchedPollRoots(obligations)
				if len(roots) == 0 {
					continue
				}
				c.doWork(func() {
					if c.workerCtx.Err() == nil {
						pollUnwatchedRootsOnce(c.workerCtx, c.engine, roots)
					}
				})
				continue
			}
			log.Printf("polling %d unwatched obligation(s)", len(obligations))
			c.doWork(func() {
				if c.workerCtx.Err() != nil {
					return
				}
				available := make(map[string]struct{})
				for _, root := range availableUnwatchedPollRoots(obligations) {
					available[absRootPath(root)] = struct{}{}
				}
				genericRoots := make(map[string]struct{})
				typedRoots := make(map[string]struct{})
				typedScopes := make([]pollingScope, 0)
				seenScopes := make(map[string]struct{})
				for _, obligation := range obligations {
					if !pollingObligationProbeAvailable(obligation) {
						continue
					}
					availableObligation := filterPollingObligationRoots(
						obligation, available,
					)
					if len(obligation.Scopes) == 0 {
						for _, root := range availableObligation.Roots {
							genericRoots[root] = struct{}{}
						}
						continue
					}
					for _, scope := range obligation.Scopes {
						if scope.CoverageKey == "" {
							if _, ok := available[absRootPath(scope.Root)]; !ok {
								continue
							}
						} else if _, err := os.Stat(scope.Root); err != nil {
							continue
						}
						scope.AuthoritativeFallback = coverageFallbackAvailable(
							scope, obligations, c.engine,
						)
						key := string(scope.Agent) + "\x00" + scope.Root + "\x00" + scope.CoverageKey
						if _, ok := seenScopes[key]; ok {
							continue
						}
						seenScopes[key] = struct{}{}
						typedScopes = append(typedScopes, scope)
						typedRoots[scope.Root] = struct{}{}
					}
				}
				if len(genericRoots) > 0 {
					pollUnwatchedRootsOnce(
						c.workerCtx, c.engine, unwatchedPollRoots(genericRoots),
					)
				}
				if len(typedScopes) > 0 {
					pollCoverageOnce(c.workerCtx, c.engine, pollingObligation{
						Roots: unwatchedPollRoots(typedRoots), Scopes: typedScopes,
					})
				}
			})
			if c.completionDelay > 0 {
				delay := c.completionDelay
				c.drainPollWake()
				timer := time.NewTimer(delay)
				select {
				case <-c.workerCtx.Done():
					if !timer.Stop() {
						<-timer.C
					}
					return
				case <-timer.C:
				}
				c.drainPollWake()
				c.requestPoll()
			}
		}
	}
}

func coverageFallbackAvailable(
	scope pollingScope, obligations []pollingObligation, engine unwatchedPollSyncer,
) bool {
	scopeRoot := absRootPath(scope.Root)
	for _, obligation := range obligations {
		overlaps := false
		for _, root := range obligation.Roots {
			root = absRootPath(root)
			if root == scopeRoot || pathWithinRoot(root, scopeRoot) ||
				pathWithinRoot(scopeRoot, root) {
				overlaps = true
				break
			}
		}
		if !overlaps {
			continue
		}
		if obligation.Probe != "" {
			if _, err := os.Stat(obligation.Probe); err != nil {
				owner, _ := engine.(activeSourceProbe)
				hasArchived := false
				if owner != nil {
					hasArchived, err = owner.HasActiveSessionSourceBelow(
						string(scope.Agent), obligation.Probe,
					)
					if err != nil {
						hasArchived = true
					}
				}
				if !obligation.NonBlockingProbe || hasArchived {
					return false
				}
			}
			continue
		}
		for _, root := range obligation.Roots {
			if _, err := os.Stat(root); err != nil {
				return false
			}
		}
	}
	return true
}

func pollingObligationProbeAvailable(obligation pollingObligation) bool {
	if obligation.Probe == "" {
		return true
	}
	_, err := os.Stat(obligation.Probe)
	return err == nil
}

func filterPollingObligationRoots(
	obligation pollingObligation, available map[string]struct{},
) pollingObligation {
	obligation.Roots = slices.DeleteFunc(
		append([]string(nil), obligation.Roots...),
		func(root string) bool {
			_, ok := available[absRootPath(root)]
			return !ok
		},
	)
	obligation.Scopes = slices.DeleteFunc(
		append([]pollingScope(nil), obligation.Scopes...),
		func(scope pollingScope) bool {
			_, ok := available[absRootPath(scope.Root)]
			return !ok
		},
	)
	return obligation
}

func allGenericPollingObligations(obligations []pollingObligation) bool {
	for _, o := range obligations {
		if len(o.Scopes) > 0 {
			return false
		}
	}
	return true
}

func (c *sharedUnwatchedPollCoordinator) drainPollWake() {
	for {
		select {
		case <-c.pollWake:
		default:
			return
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
// sessions.
func availableUnwatchedPollRoots(obligations []pollingObligation) []string {
	candidates := make(map[string]struct{})
	blocked := make(map[string]struct{})
	for _, obligation := range obligations {
		probeMissing := false
		if obligation.Probe != "" {
			if _, err := os.Stat(obligation.Probe); err != nil {
				probeMissing = true
			}
		}
		for _, root := range obligation.Roots {
			if root == "" {
				continue
			}
			if probeMissing && !obligation.NonBlockingProbe {
				blocked[filepath.Clean(root)] = struct{}{}
				continue
			}
			if probeMissing {
				continue
			}
			if _, err := os.Stat(root); err == nil {
				candidates[root] = struct{}{}
			}
		}
	}
	for root := range candidates {
		if overlapsDeferredScope(filepath.Clean(root), blocked) {
			delete(candidates, root)
		}
	}
	return unwatchedPollRoots(candidates)
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

func unwatchedPollRoots(owned map[string]struct{}) []string {
	roots := make([]string, 0, len(owned))
	for root := range owned {
		roots = append(roots, root)
	}
	slices.Sort(roots)
	return roots
}

func pollUnwatchedRootsOnce(
	ctx context.Context, engine unwatchedPollSyncer, roots []string,
) {
	if len(roots) == 0 {
		return
	}
	if err := engine.ReconcileWatchRoots(ctx, roots, false); err != nil {
		log.Printf("polling unwatched roots: %v", err)
	}
}

func pollCoverageOnce(
	ctx context.Context, engine unwatchedPollSyncer, obligation pollingObligation,
) {
	coverage, ok := engine.(boundedCoverageSyncer)
	if !ok || len(obligation.Scopes) == 0 {
		pollUnwatchedRootsOnce(ctx, engine, obligation.Roots)
		return
	}
	for _, scope := range obligation.Scopes {
		if scope.Agent == "" || scope.Root == "" {
			continue
		}
		trigger := parser.CoverageTriggerDegraded
		for page := 0; ; page++ {
			if page >= 1024 {
				log.Printf("polling %s coverage: continuation limit exceeded", scope.Agent)
				break
			}
			result, err := coverage.ReconcileCoverage(ctx, parser.CoverageTask{
				Agent: scope.Agent, CoverageKey: scope.CoverageKey,
				Root: scope.Root, Trigger: trigger,
				AuthoritativeFallback: scope.AuthoritativeFallback,
			})
			if err != nil {
				log.Printf("polling %s coverage: %v", scope.Agent, err)
				break
			}
			if result.AuditRequired {
				log.Printf("polling %s coverage requested archive audit", scope.Agent)
			}
			if !result.More {
				break
			}
			trigger = parser.CoverageTriggerContinuation
			delay := result.NextDelay
			if delay <= 0 {
				delay = 100 * time.Millisecond
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			case <-timer.C:
			}
		}
	}
}
