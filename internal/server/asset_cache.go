package server

import (
	"context"
	"os"
	"time"

	"github.com/jellydator/ttlcache/v3"

	"go.kenn.io/agentsview/internal/assets"
)

const (
	assetCacheMaxAge        = 7 * 24 * time.Hour
	assetCacheMaxEntries    = uint64(64)
	assetCacheMaxBytes      = uint64(64 << 20)
	assetCacheSweepInterval = time.Minute
)

var readAssetFile = os.ReadFile

var openAssetReadOnly = os.Open

type assetCacheEntry struct {
	body          []byte
	contentType   string
	sourceSize    int64
	sourceModTime time.Time
}

// assetCache keeps recently served asset bytes in memory. Entries expire a
// fixed time after they are stored, hits do not extend that time, and the
// cache evicts its least recently used entry when the entry or byte limit
// would be exceeded.
type assetCache struct {
	items         *ttlcache.Cache[string, *assetCacheEntry]
	maxBytes      uint64
	sweepInterval time.Duration
}

func newAssetCache() *assetCache {
	return newAssetCacheWithLimits(
		assetCacheMaxAge, assetCacheMaxEntries, assetCacheMaxBytes,
	)
}

func newAssetCacheWithLimits(
	maxAge time.Duration, maxEntries, maxBytes uint64,
) *assetCache {
	cost := func(item ttlcache.CostItem[string, *assetCacheEntry]) uint64 {
		return uint64(len(item.Value.body))
	}
	return &assetCache{
		items: ttlcache.New(
			ttlcache.WithTTL[string, *assetCacheEntry](maxAge),
			ttlcache.WithCapacity[string, *assetCacheEntry](maxEntries),
			ttlcache.WithMaxCost(maxBytes, cost),
			ttlcache.WithDisableTouchOnHit[string, *assetCacheEntry](),
		),
		maxBytes:      maxBytes,
		sweepInterval: assetCacheSweepInterval,
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
	item := cache.items.Get(filename)
	if item == nil {
		return nil, false
	}
	entry := item.Value()
	if entry.contentType != contentType ||
		entry.sourceSize != sourceSize ||
		!entry.sourceModTime.Equal(sourceModTime) {
		return nil, false
	}
	return append([]byte(nil), entry.body...), true
}

func (cache *assetCache) put(
	filename, contentType string, body []byte,
	sourceSize int64, sourceModTime time.Time,
) bool {
	if uint64(len(body)) > cache.maxBytes {
		return false
	}
	ref, err := assets.Reference(contentType, body)
	if err != nil || ref != "asset://"+filename {
		return false
	}
	cache.items.Set(filename, &assetCacheEntry{
		body:          append([]byte(nil), body...),
		contentType:   contentType,
		sourceSize:    sourceSize,
		sourceModTime: sourceModTime,
	}, ttlcache.DefaultTTL)
	return true
}

// Run releases expired entries on a fixed sweep interval until ctx is
// cancelled. Lookups already ignore expired entries, so the sweep only
// bounds how long expired bytes stay resident.
func (cache *assetCache) Run(ctx context.Context) {
	ticker := time.NewTicker(cache.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cache.items.DeleteExpired()
		}
	}
}

func (cache *assetCache) len() int {
	return cache.items.Len()
}

func (cache *assetCache) bytes() uint64 {
	var total uint64
	cache.items.Range(func(item *ttlcache.Item[string, *assetCacheEntry]) bool {
		total += uint64(len(item.Value().body))
		return true
	})
	return total
}
