package parser

import (
	"container/list"
	"os"
	"path/filepath"
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
	path     string
	info     os.FileInfo
	children map[string]*codexThreadDirectory
	threads  map[string][]string
}

func (c *codexThreadFileCache) find(root, threadID string) []string {
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
			delete(c.roots, oldest.Value.(*codexThreadDirectory).path)
			c.recent.Remove(oldest)
		}
	}
	c.recent.MoveToFront(elem)
	var paths []string
	elem.Value.(*codexThreadDirectory).collect(threadID, 0, &paths)
	slices.Sort(paths)
	return paths
}

func (d *codexThreadDirectory) collect(threadID string, depth int, paths *[]string) {
	info, err := os.Stat(d.path)
	if err != nil || !info.IsDir() {
		d.info, d.children, d.threads = nil, nil, nil
		return
	}
	if d.info == nil || !os.SameFile(info, d.info) || !info.ModTime().Equal(d.info.ModTime()) {
		entries, err := os.ReadDir(d.path)
		if err != nil {
			d.info, d.children, d.threads = nil, nil, nil
			return
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
		d.info, d.children, d.threads = info, children, threads
	}
	*paths = append(*paths, d.threads[threadID]...)
	for _, child := range d.children {
		child.collect(threadID, depth+1, paths)
	}
}
