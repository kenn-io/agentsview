//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestStoreGetTrendsTermsDateWindowEdges(t *testing.T) {
	t.Parallel()
	const schema = "agentsview_trends_window_test"
	pgURL := testPGURL(t)
	syncer, local, _, ctx := newSessionProvenancePushSync(t, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	dbtest.SeedTrendsWindow(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertTrendsWindow(t, store)
	cacheDir := t.TempDir()
	dbtest.AssertTrendsPartsReuse(t, func(retained bool) db.Store {
		opened, err := NewStore(ctx, pgURL, schema, true)
		require.NoError(t, err)
		if retained {
			require.NoError(t, opened.ConfigureUsageCache(ctx, cacheDir, pgURL, schema))
		}
		return opened
	}, func(action string) {
		dbtest.ChangeTrendsWindow(t, local, action)
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
	})
}
