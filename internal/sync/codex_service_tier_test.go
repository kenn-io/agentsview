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

func TestCodexServiceTierSessionCosts(t *testing.T) {
	for _, tc := range []struct {
		tier                  string
		input, cached, output int
		cost                  string
	}{
		{"priority", 20360, 6912, 808, "0.071334"},
		{"ultrafast", 20360, 6912, 808, "0.035667"},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			sessions := filepath.Join(t.TempDir(), "sessions")
			require.NoError(t, os.MkdirAll(sessions, 0o755))
			env := setupSingleAgentTestEnvWithDirs(t, parser.AgentCodex, []string{sessions})
			defer env.engine.Close()
			require.NoError(t, env.db.UpsertModelPricing([]db.ModelPricing{{
				ModelPattern:     "gpt-6.1-sol",
				InputPerMTok:     money.MustParseDollars("2"),
				CacheReadPerMTok: money.MustParseDollars("0.10"),
				OutputPerMTok:    money.MustParseDollars("10"),
			}, {
				ModelPattern:     "gpt-6.1-sol@priority",
				InputPerMTok:     money.MustParseDollars("4"),
				CacheReadPerMTok: money.MustParseDollars("0.2"),
				OutputPerMTok:    money.MustParseDollars("20"),
			}}))
			const uuid = "019f0000-0000-7000-8000-00000000c001"
			path := env.writeCodexSession(t, filepath.Join("2026", "10", "26"), "rollout-2026-10-26T10-00-00-"+uuid+".jsonl", testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "user", "2026-10-26T10:00:00Z"),
				testjsonl.CodexTurnContextJSON("gpt-6.1-sol", "2026-10-26T10:00:01Z"),
				`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"model_provider_id":"openai","service_tier":"`+tc.tier+`"}}}`,
				testjsonl.CodexMsgJSON("user", "hello", "2026-10-26T10:00:01Z"),
				testjsonl.CodexMsgJSON("assistant", "answer", "2026-10-26T10:00:05Z"),
				testjsonl.CodexTokenCountJSON("2026-10-26T10:00:05Z", tc.input, tc.output, tc.cached),
			))
			require.NoError(t, env.engine.SyncPathsContext(t.Context(), []string{path}))
			usage, err := env.db.GetSessionUsage(t.Context(), "codex:"+uuid, true)
			require.NoError(t, err)
			require.NotNil(t, usage)
			assert.True(t, usage.HasCost)
			assert.Equal(t, money.MustParseDollars(tc.cost), usage.Cost)
		})
	}
}
