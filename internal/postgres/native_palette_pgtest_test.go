//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestPGNativePalette(t *testing.T) {
	url := testPGURL(t)
	cleanPGSchema(t, url)
	t.Cleanup(func() { cleanPGSchema(t, url) })
	archive := testDB(t)
	dbtest.SeedNativePalette(t, archive)
	push, err := New(url, "agentsview", archive, "test", true, storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, push.Close()) })
	require.NoError(t, push.EnsureSchema(t.Context()))
	_, err = push.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(url, "agentsview", true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.CheckNativePalette(t, store)

	t.Run("upgrade unchanged mirrored corpus", func(t *testing.T) {
		// An older mirror gains the nullable projection column without a
		// local transcript edit. The recipe marker must force its backfill.
		_, err := push.pg.ExecContext(t.Context(), "UPDATE messages SET palette_text = NULL")
		require.NoError(t, err)
		require.NoError(t, push.effectiveSyncState().SetSyncState(t.Context(), "palette_corpus_recipe", "old-v0"))
		_, err = push.Push(t.Context(), false, nil)
		require.NoError(t, err)
		dbtest.CheckNativePalette(t, store)
	})
}
