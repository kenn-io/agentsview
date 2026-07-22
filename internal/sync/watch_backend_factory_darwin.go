//go:build darwin && cgo

package sync

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.kenn.io/agentsview/internal/fsevents"
)

const (
	darwinFSEventsLatency = 500 * time.Millisecond

	fseventFlagMustScanSubDirs fsevents.EventFlags = 0x00000001
	fseventFlagUserDropped     fsevents.EventFlags = 0x00000002
	fseventFlagKernelDropped   fsevents.EventFlags = 0x00000004
	fseventFlagEventIDsWrapped fsevents.EventFlags = 0x00000008
	fseventFlagRootChanged     fsevents.EventFlags = 0x00000020
	fseventFlagItemCreated     fsevents.EventFlags = 0x00000100
	fseventFlagItemRemoved     fsevents.EventFlags = 0x00000200
	fseventFlagItemRenamed     fsevents.EventFlags = 0x00000800
	fseventFlagItemModified    fsevents.EventFlags = 0x00001000
	fseventFlagItemIsFile      fsevents.EventFlags = 0x00010000
	fseventFlagItemIsDir       fsevents.EventFlags = 0x00020000
)

const darwinRootSignalLoss uint32 = 1

const (
	darwinRetryInitial       = 100 * time.Millisecond
	darwinRetryMax           = 5 * time.Second
	darwinFallbackPollingKey = "watcher-fallback"
)

// darwinFallbackPollPlan preserves one physical watch plan's path-to-scope
// mapping for global fallback polling. Each plan becomes its own polling
// obligation with the physical path as Probe: a nested physical root (Gemini's
// <root>/tmp) can be missing while its configured scope <root> exists, and a
// probe on the collapsed scope roots would let authoritative reconciliation
// tombstone every session under the absent subtree.
type darwinFallbackPollPlan struct {
	path  string
	roots []string
}

// darwinFallbackPollingObligationKey names the per-plan fallback obligation so
// each plan can be registered and released independently.
func darwinFallbackPollingObligationKey(path string) string {
	return darwinFallbackPollingKey + ":" + path
}

// appendFallbackPollPlan adds a plan's scope roots under its physical path,
// merging into an existing entry for the same path.
func appendFallbackPollPlan(
	plans []darwinFallbackPollPlan, path string, scopes []WatchScope,
) []darwinFallbackPollPlan {
	roots := appendWatchScopeRoots(nil, scopes)
	if len(roots) == 0 {
		return plans
	}
	for i := range plans {
		if plans[i].path == path {
			merged := plans[i].roots
			for _, root := range roots {
				if !slices.Contains(merged, root) {
					merged = append(merged, root)
				}
			}
			slices.Sort(merged)
			plans[i].roots = merged
			return plans
		}
	}
	plans = append(plans, darwinFallbackPollPlan{path: path, roots: roots})
	slices.SortFunc(plans, func(a, b darwinFallbackPollPlan) int {
		return strings.Compare(a.path, b.path)
	})
	return plans
}

type darwinBackendLifecycle uint8

const (
	darwinBackendNew darwinBackendLifecycle = iota
	darwinBackendRunning
	darwinBackendStopped
)

type darwinWatchRoot struct {
	logicalPath string
	nativePath  string
	recursive   bool
	state       *darwinLogicalRoot
	generation  uint64
}

type darwinRootSnapshot struct {
	roots []darwinWatchRoot
}

type darwinStream interface {
	Start() error
	Close() error
}

type darwinLogicalRoot struct {
	plan              WatchRoot
	active            bool
	ancestor          string
	stream            darwinStream
	backend           *darwinWatchBackend
	signal            atomic.Uint32
	phase             darwinRootPhase
	generation        uint64
	acknowledged      atomic.Uint64
	open              atomic.Bool
	currentGeneration atomic.Uint64
	lossRequested     bool
	retryAt           time.Time
	retryDelay        time.Duration
}

type darwinRootPhase uint8

const (
	darwinRootPending darwinRootPhase = iota
	darwinRootLossCollecting
	darwinRootActivationCollecting
	darwinRootOpen
	darwinRootPolling
)

type darwinFallbackPhase uint32

const (
	darwinFallbackInactive darwinFallbackPhase = iota
	darwinFallbackCollecting
	darwinFallbackReconciling
	darwinFallbackOpen
	darwinFallbackRecovering
)

type darwinFallbackReason uint32

const (
	darwinFallbackReasonNone darwinFallbackReason = iota
	darwinFallbackStreamCreate
	darwinFallbackStreamStart
	darwinFallbackNativeDrop
	darwinFallbackLifecycle
	darwinFallbackKqueueStart
)

type darwinReconcileRequest struct {
	scopes []WatchScope
	token  backendLifecycleToken
}

func (s *darwinLogicalRoot) acknowledgeLifecycle(generation uint64) {
	for {
		current := s.acknowledged.Load()
		if current >= generation || s.acknowledged.CompareAndSwap(current, generation) {
			break
		}
	}
	s.backend.signalLifecycle()
}

func (s *darwinLogicalRoot) isOpen(generation uint64) bool {
	return s.open.Load() && generation == s.currentGeneration.Load()
}

type darwinWatchBackend struct {
	kqueue   *fsnotifyBackend
	queue    *fsevents.Queue
	latency  time.Duration
	excludes []string

	events        chan backendEvent
	errors        chan error
	stop          chan struct{}
	done          chan struct{}
	lifecycleWake chan struct{}
	lifecycleDone chan struct{}

	sink               directBackendEventSink
	onCoverageDegraded func([]string) error
	onPollingRequired  func(PollingObligation) error
	onPollingReleased  func(string) error

	mu        sync.Mutex
	lifecycle darwinBackendLifecycle
	streams   map[string]darwinStream
	logical   map[string]*darwinLogicalRoot
	shallow   map[string]int
	ancestors map[string]int
	roots     atomic.Pointer[darwinRootSnapshot]

	newStream              func(string, func([]fsevents.Event)) (darwinStream, error)
	pathIsDirectory        func(string) bool
	lstat                  func(string) (os.FileInfo, error)
	stat                   func(string) (os.FileInfo, error)
	addShallow             func(string) error
	removeShallow          func(string) error
	addAncestor            func(string) error
	removeAncestor         func(string) error
	vnode                  *vnodeObserver
	startKqueue            func() error
	transitions            sync.WaitGroup
	workerStarted          bool
	forwardStarted         bool
	retryInitial           time.Duration
	retryMax               time.Duration
	fallbackPrepared       bool
	fallbackPollPlans      []darwinFallbackPollPlan
	fallbackRetryAt        time.Time
	fallbackRetryDelay     time.Duration
	fallbackDegradedLogged bool
	fallbackPollingOwned   bool
	fallbackPollingKeyed   bool
	fallbackGeneration     uint64
	fallbackAcknowledged   atomic.Uint64
	fallbackReason         atomic.Uint32
	fallbackPhase          atomic.Uint32
	fallbackRecoveryFailed atomic.Bool
	nativeOpen             atomic.Bool
	fallbackEventsMu       sync.Mutex
	fallbackEvents         map[backendEvent]struct{}
	fallbackEventBytes     int
	fallbackEventsFull     bool
	nativeDrops            boundedCounter
	pendingBatchOverflows  boundedCounter
	rootTransitions        boundedCounter
	fallbackActivations    boundedCounter

	stopOnce   sync.Once
	finishOnce sync.Once
}

// The created-subtree contract enables bounded discovery for directory-create
// events forwarded by the kqueue fallback. Native FSEvents deliveries bypass
// Watcher.loop through the direct sink, so this method never puts filesystem
// work on the FSEvents callback queue.
func (b *darwinWatchBackend) includeCreatedSubtreePath(root, path string) bool {
	return !shouldExcludeForRoot(b.excludes, path, root)
}

