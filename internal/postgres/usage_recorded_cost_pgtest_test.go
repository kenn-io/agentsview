//go:build pgtest

package postgres

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestRecordedCostPricesOnlyUnpricedModels(t *testing.T) {
	t.Parallel()
	const schema = usageTestSchema
	pgURL := testPGSchemaURL(t, schema)

	local := testDB(t)
	dbtest.SeedRecordedCostSessions(t, local)
	syncer, err := New(
		t.Context(), pgURL, schema, local, "recorded-cost-machine", true, storage.PusherOptions{},
	)
	require.NoError(t, err, "create PostgreSQL sync")
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err, "push recorded-cost fixture")

	cacheDir := t.TempDir()
	remote, err := NewStore(t.Context(), pgURL, schema, true)
	require.NoError(t, err, "open PostgreSQL store")
	t.Cleanup(func() { require.NoError(t, remote.Close()) })
	dbtest.AssertRecordedCostUsage(t, remote)
	require.NoError(t, remote.ConfigureUsageCache(t.Context(), cacheDir, pgURL, schema))
	dbtest.AssertRecordedCostUsage(t, remote)
	require.NoError(t, remote.WaitUsageCacheWarm(t.Context()))
	reopened, err := NewStore(t.Context(), pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	require.NoError(t, reopened.ConfigureUsageCache(t.Context(), cacheDir, pgURL, schema))
	dbtest.AssertRecordedCostUsage(t, reopened)
}
