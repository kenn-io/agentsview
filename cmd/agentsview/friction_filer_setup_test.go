// cmd/agentsview/friction_filer_setup_test.go
package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/ledger"
)

func TestNewFrictionFilerHubOnly(t *testing.T) {
	tests := []struct {
		name      string
		kata      bool
		pgURL     string
		isPGServe bool
		want      bool
		wantLog   string
	}{
		{name: "standalone_with_kata_files", kata: true, want: true, wantLog: "Kata filing enabled"},
		{name: "pg_serve_files", kata: true, pgURL: "postgres://pg.example.test/a", isPGServe: true, want: true},
		{name: "pusher_never_files", kata: true, pgURL: "postgres://pg.example.test/a", want: false, wantLog: "not the filing hub"},
		{name: "kata_off_never_files", kata: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			cfg := &config.Config{
				PG: config.PGConfig{URL: tt.pgURL}, Kata: config.KataConfig{Enabled: tt.kata, Project: "agentsview"},
				Friction: config.FrictionConfig{Kata: config.DefaultFrictionKataConfig()}, InstallationID: "inst",
			}
			hub := tt.isPGServe || !cfg.HasPGPushTarget()
			conn := kata.NewConn(kata.ConfigFrom(cfg.Kata, hub))
			runner := &review.Runner{}
			f := newFrictionFiler(frictionFilerDeps{Cfg: cfg, Store: dbtest.OpenTestDB(t), Conn: conn, Runner: runner, IsPGServe: tt.isPGServe})
			assert.Equal(t, tt.want, f != nil)
			attachFiler(runner, f)
			assert.Equal(t, tt.want, runner.Filer != nil, "Runner.Filer is nil on every non-hub")
			if tt.wantLog != "" {
				assert.Contains(t, buf.String(), tt.wantLog)
			}
		})
	}
}

func TestAttachFilerKeepsNilInterface(t *testing.T) {
	r := &review.Runner{}
	attachFiler(r, nil)
	require.Nil(t, r.Filer, "a typed-nil *filing.Filer must never enter the interface")
}

func TestNewFrictionFilerLedgerSinks(t *testing.T) {
	store := dbtest.OpenTestDB(t)
	cfg := &config.Config{InstallationID: "0123456789abcdef0123456789abcdef"}
	cfg.Kata = config.KataConfig{Enabled: true, Project: "agentsview"}
	cfg.Friction.Kata = config.DefaultFrictionKataConfig()
	cfg.Ledger = config.LedgerConfig{Enabled: true, DefaultZone: "default", Zones: []config.LedgerZoneConfig{{ID: "default"}}}
	source := cfg.Ledger.EffectiveSource(cfg.InstallationID)
	excl := &tryLockExclusive{}
	runner := &review.Runner{Ledger: ledger.NewZoneWriters(store, source, cfg.Ledger.ZoneIDs(), runInline), LedgerSource: source}
	f := newFrictionFiler(frictionFilerDeps{
		Cfg: cfg, Store: store, Runner: runner, LedgerAPIExclusive: excl.run,
	})
	require.NotNil(t, f)
	require.NotNil(t, f.Ledger)
	require.NotNil(t, f.InlineLedger)
	assert.Equal(t, source, f.LedgerSource)
	event := ledger.Event{Timestamp: time.Now().UTC(), EventClass: ledger.ClassHealth, PayloadTier: ledger.TierStructured}
	require.NoError(t, excl.run(func() error { return f.InlineLedger.Append(t.Context(), "", []ledger.Event{event}) }))
	require.NoError(t, f.Ledger.Append(t.Context(), "", []ledger.Event{event}))
	err := excl.run(func() error { return f.Ledger.Append(t.Context(), "", []ledger.Event{event}) })
	require.ErrorIs(t, err, errReentered, "the API sink takes the lock itself")
	status, err := store.LedgerStatus(t.Context(), "default")
	require.NoError(t, err)
	assert.Equal(t, 2, status.Segments)
}

func TestFrictionKinds(t *testing.T) {
	assert.Equal(t, []friction.Kind{friction.KindError, friction.KindFrustration}, frictionKinds([]string{"error", "frustration"}))
}

func TestStartFrictionReviewAttachesBeforeFirstRun(t *testing.T) {
	cfg := config.Config{Friction: config.FrictionConfig{Enabled: true, BackfillDays: 7}}
	d := dbtest.OpenTestDB(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ran := make(chan struct{}, 1)
	var attached bool
	runner, wait := startFrictionReview(ctx, cfg, d, func(f func() error) error {
		assert.True(t, attached, "the filer is attached before the RunAtStart tick")
		ran <- struct{}{}
		return f()
	}, func(*review.Runner) { attached = true })
	require.NotNil(t, runner)
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "review job did not run at startup")
	}
	cancel()
	wait()
}