func (b *darwinWatchBackend) shouldEnumerateCreatedSubtree(root, path string) bool {
	snapshot := b.roots.Load()
	owner, ok := snapshot.mostSpecificRoot(path)
	return ok && owner.recursive && owner.logicalPath == filepath.Clean(root)
}

func newWatchBackend(excludes []string) (watchBackend, error) {
	return newDarwinWatchBackend(excludes, darwinFSEventsLatency)
}

func newDarwinWatchBackend(
	excludes []string,
	latency time.Duration,
) (*darwinWatchBackend, error) {
	kqueue, err := newFSNotifyBackend(excludes)
	if err != nil {
		return nil, err
	}
	queue, err := fsevents.NewQueue()
	if err != nil {
		kqueue.Stop()
		return nil, err
	}
	backend := &darwinWatchBackend{
		kqueue:        kqueue,
		queue:         queue,
		latency:       latency,
		excludes:      normalizeExcludePatterns(excludes),
		events:        make(chan backendEvent),
		errors:        make(chan error),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		lifecycleWake: make(chan struct{}, 1),
		lifecycleDone: make(chan struct{}),
		streams:       make(map[string]darwinStream),
		logical:       make(map[string]*darwinLogicalRoot),
		shallow:       make(map[string]int),
		retryInitial:  darwinRetryInitial,
		retryMax:      darwinRetryMax,
	}
	backend.newStream = func(root string, sink func([]fsevents.Event)) (darwinStream, error) {
		return fsevents.NewStream(backend.queue, root, backend.latency, sink)
	}
	backend.lstat = os.Lstat
	backend.stat = os.Stat
	backend.pathIsDirectory = pathIsDirectory
	backend.addShallow = backend.kqueue.AddShallow
	backend.removeShallow = backend.kqueue.Remove
	backend.ancestors = make(map[string]int)
	backend.addAncestor = backend.observeAncestor
	backend.removeAncestor = backend.unobserveAncestor
	backend.startKqueue = backend.kqueue.Start
	backend.fallbackEvents = make(map[backendEvent]struct{})
	backend.nativeOpen.Store(true)
	backend.roots.Store(&darwinRootSnapshot{})
	return backend, nil
}

func (b *darwinWatchBackend) Events() <-chan backendEvent { return b.events }
func (b *darwinWatchBackend) Errors() <-chan error        { return b.errors }

func (b *darwinWatchBackend) bindEventSink(sink directBackendEventSink) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sink = sink
}

func (b *darwinWatchBackend) bindCoverageDegraded(callback func([]string) error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onCoverageDegraded = callback
}

func (b *darwinWatchBackend) bindPollingOwnership(
	required func(PollingObligation) error,
	released func(string) error,
) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onPollingRequired = required
	b.onPollingReleased = released
}

func (b *darwinWatchBackend) fallbackPhaseValue() darwinFallbackPhase {
	return darwinFallbackPhase(b.fallbackPhase.Load())
}

// RegisterRoots installs the complete provider-root plan. Missing logical
// roots retain shallow coverage on only their nearest existing ancestor.
func (b *darwinWatchBackend) RegisterRoots(
	roots []WatchRoot,
	recursiveBudget int,
) []RecursiveWatchResult {
	results := make([]RecursiveWatchResult, len(roots))
	b.mu.Lock()
	defer b.mu.Unlock()
	isDirectory := b.pathIsDirectory
	if isDirectory == nil {
		isDirectory = pathIsDirectory
	}
	streamCreationFailed := false
	for i, plan := range roots {
		plan.Path = filepath.Clean(plan.Path)
		plan.Scopes = append([]WatchScope(nil), plan.Scopes...)
		if b.lifecycle == darwinBackendStopped {
			results[i] = RecursiveWatchResult{
				Unwatched: 1, Err: errors.New("darwin watch backend is stopped"),
			}
			continue
		}
		if _, exists := b.logical[plan.Path]; exists {
			results[i].Watched = 1
			continue
		}
		state := &darwinLogicalRoot{plan: plan, backend: b}
		b.logical[plan.Path] = state
		if isDirectory(plan.Path) {
			if streamCreationFailed && plan.Recursive {
				continue
			}
			if err := b.activateRootLocked(state); err != nil {
				if plan.Recursive {
					streamCreationFailed = true
					b.requestFallback(darwinFallbackStreamCreate)
					continue
				}
				delete(b.logical, plan.Path)
				results[i] = RecursiveWatchResult{Unwatched: 1, Err: err}
				continue
			}
			results[i] = RecursiveWatchResult{
				Watched:                   1,
				MissingRootLifecycleOwned: !plan.Exists,
			}
			continue
		}
		ancestor := nearestExistingAncestor(plan.Path)
		if ancestor == "" {
			delete(b.logical, plan.Path)
			results[i] = RecursiveWatchResult{
				Unwatched: 1, Err: fmt.Errorf("no existing ancestor for %q", plan.Path),
			}
			continue
		}
		if err := b.acquireAncestorLocked(ancestor); err != nil {
			delete(b.logical, plan.Path)
			results[i] = RecursiveWatchResult{Unwatched: 1, Err: err}
			continue
		}
		state.ancestor = ancestor
		results[i] = RecursiveWatchResult{
			Watched:                   1,
			MissingRootLifecycleOwned: true,
		}
	}
	if streamCreationFailed {
		b.prepareStartupFallbackLocked(roots, results)
	}
	return results
}

func (b *darwinWatchBackend) prepareStartupFallbackLocked(
	roots []WatchRoot,
	results []RecursiveWatchResult,
) {
	for root, stream := range b.streams {
		_ = stream.Close()
		delete(b.streams, root)
		b.removeRootLocked(root, true)
	}
	var pollPlans []darwinFallbackPollPlan
	for i, plan := range roots {
		plan.Path = filepath.Clean(plan.Path)
		if !plan.Recursive {
			continue
		}
		pollPlans = appendFallbackPollPlan(pollPlans, plan.Path, plan.Scopes)
		results[i] = RecursiveWatchResult{Watched: 1}
		state := b.logical[plan.Path]
		if state != nil {
			state.active = false
			state.stream = nil
			state.open.Store(false)
		}
	}
	b.fallbackPrepared = true
	b.fallbackPollPlans = pollPlans
	b.fallbackPhase.Store(uint32(darwinFallbackCollecting))
	b.nativeOpen.Store(false)
}

func (b *darwinWatchBackend) AddRecursive(root string, _ int) RecursiveWatchResult {
	root = filepath.Clean(root)
	nativeRoot := canonicalWatchRoot(root)

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lifecycle == darwinBackendStopped {
		return RecursiveWatchResult{Unwatched: 1, Err: errors.New("darwin watch backend is stopped")}
	}
	if _, exists := b.streams[root]; exists {
		return RecursiveWatchResult{Watched: 1}
	}
	registration := darwinWatchRoot{
		logicalPath: root,
		nativePath:  nativeRoot,
		recursive:   true,
	}
	stream, err := b.newStream(root, func(events []fsevents.Event) {
		b.consumeFSEvents(registration, events)
	})
	if err != nil {
		return RecursiveWatchResult{Unwatched: 1, Err: err}
	}
	b.streams[root] = stream
	b.storeRootLocked(registration)
	if b.lifecycle == darwinBackendRunning {
		if err := stream.Start(); err != nil {
			delete(b.streams, root)
			b.removeRootLocked(root, true)
			_ = stream.Close()
			return RecursiveWatchResult{Unwatched: 1, Err: err}
		}
	}
	return RecursiveWatchResult{Watched: 1}
}

func (b *darwinWatchBackend) AddShallow(root string) error {
	root = filepath.Clean(root)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lifecycle == darwinBackendStopped {
		return errors.New("darwin watch backend is stopped")
	}
	if err := b.acquireShallowLocked(root); err != nil {
		return err
	}
	b.storeRootLocked(darwinWatchRoot{
		logicalPath: root,
		nativePath:  canonicalWatchRoot(root),
	})
	return nil
}

