//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
)

// pricingDigestRows counts the price records under each digest.
func pricingDigestRows(t *testing.T, conn *sql.DB) map[string]int {
	t.Helper()
	rows, err := conn.QueryContext(t.Context(),
		`SELECT pricing_digest, count() FROM usage_event_prices GROUP BY pricing_digest`)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var digest string
		var count int
		require.NoError(t, rows.Scan(&digest, &count))
		out[digest] = count
	}
	require.NoError(t, rows.Err())
	return out
}

func pricedDigest(t *testing.T, conn *sql.DB) string {
	t.Helper()
	meta, err := readMetadata(t.Context(), conn, usagePricedDigestKey)
	require.NoError(t, err)
	return meta[usagePricedDigestKey]
}

// A catalog change to a model no usage row names leaves the digest and its
// records alone, so the next push prices nothing again.
func TestPushKeepsPricesWhenUnusedCatalogRowsChange(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	ctx := t.Context()
	before := pricingDigestRows(t, store.conn)
	digest := pricedDigest(t, store.conn)
	require.Contains(t, before, digest)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "nobody-uses-this-model",
		InputPerMTok:  money.MustParseDollars("3"),
		OutputPerMTok: money.MustParseDollars("15"),
	}}))
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	catalog, err := chLoadPricingCatalog(ctx, store.conn, nil)
	require.NoError(t, err)
	assert.Equal(t, digest, catalog.digest)
	assert.Equal(t, digest, pricedDigest(t, store.conn))
	assert.Equal(t, before, pricingDigestRows(t, store.conn))
	assertDailyUsageParity(t, local, store)
}

// Usage of a model the digest does not cover moves the mirror to a digest
// that does, with every record already in place when readers switch.
func TestPushPricesNewModelBeforeRegisteringIt(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	ctx := t.Context()
	filter := db.UsageFilter{Timezone: "UTC"}
	baseline := dailyUsageGroupRowStats(t, store, filter)
	before := pricedDigest(t, store.conn)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "fresh-test",
		InputPerMTok:  money.MustParseDollars("2"),
		OutputPerMTok: money.MustParseDollars("4"),
	}}))
	const id = "ch-price-fresh"
	ts := "2026-01-13T05:00:00.000Z"
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: fixtureSession(id, "gamma", "fresh first", ts, 1),
		Messages: []db.Message{
			usagePriceMessage(id, 0, ts, "fresh-test", `{"input_tokens":1000,"output_tokens":500}`),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	fresh := chUsagePriceModel{model: "fresh-test", priceModel: "fresh-test"}
	models, err := loadUsagePriceModels(ctx, store.conn, false)
	require.NoError(t, err)
	assert.Contains(t, models, fresh)
	pending, err := loadUsagePriceModels(ctx, store.conn, true)
	require.NoError(t, err)
	assert.NotContains(t, pending, fresh)
	catalog, err := chLoadPricingCatalog(ctx, store.conn, nil)
	require.NoError(t, err)
	assert.NotEqual(t, before, catalog.digest)
	assert.Equal(t, catalog.digest, pricedDigest(t, store.conn))
	after := dailyUsageGroupRowStats(t, store, filter)
	assert.Equal(t, baseline.explicit, after.explicit, "the new model's events are priced from records")
	assert.Greater(t, after.events, baseline.events)
	assertDailyUsageParity(t, local, store)
}

// A model a stopped push left pending is adopted by the next push, even
// one whose batches bring no usage of it.
func TestPushAdoptsModelsAnEarlierPushLeftPending(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	ctx := t.Context()
	filter := db.UsageFilter{Timezone: "UTC"}
	baseline := dailyUsageGroupRowStats(t, store, filter)
	round := chUsagePriceModel{model: "round-test", priceModel: "round-test"}
	require.NoError(t, syncer.registerUsagePriceModels(ctx, []chUsagePriceModel{round}, true))
	// Readers no longer find records for the digest without the model.
	stopped := dailyUsageGroupRowStats(t, store, filter)
	require.Equal(t, stopped.rows, stopped.explicit)

	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	pending, err := loadUsagePriceModels(ctx, store.conn, true)
	require.NoError(t, err)
	assert.Empty(t, pending)
	catalog, err := chLoadPricingCatalog(ctx, store.conn, nil)
	require.NoError(t, err)
	assert.Contains(t, catalog.models, round)
	assert.Equal(t, catalog.digest, pricedDigest(t, store.conn))
	assert.Equal(t, baseline.explicit, dailyUsageGroupRowStats(t, store, filter).explicit)
	assertDailyUsageParity(t, local, store)
}

// insertAgedPrices writes one price record and one context under digest,
// as a push at at would have.
func insertAgedPrices(t *testing.T, conn *sql.DB, digest string, at time.Time) {
	t.Helper()
	version := uint64(at.UnixNano())
	require.NoError(t, insertRows(t.Context(), conn, "usage_price_contexts", [][]any{
		{digest, "ctx", "{}", version},
	}))
	require.NoError(t, insertRows(t.Context(), conn, "usage_event_prices", [][]any{
		{digest, "key", int64(1), int64(0), "ctx", "ctx", false, int64(-1), "", int64(1), version},
	}))
}

