package sync

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

type RecursiveWatchResult struct {
	Watched             int
	Unwatched           int
	Err                 error
	BudgetExhausted     bool
	ResourceExhausted   bool
	ResourceExhaustedAt string
}

const (
	// A callback can hold one batch while the event loop accumulates the next.
	// Bounding each batch keeps watcher backpressure below two bounded path
	// sets instead of allowing a slow sync to retain an unbounded event storm.
	defaultWatchBatchMaxEntries   = 8192
	defaultWatchBatchMaxPathBytes = 2 << 20
)

// WatchBatch describes one serialized watcher callback. FullSync is an
// explicit overflow signal: Paths is empty and the consumer must rescan all
// configured sources so coalescing never silently drops a changed path.
type WatchBatch struct {
	Paths    []string
	FullSync bool
}

// pendingWatchBatch bounds retained strings by both unique path count and the
// sum of their byte lengths. Map overhead is bounded separately by the entry
// limit. Once either limit would be exceeded, individual paths are discarded
// in favor of one full-sync marker until Take resets the accumulator.
type pendingWatchBatch struct {
	paths        map[string]struct{}
	pathBytes    int
	maxEntries   int
	maxPathBytes int
	fullSync     bool
}

func newPendingWatchBatch(maxEntries, maxPathBytes int) *pendingWatchBatch {
	return &pendingWatchBatch{
		paths:        make(map[string]struct{}),
		maxEntries:   maxEntries,
		maxPathBytes: maxPathBytes,
	}
}

func (p *pendingWatchBatch) Empty() bool {
	return !p.fullSync && len(p.paths) == 0
}

func (p *pendingWatchBatch) Add(path string) {
	if p.fullSync {
		return
	}
	if _, exists := p.paths[path]; exists {
		return
	}
	if len(p.paths)+1 > p.maxEntries ||
		p.pathBytes+len(path) > p.maxPathBytes {
		clear(p.paths)
		p.pathBytes = 0
		p.fullSync = true
		return
	}
	p.paths[path] = struct{}{}
	p.pathBytes += len(path)
}

func (p *pendingWatchBatch) Take() (WatchBatch, bool) {
	if p.Empty() {
		return WatchBatch{}, false
	}
	if p.fullSync {
		p.fullSync = false
		return WatchBatch{FullSync: true}, true
	}

	paths := make([]string, 0, len(p.paths))
	for path := range p.paths {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	clear(p.paths)
	p.pathBytes = 0
	return WatchBatch{Paths: paths}, true
}

// Watcher uses fsnotify to watch session directories for changes and triggers
// serialized callbacks with short-burst batching and a dispatch floor.
type Watcher struct {
	onChange     func(batch WatchBatch)
	watcher      *fsnotify.Watcher
	batchDelay   time.Duration
	minInterval  time.Duration
	maxEntries   int
	maxPathBytes int
	excludes     []string
	roots        []string
	shallow      []string
	rootsMu      sync.RWMutex
	dispatchMu   sync.Mutex
	stopping     bool
	stop         chan struct{}
	done         chan struct{}
	stopOnce     sync.Once
}

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

func newWatcherWithLimits(
	batchDelay, minInterval time.Duration,
	onChange func(batch WatchBatch), excludes []string,
	maxEntries, maxPathBytes int,
) (*Watcher, error) {
	if onChange == nil {
		return nil, fmt.Errorf("onChange callback is nil: %w", os.ErrInvalid)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		onChange:     onChange,
		watcher:      fsw,
		batchDelay:   batchDelay,
		minInterval:  minInterval,
		maxEntries:   maxEntries,
		maxPathBytes: maxPathBytes,
		excludes:     normalizeExcludePatterns(excludes),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	return w, nil
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
	var result RecursiveWatchResult
	root = filepath.Clean(root)
	w.addRoot(root)

	remaining := budget
	result.Err = filepath.WalkDir(root,
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip inaccessible dirs
			}
			if !d.IsDir() {
				return nil
			}
			// Skip entire excluded subtrees, but always keep the root.
			if path != root && w.shouldExcludeForRoot(path, root) {
				return filepath.SkipDir
			}
			if remaining <= 0 {
				result.BudgetExhausted = true
				return filepath.SkipAll
			}
			if addErr := w.watcher.Add(path); addErr != nil {
				result.Unwatched++
				if isWatchResourceExhaustion(addErr) {
					result.ResourceExhausted = true
					result.ResourceExhaustedAt = path
					return filepath.SkipAll
				}
				return nil
			}
			remaining--
			result.Watched++
			return nil
		})
	if errors.Is(result.Err, filepath.SkipAll) {
		result.Err = nil
	}
	return result
}

func isWatchResourceExhaustion(err error) bool {
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENOSPC)
}

// WatchShallow adds only the root directory to the watch list,
// without recursing into subdirectories. Use this for directories
// with many subdirectories where periodic sync handles changes.
// Returns true if the directory was successfully watched.
func (w *Watcher) WatchShallow(root string) bool {
	root = filepath.Clean(root)
	w.addRoot(root)
	w.addShallowRoot(root)
	return w.watcher.Add(root) == nil
}

