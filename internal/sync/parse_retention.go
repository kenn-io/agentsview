package sync

import (
	"context"
	"os"
	"runtime/debug"
	gosync "sync"
	"sync/atomic"

	"go.kenn.io/agentsview/internal/parser"
	"golang.org/x/sync/semaphore"
)

const (
	defaultParseRetentionBytes       = int64(64 << 20)
	defaultBulkParseRetentionBytes   = int64(128 << 20)
	defaultBulkPendingRetentionBytes = int64(512 << 20)
	parseRetentionFixedBytes         = int64(64 << 10)
	parseRetentionMultiplier         = int64(4)
	parseRetentionScavengeThreshold  = int64(16 << 20)
)

type parseRetentionBudget struct {
	capacity             int64
	pendingCapacity      int64
	weighted             *semaphore.Weighted
	pressure             chan struct{}
	waiters              atomic.Int64
	scavengeEveryAcquire bool
	scavengePending      atomic.Bool
	scavenge             func()
	acquired             atomic.Int64 // total successful acquisitions, for tests
}

func newParseRetentionBudget(capacity int64) *parseRetentionBudget {
	if capacity <= 0 {
		capacity = defaultParseRetentionBytes
	}
	return &parseRetentionBudget{
		capacity: capacity,
		weighted: semaphore.NewWeighted(capacity),
		pressure: make(chan struct{}, 1),
		scavenge: debug.FreeOSMemory,
	}
}

// newBulkParseRetentionBudget returns the byte-weighted budget archive-scale
// passes use (full sync, resync rebuild, remote import processing). Active and
// queued parses use the weighted semaphore while completed results transfer to
// a separately bounded collector batch. It keeps the bulk pass's end-of-pass
// scavenge even when every admitted source is smaller than the default
// budget's large-source threshold.
func newBulkParseRetentionBudget(capacity int64) *parseRetentionBudget {
	budget := newParseRetentionBudget(capacity)
	budget.pendingCapacity = defaultBulkPendingRetentionBytes
	budget.scavengeEveryAcquire = true
	return budget
}

func (budget *parseRetentionBudget) acquire(
	ctx context.Context, sourceBytes int64,
) (*parseRetentionLease, error) {
	weight := budget.weight(sourceBytes)
	retainedBytes := budget.retainedBytes(sourceBytes)
	if budget.weighted.TryAcquire(weight) {
		budget.noteKnownLargeSource(sourceBytes)
		budget.acquired.Add(1)
		return &parseRetentionLease{
			budget: budget, weight: weight, retainedBytes: retainedBytes,
		}, nil
	}
	budget.waiters.Add(1)
	defer budget.waiters.Add(-1)
	select {
	case budget.pressure <- struct{}{}:
	default:
	}
	if err := budget.weighted.Acquire(ctx, weight); err != nil {
		return nil, err
	}
	budget.noteKnownLargeSource(sourceBytes)
	budget.acquired.Add(1)
	return &parseRetentionLease{
		budget: budget, weight: weight, retainedBytes: retainedBytes,
	}, nil
}

func (budget *parseRetentionBudget) noteKnownLargeSource(sourceBytes int64) {
	if budget.scavengeEveryAcquire || sourceBytes >= parseRetentionScavengeThreshold {
		budget.scavengePending.Store(true)
	}
}

func (budget *parseRetentionBudget) scavengeIfNeeded() {
	if budget == nil || !budget.scavengePending.Swap(false) || budget.scavenge == nil {
		return
	}
	budget.scavenge()
}

func (budget *parseRetentionBudget) pressureSignal() <-chan struct{} {
	if budget == nil {
		return nil
	}
	return budget.pressure
}

func (budget *parseRetentionBudget) underPressure() bool {
	return budget != nil && budget.waiters.Load() > 0
}

func (budget *parseRetentionBudget) weight(sourceBytes int64) int64 {
	return min(budget.retainedBytes(sourceBytes), budget.capacity)
}

func (budget *parseRetentionBudget) retainedBytes(sourceBytes int64) int64 {
	limit := max(budget.capacity, budget.pendingCapacity)
	if sourceBytes <= 0 {
		return limit
	}
	if sourceBytes >= (limit-parseRetentionFixedBytes)/parseRetentionMultiplier {
		return limit
	}
	return parseRetentionFixedBytes + sourceBytes*parseRetentionMultiplier
}

type parseRetentionLease struct {
	budget *parseRetentionBudget
	weight int64
	// retainedBytes estimates the parsed payload transferred to an archive-scale
	// collector. It may exceed weight because an oversized source acquires the
	// active parse budget exclusively but must still count at full cost against
	// the collector's separate pending-data bound.
	retainedBytes int64
	once          gosync.Once
}

func (lease *parseRetentionLease) Release() {
	if lease == nil || lease.budget == nil || lease.weight <= 0 {
		return
	}
	lease.once.Do(func() {
		lease.budget.weighted.Release(lease.weight)
	})
}

func releaseParseRetentionLeases(leases []*parseRetentionLease) {
	for _, lease := range leases {
		lease.Release()
	}
}

func parseRetentionSourceBytes(file parser.DiscoveredFile) int64 {
	if file.SourceSize > 0 {
		return file.SourceSize
	}
	path := file.Path
	if file.ProviderSource != nil {
		if providerPath := providerDiscoveredPath(*file.ProviderSource); providerPath != "" {
			path = providerPath
		}
	}
	path = validatedProviderSourceStatPath(path)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0
	}
	return info.Size()
}
