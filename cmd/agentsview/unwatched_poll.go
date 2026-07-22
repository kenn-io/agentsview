package main

import (
	"context"
	"errors"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/server"
)

var errUnwatchedPollStopped = errors.New("unwatched poll coordinator stopped")

type unwatchedPollSyncer interface {
	ReconcileWatchRoots(context.Context, []string, bool) error
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
	Probe string
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
	workerCtx, workerCancel := context.WithCancel(ctx)
	coordinator := &sharedUnwatchedPollCoordinator{
		ctx:          ctx,
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
		engine:       engine,
		ticks:        ticks,
		stopTicker:   stopTicker,
		doWork:       doWork,
		add:          make(chan unwatchedPollAdd),
		pollWake:     make(chan struct{}, 1),
		pollDone:     make(chan struct{}),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
		onRootsOwned: onRootsOwned,
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
			roots := availableUnwatchedPollRoots(c.currentPollObligations())
			if len(roots) == 0 {
				continue
			}
			log.Printf("polling %d unwatched root(s)", len(roots))
			c.doWork(func() {
				if c.workerCtx.Err() != nil {
					return
				}
				pollUnwatchedRootsOnce(c.workerCtx, c.engine, roots)
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
			if probeMissing {
				blocked[root] = struct{}{}
				continue
			}
			if _, err := os.Stat(root); err == nil {
				candidates[root] = struct{}{}
			}
		}
	}
	for root := range blocked {
		delete(candidates, root)
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
