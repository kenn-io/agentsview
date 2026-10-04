// cmd/agentsview/friction_filer_setup_test.go
package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/kata"
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

func TestFrictionFilerReadsStoredFindingBeforeDigest(t *testing.T) {
	for _, pgServe := range []bool{false, true} {
		t.Run(fmt.Sprintf("pg_serve_%v", pgServe), func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			dbtest.SeedSession(t, d, "claude:today", "example", func(s *db.Session) { s.StartedAt, s.EndedAt = new("2026-10-03T12:00:00Z"), nil })
			sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "claude:today", Text: "No, use the original configuration.", Ordinal: new(1)}
			require.NoError(t, d.ReplaceSessionFriction(t.Context(), sig.SubjectID, []db.FrictionFinding{{SessionID: sig.SubjectID, Kind: "correction", Text: sig.Text, Fingerprint: sig.Fingerprint(), MessageOrdinal: sig.Ordinal}}, nil, friction.RulesVersion, "synthetic"))
			cfg := config.Config{Kata: config.KataConfig{Enabled: true}, PublicURL: "https://archive.example.test"}
			if pgServe {
				cfg.PG.URL = "postgres://pg.example.test/archive"
			}
			f := newFrictionFiler(frictionFilerDeps{Cfg: &cfg, Store: d, Runner: &review.Runner{Loc: time.UTC}, IsPGServe: pgServe})
			require.NotNil(t, f)
			stored, run, err := f.SignalForFingerprint(t.Context(), sig.Fingerprint())
			require.NoError(t, err)
			assert.Equal(t, sig.SubjectID, stored.SubjectID)
			assert.Equal(t, "2026-10-03", run.Date)
			assert.Equal(t, "https://archive.example.test", run.PublicURL)
		})
	}
}

func TestAttachFilerKeepsNilInterface(t *testing.T) {
	r := &review.Runner{}
	attachFiler(r, nil)
	require.Nil(t, r.Filer, "a typed-nil *filing.Filer must never enter the interface")
}

func TestManualFrictionFilingUsesConfiguredZoneWithoutRunner(t *testing.T) {
	for _, withRunner := range []bool{false, true} {
		t.Run(fmt.Sprintf("runner_%v", withRunner), func(t *testing.T) {
			t.Setenv(friction.ZoneEnvVar, "America/Los_Angeles")
			d := dbtest.OpenTestDB(t)
			dbtest.SeedSession(t, d, "claude:midnight", "example", func(s *db.Session) {
				s.StartedAt, s.EndedAt = new("2026-10-04T00:30:00Z"), nil
			})
			sig := friction.Signal{Kind: friction.KindCorrection, SubjectKind: friction.SubjectSession, SubjectID: "claude:midnight", Text: "No, keep the original configuration."}
			require.NoError(t, d.ReplaceSessionFriction(t.Context(), sig.SubjectID, []db.FrictionFinding{{SessionID: sig.SubjectID, Kind: "correction", Text: sig.Text, Fingerprint: sig.Fingerprint()}}, nil, friction.RulesVersion, "synthetic"))
			cfg := config.Config{Kata: config.KataConfig{Enabled: true}, Friction: config.FrictionConfig{Timezone: "UTC"}}
			var runner *review.Runner
			if withRunner {
				runner = &review.Runner{}
			}
			f := newFrictionFiler(frictionFilerDeps{Cfg: &cfg, Store: d, Runner: runner})
			require.NotNil(t, f)
			_, run, err := f.SignalForFingerprint(t.Context(), sig.Fingerprint())
			require.NoError(t, err)
			assert.Equal(t, "2026-10-03", run.Date)
		})
	}
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
