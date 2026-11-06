package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestUsageGrokImportsCSVWithoutSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dir)
	path := filepath.Join(t.TempDir(), "usage.csv")
	content := `Billing Item ID,Event Time,Product,Charge Category,Usage Type,Principal Type,Principal ID,Usage,Pricing Currency,Pricing Contracted,Model,User Email
item-a,2026-11-05T00:30:00Z,grok-build,usage,included,user,user-a,"{""Prompt text tokens"":""300000001"",""Completion text tokens"":""100000002"",""Cached prompt text tokens"":""400000003"",""cache-write-text-tokens"":""200000004""}",USD,0,,member@example.com
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	for range 2 {
		_, err := executeCommand(newUsageCommand(), "grok", path)
		require.NoError(t, err)
	}
	// A later malformed row must not partially import an otherwise valid file.
	invalid := strings.Replace(content, "item-a,", "item-b,", 1) +
		strings.Replace(strings.SplitAfterN(content, "\n", 2)[1], "2026-11-05T00:30:00Z", "invalid", 1)
	require.NoError(t, os.WriteFile(path, []byte(invalid), 0o600))
	_, err := executeCommand(newUsageCommand(), "grok", path)
	require.Error(t, err)
	database, err := db.OpenReadOnly(t.Context(), filepath.Join(dir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	events, err := database.GetCursorUsageEvents(t.Context(), 0)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, "grok", events[0].Agent)
	assert.Empty(t, events[0].Model)
	assert.Zero(t, events[0].Charged.Microdollars)
	result, err := database.GetDailyUsage(t.Context(), db.UsageFilter{
		From: "2026-11-04", To: "2026-11-04", Timezone: "America/New_York", Agent: "grok",
	})
	require.NoError(t, err)
	require.Len(t, result.Daily, 1)
	assert.Equal(t, 300_000_001, result.Daily[0].InputTokens)
	assert.Equal(t, 100_000_002, result.Daily[0].OutputTokens)
	assert.Equal(t, 400_000_003, result.Daily[0].CacheReadTokens)
	assert.Equal(t, 200_000_004, result.Daily[0].CacheCreationTokens)
	assert.Zero(t, result.SessionCounts.Total)
}
