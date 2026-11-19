package clickhouse

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
	"unique"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// benchActivityArchive builds pairing inputs for a long in-progress range:
// 100,000 sessions with 25 messages and 2 tool events each on average.
func benchActivityArchive(b *testing.B, q activity.Query) (map[string]activitySessionInputs, []activity.SessionMeta, []string) {
	b.Helper()
	rng := rand.New(rand.NewPCG(3, 5))
	span := q.EffectiveEnd.Sub(q.RangeStart)
	user, assistant, model := unique.Make("user"), unique.Make("assistant"), unique.Make("model-a")
	inputs := make(map[string]activitySessionInputs, 100_000)
	metas := make([]activity.SessionMeta, 0, 100_000)
	ids := make([]string, 0, 100_000)
	for i := range 100_000 {
		id := fmt.Sprintf("session-%06d", i)
		ids = append(ids, id)
		metas = append(metas, activity.SessionMeta{SessionID: id, IsAutomated: i%7 == 0, IsSubagent: i%5 == 0})
		at := q.RangeStart.Add(time.Duration(rng.Int64N(int64(span))))
		n := 1 + rng.IntN(49)
		entry := activitySessionInputs{messages: make([]clickActivityMessage, 0, n)}
		for ordinal := range n {
			// Mostly short steps, sometimes an idle gap past the cap.
			step := time.Duration(1+rng.IntN(90)) * time.Second
			if rng.IntN(20) == 0 {
				step = time.Duration(10+rng.IntN(120)) * time.Minute
			}
			at = at.Add(step)
			m := clickActivityMessage{ordinal: ordinal, us: at.UnixMicro(), role: user, model: noModel, counted: true}
			if ordinal%2 == 1 {
				m.role, m.model = assistant, model
			}
			entry.messages = append(entry.messages, m)
			if rng.IntN(12) == 0 {
				entry.events = append(entry.events, clickActivityTerminal{ordinal: ordinal, us: at.Add(time.Second).UnixMicro()})
			}
		}
		inputs[id] = entry
	}
	return inputs, metas, ids
}

// BenchmarkActivityReportRefresh compares one refresh of a long
// in-progress report before and after keeping derived runs: pairing every
// session and sweeping every candidate, against deriving only the sessions
// a push changed and sweeping the runs.
func BenchmarkActivityReportRefresh(b *testing.B) {
	now := time.Date(2026, 8, 20, 15, 4, 5, 0, time.UTC)
	q, err := activity.ResolveQuery(activity.QueryInput{
		Preset: "custom", From: "2026-01-05T00:00:00Z", To: "2026-08-24T00:00:00Z", Timezone: "UTC", BucketOverride: "1w",
	}, now)
	require.NoError(b, err)
	inputs, metas, ids := benchActivityArchive(b, q)
	params := activityParams(q)
	ctx := b.Context()
	lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second)
	var store Store

	sessionRows := make([]activity.SessionRow, len(metas))
	for i, m := range metas {
		sessionRows[i] = activity.SessionRow{SessionID: m.SessionID, IsAutomated: m.IsAutomated, IsSubagent: m.IsSubagent}
	}
	b.Run("before/pair", func(b *testing.B) {
		for b.Loop() {
			_, _, err := store.pairActivitySessions(ctx, inputs, ids, lower, q.EffectiveEnd, q)
			require.NoError(b, err)
		}
	})
	paired, terminal, err := store.pairActivitySessions(ctx, inputs, ids, lower, q.EffectiveEnd, q)
	require.NoError(b, err)
	b.Run("before/sweep", func(b *testing.B) {
		for b.Loop() {
			_, err := activity.BuildCandidateArtifactsFromSource(ctx, params, metas,
				activity.MergeCandidateSlice(terminal, func(_ context.Context, yield func(activity.IntervalCandidate) error) error {
					for _, c := range paired {
						if err := yield(c); err != nil {
							return err
						}
					}
					return nil
				}), nil)
			require.NoError(b, err)
		}
		b.ReportMetric(float64(len(paired)+len(terminal)), "candidates")
	})
	b.Run("before/counts", func(b *testing.B) {
		artifacts := activity.CandidateArtifacts{
			Report: activity.Report{Buckets: make([]activity.Bucket, 40)}, Sessions: sessionRows, Membership: map[string]activity.BucketMembership{},
		}
		for b.Loop() {
			counts := activity.NewMessageAccumulator(q, &artifacts)
			for _, id := range ids {
				for _, m := range inputs[id].messages {
					if m.counted && m.stamped() && !m.at().Before(q.RangeStart) {
						counts.Add(id, m.role.Value(), m.at())
					}
				}
			}
		}
	})

	derived := make(map[string]activitySessionDerived, len(ids))
	deriver := newActivityDeriver(q.GapCapSeconds)
	for _, id := range ids {
		derived[id] = deriver.derive(id, inputs[id])
	}
	b.Run("after/derive-all", func(b *testing.B) {
		all := make(map[string]activitySessionDerived, len(ids))
		for b.Loop() {
			deriver := newActivityDeriver(q.GapCapSeconds)
			for _, id := range ids {
				all[id] = deriver.derive(id, inputs[id])
			}
		}
		var bytes int64
		for id, d := range all {
			bytes += d.bytes(id)
		}
		b.ReportMetric(float64(bytes)/(1<<20), "memo-MiB")
	})
	b.Run("after/derive-changed", func(b *testing.B) {
		for b.Loop() {
			deriver := newActivityDeriver(q.GapCapSeconds)
			for _, id := range ids[:10] {
				derived[id] = deriver.derive(id, inputs[id])
			}
		}
	})
	b.Run("after/clip", func(b *testing.B) {
		for b.Loop() {
			clipActivityRuns(derived, ids, q)
		}
	})
	runs := clipActivityRuns(derived, ids, q)
	b.Run("after/sweep", func(b *testing.B) {
		for b.Loop() {
			_, err := activity.BuildRunArtifactsWithSurvivorUsage(ctx, params, metas,
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
		b.ReportMetric(float64(len(runs)), "runs")
	})
	b.Run("after/counts", func(b *testing.B) {
		done := make(chan struct{})
		close(done)
		pairing := &activityReportPairing{done: done, sessions: derived}
		artifacts := activity.CandidateArtifacts{
			Report: activity.Report{Buckets: make([]activity.Bucket, 40)}, Sessions: sessionRows, Membership: map[string]activity.BucketMembership{},
		}
		for b.Loop() {
			require.NoError(b, pairing.countMessages(ctx, ids, q, &artifacts))
		}
	})
}
