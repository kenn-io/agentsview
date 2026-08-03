package db

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func TestBunStoreGetSessionTimingUsesOneGuardForSubagentHydration(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	store := bun.NewDB(raw, sqlitedialect.New())
	require.NoError(t, CreateCommonSchema(t.Context(), store))
	_, err = store.NewInsert().Model(&bunmodel.SourceArchive{
		SourceArchiveID: "archive", SourceArchiveSalt: "salt",
	}).Exec(t.Context())
	require.NoError(t, err)

	timestamp := func(hour, minute int) *bunmodel.Timestamp {
		value := bunmodel.NewTimestamp(
			time.Date(2026, 8, 2, hour, minute, 0, 0, time.UTC),
		)
		return &value
	}
	parentID := "timing-parent"
	rows := []bunmodel.Session{
		{
			ID: parentID, Project: "alpha", Machine: "host", Agent: "codex",
			StartedAt: timestamp(10, 0), EndedAt: timestamp(10, 10),
			CreatedAt: *timestamp(10, 0), SourceArchiveID: "archive",
			SourceDatabaseGeneration: "generation",
		},
		{
			ID: "timing-child", Project: "alpha", Machine: "host", Agent: "codex",
			StartedAt: timestamp(10, 2), EndedAt: timestamp(10, 4),
			CreatedAt: *timestamp(10, 2), ParentSessionID: &parentID,
			RelationshipType: "subagent", SourceArchiveID: "archive",
			SourceDatabaseGeneration: "generation",
		},
	}
	for index := range rows {
		_, err = store.NewInsert().Model(&rows[index]).Exec(t.Context())
		require.NoError(t, err)
	}
	messageID := int64(1)
	_, err = store.NewInsert().Model(&bunmodel.Message{
		ID: &messageID, SessionID: parentID, Ordinal: 0, Role: "assistant",
		Content: "delegate", ContentLength: 8, Timestamp: timestamp(10, 1),
		HasToolUse: true, TokenUsage: json.RawMessage(`{}`),
	}).Exec(t.Context())
	require.NoError(t, err)
	toolID := int64(1)
	subagentID := "timing-child"
	_, err = store.NewInsert().Model(&bunmodel.ToolCall{
		ID: &toolID, MessageID: &messageID, SessionID: parentID,
		MessageOrdinal: 0, ToolName: "Task", Category: "Task",
		ToolUseID: "call-child", SubagentSessionID: &subagentID,
	}).Exec(t.Context())
	require.NoError(t, err)

	backend := &sessionContractBackend{store: store}
	common := NewBunStore(backend)
	timing, err := common.GetSessionTiming(t.Context(), parentID)
	require.NoError(t, err)
	require.NotNil(t, timing)
	require.Len(t, timing.Turns, 1)
	require.Len(t, timing.Turns[0].Calls, 1)
	require.NotNil(t, timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(120000), *timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, 1, backend.viewCalls)
}
