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
	"syscall"

	"github.com/fsnotify/fsnotify"
)

type fsnotifyBackend struct {
	watcher           *fsnotify.Watcher
	errorInput        <-chan error
	watchOps          fsnotifyWatchOps
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
	rootScopes        map[string][]string
	degradedRoots     map[string]struct{}
	onPollingRequired func(PollingObligation) error
	lifecycleMu       sync.Mutex
	lifecycle         fsnotifyBackendLifecycle
	stop              chan struct{}
	done              chan struct{}
	finishOnce        sync.Once
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
		errorInput:      watcher.Errors,
		watchOps:        watcher,
		events:          make(chan backendEvent),
		errors:          make(chan error, 1),
		excludes:        normalizeExcludePatterns(excludes),
		watchOwners:     make(map[string]map[string]struct{}),
		watchBudgetCost: make(map[string]int),
		rootScopes:      make(map[string][]string),
		degradedRoots:   make(map[string]struct{}),
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}, nil
}

func (b *fsnotifyBackend) Events() <-chan backendEvent { return b.events }
func (b *fsnotifyBackend) Errors() <-chan error        { return b.errors }

func (b *fsnotifyBackend) AddRecursive(root string, budget int) RecursiveWatchResult {
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
			if remaining <= 0 {
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
			b.watchBudgetCost[path]++
			b.addWatchOwner(path, root)
			remaining--
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
	b.rootScopes = make(map[string][]string, len(roots))
	for _, root := range roots {
		path := filepath.Clean(root.Path)
		for _, scope := range root.Scopes {
			if scope.SyncDir != "" {
				b.rootScopes[path] = append(b.rootScopes[path], scope.SyncDir)
			}
		}
		slices.Sort(b.rootScopes[path])
		b.rootScopes[path] = slices.Compact(b.rootScopes[path])
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
	go b.loop()
	return nil
}

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
	_ = b.watcher.Close()
	if !wasRunning {
		b.finish()
	}
	done := b.done
	b.lifecycleMu.Unlock()
	<-done
}

func (b *fsnotifyBackend) Name() string { return "fsnotify" }

func (b *fsnotifyBackend) loop() {
	defer b.finish()
	for {
		select {
		case <-b.stop:
			return
		case event, ok := <-b.watcher.Events:
			if !ok {
				return
			}
			translated, relevant := b.translateEvent(event)
			if !relevant {
				continue
			}
			select {
			case b.events <- translated:
			case <-b.stop:
				return
			}
		case err, ok := <-b.errorInput:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				select {
				case b.events <- backendEvent{Op: backendOpFullSync}:
				case <-b.stop:
					return
				}
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
		scopes := append([]string(nil), b.rootScopes[root]...)
		b.watchMu.Unlock()
		if len(scopes) == 0 {
			scopes = []string{root}
		}
		if required == nil {
			b.reportError(fmt.Errorf(
				"fsnotify coverage degraded for %s without polling callback", root,
			))
			continue
		}
		if err := required(PollingObligation{
			Key: "fsnotify-runtime:" + root, Roots: scopes, Probe: root,
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
