package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

// These tests drive the daemon's collection with real file events, so they
// run in the macOS watcher job as well as on Linux and Windows.

func TestBackgroundIntervalHoldsChangesUntilNeeded(t *testing.T) {
	for _, tt := range []struct {
		name string
		need func(t *testing.T, ingestion *daemonIngestion)
	}{
		{
			name: "an archive read syncs them",
			need: func(t *testing.T, ingestion *daemonIngestion) {
				t.Helper()
				ingestion.EnsureFresh(t.Context())
			},
		},
		{
			name: "a live hold syncs them without a read",
			need: func(t *testing.T, ingestion *daemonIngestion) {
				t.Helper()
				t.Cleanup(ingestion.SyncIntervalOverride().Hold(10 * time.Millisecond))
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			database := dbtest.OpenTestDB(t)
			root := t.TempDir()
			writeReloadGeminiSession(t, root, "before")
			cfg := config.Config{
				Host:                   "127.0.0.1",
				AgentDirs:              map[parser.AgentType][]string{parser.AgentGemini: {root}},
				SyncInterval:           time.Second,
				BackgroundSyncInterval: time.Hour,
			}
			engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
			t.Cleanup(engine.Close)
			engine.SyncAll(t.Context(), nil)
			ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
			t.Cleanup(ingestion.Stop)
			ingestion.OpenWatcherDispatch()

			// The first event after a quiet period syncs right away and later
			// ones wait for the background interval. Which event comes first
			// differs by platform: it can be the new session's directory, which
			// leaves the session file itself waiting. A read brings it in
			// either way, and from then on the interval is in force.
			writeReloadGeminiSession(t, root, "first")
			require.Eventually(t, func() bool {
				ingestion.EnsureFresh(t.Context())
				return sessionImported(t, database, "gemini:first")
			}, 20*time.Second, 100*time.Millisecond)
			writeReloadGeminiSession(t, root, "held")
			require.Never(t, func() bool { return sessionImported(t, database, "gemini:held") },
				2*time.Second, 50*time.Millisecond, "nobody needs the change yet")

			require.Eventually(t, func() bool {
				tt.need(t, ingestion)
				return sessionImported(t, database, "gemini:held")
			}, 20*time.Second, 100*time.Millisecond)
		})
	}
}

func TestIdleWakeSyncsCollectedChangesWithoutDiscovery(t *testing.T) {
	var lists atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lists.Add(1)
		_, _ = io.WriteString(w, "<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>")
	}))
	defer remote.Close()
	t.Setenv("AWS_S3_ENDPOINT", remote.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	database := dbtest.OpenTestDB(t)
	root := t.TempDir()
	writeReloadGeminiSession(t, root, "before")
	cfg := config.Config{Host: "127.0.0.1", AgentDirs: map[parser.AgentType][]string{
		parser.AgentGemini: {root},
		// Listing this root is how the test sees a discovery pass.
		parser.AgentClaude: {"s3://bucket/sessions"},
	}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	engine.SyncAll(t.Context(), nil)
	require.Positive(t, lists.Load(), "a full pass lists the remote root")
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	t.Cleanup(ingestion.Stop)
	pauseAfterStartup(t, ingestion)
	lists.Store(0)

	writeReloadGeminiSession(t, root, "overnight")
	require.Never(t, func() bool { return sessionImported(t, database, "gemini:overnight") },
		2*time.Second, 50*time.Millisecond, "a paused daemon syncs nothing")

	require.Eventually(t, func() bool {
		ingestion.EnsureFresh(t.Context())
		return sessionImported(t, database, "gemini:overnight")
	}, 20*time.Second, 100*time.Millisecond, "the wake syncs the change collected during the pause")
	ingestion.applying.Wait()
	assert.Zero(t, lists.Load(), "the wake reads the changed file and does not rediscover every source")
}
