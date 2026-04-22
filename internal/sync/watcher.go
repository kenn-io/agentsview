package sync

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher uses fsnotify to watch session directories for changes
// and triggers a callback with debouncing.
type Watcher struct {
	onChange func(paths []string)
	watcher  *fsnotify.Watcher
	debounce time.Duration
	excludes []string
	roots    []string
	rootsMu  sync.RWMutex
	pending  map[string]time.Time
	mu       sync.Mutex
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	now      func() time.Time
}

// NewWatcher creates a file watcher that calls onChange when
// files are modified after the debounce period elapses.
func NewWatcher(debounce time.Duration, onChange func(paths []string), excludes []string) (*Watcher, error) {
	if onChange == nil {
		return nil, fmt.Errorf("onChange callback is nil: %w", os.ErrInvalid)
	}

	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	w := &Watcher{
		onChange: onChange,
		watcher:  fsw,
		debounce: debounce,
		excludes: normalizeExcludePatterns(excludes),
		pending:  make(map[string]time.Time),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		now:      time.Now,
	}
	return w, nil
}

// WatchRecursive walks a directory tree and adds all
// subdirectories to the watch list. Returns the number
// of directories watched and unwatched (failed to add).
func (w *Watcher) WatchRecursive(root string) (watched int, unwatched int, err error) {
	root = filepath.Clean(root)
	w.addRoot(root)
	err = filepath.WalkDir(root,
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip inaccessible dirs
			}
			if d.IsDir() {
				// Skip entire excluded subtrees, but always keep the root.
				if path != root && w.shouldExcludeForRoot(path, root) {
					return filepath.SkipDir
				}
				if addErr := w.watcher.Add(path); addErr != nil {
					unwatched++
				} else {
					watched++
				}
			}
			return nil
		})
	return watched, unwatched, err
}

// WatchShallow adds only the root directory to the watch list,
// without recursing into subdirectories. Use this for directories
// with many subdirectories where periodic sync handles changes.
// Returns true if the directory was successfully watched.
func (w *Watcher) WatchShallow(root string) bool {
	root = filepath.Clean(root)
	w.addRoot(root)
	return w.watcher.Add(root) == nil
}

// Start begins processing file events in a goroutine.
func (w *Watcher) Start() {
	go w.loop()
}

// Stop stops the watcher and waits for it to finish.
func (w *Watcher) Stop() {
	w.stopOnce.Do(func() {
		close(w.stop)
		<-w.done
		w.watcher.Close()
	})
}

func (w *Watcher) loop() {
	defer close(w.done)
	ticker := time.NewTicker(w.debounce)
	defer ticker.Stop()

	for {
		select {
		case <-w.stop:
			return

		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			w.handleEvent(event)

		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher error: %v", err)

		case <-ticker.C:
			w.flush()
		}
	}
}

// handleEvent processes a single fsnotify event, auto-watching
// newly created directories and recording pending changes.
func (w *Watcher) handleEvent(event fsnotify.Event) {
	if event.Op&(fsnotify.Write|
		fsnotify.Create|
		fsnotify.Remove|
		fsnotify.Rename) == 0 {
		return
	}

	if event.Op&fsnotify.Create != 0 {
		isDir, excluded := w.watchIfDir(event.Name)
		if isDir && excluded {
			return
		}
	}

	w.mu.Lock()
	w.pending[event.Name] = w.now()
	w.mu.Unlock()
}

// watchIfDir adds a path to the watch list if it is a directory.
// Returns whether path is a directory and whether it was excluded.
func (w *Watcher) watchIfDir(path string) (isDir bool, excluded bool) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false, false
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

func (w *Watcher) flush() {
	w.mu.Lock()
	if len(w.pending) == 0 {
		w.mu.Unlock()
		return
	}

	now := w.now()
	var ready []string
	for path, t := range w.pending {
		if now.Sub(t) >= w.debounce {
			ready = append(ready, path)
		}
	}

	for _, path := range ready {
		delete(w.pending, path)
	}
	w.mu.Unlock()

	if len(ready) > 0 {
		log.Printf("watcher: %d file(s) changed, triggering sync",
			len(ready))
		w.onChange(ready)
	}
}
