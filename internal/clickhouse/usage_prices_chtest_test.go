//go:build chtest

package clickhouse

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
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
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
	"go.kenn.io/agentsview/internal/storage"
)

const (
	usagePriceRoundID   = "cron_price-check_20260112_030000"
	usagePriceTierID    = "cron_price-check_20260112_031000"
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
		if i < 2 {
			writes[i].Session.Agent = "hermes"
			writes[i].Session.GroupKey = "price-check"
		}
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
	err = store.forEachDailyUsageGroupRow(t.Context(), filter, catalog.digest, customModels, false, func(r chDailyUsageGroupRow) error {
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

func normalizeSQLiteTopSessions(t *testing.T, entries []db.TopSessionEntry) {
	t.Helper()
	for i := range entries {
		if entries[i].StartedAt != "" {
			parsed, err := time.Parse(time.RFC3339Nano, entries[i].StartedAt)
			require.NoError(t, err)
			entries[i].StartedAt = parsed.UTC().Format(time.RFC3339Nano)
		}
		if entries[i].Machine == "local" {
			entries[i].Machine = fixtureMachine
		}
	}
}

func newTopSessionsRetryStore(t *testing.T, target Target, readingContexts *atomic.Bool) *Store {
	t.Helper()
	options, err := parseDSN(target.URL)
	require.NoError(t, err)
	options.Auth.Database = target.Database
	options.Settings["final"] = 1
	options.Debug = true
	options.Debugf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, "sending query") {
			readingContexts.Store(strings.Contains(line, "FROM usage_price_contexts"))
		}
	}
	store := NewStoreFromDB(chdriver.OpenDB(options))
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func TestDailyUsagePersistedPricesMatchSQLite(t *testing.T) {
	t.Parallel()
	store, _, local := newUsagePriceStore(t)
	assertDailyUsageParity(t, local, store)
	for _, filter := range []db.UsageFilter{{}, {Model: " , "}, {ExcludeModel: " , "}, {Model: " , ", ExcludeModel: " , "}} {
		filter.Agent, filter.Timezone = "copilot", "UTC"
		daily, err := store.GetDailyUsage(t.Context(), filter)
		require.NoError(t, err)
		assert.Equal(t, money.MustParseDollars("0.5"), daily.Totals.TotalCost)
		top, err := store.GetTopSessionsByCost(t.Context(), filter, 10)
		require.NoError(t, err)
		require.Len(t, top, 1)
		assert.Equal(t, money.MustParseDollars("0.5"), top[0].Cost)
	}

	// Only the Copilot authoritative cost stays an explicit row; every other
	// event arrives summed.
	stats := dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC"})
	assert.Equal(t, 1, stats.explicit)
	assert.Less(t, stats.rows, stats.events)
}

// TestDailyUsageCodexAutoReviewPricesAsLuna proves the ClickHouse CASE arm and
// the persisted price records price codex-auto-review at Luna rates, matching
// SQLite.
func TestDailyUsageCodexAutoReviewPricesAsLuna(t *testing.T) {
	t.Parallel()
	local, target := seedUsagePriceFixture(t)
	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:     pricingpkg.GPT56LunaCanonical,
		InputPerMTok:     money.MustParseDollars("0.2"),
		OutputPerMTok:    money.MustParseDollars("1.2"),
		CacheReadPerMTok: money.MustParseDollars("0.02"),
	}}))
	ts := "2026-01-12T05:00:00.000Z"
	tokenUsage := `{"cache_read_input_tokens":900000,"input_tokens":100000,"output_tokens":10000}`
	var writes []db.SessionBatchWrite
	for _, fixture := range []struct{ id, model string }{
		{"ch-price-auto-review", pricingpkg.CodexAutoReviewModelName},
		{"ch-price-luna", pricingpkg.GPT56LunaCanonical},
	} {
		session := fixtureSession(fixture.id, "gamma", fixture.id, ts, 1)
		session.Agent = "codex"
		writes = append(writes, db.SessionBatchWrite{
			Session:         session,
			Messages:        []db.Message{usagePriceMessage(fixture.id, 0, ts, fixture.model, tokenUsage)},
			DataVersion:     1,
			ReplaceMessages: true,
		})
	}
	_, err := local.WriteSessionBatchAtomic(t.Context(), writes)
	require.NoError(t, err)

	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	assertDailyUsageParity(t, local, store)

	filter := db.UsageFilter{Timezone: "UTC", Agent: "codex", Breakdowns: true}
	got, err := store.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	review := modelCost(t, got, pricingpkg.CodexAutoReviewModelName)
	assert.NotZero(t, review.Microdollars, "auto-review cost")
	assert.Equal(t, modelCost(t, got, pricingpkg.GPT56LunaCanonical), review)
	stats := dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC", Agent: "codex"})
	assert.Zero(t, stats.explicit)
}

