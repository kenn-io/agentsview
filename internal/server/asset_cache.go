package server

import (
	"context"
	"os"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/assets"
)

const (
	assetCacheMaxAge     = 7 * 24 * time.Hour
	assetCacheMaxEntries = 64
	assetCacheMaxBytes   = int64(64 << 20)
)

var readAssetFile = os.ReadFile

var openAssetReadOnly = os.Open

type assetCacheEntry struct {
	body          []byte
	contentType   string
	sourceSize    int64
	sourceModTime time.Time
	generatedAt   time.Time
}

type assetCache struct {
	mu         sync.Mutex
	entries    map[string]*assetCacheEntry
	now        func() time.Time
	maxAge     time.Duration
	maxEntries int
	maxBytes   int64
	bytes      int64
	notify     chan struct{}
}

func newAssetCache() *assetCache {
	return &assetCache{
		entries:    make(map[string]*assetCacheEntry),
		now:        time.Now,
		maxAge:     assetCacheMaxAge,
		maxEntries: assetCacheMaxEntries,
		maxBytes:   assetCacheMaxBytes,
		notify:     make(chan struct{}, 1),
	}
}

func (cache *assetCache) read(
	filename, filePath, contentType string,
) ([]byte, error) {
	file, err := openAssetReadOnly(filePath)
	if err != nil {
		return nil, err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil || closeErr != nil || !info.Mode().IsRegular() {
		return readAssetFile(filePath)
	}

	if body, ok := cache.get(
		filename, contentType, info.Size(), info.ModTime(),
	); ok {
		return body, nil
	}

	body, err := readAssetFile(filePath)
	if err != nil {
		return nil, err
	}
	cache.put(filename, contentType, body, info.Size(), info.ModTime())
	return body, nil
}

func (cache *assetCache) get(
	filename, contentType string, sourceSize int64, sourceModTime time.Time,
) ([]byte, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.expireLocked(now)
	entry := cache.entries[filename]
	if entry == nil || entry.contentType != contentType ||
		entry.sourceSize != sourceSize ||
		!entry.sourceModTime.Equal(sourceModTime) {
		return nil, false
	}
	cache.signal()
	return append([]byte(nil), entry.body...), true
}

func (cache *assetCache) put(
	filename, contentType string, body []byte,
	sourceSize int64, sourceModTime time.Time,
) bool {
	if cache.maxEntries <= 0 || int64(len(body)) > cache.maxBytes {
		return false
	}
	ref, err := assets.Reference(contentType, body)
	if err != nil || ref != "asset://"+filename {
		return false
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.expireLocked(now)
	if existing := cache.entries[filename]; existing != nil {
		cache.removeLocked(filename, existing)
	}
	for len(cache.entries) >= cache.maxEntries ||
		cache.bytes+int64(len(body)) > cache.maxBytes {
		oldestID, oldest := cache.oldestLocked()
		if oldest == nil {
			return false
		}
		cache.removeLocked(oldestID, oldest)
	}
	cache.entries[filename] = &assetCacheEntry{
		body:          append([]byte(nil), body...),
		contentType:   contentType,
		sourceSize:    sourceSize,
		sourceModTime: sourceModTime,
		generatedAt:   now,
	}
	cache.bytes += int64(len(body))
	cache.signal()
	return true
}

func (cache *assetCache) expireAndNextWait() (time.Duration, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.expireLocked(now)
	var next time.Time
	for _, entry := range cache.entries {
		deadline := entry.generatedAt.Add(cache.maxAge)
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	if next.IsZero() {
		return 0, false
	}
	return max(next.Sub(now), 0), true
}

func (cache *assetCache) expireLocked(now time.Time) {
	for filename, entry := range cache.entries {
		if !now.Before(entry.generatedAt.Add(cache.maxAge)) {
			cache.removeLocked(filename, entry)
		}
	}
}

func (cache *assetCache) Run(ctx context.Context) {
	for {
		wait, ok := cache.expireAndNextWait()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-cache.notify:
				continue
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-cache.notify:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (cache *assetCache) signal() {
	select {
	case cache.notify <- struct{}{}:
	default:
	}
}

func (cache *assetCache) oldestLocked() (string, *assetCacheEntry) {
	var oldestID string
	var oldest *assetCacheEntry
	for filename, entry := range cache.entries {
		if oldest == nil || entry.generatedAt.Before(oldest.generatedAt) ||
			entry.generatedAt.Equal(oldest.generatedAt) && filename < oldestID {
			oldestID, oldest = filename, entry
		}
	}
	return oldestID, oldest
}

func (cache *assetCache) removeLocked(
	filename string, entry *assetCacheEntry,
) {
	delete(cache.entries, filename)
	cache.bytes -= int64(len(entry.body))
}
