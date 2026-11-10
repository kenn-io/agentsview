package activity

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runSlice streams runs in order.
func runSlice(runs []Run) RunSource {
	return func(_ context.Context, yield func(Run) error) error {
		for _, run := range runs {
			if err := yield(run); err != nil {
				return err
			}
		}
		return nil
	}
}

func runsTestQuery(t *testing.T) Query {
	t.Helper()
	q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC", BucketOverride: "1h"},
		time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	return q
}

func runsTestParams(q Query) Params {
	return Params{
		RangeStart: q.RangeStart, RangeEnd: q.RangeEnd, Loc: q.Loc, EffectiveEnd: q.EffectiveEnd,
		GapCapSeconds: q.GapCapSeconds, Bucket: q.Bucket,
	}
}

func at(hour, minute, second, ms int) time.Time {
	return time.Date(2026, 1, 10, hour, minute, second, ms*int(time.Millisecond), time.UTC)
}

// sessionRuns derives runs from candidates given in sweep order.
func sessionRuns(candidates ...IntervalCandidate) SessionRuns {
	b := NewSessionRunsBuilder(300)
	for _, c := range candidates {
		b.Add(c)
	}
	return b.Finish()
}

func sweepRuns(t *testing.T, q Query, runs []Run) CandidateArtifacts {
	t.Helper()
	var sessions []SessionMeta
	seen := map[string]bool{}
	for _, run := range runs {
		if !seen[run.SessionID] {
			seen[run.SessionID] = true
			sessions = append(sessions, SessionMeta{SessionID: run.SessionID})
		}
	}
	artifacts, err := BuildRunArtifactsWithSurvivorUsage(t.Context(), runsTestParams(q), sessions,
		runSlice(runs), func() ([]UsageRow, error) { return nil, nil })
	require.NoError(t, err)
	return artifacts
}

// A run joins touching pieces of one model, so it can be longer than the
// gap cap; the sweep must not cap it again.
func TestRunLongerThanGapCapKeepsItsLength(t *testing.T) {
	q := runsTestQuery(t)
	runs := sessionRuns(
		IntervalCandidate{SessionID: "s", Start: at(11, 0, 0, 700), End: at(11, 20, 0, 0), ClosingRole: "assistant", ClosingModel: "model-b"},
		IntervalCandidate{SessionID: "s", Start: at(11, 5, 0, 700), End: at(11, 9, 0, 700), ClosingRole: "assistant", ClosingModel: "model-b"},
	)
	clipped := runs.AppendClipped(nil, "s", q.RangeStart, q.EffectiveEnd)
	assert.Equal(t, []Run{{SessionID: "s", Start: at(11, 0, 0, 700), End: at(11, 9, 0, 700), Tail: at(11, 5, 0, 700), Model: "model-b"}}, clipped)
	artifacts := sweepRuns(t, q, clipped)
	require.Len(t, artifacts.Sessions, 1)
	require.NotNil(t, artifacts.Sessions[0].AgentMinutes)
	// 5 capped minutes plus 4 touching minutes.
	assert.InDelta(t, 9.0, *artifacts.Sessions[0].AgentMinutes, 0)
}

// A piece that starts and ends inside the first second of a bucket puts
// its session in that bucket, whether it stands alone or ends a run.
func TestPieceInFirstSecondOfBucketAddsMembership(t *testing.T) {
	q := runsTestQuery(t)
	alone, err := BuildCandidateArtifacts(t.Context(), runsTestParams(q), []SessionMeta{{SessionID: "alone"}},
		[]IntervalCandidate{{SessionID: "alone", Start: at(11, 0, 0, 100), End: at(11, 0, 0, 600), ClosingRole: "user"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, BucketMembership{1 << 11}, alone.Membership["alone"])

	runs := sessionRuns(
		IntervalCandidate{SessionID: "run", Start: at(10, 59, 59, 400), End: at(11, 0, 0, 200), ClosingRole: "user"},
		IntervalCandidate{SessionID: "run", Start: at(11, 0, 0, 200), End: at(11, 0, 0, 700), ClosingRole: "user"},
	)
	clipped := runs.AppendClipped(nil, "run", q.RangeStart, q.EffectiveEnd)
	require.Equal(t, []Run{{SessionID: "run", Start: at(10, 59, 59, 400), End: at(11, 0, 0, 700), Tail: at(11, 0, 0, 200), Model: "unknown"}}, clipped)
	assert.Equal(t, BucketMembership{1<<10 | 1<<11}, sweepRuns(t, q, clipped).Membership["run"])
}

// Clipping a run keeps the start of the last piece left inside the range.
func TestAppendClippedTail(t *testing.T) {
	runs := sessionRuns(
		IntervalCandidate{SessionID: "s", Start: at(11, 0, 0, 200), End: at(11, 0, 0, 900), ClosingRole: "user"},
		IntervalCandidate{SessionID: "s", Start: at(11, 0, 0, 900), End: at(11, 4, 0, 0), ClosingRole: "user"},
		IntervalCandidate{SessionID: "s", Start: at(11, 4, 0, 0), End: at(11, 6, 0, 0), ClosingRole: "user"},
	)
	tests := []struct {
		name       string
		start, end time.Time
		want       []Run
	}{
		{
			"whole", at(0, 0, 0, 0), at(23, 0, 0, 0),
			[]Run{{SessionID: "s", Start: at(11, 0, 0, 200), End: at(11, 6, 0, 0), Tail: at(11, 4, 0, 0), Model: "unknown"}},
		},
		{
			"end cuts the second piece", at(0, 0, 0, 0), at(11, 3, 0, 0),
			[]Run{{SessionID: "s", Start: at(11, 0, 0, 200), End: at(11, 3, 0, 0), Tail: at(11, 0, 0, 900), Model: "unknown"}},
		},
		{
			"end at a piece boundary", at(0, 0, 0, 0), at(11, 4, 0, 0),
			[]Run{{SessionID: "s", Start: at(11, 0, 0, 200), End: at(11, 4, 0, 0), Tail: at(11, 0, 0, 900), Model: "unknown"}},
		},
		{
			"start cuts the last piece", at(11, 5, 0, 0), at(23, 0, 0, 0),
			[]Run{{SessionID: "s", Start: at(11, 5, 0, 0), End: at(11, 6, 0, 0), Tail: at(11, 5, 0, 0), Model: "unknown"}},
		},
		{"range after the run", at(11, 6, 0, 0), at(23, 0, 0, 0), nil},
		{"range before the run", at(0, 0, 0, 0), at(11, 0, 0, 200), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, runs.AppendClipped(nil, "s", tt.start, tt.end))
		})
	}
}

