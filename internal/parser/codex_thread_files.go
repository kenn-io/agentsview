package parser

import (
	"container/list"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
)

// Providers are constructed per source. Share the inventory on their factory,
// and bound its roots because S3 parses use short-lived materialization roots.
type codexThreadFileCache struct {
	mu     sync.Mutex
	roots  map[string]*list.Element
	recent list.List
}

type codexThreadDirectory struct {
	path       string
	info       os.FileInfo
	changeTime int64
	children   map[string]*codexThreadDirectory
	threads    map[string][]string
	watch      codexThreadWatch
	batch      *codexThreadInventoryBatch
	inventory  map[string][]string
}

type codexThreadInventoryContextKey struct{}

type codexThreadInventoryBatch struct {
	mu     sync.Mutex
	closed atomic.Bool
	roots  map[codexParentInventoryRoot]map[string]*codexParentInventoryDependency
}

type codexParentInventoryRoot struct {
	cache *codexThreadFileCache
	path  string
}

type codexParentInventoryDependency struct {
	paths   []string
	changed bool
	sources map[CodexParentInventorySource]struct{}
}

// CodexParentInventorySource identifies a fork whose parent files changed
// during a batch and whose stored result must remain eligible for retry.
type CodexParentInventorySource struct {
	Agent AgentType
	Path  string
}

// WithCodexParentInventoryBatch checks each root once while parsing a batch.
// Call ValidateCodexParentInventoryBatch after all workers finish, before
// treating their results as current. Parent files are checked on every parse.
func WithCodexParentInventoryBatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, codexThreadInventoryContextKey{}, new(codexThreadInventoryBatch))
}

func codexParentInventoryBatch(ctx context.Context) *codexThreadInventoryBatch {
	batch, _ := ctx.Value(codexThreadInventoryContextKey{}).(*codexThreadInventoryBatch)
	if batch != nil && batch.closed.Load() {
		return nil
	}
	return batch
}

func (c *codexThreadFileCache) directory(root string) *codexThreadDirectory {
	if c.roots == nil {
		c.roots = make(map[string]*list.Element)
	}
	elem := c.roots[root]
	if elem == nil {
		elem = c.recent.PushFront(&codexThreadDirectory{path: root, inventory: make(map[string][]string)})
		c.roots[root] = elem
		if len(c.roots) > codexParentTurnCacheMaxEntries {
			oldest := c.recent.Back()
			oldest.Value.(*codexThreadDirectory).closeWatches()
			delete(c.roots, oldest.Value.(*codexThreadDirectory).path)
			c.recent.Remove(oldest)
		}
	}
	c.recent.MoveToFront(elem)
	return elem.Value.(*codexThreadDirectory)
}

func (c *codexThreadFileCache) find(ctx context.Context, root, threadID string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	directory := c.directory(filepath.Clean(root))
	batch := codexParentInventoryBatch(ctx)
	if batch == nil || directory.batch != batch {
		directory.batch = nil
		if err := directory.refresh(0, directory.inventory); err != nil {
			return nil, err
		}
		directory.batch = batch
	}
	return slices.Clone(directory.inventory[threadID]), nil
}

func (c *codexThreadFileCache) recordParent(ctx context.Context, root, threadID string, paths []string, source CodexParentInventorySource) {
	batch := codexParentInventoryBatch(ctx)
	if batch == nil {
		return
	}
	batch.mu.Lock()
	defer batch.mu.Unlock()
	if batch.closed.Load() {
		return
	}
	if batch.roots == nil {
		batch.roots = make(map[codexParentInventoryRoot]map[string]*codexParentInventoryDependency)
	}
	key := codexParentInventoryRoot{cache: c, path: filepath.Clean(root)}
	parents := batch.roots[key]
	if parents == nil {
		parents = make(map[string]*codexParentInventoryDependency)
		batch.roots[key] = parents
	}
	parent := parents[threadID]
	if parent == nil {
		parent = &codexParentInventoryDependency{paths: slices.Clone(paths), sources: make(map[CodexParentInventorySource]struct{})}
		parents[threadID] = parent
	} else if !slices.Equal(parent.paths, paths) {
		parent.changed = true
	}
	parent.sources[source] = struct{}{}
}

