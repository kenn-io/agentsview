package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
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

func TestActivityReportBuildGroupSharesBuildAfterOneWaiterCancels(t *testing.T) {
	group := newActivityReportBuildGroup()
	started := make(chan struct{})
	release := make(chan struct{})
	var builds atomic.Int32
	build := func(ctx context.Context) (activity.CandidateArtifacts, error) {
		if builds.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return activity.CandidateArtifacts{Sessions: []activity.SessionRow{{SessionID: "ok"}}}, nil
		case <-ctx.Done():
			return activity.CandidateArtifacts{}, ctx.Err()
		}
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := group.do(firstCtx, "same", build)
		firstDone <- err
	}()
	<-started
	type result struct {
		artifacts activity.CandidateArtifacts
		err       error
	}
	secondDone := make(chan result, 1)
	go func() {
		artifacts, err := group.do(context.Background(), "same", build)
		secondDone <- result{artifacts: artifacts, err: err}
	}()
	require.Eventually(t, func() bool {
		group.mu.Lock()
		defer group.mu.Unlock()
		return group.flights["same"] != nil && group.flights["same"].waiters == 2
	}, time.Second, time.Millisecond)
	cancelFirst()
	require.ErrorIs(t, <-firstDone, context.Canceled)
	close(release)
	second := <-secondDone
	require.NoError(t, second.err)
	require.Equal(t, "ok", second.artifacts.Sessions[0].SessionID)
	require.Equal(t, int32(1), builds.Load())
}

func TestActivityReportBuildGroupCancelsAbandonedBuild(t *testing.T) {
	group := newActivityReportBuildGroup()
	buildCanceled := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := group.do(ctx, "abandoned", func(ctx context.Context) (
			activity.CandidateArtifacts, error,
		) {
			<-ctx.Done()
			close(buildCanceled)
			return activity.CandidateArtifacts{}, ctx.Err()
		})
		done <- err
	}()
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	select {
	case <-buildCanceled:
	case <-time.After(time.Second):
		require.FailNow(t, "abandoned build was not canceled")
	}
}

func TestActivityReportBuildGroupStartsFreshAfterLastWaiterCancels(t *testing.T) {
	group := newActivityReportBuildGroup()
	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	releaseSecond := make(chan struct{})
	var releaseFirstOnce, releaseSecondOnce sync.Once
	t.Cleanup(func() {
		releaseFirstOnce.Do(func() { close(releaseFirst) })
		releaseSecondOnce.Do(func() { close(releaseSecond) })
	})
	var builds atomic.Int32
	build := func(ctx context.Context) (activity.CandidateArtifacts, error) {
		switch builds.Add(1) {
		case 1:
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			<-releaseFirst
			return activity.CandidateArtifacts{}, ctx.Err()
		case 2:
			close(secondStarted)
			<-releaseSecond
			return activity.CandidateArtifacts{
				Sessions: []activity.SessionRow{{SessionID: "fresh"}},
			}, nil
		default:
			return activity.CandidateArtifacts{
				Sessions: []activity.SessionRow{{SessionID: "duplicate"}},
			}, nil
		}
	}

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() {
		_, err := group.do(firstCtx, "same", build)
		firstDone <- err
	}()
	<-firstStarted
	group.mu.Lock()
	firstFlight := group.flights["same"]
	group.mu.Unlock()
	require.NotNil(t, firstFlight)

	cancelFirst()
	require.ErrorIs(t, <-firstDone, context.Canceled)
	select {
	case <-firstCanceled:
	case <-time.After(time.Second):
		require.FailNow(t, "abandoned build was not canceled")
	}

	type result struct {
		artifacts activity.CandidateArtifacts
		err       error
	}
	secondDone := make(chan result, 1)
	go func() {
		artifacts, err := group.do(context.Background(), "same", build)
		secondDone <- result{artifacts: artifacts, err: err}
	}()
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		require.FailNow(t, "replacement request joined the canceled flight")
	}
	group.mu.Lock()
	secondFlight := group.flights["same"]
	group.mu.Unlock()
	require.NotNil(t, secondFlight)
	require.NotSame(t, firstFlight, secondFlight)

	releaseFirstOnce.Do(func() { close(releaseFirst) })
	select {
	case <-firstFlight.done:
	case <-time.After(time.Second):
		require.FailNow(t, "canceled flight did not exit")
	}
	group.mu.Lock()
	currentFlight := group.flights["same"]
	group.mu.Unlock()
	require.Same(t, secondFlight, currentFlight,
		"old build completion must not delete its replacement")

	thirdDone := make(chan result, 1)
	go func() {
		artifacts, err := group.do(context.Background(), "same", build)
		thirdDone <- result{artifacts: artifacts, err: err}
	}()
	require.Eventually(t, func() bool {
		group.mu.Lock()
		defer group.mu.Unlock()
		return group.flights["same"] == secondFlight && secondFlight.waiters == 2
	}, time.Second, time.Millisecond)
	releaseSecondOnce.Do(func() { close(releaseSecond) })

	for _, completed := range []result{<-secondDone, <-thirdDone} {
		require.NoError(t, completed.err)
		require.Len(t, completed.artifacts.Sessions, 1)
		require.Equal(t, "fresh", completed.artifacts.Sessions[0].SessionID)
	}
	require.Equal(t, int32(2), builds.Load())
}

func TestActivityReportProgressBuildsKeepCallbacksRequestLocal(t *testing.T) {
	store := &progressArtifactStore{
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	srv := &Server{activityReportFlights: newActivityReportBuildGroup()}
	var mu sync.Mutex
	seen := map[string][]int64{"first": {}, "second": {}}
	start := func(name string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := srv.buildActivityArtifacts(
				context.Background(), store, resolvedActivitySelection{},
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
	select {
	case <-store.started:
	case <-time.After(time.Second):
		require.FailNow(t, "first report build did not start")
	}
	second := start("second")
	select {
	case <-store.started:
	case <-time.After(time.Second):
		require.FailNow(t, "second progress request reused the first callback")
	}
	close(store.release)
	require.NoError(t, <-first)
	require.NoError(t, <-second)
	require.Equal(t, int32(2), store.builds.Load())
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen["first"], 1)
	require.Len(t, seen["second"], 1)
	require.NotEqual(t, seen["first"][0], seen["second"][0])
}