func (b *darwinWatchBackend) Remove(root string) error {
	root = filepath.Clean(root)
	b.mu.Lock()
	stream := b.streams[root]
	if stream != nil {
		delete(b.streams, root)
		b.removeRootLocked(root, true)
		b.mu.Unlock()
		return stream.Close()
	}
	b.removeRootLocked(root, false)
	err := b.releaseShallowLocked(root)
	b.mu.Unlock()
	return err
}

func (b *darwinWatchBackend) Start() error {
	b.mu.Lock()
	if b.lifecycle == darwinBackendRunning {
		b.mu.Unlock()
		return nil
	}
	if b.lifecycle == darwinBackendStopped {
		b.mu.Unlock()
		return errors.New("darwin watch backend is stopped")
	}
	if b.sink == nil {
		b.mu.Unlock()
		return errors.New("darwin watch backend has no event sink")
	}
	b.lifecycle = darwinBackendRunning
	b.workerStarted = true
	go b.lifecycleLoop()
	kqueueErr := b.startKqueue()
	if kqueueErr == nil {
		b.forwardStarted = true
		go b.forwardKqueue()
	} else {
		if b.fallbackPrepared {
			for _, state := range b.logical {
				b.fallbackPollPlans = appendFallbackPollPlan(
					b.fallbackPollPlans, state.plan.Path, state.plan.Scopes,
				)
			}
		}
		previous := darwinFallbackReason(b.fallbackReason.Swap(
			uint32(darwinFallbackKqueueStart),
		))
		if previous == darwinFallbackReasonNone {
			b.fallbackActivations.Inc()
		}
		b.signalLifecycle()
	}
	if b.fallbackPhaseValue() == darwinFallbackInactive {
		b.nativeOpen.Store(true)
	}
	for _, stream := range b.streams {
		if err := stream.Start(); err != nil {
			b.requestFallback(darwinFallbackStreamStart)
			break
		}
	}
	b.mu.Unlock()
	b.signalLifecycle()
	return nil
}

func (b *darwinWatchBackend) Stop() {
	b.stopOnce.Do(func() {
		b.mu.Lock()
		wasRunning := b.lifecycle == darwinBackendRunning
		workerStarted := b.workerStarted
		forwardStarted := b.forwardStarted
		b.lifecycle = darwinBackendStopped
		streams := make([]darwinStream, 0, len(b.streams))
		for _, stream := range b.streams {
			streams = append(streams, stream)
		}
		clear(b.streams)
		close(b.stop)
		b.mu.Unlock()

		for _, stream := range streams {
			_ = stream.Close()
		}
		b.kqueue.Stop()
		if b.vnode != nil {
			_ = b.vnode.Close()
		}
		b.transitions.Wait()
		if workerStarted {
			<-b.lifecycleDone
		}
		if !wasRunning || !forwardStarted {
			b.finish()
		}
	})
	<-b.done
}

func (b *darwinWatchBackend) Name() string { return "fsevents+kqueue" }

func (b *darwinWatchBackend) consumeFSEvents(
	registration darwinWatchRoot,
	events []fsevents.Event,
) {
	if nativeDeliveryRequestsFallback(events) {
		b.nativeDrops.Inc()
		b.requestFallback(darwinFallbackNativeDrop)
		return
	}
	if !b.nativeOpen.Load() && b.fallbackPhaseValue() != darwinFallbackInactive {
		return
	}
	if registration.state != nil && nativeDeliveryReportsRootLoss(registration, events) {
		if b.fallbackPhaseValue() == darwinFallbackRecovering {
			b.requestFallback(darwinFallbackLifecycle)
			return
		}
		registration.state.signal.Or(darwinRootSignalLoss)
		b.signalLifecycle()
		return
	}
	sink := b.sink
	if sink == nil {
		return
	}
	if registration.state != nil &&
		!registration.state.isOpen(registration.generation) {
		b.accumulateReconcile(registration.state.plan.Scopes, backendLifecycleToken{
			gate: registration.state, generation: registration.generation,
		})
		return
	}
	sink.TryAccumulate(func(add func(backendEvent) bool) bool {
		meaningful := false
		snapshot := b.roots.Load()
		for _, event := range events {
			event.Path = logicalFSEventPath(registration, event.Path)
			translated, relevant := translateFSEvent(registration.logicalPath, event)
			if !relevant {
				continue
			}
			owner, ok := snapshot.mostSpecificRecursiveRoot(translated.Path)
			if !ok || !owner.recursive || owner.logicalPath != registration.logicalPath {
				continue
			}
			if translated.Op&backendOpReconcileRootChange == 0 &&
				shouldExcludeForRoot(b.excludes, translated.Path, owner.logicalPath) {
				continue
			}
			// FSEvents accompanies descendant changes with an ordinary modified
			// event for the watched directory itself. The descendant file event is
			// the changed-path work; retaining the root event would defeat subtree
			// exclusions and schedule redundant root scans. Authoritative
			// RootChanged events already map to reconciliation and remain intact.
			if translated.Path == owner.logicalPath &&
				translated.Op&backendOpReconcileRootChange == 0 {
				continue
			}
			translated.Root = owner.logicalPath
			meaningful = true
			if !add(translated) {
				return true
			}
		}
		return meaningful
	})
}

func nativeDeliveryRequestsFallback(events []fsevents.Event) bool {
	for _, event := range events {
		if event.Flags&(fseventFlagMustScanSubDirs|
			fseventFlagUserDropped|
			fseventFlagKernelDropped|
			fseventFlagEventIDsWrapped) != 0 {
			return true
		}
	}
	return false
}

// translateFSEvent maps a native FSEvents delivery to a backend event. Drop
// and rescan flags never reach translation: consumeFSEvents diverts them to
// fallback via nativeDeliveryRequestsFallback before translating.
func translateFSEvent(root string, event fsevents.Event) (backendEvent, bool) {
	flags := event.Flags
	itemType := backendItemUnknown
	switch {
	case flags&fseventFlagItemIsFile != 0:
		itemType = backendItemFile
	case flags&fseventFlagItemIsDir != 0:
		itemType = backendItemDirectory
	}
	if flags&fseventFlagRootChanged != 0 {
		return backendEvent{
			Path:     filepath.Clean(event.Path),
			Root:     filepath.Clean(root),
			Op:       backendOpReconcileRootChange,
			ItemType: itemType,
		}, true
	}

	var op backendOp
	if flags&fseventFlagItemCreated != 0 {
		op |= backendOpCreate
	}
	if flags&fseventFlagItemModified != 0 {
		op |= backendOpWrite
	}
	if flags&fseventFlagItemRemoved != 0 {
		op |= backendOpRemove
	}
	if flags&fseventFlagItemRenamed != 0 {
		op |= backendOpRename
	}
	if op == backendOpUnknown {
		return backendEvent{}, false
	}
	return backendEvent{
		Path:     filepath.Clean(event.Path),
		Root:     filepath.Clean(root),
		Op:       op,
		ItemType: itemType,
	}, true
}

func (b *darwinWatchBackend) forwardKqueue() {
	defer b.finish()
	events := b.kqueue.Events()
	errorsCh := b.kqueue.Errors()
	for events != nil || errorsCh != nil {
		select {
		case <-b.stop:
			return
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			event, dispatch := b.handleKqueueEvent(event)
			if dispatch {
				select {
				case b.events <- event:
				case <-b.stop:
					return
				}
			}
		case err, ok := <-errorsCh:
			if !ok {
				errorsCh = nil
				continue
			}
			select {
			case b.errors <- err:
			case <-b.stop:
				return
			}
		}
	}
}

