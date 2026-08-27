package duckdb

import (
	"context"
	"encoding/json/jsontext"
	"testing"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDailyUsageKimiDateAliasPricing proves the DuckDB aggregate path
// prices the date-ambiguous Kimi aliases per row by timestamp, in
// parity with the SQLite path: rows before the 2026-07-19T00:00:00Z
// UTC cutoff use the K2.6 canonical rates, rows at/after it use K3,
// and a single day straddling the cutoff splits into both eras inside
// one SQL group (the price_model CASE keeps the eras in separate
// groups before tokens are summed).
func TestDailyUsageKimiDateAliasPricing(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:     "moonshot/kimi-k2.6",
			InputPerMTok:     money.MustParseDollars("0.95"),
			OutputPerMTok:    money.MustParseDollars("4.0"),
			CacheReadPerMTok: money.MustParseDollars("0.16"),
		},
		{
			ModelPattern:     "kimi-k3",
			InputPerMTok:     money.MustParseDollars("3.00"),
			OutputPerMTok:    money.MustParseDollars("15.00"),
			CacheReadPerMTok: money.MustParseDollars("0.30"),
		},
		{
			ModelPattern:     "k3",
			InputPerMTok:     money.MustParseDollars("3.00"),
			OutputPerMTok:    money.MustParseDollars("15.00"),
			CacheReadPerMTok: money.MustParseDollars("0.30"),
		},
	}), "UpsertModelPricing")

	// Token mix per message: 1M input + 100k output + 1M cache read.
	// K2.6 cost: 0.95 + 0.40 + 0.16 = 1.51
	// K3 cost:   3.00 + 1.50 + 0.30 = 4.80
	tokenUsage := jsontext.Value(
		`{"input_tokens":1000000,"output_tokens":100000,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":1000000}`)
	kimiMessage := func(sessionID string, ordinal int, model, ts string) db.Message {
		return db.Message{
			SessionID:  sessionID,
			Ordinal:    ordinal,
			Role:       "assistant",
			Timestamp:  ts,
			Model:      model,
			TokenUsage: tokenUsage,
		}
	}

	preSession := syncSession(
		"duck-kimi-pre", "alpha", "pre cutoff",
		"2026-07-18T12:00:00.000Z", 1)
	preSession.Agent = "kimi"
	postSession := syncSession(
		"duck-kimi-post", "alpha", "post cutoff",
		"2026-07-20T12:00:00.000Z", 2)
	postSession.Agent = "kimi"

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{
		{
			Session: preSession,
			Messages: []db.Message{
				kimiMessage("duck-kimi-pre", 0,
					"kimi-for-coding", "2026-07-18T12:00:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
		{
			Session: postSession,
			Messages: []db.Message{
				// Same model, same day, both eras: the SQL price_model
				// CASE must split this group before summing.
				kimiMessage("duck-kimi-post", 0,
					"kimi-for-coding", "2026-07-18T12:00:00.000Z"),
				kimiMessage("duck-kimi-post", 1,
					"kimi-for-coding", "2026-07-19T00:00:00.000Z"),
			},
			DataVersion:     1,
			ReplaceMessages: true,
		},
	})
	require.NoError(t, err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-07-01",
		To:       "2026-07-31",
		Timezone: "UTC",
		Model:    "kimi-for-coding",
	})
	require.NoError(t, err)

	require.Len(t, got.Daily, 2, "one entry per active day")
	assert.Equal(t, money.MustParseDollars("3.02"), got.Daily[0].TotalCost,
		"pre-cutoff day prices both rows at K2.6 rates")
	assert.Equal(t, money.MustParseDollars("4.80"), got.Daily[1].TotalCost,
		"post-cutoff day prices at K3 rates")

	require.NotNil(t, got.Pricing, "pricing block")
	require.Contains(t, got.Pricing.Models, "kimi-for-coding")
	resolutions := got.Pricing.Models["kimi-for-coding"].Resolutions
	require.Len(t, resolutions, 2)
	assert.Equal(t, "kimi-k3", resolutions[0].PricedModel)
	assert.Equal(t, "moonshot/kimi-k2.6", resolutions[1].PricedModel)
	assert.NotContains(t, got.Pricing.Models, "moonshot/kimi-k2.6")
	assert.NotContains(t, got.Pricing.Models, "kimi-k3")
}

