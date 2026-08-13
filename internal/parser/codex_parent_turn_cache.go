package parser

import (
	"container/list"
	"os"
	"path/filepath"
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
	key     codexParentTurnCacheKey
	turnIDs map[string]struct{}
}

type codexParentTurnCache struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[codexParentTurnCacheKey]*list.Element
	parents    map[string]codexParentTurnCacheKey
	recent     *list.List
}

func newCodexParentTurnCache(maxEntries int) *codexParentTurnCache {
	return &codexParentTurnCache{
		maxEntries: maxEntries,
		entries:    make(map[codexParentTurnCacheKey]*list.Element),
		parents:    make(map[string]codexParentTurnCacheKey),
		recent:     list.New(),
	}
}

func (c *codexParentTurnCache) GetParent(
	parentKey string,
) (map[string]struct{}, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	key, ok := c.parents[parentKey]
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	info, err := os.Stat(key.path)
	if err != nil || codexParentTurnCacheKeyFor(key.path, info) != key {
		c.mu.Lock()
		delete(c.parents, parentKey)
		c.mu.Unlock()
		return nil, false
	}
	return c.Get(key)
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

func (c *codexParentTurnCache) Get(
	key codexParentTurnCacheKey,
) (map[string]struct{}, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.recent.MoveToFront(elem)
	return elem.Value.(codexParentTurnCacheEntry).turnIDs, true
}

func (c *codexParentTurnCache) Put(
	key codexParentTurnCacheKey,
	turnIDs map[string]struct{},
) {
	if c == nil || c.maxEntries <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		elem.Value = codexParentTurnCacheEntry{key: key, turnIDs: turnIDs}
		c.recent.MoveToFront(elem)
		return
	}
	elem := c.recent.PushFront(codexParentTurnCacheEntry{
		key: key, turnIDs: turnIDs,
	})
	c.entries[key] = elem
	for len(c.entries) > c.maxEntries {
		oldest := c.recent.Back()
		entry := oldest.Value.(codexParentTurnCacheEntry)
		delete(c.entries, entry.key)
		for parentKey, key := range c.parents {
			if key == entry.key {
				delete(c.parents, parentKey)
			}
		}
		c.recent.Remove(oldest)
	}
}

func (c *codexParentTurnCache) PutParent(
	parentKey string,
	key codexParentTurnCacheKey,
	turnIDs map[string]struct{},
) {
	c.Put(key, turnIDs)
	if c == nil || c.maxEntries <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		c.parents[parentKey] = key
	}
}
