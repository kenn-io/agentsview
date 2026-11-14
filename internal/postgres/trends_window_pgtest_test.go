//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
)

func TestStoreGetTrendsTermsDateWindowEdges(t *testing.T) {
	t.Parallel()
	const schema = pgPushTestSchema
	syncer, local, _, ctx := newSessionProvenancePushSync(t, schema)
	pgURL := testPGURL(t)
	dbtest.SeedTrendsWindow(t, local)
	_, err := syncer.Push(ctx, true, nil)
	require.NoError(t, err)
	store, err := NewStore(ctx, pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertTrendsWindow(t, store)
}
