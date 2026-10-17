package main

import (
	"context"
	"errors"
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
		// Seed a readable session as an import does before publishing its event.
		dbtest.SeedSessionWithMessages(t, database, "imported", "project-a", []db.Message{
			{SessionID: "imported", Ordinal: 0, Role: "user", Content: "hello"},
		})
		importedAt := time.Now()
		broadcaster.Emit("all")
		first := <-attempts
		second := <-attempts
		<-done
		assert.Equal(t, importedAt, first)
		assert.Equal(t, time.Hour, second.Sub(first))
		assert.EqualValues(t, 2, sends.Load())
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
				t.Error("empty archive sent setup telemetry")
				return nil
			})
		}()
		synctest.Wait()
		cancel()
		<-done
		// The enabled gate must leave a readable archive unclaimed when opted out.
		dbtest.SeedSession(t, database, "session-one", "project-a")
		startSetupTelemetry(t.Context(), telemetry.DisabledReporter(), cfg, database, broadcaster)()
		sent := false
		require.NoError(t, cfg.ClaimSetupFinished(func() error { sent = true; return nil }))
		assert.True(t, sent)
	})
}
