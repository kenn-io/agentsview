package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud"
	"go.kenn.io/agentsview/internal/poller"
	"go.kenn.io/agentsview/internal/server"
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
		token  string
		want   string
	}{
		{
			"a missing token says so", http.StatusUnauthorized, "",
			"  codex-cloud status unavailable: the server needs an auth token, and this command has none. " +
				"Set auth_token in config.toml, or AGENTSVIEW_AUTH_TOKEN, to the server's token.",
		},
		{
			"a rejected token says so", http.StatusUnauthorized, "stale",
			"  codex-cloud status unavailable: the server did not accept this command's auth token. " +
				"Check that auth_token in config.toml, or AGENTSVIEW_AUTH_TOKEN, matches the server's.",
		},
		{
			"a refusal does not blame the token", http.StatusForbidden, "secret",
			"  codex-cloud status unavailable: the server answered with HTTP status 403. The server log, serve.log in the AgentsView data directory, may say why.",
		},
		{
			"another failure names its status", http.StatusInternalServerError, "",
			"  codex-cloud status unavailable: the server answered with HTTP status 500. The server log, serve.log in the AgentsView data directory, may say why.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()
			assert.Equal(t, []string{tt.want}, codexCloudStatusLines(t.Context(), srv.URL, tt.token))
		})
	}
	t.Run("an unreachable server says so", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		assert.Equal(t, []string{"  codex-cloud status unavailable: the server could not be reached. " +
			"It may have just stopped; run `agentsview serve status` again to check."},
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

// A background daemon reaps itself when idle. Each Codex cloud job holds
// a work lease while it runs, so a long import is not cut off.
func TestCodexCloudJobsKeepTheDaemonAwakeWhileRunning(t *testing.T) {
	for _, tt := range []struct {
		name string
		job  func(gates func() codexcloud.Gates, idle *server.IdleTracker) poller.Job
	}{
		{"sync", func(gates func() codexcloud.Gates, idle *server.IdleTracker) poller.Job {
			return codexCloudSyncJob(&codexcloud.Syncer{Gates: gates, Status: codexcloud.NewStatusBoard()}, idle)
		}},
		{"live", func(gates func() codexcloud.Gates, idle *server.IdleTracker) poller.Job {
			return codexCloudLiveJob(&codexcloud.Monitor{Gates: gates}, idle)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				running, release := make(chan struct{}), make(chan struct{})
				// The job blocks in its first step, reading the gates, until
				// the test lets it finish.
				gates := func() codexcloud.Gates {
					close(running)
					<-release
					return codexcloud.Gates{}
				}
				idled := make(chan struct{})
				tracker := server.NewIdleTracker(10*time.Millisecond, func() { close(idled) })
				job := tt.job(gates, tracker)
				finished := make(chan error, 1)
				go func() { finished <- job.Run(t.Context()) }()
				<-running
				go tracker.Run(t.Context())

				// Fake time runs well past the idle timeout while the job holds
				// its lease.
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case <-idled:
					require.FailNow(t, "the daemon idled out while the job ran")
				default:
				}
				close(release)
				require.NoError(t, <-finished)
				time.Sleep(time.Second)
				synctest.Wait()
				select {
				case <-idled:
				default:
					require.FailNow(t, "the daemon never idled after the job finished")
				}
			})
		})
	}
}
