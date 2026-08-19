package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/fsnotify/fsnotify"
)

type fsnotifyBackend struct {
	watcher           *fsnotify.Watcher
	eventInput        <-chan fsnotify.Event
	errorInput        <-chan error
	watchOps          fsnotifyWatchOps
	queue             *nativeEventQueue
	events            chan backendEvent
	errors            chan error
	excludes          []string
	roots             []string
	recursive         []string
	shallow           []string
	rootsMu           sync.RWMutex
	watchMu           sync.Mutex
	watchOwners       map[string]map[string]struct{}
	watchBudgetCost   map[string]int
	runtimeBudget     int
	rootScopes        map[string][]PollingScope
	degradedRoots     map[string]struct{}
	onPollingRequired func(PollingObligation) error
	lifecycleMu       sync.Mutex
	lifecycle         fsnotifyBackendLifecycle
	stop              chan struct{}
	pumpOnce          sync.Once
	pumpStarted       atomic.Bool
	pumpStop          chan struct{}
	pumpDone          chan struct{}
	done              chan struct{}
	finishOnce        sync.Once
}

// nativeEventQueueLimit bounds events held between the native reader and the
// translating loop. It matches inotify's default max_queued_events; past it
// the queue drops pending events and reports an overflow, which the loop
// already turns into a lost-events full sync.
const nativeEventQueueLimit = 16384

type nativeItem struct {
	event fsnotify.Event
	err   error
}

// nativeEventQueue decouples reading the native watcher from translating its
// events. fsnotify's Windows backend services Add and Remove on the same
// goroutine that delivers events over an unbuffered channel, so the goroutine
// that consumes events must never be the one waiting on Add or Remove.
type nativeEventQueue struct {
	mu       sync.Mutex
	items    []nativeItem
	overflow bool
	closed   bool
	signal   chan struct{}
}

func newNativeEventQueue() *nativeEventQueue {
	return &nativeEventQueue{signal: make(chan struct{}, 1)}
}

func (q *nativeEventQueue) push(item nativeItem) {
	q.mu.Lock()
	if len(q.items) >= nativeEventQueueLimit {
		q.items = nil
		q.overflow = true
	} else {
		q.items = append(q.items, item)
	}
	q.mu.Unlock()
	q.wake()
}

func (q *nativeEventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.wake()
}

func (q *nativeEventQueue) wake() {
	select {
	case q.signal <- struct{}{}:
	default:
	}
}

// next blocks until an item is available, the queue is closed and drained, or
// stop is closed. An overflow surfaces as fsnotify.ErrEventOverflow ahead of
// anything queued after it.
func (q *nativeEventQueue) next(stop <-chan struct{}) (nativeItem, bool) {
	for {
		q.mu.Lock()
		if q.overflow {
			q.overflow = false
			q.mu.Unlock()
			return nativeItem{err: fsnotify.ErrEventOverflow}, true
		}
		if len(q.items) > 0 {
			item := q.items[0]
			q.items[0] = nativeItem{}
			q.items = q.items[1:]
			if len(q.items) == 0 {
				q.items = nil
			}
			q.mu.Unlock()
			return item, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return nativeItem{}, false
		}
		select {
		case <-q.signal:
		case <-stop:
			return nativeItem{}, false
		}
	}
}

type fsnotifyWatchOps interface {
	Add(path string) error
	Remove(path string) error
}

type fsnotifyBackendLifecycle uint8

const (
	fsnotifyBackendNew fsnotifyBackendLifecycle = iota
	fsnotifyBackendRunning
	fsnotifyBackendStopped
)

func newFSNotifyBackend(excludes []string) (*fsnotifyBackend, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &fsnotifyBackend{
		watcher:         watcher,
		eventInput:      watcher.Events,
		errorInput:      watcher.Errors,
		watchOps:        watcher,
		queue:           newNativeEventQueue(),
		events:          make(chan backendEvent),
		errors:          make(chan error, 1),
		excludes:        normalizeExcludePatterns(excludes),
		watchOwners:     make(map[string]map[string]struct{}),
		watchBudgetCost: make(map[string]int),
		rootScopes:      make(map[string][]PollingScope),
		degradedRoots:   make(map[string]struct{}),
		stop:            make(chan struct{}),
		pumpStop:        make(chan struct{}),
		pumpDone:        make(chan struct{}),
		done:            make(chan struct{}),
	}, nil
}

