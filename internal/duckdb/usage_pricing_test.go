package duckdb

import (
	"context"
	"encoding/json"
	"testing"

	"go.kenn.io/agentsview/internal/db"

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
			ModelPattern: "moonshot/kimi-k2.6",
			InputPerMTok: 0.95, OutputPerMTok: 4.0,
			CacheCreationPerMTok: 0, CacheReadPerMTok: 0.16,
		},
		{
			ModelPattern: "kimi-k3",
			InputPerMTok: 3.00, OutputPerMTok: 15.00,
			CacheCreationPerMTok: 0, CacheReadPerMTok: 0.30,
		},
		{
			ModelPattern: "k3",
			InputPerMTok: 3.00, OutputPerMTok: 15.00,
			CacheCreationPerMTok: 0, CacheReadPerMTok: 0.30,
		},
	}), "UpsertModelPricing")

	// Token mix per message: 1M input + 100k output + 1M cache read.
	// K2.6 cost: 0.95 + 0.40 + 0.16 = 1.51
	// K3 cost:   3.00 + 1.50 + 0.30 = 4.80
	tokenUsage := json.RawMessage(
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
	assert.InDelta(t, 1.51+1.51, got.Daily[0].TotalCost, 1e-9,
		"pre-cutoff day prices both rows at K2.6 rates")
	assert.InDelta(t, 4.80, got.Daily[1].TotalCost, 1e-9,
		"post-cutoff day prices at K3 rates")

	require.NotNil(t, got.Pricing, "pricing block")
	assert.Contains(t, got.Pricing.Models, "moonshot/kimi-k2.6")
	assert.Contains(t, got.Pricing.Models, "kimi-k3")
	assert.NotContains(t, got.Pricing.Models, "kimi-for-coding")
}

// TestSessionUsageKimiDateAliasPricing proves the per-row session
// usage path (breakdown rows) applies the same date-based mapping as
// the aggregate path.
func TestSessionUsageKimiDateAliasPricing(t *testing.T) {
	ctx := context.Background()
	local := newLocalDB(t)

	require.NoError(t, local.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern: "moonshot/kimi-k2.6",
			InputPerMTok: 0.95, OutputPerMTok: 4.0,
			CacheCreationPerMTok: 0, CacheReadPerMTok: 0.16,
		},
		{
			ModelPattern: "kimi-k3",
			InputPerMTok: 3.00, OutputPerMTok: 15.00,
			CacheCreationPerMTok: 0, CacheReadPerMTok: 0.30,
		},
	}), "UpsertModelPricing")

	tokenUsage := json.RawMessage(
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
	assert.InDelta(t, 1.51+4.80, usage.CostUSD, 1e-9,
		"session cost must sum the K2.6 and K3 eras")
	require.Len(t, usage.Breakdown, 2, "one breakdown entry per row")
	assert.InDelta(t, 1.51, usage.Breakdown[0].CostUSD, 1e-9,
		"pre-cutoff breakdown row at K2.6 rates")
	assert.InDelta(t, 4.80, usage.Breakdown[1].CostUSD, 1e-9,
		"post-cutoff breakdown row at K3 rates")
}
