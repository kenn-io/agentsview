package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type RecursiveWatchResult struct {
	Watched   int
	Unwatched int
	Err       error
	// MissingRootLifecycleOwned reports that the backend has durable native
	// coverage for a root reported missing in the startup plan. Callers do not
	// need a periodic polling obligation while this ownership is active.
	MissingRootLifecycleOwned bool
	BudgetExhausted           bool
	ResourceExhausted         bool
	ResourceExhaustedAt       string
}

const (
	// A callback can hold one batch while the event loop accumulates the next.
	// Bounding each batch keeps watcher backpressure below two bounded path
	// sets instead of allowing a slow sync to retain an unbounded event storm.
	defaultWatchBatchMaxEntries   = 8192
	defaultWatchBatchMaxPathBytes = 2 << 20
)

type WatchItemType uint8

const (
	ItemIsUnknown WatchItemType = iota
	ItemIsFile
	ItemIsDir
)

type WatchRename struct {
	Path     string
	Root     string
	Agent    string
	ItemType WatchItemType
}

// WatchBatch describes one serialized watcher callback. FullSync is an
// explicit overflow signal: retained strings are empty and the consumer must
// rescan all configured sources so coalescing never silently drops a change.
// LostEvents distinguishes overflow recovery from an ordinary authoritative
// reconciliation so freshness shortcuts can be invalidated only when needed.
type WatchBatch struct {
	Paths           []string
	Renames         []WatchRename
	ReconcileRoots  []string
	FullSync        bool
	LostEvents      bool
	lifecycleTokens []backendLifecycleToken
}

type WatchCallback func(context.Context, WatchBatch) error

// PollingObligation identifies one independently releasable reason that a set
// of configured roots needs authoritative polling coverage.
type PollingObligation struct {
	Key   string
	Roots []string
	// Probe is the physical watcher path whose availability gates this
	// obligation's reconciliation Roots. For nested provider roots (e.g.
	// Gemini's <root>/tmp) the physical path differs from the configured
	// reconciliation scope; probing the scope instead would let polling
	// authoritatively reconcile a present <root> while the missing physical
	// subtree holds every session, tombstoning all of them. Empty means the
	// Roots themselves are the physical paths to probe.
	Probe string
}

// WatcherOptions configures runtime ownership handoffs that are not needed by
// portable watcher backends.
type WatcherOptions struct {
	OnCoverageDegraded func([]string) error
	OnPollingRequired  func(PollingObligation) error
	OnPollingReleased  func(string) error
}

// WatchRetryError carries the authoritative reconciliation scope selected by a
// callback after it classifies a batch. The watcher consumes only FullSync and
// ReconcileRoots from WatchRetryBatch; ordinary paths and rename metadata are
// never replayed through this protocol.
type WatchRetryError interface {
	error
	WatchRetryBatch() WatchBatch
}

// pendingWatchBatch bounds retained strings by both unique path count and the
// sum of their byte lengths. Map overhead is bounded separately by the entry
// limit. Once either limit would be exceeded, individual paths are discarded
// in favor of one full-sync marker until Take resets the accumulator.
type pendingWatchBatch struct {
	paths          map[string]struct{}
	renames        map[WatchRename]struct{}
	backendRenames map[pendingBackendRename]struct{}
	roots          map[string]struct{}
	strings        map[string]struct{}
	lifecycle      map[backendLifecycleToken]struct{}
	pathBytes      int
	maxEntries     int
	maxPathBytes   int
	fullSync       bool
	lostEvents     bool
	onOverflow     func()
}

type pendingBackendRename struct {
	Path     string
	Root     string
	ItemType backendItemType
}

func newPendingWatchBatch(maxEntries, maxPathBytes int) *pendingWatchBatch {
	return &pendingWatchBatch{
		paths:          make(map[string]struct{}),
		renames:        make(map[WatchRename]struct{}),
		backendRenames: make(map[pendingBackendRename]struct{}),
		roots:          make(map[string]struct{}),
		strings:        make(map[string]struct{}),
		lifecycle:      make(map[backendLifecycleToken]struct{}),
		maxEntries:     maxEntries,
		maxPathBytes:   maxPathBytes,
	}
}

func (p *pendingWatchBatch) Empty() bool {
	return !p.fullSync && len(p.paths) == 0 && len(p.renames) == 0 &&
		len(p.backendRenames) == 0 && len(p.roots) == 0 && len(p.lifecycle) == 0
}

func (p *pendingWatchBatch) Add(path string) {
	if _, exists := p.paths[path]; exists {
		return
	}
	if !p.retainStrings(path) {
		return
	}
	p.paths[path] = struct{}{}
}

