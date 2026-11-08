//go:build chtest

package clickhouse_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/clickhouse"
	"go.kenn.io/agentsview/internal/clickhouse/chtest"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/storage"
)

// TestAnalyticsToolsCountsEveryCallInAMessage checks that every tool call
// counts when several calls share one message.
func TestAnalyticsToolsCountsEveryCallInAMessage(t *testing.T) {
	t.Parallel()
	local := dbtest.OpenTestDB(t)
	const sessionID = "multi-call-message"
	dbtest.SeedSession(t, local, sessionID, "multi-call", func(s *db.Session) {
		s.MessageCount, s.UserMessageCount = 2, 1
		s.StartedAt = dbtest.Ptr("2026-04-26T10:00:00Z")
		s.EndedAt = dbtest.Ptr("2026-04-26T10:00:05Z")
	})
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), sessionID, []db.Message{
		{
			SessionID: sessionID, Ordinal: 0, Role: "user", Content: "Read both files",
			ContentLength: len("Read both files"), Timestamp: "2026-04-26T10:00:00Z",
		},
		{
			SessionID: sessionID, Ordinal: 1, Role: "assistant", Content: "tool calls",
			ContentLength: len("tool calls"), Timestamp: "2026-04-26T10:00:01Z", HasToolUse: true,
			ToolCalls: []db.ToolCall{
				{ToolName: "Read", Category: "Read", ToolUseID: "read-1", InputJSON: `{"file_path":"a.go"}`},
				{ToolName: "Read", Category: "Read", ToolUseID: "read-2", InputJSON: `{"file_path":"b.go"}`},
				{ToolName: "Grep", Category: "Grep", ToolUseID: "grep-1", InputJSON: `{"pattern":"main"}`},
			},
		},
	}))

	dsn, database := chtest.FreshDatabase(t)
	target := clickhouse.Target{URL: dsn, Database: database}
	syncer, err := clickhouse.New(t.Context(), target, local, "multi-call-host", storage.PusherOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syncer.Close()) })
	_, err = syncer.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store, err := clickhouse.NewStore(t.Context(), target)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	filter := db.AnalyticsFilter{From: "2026-04-26", To: "2026-04-26"}
	got, err := store.GetAnalyticsTools(t.Context(), filter)
	require.NoError(t, err)
	calls := map[string]int{}
	for _, tool := range got.ByTool {
		calls[tool.ToolName] = tool.CallCount
	}
	assert.Equal(t, map[string]int{"Read": 2, "Grep": 1}, calls)
	assert.Equal(t, 3, got.TotalCalls)
	want, err := local.GetAnalyticsTools(t.Context(), filter)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
