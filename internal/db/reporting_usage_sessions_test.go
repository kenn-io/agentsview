package db

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A usage row belongs to the window by its own timestamp, or by its session's
// start when it has none. Each session below isolates one of those cases.
func TestReportingUsageSessionsFromMatchesUsageTimestamps(t *testing.T) {
	d := testDB(t)
	const inRange, outOfRange = "2026-06-10T12:00:00Z", "2026-06-12T12:00:00Z"
	usage := `{"input_tokens":1,"output_tokens":1}`
	sessions := []struct{ id, startedAt string }{
		{"msg-in", outOfRange},
		{"msg-out", inRange},
		{"msg-null", inRange},
		{"msg-empty", inRange},
		{"msg-null-out", outOfRange},
		{"msg-synthetic", inRange},
		{"msg-no-usage", inRange},
		{"msg-deleted", inRange},
		{"event-in", outOfRange},
		{"event-null", inRange},
		{"event-empty", inRange},
		{"event-no-model", inRange},
	}
	for _, s := range sessions {
		_, err := d.getWriter().Exec(t.Context(),
			`INSERT INTO sessions(id, project, started_at, message_count) VALUES (?, 'p', ?, 1)`,
			s.id, s.startedAt)
		require.NoError(t, err)
	}
	for _, m := range []struct {
		sessionID string
		timestamp any
		model     string
		usage     string
	}{
		{"msg-in", inRange, "model-a", usage},
		{"msg-out", outOfRange, "model-a", usage},
		{"msg-null", nil, "model-a", usage},
		{"msg-empty", "", "model-a", usage},
		{"msg-null-out", nil, "model-a", usage},
		{"msg-synthetic", inRange, "<synthetic>", usage},
		{"msg-no-usage", inRange, "model-a", ""},
		{"msg-deleted", inRange, "model-a", usage},
	} {
		_, err := d.getWriter().Exec(t.Context(),
			`INSERT INTO messages(session_id, ordinal, role, content, timestamp, model, token_usage)
			 VALUES (?, 0, 'assistant', 'x', ?, ?, ?)`,
			m.sessionID, m.timestamp, m.model, m.usage)
		require.NoError(t, err)
	}
	for _, e := range []struct {
		sessionID  string
		occurredAt any
		model      string
	}{
		{"event-in", inRange, "model-a"},
		{"event-null", nil, "model-a"},
		{"event-empty", "", "model-a"},
		{"event-no-model", inRange, ""},
	} {
		_, err := d.getWriter().Exec(t.Context(),
			`INSERT INTO usage_events(session_id, source, model, occurred_at, input_tokens)
			 VALUES (?, 'test', ?, ?, 1)`,
			e.sessionID, e.model, e.occurredAt)
		require.NoError(t, err)
	}
	_, err := d.getWriter().Exec(t.Context(),
		`UPDATE sessions SET deleted_at = ? WHERE id = 'msg-deleted'`, inRange)
	require.NoError(t, err)

	tx, err := d.getReader().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	metas, ids, err := d.reportingUsageSessionsFrom(
		t.Context(), tx, "2026-06-10T00:00:00Z", "2026-06-10T23:59:59Z")
	require.NoError(t, err)
	assert.Equal(t, []string{"event-in", "event-null", "msg-empty", "msg-in", "msg-null"}, ids)
	require.Len(t, metas, len(ids))
	assert.Equal(t, "msg-in", metas[3].SessionID)
	assert.Equal(t, outOfRange, metas[3].StartedAt)
}
