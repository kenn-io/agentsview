package db

import (
	"encoding/json/jsontext"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type storedUsageCounters struct {
	outputTokens, webSearchRequests int64
}

func readStoredUsageCounters(t *testing.T, d *DB, sessionID string) []storedUsageCounters {
	t.Helper()
	rows, err := d.Reader().Query(t.Context(), `
		SELECT usage_output_tokens, usage_web_search_requests
		FROM messages WHERE session_id = ? ORDER BY ordinal`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	var got []storedUsageCounters
	for rows.Next() {
		var c storedUsageCounters
		require.NoError(t, rows.Scan(&c.outputTokens, &c.webSearchRequests))
		got = append(got, c)
	}
	require.NoError(t, rows.Err())
	return got
}

func TestOpenFillsMessageUsageCountersOnOlderArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	d := testDBAtPath(t, path, "source")
	insertSession(t, d, "s", "proj")
	withUsage := func(ordinal int, usage string) Message {
		m := asstMsg("s", ordinal, "reply")
		m.TokenUsage = jsontext.Value(usage)
		return m
	}
	insertMessages(t, d,
		userMsg("s", 0, "question"),
		withUsage(1, `{"output_tokens":250,"server_tool_use":{"web_search_requests":3}}`),
		withUsage(2, `{"output_tokens":-5,"server_tool_use":{"web_search_requests":-2}}`),
		withUsage(3, `{"output_tokens":40,"server_tool_use":{"web_sea`),
	)
	// Rebuild the shape an older release wrote: no stored counter columns, and
	// a covering index without them.
	_, err := d.getWriter().Exec(t.Context(), `DROP INDEX idx_messages_usage_session_covering;
 ALTER TABLE messages DROP COLUMN usage_output_tokens;
 ALTER TABLE messages DROP COLUMN usage_web_search_requests;`)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	d, err = Open(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })

	assert.Equal(t, []storedUsageCounters{
		{0, 0},
		{250, 3},
		{0, 0},
		// A cut-off blob still yields the output tokens read before the cut.
		{40, 0},
	}, readStoredUsageCounters(t, d, "s"))
}

func TestLateMessageUsageStoresUsageCounters(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s1", "proj")
	insertMessages(t, d, asstMsg("s1", 0, "reply"))

	_, err := d.WriteSessionIncremental(t.Context(), "s1", nil, IncrementalSessionUpdate{
		MsgCount:    1,
		NextOrdinal: 1,
		MessageTokenUsageUpdates: []MessageTokenUsageUpdate{{
			Ordinal:          0,
			TokenUsage:       jsontext.Value(`{"input_tokens":10,"output_tokens":250,"server_tool_use":{"web_search_requests":2}}`),
			ContextTokens:    10,
			OutputTokens:     250,
			HasContextTokens: true,
			HasOutputTokens:  true,
		}},
	})
	require.NoError(t, err)

	assert.Equal(t, []storedUsageCounters{{250, 2}}, readStoredUsageCounters(t, d, "s1"))
}