// Start begins processing file events in a goroutine.
func (w *Watcher) Start() {
	go w.loop()
}

// Stop stops the watcher and waits for it to finish.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		w.dispatchMu.Lock()
		w.stopping = true
		close(w.stop)
		w.dispatchMu.Unlock()
		<-w.done
		w.watcher.Close()
	})
}

func (w *Watcher) loop() {
	batches := make(chan WatchBatch)
	callbackDone := make(chan time.Time, 1)
	var worker sync.WaitGroup
	worker.Go(func() {
		for batch := range batches {
			if batch.FullSync {
				log.Printf("watcher: pending path limit exceeded, triggering full sync")
			} else {
				log.Printf("watcher: %d file(s) changed, triggering sync", len(batch.Paths))
			}
			startedAt := time.Now()
			w.onChange(batch)
			callbackDone <- startedAt
		}
	})

	pending := newPendingWatchBatch(w.maxEntries, w.maxPathBytes)
	var firstPendingAt time.Time
	var lastDispatch time.Time
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
		close(w.done)
	}()

	schedule := func() {
		if callbackBusy || pending.Empty() || timerC != nil {
			return
		}
		deadline := firstPendingAt.Add(w.batchDelay)
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
		if callbackBusy || pending.Empty() {
			return true
		}
		w.dispatchMu.Lock()
		defer w.dispatchMu.Unlock()
		if w.stopping {
			return false
		}
		batch, ok := pending.Take()
		if !ok {
			return true
		}
		firstPendingAt = time.Time{}
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

		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			path, relevant := w.handleEvent(event)
			if !relevant {
				continue
			}
			if pending.Empty() {
				firstPendingAt = time.Now()
			}
			pending.Add(path)
			schedule()

		case err, ok := <-w.watcher.Errors:
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

		case startedAt := <-callbackDone:
			lastDispatch = startedAt
			callbackBusy = false
			schedule()
		}
	}
}

// handleEvent processes a single fsnotify event, auto-watching
// newly created directories and returning relevant changed paths.
func (w *Watcher) handleEvent(event fsnotify.Event) (string, bool) {
	if event.Op&(fsnotify.Write|
		fsnotify.Create|
		fsnotify.Remove|
		fsnotify.Rename) == 0 {
		return "", false
	}

	if event.Op&fsnotify.Create != 0 {
		isDir, excluded := w.watchIfDir(event.Name)
		if isDir && excluded {
			return "", false
		}
	}

	return filepath.Clean(event.Name), true
}

// watchIfDir adds a path to the watch list if it is a directory.
// Returns whether path is a directory and whether it was excluded.
func (w *Watcher) watchIfDir(path string) (isDir bool, excluded bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false, false
	}
	if w.isUnderShallowRoot(path) {
		return true, false
	}
	if w.shouldExclude(path) {
		return true, true
	}
	_ = w.watcher.Add(path)
	return true, false
}

func normalizeExcludePatterns(patterns []string) []string {
	if len(patterns) == 0 {
		return nil
	}
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(filepath.Clean(p))
		if p == "" || p == "." {
			continue
		}
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

func (w *Watcher) addRoot(root string) {
	w.rootsMu.Lock()
	defer w.rootsMu.Unlock()
	if !slices.Contains(w.roots, root) {
		w.roots = append(w.roots, root)
	}
}

func (w *Watcher) addShallowRoot(root string) {
	w.rootsMu.Lock()
	defer w.rootsMu.Unlock()
	if !slices.Contains(w.shallow, root) {
		w.shallow = append(w.shallow, root)
	}
}

// isUnderShallowRoot reports whether path's most specific containing watch
// root is a shallow root. A path that also sits under a more specific
// recursive root (for example a new sessions/YYYY/MM/DD directory beneath a
// recursive sessions root that itself lives inside a shallow parent root) is
// NOT shadowed, so auto-watching still adds new subdirectories of recursive
// roots.
func (w *Watcher) isUnderShallowRoot(path string) bool {
	root, ok := w.mostSpecificContainingRoot(path)
	if !ok {
		return false
	}
	w.rootsMu.RLock()
	defer w.rootsMu.RUnlock()
	return slices.Contains(w.shallow, root)
}

func (w *Watcher) shouldExclude(path string) bool {
	if len(w.excludes) == 0 {
		return false
	}
	root, ok := w.mostSpecificContainingRoot(path)
	if !ok {
		return false
	}
	return w.shouldExcludeForRoot(path, root)
}

func (w *Watcher) shouldExcludeForRoot(path string, root string) bool {
	if len(w.excludes) == 0 {
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

	for _, pat := range w.excludes {
		if strings.Contains(pat, string(filepath.Separator)) {
			if ok, _ := filepath.Match(pat, rel); ok {
				return true
			}
			continue
		}
		for _, part := range parts {
			if ok, _ := filepath.Match(pat, part); ok {
				return true
			}
		}
	}
	return false
}

func (w *Watcher) mostSpecificContainingRoot(path string) (string, bool) {
	w.rootsMu.RLock()
	defer w.rootsMu.RUnlock()

	if len(w.roots) == 0 {
		return "", false
	}

	clean := filepath.Clean(path)
	var best string
	for _, root := range w.roots {
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
