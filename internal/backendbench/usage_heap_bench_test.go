//go:build benchdb

package backendbench

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// BenchmarkDailyUsageHeap samples Go heap during simultaneous cold requests.
// Run with -benchtime=1x. It includes fixture heap, excludes driver-native memory,
// and samples rather than measuring an exact peak.
func BenchmarkDailyUsageHeap(b *testing.B) {
	ctx := context.Background()
	stores := setupBenchmarkStores(ctx, b, benchmarkFixtureFromEnv(b))
	for _, backend := range stores {
		b.Run(backend.name, func(b *testing.B) {
			for _, workers := range []int{1, 4} {
				b.Run(fmt.Sprint(workers), func(b *testing.B) {
					b.StopTimer()
					runtime.GC()
					runtime.GC()
					var stats runtime.MemStats
					runtime.ReadMemStats(&stats)
					baseline := stats.HeapAlloc
					peak := baseline
					stop, sampled := make(chan struct{}), make(chan struct{})
					go func() {
						defer close(sampled)
						ticker := time.NewTicker(5 * time.Millisecond)
						defer ticker.Stop()
						for {
							select {
							case <-stop:
								return
							case <-ticker.C:
								var sample runtime.MemStats
								runtime.ReadMemStats(&sample)
								peak = max(peak, sample.HeapAlloc)
							}
						}
					}()
					b.StartTimer()
					for range b.N {
						var wg sync.WaitGroup
						errors := make(chan error, workers)
						start := make(chan struct{})
						for range workers {
							wg.Go(func() {
								<-start
								result, err := backend.store.GetDailyUsage(ctx, db.UsageFilter{Timezone: "UTC"})
								if err == nil && len(result.Daily) == 0 {
									err = fmt.Errorf("expected usage days")
								}
								errors <- err
							})
						}
						close(start)
						wg.Wait()
						close(errors)
						for err := range errors {
							if err != nil {
								b.Error(err)
							}
						}
					}
					b.StopTimer()
					close(stop)
					<-sampled
					runtime.ReadMemStats(&stats)
					peak = max(peak, stats.HeapAlloc)
					b.ReportMetric(float64(baseline), "baseline-heap-B")
					b.ReportMetric(float64(peak), "sampled-peak-heap-B")
					runtime.GC()
					runtime.ReadMemStats(&stats)
					b.ReportMetric(float64(stats.HeapAlloc), "after-one-GC-B")
					runtime.GC()
					runtime.ReadMemStats(&stats)
					b.ReportMetric(float64(stats.HeapAlloc), "after-two-GC-B")
				})
			}
		})
	}
}