func nativeDeliveryReportsRootLoss(
	registration darwinWatchRoot,
	events []fsevents.Event,
) bool {
	for _, event := range events {
		if event.Flags&fseventFlagRootChanged != 0 {
			return true
		}
		path := logicalFSEventPath(registration, event.Path)
		if filepath.Clean(path) == registration.logicalPath &&
			event.Flags&(fseventFlagItemRemoved|fseventFlagItemRenamed) != 0 {
			return true
		}
	}
	return false
}

func (b *darwinWatchBackend) signalLifecycle() {
	select {
	case b.lifecycleWake <- struct{}{}:
	default:
	}
}

func (b *darwinWatchBackend) requestFallback(reason darwinFallbackReason) {
	if reason == darwinFallbackReasonNone {
		return
	}
	if b.fallbackPhaseValue() == darwinFallbackRecovering {
		b.fallbackRecoveryFailed.Store(true)
		b.signalLifecycle()
		return
	}
	if b.fallbackReason.CompareAndSwap(
		uint32(darwinFallbackReasonNone), uint32(reason),
	) {
		b.fallbackActivations.Inc()
		b.signalLifecycle()
	}
}

func (b *darwinWatchBackend) acknowledgeLifecycle(generation uint64) {
	for {
		current := b.fallbackAcknowledged.Load()
		if current >= generation ||
			b.fallbackAcknowledged.CompareAndSwap(current, generation) {
			break
		}
	}
	b.signalLifecycle()
}

func (b *darwinWatchBackend) lifecycleLoop() {
	defer close(b.lifecycleDone)
	for {
		delay, retry := b.nextLifecycleRetry()
		var timer *time.Timer
		var timerC <-chan time.Time
		if retry {
			timer = time.NewTimer(delay)
			timerC = timer.C
		}
		select {
		case <-b.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-b.lifecycleWake:
			if timer != nil {
				timer.Stop()
			}
			b.processLifecycleSignals()
		case <-timerC:
			b.processLifecycleRetry()
		}
	}
}

func (b *darwinWatchBackend) processLifecycleSignals() {
	b.processLifecycle(true)
}

func (b *darwinWatchBackend) processLifecycleRetry() {
	b.processLifecycle(false)
}

func (b *darwinWatchBackend) processLifecycle(checkPending bool) {
	if b.processFallbackTransition() {
		return
	}
	var requests []darwinReconcileRequest
	var releasedPolling []string
	now := time.Now()
	b.mu.Lock()
	if b.lifecycle == darwinBackendStopped {
		b.mu.Unlock()
		return
	}
	for _, state := range b.logical {
		lossSignaled := state.signal.Swap(0)&darwinRootSignalLoss != 0
		if lossSignaled {
			state.lossRequested = true
		}
		if state.lossRequested && state.active &&
			(lossSignaled || b.retryDueLocked(state, now)) {
			ancestor := nearestExistingAncestor(filepath.Dir(state.plan.Path))
			if ancestor == "" {
				requests = append(requests, darwinReconcileRequest{scopes: state.plan.Scopes})
				b.scheduleRetryLocked(state, now)
				continue
			}
			if err := b.acquireAncestorLocked(ancestor); err != nil {
				b.reportErrorLocked(fmt.Errorf("cover lost watch root: %w", err))
				b.requestFallback(darwinFallbackLifecycle)
				requests = append(requests, darwinReconcileRequest{scopes: state.plan.Scopes})
				b.scheduleRetryLocked(state, now)
				continue
			}
			if err := b.installRootPollingLocked(state); err != nil {
				b.reportErrorLocked(fmt.Errorf(
					"install polling coverage for lost root %s: %w",
					state.plan.Path, err,
				))
				b.requestFallback(darwinFallbackLifecycle)
				requests = append(requests, darwinReconcileRequest{scopes: state.plan.Scopes})
				b.scheduleRetryLocked(state, now)
				continue
			}
			oldAncestor := state.ancestor
			stream := state.stream
			state.active = false
			state.ancestor = ancestor
			state.stream = nil
			state.lossRequested = false
			state.generation++
			state.currentGeneration.Store(state.generation)
			state.open.Store(false)
			state.phase = darwinRootLossCollecting
			b.rootTransitions.Inc()
			if state.plan.Recursive {
				delete(b.streams, state.plan.Path)
				b.removeRootLocked(state.plan.Path, true)
			} else {
				b.removeRootLocked(state.plan.Path, false)
				if oldAncestor != "" {
					_ = b.releaseShallowLocked(oldAncestor)
				}
			}
			if stream != nil {
				_ = stream.Close()
			}
			token := backendLifecycleToken{gate: state, generation: state.generation}
			requests = append(requests, darwinReconcileRequest{
				scopes: state.plan.Scopes, token: token,
			})
			b.scheduleRetryLocked(state, now)
			continue
		}

		if state.phase == darwinRootLossCollecting {
			if state.acknowledged.Load() >= state.generation {
				state.phase = darwinRootPending
				b.resetRetryLocked(state)
			} else if b.retryDueLocked(state, now) {
				requests = append(requests, darwinReconcileRequest{
					scopes: state.plan.Scopes,
					token:  backendLifecycleToken{gate: state, generation: state.generation},
				})
				b.scheduleRetryLocked(state, now)
			}
			if state.phase == darwinRootLossCollecting {
				continue
			}
		}
		if state.phase == darwinRootActivationCollecting &&
			state.acknowledged.Load() >= state.generation {
			state.phase = darwinRootOpen
			b.rootTransitions.Inc()
			state.open.Store(true)
			b.resetRetryLocked(state)
			releasedPolling = append(releasedPolling, state.plan.Path)
		}
		if state.phase == darwinRootActivationCollecting {
			if b.retryDueLocked(state, now) {
				requests = append(requests, darwinReconcileRequest{
					scopes: state.plan.Scopes,
					token:  backendLifecycleToken{gate: state, generation: state.generation},
				})
				b.scheduleRetryLocked(state, now)
			}
			continue
		}
		if state.phase != darwinRootPending ||
			(!checkPending && !b.retryDueLocked(state, now)) {
			continue
		}

		info, lstatErr := b.lstat(state.plan.Path)
		if lstatErr == nil && state.plan.Recursive && info.Mode()&os.ModeSymlink != 0 {
			if b.requireRootPollingLocked(state, now) {
				requests = append(requests, darwinReconcileRequest{scopes: state.plan.Scopes})
			}
			continue
		}
		ancestor := nearestExistingAncestor(state.plan.Path)
		if ancestor == "" ||
			(ancestor == state.ancestor && ancestor != state.plan.Path) {
			b.schedulePendingRetryLocked(state, now)
			continue
		}
		old := state.ancestor
		if ancestor == state.plan.Path {
			linkInfo, linkErr := b.lstat(state.plan.Path)
			if linkErr == nil && state.plan.Recursive &&
				linkInfo.Mode()&os.ModeSymlink != 0 {
				if b.requireRootPollingLocked(state, now) {
					requests = append(requests, darwinReconcileRequest{scopes: state.plan.Scopes})
				}
				continue
			}
			info, statErr := b.stat(state.plan.Path)
			if statErr != nil || !info.IsDir() {
				b.schedulePendingRetryLocked(state, now)
				continue
			}
			if err := b.activateRootLocked(state); err != nil {
				b.reportErrorLocked(fmt.Errorf("activate watch root: %w", err))
				b.requestFallback(darwinFallbackLifecycle)
				requests = append(requests, darwinReconcileRequest{scopes: state.plan.Scopes})
				b.scheduleRetryLocked(state, now)
				continue
			}
			requests = append(requests, darwinReconcileRequest{
				scopes: state.plan.Scopes,
				token:  backendLifecycleToken{gate: state, generation: state.generation},
			})
			b.scheduleRetryLocked(state, now)
		} else {
			if err := b.acquireAncestorLocked(ancestor); err != nil {
				b.reportErrorLocked(fmt.Errorf("advance watch root: %w", err))
				b.requestFallback(darwinFallbackLifecycle)
				requests = append(requests, darwinReconcileRequest{scopes: state.plan.Scopes})
				b.scheduleRetryLocked(state, now)
				continue
			}
			state.ancestor = ancestor
			requests = append(requests, darwinReconcileRequest{scopes: state.plan.Scopes})
			b.schedulePendingRetryLocked(state, now)
		}
		if old != "" {
			_ = b.releaseAncestorLocked(old)
		}
	}
	releaseCallback := b.onPollingReleased
	b.mu.Unlock()
	if releaseCallback != nil {
		for _, key := range releasedPolling {
			if err := releaseCallback(key); err != nil {
				log.Printf("watcher: release polling obligation %q: %v", key, err)
			}
		}
	}
	for _, request := range requests {
		b.accumulateReconcile(request.scopes, request.token)
	}
}