func (b *fsnotifyBackend) Events() <-chan backendEvent { return b.events }
func (b *fsnotifyBackend) Errors() <-chan error        { return b.errors }

func (b *fsnotifyBackend) AddRecursive(root string, budget int) RecursiveWatchResult {
	b.startPump()
	b.watchMu.Lock()
	defer b.watchMu.Unlock()

	var result RecursiveWatchResult
	root = filepath.Clean(root)
	b.addRecursiveRoot(root)

	remaining := budget
	result.Err = filepath.WalkDir(root,
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				// WalkDir surfaces errors only for the root stat and failed
				// directory reads, so this subtree's descendants get no
				// native watches. Count it as degraded coverage — the owning
				// logical root then gains a polling obligation instead of
				// the result appearing fully watched — and keep walking the
				// accessible remainder.
				result.Unwatched++
				return nil
			}
			if !d.IsDir() {
				return nil
			}
			// Skip entire excluded subtrees, but always keep the root.
			if path != root && b.shouldExcludeForRoot(path, root) {
				return filepath.SkipDir
			}
			// A directory an earlier root already watches natively is shared,
			// not installed again: the kernel watch is already paid for, so
			// settle reuse before the budget check or an overlapping root
			// would be refused coverage that already exists.
			if len(b.watchOwners[path]) > 0 {
				b.addWatchOwner(path, root)
				result.Watched++
				return nil
			}
			// The root's own directory is the mandatory part of a recursive
			// registration and its subtree is the discretionary part. A
			// shallow unit's watch is installed unconditionally, so a plan
			// that merges one into a recursive unit at the same path would
			// lose that coverage the moment the budget ran out. The subtree
			// below still reports BudgetExhausted and hands off to polling.
			mandatory := path == root
			if remaining <= 0 && !mandatory {
				result.BudgetExhausted = true
				return filepath.SkipAll
			}
			if addErr := b.watchOps.Add(path); addErr != nil {
				result.Unwatched++
				if isWatchResourceExhaustion(addErr) {
					result.ResourceExhausted = true
					result.ResourceExhaustedAt = path
					return filepath.SkipAll
				}
				return nil
			}
			// A mandatory watch installed past the budget sits outside the
			// accounting entirely, the way a shallow root already does: it is
			// not charged, so removing it must not refund a slot the process
			// never spent. Charging it and refunding it would leave headroom
			// above the cap that runtime subtree adds could then claim.
			if remaining > 0 {
				b.watchBudgetCost[path]++
				remaining--
				result.Allocated++
			}
			b.addWatchOwner(path, root)
			result.Watched++
			return nil
		})
	if errors.Is(result.Err, filepath.SkipAll) {
		result.Err = nil
	}
	b.runtimeBudget = max(remaining, 0)
	return result
}

func (b *fsnotifyBackend) setWatchRootPlan(roots []WatchRoot) {
	b.watchMu.Lock()
	defer b.watchMu.Unlock()
	b.rootScopes = make(map[string][]PollingScope, len(roots))
	for _, root := range roots {
		path := filepath.Clean(root.Path)
		for _, scope := range root.Scopes {
			if scope.SyncDir == "" {
				continue
			}
			ps := PollingScope{Agent: scope.Agent, Root: filepath.Clean(scope.SyncDir)}
			if !slices.Contains(b.rootScopes[path], ps) {
				b.rootScopes[path] = append(b.rootScopes[path], ps)
			}
		}
		slices.SortFunc(b.rootScopes[path], func(a, b PollingScope) int {
			if a.Agent != b.Agent {
				return strings.Compare(a.Agent, b.Agent)
			}
			return strings.Compare(a.Root, b.Root)
		})
	}
}

func (b *fsnotifyBackend) bindPollingOwnership(
	required func(PollingObligation) error,
	_ func(string) error,
) {
	b.watchMu.Lock()
	defer b.watchMu.Unlock()
	b.onPollingRequired = required
}

func (b *fsnotifyBackend) AddShallow(root string) error {
	b.startPump()
	b.watchMu.Lock()
	defer b.watchMu.Unlock()

	root = filepath.Clean(root)
	if err := b.watchOps.Add(root); err != nil {
		return err
	}
	b.addShallowRoot(root)
	b.addWatchOwner(root, root)
	return nil
}

