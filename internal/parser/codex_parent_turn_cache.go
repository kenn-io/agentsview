package parser

import (
	"container/list"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

const codexParentTurnCacheMaxEntries = 64

type codexParentTurnCacheKey struct {
	path   string
	size   int64
	mtime  int64
	inode  uint64
	device uint64
}

type codexParentTurnCacheEntry struct {
	files   []codexParentTurnCacheKey
	turnIDs map[string]struct{}
}

type codexParentTurnCache struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[string]*list.Element
	recent     *list.List
}

func newCodexParentTurnCache(maxEntries int) *codexParentTurnCache {
	return &codexParentTurnCache{
		maxEntries: maxEntries,
		entries:    make(map[string]*list.Element),
		recent:     list.New(),
	}
}

func newCodexProductionParentTurnCache() *codexParentTurnCache {
	return newCodexParentTurnCache(codexParentTurnCacheMaxEntries)
}

func codexParentTurnCacheKeyFor(
	path string,
	info os.FileInfo,
) codexParentTurnCacheKey {
	inode, device := sourceFileIdentity(info)
	return codexParentTurnCacheKey{
		path:   filepath.Clean(path),
		size:   info.Size(),
		mtime:  info.ModTime().UnixNano(),
		inode:  inode,
		device: device,
	}
}

// Get and Put use the first rollout's path to locate an inventory. Every
// file identity must match before reusing its immutable combined turn set.
// Inventories are nonempty and remain unchanged after Put.
func (c *codexParentTurnCache) Get(
	files []codexParentTurnCacheKey,
) (map[string]struct{}, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[files[0].path]
	if !ok || !slices.Equal(elem.Value.(codexParentTurnCacheEntry).files, files) {
		return nil, false
	}
	c.recent.MoveToFront(elem)
	return elem.Value.(codexParentTurnCacheEntry).turnIDs, true
}

func (c *codexParentTurnCache) Put(
	files []codexParentTurnCacheKey,
	turnIDs map[string]struct{},
) {
	if c == nil || c.maxEntries <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := files[0].path
	if elem, ok := c.entries[key]; ok {
		elem.Value = codexParentTurnCacheEntry{files: files, turnIDs: turnIDs}
		c.recent.MoveToFront(elem)
		return
	}
	elem := c.recent.PushFront(codexParentTurnCacheEntry{
		files: files, turnIDs: turnIDs,
	})
	c.entries[key] = elem
	for len(c.entries) > c.maxEntries {
		oldest := c.recent.Back()
		entry := oldest.Value.(codexParentTurnCacheEntry)
		delete(c.entries, entry.files[0].path)
		c.recent.Remove(oldest)
	}
}
