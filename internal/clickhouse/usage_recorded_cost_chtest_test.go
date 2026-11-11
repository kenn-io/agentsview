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
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err, "push to ClickHouse")
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	t.Run("raw rows", func(t *testing.T) {
		dbtest.AssertRecordedCostUsage(t, store)
	})
	require.NoError(t, refreshPreparedUsage(ctx, store.DB(), true), "refresh prepared usage")
	t.Run("prepared usage", func(t *testing.T) {
		dbtest.AssertRecordedCostUsage(t, store)
	})
}
