//go:build chtest

package clickhouse

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

func TestStoreActivityReportAndRecentEdits(t *testing.T) {
	store, _, _ := newPushedStore(t)
	ctx := context.Background()
	fixedNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-01-10", Timezone: "UTC",
	}, fixedNow)
	require.NoError(t, err)

	report, err := store.GetActivityReport(ctx, db.AnalyticsFilter{
		Timezone:         "UTC",
		IncludeSubagents: true,
	}, q)
	require.NoError(t, err)
	assert.False(t, report.Partial)
	assert.Equal(t, 2, report.Totals.Sessions,
		"alpha root and its subagent child on 2026-01-10")

	edits, err := store.RecentEdits(ctx, db.RecentEditsParams{})
	require.NoError(t, err)
	require.Len(t, edits.Files, 1)
	assert.Equal(t, "alpha", edits.Files[0].Project)
	assert.Equal(t, "src/main.go", edits.Files[0].FilePath)
	assert.Equal(t, 1, edits.Files[0].EditCount)
	assert.Equal(t, fixtureAlphaID, edits.Files[0].LastSessionID)
}

func TestActivityReportIncludesSessionWithOnlyToolEventInWindow(t *testing.T) {
	store, syncer, local := newPushedStore(t)
	ctx := context.Background()
	const sessionID = "ch-tool-window"
	started := "2026-01-09T10:00:00.000Z"
	called := "2026-01-09T10:01:00.000Z"
	completed := "2026-01-10T12:00:00.000Z"
	sess := fixtureSession(sessionID, "tools", "run the sample", started, 2)
	sess.EndedAt = &started
	call := fixtureMessage(sessionID, 1, "assistant", "", called, db.ToolCall{
		ToolName:  "sample_tool",
		Category:  "Other",
		ToolUseID: "sample-call",
		ResultEvents: []db.ToolResultEvent{
			{
				ToolUseID: "sample-call", Source: "tool_execution",
				Status: "started", Timestamp: called, EventIndex: 0,
			},
			{
				ToolUseID: "sample-call", Source: "tool_execution",
				Status: "completed", Timestamp: completed, EventIndex: 1,
			},
		},
	})
	call.HasToolUse = true
	_, err := local.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{
		Session: sess,
		Messages: []db.Message{
			fixtureMessage(sessionID, 0, "user", "run the sample", started),
			call,
		},
		DataVersion:     1,
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	_, err = syncer.Push(ctx, false, nil)
	require.NoError(t, err)

	fixedNow := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "day", Date: "2026-01-10", Timezone: "UTC",
	}, fixedNow)
	require.NoError(t, err)
	report, err := store.GetActivityReport(ctx, db.AnalyticsFilter{Timezone: "UTC"}, q)
	require.NoError(t, err)
	ids := make([]string, 0, len(report.BySession))
	for _, row := range report.BySession {
		ids = append(ids, row.SessionID)
	}
	assert.Contains(t, ids, sessionID,
		"a session whose only in-window activity is a tool_execution event must appear")
}