// AddCreatedSubtreePath retains one file found under a newly created
// directory while reserving enough capacity for the owning-root fallback. If
// the file would consume that reserve, the root is retained instead and the
// caller stops enumerating. Unrelated pending work can still exhaust even the
// reserve, in which case the existing full-sync overflow contract applies.
func (p *pendingWatchBatch) AddCreatedSubtreePath(path, root string) bool {
	if p.fullSync {
		return false
	}
	if _, exists := p.paths[path]; exists {
		return true
	}
	if root == "" || p.canRetainStrings(path, root) {
		p.Add(path)
		return !p.fullSync
	}
	p.AddReconcileRoot(root)
	return false
}

func (p *pendingWatchBatch) AddRename(rename WatchRename) {
	if rename.Path == "" {
		return
	}
	if _, exists := p.renames[rename]; exists {
		return
	}
	if len(p.renames)+1 > p.maxEntries {
		p.overflow()
		return
	}
	if !p.retainStrings(rename.Path, rename.Root, rename.Agent) {
		return
	}
	p.renames[rename] = struct{}{}
}

func (p *pendingWatchBatch) AddReconcileRoot(root string) {
	if root == "" {
		return
	}
	if _, exists := p.roots[root]; exists {
		return
	}
	if !p.retainStrings(root) {
		return
	}
	p.roots[root] = struct{}{}
}

func (p *pendingWatchBatch) AddBackendEvent(event backendEvent) bool {
	if event.Op == backendOpUnknown {
		return !p.fullSync
	}
	wasFullSync := p.fullSync
	// Lifecycle acknowledgement remains necessary after a batch has already
	// been promoted to authoritative reconciliation. Record the token first so
	// sustained overflow cannot strand a backend gate.
	if event.Lifecycle.valid() {
		p.AddLifecycle(event.Lifecycle)
	}
	if event.Op&backendOpFullSync != 0 {
		p.AddFullSync()
		return false
	}
	if event.Op&backendOpReconcileRootChange != 0 {
		root := event.Root
		if root == "" {
			root = event.Path
		}
		p.AddReconcileRoot(root)
	}
	if event.Op&backendOpRename != 0 {
		rename := pendingBackendRename{
			Path:     event.Path,
			Root:     event.Root,
			ItemType: event.ItemType,
		}
		if rename.Path == "" {
			return wasFullSync || !p.fullSync
		}
		if _, exists := p.backendRenames[rename]; exists {
			return true
		}
		if !p.retainStrings(rename.Path, rename.Root) {
			return false
		}
		p.backendRenames[rename] = struct{}{}
		return wasFullSync || !p.fullSync
	}
	if event.Op&backendOpReconcileRootChange == 0 && event.Path != "" {
		p.Add(event.Path)
	}
	return wasFullSync || !p.fullSync
}

func (p *pendingWatchBatch) AddLifecycle(token backendLifecycleToken) {
	if !token.valid() {
		return
	}
	if _, exists := p.lifecycle[token]; exists {
		return
	}
	if len(p.strings)+len(p.lifecycle)+1 > p.maxEntries {
		p.overflow()
		if len(p.lifecycle)+1 > p.maxEntries {
			return
		}
	}
	p.lifecycle[token] = struct{}{}
}

func (p *pendingWatchBatch) AddFullSync() {
	p.makeFullSync(true)
}

// retainFullSync adds a retry marker without discarding newer fine-grained
// work that arrived while the failed authoritative callback was running.
func (p *pendingWatchBatch) retainFullSync() {
	p.fullSync = true
}

// merge retains another bounded accumulator's observable work. The source is
// immutable for the duration of the call, which lets native callbacks publish
// through an atomic handoff while the watcher drains the primary accumulator.
func (p *pendingWatchBatch) merge(other *pendingWatchBatch) {
	if other == nil {
		return
	}
	if other.fullSync {
		p.makeFullSync(other.lostEvents)
	}
	for path := range other.paths {
		p.Add(path)
	}
	for rename := range other.renames {
		p.AddRename(rename)
	}
	for rename := range other.backendRenames {
		p.AddBackendEvent(backendEvent{
			Path: rename.Path, Root: rename.Root,
			Op: backendOpRename, ItemType: rename.ItemType,
		})
	}
	for root := range other.roots {
		p.AddReconcileRoot(root)
	}
	for token := range other.lifecycle {
		p.AddLifecycle(token)
	}
}

func (p *pendingWatchBatch) retainStrings(values ...string) bool {
	additionalEntries, additionalBytes := p.retainedStringCost(values...)
	if len(p.strings)+additionalEntries > p.maxEntries ||
		p.pathBytes+additionalBytes > p.maxPathBytes {
		p.overflow()
		return false
	}
	for _, value := range values {
		if value != "" {
			p.strings[value] = struct{}{}
		}
	}
	p.pathBytes += additionalBytes
	return true
}