func TestDailyUsagePersistedPricesKeepRequestRounding(t *testing.T) {
	t.Parallel()
	store, _, _ := newUsagePriceStore(t)
	got, err := store.GetDailyUsage(t.Context(), db.UsageFilter{
		Timezone: "UTC", Agent: "hermes",
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
	t.Parallel()
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
	t.Parallel()
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
		Timezone: "UTC", Agent: "hermes",
	})
	require.NoError(t, err)
	assert.Zero(t, modelCost(t, got, "round-test"))
}

func TestDailyUsagePricesFollowPricingRefresh(t *testing.T) {
	t.Parallel()
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
	filter := db.UsageFilter{Timezone: "UTC", Agent: "hermes"}
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
	t.Parallel()
	store, _, local := newUsagePriceStore(t)
	custom := map[string]config.CustomModelRate{
		"round-test": {InputMicrodollarsPerMTok: 5_000_000},
	}
	local.SetCustomPricing(custom)
	store.SetCustomPricing(custom)
	assertDailyUsageParity(t, local, store)

	got, err := store.GetDailyUsage(t.Context(), db.UsageFilter{
		Timezone: "UTC", Agent: "hermes",
	})
	require.NoError(t, err)
	// $5/MTok input: 11 tokens over eight requests cost 55 microdollars.
	assert.Equal(t, money.Money{Microdollars: 55}, modelCost(t, got, "round-test"))
}

func TestPushPricesCursorUsageAddedLater(t *testing.T) {
	t.Parallel()
	store, syncer, local := newUsagePriceStore(t)
	require.NoError(t, local.InsertCursorUsageEvents(t.Context(), []db.CursorUsageEvent{{
		OccurredAt: "2026-01-13T05:00:00.000Z", Model: "tier-test", Kind: "usage",
		InputTokens: 70, OutputTokens: 7, CacheReadTokens: 9,
		Charged: money.MustParseDollars("0.04"), DedupKey: "cursor-3",
	}}))
	_, err := syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assertDailyUsageParity(t, local, store)
	stats := dailyUsageGroupRowStats(t, store, db.UsageFilter{Timezone: "UTC"})
	assert.Equal(t, 1, stats.explicit)
}

// Serve installs custom rates after it opens the store. The background
// work it starts afterwards must price with them, even when a session
// pushed since the last refresh makes the first prepared state load the
// pricing catalog.
func TestServeStoreBackgroundPricesWithCustomRates(t *testing.T) {
	ctx := t.Context()
	store, syncer, local := newUsagePriceStore(t)
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	for _, stmt := range []string{"SYSTEM WAIT VIEW prepare_usage", "SYSTEM STOP VIEW prepare_usage"} {
		_, err := store.DB().ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	appendMessage(t, local, usagePriceRoundID, "one more turn", "2026-01-13T06:00:00.000Z")
	_, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	opened, err := (Backend{}).OpenServeStore(ctx, storage.ReplicaTarget{
		URL: syncer.target.URL, Schema: syncer.target.Database,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, opened.Close()) })
	custom := map[string]config.CustomModelRate{
		"round-test": {InputMicrodollarsPerMTok: 5_000_000},
	}
	opened.SetCustomPricing(custom)
	serve, ok := opened.(*Store)
	require.True(t, ok)
	serve.StartBackground(ctx)
	state, err := serve.preparedUsageState(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, state.changed, "the startup state must price a delta")

	local.SetCustomPricing(custom)
	assertDailyUsageParity(t, local, serve)
}

