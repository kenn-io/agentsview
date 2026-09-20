package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type delayedUsageStore struct {
	db.Store
	delay time.Duration
}

func (s delayedUsageStore) GetDailyUsage(ctx context.Context, f db.UsageFilter) (db.DailyUsageResult, error) {
	select {
	case <-ctx.Done():
		return db.DailyUsageResult{}, ctx.Err()
	case <-time.After(s.delay):
		return s.Store.GetDailyUsage(ctx, f)
	}
}

func (s delayedUsageStore) GetTopSessionsByCost(ctx context.Context, f db.UsageFilter, limit int) ([]db.TopSessionEntry, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.delay):
		return s.Store.GetTopSessionsByCost(ctx, f, limit)
	}
}

func TestUsageSummaryWaitsForPreparationBeyondWriteTimeout(t *testing.T) {
	for _, path := range []string{
		"/summary", "/summary/stream", "/top-sessions",
		"/comparison", "/pairwise-comparison",
	} {
		t.Run(path, func(t *testing.T) {
			s := testServer(t, 10*time.Millisecond)
			s.db = delayedUsageStore{Store: s.db, delay: 50 * time.Millisecond}
			s.sessions = service.NewReadOnlyBackend(s.db)
			ts := httptest.NewServer(s.Handler())
			t.Cleanup(ts.Close)
			params := oneDayUsageRange + "&current_microdollars=0&left_dimension=model&left_value=model-a&right_dimension=model&right_value=model-b"
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/api/v1/usage"+path+"?"+params, nil)
			require.NoError(t, err)
			req.Host = "127.0.0.1:0"
			resp, err := ts.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			if path == "/summary" || path == "/summary/stream" {
				assert.Contains(t, string(body), `"daily":[]`)
			}
			if path == "/summary/stream" {
				assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
				assert.Contains(t, string(body), "event: progress\n")
				assert.Contains(t, string(body), "event: done\n")
				assert.Contains(t, string(body), "Reading archived sessions for this report")
			}
		})
	}
}

func TestUsageSummaryStreamReportsBeforeQueryFinishesAndCancels(t *testing.T) {
	s := testServer(t, time.Second)
	entered := make(chan struct{})
	canceled := make(chan struct{})
	s.sessions = service.NewReadOnlyBackend(&waitingUsageStore{
		Store: s.db, entered: entered, canceled: canceled,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/api/v1/usage/summary/stream?"+oneDayUsageRange, nil)
	require.NoError(t, err)
	req.Host = "127.0.0.1:0"
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data := make([]byte, len("event: progress\n"))
	_, err = io.ReadFull(resp.Body, data)
	require.NoError(t, err)
	assert.Equal(t, "event: progress\n", string(data), "progress must arrive while the query is blocked")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "usage query did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "disconnect did not cancel the usage query")
	}
}

type waitingUsageStore struct {
	db.Store
	entered, canceled chan struct{}
}

func (s *waitingUsageStore) GetDailyUsage(ctx context.Context, _ db.UsageFilter) (db.DailyUsageResult, error) {
	close(s.entered)
	<-ctx.Done()
	close(s.canceled)
	return db.DailyUsageResult{}, ctx.Err()
}