func TestPushDropsUnusedPricingDigests(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	ctx := t.Context()
	current := pricedDigest(t, store.conn)
	now := time.Now()
	insertAgedPrices(t, store.conn, "stale-digest", now.Add(-usagePriceRetention-time.Hour))
	insertAgedPrices(t, store.conn, "recent-digest", now.Add(-time.Hour))

	appendMessage(t, local, usagePriceTierID, "later", "2026-01-13T04:00:00.000Z")
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	digests := pricingDigestRows(t, store.conn)
	assert.NotContains(t, digests, "stale-digest")
	assert.Contains(t, digests, "recent-digest")
	assert.Contains(t, digests, current)
	var contexts int
	require.NoError(t, store.conn.QueryRowContext(ctx,
		`SELECT count() FROM usage_price_contexts WHERE pricing_digest = 'stale-digest'`).Scan(&contexts))
	assert.Zero(t, contexts)
	assertDailyUsageParity(t, local, store)
}

// The digest the mirror was last priced under and the one prepared_usage was
// built from stay however old their records are: readers join them.
func TestPruneKeepsCurrentAndPreparedDigests(t *testing.T) {
	t.Parallel()
	store, syncer, _ := newUsagePriceStore(t)
	ctx := t.Context()
	aged := time.Now().Add(-usagePriceRetention - time.Hour)
	for _, digest := range []string{"pushing-digest", "mirror-digest", "prepared-digest", "stale-digest"} {
		insertAgedPrices(t, store.conn, digest, aged)
	}
	require.NoError(t, writeMetadata(ctx, store.conn, map[string]string{
		usagePricedDigestKey: "mirror-digest",
	}))
	// Stop the refreshes so the stamp below stays until the prune reads it.
	_, err := store.conn.ExecContext(ctx, "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := store.conn.ExecContext(context.WithoutCancel(t.Context()), "SYSTEM START VIEW prepare_usage")
		require.NoError(t, err)
	})
	_, err = store.conn.ExecContext(ctx, "TRUNCATE TABLE prepared_usage")
	require.NoError(t, err)
	_, err = store.conn.ExecContext(ctx,
		"INSERT INTO prepared_usage (pricing_digest) VALUES ('prepared-digest')")
	require.NoError(t, err)

	require.NoError(t, syncer.pruneUsagePrices(ctx, "pushing-digest"))

	digests := pricingDigestRows(t, store.conn)
	assert.Contains(t, digests, "pushing-digest")
	assert.Contains(t, digests, "mirror-digest")
	assert.Contains(t, digests, "prepared-digest")
	assert.NotContains(t, digests, "stale-digest")
}

// A mirror whose price tables predate partitioning gets them recreated
// partitioned, and the next push prices it again.
func TestEnsureSchemaPartitionsPriceTables(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	ctx := t.Context()
	for _, table := range []string{"usage_event_prices", "usage_price_contexts"} {
		spec, ok := tableByName(table)
		require.True(t, ok)
		spec.partitionBy = ""
		_, err := store.conn.ExecContext(ctx, "DROP TABLE "+table)
		require.NoError(t, err)
		_, err = store.conn.ExecContext(ctx, spec.createSQL())
		require.NoError(t, err)
	}
	insertAgedPrices(t, store.conn, "unpartitioned-digest", time.Now())

	require.NoError(t, EnsureSchemaOn(ctx, store.conn))

	for _, table := range []string{"usage_event_prices", "usage_price_contexts"} {
		var key string
		var rows int
		require.NoError(t, store.conn.QueryRowContext(ctx,
			`SELECT partition_key, total_rows FROM system.tables
			WHERE database = currentDatabase() AND name = ?`, table).Scan(&key, &rows))
		assert.Equal(t, "pricing_digest", key, table)
		assert.Zero(t, rows, table)
	}

	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	digests := pricingDigestRows(t, store.conn)
	assert.Positive(t, digests[pricedDigest(t, store.conn)])
	assertDailyUsageParity(t, local, store)
}

func dayModelCost(t *testing.T, result db.DailyUsageResult, date, model string) money.Money {
	t.Helper()
	for _, day := range result.Daily {
		if day.Date != date {
			continue
		}
		for _, breakdown := range day.ModelBreakdowns {
			if breakdown.ModelName == model {
				return breakdown.Cost
			}
		}
	}
	require.Failf(t, "no cost", "model %s has no cost on %s", model, date)
	return money.Money{}
}

// A past day's usage costs what its rate was that day. When the catalog
// later corrects that past rate, the next push reprices the past day, and
// later days keep their own rate.
func TestPushRepricesPastDaysWhenPastRateChanges(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	ctx := t.Context()
	const model = "gpt-hist-test"
	require.NoError(t, local.UpsertGenAIPricing(ctx, genAIHistoryDocument(t, "1", "1")))
	for _, s := range []struct{ id, ts string }{
		{"ch-price-hist-past", "2026-01-12T05:00:00.000Z"},
		{"ch-price-hist-now", "2026-01-14T05:00:00.000Z"},
	} {
		_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
			Session: fixtureSession(s.id, "gamma", "history first", s.ts, 1),
			Messages: []db.Message{
				usagePriceMessage(s.id, 0, s.ts, model, `{"input_tokens":1000000}`),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		}})
		require.NoError(t, err)
	}
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	filter := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	got, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("1"), dayModelCost(t, got, "2026-01-12", model))
	assert.Equal(t, money.MustParseDollars("3"), dayModelCost(t, got, "2026-01-14", model))
	priced := dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC"})
	digest := pricedDigest(t, store.conn)

	require.NoError(t, local.UpsertGenAIPricing(ctx, genAIHistoryDocument(t, "2", "1")))
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	assert.NotEqual(t, digest, pricedDigest(t, store.conn))
	got, err = store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("2"), dayModelCost(t, got, "2026-01-12", model))
	assert.Equal(t, money.MustParseDollars("3"), dayModelCost(t, got, "2026-01-14", model))
	assert.Equal(t, priced.explicit, dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC"}).explicit,
		"the past day is priced from the new digest's records")
	assertDailyUsageParity(t, local, store)
}
