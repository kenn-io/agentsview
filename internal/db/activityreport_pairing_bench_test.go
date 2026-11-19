package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// seedActivityTranscriptBenchmark writes a month of transcripts: 2,000
// sessions of 50 alternating user and assistant messages with 1-3 KB bodies,
// so reading a message's model costs what it does in a real archive.
func seedActivityTranscriptBenchmark(b *testing.B) (*DB, []string, activity.Query) {
	b.Helper()
	d := testDB(b)
	const sessionCount = 2_000
	for _, stmt := range []string{
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 1999)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at, message_count)
		SELECT printf('pair-%04d', i), 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-01T00:00:00Z', printf('+%d minutes', i * 20)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-01T00:49:00Z', printf('+%d minutes', i * 20)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-07-01T00:00:00Z', printf('+%d minutes', i * 20)),
			50
		FROM n`,
		`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 49)
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, content_length, model)
		SELECT s.id, n.i,
			CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END,
			hex(randomblob(500 + (n.i * 397 + CAST(substr(s.id, 6) AS INTEGER) * 7919) % 1000)),
			strftime('%Y-%m-%dT%H:%M:%fZ', s.started_at, printf('+%d.%03d seconds', n.i * 60, n.i * 37 % 1000)),
			1000 + (n.i * 397) % 2000,
			CASE WHEN n.i % 2 = 1 THEN 'claude-sonnet-4-5' ELSE '' END
		FROM sessions s CROSS JOIN n`,
		`ANALYZE`,
	} {
		_, err := d.getWriter().Exec(b.Context(), stmt)
		require.NoError(b, err)
	}
	ids := make([]string, sessionCount)
	for i := range ids {
		ids[i] = fmt.Sprintf("pair-%04d", i)
	}
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "month", Date: "2026-07-01", Timezone: "UTC",
	}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(b, err)
	return d, ids, q
}

// BenchmarkSQLiteActivityReportPairTranscripts pairs a month of transcripts
// the way a first Activity open does, before the pairing memo holds them.
func BenchmarkSQLiteActivityReportPairTranscripts(b *testing.B) {
	d, ids, q := seedActivityTranscriptBenchmark(b)
	sessionCount := len(ids)
	source := d.activityReportCandidateSQL(ids, q, nil)
	b.ReportAllocs()
	for b.Loop() {
		count := 0
		require.NoError(b, source(b.Context(), func(activity.IntervalCandidate) error {
			count++
			return nil
		}))
		require.Equal(b, sessionCount*49, count)
	}
}

// BenchmarkSQLiteActivityReportFirstOpen builds a month's report with an
// empty pairing memo, as the first Activity open after a start does.
func BenchmarkSQLiteActivityReportFirstOpen(b *testing.B) {
	d, ids, q := seedActivityTranscriptBenchmark(b)
	b.ReportAllocs()
	for b.Loop() {
		d.activityMemo.swap(func() {})
		artifacts, err := d.BuildActivityReportArtifacts(b.Context(), AnalyticsFilter{Timezone: "UTC"}, q, nil)
		require.NoError(b, err)
		require.Len(b, artifacts.Sessions, len(ids))
	}
}
