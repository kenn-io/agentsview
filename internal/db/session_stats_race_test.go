//go:build race

package db

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOutcomeStatsWriterCloseRaceDoesNotPanic exercises the maintenance
// writer transition under the race detector. The normal suite covers the
// closed-writer state deterministically without paying for hundreds of git
// scans on every platform.
func TestOutcomeStatsWriterCloseRaceDoesNotPanic(t *testing.T) {
	skipIfNoGit(t)
	d := testDB(t)
	repo := statsOutcomeRepo(t)
	insertSessionFixture(t, d, sessionFixture{
		id: "writer-race", agent: "claude", userMsgs: 5,
		startedAt: hoursAgo(5), cwd: repo,
	})

	done := make(chan struct{})
	toggleErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			if err := d.CloseWriter(); err != nil {
				toggleErr <- err
				return
			}
			if err := d.ReopenWriter(); err != nil {
				toggleErr <- err
				return
			}
		}
	})

	for range 300 {
		// The race detector validates synchronization; transient errors while
		// the writer is closed are acceptable for this stress case.
		_, _ = d.GetSessionStats(context.Background(), StatsFilter{
			Since: "28d", IncludeGitOutcomes: true,
		})
	}
	close(done)
	wg.Wait()
	select {
	case err := <-toggleErr:
		require.NoError(t, err, "writer toggling failed")
	default:
	}
}
