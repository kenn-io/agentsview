package main

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction/review"
	"go.kenn.io/agentsview/internal/ledger"
)

// tryLockExclusive models engine.RunExclusive (a non-reentrant mutex) but
// fails instead of deadlocking when re-entered.
type tryLockExclusive struct{ mu sync.Mutex }

var errReentered = errors.New("exclusive re-entered")

func (e *tryLockExclusive) run(work func() error) error {
	if !e.mu.TryLock() {
		return errReentered
	}
	defer e.mu.Unlock()
	return work()
}

func TestFrictionLedgerSinks(t *testing.T) {
	cfg := config.Config{InstallationID: "0123456789abcdef0123456789abcdef"}
	cfg.Ledger = config.LedgerConfig{Enabled: true, DefaultZone: "default", Zones: []config.LedgerZoneConfig{{ID: "default"}}}

	t.Run("disabled_ledger_yields_nil_parts", func(t *testing.T) {
		off := cfg
		off.Ledger.Enabled = false
		parts := frictionLedgerWiring(off, dbtest.OpenTestDB(t), dbtest.OpenTestDB(t), nil)
		assert.Nil(t, parts.JobSink)
		assert.Nil(t, parts.APISink)
		assert.Nil(t, parts.Diagnostics)
	})

	// Review Focus 1.
	t.Run("job_sink_never_reenters_exclusive", func(t *testing.T) {
		store := dbtest.OpenTestDB(t)
		excl := &tryLockExclusive{}
		parts := frictionLedgerWiring(cfg, store, store, excl.run)
		ev := ledger.Event{Timestamp: time.Now().UTC(), EventClass: ledger.ClassHealth, PayloadTier: ledger.TierStructured}
		// The friction job appends while already holding the exclusive section.
		err := excl.run(func() error { return parts.JobSink.Append(t.Context(), "", []ledger.Event{ev}) })
		require.NoError(t, err)
		// The API path takes the section itself.
		require.NoError(t, parts.APISink.Append(t.Context(), "", []ledger.Event{ev}))
		status, err := store.LedgerStatus(t.Context(), "default")
		require.NoError(t, err)
		assert.Equal(t, 2, status.Segments)
		assert.Equal(t, "av-0123456789abcdef0123456789abcdef", parts.Source)
	})

	t.Run("diagnostics_only_when_enabled_and_default_zones", func(t *testing.T) {
		on := cfg
		on.Friction.Diagnostics = config.FrictionDiagnosticsConfig{Enabled: true, Subsystems: []string{"*"}}
		on.LocalMachineName = "laptop"
		parts := frictionLedgerWiring(on, dbtest.OpenTestDB(t), dbtest.OpenTestDB(t), nil)
		src, ok := parts.Diagnostics.(review.LedgerDiagnostics)
		require.True(t, ok)
		assert.Equal(t, []string{"default"}, src.Zones)
		assert.Equal(t, "laptop", src.LocalLabel)
		assert.NotNil(t, src.Digested, "the source enforces PR 5's cross-date contract")
		assert.Nil(t, frictionLedgerWiring(cfg, dbtest.OpenTestDB(t), dbtest.OpenTestDB(t), nil).Diagnostics)
	})

	t.Run("runner_receives_the_configured_parts", func(t *testing.T) {
		on := cfg
		on.Friction = config.FrictionConfig{
			Enabled: true, BackfillDays: 7,
			Diagnostics: config.FrictionDiagnosticsConfig{Enabled: true, Subsystems: []string{"*"}},
		}
		store := dbtest.OpenTestDB(t)
		runner, err := newFrictionRunner(on, store, time.Now)
		require.NoError(t, err)
		require.NotNil(t, runner)
		assert.NotNil(t, runner.Ledger)
		assert.Equal(t, "av-0123456789abcdef0123456789abcdef", runner.LedgerSource)
		_, ok := runner.Diagnostics.(review.LedgerDiagnostics)
		assert.True(t, ok)
	})
}
