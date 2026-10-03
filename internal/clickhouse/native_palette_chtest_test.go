//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestCHNativePalette(t *testing.T) {
	archive, target := seedFixture(t)
	dbtest.SeedNativePalette(t, archive)
	push := newTestSync(t, archive, target, storage.PusherOptions{})
	_, err := push.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.CheckNativePalette(t, store)

	t.Run("upgrade unchanged mirrored corpus", func(t *testing.T) {
		_, err := push.conn.ExecContext(t.Context(), "ALTER TABLE messages UPDATE palette_text = '' WHERE session_id IN ('native-search', 'palette-newer') SETTINGS mutations_sync = 1")
		require.NoError(t, err)
		require.NoError(t, writeMetadata(t.Context(), push.conn, map[string]string{
			push.archiveKey("palette_corpus_recipe"): "old-v0",
		}))
		_, err = push.Push(t.Context(), false, nil)
		require.NoError(t, err)
		dbtest.CheckNativePalette(t, store)
	})
}
