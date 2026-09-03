package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"go.kenn.io/agentsview/internal/db/bunmodel"
)

func TestBunStoreMessageCompositeReadsPublishOnlyAcceptedReplayAttempt(
	t *testing.T,
) {
	first := testDB(t)
	second := testDB(t)
	const sessionID = "replayed-messages"
	seed := func(database *DB, label string) {
		t.Helper()
		require.NoError(t, database.UpsertSession(Session{
			ID: sessionID, Project: "replaying-reads", Machine: "host", Agent: "codex",
			MessageCount: 1, UserMessageCount: 1,
		}))
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: sessionID, Ordinal: 5, Role: "assistant",
			Content: label + " message", ContentLength: len(label) + len(" message"),
			HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: label + " tool", Category: "Read", ToolUseID: label + "-call",
				ResultEvents: []ToolResultEvent{{
					ToolUseID: label + "-call", Source: "tool_execution",
					Status: label + " status", Content: label + " result",
				}},
			}},
		}}))
	}
	seed(first, "rejected")
	seed(second, "accepted")

	store := NewBunStore(&replayingReadBackend{
		first: first.bunReader, second: second.bunReader,
	})
	anchor := 5
	reads := []struct {
		name string
		read func(context.Context) ([]Message, error)
	}{
		{
			name: "messages",
			read: func(ctx context.Context) ([]Message, error) {
				return store.GetMessages(ctx, sessionID, 0, 10, true)
			},
		},
		{
			name: "message window",
			read: func(ctx context.Context) ([]Message, error) {
				return store.GetMessagesWindow(ctx, sessionID, MessageWindow{
					Around: &anchor,
				})
			},
		},
		{
			name: "all messages",
			read: func(ctx context.Context) ([]Message, error) {
				return store.GetAllMessages(ctx, sessionID)
			},
		},
	}
	for _, test := range reads {
		t.Run(test.name, func(t *testing.T) {
			messages, err := test.read(t.Context())
			require.NoError(t, err)
			require.Len(t, messages, 1)
			assert.Equal(t, "accepted message", messages[0].Content)
			require.Len(t, messages[0].ToolCalls, 1)
			assert.Equal(t, "accepted tool", messages[0].ToolCalls[0].ToolName)
			require.NotEmpty(t, messages[0].ToolCalls[0].ResultEvents)
			for _, event := range messages[0].ToolCalls[0].ResultEvents {
				assert.Equal(t, "accepted status", event.Status)
			}
		})
	}
}

func TestBunStoreSessionTimingPublishesOnlyAcceptedReplayAttempt(t *testing.T) {
	first := testDB(t)
	second := testDB(t)
	const sessionID = "replayed-timing"
	seed := func(database *DB, label, started, ended, messageAt string) {
		t.Helper()
		require.NoError(t, database.UpsertSession(Session{
			ID: sessionID, Project: "replaying-reads", Machine: "host", Agent: "codex",
			StartedAt: &started, EndedAt: &ended, MessageCount: 1,
		}))
		require.NoError(t, database.InsertMessages([]Message{{
			SessionID: sessionID, Ordinal: 0, Role: "assistant",
			Content: label, ContentLength: len(label), Timestamp: messageAt,
			HasToolUse: true,
			ToolCalls: []ToolCall{{
				ToolName: label + " tool", Category: "Read", ToolUseID: label + "-call",
			}},
		}}))
	}
	seed(first, "rejected", "2026-08-01T10:00:00Z", "2026-08-01T10:10:00Z",
		"2026-08-01T10:01:00Z")
	seed(second, "accepted", "2026-08-01T12:00:00Z", "2026-08-01T12:03:00Z",
		"2026-08-01T12:01:00Z")

	timing, err := NewBunStore(&replayingReadBackend{
		first: first.bunReader, second: second.bunReader,
	}).GetSessionTiming(t.Context(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, timing)
	require.Len(t, timing.Turns, 1)
	require.Len(t, timing.Turns[0].Calls, 1)
	assert.Equal(t, "accepted tool", timing.Turns[0].Calls[0].ToolName)
	require.NotNil(t, timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(120_000), *timing.Turns[0].Calls[0].DurationMs)
}

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

	hook := new(countingQueryHook)
	backend := &sessionContractBackend{store: store.WithQueryHook(hook)}
	common := NewBunStore(backend)
	timing, err := common.GetSessionTiming(t.Context(), parentID)
	require.NoError(t, err)
	require.NotNil(t, timing)
	require.Len(t, timing.Turns, 1)
	require.Len(t, timing.Turns[0].Calls, 1)
	require.NotNil(t, timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(120000), *timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, 1, backend.viewCalls)

	var checkedMessages, checkedCalls, checkedEvents bool
	for _, query := range hook.queries {
		lower := strings.ToLower(query)
		switch {
		case strings.Contains(lower, `from "messages"`):
			checkedMessages = true
			assert.NotContains(t, lower, `"content"`)
			assert.NotContains(t, lower, `"thinking_text"`)
			assert.NotContains(t, lower, `"token_usage"`)
		case strings.Contains(lower, `from "tool_calls"`):
			checkedCalls = true
			assert.NotContains(t, lower, `"result_content"`)
		case strings.Contains(lower, `from "tool_result_events"`):
			checkedEvents = true
			assert.NotContains(t, lower, `"content"`)
		}
	}
	assert.True(t, checkedMessages)
	assert.True(t, checkedCalls)
	assert.True(t, checkedEvents)
}

