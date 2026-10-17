package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/telemetry"
)

func TestSetupTelemetryExistingContentAndRestart(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "session-one", "project-a")
	cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	broadcaster := server.NewBroadcaster(0)
	sends := 0
	for range 2 {
		runSetupTelemetry(t.Context(), time.Hour, cfg, database, broadcaster, func() error {
			sends++
			return nil
		})
	}
	assert.Equal(t, 1, sends)
}

func TestSetupTelemetryImportAndRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		broadcaster := server.NewBroadcaster(0)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		attempts := make(chan time.Time, 2)
		var sends atomic.Int32
		go func() {
			defer close(done)
			runSetupTelemetry(ctx, time.Hour, cfg, database, broadcaster, func() error {
				attempts <- time.Now()
				if sends.Add(1) == 1 {
					return errors.New("queue full")
				}
				return nil
			})
		}()
		synctest.Wait()
		// An empty first sync leaves setup pending.
		broadcaster.Emit("all")
		synctest.Wait()
		assert.Zero(t, sends.Load())
		// A foreground sync commits visible sessions before its completion event.
		dbtest.SeedSessionWithMessages(t, database, "imported", "project-a", []db.Message{
			{SessionID: "imported", Ordinal: 0, Role: "user", Content: "hello"},
		})
		importedAt := time.Now()
		broadcaster.Emit("sync")
		synctest.Wait()
		assert.EqualValues(t, 1, sends.Load(), "foreground sync captures setup promptly")
		first := <-attempts
		second := <-attempts
		<-done
		assert.Equal(t, importedAt, first)
		assert.Equal(t, time.Hour, second.Sub(first))
		assert.EqualValues(t, 2, sends.Load())
	})
}

func TestSetupTelemetryClaimedEmptyArchiveExits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		require.NoError(t, cfg.ClaimSetupFinished(func() error { return nil }))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			runSetupTelemetry(ctx, time.Hour, cfg, database, server.NewBroadcaster(0), func() error {
				assert.Fail(t, "claimed installation reported again")
				return nil
			})
		}()
		synctest.Wait()
		select {
		case <-done:
		default:
			assert.Fail(t, "claimed installation kept observing an empty archive")
		}
	})
}

func TestSetupTelemetryCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		ctx, cancel := context.WithCancel(t.Context())
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		broadcaster := server.NewBroadcaster(0)
		done := make(chan struct{})
		go func() {
			defer close(done)
			runSetupTelemetry(ctx, time.Hour, cfg, database, broadcaster, func() error {
				assert.Fail(t, "empty archive sent setup telemetry")
				return nil
			})
		}()
		synctest.Wait()
		cancel()
		<-done
	})
}

func TestSetupTelemetryRestoreRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		dbtest.SeedSession(t, database, "restored", "project-a")
		require.NoError(t, database.SoftDeleteSession(t.Context(), "restored"))
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		broadcaster := server.NewBroadcaster(0)
		cfg.Host, cfg.Port = "127.0.0.1", 8080
		srv := server.New(cfg, database, nil, server.WithBroadcaster(broadcaster))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		var sends atomic.Int32
		go func() {
			defer close(done)
			runSetupTelemetry(ctx, time.Hour, cfg, database, broadcaster, func() error {
				sends.Add(1)
				return nil
			})
		}()
		synctest.Wait()
		assert.Zero(t, sends.Load())
		w := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8080/api/v1/sessions/restored/restore", nil)
		req.Header.Set("Origin", "http://127.0.0.1:8080")
		srv.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code, "%s", w.Body.String())
		synctest.Wait()
		assert.EqualValues(t, 1, sends.Load())
		claimed, err := cfg.SetupFinishedClaimed()
		require.NoError(t, err)
		assert.True(t, claimed)
		cancel()
		<-done
	})
}

func TestSetupTelemetryOptOutLeavesClaim(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	dbtest.SeedSession(t, database, "session-one", "project-a")
	cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
	startSetupTelemetry(t.Context(), telemetry.DisabledReporter(), cfg, database, server.NewBroadcaster(0))()
	sent := false
	require.NoError(t, cfg.ClaimSetupFinished(func() error { sent = true; return nil }))
	assert.True(t, sent)
}
