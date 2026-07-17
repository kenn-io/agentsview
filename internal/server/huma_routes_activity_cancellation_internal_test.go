package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

type cancelAwareActivityStore struct {
	db.Store
	started  chan struct{}
	canceled chan struct{}
}

func (s *cancelAwareActivityStore) GetActivityReport(
	ctx context.Context,
	_ db.AnalyticsFilter,
	_ activity.Query,
) (activity.Report, error) {
	close(s.started)
	<-ctx.Done()
	close(s.canceled)
	return activity.Report{}, ctx.Err()
}

func TestActivityReportPropagatesRequestCancellationToStore(t *testing.T) {
	store := &cancelAwareActivityStore{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	server := newRoutedTestServerWithStore(t, store)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req := httptest.NewRequest(
		http.MethodGet,
		"/api/v1/activity/report?preset=day&date=2026-07-14&timezone=UTC",
		nil,
	).WithContext(ctx)
	done := make(chan struct{})

	go func() {
		server.mux.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	requireChannelClosed(t, store.started, "activity query did not start")
	cancel()
	requireChannelClosed(t, store.canceled, "store did not observe cancellation")
	requireChannelClosed(t, done, "activity handler did not return")
}

func requireChannelClosed(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		require.FailNow(t, message)
	}
}
