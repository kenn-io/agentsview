package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

type cancelAwareActivityStore struct {
	db.Store
	started  chan struct{}
	canceled chan struct{}
}

type failingActivityReportStore struct {
	db.Store
	probeErr error
	buildErr error
}

func (s *failingActivityReportStore) ActivityReportSourceProbe(
	context.Context,
) (activity.SourceProbe, error) {
	return activity.SourceProbe{}, s.probeErr
}

func (s *failingActivityReportStore) BuildActivityReportArtifacts(
	context.Context,
	db.AnalyticsFilter,
	activity.Query,
	activity.ProgressFunc,
) (activity.CandidateArtifacts, error) {
	return activity.CandidateArtifacts{}, s.buildErr
}

func (s *failingActivityReportStore) EncodeActivityReportToken([]byte) (string, error) {
	return "signed-report-id", nil
}

func (s *failingActivityReportStore) DecodeActivityReportToken(string) ([]byte, error) {
	return nil, errors.New("unused")
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

func TestActivityReportRedactsProbeErrors(t *testing.T) {
	const privateDetail = "database /private/archive.db probe failed"
	store := &failingActivityReportStore{probeErr: errors.New(privateDetail)}
	server := newRoutedTestServerWithStore(t, store)

	recorder := serveGet(t, server,
		"/api/v1/activity/report?preset=day&date=2026-07-14&timezone=UTC")

	assertRecorderStatus(t, recorder, http.StatusInternalServerError)
	assert.Contains(t, recorder.Body.String(), "internal error")
	assert.NotContains(t, recorder.Body.String(), privateDetail)
}

func TestActivityReportRedactsBuildErrors(t *testing.T) {
	const privateDetail = "sql: failed to read /private/archive.db"
	for _, test := range []struct {
		name   string
		accept string
	}{
		{name: "json", accept: "application/json"},
		{name: "sse", accept: "text/event-stream"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &failingActivityReportStore{buildErr: errors.New(privateDetail)}
			server := newRoutedTestServerWithStore(t, store)
			req := httptest.NewRequest(http.MethodGet,
				"/api/v1/activity/report?preset=day&date=2026-07-14&timezone=UTC",
				nil)
			req.Header.Set("Accept", test.accept)
			recorder := httptest.NewRecorder()

			server.mux.ServeHTTP(recorder, req)

			if strings.Contains(test.accept, "text/event-stream") {
				assertRecorderStatus(t, recorder, http.StatusOK)
				assert.Contains(t, recorder.Body.String(), "event: error")
			} else {
				assertRecorderStatus(t, recorder, http.StatusInternalServerError)
			}
			assert.Contains(t, recorder.Body.String(), "internal error")
			assert.NotContains(t, recorder.Body.String(), privateDetail)
		})
	}
}

func requireChannelClosed(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		require.FailNow(t, message)
	}
}
