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

// A nonzero size gives every processing batch a distinct pointer identity.
type codexThreadInventoryBatch struct{ _ byte }

// WithCodexParentInventoryBatch refreshes each root's parent-file inventory
// once during a sync batch. Individual parent files are still checked on every
// parse. Callers must start a new batch for the next sync or watcher event.
func WithCodexParentInventoryBatch(ctx context.Context) context.Context {
	return context.WithValue(ctx, codexThreadInventoryContextKey{}, new(codexThreadInventoryBatch))
}

func (c *codexThreadFileCache) find(ctx context.Context, root, threadID string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	root = filepath.Clean(root)
	if c.roots == nil {
		c.roots = make(map[string]*list.Element)
	}
	elem, ok := c.roots[root]
	if !ok {
		elem = c.recent.PushFront(&codexThreadDirectory{path: root})
		c.roots[root] = elem
		if len(c.roots) > codexParentTurnCacheMaxEntries {
			oldest := c.recent.Back()
			oldest.Value.(*codexThreadDirectory).closeWatches()
			delete(c.roots, oldest.Value.(*codexThreadDirectory).path)
			c.recent.Remove(oldest)
		}
	}
	c.recent.MoveToFront(elem)
	directory := elem.Value.(*codexThreadDirectory)
	batch, _ := ctx.Value(codexThreadInventoryContextKey{}).(*codexThreadInventoryBatch)
	if batch != nil && directory.batch == batch {
		return slices.Clone(directory.inventory[threadID]), nil
	}
	directory.batch, directory.inventory = nil, nil
	lookupID := threadID
	if batch != nil {
		lookupID = ""
	}
	var paths []string
	if err := directory.collect(lookupID, 0, &paths); err != nil {
		return nil, err
	}
	slices.Sort(paths)
	if batch != nil {
		inventory := make(map[string][]string)
		for _, path := range paths {
			key := CodexSessionUUIDFromFilename(filepath.Base(path))
			id := CodexThreadIDFromSessionKey(key)
			inventory[id] = append(inventory[id], path)
		}
		directory.batch, directory.inventory = batch, inventory
		return slices.Clone(inventory[threadID]), nil
	}
	return paths, nil
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

func (d *codexThreadDirectory) collect(threadID string, depth int, paths *[]string) error {
	info, changeTime, err := statCodexThreadDirectory(d.path)
	if err != nil || !info.IsDir() {
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
				child.closeWatches()
			}
		}
		d.info, d.children, d.threads = info, children, threads
		d.changeTime = changeTime
	}
	if threadID == "" {
		for _, files := range d.threads {
			*paths = append(*paths, files...)
		}
	} else {
		*paths = append(*paths, d.threads[threadID]...)
	}
	for _, child := range d.children {
		if err := child.collect(threadID, depth+1, paths); err != nil {
			return err
		}
	}
	return nil
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
