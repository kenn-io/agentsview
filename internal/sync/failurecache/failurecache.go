// Package failurecache remembers source files whose parse failed for a reason
// that will not change until the file does, so sync can skip them instead of
// failing the same way on every pass.
package failurecache

import (
	"fmt"
	"maps"
	"sync"

	"go.kenn.io/agentsview/internal/db"
)

// Identity is what "unchanged" means for a source file.
type Identity = db.SourceFailure

// Cache is safe for concurrent use. The zero value is an empty cache.
//
// Memory is authoritative: entries describe source files, not archive rows, so
// they stay valid whichever archive database is live. Flush makes them survive
// a restart.
type Cache struct {
	mu       sync.Mutex
	entries  map[string]Identity
	revision uint64
	// flushedTo and flushedRevision identify the last durable copy, so an
	// unchanged cache costs no write on an idle pass.
	flushedTo       *db.DB
	flushedRevision uint64
}

// Load replaces the cache contents with the failures persisted in source.
func (c *Cache) Load(source *db.DB) error {
	loaded, err := source.LoadSourceFailures()
	if err != nil {
		return fmt.Errorf("loading failure cache: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = loaded
	c.revision++
	c.flushedTo = source
	c.flushedRevision = c.revision
	return nil
}

// Check reports whether key is a known failure whose source still has
// identity id. An entry recorded for a different identity is stale and is
// removed, so the caller parses the changed source again.
func (c *Cache) Check(key string, id Identity) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	recorded, ok := c.entries[key]
	if !ok {
		return false
	}
	if recorded != id {
		delete(c.entries, key)
		c.revision++
		return false
	}
	return true
}

// Record remembers that the source behind key failed while it had identity id.
func (c *Cache) Record(key string, id Identity) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if recorded, ok := c.entries[key]; ok && recorded == id {
		return
	}
	if c.entries == nil {
		c.entries = make(map[string]Identity)
	}
	c.entries[key] = id
	c.revision++
}

// Clear forgets key so its source is parsed again.
func (c *Cache) Clear(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; !ok {
		return
	}
	delete(c.entries, key)
	c.revision++
}

// Flush persists the cache into target. It writes nothing when target already
// holds the current contents.
func (c *Cache) Flush(target *db.DB) error {
	c.mu.Lock()
	if target == c.flushedTo && c.revision == c.flushedRevision {
		c.mu.Unlock()
		return nil
	}
	snapshot := maps.Clone(c.entries)
	revision := c.revision
	c.mu.Unlock()

	if err := target.ReplaceSourceFailures(snapshot); err != nil {
		return fmt.Errorf("persisting failure cache: %w", err)
	}
	c.mu.Lock()
	c.flushedTo = target
	c.flushedRevision = revision
	c.mu.Unlock()
	return nil
}
