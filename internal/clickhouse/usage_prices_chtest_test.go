//go:build chtest

package clickhouse

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/storage"
)

const (
	usagePriceRoundID   = "ch-price-round"
	usagePriceTierID    = "ch-price-tier"
	usagePriceSnapAID   = "ch-price-snap-a"
	usagePriceSnapBID   = "ch-price-snap-b"
	usagePriceCopilotID = "ch-price-copilot"
	usagePriceMixedID   = "ch-price-mixed"
)

func usagePriceMessage(
	sessionID string, ordinal int, ts, model, tokenUsage string,
) db.Message {
	m := fixtureMessage(sessionID, ordinal, "assistant", "usage", ts)
	m.Model = model
	m.TokenUsage = []byte(tokenUsage)
	return m
}

// seedUsagePriceFixture extends seedFixture with sessions that exercise
// per-request rounding, tiered rates, Claude snapshot survivors, a Copilot
// authoritative cost, reported costs, a provider discount, an unpriced model,
// and Cursor admin usage.
func seedUsagePriceFixture(t *testing.T) (*db.DB, Target) {
	t.Helper()
	local, target := seedFixture(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:  "round-test",
			InputPerMTok:  money.MustParseDollars("0.4"),
			OutputPerMTok: money.MustParseDollars("0.6"),
		},
		{
			ModelPattern:     "tier-test",
			InputPerMTok:     money.MustParseDollars("1"),
			OutputPerMTok:    money.MustParseDollars("10"),
			CacheReadPerMTok: money.MustParseDollars("0.1"),
			Bands: []db.PricingBand{{
				AboveInputTokens: 1000,
				InputPerMTok:     money.MustParseDollars("2"),
				OutputPerMTok:    money.MustParseDollars("20"),
				CacheReadPerMTok: money.MustParseDollars("0.2"),
			}},
		},
	}))

	day := "2026-01-12"
	at := func(minute int) string {
		return fmt.Sprintf("%sT03:%02d:00.000Z", day, minute)
	}
	var roundMessages []db.Message
	for i := range 5 {
		roundMessages = append(roundMessages, usagePriceMessage(
			usagePriceRoundID, i, at(i), "round-test", `{"input_tokens":1}`))
	}
	for i := 5; i < 8; i++ {
		roundMessages = append(roundMessages, usagePriceMessage(
			usagePriceRoundID, i, at(i), "round-test", `{"input_tokens":2}`))
	}

	snapshot := func(sessionID, ts, tokenUsage string) db.Message {
		m := usagePriceMessage(sessionID, 0, ts, "claude-test", tokenUsage)
		m.ClaudeMessageID = "msg_price_1"
		m.ClaudeRequestID = "req_price_1"
		return m
	}
	posit := usagePriceMessage(usagePriceMixedID, 0, at(30), "claude-test",
		`{"input_tokens":1000,"output_tokens":100,"cache_read_input_tokens":400}`)
	posit.ProviderID = pricingpkg.PositAssistantProviderID
	copilotCost := money.MustParseDollars("0.5")
	reportedCost := money.MustParseDollars("0.0123")
	copilot := fixtureSession(usagePriceCopilotID, "gamma", "copilot first", at(40), 2)
	copilot.Agent = "copilot"

	writes := []db.SessionBatchWrite{
		{
			Session:  fixtureSession(usagePriceRoundID, "gamma", "round first", at(0), len(roundMessages)),
			Messages: roundMessages,
		},
		{
			Session: fixtureSession(usagePriceTierID, "gamma", "tier first", at(10), 2),
			Messages: []db.Message{
				usagePriceMessage(usagePriceTierID, 0, at(10), "tier-test",
					`{"input_tokens":500,"output_tokens":100}`),
				usagePriceMessage(usagePriceTierID, 1, at(11), "tier-test",
					`{"input_tokens":900,"output_tokens":100,"cache_read_input_tokens":1100}`),
			},
			UsageEvents: []db.UsageEvent{{
				Source: "hermes", Model: "tier-test", InputTokens: 5000,
				OutputTokens: 10, OccurredAt: at(12), DedupKey: "tier-aggregate",
			}},
		},
		{
			Session: fixtureSession(usagePriceSnapAID, "gamma", "snap a", at(20), 1),
			Messages: []db.Message{snapshot(usagePriceSnapAID, at(20),
				`{"input_tokens":7,"output_tokens":5,"server_tool_use":{"web_search_requests":2}}`)},
		},
		{
			Session: fixtureSession(usagePriceSnapBID, "delta", "snap b", at(21), 1),
			Messages: []db.Message{snapshot(usagePriceSnapBID, at(21),
				`{"input_tokens":7,"output_tokens":50}`)},
		},
		{
			Session: fixtureSession(usagePriceMixedID, "delta", "mixed first", at(30), 4),
			Messages: []db.Message{
				posit,
				usagePriceMessage(usagePriceMixedID, 1, at(31), "mystery-model",
					`{"input_tokens":11,"output_tokens":13}`),
				usagePriceMessage(usagePriceMixedID, 2, at(32), "claude-test",
					`{"input_tokens":3,"output_tokens":0,"reasoning_tokens":9}`),
				// Late evening UTC: a different local date west of Greenwich
				// than the rest of the session.
				usagePriceMessage(usagePriceMixedID, 3, day+"T23:30:00.000Z", "claude-test",
					`{"input_tokens":21,"output_tokens":34,"cache_creation_input_tokens":55}`),
			},
			UsageEvents: []db.UsageEvent{{
				Source: "hermes", Model: "claude-test", InputTokens: 300,
				OutputTokens: 30, CacheReadInputTokens: 60, Cost: &reportedCost,
				CostStatus: "reported", CostSource: string(export.CostSourceReported),
				OccurredAt: at(33), DedupKey: "mixed-reported",
			}},
		},
		{
			Session: copilot,
			Messages: []db.Message{
				usagePriceMessage(usagePriceCopilotID, 0, at(40), "claude-test",
					`{"input_tokens":100,"output_tokens":10}`),
				usagePriceMessage(usagePriceCopilotID, 1, at(41), "round-test",
					`{"input_tokens":100000,"output_tokens":10}`),
			},
			UsageEvents: []db.UsageEvent{{
				Source: "copilot-shutdown", Model: "claude-test", InputTokens: 50,
				Cost:       &copilotCost,
				CostStatus: "reported", CostSource: db.CopilotReportedCostSource,
				OccurredAt: at(42), DedupKey: "copilot-total",
			}},
		},
	}
	for i := range writes {
		writes[i].DataVersion = 1
		writes[i].ReplaceMessages = true
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err)
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{
		{
			OccurredAt: at(50), Model: "claude-test", Kind: "usage",
			InputTokens: 40, OutputTokens: 4, CacheReadTokens: 8,
			Charged: money.MustParseDollars("0.02"), DedupKey: "cursor-1",
		},
		{
			OccurredAt: at(51), Model: "claude-test", Kind: "usage",
			InputTokens: 41, OutputTokens: 5, IsHeadless: true,
			Charged: money.MustParseDollars("0.03"), DedupKey: "cursor-2",
		},
	}))
	return local, target
}

