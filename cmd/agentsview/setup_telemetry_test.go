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
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
)

func TestSetupTelemetryWakeSourcesAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name, scope    string
		initial, retry bool
	}{
		{name: "initial visible content", initial: true},
		{name: "sync and timer retry", scope: "sync", retry: true},
		{name: "sessions", scope: "sessions"},
		{name: "empty archive cancellation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				database := dbtest.OpenTestDB(t)
				if tc.initial {
					dbtest.SeedSession(t, database, "session-one", "project-a")
				}
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
						if sends.Add(1) == 1 && tc.retry {
							return errors.New("queue full")
						}
						return nil
					})
				}()
				synctest.Wait()
				if !tc.initial {
					assert.Zero(t, sends.Load())
					if tc.scope == "" {
						cancel()
						<-done
						assert.Zero(t, sends.Load())
						return
					}
					dbtest.SeedSession(t, database, "session-one", "project-a")
					if tc.scope == "sync" {
						broadcaster.Emit("all")
						synctest.Wait()
						assert.Zero(t, sends.Load(), "all leaves visible content pending")
					}
					broadcaster.Emit(tc.scope)
					synctest.Wait()
				}
				assert.EqualValues(t, 1, sends.Load(), "capture starts without waiting for the timer")
				first := <-attempts
				if tc.retry {
					second := <-attempts
					assert.Equal(t, time.Hour, second.Sub(first))
				}
				<-done
				want := int32(1)
				if tc.retry {
					want = 2
				}
				assert.Equal(t, want, sends.Load())
			})
		})
	}
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
