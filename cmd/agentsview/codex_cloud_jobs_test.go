package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/poller"
)

func TestCodexCloudServeStatusLines(t *testing.T) {
	t.Run("one line per row", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/api/v1/codex-cloud/status", r.URL.Path)
			assert.Equal(t, "Bearer secret", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"key":"k","homes":["/homes/a","/homes/b"],"state":"importing","found":3},` +
				`{"key":"/homes/c","homes":["/homes/c"],"state":"error","message":"Signed out."},` +
				`{"key":"codex-cloud","homes":[],"state":"off_disabled","message":"Off: enabled is false."},` +
				`{"key":"j","homes":["/homes/d"],"state":"importing","imported":0,"total":1}]`))
		}))
		defer srv.Close()
		assert.Equal(t, []string{
			"  codex-cloud /homes/a, /homes/b: Importing, found 3 chats so far",
			"  codex-cloud /homes/c: Signed out.",
			"  codex-cloud: Off: enabled is false.",
			"  codex-cloud /homes/d: Importing 0 of 1 chat",
		}, codexCloudStatusLines(t.Context(), srv.URL, "secret"))
	})
	t.Run("an older daemon adds nothing", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		defer srv.Close()
		assert.Empty(t, codexCloudStatusLines(t.Context(), srv.URL, ""))
	})
	for _, tt := range []struct {
		name   string
		status int
		want   string
	}{
		{
			"a rejected token says so", http.StatusUnauthorized,
			"  codex-cloud status unavailable: the server did not accept this command's auth token.",
		},
		{
			"another failure names its status", http.StatusInternalServerError,
			"  codex-cloud status unavailable: the server answered with HTTP status 500.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			assert.Equal(t, []string{tt.want}, codexCloudStatusLines(t.Context(), srv.URL, ""))
		})
	}
	t.Run("an unreachable server says so", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		assert.Equal(t, []string{"  codex-cloud status unavailable: the server could not be reached."},
			codexCloudStatusLines(t.Context(), srv.URL, ""))
	})
}

// TestCodexCloudTriggerCoalesces: several joined turns completing while a
// sync run is active queue one more run, and a trigger already queued is
// not an error.
func TestCodexCloudTriggerCoalesces(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	var runs atomic.Int32
	scheduler := poller.Start(ctx, poller.Job{
		Name: codexCloudSyncJobName, Interval: time.Hour, RunAtStart: true,
		Run: func(ctx context.Context) error {
			runs.Add(1)
			started <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	})
	defer scheduler.Wait()
	defer cancel()
	trigger := &codexCloudTrigger{}
	trigger.scheduler.Store(scheduler)
	<-started

	results := make(chan error, 3)
	for range 3 {
		go func() { results <- trigger.Trigger() }()
	}
	for range 2 {
		require.NoError(t, <-results, "a trigger already queued counts as success")
	}
	close(release)
	require.NoError(t, <-results)
	assert.Equal(t, int32(2), runs.Load(), "three completions add one run")
}
