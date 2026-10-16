package main

import (
	"context"
	"errors"
	"fmt"
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
	Key           string
	Scopes        []pollingScope
	ReleaseScopes []pollingScope
	// Probe mirrors sync.PollingObligation.Probe: the physical watcher path
	// whose availability gates this obligation's reconciliation scopes. When
	// it is missing, the scopes are deferred rather than reconciled
	// authoritatively — a nested physical root (Gemini's <root>/tmp) can
	// vanish while its configured scope <root> still exists, and reconciling
	// the scope then would tombstone every session under the missing
	// subtree. Empty means the Scopes' roots themselves are probed.
	Probe          string
	releasePending bool
	releaseVersion uint64 // A completed pass must preserve newer polling ownership.
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
	now          func() time.Time
	after        func(time.Duration) <-chan time.Time
	add          chan unwatchedPollAdd
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
		ctx:          ctx,
		workerCtx:    workerCtx,
		workerCancel: workerCancel,
		engine:       engine,
		ticks:        ticks,
		stopTicker:   stopTicker,
		doWork:       doWork,
		now:          now,
		after:        after,
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
	if strings.HasPrefix(key, agentsync.FSNotifyFolderPollingPrefix) {
		if err := c.updateRoots(pollingObligation{Key: key, releasePending: true}, false); err != nil {
			return err
		}
		c.requestPoll()
		return nil
	}
	return c.updateRoots(pollingObligation{Key: key}, true)
}

func (c *sharedUnwatchedPollCoordinator) updateRoots(
	obligation pollingObligation, remove bool,
) error {
	request := unwatchedPollAdd{
		obligation: pollingObligation{
			Key:            obligation.Key,
			Scopes:         append([]pollingScope(nil), obligation.Scopes...),
			ReleaseScopes:  append([]pollingScope(nil), obligation.ReleaseScopes...),
			Probe:          obligation.Probe,
			releasePending: obligation.releasePending,
			releaseVersion: obligation.releaseVersion,
		},
		remove: remove,
		done:   make(chan struct{}),
	}
	select {
	case <-c.workerCtx.Done():
		return errUnwatchedPollStopped
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
	var releaseVersion uint64
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.stop:
			return
		case request := <-c.add:
			if request.remove {
				if request.obligation.releaseVersion == 0 || obligations[request.obligation.Key].releaseVersion == request.obligation.releaseVersion {
					delete(obligations, request.obligation.Key)
				}
			} else if request.obligation.releasePending {
				if obligation, ok := obligations[request.obligation.Key]; ok {
					releaseVersion++
					obligation.releasePending = true
					if len(obligation.ReleaseScopes) > 0 {
						obligation.Scopes = obligation.ReleaseScopes
					}
					obligation.releaseVersion = releaseVersion
					obligations[obligation.Key] = obligation
				}
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
			groups, pending := availableUnwatchedPollScopes(c.currentPollObligations())
			totalRoots := countUniqueRoots(groups)
			if totalRoots == 0 && len(pending) == 0 {
				continue
			}
			if totalRoots > 0 {
				log.Printf("polling %d unwatched root(s): %s", totalRoots, formatUnwatchedPollScopes(groups))
			}
			startedAt := c.now()
			c.doWork(func() {
				if c.workerCtx.Err() != nil {
					return
				}
				if err := pollUnwatchedScopesOnce(c.workerCtx, c.engine, groups); err != nil {
					log.Printf("polling unwatched roots failed after %s: %v",
						c.now().Sub(startedAt).Round(time.Millisecond), err)
					return
				}
				for key, version := range pending {
					if err := c.updateRoots(pollingObligation{Key: key, releaseVersion: version}, true); err != nil {
						return
					}
				}
			})
			completedAt := c.now()
			if totalRoots > 0 {
				log.Printf("polled %d unwatched root(s) in %s", totalRoots, completedAt.Sub(startedAt).Round(time.Millisecond))
			}
			c.pollMu.Lock()
			c.lastCompletion = completedAt
			c.pollMu.Unlock()
		}
	}
}

// availableUnwatchedPollScopes selects the reconciliation scopes whose
// obligations are currently pollable, grouped by agent. An obligation with a
// missing probe defers its scopes until recovery queues their final pass.
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
) (map[parser.AgentType][]string, map[string]uint64) {
	// blocked[agent][cleanRoot] = true when agent's probe is missing.
	blocked := make(map[parser.AgentType]map[string]struct{})
	// candidates[agent][root] = true when the root exists and probe is present.
	candidates := make(map[parser.AgentType]map[string]struct{})

	for _, obligation := range obligations {
		probeMissing := false
		if obligation.Probe != "" && !obligation.releasePending {
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
	pending := make(map[string]uint64)
	for _, obligation := range obligations {
		if !obligation.releasePending {
			continue
		}
		covered := true
		for _, scope := range obligation.Scopes {
			if scope.Root == "" || slices.Contains(result[scope.Agent], scope.Root) {
				continue
			}
			if _, err := os.Stat(scope.Root); !errors.Is(err, os.ErrNotExist) {
				covered = false
			}
		}
		if covered {
			pending[obligation.Key] = obligation.releaseVersion
		}
	}
	return result, pending
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

func formatUnwatchedPollScopes(groups map[parser.AgentType][]string) string {
	agents := make([]parser.AgentType, 0, len(groups))
	for agent := range groups {
		agents = append(agents, agent)
	}
	slices.SortFunc(agents, func(a, b parser.AgentType) int {
		return strings.Compare(string(a), string(b))
	})
	parts := make([]string, 0, len(agents))
	for _, agent := range agents {
		label := string(agent)
		if label == "" {
			label = "unscoped"
		}
		parts = append(parts, fmt.Sprintf("%s=%v", label, groups[agent]))
	}
	return strings.Join(parts, " ")
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
