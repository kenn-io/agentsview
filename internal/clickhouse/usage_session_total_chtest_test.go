//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

func TestSessionUsageTotalStaysApartFromUsage(t *testing.T) {
	ctx := t.Context()
	local := dbtest.OpenTestDB(t)
	dbtest.SeedSessionUsageTotal(t, local)
	dsn, database := chtest.FreshDatabase(t)
	target := Target{URL: dsn, Database: database}
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err, "push to ClickHouse")
	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	dbtest.AssertSessionUsageTotal(t, store)

	// Removing the total locally removes it from the mirror.
	require.NoError(t, local.ReplaceSessionUsageTotal(ctx, dbtest.SessionTotalSessionID, nil))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err, "push after removal")
	got, err := store.GetSessionUsage(ctx, dbtest.SessionTotalSessionID, false)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Nil(t, got.SessionTotal)
}