func usagePriceFilters() map[string]db.UsageFilter {
	return map[string]db.UsageFilter{
		"utc":        {Timezone: "UTC"},
		"breakdowns": {Timezone: "UTC", Breakdowns: true},
		"new york":   {Timezone: "America/New_York", Breakdowns: true},
		"window":     {Timezone: "UTC", From: "2026-01-12", To: "2026-01-12", Breakdowns: true},
		"model":      {Timezone: "UTC", Model: "round-test", Breakdowns: true},
		"no model":   {Timezone: "UTC", ExcludeModel: "claude-test"},
		"agent":      {Timezone: "UTC", Agent: "claude", Breakdowns: true},
		"project":    {Timezone: "UTC", Project: "delta", Breakdowns: true},
		"human":      {Timezone: "UTC", AutomatedScope: "human"},
	}
}

// dailyUsageWire renders the fields both backends compute as the API sends
// them. The project identity map is resolved by separate code and is covered
// elsewhere. Push publishes the archive's "local" machine under the pusher's
// machine name, so SQLite results are compared under that name.
func dailyUsageWire(t *testing.T, result db.DailyUsageResult, sqlite bool) string {
	t.Helper()
	if sqlite {
		for i := range result.Daily {
			for j := range result.Daily[i].MachineBreakdowns {
				if result.Daily[i].MachineBreakdowns[j].MachineName == "local" {
					result.Daily[i].MachineBreakdowns[j].MachineName = fixtureMachine
				}
			}
		}
	}
	data, err := json.Marshal(map[string]any{
		"daily":          result.Daily,
		"totals":         result.Totals,
		"pricing":        result.Pricing,
		"session_counts": result.SessionCounts,
	}, json.Deterministic(true), jsontext.WithIndent("  "))
	require.NoError(t, err)
	return string(data)
}

