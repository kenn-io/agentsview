package clickhouse

import (
	"cmp"
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
	"unique"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
)

// generateActivitySessions builds pairing inputs with the shapes the run
// derivation must preserve: clock reversals, unstamped messages, tool
// events, sessions with only tool events, gaps of exactly the cap, and
// sub-second activity across hour boundaries and the instants in hot.
func generateActivitySessions(rng *rand.Rand, base time.Time, hot []time.Time, sessions int) (map[string]activitySessionInputs, []activity.SessionMeta) {
	roles := []string{"user", "assistant", "assistant", "tool", "system"}
	models := []string{"", "model-a", "model-b"}
	steps := []time.Duration{
		0, time.Microsecond, 300 * time.Second, 300*time.Second + time.Microsecond,
		300*time.Second - time.Microsecond, 999 * time.Millisecond, time.Second,
		-90 * time.Second, -time.Microsecond,
	}
	inputs := map[string]activitySessionInputs{}
	metas := make([]activity.SessionMeta, 0, sessions)
	for i := range sessions {
		id := fmt.Sprintf("s%03d", i)
		metas = append(metas, activity.SessionMeta{
			SessionID: id, IsAutomated: rng.IntN(4) == 0, IsSubagent: rng.IntN(5) == 0,
		})
		// Start near an hour boundary, often inside its first second.
		at := base.Add(time.Duration(rng.IntN(30)) * time.Hour)
		dense := false
		switch rng.IntN(4) {
		case 0:
			at = at.Add(time.Duration(rng.IntN(1_000_000)) * time.Microsecond)
		case 1:
			at = at.Add(-time.Duration(rng.IntN(600)) * time.Second)
		case 2:
			// Sub-second steps across an hour boundary or a cut instant.
			dense = true
			if rng.IntN(2) == 0 {
				at = hot[rng.IntN(len(hot))]
			}
			at = at.Add(-time.Duration(rng.IntN(1_500_000)) * time.Microsecond)
		default:
			at = at.Add(time.Duration(rng.IntN(3600*1_000_000)) * time.Microsecond)
		}
		var entry activitySessionInputs
		messages := rng.IntN(16)
		if rng.IntN(8) == 0 {
			messages = 0
		}
		var stamps []int64
		for ordinal := range messages {
			if dense {
				at = at.Add(time.Duration(rng.IntN(700_000)) * time.Microsecond)
			} else if rng.IntN(3) == 0 {
				at = at.Add(steps[rng.IntN(len(steps))])
			} else {
				at = at.Add(time.Duration(rng.IntN(400*1_000_000)) * time.Microsecond)
			}
			role := roles[rng.IntN(len(roles))]
			m := clickActivityMessage{
				ordinal: ordinal, us: noTimestamp, role: unique.Make(role),
				model: unique.Make(models[rng.IntN(len(models))]), counted: role != "system" && rng.IntN(6) != 0,
			}
			if rng.IntN(6) != 0 {
				m.us = at.UnixMicro()
				stamps = append(stamps, m.us)
			}
			entry.messages = append(entry.messages, m)
		}
		events := rng.IntN(6)
		if messages == 0 {
			events = 1 + rng.IntN(6)
		}
		for range events {
			e := clickActivityTerminal{ordinal: rng.IntN(max(1, messages)), callIndex: rng.IntN(2), eventIndex: rng.IntN(2)}
			switch {
			case len(stamps) > 0 && rng.IntN(3) == 0:
				// Tie with a message, or land just after it.
				e.us = stamps[rng.IntN(len(stamps))] + int64(rng.IntN(2))
			case len(stamps) > 0:
				e.us = stamps[rng.IntN(len(stamps))] + int64(rng.IntN(700*1_000_000)) - 100*1_000_000
			default:
				e.us = at.Add(time.Duration(rng.IntN(900)) * time.Second).UnixMicro()
			}
			entry.events = append(entry.events, e)
		}
		// The read orders tool events by time, then call and event index.
		slices.SortFunc(entry.events, func(a, b clickActivityTerminal) int {
			return cmp.Or(cmp.Compare(a.us, b.us), cmp.Compare(a.callIndex, b.callIndex), cmp.Compare(a.eventIndex, b.eventIndex))
		})
		inputs[id] = entry
	}
	return inputs, metas
}