func TestBunStoreGetAllMessagesBatchesToolHydration(t *testing.T) {
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
	_, err = store.NewInsert().Model(&bunmodel.Session{
		ID: "large-tool-hydration", Project: "alpha", Machine: "host", Agent: "codex",
		CreatedAt:       bunmodel.NewTimestamp(time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)),
		SourceArchiveID: "archive", SourceDatabaseGeneration: "generation",
	}).Exec(t.Context())
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `
		WITH RECURSIVE ordinals(value) AS (
			SELECT 0 UNION ALL SELECT value + 1 FROM ordinals WHERE value < 1000
		)
		INSERT INTO messages (
			session_id, ordinal, role, content, token_usage
		)
		SELECT 'large-tool-hydration', value, 'assistant', '', '{}'
		FROM ordinals`)
	require.NoError(t, err)

	hook := new(countingQueryHook)
	common := NewBunStore(&sessionContractBackend{store: store.WithQueryHook(hook)})
	messages, err := common.GetAllMessages(t.Context(), "large-tool-hydration")
	require.NoError(t, err)
	require.Len(t, messages, 1001)
	assert.Equal(t, 7, hook.selects,
		"one message query plus three bounded call and event batches")
}

func TestBunStoreGetSessionTimingFallsBackToEventsForMissingSubagent(
	t *testing.T,
) {
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
	parse := func(value string) *bunmodel.Timestamp {
		parsed, parseErr := bunmodel.ParseTimestamp(value)
		require.NoError(t, parseErr)
		return &parsed
	}
	_, err = store.NewInsert().Model(&bunmodel.Session{
		ID: "missing-subagent-parent", Project: "alpha", Machine: "host", Agent: "codex",
		StartedAt:       parse("2026-08-02T10:00:00Z"),
		EndedAt:         parse("2026-08-02T10:02:00Z"),
		CreatedAt:       *parse("2026-08-02T10:00:00Z"),
		SourceArchiveID: "archive", SourceDatabaseGeneration: "generation",
	}).Exec(t.Context())
	require.NoError(t, err)
	messageID := int64(1)
	_, err = store.NewInsert().Model(&bunmodel.Message{
		ID: &messageID, SessionID: "missing-subagent-parent", Ordinal: 0,
		Role: "assistant", Content: "delegate", ContentLength: 8,
		Timestamp: parse("2026-08-02T10:01:00Z"), HasToolUse: true,
		TokenUsage: json.RawMessage(`{}`),
	}).Exec(t.Context())
	require.NoError(t, err)
	missingSubagent := "missing-child"
	_, err = store.NewInsert().Model(&bunmodel.ToolCall{
		SessionID: "missing-subagent-parent", MessageOrdinal: 0,
		ToolName: "Task", Category: "Task", ToolUseID: "call-missing",
		SubagentSessionID: &missingSubagent,
	}).Exec(t.Context())
	require.NoError(t, err)
	for index, event := range []struct {
		status, at string
	}{
		{status: "started", at: "2026-08-02T10:01:05Z"},
		{status: "completed", at: "2026-08-02T10:01:20Z"},
	} {
		_, err = store.NewInsert().Model(&bunmodel.ToolResultEvent{
			SessionID: "missing-subagent-parent", ToolCallMessageOrdinal: 0,
			CallIndex: 0, Source: "tool_execution", Status: event.status,
			Timestamp: parse(event.at), EventIndex: index,
		}).Exec(t.Context())
		require.NoError(t, err)
	}

	timing, err := NewBunStore(&sessionContractBackend{store: store}).
		GetSessionTiming(t.Context(), "missing-subagent-parent")
	require.NoError(t, err)
	require.NotNil(t, timing)
	require.Len(t, timing.Turns, 1)
	require.Len(t, timing.Turns[0].Calls, 1)
	require.NotNil(t, timing.Turns[0].Calls[0].DurationMs)
	assert.Equal(t, int64(15000), *timing.Turns[0].Calls[0].DurationMs)
	require.NotNil(t, timing.Turns[0].DurationMs)
	assert.Equal(t, int64(20000), *timing.Turns[0].DurationMs)
}