func assertDailyUsageParity(
	t *testing.T, local *db.DB, store *Store,
) {
	t.Helper()
	for name, filter := range usagePriceFilters() {
		want, err := local.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err, name)
		got, err := store.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err, name)
		require.NotEmpty(t, got.Daily, name)
		assert.Equal(t, dailyUsageWire(t, want, true), dailyUsageWire(t, got, false), name)
	}
}

type usageGroupRowStats struct {
	rows, explicit, events int
}

func dailyUsageGroupRowStats(
	t *testing.T, store *Store, filter db.UsageFilter,
) usageGroupRowStats {
	t.Helper()
	catalog, err := chLoadPricingCatalog(t.Context(), store.conn, store.customPricing)
	require.NoError(t, err)
	contexts, err := loadUsagePriceContexts(t.Context(), store.conn, catalog.digest)
	require.NoError(t, err)
	customModels := chCustomPricedModels(contexts, export.NewPricingResolver(catalog.rows))
	var stats usageGroupRowStats
	err = store.forEachDailyUsageGroupRow(t.Context(), filter, catalog.digest, customModels, func(r chDailyUsageGroupRow) error {
		stats.rows++
		stats.events += r.events
		if r.explicit {
			stats.explicit++
		}
		return nil
	})
	require.NoError(t, err)
	return stats
}

func newUsagePriceStore(t *testing.T) (*Store, *Sync, *db.DB) {
	t.Helper()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store, syncer, local
}

func modelCost(t *testing.T, result db.DailyUsageResult, model string) money.Money {
	t.Helper()
	var total money.Money
	for _, day := range result.Daily {
		for _, breakdown := range day.ModelBreakdowns {
			if breakdown.ModelName != model {
				continue
			}
			var err error
			total, err = money.Add(total, breakdown.Cost)
			require.NoError(t, err)
		}
	}
	return total
}

func TestDailyUsagePersistedPricesMatchSQLite(t *testing.T) {
	store, _, local := newUsagePriceStore(t)
	assertDailyUsageParity(t, local, store)

	// Only the Copilot authoritative cost stays an explicit row; every other
	// event arrives summed.
	stats := dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC"})
	assert.Equal(t, 1, stats.explicit)
	assert.Less(t, stats.rows, stats.events)
}

