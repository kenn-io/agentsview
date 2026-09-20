//go:build chtest

package clickhouse

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/storage"
)

func TestUsageCatalogCustomOverrideUsesOneSnapshot(t *testing.T) {
	store, syncer, local := newUsagePriceStore(t)
	before, err := chLoadPricingCatalog(t.Context(), store.conn, nil)
	require.NoError(t, err)
	var refreshed atomic.Bool
	refreshResult := make(chan error, 1)
	ctx := chdriver.Context(t.Context(), chdriver.WithProgress(func(*chdriver.Progress) {
		if !refreshed.CompareAndSwap(false, true) {
			return
		}
		// The server has read the first catalog query. Publish new rates
		// before another query can reload that catalog for custom overrides.
		err := local.UpsertModelPricing([]db.ModelPricing{{
			ModelPattern: "round-test", InputPerMTok: money.MustParseDollars("0.8"),
		}})
		if err == nil {
			err = syncer.syncModelPricing(t.Context())
		}
		refreshResult <- err
	}))
	catalog, err := chLoadPricingCatalog(ctx, store.conn, map[string]config.CustomModelRate{
		"claude-test": {InputMicrodollarsPerMTok: 2_000_000},
	})
	require.NoError(t, err)
	require.True(t, refreshed.Load(), "the competing catalog refresh must execute")
	require.NoError(t, <-refreshResult)
	assert.Equal(t, before.digest, catalog.digest)
	resolver := export.NewPricingResolver(catalog.rows)
	assert.Equal(t, money.MustParseDollars("0.4"), resolver.Lookup("round-test").Rates.InputPerMTok)
	assert.Equal(t, money.MustParseDollars("2"), resolver.Lookup("claude-test").Rates.InputPerMTok)
}

func TestDailyUsageAcceptsLargeReportedCost(t *testing.T) {
	local, target := seedFixture(t)
	stamp := "2026-01-12T12:00:00Z"
	cost := money.Money{Microdollars: 9_100_000_000_000_000_000}
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: fixtureSession("large-reported", "gamma", "reported", stamp, 0),
		UsageEvents: []db.UsageEvent{{Source: "hermes", Model: "large-reported", OccurredAt: stamp,
			Cost: &cost, CostStatus: "reported", CostSource: string(export.CostSourceReported), DedupKey: "large-reported"}},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	got, err := store.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC", Model: "large-reported"})
	require.NoError(t, err)
	require.NotEmpty(t, got.Daily)
	assert.Equal(t, cost, got.Totals.TotalCost)

	// Two individually valid costs exceed Int64 when ClickHouse groups them.
	_, err = local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: fixtureSession("large-reported", "gamma", "reported", stamp, 0),
		UsageEvents: []db.UsageEvent{
			{Source: "hermes", Model: "large-reported", OccurredAt: stamp,
				Cost: &cost, CostStatus: "reported", CostSource: string(export.CostSourceReported), DedupKey: "large-reported"},
			{Source: "hermes", Model: "large-reported", OccurredAt: stamp,
				Cost: &cost, CostStatus: "reported", CostSource: string(export.CostSourceReported), DedupKey: "large-reported-second"},
		},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	_, err = store.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC", Model: "large-reported"})
	assert.ErrorIs(t, err, money.ErrOverflow)
}

func TestDailyUsagePersistedErrorsKeepMoneyIdentity(t *testing.T) {
	local, target := seedFixture(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "overflow-rate", InputPerMTok: money.MustParseDollars("9000000000000"),
	}}))
	stamp := "2026-01-12T12:00:00Z"
	id := "overflow-rate-session"
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:     fixtureSession(id, "gamma", "overflow", stamp, 1),
		Messages:    []db.Message{usagePriceMessage(id, 0, stamp, "overflow-rate", `{"input_tokens":2000000}`)},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	filter := db.UsageFilter{Timezone: "UTC", Model: "overflow-rate"}
	_, err = store.GetDailyUsage(t.Context(), filter)
	assert.ErrorIs(t, err, money.ErrOverflow, "persisted prices must retain the error identity")
	_, err = store.conn.ExecContext(t.Context(), "TRUNCATE TABLE usage_event_prices")
	require.NoError(t, err)
	_, err = store.GetDailyUsage(t.Context(), filter)
	assert.ErrorIs(t, err, money.ErrOverflow, "cold pricing must report the same error")
}

func TestUsagePricingAcrossBatchesAndColdRead(t *testing.T) {
	local, target := seedFixture(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "batch-price", InputPerMTok: money.MustParseDollars("0.4"),
	}}))
	const count = 20_003
	start := time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)
	id := "batch-pricing-session"
	messages := make([]db.Message, count)
	for i := range messages {
		messages[i] = usagePriceMessage(id, i, start.Add(time.Duration(i)*time.Second).Format(time.RFC3339),
			"batch-price", `{"input_tokens":2}`)
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session:  fixtureSession(id, "gamma", "batch pricing", start.Format(time.RFC3339), count),
		Messages: messages, DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	filter := db.UsageFilter{Timezone: "UTC", Model: "batch-price"}
	warm, err := store.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.EqualValues(t, 20_003, warm.Totals.TotalCost.Microdollars, "each 0.8-microdollar request rounds to one")
	assert.EqualValues(t, 40_006, warm.Totals.InputTokens)
	_, err = store.conn.ExecContext(t.Context(), "TRUNCATE TABLE usage_event_prices")
	require.NoError(t, err)
	cold, err := store.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, dailyUsageWire(t, warm, false), dailyUsageWire(t, cold, false))
	pricer, err := syncer.newUsagePricer(t.Context())
	require.NoError(t, err)
	batches, inputs := 0, 0
	err = syncer.forEachUnpricedUsageBatch(t.Context(), usagePriceScope{sessionIDs: []string{id}}, pricer.digest,
		func(batch []chUsagePriceInput) error {
			if len(batch) > 20_000 {
				return fmt.Errorf("pricing batch retained %d inputs", len(batch))
			}
			batches++
			inputs += len(batch)
			return nil
		})
	require.NoError(t, err)
	assert.Equal(t, 2, batches)
	assert.Equal(t, count, inputs)
}