func (b *fsnotifyBackend) Remove(root string) error {
	b.startPump()
	b.watchMu.Lock()
	defer b.watchMu.Unlock()

	root = filepath.Clean(root)
	b.rootsMu.Lock()
	wasRecursive := slices.Contains(b.recursive, root)
	wasShallow := slices.Contains(b.shallow, root)
	if !wasRecursive && !wasShallow {
		b.rootsMu.Unlock()
		return fmt.Errorf("%w: %s", fsnotify.ErrNonExistentWatch, root)
	}
	b.recursive = slices.DeleteFunc(b.recursive, func(candidate string) bool {
		return candidate == root
	})
	b.shallow = slices.DeleteFunc(b.shallow, func(candidate string) bool {
		return candidate == root
	})
	b.roots = slices.DeleteFunc(b.roots, func(candidate string) bool {
		return candidate == root
	})
	b.rootsMu.Unlock()

	ownedPaths := make([]string, 0)
	for path, owners := range b.watchOwners {
		if _, owned := owners[root]; owned {
			ownedPaths = append(ownedPaths, path)
		}
	}
	slices.Sort(ownedPaths)
	var removeErrs []error
	for _, path := range ownedPaths {
		owners := b.watchOwners[path]
		delete(owners, root)
		if len(owners) > 0 {
			continue
		}
		delete(b.watchOwners, path)
		if err := b.watchOps.Remove(path); err != nil &&
			!errors.Is(err, fsnotify.ErrNonExistentWatch) {
			removeErrs = append(removeErrs,
				fmt.Errorf("remove native watch %q: %w", path, err))
		}
		b.reclaimWatchBudgetLocked(path)
	}
	return errors.Join(removeErrs...)
}

func (b *fsnotifyBackend) Start() error {
	b.lifecycleMu.Lock()
	defer b.lifecycleMu.Unlock()
	if b.lifecycle != fsnotifyBackendNew {
		return nil
	}
	b.lifecycle = fsnotifyBackendRunning
	b.startPump()
	go b.loop()
	return nil
}

// Stop ends event delivery and closes the native watcher. When the loop is
// running it owns the shutdown order: it closes the native watcher only after
// it has left any in-flight Add or Remove, so a pending request cannot be
// abandoned by fsnotify's Close, then releases the pump. When the loop never
// ran, Stop releases the pump itself before closing the native watcher.
func (b *fsnotifyBackend) Stop() {
	b.lifecycleMu.Lock()
	if b.lifecycle == fsnotifyBackendStopped {
		done := b.done
		b.lifecycleMu.Unlock()
		<-done
		return
	}
	wasRunning := b.lifecycle == fsnotifyBackendRunning
	b.lifecycle = fsnotifyBackendStopped
	close(b.stop)
	if !wasRunning {
		close(b.pumpStop)
		if b.pumpStarted.Load() {
			<-b.pumpDone
		}
		_ = b.watcher.Close()
		b.finish()
	}
	done := b.done
	b.lifecycleMu.Unlock()
	<-done
}

func (b *fsnotifyBackend) Name() string { return "fsnotify" }

// startPump runs the native reader before the first native Add or Remove.
// On Windows, fsnotify services those requests on the goroutine that
// delivers events over an unbuffered channel, and registration installs
// watches before Start runs, so an event arriving between registration Adds
// would block the next Add forever if nothing consumed events yet. The input
// channels are captured here so a test seam assigned before the first native
// operation is never read concurrently with the pump.
func (b *fsnotifyBackend) startPump() {
	b.pumpOnce.Do(func() {
		b.pumpStarted.Store(true)
		go b.pump(b.eventInput, b.errorInput)
	})
}

// pump is the sole reader of the native watcher's channels. It never calls
// back into the native watcher, so fsnotify's reader is never left blocked
// delivering an event while the loop waits on Add or Remove.
func (b *fsnotifyBackend) pump(
	eventInput <-chan fsnotify.Event,
	errorInput <-chan error,
) {
	defer close(b.pumpDone)
	defer b.queue.close()
	for {
		select {
		case <-b.pumpStop:
			return
		case event, ok := <-eventInput:
			if !ok {
				return
			}
			b.queue.push(nativeItem{event: event})
		case err, ok := <-errorInput:
			if !ok {
				return
			}
			b.queue.push(nativeItem{err: err})
		}
	}
}

func (b *fsnotifyBackend) loop() {
	defer func() {
		_ = b.watcher.Close()
		close(b.pumpStop)
		<-b.pumpDone
		b.finish()
	}()
	for {
		item, ok := b.queue.next(b.stop)
		if !ok {
			return
		}
		if item.err != nil {
			if !b.forwardNativeError(item.err) {
				return
			}
			continue
		}
		translated, relevant := b.translateEvent(item.event)
		if !relevant {
			continue
		}
		select {
		case b.events <- translated:
		case <-b.stop:
			return
		}
	}
}

