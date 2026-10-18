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
		name, scope             string
		initial, retry, claimed bool
	}{
		{name: "broadcast wake", scope: "sessions"},
		{name: "hourly retry after failure", initial: true, retry: true},
		{name: "empty archive cancellation"},
		{name: "claimed empty archive", claimed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				database := dbtest.OpenTestDB(t)
				if tc.initial {
					dbtest.SeedSession(t, database, "session-one", "project-a")
				}
				cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
				if tc.claimed {
					require.NoError(t, cfg.ClaimSetupFinished(func() error { return nil }))
				}
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
				if tc.claimed {
					assert.Zero(t, sends.Load(), "claimed installation reported again")
					select {
					case <-done:
					default:
						assert.Fail(t, "claimed installation kept observing the archive")
					}
					return
				}
				if !tc.initial {
					assert.Zero(t, sends.Load())
					if tc.scope == "" {
						cancel()
						<-done
						assert.Zero(t, sends.Load())
						return
					}
					dbtest.SeedSession(t, database, "session-one", "project-a")
					broadcaster.Emit(tc.scope)
					synctest.Wait()
				}
				assert.EqualValues(t, 1, sends.Load(), "capture starts without waiting for the timer")
				first := <-attempts
				if tc.retry {
					broadcaster.Emit("sessions")
					synctest.Wait()
					assert.EqualValues(t, 1, sends.Load(), "broadcast after failure must wait for the timer")
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

func TestSetupTelemetryResumesBroadcastWakesAfterCleanRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		database := dbtest.OpenTestDB(t)
		dbtest.SeedSession(t, database, "session-one", "project-a")
		cfg := config.Config{DataDir: t.TempDir(), InstallationID: "install-one"}
		broadcaster := server.NewBroadcaster(0)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		var sends atomic.Int32
		go func() {
			defer close(done)
			runSetupTelemetry(ctx, time.Hour, cfg, database, broadcaster, func() error {
				if sends.Add(1) == 1 {
					return errors.New("queue full")
				}
				return nil
			})
		}()
		synctest.Wait()
		require.EqualValues(t, 1, sends.Load())

		// The hourly retry finds nothing visible and completes cleanly.
		require.NoError(t, database.SoftDeleteSession(t.Context(), "session-one"))
		time.Sleep(time.Hour + time.Minute)
		synctest.Wait()
		require.EqualValues(t, 1, sends.Load())

		_, err := database.RestoreSession(t.Context(), "session-one")
		require.NoError(t, err)
		broadcaster.Emit("sessions")
		synctest.Wait()
		assert.EqualValues(t, 2, sends.Load(), "broadcast after a clean retry must wake the check")
		<-done
	})
}