// A stream that leaves out candidates starting at or after the effective
// end floored to microseconds, as SQLite's does, gives the same report as
// the runs derived from every candidate and clipped with that bound.
func TestRunsMatchCandidatesWithMicrosecondStartBound(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 8))
	base := time.Date(2026, 1, 10, 9, 0, 0, 0, time.UTC)
	ns := func(n int) time.Duration { return time.Duration(n) }
	for trial := range 300 {
		var candidates []IntervalCandidate
		var sessions []SessionMeta
		for s := range 1 + rng.IntN(6) {
			id := fmt.Sprintf("s%d", s)
			sessions = append(sessions, SessionMeta{SessionID: id, IsAutomated: s%3 == 1, IsSubagent: s%3 == 2})
			at := base.Add(ns(rng.IntN(int(20 * time.Minute))))
			for range rng.IntN(12) {
				// Steps of a few microseconds land pieces on the bound.
				step := ns(rng.IntN(4000))
				if rng.IntN(3) == 0 {
					step = ns(rng.IntN(int(6 * time.Minute)))
				}
				start := at.Add(step)
				end := start.Add(ns(rng.IntN(int(7*time.Minute))) - ns(rng.IntN(2000)))
				if rng.IntN(2) == 0 {
					at = start
				}
				model := []string{"", "a", "b"}[rng.IntN(3)]
				candidates = append(candidates, IntervalCandidate{
					SessionID: id, StartOrdinal: rng.IntN(5), Start: start, End: end,
					ClosingRole: "assistant", ClosingModel: model, PriorModel: "unknown",
				})
			}
		}
		slices.SortStableFunc(candidates, func(a, b IntervalCandidate) int {
			if CandidateLess(a, b) {
				return -1
			}
			if CandidateLess(b, a) {
				return 1
			}
			return 0
		})
		// End near the activity with a sub-microsecond part, often at a
		// microsecond some candidate starts in.
		end := base.Add(ns(rng.IntN(int(25 * time.Minute))))
		if len(candidates) > 0 {
			// A candidate's start, or its end, where a trimmed piece starts.
			c := candidates[rng.IntN(len(candidates))]
			switch rng.IntN(3) {
			case 0:
				end = c.Start.Truncate(time.Microsecond).Add(ns(1 + rng.IntN(999)))
			case 1:
				end = c.End.Truncate(time.Microsecond).Add(ns(1 + rng.IntN(999)))
			}
		}
		q, err := ResolveQuery(QueryInput{Preset: "day", Date: "2026-01-10", Timezone: "UTC", BucketOverride: "5m"}, end)
		require.NoError(t, err)
		bound := time.UnixMicro(q.EffectiveEnd.UnixMicro()).UTC()

		var streamed []IntervalCandidate
		builders := map[string]*SessionRunsBuilder{}
		for _, c := range candidates {
			if c.Start.UnixMicro() < q.EffectiveEnd.UnixMicro() {
				streamed = append(streamed, c)
			}
			if builders[c.SessionID] == nil {
				builders[c.SessionID] = NewSessionRunsBuilder(q.GapCapSeconds)
			}
			builders[c.SessionID].Add(c)
		}
		var runs []Run
		for _, session := range sessions {
			if b := builders[session.SessionID]; b != nil {
				runs = b.Finish().AppendClippedBefore(runs, session.SessionID, q.RangeStart, q.EffectiveEnd, bound)
			}
		}
		SortRuns(runs)
		p := runsTestParams(q)
		p.Partial = q.Partial
		want, err := BuildCandidateArtifacts(t.Context(), p, sessions, streamed, nil)
		require.NoError(t, err)
		got, err := BuildRunArtifactsWithSurvivorUsage(t.Context(), p, sessions,
			runSlice(runs), func() ([]UsageRow, error) { return nil, nil })
		require.NoError(t, err)
		require.Equal(t, want, got, "trial %d", trial)
	}
}
