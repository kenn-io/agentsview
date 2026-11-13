//go:build !(windows && arm64)

package duckdb

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// Sweeping the kept runs gives the report that sweeping every candidate of
// the range's pairing gives, from a cold memo, from a kept one, and after a
// push changes sessions. Sessions cover clock reversals, unstamped messages,
// tool events, sessions with only tool events, gaps of exactly the cap, and
// sub-second activity across hour boundaries and the effective end's
// microsecond.
func TestDuckDBActivityRunsMatchCandidateSweep(t *testing.T) {
	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)
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
	stamp := func(at time.Time) string { return at.Truncate(time.Microsecond).Format(time.RFC3339Nano) }
	messageID := 0
	addMessage := func(id string, ordinal int, role string, ts any, model string, system bool, subtype string) {
		messageID++
		_, err := conn.ExecContext(ctx, `INSERT INTO messages
			(id, session_id, ordinal, role, content, timestamp, model, is_system, source_subtype)
			VALUES (?, ?, ?, ?, '', CAST(? AS TIMESTAMP), ?, ?, ?)`,
			messageID, id, ordinal, role, ts, model, system, subtype)
		require.NoError(t, err)
	}
	var ids []string
	var metas []activity.SessionMeta
	for i := range 150 {
		id := fmt.Sprintf("run-%03d", i)
		ids = append(ids, id)
		metas = append(metas, activity.SessionMeta{SessionID: id, IsAutomated: rng.IntN(4) == 0, IsSubagent: rng.IntN(5) == 0})
		_, err := conn.ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent, agentsview_push_fingerprint)
			VALUES (?, 'machine', 'project', 'codex', 'push-1')`, id)
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
				ts = stamp(at)
				stamps = append(stamps, at.Truncate(time.Microsecond))
			}
			subtype := ""
			if rng.IntN(8) == 0 {
				subtype = "tool_result"
			}
			addMessage(id, ordinal, roles[rng.IntN(len(roles))], ts, models[rng.IntN(len(models))], rng.IntN(10) == 0, subtype)
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
			_, err := conn.ExecContext(ctx, `INSERT INTO tool_result_events
				(session_id, tool_call_message_ordinal, call_index, tool_use_id, source, status, content, timestamp, event_index)
				VALUES (?, ?, ?, 'call', 'tool_execution', 'completed', '', CAST(? AS TIMESTAMP), ?)`,
				id, rng.IntN(max(1, messages)), rng.IntN(2), stamp(ts), e)
			require.NoError(t, err)
		}
	}
	later := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	queries := []struct {
		name  string
		input activity.QueryInput
		now   time.Time
		gap   float64
	}{
		{"ended day", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, later, 0},
		{"in progress", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, hot[0], 0},
		{"in progress at a second", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, hot[2], 0},
		{"zoned day", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "Asia/Tokyo"}, later, 0},
		{"custom 5m", activity.QueryInput{Preset: "custom", From: "2026-06-16T03:00:00.250Z", To: "2026-06-16T09:30:00.75Z", Timezone: "UTC", BucketOverride: "5m"}, later, 0},
		{"week", activity.QueryInput{Preset: "week", Date: "2026-06-16", Timezone: "UTC"}, later, 0},
		{"two minute cap", activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"}, later, 120},
	}
	check := func(memo string) {
		t.Helper()
		for _, query := range queries {
			q, err := activity.ResolveQuery(query.input, query.now)
			require.NoError(t, err)
			if query.gap != 0 {
				q.GapCapSeconds = query.gap
			}
			p := activity.Params{
				RangeStart: q.RangeStart, RangeEnd: q.RangeEnd, Loc: q.Loc, EffectiveEnd: q.EffectiveEnd,
				Partial: q.Partial, GapCapSeconds: q.GapCapSeconds, Bucket: q.Bucket,
			}
			want, err := activity.BuildCandidateArtifactsFromSource(ctx, p, metas, store.ActivityReportCandidateSource(ids, q), nil)
			require.NoError(t, err)
			require.NoError(t, store.scanActivityCountedMessages(ctx, ids, activity.NewMessageAccumulator(q, &want).Add))
			if memo == "cold" {
				store.activityRuns = activityRunsMemo{}
			}
			backend := activityReportBackend{Store: store, kept: &activityReportKept{}}
			got, err := activity.BuildRunArtifactsWithSurvivorUsage(ctx, p, metas,
				backend.ActivityReportRunSource(ids, q), func() ([]activity.UsageRow, error) { return nil, nil })
			require.NoError(t, err)
			require.NoError(t, backend.ActivityReportMessageCounts(ctx, ids, q, &got))
			require.Equal(t, want, got, "%s, %s memo", query.name, memo)
		}
	}
	check("cold")
	check("kept")
	// A push adds activity to every tenth session in the day and changes its
	// fingerprint.
	for i := 0; i < len(ids); i += 10 {
		at := time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		addMessage(ids[i], 100, "user", stamp(at), "", false, "")
		addMessage(ids[i], 101, "assistant", stamp(at.Add(40*time.Second)), "model-b", false, "")
		_, err := conn.ExecContext(ctx, `UPDATE sessions SET agentsview_push_fingerprint = 'push-2' WHERE id = ?`, ids[i])
		require.NoError(t, err)
	}
	check("pushed")
}

// A session without a push fingerprint has no version to key its runs by,
// so every report pairs it again.
func TestDuckDBActivityRunsRepairUnfingerprintedSessions(t *testing.T) {
	ctx := t.Context()
	conn := openTestDuckDB(t)
	require.NoError(t, EnsureSchema(ctx, conn))
	store := NewStoreFromDB(conn)
	_, err := conn.ExecContext(ctx, `INSERT INTO sessions (id, machine, project, agent) VALUES ('s', 'machine', 'project', 'codex')`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `INSERT INTO messages (id, session_id, ordinal, role, content, timestamp)
		VALUES (1, 's', 0, 'user', '', TIMESTAMP '2026-06-16 09:00:00'),
			(2, 's', 1, 'assistant', '', TIMESTAMP '2026-06-16 09:01:00')`)
	require.NoError(t, err)
	q, err := activity.ResolveQuery(activity.QueryInput{Preset: "day", Date: "2026-06-16", Timezone: "UTC"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	spans := func() []time.Duration {
		runs, _, err := store.activityReportRuns(ctx, []string{"s"}, q)
		require.NoError(t, err)
		var out []time.Duration
		for _, run := range runs {
			out = append(out, run.End.Sub(run.Start))
		}
		return out
	}
	require.Equal(t, []time.Duration{time.Minute}, spans())
	_, err = conn.ExecContext(ctx, `INSERT INTO messages (id, session_id, ordinal, role, content, timestamp)
		VALUES (3, 's', 2, 'user', '', TIMESTAMP '2026-06-16 09:03:00')`)
	require.NoError(t, err)
	require.Equal(t, []time.Duration{3 * time.Minute}, spans())
}
