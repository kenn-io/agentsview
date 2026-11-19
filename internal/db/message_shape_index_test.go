package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageSessionRoleIndexWidensOnPopulatedArchive(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "shape", "project", func(s *Session) {
		s.StartedAt = new("2024-06-01T09:00:00Z")
		s.EndedAt = new("2024-06-01T10:00:00Z")
		s.MessageCount = 6
	})
	for i := range 6 {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		insertMessages(t, d, Message{
			SessionID: "shape", Ordinal: i, Role: role,
			Content: fmt.Sprintf("msg %d", i), HasToolUse: role == "assistant",
		})
	}
	_, err := d.getWriter().Exec(t.Context(), `
		DROP INDEX idx_messages_session_role;
		CREATE INDEX idx_messages_session_role ON messages(session_id, role);
		ANALYZE`)
	require.NoError(t, err)
	before, err := d.GetAnalyticsSessionShape(t.Context(), baseFilter())
	require.NoError(t, err)

	output := captureLog(t)
	path := d.Path()
	require.NoError(t, d.Close())
	d, err = OpenIsolated(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	assert.Contains(t, output.String(), "rebuilding stale SQLite index idx_messages_session_role")
	requireIndexColumns(t, d, "idx_messages_session_role", messageSessionRoleIndexColumns)
	var stat string
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		`SELECT stat FROM sqlite_stat1 WHERE idx = 'idx_messages_session_role'`).Scan(&stat))
	assert.NotEmpty(t, stat)
	assert.Contains(t, queryPlanOf(t, d, autonomyChunkSQL("(?)"), "shape"),
		"USING COVERING INDEX idx_messages_session_role")

	after, err := d.GetAnalyticsSessionShape(t.Context(), baseFilter())
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Equal(t, 1, after.Count)
}
