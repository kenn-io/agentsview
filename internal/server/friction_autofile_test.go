package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/server"
)

func TestFrictionJobFilesAutomaticallyOnTheHub(t *testing.T) {
	tests := []struct {
		name     string
		autoFile bool
		hub      bool
		filed    bool
	}{
		{name: "auto_file_on_hub", autoFile: true, hub: true, filed: true},
		{name: "auto_file_off", hub: true},
		{name: "not_the_hub", autoFile: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
			runner := &review.Runner{Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
			te := setupWithServerOpts(t, []server.Option{
				server.WithFriction(runner, nil),
				server.WithKataConn(kata.NewConn(kata.Config{
					Enabled: true, Hub: tt.hub, Endpoint: s.Endpoint(), Project: katatest.DefaultProjectName,
				})),
			}, func(c *config.Config) {
				c.Friction.BackfillDays = 7
				c.Friction.Kata.AutoFile = tt.autoFile
			})
			runner.Store = te.db
			const date = "2026-09-14"
			seedFrictionDigest(t, te.db, date)
			fp := frictionTestSnapshot(date).Signals[0].Fingerprint()

			require.NoError(t, review.Job(runner, nil).Run(t.Context()))
			if !tt.filed {
				assert.Nil(t, patternIssue(t, te, fp))
				assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"))
				return
			}
			issue := patternIssue(t, te, fp)
			require.NotNil(t, issue, "the hourly job filed the recent pattern")
			assert.Equal(t, "created", issue.Source)
		})
	}
}

func TestFrictionJobDoesNotFileWhereLinksCannotBeSaved(t *testing.T) {
	s := katatest.New(t)
	dir := tempDirWithRetryCleanup(t)
	cfg := config.Config{
		Host: "127.0.0.1", DataDir: dir, DBPath: filepath.Join(dir, "test.db"), WriteTimeout: 30 * time.Second,
	}
	cfg.Friction.BackfillDays = 7
	cfg.Friction.Kata.AutoFile = true
	database := dbtest.OpenTestDBAt(t, cfg.DBPath)
	seedFrictionDigest(t, database, "2026-09-14")
	fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
	runner := &review.Runner{Store: database, Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
	server.New(cfg, linksUnwritableStore{database}, nil,
		server.WithFriction(runner, nil),
		server.WithKataConn(kata.NewConn(kata.Config{
			Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: katatest.DefaultProjectName,
		})))

	require.NoError(t, review.Job(runner, nil).Run(t.Context()))
	assert.Empty(t, s.RequestsMatching(http.MethodPost, "/issues"), "no issue is created without a place for its link")
}

// The local daemon starts the friction job after the server exists and
// installs it with WithFriction then; automatic filing must still run.
func TestFrictionJobFilesWhenInstalledAfterTheServer(t *testing.T) {
	s := katatest.New(t)
	fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
	runner := &review.Runner{Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
	te := setupWithServerOpts(t, []server.Option{
		server.WithKataConn(kata.NewConn(kata.Config{
			Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: katatest.DefaultProjectName,
		})),
	}, func(c *config.Config) {
		c.Friction.BackfillDays = 7
		c.Friction.Kata.AutoFile = true
	})
	runner.Store = te.db
	const date = "2026-09-14"
	seedFrictionDigest(t, te.db, date)
	server.WithFriction(runner, nil)(te.srv)

	require.NoError(t, review.Job(runner, nil).Run(t.Context()))
	require.NotNil(t, patternIssue(t, te, frictionTestSnapshot(date).Signals[0].Fingerprint()))
}

// A daemon that idles out while automatic filing waits on Kata would stop
// between creating an issue and saving its link, so the pass holds a work
// lease.
func TestAutomaticFilingKeepsTheDaemonAwake(t *testing.T) {
	s := katatest.New(t)
	target, err := url.Parse(s.Endpoint())
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues") {
			time.Sleep(time.Second)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(slow.Close)

	idled := make(chan struct{}, 1)
	tracker := server.NewIdleTracker(200*time.Millisecond, func() { idled <- struct{}{} })
	fixed := time.Date(2026, 9, 16, 2, 30, 0, 0, time.UTC)
	runner := &review.Runner{Loc: time.UTC, Now: func() time.Time { return fixed }, BackfillDays: 7}
	te := setupWithServerOpts(t, []server.Option{
		server.WithFriction(runner, nil), server.WithIdleTracker(tracker),
		server.WithKataConn(kata.NewConn(kata.Config{
			Enabled: true, Hub: true, Endpoint: slow.URL, Project: katatest.DefaultProjectName,
		})),
	}, func(c *config.Config) {
		c.Friction.BackfillDays = 7
		c.Friction.Kata.AutoFile = true
	})
	runner.Store = te.db
	seedFrictionDigest(t, te.db, "2026-09-14")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tracker.Touch()
	go tracker.Run(ctx)

	require.NoError(t, review.Job(runner, nil).Run(t.Context()))
	select {
	case <-idled:
		require.Fail(t, "the daemon went idle while automatic filing waited on Kata")
	default:
	}
	assert.NotEmpty(t, s.RequestsMatching(http.MethodPost, "/issues"), "the pass reached Kata")
}
