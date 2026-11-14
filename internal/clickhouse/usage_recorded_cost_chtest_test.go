//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestRecordedCostPricesOnlyUnpricedModels(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	dbtest.SeedRecordedCostSessions(t, local)
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	// A stopped view keeps prepared usage empty until the test refreshes it,
	// so the first read uses raw rows and the second prepared rows.
	_, err = store.DB().ExecContext(ctx, "ALTER TABLE prepare_usage MODIFY REFRESH EVERY 1 DAY")
	require.NoError(t, err)
	_, err = store.DB().ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	requireReady := func(want bool) {
		t.Helper()
		ready, err := store.preparedUsageReady(ctx)
		require.NoError(t, err)
		require.Equal(t, want, ready, "prepared usage ready")
	}
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err, "push to ClickHouse")

	t.Run("raw rows", func(t *testing.T) {
		requireReady(false)
		dbtest.AssertRecordedCostUsage(t, store)
	})
	for _, stmt := range []string{
		"SYSTEM START VIEW prepare_usage", "SYSTEM REFRESH VIEW prepare_usage",
		"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage",
	} {
		_, err := store.DB().ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	t.Run("prepared usage", func(t *testing.T) {
		requireReady(true)
		dbtest.AssertRecordedCostUsage(t, store)
	})
}