func (p *pendingWatchBatch) canRetainStrings(values ...string) bool {
	additionalEntries, additionalBytes := p.retainedStringCost(values...)
	return len(p.strings)+additionalEntries <= p.maxEntries &&
		p.pathBytes+additionalBytes <= p.maxPathBytes
}

func (p *pendingWatchBatch) retainedStringCost(values ...string) (int, int) {
	additionalEntries := 0
	additionalBytes := 0
	for i, value := range values {
		if value == "" {
			continue
		}
		if _, exists := p.strings[value]; exists {
			continue
		}
		duplicate := slices.Contains(values[:i], value)
		if duplicate {
			continue
		}
		additionalEntries++
		additionalBytes += len(value)
	}
	return additionalEntries, additionalBytes
}

func (p *pendingWatchBatch) overflow() {
	if !p.fullSync && p.onOverflow != nil {
		p.onOverflow()
	}
	p.makeFullSync(true)
}

func (p *pendingWatchBatch) makeFullSync(lostEvents bool) {
	clear(p.paths)
	clear(p.renames)
	clear(p.backendRenames)
	clear(p.roots)
	clear(p.strings)
	p.pathBytes = 0
	p.fullSync = true
	p.lostEvents = p.lostEvents || lostEvents
}

func (p *pendingWatchBatch) Take() (WatchBatch, bool) {
	return p.TakeWithRootAgents(nil)
}

