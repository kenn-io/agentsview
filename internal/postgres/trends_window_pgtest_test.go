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
	require.NoError(t, store.ConfigureUsageCache(ctx, t.TempDir(), pgURL, schema))
	dbtest.AssertTrendsWindow(t, store)
	dbtest.AssertTrendsPartsReuse(t, func() db.Store {
		opened, err := NewStore(ctx, pgURL, schema, true)
		require.NoError(t, err)
		require.NoError(t, opened.ConfigureUsageCache(ctx, t.TempDir(), pgURL, schema))
		return opened
	}, func() {
		dbtest.RewriteTrendsWindow(t, local)
		_, err := syncer.Push(ctx, false, nil)
		require.NoError(t, err)
	})
}