// forwardNativeError reports false when the backend is stopping.
func (b *fsnotifyBackend) forwardNativeError(err error) bool {
	if errors.Is(err, fsnotify.ErrEventOverflow) {
		// Overflow dropped raw events before their watch-maintenance side
		// effects ran, so creates under recursive roots may have left
		// subtrees without native watches. The full sync recovers the data
		// but not the watches; polling covers the lost subtrees. Shallow
		// roots are revalidated in place instead.
		b.requireRuntimePolling(b.recursiveRootsSnapshot())
		b.reinstallShallowWatches()
		select {
		case b.events <- backendEvent{Op: backendOpFullSync}:
			return true
		case <-b.stop:
			return false
		}
	}
	select {
	case b.errors <- err:
		return true
	case <-b.stop:
		return false
	}
}

func (b *fsnotifyBackend) finish() {
	b.finishOnce.Do(func() {
		close(b.events)
		close(b.errors)
		close(b.done)
	})
}

func (b *fsnotifyBackend) translateEvent(event fsnotify.Event) (backendEvent, bool) {
	op := translateFSNotifyOp(event.Op)
	if op == backendOpUnknown {
		return backendEvent{}, false
	}

	itemType := backendItemUnknown
	if op&(backendOpRemove|backendOpRename) != 0 {
		removed, lostRoots := b.forgetRemovedSubtree(event.Name)
		if removed {
			itemType = backendItemDirectory
		}
		b.requireRuntimePolling(lostRoots)
	}
	if op&backendOpCreate != 0 {
		var excluded bool
		itemType, excluded = b.watchCreatedPath(event.Name)
		if excluded {
			return backendEvent{}, false
		}
	}
	root, _ := b.mostSpecificContainingRoot(event.Name)

	return backendEvent{
		Path:     filepath.Clean(event.Name),
		Root:     root,
		Op:       op,
		ItemType: itemType,
	}, true
}

func translateFSNotifyOp(op fsnotify.Op) backendOp {
	var translated backendOp
	if op&fsnotify.Create != 0 {
		translated |= backendOpCreate
	}
	if op&fsnotify.Write != 0 {
		translated |= backendOpWrite
	}
	if op&fsnotify.Remove != 0 {
		translated |= backendOpRemove
	}
	if op&fsnotify.Rename != 0 {
		translated |= backendOpRename
	}
	return translated
}

// watchCreatedPath classifies a created path and adds it to the watch list when
// it is an included directory.
func (b *fsnotifyBackend) watchCreatedPath(path string) (backendItemType, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return backendItemUnknown, false
	}
	if !info.IsDir() {
		return backendItemFile, false
	}

	b.watchMu.Lock()
	if b.isUnderShallowRoot(path) {
		b.watchMu.Unlock()
		return backendItemDirectory, false
	}
	if b.shouldExclude(path) {
		b.watchMu.Unlock()
		return backendItemDirectory, true
	}
	owners := b.recursiveOwnersForPath(path)
	if len(owners) == 0 {
		b.watchMu.Unlock()
		return backendItemDirectory, false
	}
	degraded := b.addRuntimeSubtreeLocked(path, owners)
	b.watchMu.Unlock()
	b.requireRuntimePolling(degraded)
	return backendItemDirectory, false
}

// addRuntimeSubtreeLocked recursively covers a newly created or moved-in
// directory without exceeding the startup recursive-watch budget. Caller holds
// watchMu. Any incomplete owner is returned for scoped polling handoff.
func (b *fsnotifyBackend) addRuntimeSubtreeLocked(
	path string,
	owners []string,
) []string {
	degraded := make(map[string]struct{})
	_ = filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			for _, root := range owners {
				degraded[root] = struct{}{}
			}
			return filepath.SkipAll
		}
		if !entry.IsDir() {
			return nil
		}
		currentOwners := make([]string, 0, len(owners))
		for _, root := range owners {
			if !b.shouldExcludeForRoot(current, root) {
				currentOwners = append(currentOwners, root)
			}
		}
		if len(currentOwners) == 0 {
			return filepath.SkipDir
		}
		if len(b.watchOwners[current]) == 0 {
			if b.runtimeBudget <= 0 {
				for _, root := range currentOwners {
					degraded[root] = struct{}{}
				}
				return filepath.SkipAll
			}
			if err := b.watchOps.Add(current); err != nil {
				for _, root := range currentOwners {
					degraded[root] = struct{}{}
				}
				return filepath.SkipAll
			}
			b.watchBudgetCost[current]++
			b.runtimeBudget--
		}
		for _, root := range currentOwners {
			b.addWatchOwner(current, root)
		}
		return nil
	})
	roots := make([]string, 0, len(degraded))
	for root := range degraded {
		roots = append(roots, root)
	}
	slices.Sort(roots)
	return roots
}

