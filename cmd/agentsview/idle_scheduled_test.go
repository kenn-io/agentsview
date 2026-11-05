package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestIdlePendingRecoveryStillRecomputesDeferredSignals(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
	}))
	t.Cleanup(remote.Close)
	t.Setenv("AWS_S3_ENDPOINT", remote.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	database := dbtest.OpenTestDB(t)
	cfg := config.Config{AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {"s3://bucket/sessions"}}}
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	ingestion.Suspend()
	ingestion.applying.Wait()
	require.False(t, ingestion.EnsureFresh(t.Context()))
	ingestion.applying.Wait()
	dbtest.SeedSession(t, database, "deferred", "project")
	require.NoError(t, database.UpdateSessionSignals(t.Context(), "deferred", db.SessionSignalUpdate{SignalsPendingSince: new("1990-01-01T00:00:00Z")}))
	before, err := database.PendingSignalSessions(t.Context(), "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	require.Equal(t, []string{"deferred"}, before)
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			startPeriodicSync(ctx, cfg, ingestion, engine, database, nil, nil, false, nil, nil)
		}()
		time.Sleep(15 * time.Minute)
		synctest.Wait()
		cancel()
		<-finished
		ingestion.Stop()
	})
	after, err := database.PendingSignalSessions(t.Context(), "2026-01-01T00:00:00Z")
	require.NoError(t, err)
	assert.Empty(t, after, "a failed source must not displace deferred signal recomputation")
}
