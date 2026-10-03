package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func seedCodexCacheWritePricing(t *testing.T, database *db.DB) {
	t.Helper()
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern:         "gpt-5.6-luna",
		InputPerMTok:         money.MustParseDollars("0.20"),
		OutputPerMTok:        money.MustParseDollars("1.20"),
		CacheCreationPerMTok: money.MustParseDollars("0.25"),
		CacheReadPerMTok:     money.MustParseDollars("0.02"),
		Bands: []db.PricingBand{{
			AboveInputTokens:     272000,
			InputPerMTok:         money.MustParseDollars("0.40"),
			OutputPerMTok:        money.MustParseDollars("1.80"),
			CacheCreationPerMTok: money.MustParseDollars("0.50"),
			CacheReadPerMTok:     money.MustParseDollars("0.04"),
		}},
	}}))
}

func codexCacheWriteRollout(uuid string, input, output, cached, cacheWrite int) string {
	return testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "user", "2026-09-15T10:00:00Z"),
		testjsonl.CodexTurnContextJSON("gpt-5.6-luna", "2026-09-15T10:00:01Z"),
		testjsonl.CodexMsgJSON("user", "hello", "2026-09-15T10:00:01Z"),
		testjsonl.CodexMsgJSON("assistant", "hi there", "2026-09-15T10:00:05Z"),
		testjsonl.CodexTokenCountWithCacheWriteJSON("2026-09-15T10:00:05Z", input, output, cached, cacheWrite),
	)
}

func TestCodexCacheWriteTokensPriceAtCacheWriteRate(t *testing.T) {
	sessions := filepath.Join(t.TempDir(), "sessions")
	require.NoError(t, os.MkdirAll(sessions, 0o755))
	env := setupSingleAgentTestEnvWithDirs(t, parser.AgentCodex, []string{sessions})
	seedCodexCacheWritePricing(t, env.db)

	syncRollout := func(t *testing.T, uuid string, input, output, cached, cacheWrite int) *db.SessionUsage {
		t.Helper()
		path := env.writeCodexSession(
			t, filepath.Join("2026", "09", "15"),
			"rollout-2026-09-15T10-00-00-"+uuid+".jsonl",
			codexCacheWriteRollout(uuid, input, output, cached, cacheWrite),
		)
		require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{path}))
		usage, err := env.db.GetSessionUsage(t.Context(), "codex:"+uuid, true)
		require.NoError(t, err)
		require.NotNil(t, usage)
		require.True(t, usage.HasCost)
		return usage
	}

	t.Run("split", func(t *testing.T) {
		usage := syncRollout(t, "019f0000-0000-7000-8000-00000000c001", 100000, 10000, 40000, 60000)
		// 60,000 x 0.25 + 40,000 x 0.02 + 10,000 x 1.20 per MTok.
		assert.Equal(t, money.MustParseDollars("0.0278"), usage.Cost)
		assert.Equal(t, 100000, usage.PeakContextTokens)
		require.Len(t, usage.Breakdown, 1)
		row := usage.Breakdown[0]
		assert.Equal(t, 0, row.InputTokens)
		assert.Equal(t, 60000, row.CacheCreationInputTokens)
		assert.Equal(t, 40000, row.CacheReadInputTokens)
		assert.Equal(t, 10000, row.OutputTokens)

		daily, err := env.db.GetDailyUsage(t.Context(), db.UsageFilter{
			From: "2026-09-15", To: "2026-09-15", Agent: "codex",
		})
		require.NoError(t, err)
		assert.Equal(t, money.MustParseDollars("0.0278"), daily.Totals.TotalCost)
	})

	t.Run("band", func(t *testing.T) {
		usage := syncRollout(t, "019f0000-0000-7000-8000-00000000c002", 300000, 0, 100000, 150000)
		// Above 272k: 50,000 x 0.40 + 150,000 x 0.50 + 100,000 x 0.04 per MTok.
		assert.Equal(t, money.MustParseDollars("0.099"), usage.Cost)
		assert.Equal(t, 300000, usage.PeakContextTokens)
	})
}
