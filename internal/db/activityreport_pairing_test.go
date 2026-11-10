package db

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// legacyActivityReportCandidatesSQL is the SQL pairing that Go pairing
// replaced. It stays here as the oracle the replacement must match.
const legacyActivityReportCandidatesSQL = `SELECT
	m.session_id, m.ordinal, successor.ordinal,
	m.timestamp, successor.timestamp,
	successor.role, successor.model,
	COALESCE((
		SELECT prior.model
		FROM messages prior
		WHERE prior.session_id = m.session_id
			AND prior.ordinal <= m.ordinal
			AND prior.role = 'assistant'
			AND prior.model != ''
			AND prior.timestamp IS NOT NULL
			AND prior.timestamp != ''
			AND agentsview_timestamp_unix_micro(prior.timestamp) IS NOT NULL
			AND agentsview_timestamp_unix_micro(prior.timestamp) > (
				SELECT agentsview_timestamp_unix_micro(prior_previous.timestamp)
				FROM messages prior_previous
				WHERE prior_previous.session_id = prior.session_id
					AND prior_previous.ordinal < prior.ordinal
					AND prior_previous.timestamp IS NOT NULL
					AND prior_previous.timestamp != ''
					AND agentsview_timestamp_unix_micro(
						prior_previous.timestamp) IS NOT NULL
				ORDER BY prior_previous.ordinal DESC
				LIMIT 1
			)
		ORDER BY prior.ordinal DESC
		LIMIT 1
	), 'unknown')
FROM messages m INDEXED BY idx_messages_velocity
JOIN messages successor ON successor.id = (
	SELECT next.id
	FROM messages next
	WHERE next.session_id = m.session_id
		AND next.ordinal > m.ordinal
		AND next.timestamp IS NOT NULL
		AND next.timestamp != ''
		AND agentsview_timestamp_unix_micro(next.timestamp) IS NOT NULL
	ORDER BY next.ordinal
	LIMIT 1
)
WHERE m.session_id IN (SELECT value FROM json_each(?))
	AND m.timestamp IS NOT NULL
	AND m.timestamp != ''
	AND m.timestamp >= ?
	AND m.timestamp < ?
	AND agentsview_timestamp_unix_micro(m.timestamp) IS NOT NULL
	AND agentsview_timestamp_unix_micro(m.timestamp) >= ?
	AND agentsview_timestamp_unix_micro(m.timestamp) < ?
ORDER BY agentsview_timestamp_unix_micro(m.timestamp),
	m.session_id, m.ordinal`

func legacyActivityReportCandidates(
	t *testing.T, d *DB, ids []string, q activity.Query,
) []activity.IntervalCandidate {
	t.Helper()
	encoded, err := json.Marshal(ids)
	require.NoError(t, err)
	lowerTime := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second).UTC()
	upperTime := q.EffectiveEnd.UTC()
	rows, err := d.getReader().QueryContext(t.Context(), legacyActivityReportCandidatesSQL,
		string(encoded),
		PaddedUTCBound(lowerTime.Format(time.RFC3339), -14),
		PaddedUTCBound(upperTime.Format(time.RFC3339), 14),
		lowerTime.UnixMicro(), upperTime.UnixMicro())
	require.NoError(t, err)
	defer rows.Close()
	var out []activity.IntervalCandidate
	for rows.Next() {
		var c activity.IntervalCandidate
		var start, end string
		require.NoError(t, rows.Scan(&c.SessionID, &c.StartOrdinal, &c.EndOrdinal,
			&start, &end, &c.ClosingRole, &c.ClosingModel, &c.PriorModel))
		c.Start, err = time.Parse(time.RFC3339Nano, start)
		require.NoError(t, err)
		c.End, err = time.Parse(time.RFC3339Nano, end)
		require.NoError(t, err)
		c.Start, c.End = c.Start.UTC(), c.End.UTC()
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestSQLiteActivityReportPairingMatchesSQLPairing(t *testing.T) {
	d := testDB(t)
	sessions := map[string][]struct{ role, ts, model string }{
		"a-mixed": {
			{"user", "2026-06-15T23:50:00Z", ""},
			{"assistant", "2026-06-15T23:58:00.5Z", "old"},
			{"assistant", "2026-06-15T23:57:00Z", "reversed"},
			{"user", "", ""},
			{"assistant", "not-a-timestamp", "malformed"},
			{"user", "2026-06-16T00:00:00Z", ""},
			{"assistant", "2026-06-16T00:00:00.000001Z", "same-second"},
			{"assistant", "2026-06-16T00:00:00.0000012Z", "same-micro"},
			{"tool", "2026-06-16T01:00:00+02:00", "tool-model"},
			{"assistant", "2026-06-16T09:30:00.123456789Z", ""},
			{"user", "2026-06-16T23:59:59.9999999Z", ""},
			{"assistant", "2026-06-17T00:00:00Z", "after"},
			{"user", "2026-06-17T08:00:00Z", ""},
		},
		"b-modelless": {
			{"user", "2026-06-16T10:00:00Z", ""},
			{"assistant", "2026-06-16T10:01:00Z", ""},
			{"assistant", "2026-06-16T10:01:00Z", "tie"},
			{"assistant", "2026-06-16T10:02:00Z", "late"},
		},
		"c-single": {
			{"user", "2026-06-16T12:00:00Z", ""},
		},
		// An instant inside the day whose text sorts past the padded window.
		"e-far-offset": {
			{"user", "2026-06-17T18:00:00+20:00", ""},
			{"assistant", "2026-06-16T23:00:00Z", "m"},
		},
		"d-before": {
			{"user", "2026-06-10T12:00:00Z", ""},
			{"assistant", "2026-06-16T12:00:00Z", "m"},
		},
	}
	var ids []string
	for id, messages := range sessions {
		ids = append(ids, id)
		insertSession(t, d, id, "p")
		for ordinal, m := range messages {
			seedMessage(t, d, id, ordinal*3, m.role, m.ts, m.model)
		}
	}
	day := dayQuery(t, "2026-06-16", "UTC")
	partial := day
	partial.EffectiveEnd = time.Date(2026, 6, 16, 9, 30, 0, 123456500, time.UTC)
	tokyo := dayQuery(t, "2026-06-16", "Asia/Tokyo")
	for _, tt := range []struct {
		name string
		q    activity.Query
	}{
		{"utc day", day},
		{"partial day", partial},
		{"offset day", tokyo},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := legacyActivityReportCandidates(t, d, ids, tt.q)
			require.NotEmpty(t, want)
			got, err := d.activityReportCandidates(t.Context(), ids, tt.q)
			require.NoError(t, err)
			assert.Equal(t, want, got)
			d.activityMemo.swap(func() {})
		})
	}
}

