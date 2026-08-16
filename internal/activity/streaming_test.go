package activity

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPairActivityEventsPreservesOrdinalAdjacencyAtRangeEdges(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	events := []ActivityEvent{
		{SessionID: "edge", Ordinal: 1, Timestamp: "2026-06-15T23:54:59Z", Role: "user"},
		{SessionID: "edge", Ordinal: 2, Timestamp: "2026-06-15T23:59:30Z", Role: "assistant", Model: "old"},
		{SessionID: "edge", Ordinal: 3, Timestamp: "2026-06-16T23:59:00Z", Role: "user"},
		{SessionID: "edge", Ordinal: 4, Timestamp: "2026-06-17T00:20:00Z", Role: "assistant", Model: "new"},
		{SessionID: "nonmonotone", Ordinal: 1, Timestamp: "2026-06-16T10:05:00Z", Role: "user"},
		{SessionID: "nonmonotone", Ordinal: 2, Timestamp: "2026-06-16T09:00:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "nonmonotone", Ordinal: 3, Timestamp: "2026-06-16T10:06:00Z", Role: "assistant", Model: "m2"},
	}

	got := PairActivityEvents(events, p.RangeStart, p.EffectiveEnd, 5*time.Minute)

	require.Len(t, got, 4)
	assert.Equal(t, IntervalCandidate{
		SessionID: "edge", StartOrdinal: 2, EndOrdinal: 3,
		Start:       mustStart(t, "2026-06-15T23:59:30Z"),
		End:         mustStart(t, "2026-06-16T23:59:00Z"),
		ClosingRole: "user", ClosingModel: "", PriorModel: "old",
	}, got[0], "a start inside the left cap window remains eligible")
	assert.Equal(t, IntervalCandidate{
		SessionID: "nonmonotone", StartOrdinal: 2, EndOrdinal: 3,
		Start:       mustStart(t, "2026-06-16T09:00:00Z"),
		End:         mustStart(t, "2026-06-16T10:06:00Z"),
		ClosingRole: "assistant", ClosingModel: "m2", PriorModel: "unknown",
	}, got[1])
	assert.Equal(t, IntervalCandidate{
		SessionID: "nonmonotone", StartOrdinal: 1, EndOrdinal: 2,
		Start:       mustStart(t, "2026-06-16T10:05:00Z"),
		End:         mustStart(t, "2026-06-16T09:00:00Z"),
		ClosingRole: "assistant", ClosingModel: "m1", PriorModel: "unknown",
	}, got[2], "the adapter emits the real adjacent pair for shared rejection")
	assert.Equal(t, IntervalCandidate{
		SessionID: "edge", StartOrdinal: 3, EndOrdinal: 4,
		Start:       mustStart(t, "2026-06-16T23:59:00Z"),
		End:         mustStart(t, "2026-06-17T00:20:00Z"),
		ClosingRole: "assistant", ClosingModel: "new", PriorModel: "old",
	}, got[3], "a successor beyond the right bound remains attached")
}

func TestAggregateCandidatesDifferential(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	sessions := []SessionMeta{
		{SessionID: "a", Title: "A", Project: "p", Agent: "claude"},
		{SessionID: "b", Title: "B", Project: "p", Agent: "codex", IsAutomated: true},
	}
	events := []ActivityEvent{
		{SessionID: "a", Ordinal: 1, Timestamp: "2026-06-15T23:59:30Z", Role: "user"},
		{SessionID: "a", Ordinal: 2, Timestamp: "2026-06-16T00:01:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "a", Ordinal: 3, Timestamp: "2026-06-16T00:04:00.500000Z", Role: "assistant", Model: "m1"},
		{SessionID: "b", Ordinal: 1, Timestamp: "2026-06-16T23:59:00Z", Role: "user"},
		{SessionID: "b", Ordinal: 2, Timestamp: "2026-06-17T00:20:00Z", Role: "assistant", Model: "m2"},
	}

	want, err := Aggregate(p, append([]SessionMeta(nil), sessions...), events, nil)
	require.NoError(t, err)
	candidates := PairActivityEvents(events, p.RangeStart, p.EffectiveEnd, 5*time.Minute)
	got, err := AggregateCandidates(
		context.Background(), p, append([]SessionMeta(nil), sessions...), candidates, nil,
	)
	require.NoError(t, err)

	want.Intervals = []ReportInterval{}
	assert.Empty(t, got.Intervals, "candidate reports do not expose raw intervals")
	assert.Equal(t, want, got)
}

func TestBuildCandidateArtifactsUsesSecondPrecisionMembership(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	events := []ActivityEvent{
		{SessionID: "edge", Ordinal: 1, Timestamp: "2026-06-16T00:04:59.900000Z", Role: "user"},
		{SessionID: "edge", Ordinal: 2, Timestamp: "2026-06-16T00:05:00.100000Z", Role: "assistant", Model: "m1"},
		{SessionID: "point", Ordinal: 1, Timestamp: "2026-06-16T00:07:00.100000Z", Role: "user"},
		{SessionID: "point", Ordinal: 2, Timestamp: "2026-06-16T00:07:00.900000Z", Role: "assistant", Model: "m2"},
	}
	sessions := []SessionMeta{
		{SessionID: "edge", Project: "p", Agent: "claude"},
		{SessionID: "point", Project: "p", Agent: "claude"},
	}
	candidates := PairActivityEvents(events, p.RangeStart, p.EffectiveEnd, 5*time.Minute)

	artifacts, err := BuildCandidateArtifacts(
		context.Background(), p, sessions, candidates, nil,
	)
	require.NoError(t, err)

	assert.True(t, artifacts.Membership["edge"].Contains(0))
	assert.False(t, artifacts.Membership["edge"].Contains(1),
		"whole-second drill-down omits the microsecond-only right overlap")
	assert.True(t, artifacts.Membership["point"].Contains(1),
		"a sub-second span collapses to a point in its containing half-open bucket")
	assert.InDelta(t, 0.1/60, artifacts.Report.Buckets[0].AgentMinutes, 1e-12)
	assert.InDelta(t, 0.9/60, artifacts.Report.Buckets[1].AgentMinutes, 1e-12)
}

func TestBuildCandidateArtifactsNormalizesFractionalCustomBucketBoundsForMembership(t *testing.T) {
	query, err := ResolveQuery(QueryInput{
		Preset: "custom", Timezone: "UTC", BucketOverride: "5m",
		From: "2026-06-16T00:00:00.500Z", To: "2026-06-16T00:10:00.500Z",
	}, fixedNow(t))
	require.NoError(t, err)
	p := paramsFromQuery(query)
	events := []ActivityEvent{
		{SessionID: "range-start", Ordinal: 1, Timestamp: "2026-06-16T00:00:00.600Z", Role: "user"},
		{SessionID: "range-start", Ordinal: 2, Timestamp: "2026-06-16T00:00:00.900Z", Role: "assistant"},
		{SessionID: "bucket-boundary", Ordinal: 1, Timestamp: "2026-06-16T00:05:00.100Z", Role: "user"},
		{SessionID: "bucket-boundary", Ordinal: 2, Timestamp: "2026-06-16T00:05:00.900Z", Role: "assistant"},
	}
	candidates := PairActivityEvents(
		events, p.RangeStart, p.EffectiveEnd, 5*time.Minute,
	)

	artifacts, err := BuildCandidateArtifacts(
		context.Background(), p, nil, candidates, nil,
	)
	require.NoError(t, err)
	require.Len(t, artifacts.Report.Buckets, 2)
	assert.Equal(t, "2026-06-16T00:00:00Z", artifacts.Report.Buckets[0].Start)
	assert.Equal(t, "2026-06-16T00:05:00Z", artifacts.Report.Buckets[1].Start)
	assert.True(t, artifacts.Membership["range-start"].Contains(0),
		"the first serialized bucket includes a point at its start")
	assert.False(t, artifacts.Membership["range-start"].Contains(1))
	assert.False(t, artifacts.Membership["bucket-boundary"].Contains(0))
	assert.True(t, artifacts.Membership["bucket-boundary"].Contains(1),
		"a point at the serialized boundary belongs to the following bucket")
}

func TestAggregateCandidatesCarriesConcurrencyAcrossBucketBoundary(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	events := []ActivityEvent{
		{SessionID: "carried", Ordinal: 1, Timestamp: "2026-06-16T00:04:00Z", Role: "user"},
		{SessionID: "carried", Ordinal: 2, Timestamp: "2026-06-16T00:06:00Z", Role: "assistant", Model: "m1"},
		{SessionID: "new", Ordinal: 1, Timestamp: "2026-06-16T00:05:30Z", Role: "user"},
		{SessionID: "new", Ordinal: 2, Timestamp: "2026-06-16T00:06:00Z", Role: "assistant", Model: "m2"},
	}
	candidates := PairActivityEvents(events, p.RangeStart, p.EffectiveEnd, 5*time.Minute)

	got, err := AggregateCandidates(context.Background(), p, nil, candidates, nil)
	require.NoError(t, err)

	assert.Equal(t, 2, got.Buckets[1].MaxAgents)
	assert.Equal(t, 2, got.Peak.Agents)
}

func TestAggregateCandidatesRandomizedDifferential(t *testing.T) {
	p := baseParams(t, "2026-06-16", "UTC")
	random := rand.New(rand.NewSource(42))
	for iteration := range 100 {
		var sessions []SessionMeta
		var events []ActivityEvent
		for sessionIndex := range 8 {
			sessionID := fmt.Sprintf("s-%02d", sessionIndex)
			sessions = append(sessions, SessionMeta{
				SessionID: sessionID, Project: "p", Agent: "agent",
				IsAutomated: sessionIndex%3 == 0,
			})
			instant := p.RangeStart.Add(time.Duration(random.Intn(26*60)-10) * time.Minute)
			for ordinal := range 12 {
				if ordinal > 0 {
					deltaMicros := random.Intn(9*60*1_000_000) - 30*1_000_000
					instant = instant.Add(time.Duration(deltaMicros) * time.Microsecond)
				}
				role := "user"
				model := ""
				if ordinal%2 == 1 {
					role = "assistant"
					model = fmt.Sprintf("m-%d", ordinal%3)
				}
				events = append(events, ActivityEvent{
					SessionID: sessionID, Ordinal: ordinal,
					Timestamp: instant.UTC().Format(time.RFC3339Nano),
					Role:      role, Model: model,
				})
			}
		}

		want, err := Aggregate(
			p, append([]SessionMeta(nil), sessions...), events, nil,
		)
		require.NoError(t, err)
		candidates := PairActivityEvents(
			events, p.RangeStart, p.EffectiveEnd, 5*time.Minute,
		)
		got, err := AggregateCandidates(
			context.Background(), p,
			append([]SessionMeta(nil), sessions...), candidates, nil,
		)
		require.NoError(t, err)
		want.Intervals = []ReportInterval{}
		assert.Equalf(t, normalizeReportMinutes(want), normalizeReportMinutes(got),
			"iteration %d", iteration)
	}
}

func normalizeReportMinutes(report Report) Report {
	normalize := func(value float64) float64 {
		return math.Round(value*1e9) / 1e9
	}
	report.Totals.ActiveMinutes = normalize(report.Totals.ActiveMinutes)
	report.Totals.IdleMinutes = normalize(report.Totals.IdleMinutes)
	report.Totals.AgentMinutes = normalize(report.Totals.AgentMinutes)
	report.Totals.AutomatedAgentMinutes = normalize(report.Totals.AutomatedAgentMinutes)
	report.Totals.InteractiveAgentMinutes = normalize(report.Totals.InteractiveAgentMinutes)
	for index := range report.Buckets {
		report.Buckets[index].AgentMinutes = normalize(report.Buckets[index].AgentMinutes)
	}
	for _, rows := range [][]KeyMinutes{report.ByProject, report.ByModel, report.ByAgent} {
		for index := range rows {
			rows[index].AgentMinutes = normalize(rows[index].AgentMinutes)
			rows[index].AutomatedAgentMinutes = normalize(rows[index].AutomatedAgentMinutes)
			rows[index].InteractiveAgentMinutes = normalize(rows[index].InteractiveAgentMinutes)
		}
	}
	for index := range report.BySession {
		if report.BySession[index].AgentMinutes != nil {
			minutes := normalize(*report.BySession[index].AgentMinutes)
			report.BySession[index].AgentMinutes = &minutes
		}
	}
	return report
}