func TestDailyUsagePersistedPricesKeepRequestRounding(t *testing.T) {
	store, _, _ := newUsagePriceStore(t)
	got, err := store.GetDailyUsage(t.Context(), db.UsageFilter{
		Timezone: "UTC", Agent: "claude",
	})
	require.NoError(t, err)

	// round-test bills $0.4/MTok input. One token is 0.4 microdollars and
	// rounds to 0 per request; two tokens are 0.8 and round to 1. Five
	// one-token and three two-token requests cost 3 microdollars. Rounding
	// the 11-token sum instead would give 4.
	assert.Equal(t, money.Money{Microdollars: 3}, modelCost(t, got, "round-test"))

	// tier-test: the 500-token request bills base rates (500*1 + 100*10 =
	// 1500). The second request has 900 + 1100 cached = 2000 input tokens,
	// above the 1000 band: 900*2 + 100*20 + 1100*0.2 = 4020. The hermes event
	// is not request scoped and bills base rates: 5000*1 + 10*10 = 5100.
	assert.Equal(t, money.Money{Microdollars: 1500 + 4020 + 5100},
		modelCost(t, got, "tier-test"))
	require.NotNil(t, got.Pricing)
	tier := got.Pricing.Models["tier-test"]
	require.Len(t, tier.Resolutions, 1)
	application := tier.Resolutions[0].Application
	assert.Equal(t, 1, application.BaseRequestCount)
	assert.Equal(t, 1, application.AggregateRowCount)
	require.Len(t, application.Bands, 1)
	assert.Equal(t, 1000, application.Bands[0].AboveInputTokens)
	assert.Equal(t, 1, application.Bands[0].RequestCount)
}

func TestDailyUsagePricesMissingRecordsForTheRead(t *testing.T) {
	store, syncer, local := newUsagePriceStore(t)
	// Events without billable usage take the resolver's zero-usage path.
	zero := fixtureSession("ch-price-zero", "delta", "zero first", "2026-01-12T05:00:00.000Z", 2)
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: zero,
		Messages: []db.Message{
			usagePriceMessage(zero.ID, 0, "2026-01-12T05:00:00.000Z", "claude-test",
				`{"input_tokens":0,"output_tokens":0}`),
			usagePriceMessage(zero.ID, 1, "2026-01-12T05:01:00.000Z", "claude-test",
				`{"input_tokens":0,"server_tool_use":{"web_search_requests":3}}`),
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)

	filter := db.UsageFilter{Timezone: "UTC", Breakdowns: true}
	warm, err := store.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	warmStats := dailyUsageGroupRowStats(t, store, filter)

	// A mirror with no price records is an ordinary cache miss.
	for _, table := range []string{"usage_event_prices", "usage_price_contexts"} {
		_, err := store.conn.ExecContext(t.Context(), "TRUNCATE TABLE "+table)
		require.NoError(t, err)
	}
	cold, err := store.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, dailyUsageWire(t, warm, false), dailyUsageWire(t, cold, false))
	want, err := local.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, want.Totals, cold.Totals)

	coldStats := dailyUsageGroupRowStats(t, store, filter)
	assert.Equal(t, coldStats.rows, coldStats.explicit)
	assert.Equal(t, warmStats.events, coldStats.events)

	// The reader never writes: the records are still absent.
	var records int
	require.NoError(t, store.conn.QueryRowContext(t.Context(),
		"SELECT count() FROM usage_event_prices").Scan(&records))
	assert.Zero(t, records)
}

func TestDailyUsagePricesFollowSourceUpdatesAndDeletes(t *testing.T) {
	store, syncer, local := newUsagePriceStore(t)

	appendMessage(t, local, usagePriceTierID, "later", "2026-01-13T04:00:00.000Z")
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assertDailyUsageParity(t, local, store)
	filter := db.UsageFilter{Timezone: "UTC"}
	assert.Equal(t, 1, dailyUsageGroupRowStats(t, store, filter).explicit,
		"the pushed batch prices its own new events")

	require.NoError(t, local.SoftDeleteSession(t.Context(), usagePriceRoundID))
	deleted, err := local.DeleteSessionIfTrashed(t.Context(), usagePriceRoundID)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	// Deleting the first snapshot session moves the survivor's attribution
	// to the remaining session.
	require.NoError(t, local.SoftDeleteSession(t.Context(), usagePriceSnapAID))
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assertDailyUsageParity(t, local, store)
	got, err := store.GetDailyUsage(t.Context(), db.UsageFilter{
		Timezone: "UTC", Agent: "claude",
	})
	require.NoError(t, err)
	assert.Zero(t, modelCost(t, got, "round-test"))
}