func TestDailyUsageKimiFixedK26AliasPricing(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "moonshot/kimi-k2.6",
		InputPerMTok: money.MustParseDollars("0.95"),
	}}), "UpsertModelPricing")

	session := syncSession(
		"duck-kimi-k2d6", "alpha", "fixed K2.6 alias",
		"2026-07-20T12:00:00.000Z", 1)
	session.Agent = "kimi-work"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: session,
		Messages: []db.Message{{
			SessionID: "duck-kimi-k2d6",
			Ordinal:   0,
			Role:      "assistant",
			Timestamp: "2026-07-20T12:00:00.000Z",
			Model:     "k2d6-agent",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000000,"output_tokens":0}`),
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-07-20",
		To:       "2026-07-20",
		Timezone: "UTC",
		Model:    "k2d6-agent",
	})
	require.NoError(t, err)
	assert.Equal(t, money.MustParseDollars("0.95"), got.Totals.TotalCost)
	require.NotNil(t, got.Pricing)
	resolutions := got.Pricing.Models["k2d6-agent"].Resolutions
	require.Len(t, resolutions, 1)
	assert.Equal(t, "moonshot/kimi-k2.6", resolutions[0].PricedModel)
}

// TestSessionUsageKimiDateAliasPricing proves the per-row session
// usage path (breakdown rows) applies the same date-based mapping as
// the aggregate path.
func TestSessionUsageKimiDateAliasPricing(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:     "moonshot/kimi-k2.6",
			InputPerMTok:     money.MustParseDollars("0.95"),
			OutputPerMTok:    money.MustParseDollars("4.0"),
			CacheReadPerMTok: money.MustParseDollars("0.16"),
		},
		{
			ModelPattern:     "kimi-k3",
			InputPerMTok:     money.MustParseDollars("3.00"),
			OutputPerMTok:    money.MustParseDollars("15.00"),
			CacheReadPerMTok: money.MustParseDollars("0.30"),
		},
	}), "UpsertModelPricing")

	tokenUsage := jsontext.Value(
		`{"input_tokens":1000000,"output_tokens":100000,` +
			`"cache_creation_input_tokens":0,"cache_read_input_tokens":1000000}`)
	session := syncSession(
		"duck-kimi-session", "alpha", "date alias session",
		"2026-07-18T12:00:00.000Z", 2)
	session.Agent = "kimi"

	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: session,
		Messages: []db.Message{
			{
				SessionID:  "duck-kimi-session",
				Ordinal:    0,
				Role:       "assistant",
				Timestamp:  "2026-07-18T12:00:00.000Z",
				Model:      "kimi-for-coding",
				TokenUsage: tokenUsage,
			},
			{
				SessionID:  "duck-kimi-session",
				Ordinal:    1,
				Role:       "assistant",
				Timestamp:  "2026-07-20T12:00:00.000Z",
				Model:      "kimi-for-coding",
				TokenUsage: tokenUsage,
			},
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	usage, err := store.GetSessionUsage(ctx, "duck-kimi-session", true)
	require.NoError(t, err)
	require.NotNil(t, usage)

	assert.True(t, usage.HasCost, "session must be priced")
	assert.Equal(t, money.MustParseDollars("6.31"), usage.Cost,
		"session cost must sum the K2.6 and K3 eras")
	require.Len(t, usage.Breakdown, 2, "one breakdown entry per row")
	assert.Equal(t, money.MustParseDollars("1.51"), usage.Breakdown[0].Cost,
		"pre-cutoff breakdown row at K2.6 rates")
	assert.Equal(t, money.MustParseDollars("4.80"), usage.Breakdown[1].Cost,
		"post-cutoff breakdown row at K3 rates")
}

func TestSessionUsageKimiExactCustomAliasPricing(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)
	customPricing := map[string]config.CustomModelRate{
		"kimi-for-coding": {
			InputMicrodollarsPerMTok: money.MustParseDollars("7").Microdollars,
		},
	}
	local.SetCustomPricing(customPricing)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "kimi-k3",
		InputPerMTok: money.MustParseDollars("2"),
	}}), "UpsertModelPricing")

	session := syncSession(
		"duck-kimi-custom-alias", "alpha", "custom alias session",
		"2026-07-20T12:00:00.000Z", 1)
	session.Agent = "kimi"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: session,
		Messages: []db.Message{{
			SessionID: "duck-kimi-custom-alias",
			Ordinal:   0,
			Role:      "assistant",
			Timestamp: "2026-07-20T12:00:00.000Z",
			Model:     "kimi-for-coding",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000000,"output_tokens":0}`),
		}},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	want, err := local.GetSessionUsage(ctx, "duck-kimi-custom-alias", true)
	require.NoError(t, err)
	require.NotNil(t, want)
	require.Len(t, want.Breakdown, 1)
	assert.Equal(t, money.MustParseDollars("7"), want.Cost)
	assert.Equal(t, want.Cost, want.Breakdown[0].Cost)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())
	store.SetCustomPricing(customPricing)

	usage, err := store.GetSessionUsage(ctx, "duck-kimi-custom-alias", true)
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.True(t, usage.HasCost)
	assert.Equal(t, money.MustParseDollars("7"), usage.Cost,
		"session total must use the exact reported-model override")
	assert.Equal(t, want.Cost, usage.Cost)
	require.Len(t, usage.Breakdown, 1)
	assert.True(t, usage.Breakdown[0].HasCost)
	assert.Equal(t, want.Breakdown[0].Cost, usage.Breakdown[0].Cost,
		"DuckDB breakdown must match SQLite's exact reported-model override")
}