// candidateArtifacts builds a report the way the candidate stream did
// before the runs: pair the range, sweep every candidate, and count every
// message through MessageAccumulator.Add.
func candidateArtifacts(
	t *testing.T, inputs map[string]activitySessionInputs, metas []activity.SessionMeta, ids []string, q activity.Query,
) activity.CandidateArtifacts {
	t.Helper()
	var store Store
	lower := q.RangeStart.Add(-time.Duration(q.GapCapSeconds) * time.Second)
	paired, terminal, err := store.pairActivitySessions(t.Context(), inputs, ids, lower, q.EffectiveEnd, q)
	require.NoError(t, err)
	artifacts, err := activity.BuildCandidateArtifactsFromSource(t.Context(), activityParams(q), metas,
		activity.MergeCandidateSlice(terminal, func(_ context.Context, yield func(activity.IntervalCandidate) error) error {
			for _, c := range paired {
				if err := yield(c); err != nil {
					return err
				}
			}
			return nil
		}), nil)
	require.NoError(t, err)
	counts := activity.NewMessageAccumulator(q, &artifacts)
	for _, id := range ids {
		for _, m := range inputs[id].messages {
			if m.counted && m.stamped() && !m.at().Before(q.RangeStart) {
				counts.Add(id, m.role.Value(), m.at())
			}
		}
	}
	return artifacts
}

// runArtifacts builds the same report from derived runs.
func runArtifacts(
	t *testing.T, derived map[string]activitySessionDerived, metas []activity.SessionMeta, ids []string, q activity.Query,
) activity.CandidateArtifacts {
	t.Helper()
	runs := clipActivityRuns(derived, ids, q)
	artifacts, err := activity.BuildRunArtifactsWithSurvivorUsage(t.Context(), activityParams(q), metas,
		func(_ context.Context, yield func(activity.Run) error) error {
			for _, run := range runs {
				if err := yield(run); err != nil {
					return err
				}
			}
			return nil
		}, func() ([]activity.UsageRow, error) { return nil, nil })
	require.NoError(t, err)
	done := make(chan struct{})
	close(done)
	pairing := &activityReportPairing{done: done, sessions: derived}
	require.NoError(t, pairing.countMessages(t.Context(), ids, q, &artifacts))
	return artifacts
}

func activityParams(q activity.Query) activity.Params {
	return activity.Params{
		RangeStart: q.RangeStart, RangeEnd: q.RangeEnd, Loc: q.Loc, EffectiveEnd: q.EffectiveEnd,
		Partial: q.Partial, GapCapSeconds: q.GapCapSeconds, Bucket: q.Bucket,
	}
}

// Sweeping each session's derived runs gives the report that sweeping
// every candidate gives, for ranges that cut through runs and buckets
// whose first second holds activity.
func TestActivityRunsMatchCandidateSweep(t *testing.T) {
	base := time.Date(2026, 1, 9, 22, 0, 0, 0, time.UTC)
	queries := []struct {
		name  string
		input activity.QueryInput
		now   time.Time
	}{
		{"ended day", activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"zoned day", activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "America/New_York"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"in progress", activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"}, time.Date(2026, 1, 10, 13, 17, 42, 123456789, time.UTC)},
		{"in progress at a second", activity.QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC"}, time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)},
		{"custom 5m", activity.QueryInput{Preset: "custom", From: "2026-01-10T03:00:00.250Z", To: "2026-01-10T09:30:00.75Z", Timezone: "UTC", BucketOverride: "5m"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"week", activity.QueryInput{Preset: "week", Date: "2026-01-10", Timezone: "UTC"}, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
	}
	// Range ends and starts that cut through runs.
	cuts := []time.Time{
		time.Date(2026, 1, 10, 13, 17, 42, 123456789, time.UTC),
		time.Date(2026, 1, 10, 3, 0, 0, 250_000_000, time.UTC),
		time.Date(2026, 1, 10, 9, 30, 0, 750_000_000, time.UTC),
	}
	rng := rand.New(rand.NewPCG(7, 11))
	for trial := range 200 {
		inputs, metas := generateActivitySessions(rng, base, cuts, 1+rng.IntN(40))
		ids := make([]string, 0, len(metas))
		for _, meta := range metas {
			ids = append(ids, meta.SessionID)
		}
		deriver := newActivityDeriver(300)
		derived := map[string]activitySessionDerived{}
		for _, id := range ids {
			derived[id] = deriver.derive(id, inputs[id])
		}
		for _, query := range queries {
			q, err := activity.ResolveQuery(query.input, query.now)
			require.NoError(t, err)
			want := candidateArtifacts(t, inputs, metas, ids, q)
			got := runArtifacts(t, derived, metas, ids, q)
			require.Equal(t, want, got, "trial %d, %s", trial, query.name)
		}
	}
}