func (p *pendingWatchBatch) TakeWithRootAgents(
	agentsForRoot func(string) []string,
) (WatchBatch, bool) {
	if p.Empty() {
		return WatchBatch{}, false
	}
	if p.fullSync {
		p.fullSync = false
		lostEvents := p.lostEvents
		p.lostEvents = false
		tokens := p.takeLifecycleTokens()
		return WatchBatch{
			FullSync: true, LostEvents: lostEvents, lifecycleTokens: tokens,
		}, true
	}
	for rename := range p.backendRenames {
		agents := []string{""}
		if agentsForRoot != nil {
			agents = agentsForRoot(rename.Root)
			if len(agents) == 0 {
				agents = []string{""}
			}
		}
		for _, agent := range agents {
			p.AddRename(WatchRename{
				Path:     rename.Path,
				Root:     rename.Root,
				Agent:    agent,
				ItemType: watchItemType(rename.ItemType),
			})
			if p.fullSync {
				p.fullSync = false
				lostEvents := p.lostEvents
				p.lostEvents = false
				tokens := p.takeLifecycleTokens()
				return WatchBatch{
					FullSync: true, LostEvents: lostEvents, lifecycleTokens: tokens,
				}, true
			}
		}
	}

	paths := make([]string, 0, len(p.paths))
	for path := range p.paths {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	renames := make([]WatchRename, 0, len(p.renames))
	for rename := range p.renames {
		renames = append(renames, rename)
	}
	slices.SortFunc(renames, func(a, b WatchRename) int {
		if c := strings.Compare(a.Path, b.Path); c != 0 {
			return c
		}
		if c := strings.Compare(a.Root, b.Root); c != 0 {
			return c
		}
		if c := strings.Compare(a.Agent, b.Agent); c != 0 {
			return c
		}
		return int(a.ItemType) - int(b.ItemType)
	})
	roots := make([]string, 0, len(p.roots))
	for root := range p.roots {
		roots = append(roots, root)
	}
	slices.Sort(roots)
	clear(p.paths)
	clear(p.renames)
	clear(p.backendRenames)
	clear(p.roots)
	clear(p.strings)
	tokens := p.takeLifecycleTokens()
	p.pathBytes = 0
	lostEvents := p.lostEvents
	p.lostEvents = false
	return WatchBatch{
		Paths: paths, Renames: renames, ReconcileRoots: roots,
		LostEvents: lostEvents, lifecycleTokens: tokens,
	}, true
}

func (p *pendingWatchBatch) takeLifecycleTokens() []backendLifecycleToken {
	if len(p.lifecycle) == 0 {
		return nil
	}
	tokens := make([]backendLifecycleToken, 0, len(p.lifecycle))
	for token := range p.lifecycle {
		tokens = append(tokens, token)
	}
	clear(p.lifecycle)
	return tokens
}

type watchEventSink struct {
	mu        sync.Mutex
	pending   *pendingWatchBatch
	handoff   atomic.Pointer[pendingWatchBatch]
	overflows boundedCounter
	wake      chan struct{}
}

type boundedCounter struct {
	value atomic.Uint32
}

func (c *boundedCounter) Add(delta uint32) {
	for delta > 0 {
		current := c.value.Load()
		if current == math.MaxUint32 {
			return
		}
		remaining := math.MaxUint32 - current
		increment := min(delta, remaining)
		if c.value.CompareAndSwap(current, current+increment) {
			return
		}
	}
}

func (c *boundedCounter) Inc() { c.Add(1) }

func (c *boundedCounter) Load() uint32 { return c.value.Load() }

func newWatchEventSink(maxEntries, maxPathBytes int) *watchEventSink {
	sink := &watchEventSink{
		pending: newPendingWatchBatch(maxEntries, maxPathBytes),
		wake:    make(chan struct{}, 1),
	}
	sink.pending.onOverflow = sink.overflows.Inc
	return sink
}

func (s *watchEventSink) TryAccumulate(
	accumulate func(add func(backendEvent) bool) bool,
) bool {
	batch := newPendingWatchBatch(s.pending.maxEntries, s.pending.maxPathBytes)
	batch.onOverflow = s.overflows.Inc
	meaningful := accumulate(batch.AddBackendEvent)
	if batch.Empty() {
		return true
	}
	s.publishHandoff(batch)
	if meaningful {
		s.signal()
	}
	return true
}

func (s *watchEventSink) Add(event backendEvent) {
	s.mu.Lock()
	s.absorbHandoff()
	s.pending.AddBackendEvent(event)
	s.mu.Unlock()
}

func (s *watchEventSink) AddCreatedSubtreePath(path, root string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.absorbHandoff()
	return s.pending.AddCreatedSubtreePath(path, root)
}

// RetainAuthoritative records a lifecycle-bearing full reconciliation through
// the same lock-free bounded handoff used by native callbacks.
func (s *watchEventSink) RetainAuthoritative(token backendLifecycleToken) {
	batch := newPendingWatchBatch(s.pending.maxEntries, s.pending.maxPathBytes)
	batch.AddFullSync()
	batch.AddLifecycle(token)
	s.publishHandoff(batch)
	s.signal()
}

func (s *watchEventSink) Empty() bool {
	if s.handoff.Load() != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending.Empty()
}

func (s *watchEventSink) Take(
	agentsForRoot func(string) []string,
) (WatchBatch, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.absorbHandoff()
	return s.pending.TakeWithRootAgents(agentsForRoot)
}

func (s *watchEventSink) RetainRetry(retry WatchBatch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.absorbHandoff()
	retainWatchRetry(s.pending, retry)
}

func (s *watchEventSink) publishHandoff(batch *pendingWatchBatch) {
	for {
		current := s.handoff.Load()
		if current == nil {
			if s.handoff.CompareAndSwap(nil, batch) {
				return
			}
			continue
		}

		merged := newPendingWatchBatch(current.maxEntries, current.maxPathBytes)
		merged.merge(current)
		merged.merge(batch)
		capacityOverflow := !current.fullSync && !batch.fullSync && merged.fullSync
		if s.handoff.CompareAndSwap(current, merged) {
			if capacityOverflow {
				s.overflows.Inc()
			}
			return
		}
	}
}

// absorbHandoff runs only while mu is held. Native callbacks never acquire mu:
// they publish one independently bounded accumulator and return immediately.
func (s *watchEventSink) absorbHandoff() bool {
	batch := s.handoff.Swap(nil)
	if batch == nil {
		return false
	}
	s.pending.merge(batch)
	return true
}

func (s *watchEventSink) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Watcher schedules backend changes into serialized callbacks with short-burst
// batching and a dispatch floor.
type Watcher struct {
	onChange            WatchCallback
	onCoverageDegraded  func([]string) error
	callbackCtx         context.Context
	callbackCancel      context.CancelFunc
	backend             watchBackend
	eventSink           *watchEventSink
	batchDelay          time.Duration
	minInterval         time.Duration
	maxEntries          int
	dispatchEnabled     atomic.Bool
	dispatchMu          sync.Mutex
	rootAgentsMu        sync.RWMutex
	rootAgents          map[string][]string
	registeredSyncRoots []string
	stopping            bool
	lifecycleMu         sync.Mutex
	lifecycle           watcherLifecycle
	startErr            error
	stop                chan struct{}
	done                chan struct{}
	backendStopOnce     sync.Once
	doneOnce            sync.Once
}

// completeRootPlanBackend owns lifecycle coverage for roots that may not
// exist yet. Portable backends continue using Watcher's per-root fallback.
type completeRootPlanBackend interface {
	RegisterRoots([]WatchRoot, int) []RecursiveWatchResult
}

type watcherLifecycle uint8

const (
	watcherNew watcherLifecycle = iota
	watcherCollecting
	watcherDispatching
	watcherStopped
)

// NewWatcher creates a file watcher that uses delay for both batching and the
// minimum interval between callbacks.
func NewWatcher(delay time.Duration, onChange func(batch WatchBatch), excludes []string) (*Watcher, error) {
	return NewWatcherWithInterval(delay, delay, onChange, excludes)
}

// NewWatcherWithInterval creates a file watcher with separate batching and
// minimum callback intervals.
func NewWatcherWithInterval(
	batchDelay, minInterval time.Duration,
	onChange func(batch WatchBatch), excludes []string,
) (*Watcher, error) {
	return newWatcherWithLimits(
		batchDelay,
		minInterval,
		onChange,
		excludes,
		defaultWatchBatchMaxEntries,
		defaultWatchBatchMaxPathBytes,
	)
}

// NewWatcherWithCallback creates a watcher whose serialized callback reports
// whether an authoritative reconciliation succeeded. Failed reconciliation
// markers remain pending for a later callback attempt.
func NewWatcherWithCallback(
	batchDelay, minInterval time.Duration,
	onChange WatchCallback, excludes []string, options WatcherOptions,
) (*Watcher, error) {
	backend, err := newWatchBackend(excludes)
	if err != nil {
		return nil, err
	}
	return newWatcherWithBackendOptions(
		batchDelay, minInterval, onChange, backend,
		defaultWatchBatchMaxEntries, defaultWatchBatchMaxPathBytes,
		options,
	)
}

func newWatcherWithLimits(
	batchDelay, minInterval time.Duration,
	onChange func(batch WatchBatch), excludes []string,
	maxEntries, maxPathBytes int,
) (*Watcher, error) {
	if onChange == nil {
		return nil, fmt.Errorf("onChange callback is nil: %w", os.ErrInvalid)
	}

	backend, err := newWatchBackend(excludes)
	if err != nil {
		return nil, err
	}
	return newWatcherWithBackend(
		batchDelay,
		minInterval,
		func(_ context.Context, batch WatchBatch) error {
			for _, rename := range batch.Renames {
				if !slices.Contains(batch.Paths, rename.Path) {
					batch.Paths = append(batch.Paths, rename.Path)
				}
			}
			slices.Sort(batch.Paths)
			onChange(batch)
			return nil
		},
		backend,
		maxEntries,
		maxPathBytes,
	)
}

func newWatcherWithBackend(
	batchDelay, minInterval time.Duration,
	onChange WatchCallback, backend watchBackend,
	maxEntries, maxPathBytes int,
) (*Watcher, error) {
	return newWatcherWithBackendOptions(
		batchDelay, minInterval, onChange, backend, maxEntries, maxPathBytes,
		WatcherOptions{},
	)
}

func newWatcherWithBackendOptions(
	batchDelay, minInterval time.Duration,
	onChange WatchCallback, backend watchBackend,
	maxEntries, maxPathBytes int,
	options WatcherOptions,
) (*Watcher, error) {
	if onChange == nil {
		return nil, fmt.Errorf("onChange callback is nil: %w", os.ErrInvalid)
	}
	if backend == nil {
		return nil, fmt.Errorf("watch backend is nil: %w", os.ErrInvalid)
	}

	callbackCtx, callbackCancel := context.WithCancel(context.Background())
	w := &Watcher{
		onChange:           onChange,
		onCoverageDegraded: options.OnCoverageDegraded,
		callbackCtx:        callbackCtx,
		callbackCancel:     callbackCancel,
		backend:            backend,
		eventSink:          newWatchEventSink(maxEntries, maxPathBytes),
		batchDelay:         batchDelay,
		minInterval:        minInterval,
		maxEntries:         maxEntries,
		rootAgents:         make(map[string][]string),
		stop:               make(chan struct{}),
		done:               make(chan struct{}),
	}
	if direct, ok := backend.(directEventBackend); ok {
		direct.bindEventSink(w.eventSink)
	}
	if degraded, ok := backend.(coverageDegradationBackend); ok {
		degraded.bindCoverageDegraded(options.OnCoverageDegraded)
	}
	if ownership, ok := backend.(pollingOwnershipBackend); ok {
		ownership.bindPollingOwnership(
			options.OnPollingRequired, options.OnPollingReleased,
		)
	}
	return w, nil
}

// SetRootAgents records the exact configured agents that own a logical watch
// root. It must be called before Start for roots whose rename events need
// indexed descendant classification.
func (w *Watcher) SetRootAgents(root string, agents []string) {
	w.rootAgentsMu.Lock()
	defer w.rootAgentsMu.Unlock()
	owners := append([]string(nil), agents...)
	slices.Sort(owners)
	owners = slices.Compact(owners)
	w.rootAgents[filepath.Clean(root)] = owners
}

func (w *Watcher) agentsForRoot(root string) []string {
	w.rootAgentsMu.RLock()
	defer w.rootAgentsMu.RUnlock()
	return append([]string(nil), w.rootAgents[filepath.Clean(root)]...)
}

// WatchRecursive walks a directory tree and adds all
// subdirectories to the watch list. Returns the number
// of directories watched and unwatched (failed to add).
func (w *Watcher) WatchRecursive(root string) (watched int, unwatched int, err error) {
	result := w.WatchRecursiveBudgeted(root, math.MaxInt)
	return result.Watched, result.Unwatched, result.Err
}

// WatchRecursiveBudgeted walks a directory tree and adds at most
// budget subdirectories to the watch list. The walk stops as soon
// as the budget is exhausted or fsnotify reports resource
// exhaustion, so the caller can degrade the rest of the tree to
// polling without continuing to traverse it.
func (w *Watcher) WatchRecursiveBudgeted(root string, budget int) RecursiveWatchResult {
	return w.backend.AddRecursive(root, budget)
}

// WatchShallow adds only the root directory to the watch list,
// without recursing into subdirectories. Use this for directories
// with many subdirectories where periodic sync handles changes.
// Returns true if the directory was successfully watched.
func (w *Watcher) WatchShallow(root string) bool {
	return w.backend.AddShallow(root) == nil
}

// Start begins processing file events and dispatching callbacks. Callers that
// need to cover a startup scan use StartCollecting followed by OpenDispatch.
func (w *Watcher) Start() error {
	return w.start(true)
}

// StartCollecting begins draining the native backend into the bounded event
// sink without invoking callbacks.
func (w *Watcher) StartCollecting() error {
	return w.start(false)
}

func (w *Watcher) start(openDispatch bool) error {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if w.lifecycle == watcherCollecting && openDispatch {
		w.lifecycle = watcherDispatching
		w.dispatchEnabled.Store(true)
		w.eventSink.signal()
		return nil
	}
	if w.lifecycle != watcherNew {
		return w.startErr
	}
	if err := w.backend.Start(); err != nil {
		w.lifecycle = watcherStopped
		w.beginStopping()
		w.stopBackend()
		w.finish()
		if w.onCoverageDegraded != nil && len(w.registeredSyncRoots) > 0 {
			err = errors.Join(err, w.onCoverageDegraded(w.registeredSyncRoots))
		}
		w.startErr = fmt.Errorf("starting %s backend: %w", w.backend.Name(), err)
		log.Printf("watcher: %v", w.startErr)
		return w.startErr
	}
	w.lifecycle = watcherCollecting
	go w.loop()
	if openDispatch {
		w.lifecycle = watcherDispatching
		w.dispatchEnabled.Store(true)
	}
	return nil
}

// QueueRetryBatch enqueues a retry batch into the watcher's event sink
// through the same retention path a failed callback uses, preserving the
// batch's LostEvents value: an ordinary queued full sync must not clear
// freshness caches the way a watcher-overflow full sync does. During
// collecting mode the batch dispatches when OpenDispatch fires; afterwards it
// dispatches like any backend event and follows the callback retry machinery
// on failure. The daemon uses it to hand a failed startup gap reconciliation
// to the watcher instead of leaving the affected roots undiscovered until an
// unrelated event, manual sync, or audit.
func (w *Watcher) QueueRetryBatch(batch WatchBatch) {
	if !batch.FullSync && len(batch.ReconcileRoots) == 0 &&
		len(batch.Paths) == 0 {
		return
	}
	w.eventSink.RetainRetry(batch)
	w.eventSink.signal()
}

// OpenDispatch transitions a collecting watcher to callback dispatch. It is
// idempotent and has no effect after Stop.
func (w *Watcher) OpenDispatch() {
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if w.lifecycle != watcherCollecting {
		return
	}
	w.lifecycle = watcherDispatching
	w.dispatchEnabled.Store(true)
	w.eventSink.signal()
}

// Stop stops the watcher and waits for it to finish.
func (w *Watcher) Stop() {
	w.lifecycleMu.Lock()
	if w.lifecycle == watcherStopped {
		done := w.done
		w.lifecycleMu.Unlock()
		<-done
		return
	}
	wasRunning := w.lifecycle == watcherCollecting ||
		w.lifecycle == watcherDispatching
	w.lifecycle = watcherStopped
	w.dispatchEnabled.Store(false)
	w.beginStopping()
	w.stopBackend()
	if !wasRunning {
		w.finish()
	}
	done := w.done
	w.lifecycleMu.Unlock()
	<-done
}

func (w *Watcher) beginStopping() {
	w.dispatchMu.Lock()
	defer w.dispatchMu.Unlock()
	if w.stopping {
		return
	}
	w.stopping = true
	w.callbackCancel()
	close(w.stop)
}

func (w *Watcher) stopBackend() {
	w.backendStopOnce.Do(w.backend.Stop)
}

func (w *Watcher) finish() {
	w.doneOnce.Do(func() { close(w.done) })
}

func (w *Watcher) loop() {
	batches := make(chan WatchBatch)
	type callbackResult struct {
		batch     WatchBatch
		startedAt time.Time
		err       error
	}
	callbackDone := make(chan callbackResult, 1)
	var worker sync.WaitGroup
	worker.Go(func() {
		for batch := range batches {
			if batch.FullSync {
				log.Printf(
					"watcher: authoritative full sync requested (pending overflows=%d)",
					w.eventSink.overflows.Load(),
				)
			} else {
				log.Printf("watcher: %d file(s) changed, triggering sync", len(batch.Paths))
			}
			startedAt := time.Now()
			err := w.onChange(w.callbackCtx, batch)
			callbackDone <- callbackResult{batch: batch, startedAt: startedAt, err: err}
		}
	})

	var firstPendingAt time.Time
	pendingDelay := w.batchDelay
	var lastDispatch time.Time
	consecutiveFailures := 0
	var timer *time.Timer
	var timerC <-chan time.Time
	callbackBusy := false

	stopTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = nil
		timerC = nil
	}
	defer func() {
		stopTimer()
		close(batches)
		worker.Wait()
		w.finish()
	}()

	schedule := func() {
		if !w.dispatchEnabled.Load() || callbackBusy ||
			w.eventSink.Empty() || timerC != nil {
			return
		}
		deadline := firstPendingAt.Add(pendingDelay)
		if !lastDispatch.IsZero() {
			floor := lastDispatch.Add(w.minInterval)
			if floor.After(deadline) {
				deadline = floor
			}
		}
		timer = time.NewTimer(time.Until(deadline))
		timerC = timer.C
	}

	dispatch := func() bool {
		if !w.dispatchEnabled.Load() || callbackBusy || w.eventSink.Empty() {
			return true
		}
		w.dispatchMu.Lock()
		defer w.dispatchMu.Unlock()
		if w.stopping {
			return false
		}
		batch, ok := w.eventSink.Take(w.agentsForRoot)
		if !ok {
			return true
		}
		firstPendingAt = time.Time{}
		pendingDelay = w.batchDelay
		callbackBusy = true
		batches <- batch
		return true
	}

	for {
		// Give shutdown priority over ready event and timer channels. Pending
		// changes are recovered by the next startup sync.
		select {
		case <-w.stop:
			return
		default:
		}
		select {
		case <-w.stop:
			return

		case event, ok := <-w.backend.Events():
			if !ok {
				return
			}
			if event.Op == backendOpUnknown ||
				(event.Path == "" && event.Op&backendOpFullSync == 0) {
				continue
			}
			if w.eventSink.Empty() {
				firstPendingAt = time.Now()
				pendingDelay = w.batchDelay
			}
			w.accumulateBackendEvent(event)
			schedule()

		case <-w.eventSink.wake:
			if w.eventSink.Empty() {
				continue
			}
			if firstPendingAt.IsZero() {
				firstPendingAt = time.Now()
			}
			pendingDelay = 0
			if timerC != nil {
				stopTimer()
			}
			schedule()

		case err, ok := <-w.backend.Errors():
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)

		case <-timerC:
			timer = nil
			timerC = nil
			select {
			case <-w.stop:
				return
			default:
			}
			if !dispatch() {
				return
			}

		case result := <-callbackDone:
			callbackBusy = false
			if result.err != nil {
				lastDispatch = time.Now()
				consecutiveFailures++
				log.Printf("watcher callback: %v", result.err)
				wasEmpty := w.eventSink.Empty()
				retryRetained := false
				if retry, ok := callbackRetryBatch(result.err); ok {
					retry.lifecycleTokens = append(
						[]backendLifecycleToken(nil), result.batch.lifecycleTokens...,
					)
					w.eventSink.RetainRetry(retry)
					retryRetained = true
				} else {
					switch {
					case result.batch.FullSync:
						w.eventSink.RetainRetry(WatchBatch{
							FullSync:        true,
							LostEvents:      result.batch.LostEvents,
							lifecycleTokens: result.batch.lifecycleTokens,
						})
						retryRetained = true
					case renameBatchNeedsFullRetry(result.batch.Renames):
						// Classification did not report its result, so an ambiguous
						// rename must conservatively retain full reconciliation.
						w.eventSink.RetainRetry(WatchBatch{
							FullSync:        true,
							LostEvents:      result.batch.LostEvents,
							lifecycleTokens: result.batch.lifecycleTokens,
						})
						retryRetained = true
					case len(result.batch.ReconcileRoots) > 0:
						w.eventSink.RetainRetry(WatchBatch{
							ReconcileRoots:  result.batch.ReconcileRoots,
							LostEvents:      result.batch.LostEvents,
							lifecycleTokens: result.batch.lifecycleTokens,
						})
						retryRetained = true
					}
				}
				if !retryRetained {
					consecutiveFailures = 0
				}
				if wasEmpty && !w.eventSink.Empty() {
					firstPendingAt = time.Now()
					pendingDelay = watcherRetryDelay(
						max(w.batchDelay, w.minInterval), consecutiveFailures,
					)
				}
			} else {
				lastDispatch = result.startedAt
				consecutiveFailures = 0
				for _, token := range result.batch.lifecycleTokens {
					token.gate.acknowledgeLifecycle(token.generation)
				}
			}
			schedule()
		}
	}
}