// TestDailyUsageClaude1hCacheWritePricing replays issue #1452's sample
// session through the DuckDB mirror: the nested cache_creation TTL split
// prices 1h writes at the 1h rate, matching Claude Code's total_cost_usd.
func TestDailyUsageClaude1hCacheWritePricing(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:           "claude-fable-5",
		InputPerMTok:           money.MustParseDollars("10.0"),
		OutputPerMTok:          money.MustParseDollars("50.0"),
		CacheCreationPerMTok:   money.MustParseDollars("12.50"),
		CacheCreation1hPerMTok: money.MustParseDollars("20.0"),
		CacheReadPerMTok:       money.MustParseDollars("1.00"),
	}}), "UpsertModelPricing")

	session := syncSession(
		"duck-1h-cache", "alpha", "1h cache writes",
		"2026-08-13T11:59:00.000Z", 1)
	session.Agent = "claude"
	_, err := local.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: session,
		Messages: []db.Message{
			{
				SessionID: "duck-1h-cache", Ordinal: 0,
				Role: "assistant", Timestamp: "2026-08-13T12:00:05.000Z",
				Model: "claude-fable-5",
				TokenUsage: jsontext.Value(
					`{"input_tokens":2,"output_tokens":62,` +
						`"cache_creation_input_tokens":8989,` +
						`"cache_read_input_tokens":15892,` +
						`"cache_creation":{"ephemeral_1h_input_tokens":8989,` +
						`"ephemeral_5m_input_tokens":0}}`),
			},
			{
				SessionID: "duck-1h-cache", Ordinal: 1,
				Role: "assistant", Timestamp: "2026-08-13T12:01:00.000Z",
				Model: "claude-fable-5",
				TokenUsage: jsontext.Value(
					`{"input_tokens":2,"output_tokens":6,` +
						`"cache_creation_input_tokens":77,` +
						`"cache_read_input_tokens":24881,` +
						`"cache_creation":{"ephemeral_1h_input_tokens":77,` +
						`"ephemeral_5m_input_tokens":0}}`),
			},
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)

	syncer := newInMemoryTestSync(t, local, SyncOptions{})
	require.NoError(t, createSchema(ctx, syncer.DB()))
	_, err = syncer.pushEverything(ctx, nil)
	require.NoError(t, err)
	store := NewStoreFromDB(syncer.DB())

	got, err := store.GetDailyUsage(ctx, db.UsageFilter{
		From:     "2026-08-01",
		To:       "2026-08-31",
		Timezone: "UTC",
	})
	require.NoError(t, err)

	// 2x10 + 62x50 + 8989x20 + 15892x1 = $0.198792, plus
	// 2x10 + 6x50 + 77x20 + 24881x1 = $0.026741: $0.225533 total,
	// matching Claude Code's own total_cost_usd. The 5m-rate misprice
	// would read $0.157539.
	require.Len(t, got.Daily, 1)
	assert.Equal(t, money.Money{Microdollars: 225_533},
		got.Daily[0].TotalCost)

	usage, err := store.GetSessionUsage(ctx, "duck-1h-cache", false)
	require.NoError(t, err)
	assert.Equal(t, money.Money{Microdollars: 225_533}, usage.Cost,
		"per-session cost")
}
