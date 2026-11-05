//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestStoreGetTrendsTermsDateWindowEdges(t *testing.T) {
	const schema = "agentsview_trends_window_test"
	pgURL := testPGURL(t)
	syncer, local, _, ctx := newSessionProvenancePushSync(t, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	dbtest.SeedTrendsWindow(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertTrendsWindow(t, store)
}