func (b *darwinWatchBackend) requireRootPollingLocked(
	state *darwinLogicalRoot,
	now time.Time,
) bool {
	if err := b.installRootPollingLocked(state); err != nil {
		b.reportErrorLocked(fmt.Errorf("install polling coverage for %s: %w", state.plan.Path, err))
		b.schedulePendingRetryLocked(state, now)
		return false
	}
	state.phase = darwinRootPolling
	b.resetRetryLocked(state)
	return true
}

func (b *darwinWatchBackend) installRootPollingLocked(
	state *darwinLogicalRoot,
) error {
	roots := appendWatchScopeRoots(nil, state.plan.Scopes)
	if b.onPollingRequired != nil {
		return b.onPollingRequired(PollingObligation{
			Key: state.plan.Path, Roots: roots, Probe: state.plan.Path,
		})
	}
	if b.onCoverageDegraded != nil {
		return b.onCoverageDegraded(roots)
	}
	return nil
}

func (b *darwinWatchBackend) processFallbackTransition() bool {
	phase := b.fallbackPhaseValue()
	reason := darwinFallbackReason(b.fallbackReason.Load())
	if phase == darwinFallbackInactive && reason == darwinFallbackReasonNone {
		return false
	}
	if phase == darwinFallbackRecovering {
		b.processNativeRecoveryHandoff()
		return true
	}
	if phase == darwinFallbackOpen {
		b.processNativeRecoveryRetry()
		return b.fallbackPhaseValue() != darwinFallbackOpen
	}
	if phase == darwinFallbackReconciling {
		b.mu.Lock()
		generation := b.fallbackGeneration
		b.mu.Unlock()
		if b.fallbackAcknowledged.Load() >= generation {
			b.openFallbackDispatch()
		}
		return true
	}

	now := time.Now()
	b.mu.Lock()
	if !b.fallbackRetryAt.IsZero() && now.Before(b.fallbackRetryAt) {
		b.mu.Unlock()
		return true
	}
	prepared := b.fallbackPrepared
	b.mu.Unlock()
	if !prepared {
		b.prepareRuntimeFallback()
	}

	b.mu.Lock()
	pollPlans := append([]darwinFallbackPollPlan(nil), b.fallbackPollPlans...)
	pollingOwned := b.fallbackPollingOwned
	b.mu.Unlock()
	if len(pollPlans) > 0 {
		var err error
		if !pollingOwned {
			err = b.requireFallbackPolling(pollPlans)
		}
		if err != nil {
			b.mu.Lock()
			if !b.fallbackDegradedLogged {
				log.Printf(
					"watcher: backend=%s fallback=%s polling registration failed root_count=%d",
					b.Name(), fallbackReasonLabel(reason), len(pollPlans),
				)
				b.fallbackDegradedLogged = true
			}
			b.scheduleFallbackRetryLocked(now)
			b.mu.Unlock()
			return true
		}
		b.mu.Lock()
		b.fallbackPollingOwned = true
		b.mu.Unlock()
	}

	b.beginFallbackReconciliation()
	return true
}

func (b *darwinWatchBackend) prepareRuntimeFallback() {
	b.mu.Lock()
	if b.fallbackPrepared {
		b.mu.Unlock()
		return
	}
	b.fallbackPhase.Store(uint32(darwinFallbackCollecting))
	b.rootTransitions.Inc()
	var pollPlans []darwinFallbackPollPlan
	for _, state := range b.logical {
		pollPlans = appendFallbackPollPlan(
			pollPlans, state.plan.Path, state.plan.Scopes,
		)
	}
	b.fallbackPollPlans = pollPlans
	b.fallbackPrepared = true
	b.mu.Unlock()
}

func (b *darwinWatchBackend) beginFallbackReconciliation() {
	b.fallbackEventsMu.Lock()
	clear(b.fallbackEvents)
	b.fallbackEventBytes = 0
	b.fallbackEventsFull = false
	b.fallbackPhase.Store(uint32(darwinFallbackReconciling))
	b.fallbackEventsMu.Unlock()
	b.rootTransitions.Inc()
	b.nativeOpen.Store(false)

	b.mu.Lock()
	streams := make([]darwinStream, 0, len(b.streams))
	for root, stream := range b.streams {
		streams = append(streams, stream)
		delete(b.streams, root)
		b.removeRootLocked(root, true)
	}
	for _, state := range b.logical {
		state.stream = nil
		state.open.Store(false)
		if state.plan.Recursive {
			state.active = false
		}
	}
	b.fallbackGeneration++
	generation := b.fallbackGeneration
	b.fallbackRetryAt = time.Time{}
	b.fallbackRetryDelay = 0
	b.mu.Unlock()
	for _, stream := range streams {
		_ = stream.Close()
	}

	sink := b.sink
	if sink != nil {
		sink.RetainAuthoritative(backendLifecycleToken{
			gate: b, generation: generation,
		})
	}
}

func (b *darwinWatchBackend) openFallbackDispatch() {
	// Kqueue startup can fail after an earlier stream failure selected fallback.
	// Refresh the reason so that unrecoverable kqueue failure dominates the
	// retry policy even if the lifecycle worker observed the earlier reason.
	reason := darwinFallbackReason(b.fallbackReason.Load())
	b.fallbackEventsMu.Lock()
	sink := b.sink
	if sink != nil && (b.fallbackEventsFull || len(b.fallbackEvents) > 0) {
		sink.TryAccumulate(func(add func(backendEvent) bool) bool {
			if b.fallbackEventsFull {
				add(backendEvent{Op: backendOpFullSync})
				return true
			}
			for event := range b.fallbackEvents {
				if !add(event) {
					break
				}
			}
			return true
		})
	}
	clear(b.fallbackEvents)
	b.fallbackEventBytes = 0
	b.fallbackEventsFull = false
	b.fallbackPhase.Store(uint32(darwinFallbackOpen))
	b.fallbackEventsMu.Unlock()
	b.rootTransitions.Inc()

	b.mu.Lock()
	if reason == darwinFallbackKqueueStart {
		b.fallbackRetryAt = time.Time{}
		b.fallbackRetryDelay = 0
	} else {
		b.scheduleFallbackRetryLocked(time.Now())
	}
	pollRoots := len(b.fallbackPollPlans)
	b.mu.Unlock()
	log.Printf(
		"watcher: backend=%s fallback=%s reconcile=full polling_roots=%d native_drops=%d pending_overflows=%d root_transitions=%d fallback_activations=%d",
		b.Name(), fallbackReasonLabel(reason), pollRoots,
		b.nativeDrops.Load(), b.pendingBatchOverflows.Load(),
		b.rootTransitions.Load(), b.fallbackActivations.Load(),
	)
}