// forgetRemovedSubtree clears native-watch ownership invalidated by a
// directory remove or rename event. Budget is tied to active native watches,
// so churn can reuse slots without growing the ownership ledger forever.
func (b *fsnotifyBackend) forgetRemovedSubtree(path string) (bool, []string) {
	b.watchMu.Lock()
	defer b.watchMu.Unlock()

	path = filepath.Clean(path)
	removed := make([]string, 0)
	lostRoots := make(map[string]struct{})
	for watched := range b.watchOwners {
		if pathAtOrBelow(path, watched) {
			removed = append(removed, watched)
			for owner := range b.watchOwners[watched] {
				if pathAtOrBelow(path, owner) {
					lostRoots[owner] = struct{}{}
				}
			}
		}
	}
	if len(removed) == 0 {
		return false, nil
	}
	slices.Sort(removed)
	for _, watched := range removed {
		delete(b.watchOwners, watched)
		if err := b.watchOps.Remove(watched); err != nil &&
			!errors.Is(err, fsnotify.ErrNonExistentWatch) {
			b.reportError(fmt.Errorf(
				"remove invalidated native watch %q: %w", watched, err,
			))
		}
		b.reclaimWatchBudgetLocked(watched)
	}
	roots := make([]string, 0, len(lostRoots))
	for root := range lostRoots {
		roots = append(roots, root)
	}
	slices.Sort(roots)
	return true, roots
}

func (b *fsnotifyBackend) reclaimWatchBudgetLocked(path string) {
	cost := b.watchBudgetCost[path]
	delete(b.watchBudgetCost, path)
	if cost > math.MaxInt-b.runtimeBudget {
		b.runtimeBudget = math.MaxInt
		return
	}
	b.runtimeBudget += cost
}

func (b *fsnotifyBackend) requireRuntimePolling(roots []string) {
	for _, root := range roots {
		b.watchMu.Lock()
		if _, already := b.degradedRoots[root]; already {
			b.watchMu.Unlock()
			continue
		}
		required := b.onPollingRequired
		scopes := append([]PollingScope(nil), b.rootScopes[root]...)
		b.watchMu.Unlock()
		if len(scopes) == 0 {
			scopes = []PollingScope{{Root: root}}
		}
		if required == nil {
			b.reportError(fmt.Errorf(
				"fsnotify coverage degraded for %s without polling callback", root,
			))
			continue
		}
		if err := required(PollingObligation{
			Key: "fsnotify-runtime:" + root, Scopes: scopes, Probe: root,
		}); err != nil {
			b.reportError(fmt.Errorf(
				"transfer fsnotify coverage for %s to polling: %w", root, err,
			))
			continue
		}
		b.watchMu.Lock()
		b.degradedRoots[root] = struct{}{}
		b.watchMu.Unlock()
	}
}

func (b *fsnotifyBackend) reportError(err error) {
	select {
	case b.errors <- err:
	default:
	}
}

func normalizeExcludePatterns(patterns []string) []string {
	if len(patterns) == 0 {
		return nil
	}
	out := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(filepath.Clean(pattern))
		if pattern == "" || pattern == "." {
			continue
		}
		if !slices.Contains(out, pattern) {
			out = append(out, pattern)
		}
	}
	return out
}

// reinstallShallowWatches revalidates shallow-root coverage after overflow.
// A dropped removal loses the native watch even when the directory was
// recreated, so every shallow root is re-added; a root that cannot be
// re-added moves to polling exactly as an observed removal would.
func (b *fsnotifyBackend) reinstallShallowWatches() {
	b.rootsMu.RLock()
	shallow := append([]string(nil), b.shallow...)
	b.rootsMu.RUnlock()
	lost := make([]string, 0, len(shallow))
	for _, root := range shallow {
		b.watchMu.Lock()
		err := b.watchOps.Add(root)
		b.watchMu.Unlock()
		if err != nil {
			lost = append(lost, root)
		}
	}
	b.requireRuntimePolling(lost)
}