func TestCopilotCostFollowsLatestEvent(t *testing.T) {
	t.Parallel()
	local, target := seedFixture(t)
	firstCost := money.MustParseDollars("0.30")
	latestCost := money.MustParseDollars("0.50")
	session := fixtureSession(
		"copilot-order", "gamma", "copilot order", "2026-01-12T05:00:00.000Z", 0,
	)
	session.Agent = "copilot"
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: session,
		UsageEvents: []db.UsageEvent{
			{
				Source: "shutdown", Model: "zzz-model", InputTokens: 5,
				Cost: &firstCost, CostStatus: "exact",
				CostSource: db.CopilotReportedCostSource,
				OccurredAt: "2026-01-12T05:01:00.000Z", DedupKey: "copilot-order-first",
			},
			{
				Source: "shutdown", Model: "aaa-model", InputTokens: 5,
				Cost: &latestCost, CostStatus: "exact",
				CostSource: db.CopilotReportedCostSource,
				OccurredAt: "2026-01-12T05:02:00.000Z", DedupKey: "copilot-order-latest",
			},
		},
		DataVersion: 1, ReplaceMessages: true,
	}})
	require.NoError(t, err)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	_, err = store.DB().ExecContext(t.Context(), "SYSTEM WAIT VIEW prepare_usage")
	require.NoError(t, err)

	filter := db.UsageFilter{Timezone: "UTC", Agent: "copilot"}
	costOf := func(entries []db.TopSessionEntry) money.Money {
		for _, e := range entries {
			if e.SessionID == session.ID {
				return e.Cost
			}
		}
		t.Fatalf("session %s missing", session.ID)
		return money.Money{}
	}
	got, err := store.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	assert.Equal(t, latestCost, costOf(got), "clickhouse top")

	lsu, err := local.GetSessionUsage(t.Context(), session.ID, false)
	require.NoError(t, err)
	csu, err := store.GetSessionUsage(t.Context(), session.ID, false)
	require.NoError(t, err)
	assert.Equal(t, lsu.Cost, csu.Cost, "session usage matches sqlite")
	ld, err := local.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	cd, err := store.GetDailyUsage(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, ld.Totals.TotalCost, cd.Totals.TotalCost, "daily usage matches sqlite")
}

func TestTopSessionsStoredPricesMatchSQLite(t *testing.T) {
	t.Parallel()
	rankings := map[string]struct {
		sort       string
		tokenTypes db.UsageTokenTypes
	}{
		"cost":            {},
		"tokens":          {sort: db.TopSessionsSortTokens},
		"tokens selected": {sort: db.TopSessionsSortTokens, tokenTypes: db.UsageTokenTypeCacheRead},
	}
	for _, custom := range []map[string]config.CustomModelRate{
		nil,
		{"round-test": {InputMicrodollarsPerMTok: 2_000_000, OutputMicrodollarsPerMTok: 3_000_000}},
	} {
		store, _, local := newUsagePriceStore(t)
		_, err := store.DB().ExecContext(t.Context(), "SYSTEM WAIT VIEW prepare_usage")
		require.NoError(t, err)
		state, err := store.preparedUsageState(t.Context())
		require.NoError(t, err)
		require.True(t, state.ready, "prepared usage must be ready for this parity test")
		local.SetCustomPricing(custom)
		store.SetCustomPricing(custom)
		catalog, err := chLoadPricingCatalog(t.Context(), store.conn, store.customPricing)
		require.NoError(t, err)
		require.Equal(t, catalog.digest, state.pricingDigest,
			"the prepared usage must carry stored prices for this catalog")
		filters := usagePriceFilters()
		if custom == nil {
			filters["grouped"] = db.UsageFilter{Timezone: "UTC", TopSessionsByGroup: true}
		}
		for name, filter := range filters {
			for rankingName, ranking := range rankings {
				filter.TopSessionsSort = ranking.sort
				filter.TopSessionsTokenTypes = ranking.tokenTypes
				name := name + "/" + rankingName
				want, err := local.GetTopSessionsByCost(t.Context(), filter, 5)
				require.NoError(t, err, name)
				require.NotEmpty(t, want, name)
				normalizeSQLiteTopSessions(t, want)
				got, err := store.GetTopSessionsByCost(t.Context(), filter, 5)
				require.NoError(t, err, name)
				assert.Equal(t, want, got, name)
			}
		}
	}
}

// repointPriceContext moves one round-test price record to a context under a
// new ID with the same content, as a concurrent push adding a context under
// the reader's digest would.
func repointPriceContext(ctx context.Context, conn *sql.DB, id string) error {
	_, err := conn.ExecContext(ctx, `
		INSERT INTO usage_price_contexts (pricing_digest, context_id, context_json, push_version)
		SELECT c.pricing_digest, ?, c.context_json, toUInt64(toUnixTimestamp64Nano(now64(9)))
		FROM usage_price_contexts c
		WHERE c.pricing_digest = (SELECT value FROM sync_metadata WHERE key = ?)
			AND position(c.context_json, '"round-test"') > 0
		LIMIT 1`, id, usagePricedDigestKey)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `
		INSERT INTO usage_event_prices
		SELECT pricing_digest, price_key, token_cost_microdollars, cache_savings_microdollars,
			?, ?, request_scoped, band_above_input_tokens, price_error, priced,
			toUInt64(toUnixTimestamp64Nano(now64(9)))
		FROM usage_event_prices
		WHERE pricing_digest = (SELECT value FROM sync_metadata WHERE key = ?)
			AND billed_context_id IN (
				SELECT context_id FROM usage_price_contexts
				WHERE position(context_json, '"round-test"') > 0)`,
		id, id, usagePricedDigestKey)
	return err
}

