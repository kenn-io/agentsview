//go:build fts5

package db_test

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db/testdata/usagefixture"
)

// This fixture uses public archive/cache calls so the same benchmark can
// measure the pre-journal implementation. About 3% of one-row sessions span
// midnight, giving roughly 30,900 daily rows at 30,000 sessions.
func BenchmarkUsageRollupJournaled(b *testing.B) {
	count := 300
	if raw := os.Getenv("AGENTSVIEW_BENCH_SESSIONS"); raw != "" {
		var err error
		count, err = strconv.Atoi(raw)
		require.NoError(b, err)
		require.Positive(b, count)
	}
	for _, dailyRows := range []int{1, 3} {
		b.Run(fmt.Sprintf("rows-%d", dailyRows), func(b *testing.B) {
			database, filter, want := usagefixture.SeedJournal(b, count, dailyRows)
			for _, readers := range []int{1, 7} {
				b.Run(fmt.Sprintf("readers-%d", readers), func(b *testing.B) {
					b.ReportAllocs()
					for range b.N {
						if readers == 1 {
							got, err := database.GetDailyUsage(b.Context(), filter)
							require.NoError(b, err)
							require.Equal(b, want, got)
							continue
						}
						var wg sync.WaitGroup
						errors := make(chan error, readers)
						for range readers {
							wg.Go(func() {
								got, err := database.GetDailyUsage(b.Context(), filter)
								// Fatal assertions belong to the benchmark goroutine.
								if err != nil {
									errors <- err
									return
								}
								assert.Equal(b, want, got)
							})
						}
						wg.Wait()
						close(errors)
						for err := range errors {
							require.NoError(b, err)
						}
					}
				})
			}
		})
	}
}
