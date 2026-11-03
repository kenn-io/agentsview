//go:build chtest

package clickhouse

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseUsageCostFromFixture(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local, target := seedFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	store, err := NewStore(ctx, target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	want, err := local.GetSessionUsage(ctx, fixtureAlphaID, true)
	require.NoError(t, err)
	require.NotNil(t, want)
	got, err := store.GetSessionUsage(ctx, fixtureAlphaID, true)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, want.Cost, got.Cost, "GetSessionUsage cost should match SQLite")
	assert.Equal(t, want.HasCost, got.HasCost)
	assert.GreaterOrEqual(t, got.Cost.Microdollars, int64(105),
		"session cost should include the seeded usage-event cost")
}
