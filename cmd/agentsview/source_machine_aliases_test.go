package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func TestDirectArchiveReportsResolveMachineAliases(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	require.NoError(database.SetSyncState(db.MachineAliasKeyPrefix+"old-host", "installation-a"))
	backend := localArchiveQueryBackend{database: database, offline: true, skipFreshData: true}
	usage, err := backend.DailyUsage(t.Context(), dailyUsageQuery{
		Filter: db.UsageFilter{From: "2026-06-15", To: "2026-06-15", Machine: "old-host"},
	})
	require.NoError(err)
	assert.Equal(500, usage.Totals.OutputTokens)
	activity, err := backend.ActivityReport(t.Context(), ActivityReportConfig{
		Preset: "day", Date: "2026-06-15", Timezone: "UTC", Machine: "old-host", Offline: true,
	})
	require.NoError(err)
	assert.Equal(500, activity.Totals.OutputTokens)
	pages, err := collectExportSessionPages(t.Context(), database, exportSessionsConfig{
		Machine: "old-host", Limit: 10, Format: "json", IncludeOneShot: true,
	})
	require.NoError(err)
	require.Len(pages, 1)
	require.Len(pages[0].Rows, 1)
	assert.Equal("session-a", pages[0].Rows[0].ID)
}
