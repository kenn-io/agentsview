package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCoordinatedSyncReportsWaitingWithoutReplacingActiveProgress(t *testing.T) {
	for name, run := range map[string]func(*Engine, context.Context, ProgressFunc) error{
		"sync": func(e *Engine, ctx context.Context, p ProgressFunc) error {
			_, err := e.SyncThenRun(ctx, false, p, func(bool) error { return nil })
			return err
		},
		"rebuild": func(e *Engine, ctx context.Context, p ProgressFunc) error {
			_, err := e.SyncThenRunWithRebuild(ctx, false, p, nil, nil, func(bool, bool) error { return nil })
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := newEngineFixture(t)
			active := Progress{Phase: PhaseSyncing, Detail: "Processing local sessions", SessionsDone: 4, SessionsTotal: 10}
			fx.engine.UpdateProgress(active)
			before, ok := fx.engine.CurrentProgress()
			require.True(t, ok)
			fx.engine.syncMu.Lock()
			locked := true
			defer func() {
				if locked {
					fx.engine.syncMu.Unlock()
				}
			}()
			progress := make(chan Progress, 1)
			done := make(chan error, 1)
			go func() {
				done <- run(fx.engine, t.Context(), func(p Progress) {
					select {
					case progress <- p:
					default:
					}
				})
			}()
			select {
			case p := <-progress:
				assert.Equal(t, "Waiting for the current sync to finish", p.Detail)
				current, ok := fx.engine.CurrentProgress()
				assert.True(t, ok)
				assert.Equal(t, before, current)
			case <-time.After(time.Second):
				assert.Fail(t, "waiting sync emitted no progress while another sync held the lock")
			}
			fx.engine.syncMu.Unlock()
			locked = false
			require.NoError(t, <-done)
		})
	}
}
