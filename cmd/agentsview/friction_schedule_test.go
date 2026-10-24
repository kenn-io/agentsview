package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/server"
)

type readOnlyFrictionStore struct {
	review.Store
	readOnly  bool
	available *bool
}

func (s readOnlyFrictionStore) ReadOnly() bool { return s.readOnly }

type availableFrictionStore struct {
	readOnlyFrictionStore
}

func (s availableFrictionStore) FrictionAvailable() bool { return *s.available }

func TestNewFrictionRunner(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	yes, no := true, false
	base := config.Config{
		Friction:  config.FrictionConfig{Enabled: true, Timezone: "Asia/Tokyo", BackfillDays: 30},
		PublicURL: "https://av.example",
	}
	tests := []struct {
		name    string
		cfg     func() config.Config
		store   frictionRunnerStore
		env     string
		wantNil bool
		wantTZ  string
	}{
		{name: "disabled", cfg: func() config.Config { c := base; c.Friction.Enabled = false; return c }, store: d, wantNil: true},
		{name: "sqlite writable", cfg: func() config.Config { return base }, store: d, wantTZ: "Asia/Tokyo"},
		{name: "read-only sqlite", cfg: func() config.Config { return base }, store: readOnlyFrictionStore{Store: d, readOnly: true}, wantNil: true},
		{
			name: "pg without write access", cfg: func() config.Config { return base },
			store: availableFrictionStore{readOnlyFrictionStore{Store: d, readOnly: true, available: &no}}, wantNil: true,
		},
		{
			name: "pg with write access", cfg: func() config.Config { return base },
			store: availableFrictionStore{readOnlyFrictionStore{Store: d, readOnly: true, available: &yes}}, wantTZ: "Asia/Tokyo",
		},
		{name: "env overrides zone", cfg: func() config.Config { return base }, store: d, env: "Europe/Berlin", wantTZ: "Europe/Berlin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENTSVIEW_FRICTION_TZ", tt.env)
			r, err := newFrictionRunner(tt.cfg(), tt.store, time.Now)
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, r)
				return
			}
			require.NotNil(t, r)
			assert.Equal(t, tt.wantTZ, r.Loc.String())
			assert.Equal(t, 30, r.BackfillDays)
		})
	}
	t.Setenv("AGENTSVIEW_FRICTION_TZ", "Not/AZone")
	_, err := newFrictionRunner(base, d, time.Now)
	require.Error(t, err)
}

func TestFrictionReviewIsOptIn(t *testing.T) {
	t.Setenv("AGENTSVIEW_FRICTION_TZ", "")
	cfg, err := config.Default()
	require.NoError(t, err)
	d := dbtest.OpenTestDB(t)
	r, err := newFrictionRunner(cfg, d, time.Now)
	require.NoError(t, err)
	assert.Nil(t, r)
	cfg.Friction.Enabled = true
	r, err = newFrictionRunner(cfg, d, time.Now)
	require.NoError(t, err)
	require.NotNil(t, r)
	assert.Nil(t, r.Diagnostics)
}

func TestFrictionReviewSkipsUsageOnlyArchive(t *testing.T) {
	t.Setenv("AGENTSVIEW_FRICTION_TZ", "")
	d := dbtest.OpenTestDB(t)
	ended := time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)
	dbtest.SeedSession(t, d, "retained", "proj", func(s *db.Session) { s.EndedAt = &ended })
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "retained", db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{
			RulesVersion: friction.RulesVersion, Hash: "retained",
			Findings: []db.FrictionFinding{{
				SessionID: "retained", Kind: "correction", Detector: "correction.coding",
				Text: "Use the existing helper", Title: "Use the existing helper", Fingerprint: "correction-helper",
				RulesVersion: friction.RulesVersion,
			}},
		},
	}))
	d.SetArchiveContent(config.ArchiveContentUsage)
	cfg := config.Config{NoSync: true, Friction: config.FrictionConfig{Enabled: true, Timezone: "UTC"}}
	ctx, cancel := context.WithCancel(t.Context())
	runner, wait := startFrictionReview(ctx, cfg, d, nil)
	t.Cleanup(func() { cancel(); wait() })
	require.Nil(t, runner, "a usage-only archive must not start digest work")
	got, err := d.GetFrictionDigest(t.Context(), ended[:10])
	require.NoError(t, err)
	assert.Nil(t, got)
}

// Pause after selecting real subjects, before retrieving their findings.
type pausedFrictionStore struct {
	*db.DB
	selected chan struct{}
	resume   <-chan struct{}
}

