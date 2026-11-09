package activity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		func(_ context.Context, yield func(Run) error) error {
			for _, run := range runs {
				if err := yield(run); err != nil {
					return err
				}
			}
			return nil
		}, func() ([]UsageRow, error) { return nil, nil })
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
