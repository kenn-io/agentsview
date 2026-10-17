package postgres

import (
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// A daemon push runs outside the sync lock, so a worker handoff can close the
// archive writer while the push saves its progress. The save must wait for
// the writer instead of failing the push.
func TestHandoffSyncStateWaitsForTheWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, local.Close()) })
		require.NoError(t, local.CloseWriter())

		done := make(chan error, 1)
		go func() {
			done <- handoffSyncState{local}.SetSyncState(t.Context(), "last_push_at", "cutoff")
		}()
		synctest.Wait()
		select {
		case err := <-done:
			require.Failf(t, "saved while the writer was closed", "err: %v", err)
		default:
		}
		require.NoError(t, local.ReopenWriter())
		require.NoError(t, <-done)

		got, err := local.GetSyncState(t.Context(), "last_push_at")
		require.NoError(t, err)
		assert.Equal(t, "cutoff", got)
	})
}