func (w *Watcher) accumulateBackendEvent(event backendEvent) {
	filter, enumerate := w.backend.(createdSubtreePathFilter)
	if !enumerate || event.Op&backendOpCreate == 0 ||
		event.Op&(backendOpRemove|backendOpRename) != 0 ||
		event.ItemType != backendItemDirectory ||
		!filter.shouldEnumerateCreatedSubtree(event.Root, event.Path) {
		w.eventSink.Add(event)
		return
	}
	root := event.Root
	if root == "" {
		root = event.Path
	}
	// Preserve the directory-create event itself for behavior parity with the
	// portable backend. Descendant enumeration supplements that event; it does
	// not replace the path that caused the backend to install new watches.
	w.eventSink.Add(event)
	visits := 0
	visitBudgetExceeded := false
	canceled := false
	err := filepath.WalkDir(event.Path, func(
		path string, entry os.DirEntry, walkErr error,
	) error {
		if w.callbackCtx.Err() != nil {
			canceled = true
			return filepath.SkipAll
		}
		if walkErr != nil {
			return walkErr
		}
		visits++
		if visits > w.maxEntries {
			visitBudgetExceeded = true
			return filepath.SkipAll
		}
		if path == event.Path {
			return nil
		}
		if !filter.includeCreatedSubtreePath(root, path) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !w.eventSink.AddCreatedSubtreePath(path, root) {
			return filepath.SkipAll
		}
		return nil
	})
	if canceled || w.callbackCtx.Err() != nil {
		return
	}
	if visitBudgetExceeded || err != nil {
		w.eventSink.Add(backendEvent{
			Path: root, Root: root, Op: backendOpReconcileRootChange,
		})
	}
}

