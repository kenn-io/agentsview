package parser

import (
	"container/list"
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

func (c *codexThreadFileCache) invalidate(roots []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, root := range roots {
		root = filepath.Clean(root)
		if elem := c.roots[root]; elem != nil {
			delete(c.roots, root)
			c.recent.Remove(elem)
		}
	}
}

func (d *codexThreadDirectory) collect(threadID string, depth int, paths *[]string) {
	info, changeTime, err := statCodexThreadDirectory(d.path)
	if err != nil || !info.IsDir() {
		d.info, d.children, d.threads = nil, nil, nil
		return
	}
	if d.info == nil || !os.SameFile(info, d.info) || !info.ModTime().Equal(d.info.ModTime()) || changeTime != d.changeTime {
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
		d.changeTime = changeTime
	}
	*paths = append(*paths, d.threads[threadID]...)
	for _, child := range d.children {
		child.collect(threadID, depth+1, paths)
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