func TestDailyUsagePricesFollowPricingRefresh(t *testing.T) {
	store, syncer, local := newUsagePriceStore(t)
	ctx := t.Context()
	before, err := chLoadPricingCatalog(ctx, store.conn, nil)
	require.NoError(t, err)
	meta, err := readMetadata(ctx, store.conn, usagePricedDigestKey, schemaVersionKey)
	require.NoError(t, err)
	assert.Equal(t, before.digest, meta[usagePricedDigestKey])
	assert.Equal(t, strconv.Itoa(SchemaVersion), meta[schemaVersionKey])

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:  "round-test",
		InputPerMTok:  money.MustParseDollars("0.8"),
		OutputPerMTok: money.MustParseDollars("0.6"),
	}}))

	// The catalog reaches the mirror before any price record does, as when
	// another exporter publishes new rates. Reads use the new rates at once.
	require.NoError(t, syncer.syncModelPricing(ctx))
	after, err := chLoadPricingCatalog(ctx, store.conn, nil)
	require.NoError(t, err)
	require.NotEqual(t, before.digest, after.digest)
	assertDailyUsageParity(t, local, store)
	filter := db.UsageFilter{Timezone: "UTC", Agent: "claude"}
	got, err := store.GetDailyUsage(ctx, filter)
	require.NoError(t, err)
	// 0.8 and 1.6 microdollars round to 1 and 2: 5*1 + 3*2.
	assert.Equal(t, money.Money{Microdollars: 11}, modelCost(t, got, "round-test"))
	stats := dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC"})
	assert.Equal(t, stats.rows, stats.explicit)

	// The next push prices the mirror under the new digest and keeps the
	// records other exporters and in-flight readers may still use.
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	assertDailyUsageParity(t, local, store)
	stats = dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC"})
	assert.Equal(t, 1, stats.explicit)
	meta, err = readMetadata(ctx, store.conn, usagePricedDigestKey)
	require.NoError(t, err)
	assert.Equal(t, after.digest, meta[usagePricedDigestKey])
	for _, digest := range []string{before.digest, after.digest} {
		var records int
		require.NoError(t, store.conn.QueryRowContext(ctx,
			"SELECT count() FROM usage_event_prices WHERE pricing_digest = ?",
			digest).Scan(&records))
		assert.Positive(t, records, digest)
	}
}

func TestDailyUsageCustomRatesOverridePersistedPrices(t *testing.T) {
	store, _, local := newUsagePriceStore(t)
	custom := map[string]config.CustomModelRate{
		"round-test": {InputMicrodollarsPerMTok: 5_000_000},
	}
	local.SetCustomPricing(custom)
	store.SetCustomPricing(custom)
	assertDailyUsageParity(t, local, store)

	got, err := store.GetDailyUsage(t.Context(), db.UsageFilter{
		Timezone: "UTC", Agent: "claude",
	})
	require.NoError(t, err)
	// $5/MTok input: 11 tokens over eight requests cost 55 microdollars.
	assert.Equal(t, money.Money{Microdollars: 55}, modelCost(t, got, "round-test"))
}

func TestPushPricesCursorUsageAddedLater(t *testing.T) {
	store, syncer, local := newUsagePriceStore(t)
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{{
		OccurredAt: "2026-01-13T05:00:00.000Z", Model: "tier-test", Kind: "usage",
		InputTokens: 70, OutputTokens: 7, CacheReadTokens: 9,
		Charged: money.MustParseDollars("0.04"), DedupKey: "cursor-3",
	}}))
	_, err := syncer.Push(context.Background(), false, nil)
	require.NoError(t, err)
	assertDailyUsageParity(t, local, store)
	stats := dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC"})
	assert.Equal(t, 1, stats.explicit)
}