func (s pausedFrictionStore) FrictionSubjectsForDate(ctx context.Context, date string, loc *time.Location, includeDigested bool) ([]db.FrictionSubject, error) {
	subjects, err := s.DB.FrictionSubjectsForDate(ctx, date, loc, includeDigested)
	close(s.selected)
	select {
	case <-s.resume:
		return subjects, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestLocalFrictionReviewNoSyncSharesResyncBarrier(t *testing.T) {
	t.Setenv("AGENTSVIEW_FRICTION_TZ", "")
	d := dbtest.OpenTestDB(t)
	ended := time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)
	dbtest.SeedSession(t, d, "s", "proj", func(s *db.Session) { s.EndedAt = &ended })
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "s", db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{
			RulesVersion: friction.RulesVersion, Hash: "s",
			Findings: []db.FrictionFinding{{
				SessionID: "s", Kind: "correction", Detector: "correction.coding",
				Text: "Use the existing helper", Title: "Use the existing helper", Fingerprint: "correction-helper",
				RulesVersion: friction.RulesVersion,
			}},
		},
	}))
	cfg := config.Config{
		Host: "127.0.0.1", DataDir: t.TempDir(), DBPath: d.Path(), NoSync: true,
		Friction: config.FrictionConfig{Enabled: true, Timezone: "UTC", BackfillDays: 1},
	}
	srv := server.New(cfg, d, nil, server.WithBaseContext(t.Context()))
	httpServer := httptest.NewServer(srv.Handler())
	t.Cleanup(httpServer.Close)
	selected, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	ctx, cancel := context.WithCancel(t.Context())
	wait := startLocalFrictionReview(ctx, cfg, pausedFrictionStore{d, selected, resume}, srv, nil)
	t.Cleanup(func() {
		release()
		cancel()
		wait()
		require.NoError(t, srv.Shutdown(context.WithoutCancel(t.Context())))
	})
	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "digest did not select the seeded session")
	}

	requestCtx, requestCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer requestCancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, httpServer.URL+"/api/v1/resync", nil)
	require.NoError(t, err)
	req.Host = "127.0.0.1:0"
	req.Header.Set("Origin", "http://127.0.0.1:0")
	req.Header.Set("Accept", "text/event-stream")
	response, err := httpServer.Client().Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	reader := bufio.NewReader(response.Body)
	event, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "event: progress\n", event)
	progress, err := reader.ReadString('\n')
	require.NoError(t, err)
	assert.Contains(t, progress, "Waiting for the current sync to finish")

	release()
	remaining, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Contains(t, string(remaining), "event: done\n")
	assert.NotContains(t, string(remaining), "event: error\n")
	got, err := d.GetFrictionDigest(t.Context(), ended[:10])
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Contains(t, string(got.Markdown), "Use the existing helper",
		"the digest must capture findings before resync replaces the archive")
}

type fakeTracker struct {
	active, max int
	draining    bool
}

func (f *fakeTracker) BeginWork() (func(), bool) {
	if f.draining {
		return func() {}, false
	}
	f.active++
	f.max = max(f.max, f.active)
	return func() { f.active-- }, true
}

type fakeExclusive struct{ calls int }

func (f *fakeExclusive) RunExclusive(work func() error) error { f.calls++; return work() }

func TestFrictionExclusiveKeepsDaemonAlive(t *testing.T) {
	tracker := &fakeTracker{}
	lock := &fakeExclusive{}
	excl := frictionExclusive(tracker, lock.RunExclusive)
	sawActive := false
	require.NoError(t, excl(func() error { sawActive = tracker.active == 1; return nil }))
	assert.True(t, sawActive, "the idle tracker counts the job as work while it runs")
	assert.Equal(t, 0, tracker.active)
	assert.Equal(t, 1, lock.calls, "work runs under engine.RunExclusive")

	boom := errors.New("boom")
	require.ErrorIs(t, excl(func() error { return boom }), boom)

	tracker.draining = true
	ran := false
	require.NoError(t, excl(func() error { ran = true; return nil }))
	assert.False(t, ran, "a draining daemon skips the job")

	direct := frictionExclusive(nil, nil)
	require.NoError(t, direct(func() error { ran = true; return nil }))
	assert.True(t, ran)
}

func TestHubFrictionDefaultsToUTCDates(t *testing.T) {
	t.Setenv("TZ", "Pacific/Honolulu")
	t.Setenv("AGENTSVIEW_FRICTION_TZ", "")
	d := dbtest.OpenTestDB(t)
	ended := "2026-09-21T12:00:00Z"
	dbtest.SeedSession(t, d, "s", "proj", func(s *db.Session) { s.EndedAt = &ended })
	require.NoError(t, d.UpdateSessionSignals(t.Context(), "s", db.SessionSignalUpdate{
		Friction: &db.SessionFrictionUpdate{RulesVersion: friction.RulesVersion, Hash: "s"},
	}))
	available := true
	store := availableFrictionStore{readOnlyFrictionStore{Store: d, readOnly: true, available: &available}}
	cfg := config.Config{Friction: config.FrictionConfig{Enabled: true}}
	r, err := newFrictionRunner(cfg, store, func() time.Time {
		return time.Date(2026, 9, 22, 7, 0, 0, 0, time.UTC)
	})
	require.NoError(t, err)
	require.NotNil(t, r)
	_, err = r.CatchUp(t.Context())
	require.NoError(t, err)
	got, err := d.GetFrictionDigest(t.Context(), "2026-09-21")
	require.NoError(t, err)
	require.NotNil(t, got, "hub dates must not depend on the serve process's local zone")
	assert.Equal(t, "UTC", got.Timezone)
	assert.Equal(t, 1, got.SessionsScanned)
}
