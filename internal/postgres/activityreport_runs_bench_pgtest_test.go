//go:build pgtest

package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// BenchmarkPGActivityReportRefresh compares one refresh of a long
// in-progress report over 100,000 sessions with 25 messages each, before and
// after keeping runs: pairing every session and sweeping every candidate,
// against pairing only ten changed sessions and sweeping the kept runs.
// before/transcripts is the part of before/pair that kept transcripts
// skipped.
func BenchmarkPGActivityReportRefresh(b *testing.B) {
	ctx := b.Context()
	_, store := prepareUsageSchema(b, "agentsview_activity_refresh_bench")
	const sessions = 100_000
	for _, stmt := range []string{
		`INSERT INTO sessions (id, machine, project, agent, started_at)
		SELECT format('refresh-%s', lpad(i::text, 6, '0')), 'machine', 'project', 'claude',
			timestamptz '2026-01-05 00:00:00+00' + (i * 7919 % (210 * 86400)) * interval '1 second'
		FROM generate_series(0, 99999) i`,
		`INSERT INTO messages (session_id, ordinal, role, content, timestamp, model)
		SELECT s.id, n, CASE WHEN n % 2 = 0 THEN 'user' ELSE 'assistant' END, '',
			s.started_at + (n * (20 + (hashtext(s.id) & 63)) + CASE WHEN n >= 12 THEN 1800 ELSE 0 END) * interval '1 second'
				+ (n * 37 % 1000) * interval '1 millisecond',
			CASE WHEN n % 2 = 1 THEN 'model-a' ELSE '' END
		FROM sessions s CROSS JOIN generate_series(0, 24) n`,
		`INSERT INTO tool_result_events (session_id, tool_call_message_ordinal, call_index, tool_use_id, source, status, content, timestamp, event_index)
		SELECT session_id, ordinal, 0, 'call', 'tool_execution', 'completed', '', timestamp + interval '1 second', 0
		FROM messages WHERE ordinal % 12 = 5`,
		`ANALYZE`,
	} {
		_, err := store.DB().ExecContext(ctx, stmt)
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

	pair := func() ([]activity.IntervalCandidate, []pgActivityCountedMessage) {
		var candidates []activity.IntervalCandidate
		var counted []pgActivityCountedMessage
		require.NoError(b, store.activityReportCandidateSourceWithCounts(ids, q, &counted)(ctx, func(c activity.IntervalCandidate) error {
			candidates = append(candidates, c)
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
	b.Run("before/transcripts", func(b *testing.B) {
		for b.Loop() {
			require.NoError(b, store.activityTranscriptSource(ctx, ids, func(string, []activity.TranscriptMessage) {}))
		}
	})
	b.Run("before/sweep", func(b *testing.B) {
		for b.Loop() {
			_, err := activity.BuildCandidateArtifacts(ctx, p, metas, candidates, nil)
			require.NoError(b, err)
		}
	})
	b.Run("before/counts", func(b *testing.B) {
		backend := activityReportBackend{Store: store, counted: &counted}
		artifacts := emptyArtifacts()
		for b.Loop() {
			require.NoError(b, backend.ActivityReportMessageCounts(ctx, ids, q, &artifacts))
		}
	})

	// Keep every session, then refresh as after/refresh-runs does, so each
	// step below measures the whole archive when it runs alone.
	inputs := &pgActivityInputs{sessions: map[string]pgActivitySession{}}
	_, err = store.activityReportRuns(ctx, ids, q, inputs)
	require.NoError(b, err)
	forget := func() {
		for _, id := range ids[:10] {
			delete(inputs.sessions, id)
		}
	}
	forget()
	runs, err := store.activityReportRuns(ctx, ids, q, inputs)
	require.NoError(b, err)
	b.Run("after/derive-all", func(b *testing.B) {
		all := &pgActivityInputs{}
		for b.Loop() {
			all.sessions = map[string]pgActivitySession{}
			_, err := store.activityReportRuns(ctx, ids, q, all)
			require.NoError(b, err)
		}
		b.ReportMetric(float64(pgActivityInputBytes(*all))/(1<<20), "kept-MiB")
	})
	b.Run("after/refresh-runs", func(b *testing.B) {
		for b.Loop() {
			// Ten sessions changed since the last refresh.
			forget()
			_, err := store.activityReportRuns(ctx, ids, q, inputs)
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
		backend := activityReportRunBackend{activityReportBackend{Store: store, inputs: inputs}}
		artifacts := emptyArtifacts()
		for b.Loop() {
			require.NoError(b, backend.ActivityReportMessageCounts(ctx, ids, q, &artifacts))
		}
	})
}
