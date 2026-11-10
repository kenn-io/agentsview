package db

import (
	"context"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// runSlice streams runs in order.
func runSlice(runs []activity.Run) activity.RunSource {
	return func(_ context.Context, yield func(activity.Run) error) error {
		for _, run := range runs {
			if err := yield(run); err != nil {
				return err
			}
		}
		return nil
	}
}

// seedActivityRunSessions stores sessions with the shapes the runs must
// preserve: clock reversals, unstamped messages, tool events, sessions with
// only tool events, gaps of exactly the cap, sub-microsecond times, offsets
// the message counter compares as stored strings, and sub-second activity
// across hour boundaries, and nanosecond steps through the instants in hot.
func seedActivityRunSessions(t *testing.T, d *DB, rng *rand.Rand, hot []time.Time, sessions int) ([]string, []activity.SessionMeta) {
	t.Helper()
	base := time.Date(2026, 6, 15, 22, 0, 0, 0, time.UTC)
	steps := []time.Duration{
		0, time.Nanosecond, time.Microsecond, 300 * time.Second, 300*time.Second + time.Microsecond,
		300*time.Second - time.Microsecond, 999 * time.Millisecond, time.Second, -90 * time.Second, -time.Microsecond,
	}
	zones := []*time.Location{
		time.UTC, time.FixedZone("", 2*3600), time.FixedZone("", 14*3600), time.FixedZone("", -(13*3600 + 59*60)),
		time.FixedZone("", -14*3600), time.FixedZone("", 20*3600),
	}
	roles := []string{"user", "assistant", "assistant", "tool"}
	models := []string{"", "model-a", "model-b"}
	// Most stored times are whole microseconds; some keep nanoseconds.
	format := func(at time.Time) string {
		if rng.IntN(4) != 0 {
			at = at.Truncate(time.Microsecond)
		}
		return at.In(zones[rng.IntN(len(zones))]).Format(time.RFC3339Nano)
	}
	var ids []string
	var metas []activity.SessionMeta
	for i := range sessions {
		id := fmt.Sprintf("s%03d", i)
		ids = append(ids, id)
		meta := activity.SessionMeta{SessionID: id, IsAutomated: rng.IntN(4) == 0, IsSubagent: rng.IntN(5) == 0}
		metas = append(metas, meta)
		insertSession(t, d, id, "p", func(s *Session) {
			s.IsAutomated = meta.IsAutomated
		})
		at := base.Add(time.Duration(rng.IntN(30)) * time.Hour)
		at = at.Add(time.Duration(rng.Int64N(int64(2*time.Second))) - time.Second)
		dense, pinpoint := rng.IntN(3) == 0, rng.IntN(4) == 0
		if pinpoint {
			at = hot[rng.IntN(len(hot))].Truncate(time.Microsecond).Add(-time.Duration(rng.IntN(3000)))
		}
		var messages []Message
		var stamps []time.Time
		for ordinal := range rng.IntN(14) {
			switch {
			case pinpoint:
				at = at.Add(time.Duration(rng.IntN(1500)))
			case dense:
				at = at.Add(time.Duration(rng.Int64N(int64(700 * time.Millisecond))))
			case rng.IntN(3) == 0:
				at = at.Add(steps[rng.IntN(len(steps))])
			default:
				at = at.Add(time.Duration(rng.Int64N(int64(400 * time.Second))))
			}
			m := Message{
				SessionID: id, Ordinal: ordinal, Role: roles[rng.IntN(len(roles))], Content: "x",
				Model: models[rng.IntN(len(models))], IsSystem: rng.IntN(10) == 0,
			}
			if rng.IntN(8) == 0 {
				m.SourceSubtype = "tool_result"
			}
			if rng.IntN(6) != 0 {
				m.Timestamp = format(at)
				stamps = append(stamps, at)
			}
			messages = append(messages, m)
		}
		if len(messages) > 0 {
			insertMessages(t, d, messages...)
		}
		events := rng.IntN(5)
		if len(messages) == 0 {
			events = 1 + rng.IntN(4)
		}
		for e := range events {
			var ts time.Time
			switch {
			case len(stamps) > 0 && rng.IntN(3) == 0:
				ts = stamps[rng.IntN(len(stamps))].Add(time.Duration(rng.IntN(2)) * time.Microsecond)
			case len(stamps) > 0:
				ts = stamps[rng.IntN(len(stamps))].Add(time.Duration(rng.Int64N(int64(500*time.Second))) - 100*time.Second)
			default:
				ts = at.Add(time.Duration(rng.Int64N(int64(900 * time.Second))))
			}
			timingInsertToolResultEvent(t, d, id, rng.IntN(max(1, len(messages))), rng.IntN(2),
				fmt.Sprintf("call-%d", e), "completed", format(ts), e)
		}
	}
	return ids, metas
}

// Sweeping the kept runs gives the report that sweeping every candidate of
// the same pairing scan gives, from a cold memo and from a kept one.
func TestSQLiteActivityRunsMatchCandidateSweep(t *testing.T) {
	ctx := t.Context()
	later := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	queries := []struct {
		name  string
		input activity.QueryInput
		now   time.Time
	}{
		{"ended day", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, later},
		{"in progress", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, time.Date(2026, 6, 16, 13, 17, 42, 123456789, time.UTC)},
		{"in progress at a second", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC)},
		// Same pairing window start as the day, later end: kept windows
		// end too early to answer it.
		{"two days", activity.QueryInput{Preset: "custom", From: "2026-06-16T00:00:00Z", To: "2026-06-18T00:00:00Z", Timezone: "UTC"}, later},
		{"zoned day", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "Asia/Tokyo"}, later},
		{"custom 5m", activity.QueryInput{Preset: "custom", From: "2026-06-16T03:00:00.250Z", To: "2026-06-16T09:30:00.75Z", Timezone: "UTC", BucketOverride: "5m"}, later},
		{"week", activity.QueryInput{Preset: "week", Date: "2026-06-16", Timezone: "UTC"}, later},
	}
	hot := []time.Time{
		time.Date(2026, 6, 16, 13, 17, 42, 123456789, time.UTC),
		time.Date(2026, 6, 16, 3, 0, 0, 250_000_000, time.UTC),
		time.Date(2026, 6, 16, 9, 30, 0, 750_000_000, time.UTC),
		time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC),
	}
	rng := rand.New(rand.NewPCG(9, 4))
	for trial := range 6 {
		d := testDB(t)
		ids, metas := seedActivityRunSessions(t, d, rng, hot, 40)
		for _, query := range queries {
			q, err := activity.ResolveQuery(query.input, query.now)
			require.NoError(t, err)
			p := activity.Params{
				RangeStart: q.RangeStart, RangeEnd: q.RangeEnd, Loc: q.Loc, EffectiveEnd: q.EffectiveEnd,
				Partial: q.Partial, GapCapSeconds: q.GapCapSeconds, Bucket: q.Bucket,
			}
			want, err := activity.BuildCandidateArtifactsFromSource(ctx, p, metas, d.activityReportCandidateSource(ids, q), nil)
			require.NoError(t, err)
			counts := newActivityMessageCounter(q, activity.NewMessageAccumulator(q, &want))
			require.NoError(t, d.activityReportCandidateSQL(ids, activityPairingWindow(q), counts)(ctx,
				func(activity.IntervalCandidate) error { return nil }))
			// carried reuses what earlier queries kept.
			for _, memo := range []string{"carried", "cold", "kept"} {
				if memo == "cold" {
					d.activityMemo.swap(func() {})
				}
				runs, counted, err := d.activityReportRuns(ctx, ids, q)
				require.NoError(t, err)
				got, err := activity.BuildRunArtifactsWithSurvivorUsage(ctx, p, metas,
					runSlice(runs), func() ([]activity.UsageRow, error) { return nil, nil })
				require.NoError(t, err)
				backend := activityReportBackend{DB: d, counted: &activityReportCounted{sessions: counted, complete: true}}
				require.NoError(t, backend.ActivityReportMessageCounts(ctx, ids, q, &got))
				require.Equal(t, want, got, "trial %d, %s, %s memo", trial, query.name, memo)
			}
		}
	}
}