// requireFallbackPolling registers one polling obligation per physical watch
// plan, with the plan's path as Probe, so a missing physical subtree defers
// its scope roots instead of letting authoritative reconciliation tombstone
// the sessions under it. Registration is idempotent per key, so a partial
// failure is safe to retry with the full plan set.
func (b *darwinWatchBackend) requireFallbackPolling(
	plans []darwinFallbackPollPlan,
) error {
	b.mu.Lock()
	required := b.onPollingRequired
	degraded := b.onCoverageDegraded
	b.mu.Unlock()
	if required != nil {
		for _, plan := range plans {
			if err := required(PollingObligation{
				Key:   darwinFallbackPollingObligationKey(plan.path),
				Roots: plan.roots,
				Probe: plan.path,
			}); err != nil {
				return err
			}
		}
		b.mu.Lock()
		b.fallbackPollingKeyed = true
		b.mu.Unlock()
		return nil
	}
	if degraded != nil {
		// The degraded callback carries no probe and its only darwin consumers
		// (the PG push loops) widen non-authoritative push polling; the
		// daemon's authoritative reconciliation consumer always registers
		// keyed obligations above. Handing over every scope root here keeps
		// that coverage, and stat-gating on a one-shot registration would
		// silently drop a scope forever if its physical path is briefly
		// absent.
		var roots []string
		for _, plan := range plans {
			for _, root := range plan.roots {
				if !slices.Contains(roots, root) {
					roots = append(roots, root)
				}
			}
		}
		slices.Sort(roots)
		return degraded(roots)
	}
	return errors.New("no degraded coverage owner")
}

type darwinRecoveryStream struct {
	state        *darwinLogicalRoot
	registration darwinWatchRoot
	stream       darwinStream
	started      bool
}

