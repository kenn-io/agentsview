package main

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction/review"
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
			assert.Equal(t, "https://av.example", r.PublicURL)
		})
	}
	t.Setenv("AGENTSVIEW_FRICTION_TZ", "Not/AZone")
	_, err := newFrictionRunner(base, d, time.Now)
	require.Error(t, err)
}

func TestFrictionOnByDefault(t *testing.T) {
	t.Setenv("AGENTSVIEW_FRICTION_TZ", "")
	cfg, err := config.Default()
	require.NoError(t, err)
	r, err := newFrictionRunner(cfg, dbtest.OpenTestDB(t), time.Now)
	require.NoError(t, err)
	assert.NotNil(t, r, "[friction] is on by default (D34)")
	assert.Nil(t, r.Diagnostics, "diagnostics stay off until a source is configured")

	cfg.Friction.Enabled = false
	r, err = newFrictionRunner(cfg, dbtest.OpenTestDB(t), time.Now)
	require.NoError(t, err)
	assert.Nil(t, r, "explicit enabled = false disables the job and FrictionAvailable")
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
	excl := frictionExclusive(tracker, lock)
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

func TestSerialExclusive(t *testing.T) {
	excl := serialExclusive()
	inside := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	go func() { _ = excl(func() error { close(inside); <-release; return nil }) }()
	<-inside
	second := make(chan struct{})
	go func() { _ = excl(func() error { close(second); return nil }) }()
	assert.Never(t, func() bool {
		select {
		case <-second:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "second call entered while the first held the lock")
	close(release)
	require.Eventually(t, func() bool {
		select {
		case <-second:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond, "second call never ran")
}
