//go:build chtest

package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// Activity buckets count only visible, timestamped messages: system rows,
// injected user prompts, and rows without a timestamp stay out of the
// buckets but count toward the total, and tool-result user rows add no user
// turn.
func TestGetSessionActivityCountsVisibleMessages(t *testing.T) {
	ctx := t.Context()
	const sessionID = "activity-buckets"
	local, target := seedFixture(t)
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
	_, err := local.WriteSessionBatchAtomic(ctx, []db.SessionBatchWrite{{
		Session: fixtureSession(sessionID, "alpha", "start", "2026-01-01T10:00:00Z", 8),
		Messages: []db.Message{
			msg(0, "user", "start", "2026-01-01T10:00:00Z"),
			withTool,
			toolResult,
			msg(3, "user", "  <task-notification> done", "2026-01-01T10:01:00Z"),
			msg(4, "system", "compacted", "2026-01-01T10:20:00Z"),
			msg(5, "assistant", "no clock", ""),
			msg(6, "user", "next", "2026-01-01T10:02:00Z"),
			msg(7, "assistant", "done", "2026-01-01T10:02:10Z"),
		},
	}})
	require.NoError(t, err)
	syncer := newTestSync(t, local, target, storage.PusherOptions{})
	result, err := syncer.Push(ctx, false, nil)
	require.NoError(t, err)
	require.Zero(t, result.Errors)
	store := newTestStore(t, syncer.conn, syncer.target)

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
}
