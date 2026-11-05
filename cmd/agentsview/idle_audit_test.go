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
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
)

func TestIdleAuditRunsAfterWakeWithUnavailableS3(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<Error><Code>AccessDenied</Code></Error>"))
	}))
	t.Cleanup(remote.Close)
	t.Setenv("AWS_S3_ENDPOINT", remote.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_REGION", "us-east-1")
	cfg := testConfigWithClaudeFixture(t)
	cfg.AgentDirs[parser.AgentClaude] = append(cfg.AgentDirs[parser.AgentClaude], "s3://bucket/sessions")
	seed := dbtest.OpenTestDBAt(t, cfg.DBPath)
	require.NoError(t, seed.Close())
	markArchiveStale(t, cfg.DBPath)
	database, lock := openTestWriteDB(t, cfg)
	engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{AgentDirs: cfg.AgentDirs, Machine: "test-machine"})
	t.Cleanup(engine.Close)
	ingestion := newDaemonIngestion(t.Context(), cfg, engine, database, server.NewIdleTracker(0, func() {}), nil)
	t.Cleanup(ingestion.Stop)
	ingestion.Suspend()
	ingestion.applying.Wait()
	require.False(t, ingestion.EnsureFresh(t.Context()))
	ingestion.applying.Wait()
	require.True(t, ingestion.Active(), "the reader restored collection")
	require.True(t, database.NeedsResync(), "the archive still needs its startup rebuild")
	var attempts atomic.Int32
	restore := stubLaunchSyncWorker(t, func(_ context.Context, _ config.Config, request syncWorkerRequest, _ func(workerLine)) (workerResult, error) {
		assert.Equal(t, "resync-build", request.Mode)
		attempts.Add(1)
		return workerResult{}, errWorkerSpawn
	})
	defer restore()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	// Use the immediate startup audit path to make the maintenance deadline due.
	ready := make(chan struct{})
	go func() {
		defer close(done)
		startArchiveAudit(ctx, cfg, engine, database, lock, nil, nil, ready, ingestion, nil)
	}()
	assert.Eventually(t, func() bool { return attempts.Load() > 0 }, time.Second, 10*time.Millisecond, "a failed S3 catch-up must not indefinitely stop archive maintenance after the reader resumes collection")
	cancel()
	<-done
	assert.True(t, ingestion.Pending(), "an unavailable source must keep the reader warning while maintenance retries")
}