func TestSQLiteActivityReportMemoCountsMatchSQLCounts(t *testing.T) {
	d := testDB(t)
	messages := []Message{
		{Role: "user", Timestamp: "2026-06-15T23:59:59.9999999Z"},
		{Role: "user", Timestamp: "2026-06-16T00:00:00Z"},
		{Role: "assistant", Timestamp: "2026-06-16T00:04:59.999999Z"},
		{Role: "assistant", Timestamp: "2026-06-16T09:30:00.1234565Z"},
		{Role: "user", Timestamp: "2026-06-16T09:30:00.1234566Z"},
		{Role: "user", Timestamp: "2026-06-16T03:00:00+02:00"},
		{Role: "assistant", Timestamp: "2026-06-17T18:00:00+20:00"},
		{Role: "user", Timestamp: "2026-06-16T05:00:00Z", SourceSubtype: "tool_result"},
		{Role: "assistant", Timestamp: "2026-06-16T05:01:00Z", IsSystem: true},
		{Role: "tool", Timestamp: "2026-06-16T05:02:00Z"},
		{Role: "user", Timestamp: "2026-06-16T05:invalid"},
		{Role: "user"},
	}
	for _, id := range []string{"interactive", "automated"} {
		insertSession(t, d, id, "p", func(s *Session) {
			s.StartedAt = Ptr("2026-06-15T23:59:59Z")
			s.EndedAt = Ptr("2026-06-16T09:31:00Z")
			s.IsAutomated = id == "automated"
		})
		for i := range messages {
			messages[i].SessionID = id
			messages[i].Ordinal = i
			messages[i].Content = "message"
		}
		insertMessages(t, d, messages...)
	}
	q := dayQuery(t, "2026-06-16", "UTC")
	q.EffectiveEnd = time.Date(2026, 6, 16, 9, 30, 0, 123456600, time.UTC)
	counts := func(t *testing.T, keep bool) []int {
		t.Helper()
		d.activityMemo.swap(func() {})
		clearKeptActivity(d)
		old := activityMemoMaxBytes
		if !keep {
			activityMemoMaxBytes = 0
		}
		artifacts, err := d.BuildActivityReportArtifacts(t.Context(), AnalyticsFilter{Timezone: "UTC"}, q, nil)
		activityMemoMaxBytes = old
		require.NoError(t, err)
		require.Equal(t, keep, len(d.activityMemo.entries) > 0)
		var out []int
		for _, bucket := range artifacts.Report.Buckets {
			out = append(out, bucket.UserMessages, bucket.AssistantMessages)
		}
		return out
	}
	read := counts(t, false)
	require.Contains(t, read, 2)
	assert.Equal(t, read, counts(t, true))

	// Rows rewritten without a new session version prove the memo serves counts.
	_, err := d.getWriter().ExecContext(t.Context(), `UPDATE messages SET is_system = 1`)
	require.NoError(t, err)
	clearKeptActivity(d)
	artifacts, err := d.BuildActivityReportArtifacts(t.Context(), AnalyticsFilter{Timezone: "UTC"}, q, nil)
	require.NoError(t, err)
	var kept []int
	for _, bucket := range artifacts.Report.Buckets {
		kept = append(kept, bucket.UserMessages, bucket.AssistantMessages)
	}
	assert.Equal(t, read, kept)
	assert.Equal(t, make([]int, len(read)), counts(t, false))
}
