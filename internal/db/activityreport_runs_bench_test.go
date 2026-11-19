package db

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// BenchmarkSQLiteActivityReportRefresh compares one refresh of a long
// in-progress report over 100,000 sessions with 25 messages each, before and
// after keeping runs: pairing every session and sweeping every candidate,
// against pairing only ten changed sessions and sweeping the kept runs.
func BenchmarkSQLiteActivityReportRefresh(b *testing.B) {
	d := testDB(b)
	const sessions = 100_000
	span := int((210 * 24 * time.Hour).Seconds())
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < ? - 1)
		INSERT INTO sessions(id, project, agent, started_at, ended_at, created_at, message_count)
		SELECT printf('refresh-%06d', i), 'project-' || (i % 20), 'claude',
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-05T00:00:00Z', printf('+%d seconds', i * 7919 % ?)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-05T02:00:00Z', printf('+%d seconds', i * 7919 % ?)),
			strftime('%Y-%m-%dT%H:%M:%SZ', '2026-01-05T00:00:00Z', printf('+%d seconds', i * 7919 % ?)),
			25
		FROM n`, []any{sessions, span, span, span}},
		{`WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i + 1 FROM n WHERE i < 24)
		INSERT INTO messages(session_id, ordinal, role, content, timestamp, model)
		SELECT s.id, n.i, CASE WHEN n.i % 2 = 0 THEN 'user' ELSE 'assistant' END, 'x',
			strftime('%Y-%m-%dT%H:%M:%fZ', s.started_at,
				printf('+%d.%03d seconds', n.i * (20 + (s.rowid + n.i * 13) % 70) + CASE WHEN n.i = 12 THEN 1800 ELSE 0 END,
					n.i * 37 % 1000)),
			CASE WHEN n.i % 2 = 1 THEN 'model-a' ELSE '' END
		FROM sessions s CROSS JOIN n`, nil},
		{`INSERT INTO tool_result_events(session_id, tool_call_message_ordinal, call_index, tool_use_id, source, status, content, timestamp, event_index)
		SELECT session_id, ordinal, 0, 'call', 'tool_execution', 'completed', '',
			strftime('%Y-%m-%dT%H:%M:%fZ', timestamp, '+1 seconds'), 0
		FROM messages WHERE ordinal % 12 = 5`, nil},
		{`ANALYZE`, nil},
	} {
		_, err := d.getWriter().Exec(b.Context(), stmt.sql, stmt.args...)
		require.NoError(b, err)
	}
	ids := make([]string, sessions)
	metas := make([]activity.SessionMeta, sessions)
	for i := range ids {
		ids[i] = fmt.Sprintf("refresh-%06d", i)
		metas[i] = activity.SessionMeta{SessionID: ids[i], IsAutomated: i%7 == 0}
	}
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", From: "2026-01-05T00:00:00Z", To: "2026-08-24T00:00:00Z", Timezone: "UTC", BucketOverride: "1w",
	}, time.Date(2026, 8, 20, 15, 4, 5, 0, time.UTC))
	require.NoError(b, err)
	p := activity.Params{
		RangeStart: q.RangeStart, RangeEnd: q.RangeEnd, Loc: q.Loc, EffectiveEnd: q.EffectiveEnd,
		Partial: q.Partial, GapCapSeconds: q.GapCapSeconds, Bucket: q.Bucket,
	}
	ctx := b.Context()

	sessionRows := make([]activity.SessionRow, len(metas))
	for i, m := range metas {
		sessionRows[i] = activity.SessionRow{SessionID: m.SessionID, IsAutomated: m.IsAutomated, IsSubagent: m.IsSubagent}
	}
	type countedMessage struct {
		id string
		m  activityCountedMessage
	}
	pair := func() ([]activity.IntervalCandidate, []countedMessage) {
		var candidates []activity.IntervalCandidate
		var counted []countedMessage
		upper := q.EffectiveEnd.UnixMicro()
		require.NoError(b, d.activityReportCandidateSQL(ids, activityPairingWindow(q), func(id string, m activityCountedMessage) {
			counted = append(counted, countedMessage{id, m})
		})(ctx, func(c activity.IntervalCandidate) error {
			if c.Start.UnixMicro() < upper {
				candidates = append(candidates, c)
			}
			return nil
		}))
		return candidates, counted
	}
	candidates, counted := pair()
	b.Run("before/pair", func(b *testing.B) {
		for b.Loop() {
			pair()
		}
		b.ReportMetric(float64(len(candidates)), "candidates")
	})
	b.Run("before/sweep", func(b *testing.B) {
		for b.Loop() {
			_, err := activity.BuildCandidateArtifacts(ctx, p, metas, candidates, nil)
			require.NoError(b, err)
		}
	})
	b.Run("before/counts", func(b *testing.B) {
		artifacts := activity.CandidateArtifacts{
			Report: activity.Report{Buckets: make([]activity.Bucket, 40)}, Sessions: sessionRows, Membership: map[string]activity.BucketMembership{},
		}
		for b.Loop() {
			add := newActivityMessageCounter(q, activity.NewMessageAccumulator(q, &artifacts))
			for _, c := range counted {
				add(c.id, c.m)
			}
		}
	})

	// Keep every session, then refresh as after/refresh-runs does, so each
	// step below measures the whole archive when it runs alone.
	_, _, err = d.activityReportRuns(ctx, ids, q)
	require.NoError(b, err)
	forget := func() {
		d.activityMemo.mu.Lock()
		defer d.activityMemo.mu.Unlock()
		for _, id := range ids[:10] {
			for key, element := range d.activityMemo.entries {
				if key.id == id {
					d.activityMemo.bytes -= element.Value.(activityMemoEntry).bytes()
					d.activityMemo.order.Remove(element)
					delete(d.activityMemo.entries, key)
				}
			}
		}
	}
	forget()
	runs, kept, err := d.activityReportRuns(ctx, ids, q)
	require.NoError(b, err)
	b.Run("after/derive-all", func(b *testing.B) {
		for b.Loop() {
			d.activityMemo.swap(func() {})
			_, _, err := d.activityReportRuns(ctx, ids, q)
			require.NoError(b, err)
		}
		require.Len(b, d.activityMemo.entries, sessions)
		b.ReportMetric(float64(d.activityMemo.bytes)/(1<<20), "memo-MiB")
	})
	b.Run("after/refresh-runs", func(b *testing.B) {
		for b.Loop() {
			// Ten sessions changed since the last refresh.
			b.StopTimer()
			forget()
			b.StartTimer()
			_, _, err := d.activityReportRuns(ctx, ids, q)
			require.NoError(b, err)
		}
		b.ReportMetric(float64(len(runs)), "runs")
	})
	b.Run("after/sweep", func(b *testing.B) {
		for b.Loop() {
			_, err := activity.BuildRunArtifactsWithSurvivorUsage(ctx, p, metas,
				runSlice(runs), func() ([]activity.UsageRow, error) { return nil, nil })
			require.NoError(b, err)
		}
	})
	b.Run("after/counts", func(b *testing.B) {
		artifacts := activity.CandidateArtifacts{
			Report: activity.Report{Buckets: make([]activity.Bucket, 40)}, Sessions: sessionRows, Membership: map[string]activity.BucketMembership{},
		}
		backend := activityReportBackend{DB: d, counted: &activityReportCounted{sessions: kept, complete: true}}
		for b.Loop() {
			require.NoError(b, backend.ActivityReportMessageCounts(ctx, ids, q, &artifacts))
		}
	})
}
