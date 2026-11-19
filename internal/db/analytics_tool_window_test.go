package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tool and skill analytics keep a session when it started in the window or
// holds a message that is in the window or has no readable timestamp.
func TestToolSessionWindowMatchesMessageTimestamps(t *testing.T) {
	d := testDB(t)
	for _, s := range []struct{ id, startedAt string }{
		{"started-in", "2024-06-10T09:00:00Z"},
		{"message-in", "2024-05-01T09:00:00Z"},
		{"message-null", "2024-05-01T09:00:00Z"},
		{"message-empty", "2024-05-01T09:00:00Z"},
		{"message-unreadable", "2024-05-01T09:00:00Z"},
		{"message-out", "2024-05-01T09:00:00Z"},
	} {
		_, err := d.getWriter().Exec(t.Context(),
			`INSERT INTO sessions(id, project, started_at, message_count) VALUES (?, 'p', ?, 2)`,
			s.id, s.startedAt)
		require.NoError(t, err)
	}
	for _, m := range []struct {
		sessionID string
		timestamp any
	}{
		{"started-in", "2024-05-01T09:00:00Z"},
		{"message-in", "2024-05-01T09:00:00Z"},
		{"message-in", "2024-06-15T09:00:00Z"},
		{"message-null", nil},
		{"message-empty", ""},
		{"message-unreadable", "not-a-timestamp"},
		{"message-out", "2024-05-01T09:00:00Z"},
		{"message-out", "2024-07-15T09:00:00Z"},
	} {
		_, err := d.getWriter().Exec(t.Context(),
			`INSERT INTO messages(session_id, ordinal, role, content, timestamp)
			 VALUES (?, (SELECT COUNT(*) FROM messages WHERE session_id = ?), 'assistant', 'x', ?)`,
			m.sessionID, m.sessionID, m.timestamp)
		require.NoError(t, err)
	}

	f := AnalyticsFilter{From: "2024-06-01", To: "2024-06-30", Timezone: "UTC"}
	pred, args := f.toolSessionWindowSQL("COALESCE(NULLIF(started_at, ''), created_at)", "sessions.id")
	query := `SELECT id FROM sessions WHERE ` + pred + ` ORDER BY id`
	rows, err := d.getReader().QueryContext(t.Context(), query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		got = append(got, id)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"message-empty", "message-in", "message-null", "message-unreadable", "started-in",
	}, got)
	assert.Contains(t, queryPlanOf(t, d, query, args...),
		"SEARCH wm USING COVERING INDEX idx_messages_unparsed_timestamp (session_id=?)")
}
