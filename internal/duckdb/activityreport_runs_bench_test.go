//go:build !(windows && arm64)

package duckdb

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// BenchmarkDuckDBActivityReportRefresh compares one refresh of a long
// in-progress report over 100,000 sessions with 25 messages each, before and
// after keeping runs: pairing every session and sweeping every candidate,
// against pairing only ten changed sessions and sweeping the kept runs.
func BenchmarkDuckDBActivityReportRefresh(b *testing.B) {
	ctx := b.Context()
	conn, err := openDuckDB(filepath.Join(b.TempDir(), "activity-refresh.duckdb"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, conn.Close()) })
	require.NoError(b, configureDuckDBThreads(ctx, conn))
	require.NoError(b, EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)
	const sessions = 100_000
	for _, stmt := range []string{
		`INSERT INTO sessions (id, machine, project, agent, started_at, agentsview_push_fingerprint)
		SELECT 'refresh-' || lpad(CAST(i AS VARCHAR), 6, '0'), 'machine', 'project', 'claude',
			TIMESTAMP '2026-01-05 00:00:00' + to_seconds(i * 7919 % (210 * 86400)), 'push-1'
		FROM range(100000) t(i)`,
		`INSERT INTO messages (id, session_id, ordinal, role, content, timestamp, model)
		SELECT row_number() OVER (), s.id, n, CASE WHEN n % 2 = 0 THEN 'user' ELSE 'assistant' END, '',
			s.started_at + to_seconds(n * (20 + hash(s.id) % 64) + CASE WHEN n >= 12 THEN 1800 ELSE 0 END)
				+ to_milliseconds(n * 37 % 1000),
			CASE WHEN n % 2 = 1 THEN 'model-a' ELSE '' END
		FROM sessions s CROSS JOIN range(25) t(n)`,
		`INSERT INTO tool_result_events (session_id, tool_call_message_ordinal, call_index, tool_use_id, source, status, content, timestamp, event_index)
		SELECT session_id, ordinal, 0, 'call', 'tool_execution', 'completed', '', timestamp + INTERVAL 1 SECOND, 0
		FROM messages WHERE ordinal % 12 = 5`,
	} {
		_, err := conn.ExecContext(ctx, stmt)
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
	sessionRows := make([]activity.SessionRow, len(metas))
	for i, m := range metas {
		sessionRows[i] = activity.SessionRow{SessionID: m.SessionID, IsAutomated: m.IsAutomated, IsSubagent: m.IsSubagent}
	}
	emptyArtifacts := func() activity.CandidateArtifacts {
		return activity.CandidateArtifacts{
			Report: activity.Report{Buckets: make([]activity.Bucket, 40)}, Sessions: sessionRows, Membership: map[string]activity.BucketMembership{},
		}
	}

	pair := func() []activity.IntervalCandidate {
		var candidates []activity.IntervalCandidate
		require.NoError(b, store.ActivityReportCandidateSource(ids, q)(ctx, func(c activity.IntervalCandidate) error {
			candidates = append(candidates, c)
			return nil
		}))
		return candidates
	}
	candidates := pair()
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
		artifacts := emptyArtifacts()
		for b.Loop() {
			require.NoError(b, store.scanActivityCountedMessages(ctx, ids, activity.NewMessageAccumulator(q, &artifacts).Add))
		}
	})

	// Keep every session, then refresh as after/refresh-runs does, so each
	// step below measures the whole archive when it runs alone.
	_, _, err = store.activityReportRuns(ctx, ids, q)
	require.NoError(b, err)
	forget := func() {
		m := &store.activityRuns
		for _, id := range ids[:10] {
			key := activityRunsKey{id, q.GapCapSeconds}
			if element := m.entries[key]; element != nil {
				m.bytes -= element.Value.(activityRunsEntry).bytes()
				m.order.Remove(element)
				delete(m.entries, key)
			}
		}
	}
	forget()
	runs, kept, err := store.activityReportRuns(ctx, ids, q)
	require.NoError(b, err)
	b.Run("after/derive-all", func(b *testing.B) {
		for b.Loop() {
			store.activityRuns = activityRunsMemo{}
			_, _, err := store.activityReportRuns(ctx, ids, q)
			require.NoError(b, err)
		}
		require.Len(b, store.activityRuns.entries, sessions)
		b.ReportMetric(float64(store.activityRuns.bytes)/(1<<20), "memo-MiB")
	})
	b.Run("after/refresh-runs", func(b *testing.B) {
		for b.Loop() {
			// Ten sessions changed since the last refresh.
			b.StopTimer()
			forget()
			b.StartTimer()
			_, _, err := store.activityReportRuns(ctx, ids, q)
			require.NoError(b, err)
		}
		b.ReportMetric(float64(len(runs)), "runs")
	})
	b.Run("after/sweep", func(b *testing.B) {
		for b.Loop() {
			_, err := activity.BuildRunArtifactsWithSurvivorUsage(ctx, p, metas,
				func(_ context.Context, yield func(activity.Run) error) error {
					for _, run := range runs {
						if err := yield(run); err != nil {
							return err
						}
					}
					return nil
				}, func() ([]activity.UsageRow, error) { return nil, nil })
			require.NoError(b, err)
		}
	})
	b.Run("after/counts", func(b *testing.B) {
		backend := activityReportBackend{Store: store, kept: &activityReportKept{sessions: kept, complete: true}}
		artifacts := emptyArtifacts()
		for b.Loop() {
			require.NoError(b, backend.ActivityReportMessageCounts(ctx, ids, q, &artifacts))
		}
	})
}