func (b *darwinWatchBackend) processNativeRecoveryRetry() {
	now := time.Now()
	b.mu.Lock()
	if !b.fallbackRetryAt.IsZero() && now.Before(b.fallbackRetryAt) {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	if err := b.beginNativeRecovery(now); err != nil {
		b.mu.Lock()
		b.reportErrorLocked(fmt.Errorf("recover native watch streams: %w", err))
		b.scheduleFallbackRetryLocked(now)
		b.mu.Unlock()
	}
}

func (b *darwinWatchBackend) beginNativeRecovery(now time.Time) error {
	b.mu.Lock()
	if b.lifecycle == darwinBackendStopped ||
		b.fallbackPhaseValue() != darwinFallbackOpen {
		b.mu.Unlock()
		return nil
	}
	if darwinFallbackReason(b.fallbackReason.Load()) == darwinFallbackKqueueStart {
		b.fallbackRetryAt = time.Time{}
		b.fallbackRetryDelay = 0
		b.mu.Unlock()
		return nil
	}
	recovering := make([]darwinRecoveryStream, 0, len(b.logical))
	var skipped []*darwinLogicalRoot
	for _, state := range b.logical {
		if !state.plan.Recursive {
			continue
		}
		linkInfo, linkErr := b.lstat(state.plan.Path)
		if linkErr != nil || linkInfo.Mode()&os.ModeSymlink != 0 {
			skipped = append(skipped, state)
			continue
		}
		info, statErr := b.stat(state.plan.Path)
		if statErr != nil || !info.IsDir() {
			skipped = append(skipped, state)
			continue
		}
		if state.stream != nil && state.active {
			recovering = append(recovering, darwinRecoveryStream{
				state: state,
				registration: darwinWatchRoot{
					logicalPath: state.plan.Path,
					nativePath:  canonicalWatchRoot(state.plan.Path),
					recursive:   true,
					state:       state,
					generation:  state.generation,
				},
				stream:  state.stream,
				started: true,
			})
			continue
		}
		generation := state.generation + 1
		registration := darwinWatchRoot{
			logicalPath: state.plan.Path,
			nativePath:  canonicalWatchRoot(state.plan.Path),
			recursive:   true,
			state:       state,
			generation:  generation,
		}
		stream, err := b.newStream(state.plan.Path, func(events []fsevents.Event) {
			b.consumeFSEvents(registration, events)
		})
		if err != nil {
			b.mu.Unlock()
			for _, candidate := range recovering {
				if !candidate.started {
					_ = candidate.stream.Close()
				}
			}
			return err
		}
		recovering = append(recovering, darwinRecoveryStream{
			state: state, registration: registration, stream: stream,
		})
	}
	for _, state := range skipped {
		if err := b.installRootPollingLocked(state); err != nil {
			b.mu.Unlock()
			for _, candidate := range recovering {
				if !candidate.started {
					_ = candidate.stream.Close()
				}
			}
			return fmt.Errorf("transfer fallback polling for %s: %w", state.plan.Path, err)
		}
		linkInfo, linkErr := b.lstat(state.plan.Path)
		if linkErr == nil && linkInfo.Mode()&os.ModeSymlink != 0 {
			state.phase = darwinRootPolling
			b.resetRetryLocked(state)
		} else {
			state.phase = darwinRootPending
			b.schedulePendingRetryLocked(state, now)
		}
	}
	if len(recovering) == 0 {
		for _, state := range b.logical {
			if state.phase == darwinRootPending ||
				state.phase == darwinRootLossCollecting ||
				state.phase == darwinRootActivationCollecting ||
				(!state.plan.Recursive && !state.active) {
				b.scheduleFallbackRetryLocked(now)
				b.mu.Unlock()
				return nil
			}
		}
		b.fallbackGeneration++
		generation := b.fallbackGeneration
		b.fallbackRetryAt = time.Time{}
		b.fallbackRetryDelay = 0
		b.fallbackRecoveryFailed.Store(false)
		b.fallbackPhase.Store(uint32(darwinFallbackRecovering))
		b.nativeOpen.Store(true)
		b.mu.Unlock()
		if b.sink != nil {
			b.sink.RetainAuthoritative(backendLifecycleToken{
				gate: b, generation: generation,
			})
		}
		return nil
	}

	b.fallbackGeneration++
	generation := b.fallbackGeneration
	b.fallbackRetryAt = time.Time{}
	b.fallbackRetryDelay = 0
	b.fallbackRecoveryFailed.Store(false)
	b.fallbackPhase.Store(uint32(darwinFallbackRecovering))
	// Keep newly-started streams closed to dispatch until every stream owns
	// coverage. Changes during startup precede the authoritative marker below
	// and are therefore included by that reconciliation.
	b.nativeOpen.Store(false)
	for _, candidate := range recovering {
		state := candidate.state
		state.generation = candidate.registration.generation
		state.currentGeneration.Store(state.generation)
		state.acknowledged.Store(state.generation)
		state.phase = darwinRootOpen
		state.active = true
		state.ancestor = ""
		state.stream = candidate.stream
		state.open.Store(true)
		b.streams[state.plan.Path] = candidate.stream
		b.storeRootLocked(candidate.registration)
	}
	for _, candidate := range recovering {
		if candidate.started {
			continue
		}
		if err := candidate.stream.Start(); err != nil {
			streams := b.abortNativeRecoveryLocked(now)
			b.mu.Unlock()
			for _, stream := range streams {
				_ = stream.Close()
			}
			return err
		}
	}
	failed := b.fallbackRecoveryFailed.Load()
	b.mu.Unlock()
	if failed {
		b.abortNativeRecovery(now)
		return errors.New("native stream failed during recovery startup")
	}
	// Open native accumulation only after all streams have started, then queue
	// the authoritative marker immediately. Any event that wins this small
	// ordering window happened before reconciliation begins; events after the
	// marker is taken remain pending behind the serialized callback.
	b.nativeOpen.Store(true)
	if b.sink != nil {
		b.sink.RetainAuthoritative(backendLifecycleToken{
			gate: b, generation: generation,
		})
	}
	return nil
}

func (b *darwinWatchBackend) processNativeRecoveryHandoff() {
	if b.fallbackRecoveryFailed.Load() {
		b.abortNativeRecovery(time.Now())
		return
	}
	b.mu.Lock()
	generation := b.fallbackGeneration
	if b.fallbackAcknowledged.Load() < generation {
		b.mu.Unlock()
		return
	}
	pollingOwned := b.fallbackPollingOwned
	pollingKeyed := b.fallbackPollingKeyed
	release := b.onPollingReleased
	releasePlans := append([]darwinFallbackPollPlan(nil), b.fallbackPollPlans...)
	b.mu.Unlock()

	if pollingOwned && pollingKeyed && release != nil {
		for _, plan := range releasePlans {
			key := darwinFallbackPollingObligationKey(plan.path)
			if err := release(key); err != nil {
				b.mu.Lock()
				b.reportErrorLocked(fmt.Errorf("release fallback polling: %w", err))
				b.scheduleFallbackRetryLocked(time.Now())
				b.mu.Unlock()
				return
			}
		}
	}

	b.mu.Lock()
	if b.fallbackRecoveryFailed.Load() {
		pollPlans := append([]darwinFallbackPollPlan(nil), b.fallbackPollPlans...)
		b.mu.Unlock()
		if len(pollPlans) > 0 {
			if err := b.requireFallbackPolling(pollPlans); err != nil {
				b.mu.Lock()
				b.reportErrorLocked(fmt.Errorf("restore fallback polling: %w", err))
				b.scheduleFallbackRetryLocked(time.Now())
				b.mu.Unlock()
				return
			}
		}
		b.mu.Lock()
		streams := b.abortNativeRecoveryLocked(time.Now())
		b.mu.Unlock()
		for _, stream := range streams {
			_ = stream.Close()
		}
		return
	}
	b.fallbackPollingOwned = false
	b.fallbackPollingKeyed = false
	b.fallbackPrepared = false
	b.fallbackDegradedLogged = false
	b.fallbackPollPlans = nil
	b.fallbackRetryAt = time.Time{}
	b.fallbackRetryDelay = 0
	b.fallbackReason.Store(uint32(darwinFallbackReasonNone))
	b.fallbackPhase.Store(uint32(darwinFallbackInactive))
	b.nativeOpen.Store(true)
	b.rootTransitions.Inc()
	b.mu.Unlock()
	if b.fallbackRecoveryFailed.Swap(false) {
		b.requestFallback(darwinFallbackNativeDrop)
		return
	}
	// Recovery handoff takes priority in the lifecycle loop. Wake it once more
	// so root-local signals accumulated during fallback are processed normally.
	b.signalLifecycle()
}

func (b *darwinWatchBackend) abortNativeRecovery(now time.Time) {
	b.mu.Lock()
	streams := b.abortNativeRecoveryLocked(now)
	b.mu.Unlock()
	for _, stream := range streams {
		_ = stream.Close()
	}
}

func (b *darwinWatchBackend) abortNativeRecoveryLocked(now time.Time) []darwinStream {
	streams := make([]darwinStream, 0, len(b.streams))
	for root, stream := range b.streams {
		streams = append(streams, stream)
		delete(b.streams, root)
		b.removeRootLocked(root, true)
	}
	for _, state := range b.logical {
		if !state.plan.Recursive {
			continue
		}
		state.active = false
		state.stream = nil
		state.open.Store(false)
	}
	b.fallbackRecoveryFailed.Store(false)
	b.fallbackPhase.Store(uint32(darwinFallbackOpen))
	b.nativeOpen.Store(false)
	b.scheduleFallbackRetryLocked(now)
	return streams
}

func (b *darwinWatchBackend) scheduleFallbackRetryLocked(now time.Time) {
	if b.fallbackRetryDelay == 0 {
		b.fallbackRetryDelay = b.retryInitial
	} else {
		b.fallbackRetryDelay = min(b.fallbackRetryDelay*2, b.retryMax)
	}
	b.fallbackRetryAt = now.Add(b.fallbackRetryDelay)
}

func appendWatchScopeRoots(roots []string, scopes []WatchScope) []string {
	for _, scope := range scopes {
		if scope.SyncDir != "" && !slices.Contains(roots, filepath.Clean(scope.SyncDir)) {
			roots = append(roots, filepath.Clean(scope.SyncDir))
		}
	}
	slices.Sort(roots)
	return roots
}

func fallbackReasonLabel(reason darwinFallbackReason) string {
	switch reason {
	case darwinFallbackStreamCreate:
		return "stream-create"
	case darwinFallbackStreamStart:
		return "stream-start"
	case darwinFallbackNativeDrop:
		return "native-drop"
	case darwinFallbackLifecycle:
		return "lifecycle"
	case darwinFallbackKqueueStart:
		return "kqueue-start"
	default:
		return "unknown"
	}
}

func (b *darwinWatchBackend) nextLifecycleRetry() (time.Duration, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	earliest := b.fallbackRetryAt
	if b.fallbackPhaseValue() == darwinFallbackInactive &&
		darwinFallbackReason(b.fallbackReason.Load()) == darwinFallbackReasonNone {
		for _, state := range b.logical {
			if state.retryAt.IsZero() {
				continue
			}
			if earliest.IsZero() || state.retryAt.Before(earliest) {
				earliest = state.retryAt
			}
		}
	}
	if earliest.IsZero() {
		return 0, false
	}
	return max(time.Until(earliest), 0), true
}

func (b *darwinWatchBackend) retryDueLocked(
	state *darwinLogicalRoot,
	now time.Time,
) bool {
	return state.retryAt.IsZero() || !now.Before(state.retryAt)
}

func (b *darwinWatchBackend) scheduleRetryLocked(
	state *darwinLogicalRoot,
	now time.Time,
) {
	if state.retryDelay == 0 {
		state.retryDelay = b.retryInitial
	} else {
		state.retryDelay = min(state.retryDelay*2, b.retryMax)
	}
	state.retryAt = now.Add(state.retryDelay)
}

func (b *darwinWatchBackend) schedulePendingRetryLocked(
	state *darwinLogicalRoot,
	now time.Time,
) {
	if !state.retryAt.IsZero() && now.Before(state.retryAt) {
		return
	}
	b.scheduleRetryLocked(state, now)
}

func (b *darwinWatchBackend) resetRetryLocked(state *darwinLogicalRoot) {
	state.retryAt = time.Time{}
	state.retryDelay = 0
}

// handleKqueueEvent advances pending roots before the native event can reach
// the watcher. The resulting scope reconciliation is authoritative, so the
// component-creation event itself does not need a second dispatch.
func (b *darwinWatchBackend) handleKqueueEvent(
	event backendEvent,
) (backendEvent, bool) {
	snapshot := b.roots.Load()
	owner, ok := snapshot.mostSpecificRoot(event.Path)
	if ok {
		event.Root = owner.logicalPath
	}
	if b.collectFallbackEvent(event) {
		return backendEvent{}, false
	}
	if b.fallbackPhaseValue() == darwinFallbackOpen {
		if !ok {
			return backendEvent{}, false
		}
		return event, true
	}
	if ok && !owner.recursive && event.Path == owner.logicalPath &&
		event.Op&(backendOpRemove|backendOpRename) != 0 && owner.state != nil {
		owner.state.signal.Or(darwinRootSignalLoss)
	}
	b.signalLifecycle()
	if !ok || owner.recursive ||
		(owner.state != nil && !owner.state.isOpen(owner.generation)) {
		return backendEvent{}, false
	}
	return event, true
}

func (b *darwinWatchBackend) collectFallbackEvent(event backendEvent) bool {
	b.fallbackEventsMu.Lock()
	defer b.fallbackEventsMu.Unlock()
	phase := b.fallbackPhaseValue()
	if phase != darwinFallbackCollecting && phase != darwinFallbackReconciling {
		return false
	}
	if b.fallbackEventsFull || event.Op == backendOpUnknown {
		return true
	}
	if _, exists := b.fallbackEvents[event]; exists {
		return true
	}
	bytes := len(event.Path) + len(event.Root)
	if len(b.fallbackEvents)+1 > defaultWatchBatchMaxEntries ||
		b.fallbackEventBytes+bytes > defaultWatchBatchMaxPathBytes {
		clear(b.fallbackEvents)
		b.fallbackEventBytes = 0
		b.fallbackEventsFull = true
		b.pendingBatchOverflows.Inc()
		return true
	}
	b.fallbackEvents[event] = struct{}{}
	b.fallbackEventBytes += bytes
	return true
}

func (b *darwinWatchBackend) activateRootLocked(state *darwinLogicalRoot) error {
	root := state.plan.Path
	state.generation++
	state.currentGeneration.Store(state.generation)
	state.acknowledged.Store(state.generation)
	state.open.Store(b.lifecycle != darwinBackendRunning)
	if b.lifecycle == darwinBackendRunning {
		state.phase = darwinRootActivationCollecting
		state.acknowledged.Store(state.generation - 1)
	} else {
		state.phase = darwinRootOpen
	}
	registration := darwinWatchRoot{
		logicalPath: root,
		nativePath:  canonicalWatchRoot(root),
		recursive:   state.plan.Recursive,
		state:       state,
		generation:  state.generation,
	}
	if !state.plan.Recursive {
		if err := b.acquireShallowLocked(root); err != nil {
			state.phase = darwinRootPending
			state.open.Store(false)
			return err
		}
		state.active = true
		state.ancestor = root
		b.storeRootLocked(registration)
		return nil
	}
	stream, err := b.newStream(root, func(events []fsevents.Event) {
		b.consumeFSEvents(registration, events)
	})
	if err != nil {
		state.phase = darwinRootPending
		state.open.Store(false)
		return err
	}
	b.streams[root] = stream
	b.storeRootLocked(registration)
	if b.lifecycle == darwinBackendRunning {
		if err := stream.Start(); err != nil {
			delete(b.streams, root)
			b.removeRootLocked(root, true)
			_ = stream.Close()
			state.phase = darwinRootPending
			state.open.Store(false)
			return err
		}
	}
	state.active = true
	state.ancestor = ""
	state.stream = stream
	return nil
}

func (b *darwinWatchBackend) acquireShallowLocked(path string) error {
	path = filepath.Clean(path)
	if b.shallow[path] > 0 {
		b.shallow[path]++
		return nil
	}
	if err := b.addShallow(path); err != nil {
		return err
	}
	b.shallow[path] = 1
	return nil
}

func (b *darwinWatchBackend) releaseShallowLocked(path string) error {
	path = filepath.Clean(path)
	refs := b.shallow[path]
	if refs == 0 {
		return nil
	}
	if refs > 1 {
		b.shallow[path] = refs - 1
		return nil
	}
	delete(b.shallow, path)
	return b.removeShallow(path)
}

// observeAncestor lazily creates the single-descriptor vnode observer on the
// first ancestor add, so daemons with no missing roots hold no kqueue.
func (b *darwinWatchBackend) observeAncestor(path string) error {
	if b.vnode == nil {
		observer, err := newVnodeObserver(b.signalLifecycle)
		if err != nil {
			return err
		}
		b.vnode = observer
	}
	return b.vnode.Add(path)
}

func (b *darwinWatchBackend) unobserveAncestor(path string) error {
	if b.vnode == nil {
		return nil
	}
	return b.vnode.Remove(path)
}

func (b *darwinWatchBackend) acquireAncestorLocked(path string) error {
	path = filepath.Clean(path)
	if b.ancestors[path] > 0 {
		b.ancestors[path]++
		return nil
	}
	if err := b.addAncestor(path); err != nil {
		return err
	}
	b.ancestors[path] = 1
	return nil
}

func (b *darwinWatchBackend) releaseAncestorLocked(path string) error {
	path = filepath.Clean(path)
	refs := b.ancestors[path]
	if refs == 0 {
		return nil
	}
	if refs > 1 {
		b.ancestors[path] = refs - 1
		return nil
	}
	delete(b.ancestors, path)
	return b.removeAncestor(path)
}

func (b *darwinWatchBackend) accumulateReconcile(
	scopes []WatchScope,
	token backendLifecycleToken,
) {
	sink := b.sink
	if sink == nil {
		return
	}
	sink.TryAccumulate(func(add func(backendEvent) bool) bool {
		meaningful := false
		seen := make(map[string]struct{}, len(scopes))
		for _, scope := range scopes {
			root := filepath.Clean(scope.SyncDir)
			if scope.SyncDir == "" {
				continue
			}
			if _, exists := seen[root]; exists {
				continue
			}
			seen[root] = struct{}{}
			meaningful = true
			if !add(backendEvent{
				Root: root, Op: backendOpReconcileRootChange, Lifecycle: token,
			}) {
				return true
			}
		}
		return meaningful
	})
}

func (b *darwinWatchBackend) reportErrorLocked(err error) {
	select {
	case b.errors <- err:
	default:
	}
}

func pathIsDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info != nil && info.IsDir()
}

