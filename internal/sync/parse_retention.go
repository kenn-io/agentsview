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
	defaultParseRetentionBytes      = int64(64 << 20)
	parseRetentionFixedBytes        = int64(64 << 10)
	parseRetentionMultiplier        = int64(4)
	parseRetentionScavengeThreshold = int64(16 << 20)
	// parseBatchBytesLimit bounds a pending write batch by estimated source
	// bytes as well as by session count: a 5KiB session and a 945MiB session
	// must not both count as "1". Batches flush when either limit is reached.
	parseBatchBytesLimit = defaultParseRetentionBytes
)

type parseRetentionBudget struct {
	capacity int64
	// weighted is nil for the bulk budget, which admits every parse
	// immediately so archive-scale passes run at full worker parallelism.
	weighted        *semaphore.Weighted
	pressure        chan struct{}
	waiters         atomic.Int64
	scavengePending atomic.Bool
	scavenge        func()
	acquired        atomic.Int64 // total successful acquisitions, for tests
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

// newBulkParseRetentionBudget returns the budget archive-scale passes use
// (full sync, resync rebuild, remote import processing). Bulk passes are
// byte-budgeted like every other pass: large sources acquire the whole
// capacity and run exclusively, small sources share it, and batches flush
// when the pending estimated bytes reach the capacity. This keeps a bulk
// pass's peak parse memory bounded by the configured budget plus one
// oversized parse instead of by worker parallelism. The pass still releases
// retained memory back to the OS in one scavenge once it completes.
func newBulkParseRetentionBudget() *parseRetentionBudget {
	return newParseRetentionBudget(defaultParseRetentionBytes)
}

func (budget *parseRetentionBudget) acquire(
	ctx context.Context, sourceBytes int64,
) (*parseRetentionLease, error) {
	weight := budget.weight(sourceBytes)
	if budget.weighted.TryAcquire(weight) {
		budget.noteKnownLargeSource(sourceBytes)
		budget.acquired.Add(1)
		return &parseRetentionLease{budget: budget, weight: weight}, nil
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
	return &parseRetentionLease{budget: budget, weight: weight}, nil
}

func (budget *parseRetentionBudget) noteKnownLargeSource(sourceBytes int64) {
	if sourceBytes >= parseRetentionScavengeThreshold {
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
	if sourceBytes <= 0 {
		return budget.capacity
	}
	if sourceBytes >= (budget.capacity-parseRetentionFixedBytes)/parseRetentionMultiplier {
		return budget.capacity
	}
	return parseRetentionFixedBytes + sourceBytes*parseRetentionMultiplier
}

type parseRetentionLease struct {
	budget *parseRetentionBudget
	weight int64
	once   gosync.Once
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
