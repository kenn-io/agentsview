package server

import (
	"context"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/activity"
)

const (
	activityReportCacheIdle     = 15 * time.Minute
	activityReportCacheEntries  = 3
	activityReportCacheRows     = 750_000
	activityReportCacheMaxBytes = int64(256 << 20)
)

type activityReportCacheEntry struct {
	artifacts  activity.CandidateArtifacts
	digest     string
	size       int64
	lastAccess time.Time
	orders     map[activityReportOrderKey][]int
}

type activityReportOrderKey struct {
	sort      activity.SessionSort
	direction string
	bucket    int
}

type activityReportCache struct {
	mu         sync.Mutex
	entries    map[string]*activityReportCacheEntry
	now        func() time.Time
	idle       time.Duration
	maxEntries int
	maxRows    int
	maxBytes   int64
	rows       int
	bytes      int64
	notify     chan struct{}
}

func newActivityReportCache() *activityReportCache {
	return &activityReportCache{
		entries: make(map[string]*activityReportCacheEntry), now: time.Now,
		idle: activityReportCacheIdle, maxEntries: activityReportCacheEntries,
		maxRows: activityReportCacheRows, maxBytes: activityReportCacheMaxBytes,
		notify: make(chan struct{}, 1),
	}
}

// Run releases idle entries even when no later request touches the cache. The
// server owns this loop and cancels it when Serve returns.
func (cache *activityReportCache) Run(ctx context.Context) {
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

func (cache *activityReportCache) expireAndNextWait() (time.Duration, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.expireLocked(now)
	var next time.Time
	for _, entry := range cache.entries {
		deadline := entry.lastAccess.Add(cache.idle)
		if next.IsZero() || deadline.Before(next) {
			next = deadline
		}
	}
	if next.IsZero() {
		return 0, false
	}
	return max(next.Sub(now), 0), true
}

func (cache *activityReportCache) signal() {
	select {
	case cache.notify <- struct{}{}:
	default:
	}
}

func (cache *activityReportCache) get(
	reportID string,
) (activity.CandidateArtifacts, string, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.expireLocked(now)
	entry := cache.entries[reportID]
	if entry == nil {
		return activity.CandidateArtifacts{}, "", false
	}
	entry.lastAccess = now
	cache.signal()
	return entry.artifacts, entry.digest, true
}

func (cache *activityReportCache) put(
	reportID, digest string, artifacts activity.CandidateArtifacts,
) bool {
	size := activity.EstimatedArtifactBytes(artifacts)
	rows := len(artifacts.Sessions)
	if rows > cache.maxRows || size > cache.maxBytes {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.expireLocked(now)
	if existing := cache.entries[reportID]; existing != nil {
		cache.removeLocked(reportID, existing)
	}
	for len(cache.entries) >= cache.maxEntries ||
		cache.rows+rows > cache.maxRows ||
		cache.bytes+size > cache.maxBytes {
		oldestID, oldest := cache.oldestLocked()
		if oldest == nil {
			break
		}
		cache.removeLocked(oldestID, oldest)
	}
	cache.entries[reportID] = &activityReportCacheEntry{
		artifacts: artifacts, digest: digest, size: size, lastAccess: now,
		orders: make(map[activityReportOrderKey][]int),
	}
	cache.rows += rows
	cache.bytes += size
	cache.signal()
	return true
}

func (cache *activityReportCache) page(
	reportID string, options activity.SessionPageOptions,
) (activity.SessionPage, bool, error) {
	options, err := activity.NormalizeSessionPageOptions(options)
	if err != nil {
		return activity.SessionPage{}, false, err
	}
	bucket := -1
	if options.Bucket != nil {
		bucket = *options.Bucket
	}
	key := activityReportOrderKey{
		sort: options.Sort, direction: options.Direction, bucket: bucket,
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := cache.now()
	cache.expireLocked(now)
	entry := cache.entries[reportID]
	if entry == nil {
		return activity.SessionPage{}, false, nil
	}
	entry.lastAccess = now
	cache.signal()
	order := entry.orders[key]
	if order == nil {
		order, err = activity.OrderSessions(
			entry.artifacts.Sessions, entry.artifacts.Membership, options,
		)
		if err != nil {
			return activity.SessionPage{}, true, err
		}
		orderBytes := int64(len(order) * 8)
		if cache.bytes+orderBytes <= cache.maxBytes {
			entry.orders[key] = order
			entry.size += orderBytes
			cache.bytes += orderBytes
		}
	}
	page, err := activity.PageSessionsFromOrder(
		entry.artifacts.Sessions, order, options,
	)
	return page, true, err
}

func (cache *activityReportCache) expireLocked(now time.Time) {
	for reportID, entry := range cache.entries {
		if now.Sub(entry.lastAccess) >= cache.idle {
			cache.removeLocked(reportID, entry)
		}
	}
}

func (cache *activityReportCache) oldestLocked() (string, *activityReportCacheEntry) {
	var oldestID string
	var oldest *activityReportCacheEntry
	for reportID, entry := range cache.entries {
		if oldest == nil || entry.lastAccess.Before(oldest.lastAccess) ||
			entry.lastAccess.Equal(oldest.lastAccess) && reportID < oldestID {
			oldestID, oldest = reportID, entry
		}
	}
	return oldestID, oldest
}

func (cache *activityReportCache) removeLocked(
	reportID string, entry *activityReportCacheEntry,
) {
	delete(cache.entries, reportID)
	cache.rows -= len(entry.artifacts.Sessions)
	cache.bytes -= entry.size
}