func nearestExistingAncestor(path string) string {
	path = filepath.Clean(path)
	for {
		if pathIsDirectory(path) {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}

func (b *darwinWatchBackend) finish() {
	b.finishOnce.Do(func() {
		close(b.events)
		close(b.errors)
		close(b.done)
	})
}

func (b *darwinWatchBackend) storeRootLocked(root darwinWatchRoot) {
	current := b.roots.Load()
	next := &darwinRootSnapshot{roots: append([]darwinWatchRoot(nil), current.roots...)}
	for i := range next.roots {
		if next.roots[i].logicalPath == root.logicalPath &&
			next.roots[i].recursive == root.recursive {
			next.roots[i] = root
			b.roots.Store(next)
			return
		}
	}
	next.roots = append(next.roots, root)
	b.roots.Store(next)
}

func (b *darwinWatchBackend) removeRootLocked(root string, recursive bool) {
	current := b.roots.Load()
	next := &darwinRootSnapshot{roots: slices.DeleteFunc(
		append([]darwinWatchRoot(nil), current.roots...),
		func(candidate darwinWatchRoot) bool {
			return candidate.logicalPath == root && candidate.recursive == recursive
		},
	)}
	b.roots.Store(next)
}

func (s *darwinRootSnapshot) mostSpecificRoot(path string) (darwinWatchRoot, bool) {
	if s == nil {
		return darwinWatchRoot{}, false
	}
	clean := filepath.Clean(path)
	var best darwinWatchRoot
	for _, root := range s.roots {
		if !pathAtOrBelow(root.logicalPath, clean) {
			continue
		}
		if best.logicalPath == "" || len(root.logicalPath) > len(best.logicalPath) ||
			(len(root.logicalPath) == len(best.logicalPath) && root.recursive) {
			best = root
		}
	}
	return best, best.logicalPath != ""
}

// mostSpecificRecursiveRoot selects ownership among native recursive streams.
// Shallow kqueue registrations can be nested within a recursive root, but they
// cover only their direct directory and must not shadow descendant FSEvents.
func (s *darwinRootSnapshot) mostSpecificRecursiveRoot(path string) (darwinWatchRoot, bool) {
	if s == nil {
		return darwinWatchRoot{}, false
	}
	clean := filepath.Clean(path)
	var best darwinWatchRoot
	for _, root := range s.roots {
		if !root.recursive || !pathAtOrBelow(root.logicalPath, clean) {
			continue
		}
		if best.logicalPath == "" || len(root.logicalPath) > len(best.logicalPath) {
			best = root
		}
	}
	return best, best.logicalPath != ""
}

func canonicalWatchRoot(root string) string {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return root
	}
	return filepath.Clean(canonical)
}

func logicalFSEventPath(root darwinWatchRoot, path string) string {
	path = filepath.Clean(path)
	if root.nativePath == root.logicalPath || !pathAtOrBelow(root.nativePath, path) {
		return path
	}
	rel, err := filepath.Rel(root.nativePath, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.Join(root.logicalPath, rel)
}
