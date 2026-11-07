//go:build !(windows && arm64)

package duckdb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// Activity buckets count only visible, timestamped messages: system rows,
// injected user prompts, and rows without a timestamp stay out of the
// buckets but count toward the total, and tool-result user rows add no user
// turn. A malformed token_usage blob and tool-call input do not affect it.
func TestDuckGetSessionActivityCountsVisibleMessages(t *testing.T) {
	ctx := t.Context()
	const sessionID = "sActivity"
	store := newDuckWindowStore(t, func(local *db.DB) {
		require.NoError(t, local.UpsertSession(ctx, db.Session{
			ID: sessionID, Project: "proj", Machine: "local", Agent: "claude",
			MessageCount: 8, CreatedAt: "2026-01-01T10:00:00Z",
		}))
		msg := func(ordinal int, role, content, ts string) db.Message {
			return db.Message{
				SessionID: sessionID, Ordinal: ordinal, Role: role,
				Content: content, ContentLength: len(content), Timestamp: ts,
				IsSystem: role == "system",
			}
		}
		withTool := msg(1, "assistant", "editing", "2026-01-01T10:00:30Z")
		withTool.HasToolUse = true
		withTool.ToolCalls = []db.ToolCall{{
			ToolName: "Edit", Category: "Edit", ToolUseID: "toolu_1",
			InputJSON: `{"file_path":"/w/a.go","old_string":"x","new_string":"y"}`,
		}}
		toolResult := msg(2, "user", "edited", "2026-01-01T10:00:40Z")
		toolResult.SourceSubtype = "tool_result"
		require.NoError(t, local.InsertMessages(ctx, []db.Message{
			msg(0, "user", "start", "2026-01-01T10:00:00Z"),
			withTool,
			toolResult,
			msg(3, "user", "  <task-notification> done", "2026-01-01T10:01:00Z"),
			msg(4, "system", "compacted", "2026-01-01T10:20:00Z"),
			msg(5, "assistant", "no clock", ""),
			msg(6, "user", "next", "2026-01-01T10:02:00Z"),
			msg(7, "assistant", "done", "2026-01-01T10:02:10Z"),
		}))
	})
	_, err := store.DB().ExecContext(ctx,
		`UPDATE messages SET token_usage = ? WHERE session_id = ? AND ordinal = ?`,
		`{"input_tokens":4,"output_tokens":`, sessionID, 1)
	require.NoError(t, err)

	activity, err := store.GetSessionActivity(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, &db.SessionActivityResponse{
		IntervalSeconds: 60,
		TotalMessages:   8,
		Buckets: []db.SessionActivityBucket{
			{
				StartTime: "2026-01-01T10:00:00Z", EndTime: "2026-01-01T10:01:00Z",
				UserCount: 1, AssistantCount: 1, FirstOrdinal: new(0),
			},
			{StartTime: "2026-01-01T10:01:00Z", EndTime: "2026-01-01T10:02:00Z"},
			{
				StartTime: "2026-01-01T10:02:00Z", EndTime: "2026-01-01T10:03:00Z",
				UserCount: 1, AssistantCount: 1, FirstOrdinal: new(6),
			},
		},
	}, activity)

	empty, err := store.GetSessionActivity(ctx, "missing")
	require.NoError(t, err)
	assert.Equal(t, &db.SessionActivityResponse{Buckets: []db.SessionActivityBucket{}}, empty)
}
