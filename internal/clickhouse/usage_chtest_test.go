//go:build chtest

package clickhouse

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func TestClickHouseUsageCostFromFixture(t *testing.T) {
	ctx := context.Background()
	local, target := seedFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.NoError(t, syncer.syncModelPricing(ctx))

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

	// Independent pin: 10 input * $3/MTok + 5 output * $15/MTok = $0.000105.
	eventCost, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: 10, Rate: money.MustParseDollars("3")},
		{Tokens: 5, Rate: money.MustParseDollars("15")},
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.000105"), eventCost)
	assert.GreaterOrEqual(t, got.Cost.Microdollars, eventCost.Microdollars,
		"session cost should include the seeded usage-event cost")

	filter := db.UsageFilter{
		From:     "2026-01-10",
		To:       "2026-01-10",
		Timezone: "UTC",
	}
	wantDaily, err := local.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	gotDaily, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	require.NotEmpty(t, gotDaily.Daily)
	assert.Equal(t, "2026-01-10", gotDaily.Daily[0].Date)
	assert.Equal(t, wantDaily.Totals.TotalCost, gotDaily.Totals.TotalCost,
		"GetDailyUsage totals should match SQLite")
	assert.GreaterOrEqual(t, gotDaily.Totals.TotalCost.Microdollars, eventCost.Microdollars,
		"UTC window covering 2026-01-10 should include the usage-event cost")
}