// Message counts compare a stored time below a microsecond against the
// effective end, and an offset past +14:00 through its stored string.
func TestSQLiteActivityMessageCountsAtStoredTimeEdges(t *testing.T) {
	d := testDB(t)
	insertSession(t, d, "s", "p", func(s *Session) {
		s.StartedAt = Ptr("2026-06-16T09:00:00Z")
		s.EndedAt = Ptr("2026-06-16T09:31:00Z")
	})
	insertMessages(t, d,
		Message{SessionID: "s", Ordinal: 0, Role: "user", Content: "x", Timestamp: "2026-06-16T09:30:00.1234565Z"},
		Message{SessionID: "s", Ordinal: 1, Role: "user", Content: "x", Timestamp: "2026-06-16T09:30:00.1234567Z"},
		// 09:00 UTC, but its stored string sorts after the padded end.
		Message{SessionID: "s", Ordinal: 2, Role: "assistant", Content: "x", Timestamp: "2026-06-17T05:00:00+20:00"},
		Message{SessionID: "s", Ordinal: 3, Role: "assistant", Content: "x", Timestamp: "2026-06-16T09:10:00Z"},
	)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC", BucketOverride: "1h"},
		time.Date(2026, 6, 16, 9, 30, 0, 123456600, time.UTC))
	require.NoError(t, err)
	artifacts, err := d.BuildActivityReportArtifacts(t.Context(), AnalyticsFilter{Timezone: "UTC"}, q, nil)
	require.NoError(t, err)
	bucket := artifacts.Report.Buckets[9]
	assert.Equal(t, [2]int{1, 1}, [2]int{bucket.UserMessages, bucket.AssistantMessages})
}
