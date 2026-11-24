package sync_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/sync"
)

func TestAntigravityModelNormalizationUpgradeReparsesInPlace(t *testing.T) {
	t.Parallel()
	scope := db.ScopedDataVersionProviders(185, 186)
	require.ElementsMatch(t, []parser.AgentType{
		parser.AgentAntigravity,
		parser.AgentAntigravityCLI,
	}, scope)

	const (
		id       = "cccccccc-dddd-eeee-ffff-000000000000"
		previous = 185
	)
	corpusDB := filepath.Join(
		"..", "parser", "testdata", "data-version-corpus",
		"antigravity-cli", "conversations", id+".db",
	)
	corpusBytes, err := os.ReadFile(corpusDB)
	require.NoError(t, err)

	for _, agent := range []parser.AgentType{
		parser.AgentAntigravity,
		parser.AgentAntigravityCLI,
	} {
		t.Run(string(agent), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			source := filepath.Join(root, "conversations", id+".db")
			require.NoError(t, os.MkdirAll(filepath.Dir(source), 0o755))
			require.NoError(t, os.WriteFile(source, corpusBytes, 0o600))

			sessionID := string(agent) + ":" + id
			archivePath := filepath.Join(t.TempDir(), "archive.db")
			database, err := db.OpenIsolated(t.Context(), archivePath)
			require.NoError(t, err)
			cfg := sync.EngineConfig{
				AgentDirs: map[parser.AgentType][]string{agent: {root}},
				Machine:   "local",
			}
			engine := sync.NewEngine(t.Context(), database, cfg)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			engine.Close()
			require.NoError(t, database.Close())

			// Reproduce version 185 stored rows (display names with parenthesized
			// reasoning levels and empty reasoning_effort) without changing the source.
			archive, err := sql.Open("sqlite3", archivePath)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(),
				`UPDATE messages SET model = 'Gemini 4 Neon (High)', reasoning_effort = '' WHERE session_id = ? AND ordinal = 1`,
				sessionID)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(),
				`UPDATE messages SET model = 'Gemini 4 Neon (Low)', reasoning_effort = '' WHERE session_id = ? AND ordinal = 3`,
				sessionID)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(),
				`UPDATE usage_events SET model = CASE WHEN input_tokens = 2400 THEN 'Gemini 4 Neon (High)' ELSE 'Gemini 4 Neon (Low)' END WHERE session_id = ?`,
				sessionID)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(),
				`UPDATE sessions SET data_version = ? WHERE id = ?`,
				previous, sessionID)
			require.NoError(t, err)
			_, err = archive.ExecContext(t.Context(), `PRAGMA user_version = 185`)
			require.NoError(t, err)
			require.NoError(t, archive.Close())

			_, fullRebuild, err := db.ArchiveResyncState(t.Context(), archivePath)
			require.NoError(t, err)
			assert.False(t, fullRebuild, "version 186 must reparse Antigravity in place")

			reopened, err := db.OpenIsolated(t.Context(), archivePath)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
			upgraded := sync.NewEngine(t.Context(), reopened, cfg)
			t.Cleanup(upgraded.Close)
			require.Equal(t, 1, upgraded.SyncAll(t.Context(), nil).Synced)

			assert.Equal(t, source, reopened.GetSessionFilePath(t.Context(), sessionID))
			msgs := fetchMessages(t, reopened, sessionID)
			require.Len(t, msgs, 4)
			assert.Equal(t, "gemini-4-neon", msgs[1].Model)
			assert.Equal(t, "high", msgs[1].ReasoningEffort)
			assert.Equal(t, "gemini-4-neon", msgs[3].Model)
			assert.Equal(t, "low", msgs[3].ReasoningEffort)

			daily, err := reopened.GetDailyUsage(t.Context(), db.UsageFilter{Timezone: "UTC"})
			require.NoError(t, err)
			require.Len(t, daily.Daily, 1)
			assert.Equal(t, []string{"gemini-4-neon"}, daily.Daily[0].ModelsUsed)
			require.Len(t, daily.Daily[0].ModelBreakdowns, 1)
			assert.Equal(t, "gemini-4-neon", daily.Daily[0].ModelBreakdowns[0].ModelName)
			assert.Equal(t, 3600, daily.Daily[0].ModelBreakdowns[0].InputTokens)
			assert.Equal(t, 270, daily.Daily[0].ModelBreakdowns[0].OutputTokens)
			assert.Equal(t, 200, daily.Daily[0].ModelBreakdowns[0].CacheReadTokens)
		})
	}
}