// ValidateCodexParentInventoryBatch closes the batch and returns forks whose
// parent inventory changed or could not be checked. Later parses on this
// context use standalone lookups. Call only after the batch's workers finish.
func ValidateCodexParentInventoryBatch(ctx context.Context) ([]CodexParentInventorySource, error) {
	batch := codexParentInventoryBatch(ctx)
	if batch == nil {
		return nil, nil
	}
	batch.mu.Lock()
	defer batch.mu.Unlock()
	batch.closed.Store(true)
	affected := make(map[CodexParentInventorySource]struct{})
	var validationErr error
	for root, parents := range batch.roots {
		root.cache.mu.Lock()
		directory := root.cache.directory(root.path)
		directory.batch = nil
		err := directory.refresh(0, directory.inventory)
		validationErr = errors.Join(validationErr, err)
		for id, parent := range parents {
			if err != nil || parent.changed || !slices.Equal(parent.paths, directory.inventory[id]) {
				for source := range parent.sources {
					affected[source] = struct{}{}
				}
			}
		}
		root.cache.mu.Unlock()
	}
	batch.roots = nil
	sources := make([]CodexParentInventorySource, 0, len(affected))
	for source := range affected {
		sources = append(sources, source)
	}
	return sources, validationErr
}

func (c *codexThreadFileCache) invalidate(roots []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, root := range roots {
		root = filepath.Clean(root)
		if elem := c.roots[root]; elem != nil {
			elem.Value.(*codexThreadDirectory).closeWatches()
			delete(c.roots, root)
			c.recent.Remove(elem)
		}
	}
}

func (d *codexThreadDirectory) closeWatches() {
	d.watch.close()
	for _, child := range d.children {
		child.closeWatches()
	}
}

func (d *codexThreadDirectory) refresh(depth int, inventory map[string][]string) error {
	info, changeTime, err := statCodexThreadDirectory(d.path)
	if err != nil || !info.IsDir() {
		d.removeInventory(inventory)
		d.closeWatches()
		d.info, d.children, d.threads = nil, nil, nil
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := d.refreshWatch(info); err != nil {
		return err
	}
	if d.info == nil || !os.SameFile(info, d.info) || !info.ModTime().Equal(d.info.ModTime()) || changeTime != d.changeTime {
		entries, err := os.ReadDir(d.path)
		if err != nil {
			d.removeInventory(inventory)
			d.closeWatches()
			d.info, d.children, d.threads = nil, nil, nil
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		children := make(map[string]*codexThreadDirectory)
		threads := make(map[string][]string)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				if depth < 3 && IsDigits(name) {
					child := d.children[name]
					if child == nil {
						child = &codexThreadDirectory{path: filepath.Join(d.path, name)}
					}
					children[name] = child
				}
				continue
			}
			if depth != 0 && depth != 3 {
				continue
			}
			key := CodexSessionUUIDFromFilename(name)
			if key != "" {
				thread := CodexThreadIDFromSessionKey(key)
				threads[thread] = append(threads[thread], filepath.Join(d.path, name))
			}
		}
		for name, child := range d.children {
			if children[name] == nil {
				child.removeInventory(inventory)
				child.closeWatches()
			}
		}
		for id, old := range d.threads {
			if !slices.Equal(old, threads[id]) {
				replaceCodexInventoryPaths(inventory, id, old, threads[id])
			}
		}
		for id, paths := range threads {
			if _, existed := d.threads[id]; !existed {
				replaceCodexInventoryPaths(inventory, id, nil, paths)
			}
		}
		d.info, d.children, d.threads = info, children, threads
		d.changeTime = changeTime
	}
	for _, child := range d.children {
		if err := child.refresh(depth+1, inventory); err != nil {
			return err
		}
	}
	return nil
}

// Replace only a changed directory's contribution; unchanged thread slices
// remain reusable across batches without flattening the entire filename tree.
func replaceCodexInventoryPaths(inventory map[string][]string, id string, old, current []string) {
	paths := make([]string, 0, len(inventory[id])+len(current))
	for _, path := range inventory[id] {
		if !slices.Contains(old, path) {
			paths = append(paths, path)
		}
	}
	paths = append(paths, current...)
	slices.Sort(paths)
	if len(paths) == 0 {
		delete(inventory, id)
	} else {
		inventory[id] = paths
	}
}

func (d *codexThreadDirectory) removeInventory(inventory map[string][]string) {
	for id, paths := range d.threads {
		replaceCodexInventoryPaths(inventory, id, paths, nil)
	}
	for _, child := range d.children {
		child.removeInventory(inventory)
	}
}

func statCodexThreadDirectory(path string) (os.FileInfo, int64, error) {
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			return nil, 0, err
		}
		changeTime, _ := codexIndexChangeTimeForFile(nil, info)
		return info, changeTime, nil
	}
	// Windows pathname attributes can retain an old directory mtime after
	// children change. Read identity, mtime, and metadata change time through
	// the same handle, since directory entries can change without a new mtime.
	dir, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return nil, 0, err
	}
	changeTime, _ := codexIndexChangeTimeForFile(dir, info)
	return info, changeTime, nil
}
