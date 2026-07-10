package parser

import (
	"container/list"
	"crypto/sha256"
	"path/filepath"
	"strings"
	"sync"
)

const (
	codexCursorCacheMaxEntries = 256
	codexCursorCacheMaxBytes   = 2 << 20

	// Account for the map bucket, list element, pointers, string headers, and
	// allocator overhead that are not represented by the variable-length path
	// and cursor strings below. The cache is intentionally an estimate rather
	// than a heap profiler, but this conservative allowance keeps its retained
	// memory bounded near the configured byte limit.
	codexCursorEntryOverheadBytes = 256
)

// codexCursorState is the compact state needed to make a tail parse behave as
// though the already-persisted prefix had just been scanned. It deliberately
// excludes parsed messages, raw transcript data, tool maps, and open files.
type codexCursorState struct {
	model                    string
	cwd                      string
	firstUserDigest          [sha256.Size]byte
	firstUserSeen            bool
	sawUserTurnAfterFirst    bool
	mayReplayFirstUserPrompt bool
	lastTokenUsageDigest     [sha256.Size]byte
	lastTokenUsageSeen       bool
	forkGate                 codexForkGate
	lastTaskEvent            string
}

// observeUserPrompt advances the first-user replay state using only a digest
// of the full normalized prompt. first reports the initial genuine prompt;
// replay reports the one positively identified post-abort re-emission that the
// caller must suppress.
func (s *codexCursorState) observeUserPrompt(content string) (first, replay bool) {
	digest := sha256.Sum256([]byte(content))
	if !s.firstUserSeen {
		s.firstUserDigest = digest
		s.firstUserSeen = true
		return true, false
	}
	if digest == s.firstUserDigest &&
		!s.sawUserTurnAfterFirst &&
		s.mayReplayFirstUserPrompt {
		s.mayReplayFirstUserPrompt = false
		return false, true
	}
	s.sawUserTurnAfterFirst = true
	s.mayReplayFirstUserPrompt = false
	return false, false
}

func (s *codexCursorState) markFirstUserReplayPossible() {
	if !s.firstUserSeen || s.sawUserTurnAfterFirst {
		return
	}
	s.mayReplayFirstUserPrompt = true
}

func (s *codexCursorState) observeTaskEvent(eventType string) {
	switch eventType {
	case "task_started", "task_complete", "turn_aborted":
		s.lastTaskEvent = eventType
		if eventType == "turn_aborted" {
			s.markFirstUserReplayPossible()
		}
	}
}

// observeTokenUsage records the exact streaming token payload compactly and
// reports whether it repeats the most recently observed payload.
func (s *codexCursorState) observeTokenUsage(raw string) bool {
	digest := sha256.Sum256([]byte(raw))
	duplicate := s.lastTokenUsageSeen && digest == s.lastTokenUsageDigest
	s.lastTokenUsageDigest = digest
	s.lastTokenUsageSeen = true
	return duplicate
}

type codexCursorKey struct {
	path   string
	offset int64
	inode  uint64
	device uint64
}

type codexCursorEntry struct {
	key   codexCursorKey
	state codexCursorState
	bytes int64
}

// codexCursorCache is a concurrency-safe LRU keyed by an exact physical-file
// version and safe resume offset. Multiple offsets for the same file coexist;
// the caller's persisted offset decides which version is eligible.
type codexCursorCache struct {
	mu         sync.Mutex
	maxEntries int
	maxBytes   int64
	totalBytes int64
	entries    map[codexCursorKey]*list.Element
	recent     *list.List
}

func newCodexCursorCache(maxEntries int, maxBytes int64) *codexCursorCache {
	return &codexCursorCache{
		maxEntries: maxEntries,
		maxBytes:   maxBytes,
		entries:    make(map[codexCursorKey]*list.Element),
		recent:     list.New(),
	}
}

func newProductionCodexCursorCache() *codexCursorCache {
	return newCodexCursorCache(
		codexCursorCacheMaxEntries,
		codexCursorCacheMaxBytes,
	)
}

func (c *codexCursorCache) Get(
	path string,
	offset int64,
	inode uint64,
	device uint64,
) (codexCursorState, bool) {
	if c == nil {
		return codexCursorState{}, false
	}
	key := newCodexCursorKey(path, offset, inode, device)

	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.entries[key]
	if !ok {
		return codexCursorState{}, false
	}
	c.recent.MoveToFront(elem)
	return elem.Value.(codexCursorEntry).state, true
}

// Put stages one exact cursor version. It returns false when the entry cannot
// fit by itself; an oversized replacement leaves any existing value intact.
func (c *codexCursorCache) Put(
	path string,
	offset int64,
	inode uint64,
	device uint64,
	state codexCursorState,
) bool {
	if c == nil || c.maxEntries <= 0 || c.maxBytes <= 0 {
		return false
	}
	key := codexCursorKey{
		path:   filepath.Clean(path),
		offset: offset,
		inode:  inode,
		device: device,
	}
	entryBytes := estimateCodexCursorEntryBytes(key, state)
	if entryBytes > c.maxBytes {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.entries[key]; ok {
		entry := elem.Value.(codexCursorEntry)
		if entry.state == state {
			c.recent.MoveToFront(elem)
			return true
		}
		state = cloneCodexCursorState(state)
		c.totalBytes -= entry.bytes
		entry.state = state
		entry.bytes = entryBytes
		elem.Value = entry
		c.totalBytes += entryBytes
		c.recent.MoveToFront(elem)
		c.evictLocked()
		return true
	}

	key.path = strings.Clone(key.path)
	state = cloneCodexCursorState(state)
	entry := codexCursorEntry{key: key, state: state, bytes: entryBytes}
	elem := c.recent.PushFront(entry)
	c.entries[key] = elem
	c.totalBytes += entryBytes
	c.evictLocked()
	return true
}

func (c *codexCursorCache) evictLocked() {
	for len(c.entries) > c.maxEntries || c.totalBytes > c.maxBytes {
		elem := c.recent.Back()
		if elem == nil {
			return
		}
		entry := elem.Value.(codexCursorEntry)
		delete(c.entries, entry.key)
		c.totalBytes -= entry.bytes
		c.recent.Remove(elem)
	}
}

func newCodexCursorKey(
	path string,
	offset int64,
	inode uint64,
	device uint64,
) codexCursorKey {
	return codexCursorKey{
		path:   strings.Clone(filepath.Clean(path)),
		offset: offset,
		inode:  inode,
		device: device,
	}
}

func cloneCodexCursorState(state codexCursorState) codexCursorState {
	state.model = strings.Clone(state.model)
	state.cwd = strings.Clone(state.cwd)
	state.lastTaskEvent = strings.Clone(state.lastTaskEvent)
	return state
}

func estimateCodexCursorEntryBytes(
	key codexCursorKey,
	state codexCursorState,
) int64 {
	return codexCursorEntryOverheadBytes + int64(
		len(key.path)+
			len(state.model)+
			len(state.cwd)+
			len(state.lastTaskEvent),
	)
}
