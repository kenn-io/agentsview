package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageVelocityIndexWidensOnPopulatedArchive(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "pairs", "project", func(s *Session) {
		s.Agent = "claude"
		s.StartedAt = new("2026-06-16T10:00:00Z")
		s.EndedAt = new("2026-06-16T10:05:00Z")
		s.MessageCount = 6
	})
	insertMessages(t, d,
		Message{SessionID: "pairs", Ordinal: 0, Role: "user", Content: "a", Timestamp: "2026-06-16T10:00:00Z"},
		Message{SessionID: "pairs", Ordinal: 1, Role: "assistant", Content: "b", Timestamp: "2026-06-16T10:01:00Z", Model: "model-a"},
		Message{SessionID: "pairs", Ordinal: 2, Role: "user", Content: "c", Timestamp: "2026-06-16T10:02:00Z", SourceSubtype: "tool_result"},
		Message{SessionID: "pairs", Ordinal: 3, Role: "user", Content: "d", Timestamp: "2026-06-16T10:03:00Z", IsSystem: true},
		Message{SessionID: "pairs", Ordinal: 4, Role: "assistant", Content: "e", Timestamp: "2026-06-16T10:04:00Z", Model: "model-b"},
		Message{SessionID: "pairs", Ordinal: 5, Role: "user", Content: "f", Timestamp: "2026-06-16T10:05:00Z"},
	)
	_, err := d.getWriter().Exec(t.Context(), `
		DROP INDEX idx_messages_velocity;
		CREATE INDEX idx_messages_velocity
			ON messages(session_id, ordinal, role, timestamp, content_length);
		ANALYZE`)
	require.NoError(t, err)
	q := dayQuery(t, "2026-06-16", "UTC")
	f := AnalyticsFilter{Timezone: "UTC"}
	before, err := d.GetActivityReport(t.Context(), f, q)
	require.NoError(t, err)

	output := captureLog(t)
	path := d.Path()
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	assert.Contains(t, output.String(), "rebuilding stale SQLite index idx_messages_velocity")
	requireIndexColumns(t, d, "idx_messages_velocity", messageVelocityIndexColumns)
	assert.Contains(t, queryPlanOf(t, d, activityReportMessagesSQL, `["pairs"]`),
		"USING COVERING INDEX idx_messages_velocity")

	after, err := d.GetActivityReport(t.Context(), f, q)
	require.NoError(t, err)
	before.ReportID, after.ReportID = "", ""
	assert.Equal(t, before, after)
	var users, assistants int
	for _, bucket := range after.Buckets {
		users += bucket.UserMessages
		assistants += bucket.AssistantMessages
	}
	assert.Equal(t, 2, users)
	assert.Equal(t, 2, assistants)
	assert.Equal(t, 2, after.Totals.DistinctModels)
}
