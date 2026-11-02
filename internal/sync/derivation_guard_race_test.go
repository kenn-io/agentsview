package sync

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
)

// Resync substitutes the staged archive while holding syncMu. Entry points
// must check the engine's fixed derivation policy without reading that pointer.
func TestDerivationGuardsDuringArchiveSwap(t *testing.T) {
	database := dbtest.OpenTestDB(t)
	staged := dbtest.OpenTestDB(t)
	require.NoError(t, database.EnableArchiveOnly(t.Context()))
	engine := NewEngine(t.Context(), database, EngineConfig{
		Ephemeral: true, ProviderFactories: []parser.ProviderFactory{},
	})
	defer engine.Close()
	engine.ReleaseStartupMaintenance()

	checks := []struct {
		name string
		run  func(context.Context, func() error) error
	}{
		{"startup", engine.RunStartupMaintenance},
		{"exclusive", func(_ context.Context, work func() error) error { return engine.RunExclusive(work) }},
		{"context", engine.RunExclusiveCtx},
		{"try", func(_ context.Context, work func() error) error { return engine.TryRunExclusive(work) }},
		{"flushed", func(_ context.Context, work func() error) error { return engine.RunExclusiveFlushed(work) }},
		{"friction", func(ctx context.Context, _ func() error) error {
			_, err := engine.BackfillFriction(ctx)
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			start := make(chan struct{})
			var swaps sync.WaitGroup
			swaps.Go(func() {
				<-start
				for range 500 {
					engine.syncMu.Lock()
					engine.db = staged
					runtime.Gosched()
					engine.db = database
					engine.syncMu.Unlock()
					runtime.Gosched()
				}
			})
			close(start)
			var callErr error
			for range 500 {
				err := check.run(t.Context(), func() error { return nil })
				if err != nil && !errors.Is(err, ErrSyncInProgress) {
					callErr = err
					break
				}
			}
			swaps.Wait()
			require.NoError(t, callErr)
		})
	}
}
