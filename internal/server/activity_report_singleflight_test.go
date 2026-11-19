package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/kept"
)

type progressArtifactStore struct {
	started chan struct{}
	release chan struct{}
	builds  atomic.Int32
}

func (store *progressArtifactStore) BuildActivityReportArtifacts(
	ctx context.Context,
	_ db.AnalyticsFilter,
	_ activity.Query,
	onProgress activity.ProgressFunc,
) (activity.CandidateArtifacts, error) {
	call := store.builds.Add(1)
	onProgress(activity.Progress{RowsProcessed: int64(call)})
	store.started <- struct{}{}
	select {
	case <-store.release:
		return activity.CandidateArtifacts{}, nil
	case <-ctx.Done():
		return activity.CandidateArtifacts{}, ctx.Err()
	}
}

func TestActivityReportProgressBuildsKeepCallbacksRequestLocal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := &progressArtifactStore{
			started: make(chan struct{}, 2),
			release: make(chan struct{}),
		}
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(store.release) }) }
		t.Cleanup(release)
		srv := &Server{activityReportFlights: &kept.Flights[activity.CandidateArtifacts]{}}
		var mu sync.Mutex
		seen := map[string][]int64{"first": {}, "second": {}}
		start := func(name string) <-chan error {
			done := make(chan error, 1)
			go func() {
				_, err := srv.buildActivityArtifacts(
					t.Context(), store, resolvedActivitySelection{},
					activity.SourceProbe{}, func(progress activity.Progress) {
						mu.Lock()
						seen[name] = append(seen[name], progress.RowsProcessed)
						mu.Unlock()
					},
				)
				done <- err
			}()
			return done
		}

		first := start("first")
		synctest.Wait()
		select {
		case <-store.started:
		default:
			require.FailNow(t, "first report build did not start")
		}
		second := start("second")
		synctest.Wait()
		select {
		case <-store.started:
		default:
			require.FailNow(t, "second progress request reused the first callback")
		}
		release()
		require.NoError(t, <-first)
		require.NoError(t, <-second)
		require.Equal(t, int32(2), store.builds.Load())
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, seen["first"], 1)
		require.Len(t, seen["second"], 1)
		require.NotEqual(t, seen["first"][0], seen["second"][0])
	})
}
