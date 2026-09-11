package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestDirectArchiveReportsResolveMachineAliases(t *testing.T) {
	database := newTestDB(t)
	started, ended := "2026-06-15T10:00:00Z", "2026-06-15T10:05:00Z"
	_, err := database.WriteSessionBatchAtomic([]db.SessionBatchWrite{{
		Session: db.Session{
			ID: "session-a", Project: "project-a", Machine: "installation-a", Agent: "claude",
			StartedAt: &started, EndedAt: &ended, CreatedAt: started,
			MessageCount: 2, UserMessageCount: 1, RelationshipType: "root",
		},
		Messages: []db.Message{
			{SessionID: "session-a", Ordinal: 0, Role: "user", Content: "question", Timestamp: started},
			{SessionID: "session-a", Ordinal: 1, Role: "assistant", Content: "answer", Timestamp: ended,
				Model: "claude-sonnet-4-20250514", OutputTokens: 500, HasOutputTokens: true,
				TokenUsage: []byte(`{"input_tokens":100,"output_tokens":500}`)},
		},
		ReplaceMessages: true,
	}})
	require.NoError(t, err)
	require.NoError(t, database.SetSyncState(db.MachineAliasKeyPrefix+"old-host", "installation-a"))
	backend := localArchiveQueryBackend{database: database, offline: true, skipFreshData: true}
	usage, err := backend.DailyUsage(t.Context(), dailyUsageQuery{
		Filter: db.UsageFilter{From: "2026-06-15", To: "2026-06-15", Machine: "old-host"},
	})
	require.NoError(t, err)
	assert.Equal(t, 500, usage.Totals.OutputTokens)
	activity, err := backend.ActivityReport(t.Context(), ActivityReportConfig{
		Preset: "day", Date: "2026-06-15", Timezone: "UTC", Machine: "old-host", Offline: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 500, activity.Totals.OutputTokens)
	pages, err := collectExportSessionPages(t.Context(), database, exportSessionsConfig{
		Machine: "old-host", Limit: 10, Format: "json", IncludeOneShot: true,
	})
	require.NoError(t, err)
	require.Len(t, pages, 1)
	require.Len(t, pages[0].Rows, 1)
	assert.Equal(t, "session-a", pages[0].Rows[0].ID)
}