func TestTopSessionsRetriesWhenContextsChange(t *testing.T) {
	t.Parallel()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	// Keep reads on raw usage, which join the records the callback changes.
	_, err := syncer.conn.ExecContext(t.Context(), "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)

	// The driver's query log identifies the context SELECT. Its progress
	// callback runs after the server reads those contexts and before the
	// caller can issue the group query.
	var readingContexts atomic.Bool
	var attempts atomic.Int32
	store := newTopSessionsRetryStore(t, target, &readingContexts)
	changed := make(chan error, 1)
	ctx := chdriver.Context(t.Context(), chdriver.WithProgress(func(p *chdriver.Progress) {
		if p.Rows == 0 || !readingContexts.CompareAndSwap(true, false) {
			return
		}
		if attempts.Add(1) == 1 {
			changed <- repointPriceContext(t.Context(), syncer.conn, "changed-context")
		}
	}))
	filter := db.UsageFilter{Timezone: "UTC", Model: "round-test"}
	top, err := store.GetTopSessionsByCost(ctx, filter, 10)
	require.NoError(t, <-changed)
	require.NoError(t, err)
	assert.EqualValues(t, 2, attempts.Load(), "the read retries once with the new context")
	want, err := local.GetTopSessionsByCost(t.Context(), filter, 10)
	require.NoError(t, err)
	normalizeSQLiteTopSessions(t, want)
	assert.Equal(t, want, top)
}

// A read whose records keep naming contexts it did not load prices every
// row per request once its retries are spent.
func TestTopSessionsPricePerRequestWhenRetriesExhaust(t *testing.T) {
	t.Parallel()
	local, target := seedUsagePriceFixture(t)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	// Keep reads on raw usage, which join the records the callback changes.
	_, err := syncer.conn.ExecContext(t.Context(), "SYSTEM STOP VIEW prepare_usage")
	require.NoError(t, err)
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)

	var readingContexts atomic.Bool
	var attempts atomic.Int32
	store := newTopSessionsRetryStore(t, target, &readingContexts)
	changes := make(chan error, 3)
	ctx := chdriver.Context(t.Context(), chdriver.WithProgress(func(p *chdriver.Progress) {
		if p.Rows == 0 || !readingContexts.CompareAndSwap(true, false) {
			return
		}
		attempt := attempts.Add(1)
		// A fourth attempt gets no change, so an excessive retry returns
		// success and fails the exhaustion assertion instead of looping
		// indefinitely.
		if attempt > 3 {
			return
		}
		changes <- repointPriceContext(t.Context(), syncer.conn, fmt.Sprintf("changed-context-%d", attempt))
	}))
	filter := db.UsageFilter{Timezone: "UTC", Project: "gamma"}
	top, err := store.GetTopSessionsByCost(ctx, filter, 20)
	require.NoError(t, err)
	require.EqualValues(t, 3, attempts.Load())
	for range 3 {
		require.NoError(t, <-changes)
	}
	want, err := local.GetTopSessionsByCost(t.Context(), filter, 20)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	normalizeSQLiteTopSessions(t, want)
	assert.Equal(t, want, top)
}

// Records can outlive their contexts when a push revives a digest while
// another push drops its partitions. Reads still answer, priced per request.
func TestUsageReadsPriceRecordsWithoutContextsPerRequest(t *testing.T) {
	t.Parallel()
	store, _, local := newUsagePriceStore(t)
	ctx := t.Context()
	digest := pricedDigest(t, store.conn)
	require.NotZero(t, pricingDigestRows(t, store.conn)[digest])
	_, err := store.conn.ExecContext(ctx,
		"ALTER TABLE usage_price_contexts DROP PARTITION ?", digest)
	require.NoError(t, err)

	assertDailyUsageParity(t, local, store)
	filter := db.UsageFilter{Timezone: "UTC"}
	want, err := local.GetTopSessionsByCost(ctx, filter, 20)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	normalizeSQLiteTopSessions(t, want)
	top, err := store.GetTopSessionsByCost(ctx, filter, 20)
	require.NoError(t, err)
	assert.Equal(t, want, top)
}
