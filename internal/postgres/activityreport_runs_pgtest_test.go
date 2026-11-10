//go:build pgtest

package postgres

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// Sweeping the kept runs gives the report that sweeping every candidate of
// the range's pairing gives, from fresh inputs and from kept ones. Sessions
// cover clock reversals, unstamped messages, tool events, sessions with only
// tool events, gaps of exactly the cap, and sub-second activity across hour
// boundaries and the effective end's microsecond.
func TestPGActivityRunsMatchCandidateSweep(t *testing.T) {
	ctx := t.Context()
	_, store := prepareUsageSchema(t, "agentsview_activity_runs_test")
	rng := rand.New(rand.NewPCG(4, 9))
	hot := []time.Time{
		time.Date(2026, 6, 16, 13, 17, 42, 123456789, time.UTC),
		time.Date(2026, 6, 16, 3, 0, 0, 250_000_000, time.UTC),
		time.Date(2026, 6, 16, 9, 0, 0, 0, time.UTC),
	}
	steps := []time.Duration{
		0, time.Microsecond, 300 * time.Second, 300*time.Second + time.Microsecond,
		300*time.Second - time.Microsecond, 999 * time.Millisecond, time.Second, -90 * time.Second, -time.Microsecond,
	}
	roles := []string{"user", "assistant", "assistant", "tool"}
	models := []string{"", "model-a", "model-b"}
	base := time.Date(2026, 6, 15, 22, 0, 0, 0, time.UTC)
	var ids []string
	var metas []activity.SessionMeta
	for i := range 150 {
		id := fmt.Sprintf("run-%03d", i)
		ids = append(ids, id)
		metas = append(metas, activity.SessionMeta{SessionID: id, IsAutomated: rng.IntN(4) == 0, IsSubagent: rng.IntN(5) == 0})
		_, err := store.DB().ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent) VALUES ($1, 'machine', 'project', 'codex')`, id)
		require.NoError(t, err)
		at := base.Add(time.Duration(rng.IntN(30))*time.Hour + time.Duration(rng.Int64N(int64(2*time.Second))) - time.Second)
		dense, pinpoint := rng.IntN(3) == 0, rng.IntN(4) == 0
		if pinpoint {
			at = hot[rng.IntN(len(hot))].Truncate(time.Microsecond).Add(-time.Duration(rng.IntN(3)) * time.Microsecond)
		}
		var stamps []time.Time
		messages := rng.IntN(14)
		for ordinal := range messages {
			switch {
			case pinpoint:
				at = at.Add(time.Duration(rng.IntN(2)) * time.Microsecond)
			case dense:
				at = at.Add(time.Duration(rng.Int64N(int64(700 * time.Millisecond))))
			case rng.IntN(3) == 0:
				at = at.Add(steps[rng.IntN(len(steps))])
			default:
				at = at.Add(time.Duration(rng.Int64N(int64(400 * time.Second))))
			}
			var ts any
			if rng.IntN(6) != 0 {
				stamp := at.Truncate(time.Microsecond)
				ts = stamp
				stamps = append(stamps, stamp)
			}
			subtype := ""
			if rng.IntN(8) == 0 {
				subtype = "tool_result"
			}
			_, err := store.DB().ExecContext(ctx, `INSERT INTO messages
				(session_id, ordinal, role, content, timestamp, model, is_system, source_subtype)
				VALUES ($1, $2, $3, '', $4, $5, $6, $7)`,
				id, ordinal, roles[rng.IntN(len(roles))], ts, models[rng.IntN(len(models))], rng.IntN(10) == 0, subtype)
			require.NoError(t, err)
		}
		events := rng.IntN(5)
		if messages == 0 {
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
			_, err := store.DB().ExecContext(ctx, `INSERT INTO tool_result_events
				(session_id, tool_call_message_ordinal, call_index, tool_use_id, source, status, content, timestamp, event_index)
				VALUES ($1, $2, $3, 'call', 'tool_execution', 'completed', '', $4, $5)`,
				id, rng.IntN(max(1, messages)), rng.IntN(2), ts.Truncate(time.Microsecond), e)
			require.NoError(t, err)
		}
	}
	later := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	queries := []struct {
		name  string
		input activity.QueryInput
		now   time.Time
	}{
		{"ended day", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, later},
		{"in progress", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, hot[0]},
		{"in progress at a second", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, hot[2]},
		{"zoned day", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "Asia/Tokyo"}, later},
		{"custom 5m", activity.QueryInput{Preset: "custom", From: "2026-06-16T03:00:00.250Z", To: "2026-06-16T09:30:00.75Z", Timezone: "UTC", BucketOverride: "5m"}, later},
		{"week", activity.QueryInput{Preset: "week", Date: "2026-06-16", Timezone: "UTC"}, later},
	}
	// One kept set serves every range, as kept inputs serve every effective
	// end of a slot.
	kept := &pgActivityInputs{sessions: map[string]pgActivitySession{}}
	for _, query := range queries {
		q, err := activity.ResolveQuery(query.input, query.now)
		require.NoError(t, err)
		p := activity.Params{
			RangeStart: q.RangeStart, RangeEnd: q.RangeEnd, Loc: q.Loc, EffectiveEnd: q.EffectiveEnd,
			Partial: q.Partial, GapCapSeconds: q.GapCapSeconds, Bucket: q.Bucket,
		}
		var counted []pgActivityCountedMessage
		candidates := activityReportBackend{Store: store, counted: &counted}
		want, err := activity.BuildCandidateArtifactsFromSource(ctx, p, metas, candidates.ActivityReportCandidateSource(ids, q), nil)
		require.NoError(t, err)
		require.NoError(t, candidates.ActivityReportMessageCounts(ctx, ids, q, &want))
		for _, inputs := range []*pgActivityInputs{{sessions: map[string]pgActivitySession{}}, kept} {
			runs := activityReportRunBackend{activityReportBackend{Store: store, inputs: inputs}}
			got, err := activity.BuildRunArtifactsWithSurvivorUsage(ctx, p, metas,
				runs.ActivityReportRunSource(ids, q), func() ([]activity.UsageRow, error) { return nil, nil })
			require.NoError(t, err)
			require.NoError(t, runs.ActivityReportMessageCounts(ctx, ids, q, &got))
			require.Equal(t, want, got, query.name)
		}
	}
}