func callbackRetryBatch(err error) (WatchBatch, bool) {
	var retryErr WatchRetryError
	if !errors.As(err, &retryErr) {
		return WatchBatch{}, false
	}
	retry := retryErr.WatchRetryBatch()
	if retry.FullSync {
		return WatchBatch{FullSync: true, LostEvents: retry.LostEvents}, true
	}
	if len(retry.Paths) == 0 && len(retry.ReconcileRoots) == 0 {
		return WatchBatch{}, false
	}
	return WatchBatch{
		Paths:          append([]string(nil), retry.Paths...),
		ReconcileRoots: append([]string(nil), retry.ReconcileRoots...),
		LostEvents:     retry.LostEvents,
	}, true
}

func retainWatchRetry(pending *pendingWatchBatch, retry WatchBatch) {
	if retry.FullSync {
		pending.retainFullSync()
	} else {
		for _, path := range retry.Paths {
			pending.Add(path)
		}
		for _, root := range retry.ReconcileRoots {
			pending.AddReconcileRoot(root)
		}
	}
	pending.lostEvents = pending.lostEvents || retry.LostEvents
	for _, token := range retry.lifecycleTokens {
		pending.AddLifecycle(token)
	}
}

const watcherRetryMaxDelay = 2 * time.Minute

func watcherRetryDelay(base time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if base <= 0 {
		base = time.Millisecond
	}
	delay := base
	for range failures - 1 {
		if delay >= watcherRetryMaxDelay/2 {
			return watcherRetryMaxDelay
		}
		delay *= 2
	}
	return min(delay, watcherRetryMaxDelay)
}

func renameBatchNeedsFullRetry(renames []WatchRename) bool {
	return slices.ContainsFunc(renames, func(rename WatchRename) bool {
		return rename.ItemType != ItemIsFile
	})
}

func watchItemType(itemType backendItemType) WatchItemType {
	switch itemType {
	case backendItemFile:
		return ItemIsFile
	case backendItemDirectory:
		return ItemIsDir
	default:
		return ItemIsUnknown
	}
}