func (b *fsnotifyBackend) recursiveRootsSnapshot() []string {
	b.rootsMu.RLock()
	defer b.rootsMu.RUnlock()
	return append([]string(nil), b.recursive...)
}

func (b *fsnotifyBackend) addRecursiveRoot(root string) {
	b.rootsMu.Lock()
	defer b.rootsMu.Unlock()
	if !slices.Contains(b.roots, root) {
		b.roots = append(b.roots, root)
	}
	if !slices.Contains(b.recursive, root) {
		b.recursive = append(b.recursive, root)
	}
}

func (b *fsnotifyBackend) addShallowRoot(root string) {
	b.rootsMu.Lock()
	defer b.rootsMu.Unlock()
	if !slices.Contains(b.shallow, root) {
		b.shallow = append(b.shallow, root)
	}
	if !slices.Contains(b.roots, root) {
		b.roots = append(b.roots, root)
	}
}

// isUnderShallowRoot reports whether path's most specific containing watch
// root is a shallow root. A path that also sits under a more specific
// recursive root is not shadowed, so new subdirectories are still watched.
func (b *fsnotifyBackend) isUnderShallowRoot(path string) bool {
	root, ok := b.mostSpecificContainingRoot(path)
	if !ok {
		return false
	}
	b.rootsMu.RLock()
	defer b.rootsMu.RUnlock()
	return slices.Contains(b.shallow, root) && !slices.Contains(b.recursive, root)
}

func pathAtOrBelow(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." ||
		(rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func (b *fsnotifyBackend) recursiveOwnersForPath(path string) []string {
	b.rootsMu.RLock()
	defer b.rootsMu.RUnlock()
	owners := make([]string, 0, len(b.recursive))
	for _, root := range b.recursive {
		if _, degraded := b.degradedRoots[root]; degraded {
			continue
		}
		if pathAtOrBelow(root, path) && !b.shouldExcludeForRoot(path, root) {
			owners = append(owners, root)
		}
	}
	return owners
}

// addWatchOwner records ownership only after the native Add succeeds. Caller
// holds watchMu, which serializes the native watch and its ownership ledger.
func (b *fsnotifyBackend) addWatchOwner(path, root string) {
	owners := b.watchOwners[path]
	if owners == nil {
		owners = make(map[string]struct{})
		b.watchOwners[path] = owners
	}
	owners[root] = struct{}{}
}

func (b *fsnotifyBackend) shouldExclude(path string) bool {
	if len(b.excludes) == 0 {
		return false
	}
	root, ok := b.mostSpecificContainingRoot(path)
	if !ok {
		return false
	}
	return b.shouldExcludeForRoot(path, root)
}

func (b *fsnotifyBackend) shouldExcludeForRoot(path string, root string) bool {
	return shouldExcludeForRoot(b.excludes, path, root)
}

func (b *fsnotifyBackend) includeCreatedSubtreePath(root, path string) bool {
	return !b.shouldExcludeForRoot(path, root)
}

func (b *fsnotifyBackend) shouldEnumerateCreatedSubtree(root, path string) bool {
	b.rootsMu.RLock()
	defer b.rootsMu.RUnlock()
	return slices.Contains(b.recursive, filepath.Clean(root)) &&
		pathAtOrBelow(root, path)
}

func shouldExcludeForRoot(excludes []string, path string, root string) bool {
	if len(excludes) == 0 {
		return false
	}
	clean := filepath.Clean(path)
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	rel = filepath.Clean(rel)
	if rel == "." {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))

	for _, pattern := range excludes {
		if strings.Contains(pattern, string(filepath.Separator)) {
			if ok, _ := filepath.Match(pattern, rel); ok {
				return true
			}
			continue
		}
		for _, part := range parts {
			if ok, _ := filepath.Match(pattern, part); ok {
				return true
			}
		}
	}
	return false
}

func (b *fsnotifyBackend) mostSpecificContainingRoot(path string) (string, bool) {
	b.rootsMu.RLock()
	defer b.rootsMu.RUnlock()

	if len(b.roots) == 0 {
		return "", false
	}

	clean := filepath.Clean(path)
	var best string
	for _, root := range b.roots {
		rel, err := filepath.Rel(root, clean)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if best == "" || len(root) > len(best) {
			best = root
		}
	}
	if best == "" {
		return "", false
	}
	return best, true
}

func isWatchResourceExhaustion(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENOSPC)
}
